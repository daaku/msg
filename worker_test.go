package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daaku/ensure"
)

func needMagick(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("magick is not installed")
	}
}

// runMedia processes pending files until none are left.
func (ta *testApp) runMedia() {
	ta.t.Helper()
	for ta.app.mediaOnce(ta.t.Context()) {
	}
}

func solid(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 200, byte(i/4%256), 30, 255
	}
	return img
}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	ensure.Nil(t, png.Encode(&b, solid(w, h)))
	return b.Bytes()
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	ensure.Nil(t, jpeg.Encode(&b, solid(w, h), &jpeg.Options{Quality: 60}))
	return b.Bytes()
}

// jpegWithEXIF is a JPEG carrying an EXIF block with orientation 6 (rotate
// 90 degrees) and a GPS entry, like a photo from a phone.
func jpegWithEXIF(t *testing.T, w, h int) []byte {
	t.Helper()
	le := binary.LittleEndian
	entry := func(tag, typ uint16, count uint32, value []byte) []byte {
		e := make([]byte, 12)
		le.PutUint16(e[0:], tag)
		le.PutUint16(e[2:], typ)
		le.PutUint32(e[4:], count)
		copy(e[8:], value)
		return e
	}
	u32 := func(v uint32) []byte { b := make([]byte, 4); le.PutUint32(b, v); return b }
	var tiff bytes.Buffer
	tiff.WriteString("II*\x00")
	tiff.Write(u32(8))
	// IFD0 at 8: two entries, then the offset of the next IFD (none)
	gpsOffset := uint32(8 + 2 + 2*12 + 4)
	tiff.Write([]byte{2, 0})
	tiff.Write(entry(0x0112, 3, 1, []byte{6, 0}))   // Orientation
	tiff.Write(entry(0x8825, 4, 1, u32(gpsOffset))) // GPS IFD pointer
	tiff.Write(u32(0))
	// GPS IFD: latitude reference "N"
	tiff.Write([]byte{1, 0})
	tiff.Write(entry(0x0001, 2, 2, []byte{'N', 0}))
	tiff.Write(u32(0))

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	seg := []byte{0xff, 0xe1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	seg = append(seg, payload...)
	plain := jpegOf(t, w, h)
	out := append([]byte{}, plain[:2]...) // SOI
	out = append(out, seg...)
	return append(out, plain[2:]...)
}

func gifOf(t *testing.T, frames int) []byte {
	t.Helper()
	g := &gif.GIF{}
	for i := range frames {
		img := image.NewPaletted(image.Rect(0, 0, 30, 20), palette.Plan9)
		for j := range img.Pix {
			img.Pix[j] = uint8(i*40 + 10)
		}
		img.Set(i, i, color.White)
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 20)
	}
	var b bytes.Buffer
	ensure.Nil(t, gif.EncodeAll(&b, g))
	return b.Bytes()
}

// identify runs magick identify on bytes of the given format.
func identify(t *testing.T, format, fmtArg string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x")
	ensure.Nil(t, os.WriteFile(path, data, 0o600))
	out, err := exec.Command("magick", "identify", "-format", fmtArg, format+":"+path).Output()
	ensure.Nil(t, err)
	return string(out)
}

func (ta *testApp) object(key string) []byte {
	ta.t.Helper()
	rc, err := ta.app.s3.get(ta.t.Context(), key, "")
	return readAll(ta.t, rc, err)
}

func (ta *testApp) objectMissing(key string) bool {
	_, err := ta.app.s3.get(ta.t.Context(), key, "")
	return s3Status(err) == http.StatusNotFound
}

func mediaSetup(t *testing.T) (*testApp, *testUser, Chat) {
	t.Helper()
	needMagick(t)
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	return ta, alice, ta.newChat(alice, "Team")
}

