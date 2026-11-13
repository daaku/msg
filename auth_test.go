package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/daaku/ensure"
	"github.com/descope/virtualwebauthn"
)

func countRows(t *testing.T, ta *testApp, table string) int {
	t.Helper()
	var n int
	ensure.Nil(t, ta.app.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM "+table).Scan(&n))
	return n
}

func bootstrapInvites(t *testing.T, ta *testApp) []Invite {
	t.Helper()
	rows, err := ta.app.db.QueryContext(t.Context(),
		"SELECT token, username, perm_admin, expires_at FROM invites WHERE created_by IS NULL")
	ensure.Nil(t, err)
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var inv Invite
		ensure.Nil(t, rows.Scan(&inv.Token, &inv.Username, &inv.PermAdmin, &inv.ExpiresAt))
		out = append(out, inv)
	}
	return out
}

func TestValidUsername(t *testing.T) {
	for in, want := range map[string]string{
		"alice":                 "alice",
		"  Alice ":              "alice",
		"a1":                    "a1",
		"a.b-c_d":               "a.b-c_d",
		"9lives":                "9lives",
		strings.Repeat("a", 32): strings.Repeat("a", 32),
	} {
		got, err := validUsername(in)
		ensure.Nil(t, err, in)
		ensure.DeepEqual(t, got, want)
	}
	for _, in := range []string{
		"", "a", ".ab", "-ab", "_ab", "a b", "a@b", "é1", "a/b",
		strings.Repeat("a", 33), "<script>",
	} {
		_, err := validUsername(in)
		ensure.True(t, err != nil, in)
	}
}

func TestBootstrap(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()

	ensure.Nil(t, ta.app.bootstrap(ctx))
	first := bootstrapInvites(t, ta)
	ensure.DeepEqual(t, len(first), 1)
	ensure.DeepEqual(t, first[0].Username, "admin")
	ensure.True(t, first[0].PermAdmin)
	ensure.DeepEqual(t, first[0].ExpiresAt, testNow.Add(24*time.Hour).Unix())
	ensure.True(t, len(first[0].Token) >= 43, "256 bit token")

	// a second start replaces it
	ensure.Nil(t, ta.app.bootstrap(ctx))
	second := bootstrapInvites(t, ta)
	ensure.DeepEqual(t, len(second), 1)
	ensure.True(t, second[0].Token != first[0].Token)
	ensure.DeepEqual(t, ta.get(invitePath(first[0].Token)).Code, http.StatusNotFound)

	// the link creates the admin
	inv, err := ta.app.q.InviteByToken(ctx, InviteByTokenParams{
		Token: second[0].Token, Now: testNow.Unix()})
	ensure.Nil(t, err)
	w, _, _ := ta.acceptInvite(inv)
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	admin, err := ta.app.q.UserByUsername(ctx, "admin")
	ensure.Nil(t, err)
	ensure.True(t, admin.PermAdmin)
	ensure.False(t, admin.InvitedBy.Valid)

	// with a user present nothing is created
	ensure.Nil(t, ta.app.bootstrap(ctx))
	ensure.DeepEqual(t, len(bootstrapInvites(t, ta)), 0)
}

func TestInvitePage(t *testing.T) {
	ta := newTestApp(t)
	inv := ta.newInvite("bob", false)
	w := ta.get(invitePath(inv.Token))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.StringContains(t, w.Body.String(), "Welcome, bob.")
	ensure.StringContains(t, w.Body.String(),
		`data-pk-create="`+invitePath(inv.Token)+`/options"`)
	ensure.DeepEqual(t, w.Header().Get("Referrer-Policy"), "no-referrer")
	ensure.DeepEqual(t, w.Header().Get("Cache-Control"), "no-store")

	wo := ta.get(invitePath(inv.Token) + "/options")
	ensure.DeepEqual(t, wo.Header().Get("Cache-Control"), "no-store")
	ensure.DeepEqual(t, wo.Header().Get("Referrer-Policy"), "no-referrer")
}

func TestInviteAccept(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	inviter := ta.newUser("inviter")
	inv, err := ta.app.createInvite(ctx, ta.app.q, inviteParams{
		Username: "bob", CreatedBy: inviter.ID, TTL: time.Hour})
	ensure.Nil(t, err)

	w, _, cred := ta.acceptInvite(inv)
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther, w.Body.String())
	ensure.DeepEqual(t, w.Header().Get("Location"), pHome)

	bob, err := ta.app.q.UserByUsername(ctx, "bob")
	ensure.Nil(t, err)
	ensure.DeepEqual(t, bob.Handle, inv.Handle)
	ensure.False(t, bob.PermAdmin)
	ensure.False(t, bob.PermInvite)
	ensure.False(t, bob.PermGroup)
	ensure.DeepEqual(t, bob.InvitedBy.Int64, inviter.ID)
	ensure.DeepEqual(t, bob.CreatedAt, testNow.Unix())

	creds, err := ta.app.q.CredentialsByUser(ctx, bob.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, len(creds), 1)
	ensure.DeepEqual(t, creds[0].CredentialID, cred.ID)

	_, err = ta.app.q.InviteByToken(ctx, InviteByTokenParams{Token: inv.Token})
	ensure.True(t, err != nil, "invite must be deleted")

	// the returned cookie authenticates
	home := ta.get(pHome, withCookies(w))
	ensure.DeepEqual(t, home.Code, http.StatusOK)
	ensure.StringContains(t, home.Body.String(), `id="sidebar"`)
}

func TestInviteSingleUse(t *testing.T) {
	ta := newTestApp(t)
	inv := ta.newInvite("bob", false)
	w, _, _ := ta.acceptInvite(inv)
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)

	w, _, _ = ta.acceptInvite(inv)
	ensure.DeepEqual(t, w.Code, http.StatusNotFound)
	ensure.DeepEqual(t, countRows(t, ta, "users"), 1)
	ensure.DeepEqual(t, countRows(t, ta, "credentials"), 1)
}

// The delete inside the transaction decides the winner when two browsers
// both hold a valid ceremony for the same link.
func TestInviteRace(t *testing.T) {
	ta := newTestApp(t)
	inv := ta.newInvite("bob", false)
	path := invitePath(inv.Token)

	type attempt struct {
		wo   *httptest.ResponseRecorder
		resp string
	}
	var attempts []attempt
	for range 2 {
		auth := virtualwebauthn.NewAuthenticatorWithOptions(
			virtualwebauthn.AuthenticatorOptions{UserHandle: inv.Handle})
		cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
		wo := ta.get(path + "/options")
		ao, err := virtualwebauthn.ParseAttestationOptions(wo.Body.String())
		ensure.Nil(t, err)
		attempts = append(attempts, attempt{wo,
			virtualwebauthn.CreateAttestationResponse(testRP, auth, cred, *ao)})
	}
	codes := []int{}
	for _, at := range attempts {
		w := ta.post(path, url.Values{"json": {at.resp}}, withCookies(at.wo))
		codes = append(codes, w.Code)
	}
	ensure.DeepEqual(t, codes, []int{http.StatusSeeOther, http.StatusNotFound})
	ensure.DeepEqual(t, countRows(t, ta, "users"), 1)
	ensure.DeepEqual(t, countRows(t, ta, "credentials"), 1)
}

func TestInviteExpired(t *testing.T) {
	ta := newTestApp(t)
	inv := ta.newInvite("bob", false)
	path := invitePath(inv.Token)

	// a ceremony started in time does not outlive the invite
	wo := ta.get(path + "/options")
	ensure.DeepEqual(t, wo.Code, http.StatusOK)

	ta.app.now = func() time.Time { return testNow.Add(time.Hour) }
	ensure.DeepEqual(t, ta.get(path).Code, http.StatusNotFound)
	ensure.StringContains(t, ta.get(path).Body.String(), msgInviteInvalid)
	ensure.DeepEqual(t, ta.get(path+"/options").Code, http.StatusNotFound)
	w := ta.post(path, url.Values{"json": {"{}"}}, withCookies(wo))
	ensure.DeepEqual(t, w.Code, http.StatusNotFound)
	ensure.DeepEqual(t, countRows(t, ta, "users"), 0)

	ensure.DeepEqual(t, ta.get(pInvite+"nope").Code, http.StatusNotFound)
}

