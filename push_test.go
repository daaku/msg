package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daaku/ensure"
	"github.com/daaku/webpush"
)

func subJSON(endpoint string) string {
	return `{"endpoint":"` + endpoint + `","expirationTime":null,"keys":{"p256dh":"BNcR","auth":"tBHI"}}`
}

// subscribe posts a push subscription for the endpoint as the user.
func (ta *testApp) subscribe(u *testUser, endpoint string) {
	ta.t.Helper()
	w := ta.post(pPushSubscribe, url.Values{"json": {subJSON(endpoint)}}, ta.as(u))
	ensure.DeepEqual(ta.t, w.Code, http.StatusSeeOther, w.Body.String())
	ensure.DeepEqual(ta.t, w.Header().Get("Location"), pSettings)
}

func (ta *testApp) subs(userID int64) []PushSubscription {
	ta.t.Helper()
	subs, err := ta.app.q.PushSubsByUser(ta.t.Context(), userID)
	ensure.Nil(ta.t, err)
	return subs
}

func endpoints(sends []fakeSend) []string {
	var out []string
	for _, s := range sends {
		out = append(out, s.Endpoint)
	}
	return out
}

func TestVAPIDKeyStable(t *testing.T) {
	dir := t.TempDir()
	a1 := newTestAppIn(t, dir).app
	a2 := newTestAppIn(t, dir).app
	ensure.True(t, a1.vapidPublic != "")
	ensure.DeepEqual(t, a1.vapidPublic, a2.vapidPublic)
	stored, err := a1.q.ConfigGet(t.Context(), "vapid")
	ensure.Nil(t, err)
	key, err := webpush.ParseVAPIDKey(string(stored))
	ensure.Nil(t, err)
	pub, err := vapidPublic(key)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, pub, a1.vapidPublic)
	ensure.NotDeepEqual(t, newTestApp(t).app.vapidPublic, a1.vapidPublic)
}

func TestPushSubscriber(t *testing.T) {
	ensure.DeepEqual(t, pushSubscriber("https://chat.example.com"), "https://chat.example.com")
	ensure.DeepEqual(t, pushSubscriber("http://localhost:8080"), "https://localhost:8080")
	// the real sender is set up with it
	cfg := newTestApp(t).app.cfg
	a, err := newApp(t.Context(), cfg)
	ensure.Nil(t, err)
	defer a.close()
	c := a.push.(*webpush.Client)
	ensure.DeepEqual(t, c.Subscriber, "https://localhost:8080")
	ensure.DeepEqual(t, c.TTL, 24*time.Hour)
	ensure.DeepEqual(t, c.Urgency, webpush.UrgencyHigh)
	ensure.DeepEqual(t, c.Client.Timeout, 30*time.Second)
}

func TestPushSubscribe(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	bob := ta.newUser("bob")

	ensure.DeepEqual(t, ta.post(pPushSubscribe, url.Values{"json": {subJSON("https://push.example/a")}}).Code,
		http.StatusUnauthorized)

	ta.subscribe(alice, "https://push.example/a")
	subs := ta.subs(alice.ID)
	ensure.DeepEqual(t, len(subs), 1)
	ensure.DeepEqual(t, subs[0].Endpoint, "https://push.example/a")
	ensure.StringContains(t, string(subs[0].Subscription), `"p256dh":"BNcR"`)
	ensure.StringContains(t, subs[0].Label, " on ")

	// the same endpoint again updates the row
	ta.subscribe(alice, "https://push.example/a")
	ensure.DeepEqual(t, len(ta.subs(alice.ID)), 1)
	ensure.DeepEqual(t, ta.subs(alice.ID)[0].ID, subs[0].ID)
	ensure.DeepEqual(t, countRows(t, ta, "push_subscriptions"), 1)

	// another user on the same browser takes it over
	ta.subscribe(bob, "https://push.example/a")
	ensure.DeepEqual(t, len(ta.subs(alice.ID)), 0)
	ensure.DeepEqual(t, len(ta.subs(bob.ID)), 1)
	ensure.DeepEqual(t, countRows(t, ta, "push_subscriptions"), 1)

	for _, bad := range []string{
		subJSON("http://push.example/a"),
		`{"endpoint":"https://push.example/b","keys":{"p256dh":"","auth":"x"}}`,
		`{"endpoint":"https://push.example/b"}`,
		subJSON("https:///nohost"),
		`nope`,
		``,
	} {
		w := ta.post(pPushSubscribe, url.Values{"json": {bad}}, ta.as(alice))
		ensure.DeepEqual(t, w.Code, http.StatusBadRequest, bad)
	}
	ensure.DeepEqual(t, countRows(t, ta, "push_subscriptions"), 1)
}