func TestImageConversion(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	c, _ := ta.app.hub.subscribe(alice.ID, chat.ID)

	id := ta.upload(alice, chat, "big.jpg", jpegOf(t, 6000, 4000))
	ensure.DeepEqual(t, ta.file(id).Status, statusPending)
	pinged(c)
	ta.runMedia()

	f := ta.file(id)
	ensure.DeepEqual(t, f.Status, statusReady)
	ensure.DeepEqual(t, f.ConvType, "image/webp")
	ensure.DeepEqual(t, []int64{f.Width, f.Height}, []int64{3840, 2560})
	ensure.DeepEqual(t, f.Attempts, int64(1))
	out := ta.object(fileKey(f, "converted"))
	ensure.DeepEqual(t, f.ConvSize, int64(len(out)))
	ensure.DeepEqual(t, strings.TrimSpace(identify(t, "webp", "%m %w %h", out)), "WEBP 3840 2560")
	ensure.True(t, pinged(c), "the chat is pinged when the image is ready")

	// the stored object is served inline with a long cache life
	u, _ := ta.app.s3.presignStable(fileKey(f, "converted"), time.Hour)
	w := ta.plainHTTP(http.MethodGet, u, nil)
	ensure.DeepEqual(t, w.Header().Get("Content-Type"), "image/webp")
	ensure.DeepEqual(t, w.Header().Get("Cache-Control"), cacheForever)
	ensure.DeepEqual(t, w.Header().Get("Content-Disposition"), "inline")

	// rendered lazily with its dimensions
	page := ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, page, `<img id="f`+idStr(id)+`" src="/f/`+idStr(id)+
		`/c" width="352" height="235" alt="big.jpg" loading="lazy" decoding="async">`)
	ensure.StringContains(t, page, `<a class="media" href="/f/`+idStr(id)+`/c" target="_blank" rel="noopener">`)
	ensure.False(t, strings.Contains(page, "Processing…"))
	// the bubble is the picture, with the time on top of it
	ensure.StringContains(t, page, `-file" class="msg-file msg-media"><a class="media"`)
	ensure.StringContains(t, page, `decoding="async"></a><div id="m`)
	ensure.StringContains(t, page, `-meta" class="msg-meta"><time id="m`)

	// the converted variant redirects and counts its own size
	before, _ := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, ta.get(filePath(id, "c"), ta.as(alice)).Code, http.StatusFound)
	after, _ := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, after.BytesDownloaded-before.BytesDownloaded, f.ConvSize)
	ensure.DeepEqual(t, ta.get(filePath(id, "p"), ta.as(alice)).Code, http.StatusNotFound)

	// the temp directory is cleaned up
	entries, _ := os.ReadDir(ta.app.tmpDir())
	ensure.DeepEqual(t, len(entries), 0)
}

func TestImageNotUpscaled(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	id := ta.upload(alice, chat, "small.png", pngOf(t, 40, 30))
	ta.runMedia()
	f := ta.file(id)
	ensure.DeepEqual(t, f.Status, statusReady)
	ensure.DeepEqual(t, []int64{f.Width, f.Height}, []int64{40, 30})
}

func TestImageEXIFRemoved(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	in := jpegWithEXIF(t, 40, 20)
	// the fixture really has what we claim it has
	ensure.StringContains(t, identify(t, "jpeg", "%[EXIF:*]", in), "exif:Orientation=6")
	ensure.StringContains(t, identify(t, "jpeg", "%[EXIF:*]", in), "exif:GPSLatitudeRef=N")

	id := ta.upload(alice, chat, "phone.jpg", in)
	ta.runMedia()
	f := ta.file(id)
	ensure.DeepEqual(t, f.Status, statusReady)
	// rotated upright
	ensure.DeepEqual(t, []int64{f.Width, f.Height}, []int64{20, 40})
	out := ta.object(fileKey(f, "converted"))
	ensure.DeepEqual(t, strings.TrimSpace(identify(t, "webp", "%[EXIF:*]", out)), "")
	ensure.False(t, bytes.Contains(out, []byte("Exif")), "no EXIF chunk in the output")
	ensure.False(t, bytes.Contains(out, []byte("GPS")))
}

func TestImageAnimatedGIF(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	id := ta.upload(alice, chat, "anim.gif", gifOf(t, 3))
	ta.runMedia()
	f := ta.file(id)
	ensure.DeepEqual(t, f.Status, statusReady)
	ensure.DeepEqual(t, []int64{f.Width, f.Height}, []int64{30, 20})
	frames := strings.Fields(identify(t, "webp", "%n\n", ta.object(fileKey(f, "converted"))))
	ensure.DeepEqual(t, frames[0], "3")
}

func TestImageFormats(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	for format, want := range map[string][2]string{
		"jpeg": {"image/jpeg", "JPEG"},
		"avif": {"image/avif", "AVIF"},
	} {
		ta.app.cfg.ImageFormat = format
		// an animated input becomes a single frame in these formats
		id := ta.upload(alice, chat, "anim.gif", gifOf(t, 3))
		ta.runMedia()
		f := ta.file(id)
		ensure.DeepEqual(t, f.Status, statusReady, format)
		ensure.DeepEqual(t, f.ConvType, want[0])
		got := strings.Fields(identify(t, format, "%m %n\n", ta.object(fileKey(f, "converted"))))
		ensure.DeepEqual(t, got[:2], []string{want[1], "1"})
	}
}

