package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daaku/ensure"
)

func allowAll(netip.AddrPort) bool { return true }

// previewSetup is an app whose preview fetcher may reach local test servers.
func previewSetup(t *testing.T) (*testApp, *testUser, Chat) {
	t.Helper()
	ta := newTestApp(t)
	ta.app.previewAllow = allowAll
	alice := ta.newUser("alice", "group")
	return ta, alice, ta.newChat(alice, "Team")
}

func (ta *testApp) runPreviews() {
	ta.t.Helper()
	for ta.app.previewOnce(ta.t.Context()) {
	}
}

func (ta *testApp) preview(msgID int64) Preview {
	ta.t.Helper()
	p, err := ta.app.q.PreviewByMessage(ta.t.Context(), msgID)
	ensure.Nil(ta.t, err)
	return p
}

// send posts a text message through the handler and returns it.
func (ta *testApp) send(u *testUser, chat Chat, body string) Message {
	ta.t.Helper()
	w := ta.post(chatPath(chat.ID)+"/send", url.Values{"body": {body}}, ta.as(u))
	ensure.DeepEqual(ta.t, w.Code, http.StatusSeeOther)
	var id int64
	ensure.Nil(ta.t, ta.app.db.QueryRowContext(ta.t.Context(),
		"SELECT max(id) FROM messages WHERE chat_id = ?", chat.ID).Scan(&id))
	m, err := ta.app.q.MessageByID(ta.t.Context(), id)
	ensure.Nil(ta.t, err)
	return m
}

func htmlPage(head string) string {
	return "<!doctype html><html><head>" + head + "</head><body><p>hello</p></body></html>"
}

func serveHTML(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPublicAddr(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "127.8.9.10", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "100.127.255.255", "0.0.0.0", "0.1.2.3",
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.19.255.255", "198.51.100.7", "203.0.113.9",
		"224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"::", "::1", "fe80::1", "fe80::1%eth0", "fc00::1", "fd12:3456::1", "ff02::1", "ff01::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::7f00:1", "64:ff9b::a00:1", "2001:db8::1", "2002:7f00:1::1",
	} {
		ensure.False(t, publicAddr(netip.MustParseAddr(s)), s)
	}
	for _, s := range []string{
		"1.1.1.1", "8.8.8.8", "93.184.216.34", "172.32.0.1", "100.63.255.255", "100.128.0.1",
		"2606:4700:4700::1111", "2a00:1450:4001::1", "::ffff:1.1.1.1",
	} {
		ensure.True(t, publicAddr(netip.MustParseAddr(s)), s)
	}
	ensure.False(t, publicAddr(netip.Addr{}))

	// only the web ports
	ensure.True(t, previewAllowStrict(netip.MustParseAddrPort("1.1.1.1:443")))
	ensure.True(t, previewAllowStrict(netip.MustParseAddrPort("1.1.1.1:80")))
	for _, s := range []string{"1.1.1.1:8080", "1.1.1.1:22", "1.1.1.1:6379", "127.0.0.1:80", "[::1]:443"} {
		ensure.False(t, previewAllowStrict(netip.MustParseAddrPort(s)), s)
	}
}

func TestCheckPreviewURL(t *testing.T) {
	for _, s := range []string{"http://example.com", "https://example.com/a?b=c#d", "https://[2606:4700::1]/x"} {
		_, err := checkPreviewURL(s)
		ensure.Nil(t, err, s)
	}
	for _, s := range []string{
		"ftp://example.com/x", "javascript:alert(1)", "file:///etc/passwd", "gopher://x/",
		"https://user:pass@example.com/", "https://user@example.com/", "http:///path", "https://",
		"//example.com/x", "example.com", "", "http://%zz/",
		"https://example.com/" + strings.Repeat("a", 2000),
	} {
		_, err := checkPreviewURL(s)
		ensure.True(t, err != nil, s)
	}
}