func TestPushUnsubscribe(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	bob := ta.newUser("bob")
	ta.subscribe(alice, "https://push.example/a1")
	ta.subscribe(alice, "https://push.example/a2")
	ta.subscribe(bob, "https://push.example/b")
	a1 := ta.subs(alice.ID)[0]
	b := ta.subs(bob.ID)[0]

	id := func(s PushSubscription) url.Values { return url.Values{"id": {idStr(s.ID)}} }
	ensure.DeepEqual(t, ta.post(pPushUnsubscribe, id(a1)).Code, http.StatusUnauthorized)
	// someone else's row is not found and stays
	ensure.DeepEqual(t, ta.post(pPushUnsubscribe, id(b), ta.as(alice)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, len(ta.subs(bob.ID)), 1)
	ensure.DeepEqual(t, ta.post(pPushUnsubscribe, url.Values{"id": {"x"}}, ta.as(alice)).Code, http.StatusBadRequest)

	w := ta.post(pPushUnsubscribe, id(a1), ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	left := ta.subs(alice.ID)
	ensure.DeepEqual(t, len(left), 1)
	ensure.DeepEqual(t, left[0].Endpoint, "https://push.example/a2")
}

func TestPushSettingsSection(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	bob := ta.newUser("bob")
	ta.subscribe(alice, "https://push.example/a")
	ta.subscribe(bob, "https://push.example/b")
	body := ta.get(pSettings, ta.as(alice)).Body.String()
	ensure.StringContains(t, body, `<section id="notifications"`)
	ensure.StringContains(t, body, `action="/push/subscribe"`)
	ensure.StringContains(t, body, `data-push-subscribe="`+ta.app.vapidPublic+`"`)
	ensure.StringContains(t, body, "Enable notifications on this device")
	ensure.StringContains(t, body, `class="push-hint denied"`)
	ensure.StringContains(t, body, `class="push-hint unavailable"`)
	ensure.StringContains(t, body, fmt.Sprintf(`id="push-sub-%d"`, ta.subs(alice.ID)[0].ID))
	ensure.StringDoesNotContain(t, body, fmt.Sprintf(`id="push-sub-%d"`, ta.subs(bob.ID)[0].ID))
	ensure.StringContains(t, body, `action="/push/unsubscribe"`)
	ensure.StringContains(t, body, `<meta data-sw="/sw.js">`)

	// the service worker is only registered for signed in users
	ensure.StringDoesNotContain(t, ta.get(pSignIn).Body.String(), "data-sw")
	// the sidebar prompt is always rendered; CSS shows it
	ensure.StringContains(t, ta.get(pHome, ta.as(alice)).Body.String(),
		`id="push-prompt" class="push-prompt-link" href="/settings#notifications"`)
}

func TestPushOnMessage(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	dave := ta.newUser("dave") // not a member
	chat := ta.newChat(alice, "Team", bob, carol)
	ta.subscribe(alice, "https://push.example/alice")
	ta.subscribe(bob, "https://push.example/bob1")
	ta.subscribe(bob, "https://push.example/bob2")
	ta.subscribe(carol, "https://push.example/carol")
	ta.subscribe(dave, "https://push.example/dave")

	w := ta.post(chatPath(chat.ID)+"/send", url.Values{"body": {"hello"}}, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	sent := endpoints(ta.sends())
	ensure.SameElements(t, sent, []string{
		"https://push.example/bob1", "https://push.example/bob2", "https://push.example/carol"})
	us, _ := ta.stats(bob.ID, chat.ID)
	ensure.DeepEqual(t, us.PushesSent, int64(2))
	us, _ = ta.stats(carol.ID, chat.ID)
	ensure.DeepEqual(t, us.PushesSent, int64(1))
	us, _ = ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, us.PushesSent, int64(0))

	// reactions and edits do not push
	m := ta.newMessage(chat, bob, "second")
	ta.sends()
	ensure.DeepEqual(t, react(ta, chat, m, "👍", alice), http.StatusSeeOther)
	ta.post(messagePath(chat.ID, m.ID)+"/edit", url.Values{"body": {"changed"}}, ta.as(bob))
	ta.post(chatPath(chat.ID)+"/rename", url.Values{"name": {"Crew"}}, ta.as(alice))
	ensure.DeepEqual(t, len(ta.sends()), 0)
}

func TestPushSkipsViewers(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	dave := ta.newUser("dave")
	chat := ta.newChat(alice, "Team", bob, carol, dave)
	other := ta.newChat(alice, "Other", bob, carol)
	ta.subscribe(bob, "https://push.example/bob")
	ta.subscribe(carol, "https://push.example/carol")
	ta.subscribe(dave, "https://push.example/dave")
	srv := ta.server()
	ta.stream(srv, bob, "?chat="+idStr(chat.ID))    // looking at it
	ta.stream(srv, carol, "?chat="+idStr(other.ID)) // looking at another chat
	// dave has no stream at all

	ta.newMessage(chat, alice, "hi")
	ensure.SameElements(t, endpoints(ta.sends()),
		[]string{"https://push.example/carol", "https://push.example/dave"})
}

func TestPushErrors(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "Team", bob)
	ta.subscribe(bob, "https://push.example/gone")
	ta.subscribe(bob, "https://push.example/flaky")
	ta.subscribe(bob, "https://push.example/ok")
	ta.push.failWith("https://push.example/gone", &webpush.Error{StatusCode: 410, Permanent: true})
	ta.push.failWith("https://push.example/flaky", errors.New("connection reset"))

	ta.newMessage(chat, alice, "hi")
	ensure.DeepEqual(t, endpoints(ta.sends()), []string{"https://push.example/ok"})
	var left []string
	for _, s := range ta.subs(bob.ID) {
		left = append(left, s.Endpoint)
	}
	ensure.SameElements(t, left, []string{"https://push.example/flaky", "https://push.example/ok"})
	us, _ := ta.stats(bob.ID, chat.ID)
	ensure.DeepEqual(t, us.PushesSent, int64(1))

	// a stored subscription that cannot be read is deleted
	_, err := ta.app.db.ExecContext(t.Context(),
		"UPDATE push_subscriptions SET subscription = 'junk' WHERE endpoint = 'https://push.example/ok'")
	ensure.Nil(t, err)
	ta.newMessage(chat, alice, "again")
	ensure.DeepEqual(t, len(ta.sends()), 0)
	ensure.DeepEqual(t, len(ta.subs(bob.ID)), 1)
}

func TestPushPayload(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "Team", bob)
	dm := ta.newDM(alice, bob)
	ta.subscribe(bob, "https://push.example/bob")

	ta.newMessage(chat, alice, "hello there")
	sent := ta.sends()
	ensure.DeepEqual(t, len(sent), 1)
	ensure.DeepEqual(t, sent[0].Payload, pushPayload{
		Title: "Team",
		Body:  "alice: hello there",
		Tag:   fmt.Sprintf("chat-%d", chat.ID),
		Data:  pushPayloadData{URL: chatPath(chat.ID)},
	})

	long := strings.Repeat("é", 200)
	ta.newMessage(chat, alice, long)
	sent = ta.sends()
	ensure.DeepEqual(t, sent[0].Payload.Body, "alice: "+strings.Repeat("é", 120))

	ta.newMessage(dm, alice, "psst")
	sent = ta.sends()
	ensure.DeepEqual(t, sent[0].Payload, pushPayload{
		Title: "alice",
		Body:  "psst",
		Tag:   fmt.Sprintf("chat-%d", dm.ID),
		Data:  pushPayloadData{URL: chatPath(dm.ID)},
	})

	// files use the file wording
	ta.upload(alice, chat, "pic.png", tinyPNG(t))
	ta.upload(alice, chat, "clip.mp4", tinyMP4)
	ta.upload(alice, dm, "notes.txt", []byte("plain text"))
	var bodies []string
	for _, s := range ta.sends() {
		bodies = append(bodies, s.Payload.Body)
	}
	ensure.SameElements(t, bodies, []string{"alice: 📷 Photo", "alice: 🎬 Video", "📎 notes.txt"})
}