func TestInviteBadCeremony(t *testing.T) {
	ta := newTestApp(t)
	inv := ta.newInvite("bob", false)
	path := invitePath(inv.Token)

	// no ceremony cookie
	w := ta.post(path, url.Values{"json": {"{}"}})
	ensure.DeepEqual(t, w.Code, http.StatusBadRequest)

	// garbage response
	wo := ta.get(path + "/options")
	w = ta.post(path, url.Values{"json": {"not json"}}, withCookies(wo))
	ensure.DeepEqual(t, w.Code, http.StatusBadRequest)

	// a response for a different invite's challenge and handle
	other := ta.newInvite("carol", false)
	auth := virtualwebauthn.NewAuthenticatorWithOptions(
		virtualwebauthn.AuthenticatorOptions{UserHandle: other.Handle})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	woOther := ta.get(invitePath(other.Token) + "/options")
	ao, err := virtualwebauthn.ParseAttestationOptions(woOther.Body.String())
	ensure.Nil(t, err)
	resp := virtualwebauthn.CreateAttestationResponse(testRP, auth, cred, *ao)
	w = ta.post(path, url.Values{"json": {resp}}, withCookies(woOther))
	ensure.DeepEqual(t, w.Code, http.StatusBadRequest)

	// wrong origin
	wo = ta.get(path + "/options")
	ao, err = virtualwebauthn.ParseAttestationOptions(wo.Body.String())
	ensure.Nil(t, err)
	evil := virtualwebauthn.RelyingParty{ID: "localhost", Name: "msg", Origin: "https://evil.example"}
	resp = virtualwebauthn.CreateAttestationResponse(evil, auth, cred, *ao)
	w = ta.post(path, url.Values{"json": {resp}}, withCookies(wo))
	ensure.DeepEqual(t, w.Code, http.StatusBadRequest)

	ensure.DeepEqual(t, countRows(t, ta, "users"), 0)
	ensure.DeepEqual(t, countRows(t, ta, "invites"), 2)
}

func TestAddPasskeyLink(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	bob := ta.newUser("bob")
	inv, err := ta.app.createInvite(ctx, ta.app.q, inviteParams{
		User: &bob.User, CreatedBy: bob.ID, TTL: time.Hour})
	ensure.Nil(t, err)
	ensure.DeepEqual(t, inv.Handle, bob.Handle)

	page := ta.get(invitePath(inv.Token))
	ensure.StringContains(t, page.Body.String(), "Add a passkey for bob")

	// the existing passkey is excluded
	wo := ta.get(invitePath(inv.Token) + "/options")
	ao, err := virtualwebauthn.ParseAttestationOptions(wo.Body.String())
	ensure.Nil(t, err)
	ensure.DeepEqual(t, len(ao.ExcludeCredentials), 1)

	w, auth, cred := ta.acceptInvite(inv)
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther, w.Body.String())
	ensure.DeepEqual(t, countRows(t, ta, "users"), 1)
	creds, err := ta.app.q.CredentialsByUser(ctx, bob.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, len(creds), 2)

	// the new session is bound to the new passkey
	ensure.DeepEqual(t, ta.get(pHome, withCookies(w)).Code, http.StatusOK)
	_, err = ta.app.db.ExecContext(ctx, "DELETE FROM credentials WHERE id = ?", creds[1].ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, ta.get(pHome, withCookies(w)).Code, http.StatusSeeOther)
	ensure.DeepEqual(t, ta.get(pHome, ta.as(bob)).Code, http.StatusOK)
	_, _ = auth, cred
}

func TestCredentialCannotBelongToTwoUsers(t *testing.T) {
	ta := newTestApp(t)
	bob := ta.newUser("bob")
	inv := ta.newInvite("carol", false)
	path := invitePath(inv.Token)
	auth := virtualwebauthn.NewAuthenticatorWithOptions(
		virtualwebauthn.AuthenticatorOptions{UserHandle: inv.Handle})
	w := ta.register(path+"/options", path, &auth, bob.cred)
	ensure.DeepEqual(t, w.Code, http.StatusBadRequest)
	ensure.DeepEqual(t, countRows(t, ta, "users"), 1)
	// the failed transaction rolled back, so the link still works
	ensure.DeepEqual(t, ta.get(path).Code, http.StatusOK)
}

func TestSignIn(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	bob := ta.newUser("bob")

	page := ta.get(pSignIn)
	ensure.StringContains(t, page.Body.String(), `data-pk-get="/sign-in/options"`)

	ta.app.now = func() time.Time { return testNow.Add(time.Minute) }
	w := ta.signIn(bob.auth, bob.cred)
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, w.Header().Get("Location"), pHome)
	ensure.DeepEqual(t, ta.get(pHome, withCookies(w)).Code, http.StatusOK)

	row, err := ta.app.q.CredentialByCredentialID(ctx, bob.cred.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, row.LastUsedAt, testNow.Add(time.Minute).Unix())

	// already signed in: the sign in page goes home
	ensure.DeepEqual(t, ta.get(pSignIn, ta.as(bob)).Code, http.StatusSeeOther)

	// sign out
	out := ta.post(pSignOut, nil, ta.as(bob))
	ensure.DeepEqual(t, out.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, out.Header().Get("Location"), pSignIn)
	ensure.True(t, findCookie(out, "s").MaxAge < 0)
}

func TestSignInFailures(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	bob := ta.newUser("bob")
	rejected := func(w *httptest.ResponseRecorder) {
		t.Helper()
		ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
		ensure.DeepEqual(t, w.Header().Get("Location"), pSignIn)
		ensure.True(t, findCookie(w, "s") == nil)
	}

	// no ceremony cookie
	rejected(ta.post(pSignIn, url.Values{"json": {"{}"}}))

	// garbage
	wo := ta.get(pSignInOptions)
	rejected(ta.post(pSignIn, url.Values{"json": {"nope"}}, withCookies(wo)))

	// a challenge cannot be replayed without its cookie from another ceremony
	wo1 := ta.get(pSignInOptions)
	ao, err := virtualwebauthn.ParseAssertionOptions(wo1.Body.String())
	ensure.Nil(t, err)
	resp := virtualwebauthn.CreateAssertionResponse(testRP, *bob.auth, bob.cred, *ao)
	wo2 := ta.get(pSignInOptions)
	rejected(ta.post(pSignIn, url.Values{"json": {resp}}, withCookies(wo2)))

	// wrong origin
	evil := virtualwebauthn.RelyingParty{ID: "localhost", Name: "msg", Origin: "https://evil.example"}
	wo = ta.get(pSignInOptions)
	ao, err = virtualwebauthn.ParseAssertionOptions(wo.Body.String())
	ensure.Nil(t, err)
	resp = virtualwebauthn.CreateAssertionResponse(evil, *bob.auth, bob.cred, *ao)
	rejected(ta.post(pSignIn, url.Values{"json": {resp}}, withCookies(wo)))

	// unknown user handle
	stranger := virtualwebauthn.NewAuthenticatorWithOptions(
		virtualwebauthn.AuthenticatorOptions{UserHandle: []byte("0123456789abcdef")})
	sc := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	stranger.AddCredential(sc)
	w := ta.signIn(&stranger, sc)
	rejected(w)
	flash := ta.get(pSignIn, withCookies(w))
	ensure.StringContains(t, flash.Body.String(), "This passkey is not registered here.")

	// a passkey deleted from the database
	_, err = ta.app.db.ExecContext(ctx, "DELETE FROM credentials WHERE id = ?", bob.credID)
	ensure.Nil(t, err)
	rejected(ta.signIn(bob.auth, bob.cred))
}

func TestSessionDiesWithCredential(t *testing.T) {
	ta := newTestApp(t)
	bob := ta.newUser("bob")
	ensure.DeepEqual(t, ta.get(pHome, ta.as(bob)).Code, http.StatusOK)
	_, err := ta.app.db.ExecContext(t.Context(),
		"DELETE FROM credentials WHERE id = ?", bob.credID)
	ensure.Nil(t, err)
	w := ta.get(pHome, ta.as(bob))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, w.Header().Get("Location"), pSignIn)
	ensure.True(t, findCookie(w, "s").MaxAge < 0, "cookie cleared")
}

