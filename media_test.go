package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daaku/ensure"
)

// apiPost posts a JSON body the way the upload script does.
func (ta *testApp) apiPost(path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	ta.t.Helper()
	b, err := json.Marshal(body)
	ensure.Nil(ta.t, err)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return ta.do(r, opts...)
}

func decodeJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	ensure.Nil(t, json.Unmarshal(w.Body.Bytes(), &v), w.Body.String())
	return v
}

// uploadInit starts an upload and returns the answer.
func (ta *testApp) uploadInit(user *testUser, chat Chat, name string, size int64) uploadInitOut {
	ta.t.Helper()
	w := ta.apiPost(pUploadInit, uploadInitIn{Chat: chat.ID, Name: name, Size: size}, ta.as(user))
	ensure.DeepEqual(ta.t, w.Code, http.StatusOK, w.Body.String())
	return decodeJSON[uploadInitOut](ta.t, w)
}

// uploadParts PUTs data to the part URLs like a browser would.
func (ta *testApp) uploadParts(urls []string, data []byte) {
	ta.t.Helper()
	for i, u := range urls {
		end := min(int64(i+1)*partSize, int64(len(data)))
		w := ta.plainHTTP(http.MethodPut, u, data[int64(i)*partSize:end])
		ensure.DeepEqual(ta.t, w.Code, http.StatusOK, w.Body.String())
	}
}

// upload performs a whole upload and returns the file id.
func (ta *testApp) upload(user *testUser, chat Chat, name string, data []byte) int64 {
	ta.t.Helper()
	out := ta.uploadInit(user, chat, name, int64(len(data)))
	ta.uploadParts(out.URLs, data)
	w := ta.apiPost(pUploadComplete, uploadFileIn{File: out.File}, ta.as(user))
	ensure.DeepEqual(ta.t, w.Code, http.StatusOK, w.Body.String())
	return out.File
}

func (ta *testApp) file(id int64) File {
	ta.t.Helper()
	f, err := ta.app.q.FileByID(ta.t.Context(), id)
	ensure.Nil(ta.t, err)
	return f
}

func (ta *testApp) stats(userID, chatID int64) (UserStat, ChatStat) {
	ta.t.Helper()
	us, err := ta.app.userStats(ta.t.Context(), userID)
	ensure.Nil(ta.t, err)
	cs, err := ta.app.chatStats(ta.t.Context(), chatID)
	ensure.Nil(ta.t, err)
	return us, cs
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	ensure.Nil(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 3))))
	return b.Bytes()
}

// tinyMP4 is only the start of an MP4 file, enough to be recognised.
var tinyMP4 = []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2\x00\x00\x00\x08free")

func TestSniffMedia(t *testing.T) {
	ftyp := func(brand string) string { return "\x00\x00\x00\x18ftyp" + brand + "\x00\x00\x00\x00" }
	cases := []struct {
		name, head, kind, format string
	}{
		{"jpeg", "\xff\xd8\xff\xe0\x00\x10JFIF", kindImage, "jpeg"},
		{"png", "\x89PNG\r\n\x1a\n\x00\x00", kindImage, "png"},
		{"gif87", "GIF87a\x01\x00", kindImage, "gif"},
		{"gif89", "GIF89a\x01\x00", kindImage, "gif"},
		{"webp", "RIFF\x24\x00\x00\x00WEBPVP8 ", kindImage, "webp"},
		{"heic", ftyp("heic"), kindImage, "heic"},
		{"heix", ftyp("heix"), kindImage, "heic"},
		{"hevc", ftyp("hevc"), kindImage, "heic"},
		{"hevx", ftyp("hevx"), kindImage, "heic"},
		{"heim", ftyp("heim"), kindImage, "heic"},
		{"heis", ftyp("heis"), kindImage, "heic"},
		{"mif1", ftyp("mif1"), kindImage, "heic"},
		{"msf1", ftyp("msf1"), kindImage, "heic"},
		{"avif", ftyp("avif"), kindImage, "avif"},
		{"avis", ftyp("avis"), kindImage, "avif"},
		{"m4a", ftyp("M4A "), kindOther, ""},
		{"m4b", ftyp("M4B "), kindOther, ""},
		{"mp4", ftyp("isom"), kindVideo, "mov"},
		{"mp42", ftyp("mp42"), kindVideo, "mov"},
		{"quicktime", ftyp("qt  "), kindVideo, "mov"},
		{"3gp", ftyp("3gp4"), kindVideo, "mov"},
		{"matroska", "\x1a\x45\xdf\xa3\x01\x00\x00", kindVideo, "matroska"},
		{"avi", "RIFF\x24\x00\x00\x00AVI LIST", kindVideo, "avi"},
		{"wav is riff but not media we convert", "RIFF\x24\x00\x00\x00WAVEfmt ", kindOther, ""},
		{"html named jpg", "<!doctype html><script>alert(1)</script>", kindOther, ""},
		{"hls playlist named mp4", "#EXTM3U\n#EXT-X-TARGETDURATION:1\n/etc/passwd\n", kindOther, ""},
		{"svg", `<svg xmlns="http://www.w3.org/2000/svg">`, kindOther, ""},
		{"pdf", "%PDF-1.7\n", kindOther, ""},
		{"empty", "", kindOther, ""},
		{"short ftyp", "\x00\x00\x00\x18ftyp", kindOther, ""},
		{"truncated jpeg magic", "\xff\xd8", kindOther, ""},
	}
	for _, c := range cases {
		kind, format := sniffMedia([]byte(c.head))
		ensure.DeepEqual(t, kind, c.kind, c.name)
		ensure.DeepEqual(t, format, c.format, c.name)
	}
}

