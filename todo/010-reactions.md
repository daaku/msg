# 010 Emoji reactions

Let members add and remove emoji reactions on messages. Pure HTML forms plus
the existing `data-morph` enhancement, with one added line of JavaScript.
Code goes in `chat.go`.

## Behaviour

**Palette.** A fixed list in Go, in this order:

```
👍 ❤️ 😂 😮 😢 🙏 🎉 🔥 👏 😍 🤔 👀 💯 ✅ ❌ 😡
```

Only these values are accepted. Anything else is `httpErr(400)`.

**`POST /c/{id}/react`** (member): form fields `message` (id) and `emoji`.
The message must belong to that chat, otherwise 404. Toggle: delete the row
for (message, user, emoji); if nothing was deleted, insert it. Then
`a.notifyChat` and redirect to `/c/{id}`. Reactions do not create push
notifications and do not touch `last_message_at`.

**Rendering.** Inside each `article#m{id}`, after the body:

```html
<div id="m123-reactions" class="reactions">
  <form method="post" action="/c/5/react" data-morph>
    <input type="hidden" name="message" value="123">
    <button id="m123-r-1f44d" name="emoji" value="👍" class="chip on"
      title="alice, bob">👍 <span>2</span></button>
    ...one button per emoji that has at least one reaction...
  </form>
  <button class="chip add" popovertarget="m123-pick" aria-label="Add reaction">icon</button>
  <div id="m123-pick" popover class="picker">
    <form method="post" action="/c/5/react" data-morph>
      <input type="hidden" name="message" value="123">
      ...one button per palette emoji, name="emoji"...
    </form>
  </div>
</div>
```

- Chips are ordered by palette order. `on` marks emoji the viewer has used.
  `title` lists the usernames, in the order they reacted.
- The chip id suffix is the emoji's code points in hex joined with `-`.
- The picker uses the HTML `popover` attribute: it opens and closes with no
  JavaScript, and because its open state is not an attribute it survives a
  morph. Style it as a sheet fixed to the bottom centre of the viewport with
  large touch targets (at least 44px). A submit button cannot also carry
  `popovertarget` (a button with a form owner only submits), so the sheet
  would stay open after a choice. Close it in the existing `data-morph` form
  handler from task 009: after a successful submit, call
  `form.closest('[popover]')?.hidePopover()`. That one line is the only
  JavaScript change in this task.
- Viewers who are not members (admin looking in) see the chips as plain
  spans, with no forms and no add button.
- Load all reactions for the rendered messages with one query using
  `sqlc.slice`, then group in Go. Do not query per message.

Queries: `ReactionDelete` (`:execrows`), `ReactionAdd`,
`ReactionsForMessages` (joined with usernames, ordered by `created_at`,
`user_id`), `MessageInChat`.

## Tests

- Toggle on then off; two users on the same emoji give count 2; counts and
  `on` state render correctly for each viewer; `title` has both usernames.
- Emoji outside the palette is 400. A message id from another chat is 404.
  Non member is 404. Signed out is 401.
- A reaction pings the group (subscribe to the hub in the test and assert a
  ping).
- With 100 messages rendered, the handler runs a constant number of queries
  (count them with a wrapper around the `DBTX`, or assert the code path uses
  the slice query once).
- Markup contract: `m<id>-reactions`, `m<id>-pick` present and unique.

## Done when

- In a browser: adding and removing a reaction updates both members' views
  live, the picker works on a phone sized viewport, and an incoming message
  while the picker is open does not close it. State if not checked.
- Tests above pass and `./.hooks/pre-push` passes.
