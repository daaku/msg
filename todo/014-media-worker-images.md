# 014 Media worker and image conversion

Use fable.

Add the background worker that converts uploaded media, implement image
conversion with ImageMagick, and show converted images in the chat. Video is
task 015 and plugs into the same worker. Code goes in `media.go`.

Running an image decoder on files from users is the riskiest thing this
server does. Follow the exec rules exactly.

## Worker

- `a.startMedia()` starts 2 worker goroutines with `a.bg.Go`. Before starting
  them, reset every file with `status = 'processing'` to `'pending'` (a
  previous process died mid conversion).
- Each worker loops: claim a file; if none, wait for `a.mediaWake`, a one
  minute ticker, or `a.ctx.Done()`.
- Claim atomically:

  ```sql
  -- name: FileClaim :one
  UPDATE files SET status = 'processing', attempts = attempts + 1
  WHERE id = (
    SELECT f.id FROM files f
    WHERE f.status = 'pending' AND f.kind IN ('image', 'video')
    ORDER BY f.id LIMIT 1
  )
  RETURNING *;
  ```

- Process one file:
  1. Make a temp directory under `<STATE_DIRECTORY>/tmp` and remove it when
     done. Remove everything in `<STATE_DIRECTORY>/tmp` at startup.
  2. Download `<prefix>/original` to `in` in that directory.
  3. Read the first 4096 bytes and run `sniffMedia` again. If the kind no
     longer matches the row, mark `failed`.
  4. Call `a.convertImage` or `a.convertVideo` (stub the latter to return an
     error wrapped as permanent until task 015).
  5. Upload the outputs, update the row to `ready` with sizes, type and
     dimensions, then `a.notifyChat`.
- Failure policy: an error from running a converter (non zero exit, timeout,
  unsupported input) is permanent: set `failed`. Any other error (S3,
  filesystem, database) is transient: set back to `pending` if `attempts <
  3`, else `failed`. Use a `permanentError` wrapper type to tell them apart.
  After any terminal state call `a.notifyChat` so the placeholder is replaced.
- A `failed` file renders as a plain download of the original (already done
  in task 012).
- Shutdown: the exec uses a context derived from `a.ctx`, so Ctrl-C kills a
  running conversion; the file stays `processing` and is reset at next start.

## Exec rules

One helper for every external command:

```go
func (a *app) exec(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) ([]byte, error)
```

- `exec.CommandContext` with the timeout, argv only, never a shell.
- `cmd.Dir` is the temp directory. `cmd.Env` is a minimal environment:
  `PATH`, `HOME` and `TMPDIR` set to the temp directory, `LC_ALL=C`.
- `cmd.WaitDelay = 5 * time.Second` so a hung child is killed.
- Capture stdout; capture stderr into a buffer capped at 64 KiB and include
  its tail in the returned error.
- Wrap failures in `permanentError`.

At startup, look up `magick`, `ffmpeg` and `ffprobe` with `exec.LookPath` and
log one warning line for each that is missing. Do not refuse to start.

## Image conversion

`a.convertImage(ctx, dir, inFormat string) (out mediaResult, err error)`
where `inFormat` comes from `sniffMedia` and must be one of `jpeg`, `png`,
`gif`, `webp`, `heic`, `avif`. Anything else is a permanent error before any
command runs.

Command (verified on ImageMagick 7.1.2), timeout 2 minutes:

```
magick -limit memory 512MiB -limit map 1GiB -limit disk 4GiB -limit time 120
  <inFormat>:in
  [-coalesce]                      only when inFormat is gif
  -auto-orient
  -resize <N>x<N>>                 N = IMAGE_RESOLUTION long edge; ">" means shrink only
  +profile !icc,*                  drop EXIF, GPS, XMP; keep the colour profile
  -quality 82
  <outFormat>:out
```

- The explicit `<inFormat>:` prefix is mandatory. It stops ImageMagick from
  choosing a decoder from the file's content, which is how its dangerous
  coders (MVG, MSL, SVG, PS, URL) get reached.
- `outFormat` and content type from `IMAGE_FORMAT`: `webp` `image/webp`,
  `avif` `image/avif`, `jpeg` `image/jpeg`.
- Animated GIF and WebP inputs stay animated when the output is `webp`. Do
  not add `[0]`. For `jpeg` and `avif` output ImageMagick writes the first
  frame or several files; in that case append `[0]` to the input to take the
  first frame only.
- Dimensions: `magick identify -format "%w %h\n" <outFormat>:out[0]`, parse
  the first line.
- Upload `out` to `<prefix>/converted` with `Content-Type`, `Cache-Control:
  private, max-age=31536000, immutable` and `Content-Disposition: inline`.
- Result: `conv_type`, `conv_size`, `width`, `height`.

Make the core reusable: `a.magickConvert(ctx, dir, inFormat, inName, outName
string, maxEdge int) (w, h int, err error)`. Task 015 uses it for posters and
task 016 for preview images with a smaller `maxEdge`.

## Rendering

For a `ready` image, `m{id}-file` contains:

```html
<a class="media" href="/f/9/c" target="_blank" rel="noopener">
  <img id="f9" src="/f/9/c" width="1600" height="1200" alt="holiday.jpg"
    loading="lazy" decoding="async">
</a>
```

CSS: `max-width: min(100%, 24rem)`, `max-height: 60vh`, `height: auto`,
keeping the aspect ratio from the attributes so the layout does not shift
when the image loads. The sidebar snippet for an image message is
"username: 📷 Photo".

## Tests (skip the ones that need `magick` when it is not installed)

Generate fixtures at test time: `magick -size 6000x4000 gradient:red-blue
jpeg:big.jpg`, a 3 frame GIF (`magick -size 300x200 xc:red xc:green xc:blue
-set delay 20 -loop 0 gif:anim.gif`), a small PNG. Note that these commands
must be run by `bash` or with argv; zsh does not split unquoted variables.

- Large JPEG: output is `image/webp`, long edge exactly 3840, row is `ready`
  with matching `width`, `height`, `conv_size`, and the stored object has the
  right metadata.
- Small PNG is not upscaled.
- EXIF is removed: write a fixture with an orientation and a GPS tag (use
  `exiftool` if present, or commit a tiny JPEG fixture under `testdata/` that
  has them), convert, and assert `magick identify -format "%[EXIF:*]"` prints
  nothing and the pixels are rotated upright.
- Animated GIF gives a WebP with 3 frames.
- `IMAGE_FORMAT=jpeg` and `avif` produce those types.
- Format pinning: a GIF passed with `inFormat = "jpeg"` fails, and a text
  file containing MVG commands named as a JPEG fails, both as permanent
  errors with the file ending `failed`.
- Worker: an uploaded PNG goes `pending` to `ready` and the group is pinged;
  a corrupt image goes to `failed` and renders the download row; a transient
  S3 error (inject through a stub transport) retries and then succeeds;
  after 3 transient failures the file is `failed`; `processing` rows are
  reset at startup; two workers never claim the same file (run 20 files
  with `-race`).
- Rendering: a ready image has `loading="lazy"`, `width` and `height`.
- `/f/{id}/c` now redirects for the converted image and counts `conv_size`.

## Done when

- Uploading a phone photo in the browser shows "Processing…" and then the
  image, in both members' views, without a reload.
- `go test -race ./...` passes with ImageMagick installed.
- `./.hooks/pre-push` passes.