func TestCleanFileName(t *testing.T) {
	for in, want := range map[string]string{
		"photo.jpg":             "photo.jpg",
		"../../etc/passwd":      "passwd",
		`C:\Users\me\report.pd`: "report.pd",
		"":                      "file",
		"   ":                   "file",
		"..":                    "file",
		"/":                     "file",
		"a\nb\r\x00c.txt":       "abc.txt",
		"  spaced name.txt  ":   "spaced name.txt",
		"смешно 😀.png":          "смешно 😀.png",
		"bad\xffbyte":           "badbyte",
	} {
		ensure.DeepEqual(t, cleanFileName(in), want, in)
	}
	long := cleanFileName(strings.Repeat("é", 300))
	ensure.True(t, len(long) <= maxFileName)
	ensure.DeepEqual(t, long, strings.Repeat("é", 100))
}

func TestCheckParts(t *testing.T) {
	full := s3Part{Size: partSize}
	p := func(n int, size int64) s3Part { return s3Part{Number: n, Size: size} }
	ensure.True(t, checkParts([]s3Part{p(1, 1)}, 1))
	ensure.True(t, checkParts([]s3Part{p(1, partSize)}, partSize))
	ensure.True(t, checkParts([]s3Part{p(1, partSize), p(2, 1)}, partSize+1))
	ensure.False(t, checkParts(nil, 1), "no parts")
	ensure.False(t, checkParts([]s3Part{p(1, 2)}, 1), "more bytes than declared")
	ensure.False(t, checkParts([]s3Part{p(1, 1)}, 2), "fewer bytes than declared")
	ensure.False(t, checkParts([]s3Part{p(1, partSize)}, partSize+1), "missing last part")
	ensure.False(t, checkParts([]s3Part{p(2, 1)}, 1), "wrong number")
	ensure.False(t, checkParts([]s3Part{p(1, partSize-1), p(2, 2)}, partSize+1), "short first part")
	ensure.False(t, checkParts([]s3Part{p(1, partSize), p(2, 1), p(3, 1)}, partSize+1), "extra part")
	ensure.False(t, checkParts([]s3Part{p(1, partSize+1), p(2, 0)}, partSize+1), "oversized part")
	_ = full
}

func TestUploadHappyPath(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "Team", bob)

	for _, size := range []int{1, partSize + 1} {
		data := randData(t, size)
		before, beforeChat := ta.stats(alice.ID, chat.ID)
		c, _ := ta.app.hub.subscribe(bob.ID, 0)

		id := ta.upload(alice, chat, "notes.bin", data)

		f := ta.file(id)
		ensure.DeepEqual(t, f.Status, statusReady)
		ensure.DeepEqual(t, f.Kind, kindOther)
		ensure.DeepEqual(t, f.Size, int64(size))
		ensure.DeepEqual(t, f.UploadID, "")
		ensure.DeepEqual(t, f.Name, "notes.bin")
		ensure.DeepEqual(t, len(f.S3Prefix), 32)

		rc, err := ta.app.s3.get(ctx, fileKey(f, "original"), "")
		ensure.True(t, bytes.Equal(readAll(t, rc, err), data))

		var msgID, fileID int64
		var body string
		ensure.Nil(t, ta.app.db.QueryRowContext(ctx,
			"SELECT id, file_id, body FROM messages WHERE file_id = ?", id).Scan(&msgID, &fileID, &body))
		ensure.DeepEqual(t, body, "")

		after, afterChat := ta.stats(alice.ID, chat.ID)
		ensure.DeepEqual(t, after.FilesUploaded-before.FilesUploaded, int64(1))
		ensure.DeepEqual(t, after.BytesUploaded-before.BytesUploaded, int64(size))
		ensure.DeepEqual(t, after.MessagesSent-before.MessagesSent, int64(1))
		ensure.DeepEqual(t, after.ImagesUploaded+after.VideosUploaded, int64(0))
		ensure.DeepEqual(t, afterChat.FilesUploaded-beforeChat.FilesUploaded, int64(1))
		ensure.DeepEqual(t, afterChat.BytesUploaded-beforeChat.BytesUploaded, int64(size))
		ensure.True(t, pinged(c), "the chat is pinged")

		// the stored original is an opaque download carrying the name
		u, _ := ta.app.s3.presignStable(fileKey(f, "original"), time.Hour)
		w := ta.plainHTTP(http.MethodGet, u, nil)
		ensure.DeepEqual(t, w.Header().Get("Content-Type"), "application/octet-stream")
		ensure.DeepEqual(t, w.Header().Get("Content-Disposition"), "attachment; filename*=UTF-8''notes.bin")
		ensure.DeepEqual(t, w.Header().Get("Cache-Control"), cacheForever)

		// rendered as a download row for both members, and in the chat list
		for _, u := range []*testUser{alice, bob} {
			page := ta.get(chatPath(chat.ID), ta.as(u)).Body.String()
			ensure.StringContains(t, page, `id="m`+idStr(msgID)+`-file"`)
			ensure.StringContains(t, page, `href="/f/`+idStr(id)+`/o"`)
			ensure.StringContains(t, page, `download="notes.bin"`)
			ensure.StringContains(t, page, fmtBytes(int64(size)))
			ensure.False(t, strings.Contains(page, `id="m`+idStr(msgID)+`-body"`))
			ensure.False(t, strings.Contains(page, `id="m`+idStr(msgID)+`-edit"`), "no edit for files")
		}
		ensure.StringContains(t, ta.get(pHome, ta.as(bob)).Body.String(), "alice: 📎 notes.bin")
	}
}

