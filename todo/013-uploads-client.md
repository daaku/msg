# 013 Chunked background upload in the browser

Add the attach button to the composer and the JavaScript that uploads chosen
files to S3 in chunks, in the background, with a subtle circular progress
indicator at the top right. This is item 6 of the JavaScript inventory in
`agents.md`. The server endpoints exist from task 012.

## Markup (`chat.go`)

Inside `#composer`, before the textarea:

```html
<label id="composer-attach" class="btn icon" aria-label="Attach files">
  icon
  <input id="composer-files" type="file" multiple hidden
    data-upload="5" data-max="104857600">
</label>
```

`data-upload` is the chat id and `data-max` is `MAX_UPLOAD_SIZE` in bytes.

Inside `#app` but outside `#pane`, rendered on every `a.chatPage`:

```html
<div id="upload-ring" class="upload-ring" role="progressbar"
  aria-label="Uploading" aria-valuemin="0" aria-valuemax="100"></div>
<div id="upload-error" class="upload-error" role="alert"></div>
```

## Progress ring (CSS only for the drawing)

- Fixed at the top right, inside the safe area, about 1.75rem across, above
  page content, `pointer-events: none`.
- Hidden unless `html.uploading` is set.
- Drawn with `conic-gradient(var(--accent) calc(var(--upload) * 1%),
  var(--ring-track) 0)` and a radial mask to make it a ring. `--upload` is a
  number from 0 to 100 set on the `<html>` element.
- The JavaScript only does `document.documentElement.style.setProperty(
  '--upload', pct)` and toggles the `uploading` class. State lives on
  `<html>`, which is outside `#app`, so morphing does not disturb it. Setting
  a custom property from script is allowed by the CSP.

## JavaScript (`assets/main.js`)

One delegated `change` listener for `[data-upload]`:

1. Copy the `FileList` into a queue of `{file, chat}` and reset the input's
   value so the same file can be chosen again.
2. Files larger than `data-max` are not queued; show "<name> is larger than
   the <limit> limit" in `#upload-error`.
3. A single worker loop drains the queue, one file at a time. Selecting more
   files while it runs appends to the queue.
4. For each file:
   - `POST /upload/init` with `{chat, name: file.name, size: file.size}`.
   - For each URL `i`: `PUT` `file.slice(i * partSize, (i + 1) * partSize)`
     using `XMLHttpRequest` (needed for `upload.onprogress`; `fetch` cannot
     report upload progress). No custom headers. Success is a 2xx status.
   - Retry a failed part up to 5 times with waits of 1, 2, 4, 8 and 16
     seconds. Wait for `navigator.onLine` to be true before a retry.
   - `POST /upload/complete` with `{file}`.
   - On a final failure: `POST /upload/abort`, show "<name> could not be
     uploaded" in `#upload-error`, continue with the next file.
5. Progress is bytes sent over total bytes of everything queued since the
   queue was last empty, including the in flight part's `loaded` value.
6. While the queue is not empty: `html.uploading` is set and a `beforeunload`
   handler asks for confirmation. Both are removed when it drains.
7. `#upload-error` is cleared when a new selection is made. Because it is
   inside `#app`, write the message with `textContent` and also keep it in a
   variable, and re-apply it after each `refresh()` (the morph would
   otherwise blank it). Keep this simple: one function `showUploadError()`
   called at the end of `refresh()`.

There is no need to call `refresh()` after complete: the server pings the
group through `afterMessage`.

Do not add drag and drop, paste handling, previews, pause or resume, or
parallel parts.

## Tests

Go:

- A group page for a member contains `id="composer-files"` with the right
  `data-upload` and `data-max`, and `id="upload-ring"`. A non member admin
  view has no attach control.

Browser, using the fake S3 (`S3_ENDPOINT=fake DEV=1 go run .`). Create test
files with `head -c 30000000 /dev/urandom > /tmp/.../big.bin`:

1. Upload a 30 MB file: the ring appears and advances smoothly, four `PUT`
   requests go to `/fakes3/`, the message appears in both members' views
   without a reload, and downloading it gives identical bytes (`sha256sum`).
2. Select three files at once: three messages, in selection order.
3. While an upload runs, type and send a text message: it sends, and the
   upload continues.
4. Throttle the network to offline mid upload for ten seconds, then back: the
   upload finishes.
5. Choose a file over the limit (start the server with
   `MAX_UPLOAD_SIZE=1MB`): nothing is uploaded and the error text shows.
6. Try to close the tab mid upload: the browser asks for confirmation.
7. No CSP violations in the console.

Report each as passed or not run.

## Done when

- The browser checks pass, or are listed as not run with the reason.
- `./.hooks/pre-push` passes.
