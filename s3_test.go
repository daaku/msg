package main

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/daaku/ensure"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	ensure.Nil(t, err)
	return u
}

// The example from the AWS documentation "Authenticating Requests: Using
// Query Parameters (AWS Signature Version 4)".
func TestS3PresignAWSVector(t *testing.T) {
	c := &s3Client{
		endpoint:  mustURL(t, "https://s3.amazonaws.com"),
		region:    "us-east-1",
		bucket:    "examplebucket",
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got := c.presign(http.MethodGet, "test.txt", nil, at, 24*time.Hour)
	ensure.DeepEqual(t, got, "https://examplebucket.s3.amazonaws.com/test.txt"+
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256"+
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request"+
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host"+
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404")
}

func TestS3Escape(t *testing.T) {
	ensure.DeepEqual(t, s3Escape("AZaz09-._~", false), "AZaz09-._~")
	ensure.DeepEqual(t, s3Escape("a b+c&d=e/f", false), "a%20b%2Bc%26d%3De%2Ff")
	ensure.DeepEqual(t, s3Escape("a b/c d", true), "a%20b/c%20d")
	ensure.DeepEqual(t, s3Escape("é☃", false), "%C3%A9%E2%98%83")
	ensure.DeepEqual(t, s3CanonicalQuery(url.Values{
		"b": {"2", "1"}, "a": {"x/y=z w"}, "uploads": {""},
	}), "a=x%2Fy%3Dz%20w&b=1&b=2&uploads=")
}

func TestS3URLShapes(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c := &s3Client{endpoint: mustURL(t, "https://s3.example.com"), region: "auto",
		bucket: "b", accessKey: "k", secretKey: "s", pathStyle: true}
	u := c.presign(http.MethodGet, "dir/a b+c~é.txt", nil, at, time.Hour)
	ensure.True(t, strings.HasPrefix(u, "https://s3.example.com/b/dir/a%20b%2Bc~%C3%A9.txt?"), u)
	ensure.DeepEqual(t, c.origin(), "https://s3.example.com")

	c.pathStyle = false
	u = c.presign(http.MethodGet, "k", nil, at, time.Hour)
	ensure.True(t, strings.HasPrefix(u, "https://b.s3.example.com/k?"), u)
	ensure.DeepEqual(t, c.origin(), "https://b.s3.example.com")

	// a non default port is part of the host, a default one is not
	c = &s3Client{endpoint: mustURL(t, "http://localhost:9000"), region: "auto",
		bucket: "b", accessKey: "k", secretKey: "s", pathStyle: true}
	ensure.DeepEqual(t, c.host(), "localhost:9000")
	ensure.True(t, strings.HasPrefix(c.presign(http.MethodGet, "k", nil, at, time.Hour),
		"http://localhost:9000/b/k?"))
	c.endpoint = mustURL(t, "https://s3.example.com:443")
	ensure.DeepEqual(t, c.host(), "s3.example.com")

	// different inputs give different signatures
	sig := func(u string) string { return mustURL(t, u).Query().Get("X-Amz-Signature") }
	base := sig(c.presign(http.MethodGet, "k", nil, at, time.Hour))
	ensure.DeepEqual(t, len(base), 64)
	ensure.True(t, base != sig(c.presign(http.MethodPut, "k", nil, at, time.Hour)))
	ensure.True(t, base != sig(c.presign(http.MethodGet, "k2", nil, at, time.Hour)))
	ensure.True(t, base != sig(c.presign(http.MethodGet, "k", url.Values{"uploadId": {"x"}}, at, time.Hour)))
	ensure.True(t, base != sig(c.presign(http.MethodGet, "k", nil, at, 2*time.Hour)))
}

func TestS3ConfigOriginMatchesClient(t *testing.T) {
	for _, pathStyle := range []string{"true", "false"} {
		cfg, err := parseConfig(func(k string) string {
			return map[string]string{
				"S3_ENDPOINT": "https://s3.example.com", "S3_BUCKET": "media",
				"S3_ACCESS_KEY_ID": "k", "S3_SECRET_ACCESS_KEY": "s", "S3_PATH_STYLE": pathStyle,
			}[k]
		})
		ensure.Nil(t, err)
		c, fake, err := newS3(cfg, time.Now)
		ensure.Nil(t, err)
		ensure.True(t, fake == nil)
		ensure.DeepEqual(t, c.origin(), cfg.s3Origin())
		ensure.StringContains(t, cfg.csp(), "img-src 'self' data: "+c.origin())
	}
}

func TestS3NotConfigured(t *testing.T) {
	cfg, err := parseConfig(func(string) string { return "" })
	ensure.Nil(t, err)
	_, _, err = newS3(cfg, time.Now)
	ensure.StringContains(t, err.Error(), "S3_ENDPOINT")
	ensure.StringContains(t, err.Error(), "S3_ENDPOINT=fake")

	cfg, err = parseConfig(func(k string) string {
		return map[string]string{"S3_ENDPOINT": "https://s3.example.com", "S3_BUCKET": "b"}[k]
	})
	ensure.Nil(t, err)
	_, _, err = newS3(cfg, time.Now)
	ensure.StringContains(t, err.Error(), "S3_ACCESS_KEY_ID, S3_SECRET_ACCESS_KEY")
}

func TestS3PresignStable(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	c := &s3Client{endpoint: mustURL(t, "https://s3.example.com"), region: "auto",
		bucket: "b", accessKey: "k", secretKey: "s", pathStyle: true,
		now: func() time.Time { return now }}
	validity := 7 * 24 * time.Hour
	window := validity / 2
	start := time.Unix(now.Unix()/int64(window.Seconds())*int64(window.Seconds()), 0)

	now = start
	first, maxAge := c.presignStable("k", validity)
	ensure.DeepEqual(t, maxAge, window)
	now = start.Add(window - time.Second)
	last, _ := c.presignStable("k", validity)
	ensure.DeepEqual(t, first, last)

	// at the very end of a window at least half the validity remains
	q := mustURL(t, last).Query()
	signedAt, err := time.Parse(s3TimeFormat, q.Get("X-Amz-Date"))
	ensure.Nil(t, err)
	ensure.DeepEqual(t, q.Get("X-Amz-Expires"), "604800")
	ensure.True(t, signedAt.Add(validity).Sub(now) >= window)

	now = start.Add(window)
	next, _ := c.presignStable("k", validity)
	ensure.True(t, next != first)
}

func randData(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	ensure.Nil(t, err)
	return b
}

func readAll(t *testing.T, rc io.ReadCloser, err error) []byte {
	t.Helper()
	ensure.Nil(t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	ensure.Nil(t, err)
	return b
}

// plainHTTP is a client like a browser: it reaches the fake over the app's
// handler, not through the client's in process transport.
func (ta *testApp) plainHTTP(method, rawURL string, body []byte) *httptest.ResponseRecorder {
	ta.t.Helper()
	u, err := url.Parse(rawURL)
	ensure.Nil(ta.t, err)
	r := httptest.NewRequest(method, u.RequestURI(), bytes.NewReader(body))
	w := httptest.NewRecorder()
	ta.h.ServeHTTP(w, r)
	return w
}

func TestFakeS3Objects(t *testing.T) {
	ta := newTestApp(t)
	c, ctx := ta.app.s3, t.Context()
	data := randData(t, 3000)
	meta := http.Header{
		"Content-Type":        {"image/webp"},
		"Cache-Control":       {"private, max-age=31536000, immutable"},
		"Content-Disposition": {"inline"},
	}
	ensure.Nil(t, c.put(ctx, "ab/cd/original", bytes.NewReader(data), int64(len(data)), meta))
	rc, err := c.get(ctx, "ab/cd/original", "")
	ensure.DeepEqual(t, readAll(t, rc, err), data)
	rc, err = c.get(ctx, "ab/cd/original", "bytes=10-19")
	ensure.DeepEqual(t, readAll(t, rc, err), data[10:20])

	// a browser gets the stored metadata through a signed URL
	u, maxAge := c.presignStable("ab/cd/original", time.Hour)
	ensure.DeepEqual(t, maxAge, 30*time.Minute)
	w := ta.plainHTTP(http.MethodGet, u, nil)
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.DeepEqual(t, w.Body.Bytes(), data)
	for name, v := range meta {
		ensure.DeepEqual(t, w.Header().Get(name), v[0])
	}

	_, err = c.get(ctx, "missing", "")
	ensure.DeepEqual(t, s3Status(err), http.StatusNotFound)
	ensure.StringContains(t, err.Error(), "NoSuchKey")

	ensure.Nil(t, c.del(ctx, "ab/cd/original"))
	_, err = c.get(ctx, "ab/cd/original", "")
	ensure.DeepEqual(t, s3Status(err), http.StatusNotFound)
	ensure.Nil(t, c.del(ctx, "ab/cd/original"), "deleting a missing object is fine")
}

func TestFakeS3Multipart(t *testing.T) {
	ta := newTestApp(t)
	c, ctx := ta.app.s3, t.Context()
	key := "0123abcd/original"
	meta := http.Header{
		"Content-Type":        {"application/octet-stream"},
		"Content-Disposition": {"attachment; filename*=UTF-8''a%20b.bin"},
	}
	id, err := c.createMultipart(ctx, key, meta)
	ensure.Nil(t, err)

	p1, p2 := randData(t, 6<<20), randData(t, 1<<20)
	for i, p := range [][]byte{p1, p2} {
		w := ta.plainHTTP(http.MethodPut, c.presignPart(key, id, i+1, time.Hour), p)
		ensure.DeepEqual(t, w.Code, http.StatusOK, w.Body.String())
		ensure.True(t, w.Header().Get("ETag") != "")
	}
	parts, err := c.listParts(ctx, key, id)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, len(parts), 2)
	ensure.DeepEqual(t, []int{parts[0].Number, parts[1].Number}, []int{1, 2})
	ensure.DeepEqual(t, []int64{parts[0].Size, parts[1].Size}, []int64{6 << 20, 1 << 20})

	// the object does not exist until the upload is completed
	_, err = c.get(ctx, key, "")
	ensure.DeepEqual(t, s3Status(err), http.StatusNotFound)

	ensure.Nil(t, c.completeMultipart(ctx, key, id, parts))
	rc, err := c.get(ctx, key, "")
	ensure.DeepEqual(t, readAll(t, rc, err), append(slicesClone(p1), p2...))

	u, _ := c.presignStable(key, time.Hour)
	w := ta.plainHTTP(http.MethodGet, u, nil)
	ensure.DeepEqual(t, w.Header().Get("Content-Disposition"), meta.Get("Content-Disposition"))
	ensure.DeepEqual(t, w.Header().Get("Content-Type"), "application/octet-stream")

	// the upload is gone afterwards
	_, err = c.listParts(ctx, key, id)
	ensure.DeepEqual(t, s3Status(err), http.StatusNotFound)
}

func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }

func TestFakeS3MultipartFailures(t *testing.T) {
	ta := newTestApp(t)
	c, ctx := ta.app.s3, t.Context()
	key := "k/original"

	// abort, then nothing is left
	id, err := c.createMultipart(ctx, key, nil)
	ensure.Nil(t, err)
	ta.plainHTTP(http.MethodPut, c.presignPart(key, id, 1, time.Hour), []byte("x"))
	ensure.Nil(t, c.abortMultipart(ctx, key, id))
	_, err = c.listParts(ctx, key, id)
	ensure.DeepEqual(t, s3Status(err), http.StatusNotFound)
	ensure.Nil(t, c.abortMultipart(ctx, key, id), "aborting twice is fine")

	// a non final part under 5 MiB is refused, as on real S3
	id, err = c.createMultipart(ctx, key, nil)
	ensure.Nil(t, err)
	ta.plainHTTP(http.MethodPut, c.presignPart(key, id, 1, time.Hour), []byte("small"))
	ta.plainHTTP(http.MethodPut, c.presignPart(key, id, 2, time.Hour), []byte("last"))
	parts, err := c.listParts(ctx, key, id)
	ensure.Nil(t, err)
	err = c.completeMultipart(ctx, key, id, parts)
	ensure.StringContains(t, err.Error(), "EntityTooSmall")

	// a wrong ETag is refused
	bad := []s3Part{{Number: 2, ETag: `"0000"`}}
	err = c.completeMultipart(ctx, key, id, bad)
	ensure.StringContains(t, err.Error(), "InvalidPart")

	// an upload id that belongs to another key is not found
	_, err = c.listParts(ctx, "other/original", id)
	ensure.DeepEqual(t, s3Status(err), http.StatusNotFound)
}

func TestFakeS3RejectsBadRequests(t *testing.T) {
	ta := newTestApp(t)
	c, ctx := ta.app.s3, t.Context()
	ensure.Nil(t, c.put(ctx, "a/original", strings.NewReader("secret"), 6, nil))
	ensure.Nil(t, c.put(ctx, "b/original", strings.NewReader("other"), 5, nil))
	good := c.presign(http.MethodGet, "a/original", nil, testNow, time.Hour)
	ensure.DeepEqual(t, ta.plainHTTP(http.MethodGet, good, nil).Code, http.StatusOK)

	forbidden := func(method, u string) {
		t.Helper()
		w := ta.plainHTTP(method, u, nil)
		ensure.DeepEqual(t, w.Code, http.StatusForbidden, u)
		ensure.StringContains(t, w.Body.String(), "SignatureDoesNotMatch")
	}
	// tampered signature
	forbidden(http.MethodGet, good[:len(good)-4]+"0000")
	// the signature of one key used for another
	forbidden(http.MethodGet, strings.Replace(good, "a/original", "b/original", 1))
	// a different method than was signed
	forbidden(http.MethodDelete, good)
	forbidden(http.MethodPut, good)
	// an added query parameter
	forbidden(http.MethodGet, good+"&uploads=")
	// a longer lifetime than was signed
	forbidden(http.MethodGet, strings.Replace(good, "X-Amz-Expires=3600", "X-Amz-Expires=99999", 1))
	// no signature at all
	forbidden(http.MethodGet, strings.SplitN(good, "?", 2)[0])
	// path tricks: the router cleans such paths before they reach the fake,
	// and the fake itself refuses them even when correctly signed
	for _, key := range []string{"../../msg.db", "a/../../x", "/etc/passwd", ""} {
		u := mustURL(t, c.presign(http.MethodGet, key, nil, testNow, time.Hour))
		ensure.True(t, ta.plainHTTP(http.MethodGet, u.String(), nil).Code != http.StatusOK, key)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.URL = u
		w := httptest.NewRecorder()
		ta.app.fakeS3.ServeHTTP(w, r)
		ensure.DeepEqual(t, w.Code, http.StatusForbidden, key)
	}

	// expired
	ta.app.now = func() time.Time { return testNow.Add(time.Hour) }
	forbidden(http.MethodGet, good)
	ta.app.now = func() time.Time { return testNow.Add(time.Hour - time.Second) }
	ensure.DeepEqual(t, ta.plainHTTP(http.MethodGet, good, nil).Code, http.StatusOK)

	// the object was never changed by any of this
	rc, err := c.get(ctx, "a/original", "")
	ensure.DeepEqual(t, string(readAll(t, rc, err)), "secret")
}

type stubTransport func(*http.Request) (*http.Response, error)

func (f stubTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func stubResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body))}
}

