package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/daaku/serr"
)

// The S3 client is built entirely on presigned URLs (AWS Signature Version 4
// with query parameters). The browser needs presigned URLs to talk to the
// bucket directly, so the server's own calls use the same single signing
// path: presign, then make a plain HTTP request.

const (
	s3Algorithm  = "AWS4-HMAC-SHA256"
	s3Unsigned   = "UNSIGNED-PAYLOAD"
	s3TimeFormat = "20060102T150405Z"
	s3CallExpiry = 15 * time.Minute // lifetime of URLs for the server's own calls
	s3MinPart    = 5 << 20          // S3's minimum size of every part but the last
)

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

type s3Part struct {
	Number int
	ETag   string
	Size   int64
}

// s3Error is a non 2xx answer from the bucket.
type s3Error struct {
	Op     string
	Status int
	Body   string
}

func (e *s3Error) Error() string {
	return fmt.Sprintf("s3 %s: status %d: %s", e.Op, e.Status, e.Body)
}

// s3Escape percent encodes everything outside the unreserved set, as SigV4
// requires. url.QueryEscape is wrong for this: it turns a space into "+".
func s3Escape(s string, keepSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}

func s3CanonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := slices.Clone(q[k])
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, s3Escape(k, false)+"="+s3Escape(v, false))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// host is the Host header value: the port is included only when it is not
// the default of the scheme.
func (c *s3Client) host() string {
	host := c.endpoint.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if !c.pathStyle {
		host = c.bucket + "." + host
	}
	port := c.endpoint.Port()
	if port == "" || (c.endpoint.Scheme == "https" && port == "443") ||
		(c.endpoint.Scheme == "http" && port == "80") {
		return host
	}
	return host + ":" + port
}

// prefix is the URL path in front of every object key.
func (c *s3Client) prefix() string {
	base := strings.TrimSuffix(c.endpoint.Path, "/")
	if c.pathStyle {
		return base + "/" + c.bucket + "/"
	}
	return base + "/"
}

// origin is where browsers load objects from, for the CSP.
func (c *s3Client) origin() string {
	return c.endpoint.Scheme + "://" + c.host()
}

// presign returns a URL that allows method on key, with the extra query
// parameters q, from the signing time at for the duration expires.
func (c *s3Client) presign(method, key string, q url.Values, at time.Time, expires time.Duration) string {
	at = at.UTC()
	date := at.Format("20060102")
	scope := date + "/" + c.region + "/s3/aws4_request"
	params := url.Values{}
	for k, vs := range q {
		params[k] = vs
	}
	params.Set("X-Amz-Algorithm", s3Algorithm)
	params.Set("X-Amz-Credential", c.accessKey+"/"+scope)
	params.Set("X-Amz-Date", at.Format(s3TimeFormat))
	params.Set("X-Amz-Expires", strconv.FormatInt(int64(expires.Seconds()), 10))
	params.Set("X-Amz-SignedHeaders", "host")

	host := c.host()
	uri := s3Escape(c.prefix()+key, true)
	query := s3CanonicalQuery(params)
	canonical := strings.Join([]string{
		method, uri, query, "host:" + host, "", "host", s3Unsigned,
	}, "\n")
	hash := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{
		s3Algorithm, at.Format(s3TimeFormat), scope, hex.EncodeToString(hash[:]),
	}, "\n")
	k := hmacSHA256([]byte("AWS4"+c.secretKey), date)
	k = hmacSHA256(k, c.region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	return c.endpoint.Scheme + "://" + host + uri + "?" + query + "&X-Amz-Signature=" + sig
}

// presignStable returns a GET URL that is identical for everyone during a
// window of validity/2 and valid for at least that long from now. Callers
// may let browsers cache a redirect to it for maxAge. A stable URL is what
// lets the browser cache the object itself.
func (c *s3Client) presignStable(key string, validity time.Duration) (u string, maxAge time.Duration) {
	window := int64((validity / 2).Seconds())
	if window < 1 {
		window = 1
	}
	at := time.Unix(c.now().Unix()/window*window, 0)
	return c.presign(http.MethodGet, key, nil, at, validity), validity / 2
}

