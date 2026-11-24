// Msgd shows the new messages of a msg server in a window of its own, in
// the top right corner of the screen, that stays until they are opened or
// dismissed. It holds the server's event stream open, which sends each new
// message as JSON.
//
//	msgd login https://msg.example.com   # paste the token from Settings
//	msgd                                 # run, as the systemd user unit does
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// eventMessage is a new message as /events?messages=1 sends it.
type eventMessage struct {
	ID         int64     `json:"id"`
	Sent       time.Time `json:"sent"`
	Text       string    `json:"text"`
	Attachment string    `json:"attachment"` // "image", "video", "file", or ""
	Mine       bool      `json:"mine"`
	Chat       struct {
		ID    int64  `json:"id"`
		DM    bool   `json:"dm"`
		Name  string `json:"name"`
		URL   string `json:"url"`
		Image string `json:"image"`
	} `json:"chat"`
	Sender struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Image string `json:"image"`
	} `json:"sender"`
}

// describe is the line a notification shows for a message: its text, or
// what it sent where that is no text to read.
func describe(m eventMessage) string {
	switch m.Attachment {
	case "image":
		return "sent a photo"
	case "video":
		return "sent a video"
	case "file":
		return "sent a file"
	}
	text := strings.TrimSpace(m.Text)
	if u, err := url.Parse(text); err == nil && (u.Scheme == "http" || u.Scheme == "https") &&
		u.Host != "" && !strings.ContainsAny(text, " \n\t") {
		return "sent a link"
	}
	return strings.Join(strings.Fields(text), " ")
}

// state

// state is what msgd keeps between runs, in a file only the user can read:
// it holds the session token.
type state struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// LastEventID is the id of the last message the stream sent, which a
	// reconnect gives back to get those sent since.
	LastEventID int64 `json:"lastEventID"`
}

func statePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "msgd", "state.json"), nil
}

func loadState(path string) (*state, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func saveState(path string, s *state) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// client

var errSignedOut = errors.New("signed out: create a new token in Settings and run msgd login")

// client talks to the server as the browser does, with the session cookie,
// and keeps the cookie the server renews.
type client struct {
	base   *url.URL
	path   string       // of the state file
	pics   *http.Client // with a timeout
	stream *http.Client // without one

	mu sync.Mutex
	st *state
}

func newClient(path string, st *state) (*client, error) {
	base, err := url.Parse(st.URL)
	if err != nil {
		return nil, err
	}
	// A redirect is the server sending a signed out client to sign in.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{
		base:   base,
		path:   path,
		st:     st,
		pics:   &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect},
		stream: &http.Client{CheckRedirect: noRedirect},
	}, nil
}

func (c *client) abs(path string) string {
	u, err := c.base.Parse(path)
	if err != nil {
		return c.base.String()
	}
	return u.String()
}

// with runs fn with the state, which it may change, and saves it after.
func (c *client) with(fn func(st *state)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c.st)
	return saveState(c.path, c.st)
}

func (c *client) lastEventID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.LastEventID
}

func (c *client) do(hc *http.Client, req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	req.AddCookie(&http.Cookie{Name: "s", Value: c.st.Token})
	c.mu.Unlock()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	for _, ck := range resp.Cookies() {
		if ck.Name != "s" {
			continue
		}
		if ck.MaxAge < 0 || ck.Value == "" {
			resp.Body.Close()
			return nil, errSignedOut
		}
		if err := c.with(func(st *state) { st.Token = ck.Value }); err != nil {
			log.Printf("save the renewed session: %v", err)
		}
	}
	if resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, errSignedOut
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: status %d", req.URL.Path, resp.StatusCode)
	}
	return resp, nil
}

