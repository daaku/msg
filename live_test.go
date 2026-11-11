package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/daaku/ensure"
)

func pinged(c *hubConn) bool {
	select {
	case <-c.ch:
		return true
	default:
		return false
	}
}

func TestHub(t *testing.T) {
	h := newHub()
	h.notifyUsers(1, 2) // no subscribers: must not block
	h.notifyViewers(9)

	c, cancel := h.subscribe(1, 5)
	other, cancelOther := h.subscribe(2, 0)
	defer cancelOther()

	h.notifyUsers(1)
	h.notifyUsers(1) // coalesced with the pending one
	ensure.True(t, pinged(c))
	ensure.False(t, pinged(c))
	ensure.False(t, pinged(other))

	h.notifyViewers(5)
	ensure.True(t, pinged(c))
	ensure.False(t, pinged(other))

	ensure.True(t, h.viewing(1, 5))
	ensure.False(t, h.viewing(1, 6))
	ensure.False(t, h.viewing(2, 5))
	ensure.False(t, h.viewing(3, 5))

	cancel()
	ensure.False(t, h.viewing(1, 5))
	h.notifyUsers(1)
	ensure.False(t, pinged(c))
	ensure.DeepEqual(t, len(h.conns), 1)
	cancel() // idempotent
}

func TestHubConcurrent(t *testing.T) {
	h := newHub()
	done := make(chan struct{})
	for i := range int64(8) {
		go func() {
			for range 200 {
				_, cancel := h.subscribe(i%3, i)
				h.notifyUsers(0, 1, 2)
				h.notifyViewers(i)
				h.viewing(i%3, i)
				cancel()
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
	ensure.DeepEqual(t, len(h.conns), 0)
}

// server serves the app over real HTTP. It is closed after the streams
// opened on it, which register their own cleanup later and so run first.
func (ta *testApp) server() *httptest.Server {
	srv := httptest.NewServer(ta.h)
	ta.t.Cleanup(srv.Close)
	return srv
}

// stream is an open /events connection made over real HTTP.
type stream struct {
	t      *testing.T
	lines  chan string
	done   chan struct{}
	cancel context.CancelFunc
}

func (ta *testApp) stream(srv *httptest.Server, u *testUser, query string) *stream {
	ta.t.Helper()
	ctx, cancel := context.WithCancel(ta.t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+pEvents+query, nil)
	ensure.Nil(ta.t, err)
	req.AddCookie(u.cookie)
	res, err := http.DefaultClient.Do(req)
	ensure.Nil(ta.t, err)
	ensure.DeepEqual(ta.t, res.StatusCode, http.StatusOK)
	ensure.DeepEqual(ta.t, res.Header.Get("Content-Type"), "text/event-stream")
	ensure.DeepEqual(ta.t, res.Header.Get("Cache-Control"), "no-store")
	s := &stream{t: ta.t, lines: make(chan string, 16), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.done)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				select {
				case s.lines <- line:
				case <-ctx.Done(): // nobody is reading any more
					return
				}
			}
		}
	}()
	ta.t.Cleanup(func() {
		cancel()
		<-s.done
	})
	ensure.DeepEqual(ta.t, s.next(), "retry: 3000")
	return s
}

func (s *stream) next() string {
	s.t.Helper()
	select {
	case line := <-s.lines:
		return line
	case <-time.After(2 * time.Second):
		s.t.Fatal("no event within 2s")
		return ""
	}
}

func (s *stream) quiet() {
	s.t.Helper()
	select {
	case line := <-s.lines:
		s.t.Fatalf("unexpected event %q", line)
	case <-time.After(50 * time.Millisecond):
	}
}

func (s *stream) closed() {
	s.t.Helper()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		s.t.Fatal("stream did not end within 2s")
	}
}

