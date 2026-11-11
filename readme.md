# msg

A small, self-hosted, web based group chat written in Go. It is a single CGO
free binary that keeps its state in one SQLite file and stores uploaded files
in an S3 compatible bucket. Pages are rendered on the server, JavaScript is
used only where it has to be, and passkeys are the only way to sign in.

- Passkey sign in only: no passwords, no email, no contact details of any kind
- An admin account bootstrapped from a URL printed in the log
- Single use invite links, each for a chosen username
- Permissions per user: admin, send invites, create groups
- Groups and direct messages in one chat list; archiving per user
- Messages can be edited and deleted by their author, with no history kept
- Emoji reactions
- File uploads straight from the browser to the bucket in chunks, in the
  background, with a progress ring
- Images converted with ImageMagick and videos with ffmpeg; the original is
  always kept
- Link previews, fetched by a client that refuses private and local addresses
- Live updates over Server-Sent Events
- Web push notifications, on any number of devices per user
- Snoozing a message: a reminder push for yourself only, at a time you pick
- Stats per user and per group
- Passkeys, notifications and stats on one settings page
- Installable as a home screen app, including on iOS

## Requirements

To build: Go 1.27 or newer (see `go.mod`).

At runtime:

- `magick` (ImageMagick 7) with WebP support, plus HEIC and AVIF support
  (libheif) if those inputs or `IMAGE_FORMAT=avif` matter to you. Check with
  `magick -list format | grep -Ei '^ *(webp|heic|avif)'`.
- `ffmpeg` and `ffprobe` with the `libx264` encoder (or `libsvtav1` for
  `VIDEO_FORMAT=av1`) and the `aac` encoder. Check with
  `ffmpeg -hide_banner -encoders | grep -E 'libx264|libsvtav1|aac'`.
- An S3 compatible bucket.
- A reverse proxy that terminates TLS.

The server starts without `magick`, `ffmpeg` or `ffprobe` and prints a
warning; images and videos are then offered as plain downloads.

## Quick start for development

```sh
git config core.hooksPath .hooks/
S3_ENDPOINT=fake DEV=1 go run .
```

On the first start the log has a line like

```
Bootstrap admin: http://localhost:8080/invite/...
```

Open it in a browser on the same machine and create the admin passkey.
`localhost` counts as a secure context, so passkeys work without TLS.

`S3_ENDPOINT=fake` uses a bucket built into the server that keeps objects in
`data/fakes3/`. `DEV=1` turns on live reload and shows stack traces on error
pages. Instead of setting them on the command line, `cp .env.sample .env`:
the `.env` file in the working directory is read at start, and variables
already set in the environment win over it.

State lives in `data/` (the database is `data/msg.db`). Delete the directory
to start over.

The `pre-commit` hook runs the tests. The `pre-push` hook checks `go mod
tidy`, regenerates code, requires a clean tree, builds and runs the tests.

The database queries are generated with sqlc from `db/query.sql`. After
changing anything in `db/`, run

```sh
go generate ./...
```

and commit the generated files.

## Configuration

