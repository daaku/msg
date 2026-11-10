# 015 Upload endpoints and file redirects

Use fable.

Server side of file sharing in `media.go`: start a multipart upload, verify
and finish it, turn it into a message, and serve files by redirecting to
signed S3 URLs while counting download bytes. The browser side is task 016
and conversion is tasks 017 and 018. This task is a trust boundary: the
client's claims about size and type are never believed.

## Constants

- `partSize = 8 << 20` (8 MiB; S3 requires at least 5 MiB for all but the last
  part).
- Part URL lifetime: 6 hours.
- Stale upload age: 24 hours.

## Endpoints

All three are `accUser`, take a form encoded or JSON body (pick one, JSON is
suggested) and answer JSON. Errors are JSON `{"error": "..."}` with a 4xx
status. They are called by `fetch` from task 016.

**`POST /upload/init`** `{chat, name, size}`

1. `a.chatMember` on `chat`.
2. `size` must be at least 1 and at most `MAX_UPLOAD_SIZE`, else 413 with a
   message that includes the limit in human form.
3. `name`: keep the base name only, drop control characters, trim, cut to 200
   bytes on a rune boundary, default `file` when empty.
4. `s3_prefix` = 16 random bytes in hex. Key = `<prefix>/original`.
5. `createMultipart` with metadata `Content-Type: application/octet-stream`,
   `Content-Disposition: attachment; filename*=UTF-8''<percent encoded name>`
   and `Cache-Control: private, max-age=31536000, immutable`.
6. Insert the `files` row: `status = 'uploading'`, `kind = 'other'`, `size`,
   `upload_id`.
7. Answer `{file, partSize, urls: [...]}` with one presigned part URL per
   part, `ceil(size / partSize)` of them.

**`POST /upload/complete`** `{file}`

1. Load the file; it must belong to the current user and be `uploading`,
   else 404.
2. `listParts`. Requirements: part numbers are exactly `1..N` with `N` as
   computed at init; every part except the last is exactly `partSize`; the
   sum equals `files.size`. On any mismatch: `abortMultipart`, delete the row,
   answer 400.
3. `completeMultipart` with those parts.
4. Sniff: `get` the first 4096 bytes with a `Range` header and run
   `sniffMedia`. Set `kind` to `image`, `video` or `other`.
5. In one transaction: update the file (`kind`; `status = 'pending'` for
   image and video, `'ready'` for other; clear `upload_id`) with a guard
   `WHERE id = ? AND status = 'uploading'` and stop if no row changed (a
   concurrent complete won); add to `user_stats` and `chat_stats`: 1 to
   `images_uploaded`, `videos_uploaded` or `files_uploaded` according to
   `kind`, and `size` to `bytes_uploaded`.
6. `a.createMessage(ctx, chat, userID, "", fileID)`, which bumps
   `messages_sent` and runs `afterMessage`.
7. Wake the media worker: `a.wakeMedia()`. Define it now as a non blocking
   send on a buffered channel `a.mediaWake`; task 017 adds the receiver.
8. Answer `{ok: true}`.

The user must still be a member at complete time; check `a.chatMember` again
and treat failure like a mismatch (abort and delete).

**`POST /upload/abort`** `{file}`: own file in `uploading` only.
`abortMultipart`, delete the row. Always answer `{ok: true}`.

## Sniffing

`sniffMedia(head []byte) (kind, format string)` returns the kind and the
explicit format name that the converters will be told to use. It is the only
thing that decides how a file is treated; the file name and the browser's
content type are ignored.

| Bytes | Result |
| --- | --- |
| `FF D8 FF` | image, `jpeg` |
| `89 50 4E 47 0D 0A 1A 0A` | image, `png` |
| `GIF87a` or `GIF89a` | image, `gif` |
| `RIFF` at 0 and `WEBP` at 8 | image, `webp` |
| `ftyp` at 4, brand at 8 is one of `heic heix hevc hevx heim heis mif1 msf1` | image, `heic` |
| `ftyp` at 4, brand `avif` or `avis` | image, `avif` |
| `ftyp` at 4, brand `M4A ` or `M4B ` | other |
| `ftyp` at 4, any other brand | video, `mov` |
| `1A 45 DF A3` | video, `matroska` |
| `RIFF` at 0 and `AVI ` at 8 | video, `avi` |
| anything else | other, empty format |

## Serving: `GET /f/{id}/{variant}` (`accUser`)

