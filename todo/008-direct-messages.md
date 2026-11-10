# 008 Direct messages

Add direct messages: a chat between exactly two users that anyone can start
without a permission. The chat list shows direct messages and groups
together. Code goes in `chat.go`.

A direct message is a row in `chats` with `is_dm = TRUE`, an empty `name`,
`dm_key = "<lower user id>:<higher user id>"`, and exactly two `members`
rows. Everything that works on a chat (messages, reactions, uploads, live
updates, push, stats) works on it unchanged because it is a chat.

## Rules

- Any signed in user may start a direct message with any other user. Not
  with themselves.
- There is at most one direct message per pair of users. The unique index on
  `dm_key` enforces it.
- Only the two participants can see a direct message. Admins get 404, unlike
  groups. This is the one place where "admin passes every check" does not
  apply.
- A direct message cannot be renamed and members cannot be added or removed.

## Changes

**`a.chatAccess`**: when `chat.IsDm`, a non member always gets 404 (also an
admin) and `manage` is always false.

**Display name.** `a.chatTitle(chat Chat, viewer *User, other string)
string`: the group's name, or for a direct message the other participant's
username. Use it everywhere a chat's name is shown: sidebar, pane header,
info page, page title. `ChatsForUser` (and the later `ChatsForUserWithLast`)
must return the other participant's username for direct messages in the same
query, with a join such as:

```sql
LEFT JOIN members other ON chats.is_dm
  AND other.chat_id = chats.id AND other.user_id != @user_id
LEFT JOIN users other_user ON other_user.id = other.user_id
```

**Sidebar.** One list holding both kinds, ordered as before by
`last_message_at`. Each row gets a class `dm` or `group` and a small leading
icon (person or people) so they can be told apart. Add a "New message" icon
link to `/dm` in the sidebar header, visible to everyone. Update the empty
state: everyone can at least start a direct message.

**`GET /dm`** (`accUser`): back link to `/`, title "New message", the list of
all other users by username. Each row is a form posting to `POST /dm` with a
hidden `user` id and the username as the button.

**`POST /dm`** (`accUser`): `user` must exist and differ from the current
user, else 404. In one transaction: look up the chat by `dm_key`; if missing,
create it (`created_by` = current user, `last_message_at` = now) and add both
members (`added_by` = current user). A unique violation on `dm_key` means a
concurrent request created it: load it. Redirect to `/c/{id}`. The new chat
appears in the other user's list immediately; call `a.notifyChat` once task
011 exists (leave a comment for it, or add the call if 011 is already done).

**Info page** for a direct message: shows the other user's name and nothing
to manage. The rename, add, remove and join controls are not rendered, and
their endpoints answer 404 for a direct message.

**`/new`** (create group) is unchanged and still needs the group permission.

Queries: `ChatByDMKey`, `ChatCreateDM`, update `ChatsForUser`.

## Tests

Add `ta.newDM(a, b)` to the harness.

- Starting: creates one chat with two members; starting again from either
  side returns the same chat; with yourself is 404; with a missing user is
  404; signed out is 401.
- 20 concurrent `POST /dm` for the same pair create exactly one chat.
- A user with no permissions at all can start one.
- Access: both participants 200; a third user 404; an admin who is not a
  participant 404 on the page, the info page and (after later tasks) every
  other chat route.
- Rename, member add and member remove are 404 on a direct message, for a
  participant and for an admin.
- The sidebar shows the other user's username for each viewer (A sees "b", B
  sees "a"), marks the row `dm`, and mixes direct messages and groups in one
  list ordered by `last_message_at`.
- The pane header and page title use the other user's username.

## Done when

- Tests above pass and `./.hooks/pre-push` passes.