func TestPushOpened(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	ensure.DeepEqual(t, ta.post(pPushOpened, nil).Code, http.StatusUnauthorized)
	w := ta.post(pPushOpened, nil, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusNoContent)
	ta.post(pPushOpened, nil, ta.as(alice))
	us, err := ta.app.userStats(t.Context(), alice.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, us, UserStat{UserID: alice.ID, PushesOpened: 2})
}

func TestServiceWorker(t *testing.T) {
	ta := newTestApp(t)
	w := ta.get(pSWJS)
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.DeepEqual(t, w.Header().Get("Cache-Control"), "no-cache")
	ensure.StringContains(t, w.Header().Get("Content-Type"), "javascript")
	body := w.Body.String()
	ensure.StringContains(t, body, "notificationclick")
	ensure.StringDoesNotContain(t, body, "'fetch'")
}

func TestPushShutdownWaits(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "Team", bob)
	ta.subscribe(bob, "https://push.example/bob")
	block := make(chan struct{})
	ta.push.mu.Lock()
	ta.push.block = block
	ta.push.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	ta.app.ctx = ctx

	ta.newMessage(chat, alice, "hi")
	deadline := time.Now().Add(2 * time.Second)
	for {
		ta.push.mu.Lock()
		busy := ta.push.busy
		ta.push.mu.Unlock()
		if busy == 1 {
			break
		}
		ensure.True(t, time.Now().Before(deadline), "the send did not start")
		time.Sleep(time.Millisecond)
	}
	waited := make(chan struct{})
	go func() {
		ta.app.bg.Wait()
		close(waited)
	}()
	cancel()
	select {
	case <-waited:
		t.Fatal("Wait returned while a send was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(block)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after the send finished")
	}
}

