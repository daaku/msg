# 020 Web Push notifications

Let a user enable notifications on each of their devices, send a push to the
other members when a message arrives, and count pushes sent and opened. Code
goes in `push.go`, the service worker in `assets/sw.js`.

Read bento's `push/push.go`, `assets/sw.js` and the push parts of
`assets/main.js` first; this task adapts them. The library is
`github.com/daaku/webpush` (`go doc github.com/daaku/webpush`).

## Setup

- VAPID key: on first start call `webpush.GenerateVAPIDKey()` and store the
  string in the `config` table under `vapid` with `ConfigSetDefault`, then
  read it back and `webpush.ParseVAPIDKey`. Same pattern as the cookie
  secret.
- A `pushSender` interface with the `Send(ctx, message []byte, s
  *webpush.Subscription) error` method. The real one is a `webpush.Client`
  with `Subscriber: PUBLIC_URL`, `TTL: 24 * time.Hour`,
  `Urgency: webpush.UrgencyHigh`, and an `http.Client` with a 30 second
  timeout. `newTestApp` installs a fake that records calls and can be told to
  fail.
- Serve `GET /sw.js` from the embedded file with `Cache-Control: no-cache`.
  It must be at the root path, not under `/assets/`, to control the whole
  origin. Register it from `main.js` on every page for signed in users
  (`data-sw` on `<body>` or a `<meta>`, as bento does).

## Subscribing

On `/settings`, a "Notifications" section:

- A form posting to `POST /push/subscribe` with a hidden `json` input and a
  button "Enable notifications on this device" carrying
  `data-push-subscribe="<VAPID public key, base64url>"`. JavaScript (take
  bento's block, including its handling of a stale subscription with a
  different key) calls `pushManager.subscribe`, puts the JSON in the input
  and submits.
- The user's subscriptions: label (browser on OS, from the user agent at
  subscribe time), created date, and a "Remove" button posting to
  `POST /push/unsubscribe` with the row id.
- CSS classes on `<body>` set by the script (`push-granted`, `push-denied`,
  `push-prompt`, `push-unavailable`, as bento does) decide which of these
  hints is visible: denied says to allow notifications in the browser's site
  settings; unavailable says that on an iPhone or iPad the app must first be
  added to the home screen (task 022 covers installing).

Also show a one line prompt linking to `/settings#notifications` at the top
of the sidebar when the body has `push-prompt`. It is plain CSS driven by the
body class; no extra script.

`POST /push/subscribe` (`accUser`): parse the JSON into
`webpush.Subscription`; require an `https` endpoint and non empty keys;
upsert on `endpoint` (a device that re-subscribes, or a different user on the
same browser, takes over the row: update `user_id`, `subscription`, `label`).
Redirect to `/settings`.

`POST /push/unsubscribe` (`accUser`): delete where id and `user_id` match.

## Sending

In `afterMessage`, after the live ping, call `a.pushMessage(chat, msg)` which
does not block the request:

1. Recipients: `MemberIDs(chat)` minus the author, minus anyone for whom
   `a.hub.viewing(userID, chat.ID)` is true (they are looking at the group
   right now).
2. Payload JSON, at most about 2 KiB:

   ```json
   {"title": "<group name>", "body": "<username>: <snippet>",
    "tag": "chat-<id>", "data": {"url": "/c/<id>"}}
   ```

   For a direct message the title is the sender's username and the body is
   just the snippet.

   Snippet: the first 120 runes of the body; "📷 Photo", "🎬 Video" or
   "📎 <name>" for files.
3. For each subscription of each recipient, in a goroutine started with
   `a.bg.Go`, limited to 8 concurrent sends by a semaphore channel, with a
   context derived from `a.ctx` and a 30 second timeout: `Send`.
   - Success: `a.statUser` adds 1 to `pushes_sent` for the recipient.
   - `*webpush.Error` with `Permanent` true, or a subscription that fails to
     parse: delete the row.
   - Other errors: log one line with the endpoint host only. Do not retry.

Reactions, edits, deletes, membership changes and conversions do not send
pushes. Put the per subscription send, with its concurrency limit, error
handling and counting, in one function `a.pushTo(userID int64, payload
[]byte)`; task 021 reuses it for reminders.

## Service worker (`assets/sw.js`)

- `push`: parse the JSON; `showNotification(title, {body, tag, data})`.
  Always show one: browsers require a visible notification for each push.
- `notificationclick`: close it; `fetch('/push/opened', {method: 'POST',
  keepalive: true})` inside `waitUntil`; then find a window client of this
  origin: if one exists `focus()` it and `navigate(url)`, otherwise
  `clients.openWindow(url)`.
- `install`: `skipWaiting()`. `activate`: `clients.claim()`.
- No `fetch` handler and no caching.

`POST /push/opened` (`accUser`): add 1 to the user's `pushes_opened`. Answer
204.

Queries: `PushSubUpsert`, `PushSubsByUser`, `PushSubsByUsers` (slice),
`PushSubDelete`, `PushSubDeleteByID`.

## Tests (`push_test.go`)

- The VAPID key is created once and is stable across restarts.
- Subscribe stores a row; the same endpoint again updates instead of adding;
  a second user subscribing with the same endpoint takes it over; an `http`
  endpoint or missing keys is 400; unsubscribe deletes only own rows.
- A message in a three member group sends to the two other members' devices
  (a member with two subscriptions gets two sends) and never to the author.
  `pushes_sent` rises per successful send.
- A member with an open `/events?chat=<id>` stream gets no push; with a
  stream for a different chat, or none, they do.
- A permanent error deletes that subscription and leaves others; a transient
  error keeps it; neither counts as sent.
- Direct message payload: title is the sender's username, body is the
  snippet without a prefix.
- Payload: title is the group name, body has the username and snippet, long
  bodies are cut, file messages use the file wording, `data.url` is the
  group URL.
- `POST /push/opened` increments `pushes_opened`; signed out is 401.
- `/sw.js` is served at the root with `no-cache` and a JavaScript content
  type.
- Shutdown waits for in flight sends (`a.bg.Wait` returns after the fake
  sender unblocks).

## Done when

- On a desktop browser against `DEV=1` on localhost: enable notifications,
  send a message from a second profile while the first has the tab hidden,
  the notification appears, clicking it focuses the app on that group, and
  the settings page stats later show one sent and one opened.
- Report which browsers and devices were tried. Push on iOS needs the
  installed app from task 022 and a real HTTPS origin; say if that was not
  available.
- `./.hooks/pre-push` passes.
