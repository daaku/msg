package main

import (
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/daaku/ensure"
	"github.com/daaku/serr"
	"github.com/descope/virtualwebauthn"
)

var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type testApp struct {
	t   *testing.T
	app *app
	h   http.Handler
}

// newTestApp builds an app with default configuration on a temporary state
// directory with a real, migrated database and a fixed clock.
func newTestApp(t *testing.T) *testApp {
	t.Helper()
	return newTestAppIn(t, t.TempDir())
}

// newTestAppIn is newTestApp on the given state directory.
func newTestAppIn(t *testing.T, dir string) *testApp {
	t.Helper()
	cfg, err := parseConfig(func(key string) string {
		if key == "STATE_DIRECTORY" {
			return dir
		}
		return ""
	})
	ensure.Nil(t, err)
	a, err := newApp(t.Context(), cfg)
	ensure.Nil(t, err)
	t.Cleanup(func() { ensure.Nil(t, a.close()) })
	a.now = func() time.Time { return testNow }
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
		"S3_URL_VALIDITY":  "0d",
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

	// providers that allow longer than 7 days may be given more
	env["S3_URL_VALIDITY"] = "30d"
	cfg, err = parseConfig(func(k string) string { return env[k] })
	ensure.Nil(t, err)
	ensure.DeepEqual(t, cfg.S3URLValidity, 30*24*time.Hour)
	ensure.DeepEqual(t, cfg.MaxUploadSize, int64(1<<30))
	ensure.DeepEqual(t, cfg.ImageFormat, "avif")
	ensure.DeepEqual(t, cfg.ImageResolution, 2560)
	ensure.DeepEqual(t, cfg.VideoFormat, "av1")
	ensure.DeepEqual(t, cfg.VideoResolution, 1280)
}

func TestMigrations(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	files, err := fs.Glob(migrationsFS, migrations.Glob)
	ensure.Nil(t, err)
	ensure.True(t, len(files) > 0)
	var applied int
	ensure.Nil(t, ta.app.db.QueryRowContext(ctx,
		"SELECT count(*) FROM db_migrations").Scan(&applied))
	ensure.DeepEqual(t, applied, len(files))

	// every table exists and is empty
	for _, table := range []string{"config", "users", "credentials", "invites",
		"chats", "members", "files", "messages", "previews", "reactions",
		"snoozes", "push_subscriptions", "user_stats", "chat_stats"} {
		var n int
		ensure.Nil(t, ta.app.db.QueryRowContext(ctx,
			"SELECT count(*) FROM "+table).Scan(&n), table)
	}
	n, err := ta.app.q.UserCount(ctx)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, n, int64(0))

	// applying again is a no op
	ensure.Nil(t, migrations.Migrate(ctx, ta.app.db))
	ensure.Nil(t, ta.app.db.QueryRowContext(ctx,
		"SELECT count(*) FROM db_migrations").Scan(&applied))
	ensure.DeepEqual(t, applied, len(files))

	// query.sql is not a migration
	for _, f := range files {
		ensure.StringDoesNotContain(t, f, "query.sql")
	}
}

func TestSecret(t *testing.T) {
	dir := t.TempDir()
	a1 := newTestAppIn(t, dir).app
	a2 := newTestAppIn(t, dir).app
	ensure.DeepEqual(t, len(a1.secret("x")), 32)
	ensure.DeepEqual(t, a1.secret("x"), a2.secret("x"))
	ensure.NotDeepEqual(t, a1.secret("x"), a1.secret("y"))
	other := newTestApp(t).app
	ensure.NotDeepEqual(t, a1.secret("x"), other.secret("x"))
}

func TestTxRollsBack(t *testing.T) {
	ta := newTestApp(t)
	a := ta.app
	ctx := t.Context()
	errBoom := errors.New("boom")
	err := a.tx(ctx, func(q *Queries) error {
		ensure.Nil(t, q.ConfigSetDefault(ctx, ConfigSetDefaultParams{Key: "k", Value: []byte("v")}))
		return errBoom
	})
	ensure.True(t, errors.Is(err, errBoom))
	_, err = a.q.ConfigGet(ctx, "k")
	ensure.True(t, errors.Is(err, sql.ErrNoRows), err)

	func() {
		defer ensure.PanicDeepEqual(t, "oops")
		_ = a.tx(ctx, func(q *Queries) error {
			ensure.Nil(t, q.ConfigSetDefault(ctx, ConfigSetDefaultParams{Key: "k", Value: []byte("v")}))
			panic("oops")
		})
	}()
	_, err = a.q.ConfigGet(ctx, "k")
	ensure.True(t, errors.Is(err, sql.ErrNoRows), err)

	ensure.Nil(t, a.tx(ctx, func(q *Queries) error {
		return q.ConfigSetDefault(ctx, ConfigSetDefaultParams{Key: "k", Value: []byte("v")})
	}))
	v, err := a.q.ConfigGet(ctx, "k")
	ensure.Nil(t, err)
	ensure.DeepEqual(t, v, []byte("v"))
}

func TestForeignKeysEnforced(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	_, err := ta.app.db.ExecContext(ctx,
		"INSERT INTO users (id, username, handle, created_at) VALUES (1, 'a', x'01', 1)")
	ensure.Nil(t, err)
	_, err = ta.app.db.ExecContext(ctx,
		"INSERT INTO members (chat_id, user_id, added_by, created_at) VALUES (99, 1, 1, 1)")
	ensure.Err(t, err, regexp.MustCompile("FOREIGN KEY"))
}

