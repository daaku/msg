# 014 S3 client with SigV4 presigning, and a fake S3 for dev and tests

Use fable.

Write `s3.go`: a small S3 client built entirely on presigned URLs, and an in
process fake S3 server so the app and its tests run without any external
service. No AWS SDK and no other S3 library; this is a few hundred lines and
we need exact control over the URLs.

## Why presigned URLs for everything

The browser must upload to and download from S3 directly, so we need query
string signing anyway. Every server side call (create multipart, list parts,
complete, get, put, delete) is then just "presign, then do a plain HTTP
request", which keeps one signing code path.

## Client

```go
type s3Client struct {
    endpoint  *url.URL // scheme://host[:port][/basepath]
    region    string
    bucket    string
    accessKey string
    secretKey string
    pathStyle bool
    hc        *http.Client
    now       func() time.Time
}

type s3Part struct { Number int; ETag string; Size int64 }

func (c *s3Client) origin() string                      // for the CSP
func (c *s3Client) presign(method, key string, q url.Values, at time.Time, expires time.Duration) string
func (c *s3Client) presignStable(key string, validity time.Duration) (u string, maxAge time.Duration)
func (c *s3Client) put(ctx, key string, body io.Reader, size int64, meta http.Header) error
func (c *s3Client) get(ctx, key, rangeHeader string) (io.ReadCloser, error)
func (c *s3Client) del(ctx, key string) error
func (c *s3Client) createMultipart(ctx, key string, meta http.Header) (uploadID string, err error)
func (c *s3Client) presignPart(key, uploadID string, n int, expires time.Duration) string
func (c *s3Client) listParts(ctx, key, uploadID string) ([]s3Part, error)
func (c *s3Client) completeMultipart(ctx, key, uploadID string, parts []s3Part) error
func (c *s3Client) abortMultipart(ctx, key, uploadID string) error
func (c *s3Client) selfCheck(ctx context.Context, validity time.Duration) error
```

**Signing (AWS Signature Version 4, query parameters).**

- Path style URL: `endpoint/bucket/key`. Virtual hosted: `bucket.host/key`.
- Query parameters added: `X-Amz-Algorithm=AWS4-HMAC-SHA256`,
  `X-Amz-Credential=<access>/<yyyymmdd>/<region>/s3/aws4_request`,
  `X-Amz-Date=<yyyymmddThhmmssZ>`, `X-Amz-Expires=<seconds>`,
  `X-Amz-SignedHeaders=host`, then `X-Amz-Signature`.
- Canonical request: method, canonical URI, canonical query string,
  `host:<host>\n`, empty line, `host`, `UNSIGNED-PAYLOAD`.
- Canonical URI: each path segment percent encoded once with the unreserved
  set `A-Z a-z 0-9 - . _ ~`; `/` kept. Canonical query: every name and value
  encoded with the same set (space is `%20`, never `+`), sorted by name.
  `url.QueryEscape` is wrong for this; write the encoder.
- The host in the signature includes the port when it is not the scheme's
  default.
- Only `host` is signed. Requests we make ourselves may add `Content-Type`,
  `Cache-Control` and `Content-Disposition` headers unsigned. Never add an
  `x-amz-*` header, those would have to be signed.

This vector must pass as a unit test (it is the example from the AWS
documentation and has been recomputed for this plan): access key
`AKIAIOSFODNN7EXAMPLE`, secret `wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`,
region `us-east-1`, virtual hosted bucket `examplebucket` on
`s3.amazonaws.com`, `GET` key `test.txt`, time `20130524T000000Z`, expires
86400 seconds. Signature:
`aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404`.

**Stable URLs.** `presignStable` signs a `GET` with the signing time rounded
down to a multiple of `validity/2` (since the Unix epoch) and
`X-Amz-Expires = validity`. So a URL is identical for everyone during a
window of `validity/2` and is always valid for at least `validity/2` from
now. Return `maxAge = validity/2`: callers may let browsers cache a redirect
to this URL for that long. A signing time in the past is legal in SigV4.

**Operations.**

- `put`: `PUT` with `Content-Length`; `meta` headers are sent as is.
- `get`: returns the body; a non 2xx is an error that includes the status and
  the first 512 bytes of the body. `rangeHeader` may be empty.
- `createMultipart`: `POST ?uploads=` with `meta` headers; parse `UploadId`
  from the XML.
- `presignPart`: `PUT ?partNumber=N&uploadId=ID`.
- `listParts`: `GET ?uploadId=ID`; parse `Part` elements (`PartNumber`,
  `ETag`, `Size`). If `IsTruncated` is true return an error: we never have
  more than a few dozen parts.