// snoozes

func (ta *testApp) snooze(u *testUser, chat Chat, m Message, when string, opts ...reqOpt) int {
	ta.t.Helper()
	return ta.post(messagePath(chat.ID, m.ID)+"/snooze", url.Values{"when": {when}},
		append([]reqOpt{ta.as(u)}, opts...)...).Code
}

func (ta *testApp) snoozeRows() []Snooze {
	ta.t.Helper()
	rows, err := ta.app.q.SnoozesDue(ta.t.Context(), 1<<62)
	ensure.Nil(ta.t, err)
	return rows
}

func withTZ(name string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "tz", Value: name}) }
}

func TestSnoozeDue(t *testing.T) {
	dubai, err := time.LoadLocation("Asia/Dubai")
	ensure.Nil(t, err)
	ny, err := time.LoadLocation("America/New_York")
	ensure.Nil(t, err)
	utc := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		ensure.Nil(t, err)
		return v
	}
	cases := []struct {
		now  string
		loc  *time.Location
		when string
		want string
	}{
		// 07:04 in Dubai
		{"2026-01-02 03:04", dubai, "1h", "2026-01-02 04:04"},
		{"2026-01-02 03:04", dubai, "3h", "2026-01-02 06:04"},
		{"2026-01-02 03:04", dubai, "tomorrow", "2026-01-03 05:00"},
		{"2026-01-02 03:04", dubai, "week", "2026-01-09 05:00"},
		// 23:59 in Dubai: tomorrow is the next calendar day there
		{"2026-01-02 19:59", dubai, "1h", "2026-01-02 20:59"},
		{"2026-01-02 19:59", dubai, "tomorrow", "2026-01-03 05:00"},
		{"2026-01-02 19:59", dubai, "week", "2026-01-09 05:00"},
		// 00:30 in Dubai on the 3rd, still the 2nd in UTC
		{"2026-01-02 20:30", dubai, "tomorrow", "2026-01-04 05:00"},
		// New York the day before summer time starts (8 March 2026)
		{"2026-03-07 15:00", ny, "1h", "2026-03-07 16:00"},
		{"2026-03-07 15:00", ny, "tomorrow", "2026-03-08 13:00"},
		{"2026-03-07 15:00", ny, "week", "2026-03-14 13:00"},
		{"2026-03-07 15:00", time.UTC, "tomorrow", "2026-03-08 09:00"},
	}
	for _, c := range cases {
		got, ok := snoozeDue(c.when, utc(c.now), c.loc)
		ensure.True(t, ok, c)
		ensure.DeepEqual(t, got.UTC().Format("2006-01-02 15:04"), c.want, c)
	}
	for _, bad := range []string{"", "2h", "Tomorrow", "1H"} {
		_, ok := snoozeDue(bad, time.Now(), time.UTC)
		ensure.False(t, ok, bad)
	}
}

