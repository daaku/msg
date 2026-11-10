# 008 Sending and showing messages

Render a group's messages and let members send text. Everything here is plain
server rendered HTML with a normal form post; live updating is task 009. Code
goes in `chat.go`, markup helpers may go in `ui.go`.

## Markup contract

Task 009 morphs this markup, so the ids below are part of the interface. Keep
them exactly.

```html
<section id="pane">
  <header id="pane-head"> back link, group name (links to info) </header>
  <div id="scroll" class="scroll">
    <div id="messages">
      <a id="earlier" href="/c/5?n=200" data-morph>Show earlier messages</a>
      <div id="day-2026-11-10" class="day">10 November 2026</div>
      <article id="m123" class="msg mine first">
        <div class="msg-who">alice</div>
        <div id="m123-body" class="msg-body">text with <a>links</a></div>
        <!-- later tasks add: m123-file, m123-preview, m123-reactions -->
        <time id="m123-time" datetime="2026-11-10T14:03:00Z">14:03</time>
      </article>
    </div>
  </div>
  <form id="composer" method="post" action="/c/5/send" data-morph>
    <textarea id="composer-body" name="body" rows="1" required
      maxlength="8000" placeholder="Message" autocomplete="off"></textarea>
    <button id="composer-send" type="submit" aria-label="Send">icon</button>
  </form>
</section>
```

- `.scroll` is the scrolling element and uses
  `display: flex; flex-direction: column-reverse; overflow-y: auto`. With the
  single child `#messages` in normal order, the browser keeps the view pinned
  to the newest message and keeps position when older messages are added
  above. Do not write scroll handling JavaScript.
- `mine` is set on the current user's messages (aligned to the end side).
  `first` is set when the previous message is from a different user or on a
  different day; only `first` messages of other users show `.msg-who`.
- `.msg-body` uses `white-space: pre-wrap; overflow-wrap: anywhere`.
- Give `.msg-who` one of 12 hue classes `hue-0` to `hue-11` chosen by
  `userID % 12` (no inline styles, the CSP forbids them).
- The composer is rendered only for members. An admin who is viewing without
  being a member sees a note with a link to the info page instead.

## Behaviour

`GET /c/{id}` (view):

- `n` query parameter: how many of the newest messages to render. Default
  100, minimum 1, clamp to 5000. Load with `MessagesLatest` (newest first,
  `LIMIT n`) and reverse in Go.
- Render `#earlier` only when the group has more than `n` messages; its link
  is the same URL with `n` doubled.
- Day separators and clock times use the viewer's time zone: read the `tz`
  cookie, `time.LoadLocation` it, fall back to UTC when missing or invalid.
  Import `time/tzdata` so the binary carries the zone database. Dates read
  like `10 November 2026`, times like `14:03`.
- Linkify: find `http://` and `https://` URLs in the body
  (`https?://[^\s<>"']+`, then strip trailing `.,;:!?)]}` characters), render
  them as `<a href target="_blank" rel="noopener noreferrer nofollow">`.
  Everything else is text through `g.Text`. Write this as
  `linkify(body string) []g.Node` and also export the first URL through
  `firstURL(body string) string` for task 016.
- The sidebar group list (task 007) now shows a snippet: "username: first 80
  characters" and the time of the latest message. Use one query for the list
  (`ChatsForUserWithLast`), not one per group.

`POST /c/{id}/send` (member):

- `body` has `\r\n` normalised to `\n` and surrounding whitespace trimmed.
  Empty: redirect back without creating anything. Longer than 8000 bytes:
  `httpErr(400)`.
- In one transaction: insert the message, set `chats.last_message_at`, and
  `a.statUser` adding 1 to `messages_sent`.
- After commit call `a.afterMessage(ctx, chat, msg)`. Create that function
  now with an empty body and a comment saying tasks 009, 016 and 017 add to
  it.
- Redirect 303 to `/c/{id}`.

Put the insert in a reusable function
`a.createMessage(ctx, chat Chat, userID int64, body string, fileID
sql.NullInt64) (Message, error)` which does the transaction and calls
`afterMessage`. Task 012 uses it for file messages.

JavaScript, add to `assets/main.js`:

- Item 8: if the `tz` cookie is missing or differs from
  `Intl.DateTimeFormat().resolvedOptions().timeZone`, set it (`path=/`,
  `max-age` one year, `samesite=lax`) and reload the page once.
- Item 7: on `keydown` in `#composer-body`, when the key is Enter without
  Shift and `matchMedia('(pointer: fine)')` matches and the event is not
  composing, prevent default and call `form.requestSubmit()`.

Queries: `MessageCreate`, `MessagesLatest` (joined with `users.username`),
`MessageCount`, `ChatTouch`, `ChatsForUserWithLast`.

## Tests

Add `ta.newMessage(chat, user, body)` to the harness.

- Send: member creates a row, bumps `last_message_at` and `messages_sent`;
  non member 404; admin non member 404 on send but 200 on view; empty body
  creates nothing; 8001 bytes is 400.
- Render: messages appear oldest first; ids `m<id>`, `m<id>-body`,
  `m<id>-time` present; `mine` only on own messages; `first` logic across
  users and across days.
- HTML in a body is escaped (send `<script>alert(1)</script>` and assert the
  raw tag is absent and the escaped text is present).
- `linkify` table test: plain text, one URL, URL followed by a full stop, URL
  in brackets, two URLs, `javascript:` is not linked, a URL containing quotes
  or angle brackets is cut there.
- Pagination: with 150 messages, default shows 100 and an `#earlier` link to
  `n=200`; with `n=200` all 150 show and there is no link.
- Time zone: with `tz=Asia/Dubai` the rendered clock time and the day
  separator shift accordingly; an invalid `tz` falls back to UTC.
- Sidebar snippet shows the latest message and the list reorders after a
  send.

## Done when

- In a browser, sending works with Enter on desktop, the view stays pinned to
  the newest message, and "Show earlier messages" keeps the reading position.
  State if you could not check in a browser.
- Tests above pass and `./.hooks/pre-push` passes.