All configuration is environment variables. See `.env.sample`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `PUBLIC_URL` | `http://localhost:8080` | External origin, like `https://chat.example.com`. Also gives the passkey RP ID (its host name) and origin |
| `APP_NAME` | `msg` | Page titles, home screen name, passkey RP name |
| `STATE_DIRECTORY` | `data` | Holds `msg.db`, temporary conversion files and the fake bucket. systemd sets it with `StateDirectory=` |
| `ADMIN_USERNAME` | `admin` | Username of the bootstrap admin |
| `DEV` | unset | `1` enables live reload and stack traces on error pages. Never in production |
| `S3_ENDPOINT` | required | `https://host` (no path), or `fake` for the built in development bucket |
| `S3_REGION` | `us-east-1` | Region used for signing |
| `S3_BUCKET` | required unless fake | |
| `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | required unless fake | |
| `S3_PATH_STYLE` | `true` | `false` selects virtual hosted style URLs (`bucket.host`) |
| `S3_URL_VALIDITY` | `7d` | Signed URL validity, like `7d` or `12h` |
| `MAX_UPLOAD_SIZE` | `100MB` | Largest accepted upload. `KB`, `MB`, `GB`, `TB` are binary: 1MB is 1048576 bytes |
| `IMAGE_FORMAT` | `webp` | `webp`, `avif` or `jpeg` |
| `IMAGE_RESOLUTION` | `4k` | Long edge cap for images and video posters |
| `VIDEO_FORMAT` | `h264` | `h264` or `av1`, always in an MP4 container with AAC audio |
| `VIDEO_RESOLUTION` | `1080p` | Long edge cap for videos |
| `ADDR` | `localhost:8080` | Listen address, like `127.0.0.1:8080` |
| `PORT` | unset | Listen on this port on all interfaces. Wins over `ADDR` |
| `LISTEN_FDS` | unset | Set by systemd socket activation. When `1`, the server uses the inherited socket and ignores `PORT` and `ADDR` |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | unset | Serve HTTPS directly with this certificate and key. Not needed behind a proxy |

Resolution names map to the long edge in pixels: `4k` 3840, `1440p` 2560,
`1080p` 1920, `720p` 1280, `480p` 854. Media is never upscaled. Changing a
format or resolution affects new uploads only.

An invalid value stops the server at start with a message naming the
variable.

The cookie secret and the VAPID key for push are not configuration. They are
generated on the first start and stored in the `config` table of `msg.db`.

## First run

When the server starts and the database has no users, it prints

```
Bootstrap admin: https://chat.example.com/invite/...
```

The link creates the user `ADMIN_USERNAME` with the admin permission and
registers a passkey for it. It works once and expires after 24 hours. Every
start while there are still no users replaces it with a new link, so if it
expired, restart the service. Once a user exists, no link is printed.

With systemd, find it with `journalctl -u msg`.

From there the admin invites people from the Invites page. An admin can also
make an "add passkey" link for any user, to recover someone who lost their
device.

## S3 bucket setup

The bucket is private: no public access and no public bucket policy. Browsers
upload to it and download from it through signed URLs made by the server.

The access key needs, on the bucket's objects: get, put, delete, and the
multipart operations (create, upload part, list parts, complete, abort). It
never lists the bucket. For AWS:

```json
{"Version": "2012-10-17",
 "Statement": [{"Effect": "Allow",
   "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject",
              "s3:ListMultipartUploadParts", "s3:AbortMultipartUpload"],
   "Resource": "arn:aws:s3:::BUCKET/*"}]}
```

At every start the server stores an object, reads it back through a signed
URL of the configured validity and deletes it. If any of that fails, it
exits with the reason.

### CORS

Browsers `PUT` upload parts directly to the bucket from the app's origin, so
the bucket needs a CORS rule for that origin:

```json
[{"AllowedOrigins": ["https://chat.example.com"],
  "AllowedMethods": ["PUT"],
  "AllowedHeaders": ["*"],
  "MaxAgeSeconds": 3600}]
```

This is the form the Cloudflare R2 dashboard takes. For the AWS CLI wrap it
as `{"CORSRules": [...]}` and run
`aws s3api put-bucket-cors --bucket BUCKET --cors-configuration file://cors.json`.

Downloads are plain `<img>` and `<video>` loads and need no CORS. The start
up check does not test CORS: a missing rule shows up as uploads that fail in
the browser.

### Incomplete uploads

The server aborts uploads that were started but not finished after 24 hours.
As a backstop, add a lifecycle rule that aborts incomplete multipart uploads
after 2 days. For AWS:

```json
{"Rules": [{"ID": "abort-incomplete-uploads", "Status": "Enabled",
  "Filter": {},
  "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 2}}]}
```

```sh
aws s3api put-bucket-lifecycle-configuration --bucket BUCKET \
  --lifecycle-configuration file://lifecycle.json
```

### Signed URL validity

`S3_URL_VALIDITY` defaults to `7d`, the most that AWS S3, Cloudflare R2 and
MinIO accept for SigV4 signed URLs. A provider that allows longer can be
given a larger value; the start up check fails if the provider rejects it.
The signed URL for a file stays the same for half the validity, so a browser
can reuse its cached copy for half the validity period before it needs a
fresh URL and downloads the file again; a longer validity means fewer
repeat downloads, but a leaked URL keeps working for longer.

### Path style

With `S3_PATH_STYLE=true` (the default) URLs look like
`https://host/bucket/key`; with `false` they look like
`https://bucket.host/key`.

- AWS S3: `false`, with `S3_ENDPOINT=https://s3.REGION.amazonaws.com` and
  `S3_REGION` set to the bucket's region. AWS is phasing out path style.
- Cloudflare R2: `true`, with
  `S3_ENDPOINT=https://ACCOUNT_ID.r2.cloudflarestorage.com` and
  `S3_REGION=auto`.
- MinIO: `true`, unless the server is set up for virtual hosts
  (`MINIO_DOMAIN`).
- Bucket names containing a dot need `true` on HTTPS, because
  `bucket.with.dots.host` does not match the provider's certificate.

## Reverse proxy

The app speaks plain HTTP and expects a proxy in front of it that terminates
TLS. Listen on loopback only, for example `ADDR=127.0.0.1:8080`. `PORT`
listens on all interfaces.

`PUBLIC_URL` must be the external HTTPS origin, like
`https://chat.example.com`. Passkeys are bound to its host name and push
notifications name it, and cookies are marked `Secure` only when it starts
with `https://`. Changing the host name later invalidates every passkey:
nobody can sign in any more.

