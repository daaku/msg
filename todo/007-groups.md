# 007 Groups and membership

Create groups, manage their members, and show the list of groups on the home
page inside the two pane layout that the chat will live in. Code goes in
`chat.go` and `ui.go`. Remember: the UI says "group", the code says `chat`.

## Layout

`a.chatPage(w, r, user, active *Chat, pane g.Node)` in `chat.go` renders the
shared shell for `/` and every `/c/...` page:

```html
<main id="app" data-chat="5">            <!-- data-chat absent on home -->
  <nav id="sidebar"> header + group list </nav>
  <section id="pane"> ... </section>
</main>
```

- Narrow screens show one pane at a time: on `/` only `#sidebar`, on a group
  page only `#pane`. From 48rem up both are visible side by side, the sidebar
  about 20rem wide. Do this in CSS with a class on `#app` (`has-chat`), no
  JavaScript.
- Sidebar header: the app name, then icon links (each only when permitted):
  new group (`/new`), invites, admin, account.
- Group list: one `<a id="chat-{id}" class="chat-item" href="/c/{id}">` per
  group the user is a member of, ordered by `last_message_at` descending then
  id descending. Each shows the group name and, after task 008, a one line
  snippet and time of the latest message. The active group has
  `aria-current="page"`.
- With no groups: a short empty state. If the user may create groups it links
  to `/new`, otherwise it says someone needs to add them to a group.
- On `/` from 48rem up, `#pane` shows a quiet "Select a group" placeholder.

## Access helper

`a.chatAccess(ctx, user *User, chatID int64) (chat Chat, member bool, manage
bool, err error)`:

- chat missing: `httpErr(404)`.
- `member`: a row in `members`.
- neither member nor admin: `httpErr(404)`.
- `manage`: admin, or `chat.created_by == user.id && user.PermGroup`.

Every `/c/{id}...` handler calls this first. Write small wrappers
`a.chatView`, `a.chatMember` (404 unless member), `a.chatManage` (404 unless
manage) so handlers state their requirement in one line.

## Routes

- `GET /new` (`accGroup`): form with a `name` field and a checkbox list of all
  other users (by username). Back link to `/`.
- `POST /new` (`accGroup`): name is trimmed, 1 to 64 characters. In one
  transaction create the chat (`last_message_at` = now), add the creator and
  every selected existing user as members (`added_by` = creator; ignore ids
  that do not exist). Redirect to `/c/{id}`.
- `GET /c/{id}` (view): for now `#pane` holds a header (back link to `/`
  visible only on narrow screens, the group name linking to the info page)
  and an empty message area. Task 008 fills it in.
- `GET /c/{id}/info` (view): back link to `/c/{id}`. Shows the name, the
  members (username, and a "Remove" button for managers), and for managers a
  rename form and an "Add members" form with checkboxes for users who are not
  members. Task 019 adds stats here.
- `POST /c/{id}/rename` (manage).
- `POST /c/{id}/members/add` (manage): adds the selected existing users,
  ignoring ones already present.
- `POST /c/{id}/members/remove` (manage): removes one user. The creator
  cannot be removed. An admin viewing a group they are not a member of sees a
  "Join" button that adds themselves through the add endpoint.

All three POSTs redirect back to the info page.

Queries: `ChatCreate`, `ChatByID`, `ChatsForUser`, `ChatRename`, `MemberAdd`
(insert or ignore), `MemberRemove`, `MemberGet`, `MembersOfChat` (with
usernames, ordered by username), `MemberIDs`, `UsersNotInChat`,
`UsersAllBasic`.

## Tests

Add `ta.newChat(creator, name, members...)` to the harness.

- Create: needs the permission; creator and selected users become members;
  empty and overlong names are refused; an unknown user id is ignored.
- Home lists only the user's groups, ordered by `last_message_at`.
- `/c/{id}` and `/c/{id}/info`: member 200, non member 404, admin non member
  200, missing id 404, non numeric id 404.
- Manage endpoints: creator with permission can; creator whose `perm_group`
  was removed cannot; plain member cannot; admin can.
- Add is idempotent. Removing the creator is refused. A removed member gets
  404 on the group afterwards.
- Markup contract: home contains `id="app"`, `id="sidebar"`, `id="pane"` and
  `id="chat-<id>"` for each group; a group page has `data-chat="<id>"`.

## Done when

- The layout switches between one and two panes at 48rem with no JavaScript.
  Check it in a browser at 360px and 1280px wide, or state that you could not.
- Tests above pass and `./.hooks/pre-push` passes.
