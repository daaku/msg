# 011 Live updates: SSE ping, full page fetch, paxi morph

Use fable.

Make pages update themselves. The server sends a contentless ping over
Server-Sent Events; the browser answers by fetching its current URL as a full
page and morphing `#app`. Forms and links marked `data-morph` use the same
refresh. Read "Live updates and the morph contract" in `agents.md` first.

## Server (`chat.go`)

**Hub.** In memory, single process:

```go
type hubConn struct {
    ch     chan struct{} // buffered, size 1
    chatID int64         // the group this tab is showing, 0 if none
}
type hub struct {
    mu    sync.Mutex
    conns map[int64]map[*hubConn]struct{} // by user id
}
func (h *hub) subscribe(userID, chatID int64) (c *hubConn, cancel func())
func (h *hub) notifyUsers(userIDs ...int64) // non blocking send to each conn
func (h *hub) notifyViewers(chatID int64)   // conns with that chatID, any user
func (h *hub) viewing(userID, chatID int64) bool
```

A send to a full channel is dropped: a ping is already pending, and one
refresh shows everything. `cancel` removes the conn and deletes empty maps.
`viewing` is used by task 020 to skip push for people looking at the group.

**`a.notifyChat(ctx, chatID)`**: load `MemberIDs`, `notifyUsers` them, and
`notifyViewers(chatID)` (covers an admin watching a group they are not a
member of). Errors are logged, not returned.

**`GET /events`** (`accUser`): optional `chat` query parameter. If present it
must pass `a.chatAccess` view, otherwise 404. Then:

- Headers: `Content-Type: text/event-stream`, `Cache-Control: no-store`,
  `X-Accel-Buffering: no`. Write `retry: 3000\n\n` and flush.
- Loop on: the conn channel (write `data: 1\n\n`, flush), a 25 second ticker
  (write `: hb\n\n`, flush), `r.Context().Done()`, and `a.ctx.Done()`. Return
  on either context or on a write error.
- Use `http.NewResponseController(w)` for `Flush` and to clear the write
  deadline.

The `a.ctx.Done()` case matters: without it, shutdown waits ten seconds for
open streams and `lands` reports an error.

**Wire the pings.** Call `a.notifyChat` from `afterMessage`, and after rename,
member add and member remove. For member remove also `notifyUsers` the removed
user, since they are no longer in `MemberIDs`. For member add, the added users
are in `MemberIDs` already. Later tasks call it after reactions, uploads,
conversions and previews.

## Client

**Vendor paxi.** Download
`https://raw.githubusercontent.com/bigskysoftware/paxi/master/paxi.js` to
`assets/paxi.js`, unmodified. The reviewed copy has sha256
`10082585e208be5c2717bdd4081053488a5df98f5a0eeccc266c9ae94b2dbee5`. If the
upstream file differs, read the diff before accepting it and note the new
hash in the commit message. Load it with `defer` before `main.js`. It defines
`window.morph(target, html)`, which morphs `target` towards the first element
parsed from `html`.

Two paxi facts that shape the code: it parses `html` in a `<template>`, where
`<html>` and `<body>` tags are dropped, so pass it the `outerHTML` of the new
`#app`, never a whole document. And it syncs attributes, so anything the
browser keeps as an attribute (for example `open` on `<details>`) is reset to
what the server sent; state kept as properties (input values, popover open
state, scroll position, video playback) survives.

**`refresh()`** in `assets/main.js`:

```js
let busy = false, again = false
async function refresh() {
  if (busy) { again = true; return }
  busy = true
  try {
    const res = await fetch(location.href, { headers: { 'X-Refresh': '1' } })
    const here = location.pathname
    if (res.redirected && new URL(res.url).pathname !== here) {
      location.href = res.url          // signed out, or moved
      return
    }
    if (res.status === 404) { location.href = '/'; return }   // lost access
    if (!res.ok) return
    const doc = new DOMParser().parseFromString(await res.text(), 'text/html')
    const next = doc.getElementById('app')
    if (!next) return
    morph(document.getElementById('app'), next.outerHTML)
    document.title = doc.title
  } catch (e) {
    console.error(e)                   // offline: the next ping retries
  } finally {
    busy = false
    if (again) { again = false; refresh() }
  }
}
```

Keep the coalescing: never run two refreshes at once, and never drop a
request that arrived while one was running.

**EventSource.** Only on pages whose `#app` has `data-live` (add that
attribute in `a.chatPage`, so home and chat pages have it and settings or
admin pages do not):

- Connect to `/events`, with `?chat=<id>` when `#app` has `data-chat`.
- `onmessage`: `refresh()`.
- `onopen`: `refresh()`, except for the very first open after page load. This
  catches up after a reconnect.
- `visibilitychange`: when hidden, close the EventSource; when visible,
  `refresh()` and connect again. Mobile browsers freeze hidden tabs, and task
  020 relies on "has an open stream for this group" meaning "is looking at
  it".
- `pagehide`: close it.

**`data-morph`.**

- Forms: intercept `submit`. Disable the submit button, `fetch(form.action,
  {method: 'POST', body: new URLSearchParams(new FormData(form, submitter)),
  redirect: 'manual'})`. A response of type `opaqueredirect` or an ok status
  is success: `form.reset()`, then `await refresh()`. Anything else: fall back
  to `form.submit()` so the server's error page is shown. Re-enable the
  button and, for `#composer`, put focus back in the textarea. Pass the
  `submitter` so a clicked button's name and value are included.
- Links: intercept `click` (plain left click only), `history.replaceState`
  to the link's href, then `refresh()`.
- Attach both with one delegated listener each on `document`, because morphed
  in elements are new nodes.

## Tests

Go (`chat_test.go`):

- Hub: subscribe then notify delivers; two notifies while unread deliver one;
  cancel removes; `viewing` true only for the matching chat; notify with no
  subscribers does not block. Run with `-race`.
- `/events`: signed out is 401 or redirect; `chat` the user cannot view is
  404; a connected client receives `data: 1` after another member sends a
  message; a user who is not a member receives nothing; the handler returns
  when the request context is cancelled and when `a.ctx` is cancelled. Use
  `httptest.NewServer` and a real client with a read deadline.
- Removed member is pinged.
- Every page rendered through `a.chatPage` has `data-live`.

Browser (do this, it is the only real check of the morph): with two browser
profiles signed in as different members of one group,

1. A message sent by one appears for the other within a second, without a
   page load (the network panel shows a fetch, not a navigation).
2. Text typed but not sent in the composer survives an incoming message.
3. Sending keeps focus in the composer and clears it.
4. Scrolled up reading older messages, an incoming message does not move the
   view. At the bottom, it stays pinned to the bottom.
5. "Show earlier messages" adds messages above without a jump.
6. Hiding the tab closes the stream (check the network panel); showing it
   refreshes.
7. Removing a member from the group sends their open tab to `/`.
8. Stop the server with Ctrl-C while a tab is open: it exits in under two
   seconds with no error. Start it again: the tab reconnects and refreshes.

Chromium is installed at `/usr/bin/chromium`. If you cannot drive a browser,
say which of these were not checked.

## Done when

- `go test -race ./...` passes.
- The browser checks pass or are explicitly reported as not run.
- `./.hooks/pre-push` passes.
