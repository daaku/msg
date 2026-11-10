# 003 Passkey sign in, sessions, invite acceptance, admin bootstrap

Use fable.

Build the whole authentication core in `auth.go`: sessions, passkey sign in,
accepting an invite (which registers a passkey), and the bootstrap admin URL
printed to the log. Creating invites from the UI is task 005; this task
creates invite rows only for the bootstrap and from tests.

Read `agents.md` ("Authentication and permissions") and bento's
`auth/auth.go`, `auth/auth_test.go` and `assets/main.js` first. Differences
from bento: no email, no codes, users are found by a random handle, the
session is bound to a credential, and registration happens before a user row
exists.

## Design

**WebAuthn config.** RP ID is the hostname of `PUBLIC_URL`, origin is the
origin of `PUBLIC_URL`, display name is `APP_NAME`.

**User adapter.** A type implementing `webauthn.User` built from a handle, a
username and a slice of already loaded credentials. `WebAuthnID()` returns the
16 byte handle. Do not do database calls or panic inside the interface methods
(bento does; load the credentials before constructing the value).

**Session.** Cookie `s`, value `session{UserID, CredentialID, IssuedAt}` sealed
with sookie using `a.secret("session")`. `HttpOnly`, `SameSite=Lax`, `Path=/`,
`Secure` when `PUBLIC_URL` is https, max age 90 days. In `wrap`: open the
cookie, load the user with a query that joins `credentials` on
`credentials.id = CredentialID AND credentials.user_id = users.id`
(`UserBySession`). No row means signed out: delete the cookie. If `IssuedAt`
is more than 24 hours old, issue a fresh cookie (sliding expiry). Put the
`*User` and the credential row id in the request context; `userFrom(ctx)`
returns nil when signed out.

**Access levels.** Finish `wrap`: `accUser` needs a user; `accInvite` needs
`PermInvite || PermAdmin`; `accGroup` needs `PermGroup || PermAdmin`;
`accAdmin` needs `PermAdmin`. Signed out: `GET` redirects to `/sign-in`, other
methods get 401. Signed in without the permission: 404.

**Sign in** (discoverable credentials):

- `GET /sign-in` (public): page with one button "Sign in with passkey"
  (`data-pk-get="/sign-in/options"` inside a form posting to `/sign-in` with a
  hidden `json` input). If already signed in, redirect to `/`.
- `GET /sign-in/options` (public): `BeginDiscoverableLogin` with user
  verification preferred. Store the session data in cookie `l` (10 minutes,
  `a.secret("login")`). Respond with the JSON options and
  `Cache-Control: no-store`.
- `POST /sign-in` (public): read and delete cookie `l`, parse the response,
  `ValidatePasskeyLogin` with a handler that loads the user by handle and
  their credentials. Find the credential row by the returned credential's raw
  id, store the updated credential JSON (sign count and flags change) and
  `last_used_at`, set the session cookie, redirect to `/`. An unknown passkey
  flashes "This passkey is not registered here." and redirects to `/sign-in`.
- `POST /sign-out` (`accUser`): delete the cookie, redirect to `/sign-in`.

**Invite acceptance.** `token` is 32 random bytes, base64 raw URL encoded.

- `GET /invite/{token}` (public): look the invite up with `expires_at > now`.
  Missing or expired: a 404 page saying the link is invalid or has expired.
  Otherwise a page: for a new user "Welcome, <username>. Create a passkey to
  finish setting up your account."; for an add passkey link "Add a passkey for
  <username> on this device." One button with
  `data-pk-create="/invite/<token>/options"` in a form posting to
  `/invite/<token>`. Send `Referrer-Policy: no-referrer` and
  `Cache-Control: no-store` on this page so the token does not leak.
- `GET /invite/{token}/options` (public): `BeginRegistration` for a user
  adapter built from `invite.handle` and `invite.username`, with resident key
  required and user verification preferred. For an add passkey link pass the
  user's existing credentials as exclusions. Session data goes in cookie `r`
  (10 minutes, `a.secret("register")`).