func TestSessionCookie(t *testing.T) {
	ta := newTestApp(t)
	bob := ta.newUser("bob")
	ensure.True(t, bob.cookie.HttpOnly)
	ensure.DeepEqual(t, bob.cookie.SameSite, http.SameSiteLaxMode)
	ensure.DeepEqual(t, bob.cookie.MaxAge, int(sessionMaxAge.Seconds()))
	ensure.False(t, bob.cookie.Secure, "http public url")

	// fresh: not reissued
	w := ta.get(pHome, ta.as(bob))
	ensure.True(t, findCookie(w, "s") == nil)

	// older than a day: reissued, and the new one works
	ta.app.now = func() time.Time { return testNow.Add(25 * time.Hour) }
	w = ta.get(pHome, ta.as(bob))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	fresh := findCookie(w, "s")
	ensure.True(t, fresh != nil && fresh.Value != bob.cookie.Value)
	ensure.DeepEqual(t, ta.get(pHome, withCookies(w)).Code, http.StatusOK)

	// tampered: ignored and cleared
	bad := *bob.cookie
	bad.Value = bad.Value[:len(bad.Value)-4] + "AAAA"
	w = ta.get(pHome, func(r *http.Request) { r.AddCookie(&bad) })
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.True(t, findCookie(w, "s").MaxAge < 0)
}

func TestSessionCookieSecureOnHTTPS(t *testing.T) {
	ta := newTestApp(t)
	ta.app.cfg.PublicURL = "https://chat.example.com"
	ensure.True(t, ta.app.cookie("s", time.Hour).Secure)
}

func TestAccessMatrix(t *testing.T) {
	ta := newTestApp(t)
	users := map[string]*testUser{
		"plain":  ta.newUser("plain"),
		"invite": ta.newUser("inviter", "invite"),
		"group":  ta.newUser("grouper", "group"),
		"admin":  ta.newUser("boss", "admin"),
	}
	ok, hidden := http.StatusOK, http.StatusNotFound
	cases := []struct {
		acc  access
		want map[string]int
	}{
		{accPublic, map[string]int{"plain": ok, "invite": ok, "group": ok, "admin": ok}},
		{accUser, map[string]int{"plain": ok, "invite": ok, "group": ok, "admin": ok}},
		{accInvite, map[string]int{"plain": hidden, "invite": ok, "group": hidden, "admin": ok}},
		{accGroup, map[string]int{"plain": hidden, "invite": hidden, "group": ok, "admin": ok}},
		{accAdmin, map[string]int{"plain": hidden, "invite": hidden, "group": hidden, "admin": ok}},
	}
	for _, c := range cases {
		h := ta.app.wrap(c.acc, func(w http.ResponseWriter, r *http.Request) error {
			w.WriteHeader(http.StatusOK)
			return nil
		})
		for name, want := range c.want {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				r := httptest.NewRequest(method, "/x", nil)
				r.AddCookie(users[name].cookie)
				w := httptest.NewRecorder()
				h(w, r)
				ensure.DeepEqual(t, w.Code, want, c.acc, name, method)
			}
		}
		// signed out
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodGet, "/x", nil))
		wantGet, wantPost := http.StatusSeeOther, http.StatusUnauthorized
		if c.acc == accPublic {
			wantGet, wantPost = ok, ok
		}
		ensure.DeepEqual(t, w.Code, wantGet, c.acc)
		w = httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "/x", nil))
		ensure.DeepEqual(t, w.Code, wantPost, c.acc)
	}
}

func TestPermissionChangeAppliesToNextRequest(t *testing.T) {
	ta := newTestApp(t)
	bob := ta.newUser("bob")
	h := ta.app.wrap(accAdmin, func(w http.ResponseWriter, r *http.Request) error {
		ensure.True(t, userFrom(r.Context()).PermAdmin)
		ensure.DeepEqual(t, credentialFrom(r.Context()), bob.credID)
		return nil
	})
	call := func() int {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.AddCookie(bob.cookie)
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	ensure.DeepEqual(t, call(), http.StatusNotFound)
	_, err := ta.app.db.ExecContext(t.Context(),
		"UPDATE users SET perm_admin = TRUE WHERE id = ?", bob.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, call(), http.StatusOK)
}

// addPasskey registers a second passkey for u through the settings page.
func (ta *testApp) addPasskey(u *testUser) (*httptest.ResponseRecorder, *virtualwebauthn.Authenticator, virtualwebauthn.Credential) {
	ta.t.Helper()
	auth := virtualwebauthn.NewAuthenticatorWithOptions(
		virtualwebauthn.AuthenticatorOptions{UserHandle: u.Handle})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	w := ta.register(pPasskeyOptions, pPasskeyCreate, &auth, cred, ta.as(u))
	auth.AddCredential(cred)
	return w, &auth, cred
}

func pendingLinks(t *testing.T, ta *testApp, userID int64) []Invite {
	t.Helper()
	links, err := ta.app.q.InvitesPendingForUser(t.Context(), InvitesPendingForUserParams{
		UserID: sql.NullInt64{Int64: userID, Valid: true},
		Now:    ta.app.now().Unix(),
	})
	ensure.Nil(t, err)
	return links
}

func credCount(t *testing.T, ta *testApp, userID int64) int64 {
	t.Helper()
	n, err := ta.app.q.CredentialCountByUser(t.Context(), userID)
	ensure.Nil(t, err)
	return n
}

// flashOf follows a redirect that set a flash and returns the page body.
func (ta *testApp) flashOf(w *httptest.ResponseRecorder, opts ...reqOpt) string {
	ta.t.Helper()
	ensure.DeepEqual(ta.t, w.Code, http.StatusSeeOther, w.Body.String())
	c := findCookie(w, "flash")
	ensure.True(ta.t, c != nil, "flash cookie")
	opts = append(opts, func(r *http.Request) { r.AddCookie(c) })
	loc, _, _ := strings.Cut(w.Header().Get("Location"), "#")
	return ta.get(loc, opts...).Body.String()
}

func TestSettingsPage(t *testing.T) {
	ta := newTestApp(t)
	bob := ta.newUser("bob", "invite", "group")
	carol := ta.newUser("carol")

	ensure.StringContains(t, ta.get(pHome, ta.as(bob)).Body.String(), `href="/settings"`)

	w := ta.get(pSettings, ta.as(bob))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.DeepEqual(t, w.Header().Get("Cache-Control"), "no-store")
	body := w.Body.String()
	ensure.StringContains(t, body, "<h1>Settings</h1>")
	ensure.StringContains(t, body, `href="/"`)
	ensure.StringContains(t, body, "<dd>bob</dd>")
	ensure.StringContains(t, body, "<dd>Invite, Create groups</dd>")
	for _, id := range []string{"passkeys", "devices"} {
		ensure.StringContains(t, body, `<section id="`+id+`"`)
	}
	ensure.StringContains(t, body, fmt.Sprintf(`id="passkey-%d"`, bob.credID))
	ensure.StringDoesNotContain(t, body, fmt.Sprintf(`id="passkey-%d"`, carol.credID))
	ensure.StringContains(t, body, "this device")
	ensure.StringContains(t, body, `data-pk-create="/passkey/options"`)
	ensure.StringContains(t, body, `action="/passkey/create"`)
	ensure.StringContains(t, body, `action="/settings/link"`)
	ensure.StringContains(t, body, `action="/sign-out"`)
	// the only passkey cannot be deleted
	ensure.StringDoesNotContain(t, body, `action="/passkey/delete"`)

	ensure.StringContains(t, ta.get(pSettings, ta.as(carol)).Body.String(), "<dd>None</dd>")
}

func TestSettingsAddPasskey(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	bob := ta.newUser("bob")

	// the existing passkey is excluded
	wo := ta.get(pPasskeyOptions, ta.as(bob))
	ensure.DeepEqual(t, wo.Code, http.StatusOK)
	ao, err := virtualwebauthn.ParseAttestationOptions(wo.Body.String())
	ensure.Nil(t, err)
	ensure.DeepEqual(t, len(ao.ExcludeCredentials), 1)

	w, auth, cred := ta.addPasskey(bob)
	ensure.StringContains(t, ta.flashOf(w, ta.as(bob)), "Passkey added.")
	ensure.DeepEqual(t, w.Header().Get("Location"), pSettings)
	ensure.DeepEqual(t, credCount(t, ta, bob.ID), int64(2))
	row, err := ta.app.q.CredentialByCredentialID(ctx, cred.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, row.UserID, bob.ID)

	// the new passkey signs in as bob
	si := ta.signIn(auth, cred)
	ensure.DeepEqual(t, si.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, si.Header().Get("Location"), pHome)
	page := ta.get(pSettings, withCookies(si)).Body.String()
	ensure.StringContains(t, page, "<dd>bob</dd>")
	ensure.StringContains(t, page, `action="/passkey/delete"`)

	// registering the same passkey again is refused
	w = ta.register(pPasskeyOptions, pPasskeyCreate, auth, cred, ta.as(bob))
	ensure.DeepEqual(t, w.Header().Get("Location"), pSettings)
	ensure.DeepEqual(t, credCount(t, ta, bob.ID), int64(2))

	// a response without a ceremony is refused with a flash
	w = ta.post(pPasskeyCreate, url.Values{"json": {"{}"}}, ta.as(bob))
	ensure.StringContains(t, ta.flashOf(w, ta.as(bob)), "The passkey request expired.")
	ensure.DeepEqual(t, credCount(t, ta, bob.ID), int64(2))
}

func TestSettingsPasskeyDelete(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	del := func(u *testUser, id int64) *httptest.ResponseRecorder {
		return ta.post(pPasskeyDelete, url.Values{"id": {strconv.FormatInt(id, 10)}}, ta.as(u))
	}

	_, _, cred2 := ta.addPasskey(bob)
	row2, err := ta.app.q.CredentialByCredentialID(ctx, cred2.ID)
	ensure.Nil(t, err)

	// another user's passkey is a no op
	w := del(bob, carol.credID)
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, credCount(t, ta, carol.ID), int64(1))
	ensure.DeepEqual(t, credCount(t, ta, bob.ID), int64(2))

	// own passkey
	w = del(bob, row2.ID)
	ensure.DeepEqual(t, w.Header().Get("Location"), pSettings)
	ensure.DeepEqual(t, credCount(t, ta, bob.ID), int64(1))
	ensure.DeepEqual(t, ta.get(pSettings, ta.as(bob)).Code, http.StatusOK)

	// the last passkey is refused
	w = del(bob, bob.credID)
	ensure.StringContains(t, ta.flashOf(w, ta.as(bob)), "You cannot delete your only passkey.")
	ensure.DeepEqual(t, credCount(t, ta, bob.ID), int64(1))

	// a bad id
	ensure.DeepEqual(t, ta.post(pPasskeyDelete, url.Values{"id": {"x"}}, ta.as(bob)).Code,
		http.StatusBadRequest)

	// deleting the session's passkey signs this browser out
	_, auth3, cred3 := ta.addPasskey(bob)
	w = del(bob, bob.credID)
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, w.Header().Get("Location"), pSignIn)
	ensure.True(t, findCookie(w, "s").MaxAge < 0)
	ensure.DeepEqual(t, credCount(t, ta, bob.ID), int64(1))
	gone := ta.get(pSettings, ta.as(bob))
	ensure.DeepEqual(t, gone.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, gone.Header().Get("Location"), pSignIn)
	// the remaining passkey still works
	ensure.DeepEqual(t, ta.signIn(auth3, cred3).Header().Get("Location"), pHome)
}