func TestUploadKinds(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	chat := ta.newChat(alice, "Team")

	img := ta.upload(alice, chat, "pic.txt", tinyPNG(t)) // the name does not matter
	f := ta.file(img)
	ensure.DeepEqual(t, f.Kind, kindImage)
	ensure.DeepEqual(t, f.Status, statusPending)
	page := ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, page, "Processing…")
	ensure.StringContains(t, ta.get(pHome, ta.as(alice)).Body.String(), "alice: 📷 Photo")
	select {
	case <-ta.app.mediaWake:
	default:
		t.Fatal("the media worker was not woken")
	}

	vid := ta.upload(alice, chat, "clip.jpg", tinyMP4)
	f = ta.file(vid)
	ensure.DeepEqual(t, f.Kind, kindVideo)
	ensure.DeepEqual(t, f.Status, statusPending)
	ensure.StringContains(t, ta.get(pHome, ta.as(alice)).Body.String(), "alice: 🎬 Video")

	// a script named like an image is just a file
	other := ta.upload(alice, chat, "x.jpg", []byte("<script>alert(1)</script>"))
	ensure.DeepEqual(t, ta.file(other).Kind, kindOther)
	ensure.DeepEqual(t, ta.file(other).Status, statusReady)

	us, cs := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, []int64{us.ImagesUploaded, us.VideosUploaded, us.FilesUploaded}, []int64{1, 1, 1})
	ensure.DeepEqual(t, []int64{cs.ImagesUploaded, cs.VideosUploaded, cs.FilesUploaded}, []int64{1, 1, 1})
	ensure.DeepEqual(t, us.MessagesSent, int64(3))

	// a failed conversion falls back to the download row
	_, err := ta.app.db.ExecContext(t.Context(), "UPDATE files SET status = 'failed' WHERE id = ?", img)
	ensure.Nil(t, err)
	page = ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, page, `href="/f/`+idStr(img)+`/o"`)
}

func TestUploadInitRejections(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	boss := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "Team")
	init := func(u *testUser, in any) *httptest.ResponseRecorder {
		if u == nil {
			return ta.apiPost(pUploadInit, in)
		}
		return ta.apiPost(pUploadInit, in, ta.as(u))
	}
	ok := uploadInitIn{Chat: chat.ID, Name: "a", Size: 5}

	ensure.DeepEqual(t, init(nil, ok).Code, http.StatusUnauthorized)
	ensure.DeepEqual(t, init(bob, ok).Code, http.StatusNotFound)
	ensure.DeepEqual(t, init(boss, ok).Code, http.StatusNotFound, "an admin who is not a member cannot upload")
	ensure.DeepEqual(t, init(alice, uploadInitIn{Chat: 999, Name: "a", Size: 5}).Code, http.StatusNotFound)
	ensure.DeepEqual(t, init(alice, uploadInitIn{Chat: chat.ID, Name: "a", Size: 0}).Code, http.StatusBadRequest)
	ensure.DeepEqual(t, init(alice, uploadInitIn{Chat: chat.ID, Name: "a", Size: -5}).Code, http.StatusBadRequest)
	ensure.DeepEqual(t, init(alice, "not an object").Code, http.StatusBadRequest)

	w := init(alice, uploadInitIn{Chat: chat.ID, Name: "a", Size: ta.app.cfg.MaxUploadSize + 1})
	ensure.DeepEqual(t, w.Code, http.StatusRequestEntityTooLarge)
	ensure.DeepEqual(t, decodeJSON[map[string]string](t, w)["error"], "The file is larger than the 100 MB limit.")
	ensure.DeepEqual(t, w.Header().Get("Content-Type"), "application/json")

	ensure.DeepEqual(t, countRows(t, ta, "files"), 0)

	// the limit itself is fine, and gives one URL per part
	out := ta.uploadInit(alice, chat, "../../etc/pass\nwd", ta.app.cfg.MaxUploadSize)
	ensure.DeepEqual(t, out.PartSize, int64(partSize))
	ensure.DeepEqual(t, len(out.URLs), 13)
	f := ta.file(out.File)
	ensure.DeepEqual(t, f.Name, "passwd")
	ensure.DeepEqual(t, f.Status, statusUploading)
	for _, u := range out.URLs {
		ensure.StringContains(t, u, "/"+f.S3Prefix+"/original?")
		ensure.False(t, strings.Contains(u, "passwd"), "the name never reaches the key")
	}
}