- `completeMultipart`: `POST ?uploadId=ID` with the XML body
  `<CompleteMultipartUpload><Part><PartNumber>..</PartNumber><ETag>..</ETag></Part>..</CompleteMultipartUpload>`.
  S3 can answer 200 with an `<Error>` document in the body. Read the body and
  treat a root element `Error` as failure.
- `abortMultipart`: `DELETE ?uploadId=ID`; a 404 is success.
- All take a context and use `c.hc`, whose timeout is unset; callers pass
  contexts with deadlines.

**`selfCheck`**: put a small object `selfcheck/<random>`, fetch it through
`presignStable(key, validity)` with a plain `http.Get`, compare the bytes,
delete it. On failure return an error that says which step failed and, when
the fetch step fails with 4xx, adds: "your S3 provider may not accept signed
URLs valid for <validity>; AWS S3, Cloudflare R2 and MinIO cap SigV4 at 7
days. Set S3_URL_VALIDITY=7d." Run it at startup and refuse to start on
failure.

## Fake S3

`type fakeS3 struct { dir string; c *s3Client }` implementing `http.Handler`,
path style only, storing objects under `dir`. It exists so that `go run .`
and `go test` need nothing installed.

- Verify every request: recompute the signature from the request's own
  `X-Amz-*` parameters with the same signing function and compare in constant
  time; reject when `X-Amz-Date + X-Amz-Expires` is in the past. 403 with an
  S3 style XML error otherwise. This makes the tests catch canonicalisation
  bugs between "what we signed" and "what was sent".
- Implement: `PUT` object, `GET` and `HEAD` object (use `http.ServeContent`
  so `Range` works), `DELETE` object, `POST ?uploads`, `PUT
  ?partNumber&uploadId`, `GET ?uploadId`, `POST ?uploadId` (complete),
  `DELETE ?uploadId`.
- Store `Content-Type`, `Cache-Control` and `Content-Disposition` given at
  `PUT` or at create multipart in a JSON sidecar file and return them on
  `GET`.
- Part ETags are the quoted hex MD5 of the part. On complete, reject with
  `EntityTooSmall` if any part other than the last is under 5 MiB, and reject
  parts whose ETag does not match, as real S3 does.
- Map keys to file paths safely: the key is hex and slashes in our use, but
  reject any key containing `..` or a leading `/`.

**Wiring in `main.go`.** When `S3_ENDPOINT=fake`: create the fake with
`dir = <STATE_DIRECTORY>/fakes3`, mount it on the mux at `/fakes3/` (public,
outside `a.wrap`), and build the client with `endpoint = PUBLIC_URL +
"/fakes3"`, bucket `dev`, fixed dummy keys, and an `http.Client` whose
`Transport` calls the fake handler directly in process (a `RoundTripper` that
runs the handler against an `httptest.ResponseRecorder`), so server side
calls do not depend on the listen address. Log one line: `using built in
fake S3 in <dir>; not for production`. Otherwise require `S3_BUCKET`,
`S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` and build a real client.
`S3_ENDPOINT` unset is a startup error that lists the variables. Add
`s3 *s3Client` to `app`, and feed `s3.origin()` into the CSP from task 001.
`newTestApp` uses the fake.

## Tests (`s3_test.go`)

- The AWS vector above.
- Encoding: keys with spaces, `+`, `&`, unicode and `~`; query values with
  `/` and `=`.
- Path style and virtual hosted URL shapes; non default port in host.
- `presignStable`: two calls within a window give the same URL; calls in
  adjacent windows differ; remaining validity is at least `validity/2` at the
  end of a window (drive `now`).
- Against the fake: put, get, ranged get, delete, get after delete fails; a
  full multipart flow with two parts (6 MiB and 1 MiB) where the parts are
  uploaded with a plain `http.Client` to the presigned part URLs, then
  `listParts` sizes and `completeMultipart`, then `get` returns the
  concatenation; metadata set at create is returned on get; abort then list
  fails; complete with a small non final part fails.
- The fake rejects: a tampered signature, a changed key, an expired URL, a
  different method than was signed.
- `completeMultipart` treats a 200 response with an `<Error>` body as an
  error (use a stub `RoundTripper`).
- `selfCheck` passes against the fake and returns the 7 day hint when the
  fetch step gets a 403 (stub).

## Done when

- `S3_ENDPOINT=fake DEV=1 go run .` starts and logs the fake S3 line; without
  `S3_ENDPOINT` it exits with a clear message.
- If you have credentials for a real S3 compatible bucket, run `selfCheck`
  and the multipart test against it and report the provider and the result.
  If not, say so.
- `./.hooks/pre-push` passes.