// picture fetches and decodes a profile picture or a group icon.
func (c *client) picture(ctx context.Context, path string) (*ui.Bitmap, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.abs(path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(c.pics, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return ui.DecodeBitmap(data)
}

// watching

// note is what the window shows of one chat: its newest messages since the
// user last opened or dismissed it.
type note struct {
	Chat   int64
	Title  string     // the group's name, or the sender of a direct message
	Group  bool       // the lines name their senders
	Image  *ui.Bitmap // the group's icon or the sender's picture, nil for none
	URL    string     // opens the chat at the first message shown
	Lines  []line     // oldest first
	Latest time.Time
}

type line struct {
	ID     int64
	Sender string
	Text   string
}

// Events reports what the window should show. The calls come from the
// watcher's goroutine.
type Events struct {
	Add   func(n note)     // a new message in a chat, to add to what it shows
	Clear func(chat int64) // the user wrote in the chat, so has seen it
}

type watcher struct {
	c      *client
	events Events
	images map[string]*ui.Bitmap // by path: pictures never change at a path
}

func newWatcher(c *client, events Events) *watcher {
	return &watcher{c: c, events: events, images: map[string]*ui.Bitmap{}}
}

func (w *watcher) image(ctx context.Context, path string) *ui.Bitmap {
	if path == "" {
		return nil
	}
	if b, ok := w.images[path]; ok {
		return b
	}
	b, err := w.c.picture(ctx, path)
	if err != nil {
		log.Printf("picture %s: %v", path, err)
		return nil
	}
	w.images[path] = b
	return b
}

func (w *watcher) handle(ctx context.Context, m eventMessage) {
	if m.Mine {
		// Writing in a chat is reading it.
		w.events.Clear(m.Chat.ID)
		return
	}
	n := note{
		Chat:   m.Chat.ID,
		Title:  m.Chat.Name,
		Group:  !m.Chat.DM,
		URL:    w.c.abs(fmt.Sprintf("%s?m=%d", m.Chat.URL, m.ID)),
		Latest: m.Sent,
		Lines:  []line{{ID: m.ID, Sender: m.Sender.Name, Text: describe(m)}},
	}
	if n.Group {
		n.Image = w.image(ctx, m.Chat.Image)
	} else {
		n.Title = m.Sender.Name
		n.Image = w.image(ctx, m.Sender.Image)
	}
	w.events.Add(n)
}

const (
	heartbeatTimeout = 70 * time.Second // the server sends one every 25s
	maxBackoff       = time.Minute
)

// run keeps the event stream open until ctx is done or the session ends.
// reconnect drops the stream at once, as after a resume, when it may be
// dead without a word.
func (w *watcher) run(ctx context.Context, reconnect <-chan struct{}) error {
	backoff := time.Second
	for {
		sctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- w.stream(sctx) }()
		var err error
		select {
		case err = <-done:
		case <-reconnect:
			backoff = time.Second
		}
		cancel()
		if errors.Is(err, errSignedOut) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.Printf("events: %v", err)
		}
		select {
		case <-time.After(backoff):
			backoff = min(2*backoff, maxBackoff)
		case <-reconnect:
			backoff = time.Second
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// stream reads /events?messages=1 until it fails. The server first sends
// what came since the last message msgd saw, then each new one.
func (w *watcher) stream(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.c.abs("/events?messages=1"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if id := w.c.lastEventID(); id > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(id, 10))
	}
	resp, err := w.c.do(w.c.stream, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	watchdog := time.AfterFunc(heartbeatTimeout, cancel)
	defer watchdog.Stop()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	var data []string
	for sc.Scan() {
		watchdog.Reset(heartbeatTimeout)
		l := sc.Text()
		if d, ok := strings.CutPrefix(l, "data:"); ok {
			data = append(data, strings.TrimPrefix(d, " "))
		}
		if l != "" || data == nil {
			continue
		}
		var m eventMessage
		err := json.Unmarshal([]byte(strings.Join(data, "\n")), &m)
		data = nil
		if err != nil {
			log.Printf("event: %v", err)
			continue
		}
		w.handle(ctx, m)
		if err := w.c.with(func(st *state) { st.LastEventID = m.ID }); err != nil {
			log.Printf("save state: %v", err)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("the stream ended")
}

// window

const (
	width      = 380
	margin     = 16
	pad        = 12
	gap        = 8
	headHeight = 36
	titleH     = 22
	lineH      = 20
	maxLines   = 4 // of a chat; the rest are counted
	avatarSize = 40
)

var (
	white  = ui.RGBA(255, 255, 255, 1)
	muted  = ui.RGBA(255, 255, 255, 0.6)
	card   = ui.RGBA(24, 24, 28, 0.97)
	hover  = ui.RGBA(40, 40, 46, 0.97)
	accent = ui.Hex("#f2a93b") // marigold, as msg's
)

type app struct {
	win      *mygo.Window
	notes    []note // in the order their chats came
	err      error  // shown until the app restarts
	retry    *time.Timer
	quitting bool
}

// shown is how many lines a note shows, and whether one more counts the
// rest.
func shown(n note) (lines int, more bool) {
	if len(n.Lines) > maxLines {
		return maxLines - 1, true
	}
	return len(n.Lines), false
}

func cardHeight(n note) float32 {
	lines, more := shown(n)
	if more {
		lines++
	}
	return 2*pad + titleH + float32(lines)*lineH
}

func (a *app) height() int {
	h := float32(headHeight)
	if a.err != nil {
		h += gap + 2*pad + 2*lineH
	}
	for _, n := range a.notes {
		h += gap + cardHeight(n)
	}
	return int(h)
}

func (a *app) add(n note) {
	i := slices.IndexFunc(a.notes, func(old note) bool { return old.Chat == n.Chat })
	if i < 0 {
		a.notes = append(a.notes, n)
	} else {
		// Keep its place and the lines it shows, and open where the user
		// stopped.
		old := a.notes[i]
		n.Lines = append(old.Lines, n.Lines...)
		n.URL = old.URL
		a.notes[i] = n
	}
	a.layout()
}

func (a *app) clearChat(chat int64) {
	a.notes = slices.DeleteFunc(a.notes, func(n note) bool { return n.Chat == chat })
	a.layout()
}

func (a *app) clearAll() {
	a.notes = nil
	a.layout()
}

func (a *app) open(c *ui.Context, n note) {
	c.OpenURLThen(n.URL, func(err error) {
		if err != nil {
			log.Printf("open %s: %v", n.URL, err)
		}
	})
	a.clearChat(n.Chat)
}

// layout sizes the window to what it shows, in the top right corner of the
// primary display, and shows it while there is something to show.
func (a *app) layout() {
	if a.win == nil {
		return // a view under test
	}
	if len(a.notes) == 0 && a.err == nil {
		a.win.Hide()
		return
	}
	area := mygo.Rectangle{Width: 1280, Height: 800}
	if ds := mygo.Screen.Displays(); len(ds) > 0 {
		area = ds[0].WorkArea
	}
	// One call: SetPosition after SetSize would ask for the size the window
	// still has, as Wayland resizes it later.
	a.win.SetBounds(mygo.Rectangle{X: area.X + area.Width - width - margin, Y: area.Y + margin,
		Width: width, Height: min(a.height(), area.Height-2*margin)})
	if !a.win.IsVisible() {
		a.win.ShowInactive() // leave the keyboard where the user is typing
	}
}

// lost hides the window whose surface the compositor closed, as displays
// sleep, and shows it again a while later should no resume come.
func (a *app) lost() {
	a.win.Hide()
	if a.retry == nil {
		a.retry = time.AfterFunc(2*time.Second, func() { mygo.RunOnMain(a.reshow) })
	} else {
		a.retry.Reset(2 * time.Second)
	}
}

func (a *app) reshow() {
	a.win.Hide()
	a.layout()
}

func (a *app) view(c *ui.Context) {
	c.Root().Background(ui.Transparent)
	if c.Shortcut(0, ui.KeyEscape) {
		a.clearAll()
	}
	ui.Column(c).Fill().Gap(gap).Children(func() {
		ui.Row(c).FillWidth().Height(headHeight).AlignItems(ui.Center).PaddingX(pad).Gap(8).
			Background(card).Radius(10).Children(func() {
			title := "1 chat"
			if len(a.notes) != 1 {
				title = fmt.Sprintf("%d chats", len(a.notes))
			}
			ui.Text(c, title).FontSize(14).Bold().TextColor(accent).Grow(1)
			if len(a.notes) > 0 && ui.Button(c, "Dismiss all").TabStop(false).Clicked() {
				a.clearAll()
			}
		})
		if a.err != nil {
			ui.Column(c).FillWidth().Height(2*pad+2*lineH).Padding(pad).Background(card).Radius(10).
				Border(1, accent).Children(func() {
				ui.Text(c, a.err.Error()).FontSize(13).TextColor(white).Wrap().MaxLines(2)
			})
		}
		ui.Scroll(c).Grow(1).Children(func() {
			ui.Column(c).FillWidth().Gap(gap).Children(func() {
				for _, n := range a.notes {
					a.card(c, n)
				}
			})
		})
	})
}

func (a *app) card(c *ui.Context, n note) {
	b := ui.ButtonBase(c).Key(n.Chat).FillWidth().Height(cardHeight(n)).Padding(pad).Gap(pad).
		AlignItems(ui.Start).Justify(ui.Start).Background(card).Radius(10).TabStop(false).
		Tooltip(n.URL).Cursor(ui.CursorPointer)
	if b.Hovered() {
		b.Background(hover)
	}
	b.Children(func() {
		ui.Avatar(c, n.Title, n.Image).Size(avatarSize, avatarSize).Shrink(0)
		ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Children(func() {
			ui.Row(c).FillWidth().Height(titleH).AlignItems(ui.Center).Gap(8).Children(func() {
				ui.Text(c, n.Title).FontSize(15).Bold().TextColor(white).MaxLines(1).Ellipsis("…").
					Grow(1).Shrink(1).MinWidth(0)
				ui.Text(c, n.Latest.Local().Format("15:04")).FontSize(12).TextColor(muted).Shrink(0)
			})
			count, more := shown(n)
			if more {
				ui.Text(c, fmt.Sprintf("%d earlier", len(n.Lines)-count)).FontSize(12).
					TextColor(muted).Height(lineH)
			}
			for _, l := range n.Lines[len(n.Lines)-count:] {
				text := l.Text
				if n.Group {
					text = l.Sender + ": " + text
				}
				ui.Text(c, text).Key(l.ID).FontSize(14).TextColor(white).Height(lineH).
					MaxLines(1).Ellipsis("…").FillWidth()
			}
		})
	})
	if b.Clicked() {
		a.open(c, n)
	}
}

// commands

func login(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: msgd login https://msg.example.com")
	}
	base, err := url.Parse(strings.TrimRight(args[0], "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return fmt.Errorf("%q is not the server's URL", args[0])
	}
	fmt.Fprint(os.Stderr, "Create a token under Settings, Desktop app, and paste it here: ")
	tok, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && tok == "" {
		return err
	}
	path, err := statePath()
	if err != nil {
		return err
	}
	// A new state starts with no last event: the first run starts quiet.
	c, err := newClient(path, &state{URL: base.String(), Token: strings.TrimSpace(tok)})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.abs("/events?messages=1"), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(c.pics, req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if err := c.with(func(*state) {}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Signed in. State is in %s\n", path)
	return nil
}

func main() {
	log.SetFlags(0)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			if err := login(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		default:
			log.Fatalf("usage: msgd [login URL]")
		}
	}
	path, err := statePath()
	if err != nil {
		log.Fatal(err)
	}
	st, err := loadState(path)
	if errors.Is(err, os.ErrNotExist) {
		log.Print("not signed in: run msgd login URL")
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
	c, err := newClient(path, st)
	if err != nil {
		log.Fatal(err)
	}

	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory) // no Dock icon (macOS)
	mygo.App.OnWindowAllClosed(func() {})                        // a daemon: keep running
	mygo.App.WhenReady(func() {
		a := &app{}
		a.win = mygo.NewWindow(mygo.WindowOptions{
			Title: "msgd",
			// An always on top, frameless window out of the taskbar is an
			// overlay: on Wayland a layer surface that takes the keyboard
			// only when clicked, so a new message never steals it.
			AlwaysOnTop:   true,
			SkipTaskbar:   true,
			Frameless:     true,
			Transparent:   true,
			DisableResize: true,
			Hidden:        true,
			Width:         width,
			Height:        headHeight,
			Content:       ui.View(a.view),
		})
		// The compositor closes a layer surface whose output goes away, as
		// displays sleep. Keep the notes and show them again.
		a.win.OnClose(func(e *mygo.CloseEvent) {
			if a.quitting {
				return
			}
			e.PreventDefault()
			a.lost()
		})
		mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { a.quitting = true })
		reconnect := make(chan struct{}, 1)
		mygo.Power.OnResume(func() {
			a.reshow()
			select {
			case reconnect <- struct{}{}:
			default:
			}
		})
		mygo.Screen.OnDisplaysChanged(a.reshow)
		w := newWatcher(c, Events{
			Add:   func(n note) { a.win.Update(func() { a.add(n) }) },
			Clear: func(chat int64) { a.win.Update(func() { a.clearChat(chat) }) },
		})
		go func() {
			err := w.run(context.Background(), reconnect)
			log.Print(err)
			a.win.Update(func() { a.err = err; a.layout() })
			if errors.Is(err, errSignedOut) {
				// Keep showing it: a restart would not sign in again.
				return
			}
			os.Exit(1) // systemd restarts us
		}()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