func TestFmtDue(t *testing.T) {
	now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	ensure.DeepEqual(t, fmtDue(now.Add(3*time.Hour), now, time.UTC), "13:00")
	ensure.DeepEqual(t, fmtDue(time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC), now, time.UTC), "Tomorrow 09:00")
	ensure.DeepEqual(t, fmtDue(time.Date(2026, 1, 9, 9, 0, 0, 0, time.UTC), now, time.UTC), "Fri 9 Jan 09:00")
	ensure.DeepEqual(t, fmtDue(time.Date(2027, 1, 1, 9, 0, 0, 0, time.UTC), now, time.UTC), "1 Jan 2027 09:00")
	dubai, _ := time.LoadLocation("Asia/Dubai")
	// 22:00 UTC is 02:00 the next day in Dubai
	ensure.DeepEqual(t, fmtDue(time.Date(2026, 1, 2, 22, 0, 0, 0, time.UTC), now, dubai), "Tomorrow 02:00")
}

func TestSnoozeRoutes(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	boss := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "Team", bob)
	other := ta.newChat(alice, "Other")
	m := ta.newMessage(chat, bob, "remember this")
	mo := ta.newMessage(other, alice, "elsewhere")

	ensure.DeepEqual(t, ta.post(messagePath(chat.ID, m.ID)+"/snooze", url.Values{"when": {"1h"}}).Code,
		http.StatusUnauthorized)
	ensure.DeepEqual(t, ta.snooze(carol, chat, m, "1h"), http.StatusNotFound)
	ensure.DeepEqual(t, ta.snooze(boss, chat, m, "1h"), http.StatusNotFound) // admin, not a member
	// a message from another chat
	ensure.DeepEqual(t, ta.post(messagePath(chat.ID, mo.ID)+"/snooze", url.Values{"when": {"1h"}},
		ta.as(alice)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.snooze(alice, chat, m, "soon"), http.StatusBadRequest)
	ensure.DeepEqual(t, len(ta.snoozeRows()), 0)

	srv := ta.server()
	aliceS := ta.stream(srv, alice, "")
	bobS := ta.stream(srv, bob, "")
	w := ta.post(messagePath(chat.ID, m.ID)+"/snooze", url.Values{"when": {"tomorrow"}},
		ta.as(alice), withTZ("Asia/Dubai"))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, w.Header().Get("Location"), chatPath(chat.ID))
	ensure.DeepEqual(t, aliceS.next(), "data: 1")
	bobS.quiet()
	rows := ta.snoozeRows()
	ensure.DeepEqual(t, len(rows), 1)
	ensure.DeepEqual(t, rows[0], Snooze{MessageID: m.ID, UserID: alice.ID,
		DueAt: time.Date(2026, 1, 3, 5, 0, 0, 0, time.UTC).Unix(), CreatedAt: testNow.Unix()})

	// again replaces the time; one's own message works too
	ensure.DeepEqual(t, ta.snooze(alice, chat, m, "1h"), http.StatusSeeOther)
	ensure.DeepEqual(t, ta.snooze(bob, chat, m, "3h"), http.StatusSeeOther)
	rows = ta.snoozeRows()
	ensure.DeepEqual(t, len(rows), 2)
	ensure.DeepEqual(t, rows[0].UserID, alice.ID)
	ensure.DeepEqual(t, rows[0].DueAt, testNow.Add(time.Hour).Unix())
	aliceS.next()

	// unsnooze
	unsnooze := func(u *testUser, mm Message) int {
		return ta.post(messagePath(chat.ID, mm.ID)+"/unsnooze", nil, ta.as(u)).Code
	}
	ensure.DeepEqual(t, unsnooze(carol, m), http.StatusNotFound)
	ensure.DeepEqual(t, unsnooze(alice, m), http.StatusSeeOther)
	ensure.DeepEqual(t, aliceS.next(), "data: 1")
	rows = ta.snoozeRows()
	ensure.DeepEqual(t, len(rows), 1)
	ensure.DeepEqual(t, rows[0].UserID, bob.ID)
}

