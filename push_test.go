package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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
