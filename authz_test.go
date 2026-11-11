package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daaku/ensure"
	"github.com/descope/virtualwebauthn"
)

// The authorisation matrix: every registered route, requested as every kind
// of user. A route is either allowed for a role, or it must be refused in the
// standard way: a redirect to sign in (GET) or 401 (other methods) when
// signed out, and 404 when signed in, so that nothing is revealed.

type role int

const (
	rOut     role = iota // signed out
	rPlain               // no permissions, in no chat
	rOther               // member of a different group
	rMember              // member of the group
	rCreator             // creator of the group, holds the group permission
	rInvite              // only the invite permission
	rGroup               // only the group permission (creator of the other group)
	rAdmin               // admin, member of nothing
	roleCount
)

var roleNames = []string{"signed out", "plain", "other group", "member", "creator", "invite only", "group only", "admin"}

type roleSet uint

func roles(rs ...role) roleSet {
	var s roleSet
	for _, r := range rs {
		s |= 1 << r
	}
	return s
}

func (s roleSet) has(r role) bool { return s&(1<<r) != 0 }

var (
	everyone = roles(rOut, rPlain, rOther, rMember, rCreator, rInvite, rGroup, rAdmin)
	signedIn = everyone &^ roles(rOut)
	viewers  = roles(rMember, rCreator, rAdmin)
	members  = roles(rMember, rCreator)
	managers = roles(rCreator, rAdmin)
)

type matrixRow struct {
	allowed roleSet
	// req builds the request for a role. Rows that change or consume
	// something build a fresh target on every call.
	req func(r role) *http.Request
	// after checks side effects once the response is in.
	after func(r role, w *httptest.ResponseRecorder)
}

type matrix struct {
	t     *testing.T
	ta    *testApp
	users [roleCount]*testUser
	g, h  Chat // the group under test, and a different one
	dm    Chat // between member and creator
	file  int64
	n     int
}

