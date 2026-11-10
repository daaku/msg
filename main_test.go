package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/daaku/ensure"
	"github.com/daaku/serr"
)

var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type testApp struct {
	t   *testing.T
	app *app
	h   http.Handler
}

// newTestApp builds an app with default configuration on a temporary state
// directory and a fixed clock.
func newTestApp(t *testing.T) *testApp {
	t.Helper()
	cfg, err := parseConfig(func(key string) string {
		if key == "STATE_DIRECTORY" {
			return t.TempDir()
		}
		return ""
	})
	ensure.Nil(t, err)
	a := &app{
		cfg: cfg,
		ctx: t.Context(),
		now: func() time.Time { return testNow },
	}
	return &testApp{t: t, app: a, h: a.handler()}
}

type reqOpt func(*http.Request)

func withHeader(key, value string) reqOpt {
	return func(r *http.Request) { r.Header.Set(key, value) }
}

func (ta *testApp) do(r *http.Request, opts ...reqOpt) *httptest.ResponseRecorder {
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	ta.h.ServeHTTP(w, r)
	return w
}

func (ta *testApp) get(path string, opts ...reqOpt) *httptest.ResponseRecorder {
	return ta.do(httptest.NewRequest(http.MethodGet, path, nil), opts...)
}

func (ta *testApp) post(path string, form url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return ta.do(r, opts...)
}

func TestHomeRedirectsSignedOut(t *testing.T) {
	ta := newTestApp(t)
	w := ta.get("/")
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, w.Header().Get("Location"), pSignIn)
}

func TestSignInPage(t *testing.T) {
	ta := newTestApp(t)
	w := ta.get(pSignIn)
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	body := w.Body.String()
	ensure.StringContains(t, body, `id="app"`)
	ensure.StringContains(t, body, "<title>Sign in · msg</title>")
	ensure.StringContains(t, body, `id="flash"`)
}

func TestSignedOutPostIsUnauthorized(t *testing.T) {
	ta := newTestApp(t)
	a := ta.app
	h := a.wrap(accUser, func(w http.ResponseWriter, r *http.Request) error {
		t.Fatal("handler must not run")
		return nil
	})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/x", nil))
	ensure.DeepEqual(t, w.Code, http.StatusUnauthorized)
}

func TestHTTPErrAndInternalError(t *testing.T) {
	ta := newTestApp(t)
	a := ta.app
	w := httptest.NewRecorder()
	a.wrap(accPublic, func(w http.ResponseWriter, r *http.Request) error {
		return httpErr(http.StatusNotFound, "No such thing.")
	})(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	ensure.DeepEqual(t, w.Code, http.StatusNotFound)
	ensure.StringContains(t, w.Body.String(), "No such thing.")

	w = httptest.NewRecorder()
	a.wrap(accPublic, func(w http.ResponseWriter, r *http.Request) error {
		return serr.Errorf("secret detail")
	})(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	ensure.DeepEqual(t, w.Code, http.StatusInternalServerError)
	ensure.StringDoesNotContain(t, w.Body.String(), "secret detail")

	a.cfg.Dev = true
	w = httptest.NewRecorder()
	a.wrap(accPublic, func(w http.ResponseWriter, r *http.Request) error {
		return serr.Errorf("secret detail")
	})(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	ensure.DeepEqual(t, w.Code, http.StatusInternalServerError)
	ensure.StringContains(t, w.Body.String(), "secret detail")
}

func TestCrossSitePostRejected(t *testing.T) {
	ta := newTestApp(t)
	w := ta.post(pSignIn, nil, withHeader("Sec-Fetch-Site", "cross-site"))
	ensure.DeepEqual(t, w.Code, http.StatusForbidden)
	w = ta.post(pSignIn, nil)
	ensure.True(t, w.Code != http.StatusForbidden, w.Code)
}

func TestSecurityHeaders(t *testing.T) {
	ta := newTestApp(t)
	w := ta.get(pSignIn)
	csp := w.Header().Get("Content-Security-Policy")
	ensure.StringContains(t, csp, "default-src 'self'")
	ensure.StringContains(t, csp, "frame-ancestors 'none'")
	ensure.DeepEqual(t, w.Header().Get("X-Content-Type-Options"), "nosniff")
	ensure.DeepEqual(t, w.Header().Get("X-Frame-Options"), "deny")
	ensure.DeepEqual(t, w.Header().Get("Referrer-Policy"), "same-origin")
	ensure.DeepEqual(t, w.Header().Get("Cross-Origin-Opener-Policy"), "same-origin")
	ensure.DeepEqual(t, w.Header().Get("Cross-Origin-Embedder-Policy"), "")
}

func TestCSPS3Origin(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "img-src 'self' data:; media-src 'self';"},
		{map[string]string{"S3_ENDPOINT": "fake"}, "img-src 'self' data:; media-src 'self';"},
		{
			map[string]string{"S3_ENDPOINT": "https://s3.example.com", "S3_BUCKET": "b"},
			"connect-src 'self' https://s3.example.com;",
		},
		{
			map[string]string{"S3_ENDPOINT": "https://s3.example.com", "S3_BUCKET": "b", "S3_PATH_STYLE": "false"},
			"media-src 'self' https://b.s3.example.com;",
		},
	}
	for _, c := range cases {
		cfg, err := parseConfig(func(k string) string { return c.env[k] })
		ensure.Nil(t, err)
		ensure.StringContains(t, cfg.csp(), c.want)
	}
}

func TestAssetsAndRobots(t *testing.T) {
	ta := newTestApp(t)
	w := ta.get(assetPath("main.css"))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.StringContains(t, w.Header().Get("Content-Type"), "text/css")
	w = ta.get(pRobotsTxt)
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.StringContains(t, w.Body.String(), "Disallow: /")
	// livereload is only served in development
	ensure.DeepEqual(t, ta.get(pLivereloadJS).Code, http.StatusNotFound)
}

func TestDevLivereloadJS(t *testing.T) {
	ta := newTestApp(t)
	ta.app.cfg.Dev = true
	ta.h = ta.app.handler()
	w := ta.get(pLivereloadJS)
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.StringContains(t, w.Header().Get("Content-Type"), "text/javascript")
	ensure.StringContains(t, ta.get(pSignIn).Body.String(), `src="/livereload.js"`)
}

func TestFlash(t *testing.T) {
	ta := newTestApp(t)
	a := ta.app
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	ensure.Nil(t, a.flash(w, r, pSignIn, Flash{Kind: flashSuccess, Content: "Hello <b>"}))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	cookies := w.Result().Cookies()
	ensure.DeepEqual(t, len(cookies), 1)

	w = ta.get(pSignIn, func(r *http.Request) { r.AddCookie(cookies[0]) })
	body := w.Body.String()
	ensure.StringContains(t, body, `class="alert success"`)
	ensure.StringContains(t, body, "Hello &lt;b&gt;")
	// reading it clears it
	ensure.StringContains(t, w.Header().Get("Set-Cookie"), "Max-Age=0")
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"100MB", 100 << 20, false},
		{"1GB", 1 << 30, false},
		{"512KB", 512 << 10, false},
		{"1mb", 1 << 20, false},
		{"1048576", 1048576, false},
		{"10B", 10, false},
		{"", 0, true},
		{"MB", 0, true},
		{"0", 0, true},
		{"-1MB", 0, true},
		{"1.5MB", 0, true},
		{"10XB", 0, true},
		{"99999999999TB", 0, true},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		ensure.DeepEqual(t, err != nil, c.err, c.in)
		ensure.DeepEqual(t, got, c.want, c.in)
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"", 0, true},
		{"d", 0, true},
		{"0d", 0, true},
		{"-1h", 0, true},
		{"1.5d", 0, true},
		{"soon", 0, true},
	}
	for _, c := range cases {
		got, err := parseDuration(c.in)
		ensure.DeepEqual(t, err != nil, c.err, c.in)
		ensure.DeepEqual(t, got, c.want, c.in)
	}
}