func TestSnoozeRender(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	chat := ta.newChat(alice, "Team", bob, carol)
	m := ta.newMessage(chat, bob, "remember this")
	marker := `id="m` + idStr(m.ID) + `-snooze"`
	hint := "Enable notifications in Settings</a> to get reminders."

	body := ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, body, `id="m`+idStr(m.ID)+`-remind"`)
	ensure.StringContains(t, body, `action="`+messagePath(chat.ID, m.ID)+`/snooze" data-morph`)
	for _, c := range snoozeChoices {
		ensure.StringContains(t, body, `name="when" value="`+c.Value+`">`+c.Label+`</button>`)
	}
	ensure.StringDoesNotContain(t, body, marker)
	ensure.StringDoesNotContain(t, body, "Cancel reminder")
	ensure.StringContains(t, body, hint)

	ta.subscribe(alice, "https://push.example/alice")
	ensure.DeepEqual(t, ta.snooze(alice, chat, m, "3h"), http.StatusSeeOther)
	body = ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, body, `<span `+marker+` class="msg-snooze" title="Reminder 06:04">`)
	ensure.StringContains(t, body, "Cancel reminder")
	ensure.StringContains(t, body, `action="`+messagePath(chat.ID, m.ID)+`/unsnooze"`)
	ensure.StringContains(t, body, `value="tomorrow"`) // the choices stay
	ensure.StringDoesNotContain(t, body, hint)

	// nobody else sees it
	for _, u := range []*testUser{bob, carol} {
		body = ta.get(chatPath(chat.ID), ta.as(u)).Body.String()
		ensure.StringDoesNotContain(t, body, marker)
		ensure.StringDoesNotContain(t, body, "Cancel reminder")
		ensure.StringDoesNotContain(t, body, "Reminder 06:04")
	}
	// an admin viewing without being a member gets no snooze section
	boss := ta.newUser("boss", "admin")
	ensure.StringDoesNotContain(t, ta.get(chatPath(chat.ID), ta.as(boss)).Body.String(), "-remind")
}