func TestMagickFormatPinning(t *testing.T) {
	needMagick(t)
	ta := newTestApp(t)
	ctx := t.Context()
	dir := t.TempDir()
	write := func(data []byte) {
		ensure.Nil(t, os.WriteFile(filepath.Join(dir, "in"), data, 0o600))
	}

	// a GIF is not decoded when we say it is a JPEG
	write(gifOf(t, 1))
	_, _, err := ta.app.magickConvert(ctx, dir, "jpeg", "in", "out", 100, false)
	ensure.True(t, isPermanent(err), err)

	// MVG drawing commands, which can read local files, are never run
	write([]byte("push graphic-context\nviewbox 0 0 10 10\nimage over 0,0 0,0 \"/etc/passwd\"\npop graphic-context\n"))
	_, _, err = ta.app.magickConvert(ctx, dir, "jpeg", "in", "out", 100, false)
	ensure.True(t, isPermanent(err), err)
	_, statErr := os.Stat(filepath.Join(dir, "out"))
	ensure.True(t, os.IsNotExist(statErr), "nothing was written")

	// formats outside the allow list never start a process
	t.Setenv("PATH", t.TempDir())
	for _, format := range []string{"mvg", "svg", "msl", "url", "", "jpeg:x", "mov"} {
		_, _, err = ta.app.magickConvert(ctx, dir, format, "in", "out", 100, false)
		ensure.True(t, isPermanent(err), format)
		ensure.StringContains(t, err.Error(), "unsupported image format")
	}
}

func TestExec(t *testing.T) {
	ta := newTestApp(t)
	dir := t.TempDir()
	out, err := ta.app.exec(t.Context(), dir, time.Second, "sh", "-c", "pwd; echo $HOME; echo $LC_ALL; echo ${SECRET_THING:-unset}")
	ensure.Nil(t, err)
	ensure.DeepEqual(t, strings.Fields(string(out)), []string{dir, dir, "C", "unset"})

	_, err = ta.app.exec(t.Context(), dir, time.Second, "sh", "-c", "echo oops >&2; exit 3")
	ensure.True(t, isPermanent(err))
	ensure.StringContains(t, err.Error(), "oops")

	start := time.Now()
	_, err = ta.app.exec(t.Context(), dir, 100*time.Millisecond, "sleep", "30")
	ensure.True(t, isPermanent(err))
	ensure.True(t, time.Since(start) < 10*time.Second)

	// arguments are not interpreted by a shell
	out, err = ta.app.exec(t.Context(), dir, time.Second, "echo", "$(id)", ";", "ls")
	ensure.Nil(t, err)
	ensure.DeepEqual(t, strings.TrimSpace(string(out)), "$(id) ; ls")
}

func TestCorruptImageFails(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	c, _ := ta.app.hub.subscribe(alice.ID, chat.ID)
	id := ta.upload(alice, chat, "broken.jpg", append([]byte("\xff\xd8\xff\xe0"), randData(t, 500)...))
	pinged(c)
	ta.runMedia()
	f := ta.file(id)
	ensure.DeepEqual(t, f.Status, statusFailed)
	ensure.DeepEqual(t, f.Attempts, int64(1), "a converter failure is not retried")
	ensure.True(t, pinged(c))
	ensure.True(t, ta.objectMissing(fileKey(f, "converted")))
	page := ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, page, `href="/f/`+idStr(id)+`/o"`)
	ensure.StringContains(t, page, `download="broken.jpg"`)
	ensure.DeepEqual(t, ta.get(filePath(id, "c"), ta.as(alice)).Code, http.StatusNotFound)
}

// failing wraps the fake S3 transport and fails the first n GETs of objects.
func failGets(ta *testApp, n int) (restore func()) {
	real := ta.app.s3.hc
	var mu sync.Mutex
	ta.app.s3.hc = &http.Client{Transport: stubTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		fail := r.Method == http.MethodGet && n > 0
		if fail {
			n--
		}
		mu.Unlock()
		if fail {
			return stubResponse(http.StatusServiceUnavailable, "slow down"), nil
		}
		return real.Transport.RoundTrip(r)
	})}
	return func() { ta.app.s3.hc = real }
}

func TestTransientErrorsRetry(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	ctx := t.Context()

	// two failures, then success
	id := ta.upload(alice, chat, "a.png", pngOf(t, 10, 10))
	restore := failGets(ta, 2)
	ensure.True(t, ta.app.mediaOnce(ctx))
	ensure.DeepEqual(t, ta.file(id).Status, statusPending)
	ta.runMedia()
	restore()
	f := ta.file(id)
	ensure.DeepEqual(t, f.Status, statusReady)
	ensure.DeepEqual(t, f.Attempts, int64(3))

	// three failures give up
	id = ta.upload(alice, chat, "b.png", pngOf(t, 10, 10))
	restore = failGets(ta, 100)
	ta.runMedia()
	restore()
	f = ta.file(id)
	ensure.DeepEqual(t, f.Status, statusFailed)
	ensure.DeepEqual(t, f.Attempts, int64(3))
}

