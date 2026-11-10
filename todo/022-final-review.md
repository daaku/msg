# 022 Final review against the spec

Use fable.

Review the finished application as a sceptical outsider: check it against
`todo.md` line by line, hunt for security and correctness bugs, and run it
end to end in a real browser. Fix what you find, each fix with a test. This
task has no feature work.

## 1. Spec conformance

Go through every bullet in `todo.md`. For each, find the code that implements
it and a test that proves it, and write one line: implemented and tested,
implemented but untested (add the test), or missing or different (fix it, or
if it needs a decision, list it for the user). Pay attention to the negative
requirements: confirm there is no email or contact field anywhere, no read
receipts, no presence, no status, and nothing that was not asked for. If you
find an unspecified feature that slipped in, list it for the user with a
recommendation; do not silently delete it.

Confirm the JavaScript in `assets/main.js` matches the inventory in
`agents.md` and nothing else, and report its size in lines.

## 2. Authorisation matrix

Write (or complete) one table driven test that enumerates every route
registered on the mux and requests it as: signed out, a user with no
permissions who is in no group, a member of a different group, a member, the
group's creator, a user with only the invite permission, a user with only
the group permission, and an admin. Assert the expected status for each
cell. The test must fail when a new route is added without a row, so build
the route list from the same place `run()` registers them.

Specifically try to: read another group's messages, react or upload into a
group you are not in, fetch `/f/` for a file in another group, complete or
abort someone else's upload, revoke someone else's invite or link, delete
someone else's passkey or push subscription, see someone else's stats, and
reach any admin page.

## 3. Security review

Check each and write down what you verified:

- Passkeys: challenges are single use and expire; the RP ID and origin are
  checked; a credential cannot be registered to a second user; a deleted
  passkey's sessions die; the last admin cannot be removed.
- Invites: single use under concurrency (fire 20 parallel accepts of one
  token; exactly one succeeds); expiry enforced on all three endpoints;
  tokens have 256 bits; token pages are `no-store` and `no-referrer`.
- Cookies: `HttpOnly`, `Secure` on HTTPS, `SameSite`; nothing sensitive in a
  readable cookie.
- CSRF: every state changing route is `POST` and covered by cross origin
  protection; no state changing `GET` (`/f/` changes a counter, which is
  accepted and documented).
- XSS: usernames, group names, message bodies, file names, link preview
  text, push payloads, flash messages. Search for `g.Raw` and justify each
  use. Confirm the CSP is sent on every response and has no `unsafe-inline`.
- Uploads: the checks in task 012 hold; file names cannot break out of the
  `Content-Disposition` header; originals are always served as attachments.
- Converters: every `magick`, `ffmpeg` and `ffprobe` call has an explicit
  input format, a timeout, argv arguments and the minimal environment.
- Link previews: rerun the SSRF tests; try a redirect chain that ends at
  `169.254.169.254`, and an IPv6 literal for loopback.
- S3: nothing from a request reaches an object key; signed URLs are only
  handed to users who passed the group check.
- Logs: start the server, use every feature, and read the log for tokens,
  cookies, subscription keys or message bodies.
- Run `go vet ./...` and `govulncheck ./...`
  (`go run golang.org/x/vuln/cmd/govulncheck@latest ./...`).

## 4. Correctness under load and failure

- `go test -race -count=3 ./...`.
- 50 goroutines sending messages to one group while 10 hold `/events`
  streams: no errors, no `database is locked`, every message stored once,
  every stream gets pinged.
- Kill the process (SIGKILL) during an image conversion, a video conversion
  and an upload; restart; the system recovers without manual steps.
- Start with S3 unreachable: the error is clear. Make S3 fail mid
  conversion: the retry policy behaves as specified.
- 5000 messages in a group with `n=5000`: the page renders in reasonable
  time. Report the time and the query count. Add an index or fix an N+1 only
  if measurements show a problem.

## 5. End to end in a browser

With `S3_ENDPOINT=fake DEV=1 go run .` on an empty state directory, in
Chromium with a virtual authenticator (DevTools, WebAuthn panel, or the
`WebAuthn.addVirtualAuthenticator` protocol command), walk the whole product
once:

bootstrap admin, grant permissions, invite a user, accept in a second
profile, create a group, exchange messages live, react, upload an image, a
video and a PDF, post a link, enable notifications and receive one, add a
second passkey through a link, delete a passkey, check stats on the account,
group and admin pages, sign out and in.

Watch the console for errors and CSP violations throughout. Report what you
ran and what you could not.

## 6. Hand over

Finish with a short report for the user:

- What was fixed in this task.
- Open decisions that need the user (anything from section 1).
- What could not be verified here and needs checking on real hardware or
  infrastructure: a real S3 provider, an iPhone (install, passkey, push,
  video playback), a production proxy.

## Done when

- Sections 1 to 5 are done and summarised in the report.
- Every fix has a test.
- `./.hooks/pre-push` passes.
