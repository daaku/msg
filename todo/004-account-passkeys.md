# 004 Account page and managing your own passkeys

Give a signed in user a page to see their account, add another passkey on the
current device, create a link to add a passkey on another device, and delete
passkeys. All code goes in `auth.go`.

## Behaviour

`GET /account` (`accUser`) renders, top to bottom:

1. Header with a back link to `/` and the title "Account".
2. The username (read only) and the permissions the user holds, as words.
3. **Passkeys.** One row per credential: label, created date, last used date,
   and a "Delete" button (a form posting to `/passkey/delete` with the row
   id). The row for the credential the current session was made with is marked
   "this device". When the user has only one passkey the delete button is not
   rendered.
4. "Add a passkey on this device": the same `data-pk-create` pattern as the
   invite page, options from `GET /passkey/options`, posting to
   `POST /passkey/create`.
5. **Add a passkey on another device.** A button posting to
   `POST /account/link`. Pending links for this user are listed below it, each
   with a read only text field holding the full URL (`data-copy`), the expiry,
   and a "Revoke" button posting to `POST /account/link/revoke`.
6. A "Sign out" button.

Later tasks add push and stats sections to this page. Build the page as a
list of section functions so they can be appended.

Endpoints, all `accUser`:

- `GET /passkey/options`: `BeginRegistration` for the current user with their
  existing credentials excluded, resident key required. Session data in the
  `r` cookie from task 003.
- `POST /passkey/create`: verify, insert the credential, flash, redirect to
  `/account`.
- `POST /passkey/delete`: delete where `id = ? AND user_id = current`. Refuse
  with a flash when it is the user's last passkey (count inside the same
  transaction as the delete). Deleting the credential of the current session
  signs this browser out, which is correct.
- `POST /account/link`: `a.createInvite` with `user_id` = self, the user's own
  username and handle, ttl 24 hours. At most 5 pending links per user; beyond
  that flash an error.
- `POST /account/link/revoke`: delete the invite where `id = ?` and
  `user_id = current`.

JavaScript: add the `data-copy` block (item 9 in the inventory): on focus or
click, select the whole field. Nothing else.

Add a link to `/account` (gear icon) in the home page header.

## Tests

- Account page lists the user's passkeys and none of another user's.
- Add passkey through options and create with `virtualwebauthn`; the new
  passkey can sign in.
- Delete: own passkey works; another user's id is a no op; the last passkey is
  refused; deleting the session's passkey signs out.
- Link: create, appears on the page with the right URL, the URL works through
  the flow from task 003 and adds a credential; revoke removes it; another
  user cannot revoke it; the sixth link is refused.
- Signed out requests to every endpoint here are redirected or 401.

## Done when

- All of the above is covered by passing tests.
- `./.hooks/pre-push` passes.
