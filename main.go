package main

//go:generate go tool sqlc generate

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daaku/hashfs"
	"github.com/daaku/lands"
	"github.com/daaku/livereload"
	"github.com/daaku/serr"
	"github.com/daaku/sookie"
	"github.com/daaku/sqlmig"
	"github.com/go-webauthn/webauthn/webauthn"
	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"

	_ "github.com/joho/godotenv/autoload"
	_ "modernc.org/sqlite"
)

const (
	pAdmin              = "/admin"
	pAdminUser          = "/admin/u/"
	pArchived           = "/archived"
	pAssets             = "/assets/"
	pChat               = "/c/"
	pDM                 = "/dm"
	pEvents             = "/events"
	pFakeS3             = "/fakes3/"
	pFile               = "/f/"
	pUploadAbort        = "/upload/abort"
	pUploadComplete     = "/upload/complete"
	pUploadInit         = "/upload/init"
	pHome               = "/"
	pInvite             = "/invite/"
	pInvites            = "/invites"
	pInvitesRevoke      = "/invites/revoke"
	pLivereload         = "/livereload"
	pLivereloadJS       = "/livereload.js"
	pNew                = "/new"
	pPasskeyCreate      = "/passkey/create"
	pPasskeyDelete      = "/passkey/delete"
	pPasskeyOptions     = "/passkey/options"
	pPushOpened         = "/push/opened"
	pPushSubscribe      = "/push/subscribe"
	pPushUnsubscribe    = "/push/unsubscribe"
	pRobotsTxt          = "/robots.txt"
	pSettings           = "/settings"
	pSettingsLink       = "/settings/link"
	pSettingsLinkRevoke = "/settings/link/revoke"
	pSignIn             = "/sign-in"
	pSignInOptions      = "/sign-in/options"
	pSignOut            = "/sign-out"
	pSWJS               = "/sw.js"
)

const defaultAddr = "localhost:8080"

//go:embed assets/*
var assets embed.FS

func assetPath(filename string) string {
	return "/" + hashfs.Path(assets, path.Join("assets", filename))
}

// config