func TestEventsAccess(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	boss := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "Team")
	dm := ta.newDM(alice, bob)

	ensure.DeepEqual(t, ta.get(pEvents).Code, http.StatusSeeOther)
	ensure.DeepEqual(t, ta.get(pEvents+"?chat="+idStr(chat.ID), ta.as(bob)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(pEvents+"?chat=x", ta.as(alice)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(pEvents+"?chat=999", ta.as(alice)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(pEvents+"?chat="+idStr(dm.ID), ta.as(boss)).Code, http.StatusNotFound)
}

func TestEventsDelivery(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	boss := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "Team", bob)
	srv := ta.server()
	chatQ := "?chat=" + idStr(chat.ID)

	bobHome := ta.stream(srv, bob, "")
	bobChat := ta.stream(srv, bob, chatQ)
	carolHome := ta.stream(srv, carol, "")
	bossChat := ta.stream(srv, boss, chatQ) // an admin watching without being a member
	bossHome := ta.stream(srv, boss, "")
	ensure.True(t, ta.app.hub.viewing(bob.ID, chat.ID))
	ensure.False(t, ta.app.hub.viewing(carol.ID, chat.ID))

	send := ta.post(chatPath(chat.ID)+"/send", url.Values{"body": {"hi"}}, ta.as(alice))
	ensure.DeepEqual(t, send.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, bobHome.next(), "data: 1")
	ensure.DeepEqual(t, bobChat.next(), "data: 1")
	ensure.DeepEqual(t, bossChat.next(), "data: 1")
	carolHome.quiet()
	bossHome.quiet()

	// rename and membership changes ping too
	ta.post(chatPath(chat.ID)+"/rename", url.Values{"name": {"Crew"}}, ta.as(alice))
	ensure.DeepEqual(t, bobHome.next(), "data: 1")
	ensure.DeepEqual(t, bobChat.next(), "data: 1")

	ta.post(chatPath(chat.ID)+"/members/add", url.Values{"user": {idStr(carol.ID)}}, ta.as(alice))
	ensure.DeepEqual(t, carolHome.next(), "data: 1")
	ensure.DeepEqual(t, bobHome.next(), "data: 1")
	ensure.DeepEqual(t, bobChat.next(), "data: 1")

	// a removed member is told, so their open tab can leave
	ta.post(chatPath(chat.ID)+"/members/remove", url.Values{"user": {idStr(bob.ID)}}, ta.as(alice))
	ensure.DeepEqual(t, bobHome.next(), "data: 1")
	ensure.DeepEqual(t, bobChat.next(), "data: 1")
	ensure.DeepEqual(t, carolHome.next(), "data: 1")

	// the stream ends when the client goes away
	bobChat.cancel()
	bobChat.closed()
	deadline := time.Now().Add(2 * time.Second)
	for ta.app.hub.viewing(bob.ID, chat.ID) {
		ensure.True(t, time.Now().Before(deadline), "conn not removed")
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEventsEndOnShutdown(t *testing.T) {
	ta := newTestApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	ta.app.ctx = ctx
	alice := ta.newUser("alice")
	srv := ta.server()
	s := ta.stream(srv, alice, "")
	cancel()
	s.closed()
}

func TestLivePages(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	chat := ta.newChat(alice, "Team")
	msg := ta.newMessage(chat, alice, "hello")
	for _, path := range []string{
		pHome, chatPath(chat.ID), chatPath(chat.ID) + "/info", messagePath(chat.ID, msg.ID) + "/edit",
	} {
		body := ta.get(path, ta.as(alice)).Body.String()
		ensure.StringContains(t, body, `<main id="app"`, path)
		ensure.StringContains(t, body, "data-live", path)
		ensure.StringContains(t, body, "paxi.", path)
	}
	for _, path := range []string{pSettings, pSignIn} {
		body := ta.get(path, ta.as(alice)).Body.String()
		ensure.False(t, strings.Contains(body, "data-live"), path)
	}
	ensure.StringContains(t, ta.get(chatPath(chat.ID), ta.as(alice)).Body.String(),
		`data-chat="`+idStr(chat.ID)+`"`)
}
