package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
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
	"github.com/skudra-go/ant"
	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

const (
	inputJSON = "json"

	sessionMaxAge  = 90 * 24 * time.Hour
	sessionRenew   = 24 * time.Hour
	ceremonyMaxAge = 10 * time.Minute
	bootstrapTTL   = 24 * time.Hour

	msgInviteInvalid = "This link is invalid or has expired."
)

const (
	// usernamePattern is usernameRe for the HTML pattern attribute, which is
	// implicitly anchored and compiled with the v flag.
	usernamePattern = `[a-z0-9][a-z0-9_.\-]{1,31}`
	msgUsernameRule = "Usernames are 2 to 32 characters: lowercase letters, " +
		"digits, dot, dash and underscore, starting with a letter or digit."
)

// validUsername normalises a username and checks it against the rule.
func validUsername(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !usernameRe.MatchString(s) {
		return "", errors.New(msgUsernameRule)
	}
	return s, nil
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

func decodeCredentials(rows []*Credential) ([]webauthn.Credential, error) {
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
	rows, err := a.credentials(ctx, userID)
	if err != nil {
		return u, err
	}
	u.creds, err = decodeCredentials(rows)
	return u, err
}

// credentials returns the passkeys of userID, oldest first.
func (a *app) credentials(ctx context.Context, userID int64) ([]*Credential, error) {
	creds, err := a.db.Credentials.Where(ant.Eq(&Credentials.UserID, userID)).
		OrderBy(&Credentials.ID).All(ctx)
	return creds, serr.Wrap(err)
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

// newCredential builds the row for a freshly registered passkey.
func (a *app) newCredential(r *http.Request, userID int64, cred *webauthn.Credential) (*Credential, error) {
	credJSON, err := json.Marshal(cred)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	now := a.now().Unix()
	return &Credential{
		UserID:       userID,
		CredentialID: cred.ID,
		Credential:   credJSON,
		Label:        deviceLabel(r),
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
	user, err := a.db.Users.Where(ant.Eq(&Users.ID, s.UserID),
		ant.Has(&Users.Credentials, ant.Eq(&Credentials.ID, s.CredentialID))).
		Single(r.Context())
	if errors.Is(err, ant.ErrNotFound) {
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
	ai := &authInfo{user: user, credentialID: s.CredentialID}
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

	var user *User
	var lookupErr error
	_, cred, err := a.wa.ValidatePasskeyLogin(
		func(rawID, userHandle []byte) (webauthn.User, error) {
			user, lookupErr = a.db.Users.Where(ant.Col(&Users.Handle).Eq(userHandle)).Single(ctx)
			if lookupErr != nil {
				return nil, lookupErr
			}
			var wu waUser
			wu, lookupErr = a.waUserFor(ctx, user.Handle, user.Username, user.ID)
			return wu, lookupErr
		}, data, parsed)
	if lookupErr != nil && !errors.Is(lookupErr, ant.ErrNotFound) {
		return serr.Wrap(lookupErr)
	}
	if err != nil {
		return fail("This passkey is not registered here.")
	}

	row, err := a.db.Credentials.Where(ant.Col(&Credentials.CredentialID).Eq(cred.ID)).Single(ctx)
	if errors.Is(err, ant.ErrNotFound) || (err == nil && row.UserID != user.ID) {
		return fail("This passkey is not registered here.")
	}
	if err != nil {
		return serr.Wrap(err)
	}
	credJSON, err := json.Marshal(cred)
	if err != nil {
		return serr.Wrap(err)
	}
	_, err = a.db.Credentials.Where(ant.Eq(&Credentials.ID, row.ID)).Update(ctx,
		ant.Set(&Credentials.Credential, credJSON),
		ant.Set(&Credentials.LastUsedAt, a.now().Unix()))
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
func (a *app) createInvite(ctx context.Context, db *AppDB, p inviteParams) (*Invite, error) {
	now := a.now()
	inv := &Invite{
		Token:     base64.RawURLEncoding.EncodeToString(randBytes(32)),
		Username:  p.Username,
		Handle:    randBytes(16),
		PermAdmin: p.Admin,
		CreatedBy: nullID(p.CreatedBy),
		ExpiresAt: now.Add(p.TTL).Unix(),
	}
	if p.User != nil {
		inv.Username = p.User.Username
		inv.Handle = p.User.Handle
		inv.UserID = nullID(p.User.ID)
		inv.PermAdmin = false
	}
	return inv, serr.Wrap(db.Invites.Insert(ctx, inv))
}

// nullID is id as a nullable reference, NULL for 0.
func nullID(id int64) sql.NullInt64 {
	return sql.NullInt64{Int64: id, Valid: id != 0}
}

// inviteLive matches the invites that have not expired.
func (a *app) inviteLive() ant.Cond {
	return ant.Gt(&Invites.ExpiresAt, a.now().Unix())
}

// pendingLinks returns the live add passkey links of userID, newest first.
func (a *app) pendingLinks(ctx context.Context, db *AppDB, userID int64) ([]*Invite, error) {
	links, err := db.Invites.Where(ant.Eq(&Invites.UserID, nullID(userID)), a.inviteLive()).
		OrderByDesc(&Invites.ID).All(ctx)
	return links, serr.Wrap(err)
}

// revokeLink deletes the add passkey link id of userID, reporting whether
// there was one.
func (a *app) revokeLink(ctx context.Context, id, userID int64) (bool, error) {
	n, err := a.db.Invites.Where(ant.Eq(&Invites.ID, id),
		ant.Eq(&Invites.UserID, nullID(userID))).Delete(ctx)
	return n > 0, serr.Wrap(err)
}

func invitePath(token string) string {
	return pInvite + url.PathEscape(token)
}

func (a *app) inviteURL(inv *Invite) string {
	return a.cfg.PublicURL + invitePath(inv.Token)
}

// inviteFor loads the live invite named in the request path and marks the
// response so the token does not leak through caches or referrers.
func (a *app) inviteFor(w http.ResponseWriter, r *http.Request) (*Invite, error) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	inv, err := a.db.Invites.Where(ant.Eq(&Invites.Token, r.PathValue("token")), a.inviteLive()).
		Single(r.Context())
	if errors.Is(err, ant.ErrNotFound) {
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

	var row *Credential
	err = a.db.Tx(ctx, func(tx *AppDB) error {
		// Deleting the invite first is what makes the link single use.
		deleted, err := tx.Invites.Where(ant.Eq(&Invites.Token, inv.Token), a.inviteLive()).
			DeleteReturning(ctx)
		if err != nil {
			return serr.Wrap(err)
		}
		if len(deleted) == 0 {
			return httpErr(http.StatusNotFound, msgInviteInvalid)
		}
		used := deleted[0]
		userID := used.UserID.Int64
		if !used.UserID.Valid {
			user := &User{
				Username:  used.Username,
				Handle:    used.Handle,
				PermAdmin: used.PermAdmin,
				InvitedBy: used.CreatedBy,
			}
			if err := tx.Users.Insert(ctx, user); err != nil {
				return serr.Wrap(err)
			}
			userID = user.ID
		}
		if row, err = a.newCredential(r, userID, cred); err != nil {
			return err
		}
		err = tx.Credentials.Insert(ctx, row)
		if ant.IsUniqueViolation(err) {
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
	n, err := a.db.Users.Count(ctx)
	if err != nil {
		return serr.Wrap(err)
	}
	if n > 0 {
		return nil
	}
	var inv *Invite
	err = a.db.Tx(ctx, func(tx *AppDB) error {
		_, err := tx.Invites.Where(ant.IsNull(&Invites.CreatedBy), ant.IsNull(&Invites.UserID)).
			Delete(ctx)
		if err != nil {
			return serr.Wrap(err)
		}
		inv, err = a.createInvite(ctx, tx, inviteParams{
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
func passkeyList(creds []*Credential, current int64, deleteAction string, canDelete bool) g.Node {
	if len(creds) == 0 {
		return h.P(h.Class("muted"), g.Text("No passkeys."))
	}
	return h.Ul(h.Class("list"), g.Map(creds, func(c *Credential) g.Node {
		return h.Li(h.ID(fmt.Sprintf("passkey-%d", c.ID)), h.Class("row"),
			h.Div(h.Class("stack-tight"),
				h.Span(h.Class("cluster"),
					h.Strong(g.Text(c.Label)),
					g.If(c.ID == current, h.Span(h.Class("badge"), g.Text("this device")))),
				dateCell("Created", c.CreatedAt.Unix()),
				dateCell("Last used", c.LastUsedAt)),
			g.If(canDelete, h.Form(h.Method("post"), h.Action(deleteAction),
				h.Input(h.Type("hidden"), h.Name("id"), h.Value(strconv.FormatInt(c.ID, 10))),
				h.Button(h.Class("btn danger"), g.Text("Delete")))))
	}))
}

// copyField is a read only field holding the full URL of an invite, selected
// as a whole when focused.
func (a *app) copyField(id string, inv *Invite) g.Node {
	return h.Input(h.ID(id), h.Type("text"), h.ReadOnly(), h.Data("copy", ""),
		h.Value(a.inviteURL(inv)), h.Aria("label", "Link"))
}

// linkList renders pending add passkey links, each with a copyable URL, its
// expiry, and a revoke button posting to revokeAction.
func (a *app) linkList(links []*Invite, revokeAction string) g.Node {
	if len(links) == 0 {
		return h.P(h.Class("muted"), g.Text("No pending links."))
	}
	return h.Ul(h.Class("list"), g.Map(links, func(inv *Invite) g.Node {
		id := fmt.Sprintf("link-%d", inv.ID)
		return h.Li(h.ID(id), h.Class("stack-tight"),
			a.copyField(id+"-url", inv),
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
	return []settingsSection{a.settingsPicture, a.settingsPasskeys, a.settingsDevices, a.settingsNotifications, a.settingsStats}
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

// profile picture

const (
	// avatarSize is the largest accepted edge of a profile picture, in
	// pixels. main.js crops to at most this.
	avatarSize = 512
	// maxAvatarForm bounds the form that carries the picture as a data URL.
	maxAvatarForm = 1 << 20
	avatarPrefix  = "data:image/jpeg;base64,"

	msgBadPicture = "That picture could not be used."
)

// settingsPicture shows the user's profile picture with the controls to
// change it. The file never leaves the browser: main.js opens it in the crop
// dialog and posts only the chosen square, as a small JPEG.
func (a *app) settingsPicture(r *http.Request, u *User) (g.Node, error) {
	return h.Section(h.ID("picture"), h.Class("stack"),
		h.H2(g.Text("Profile picture")),
		picturePicker(avatar(u.ID, u.Username, u.Avatar, "avatar-l"), u.Avatar != "",
			pSettingsAvatar, pSettingsAvatarDel),
	), nil
}

// decodeAvatar checks a picture cropped by the browser and encodes it again,
// so that what is stored and served is always a JPEG written by this server.
// It also returns the name the picture is served under.
func decodeAvatar(dataURL string) (img []byte, name string, err error) {
	img, err = decodePicture(dataURL)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(img)
	return img, hex.EncodeToString(sum[:8]), nil
}

func decodePicture(dataURL string) ([]byte, error) {
	enc, ok := strings.CutPrefix(dataURL, avatarPrefix)
	if !ok {
		return nil, errors.New("not a JPEG data URL")
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}
	// The size is checked before decoding, so a small file that claims a
	// huge canvas costs nothing.
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if cfg.Width != cfg.Height || cfg.Width < 1 || cfg.Width > avatarSize {
		return nil, fmt.Errorf("%dx%d is not a square of at most %d pixels", cfg.Width, cfg.Height, avatarSize)
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (a *app) settingsAvatar(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarForm)
	img, name, err := decodeAvatar(r.PostFormValue("image"))
	if err != nil {
		return a.flash(w, r, pSettings, Flash{Kind: flashError, Content: msgBadPicture})
	}
	err = a.db.Tx(ctx, func(tx *AppDB) error {
		_, err := tx.Avatars.Upsert(ctx, &Avatar{UserID: user.ID, Image: img},
			ant.OnConflict(&Avatars.UserID))
		if err != nil {
			return serr.Wrap(err)
		}
		_, err = tx.Users.Where(ant.Eq(&Users.ID, user.ID)).Update(ctx, ant.Set(&Users.Avatar, name))
		return serr.Wrap(err)
	})
	if err != nil {
		return err
	}
	return a.flash(w, r, pSettings, Flash{Kind: flashSuccess, Content: "Profile picture saved."})
}

func (a *app) settingsAvatarRemove(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	err := a.db.Tx(ctx, func(tx *AppDB) error {
		if _, err := tx.Avatars.Where(ant.Eq(&Avatars.UserID, user.ID)).Delete(ctx); err != nil {
			return serr.Wrap(err)
		}
		_, err := tx.Users.Where(ant.Eq(&Users.ID, user.ID)).Update(ctx, ant.Set(&Users.Avatar, ""))
		return serr.Wrap(err)
	})
	if err != nil {
		return err
	}
	return a.flash(w, r, pSettings, Flash{Kind: flashSuccess, Content: "Profile picture removed."})
}

// avatarImage serves a profile picture to any signed in user. The last path
// element is users.avatar, which changes with the picture: an old name is
// not found, and a current one never changes, so it is cached for good.
func (a *app) avatarImage(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return errNotFound
	}
	img, err := a.db.Avatars.Where(ant.Eq(&Avatars.UserID, id),
		ant.Exists(a.db.Users.Where(ant.Eq(&Users.ID, id), ant.Eq(&Users.Avatar, r.PathValue("name"))))).
		Single(r.Context())
	if errors.Is(err, ant.ErrNotFound) {
		return errNotFound
	}
	if err != nil {
		return serr.Wrap(err)
	}
	servePicture(w, img.Image)
	return nil
}

// servePicture writes a stored picture. Its URL changes when the picture
// does, so it is cached for good.
func servePicture(w http.ResponseWriter, img []byte) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	_, _ = w.Write(img)
}

func (a *app) settingsPasskeys(r *http.Request, u *User) (g.Node, error) {
	creds, err := a.credentials(r.Context(), u.ID)
	if err != nil {
		return nil, err
	}
	return h.Section(h.ID("passkeys"), h.Class("stack"),
		h.H2(g.Text("Passkeys")),
		passkeyList(creds, credentialFrom(r.Context()), pPasskeyDelete, len(creds) > 1),
		a.passkeyForm(pPasskeyCreate, "pk-create", pPasskeyOptions, "Add a passkey on this device"),
	), nil
}

func (a *app) settingsDevices(r *http.Request, u *User) (g.Node, error) {
	links, err := a.pendingLinks(r.Context(), a.db, u.ID)
	if err != nil {
		return nil, err
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
	row, err := a.newCredential(r, user.ID, cred)
	if err != nil {
		return err
	}
	if err := a.db.Credentials.Insert(ctx, row); ant.IsUniqueViolation(err) {
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
	err = a.db.Tx(ctx, func(tx *AppDB) error {
		own := tx.Credentials.Where(ant.Eq(&Credentials.UserID, user.ID))
		n, err := own.Count(ctx)
		if err != nil {
			return serr.Wrap(err)
		}
		if n <= 1 {
			last = true
			return nil
		}
		deleted, err = own.Where(ant.Eq(&Credentials.ID, id)).Delete(ctx)
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
	err := a.db.Tx(ctx, func(tx *AppDB) error {
		links, err := a.pendingLinks(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		if len(links) >= maxPendingLinks {
			full = true
			return nil
		}
		_, err = a.createInvite(ctx, tx, inviteParams{
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
	found, err := a.revokeLink(ctx, id, user.ID)
	if err != nil {
		return err
	}
	if !found {
		return httpErr(http.StatusNotFound, "Not found.")
	}
	return a.flash(w, r, pSettings+"#devices", Flash{Kind: flashSuccess, Content: "Link revoked."})
}

// sending invites

const inviteTTL = 7 * 24 * time.Hour

// expiresIn describes how long until unix, in whole days rounded up.
func (a *app) expiresIn(unix int64) string {
	days := (unix - a.now().Unix() + 86399) / 86400
	if days <= 1 {
		return "expires in 1 day"
	}
	return fmt.Sprintf("expires in %d days", days)
}

func (a *app) invites(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	w.Header().Set("Cache-Control", "no-store") // holds invite links
	// Admins see every pending invite with its creator, others their own.
	admin := allowed(user, accAdmin)
	invs, err := a.db.Invites.Where(ant.IsNull(&Invites.UserID), a.inviteLive()).
		When(admin, func(q *ant.Query[Invite]) *ant.Query[Invite] {
			return q.Where(ant.IsNotNull(&Invites.CreatedBy)).IncludeOne(&Invites.Creator)
		}).
		When(!admin, func(q *ant.Query[Invite]) *ant.Query[Invite] {
			return q.Where(ant.Eq(&Invites.CreatedBy, nullID(user.ID)))
		}).
		OrderByDesc(&Invites.ID).All(ctx)
	if err != nil {
		return serr.Wrap(err)
	}

	var list g.Node = h.P(h.Class("muted"), g.Text("No pending invites."))
	if len(invs) > 0 {
		list = h.Ul(h.Class("list"), g.Map(invs, func(inv *Invite) g.Node {
			id := fmt.Sprintf("invite-%d", inv.ID)
			var by g.Node
			if inv.Creator != nil {
				by = h.Span(h.Class("muted"), g.Text("by "+inv.Creator.Username))
			}
			return h.Li(h.ID(id), h.Class("stack-tight"),
				h.Span(h.Class("cluster"), h.Strong(g.Text(inv.Username)), by),
				a.copyField(id+"-url", inv),
				h.Div(h.Class("row"),
					h.Span(h.Class("muted"), g.Text(a.expiresIn(inv.ExpiresAt))),
					h.Form(h.Method("post"), h.Action(pInvitesRevoke),
						h.Input(h.Type("hidden"), h.Name("id"),
							h.Value(strconv.FormatInt(inv.ID, 10))),
						h.Button(h.Class("btn secondary"), g.Text("Revoke")))))
		}))
	}

	return a.pageSimple(w, r, "Invites", backLink(pHome, "Home"),
		h.Form(h.Method("post"), h.Action(pInvites), h.Class("stack card"),
			h.Label(g.Text("Username"),
				h.Input(h.Type("text"), h.Name("username"), h.ID("username"),
					h.Value(r.URL.Query().Get("username")),
					h.AutoComplete("off"), g.Attr("autocapitalize", "none"),
					g.Attr("spellcheck", "false"), h.Required(),
					h.Pattern(usernamePattern), h.Aria("describedby", "username-hint"))),
			h.P(h.ID("username-hint"), h.Class("muted"), g.Text(msgUsernameRule)),
			h.Button(h.Class("btn"), icon("plus"), h.Span(g.Text("Create invite")))),
		h.Section(h.ID("invites"), h.Class("stack"),
			h.H2(g.Text("Pending invites")),
			list),
	).Render(w)
}

func (a *app) invitesPost(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	typed := r.FormValue("username")
	fail := func(msg string) error {
		return a.flash(w, r, pInvites+"?"+url.Values{"username": {typed}}.Encode(),
			Flash{Kind: flashError, Content: msg})
	}
	const (
		msgTaken   = "That username is taken."
		msgPending = "There is already a pending invite for that username."
	)
	username, err := validUsername(typed)
	if err != nil {
		return fail(err.Error())
	}
	var refused string
	err = a.db.Tx(ctx, func(tx *AppDB) error {
		forName := tx.Invites.Where(ant.Eq(&Invites.Username, username), ant.IsNull(&Invites.UserID))
		_, err := forName.Where(ant.Not(a.inviteLive())).Delete(ctx)
		if err != nil {
			return serr.Wrap(err)
		}
		taken, err := tx.Users.Where(ant.Eq(&Users.Username, username)).Exists(ctx)
		if err != nil {
			return serr.Wrap(err)
		}
		if taken {
			refused = msgTaken
			return nil
		}
		pending, err := forName.Exists(ctx)
		if err != nil {
			return serr.Wrap(err)
		}
		if pending {
			refused = msgPending
			return nil
		}
		_, err = a.createInvite(ctx, tx, inviteParams{
			Username:  username,
			CreatedBy: user.ID,
			TTL:       inviteTTL,
		})
		if err != nil {
			return err
		}
		return a.statUser(ctx, tx, UserStat{UserID: user.ID, InvitesSent: 1})
	})
	if ant.IsUniqueViolation(err) {
		// invites_new_username caught a racing invite for the same username.
		return fail(msgPending)
	}
	if err != nil {
		return err
	}
	if refused != "" {
		return fail(refused)
	}
	return a.flash(w, r, pInvites, Flash{Kind: flashSuccess,
		Content: "Invite created for " + username + ". Send them the link."})
}

func (a *app) invitesRevoke(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	id, err := formID(r, "id")
	if err != nil {
		return err
	}
	// Admins may revoke any user's invite, never the bootstrap one.
	n, err := a.db.Invites.Where(ant.Eq(&Invites.ID, id), ant.IsNull(&Invites.UserID),
		ant.IsNotNull(&Invites.CreatedBy)).
		When(!allowed(user, accAdmin), func(q *ant.Query[Invite]) *ant.Query[Invite] {
			return q.Where(ant.Eq(&Invites.CreatedBy, nullID(user.ID)))
		}).
		Delete(ctx)
	if err != nil {
		return serr.Wrap(err)
	}
	if n == 0 {
		return httpErr(http.StatusNotFound, "Not found.")
	}
	return a.flash(w, r, pInvites, Flash{Kind: flashSuccess, Content: "Invite revoked."})
}

// admin users

func adminUserPath(id int64) string {
	return pAdminUser + strconv.FormatInt(id, 10)
}

// permBadges renders the permissions a user holds as badges.
func permBadges(u User) g.Node {
	names := permNames(u)
	if len(names) == 0 {
		return h.Span(h.Class("muted"), g.Text("none"))
	}
	return h.Span(h.Class("cluster"), g.Map(names, func(n string) g.Node {
		return h.Span(h.Class("badge"), g.Text(n))
	}))
}

func (a *app) admin(w http.ResponseWriter, r *http.Request) error {
	users, err := a.db.Users.
		IncludeOne(&Users.Inviter).
		IncludeOne(&Users.Stats).
		With(&Users.Passkeys, ant.CountRelated(&Users.Credentials)).
		OrderBy(&Users.Username).
		All(r.Context())
	if err != nil {
		return serr.Wrap(err)
	}
	cell := func(label string, children ...g.Node) g.Node {
		return h.Td(h.Data("label", label), g.Group(children))
	}
	return a.page(w, r, "Admin",
		h.Div(h.Class("stack"),
			pageHeader("Admin", backLink(pHome, "Home")),
			h.P(h.A(h.ID("admin-groups"), h.Href(pAdminGroups), g.Text("All groups"))),
			h.Table(h.ID("users"), h.Class("table"),
				h.THead(h.Tr(
					h.Th(g.Text("Username")), h.Th(g.Text("Permissions")),
					h.Th(g.Text("Created")), h.Th(g.Text("Invited by")),
					h.Th(g.Text("Passkeys")), h.Th(g.Text("Messages sent")),
					h.Th(g.Text("Uploaded")), h.Th(g.Text("Downloaded")))),
				h.TBody(g.Map(users, func(u *User) g.Node {
					inviter := "-"
					if u.Inviter != nil {
						inviter = u.Inviter.Username
					}
					st := cmp.Or(u.Stats, &UserStat{})
					return h.Tr(h.ID(fmt.Sprintf("user-%d", u.ID)),
						cell("Username", h.A(h.Href(adminUserPath(u.ID)), g.Text(u.Username))),
						cell("Permissions", permBadges(*u)),
						cell("Created", g.Text(fmtTime(u.CreatedAt.Time, nil))),
						cell("Invited by", g.Text(inviter)),
						cell("Passkeys", g.Text(strconv.FormatInt(u.Passkeys, 10))),
						cell("Messages sent", g.Text(strconv.FormatInt(st.MessagesSent, 10))),
						cell("Uploaded", g.Text(fmtBytes(st.BytesUploaded))),
						cell("Downloaded", g.Text(fmtBytes(st.BytesDownloaded))))
				}))))).Render(w)
}

// adminTarget loads the user named by the {id} path value.
func (a *app) adminTarget(r *http.Request) (*User, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, httpErr(http.StatusNotFound, "Not found.")
	}
	u, err := a.db.Users.Find(r.Context(), id)
	if errors.Is(err, ant.ErrNotFound) {
		return u, httpErr(http.StatusNotFound, "Not found.")
	}
	return u, serr.Wrap(err)
}

func (a *app) adminUser(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	target, err := a.adminTarget(r)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store") // holds recovery links
	self := target.ID == userFrom(ctx).ID
	creds, err := a.credentials(ctx, target.ID)
	if err != nil {
		return err
	}
	links, err := a.pendingLinks(ctx, a.db, target.ID)
	if err != nil {
		return err
	}
	stats, err := a.userStatsSection(ctx, target.ID)
	if err != nil {
		return err
	}
	base := adminUserPath(target.ID)
	check := func(name, label string, on bool) g.Node {
		return h.Label(h.Class("check"),
			h.Input(h.Type("checkbox"), h.Name(name), h.Value("1"), g.If(on, h.Checked())),
			g.Text(label))
	}
	var adminControl g.Node
	if self {
		// The own admin flag cannot be removed, so there is no control for it.
		adminControl = g.Group{
			h.Input(h.Type("hidden"), h.Name("admin"), h.Value("1")),
			h.P(h.Class("muted"), g.Text("Admin. You cannot remove your own admin permission.")),
		}
	} else {
		adminControl = check("admin", "Admin", target.PermAdmin)
	}
	var passkeyNote g.Node
	if self {
		passkeyNote = h.P(h.Class("muted"), g.Text("Manage your own passkeys in "),
			h.A(h.Href(pSettings+"#passkeys"), g.Text("Settings")), g.Text("."))
	} else {
		passkeyNote = h.P(h.Class("muted"), g.Text("Deleting the last passkey locks "+
			target.Username+" out until they use a recovery link."))
	}
	return a.pageSimple(w, r, target.Username, backLink(pAdmin, "Admin"),
		h.Section(h.ID("perms"), h.Class("stack"),
			h.H2(g.Text("Permissions")),
			h.Form(h.Method("post"), h.Action(base+"/perms"), h.Class("stack card"),
				adminControl,
				check("invite", "Invite", target.PermInvite),
				check("group", "Create groups", target.PermGroup),
				h.Button(h.Class("btn"), g.Text("Save")))),
		h.Section(h.ID("passkeys"), h.Class("stack"),
			h.H2(g.Text("Passkeys")),
			passkeyNote,
			passkeyList(creds, 0, base+"/passkey/delete", !self)),
		h.Section(h.ID("recovery"), h.Class("stack"),
			h.H2(g.Text("Recovery link")),
			h.P(h.Class("muted"), g.Text("A link adds a passkey for "+target.Username+
				". It works once, for 24 hours.")),
			h.Form(h.Method("post"), h.Action(base+"/link"),
				h.Button(h.Class("btn secondary"), icon("plus"), h.Span(g.Text("Create recovery link")))),
			a.linkList(links, base+"/link/revoke")),
		stats,
	).Render(w)
}

func (a *app) adminUserPerms(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	target, err := a.adminTarget(r)
	if err != nil {
		return err
	}
	permAdmin := r.FormValue("admin") != ""
	back := adminUserPath(target.ID)
	if target.ID == userFrom(ctx).ID && !permAdmin {
		return a.flash(w, r, back, Flash{Kind: flashError,
			Content: "You cannot remove your own admin permission."})
	}
	_, err = a.db.Users.Where(ant.Eq(&Users.ID, target.ID)).Update(ctx,
		ant.Set(&Users.PermAdmin, permAdmin),
		ant.Set(&Users.PermInvite, r.FormValue("invite") != ""),
		ant.Set(&Users.PermGroup, r.FormValue("group") != ""))
	if err != nil {
		return serr.Wrap(err)
	}
	return a.flash(w, r, back, Flash{Kind: flashSuccess, Content: "Permissions saved."})
}

func (a *app) adminUserPasskeyDelete(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	target, err := a.adminTarget(r)
	if err != nil {
		return err
	}
	id, err := formID(r, "id")
	if err != nil {
		return err
	}
	back := adminUserPath(target.ID)
	admin := userFrom(ctx)
	if target.ID == admin.ID {
		return a.flash(w, r, back, Flash{Kind: flashError,
			Content: "Manage your own passkeys in Settings."})
	}
	n, err := a.db.Credentials.Where(ant.Eq(&Credentials.ID, id),
		ant.Eq(&Credentials.UserID, target.ID), ant.Ne(&Credentials.UserID, admin.ID)).
		Delete(ctx)
	if err != nil {
		return serr.Wrap(err)
	}
	if n == 0 {
		return httpErr(http.StatusNotFound, "Not found.")
	}
	return a.flash(w, r, back+"#passkeys", Flash{Kind: flashSuccess, Content: "Passkey deleted."})
}

func (a *app) adminUserLink(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	target, err := a.adminTarget(r)
	if err != nil {
		return err
	}
	_, err = a.createInvite(ctx, a.db, inviteParams{
		User:      target,
		CreatedBy: userFrom(ctx).ID,
		TTL:       addPasskeyTTL,
	})
	if err != nil {
		return err
	}
	return a.flash(w, r, adminUserPath(target.ID)+"#recovery", Flash{Kind: flashSuccess,
		Content: "Recovery link created. Send it to " + target.Username + "."})
}

func (a *app) adminUserLinkRevoke(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	target, err := a.adminTarget(r)
	if err != nil {
		return err
	}
	id, err := formID(r, "id")
	if err != nil {
		return err
	}
	found, err := a.revokeLink(ctx, id, target.ID)
	if err != nil {
		return err
	}
	if !found {
		return httpErr(http.StatusNotFound, "Not found.")
	}
	return a.flash(w, r, adminUserPath(target.ID)+"#recovery", Flash{Kind: flashSuccess,
		Content: "Link revoked."})
}