func TestUploadCompleteRejections(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "Team", bob)
	complete := func(u *testUser, id int64) int {
		return ta.apiPost(pUploadComplete, uploadFileIn{File: id}, ta.as(u)).Code
	}
	gone := func(id int64) {
		t.Helper()
		_, err := ta.app.q.FileByID(ctx, id)
		ensure.True(t, err != nil, "the row must be deleted")
	}
	data := randData(t, partSize+10)

	// a missing part
	out := ta.uploadInit(alice, chat, "a", int64(len(data)))
	ta.uploadParts(out.URLs[:1], data)
	ensure.DeepEqual(t, complete(alice, out.File), http.StatusBadRequest)
	gone(out.File)

	// no parts at all
	out = ta.uploadInit(alice, chat, "a", 5)
	ensure.DeepEqual(t, complete(alice, out.File), http.StatusBadRequest)
	gone(out.File)

	// a part with the wrong size
	out = ta.uploadInit(alice, chat, "a", int64(len(data)))
	ta.plainHTTP(http.MethodPut, out.URLs[0], data[:partSize-1])
	ta.plainHTTP(http.MethodPut, out.URLs[1], data[partSize-1:])
	ensure.DeepEqual(t, complete(alice, out.File), http.StatusBadRequest)
	gone(out.File)

	// more bytes than declared
	out = ta.uploadInit(alice, chat, "a", 5)
	ta.plainHTTP(http.MethodPut, out.URLs[0], []byte("123456"))
	ensure.DeepEqual(t, complete(alice, out.File), http.StatusBadRequest)
	gone(out.File)

	// fewer bytes than declared
	out = ta.uploadInit(alice, chat, "a", 5)
	ta.plainHTTP(http.MethodPut, out.URLs[0], []byte("1234"))
	ensure.DeepEqual(t, complete(alice, out.File), http.StatusBadRequest)
	gone(out.File)

	// someone else's upload: not found, and left alone
	out = ta.uploadInit(alice, chat, "a", 5)
	ta.uploadParts(out.URLs, []byte("12345"))
	ensure.DeepEqual(t, complete(bob, out.File), http.StatusNotFound)
	ensure.DeepEqual(t, ta.file(out.File).Status, statusUploading)
	ensure.DeepEqual(t, complete(alice, 99999), http.StatusNotFound)

	// removed from the group between init and complete
	bobUp := ta.uploadInit(bob, chat, "b", 5)
	ta.uploadParts(bobUp.URLs, []byte("12345"))
	_, err := ta.app.db.ExecContext(ctx, "DELETE FROM members WHERE chat_id = ? AND user_id = ?", chat.ID, bob.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, complete(bob, bobUp.File), http.StatusNotFound)
	gone(bobUp.File)

	// nothing above created a message or moved a counter
	ensure.DeepEqual(t, countRows(t, ta, "messages"), 0)
	for _, u := range []*testUser{alice, bob} {
		us, cs := ta.stats(u.ID, chat.ID)
		ensure.DeepEqual(t, us, UserStat{UserID: u.ID})
		ensure.DeepEqual(t, cs, ChatStat{ChatID: chat.ID})
	}

	// completing twice: the second is not found and adds nothing
	ensure.DeepEqual(t, complete(alice, out.File), http.StatusOK)
	ensure.DeepEqual(t, complete(alice, out.File), http.StatusNotFound)
	ensure.DeepEqual(t, countRows(t, ta, "messages"), 1)
	us, _ := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, us.FilesUploaded, int64(1))
}

func TestUploadCompleteConcurrent(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	chat := ta.newChat(alice, "Team")
	out := ta.uploadInit(alice, chat, "a", 5)
	ta.uploadParts(out.URLs, []byte("12345"))

	codes := make([]int, 8)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = ta.apiPost(pUploadComplete, uploadFileIn{File: out.File}, ta.as(alice)).Code
		})
	}
	wg.Wait()
	oks := 0
	for _, c := range codes {
		if c == http.StatusOK {
			oks++
		} else {
			ensure.DeepEqual(t, c, http.StatusNotFound)
		}
	}
	ensure.DeepEqual(t, oks, 1)
	ensure.DeepEqual(t, countRows(t, ta, "messages"), 1)
	us, _ := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, us.FilesUploaded, int64(1))
	ensure.DeepEqual(t, us.BytesUploaded, int64(5))
}

func TestUploadAbort(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "Team", bob)
	out := ta.uploadInit(alice, chat, "a", 5)
	ta.uploadParts(out.URLs, []byte("12345"))
	f := ta.file(out.File)

	// someone else's abort does nothing
	w := ta.apiPost(pUploadAbort, uploadFileIn{File: out.File}, ta.as(bob))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.DeepEqual(t, ta.file(out.File).Status, statusUploading)

	w = ta.apiPost(pUploadAbort, uploadFileIn{File: out.File}, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	_, err := ta.app.q.FileByID(ctx, out.File)
	ensure.True(t, err != nil)
	_, err = ta.app.s3.listParts(ctx, fileKey(f, "original"), f.UploadID)
	ensure.DeepEqual(t, s3Status(err), http.StatusNotFound, "the multipart upload is gone")

	// a finished file cannot be aborted
	id := ta.upload(alice, chat, "b", []byte("12345"))
	ta.apiPost(pUploadAbort, uploadFileIn{File: id}, ta.as(alice))
	ensure.DeepEqual(t, ta.file(id).Status, statusReady)
	ensure.DeepEqual(t, ta.apiPost(pUploadAbort, uploadFileIn{File: id}).Code, http.StatusUnauthorized)
}

