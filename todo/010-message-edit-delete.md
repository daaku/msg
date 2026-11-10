# 010 Editing and deleting messages

Let people edit and delete their own messages. No history is kept: an edit
overwrites the text and a delete removes the row. This task also introduces
the per message menu that reactions (012) and snooze (021) add to. Code goes
in `chat.go`. No JavaScript.

## Rules

- Edit: the author only, text messages only (a message with a file has no
  text to edit), and only while they are still a member of the chat.
- Delete: the author, or an admin who can view the chat. Admins cannot see
  direct messages they are not part of, so they cannot delete in them.
- An edited message shows a small "edited" label next to its time. A deleted
  message is simply gone; there is no placeholder.
- Counters in `user_stats` are not reduced by a delete. They count what was
  sent.
- Neither action sends a push notification.

## Message menu

Every message the viewer can act on gets a menu button and a popover:

```html
<article id="m123" ...>
  ...
  <button id="m123-more" class="msg-more" popovertarget="m123-menu"
    aria-label="Message actions">icon</button>
  <div id="m123-menu" popover class="sheet">
    <!-- 012 adds the emoji row here, 021 adds the snooze choices here -->
    <a id="m123-edit" href="/c/5/m/123/edit">Edit</a>
    <details id="m123-del">
      <summary>Delete</summary>
      <form method="post" action="/c/5/m/123/delete" data-morph>
        <button class="btn danger">Delete this message</button>
      </form>
    </details>
  </div>
</article>
```

- The popover is a sheet fixed to the bottom centre of the viewport, with
  touch targets of at least 44px. It opens and closes without JavaScript.
- The `<details>` is the confirmation step: Delete first reveals the real
  button. Inline `confirm()` handlers are not allowed by the CSP.
- Render only the entries the viewer may use. If there are none, render
  neither the button nor the popover. (After task 012 every member has at
  least the emoji row.)
- On pointer devices the button is visible on hover and focus; on touch
  devices it is always visible but quiet.

Build the menu with one function `a.messageMenu(view, msg) g.Node` that
collects entries, so later tasks append to it.

## Routes

- `GET /c/{id}/m/{mid}/edit` (author, member): a simple page inside the chat
  shell with a back link to `/c/{id}`, a textarea prefilled with the body
  (same limits as the composer) and a Save button posting to the same path.
- `POST /c/{id}/m/{mid}/edit`: same validation as sending. An empty body is
  refused with a flash (delete is the way to remove a message). Update `body`
  and set `edited_at = now` only when the text actually changed. Then
  `a.afterEdit(ctx, chat, msg)` and redirect to `/c/{id}`.
- `POST /c/{id}/m/{mid}/delete`: `a.deleteMessage(ctx, chat, msg)` and
  redirect to `/c/{id}`.

Every route checks that the message belongs to the chat in the path (404
otherwise).

**`a.deleteMessage(ctx, chat, msg)`** is the single place a message is
removed. For now it deletes the row; reactions cascade. It is extended by
later tasks: 015 removes the attached file and its S3 objects, 019 removes
the preview and its image, 021 relies on the cascade for snoozes. After the
delete, ping the chat (a comment until task 011 exists).

**`a.afterEdit(ctx, chat, msg)`** is the single hook for edit side effects:
ping the chat (011) and refresh the link preview (019). Create it now with
comments for both.

`chats.last_message_at` is not changed by an edit or a delete. The sidebar
snippet always shows the newest message that still exists.

Rendering: add `<span id="m123-edited" class="msg-edited">edited</span>`
before the time when `edited_at > 0`.

Queries: `MessageByID`, `MessageUpdateBody`, `MessageDelete`.

## Tests

- Edit: author changes the text, `edited_at` is set and the label renders;
  saving unchanged text leaves `edited_at` at 0; another member is 404; an
  admin is 404; an empty body is refused; over the limit is 400; HTML in the
  new body is escaped.
- A former member cannot edit or delete their old messages (404).
- Delete: author removes the row; another member is 404; an admin can delete
  in a group, including one they are not a member of; an admin cannot delete
  in a direct message they are not part of.
- A message id from another chat in the path is 404 for all three routes.
- After deleting the newest message, the sidebar snippet shows the previous
  one. After deleting the only message, the snippet is empty.
- `messages_sent` is unchanged by edit and delete.
- Menu rendering: the author sees Edit and Delete on their own text message;
  another member sees no menu on it (until 012); an admin sees Delete only.
  Ids `m<id>-more` and `m<id>-menu` are present and unique.
- Signed out requests are redirected or 401.

## Done when

- Tests above pass and `./.hooks/pre-push` passes.