func TestPreviewClientBlocksPrivateAddresses(t *testing.T) {
	ta := newTestApp(t) // strict: the default
	ctx := t.Context()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, htmlPage("<title>internal admin</title>"))
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(interface{ String() string }).String()
	port = port[strings.LastIndex(port, ":"):]

	for _, target := range []string{
		srv.URL,                   // 127.0.0.1 literal
		"http://localhost" + port, // a name that resolves to loopback
		"http://[::1]" + port,
		"http://[::ffff:127.0.0.1]" + port,
		"http://0.0.0.0" + port,
		"http://127.1" + port,
		"http://2130706433" + port, // 127.0.0.1 as a decimal number
	} {
		_, err := ta.app.fetchPreview(ctx, target)
		ensure.True(t, err != nil, target)
	}
	ensure.DeepEqual(t, hits.Load(), int64(0), "the server must see no request at all")

	// the same server is reachable once the check is opened, so the
	// failures above were the check and nothing else
	ta.app.previewAllow = allowAll
	d, err := ta.app.fetchPreview(ctx, srv.URL)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, d.Title, "internal admin")

	// the allow function sees the real address and port being dialled
	var seen []netip.AddrPort
	ta.app.previewAllow = func(ap netip.AddrPort) bool { seen = append(seen, ap); return false }
	_, err = ta.app.fetchPreview(ctx, "http://localhost"+port)
	ensure.True(t, err != nil)
	ensure.True(t, len(seen) > 0)
	for _, ap := range seen {
		ensure.True(t, ap.Addr().IsLoopback(), ap)
		ensure.DeepEqual(t, ":"+fmt.Sprint(ap.Port()), port)
	}
}

func TestPreviewRedirects(t *testing.T) {
	ta, _, _ := previewSetup(t)
	ctx := t.Context()
	var internalHits atomic.Int64
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits.Add(1)
	}))
	defer internal.Close()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/ok":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, htmlPage(`<title>landed</title><meta property="og:image" content="img.png">`))
		case strings.HasPrefix(r.URL.Path, "/hop/"):
			var n int
			fmt.Sscanf(r.URL.Path, "/hop/%d", &n)
			if n == 0 {
				http.Redirect(w, r, "/ok", http.StatusFound)
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
		case r.URL.Path == "/file":
			w.Header().Set("Location", "file:///etc/passwd")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/gopher":
			w.Header().Set("Location", "gopher://127.0.0.1:70/x")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/userinfo":
			w.Header().Set("Location", "http://user:pw@"+r.Host+"/ok")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/internal":
			http.Redirect(w, r, internal.URL+"/latest/meta-data/", http.StatusFound)
		}
	}))
	defer srv.Close()

	d, err := ta.app.fetchPreview(ctx, srv.URL+"/hop/3")
	ensure.Nil(t, err)
	ensure.DeepEqual(t, d.Title, "landed")
	// a relative image resolves against the final URL, not the first
	ensure.DeepEqual(t, d.ImageURL, srv.URL+"/img.png")

	_, err = ta.app.fetchPreview(ctx, srv.URL+"/hop/5") // six requests
	ensure.StringContains(t, err.Error(), "too many redirects")
	for _, path := range []string{"/file", "/gopher", "/userinfo"} {
		_, err = ta.app.fetchPreview(ctx, srv.URL+path)
		ensure.True(t, err != nil, path)
	}

	// a redirect to a blocked address is refused at connect time
	internalAddr := internal.Listener.Addr().String()
	srvAddr := srv.Listener.Addr().String()
	ta.app.previewAllow = func(ap netip.AddrPort) bool { return ap.String() == srvAddr }
	_, err = ta.app.fetchPreview(ctx, srv.URL+"/internal")
	ensure.True(t, err != nil)
	ensure.DeepEqual(t, internalHits.Load(), int64(0), internalAddr)
}