// do makes one of the server's own calls.
func (c *s3Client) do(ctx context.Context, op, method, key string, q url.Values, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	u := c.presign(method, key, q, c.now(), s3CallExpiry)
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	if body != nil {
		req.ContentLength = size
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, serr.Wrap(withoutURL(err))
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, serr.Wrap(&s3Error{Op: op, Status: res.StatusCode, Body: string(b)})
	}
	return res, nil
}

// call is do for operations with a small answer, which it returns.
func (c *s3Client) call(ctx context.Context, op, method, key string, q url.Values, body []byte, hdr http.Header) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	res, err := c.do(ctx, op, method, key, q, r, int64(len(body)), hdr)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return b, serr.Wrap(err)
}

func s3Status(err error) int {
	if se, ok := errors.AsType[*s3Error](err); ok {
		return se.Status
	}
	return 0
}

// put stores an object. meta carries Content-Type, Cache-Control and
// Content-Disposition, which become object metadata.
func (c *s3Client) put(ctx context.Context, key string, body io.Reader, size int64, meta http.Header) error {
	res, err := c.do(ctx, "put", http.MethodPut, key, nil, body, size, meta)
	if err != nil {
		return err
	}
	return serr.Wrap(res.Body.Close())
}

// get returns the object's content. rangeHeader may be empty.
func (c *s3Client) get(ctx context.Context, key, rangeHeader string) (io.ReadCloser, error) {
	var hdr http.Header
	if rangeHeader != "" {
		hdr = http.Header{"Range": {rangeHeader}}
	}
	res, err := c.do(ctx, "get", http.MethodGet, key, nil, nil, 0, hdr)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// del removes an object. A missing object is not an error.
func (c *s3Client) del(ctx context.Context, key string) error {
	_, err := c.call(ctx, "delete", http.MethodDelete, key, nil, nil, nil)
	if s3Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *s3Client) createMultipart(ctx context.Context, key string, meta http.Header) (uploadID string, err error) {
	b, err := c.call(ctx, "create multipart", http.MethodPost, key, url.Values{"uploads": {""}}, nil, meta)
	if err != nil {
		return "", err
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(b, &out); err != nil || out.UploadID == "" {
		return "", serr.Errorf("s3 create multipart: no upload id in %q", b)
	}
	return out.UploadID, nil
}

func s3PartQuery(uploadID string, n int) url.Values {
	return url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {uploadID}}
}

// presignPart returns the URL a browser PUTs part n to.
func (c *s3Client) presignPart(key, uploadID string, n int, expires time.Duration) string {
	return c.presign(http.MethodPut, key, s3PartQuery(uploadID, n), c.now(), expires)
}

func (c *s3Client) listParts(ctx context.Context, key, uploadID string) ([]s3Part, error) {
	b, err := c.call(ctx, "list parts", http.MethodGet, key, url.Values{"uploadId": {uploadID}}, nil, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		IsTruncated bool `xml:"IsTruncated"`
		Parts       []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
			Size       int64  `xml:"Size"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(b, &out); err != nil {
		return nil, serr.Wrap(err)
	}
	if out.IsTruncated {
		// We never have more than a few dozen parts.
		return nil, serr.Errorf("s3 list parts: truncated listing")
	}
	parts := make([]s3Part, len(out.Parts))
	for i, p := range out.Parts {
		parts[i] = s3Part{Number: p.PartNumber, ETag: p.ETag, Size: p.Size}
	}
	return parts, nil
}

type s3CompletePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type s3Complete struct {
	XMLName xml.Name         `xml:"CompleteMultipartUpload"`
	Parts   []s3CompletePart `xml:"Part"`
}

func (c *s3Client) completeMultipart(ctx context.Context, key, uploadID string, parts []s3Part) error {
	var in s3Complete
	for _, p := range parts {
		in.Parts = append(in.Parts, s3CompletePart{PartNumber: p.Number, ETag: p.ETag})
	}
	body, err := xml.Marshal(in)
	if err != nil {
		return serr.Wrap(err)
	}
	b, err := c.call(ctx, "complete multipart", http.MethodPost, key,
		url.Values{"uploadId": {uploadID}}, body, http.Header{"Content-Type": {"application/xml"}})
	if err != nil {
		return err
	}
	// S3 can answer 200 and still report a failure in the body.
	var root struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if err := xml.Unmarshal(b, &root); err != nil {
		return serr.Errorf("s3 complete multipart: unreadable answer %q", b)
	}
	if root.XMLName.Local == "Error" {
		return serr.Wrap(&s3Error{Op: "complete multipart", Status: http.StatusOK,
			Body: root.Code + ": " + root.Message})
	}
	return nil
}

// abortMultipart drops an unfinished upload. A missing upload is not an error.
func (c *s3Client) abortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := c.call(ctx, "abort multipart", http.MethodDelete, key, url.Values{"uploadId": {uploadID}}, nil, nil)
	if s3Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// selfCheck proves at startup that the bucket works end to end, including
// that the provider accepts signed URLs of the configured validity.
func (c *s3Client) selfCheck(ctx context.Context, validity time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key := "selfcheck/" + hex.EncodeToString(randBytes(8))
	want := []byte("msg self check " + key)
	if err := c.put(ctx, key, bytes.NewReader(want), int64(len(want)), nil); err != nil {
		return fmt.Errorf("S3 self check: storing an object failed: %w", err)
	}
	defer func() { _ = c.del(context.WithoutCancel(ctx), key) }()

	u, _ := c.presignStable(key, validity)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return serr.Wrap(err)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("S3 self check: fetching through a signed URL failed: %w", withoutURL(err))
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK {
		hint := ""
		if res.StatusCode >= 400 && res.StatusCode < 500 {
			hint = fmt.Sprintf(" Your S3 provider may not accept signed URLs valid for %s; "+
				"AWS S3, Cloudflare R2 and MinIO cap SigV4 at 7 days. Set S3_URL_VALIDITY=7d.", validity)
		}
		return fmt.Errorf("S3 self check: fetching through a signed URL failed with status %d: %s.%s",
			res.StatusCode, strings.TrimSpace(string(got)), hint)
	}
	if !bytes.Equal(got, want) {
		return errors.New("S3 self check: the object read back differs from what was stored")
	}
	if err := c.del(ctx, key); err != nil {
		return fmt.Errorf("S3 self check: deleting an object failed: %w", err)
	}
	return nil
}

// newS3 builds the client from configuration. With S3_ENDPOINT=fake it also
// returns the built in fake, which the caller mounts at pFakeS3.
func newS3(cfg config, now func() time.Time) (*s3Client, *fakeS3, error) {
	switch cfg.S3Endpoint {
	case "":
		return nil, nil, errors.New("S3 is not configured: set S3_ENDPOINT, S3_BUCKET, " +
			"S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY, or S3_ENDPOINT=fake for development")
	case "fake":
		endpoint, err := url.Parse(cfg.PublicURL + strings.TrimSuffix(pFakeS3, "/"))
		if err != nil {
			return nil, nil, serr.Wrap(err)
		}
		c := &s3Client{
			endpoint:  endpoint,
			region:    cfg.S3Region,
			bucket:    "dev",
			accessKey: "fake",
			secretKey: "fake",
			pathStyle: true,
			now:       now,
		}
		fake := &fakeS3{dir: filepath.Join(cfg.StateDir, "fakes3"), c: c}
		// The server's own calls run the fake in process, so they do not
		// depend on the listen address.
		c.hc = &http.Client{Transport: handlerTransport{fake}}
		return c, fake, nil
	}
	var missing []string
	for name, v := range map[string]string{
		"S3_BUCKET":            cfg.S3Bucket,
		"S3_ACCESS_KEY_ID":     cfg.S3AccessKeyID,
		"S3_SECRET_ACCESS_KEY": cfg.S3SecretAccessKey,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, nil, fmt.Errorf("S3 is not configured: missing %s", strings.Join(missing, ", "))
	}
	endpoint, err := url.Parse(cfg.S3Endpoint)
	if err != nil {
		return nil, nil, serr.Wrap(err)
	}
	return &s3Client{
		endpoint:  endpoint,
		region:    cfg.S3Region,
		bucket:    cfg.S3Bucket,
		accessKey: cfg.S3AccessKeyID,
		secretKey: cfg.S3SecretAccessKey,
		pathStyle: cfg.S3PathStyle,
		hc:        &http.Client{},
		now:       now,
	}, nil, nil
}

// handlerTransport is an http.RoundTripper that runs a handler in process.
// The response body streams, so large objects are not buffered.
type handlerTransport struct{ h http.Handler }

type pipeResponse struct {
	header http.Header
	status int
	pw     *io.PipeWriter
	ready  chan struct{}
	wrote  bool
}

func (w *pipeResponse) Header() http.Header { return w.header }

func (w *pipeResponse) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = status
	close(w.ready)
}

func (w *pipeResponse) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(b)
}

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		req.Body = http.NoBody
	}
	pr, pw := io.Pipe()
	w := &pipeResponse{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	var sent http.Header
	go func() {
		defer req.Body.Close()
		t.h.ServeHTTP(w, req)
		w.WriteHeader(http.StatusOK)
		pw.Close()
	}()
	select {
	case <-w.ready:
		sent = w.header.Clone()
	case <-req.Context().Done():
		pr.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	return &http.Response{
		Status:        strconv.Itoa(w.status) + " " + http.StatusText(w.status),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        sent,
		Body:          pr,
		ContentLength: -1,
		Request:       req,
	}, nil
}

// fakeS3 is a small S3 compatible server that stores objects on disk. It
// exists so that `go run .` and `go test` need nothing installed. It is for
// development only. It speaks path style and verifies every signature with
// the same code that made it, which catches differences between what was
// signed and what was sent.
type fakeS3 struct {
	dir string
	c   *s3Client
}

type fakeMeta struct {
	Key    string            `json:"key,omitempty"`
	Header map[string]string `json:"header"`
	ETag   string            `json:"etag,omitempty"`
}

var fakeMetaHeaders = []string{"Content-Type", "Cache-Control", "Content-Disposition"}

func fakeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string
		Message string
	}{Code: code, Message: msg})
}

func fakeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(v)
}

// verify checks the signature and the expiry of a request and returns the
// object key and the query parameters that are not part of the signature.
func (f *fakeS3) verify(r *http.Request) (key string, q url.Values, ok bool) {
	key, found := strings.CutPrefix(r.URL.Path, f.c.prefix())
	if !found || key == "" || !filepath.IsLocal(key) || strings.Contains(key, "\\") {
		return "", nil, false
	}
	all := r.URL.Query()
	q = url.Values{}
	for k, vs := range all {
		if !strings.HasPrefix(k, "X-Amz-") {
			q[k] = vs
		}
	}
	at, err := time.Parse(s3TimeFormat, all.Get("X-Amz-Date"))
	if err != nil {
		return "", nil, false
	}
	secs, err := strconv.ParseInt(all.Get("X-Amz-Expires"), 10, 64)
	if err != nil || secs <= 0 {
		return "", nil, false
	}
	expires := time.Duration(secs) * time.Second
	if !f.c.now().Before(at.Add(expires)) {
		return "", nil, false
	}
	want, err := url.Parse(f.c.presign(r.Method, key, q, at, expires))
	if err != nil {
		return "", nil, false
	}
	a, b := want.Query().Get("X-Amz-Signature"), all.Get("X-Amz-Signature")
	if subtle.ConstantTimeCompare([]byte(a), []byte(b)) != 1 {
		return "", nil, false
	}
	return key, q, true
}

func (f *fakeS3) objectPath(key string) string { return filepath.Join(f.dir, "objects", key) }
func (f *fakeS3) metaPath(key string) string   { return filepath.Join(f.dir, "meta", key+".json") }
func (f *fakeS3) uploadDir(id string) string   { return filepath.Join(f.dir, "uploads", id) }

func metaFrom(h http.Header) map[string]string {
	m := map[string]string{}
	for _, name := range fakeMetaHeaders {
		if v := h.Get(name); v != "" {
			m[name] = v
		}
	}
	return m
}

func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// saveBody writes the request body to path and returns its quoted MD5.
func saveBody(path string, body io.Reader) (etag string, size int64, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	sum := md5.New()
	size, err = io.Copy(io.MultiWriter(tmp, sum), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	return `"` + hex.EncodeToString(sum.Sum(nil)) + `"`, size, os.Rename(tmp.Name(), path)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, q, ok := f.verify(r)
	if !ok {
		fakeError(w, http.StatusForbidden, "SignatureDoesNotMatch",
			"The request signature is missing, wrong or expired.")
		return
	}
	var err error
	switch {
	case q.Has("uploads") && r.Method == http.MethodPost:
		err = f.createUpload(w, r, key)
	case q.Has("uploadId"):
		err = f.upload(w, r, key, q)
	case r.Method == http.MethodPut:
		err = f.putObject(w, r, key)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		err = f.getObject(w, r, key)
	case r.Method == http.MethodDelete:
		_ = os.Remove(f.metaPath(key))
		if err = os.Remove(f.objectPath(key)); errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		fakeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Unsupported request.")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakes3 %s %s: %v\n", r.Method, key, err)
		fakeError(w, http.StatusInternalServerError, "InternalError", err.Error())
	}
}

func (f *fakeS3) putObject(w http.ResponseWriter, r *http.Request, key string) error {
	etag, _, err := saveBody(f.objectPath(key), r.Body)
	if err != nil {
		return err
	}
	if err := writeJSONFile(f.metaPath(key), fakeMeta{Header: metaFrom(r.Header), ETag: etag}); err != nil {
		return err
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (f *fakeS3) getObject(w http.ResponseWriter, r *http.Request, key string) error {
	file, err := os.Open(f.objectPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		fakeError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	var meta fakeMeta
	_ = readJSONFile(f.metaPath(key), &meta)
	for name, v := range meta.Header {
		w.Header().Set(name, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "binary/octet-stream")
	}
	if meta.ETag != "" {
		w.Header().Set("ETag", meta.ETag)
	}
	http.ServeContent(w, r, "", stat.ModTime(), file)
	return nil
}

func (f *fakeS3) createUpload(w http.ResponseWriter, r *http.Request, key string) error {
	id := hex.EncodeToString(randBytes(16))
	meta := fakeMeta{Key: key, Header: metaFrom(r.Header)}
	if err := writeJSONFile(filepath.Join(f.uploadDir(id), "meta.json"), meta); err != nil {
		return err
	}
	fakeXML(w, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string
		Key      string
		UploadID string `xml:"UploadId"`
	}{Bucket: f.c.bucket, Key: key, UploadID: id})
	return nil
}

type fakePart struct {
	n    int
	etag string
	size int64
	path string
}

func (f *fakeS3) parts(id string) ([]fakePart, error) {
	entries, err := os.ReadDir(f.uploadDir(id))
	if err != nil {
		return nil, err
	}
	var parts []fakePart
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // meta.json and etag files
		}
		path := filepath.Join(f.uploadDir(id), e.Name())
		stat, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		etag, err := os.ReadFile(path + ".etag")
		if err != nil {
			return nil, err
		}
		parts = append(parts, fakePart{n: n, etag: string(etag), size: stat.Size(), path: path})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].n < parts[j].n })
	return parts, nil
}

// upload handles the requests that address an existing multipart upload.
func (f *fakeS3) upload(w http.ResponseWriter, r *http.Request, key string, q url.Values) error {
	id := q.Get("uploadId")
	var meta fakeMeta
	if _, err := hex.DecodeString(id); err != nil || len(id) != 32 ||
		readJSONFile(filepath.Join(f.uploadDir(id), "meta.json"), &meta) != nil || meta.Key != key {
		fakeError(w, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.")
		return nil
	}
	switch {
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, err := strconv.Atoi(q.Get("partNumber"))
		if err != nil || n < 1 || n > 10000 {
			fakeError(w, http.StatusBadRequest, "InvalidArgument", "Bad part number.")
			return nil
		}
		path := filepath.Join(f.uploadDir(id), strconv.Itoa(n))
		etag, _, err := saveBody(path, r.Body)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".etag", []byte(etag), 0o600); err != nil {
			return err
		}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		return nil

	case r.Method == http.MethodGet:
		parts, err := f.parts(id)
		if err != nil {
			return err
		}
		type part struct {
			PartNumber int
			ETag       string
			Size       int64
		}
		out := struct {
			XMLName     xml.Name `xml:"ListPartsResult"`
			Key         string
			UploadID    string `xml:"UploadId"`
			IsTruncated bool
			Part        []part
		}{Key: key, UploadID: id}
		for _, p := range parts {
			out.Part = append(out.Part, part{PartNumber: p.n, ETag: p.etag, Size: p.size})
		}
		fakeXML(w, out)
		return nil

	case r.Method == http.MethodPost:
		return f.completeUpload(w, r, key, id, meta)

	case r.Method == http.MethodDelete:
		if err := os.RemoveAll(f.uploadDir(id)); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	fakeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Unsupported request.")
	return nil
}

func (f *fakeS3) completeUpload(w http.ResponseWriter, r *http.Request, key, id string, meta fakeMeta) error {
	var in s3Complete
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := xml.Unmarshal(body, &in); err != nil || len(in.Parts) == 0 {
		fakeError(w, http.StatusBadRequest, "MalformedXML", "The XML is not well formed.")
		return nil
	}
	have, err := f.parts(id)
	if err != nil {
		return err
	}
	byNumber := map[int]fakePart{}
	for _, p := range have {
		byNumber[p.n] = p
	}
	var chosen []fakePart
	for i, want := range in.Parts {
		p, ok := byNumber[want.PartNumber]
		if !ok || p.etag != want.ETag {
			fakeError(w, http.StatusBadRequest, "InvalidPart", "A part is missing or its ETag does not match.")
			return nil
		}
		if i > 0 && want.PartNumber <= in.Parts[i-1].PartNumber {
			fakeError(w, http.StatusBadRequest, "InvalidPartOrder", "Parts must be in ascending order.")
			return nil
		}
		if i < len(in.Parts)-1 && p.size < s3MinPart {
			fakeError(w, http.StatusBadRequest, "EntityTooSmall", "A part other than the last is under 5 MiB.")
			return nil
		}
		chosen = append(chosen, p)
	}
	readers := make([]io.Reader, 0, len(chosen))
	for _, p := range chosen {
		file, err := os.Open(p.path)
		if err != nil {
			return err
		}
		defer file.Close()
		readers = append(readers, file)
	}
	etag, _, err := saveBody(f.objectPath(key), io.MultiReader(readers...))
	if err != nil {
		return err
	}
	if err := writeJSONFile(f.metaPath(key), fakeMeta{Header: meta.Header, ETag: etag}); err != nil {
		return err
	}
	if err := os.RemoveAll(f.uploadDir(id)); err != nil {
		return err
	}
	fakeXML(w, struct {
		XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
		Bucket  string
		Key     string
		ETag    string
	}{Bucket: f.c.bucket, Key: key, ETag: etag})
	return nil
}
