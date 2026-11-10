# 006 Admin: users, permissions and recovery

Give admins a user list, a way to change permissions, and a way to recover a
user who lost their passkeys. Code goes in `auth.go`.

## Behaviour

`GET /admin` (`accAdmin`): back link to `/`, title "Admin". A table (a stacked
list on narrow screens) of all users ordered by username: username, permission
badges, created date, invited by, number of passkeys. Each row links to
`/admin/u/{id}`. Task 023 adds stats columns and a link to the groups list.

`GET /admin/u/{id}` (`accAdmin`): back link to `/admin`, title is the
username. Sections:

1. **Permissions.** A form posting to `POST /admin/u/{id}/perms` with three
   checkboxes (`admin`, `invite`, `group`) and a save button.
2. **Passkeys.** Label, created, last used, and a "Delete" button for each
   (posting to `POST /admin/u/{id}/passkey/delete`). An admin may delete the
   last passkey of another user; that locks the user out until they use a
   recovery link. Say so in a short note beside the list.
3. **Recovery link.** A button posting to `POST /admin/u/{id}/link` that
   creates an add passkey link for that user (ttl 24 hours, `created_by` = the
   admin). Pending links for the user are listed with a `data-copy` field and
   a revoke button (`POST /admin/u/{id}/link/revoke`).

Rules:

- An admin cannot remove their own `admin` permission and cannot delete their
  own passkeys from this page (they use `/settings`). This guarantees there is
  always at least one admin. Enforce on the server and do not render the
  controls.
- Changing permissions takes effect on the user's next request, because the
  user row is loaded on every request. No session handling is needed.

Add a link to `/admin` in the home page header for admins.

Queries: `UsersAll` (with passkey count and inviter username),
`UserSetPerms`, `CredentialDeleteByAdmin`, `InvitesPendingForUser`,
`InviteRevokeForUser`.

## Tests

- Every route here is 404 for a non admin and redirects when signed out.
- Setting each permission on and off is reflected in the database and in what
  the target user can reach (for example `/invites`).
- An admin posting to remove their own admin flag is refused and the flag
  stays.
- Admin deletes another user's only passkey: that user's session stops
  working; a recovery link created by the admin lets them register a new
  passkey and sign in as the same user id.
- Recovery links do not change `invites_sent`.
- Unknown user id is 404.

## Done when

- Tests above pass and `./.hooks/pre-push` passes.
