# 018 Video conversion

Use fable.

Implement `a.convertVideo` in `media.go` using ffmpeg, produce a poster
image, and show videos in the chat. The worker, the exec helper and the
failure policy exist from task 017.

## Input safety

ffmpeg can be tricked by playlist formats (HLS, concat, and others) into
reading local files or making network requests. The defence is that ffmpeg
never chooses the demuxer:

- `inFormat` comes from `sniffMedia` and must be `mov`, `matroska` or `avi`.
  Anything else is a permanent error before any command runs.
- Every ffmpeg and ffprobe invocation that opens a user file passes
  `-f <inFormat>` immediately before `-i`.
- Always pass `-nostdin`. Never pass a URL; inputs and outputs are relative
  file names in the temp directory.

A playlist renamed to `.mp4` then fails with "moov atom not found" (checked
while planning). Keep a test for it.

## Commands

All verified on ffmpeg 9.0 with SVT-AV1 4.2. Timeout for the encode: 60
minutes.

Scale filter, with `N` the `VIDEO_RESOLUTION` long edge (1920 by default).
It caps the long edge, keeps the aspect ratio, never upscales, and keeps
dimensions even:

```
scale='if(gt(iw,ih),min(N,iw),-2)':'if(gt(iw,ih),-2,min(N,ih))'
```

ffmpeg applies rotation metadata before the filter, so portrait phone video
comes out upright.

`VIDEO_FORMAT=h264` is the default and is described below. `VIDEO_FORMAT=av1`:

```
ffmpeg -nostdin -hide_banner -loglevel error -y
  -f <inFormat> -i in
  -map 0:v:0 -map 0:a:0?
  -vf <scale>
  -c:v libsvtav1 -preset 8 -crf 35 -pix_fmt yuv420p10le
  -c:a aac -b:a 128k -ac 2
  -map_metadata -1
  -movflags +faststart
  -f mp4 out.mp4
```

`VIDEO_FORMAT=h264`: the same with
`-c:v libx264 -preset medium -crf 23 -pix_fmt yuv420p` as the video codec
line.

Notes:

- `-map 0:a:0?` makes audio optional, so silent videos work.
- `-map_metadata -1` drops container metadata, which on phone video includes
  the GPS location.
- `+faststart` moves the index to the front so playback can start before the
  download finishes.
- SVT-AV1 prints its banner to stderr even with `-loglevel error`. That is
  why the exec helper only reports stderr on failure.
- Content type is `video/mp4` for both.

Probe the output (not the input) for dimensions and duration:

```
ffprobe -v error -f mov -select_streams v:0
  -show_entries stream=width,height:format=duration -of json out.mp4
```

Round the duration to whole seconds, minimum 1.

Poster, from the converted file:

```
ffmpeg -nostdin -hide_banner -loglevel error -y -f mov -i out.mp4
  -frames:v 1 -c:v png -f image2 poster.png
```

then `a.magickConvert(ctx, dir, "png", "poster.png", "poster", imageEdge)`
to get the poster in `IMAGE_FORMAT`.

Upload `out.mp4` to `<prefix>/converted` and the poster to `<prefix>/poster`,
both with the one year `Cache-Control` and `Content-Disposition: inline`.
Update the row: `conv_type`, `conv_size`, `poster_type`, `poster_size`,
`width`, `height`, `duration`, `status = 'ready'`.

A file with no video stream (audio in an MP4 container that was not caught
by the brand check) makes ffmpeg fail on `-map 0:v:0`; that is a permanent
error and the file falls back to a download, which is the right outcome.

## Rendering

For a `ready` video, `m{id}-file` contains:

```html
<video id="f9" class="media" controls playsinline preload="none"
  poster="/f/9/p" width="1920" height="1080" src="/f/9/c"></video>
```

`preload="none"` means the video is only downloaded when played, and the
poster gives it a face. Same sizing CSS as images. Because the element has a
stable id and unchanged attributes, a morph does not interrupt playback;
check this in the browser. The sidebar snippet is "username: 🎬 Video".

## Tests (skip when `ffmpeg` is missing)

Fixtures generated at test time:

```
ffmpeg -nostdin -loglevel error -y -f lavfi -i testsrc2=size=2560x1440:rate=30:duration=2
  -f lavfi -i sine=frequency=440:duration=2 -c:v libx264 -pix_fmt yuv420p -c:a aac
  -metadata location=+12.34+56.78/ land.mp4
ffmpeg -nostdin -loglevel error -y -f lavfi -i testsrc2=size=1080x2400:rate=30:duration=1
  -c:v libvpx-vp9 port.webm
```

- Landscape MP4 with `av1`: output codec `av1`, 1920x1080, has an audio
  stream, duration 2, row `ready`, poster exists and is `image/webp`
  1920x1080, `location` tag is gone (probe `format_tags`).
- Portrait WebM (no audio) with `h264`: output `h264`, 864x1920, no audio
  stream, row `ready`.
- A 640x360 input is not upscaled.
- An HLS playlist saved as `evil.mp4` that references `/etc/passwd`: sniffed
  as `other` by task 015, and when forced through `convertVideo` with
  `inFormat = "mov"` it fails as a permanent error.
- `inFormat` outside the allow list is rejected without running anything
  (assert no process was started, for example by pointing `PATH` at an empty
  directory).
- A conversion is cancelled when `a.ctx` is cancelled, and the file is
  `pending` again after the startup reset.
- `/f/{id}/p` redirects and counts `poster_size`; `/f/{id}/c` counts
  `conv_size`.
- Rendering: `preload="none"`, `poster`, `width`, `height`, `playsinline`.

## Done when

- In a browser, a phone video uploads, shows "Processing…", then plays in
  both members' views, upright, and keeps playing while new messages arrive.
- Report which browsers you played the output in, for both formats. H.264
  is the default because iPhones older than the 15 Pro cannot decode AV1.
- `./.hooks/pre-push` passes.