1. Load the file. `a.chatView` on its chat, so non members get 404.
2. Pick the object and its size:
   - `o`: `<prefix>/original`, `files.size`. Allowed when status is not
     `uploading`.
   - `c`: `<prefix>/converted`, `conv_size`. Only when `status = 'ready'` and
     `conv_size > 0`.
   - `p`: `<prefix>/poster`, `poster_size`. Only when `poster_size > 0`.
   - Anything else: 404.
3. `u, maxAge := a.s3.presignStable(key, cfg.S3URLValidity)`.
4. Add the size to `bytes_downloaded` for the user and for the chat.
5. Answer 302 to `u` with `Cache-Control: private, max-age=<maxAge seconds>`.

The browser caches the redirect, so a device asks us once per window per
file, and the count approximates real downloads.

## Rendering file messages

In `chat.go`, messages with a file render `<div id="m{id}-file">` in place of
the body. Load the files for the rendered messages in one query. For this
task:

- `kind = other`, or any kind with `status = 'failed'`: a download row, an
  `<a href="/f/{id}/o" download="<name>">` with the download icon, the name
  and `fmtBytes(size)`.
- `kind` image or video with status `pending` or `processing`: a placeholder
  box with the name and the text "Processing…".
- `ready` image or video: leave a clearly marked stub that renders the same
  download row; tasks 017 and 018 replace it.

The sidebar snippet for a file message is "username: 📎 name".

## Deleting a message that has a file

Extend `a.deleteMessage` from task 010: after the message row is deleted,
delete the `files` row, then delete the S3 objects `<prefix>/original`,
`<prefix>/converted` and `<prefix>/poster` (`del` treats a missing object as
success; add that if it does not). S3 errors are logged and do not fail the
request: the database is the source of truth and an orphaned object is
harmless. Upload counters are not reduced.

A message with a file has no Edit entry in its menu.

## Cleanup

A janitor goroutine started with `a.bg.Go`: once at startup and then hourly,
for every file with `status = 'uploading'` older than 24 hours:
`abortMultipart` and delete the row. It stops on `a.ctx.Done()`.

Queries: `FileCreate`, `FileByID`, `FileCompleteUpload` (`:execrows` or
returning), `FileDelete`, `FilesStaleUploads`, `FilesByIDs`.

## Tests (`media_test.go`, against the fake S3)

Write a helper `ta.upload(user, chat, name, data) fileID` that performs init,
PUTs each part to the returned URLs with a plain `http.Client` through the
fake, and calls complete. Later tasks reuse it.

- Happy path with a 1 byte file and with a file of `partSize + 1` bytes:
  message exists with `file_id`, file is `ready` and `other` for random
  bytes, object content equals the input, stats went up by one file and the
  exact size for both user and chat, `messages_sent` went up by one.
- Kinds: upload a tiny real PNG, a tiny real MP4 (generate fixtures in the
  test with `magick` and `ffmpeg` if present, otherwise hand written headers)
  and check `kind`, `status = 'pending'` and the right counters.
- `sniffMedia` table test covering every row of the table, plus an HTML file
  named `x.jpg` (must be `other`) and an HLS playlist named `x.mp4` (must be
  `other`).
- Init: non member 404, size 0 and size over the limit rejected, name
  sanitising (`../../etc/passwd`, a 300 byte name, an empty name, a name with
  a newline).
- Complete: missing part, a part with the wrong size, more bytes than
  declared, a file that belongs to someone else, a file already completed
  (second call is 404 and creates no second message), a user removed from the
  group between init and complete. In every failure case there is no message
  and no stats change.
- Abort removes the row; aborting someone else's file does nothing.
- `/f/`: member gets a 302 to a URL that returns the bytes when fetched from
  the fake; the `Cache-Control` max age equals half the validity;
  `bytes_downloaded` grows by the file size for user and chat; non member
  404; signed out redirect; `c` and `p` are 404 for an unconverted file; an
  `uploading` file is 404; two requests in the same window return the same
  `Location`.
- The object's stored metadata is `application/octet-stream` with an
  attachment disposition carrying the name.
- Deleting a file message removes the message row, the file row and the
  original object; `/f/{id}/o` is then 404; counters are unchanged; an S3
  failure during delete still removes the rows.
- Janitor: a 25 hour old `uploading` row is aborted and deleted, a fresh one
  is kept.

## Done when

- Tests above pass and `./.hooks/pre-push` passes.
- There is no code path where a value from the request body is used as an S3
  key, a content type, or a size without the checks above.