func TestFileRedirect(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	boss := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "Team", bob)
	dm := ta.newDM(alice, bob)
	data := randData(t, 3000)
	id := ta.upload(alice, chat, "a.bin", data)
	path := filePath(id, "o")

	before, beforeChat := ta.stats(bob.ID, chat.ID)
	w := ta.get(path, ta.as(bob))
	ensure.DeepEqual(t, w.Code, http.StatusFound)
	ensure.DeepEqual(t, w.Header().Get("Cache-Control"),
		"private, max-age="+idStr(int64((ta.app.cfg.S3URLValidity/2).Seconds())))
	loc := w.Header().Get("Location")
	got := ta.plainHTTP(http.MethodGet, loc, nil)
	ensure.DeepEqual(t, got.Code, http.StatusOK)
	ensure.True(t, bytes.Equal(got.Body.Bytes(), data))

	after, afterChat := ta.stats(bob.ID, chat.ID)
	ensure.DeepEqual(t, after.BytesDownloaded-before.BytesDownloaded, int64(3000))
	ensure.DeepEqual(t, afterChat.BytesDownloaded-beforeChat.BytesDownloaded, int64(3000))

	// the same window gives the same URL, so the browser cache works
	ensure.DeepEqual(t, ta.get(path, ta.as(alice)).Header().Get("Location"), loc)
	// the signed URL never contains anything from the request
	u, err := url.Parse(loc)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, u.Path, pFakeS3+"dev/"+ta.file(id).S3Prefix+"/original")

	// access
	ensure.DeepEqual(t, ta.get(path).Code, http.StatusSeeOther)
	ensure.DeepEqual(t, ta.get(path, ta.as(carol)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(path, ta.as(boss)).Code, http.StatusFound, "an admin may view a group")
	dmFile := ta.upload(alice, dm, "private.bin", data)
	ensure.DeepEqual(t, ta.get(filePath(dmFile, "o"), ta.as(bob)).Code, http.StatusFound)
	ensure.DeepEqual(t, ta.get(filePath(dmFile, "o"), ta.as(boss)).Code, http.StatusNotFound,
		"an admin cannot fetch files of a direct message")
	us, _ := ta.stats(carol.ID, chat.ID)
	ensure.DeepEqual(t, us.BytesDownloaded, int64(0))

	// variants that do not exist
	for _, p := range []string{filePath(id, "c"), filePath(id, "p"), filePath(id, "x"),
		filePath(99999, "o"), pFile + "abc/o"} {
		ensure.DeepEqual(t, ta.get(p, ta.as(alice)).Code, http.StatusNotFound, p)
	}

	// a file that is still uploading is not served
	out := ta.uploadInit(alice, chat, "later", 5)
	ensure.DeepEqual(t, ta.get(filePath(out.File, "o"), ta.as(alice)).Code, http.StatusNotFound)

	// a removed member loses access
	_, err = ta.app.db.ExecContext(t.Context(),
		"DELETE FROM members WHERE chat_id = ? AND user_id = ?", chat.ID, bob.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, ta.get(path, ta.as(bob)).Code, http.StatusNotFound)
}

func TestDeleteFileMessage(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	chat := ta.newChat(alice, "Team")
	objectGone := func(f File) {
		t.Helper()
		_, err := ta.app.s3.get(ctx, fileKey(f, "original"), "")
		ensure.DeepEqual(t, s3Status(err), http.StatusNotFound)
	}
	msgOf := func(fileID int64) int64 {
		t.Helper()
		var id int64
		ensure.Nil(t, ta.app.db.QueryRowContext(ctx,
			"SELECT id FROM messages WHERE file_id = ?", fileID).Scan(&id))
		return id
	}

	id := ta.upload(alice, chat, "a.bin", []byte("12345"))
	f := ta.file(id)
	before, beforeChat := ta.stats(alice.ID, chat.ID)
	w := ta.post(messagePath(chat.ID, msgOf(id))+"/delete", nil, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, countRows(t, ta, "messages"), 0)
	ensure.DeepEqual(t, countRows(t, ta, "files"), 0)
	objectGone(f)
	ensure.DeepEqual(t, ta.get(filePath(id, "o"), ta.as(alice)).Code, http.StatusNotFound)
	after, afterChat := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, after, before)
	ensure.DeepEqual(t, afterChat, beforeChat)

	// an S3 failure during delete still removes the rows
	id = ta.upload(alice, chat, "b.bin", []byte("12345"))
	real := ta.app.s3.hc
	ta.app.s3.hc = &http.Client{Transport: stubTransport(func(r *http.Request) (*http.Response, error) {
		return stubResponse(http.StatusInternalServerError, "boom"), nil
	})}
	w = ta.post(messagePath(chat.ID, msgOf(id))+"/delete", nil, ta.as(alice))
	ta.app.s3.hc = real
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, countRows(t, ta, "messages"), 0)
	ensure.DeepEqual(t, countRows(t, ta, "files"), 0)

	// a text message still deletes
	m := ta.newMessage(chat, alice, "hi")
	ta.post(messagePath(chat.ID, m.ID)+"/delete", nil, ta.as(alice))
	ensure.DeepEqual(t, countRows(t, ta, "messages"), 0)
}