func TestParseResolution(t *testing.T) {
	cases := map[string]int{"4k": 3840, "1440p": 2560, "1080p": 1920, "720p": 1280, "480p": 854}
	for in, want := range cases {
		got, err := parseResolution(in)
		ensure.Nil(t, err)
		ensure.DeepEqual(t, got, want, in)
	}
	for _, in := range []string{"", "8k", "1080", "4K"} {
		_, err := parseResolution(in)
		ensure.NotNil(t, err, in)
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(func(string) string { return "" })
	ensure.Nil(t, err)
	ensure.DeepEqual(t, cfg, config{
		PublicURL:       "http://localhost:8080",
		AppName:         "msg",
		StateDir:        "data",
		AdminUsername:   "admin",
		S3Region:        "us-east-1",
		S3PathStyle:     true,
		S3URLValidity:   7 * 24 * time.Hour,
		MaxUploadSize:   100 << 20,
		ImageFormat:     "webp",
		ImageResolution: 3840,
		VideoFormat:     "h264",
		VideoResolution: 1920,
	})
}

func TestConfigBadValues(t *testing.T) {
	cases := map[string]string{
		"PUBLIC_URL":       "example.com",
		"ADMIN_USERNAME":   "Admin",
		"DEV":              "yes",
		"S3_ENDPOINT":      "ftp://x",
		"S3_PATH_STYLE":    "maybe",
		"S3_URL_VALIDITY":  "8d",
		"MAX_UPLOAD_SIZE":  "lots",
		"IMAGE_FORMAT":     "gif",
		"IMAGE_RESOLUTION": "8k",
		"VIDEO_FORMAT":     "vp9",
		"VIDEO_RESOLUTION": "2160p",
	}
	for key, value := range cases {
		_, err := parseConfig(func(k string) string {
			if k == key {
				return value
			}
			return ""
		})
		ensure.Err(t, err, regexp.MustCompile("invalid "+key+" "))
	}
}

func TestConfigValues(t *testing.T) {
	env := map[string]string{
		"PUBLIC_URL":       "https://chat.example.com/",
		"DEV":              "1",
		"S3_ENDPOINT":      "https://s3.example.com",
		"S3_PATH_STYLE":    "false",
		"S3_URL_VALIDITY":  "1d",
		"MAX_UPLOAD_SIZE":  "1GB",
		"IMAGE_FORMAT":     "avif",
		"IMAGE_RESOLUTION": "1440p",
		"VIDEO_FORMAT":     "av1",
		"VIDEO_RESOLUTION": "720p",
	}
	cfg, err := parseConfig(func(k string) string { return env[k] })
	ensure.Nil(t, err)
	ensure.DeepEqual(t, cfg.PublicURL, "https://chat.example.com")
	ensure.True(t, cfg.Dev)
	ensure.False(t, cfg.S3PathStyle)
	ensure.DeepEqual(t, cfg.S3URLValidity, 24*time.Hour)
	ensure.DeepEqual(t, cfg.MaxUploadSize, int64(1<<30))
	ensure.DeepEqual(t, cfg.ImageFormat, "avif")
	ensure.DeepEqual(t, cfg.ImageResolution, 2560)
	ensure.DeepEqual(t, cfg.VideoFormat, "av1")
	ensure.DeepEqual(t, cfg.VideoResolution, 1280)
}