func TestSettingsLinks(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	create := func() *httptest.ResponseRecorder { return ta.post(pSettingsLink, nil, ta.as(bob)) }
	revoke := func(u *testUser, id int64) *httptest.ResponseRecorder {
		return ta.post(pSettingsLinkRevoke, url.Values{"id": {strconv.FormatInt(id, 10)}}, ta.as(u))
	}

	w := create()
	ensure.StringContains(t, ta.flashOf(w, ta.as(bob)), "Link created.")
	links := pendingLinks(t, ta, bob.ID)
	ensure.DeepEqual(t, len(links), 1)
	link := links[0]
	ensure.DeepEqual(t, link.Handle, bob.Handle)
	ensure.DeepEqual(t, link.Username, "bob")
	ensure.DeepEqual(t, link.CreatedBy.Int64, bob.ID)
	ensure.DeepEqual(t, link.ExpiresAt, testNow.Add(24*time.Hour).Unix())

	page := ta.get(pSettings, ta.as(bob)).Body.String()
	ensure.StringContains(t, page, fmt.Sprintf(`id="link-%d"`, link.ID))
	ensure.StringContains(t, page, `value="http://localhost:8080/invite/`+link.Token+`"`)
	ensure.StringContains(t, page, "data-copy")
	ensure.StringContains(t, page, `action="/settings/link/revoke"`)
	ensure.StringDoesNotContain(t, ta.get(pSettings, ta.as(carol)).Body.String(), link.Token)

	// the link adds a passkey to bob
	aw, auth, cred := ta.acceptInvite(link)
	ensure.DeepEqual(t, aw.Code, http.StatusSeeOther, aw.Body.String())
	ensure.DeepEqual(t, credCount(t, ta, bob.ID), int64(2))
	ensure.DeepEqual(t, countRows(t, ta, "users"), 2)
	ensure.DeepEqual(t, len(pendingLinks(t, ta, bob.ID)), 0)
	si := ta.signIn(auth, cred)
	ensure.StringContains(t, ta.get(pSettings, withCookies(si)).Body.String(), "<dd>bob</dd>")

	// revoke: not by another user, then by the owner
	create()
	link = pendingLinks(t, ta, bob.ID)[0]
	ensure.DeepEqual(t, revoke(carol, link.ID).Code, http.StatusNotFound)
	ensure.DeepEqual(t, len(pendingLinks(t, ta, bob.ID)), 1)
	ensure.StringContains(t, ta.flashOf(revoke(bob, link.ID), ta.as(bob)), "Link revoked.")
	ensure.DeepEqual(t, len(pendingLinks(t, ta, bob.ID)), 0)
	ensure.DeepEqual(t, ta.get(invitePath(link.Token)).Code, http.StatusNotFound)

	// at most five pending links
	for range maxPendingLinks {
		ensure.StringContains(t, ta.flashOf(create(), ta.as(bob)), "Link created.")
	}
	ensure.StringContains(t, ta.flashOf(create(), ta.as(bob)), "at most 5 pending links")
	ensure.DeepEqual(t, len(pendingLinks(t, ta, bob.ID)), maxPendingLinks)

	// expired links do not count
	ta.app.now = func() time.Time { return testNow.Add(25 * time.Hour) }
	ensure.StringContains(t, ta.flashOf(create(), ta.as(bob)), "Link created.")
	ensure.DeepEqual(t, len(pendingLinks(t, ta, bob.ID)), 1)

	// add passkey links are not invites sent
	stats, err := ta.app.userStats(ctx, bob.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, stats.InvitesSent, int64(0))
}

func TestSettingsSignedOut(t *testing.T) {
	ta := newTestApp(t)
	for _, p := range []string{pSettings, pPasskeyOptions} {
		w := ta.get(p)
		ensure.DeepEqual(t, w.Code, http.StatusSeeOther, p)
		ensure.DeepEqual(t, w.Header().Get("Location"), pSignIn, p)
	}
	for _, p := range []string{pPasskeyCreate, pPasskeyDelete, pSettingsLink, pSettingsLinkRevoke} {
		ensure.DeepEqual(t, ta.post(p, url.Values{"id": {"1"}}).Code, http.StatusUnauthorized, p)
	}
}

func TestUsernamePattern(t *testing.T) {
	// The HTML pattern agrees with the server rule. Browsers anchor it.
	re := regexp.MustCompile(`^(?:` + usernamePattern + `)$`)
	for _, s := range []string{"ab", "a.b-c_d", "9lives", strings.Repeat("a", 32),
		"a", ".ab", "-ab", "Ab", "a b", strings.Repeat("a", 33), "a/b"} {
		ensure.DeepEqual(t, re.MatchString(s), usernameRe.MatchString(s), s)
	}
}

func invitesSent(t *testing.T, ta *testApp, userID int64) int64 {
	t.Helper()
	s, err := ta.app.userStats(t.Context(), userID)
	ensure.Nil(t, err)
	return s.InvitesSent
}