func TestStaleUploadCleanup(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	chat := ta.newChat(alice, "Team")

	old := ta.uploadInit(alice, chat, "old", 5)
	ta.uploadParts(old.URLs, []byte("12345"))
	oldFile := ta.file(old.File)
	done := ta.upload(alice, chat, "done", []byte("12345"))

	ta.app.now = func() time.Time { return testNow.Add(25 * time.Hour) }
	fresh := ta.uploadInit(alice, chat, "fresh", 5)

	ta.app.cleanStaleUploads(ctx)

	_, err := ta.app.q.FileByID(ctx, old.File)
	ensure.True(t, err != nil, "the stale upload is deleted")
	_, err = ta.app.s3.listParts(ctx, fileKey(oldFile, "original"), oldFile.UploadID)
	ensure.DeepEqual(t, s3Status(err), http.StatusNotFound, "and aborted")
	ensure.DeepEqual(t, ta.file(fresh.File).Status, statusUploading)
	ensure.DeepEqual(t, ta.file(done).Status, statusReady, "finished files are never touched")
}

func TestMediaBox(t *testing.T) {
	for _, c := range [][4]int64{
		// width, height, shown width, shown height
		{3840, 2560, 352, 235}, // wide: capped by the width
		{1920, 1080, 352, 198},
		{3024, 4032, 312, 416}, // tall: capped by the height
		{720, 1280, 234, 416},
		{352, 416, 352, 416}, // exactly the box
		{160, 120, 160, 120}, // small: never scaled up
		{300, 900, 139, 416},
		{9000, 10, 352, 1}, // extreme shapes keep a pixel
		{10, 9000, 1, 416},
		{0, 0, 352, 416}, // unknown size
	} {
		w, h := mediaBox(c[0], c[1])
		ensure.DeepEqual(t, []int64{w, h}, []int64{c[2], c[3]}, c)
	}
}

// albums and the media page

// mediaMessage is a message with a converted picture or video, made without
// running the converters.
func (ta *testApp) mediaMessage(user *testUser, chat Chat, kind string) Message {
	ta.t.Helper()
	t := ta.t
	file := ta.upload(user, chat, "p.bin", []byte("x"))
	_, err := ta.app.db.ExecContext(t.Context(), `UPDATE files SET kind = ?, status = 'ready', name = 'p.jpg',
		conv_type = 'image/webp', conv_size = 1, poster_type = 'image/webp', poster_size = 1,
		width = 800, height = 600 WHERE id = ?`, kind, file)
	ensure.Nil(t, err)
	var id int64
	ensure.Nil(t, ta.app.db.QueryRowContext(t.Context(), "SELECT id FROM messages WHERE file_id = ?", file).Scan(&id))
	m, err := ta.app.q.MessageByID(t.Context(), id)
	ensure.Nil(t, err)
	return m
}

// sentAt moves a message in time, in minutes after the test clock.
func (ta *testApp) sentAt(m Message, minutes int64) {
	ta.t.Helper()
	_, err := ta.app.db.ExecContext(ta.t.Context(), "UPDATE messages SET created_at = ? WHERE id = ?",
		testNow.Unix()+minutes*60, m.ID)
	ensure.Nil(ta.t, err)
}

