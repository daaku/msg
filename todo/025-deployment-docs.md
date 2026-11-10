# 025 Readme and deployment documentation

Write the documentation someone needs to run this in production behind a TLS
terminating proxy with a real S3 compatible bucket. The output is
`readme.md` and a `.env.sample`. No code changes, except fixing anything the
documentation turns out to be wrong about.

Verify every statement against the code before writing it. Where you could
not verify something on real infrastructure, mark it in your report, not in
the readme.

## `readme.md` sections

1. **What it is.** Two or three sentences and the feature list, taken from
   what is actually built.
2. **Requirements.** Go (version from `go.mod`) to build; at runtime
   `magick` (ImageMagick 7 with WebP, and HEIC and AVIF support if those
   inputs matter) and `ffmpeg` and `ffprobe` with `libsvtav1` or `libx264`
   and `aac`; an S3 compatible bucket; a reverse proxy that terminates TLS.
3. **Quick start for development.** `S3_ENDPOINT=fake DEV=1 go run .`, open
   the bootstrap URL from the log. Git hooks. `go generate`.
4. **Configuration.** The full table of environment variables with
   defaults, copied from `agents.md` and checked against `loadConfig`.
5. **First run.** The bootstrap admin URL: when it is printed, that it
   expires after 24 hours and is replaced on restart while there are no
   users.
6. **S3 bucket setup.**
   - The bucket is private. The app needs get, put, delete, and multipart
     permissions on it.
   - CORS: browsers `PUT` upload parts from the app's origin. Give the JSON:

     ```json
     [{"AllowedOrigins": ["https://chat.example.com"],
       "AllowedMethods": ["PUT"],
       "AllowedHeaders": ["*"],
       "MaxAgeSeconds": 3600}]
     ```

     Downloads are plain `<img>` and `<video>` loads and need no CORS.
   - A lifecycle rule to abort incomplete multipart uploads after 2 days, as
     a backstop to the app's own cleanup.
   - Signed URL validity: `S3_URL_VALIDITY` defaults to `7d`, the most that
     AWS S3, Cloudflare R2 and MinIO accept for SigV4 URLs. A provider that
     allows longer can be given a larger value; the server checks the
     configured value at startup. Explain the trade off in one sentence: the
     browser can reuse a cached file for half the validity period before it
     needs a fresh URL.
   - `S3_PATH_STYLE` and which common providers need which value.
7. **Reverse proxy.** The app speaks plain HTTP and expects TLS in front.
   `PUBLIC_URL` must be the external HTTPS origin (passkeys and push depend
   on it; changing the hostname later invalidates every passkey). The proxy
   must not buffer `/events` and must allow long lived responses. Give a
   Caddy example (two lines; Caddy streams by default) and an nginx example
   with `proxy_buffering off`, `proxy_read_timeout 1h` and
   `proxy_http_version 1.1` for `/events`. Recommend enabling compression at
   the proxy; the app does not compress.
8. **systemd.** A unit using `DynamicUser=yes`, `StateDirectory=msg` (which
   sets `STATE_DIRECTORY`), `EnvironmentFile=`, and hardening options that
   are compatible with running `magick` and `ffmpeg` (`NoNewPrivileges`,
   `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`).
   Mention socket activation through `lands`. Look at how bento's commit
   "updated setup with shared email secrets and DynamicUser for service"
   describes its setup for the style the user prefers.
9. **Backups.** The whole state is `msg.db` (plus `-wal`) and the bucket.
   Use `sqlite3 msg.db ".backup ..."` or stop the service; do not copy a live
   file. Losing the `config` table loses the cookie secret (everyone is
   signed out) and the VAPID key (every push subscription is void).
10. **Installing on a phone.** iOS: Share, Add to Home Screen, then enable
    notifications from Settings inside the installed app.
11. **Video format.** `h264` is the default because it plays everywhere.
    `VIDEO_FORMAT=av1` gives smaller files, but iPhones before the 15 Pro and
    other devices without AV1 hardware decoding cannot play it. Changing it
    affects new uploads only.
12. **Known limits.** Single process only (the live update hub is in
    memory). Download byte counts are estimates. Edits and deletes keep no history.
    Reminders need notifications enabled on at least one device.

## `.env.sample`

Every variable, commented, with the development values uncommented
(`S3_ENDPOINT=fake`, `DEV=1`) and production examples commented out.

## Done when

- A reader with a fresh machine could deploy from the readme alone. Test the
  development quick start from a clean clone in a temporary directory.
- Every environment variable in `loadConfig` is in the readme table and in
  `.env.sample`, and no variable is documented that the code does not read.
- The CI workflow builds with `CGO_ENABLED=0` and uploads the binary as in
  bento.
- `./.hooks/pre-push` passes.