func stubClient(t *testing.T, rt stubTransport) *s3Client {
	return &s3Client{endpoint: mustURL(t, "https://s3.example.com"), region: "auto",
		bucket: "b", accessKey: "k", secretKey: "s", pathStyle: true,
		now: time.Now, hc: &http.Client{Transport: rt}}
}

func TestS3CompleteErrorInOKBody(t *testing.T) {
	c := stubClient(t, func(r *http.Request) (*http.Response, error) {
		ensure.DeepEqual(t, r.Method, http.MethodPost)
		ensure.DeepEqual(t, r.URL.Query().Get("uploadId"), "u1")
		body, _ := io.ReadAll(r.Body)
		ensure.DeepEqual(t, string(body), `<CompleteMultipartUpload>`+
			`<Part><PartNumber>1</PartNumber><ETag>&#34;e1&#34;</ETag></Part>`+
			`</CompleteMultipartUpload>`)
		for name := range r.Header {
			ensure.False(t, strings.HasPrefix(strings.ToLower(name), "x-amz-"), name)
		}
		return stubResponse(http.StatusOK, `<?xml version="1.0"?><Error><Code>InternalError</Code>`+
			`<Message>We encountered an internal error.</Message></Error>`), nil
	})
	err := c.completeMultipart(t.Context(), "k", "u1", []s3Part{{Number: 1, ETag: `"e1"`}})
	ensure.StringContains(t, err.Error(), "InternalError")

	ok := stubClient(t, func(r *http.Request) (*http.Response, error) {
		return stubResponse(http.StatusOK, `<CompleteMultipartUploadResult><Key>k</Key></CompleteMultipartUploadResult>`), nil
	})
	ensure.Nil(t, ok.completeMultipart(t.Context(), "k", "u1", []s3Part{{Number: 1, ETag: `"e1"`}}))
}