// auth harness

var testRP = virtualwebauthn.RelyingParty{
	ID:     "localhost",
	Name:   "msg",
	Origin: "http://localhost:8080",
}

// testUser is a user with a real passkey on a virtual authenticator and a
// session made with it.
type testUser struct {
	User
	auth   *virtualwebauthn.Authenticator
	cred   virtualwebauthn.Credential
	credID int64 // credentials row id
	cookie *http.Cookie
}

// withCookies carries the cookies set by an earlier response.
func withCookies(w *httptest.ResponseRecorder) reqOpt {
	return func(r *http.Request) {
		for _, c := range w.Result().Cookies() {
			if c.MaxAge >= 0 {
				r.AddCookie(c)
			}
		}
	}
}

func findCookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// as makes the request as the given user.
func (ta *testApp) as(u *testUser) reqOpt {
	return func(r *http.Request) { r.AddCookie(u.cookie) }
}

// register runs a passkey registration against an options and a post path
// and returns the response to the post.
func (ta *testApp) register(optionsPath, postPath string, auth *virtualwebauthn.Authenticator, cred virtualwebauthn.Credential, opts ...reqOpt) *httptest.ResponseRecorder {
	ta.t.Helper()
	wo := ta.get(optionsPath, opts...)
	if wo.Code != http.StatusOK {
		return wo
	}
	ao, err := virtualwebauthn.ParseAttestationOptions(wo.Body.String())
	ensure.Nil(ta.t, err)
	resp := virtualwebauthn.CreateAttestationResponse(testRP, *auth, cred, *ao)
	return ta.post(postPath, url.Values{"json": {resp}},
		append([]reqOpt{withCookies(wo)}, opts...)...)
}

// acceptInvite registers a new passkey through an invite link.
func (ta *testApp) acceptInvite(inv Invite) (*httptest.ResponseRecorder, *virtualwebauthn.Authenticator, virtualwebauthn.Credential) {
	ta.t.Helper()
	auth := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{
		UserHandle: inv.Handle,
	})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	path := invitePath(inv.Token)
	w := ta.register(path+"/options", path, &auth, cred)
	auth.AddCredential(cred)
	return w, &auth, cred
}

// newInvite stores a new user invite created by nobody.
func (ta *testApp) newInvite(username string, admin bool) Invite {
	ta.t.Helper()
	inv, err := ta.app.createInvite(ta.t.Context(), ta.app.q, inviteParams{
		Username: username,
		Admin:    admin,
		TTL:      time.Hour,
	})
	ensure.Nil(ta.t, err)
	return inv
}

// newUser creates a user through the real invite flow and then grants the
// named permissions ("admin", "invite", "group").
func (ta *testApp) newUser(username string, perms ...string) *testUser {
	ta.t.Helper()
	t := ta.t
	w, auth, cred := ta.acceptInvite(ta.newInvite(username, false))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther, w.Body.String())
	for _, p := range perms {
		_, err := ta.app.db.ExecContext(t.Context(),
			"UPDATE users SET perm_"+p+" = TRUE WHERE username = ?", username)
		ensure.Nil(t, err)
	}
	user, err := ta.app.q.UserByUsername(t.Context(), username)
	ensure.Nil(t, err)
	row, err := ta.app.q.CredentialByCredentialID(t.Context(), cred.ID)
	ensure.Nil(t, err)
	return &testUser{
		User:   user,
		auth:   auth,
		cred:   cred,
		credID: row.ID,
		cookie: findCookie(w, "s"),
	}
}

// signIn runs a full discoverable passkey sign in.
func (ta *testApp) signIn(auth *virtualwebauthn.Authenticator, cred virtualwebauthn.Credential) *httptest.ResponseRecorder {
	ta.t.Helper()
	wo := ta.get(pSignInOptions)
	ensure.DeepEqual(ta.t, wo.Code, http.StatusOK)
	ao, err := virtualwebauthn.ParseAssertionOptions(wo.Body.String())
	ensure.Nil(ta.t, err)
	resp := virtualwebauthn.CreateAssertionResponse(testRP, *auth, cred, *ao)
	return ta.post(pSignIn, url.Values{"json": {resp}}, withCookies(wo))
}

// chat harness

// newChat creates a group made by creator with the given other members.
func (ta *testApp) newChat(creator *testUser, name string, members ...*testUser) Chat {
	ta.t.Helper()
	ids := make([]int64, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	chat, err := ta.app.createGroup(ta.t.Context(), creator.ID, name, ids)
	ensure.Nil(ta.t, err)
	return chat
}

// newDM returns the direct message between two users, creating it.
func (ta *testApp) newDM(a, b *testUser) Chat {
	ta.t.Helper()
	chat, _, err := ta.app.startDM(ta.t.Context(), a.ID, b.ID)
	ensure.Nil(ta.t, err)
	return chat
}

// newMessage posts a text message as user, through createMessage.
func (ta *testApp) newMessage(chat Chat, user *testUser, body string) Message {
	ta.t.Helper()
	msg, err := ta.app.createMessage(ta.t.Context(), chat, user.ID, body, sql.NullInt64{})
	ensure.Nil(ta.t, err)
	return msg
}
