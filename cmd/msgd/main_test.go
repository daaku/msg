package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// hiBob is a message as the msg server sends it. The same line is in
// TestEventsMessages in msg's live_test.go: change both or neither.
const hiBob = `{"id":2,"sent":"2026-01-02T03:04:05Z","text":"hi bob","mine":false,` +
	`"chat":{"id":1,"dm":false,"name":"Team","url":"/c/1"},` +
	`"sender":{"id":1,"name":"alice","image":"/avatar/1/9d6446efa8185a71"}}`

func TestDescribe(t *testing.T) {
	for _, c := range []struct {
		m    eventMessage
		want string
	}{
		{eventMessage{Text: "  two\n lines  "}, "two lines"},
		{eventMessage{Text: "https://x.example/a?b=c"}, "sent a link"},
		{eventMessage{Text: "see https://x.example"}, "see https://x.example"},
		{eventMessage{Text: "ftp://x.example"}, "ftp://x.example"},
		{eventMessage{Attachment: "image"}, "sent a photo"},
		{eventMessage{Attachment: "video"}, "sent a video"},
		{eventMessage{Attachment: "file"}, "sent a file"},
	} {
		if got := describe(c.m); got != c.want {
			t.Errorf("describe(%+v) = %q, want %q", c.m, got, c.want)
		}
	}
}

// fakeServer is msg's /events?messages=1 for a client with the token, and
// sends everyone else to sign in, as msg does.
type fakeServer struct {
	mu       sync.Mutex
	token    string
	renew    string         // a token to hand out once, as msg renews a session
	lastIDs  []string       // the Last-Event-ID of each stream
	streams  []chan string  // each stream's events
	pictures map[string]int // fetches by path
}