func TestS3ListPartsTruncated(t *testing.T) {
	c := stubClient(t, func(r *http.Request) (*http.Response, error) {
		return stubResponse(http.StatusOK, `<ListPartsResult><IsTruncated>true</IsTruncated>`+
			`<Part><PartNumber>1</PartNumber><ETag>"a"</ETag><Size>5</Size></Part></ListPartsResult>`), nil
	})
	_, err := c.listParts(t.Context(), "k", "u1")
	ensure.StringContains(t, err.Error(), "truncated")
}

func TestS3ErrorCarriesStatusAndBody(t *testing.T) {
	c := stubClient(t, func(r *http.Request) (*http.Response, error) {
		return stubResponse(http.StatusForbidden, "<Error><Code>AccessDenied</Code></Error>"), nil
	})
	_, err := c.get(t.Context(), "k", "")
	ensure.DeepEqual(t, s3Status(err), http.StatusForbidden)
	ensure.StringContains(t, err.Error(), "AccessDenied")
}

// A key that is only the prefix of other keys is not an object. The fake
// keeps such a prefix as a directory, and what reading one does depends on
// the file system.
func TestFakeS3PrefixIsNotAnObject(t *testing.T) {
	ta := newTestApp(t)
	c := ta.app.s3
	ensure.Nil(t, c.put(t.Context(), "dir/file", strings.NewReader("x"), 1, nil))
	for _, key := range []string{"dir", "dir/"} {
		_, err := c.get(t.Context(), key, "")
		ensure.DeepEqual(t, s3Status(err), http.StatusNotFound, key)
	}
	// and it stays one after the object under it is gone
	ensure.Nil(t, c.del(t.Context(), "dir/file"))
	_, err := c.get(t.Context(), "dir", "")
	ensure.DeepEqual(t, s3Status(err), http.StatusNotFound)
}

