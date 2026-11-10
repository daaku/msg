package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
	ensure.StringContains(t, home.Body.String(), "bob")
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