func TestSnoozeFiring(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "Team", bob)
	m := ta.newMessage(chat, bob, "remember this")
	ta.sends() // the message's own push is done before anyone subscribes
	ta.subscribe(alice, "https://push.example/alice1")
	ta.subscribe(alice, "https://push.example/alice2")
	ta.subscribe(bob, "https://push.example/bob")
	ta.sends()
	ensure.DeepEqual(t, ta.snooze(alice, chat, m, "1h"), http.StatusSeeOther)

	// not yet due
	ta.app.now = func() time.Time { return testNow.Add(59 * time.Minute) }
	ta.app.snoozeRound(ctx)
	ensure.DeepEqual(t, len(ta.sends()), 0)
	ensure.DeepEqual(t, len(ta.snoozeRows()), 1)

	// due, even while she is looking at the chat
	srv := ta.server()
	ta.stream(srv, alice, "?chat="+idStr(chat.ID))
	ta.app.now = func() time.Time { return testNow.Add(time.Hour) }
	ta.app.snoozeRound(ctx)
	sent := ta.sends()
	ensure.SameElements(t, endpoints(sent), []string{"https://push.example/alice1", "https://push.example/alice2"})
	ensure.DeepEqual(t, sent[0].Payload, pushPayload{
		Title: "Reminder · Team",
		Body:  "bob: remember this",
		Tag:   fmt.Sprintf("snooze-%d", m.ID),
		Data:  pushPayloadData{URL: fmt.Sprintf("%s?m=%d", chatPath(chat.ID), m.ID)},
	})
	ensure.DeepEqual(t, len(ta.snoozeRows()), 0)
	us, _ := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, us.PushesSent, int64(2))

	// it fires once
	ta.app.snoozeRound(ctx)
	ensure.DeepEqual(t, len(ta.sends()), 0)
}

func TestSnoozeDropped(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "Team", bob)
	m1 := ta.newMessage(chat, alice, "one")
	m2 := ta.newMessage(chat, alice, "two")
	ta.subscribe(bob, "https://push.example/bob")
	ta.sends()
	later := func() time.Time { return testNow.Add(2 * time.Hour) }

	// removed from the chat before it fires
	ensure.DeepEqual(t, ta.snooze(bob, chat, m1, "1h"), http.StatusSeeOther)
	ta.post(chatPath(chat.ID)+"/members/remove", url.Values{"user": {idStr(bob.ID)}}, ta.as(alice))
	ta.app.now = later
	ta.app.snoozeRound(ctx)
	ensure.DeepEqual(t, len(ta.sends()), 0)
	ensure.DeepEqual(t, len(ta.snoozeRows()), 0)

	// the message was deleted
	ta.app.now = func() time.Time { return testNow }
	ta.post(chatPath(chat.ID)+"/members/add", url.Values{"user": {idStr(bob.ID)}}, ta.as(alice))
	ensure.DeepEqual(t, ta.snooze(bob, chat, m2, "1h"), http.StatusSeeOther)
	ensure.DeepEqual(t, len(ta.snoozeRows()), 1)
	ensure.DeepEqual(t, ta.post(messagePath(chat.ID, m2.ID)+"/delete", nil, ta.as(alice)).Code, http.StatusSeeOther)
	ensure.DeepEqual(t, len(ta.snoozeRows()), 0)
	ta.app.now = later
	ta.app.snoozeRound(ctx)
	ensure.DeepEqual(t, len(ta.sends()), 0)
}

func TestSnoozeMovedBeforeFiring(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	chat := ta.newChat(alice, "Team")
	m := ta.newMessage(chat, alice, "one")
	ta.subscribe(alice, "https://push.example/alice")
	ensure.DeepEqual(t, ta.snooze(alice, chat, m, "1h"), http.StatusSeeOther)

	ta.app.now = func() time.Time { return testNow.Add(time.Hour) }
	due, err := ta.app.q.SnoozesDue(ctx, ta.app.now().Unix())
	ensure.Nil(t, err)
	ensure.DeepEqual(t, len(due), 1)
	// moved to later in between
	ensure.DeepEqual(t, ta.snooze(alice, chat, m, "3h"), http.StatusSeeOther)
	ensure.Nil(t, ta.app.fireSnooze(ctx, due[0]))
	ensure.DeepEqual(t, len(ta.sends()), 0)
	rows := ta.snoozeRows()
	ensure.DeepEqual(t, len(rows), 1)
	ensure.DeepEqual(t, rows[0].DueAt, testNow.Add(4*time.Hour).Unix())
}

