package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LumenResearch/uasurfer"
	"github.com/daaku/serr"
	"github.com/daaku/sookie"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
	"modernc.org/sqlite"
)

const (
	inputJSON = "json"

	sessionMaxAge  = 90 * 24 * time.Hour
	sessionRenew   = 24 * time.Hour
	ceremonyMaxAge = 10 * time.Minute
	bootstrapTTL   = 24 * time.Hour

	msgInviteInvalid = "This link is invalid or has expired."
)

// validUsername normalises a username and checks it against the rule.
func validUsername(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !usernameRe.MatchString(s) {
		return "", errors.New("Usernames are 2 to 32 characters: lowercase letters, " +
			"digits, dot, dash and underscore, starting with a letter or digit.")
	}
	return s, nil
}

// isUniqueErr reports whether err is an SQLite constraint violation.
func isUniqueErr(err error) bool {
	se, ok := errors.AsType[*sqlite.Error](err)
	return ok && se.Code()&0xff == 19 // SQLITE_CONSTRAINT
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never returns an error
	return b
}

// webauthn

func newWebAuthn(cfg config) (*webauthn.WebAuthn, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: cfg.AppName,
		RPOrigins:     []string{cfg.PublicURL},
	})
	return wa, serr.Wrap(err)
}

// waUser adapts a user, or a user about to be created from an invite, to the
// webauthn library. It holds everything it needs, so its methods never touch
// the database.
type waUser struct {
	handle []byte
	name   string
	creds  []webauthn.Credential
}

func (u waUser) WebAuthnID() []byte                         { return u.handle }
func (u waUser) WebAuthnName() string                       { return u.name }
func (u waUser) WebAuthnDisplayName() string                { return u.name }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func decodeCredentials(rows []Credential) ([]webauthn.Credential, error) {
	creds := make([]webauthn.Credential, len(rows))
	for i, row := range rows {
		if err := json.Unmarshal(row.Credential, &creds[i]); err != nil {
			return nil, serr.Wrap(err)
		}
	}
	return creds, nil
}

// waUserFor loads the credentials of an existing user.
func (a *app) waUserFor(ctx context.Context, handle []byte, name string, userID int64) (waUser, error) {
	u := waUser{handle: handle, name: name}
	if userID == 0 {
		return u, nil
	}
	rows, err := a.q.CredentialsByUser(ctx, userID)
	if err != nil {
		return u, serr.Wrap(err)
	}
	u.creds, err = decodeCredentials(rows)
	return u, err
}

// registrationOptions starts a passkey registration for u, excluding the
// passkeys it already has, and stores the ceremony state in a cookie.
func (a *app) registrationOptions(w http.ResponseWriter, u waUser) error {
	options, data, err := a.wa.BeginRegistration(u,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationPreferred,
		}),
		webauthn.WithExclusions(webauthn.Credentials(u.creds).CredentialDescriptors()),
	)
	if err != nil {
		return serr.Wrap(err)
	}
	c := a.cookie("r", ceremonyMaxAge)
	if err := sookie.Set(a.secret("register"), w, data, c); err != nil {
		return serr.Wrap(err)
	}
	return writeJSON(w, options)
}

