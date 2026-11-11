# msg

A small, self-hosted, web based group chat application written in Go. The
product requirements live in `todo.md`. The work is split into numbered task
files in `todo/`. This file is the durable design and convention reference:
read all of it before starting any task.

## Working on a task

- Tasks are `todo/NNN-name.md`. Do them in numeric order, one at a time. Each
  task assumes every lower numbered task is finished. If a lower numbered task
  file still exists, stop and say so.
- A task whose second paragraph is `Use fable.` is security critical or subtle.
  Hand it to the most capable model available. Do not attempt it with a small
  model.
- Read the task file fully, then read the code it touches before writing.
- Stay inside the task's scope. `todo.md` says "No unspecified features" and
  that is a hard rule. If something seems missing, write it under a
  `## Follow-ups` heading at the bottom of the next task file that fits, and do
  not build it.
- A task is done when every item under its "Done when" heading is true and
  `./.hooks/pre-push` passes. Run that script, do not assume.
- When done: delete the task file and commit everything as
  `NNN: short lowercase summary`. Do not push.
- If the task's instructions conflict with what you find in the code, trust
  this file first, then the task file, and mention the conflict in your final
  report.
- Report honestly. If a "Done when" item could not be verified (for example no
  real S3 bucket or no iPhone at hand) say exactly which one.

## Hard rules from the spec

- Go, single binary, CGO free (`CGO_ENABLED=0 go build` must work).
- SQLite through `modernc.org/sqlite`. No other datastore.
- Passkeys are the only way to authenticate. No passwords, no email, no phone
  numbers, no contact information of any kind anywhere in the schema or UI.
- Minimal JavaScript. Everything is server rendered HTML and CSS. JavaScript is
  allowed only for the items listed under "JavaScript inventory". Adding
  anything to that list needs the user's approval.
- No read receipts, no online presence, no typing indicators, no user status.
- No data encryption at rest and no end to end encryption.
- Prefer few, large files. Do not create new Go packages. Do not split a file
  because it is long.
- Not in scope, do not build: replies or threads, mentions, markdown or rich
  text, search, avatars or display names, unread counters, mute, edit or
  delete history, user deletion, rate limiting, audio messages, paste or drag
  and drop upload.
- Everything personal to a user (passkeys, add passkey links, notifications,
  stats, sign out) lives on the single `/settings` page. Do not add another
  personal page.

## Terminology

A `chat` (`chats`, `chat_id`, `/c/{id}`) is a conversation. It is either a
group (named, any number of members, created by someone with the group
permission) or a direct message (`is_dm`, exactly two members, no name,
anyone can start one). The UI says "group" and "direct message"; the code
says `chat` for both, and all features work on both unless stated. The one
difference in access: admins can view and manage every group, but a direct
message is visible only to its two participants.

## Stack