func TestParsePreview(t *testing.T) {
	final, _ := url.Parse("https://www.example.com/blog/post?x=1")
	cases := []struct {
		name, head string
		want       previewData
	}{
		{"open graph", `<title>T</title>
			<meta property="og:title" content="OG title">
			<meta property="og:description" content="OG desc">
			<meta property="og:image" content="https://cdn.example.com/a.png">
			<meta property="og:site_name" content="Example Site">
			<meta name="description" content="plain desc">`,
			previewData{"OG title", "OG desc", "Example Site", "https://cdn.example.com/a.png"}},
		{"twitter only", `<meta name="twitter:title" content="TW title">
			<meta name="twitter:description" content="TW desc">
			<meta name="twitter:image" content="/img/b.jpg">`,
			previewData{"TW title", "TW desc", "example.com", "https://www.example.com/img/b.jpg"}},
		{"title only", `<title>Just a title</title>`,
			previewData{"Just a title", "", "example.com", ""}},
		{"meta description fallback", `<title>T</title><meta name="description" content="about this">`,
			previewData{"T", "about this", "example.com", ""}},
		{"entities and whitespace", "<title>\n  Tom &amp; Jerry &lt;3\n\t &quot;cats&quot;  </title>",
			previewData{`Tom & Jerry <3 "cats"`, "", "example.com", ""}},
		{"first value wins and empty is skipped", `<meta property="og:title" content=" ">
			<meta property="og:title" content="first"><meta property="og:title" content="second">`,
			previewData{"first", "", "example.com", ""}},
		{"image with a bad scheme is dropped", `<title>T</title><meta property="og:image" content="javascript:alert(1)">`,
			previewData{"T", "", "example.com", ""}},
		{"image with credentials is dropped", `<title>T</title><meta property="og:image" content="https://u:p@x.example/a.png">`,
			previewData{"T", "", "example.com", ""}},
		{"nothing", ``, previewData{"", "", "example.com", ""}},
	}
	for _, c := range cases {
		got := parsePreview(strings.NewReader(htmlPage(c.head)), final)
		ensure.DeepEqual(t, got, c.want, c.name)
	}

	// tags after the head are ignored
	got := parsePreview(strings.NewReader(
		`<html><head><title>real</title></head><body><meta property="og:title" content="fake"><title>fake</title></body></html>`), final)
	ensure.DeepEqual(t, got.Title, "real")
	got = parsePreview(strings.NewReader(`<html><body><meta property="og:title" content="fake"></body></html>`), final)
	ensure.DeepEqual(t, got.Title, "")

	// long values are cut
	got = parsePreview(strings.NewReader(htmlPage(`<title>`+strings.Repeat("é", 500)+`</title>
		<meta name="description" content="`+strings.Repeat("d", 500)+`">`)), final)
	ensure.DeepEqual(t, got.Title, strings.Repeat("é", 200))
	ensure.DeepEqual(t, len(got.Description), 300)
}

