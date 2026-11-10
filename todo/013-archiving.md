# 013 Archiving chats

Let a user archive a chat (a group or a direct message) to move it out of
their main list. Archiving is personal: it is a flag on the user's `members`
row and nobody else can see it. Code goes in `chat.go`. No JavaScript.

## Rules

- An archived chat is not shown in the main chat list. It is shown on the
  archived list.
- Nothing else changes while a chat is archived: the user is still a member
  and can open it by URL, read and post.
- When someone else posts a message in a chat, it is unarchived for every
  member who had archived it, so new activity is never hidden. The author's
  own archived flag is left alone by their own message. Reactions, edits and
  deletes do not unarchive.

## Changes

- `ChatsForUser...` takes an `archived` boolean and filters on
  `members.archived`. The main list passes false.
- `GET /archived` (`accUser`): the same page shell as home, with the sidebar
  showing a back link to `/`, the heading "Archived" and the archived chats in
  the usual row format and order. It has `data-live` like home.
- At the bottom of the main chat list, when the user has at least one
  archived chat, a row `<a id="archived-link" href="/archived">Archived
  (N)</a>`. Get N in the same query or one extra count query.
- `POST /c/{id}/archive` and `POST /c/{id}/unarchive` (member): set the flag
  on the current user's `members` row and redirect to `/` (archive) or
  `/c/{id}` (unarchive). Admins viewing a group they are not a member of get
  404, there is no row to flag.
- The chat info page gets an "Archive" or "Unarchive" button for members.
  When an archived chat is open, the pane header also shows a small
  "Archived" label with an Unarchive button.
- In `a.createMessage`, inside the same transaction as the insert:
  `UPDATE members SET archived = FALSE WHERE chat_id = ? AND user_id != ?
  AND archived` (the author's id is the excluded one).
- After archive and unarchive, ping only the current user
  (`a.hub.notifyUsers(user.ID)`) so their other tabs and devices update.

Queries: `MemberSetArchived`, `MembersUnarchive`, `ArchivedCount`, update the
chat list queries.

## Tests

- Archive: the chat leaves the user's main list and appears on `/archived`;
  the other members' lists are unchanged; the `Archived (1)` link appears and
  disappears with the count.
- The archived chat still opens and accepts a message from the user who
  archived it, and stays archived for them after their own message.
- A message from another member unarchives it for everyone who had archived
  it, and it is back in the main list at the top.
- A reaction, an edit and a delete by another member do not unarchive.
- Works the same for a direct message.
- Non member 404 on both routes; admin non member 404; signed out 401.
- Unarchive returns it to the main list.

## Done when

- Tests above pass and `./.hooks/pre-push` passes.