| Concern | Choice |
| --- | --- |
| HTTP | `net/http` `ServeMux` with method patterns, `github.com/daaku/lands` to listen |
| HTML | `maragu.dev/gomponents` (`g` and `h` import aliases) |
| DB | `modernc.org/sqlite`, `github.com/daaku/sqlmig` migrations, `sqlc` generated queries |
| Errors | `github.com/daaku/serr` for stack traces |
| Cookies | `github.com/daaku/sookie` (encrypted) |
| Passkeys | `github.com/go-webauthn/webauthn` |
| Push | `github.com/daaku/webpush` |
| Assets | `embed` plus `github.com/daaku/hashfs` |
| Morphing | vendored `assets/paxi.js` (https://github.com/bigskysoftware/paxi) |
| Tests | standard `testing`, `github.com/daaku/ensure`, `github.com/descope/virtualwebauthn` |
| Media | exec `magick` (ImageMagick 7) and `ffmpeg` / `ffprobe` |
| Storage | S3 compatible bucket through our own small SigV4 client in `s3.go` |

`~/workspace/bento` is the reference project for style: handler shape, cookie
handling, passkey and push code, CSS tokens, git hooks and CI. Read the
matching part of bento before writing the equivalent here. Differences from
bento are deliberate and listed in this file (single package, no email, no
mocks, sessions bound to a passkey, CSP).

## File layout

Everything is `package main` in the repository root.

```
main.go        config, app struct, routing, middleware, helpers, run()
ui.go          page shell, shared components, icons, formatting helpers
auth.go        sessions, passkeys, invites, bootstrap, settings page, admin user pages
chat.go        groups, direct messages, members, archiving, messages, edit and
               delete, reactions, SSE hub, events
s3.go          SigV4 presigning S3 client and the built in fake S3 used in dev and tests
media.go       upload endpoints, /f/ redirects, conversion worker, link previews
push.go        push subscriptions, sending, opened tracking, snooze reminders
stats.go       stats helpers and stats pages
*_test.go      one test file per source file, plus main_test.go for the harness
db/NNN_*.sql   migrations (sqlmig)
db/query.sql   all sqlc queries
sqlc.yaml      sqlc config; output is sqlc_db.go, sqlc_models.go, query.sql.go
assets/        main.css, main.js, sw.js, paxi.js, icons, robots.txt
.hooks/        pre-commit (fast) and pre-push (full) checks
```

Generated files (`sqlc_db.go`, `sqlc_models.go`, `query.sql.go`) are committed
and never edited by hand. Regenerate with `go generate ./...`.

## Commands

```sh
go generate ./...        # regenerate sqlc code after editing db/
go build ./...
go test ./...
./.hooks/pre-push        # tidy check, generate check, build, test. Must pass.
S3_ENDPOINT=fake DEV=1 go run .   # run locally, no external services needed
```

On first start with an empty database the server prints a
`Bootstrap admin: <url>` line. Open it to create the admin account.

## Configuration

All configuration is environment variables, loaded from `.env` in development
(`github.com/joho/godotenv/autoload`). Parsed once in `main.go` into the
`config` struct. Never read `os.Getenv` anywhere else.

| Variable | Default | Meaning |
| --- | --- | --- |
| `PUBLIC_URL` | `http://localhost:8080` | External origin. Also gives the WebAuthn RP ID and origin |
| `APP_NAME` | `msg` | Page titles, manifest name, RP name |
| `STATE_DIRECTORY` | `data` | Holds `msg.db`, temp files, fake S3 objects |
| `ADMIN_USERNAME` | `admin` | Username of the bootstrap admin |
| `DEV` | unset | `1` enables livereload and verbose error pages |
| `S3_ENDPOINT` | required | `https://host` or the literal `fake` for the built in fake |
| `S3_REGION` | `us-east-1` | |
| `S3_BUCKET` | required unless fake | |
| `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | required unless fake | |
| `S3_PATH_STYLE` | `true` | `false` selects virtual hosted style URLs |
| `S3_URL_VALIDITY` | `7d` | Signed URL validity. 7 days is the most AWS S3, R2 and MinIO accept |
| `MAX_UPLOAD_SIZE` | `100MB` | Largest accepted upload (1MB is 1048576 bytes) |
| `IMAGE_FORMAT` | `webp` | `webp`, `avif` or `jpeg` |
| `IMAGE_RESOLUTION` | `4k` | Long edge cap. See table below |
| `VIDEO_FORMAT` | `h264` | `h264` or `av1`, always in an MP4 container with AAC audio |
| `VIDEO_RESOLUTION` | `1080p` | Long edge cap |
| `ADDR`, `PORT`, `LISTEN_FDS`, `TLS_CERT_FILE`, `TLS_KEY_FILE` | | Handled by `lands` |

Resolution names map to the long edge in pixels: `4k` 3840, `1440p` 2560,
`1080p` 1920, `720p` 1280, `480p` 854. Media is never upscaled.

The cookie master secret and the VAPID key are not configuration. They are
generated on first start and stored in the `config` table.

## Architecture

### Request handling

- Handlers are methods on `*app` with the signature
  `func (a *app) name(w http.ResponseWriter, r *http.Request) error`.
- Routes are registered in `run()` in `main.go` through
  `a.wrap(access, handler)`. Path strings are `p...` constants at the top of
  `main.go`. `access` is one of `accPublic`, `accUser`, `accInvite`,
  `accGroup`, `accAdmin`. Admin passes every check.
- `wrap` loads the session, puts the `*User` in the context (`userFrom(ctx)`),
  enforces the access level, and turns a returned error into a response. Return
  `httpErr(code, "message")` for expected failures. Any other error is logged
  with `%+v` and shown as a generic 500 (with the stack only when `DEV=1`).
- A resource the user may not see is answered with 404, not 403, so its
  existence is not revealed.
- Every state change is a `POST` from an HTML form followed by a 303 redirect
  (post, redirect, get). Feedback uses the flash cookie as in bento.
- CSRF protection is `http.NewCrossOriginProtection()` wrapping the whole mux.
  Never add a state changing `GET`.
- Wrap errors that cross a function boundary with `serr.Wrap(err)`.

### Database

- All timestamps are `INTEGER` unix seconds and are always passed in from Go
  using `a.now()`. Never use SQL time functions or column defaults for time.
  Tests replace `a.now`.
- Queries live in `db/query.sql`, named `ThingVerb` (`UserByID`,
  `MessageCreate`). Use `@name` parameters when a query has more than two.
  `sqlc.slice('ids')` for `IN` lists.
- Use `a.tx(ctx, func(q *Queries) error { ... })` when more than one statement
  must succeed together. The DSN sets `_txlock=immediate`.
- A schema change is a new `db/NNN_name.sql` file. Never edit a migration that
  has been committed in an earlier task.
- Status columns are plain text. `files.status`: `uploading`, `pending`,
  `processing`, `ready`, `failed`. `files.kind`: `image`, `video`, `other`,
  `preview`. `previews.status`: `pending`, `ready`, `failed`.

### Authentication and permissions

- A user has a `username` (lowercase, `^[a-z0-9][a-z0-9_.-]{1,31}$`) and a
  random 16 byte `handle` used as the WebAuthn user handle.
- Sign in uses discoverable credentials, so no username is typed.
- The session cookie holds `{UserID, CredentialID, IssuedAt}`. Each request
  loads the user with a join on `credentials`, so deleting a passkey ends the
  sessions that were created with it.
- Rows in `invites` are single use sign up URLs. A row with `user_id` set is
  an "add passkey" link for an existing user (used for a second device and for
  admin recovery). A row is deleted when it is used or revoked.
- Permissions are three booleans on `users`: `perm_admin`, `perm_invite`,
  `perm_group`. Always check them through `a.wrap` access levels or the
  helpers in `auth.go`, never by reading the columns in a handler.
- Group access goes through `a.chatAccess(ctx, user, chatID)`, which returns
  the chat, whether the user is a member, and whether they may manage it.
  Groups: view is member or admin; post, react, upload, archive, snooze are
  member only; manage (rename, add and remove members) is admin, or the
  creator while they hold `perm_group`. Direct messages: the two members
  only, for everything, and nothing to manage.
- A message is edited by its author only. It is deleted by its author, or by
  an admin who can view the chat.
- Archive state (`members.archived`) and snoozes are private to the user.
  Never render them for, or reveal them to, anyone else.

### Live updates and the morph contract

- `GET /events` is a Server-Sent Events stream. It carries no data, only a
  ping that means "something you can see changed".
- On a ping the browser fetches the current URL again as a normal full page,
  takes `#app` out of the response and morphs the live `#app` towards it with
  paxi. There are no partial renders and no JSON view endpoints. A page is
  correct if a plain reload shows the right thing.
- Forms marked `data-morph` are submitted with `fetch` and followed by the
  same refresh. Without JavaScript they still work as ordinary forms.
- After any change that other people can see, call `a.notifyChat(ctx, chatID)`.
- paxi matches elements by `id` among siblings. So: every element that is
  repeated in a list, or that holds state (inputs, `details`, video), must
  have a stable, unique `id` derived from its database id (`m123`,
  `m123-reactions`, `chat-5`). Never derive an id from a list position.
- An element that comes and goes in front of a list (like `#earlier`) is
  always rendered and given `hidden` when not needed. If it vanished, paxi
  would move every following sibling, which loses scroll anchoring and
  restarts playing video.
- Markup must be a pure function of database state and the URL. Do not render
  random values, the current time, or per request nonces inside `#app`,
  because every difference becomes a DOM mutation on every refresh.
- Media URLs in markup are always `/f/{fileID}/{variant}`, never signed S3
  URLs, so that they do not change between renders.

### Media

- Browser to S3 directly: uploads are S3 multipart uploads with presigned part
  URLs, downloads are a 302 from `/f/{id}/{variant}` to a presigned GET.
  Variants: `o` original, `c` converted, `p` poster.
- Object keys are `<files.s3_prefix>/original`, `/converted`, `/poster`.
  Content type, a one year `Cache-Control` and `Content-Disposition` are stored
  as object metadata when the object is created.
- Presigned GET URLs are quantised in time so the same object gives the same
  URL for a long window. This is what lets the browser cache work.
- The original is always kept. Images and videos are converted by a background
  worker and the converted object is what the chat shows.
- "Bytes downloaded" is counted in `/f/`: each redirect adds the size of that
  variant. It is an estimate, because the bytes themselves never pass through
  this server.

### Hooks other tasks extend

- `a.afterMessage(ctx, chat, msg)` in `chat.go` runs after a message row is
  committed. It is the only place that fans out side effects of a new message
  (live ping, link preview, push). Add to it, do not call those from handlers.
- `a.afterEdit(ctx, chat, msg)` is the same for an edited message (live ping,
  link preview refresh).
- `a.deleteMessage(ctx, chat, msg)` is the only place a message is removed.
  It also removes the attached file, the preview image and their S3 objects.
  Deletes are hard deletes; nothing is kept.
- `a.messageMenu(...)` builds the per message popover (`m{id}-menu`). New
  per message actions are added there, not as new controls on the bubble.
- `a.pushTo(userID, payload)` is the only way a push is sent.
- `a.bg` is a `sync.WaitGroup` and `a.ctx` is cancelled on shutdown. Every
  background goroutine must be started with `a.bg.Go` and must return when
  `a.ctx` is done. Long lived handlers (SSE) must also watch `a.ctx`.
- Stats are only ever changed through `a.statUser` and `a.statChat`.

## JavaScript inventory

`assets/main.js` may contain only these, each as a small block driven by a
`data-*` attribute. No frameworks, no build step, no inline scripts, no inline
event handler attributes (the CSP forbids them).

1. Passkey create and get (`navigator.credentials`).
2. Service worker registration and push subscribe and unsubscribe.
3. `refresh()`: fetch the current URL and morph `#app`.
4. The `EventSource` for `/events`, closed while the tab is hidden.
5. `data-morph` form and link enhancement (including closing the popover a
   submitted form sits in).
6. Chunked upload to S3 with the progress ring.
7. Enter to send in the composer on devices with a fine pointer.
8. Setting the `tz` cookie from the browser time zone.
9. Select all on focus for copyable link fields.

`assets/sw.js` handles `push` and `notificationclick` only. It does no
caching and has no `fetch` handler.

## HTML and CSS conventions

- Build markup with gomponents. Never pass user supplied text to `g.Raw`.
- The CSP is `default-src 'self'` plus the S3 origin for images, media and
  connect. No inline `style` attributes and no inline scripts. Use classes.
- One stylesheet, `assets/main.css`. Design tokens are custom properties on
  `:root`. Support light and dark with `prefers-color-scheme`.
- Mobile first. One breakpoint at 48rem switches the two pane layout on.
- Every `<img>` in a chat has `loading="lazy"`, `decoding="async"`, `width`
  and `height`.
- A chat's name is always rendered through `a.chatTitle`, which gives the
  other person's username for a direct message.
- Every page other than home has a visible back link, because an installed iOS
  app has no browser back button.
- Use real controls: `<button>` inside `<form>`, `<a>` for navigation, the
  `popover` attribute for pickers, `<details>` for disclosure.

## Security rules

- Check authorisation in every handler through the helpers. Tests for a new
  route must include a signed out request, a non member, and a member.
- Any request to a URL that came from a user (link previews, and push
  endpoints, which come from the user's browser) goes through
  `a.safeTransport()` in `media.go`, which only connects to public addresses.
  Never use `http.DefaultClient` or a plain `http.Client` for such a URL.
- Log a request by its route pattern (`logRoute`), never its path: paths hold
  invite tokens. Strip URLs from HTTP client errors with `withoutURL` before
  logging them.
- Never let ImageMagick or ffmpeg pick the input format. Sniff the bytes in Go
  and pass an explicit format (`jpeg:path`, `-f mov`). Always pass arguments as
  an argv slice, never through a shell. Always use a timeout.
- Uploaded originals are only ever served with
  `Content-Type: application/octet-stream` and `Content-Disposition:
  attachment`.
- Sizes claimed by a client are verified against S3 before they are trusted.
- Never log invite tokens other than the bootstrap URL, and never log cookies
  or push subscription keys.

## Testing

- No mocks of the database. `newTestApp(t)` in `main_test.go` builds a real
  `*app` on a temporary SQLite file, the fake S3, a fake push sender and a
  fixed clock. Helpers there create users, sessions, groups and messages.
- Test through HTTP: build a request, call the app's handler, assert on the
  status, the redirect, and the database. Assert on HTML with substring checks
  on ids and text, not on whole documents.
- Passkey flows are tested with `virtualwebauthn` (see bento's
  `auth/auth_test.go`).
- Tests that need `magick` or `ffmpeg` call `t.Skip` when the binary is
  missing. They must run when it is present.
- Every bug fixed gets a test that fails without the fix.