func TestFetchPreviewResponses(t *testing.T) {
	ta, _, _ := previewSetup(t)
	ctx := t.Context()
	serve := func(status int, contentType, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ensure.DeepEqual(t, r.Header.Get("User-Agent"), previewUserAgent)
			ensure.DeepEqual(t, r.Header.Get("Cookie"), "")
			ensure.DeepEqual(t, r.Header.Get("Authorization"), "")
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	page := htmlPage("<title>ok</title>")

	d, err := ta.app.fetchPreview(ctx, serve(200, "text/html; charset=utf-8", page))
	ensure.Nil(t, err)
	ensure.DeepEqual(t, d.Title, "ok")
	_, err = ta.app.fetchPreview(ctx, serve(200, "application/xhtml+xml", page))
	ensure.Nil(t, err)

	for name, u := range map[string]string{
		"json":      serve(200, "application/json", page),
		"image":     serve(200, "image/png", page),
		"no type":   serve(200, "", page),
		"500":       serve(500, "text/html", page),
		"404":       serve(404, "text/html", page),
		"bad url":   "ftp://example.com/",
		"no server": "http://127.0.0.1:1/",
	} {
		_, err := ta.app.fetchPreview(ctx, u)
		ensure.True(t, err != nil, name)
	}

	// a page in another encoding is decoded
	d, err = ta.app.fetchPreview(ctx, serve(200, "text/html; charset=iso-8859-1", htmlPage("<title>caf\xe9</title>")))
	ensure.Nil(t, err)
	ensure.DeepEqual(t, d.Title, "café")

	// only the first megabyte is read
	big := "<html><head>" + strings.Repeat("<!-- padding -->", 140000) + "<title>too late</title></head></html>"
	ensure.True(t, len(big) > 2<<20)
	d, err = ta.app.fetchPreview(ctx, serve(200, "text/html", big))
	ensure.Nil(t, err)
	ensure.DeepEqual(t, d.Title, "")
}

func TestPreviewEndToEnd(t *testing.T) {
	needMagick(t)
	ta, alice, chat := previewSetup(t)
	ctx := t.Context()
	var pageHits, imageHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/post":
			pageHits.Add(1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, htmlPage(`<title>x</title>
				<meta property="og:title" content="A &lt;script&gt;alert(1)&lt;/script&gt; title">
				<meta property="og:description" content="Some words">
				<meta property="og:site_name" content="The Site">
				<meta property="og:image" content="/pic">`))
		case "/pic":
			imageHits.Add(1)
			w.Header().Set("Content-Type", "text/plain") // wrong on purpose: the bytes decide
			w.Write(pngOf(t, 2400, 1200))
		case "/other":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, htmlPage(`<title>Other page</title>`))
		}
	}))
	defer srv.Close()
	link := srv.URL + "/post"

	c, _ := ta.app.hub.subscribe(alice.ID, chat.ID)
	m := ta.send(alice, chat, "look at "+link+" and "+srv.URL+"/other please")
	ensure.DeepEqual(t, ta.preview(m.ID).Status, "pending")
	ensure.DeepEqual(t, ta.preview(m.ID).Url, link, "only the first URL")
	pinged(c)
	page := ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.False(t, strings.Contains(page, "-preview"), "a pending preview renders nothing")

	ta.runPreviews()

	p := ta.preview(m.ID)
	ensure.DeepEqual(t, p.Status, "ready")
	ensure.DeepEqual(t, p.Title, "A <script>alert(1)</script> title")
	ensure.DeepEqual(t, p.Description, "Some words")
	ensure.DeepEqual(t, p.SiteName, "The Site")
	ensure.True(t, p.FileID.Valid)
	ensure.DeepEqual(t, []int64{pageHits.Load(), imageHits.Load()}, []int64{1, 1})
	ensure.True(t, pinged(c), "the chat is pinged when the preview is ready")

	f := ta.file(p.FileID.Int64)
	ensure.DeepEqual(t, f.Kind, kindPreview)
	ensure.DeepEqual(t, f.Status, statusReady)
	ensure.DeepEqual(t, []int64{f.Width, f.Height, f.Size}, []int64{1200, 600, 0})
	ensure.DeepEqual(t, f.ChatID, chat.ID)
	ensure.DeepEqual(t, f.ConvType, "image/webp")

	page = ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	id := idStr(m.ID)
	ensure.StringContains(t, page, `<a id="m`+id+`-preview" class="preview" href="`+link+
		`" target="_blank" rel="noopener noreferrer nofollow">`)
	ensure.StringContains(t, page, `<img id="f`+idStr(f.ID)+`" src="/f/`+idStr(f.ID)+
		`/c" width="1200" height="600" alt="" loading="lazy" decoding="async">`)
	ensure.StringContains(t, page, "A &lt;script&gt;alert(1)&lt;/script&gt; title")
	ensure.False(t, strings.Contains(page, "<script>alert(1)"), "fetched text is escaped")
	ensure.StringContains(t, page, "The Site")

	// the image is served like any file, and only its converted variant
	ensure.DeepEqual(t, ta.get(filePath(f.ID, "c"), ta.as(alice)).Code, http.StatusFound)
	ensure.DeepEqual(t, ta.get(filePath(f.ID, "o"), ta.as(alice)).Code, http.StatusNotFound)
	// previews never count as uploads
	us, cs := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, us.ImagesUploaded+us.FilesUploaded+us.BytesUploaded, int64(0))
	ensure.DeepEqual(t, cs.ImagesUploaded+cs.FilesUploaded+cs.BytesUploaded, int64(0))

	// editing to the same URL keeps the preview and does not fetch again
	edit := func(body string) {
		t.Helper()
		w := ta.post(messagePath(chat.ID, m.ID)+"/edit", url.Values{"body": {body}}, ta.as(alice))
		ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
		ta.runPreviews()
	}
	edit("changed words, same link " + link)
	ensure.DeepEqual(t, ta.preview(m.ID).FileID, p.FileID)
	ensure.DeepEqual(t, pageHits.Load(), int64(1))

	// editing to another URL replaces it and removes the old image
	edit("now " + srv.URL + "/other")
	p2 := ta.preview(m.ID)
	ensure.DeepEqual(t, p2.Title, "Other page")
	ensure.False(t, p2.FileID.Valid)
	ensure.True(t, ta.objectMissing(fileKey(f, "converted")))
	_, err := ta.app.q.FileByID(ctx, f.ID)
	ensure.True(t, err != nil, "the old image row is gone")

	// editing the URL away removes the preview
	edit("no link any more")
	_, err = ta.app.q.PreviewByMessage(ctx, m.ID)
	ensure.True(t, err != nil)

	// deleting a message with a ready preview leaves nothing behind
	m2 := ta.send(alice, chat, link)
	ta.runPreviews()
	f2 := ta.file(ta.preview(m2.ID).FileID.Int64)
	w := ta.post(messagePath(chat.ID, m2.ID)+"/delete", nil, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, countRows(t, ta, "previews"), 0)
	ensure.DeepEqual(t, countRows(t, ta, "files"), 0)
	ensure.True(t, ta.objectMissing(fileKey(f2, "converted")))
}