func TestSnoozeDMPayload(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	bob := ta.newUser("bob")
	dm := ta.newDM(alice, bob)
	m := ta.newMessage(dm, alice, strings.Repeat("x", 300))
	ta.subscribe(alice, "https://push.example/alice")
	ta.sends()
	ensure.DeepEqual(t, ta.snooze(alice, dm, m, "1h"), http.StatusSeeOther)
	ta.app.now = func() time.Time { return testNow.Add(time.Hour) }
	ta.app.snoozeRound(t.Context())
	sent := ta.sends()
	ensure.DeepEqual(t, len(sent), 1)
	ensure.DeepEqual(t, sent[0].Payload.Title, "Reminder · bob")
	ensure.DeepEqual(t, sent[0].Payload.Body, "alice: "+strings.Repeat("x", 120))
	ensure.DeepEqual(t, sent[0].Payload.Data.URL, fmt.Sprintf("%s?m=%d", chatPath(dm.ID), m.ID))
}

func TestJumpToMessage(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	carol := ta.newUser("carol")
	chat := ta.newChat(alice, "Team")
	var msgs []Message
	for i := range 150 {
		msgs = append(msgs, ta.newMessage(chat, alice, fmt.Sprintf("m %d", i)))
	}
	jump := func(m string) *http.Response {
		return ta.get(chatPath(chat.ID)+"?m="+m, ta.as(alice)).Result()
	}
	old := msgs[9] // 141 messages from it to the newest
	res := jump(idStr(old.ID))
	ensure.DeepEqual(t, res.StatusCode, http.StatusSeeOther)
	ensure.DeepEqual(t, res.Header.Get("Location"), fmt.Sprintf("%s?n=141#m%d", chatPath(chat.ID), old.ID))
	body := ta.get(fmt.Sprintf("%s?n=141", chatPath(chat.ID)), ta.as(alice)).Body.String()
	ensure.StringContains(t, body, fmt.Sprintf(`<article id="m%d"`, old.ID))

	recent := msgs[140]
	ensure.DeepEqual(t, jump(idStr(recent.ID)).Header.Get("Location"),
		fmt.Sprintf("%s?n=100#m%d", chatPath(chat.ID), recent.ID))

	ensure.Nil(t, ta.app.deleteMessage(t.Context(), chat, recent))
	ensure.DeepEqual(t, jump(idStr(recent.ID)).Header.Get("Location"), chatPath(chat.ID))
	ensure.DeepEqual(t, jump("x").Header.Get("Location"), chatPath(chat.ID))
	// a message of another chat is not looked up there
	other := ta.newChat(alice, "Other")
	om := ta.newMessage(other, alice, "o")
	ensure.DeepEqual(t, jump(idStr(om.ID)).Header.Get("Location"), chatPath(chat.ID))
	// someone who cannot see the chat gets 404, not a redirect
	ensure.DeepEqual(t, ta.get(chatPath(chat.ID)+"?m="+idStr(old.ID), ta.as(carol)).Code, http.StatusNotFound)
}

// A push endpoint is a URL supplied by a user. The real sender must refuse
// to connect to private addresses, like the link preview fetcher does.
func TestPushSenderRefusesPrivateAddresses(t *testing.T) {
	ta := newTestApp(t)
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	// newTestApp installs a fake sender; put the real one back
	ensure.Nil(t, ta.app.loadVAPID(t.Context()))
	client, ok := ta.app.push.(*webpush.Client)
	ensure.True(t, ok)
	sub := &webpush.Subscription{
		Endpoint: srv.URL + "/push",
		// a valid P-256 point and auth secret, as a browser would send
		Keys: webpush.Keys{
			P256dh: "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM",
			Auth:   "tBHItJI5svbpez7KI4CCXg",
		},
	}
	err := client.Send(t.Context(), []byte(`{"title":"x"}`), sub)
	ensure.True(t, err != nil)
	ensure.StringContains(t, err.Error(), "address not allowed")
	ensure.DeepEqual(t, hits.Load(), int64(0))
}