- `POST /invite/{token}` (public): read and delete cookie `r`, parse,
  `CreateCredential`. Then in one transaction:
  1. `InviteUse`: `DELETE FROM invites WHERE token = ? AND expires_at > ?
     RETURNING *`. No row means someone else used it: 404 page. This delete is
     what makes the link single use, so it must be inside the transaction and
     before any insert.
  2. If `user_id` is NULL: insert the user (`username`, `handle`,
     `perm_admin` from the invite, `invited_by = created_by`). A unique
     violation here is an internal error (it should be impossible).
  3. Insert the credential (raw id, JSON, label, `created_at`,
     `last_used_at`). Label is "<Browser> on <OS>" from the user agent using
     `github.com/LumenResearch/uasurfer` as bento does.
  Then set the session cookie for that user and the new credential and
  redirect to `/`. An existing session in the browser is simply replaced.

**Bootstrap.** At startup, after migrations, if `UserCount` is zero: delete
any invite with `created_by IS NULL AND user_id IS NULL`, create one with
`username = ADMIN_USERNAME`, a fresh handle, `perm_admin = TRUE`, expiring in
24 hours, and print `Bootstrap admin: <PUBLIC_URL>/invite/<token>` to stdout.
If users exist, print nothing. Validate `ADMIN_USERNAME` with `validUsername`
at config load.

**Helpers** other tasks will use, put them in `auth.go`:

- `validUsername(s string) (string, error)`: trims, lowercases, checks
  `^[a-z0-9][a-z0-9_.-]{1,31}$`.
- `a.createInvite(ctx, q, p inviteParams) (Invite, error)`: generates the
  token (and the handle for a new user invite), inserts. Parameters: username,
  target user id (0 for new), admin flag, creator id (0 for bootstrap), ttl.
- `a.inviteURL(inv Invite) string`.

**Queries.** `UserBySession`, `UserByHandle`, `UserByID`, `UserByUsername`,
`UserCreate`, `CredentialsByUser`, `CredentialByCredentialID`,
`CredentialCreate`, `CredentialUpdate`, `InviteByToken`, `InviteCreate`,
`InviteUse`, `InviteDeleteBootstrap`.

**JavaScript.** Add the `data-pk-create` and `data-pk-get` blocks from bento
to `assets/main.js`. If the user cancels the passkey prompt
(`NotAllowedError`) do nothing; show other errors in an element with
`role="alert"` next to the button, do not use `alert()`.

## Tests (`auth_test.go`, using `virtualwebauthn`)

Add harness helpers to `main_test.go`: `ta.newUser(username, perms...)` which
inserts a user and one credential directly and returns a struct holding the
user, the virtual authenticator and credential, and a ready session cookie;
`ta.as(user)` request option that attaches that cookie.

Cover at least:

- Bootstrap: empty database creates exactly one admin invite; a second start
  replaces it; with a user present nothing is created.
- Full invite acceptance through the three endpoints creates the user with
  the invite's username and admin flag, one credential, deletes the invite,
  and the returned cookie authenticates `GET /`.
- The same token cannot be used twice (second `POST` is 404 and creates
  nothing). Expired token is 404 on all three endpoints.
- Add passkey link: adds a second credential to the existing user, creates no
  user, and the session is bound to the new credential.
- Full sign in through options and post with the virtual authenticator.
- Sign in with a credential that was deleted from the database fails.
- A session whose credential row is deleted is signed out on the next
  request.
- Session cookie older than 24 hours is reissued; tampered cookie is ignored.
- Access matrix: for each access level, signed out, plain user, user with the
  matching permission, admin.
- `validUsername` table test.
- The invite page response carries `no-referrer` and `no-store`.

## Done when

- With `DEV=1 go run .` on an empty `data/`, opening the printed URL in a
  browser creates the admin with a passkey and lands on `/`; signing out and
  signing in with the passkey works. If no browser is available, say so in the
  report; the tests must still cover the flow.
- No query or code path can create a user without a passkey.
- `./.hooks/pre-push` passes.