func pendingInvite(t *testing.T, ta *testApp, username string) Invite {
	t.Helper()
	inv, err := ta.app.q.InvitePendingByUsername(t.Context(), InvitePendingByUsernameParams{
		Username: username, Now: ta.app.now().Unix()})
	ensure.Nil(t, err, username)
	return inv
}

func TestInvitesAccess(t *testing.T) {
	ta := newTestApp(t)
	plain := ta.newUser("plain", "group")
	inviter := ta.newUser("inviter", "invite")
	admin := ta.newUser("boss", "admin")

	ensure.DeepEqual(t, ta.get(pInvites, ta.as(plain)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.post(pInvites, url.Values{"username": {"zed"}}, ta.as(plain)).Code,
		http.StatusNotFound)
	inv := ta.newInvite("victim", false)
	ensure.DeepEqual(t, ta.post(pInvitesRevoke, url.Values{"id": {strconv.FormatInt(inv.ID, 10)}},
		ta.as(plain)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, countRows(t, ta, "invites"), 1)

	w := ta.get(pInvites)
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, w.Header().Get("Location"), pSignIn)
	ensure.DeepEqual(t, ta.post(pInvites, url.Values{"username": {"zed"}}).Code, http.StatusUnauthorized)
	ensure.DeepEqual(t, ta.post(pInvitesRevoke, url.Values{"id": {"1"}}).Code, http.StatusUnauthorized)

	// the home page links to invites only for those who may use it
	ensure.StringDoesNotContain(t, ta.get(pHome, ta.as(plain)).Body.String(), `href="/invites"`)
	for _, u := range []*testUser{inviter, admin} {
		ensure.StringContains(t, ta.get(pHome, ta.as(u)).Body.String(), `href="/invites"`)
		ensure.DeepEqual(t, ta.get(pInvites, ta.as(u)).Code, http.StatusOK)
	}
}

func TestInvitesCreate(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "invite")

	page := ta.get(pInvites, ta.as(alice)).Body.String()
	ensure.StringContains(t, page, "<h1>Invites</h1>")
	ensure.StringContains(t, page, `href="/"`)
	ensure.StringContains(t, page, `name="username"`)
	ensure.StringContains(t, page, `autocapitalize="none"`)
	ensure.StringContains(t, page, `autocomplete="off"`)
	ensure.StringContains(t, page, `spellcheck="false"`)
	ensure.StringContains(t, page, "required")
	ensure.StringContains(t, page, `pattern="`+usernamePattern+`"`)
	ensure.StringContains(t, page, "lowercase letters")

	w := ta.post(pInvites, url.Values{"username": {" Bob "}}, ta.as(alice))
	ensure.DeepEqual(t, w.Header().Get("Location"), pInvites)
	ensure.StringContains(t, ta.flashOf(w, ta.as(alice)), "Invite created for bob.")
	ensure.DeepEqual(t, countRows(t, ta, "invites"), 1)
	ensure.DeepEqual(t, invitesSent(t, ta, alice.ID), int64(1))
	inv := pendingInvite(t, ta, "bob")
	ensure.DeepEqual(t, inv.CreatedBy.Int64, alice.ID)
	ensure.False(t, inv.UserID.Valid)
	ensure.False(t, inv.PermAdmin)
	ensure.DeepEqual(t, inv.ExpiresAt, testNow.Add(7*24*time.Hour).Unix())

	page = ta.get(pInvites, ta.as(alice)).Body.String()
	ensure.StringContains(t, page, fmt.Sprintf(`id="invite-%d"`, inv.ID))
	ensure.StringContains(t, page, `value="`+ta.app.inviteURL(inv)+`"`)
	ensure.StringContains(t, page, "data-copy")
	ensure.StringContains(t, page, "expires in 7 days")
	ensure.StringContains(t, page, `action="/invites/revoke"`)

	// the new link is at the top
	ta.post(pInvites, url.Values{"username": {"carl"}}, ta.as(alice))
	page = ta.get(pInvites, ta.as(alice)).Body.String()
	ensure.True(t, strings.Index(page, ">carl<") < strings.Index(page, ">bob<"), "newest first")

	// the link works end to end
	aw, _, _ := ta.acceptInvite(inv)
	ensure.DeepEqual(t, aw.Code, http.StatusSeeOther, aw.Body.String())
	bob, err := ta.app.q.UserByUsername(ctx, "bob")
	ensure.Nil(t, err)
	ensure.False(t, bob.PermAdmin)
	ensure.False(t, bob.PermInvite)
	ensure.False(t, bob.PermGroup)
	ensure.DeepEqual(t, bob.InvitedBy.Int64, alice.ID)
	ensure.DeepEqual(t, ta.get(pInvites, withCookies(aw)).Code, http.StatusNotFound)
	ensure.StringDoesNotContain(t, ta.get(pInvites, ta.as(alice)).Body.String(), ">bob<")
}

func TestInvitesCreateRejected(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "invite")
	ta.newUser("dora")
	ta.post(pInvites, url.Values{"username": {"erin"}}, ta.as(alice))
	ensure.DeepEqual(t, invitesSent(t, ta, alice.ID), int64(1))

	cases := map[string]string{
		"bad name!": "Usernames are 2 to 32 characters",
		"x":         "Usernames are 2 to 32 characters",
		"dora":      "That username is taken.",
		"Dora":      "That username is taken.",
		"erin":      "There is already a pending invite for that username.",
		"ERIN":      "There is already a pending invite for that username.",
	}
	for typed, msg := range cases {
		w := ta.post(pInvites, url.Values{"username": {typed}}, ta.as(alice))
		ensure.DeepEqual(t, w.Header().Get("Location"),
			pInvites+"?"+url.Values{"username": {typed}}.Encode(), typed)
		page := ta.flashOf(w, ta.as(alice))
		ensure.StringContains(t, page, msg)
		// the typed value is refilled
		ensure.StringContains(t, page, `value="`+typed+`"`)
	}
	ensure.DeepEqual(t, invitesSent(t, ta, alice.ID), int64(1))
	ensure.DeepEqual(t, countRows(t, ta, "invites"), 1)
}

func TestInvitesUniqueIndexBackstop(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "invite")
	// Simulate a racing request that inserts the same username between the
	// pending check and the insert.
	_, err := ta.app.db.ExecContext(t.Context(), `
		CREATE TRIGGER race BEFORE INSERT ON invites
		WHEN NEW.username = 'bob' AND NEW.token != 'racer'
		BEGIN
			INSERT INTO invites (token, username, handle, created_at, expires_at)
			VALUES ('racer', 'bob', x'01', 0, 9999999999);
		END`)
	ensure.Nil(t, err)
	w := ta.post(pInvites, url.Values{"username": {"bob"}}, ta.as(alice))
	ensure.StringContains(t, ta.flashOf(w, ta.as(alice)),
		"There is already a pending invite for that username.")
	ensure.DeepEqual(t, invitesSent(t, ta, alice.ID), int64(0))
	ensure.DeepEqual(t, countRows(t, ta, "invites"), 0)
}

func TestInvitesExpiredDoesNotBlock(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "invite")
	ta.post(pInvites, url.Values{"username": {"bob"}}, ta.as(alice))
	old := pendingInvite(t, ta, "bob")

	ta.app.now = func() time.Time { return testNow.Add(8 * 24 * time.Hour) }
	// expired rows are not listed
	ensure.StringDoesNotContain(t, ta.get(pInvites, ta.as(alice)).Body.String(), old.Token)
	w := ta.post(pInvites, url.Values{"username": {"bob"}}, ta.as(alice))
	ensure.StringContains(t, ta.flashOf(w, ta.as(alice)), "Invite created for bob.")
	ensure.DeepEqual(t, countRows(t, ta, "invites"), 1)
	ensure.True(t, pendingInvite(t, ta, "bob").Token != old.Token)
	ensure.DeepEqual(t, invitesSent(t, ta, alice.ID), int64(2))
}