type config struct {
	PublicURL         string
	AppName           string
	StateDir          string
	AdminUsername     string
	Dev               bool
	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3PathStyle       bool
	S3URLValidity     time.Duration
	MaxUploadSize     int64
	ImageFormat       string
	ImageResolution   int
	VideoFormat       string
	VideoResolution   int
}

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{1,31}$`)

var resolutions = map[string]int{
	"4k":    3840,
	"1440p": 2560,
	"1080p": 1920,
	"720p":  1280,
	"480p":  854,
}

// loadConfig parses the configuration from the environment.
func loadConfig() (config, error) {
	return parseConfig(os.Getenv)
}

func parseConfig(getenv func(string) string) (config, error) {
	var errs []error
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	bad := func(key, v string, err error) {
		errs = append(errs, fmt.Errorf("invalid %s %q: %w", key, v, err))
	}
	parse := func(key, def string, fn func(string) error) {
		v := get(key, def)
		if err := fn(v); err != nil {
			bad(key, v, err)
		}
	}
	oneOf := func(key, def string, allowed ...string) string {
		v := get(key, def)
		for _, a := range allowed {
			if v == a {
				return v
			}
		}
		bad(key, v, fmt.Errorf("must be one of %s", strings.Join(allowed, ", ")))
		return v
	}

	var c config

	c.PublicURL = strings.TrimSuffix(get("PUBLIC_URL", "http://"+defaultAddr), "/")
	if u, err := url.Parse(c.PublicURL); err != nil {
		bad("PUBLIC_URL", c.PublicURL, err)
	} else if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		bad("PUBLIC_URL", c.PublicURL, errors.New("must be an origin like https://chat.example.com"))
	}

	c.AppName = get("APP_NAME", "msg")
	c.StateDir = get("STATE_DIRECTORY", "data")

	c.AdminUsername = get("ADMIN_USERNAME", "admin")
	if !usernameRe.MatchString(c.AdminUsername) {
		bad("ADMIN_USERNAME", c.AdminUsername, fmt.Errorf("must match %s", usernameRe))
	}

	switch v := getenv("DEV"); v {
	case "", "0":
	case "1":
		c.Dev = true
	default:
		bad("DEV", v, errors.New("must be 1 or unset"))
	}

	c.S3Endpoint = strings.TrimSuffix(getenv("S3_ENDPOINT"), "/")
	if c.S3Endpoint != "" && c.S3Endpoint != "fake" {
		if u, err := url.Parse(c.S3Endpoint); err != nil {
			bad("S3_ENDPOINT", c.S3Endpoint, err)
		} else if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
			bad("S3_ENDPOINT", c.S3Endpoint, errors.New("must be https://host or fake"))
		}
	}
	c.S3Region = get("S3_REGION", "us-east-1")
	c.S3Bucket = getenv("S3_BUCKET")
	c.S3AccessKeyID = getenv("S3_ACCESS_KEY_ID")
	c.S3SecretAccessKey = getenv("S3_SECRET_ACCESS_KEY")
	parse("S3_PATH_STYLE", "true", func(v string) (err error) {
		if c.S3PathStyle, err = strconv.ParseBool(v); err != nil {
			return errors.New("must be true or false")
		}
		return nil
	})
	parse("S3_URL_VALIDITY", "7d", func(v string) (err error) {
		c.S3URLValidity, err = parseDuration(v)
		return err
	})

	parse("MAX_UPLOAD_SIZE", "100MB", func(v string) (err error) {
		c.MaxUploadSize, err = parseSize(v)
		return err
	})

	c.ImageFormat = oneOf("IMAGE_FORMAT", "webp", "webp", "avif", "jpeg")
	parse("IMAGE_RESOLUTION", "4k", func(v string) (err error) {
		c.ImageResolution, err = parseResolution(v)
		return err
	})
	c.VideoFormat = oneOf("VIDEO_FORMAT", "h264", "h264", "av1")
	parse("VIDEO_RESOLUTION", "1080p", func(v string) (err error) {
		c.VideoResolution, err = parseResolution(v)
		return err
	})

	if err := errors.Join(errs...); err != nil {
		return config{}, err
	}
	return c, nil
}

// parseSize parses a positive byte size such as 512KB, 100MB, 1GB or 1024.
// Units are binary: 1KB is 1024 bytes.
func parseSize(s string) (int64, error) {
	num, mult := s, int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"TB", 1 << 40}, {"B", 1}} {
		if strings.HasSuffix(strings.ToUpper(s), u.suffix) {
			num, mult = s[:len(s)-len(u.suffix)], u.mult
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("must be a positive size like 100MB")
	}
	if n > math.MaxInt64/mult {
		return 0, errors.New("size too large")
	}
	return n * mult, nil
}

// parseDuration is time.ParseDuration plus a "d" suffix for whole days. The
// duration must be positive.
func parseDuration(s string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseInt(days, 10, 64)
		if err != nil || n <= 0 || n > math.MaxInt64/int64(24*time.Hour) {
			return 0, errors.New("must be a positive duration like 7d or 12h")
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, errors.New("must be a positive duration like 7d or 12h")
		}
	}
	if d <= 0 {
		return 0, errors.New("must be a positive duration like 7d or 12h")
	}
	return d, nil
}

// parseResolution maps a resolution name to the long edge in pixels.
func parseResolution(s string) (int, error) {
	if n, ok := resolutions[s]; ok {
		return n, nil
	}
	return 0, errors.New("must be one of 4k, 1440p, 1080p, 720p, 480p")
}

// s3Origin is the origin media is loaded from, or "" when there is none
// outside of this server.
func (c config) s3Origin() string {
	if c.S3Endpoint == "" || c.S3Endpoint == "fake" {
		return ""
	}
	u, err := url.Parse(c.S3Endpoint)
	if err != nil {
		return ""
	}
	if !c.S3PathStyle && c.S3Bucket != "" {
		return u.Scheme + "://" + c.S3Bucket + "." + u.Host
	}
	return u.Scheme + "://" + u.Host
}

func (c config) csp() string {
	s3 := ""
	if o := c.s3Origin(); o != "" {
		s3 = " " + o
	}
	return "default-src 'self'; " +
		"img-src 'self' data:" + s3 + "; " +
		"media-src 'self'" + s3 + "; " +
		"connect-src 'self'" + s3 + "; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
}

// app

type app struct {
	cfg config
	ctx context.Context
	bg  sync.WaitGroup
	now func() time.Time
	db  *sql.DB
	q   *Queries
	wa  *webauthn.WebAuthn
	hub *hub
	s3  *s3Client
	// fakeS3 is the built in development bucket, nil with a real one.
	fakeS3 *fakeS3
	// mediaWake tells the media worker there is a file to convert.
	mediaWake chan struct{}
	fileLocks sync.Map // file id to *sync.Mutex
	// beforeConvert is a test hook that runs just before a converter.
	beforeConvert func(File)
	push          pushSender
	vapidPublic   string        // base64url, for pushManager.subscribe
	pushSem       chan struct{} // limits concurrent push sends

	master    []byte
	secretsMu sync.Mutex
	secrets   map[string][]byte
}

// newApp opens the database, runs the migrations and loads the stored
// secrets. The caller must call close.
func newApp(ctx context.Context, cfg config) (*app, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, serr.Wrap(err)
	}
	wa, err := newWebAuthn(cfg)
	if err != nil {
		return nil, err
	}
	db, err := openDB(ctx, filepath.Join(cfg.StateDir, "msg.db"))
	if err != nil {
		return nil, err
	}
	a := &app{
		cfg:       cfg,
		ctx:       ctx,
		now:       time.Now,
		db:        db,
		q:         New(db),
		wa:        wa,
		hub:       newHub(),
		mediaWake: make(chan struct{}, 1),
		pushSem:   make(chan struct{}, pushConcurrency),
		secrets:   map[string][]byte{},
	}
	if err := a.loadMaster(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.loadVAPID(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// The clock is read through a.now, which tests replace later.
	a.s3, a.fakeS3, err = newS3(cfg, func() time.Time { return a.now() })
	if err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func (a *app) close() error {
	return serr.Wrap(a.db.Close())
}

// database

//go:embed db/[0-9]*.sql
var migrationsFS embed.FS

var migrations = sqlmig.Source{
	FS:   migrationsFS,
	Glob: "db/[0-9]*.sql",
}

var sqliteDSN = strings.Join([]string{
	"_pragma=journal_mode(wal)",
	"_pragma=foreign_keys(on)",
	"_pragma=synchronous(normal)",
	"_pragma=busy_timeout(5000)",
	"_pragma=cache_size(-20000)",
	"_pragma=temp_store(memory)",
	"_pragma=mmap_size(134217728)",
	"_txlock=immediate",
}, "&")

// openDB opens the SQLite database at file and runs the migrations.
func openDB(ctx context.Context, file string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+file+"?"+sqliteDSN)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	if err := migrations.Migrate(ctx, db); err != nil {
		db.Close()
		return nil, serr.Wrap(err)
	}
	return db, nil
}

// tx runs fn inside a transaction, committing when it returns nil and
// rolling back on an error or a panic.
func (a *app) tx(ctx context.Context, fn func(q *Queries) error) (err error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return serr.Wrap(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(a.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return serr.Wrap(err)
	}
	committed = true
	return nil
}

// loadMaster loads the master secret, generating and storing it on first
// start. Reading it back after the insert makes two racing starts agree.
func (a *app) loadMaster(ctx context.Context) error {
	fresh := make([]byte, 32)
	_, _ = rand.Read(fresh) // never returns an error
	err := a.q.ConfigSetDefault(ctx, ConfigSetDefaultParams{
		Key:   "secret",
		Value: fresh,
	})
	if err != nil {
		return serr.Wrap(err)
	}
	master, err := a.q.ConfigGet(ctx, "secret")
	if err != nil {
		return serr.Wrap(err)
	}
	if len(master) != 32 {
		return serr.Errorf("stored secret has %d bytes, want 32", len(master))
	}
	a.master = master
	return nil
}

// secret returns the 32 byte key for the given purpose, derived from the
// stored master secret.
func (a *app) secret(purpose string) []byte {
	a.secretsMu.Lock()
	defer a.secretsMu.Unlock()
	if key, ok := a.secrets[purpose]; ok {
		return key
	}
	key, err := hkdf.Expand(sha256.New, a.master, purpose, 32)
	if err != nil {
		panic(err) // only possible with an invalid length
	}
	a.secrets[purpose] = key
	return key
}

// errors

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%d %s", e.code, e.msg)
}

// httpErr is an expected failure shown to the user with the given status.
func httpErr(code int, msg string) error {
	return &httpError{code: code, msg: msg}
}

func (a *app) renderError(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := http.StatusInternalServerError, "Something went wrong."
	var detail g.Node
	if he, ok := errors.AsType[*httpError](err); ok {
		code, msg = he.code, he.msg
	} else {
		fmt.Fprintf(os.Stderr, "%s %s: %+v\n", r.Method, r.URL.Path, err)
		if a.cfg.Dev {
			detail = h.Pre(h.Class("stack-trace"), g.Text(fmt.Sprintf("%+v", err)))
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = a.pageSimple(w, r, http.StatusText(code), backLink(pHome, "Home"),
		h.P(g.Text(msg)), detail).Render(w)
}

// access control

type access int

const (
	accPublic access = iota
	accUser
	accInvite
	accGroup
	accAdmin
)

type handler func(w http.ResponseWriter, r *http.Request) error

// wrap enforces the access level and turns a returned error into a response.
func (a *app) wrap(acc access, f handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, err := a.withSession(w, r)
		if err != nil {
			a.renderError(w, r, err)
			return
		}
		if user := userFrom(r.Context()); !allowed(user, acc) {
			switch {
			case user != nil:
				// Signed in without the permission: do not reveal the page.
				a.renderError(w, r, httpErr(http.StatusNotFound, "Not found."))
			case r.Method == http.MethodGet || r.Method == http.MethodHead:
				http.Redirect(w, r, pSignIn, http.StatusSeeOther)
			default:
				a.renderError(w, r, httpErr(http.StatusUnauthorized, "Please sign in."))
			}
			return
		}
		if err := f(w, r); err != nil {
			a.renderError(w, r, err)
		}
	}
}

// flash

type flashKind string

const (
	flashError   = flashKind("error")
	flashSuccess = flashKind("success")
)

type Flash struct {
	Kind    flashKind
	Content string
}

func (a *app) flashCookie() http.Cookie {
	return http.Cookie{
		Name:     "flash",
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(a.cfg.PublicURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	}
}

// flash stores a message for the next page and redirects there.
func (a *app) flash(w http.ResponseWriter, r *http.Request, url string, f Flash) error {
	if err := sookie.Set(a.secret("flash"), w, &f, a.flashCookie()); err != nil {
		return serr.Wrap(err)
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
	return nil
}

// getFlash returns and clears the pending flash message, if any.
func (a *app) getFlash(w http.ResponseWriter, r *http.Request) *Flash {
	c := a.flashCookie()
	sookie.Del(w, r, c)
	f, err := sookie.Get[*Flash](a.secret("flash"), r, c.Name)
	if err != nil {
		return nil
	}
	return f
}

// renderFlash renders the flash region, empty when there is no message.
func (a *app) renderFlash(w http.ResponseWriter, r *http.Request) g.Node {
	f := a.getFlash(w, r)
	if f == nil || f.Content == "" {
		return h.Div(h.ID("flash"))
	}
	kind := f.Kind
	if kind == "" {
		kind = flashError
	}
	return h.Div(h.ID("flash"),
		h.Div(h.Class("alert "+string(kind)), h.Role("status"), g.Text(f.Content)))
}

// security headers

func secureHandler(csp string, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", csp)
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "deny")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	}
}

// handlers

func (a *app) static(filename string) handler {
	name := path.Join("assets", filename)
	return func(w http.ResponseWriter, r *http.Request) error {
		f, err := assets.Open(name)
		if err != nil {
			return serr.Wrap(err)
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			return serr.Wrap(err)
		}
		http.ServeContent(w, r, name, stat.ModTime(), f.(io.ReadSeeker))
		return nil
	}
}

func (a *app) livereloadJS(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, err := io.WriteString(w, livereload.JS)
	return serr.Wrap(err)
}

// handler builds the complete HTTP handler.
func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+pAssets, hashfs.FileServer(assets))
	mux.HandleFunc("GET "+pRobotsTxt, a.wrap(accPublic, a.static("robots.txt")))
	mux.HandleFunc("GET "+pSWJS, a.wrap(accPublic, a.serviceWorker))
	mux.HandleFunc("GET "+pSignIn, a.wrap(accPublic, a.signIn))
	mux.HandleFunc("GET "+pSignInOptions, a.wrap(accPublic, a.signInOptions))
	mux.HandleFunc("POST "+pSignIn, a.wrap(accPublic, a.signInPost))
	mux.HandleFunc("POST "+pSignOut, a.wrap(accUser, a.signOut))
	mux.HandleFunc("GET "+pInvite+"{token}", a.wrap(accPublic, a.invite))
	mux.HandleFunc("GET "+pInvite+"{token}/options", a.wrap(accPublic, a.inviteOptions))
	mux.HandleFunc("POST "+pInvite+"{token}", a.wrap(accPublic, a.invitePost))
	mux.HandleFunc("GET "+pHome+"{$}", a.wrap(accUser, a.home))
	if a.fakeS3 != nil {
		// Requests carry their own signature, so this is outside a.wrap.
		mux.Handle(pFakeS3, a.fakeS3)
	}
	mux.HandleFunc("GET "+pEvents, a.wrap(accUser, a.events))
	mux.HandleFunc("POST "+pUploadInit, a.wrap(accUser, api(a.uploadInit)))
	mux.HandleFunc("POST "+pUploadComplete, a.wrap(accUser, api(a.uploadComplete)))
	mux.HandleFunc("POST "+pUploadAbort, a.wrap(accUser, api(a.uploadAbort)))
	mux.HandleFunc("GET "+pFile+"{id}/{variant}", a.wrap(accUser, a.fileRedirect))
	mux.HandleFunc("GET "+pArchived, a.wrap(accUser, a.archived))
	mux.HandleFunc("GET "+pDM, a.wrap(accUser, a.dm))
	mux.HandleFunc("POST "+pDM, a.wrap(accUser, a.dmPost))
	mux.HandleFunc("GET "+pNew, a.wrap(accGroup, a.newGroup))
	mux.HandleFunc("POST "+pNew, a.wrap(accGroup, a.newGroupPost))
	mux.HandleFunc("GET "+pChat+"{id}", a.wrap(accUser, a.chat))
	mux.HandleFunc("GET "+pChat+"{id}/info", a.wrap(accUser, a.chatInfo))
	mux.HandleFunc("POST "+pChat+"{id}/send", a.wrap(accUser, a.chatSend))
	mux.HandleFunc("POST "+pChat+"{id}/react", a.wrap(accUser, a.chatReact))
	mux.HandleFunc("POST "+pChat+"{id}/archive", a.wrap(accUser, a.chatArchive))
	mux.HandleFunc("POST "+pChat+"{id}/unarchive", a.wrap(accUser, a.chatUnarchive))
	mux.HandleFunc("GET "+pChat+"{id}/m/{mid}/edit", a.wrap(accUser, a.messageEdit))
	mux.HandleFunc("POST "+pChat+"{id}/m/{mid}/edit", a.wrap(accUser, a.messageEditPost))
	mux.HandleFunc("POST "+pChat+"{id}/m/{mid}/delete", a.wrap(accUser, a.messageDelete))
	mux.HandleFunc("POST "+pChat+"{id}/rename", a.wrap(accUser, a.chatRename))
	mux.HandleFunc("POST "+pChat+"{id}/members/add", a.wrap(accUser, a.chatMembersAdd))
	mux.HandleFunc("POST "+pChat+"{id}/members/remove", a.wrap(accUser, a.chatMembersRemove))
	mux.HandleFunc("GET "+pAdmin, a.wrap(accAdmin, a.admin))
	mux.HandleFunc("GET "+pAdminUser+"{id}", a.wrap(accAdmin, a.adminUser))
	mux.HandleFunc("POST "+pAdminUser+"{id}/perms", a.wrap(accAdmin, a.adminUserPerms))
	mux.HandleFunc("POST "+pAdminUser+"{id}/passkey/delete", a.wrap(accAdmin, a.adminUserPasskeyDelete))
	mux.HandleFunc("POST "+pAdminUser+"{id}/link", a.wrap(accAdmin, a.adminUserLink))
	mux.HandleFunc("POST "+pAdminUser+"{id}/link/revoke", a.wrap(accAdmin, a.adminUserLinkRevoke))
	mux.HandleFunc("GET "+pInvites, a.wrap(accInvite, a.invites))
	mux.HandleFunc("POST "+pInvites, a.wrap(accInvite, a.invitesPost))
	mux.HandleFunc("POST "+pInvitesRevoke, a.wrap(accInvite, a.invitesRevoke))
	mux.HandleFunc("GET "+pSettings, a.wrap(accUser, a.settings))
	mux.HandleFunc("POST "+pSettingsLink, a.wrap(accUser, a.settingsLink))
	mux.HandleFunc("POST "+pSettingsLinkRevoke, a.wrap(accUser, a.settingsLinkRevoke))
	mux.HandleFunc("GET "+pPasskeyOptions, a.wrap(accUser, a.passkeyOptions))
	mux.HandleFunc("POST "+pPasskeyCreate, a.wrap(accUser, a.passkeyCreate))
	mux.HandleFunc("POST "+pPasskeyDelete, a.wrap(accUser, a.passkeyDelete))
	mux.HandleFunc("POST "+pPushSubscribe, a.wrap(accUser, a.pushSubscribe))
	mux.HandleFunc("POST "+pPushUnsubscribe, a.wrap(accUser, a.pushUnsubscribe))
	mux.HandleFunc("POST "+pPushOpened, a.wrap(accUser, a.pushOpened))
	if a.cfg.Dev {
		mux.HandleFunc("GET "+pLivereload, livereload.Handler)
		mux.HandleFunc("GET "+pLivereloadJS, a.wrap(accPublic, a.livereloadJS))
	}
	return secureHandler(a.cfg.csp(), http.NewCrossOriginProtection().Handler(mux))
}

func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	a, err := newApp(ctx, cfg)
	if err != nil {
		return err
	}
	if a.fakeS3 != nil {
		fmt.Printf("using built in fake S3 in %s; not for production\n", a.fakeS3.dir)
	}
	if err := a.s3.selfCheck(ctx, cfg.S3URLValidity); err != nil {
		_ = a.close()
		return err
	}
	if err := a.bootstrap(ctx); err != nil {
		_ = a.close()
		return err
	}
	a.startJanitor()
	if err := a.startMedia(); err != nil {
		cancel()
		a.bg.Wait()
		_ = a.close()
		return err
	}
	serveErr := lands.ListenAndServe(a.ctx, defaultAddr, a.handler())
	cancel()
	a.bg.Wait()
	if err := a.close(); err != nil && serveErr == nil {
		return err
	}
	return serr.Wrap(serveErr)
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
}
