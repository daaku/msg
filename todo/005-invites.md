# 005 Sending invites

Let users with the invite permission (and admins) create single use sign up
links for a chosen username. Code goes in `auth.go`.

## Behaviour

`GET /invites` (`accInvite`):

- Back link to `/`, title "Invites".
- A form: one text input `username` (`autocapitalize="none"`,
  `autocomplete="off"`, `spellcheck="false"`, `required`, `pattern` matching
  the username rule, a hint describing the allowed characters) and a "Create
  invite" button, posting to `POST /invites`.
- The list of pending new user invites created by this user (admins see
  everyone's, with the creator's username shown). Each row: username, a read
  only `data-copy` field with the full URL, "expires in N days", and a
  "Revoke" button posting to `POST /invites/revoke`. Expired rows are not
  shown.

`POST /invites` (`accInvite`):

- Normalise with `validUsername`. On failure flash the reason and redirect
  back, keeping the typed value in the query string so the field is refilled.
- In one transaction: delete expired new user invites with that username;
  fail with "That username is taken." if a user has it; fail with "There is
  already a pending invite for that username." if a pending invite has it;
  otherwise `a.createInvite` with ttl 7 days and `created_by` = current user,
  and `a.statUser` adding 1 to `invites_sent`.
- The partial unique index `invites_new_username` is the backstop for races.
  Map a unique constraint error to the same friendly message.
- Redirect to `/invites` with a success flash. The new link is at the top of
  the list.

`POST /invites/revoke` (`accInvite`): delete where `id = ?` and `user_id IS
NULL` and (`created_by = current` or the current user is admin). Never deletes
the bootstrap invite (`created_by IS NULL`).

Invites carry no permissions: a user created from one starts with none. Only
the bootstrap invite sets `perm_admin`.

Add a link to `/invites` in the home page header, rendered only for users who
may use it.

Queries: `InvitesPendingByCreator`, `InvitesPendingAll`,
`InviteDeleteExpiredByUsername`, `InvitePendingByUsername`, `InviteRevoke`.

## Tests

- A user without the permission gets 404 on all three routes; signed out is
  redirected.
- Create: valid username creates one invite, increments `invites_sent` by one,
  and the link works end to end (accept it with `virtualwebauthn`, the new
  user exists with no permissions and `invited_by` set).
- Rejections: invalid characters, too short, existing user, existing pending
  invite, different case of an existing username. None of these change
  `invites_sent`.
- An expired pending invite for a username does not block a new one.
- Revoke: own invite; another user's invite is refused for a non admin and
  allowed for an admin; a revoked link shows the invalid link page.
- The list shows only own invites for a non admin and all for an admin, and
  never shows add passkey links.

## Done when

- Tests above pass and `./.hooks/pre-push` passes.