func TestPreviewFailures(t *testing.T) {
	needMagick(t)
	ta, alice, chat := previewSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"title": "nope"}`)
		case "/empty":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, htmlPage(""))
		case "/htmlimage":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, htmlPage(`<title>T</title><meta property="og:image" content="/fake.png">`))
		case "/fake.png":
			w.Header().Set("Content-Type", "image/png") // lies: it is HTML
			fmt.Fprint(w, "<html><script>alert(1)</script></html>")
		case "/bigimage":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, htmlPage(`<title>Big</title><meta property="og:image" content="/big.png">`))
		case "/big.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(append(pngOf(t, 4, 4), make([]byte, 11<<20)...))
		case "/imageonly":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, htmlPage(`<meta property="og:image" content="/small.png">`))
		case "/small.png":
			w.Write(pngOf(t, 20, 10))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c, _ := ta.app.hub.subscribe(alice.ID, chat.ID)
	check := func(path, status string) Preview {
		t.Helper()
		m := ta.send(alice, chat, srv.URL+path)
		pinged(c)
		ta.runPreviews()
		p := ta.preview(m.ID)
		ensure.DeepEqual(t, p.Status, status, path)
		ensure.DeepEqual(t, pinged(c), status == "ready", path)
		return p
	}
	check("/json", "failed")
	check("/500", "failed")
	check("/empty", "failed") // neither title nor image
	// an image that is not an image, or is too big, is dropped; the card stays
	ensure.False(t, check("/htmlimage", "ready").FileID.Valid)
	ensure.False(t, check("/bigimage", "ready").FileID.Valid)
	// an image alone is enough
	ensure.True(t, check("/imageonly", "ready").FileID.Valid)
	ensure.DeepEqual(t, countRows(t, ta, "files"), 1)

	// a failed preview renders nothing and is never retried
	page := ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.DeepEqual(t, strings.Count(page, `class="preview"`), 3)
	ensure.False(t, ta.app.previewOnce(t.Context()))
}

func TestPreviewNotRequested(t *testing.T) {
	ta, alice, chat := previewSetup(t)
	// no URL, a file message, and URLs we would never fetch
	ta.send(alice, chat, "no link here")
	ta.upload(alice, chat, "https://example.com/file.bin", []byte("data"))
	ta.send(alice, chat, "https://user:secret@example.com/private")
	ensure.DeepEqual(t, countRows(t, ta, "previews"), 0)
}

func TestPreviewForDeletedOrEditedMessage(t *testing.T) {
	needMagick(t)
	ta, alice, chat := previewSetup(t)
	var m Message
	var action func()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pic":
			w.Write(pngOf(t, 20, 10))
		default:
			action() // the message changes while its preview is being fetched
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, htmlPage(`<title>T</title><meta property="og:image" content="/pic">`))
		}
	}))
	defer srv.Close()

	// deleted meanwhile
	action = func() {
		ta.post(messagePath(chat.ID, m.ID)+"/delete", nil, ta.as(alice))
	}
	m = ta.send(alice, chat, srv.URL+"/a")
	ta.runPreviews()
	ensure.DeepEqual(t, countRows(t, ta, "previews"), 0)
	ensure.DeepEqual(t, countRows(t, ta, "files"), 0)
	entries := ta.fakeObjects()
	ensure.DeepEqual(t, len(entries), 0, entries)

	// edited to another URL meanwhile: the stale result is discarded
	edited := false
	action = func() {
		if !edited {
			edited = true
			ta.post(messagePath(chat.ID, m.ID)+"/edit", url.Values{"body": {srv.URL + "/b"}}, ta.as(alice))
		}
	}
	m = ta.send(alice, chat, srv.URL+"/a")
	ta.runPreviews()
	p := ta.preview(m.ID)
	ensure.DeepEqual(t, p.Url, srv.URL+"/b")
	ensure.DeepEqual(t, p.Status, "ready")
	ensure.DeepEqual(t, countRows(t, ta, "files"), 1, "only the image of the current preview")
	ensure.DeepEqual(t, len(ta.fakeObjects()), 1)
}
