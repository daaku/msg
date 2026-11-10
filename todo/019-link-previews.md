# 019 Link previews

Use fable.

When a text message contains a URL, fetch that page on the server, extract a
title, description and image, and show a preview card under the message.
Code goes in `media.go`, rendering in `chat.go`.

The server fetching arbitrary URLs chosen by users is a server side request
forgery risk. The HTTP client described below is the core of this task.

## Flow

1. In `afterMessage`: if the message has no file and `firstURL(body)` is not
   empty, insert a `previews` row (`status = 'pending'`) and wake the preview
   worker. Only the first URL gets a preview.
2. One preview worker goroutine (`a.bg.Go`), same wake, ticker and shutdown
   pattern as the media worker. At startup it picks up any `pending` rows.
3. For each pending preview: fetch and parse; store the fields; if there is
   an image, fetch, convert and store it; set `ready`. Any failure sets
   `failed`. A preview is tried once, there are no retries.
4. After `ready` (not after `failed`), `a.notifyChat`.

A preview with neither title nor image is `failed`.

**Edits.** In `a.afterEdit` (task 010): compare `firstURL` of the new body
with the existing preview's `url`. Same: do nothing. Different or gone:
delete the old preview (and its image, as below), and if there is a new URL
insert a fresh `pending` row and wake the worker.

**Deletes.** In `a.deleteMessage`: the `previews` row goes with the message
by cascade, but its image is a `files` row and S3 object. Read
`previews.file_id` before deleting the message, then delete that file row and
its `converted` object, the same way attached files are removed. If the
worker finishes a preview for a message that no longer exists, the insert or
update fails or changes no row: remove the image it just stored.

## The safe HTTP client

`a.previewClient` is built once:

- `Transport` is a fresh `http.Transport` with `Proxy: nil` and a
  `DialContext` from a `net.Dialer` whose `Control` function inspects the
  address actually being connected to and refuses it unless it is public.
  Checking in `Control` (after DNS resolution, per connection attempt) is
  what defeats DNS rebinding; checking the hostname before the request is not
  enough.
- Refuse: unspecified, loopback, private (`IsPrivate`), link local unicast
  and multicast, interface local multicast, multicast, `100.64.0.0/10`,
  `192.0.0.0/24`, `192.0.2.0/24`, `198.18.0.0/15`, `198.51.100.0/24`,
  `203.0.113.0/24`, `240.0.0.0/4`, `64:ff9b::/96`, `2001:db8::/32`. Unmap
  IPv4 mapped IPv6 addresses before checking.
- Refuse any port other than 80 and 443.
- `CheckRedirect`: at most 5 hops, and only `http` and `https` targets.
- Total timeout 15 seconds per request. `MaxResponseHeaderBytes` 64 KiB.
  Disable keep alives.
- Requests carry `User-Agent: Mozilla/5.0 (compatible; msg-preview/1.0)` and
  `Accept: text/html,application/xhtml+xml` (or `image/*` for the image), and
  nothing else. Never cookies, never credentials from the URL.
- The checker is a field (`a.previewAllow func(netip.Addr) bool`) so tests
  can allow the loopback test server. The default is the strict one.

Before fetching, reject URLs that: are not `http` or `https`, have userinfo,
have an empty host, or are longer than 2000 bytes.

## Fetch and parse

- `GET` the URL. Require a 2xx status and a `Content-Type` of `text/html` or
  `application/xhtml+xml`.
- Read at most 1 MiB of the body. Decode to UTF-8 with
  `golang.org/x/net/html/charset.NewReader` using the response content type.
- Tokenise with `golang.org/x/net/html` and stop at `</head>` or `<body>`.
  Collect: `og:title`, `og:description`, `og:image`, `og:site_name`,
  `twitter:title`, `twitter:description`, `twitter:image`, `<meta
  name="description">`, `<title>`.
- Title: `og:title`, else `twitter:title`, else `<title>`. Description:
  `og:description`, else `twitter:description`, else meta description. Site
  name: `og:site_name`, else the final URL's host without a leading `www.`.
- Collapse whitespace, trim, and cut to 200 runes (title), 300 (description),
  60 (site name).