func (m *matrix) form(method, path string, kv ...string) *http.Request {
	v := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	var r *http.Request
	if method == http.MethodGet {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

func (m *matrix) json(path string, body any) *http.Request {
	b, err := json.Marshal(body)
	ensure.Nil(m.t, err)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

func (m *matrix) get(path string) func(role) *http.Request {
	return func(role) *http.Request { return m.form(http.MethodGet, path) }
}

func (m *matrix) post(path string, kv ...string) func(role) *http.Request {
	return func(role) *http.Request { return m.form(http.MethodPost, path, kv...) }
}

// message is a fresh message by the member in the group.
func (m *matrix) message() Message {
	m.n++
	return m.ta.newMessage(m.g, m.users[rMember], fmt.Sprintf("message %d", m.n))
}

func (m *matrix) msgPath(suffix string) string {
	return messagePath(m.g.ID, m.message().ID) + suffix
}

// pendingUpload is a fresh unfinished upload by the member, with its part.
func (m *matrix) pendingUpload() int64 {
	out := m.ta.uploadInit(m.users[rMember], m.g, "up.bin", 5)
	m.ta.uploadParts(out.URLs, []byte("12345"))
	return out.File
}

func (m *matrix) exists(table string, id int64) bool {
	var n int
	ensure.Nil(m.t, m.ta.app.db.QueryRowContext(m.t.Context(),
		"SELECT count(*) FROM "+table+" WHERE id = ?", id).Scan(&n))
	return n == 1
}

func (m *matrix) invite(by *testUser, target *User) Invite {
	m.n++
	inv, err := m.ta.app.createInvite(m.t.Context(), m.ta.app.q, inviteParams{
		Username: fmt.Sprintf("guest%d", m.n), User: target, CreatedBy: by.ID, TTL: time.Hour,
	})
	ensure.Nil(m.t, err)
	return inv
}

func (m *matrix) setMember(chat Chat, u *testUser, in bool) {
	q := "DELETE FROM members WHERE chat_id = ? AND user_id = ?"
	args := []any{chat.ID, u.ID}
	if in {
		q = "INSERT OR IGNORE INTO members (chat_id, user_id, added_by, created_at) VALUES (?, ?, ?, ?)"
		args = append(args, chat.CreatedBy, testNow.Unix())
	}
	_, err := m.ta.app.db.ExecContext(m.t.Context(), q, args...)
	ensure.Nil(m.t, err)
}

func TestAuthorisationMatrix(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	m := &matrix{t: t, ta: ta}
	m.users = [roleCount]*testUser{
		rPlain:   ta.newUser("plain"),
		rOther:   ta.newUser("other"),
		rMember:  ta.newUser("member"),
		rCreator: ta.newUser("creator", "group"),
		rInvite:  ta.newUser("inviter", "invite"),
		rGroup:   ta.newUser("grouper", "group"),
		rAdmin:   ta.newUser("boss", "admin"),
	}
	victim := ta.newUser("victim") // added to and removed from the group
	member, creator := m.users[rMember], m.users[rCreator]
	m.g = ta.newChat(creator, "G", member)
	m.h = ta.newChat(m.users[rGroup], "H", m.users[rOther])
	m.dm = ta.newDM(member, creator)
	m.file = ta.upload(member, m.g, "a.bin", []byte("hello"))
	dmFile := ta.upload(member, m.dm, "private.bin", []byte("secret"))
	dmMsg := ta.newMessage(m.dm, member, "just between us")
	ta.subscribe(member, "https://push.example/member")
	subs, err := ta.app.q.PushSubsByUser(ctx, member.ID)
	ensure.Nil(t, err)
	memberSub := subs[0].ID
	g, dm := chatPath(m.g.ID), chatPath(m.dm.ID)
	adminUser := pAdminUser + idStr(victim.ID)
	dmOnly := roles(rMember, rCreator) // the two participants, and not the admin

	rows := map[string]matrixRow{
		"GET /robots.txt":      {allowed: everyone, req: m.get("/robots.txt")},
		"GET /sw.js":           {allowed: everyone, req: m.get(pSWJS)},
		"GET /app.webmanifest": {allowed: everyone, req: m.get(pAppWebManifest)},
		"GET /sign-in":         {allowed: everyone, req: m.get(pSignIn)},
		"GET /sign-in/options": {allowed: everyone, req: m.get(pSignInOptions)},
		"POST /sign-in":        {allowed: everyone, req: m.post(pSignIn, "json", "{}")},
		"POST /sign-out":       {allowed: signedIn, req: m.post(pSignOut)},
		"GET /invite/{token}": {allowed: everyone, req: func(role) *http.Request {
			return m.form(http.MethodGet, invitePath(m.invite(m.users[rInvite], nil).Token))
		}},
		"GET /invite/{token}/options": {allowed: everyone, req: func(role) *http.Request {
			return m.form(http.MethodGet, invitePath(m.invite(m.users[rInvite], nil).Token)+"/options")
		}},
		"POST /invite/{token}": {allowed: everyone, req: func(role) *http.Request {
			return m.form(http.MethodPost, invitePath(m.invite(m.users[rInvite], nil).Token), "json", "{}")
		}},

		"GET /{$}":      {allowed: signedIn, req: m.get(pHome)},
		"GET /archived": {allowed: signedIn, req: m.get(pArchived)},
		"GET /settings": {allowed: signedIn, req: m.get(pSettings)},
		"GET /events": {allowed: signedIn, req: func(role) *http.Request {
			c, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
			t.Cleanup(cancel)
			return m.form(http.MethodGet, pEvents).WithContext(c)
		}},
		"GET /dm": {allowed: signedIn, req: m.get(pDM)},
		// everyone may message the victim
		"POST /dm": {allowed: signedIn, req: m.post(pDM, "user", idStr(victim.ID))},

		"GET /new":  {allowed: roles(rCreator, rGroup, rAdmin), req: m.get(pNew)},
		"POST /new": {allowed: roles(rCreator, rGroup, rAdmin), req: m.post(pNew, "name", "Another")},

		// the group
		"GET /c/{id}":       {allowed: viewers, req: m.get(g)},
		"GET /c/{id}/info":  {allowed: viewers, req: m.get(g + "/info")},
		"POST /c/{id}/send": {allowed: members, req: m.post(g+"/send", "body", "hi")},
		"POST /c/{id}/react": {allowed: members, req: func(role) *http.Request {
			return m.form(http.MethodPost, g+"/react", "message", idStr(m.message().ID), "emoji", "👍")
		}},
		"POST /c/{id}/archive":   {allowed: members, req: m.post(g + "/archive")},
		"POST /c/{id}/unarchive": {allowed: members, req: m.post(g + "/unarchive")},
		"GET /c/{id}/m/{mid}/edit": {allowed: roles(rMember), req: func(role) *http.Request {
			return m.form(http.MethodGet, m.msgPath("/edit"))
		}},
		"POST /c/{id}/m/{mid}/edit": {allowed: roles(rMember), req: func(role) *http.Request {
			return m.form(http.MethodPost, m.msgPath("/edit"), "body", "changed")
		}},
		"POST /c/{id}/m/{mid}/delete": {allowed: roles(rMember, rAdmin), req: func(role) *http.Request {
			return m.form(http.MethodPost, m.msgPath("/delete"))
		}},
		"POST /c/{id}/m/{mid}/snooze": {allowed: members, req: func(role) *http.Request {
			return m.form(http.MethodPost, m.msgPath("/snooze"), "when", "1h")
		}},
		"POST /c/{id}/m/{mid}/unsnooze": {allowed: members, req: func(role) *http.Request {
			return m.form(http.MethodPost, m.msgPath("/unsnooze"))
		}},
		"POST /c/{id}/rename": {allowed: managers, req: m.post(g+"/rename", "name", "G")},
		"POST /c/{id}/members/add": {allowed: managers,
			req: func(role) *http.Request {
				m.setMember(m.g, victim, false)
				return m.form(http.MethodPost, g+"/members/add", "user", idStr(victim.ID))
			},
			after: func(r role, w *httptest.ResponseRecorder) {
				ensure.DeepEqual(t, isMember(t, ta, m.g.ID, victim.ID), managers.has(r), roleNames[r])
			}},
		"POST /c/{id}/members/remove": {allowed: managers,
			req: func(role) *http.Request {
				m.setMember(m.g, victim, true)
				return m.form(http.MethodPost, g+"/members/remove", "user", idStr(victim.ID))
			},
			after: func(r role, w *httptest.ResponseRecorder) {
				ensure.DeepEqual(t, isMember(t, ta, m.g.ID, victim.ID), !managers.has(r), roleNames[r])
			}},

		// files
		"POST /upload/init": {allowed: members, req: func(role) *http.Request {
			return m.json(pUploadInit, uploadInitIn{Chat: m.g.ID, Name: "x", Size: 5})
		}},
		"POST /upload/complete": {allowed: roles(rMember), req: func(role) *http.Request {
			return m.json(pUploadComplete, uploadFileIn{File: m.pendingUpload()})
		}},
		// abort always answers ok, but only the owner's request does anything
		"POST /upload/abort": func() matrixRow {
			var id int64
			return matrixRow{allowed: signedIn,
				req: func(role) *http.Request {
					id = m.pendingUpload()
					return m.json(pUploadAbort, uploadFileIn{File: id})
				},
				after: func(r role, w *httptest.ResponseRecorder) {
					ensure.DeepEqual(t, m.exists("files", id), r != rMember, roleNames[r])
				}}
		}(),
		"GET /f/{id}/{variant}": {allowed: viewers, req: m.get(filePath(m.file, "o"))},

		// invites
		"GET /invites": {allowed: roles(rInvite, rAdmin), req: m.get(pInvites)},
		"POST /invites": {allowed: roles(rInvite, rAdmin), req: func(role) *http.Request {
			m.n++
			return m.form(http.MethodPost, pInvites, "username", fmt.Sprintf("newbie%d", m.n))
		}},
		"POST /invites/revoke": {allowed: roles(rInvite, rAdmin), req: func(role) *http.Request {
			return m.form(http.MethodPost, pInvitesRevoke, "id", idStr(m.invite(m.users[rInvite], nil).ID))
		}},

		// settings: your own things only
		"POST /settings/link": {allowed: signedIn, req: m.post(pSettingsLink)},
		"POST /settings/link/revoke": {allowed: roles(rMember), req: func(role) *http.Request {
			return m.form(http.MethodPost, pSettingsLinkRevoke, "id", idStr(m.invite(member, &member.User).ID))
		}},
		"GET /passkey/options": {allowed: signedIn, req: m.get(pPasskeyOptions)},
		"POST /passkey/create": {allowed: signedIn, req: m.post(pPasskeyCreate, "json", "{}")},
		// deleting someone else's passkey is a silent no op
		"POST /passkey/delete": {allowed: signedIn,
			req: m.post(pPasskeyDelete, "id", idStr(member.credID)),
			after: func(r role, w *httptest.ResponseRecorder) {
				ensure.True(t, m.exists("credentials", member.credID), roleNames[r])
			}},
		"POST /push/subscribe": {allowed: signedIn, req: func(r role) *http.Request {
			sub := fmt.Sprintf(`{"endpoint":"https://push.example/%d","keys":{"auth":"a","p256dh":"p"}}`, r)
			return m.form(http.MethodPost, pPushSubscribe, "json", sub)
		}},
		"POST /push/unsubscribe": {allowed: roles(rMember),
			req: func(r role) *http.Request {
				if r == rMember { // remove a spare one, keep memberSub for the others
					ta.subscribe(member, "https://push.example/spare")
					subs, err := ta.app.q.PushSubsByUser(ctx, member.ID)
					ensure.Nil(t, err)
					return m.form(http.MethodPost, pPushUnsubscribe, "id", idStr(subs[len(subs)-1].ID))
				}
				return m.form(http.MethodPost, pPushUnsubscribe, "id", idStr(memberSub))
			},
			after: func(r role, w *httptest.ResponseRecorder) {
				ensure.True(t, m.exists("push_subscriptions", memberSub), roleNames[r])
			}},
		"POST /push/opened": {allowed: signedIn, req: m.post(pPushOpened)},

		// admin
		"GET /admin":        {allowed: roles(rAdmin), req: m.get(pAdmin)},
		"GET /admin/groups": {allowed: roles(rAdmin), req: m.get(pAdminGroups)},
		"GET /admin/u/{id}": {allowed: roles(rAdmin), req: m.get(adminUser)},
		"POST /admin/u/{id}/perms": {allowed: roles(rAdmin), req: m.post(adminUser+"/perms", "invite", "1"),
			after: func(r role, w *httptest.ResponseRecorder) {
				u, err := ta.app.q.UserByID(ctx, victim.ID)
				ensure.Nil(t, err)
				ensure.DeepEqual(t, u.PermInvite, r == rAdmin, roleNames[r])
			}},
		"POST /admin/u/{id}/passkey/delete": {allowed: roles(rAdmin),
			req: m.post(adminUser+"/passkey/delete", "id", idStr(victim.credID)),
			after: func(r role, w *httptest.ResponseRecorder) {
				ensure.DeepEqual(t, m.exists("credentials", victim.credID), r != rAdmin, roleNames[r])
			}},
		"POST /admin/u/{id}/link": {allowed: roles(rAdmin), req: m.post(adminUser + "/link")},
		"POST /admin/u/{id}/link/revoke": {allowed: roles(rAdmin), req: func(role) *http.Request {
			return m.form(http.MethodPost, adminUser+"/link/revoke", "id", idStr(m.invite(m.users[rAdmin], &victim.User).ID))
		}},
	}

	// Routes that do not go through sessions at all.
	skip := map[string]bool{
		"GET /assets/": true, // public static files
		pFakeS3:        true, // every request carries its own signature
	}

	run := func(name string, row matrixRow) {
		t.Helper()
		method, _, _ := strings.Cut(name, " ")
		for r := rOut; r < roleCount; r++ {
			req := row.req(r)
			if u := m.users[r]; u != nil {
				req.AddCookie(u.cookie)
			}
			w := httptest.NewRecorder()
			ta.h.ServeHTTP(w, req)
			toSignIn := w.Code == http.StatusSeeOther && w.Header().Get("Location") == pSignIn
			var ok bool
			switch {
			case row.allowed.has(r):
				// allowed: anything but an authorisation refusal. A signed
				// in user on the sign in page is sent home, never to sign in.
				ok = w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden &&
					w.Code != http.StatusNotFound && w.Code < 500 && (!toSignIn || name == "POST /sign-in" || name == "POST /sign-out")
			case r == rOut && method == http.MethodGet:
				ok = toSignIn
			case r == rOut:
				ok = w.Code == http.StatusUnauthorized
			default:
				ok = w.Code == http.StatusNotFound
			}
			if !ok {
				t.Errorf("%s as %s: status %d (Location %q), allowed=%v", name, roleNames[r],
					w.Code, w.Header().Get("Location"), row.allowed.has(r))
			}
			if row.after != nil {
				row.after(r, w)
			}
		}
	}

	for _, pattern := range ta.app.routes {
		if skip[pattern] {
			continue
		}
		row, found := rows[pattern]
		if !found {
			t.Errorf("route %q has no row in the authorisation matrix", pattern)
			continue
		}
		run(pattern, row)
	}
	for pattern := range rows {
		found := false
		for _, p := range ta.app.routes {
			found = found || p == pattern
		}
		ensure.True(t, found, "matrix row for a route that is not registered: "+pattern)
	}

	// A direct message is closed to everyone but its two participants,
	// admins included, on every chat route.
	dmRows := map[string]matrixRow{
		"GET page": {allowed: dmOnly, req: m.get(dm)},
		"GET info": {allowed: dmOnly, req: m.get(dm + "/info")},
		"GET events": {allowed: dmOnly, req: func(role) *http.Request {
			c, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
			t.Cleanup(cancel)
			return m.form(http.MethodGet, pEvents+"?chat="+idStr(m.dm.ID)).WithContext(c)
		}},
		"POST send":    {allowed: dmOnly, req: m.post(dm+"/send", "body", "hi")},
		"POST react":   {allowed: dmOnly, req: m.post(dm+"/react", "message", idStr(dmMsg.ID), "emoji", "👍")},
		"POST archive": {allowed: dmOnly, req: m.post(dm + "/archive")},
		"POST snooze":  {allowed: dmOnly, req: m.post(messagePath(m.dm.ID, dmMsg.ID)+"/snooze", "when", "1h")},
		"GET edit":     {allowed: roles(rMember), req: m.get(messagePath(m.dm.ID, dmMsg.ID) + "/edit")},
		"GET file":     {allowed: dmOnly, req: m.get(filePath(dmFile, "o"))},
		"POST rename":  {allowed: 0, req: m.post(dm+"/rename", "name", "x")},
		"POST add":     {allowed: 0, req: m.post(dm+"/members/add", "user", idStr(victim.ID))},
		"POST remove":  {allowed: 0, req: m.post(dm+"/members/remove", "user", idStr(member.ID))},
		"POST upload": {allowed: dmOnly, req: func(role) *http.Request {
			return m.json(pUploadInit, uploadInitIn{Chat: m.dm.ID, Name: "x", Size: 5})
		}},
	}
	for name, row := range dmRows {
		run(name+" (direct message)", row)
	}
	// the last thing an admin could try: deleting in a direct message
	w := ta.post(messagePath(m.dm.ID, dmMsg.ID)+"/delete", nil, ta.as(m.users[rAdmin]))
	ensure.DeepEqual(t, w.Code, http.StatusNotFound)
	ensure.DeepEqual(t, isMember(t, ta, m.dm.ID, member.ID), true)

	// A message id from another chat never works through this chat's path.
	hMsg := ta.newMessage(m.h, m.users[rOther], "in H")
	for _, suffix := range []string{"/edit", "/delete", "/snooze", "/unsnooze"} {
		w := ta.post(messagePath(m.g.ID, hMsg.ID)+suffix, url.Values{"body": {"x"}, "when": {"1h"}}, ta.as(member))
		ensure.DeepEqual(t, w.Code, http.StatusNotFound, suffix)
	}
	w = ta.post(g+"/react", url.Values{"message": {idStr(hMsg.ID)}, "emoji": {"👍"}}, ta.as(member))
	ensure.DeepEqual(t, w.Code, http.StatusNotFound)

	// A snooze is visible to the user who set it and to nobody else.
	snoozed := m.message()
	ta.post(messagePath(m.g.ID, snoozed.ID)+"/snooze", url.Values{"when": {"1h"}}, ta.as(member))
	marker := `id="m` + idStr(snoozed.ID) + `-snooze"`
	ensure.StringContains(t, ta.get(g, ta.as(member)).Body.String(), marker)
	for _, r := range []role{rCreator, rAdmin} {
		ensure.False(t, strings.Contains(ta.get(g, ta.as(m.users[r])).Body.String(), marker), roleNames[r])
	}
	// and nobody else can cancel it
	ta.post(messagePath(m.g.ID, snoozed.ID)+"/unsnooze", nil, ta.as(creator))
	ensure.StringContains(t, ta.get(g, ta.as(member)).Body.String(), marker)

	// Archiving is personal too.
	ta.post(g+"/archive", nil, ta.as(member))
	ensure.False(t, strings.Contains(ta.get(pHome, ta.as(member)).Body.String(), `id="chat-`+idStr(m.g.ID)+`"`))
	ensure.StringContains(t, ta.get(pHome, ta.as(creator)).Body.String(), `id="chat-`+idStr(m.g.ID)+`"`)

	// Stats pages show only the viewer's own numbers.
	settings := ta.get(pSettings, ta.as(m.users[rPlain])).Body.String()
	ensure.False(t, strings.Contains(settings, "member"), "another username on the settings page")
}

// Twenty browsers finish the passkey ceremony for the same invite at once.
// Exactly one becomes the user.
func TestInviteAcceptedOnceUnderConcurrency(t *testing.T) {
	ta := newTestApp(t)
	inv := ta.newInvite("bob", false)
	path := invitePath(inv.Token)
	type attempt struct {
		wo   *httptest.ResponseRecorder
		resp string
	}
	attempts := make([]attempt, 20)
	for i := range attempts {
		auth := virtualwebauthn.NewAuthenticatorWithOptions(
			virtualwebauthn.AuthenticatorOptions{UserHandle: inv.Handle})
		cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
		wo := ta.get(path + "/options")
		ao, err := virtualwebauthn.ParseAttestationOptions(wo.Body.String())
		ensure.Nil(t, err)
		attempts[i] = attempt{wo, virtualwebauthn.CreateAttestationResponse(testRP, auth, cred, *ao)}
	}
	codes := make([]int, len(attempts))
	var wg sync.WaitGroup
	for i, at := range attempts {
		wg.Go(func() {
			codes[i] = ta.post(path, url.Values{"json": {at.resp}}, withCookies(at.wo)).Code
		})
	}
	wg.Wait()
	won := 0
	for _, c := range codes {
		if c == http.StatusSeeOther {
			won++
		} else {
			ensure.DeepEqual(t, c, http.StatusNotFound)
		}
	}
	ensure.DeepEqual(t, won, 1)
	ensure.DeepEqual(t, countRows(t, ta, "users"), 1)
	ensure.DeepEqual(t, countRows(t, ta, "credentials"), 1)
	ensure.DeepEqual(t, countRows(t, ta, "invites"), 0)
}

// Fifty senders and ten open streams on one group: every message is stored
// exactly once, nothing fails, and every stream hears about it.
func TestConcurrentSendersAndStreams(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "Team", bob)
	srv := ta.server()
	streams := make([]*stream, 10)
	for i := range streams {
		streams[i] = ta.stream(srv, bob, "?chat="+idStr(chat.ID))
	}
	const senders, each = 50, 4
	codes := make(chan int, senders*each)
	var wg sync.WaitGroup
	for i := range senders {
		wg.Go(func() {
			u := alice
			if i%2 == 0 {
				u = bob
			}
			for j := range each {
				w := ta.post(chatPath(chat.ID)+"/send",
					url.Values{"body": {fmt.Sprintf("sender %d message %d", i, j)}}, ta.as(u))
				codes <- w.Code
			}
		})
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		ensure.DeepEqual(t, c, http.StatusSeeOther)
	}
	var total, distinct int
	ensure.Nil(t, ta.app.db.QueryRowContext(t.Context(),
		"SELECT count(*), count(DISTINCT body) FROM messages WHERE chat_id = ?", chat.ID).Scan(&total, &distinct))
	ensure.DeepEqual(t, []int{total, distinct}, []int{senders * each, senders * each})
	us, _ := ta.stats(alice.ID, chat.ID)
	ub, _ := ta.stats(bob.ID, chat.ID)
	ensure.DeepEqual(t, us.MessagesSent+ub.MessagesSent, int64(senders*each))
	for _, s := range streams {
		ensure.DeepEqual(t, s.next(), "data: 1")
	}
	// a page rendered now shows the newest message
	page := ta.get(chatPath(chat.ID), ta.as(bob)).Body.String()
	ensure.DeepEqual(t, strings.Count(page, `<article id="m`), 100)
}