func TestInvitesRevoke(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "invite")
	mallory := ta.newUser("mallory", "invite")
	admin := ta.newUser("boss", "admin")
	revoke := func(u *testUser, inv Invite) *httptest.ResponseRecorder {
		return ta.post(pInvitesRevoke, url.Values{"id": {strconv.FormatInt(inv.ID, 10)}}, ta.as(u))
	}
	ta.post(pInvites, url.Values{"username": {"bob"}}, ta.as(alice))
	ta.post(pInvites, url.Values{"username": {"carl"}}, ta.as(alice))
	bob, carl := pendingInvite(t, ta, "bob"), pendingInvite(t, ta, "carl")

	// own invite
	w := revoke(alice, bob)
	ensure.StringContains(t, ta.flashOf(w, ta.as(alice)), "Invite revoked.")
	ensure.DeepEqual(t, ta.get(invitePath(bob.Token)).Code, http.StatusNotFound)
	ensure.StringContains(t, ta.get(invitePath(bob.Token)).Body.String(), msgInviteInvalid)

	// another user's: refused for a non admin, allowed for an admin
	ensure.DeepEqual(t, revoke(mallory, carl).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(invitePath(carl.Token)).Code, http.StatusOK)
	ensure.DeepEqual(t, revoke(admin, carl).Code, http.StatusSeeOther)
	ensure.DeepEqual(t, ta.get(invitePath(carl.Token)).Code, http.StatusNotFound)

	// never an add passkey link or the bootstrap invite
	link, err := ta.app.createInvite(t.Context(), ta.app.q, inviteParams{
		User: &alice.User, CreatedBy: alice.ID, TTL: time.Hour})
	ensure.Nil(t, err)
	ensure.DeepEqual(t, revoke(alice, link).Code, http.StatusNotFound)
	ensure.DeepEqual(t, revoke(admin, link).Code, http.StatusNotFound)
	boot := ta.newInvite("root", true)
	ensure.DeepEqual(t, revoke(admin, boot).Code, http.StatusNotFound)
	ensure.DeepEqual(t, countRows(t, ta, "invites"), 2)
}

func TestInvitesList(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "invite")
	mallory := ta.newUser("mallory", "invite")
	admin := ta.newUser("boss", "admin")
	ta.post(pInvites, url.Values{"username": {"bob"}}, ta.as(alice))
	ta.post(pInvites, url.Values{"username": {"carl"}}, ta.as(mallory))
	link, err := ta.app.createInvite(t.Context(), ta.app.q, inviteParams{
		User: &alice.User, CreatedBy: alice.ID, TTL: time.Hour})
	ensure.Nil(t, err)
	bob, carl := pendingInvite(t, ta, "bob"), pendingInvite(t, ta, "carl")

	page := ta.get(pInvites, ta.as(alice)).Body.String()
	ensure.StringContains(t, page, bob.Token)
	ensure.StringDoesNotContain(t, page, carl.Token)
	ensure.StringDoesNotContain(t, page, link.Token)
	ensure.StringDoesNotContain(t, page, "by alice")

	page = ta.get(pInvites, ta.as(admin)).Body.String()
	ensure.StringContains(t, page, bob.Token)
	ensure.StringContains(t, page, carl.Token)
	ensure.StringContains(t, page, "by alice")
	ensure.StringContains(t, page, "by mallory")
	ensure.StringDoesNotContain(t, page, link.Token)
}

func TestAdminAccess(t *testing.T) {
	ta := newTestApp(t)
	admin := ta.newUser("boss", "admin")
	plain := ta.newUser("plain", "invite", "group")
	base := adminUserPath(plain.ID)
	gets := []string{pAdmin, base}
	posts := []string{base + "/perms", base + "/passkey/delete", base + "/link", base + "/link/revoke"}
	form := url.Values{"id": {strconv.FormatInt(plain.credID, 10)}, "admin": {"1"}}

	for _, p := range gets {
		ensure.DeepEqual(t, ta.get(p, ta.as(plain)).Code, http.StatusNotFound, p)
		w := ta.get(p)
		ensure.DeepEqual(t, w.Code, http.StatusSeeOther, p)
		ensure.DeepEqual(t, w.Header().Get("Location"), pSignIn, p)
		ensure.DeepEqual(t, ta.get(p, ta.as(admin)).Code, http.StatusOK, p)
	}
	for _, p := range posts {
		ensure.DeepEqual(t, ta.post(p, form, ta.as(plain)).Code, http.StatusNotFound, p)
		ensure.DeepEqual(t, ta.post(p, form).Code, http.StatusUnauthorized, p)
	}
	// nothing changed
	ensure.DeepEqual(t, credCount(t, ta, plain.ID), int64(1))
	u, err := ta.app.q.UserByID(t.Context(), plain.ID)
	ensure.Nil(t, err)
	ensure.False(t, u.PermAdmin)
	ensure.DeepEqual(t, countRows(t, ta, "invites"), 0)

	ensure.StringContains(t, ta.get(pHome, ta.as(admin)).Body.String(), `href="/admin"`)
	ensure.StringDoesNotContain(t, ta.get(pHome, ta.as(plain)).Body.String(), `href="/admin"`)
}

func TestAdminUnknownUser(t *testing.T) {
	ta := newTestApp(t)
	admin := ta.newUser("boss", "admin")
	for _, id := range []string{"999", "x", "0", "-1"} {
		base := pAdminUser + id
		ensure.DeepEqual(t, ta.get(base, ta.as(admin)).Code, http.StatusNotFound, id)
		for _, p := range []string{"/perms", "/passkey/delete", "/link", "/link/revoke"} {
			ensure.DeepEqual(t, ta.post(base+p, url.Values{"id": {"1"}}, ta.as(admin)).Code,
				http.StatusNotFound, id+p)
		}
	}
	ensure.DeepEqual(t, countRows(t, ta, "invites"), 0)
}

func TestAdminList(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	admin := ta.newUser("boss", "admin")
	inv, err := ta.app.createInvite(ctx, ta.app.q, inviteParams{
		Username: "carol", CreatedBy: admin.ID, TTL: time.Hour})
	ensure.Nil(t, err)
	_, _, _ = ta.acceptInvite(inv)
	carol, err := ta.app.q.UserByUsername(ctx, "carol")
	ensure.Nil(t, err)
	alice := ta.newUser("alice", "invite")
	ta.addPasskey(alice)

	w := ta.get(pAdmin, ta.as(admin))
	body := w.Body.String()
	ensure.StringContains(t, body, "<h1>Admin</h1>")
	ensure.StringContains(t, body, `href="/"`)
	// ordered by username
	ia := strings.Index(body, `id="user-`+strconv.FormatInt(alice.ID, 10)+`"`)
	ib := strings.Index(body, `id="user-`+strconv.FormatInt(admin.ID, 10)+`"`)
	ic := strings.Index(body, `id="user-`+strconv.FormatInt(carol.ID, 10)+`"`)
	ensure.True(t, ia >= 0 && ia < ib && ib < ic, ia, ib, ic)
	ensure.StringContains(t, body, `href="`+adminUserPath(carol.ID)+`"`)
	row := body[ia:ib]
	ensure.StringContains(t, row, `<span class="badge">Invite</span>`)
	ensure.StringContains(t, row, `<td data-label="Passkeys">2</td>`)
	ensure.StringContains(t, row, `<td data-label="Invited by">-</td>`)
	ensure.StringContains(t, row, "2 Jan 2026 03:04")
	ensure.StringContains(t, body[ic:], `<td data-label="Invited by">boss</td>`)
	ensure.StringContains(t, body[ib:ic], `<span class="badge">Admin</span>`)
}