- Image URL: `og:image`, else `twitter:image`, resolved against the final
  URL after redirects; apply the same URL checks.

## Preview image

- `GET` it with the same client. Require 2xx. Read at most 10 MiB; a larger
  body is dropped (the preview is still shown without an image).
- `sniffMedia` on the bytes. Only `jpeg`, `png`, `gif`, `webp` are accepted
  here. Never trust the response content type.
- Write to a temp directory and convert with `a.magickConvert` from task 017
  with a long edge of 1200, taking only the first frame (the `[0]` suffix on
  the input). If `magickConvert` has no way to ask for the first frame yet,
  add a parameter for it.
- Create a `files` row: `kind = 'preview'`, `status = 'ready'`, `size = 0`,
  `name = 'preview'`, chat and user from the message, a fresh `s3_prefix`,
  `conv_type`, `conv_size`, `width`, `height`. Upload to
  `<prefix>/converted` with the usual metadata. Store its id in
  `previews.file_id`.
- Preview files do not touch the upload counters. Their downloads are
  counted by `/f/` like any other file.

## Rendering

After the body of a message with a `ready` preview:

```html
<a id="m123-preview" class="preview" href="https://example.com/post"
  target="_blank" rel="noopener noreferrer nofollow">
  <img id="f42" src="/f/42/c" width="1200" height="630" alt=""
    loading="lazy" decoding="async">
  <span class="preview-site">example.com</span>
  <strong class="preview-title">Title</strong>
  <span class="preview-desc">Description</span>
</a>
```

`pending` and `failed` previews render nothing. Load the previews for the
rendered messages in one query. The `href` is the URL from the message, not
anything from the fetched page. All fetched text goes through `g.Text`.

Queries: `PreviewCreate`, `PreviewClaim`, `PreviewSetReady`,
`PreviewSetFailed`, `PreviewsForMessages`.

## Tests (`media_test.go`, with `httptest` servers and `previewAllow` opened
for loopback unless the test is about blocking)

- Address checker table: `127.0.0.1`, `10.1.2.3`, `172.16.0.1`,
  `192.168.1.1`, `169.254.169.254`, `100.64.0.1`, `0.0.0.0`, `::1`,
  `fe80::1`, `fc00::1`, `::ffff:127.0.0.1`, `::ffff:10.0.0.1`,
  `64:ff9b::7f00:1` all refused; `1.1.1.1`, `93.184.216.34`,
  `2606:4700:4700::1111` allowed.
- With the strict checker, a request to a loopback `httptest` server fails
  and the server sees no connection. A public looking hostname that resolves
  to loopback is refused (use a custom resolver on the dialer, or a name such
  as `localhost`).
- A redirect to a blocked address fails. Six redirects fail. A redirect to
  `file:///etc/passwd` or `gopher://` fails.
- Port 8080 on a public address is refused (unit test the checker).
- URL checks: `ftp://`, `javascript:`, userinfo, empty host, overlong.
- Parser table: Open Graph only, Twitter only, title only, entities and
  nested whitespace, a non UTF-8 page (ISO-8859-1 with a `charset` header),
  meta tags after `</head>` are ignored, 2 MiB of padding before the tags
  gives no title.
- Non HTML content type gives `failed`. A 500 gives `failed`.
- Image: a PNG served as `text/plain` is still used (sniffing wins); an HTML
  file served as `image/png` is not; an 11 MiB image is dropped and the
  preview is `ready` without `file_id`; a relative `og:image` resolves
  against the final URL.
- End to end: sending a message with a URL creates a pending preview, the
  worker makes it `ready`, the group is pinged, the page shows the card with
  escaped title text (serve a title containing `<script>`), and the card's
  `href` equals the URL in the message.
- A file message creates no preview row.
- Editing: same URL keeps the preview; a different URL replaces it and the
  old image object is deleted; removing the URL removes the preview.
- Deleting a message with a ready preview removes the preview row, the image
  file row and the image object.
- Two URLs in one message: only the first is fetched.

## Done when

- In a browser, sending a link to a well known site shows a card within a
  few seconds without a reload.
- With the default configuration there is no way to make the server connect
  to a private address; the tests above demonstrate it.
- `./.hooks/pre-push` passes.