func TestS3SelfCheck(t *testing.T) {
	ta := newTestApp(t)
	ensure.Nil(t, ta.app.s3.selfCheck(t.Context(), 7*24*time.Hour))
	// it cleans up after itself
	_, err := ta.app.s3.get(t.Context(), "selfcheck", "")
	ensure.DeepEqual(t, s3Status(err), http.StatusNotFound)

	// a provider that refuses the signed URL gets the validity hint
	store := map[string]string{}
	c := stubClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			store[r.URL.Path] = string(b)
			return stubResponse(http.StatusOK, ""), nil
		case http.MethodDelete:
			delete(store, r.URL.Path)
			return stubResponse(http.StatusNoContent, ""), nil
		}
		return stubResponse(http.StatusForbidden, "<Error><Code>AuthorizationQueryParametersError</Code></Error>"), nil
	})
	err = c.selfCheck(t.Context(), 30*24*time.Hour)
	ensure.StringContains(t, err.Error(), "fetching through a signed URL failed with status 403")
	ensure.StringContains(t, err.Error(), "Set S3_URL_VALIDITY=7d")
	ensure.DeepEqual(t, len(store), 0, "the object is removed even on failure")

	// a failing store step says so
	c = stubClient(t, func(r *http.Request) (*http.Response, error) {
		return stubResponse(http.StatusForbidden, "denied"), nil
	})
	err = c.selfCheck(t.Context(), time.Hour)
	ensure.StringContains(t, err.Error(), "storing an object failed")
}

func TestFakeS3MountedOnlyWhenFake(t *testing.T) {
	ta := newTestApp(t)
	ensure.True(t, ta.app.fakeS3 != nil)
	// unsigned requests are refused by the fake itself, not by a sign in
	// redirect, because the route is outside the session wrapper
	w := ta.get(pFakeS3 + "dev/x")
	ensure.DeepEqual(t, w.Code, http.StatusForbidden)
}