func TestAdminPerms(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	admin := ta.newUser("boss", "admin")
	bob := ta.newUser("bob")
	set := func(perms ...string) {
		t.Helper()
		form := url.Values{}
		for _, p := range perms {
			form.Set(p, "1")
		}
		w := ta.post(adminUserPath(bob.ID)+"/perms", form, ta.as(admin))
		ensure.StringContains(t, ta.flashOf(w, ta.as(admin)), "Permissions saved.")
	}
	get := func() User {
		t.Helper()
		u, err := ta.app.q.UserByID(ctx, bob.ID)
		ensure.Nil(t, err)
		return u
	}

	page := ta.get(adminUserPath(bob.ID), ta.as(admin)).Body.String()
	ensure.StringContains(t, page, "<h1>bob</h1>")
	ensure.StringContains(t, page, `href="/admin"`)
	for _, p := range []string{"admin", "invite", "group"} {
		ensure.StringContains(t, page, `type="checkbox" name="`+p+`"`)
	}

	set("invite")
	ensure.True(t, get().PermInvite)
	ensure.False(t, get().PermGroup)
	ensure.DeepEqual(t, ta.get(pInvites, ta.as(bob)).Code, http.StatusOK)
	ensure.DeepEqual(t, ta.get(pAdmin, ta.as(bob)).Code, http.StatusNotFound)
	ensure.StringContains(t, ta.get(adminUserPath(bob.ID), ta.as(admin)).Body.String(),
		`name="invite" value="1" checked`)

	set("group")
	ensure.False(t, get().PermInvite)
	ensure.True(t, get().PermGroup)
	ensure.DeepEqual(t, ta.get(pInvites, ta.as(bob)).Code, http.StatusNotFound)

	set("admin")
	ensure.True(t, get().PermAdmin)
	ensure.False(t, get().PermGroup)
	ensure.DeepEqual(t, ta.get(pAdmin, ta.as(bob)).Code, http.StatusOK)
	ensure.DeepEqual(t, ta.get(pInvites, ta.as(bob)).Code, http.StatusOK)

	set()
	u := get()
	ensure.False(t, u.PermAdmin || u.PermInvite || u.PermGroup)
	ensure.DeepEqual(t, ta.get(pAdmin, ta.as(bob)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(pInvites, ta.as(bob)).Code, http.StatusNotFound)
}

func TestAdminCannotRemoveOwnAdmin(t *testing.T) {
	ta := newTestApp(t)
	admin := ta.newUser("boss", "admin")
	page := ta.get(adminUserPath(admin.ID), ta.as(admin)).Body.String()
	ensure.StringDoesNotContain(t, page, `type="checkbox" name="admin"`)
	ensure.StringContains(t, page, "You cannot remove your own admin permission.")
	// no delete controls for the own passkeys
	ensure.StringDoesNotContain(t, page, "/passkey/delete")

	w := ta.post(adminUserPath(admin.ID)+"/perms", url.Values{"invite": {"1"}}, ta.as(admin))
	ensure.StringContains(t, ta.flashOf(w, ta.as(admin)), "You cannot remove your own admin permission.")
	u, err := ta.app.q.UserByID(t.Context(), admin.ID)
	ensure.Nil(t, err)
	ensure.True(t, u.PermAdmin)
	ensure.False(t, u.PermInvite)

	// the form as rendered still saves the other permissions
	w = ta.post(adminUserPath(admin.ID)+"/perms", url.Values{"admin": {"1"}, "invite": {"1"}}, ta.as(admin))
	ensure.StringContains(t, ta.flashOf(w, ta.as(admin)), "Permissions saved.")
	u, err = ta.app.q.UserByID(t.Context(), admin.ID)
	ensure.Nil(t, err)
	ensure.True(t, u.PermAdmin && u.PermInvite)

	// own passkeys cannot be deleted here, even with a second one
	ta.addPasskey(admin)
	w = ta.post(adminUserPath(admin.ID)+"/passkey/delete",
		url.Values{"id": {strconv.FormatInt(admin.credID, 10)}}, ta.as(admin))
	ensure.StringContains(t, ta.flashOf(w, ta.as(admin)), "Manage your own passkeys in Settings.")
	ensure.DeepEqual(t, credCount(t, ta, admin.ID), int64(2))
}

func TestAdminRecovery(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	admin := ta.newUser("boss", "admin")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	base := adminUserPath(bob.ID)

	page := ta.get(base, ta.as(admin)).Body.String()
	ensure.StringContains(t, page, fmt.Sprintf(`id="passkey-%d"`, bob.credID))
	ensure.StringContains(t, page, `action="`+base+`/passkey/delete"`)
	ensure.StringContains(t, page, "locks bob out until they use a recovery link")

	// a passkey of another user through bob's page is not found
	ensure.DeepEqual(t, ta.post(base+"/passkey/delete",
		url.Values{"id": {strconv.FormatInt(carol.credID, 10)}}, ta.as(admin)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, credCount(t, ta, carol.ID), int64(1))

	// deleting bob's only passkey ends his session
	ensure.DeepEqual(t, ta.get(pSettings, ta.as(bob)).Code, http.StatusOK)
	w := ta.post(base+"/passkey/delete",
		url.Values{"id": {strconv.FormatInt(bob.credID, 10)}}, ta.as(admin))
	ensure.StringContains(t, ta.flashOf(w, ta.as(admin)), "Passkey deleted.")
	ensure.DeepEqual(t, credCount(t, ta, bob.ID), int64(0))
	ensure.DeepEqual(t, ta.get(pSettings, ta.as(bob)).Code, http.StatusSeeOther)

	// a recovery link
	w = ta.post(base+"/link", nil, ta.as(admin))
	ensure.StringContains(t, ta.flashOf(w, ta.as(admin)), "Recovery link created.")
	links := pendingLinks(t, ta, bob.ID)
	ensure.DeepEqual(t, len(links), 1)
	link := links[0]
	ensure.DeepEqual(t, link.CreatedBy.Int64, admin.ID)
	ensure.DeepEqual(t, link.ExpiresAt, testNow.Add(24*time.Hour).Unix())
	page = ta.get(base, ta.as(admin)).Body.String()
	ensure.StringContains(t, page, `value="`+ta.app.inviteURL(link)+`"`)
	ensure.StringContains(t, page, "data-copy")
	ensure.StringContains(t, page, `action="`+base+`/link/revoke"`)
	ensure.DeepEqual(t, invitesSent(t, ta, admin.ID), int64(0))

	// revoke, then a fresh one
	ensure.DeepEqual(t, ta.post(adminUserPath(carol.ID)+"/link/revoke",
		url.Values{"id": {strconv.FormatInt(link.ID, 10)}}, ta.as(admin)).Code, http.StatusNotFound)
	w = ta.post(base+"/link/revoke", url.Values{"id": {strconv.FormatInt(link.ID, 10)}}, ta.as(admin))
	ensure.StringContains(t, ta.flashOf(w, ta.as(admin)), "Link revoked.")
	ensure.DeepEqual(t, ta.get(invitePath(link.Token)).Code, http.StatusNotFound)
	ta.post(base+"/link", nil, ta.as(admin))
	link = pendingLinks(t, ta, bob.ID)[0]

	// bob registers a new passkey and signs in as the same user
	aw, auth, cred := ta.acceptInvite(link)
	ensure.DeepEqual(t, aw.Code, http.StatusSeeOther, aw.Body.String())
	ensure.DeepEqual(t, countRows(t, ta, "users"), 3)
	si := ta.signIn(auth, cred)
	ensure.DeepEqual(t, si.Header().Get("Location"), pHome)
	ensure.StringContains(t, ta.get(pSettings, withCookies(si)).Body.String(), "<dd>bob</dd>")
	row, err := ta.app.q.CredentialByCredentialID(ctx, cred.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, row.UserID, bob.ID)
	ensure.DeepEqual(t, invitesSent(t, ta, admin.ID), int64(0))
}

// profile picture

func testImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	return img
}

// jpegDataURL is what main.js posts for a cropped picture of the given size.
func jpegDataURL(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	ensure.Nil(t, jpeg.Encode(&buf, testImage(w, h), nil))
	return avatarPrefix + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// setAvatar posts a profile picture of the given size for u.
func (ta *testApp) setAvatar(u *testUser, w, h int) *httptest.ResponseRecorder {
	ta.t.Helper()
	return ta.post(pSettingsAvatar, url.Values{"image": {jpegDataURL(ta.t, w, h)}}, ta.as(u))
}

func avatarOf(t *testing.T, ta *testApp, userID int64) string {
	t.Helper()
	u, err := ta.app.q.UserByID(t.Context(), userID)
	ensure.Nil(t, err)
	return u.Avatar
}

func TestProfilePicture(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	bob := ta.newUser("bob")

	// nothing yet: the initial, a way to choose, nothing to remove
	page := ta.get(pSettings, ta.as(alice)).Body.String()
	ensure.StringContains(t, page, `<span class="avatar avatar-l hue-`+idStr(alice.ID%12)+`" aria-hidden="true">A</span>`)
	ensure.StringContains(t, page, `data-avatar="crop"`)
	ensure.StringContains(t, page, `<dialog id="crop"`)
	ensure.StringContains(t, page, `action="/settings/avatar"`)
	ensure.StringDoesNotContain(t, page, `action="/settings/avatar/remove"`)

	w := ta.setAvatar(alice, avatarSize, avatarSize)
	ensure.DeepEqual(t, w.Header().Get("Location"), pSettings)
	ensure.StringContains(t, ta.flashOf(w, ta.as(alice)), "Profile picture saved.")
	first := avatarOf(t, ta, alice.ID)
	ensure.DeepEqual(t, len(first), 16)
	ensure.DeepEqual(t, avatarOf(t, ta, bob.ID), "")
	page = ta.get(pSettings, ta.as(alice)).Body.String()
	ensure.StringContains(t, page, `<img class="avatar avatar-l" src="`+avatarPath(alice.ID, first)+`"`)
	ensure.StringContains(t, page, `action="/settings/avatar/remove"`)

	// served to any signed in user, as a JPEG of the posted size, for good
	for _, u := range []*testUser{alice, bob} {
		w = ta.get(avatarPath(alice.ID, first), ta.as(u))
		ensure.DeepEqual(t, w.Code, http.StatusOK)
		ensure.DeepEqual(t, w.Header().Get("Content-Type"), "image/jpeg")
		ensure.DeepEqual(t, w.Header().Get("Cache-Control"), "private, max-age=31536000, immutable")
		ensure.DeepEqual(t, w.Header().Get("X-Content-Type-Options"), "nosniff")
		cfg, err := jpeg.DecodeConfig(w.Body)
		ensure.Nil(t, err)
		ensure.DeepEqual(t, []int{cfg.Width, cfg.Height}, []int{avatarSize, avatarSize})
	}
	w = ta.get(avatarPath(alice.ID, first))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, w.Header().Get("Location"), pSignIn)

	// a new picture has a new name, and the old name is gone
	ensure.DeepEqual(t, ta.setAvatar(alice, 100, 100).Code, http.StatusSeeOther)
	second := avatarOf(t, ta, alice.ID)
	ensure.True(t, second != first && second != "")
	ensure.DeepEqual(t, ta.get(avatarPath(alice.ID, first), ta.as(bob)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(avatarPath(alice.ID, second), ta.as(bob)).Code, http.StatusOK)
	ensure.DeepEqual(t, countRows(t, ta, "avatars"), 1)

	// names that do not exist
	for _, p := range []string{
		avatarPath(bob.ID, second), avatarPath(alice.ID, "nope"),
		avatarPath(999, second), pAvatar + "x/" + second,
	} {
		ensure.DeepEqual(t, ta.get(p, ta.as(bob)).Code, http.StatusNotFound, p)
	}

	// removing is only ever your own
	w = ta.post(pSettingsAvatarDel, nil, ta.as(bob))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, avatarOf(t, ta, alice.ID), second)
	w = ta.post(pSettingsAvatarDel, nil, ta.as(alice))
	ensure.StringContains(t, ta.flashOf(w, ta.as(alice)), "Profile picture removed.")
	ensure.DeepEqual(t, avatarOf(t, ta, alice.ID), "")
	ensure.DeepEqual(t, countRows(t, ta, "avatars"), 0)
	ensure.DeepEqual(t, ta.get(avatarPath(alice.ID, second), ta.as(alice)).Code, http.StatusNotFound)

	w = ta.post(pSettingsAvatar, url.Values{"image": {jpegDataURL(t, 8, 8)}})
	ensure.DeepEqual(t, w.Code, http.StatusUnauthorized)
}

func TestProfilePictureRejected(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	var pngBuf bytes.Buffer
	ensure.Nil(t, png.Encode(&pngBuf, testImage(16, 16)))
	valid := jpegDataURL(t, 16, 16)
	cases := map[string]string{
		"missing":      "",
		"not a url":    "hello",
		"bad base64":   avatarPrefix + "!!!",
		"not a jpeg":   avatarPrefix + base64.StdEncoding.EncodeToString([]byte("hello")),
		"png as jpeg":  avatarPrefix + base64.StdEncoding.EncodeToString(pngBuf.Bytes()),
		"png data url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBuf.Bytes()),
		"wide":         jpegDataURL(t, 64, 32),
		"tall":         jpegDataURL(t, 32, 64),
		"too large":    jpegDataURL(t, avatarSize+1, avatarSize+1),
		"truncated":    valid[:len(valid)/2&^3],
		"over limit":   valid + strings.Repeat("A", maxAvatarForm),
	}
	for name, image := range cases {
		w := ta.post(pSettingsAvatar, url.Values{"image": {image}}, ta.as(alice))
		ensure.StringContains(t, ta.flashOf(w, ta.as(alice)), "That picture could not be used.", name)
		ensure.DeepEqual(t, avatarOf(t, ta, alice.ID), "", name)
	}
	ensure.DeepEqual(t, countRows(t, ta, "avatars"), 0)
}

// What is stored is encoded by the server: bytes after the image are dropped.
func TestProfilePictureReencoded(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	var buf bytes.Buffer
	ensure.Nil(t, jpeg.Encode(&buf, testImage(16, 16), nil))
	buf.WriteString("<script>alert(1)</script>")
	w := ta.post(pSettingsAvatar, url.Values{
		"image": {avatarPrefix + base64.StdEncoding.EncodeToString(buf.Bytes())}}, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	w = ta.get(avatarPath(alice.ID, avatarOf(t, ta, alice.ID)), ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.False(t, bytes.Contains(w.Body.Bytes(), []byte("<script>")))
}

// The picture appears wherever the user does.
func TestProfilePictureShown(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	group := ta.newChat(alice, "G", bob, carol)
	dm := ta.newDM(alice, bob)
	ta.newMessage(group, bob, "from bob")
	ta.newMessage(group, carol, "from carol")
	ta.newMessage(dm, bob, "hello")
	ensure.DeepEqual(t, ta.setAvatar(bob, 64, 64).Code, http.StatusSeeOther)
	src := `src="` + avatarPath(bob.ID, avatarOf(t, ta, bob.ID)) + `"`
	img := func(size string) string { return `<img class="avatar` + size + `" ` + src }
	initial := func(u *testUser, size string) string {
		return fmt.Sprintf(`<span class="avatar%s hue-%d" aria-hidden="true">%s</span>`,
			size, u.ID%12, strings.ToUpper(u.Username[:1]))
	}

	// a group: beside the first bubble of each run, never beside your own
	page := ta.get(chatPath(group.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, page, `<div id="messages" class="group">`)
	ensure.StringContains(t, page, img(" avatar-s msg-avatar"))
	ensure.StringContains(t, page, initial(carol, " avatar-s msg-avatar"))
	ensure.DeepEqual(t, strings.Count(ta.get(chatPath(group.ID), ta.as(bob)).Body.String(), src), 0)

	// a direct message: in the header and the chat list, not in the bubbles
	page = ta.get(chatPath(dm.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, page, `<div id="messages">`)
	ensure.StringContains(t, page, `class="pane-title" href="`+chatPath(dm.ID)+`/info">`+img(" avatar-s"))
	ensure.StringContains(t, page, fmt.Sprintf(`id="chat-%d" class="chat-item dm"`, dm.ID)+
		` href="`+chatPath(dm.ID)+`" aria-current="page">`+img(""))
	ensure.StringDoesNotContain(t, page, "msg-avatar")
	ensure.StringDoesNotContain(t, page, "msg-who") // nor the name: there is only one other person
	// bob sees alice there, who has no picture
	page = ta.get(chatPath(dm.ID), ta.as(bob)).Body.String()
	ensure.StringContains(t, page, initial(alice, " avatar-s"))
	ensure.DeepEqual(t, strings.Count(page, src), 0)

	// member list, the list to start a direct message, and new group
	ensure.StringContains(t, ta.get(chatPath(group.ID)+"/info", ta.as(alice)).Body.String(), img(" avatar-s"))
	ensure.StringContains(t, ta.get(pDM, ta.as(alice)).Body.String(), img(" avatar-s"))
	page = ta.get(pNew, ta.as(alice)).Body.String()
	ensure.StringContains(t, page, img(" avatar-s"))
	ensure.StringContains(t, page, initial(carol, " avatar-s"))
}