// send sends an event on the newest stream, waiting for there to be one
// after the given number.
func (f *fakeServer) send(t *testing.T, after int, ev string) {
	t.Helper()
	for i := 0; ; i++ {
		f.mu.Lock()
		n := len(f.streams)
		f.mu.Unlock()
		if n > after {
			f.streams[n-1] <- ev
			return
		}
		if i > 500 {
			t.Fatal("no stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newFakeServer(t *testing.T) (*fakeServer, *httptest.Server) {
	f := &fakeServer{token: "tok", pictures: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ck, err := r.Cookie("s")
	if err != nil || ck.Value != f.token {
		f.mu.Unlock()
		http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
		return
	}
	if f.renew != "" {
		f.token, f.renew = f.renew, ""
		http.SetCookie(w, &http.Cookie{Name: "s", Value: f.token, MaxAge: 3600})
	}
	if strings.HasPrefix(r.URL.Path, "/avatar/") || strings.Contains(r.URL.Path, "/icon/") {
		f.pictures[r.URL.Path]++
		f.mu.Unlock()
		jpeg.Encode(w, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil)
		return
	}
	if r.URL.Path != "/events" || r.URL.Query().Get("messages") != "1" {
		f.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	f.lastIDs = append(f.lastIDs, r.Header.Get("Last-Event-ID"))
	events := make(chan string, 8)
	f.streams = append(f.streams, events)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "retry: 3000\n\n")
	w.(http.Flusher).Flush()
	for {
		select {
		case l := <-events:
			fmt.Fprint(w, l)
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// event is a message event with the fields of hiBob replaced.
func event(id int64, replace ...string) string {
	data := strings.Replace(hiBob, `"id":2,`, fmt.Sprintf(`"id":%d,`, id), 1)
	for i := 0; i < len(replace); i += 2 {
		data = strings.Replace(data, replace[i], replace[i+1], 1)
	}
	return fmt.Sprintf("id: %d\ndata: %s\n\n", id, data)
}

type recorder struct {
	mu      sync.Mutex
	added   []note
	cleared []int64
	seen    chan struct{}
}

func newRecorder() *recorder { return &recorder{seen: make(chan struct{}, 16)} }

func (r *recorder) events() Events {
	return Events{
		Add: func(n note) {
			r.mu.Lock()
			r.added = append(r.added, n)
			r.mu.Unlock()
			r.seen <- struct{}{}
		},
		Clear: func(c int64) {
			r.mu.Lock()
			r.cleared = append(r.cleared, c)
			r.mu.Unlock()
			r.seen <- struct{}{}
		},
	}
}

func (r *recorder) wait(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-r.seen:
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
	}
}

func newTestClient(t *testing.T, srv *httptest.Server, st *state) (*client, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "msgd", "state.json")
	st.URL, st.Token = srv.URL, "tok"
	c, err := newClient(path, st)
	if err != nil {
		t.Fatal(err)
	}
	return c, path
}

func TestStream(t *testing.T) {
	f, srv := newFakeServer(t)
	f.renew = "tok2"
	c, path := newTestClient(t, srv, &state{})
	rec := newRecorder()
	w := newWatcher(c, rec.events())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.stream(ctx) }()

	f.send(t, 0, event(2))
	f.send(t, 0, ": hb\n\n")
	f.send(t, 0, event(3, `"text":"hi bob"`, `"text":"https://example.com"`))
	f.send(t, 0, event(4, `"text":"hi bob"`, `"text":"","attachment":"video"`,
		`"dm":false,"name":"Team"`, `"dm":true,"name":""`, `"id":1,"name":"alice"`, `"id":1,"name":"bob"`))
	f.send(t, 0, event(5, `"mine":false`, `"mine":true`))
	rec.wait(t, 4)
	cancel()
	<-done

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var got []string
	for _, n := range rec.added {
		got = append(got, fmt.Sprintf("%d %q group=%v pic=%v %s %s: %s", n.Chat, n.Title, n.Group, n.Image != nil,
			strings.TrimPrefix(n.URL, srv.URL), n.Lines[0].Sender, n.Lines[0].Text))
	}
	want := []string{
		`1 "Team" group=true pic=false /c/1?m=2 alice: hi bob`,
		`1 "Team" group=true pic=false /c/1?m=3 alice: sent a link`,
		`1 "bob" group=false pic=true /c/1?m=4 bob: sent a video`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(rec.cleared) != 1 || rec.cleared[0] != 1 {
		t.Fatalf("cleared %v", rec.cleared)
	}
	if !rec.added[0].Latest.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("latest %v", rec.added[0].Latest)
	}
	// the picture was fetched once; the state has the renewed token and
	// the last message
	if len(f.pictures) != 1 || f.pictures["/avatar/1/9d6446efa8185a71"] != 1 {
		t.Fatalf("pictures %v", f.pictures)
	}
	st, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Token != "tok2" || st.LastEventID != 5 {
		t.Fatalf("state %+v", st)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file %v %v", fi.Mode(), err)
	}
}

func TestReconnectResumes(t *testing.T) {
	f, srv := newFakeServer(t)
	c, _ := newTestClient(t, srv, &state{})
	rec := newRecorder()
	w := newWatcher(c, rec.events())
	reconnect := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.run(ctx, reconnect) }()

	f.send(t, 0, event(7))
	rec.wait(t, 1)
	reconnect <- struct{}{} // as after a resume
	f.send(t, 1, event(8))
	rec.wait(t, 1)
	f.mu.Lock()
	ids := slices.Clone(f.lastIDs)
	f.token = "other" // the session ends
	f.mu.Unlock()
	if len(ids) != 2 || ids[0] != "" || ids[1] != "7" {
		t.Fatalf("Last-Event-IDs %q", ids)
	}
	reconnect <- struct{}{}
	select {
	case err := <-done:
		if !errors.Is(err, errSignedOut) {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run went on signed out")
	}
	cancel()
}

func TestCardHeight(t *testing.T) {
	n := note{Lines: make([]line, 2)}
	if got := cardHeight(n); got != 2*pad+titleH+2*lineH {
		t.Fatalf("two lines: %v", got)
	}
	n.Lines = make([]line, 9)
	if l, more := shown(n); l != maxLines-1 || !more {
		t.Fatalf("shown %d %v", l, more)
	}
	if got := cardHeight(n); got != 2*pad+titleH+maxLines*lineH {
		t.Fatalf("nine lines: %v", got)
	}
}

func testNotes() []note {
	at := time.Date(2026, 11, 24, 15, 4, 0, 0, time.Local)
	group := note{Chat: 1, Title: "Weekend plans with the whole extended family", Group: true,
		URL: "https://msg.example/c/1?m=2", Latest: at}
	for i, text := range []string{"morning", "sent a photo", "sent a link",
		"who is bringing the barbecue this time? I can do drinks and ice but not both", "sent a video"} {
		group.Lines = append(group.Lines, line{ID: int64(i + 2), Sender: []string{"bob", "carol"}[i%2], Text: text})
	}
	dm := note{Chat: 2, Title: "bob", URL: "https://msg.example/c/2?m=9", Latest: at.Add(-time.Minute),
		Lines: []line{{ID: 9, Sender: "bob", Text: "hi alice"}}}
	return []note{dm, group}
}

func TestView(t *testing.T) {
	a := &app{}
	for _, n := range testNotes() {
		a.add(n)
	}
	if a.notes[0].Chat != 1 {
		t.Fatalf("newest first: %+v", a.notes)
	}
	tt := ui.NewTester(a.view, width, a.height())
	for _, want := range []string{"2 chats", "Dismiss all", "bob", "hi alice", "15:04",
		"bob: sent a video", "2 earlier"} {
		if !tt.HasText(want) {
			t.Errorf("no %q in %q", want, tt.Texts())
		}
	}
	if tt.HasText("bob: morning") {
		t.Errorf("an earlier line shows: %q", tt.Texts())
	}
	if dir := os.Getenv("MSGD_SNAPSHOT"); dir != "" {
		var b bytes.Buffer
		if err := png.Encode(&b, tt.Image()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "view.png"), b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// more of a chat adds to its card, which goes to the top
	a.add(note{Chat: 2, Title: "bob", URL: "https://msg.example/c/2?m=12", Lines: []line{{ID: 12, Text: "you there?"}}})
	tt.Frame()
	if a.notes[0].Chat != 2 || len(a.notes[0].Lines) != 2 || a.notes[0].URL != "https://msg.example/c/2?m=9" {
		t.Fatalf("notes %+v", a.notes)
	}

	// a click opens the chat and drops its card
	if err := tt.Click("hi alice"); err != nil {
		t.Fatal(err)
	}
	if got := tt.OpenedURLs(); len(got) != 1 || got[0] != "https://msg.example/c/2?m=9" {
		t.Fatalf("opened %q", got)
	}
	if len(a.notes) != 1 || a.notes[0].Chat != 1 {
		t.Fatalf("notes %+v", a.notes)
	}

	// dismiss drops them all
	a.add(testNotes()[0])
	tt.Frame()
	if err := tt.Click("Dismiss all"); err != nil {
		t.Fatal(err)
	}
	if len(a.notes) != 0 {
		t.Fatalf("notes %+v", a.notes)
	}
	a.notes = testNotes()
	tt.Frame()
	tt.Key(0, ui.KeyEscape)
	if len(a.notes) != 0 {
		t.Fatalf("escape left %+v", a.notes)
	}
}
