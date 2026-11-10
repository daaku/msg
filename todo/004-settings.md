# 004 Settings page and managing your own passkeys

Create the single settings page that holds everything personal to a user.
This task builds the page and its passkey sections; notifications (020) and
stats (023) are added to the same page later. There must never be a second
personal page. Here the user can see who they are, add another passkey on the
current device, create a link to add a passkey on another device, and delete
passkeys. All code goes in `auth.go`.

## Behaviour

`GET /settings` (`accUser`) renders, top to bottom:

1. Header with a back link to `/` and the title "Settings".
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
   `POST /settings/link`. Pending links for this user are listed below it, each
   with a read only text field holding the full URL (`data-copy`), the expiry,
   and a "Revoke" button posting to `POST /settings/link/revoke`.
6. A "Sign out" button.

Later tasks add notification and stats sections to this page. Build the page
as an ordered list of section functions, each rendering a `<section>` with an
`id` (`passkeys`, `devices`, later `notifications` and `stats`), so they can
be appended and linked to.

Endpoints, all `accUser`:

- `GET /passkey/options`: `BeginRegistration` for the current user with their
  existing credentials excluded, resident key required. Session data in the
  `r` cookie from task 003.
- `POST /passkey/create`: verify, insert the credential, flash, redirect to
  `/settings`.
- `POST /passkey/delete`: delete where `id = ? AND user_id = current`. Refuse
  with a flash when it is the user's last passkey (count inside the same
  transaction as the delete). Deleting the credential of the current session
  signs this browser out, which is correct.
- `POST /settings/link`: `a.createInvite` with `user_id` = self, the user's own
  username and handle, ttl 24 hours. At most 5 pending links per user; beyond
  that flash an error.
- `POST /settings/link/revoke`: delete the invite where `id = ?` and
  `user_id = current`.

JavaScript: add the `data-copy` block (item 9 in the inventory): on focus or
click, select the whole field. Nothing else.

Add a link to `/settings` (gear icon) in the home page header.

## Tests

- Settings page lists the user's passkeys and none of another user's.
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