`/events` is a Server-Sent Events stream that stays open as long as a tab is
visible and sends a heartbeat every 25 seconds. The proxy must not buffer it
and must allow long lived responses. The response carries
`X-Accel-Buffering: no`.

Uploads and downloads go straight between the browser and the bucket, so the
proxy needs no large request body limit.

The app does not compress responses. Enable compression at the proxy.

Caddy (streams by default):

```
chat.example.com
reverse_proxy 127.0.0.1:8080
```

With compression, add `encode` as a third line.

nginx:

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name chat.example.com;
    ssl_certificate     /etc/letsencrypt/live/chat.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/chat.example.com/privkey.pem;

    gzip on;
    gzip_types text/css text/javascript application/javascript
               application/json application/manifest+json image/svg+xml;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
    }

    location = /events {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_read_timeout 1h;
        gzip off;
    }
}
```

nginx compresses `text/html` whenever `gzip on` is set.

## systemd

Build the binary with `CGO_ENABLED=0 go build -trimpath` (or take `msg.tar`
from a release) and install it as `/usr/local/bin/msg`. Put the
configuration in `/etc/msg.env`, readable by root only:

```sh
PUBLIC_URL=https://chat.example.com
ADDR=127.0.0.1:8080
S3_ENDPOINT=https://ACCOUNT_ID.r2.cloudflarestorage.com
S3_REGION=auto
S3_BUCKET=msg
S3_ACCESS_KEY_ID=...
S3_SECRET_ACCESS_KEY=...
```

`/etc/systemd/system/msg.service`:

```ini
[Unit]
Description=msg
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/msg
EnvironmentFile=/etc/msg.env
DynamicUser=yes
StateDirectory=msg
Restart=on-failure
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes

[Install]
WantedBy=multi-user.target
```

```sh
systemctl enable --now msg
journalctl -u msg
```

`DynamicUser=yes` runs the service as a throwaway user that owns nothing
else on the machine. `StateDirectory=msg` creates `/var/lib/msg`, owned by
that user, and sets `STATE_DIRECTORY` to it, so the database ends up at
`/var/lib/msg/msg.db`. `EnvironmentFile=` is read by systemd as root, so the
service user cannot read it. With `ProtectSystem=strict` the state directory
is the only writable place, which is all the server, `magick` and `ffmpeg`
need: conversions run in `/var/lib/msg/tmp`. Do not add
`MemoryDenyWriteExecute=` or a `SystemCallFilter=` without testing an image
and a video upload afterwards. The server needs outbound HTTPS to the bucket,
the push services and the sites it makes link previews for.

### Socket activation

The listener can come from systemd instead, so the port stays open and
connections wait while the service restarts. With an inherited socket
`ADDR` and `PORT` are ignored. Add `/etc/systemd/system/msg.socket`:

```ini
[Socket]
ListenStream=127.0.0.1:8080

[Install]
WantedBy=sockets.target
```

```sh
systemctl enable --now msg.socket
```

## Backups

The whole state is `msg.db` (with its `msg.db-wal` and `msg.db-shm` while the
server runs) in the state directory, and the bucket. Do not copy the live
database file. Either use SQLite's online backup:

```sh
sqlite3 /var/lib/msg/msg.db ".backup /root/msg-backup.db"
```

or stop the service and copy `msg.db`. Back up the bucket with your
provider's tools.

The `config` table holds the cookie secret and the VAPID key. Losing it
signs everyone out and voids every push subscription: each device has to
enable notifications again. Passkeys are stored in the database and keep
working.

## Upgrading

Replace the binary and restart. Database migrations run at start.

## Installing on a phone

iOS: open the site in Safari, tap Share, then Add to Home Screen. Open the
app from the home screen, go to Settings and tap "Enable notifications on
this device". iOS only allows web push for apps added to the home screen.

Android: Chrome offers Install app (or Add to Home screen) in its menu.
Notifications can be enabled from Settings in the browser or the installed
app.

Each device that should get notifications enables them once on its own.

## Video format

`h264` is the default because it plays everywhere. `VIDEO_FORMAT=av1` gives
smaller files, but iPhones before the 15 Pro and other devices without AV1
hardware decoding cannot play it. Changing it affects new uploads only.

## Known limits

- Single process only: the live update hub is in memory. Do not run two
  instances against the same database.
- Download byte counts are estimates. Downloads go straight from the bucket;
  the server counts the size of a file each time it hands out a signed URL,
  and browsers cache that redirect.
- Edits and deletes keep no history.
- Snooze reminders are push notifications, so they need notifications
  enabled on at least one device.
- The built in fake bucket (`S3_ENDPOINT=fake`) is for development only.