func TestWorkerStartupRecovery(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	ctx, cancel := context.WithCancel(t.Context())
	ta.app.ctx = ctx
	id := ta.upload(alice, chat, "a.png", pngOf(t, 10, 10))
	// a previous process died in the middle of this file
	_, err := ta.app.db.ExecContext(ctx, "UPDATE files SET status = 'processing' WHERE id = ?", id)
	ensure.Nil(t, err)
	stale := filepath.Join(ta.app.tmpDir(), "f1-old")
	ensure.Nil(t, os.MkdirAll(stale, 0o700))

	ensure.Nil(t, ta.app.startMedia())
	_, err = os.Stat(stale)
	ensure.True(t, os.IsNotExist(err), "old temp files are removed")
	deadline := time.Now().Add(20 * time.Second)
	for ta.file(id).Status != statusReady {
		ensure.True(t, time.Now().Before(deadline), "the worker did not finish the file: "+ta.file(id).Status)
		time.Sleep(20 * time.Millisecond)
	}

	// a new upload wakes a waiting worker
	id2 := ta.upload(alice, chat, "b.png", pngOf(t, 10, 10))
	for ta.file(id2).Status != statusReady {
		ensure.True(t, time.Now().Before(deadline), "the worker was not woken")
		time.Sleep(20 * time.Millisecond)
	}

	// the workers stop on shutdown
	cancel()
	done := make(chan struct{})
	go func() { ta.app.bg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not stop")
	}
}

func TestWorkersClaimEachFileOnce(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	const n = 20
	data := pngOf(t, 8, 8)
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = ta.upload(alice, chat, "x.png", data)
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	ta.app.beforeConvert = func(f File) {
		mu.Lock()
		seen[f.ID]++
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { ta.runMedia() })
	}
	wg.Wait()
	ensure.DeepEqual(t, len(seen), n)
	for _, id := range ids {
		ensure.DeepEqual(t, seen[id], 1)
		ensure.DeepEqual(t, ta.file(id).Status, statusReady)
	}
}

func TestDeleteWhileConverting(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	ctx := t.Context()
	id := ta.upload(alice, chat, "a.png", pngOf(t, 10, 10))
	f := ta.file(id)
	var msgID int64
	ensure.Nil(t, ta.app.db.QueryRowContext(ctx, "SELECT id FROM messages WHERE file_id = ?", id).Scan(&msgID))

	ta.app.beforeConvert = func(File) {
		w := ta.post(messagePath(chat.ID, msgID)+"/delete", nil, ta.as(alice))
		ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	}
	ta.runMedia()
	ta.app.beforeConvert = nil

	ensure.DeepEqual(t, countRows(t, ta, "files"), 0)
	ensure.DeepEqual(t, countRows(t, ta, "messages"), 0)
	ensure.True(t, ta.objectMissing(fileKey(f, "converted")), "nothing is left behind")
	ensure.True(t, ta.objectMissing(fileKey(f, "original")))
}

func TestDeleteConvertedImage(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	ctx := t.Context()
	id := ta.upload(alice, chat, "a.png", pngOf(t, 10, 10))
	ta.runMedia()
	f := ta.file(id)
	ensure.False(t, ta.objectMissing(fileKey(f, "converted")))
	var msgID int64
	ensure.Nil(t, ta.app.db.QueryRowContext(ctx, "SELECT id FROM messages WHERE file_id = ?", id).Scan(&msgID))
	ta.post(messagePath(chat.ID, msgID)+"/delete", nil, ta.as(alice))
	ensure.True(t, ta.objectMissing(fileKey(f, "converted")))
	ensure.True(t, ta.objectMissing(fileKey(f, "original")))
}

func TestKindMismatchFails(t *testing.T) {
	ta, alice, chat := mediaSetup(t)
	id := ta.upload(alice, chat, "a.bin", []byte("just some text"))
	// the row claims an image, the bytes say otherwise
	_, err := ta.app.db.ExecContext(t.Context(),
		"UPDATE files SET kind = 'image', status = 'pending' WHERE id = ?", id)
	ensure.Nil(t, err)
	ta.runMedia()
	ensure.DeepEqual(t, ta.file(id).Status, statusFailed)
}

// fakeObjects lists the keys stored in the fake S3.
func (ta *testApp) fakeObjects() []string {
	ta.t.Helper()
	root := filepath.Join(ta.app.fakeS3.dir, "objects")
	var keys []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			keys = append(keys, rel)
		}
		return nil
	})
	return keys
}