// finishRegistration verifies the browser's response against the ceremony
// cookie, which it always clears.
func (a *app) finishRegistration(w http.ResponseWriter, r *http.Request, u waUser) (*webauthn.Credential, error) {
	c := a.cookie("r", ceremonyMaxAge)
	data, err := sookie.Get[webauthn.SessionData](a.secret("register"), r, c.Name)
	sookie.Del(w, r, c)
	if err != nil {
		return nil, httpErr(http.StatusBadRequest, "The passkey request expired. Please try again.")
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes([]byte(r.FormValue(inputJSON)))
	if err != nil {
		return nil, httpErr(http.StatusBadRequest, "The passkey response was not understood.")
	}
	cred, err := a.wa.CreateCredential(u, data, parsed)
	if err != nil {
		return nil, httpErr(http.StatusBadRequest, "The passkey could not be verified.")
	}
	return cred, nil
}

// credentialParams builds the row for a freshly registered passkey.
func (a *app) credentialParams(r *http.Request, userID int64, cred *webauthn.Credential) (CredentialCreateParams, error) {
	credJSON, err := json.Marshal(cred)
	if err != nil {
		return CredentialCreateParams{}, serr.Wrap(err)
	}
	now := a.now().Unix()
	return CredentialCreateParams{
		UserID:       userID,
		CredentialID: cred.ID,
		Credential:   credJSON,
		Label:        deviceLabel(r),
		CreatedAt:    now,
		LastUsedAt:   now,
	}, nil
}

// deviceLabel describes the browser making the request, as in "Safari on iOS".
func deviceLabel(r *http.Request) string {
	ua := uasurfer.Parse(r.Header.Get("User-Agent"))
	return fmt.Sprintf("%s on %s",
		ua.Browser.Name.StringTrimPrefix(), ua.OS.Name.StringTrimPrefix())
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	return serr.Wrap(json.NewEncoder(w).Encode(v))
}

// cookies and sessions

func (a *app) cookie(name string, maxAge time.Duration) http.Cookie {
	return http.Cookie{
		Name:     name,
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(a.cfg.PublicURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	}
}

// session is the content of the session cookie. It is bound to the passkey
// it was created with: deleting that passkey ends the session.
type session struct {
	UserID       int64
	CredentialID int64
	IssuedAt     int64
}

func (a *app) setSession(w http.ResponseWriter, userID, credentialID int64) error {
	s := session{UserID: userID, CredentialID: credentialID, IssuedAt: a.now().Unix()}
	return serr.Wrap(sookie.Set(a.secret("session"), w, s, a.cookie("s", sessionMaxAge)))
}

type authInfo struct {
	user         *User
	credentialID int64
}

type authKey struct{}

// userFrom returns the signed in user, or nil.
func userFrom(ctx context.Context) *User {
	if ai, ok := ctx.Value(authKey{}).(*authInfo); ok {
		return ai.user
	}
	return nil
}

// credentialFrom returns the row id of the passkey the session was made with.
func credentialFrom(ctx context.Context) int64 {
	if ai, ok := ctx.Value(authKey{}).(*authInfo); ok {
		return ai.credentialID
	}
	return 0
}

// withSession resolves the session cookie. A missing, invalid or dead
// session clears the cookie and leaves the request signed out.
func (a *app) withSession(w http.ResponseWriter, r *http.Request) (*http.Request, error) {
	c := a.cookie("s", sessionMaxAge)
	s, err := sookie.Get[session](a.secret("session"), r, c.Name)
	if err != nil {
		if !errors.Is(err, http.ErrNoCookie) {
			sookie.Del(w, r, c)
		}
		return r, nil
	}
	user, err := a.q.UserBySession(r.Context(), UserBySessionParams{
		UserID:       s.UserID,
		CredentialID: s.CredentialID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		sookie.Del(w, r, c)
		return r, nil
	}
	if err != nil {
		return r, serr.Wrap(err)
	}
	if a.now().Unix()-s.IssuedAt > int64(sessionRenew.Seconds()) {
		if err := a.setSession(w, s.UserID, s.CredentialID); err != nil {
			return r, err
		}
	}
	ai := &authInfo{user: &user, credentialID: s.CredentialID}
	return r.WithContext(context.WithValue(r.Context(), authKey{}, ai)), nil
}

// allowed reports whether the user (nil when signed out) passes the level.
func allowed(u *User, acc access) bool {
	switch {
	case acc == accPublic:
		return true
	case u == nil:
		return false
	case u.PermAdmin, acc == accUser:
		return true
	case acc == accInvite:
		return u.PermInvite
	case acc == accGroup:
		return u.PermGroup
	}
	return false
}

// sign in

func (a *app) passkeyForm(action, dataAttr, optionsURL, label string) g.Node {
	return h.Form(h.Method("post"), h.Action(action), h.Class("stack"),
		h.Input(h.Type("hidden"), h.Name(inputJSON)),
		h.Button(h.Class("btn"), h.Type("button"), h.Data(dataAttr, optionsURL),
			icon("key"), h.Span(g.Text(label))),
		h.P(h.Class("form-error"), h.Role("alert")))
}

func (a *app) signIn(w http.ResponseWriter, r *http.Request) error {
	if userFrom(r.Context()) != nil {
		http.Redirect(w, r, pHome, http.StatusSeeOther)
		return nil
	}
	return a.pageSimple(w, r, "Sign in", nil,
		a.passkeyForm(pSignIn, "pk-get", pSignInOptions, "Sign in with passkey"),
	).Render(w)
}

func (a *app) signInOptions(w http.ResponseWriter, r *http.Request) error {
	options, data, err := a.wa.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return serr.Wrap(err)
	}
	c := a.cookie("l", ceremonyMaxAge)
	if err := sookie.Set(a.secret("login"), w, data, c); err != nil {
		return serr.Wrap(err)
	}
	return writeJSON(w, options)
}

func (a *app) signInPost(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	fail := func(msg string) error {
		return a.flash(w, r, pSignIn, Flash{Content: msg})
	}
	c := a.cookie("l", ceremonyMaxAge)
	data, err := sookie.Get[webauthn.SessionData](a.secret("login"), r, c.Name)
	sookie.Del(w, r, c)
	if err != nil {
		return fail("The sign in request expired. Please try again.")
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes([]byte(r.FormValue(inputJSON)))
	if err != nil {
		return fail("The passkey response was not understood.")
	}

	var user User
	var lookupErr error
	_, cred, err := a.wa.ValidatePasskeyLogin(
		func(rawID, userHandle []byte) (webauthn.User, error) {
			user, lookupErr = a.q.UserByHandle(ctx, userHandle)
			if lookupErr != nil {
				return nil, lookupErr
			}
			var wu waUser
			wu, lookupErr = a.waUserFor(ctx, user.Handle, user.Username, user.ID)
			return wu, lookupErr
		}, data, parsed)
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return serr.Wrap(lookupErr)
	}
	if err != nil {
		return fail("This passkey is not registered here.")
	}

	row, err := a.q.CredentialByCredentialID(ctx, cred.ID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && row.UserID != user.ID) {
		return fail("This passkey is not registered here.")
	}
	if err != nil {
		return serr.Wrap(err)
	}
	credJSON, err := json.Marshal(cred)
	if err != nil {
		return serr.Wrap(err)
	}
	err = a.q.CredentialUpdate(ctx, CredentialUpdateParams{
		ID:         row.ID,
		Credential: credJSON,
		LastUsedAt: a.now().Unix(),
	})
	if err != nil {
		return serr.Wrap(err)
	}
	if err := a.setSession(w, user.ID, row.ID); err != nil {
		return err
	}
	http.Redirect(w, r, pHome, http.StatusSeeOther)
	return nil
}

func (a *app) signOut(w http.ResponseWriter, r *http.Request) error {
	sookie.Del(w, r, a.cookie("s", sessionMaxAge))
	http.Redirect(w, r, pSignIn, http.StatusSeeOther)
	return nil
}

// invites

type inviteParams struct {
	Username  string        // for a new user invite
	User      *User         // set for an add passkey link for an existing user
	Admin     bool          // only the bootstrap invite
	CreatedBy int64         // 0 for the bootstrap invite
	TTL       time.Duration // how long the link works
}

// createInvite stores a single use link. For a new user it reserves the
// username and picks the handle the user will get.
func (a *app) createInvite(ctx context.Context, q *Queries, p inviteParams) (Invite, error) {
	now := a.now()
	arg := InviteCreateParams{
		Token:     base64.RawURLEncoding.EncodeToString(randBytes(32)),
		Username:  p.Username,
		Handle:    randBytes(16),
		PermAdmin: p.Admin,
		CreatedBy: sql.NullInt64{Int64: p.CreatedBy, Valid: p.CreatedBy != 0},
		CreatedAt: now.Unix(),
		ExpiresAt: now.Add(p.TTL).Unix(),
	}
	if p.User != nil {
		arg.Username = p.User.Username
		arg.Handle = p.User.Handle
		arg.UserID = sql.NullInt64{Int64: p.User.ID, Valid: true}
		arg.PermAdmin = false
	}
	inv, err := q.InviteCreate(ctx, arg)
	return inv, serr.Wrap(err)
}

func invitePath(token string) string {
	return pInvite + url.PathEscape(token)
}

func (a *app) inviteURL(inv Invite) string {
	return a.cfg.PublicURL + invitePath(inv.Token)
}

// inviteFor loads the live invite named in the request path and marks the
// response so the token does not leak through caches or referrers.
func (a *app) inviteFor(w http.ResponseWriter, r *http.Request) (Invite, error) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	inv, err := a.q.InviteByToken(r.Context(), InviteByTokenParams{
		Token: r.PathValue("token"),
		Now:   a.now().Unix(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return inv, httpErr(http.StatusNotFound, msgInviteInvalid)
	}
	return inv, serr.Wrap(err)
}

func (a *app) invite(w http.ResponseWriter, r *http.Request) error {
	inv, err := a.inviteFor(w, r)
	if err != nil {
		return err
	}
	title, intro, label := "Welcome",
		"Welcome, "+inv.Username+". Create a passkey to finish setting up your account.",
		"Create passkey"
	if inv.UserID.Valid {
		title, intro, label = "Add a passkey",
			"Add a passkey for "+inv.Username+" on this device.",
			"Add passkey"
	}
	path := invitePath(inv.Token)
	return a.pageSimple(w, r, title, nil,
		h.P(g.Text(intro)),
		a.passkeyForm(path, "pk-create", path+"/options", label),
	).Render(w)
}

func (a *app) inviteOptions(w http.ResponseWriter, r *http.Request) error {
	inv, err := a.inviteFor(w, r)
	if err != nil {
		return err
	}
	u, err := a.waUserFor(r.Context(), inv.Handle, inv.Username, inv.UserID.Int64)
	if err != nil {
		return err
	}
	return a.registrationOptions(w, u)
}

func (a *app) invitePost(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	inv, err := a.inviteFor(w, r)
	if err != nil {
		return err
	}
	wu, err := a.waUserFor(ctx, inv.Handle, inv.Username, inv.UserID.Int64)
	if err != nil {
		return err
	}
	cred, err := a.finishRegistration(w, r, wu)
	if err != nil {
		return err
	}

	var row Credential
	err = a.tx(ctx, func(q *Queries) error {
		// Deleting the invite first is what makes the link single use.
		used, err := q.InviteUse(ctx, InviteUseParams{Token: inv.Token, Now: a.now().Unix()})
		if errors.Is(err, sql.ErrNoRows) {
			return httpErr(http.StatusNotFound, msgInviteInvalid)
		}
		if err != nil {
			return serr.Wrap(err)
		}
		userID := used.UserID.Int64
		if !used.UserID.Valid {
			user, err := q.UserCreate(ctx, UserCreateParams{
				Username:  used.Username,
				Handle:    used.Handle,
				PermAdmin: used.PermAdmin,
				InvitedBy: used.CreatedBy,
				CreatedAt: a.now().Unix(),
			})
			if err != nil {
				return serr.Wrap(err)
			}
			userID = user.ID
		}
		arg, err := a.credentialParams(r, userID, cred)
		if err != nil {
			return err
		}
		row, err = q.CredentialCreate(ctx, arg)
		if isUniqueErr(err) {
			return httpErr(http.StatusBadRequest, "This passkey is already registered.")
		}
		return serr.Wrap(err)
	})
	if err != nil {
		return err
	}
	if err := a.setSession(w, row.UserID, row.ID); err != nil {
		return err
	}
	http.Redirect(w, r, pHome, http.StatusSeeOther)
	return nil
}

// bootstrap prints a sign up link for the first admin while there are no
// users. Each start replaces the previous link.
func (a *app) bootstrap(ctx context.Context) error {
	n, err := a.q.UserCount(ctx)
	if err != nil {
		return serr.Wrap(err)
	}
	if n > 0 {
		return nil
	}
	var inv Invite
	err = a.tx(ctx, func(q *Queries) error {
		if err := q.InviteDeleteBootstrap(ctx); err != nil {
			return serr.Wrap(err)
		}
		inv, err = a.createInvite(ctx, q, inviteParams{
			Username: a.cfg.AdminUsername,
			Admin:    true,
			TTL:      bootstrapTTL,
		})
		return err
	})
	if err != nil {
		return err
	}
	fmt.Println("Bootstrap admin:", a.inviteURL(inv))
	return nil
}

// settings

const (
	addPasskeyTTL   = 24 * time.Hour
	maxPendingLinks = 5
)

// formID reads a row id posted in the named form field.
func formID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.FormValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, httpErr(http.StatusBadRequest, "Bad request.")
	}
	return id, nil
}

// permNames lists the permissions a user holds, as words.
func permNames(u User) []string {
	var names []string
	if u.PermAdmin {
		names = append(names, "Admin")
	}
	if u.PermInvite {
		names = append(names, "Invite")
	}
	if u.PermGroup {
		names = append(names, "Create groups")
	}
	return names
}

// dateCell renders a labelled timestamp, as in "Created 2 Jan 2026 03:04".
func dateCell(label string, unix int64) g.Node {
	return h.Span(h.Class("muted"), g.Text(label+" "+fmtTime(time.Unix(unix, 0), nil)))
}

// passkeyList renders passkey rows. current is the row id marked "this
// device" (0 for none). A delete button posting to deleteAction is rendered
// for each row when canDelete is true.
func passkeyList(creds []Credential, current int64, deleteAction string, canDelete bool) g.Node {
	if len(creds) == 0 {
		return h.P(h.Class("muted"), g.Text("No passkeys."))
	}
	return h.Ul(h.Class("list"), g.Map(creds, func(c Credential) g.Node {
		return h.Li(h.ID(fmt.Sprintf("passkey-%d", c.ID)), h.Class("row"),
			h.Div(h.Class("stack-tight"),
				h.Span(h.Class("cluster"),
					h.Strong(g.Text(c.Label)),
					g.If(c.ID == current, h.Span(h.Class("badge"), g.Text("this device")))),
				dateCell("Created", c.CreatedAt),
				dateCell("Last used", c.LastUsedAt)),
			g.If(canDelete, h.Form(h.Method("post"), h.Action(deleteAction),
				h.Input(h.Type("hidden"), h.Name("id"), h.Value(strconv.FormatInt(c.ID, 10))),
				h.Button(h.Class("btn danger"), g.Text("Delete")))))
	}))
}

// linkList renders pending add passkey links, each with a copyable URL, its
// expiry, and a revoke button posting to revokeAction.
func (a *app) linkList(links []Invite, revokeAction string) g.Node {
	if len(links) == 0 {
		return h.P(h.Class("muted"), g.Text("No pending links."))
	}
	return h.Ul(h.Class("list"), g.Map(links, func(inv Invite) g.Node {
		id := fmt.Sprintf("link-%d", inv.ID)
		return h.Li(h.ID(id), h.Class("stack-tight"),
			h.Input(h.ID(id+"-url"), h.Type("text"), h.ReadOnly(), h.Data("copy", ""),
				h.Value(a.inviteURL(inv)), h.Aria("label", "Link")),
			h.Div(h.Class("row"),
				dateCell("Expires", inv.ExpiresAt),
				h.Form(h.Method("post"), h.Action(revokeAction),
					h.Input(h.Type("hidden"), h.Name("id"), h.Value(strconv.FormatInt(inv.ID, 10))),
					h.Button(h.Class("btn secondary"), g.Text("Revoke")))))
	}))
}

// settingsSection renders one section of the settings page.
type settingsSection func(r *http.Request, u *User) (g.Node, error)

// settingsSections are the sections of the settings page, in order. Later
// features append to this list.
func (a *app) settingsSections() []settingsSection {
	return []settingsSection{a.settingsPasskeys, a.settingsDevices}
}

func (a *app) settings(w http.ResponseWriter, r *http.Request) error {
	user := userFrom(r.Context())
	w.Header().Set("Cache-Control", "no-store") // holds add passkey links
	perms := "None"
	if names := permNames(*user); len(names) > 0 {
		perms = strings.Join(names, ", ")
	}
	body := []g.Node{
		h.Section(h.ID("account"), h.Class("card"),
			h.Dl(h.Class("facts"),
				h.Dt(g.Text("Username")), h.Dd(g.Text(user.Username)),
				h.Dt(g.Text("Permissions")), h.Dd(g.Text(perms)))),
	}
	for _, section := range a.settingsSections() {
		node, err := section(r, user)
		if err != nil {
			return err
		}
		body = append(body, node)
	}
	body = append(body, h.Form(h.Method("post"), h.Action(pSignOut),
		h.Button(h.Class("btn secondary"), g.Text("Sign out"))))
	return a.pageSimple(w, r, "Settings", backLink(pHome, "Home"), body...).Render(w)
}

func (a *app) settingsPasskeys(r *http.Request, u *User) (g.Node, error) {
	creds, err := a.q.CredentialsByUser(r.Context(), u.ID)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	return h.Section(h.ID("passkeys"), h.Class("stack"),
		h.H2(g.Text("Passkeys")),
		passkeyList(creds, credentialFrom(r.Context()), pPasskeyDelete, len(creds) > 1),
		a.passkeyForm(pPasskeyCreate, "pk-create", pPasskeyOptions, "Add a passkey on this device"),
	), nil
}

func (a *app) settingsDevices(r *http.Request, u *User) (g.Node, error) {
	links, err := a.q.InvitesPendingForUser(r.Context(), InvitesPendingForUserParams{
		UserID: sql.NullInt64{Int64: u.ID, Valid: true},
		Now:    a.now().Unix(),
	})
	if err != nil {
		return nil, serr.Wrap(err)
	}
	return h.Section(h.ID("devices"), h.Class("stack"),
		h.H2(g.Text("Add a passkey on another device")),
		h.P(h.Class("muted"), g.Text("Create a link, open it on the other device and "+
			"add a passkey there. A link works once, for 24 hours.")),
		h.Form(h.Method("post"), h.Action(pSettingsLink),
			h.Button(h.Class("btn secondary"), icon("plus"), h.Span(g.Text("Create link")))),
		a.linkList(links, pSettingsLinkRevoke),
	), nil
}

func (a *app) passkeyOptions(w http.ResponseWriter, r *http.Request) error {
	user := userFrom(r.Context())
	wu, err := a.waUserFor(r.Context(), user.Handle, user.Username, user.ID)
	if err != nil {
		return err
	}
	return a.registrationOptions(w, wu)
}

func (a *app) passkeyCreate(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	fail := func(msg string) error {
		return a.flash(w, r, pSettings, Flash{Kind: flashError, Content: msg})
	}
	wu, err := a.waUserFor(ctx, user.Handle, user.Username, user.ID)
	if err != nil {
		return err
	}
	cred, err := a.finishRegistration(w, r, wu)
	if he, ok := errors.AsType[*httpError](err); ok {
		return fail(he.msg)
	}
	if err != nil {
		return err
	}
	arg, err := a.credentialParams(r, user.ID, cred)
	if err != nil {
		return err
	}
	if _, err := a.q.CredentialCreate(ctx, arg); isUniqueErr(err) {
		return fail("This passkey is already registered.")
	} else if err != nil {
		return serr.Wrap(err)
	}
	return a.flash(w, r, pSettings, Flash{Kind: flashSuccess, Content: "Passkey added."})
}

func (a *app) passkeyDelete(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	id, err := formID(r, "id")
	if err != nil {
		return err
	}
	var last bool
	var deleted int64
	err = a.tx(ctx, func(q *Queries) error {
		n, err := q.CredentialCountByUser(ctx, user.ID)
		if err != nil {
			return serr.Wrap(err)
		}
		if n <= 1 {
			last = true
			return nil
		}
		deleted, err = q.CredentialDelete(ctx, CredentialDeleteParams{ID: id, UserID: user.ID})
		return serr.Wrap(err)
	})
	switch {
	case err != nil:
		return err
	case last:
		return a.flash(w, r, pSettings, Flash{Kind: flashError,
			Content: "You cannot delete your only passkey."})
	case deleted == 0:
		http.Redirect(w, r, pSettings, http.StatusSeeOther)
		return nil
	case id == credentialFrom(ctx):
		// The session was bound to this passkey, so it is over.
		sookie.Del(w, r, a.cookie("s", sessionMaxAge))
		return a.flash(w, r, pSignIn, Flash{Kind: flashSuccess,
			Content: "Passkey deleted. You are signed out on this device."})
	}
	return a.flash(w, r, pSettings, Flash{Kind: flashSuccess, Content: "Passkey deleted."})
}

func (a *app) settingsLink(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	var full bool
	err := a.tx(ctx, func(q *Queries) error {
		links, err := q.InvitesPendingForUser(ctx, InvitesPendingForUserParams{
			UserID: sql.NullInt64{Int64: user.ID, Valid: true},
			Now:    a.now().Unix(),
		})
		if err != nil {
			return serr.Wrap(err)
		}
		if len(links) >= maxPendingLinks {
			full = true
			return nil
		}
		_, err = a.createInvite(ctx, q, inviteParams{
			User:      user,
			CreatedBy: user.ID,
			TTL:       addPasskeyTTL,
		})
		return err
	})
	if err != nil {
		return err
	}
	if full {
		return a.flash(w, r, pSettings+"#devices", Flash{Kind: flashError,
			Content: fmt.Sprintf("You can have at most %d pending links. Revoke one first.", maxPendingLinks)})
	}
	return a.flash(w, r, pSettings+"#devices", Flash{Kind: flashSuccess,
		Content: "Link created. Open it on the other device."})
}

func (a *app) settingsLinkRevoke(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	id, err := formID(r, "id")
	if err != nil {
		return err
	}
	n, err := a.q.InviteRevokeForUser(ctx, InviteRevokeForUserParams{
		ID:     id,
		UserID: sql.NullInt64{Int64: user.ID, Valid: true},
	})
	if err != nil {
		return serr.Wrap(err)
	}
	if n == 0 {
		return httpErr(http.StatusNotFound, "Not found.")
	}
	return a.flash(w, r, pSettings+"#devices", Flash{Kind: flashSuccess, Content: "Link revoked."})
}
