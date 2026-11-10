# 001 Bootstrap the project

Create the Go module, the application skeleton, the page shell, base styles,
the test harness and the repository tooling. After this task the server
starts, serves a styled placeholder home page, and `./.hooks/pre-push` passes.

Read `agents.md` first. Read `~/workspace/bento/main.go`, its `assets/`, its
`.hooks/` and `.github/` before writing; much of this task is adapting them.

## Steps

1. `go mod init github.com/daaku/msg`, Go 1.27. Add these dependencies (bento's
   `go.mod` has known good versions, newer is fine):
   `github.com/daaku/ensure`, `hashfs`, `lands`, `livereload`, `serr`,
   `sookie`, `github.com/joho/godotenv`, `maragu.dev/gomponents`.
2. `.gitignore`: `/.env`, `/data/`, `/msg`.
3. `main.go`:
   - `config` struct and `loadConfig() (config, error)` covering every
     variable in the table in `agents.md`. Unknown or malformed values are an
     error that names the variable. Write `parseSize` (`100MB`, `1GB`, `512KB`,
     plain bytes; 1MB is 1048576) and `parseDuration` (adds a `d` suffix for
     days on top of `time.ParseDuration`) and `parseResolution` (name to long
     edge pixels). `S3_*` values are parsed and stored now but nothing uses
     them until task 011; `S3_ENDPOINT` being required is enforced in 011, not
     here.
   - `app` struct with `cfg config`, `ctx context.Context`, `bg sync.WaitGroup`,
     `now func() time.Time`. More fields arrive in later tasks.
   - `httpErr(code int, msg string) error` and its type.
   - `access` type with `accPublic`, `accUser`, `accInvite`, `accGroup`,
     `accAdmin`, and `a.wrap(access, handler) http.HandlerFunc`. There are no
     users yet, so for now `wrap` treats every request as signed out: public
     routes run, anything else redirects a `GET` to `/sign-in` and answers 401
     to other methods. Task 003 fills in the session lookup. Error handling as
     described in `agents.md` ("Request handling").
   - Flash cookie helpers copied in spirit from bento (`flash`, `getFlash`,
     `renderFlash`). Until task 002 there is no stored secret, so derive the
     flash key from a fixed development constant behind a single function
     `a.secret(purpose string) []byte`; task 002 replaces its body.
   - `secureHandler` setting: `Content-Security-Policy`, `Referrer-Policy:
     same-origin`, `X-Content-Type-Options: nosniff`, `X-Frame-Options: deny`,
     `Cross-Origin-Opener-Policy: same-origin`. Do not set
     `Cross-Origin-Embedder-Policy` (it would block media from S3). The CSP
     is built once from config:
     `default-src 'self'; img-src 'self' data: S3; media-src 'self' S3;
     connect-src 'self' S3; frame-ancestors 'none'; base-uri 'none';
     form-action 'self'` where `S3` is the bucket origin (omit it when the
     endpoint is `fake` or unset).
   - Routes: `GET /assets/` (hashfs), `GET /robots.txt`, `GET /{$}` (home,
     `accUser`), `GET /sign-in` (public placeholder page with the title "Sign
     in"). When `DEV=1`: `GET /livereload` and `GET /livereload.js` which
     writes `livereload.JS` with a JavaScript content type (the CSP forbids the
     inline script bento uses).
   - `run(ctx)`: create `a.ctx` with `signal.NotifyContext` for SIGINT and
     SIGTERM, create `STATE_DIRECTORY` if missing, build the mux, wrap it with
     `secureHandler` and `http.NewCrossOriginProtection()`, serve with
     `lands.ListenAndServe(a.ctx, "localhost:8080", h)`, then `a.bg.Wait()`.
4. `ui.go`:
   - `a.page(w, r, title string, body ...g.Node) g.Node`: doctype, `lang`,
     charset, viewport `width=device-width, initial-scale=1,
     viewport-fit=cover`, title `"<title> · <APP_NAME>"`, stylesheet and
     `main.js` (`defer`) through hashfs paths, livereload script tag in dev,
     and `<body>` containing the flash region and `<main id="app">` wrapping
     `body`. The flash region is inside `#app`.
   - `a.pageSimple(...)`: the same shell for centred single column pages (sign
     in, invite, errors) with a heading and a back link slot.
   - Helpers used everywhere later: `fmtBytes(int64) string` (`1.5 MB`),
     `fmtTime(t time.Time, loc *time.Location) string`, an `icon(name)` helper
     with a small set of inline SVG icons (back arrow, send, paperclip, plus,
     key, bell, gear, people, smile, download). Take the SVG paths from
     Bootstrap Icons as bento does.
5. `assets/main.css`: reset, type and space scales (reuse bento's Utopia
   tokens), colour tokens with a dark scheme, and primitives: `.btn` with
   variants, form fields, `.alert`, `.card`, `.list`, `.stack`, `.cluster`,
   page header with back link. Keep it under about 300 lines; the real design
   pass is task 020.
6. `assets/main.js`: only the two global error handlers from bento for now,
   but report with `console.error`, not `alert`.
7. `assets/robots.txt` disallowing everything.
8. `main_test.go`: `newTestApp(t) *testApp` which builds an `*app` with config
   defaults, `STATE_DIRECTORY` set to `t.TempDir()`, a fixed `now`, and the
   full handler. Helpers `ta.get(path, opts...)` and `ta.post(path, form,
   opts...)` returning `*httptest.ResponseRecorder`. `post` must set
   `Sec-Fetch-Site: same-origin` so cross origin protection passes.
   Tests: home redirects to `/sign-in` when signed out; `/sign-in` returns 200
   and contains `id="app"`; a cross site `POST` is rejected; the CSP header is
   present; config parsing table tests for sizes, durations, resolutions and
   bad values.
9. `.hooks/pre-commit` and `.hooks/pre-push` copied from bento, and
   `.github/workflows/go.yml` plus `.github/dependabot.yml` copied from bento.
   Make the hooks executable. The build step in CI must use `CGO_ENABLED=0`.
10. `readme.md`: one paragraph on what this is, how to run it in development,
    and `git config core.hooksPath .hooks/`. Task 021 writes the full version.

## Done when

- `CGO_ENABLED=0 go build ./...` works.
- `DEV=1 go run .` serves `/sign-in` with styles applied and no CSP
  violations in the browser console.
- Stopping the server with Ctrl-C exits within a second.
- `./.hooks/pre-push` passes.