func TestAlbum(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "Team", bob)
	page := func() string { return ta.get(chatPath(chat.ID), ta.as(alice)).Body.String() }
	cell := func(first, last, m Message) string {
		return fmt.Sprintf(`<a id="m%d-c%d" class="album-cell" href="%s/media/%d/%d?at=%d">`,
			first.ID, m.ID, chatPath(chat.ID), first.ID, last.ID, m.ID)
	}

	// three in a row are three bubbles
	var ms []Message
	for range 3 {
		ms = append(ms, ta.mediaMessage(bob, chat, kindImage))
	}
	body := page()
	ensure.StringDoesNotContain(t, body, "album")
	ensure.DeepEqual(t, strings.Count(body, `class="msg-file msg-media"`), 3)

	// the fourth makes them one album, under the id of the first
	ms = append(ms, ta.mediaMessage(bob, chat, kindVideo))
	body = page()
	ensure.DeepEqual(t, strings.Count(body, `class="msg-file msg-media"`), 0)
	ensure.StringContains(t, body, fmt.Sprintf(`<article id="m%d" class="msg first msg-album">`, ms[0].ID))
	ensure.StringContains(t, body, fmt.Sprintf(`<div id="m%d-album" class="msg-media album">`, ms[0].ID))
	for _, m := range ms {
		ensure.StringContains(t, body, cell(ms[0], ms[3], m))
	}
	// every other message of it can still be linked to
	for _, m := range ms[1:] {
		ensure.StringContains(t, body, fmt.Sprintf(`<span id="m%d"></span>`, m.ID))
	}
	// lazy thumbnails of the converted picture, and the poster for a video
	ensure.DeepEqual(t, strings.Count(body, `width="176" height="176" loading="lazy" decoding="async">`), 4)
	ensure.StringContains(t, body, fmt.Sprintf(`<img src="/f/%d/c"`, ms[0].FileID.Int64))
	ensure.StringContains(t, body, fmt.Sprintf(`<img src="/f/%d/p"`, ms[3].FileID.Int64))
	ensure.DeepEqual(t, strings.Count(body, `class="album-play"`), 1)
	ensure.StringDoesNotContain(t, body, "album-more")
	// one time for the album, and no per message menu
	ensure.StringContains(t, body, fmt.Sprintf(`<div id="m%d-meta" class="msg-meta"><time id="m%d-time"`, ms[0].ID, ms[0].ID))
	ensure.StringDoesNotContain(t, body, fmt.Sprintf(`id="m%d-more"`, ms[0].ID))

	// more than four: four cells, the last one counts the rest
	ms = append(ms, ta.mediaMessage(bob, chat, kindImage), ta.mediaMessage(bob, chat, kindImage))
	body = page()
	ensure.DeepEqual(t, strings.Count(body, `class="album-cell"`), 4)
	ensure.StringContains(t, body, cell(ms[0], ms[5], ms[3]))
	ensure.StringContains(t, body, `<span class="album-more">+2</span>`)
	ensure.StringDoesNotContain(t, body, cell(ms[0], ms[5], ms[4]))
	// the count covers the video in the last cell, without its play badge
	ensure.StringDoesNotContain(t, body, "album-play")

	// 30 minutes apart still belongs, a minute more does not
	ta.sentAt(ms[4], 30)
	ta.sentAt(ms[5], 30)
	ensure.StringContains(t, page(), `<span class="album-more">+2</span>`)
	ta.sentAt(ms[4], 31)
	ta.sentAt(ms[5], 31)
	body = page()
	ensure.StringContains(t, body, cell(ms[0], ms[3], ms[3]))
	ensure.StringDoesNotContain(t, body, "album-more")
	ensure.DeepEqual(t, strings.Count(body, `class="msg-file msg-media"`), 2)

	// the gap is between neighbours, not from the first
	for i, m := range ms {
		ta.sentAt(m, int64(i)*25)
	}
	ensure.StringContains(t, page(), cell(ms[0], ms[5], ms[0]))

	// a new day starts a new run, in the viewer's time zone
	for i, m := range ms {
		ta.sentAt(m, 20*60+int64(i)*20) // 23:04 UTC onwards: three before midnight, three after
	}
	body = page()
	ensure.StringDoesNotContain(t, body, "album")
	ensure.DeepEqual(t, strings.Count(body, `class="msg-file msg-media"`), 6)
	for _, m := range ms {
		ta.sentAt(m, 0)
	}

	// something else in between splits the run
	ta.newMessage(chat, bob, "look at these")
	more := []Message{ta.mediaMessage(bob, chat, kindImage), ta.mediaMessage(bob, chat, kindImage), ta.mediaMessage(bob, chat, kindImage)}
	body = page()
	ensure.StringContains(t, body, cell(ms[0], ms[5], ms[0]))
	ensure.DeepEqual(t, strings.Count(body, `class="msg-file msg-media"`), 3)
	// and so does another sender
	mine := ta.mediaMessage(alice, chat, kindImage)
	body = page()
	ensure.DeepEqual(t, strings.Count(body, `class="msg-file msg-media"`), 4)
	ensure.StringContains(t, body, fmt.Sprintf(`<article id="m%d" class="msg mine first">`, mine.ID))
	_ = more

	// a picture that is not converted yet is not part of a run
	_, err := ta.app.db.ExecContext(t.Context(), "UPDATE files SET status = 'pending' WHERE id = ?", ms[2].FileID.Int64)
	ensure.Nil(t, err)
	body = page()
	ensure.StringDoesNotContain(t, body, "album")
}

func TestChatMediaPage(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	admin := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "Team", bob)
	other := ta.newChat(alice, "Other", bob)
	base := chatPath(chat.ID)
	text := ta.newMessage(chat, bob, "before")
	var ms []Message
	for i := range 6 {
		kind := kindImage
		if i == 2 {
			kind = kindVideo
		}
		ms = append(ms, ta.mediaMessage(bob, chat, kind))
	}
	first, last := ms[0], ms[5]
	path := func(from, to, at int64) string { return mediaPath(chat.ID, from, to, at) }
	link := func(m Message) string {
		return fmt.Sprintf(`href="%s/media/%d/%d?at=%d"`, base, first.ID, last.ID, m.ID)
	}

	w := ta.get(path(first.ID, last.ID, first.ID), ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	body := w.Body.String()
	// the first item, as the only one at full size, and lazy
	ensure.DeepEqual(t, strings.Count(body, `class="gallery-item"`), 1)
	ensure.StringContains(t, body, fmt.Sprintf(`<img id="media-%d" class="gallery-item" src="/f/%d/c" alt="p.jpg" width="800" height="600" loading="lazy" decoding="async">`,
		first.ID, first.FileID.Int64))
	ensure.StringContains(t, body, `<span id="gallery-count" class="muted">1 of 6</span>`)
	// no step back from the first, a step on to the second
	ensure.StringContains(t, body, `<a id="gallery-prev" class="gallery-step left" hidden=""></a>`)
	ensure.StringContains(t, body, `<a id="gallery-next" class="gallery-step right" `+link(ms[1])+` data-morph`)
	// the strip: every item, lazy, the shown one marked
	for i, m := range ms {
		variant := "c"
		if i == 2 {
			variant = "p"
		}
		current := ""
		if i == 0 {
			current = ` aria-current="true"`
		}
		ensure.StringContains(t, body, fmt.Sprintf(`<a id="thumb-%d" class="gallery-thumb" %s data-morph%s><img src="/f/%d/%s" alt="p.jpg" width="64" height="64" loading="lazy" decoding="async">`,
			m.ID, link(m), current, m.FileID.Int64, variant))
	}
	ensure.DeepEqual(t, strings.Count(body, `loading="lazy"`), 7)
	// back to the chat at the album
	ensure.StringContains(t, body, fmt.Sprintf(`<a class="back" href="%s?m=%d" aria-label="Back to Team">`, base, first.ID))
	w = ta.get(fmt.Sprintf("%s?m=%d", base, first.ID), ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.StringContains(t, w.Header().Get("Location"), fmt.Sprintf("#m%d", first.ID))
	// the message can still be acted on from here
	ensure.StringContains(t, body, fmt.Sprintf(`id="m%d-menu"`, first.ID))
	ensure.StringDoesNotContain(t, body, fmt.Sprintf(`action="%s/delete"`, messagePath(chat.ID, first.ID)))
	ensure.StringContains(t, ta.get(path(first.ID, last.ID, first.ID), ta.as(bob)).Body.String(),
		fmt.Sprintf(`action="%s/delete"`, messagePath(chat.ID, first.ID)))

	// a video in the middle: nothing loads until it is played
	body = ta.get(path(first.ID, last.ID, ms[2].ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, body, fmt.Sprintf(`<video id="media-%d" class="gallery-item" controls playsinline preload="none" poster="/f/%d/p" src="/f/%d/c"></video>`,
		ms[2].ID, ms[2].FileID.Int64, ms[2].FileID.Int64))
	ensure.StringContains(t, body, `>3 of 6<`)
	ensure.StringContains(t, body, `<a id="gallery-prev" class="gallery-step left" `+link(ms[1])+` data-morph`)
	ensure.StringContains(t, body, `<a id="gallery-next" class="gallery-step right" `+link(ms[3])+` data-morph`)
	ensure.StringContains(t, body, fmt.Sprintf(`<a id="thumb-%d" class="gallery-thumb" %s data-morph aria-current="true">`, ms[2].ID, link(ms[2])))
	ensure.DeepEqual(t, strings.Count(body, `aria-current="true"`), 1)

	// the last one has no step on; an unknown ?at shows the first
	body = ta.get(path(first.ID, last.ID, last.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, body, `<a id="gallery-next" class="gallery-step right" hidden=""></a>`)
	for _, q := range []string{"", "?at=x", "?at=999", fmt.Sprintf("?at=%d", text.ID)} {
		body = ta.get(fmt.Sprintf("%s/media/%d/%d%s", base, first.ID, last.ID, q), ta.as(alice)).Body.String()
		ensure.StringContains(t, body, `>1 of 6<`, q)
	}

	// a part of the run, and a single item, are runs too
	ensure.StringContains(t, ta.get(path(ms[1].ID, ms[3].ID, ms[2].ID), ta.as(alice)).Body.String(), `>2 of 3<`)
	ensure.StringContains(t, ta.get(path(ms[4].ID, ms[4].ID, ms[4].ID), ta.as(alice)).Body.String(), `>1 of 1<`)

	// who may see it: those who may see the chat
	ensure.DeepEqual(t, ta.get(path(first.ID, last.ID, first.ID), ta.as(bob)).Code, http.StatusOK)
	ensure.DeepEqual(t, ta.get(path(first.ID, last.ID, first.ID), ta.as(admin)).Code, http.StatusOK)
	ensure.DeepEqual(t, ta.get(path(first.ID, last.ID, first.ID), ta.as(carol)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(path(first.ID, last.ID, first.ID)).Header().Get("Location"), pSignIn)

	// ranges that are not a run of pictures and videos are not found
	notFound := func(name, p string) {
		t.Helper()
		ensure.DeepEqual(t, ta.get(p, ta.as(alice)).Code, http.StatusNotFound, name)
	}
	notFound("text in the range", path(text.ID, last.ID, first.ID))
	notFound("backwards", path(last.ID, first.ID, first.ID))
	notFound("nothing there", path(last.ID+50, last.ID+60, 0))
	notFound("zero", path(0, last.ID, first.ID))
	notFound("not a number", base+"/media/x/"+idStr(last.ID))
	notFound("negative", base+"/media/-1/"+idStr(last.ID))
	notFound("another chat", mediaPath(other.ID, first.ID, last.ID, first.ID))

	after := ta.newMessage(chat, bob, "after")
	notFound("text at the end", path(first.ID, after.ID, first.ID))
	late := ta.mediaMessage(bob, chat, kindImage)
	notFound("text in the middle", path(first.ID, late.ID, first.ID))
	hers := ta.mediaMessage(alice, chat, kindImage)
	notFound("two senders", path(late.ID, hers.ID, late.ID))

	ta.sentAt(ms[4], 31)
	ta.sentAt(ms[5], 31)
	notFound("more than 30 minutes apart", path(first.ID, last.ID, first.ID))
	ensure.DeepEqual(t, ta.get(path(first.ID, ms[3].ID, first.ID), ta.as(alice)).Code, http.StatusOK)
	ensure.DeepEqual(t, ta.get(path(ms[4].ID, last.ID, last.ID), ta.as(alice)).Code, http.StatusOK)

	_, err := ta.app.db.ExecContext(t.Context(), "UPDATE files SET status = 'pending' WHERE id = ?", ms[1].FileID.Int64)
	ensure.Nil(t, err)
	notFound("not converted yet", path(first.ID, ms[3].ID, first.ID))
	notFound("starts with one not converted", path(ms[1].ID, ms[3].ID, ms[1].ID))

	// a deleted message leaves a run of the rest
	ensure.DeepEqual(t, ta.post(messagePath(chat.ID, ms[1].ID)+"/delete", nil, ta.as(bob)).Code, http.StatusSeeOther)
	ensure.StringContains(t, ta.get(path(first.ID, ms[3].ID, ms[1].ID), ta.as(alice)).Body.String(), `>1 of 3<`)
}
