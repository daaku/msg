package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daaku/ensure"
)

func videoSetup(t *testing.T) (*testApp, *testUser, Chat) {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe", "magick"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " is not installed")
		}
	}
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	return ta, alice, ta.newChat(alice, "Team")
}

// ffmpegFixture runs ffmpeg in a temp directory and returns the file made.
func ffmpegFixture(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("ffmpeg", append([]string{"-nostdin", "-loglevel", "error", "-y"},
		append(args, name)...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	ensure.Nil(t, err, string(out))
	b, err := os.ReadFile(filepath.Join(dir, name))
	ensure.Nil(t, err)
	return b
}

func landscapeMP4(t *testing.T, size string) []byte {
	return ffmpegFixture(t, "in.mp4",
		"-f", "lavfi", "-i", "testsrc2=size="+size+":rate=30:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-metadata", "location=+12.34+56.78/")
}

type probed struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		PixFmt    string `json:"pix_fmt"`
	} `json:"streams"`
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
}

func probe(t *testing.T, data []byte) probed {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v.mp4")
	ensure.Nil(t, os.WriteFile(path, data, 0o600))
	out, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", path).Output()
	ensure.Nil(t, err)
	var p probed
	ensure.Nil(t, json.Unmarshal(out, &p))
	return p
}

func (p probed) codecs() map[string]string {
	m := map[string]string{}
	for _, s := range p.Streams {
		m[s.CodecType] = s.CodecName
	}
	return m
}

func TestVideoConversionH264(t *testing.T) {
	ta, alice, chat := videoSetup(t)
	ensure.DeepEqual(t, ta.app.cfg.VideoFormat, "h264", "h264 is the default")
	in := landscapeMP4(t, "2560x1440")
	ensure.StringContains(t, probe(t, in).Format.Tags["location"], "+12.34")

	c, _ := ta.app.hub.subscribe(alice.ID, chat.ID)
	id := ta.upload(alice, chat, "holiday.mov", in)
	ensure.DeepEqual(t, ta.file(id).Kind, kindVideo)
	pinged(c)
	ta.runMedia()

	f := ta.file(id)
	ensure.DeepEqual(t, f.Status, statusReady)
	ensure.DeepEqual(t, f.ConvType, "video/mp4")
	ensure.DeepEqual(t, []int64{f.Width, f.Height, f.Duration}, []int64{1920, 1080, 2})
	ensure.True(t, pinged(c))

	out := ta.object(fileKey(f, "converted"))
	ensure.DeepEqual(t, f.ConvSize, int64(len(out)))
	p := probe(t, out)
	ensure.DeepEqual(t, p.codecs(), map[string]string{"video": "h264", "audio": "aac"})
	ensure.DeepEqual(t, []int{p.Streams[0].Width, p.Streams[0].Height}, []int{1920, 1080})
	ensure.DeepEqual(t, p.Streams[0].PixFmt, "yuv420p")
	_, hasLocation := p.Format.Tags["location"]
	ensure.False(t, hasLocation, "the GPS location is removed")

	// poster
	ensure.DeepEqual(t, f.PosterType, "image/webp")
	poster := ta.object(fileKey(f, "poster"))
	ensure.DeepEqual(t, f.PosterSize, int64(len(poster)))
	ensure.DeepEqual(t, strings.TrimSpace(identify(t, "webp", "%m %w %h", poster)), "WEBP 1920 1080")

	// rendering
	page := ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, page, `<video id="f`+idStr(id)+`" class="media" controls playsinline preload="none"`+
		` poster="/f/`+idStr(id)+`/p" width="1920" height="1080" src="/f/`+idStr(id)+`/c"></video>`)
	ensure.StringContains(t, page, `-file" class="msg-file msg-media"><video id="f`+idStr(id)+`"`)
	ensure.StringContains(t, page, `/c"></video><div id="m`)
	ensure.StringContains(t, ta.get(pHome, ta.as(alice)).Body.String(), "alice: 🎬 Video")

	// poster and converted redirects count their own sizes
	before, _ := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, ta.get(filePath(id, "p"), ta.as(alice)).Code, http.StatusFound)
	mid, _ := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, mid.BytesDownloaded-before.BytesDownloaded, f.PosterSize)
	ensure.DeepEqual(t, ta.get(filePath(id, "c"), ta.as(alice)).Code, http.StatusFound)
	after, _ := ta.stats(alice.ID, chat.ID)
	ensure.DeepEqual(t, after.BytesDownloaded-mid.BytesDownloaded, f.ConvSize)

	// deleting the message removes all three objects
	var msgID int64
	ensure.Nil(t, ta.app.db.QueryRowContext(t.Context(),
		"SELECT id FROM messages WHERE file_id = ?", id).Scan(&msgID))
	ta.post(messagePath(chat.ID, msgID)+"/delete", nil, ta.as(alice))
	for _, variant := range []string{"original", "converted", "poster"} {
		ensure.True(t, ta.objectMissing(fileKey(f, variant)), variant)
	}
}

func TestVideoConversionAV1PortraitNoAudio(t *testing.T) {
	ta, alice, chat := videoSetup(t)
	ta.app.cfg.VideoFormat = "av1"
	in := ffmpegFixture(t, "in.webm",
		"-f", "lavfi", "-i", "testsrc2=size=1080x2400:rate=30:duration=1",
		"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8")
	id := ta.upload(alice, chat, "portrait.webm", in)
	ta.runMedia()
	f := ta.file(id)
	ensure.DeepEqual(t, f.Status, statusReady)
	ensure.DeepEqual(t, []int64{f.Width, f.Height, f.Duration}, []int64{864, 1920, 1})
	p := probe(t, ta.object(fileKey(f, "converted")))
	ensure.DeepEqual(t, p.codecs(), map[string]string{"video": "av1"})
	ensure.DeepEqual(t, p.Streams[0].PixFmt, "yuv420p10le")
	ensure.DeepEqual(t, strings.TrimSpace(identify(t, "webp", "%w %h", ta.object(fileKey(f, "poster")))), "864 1920")
}

func TestVideoNotUpscaledAndEvenDimensions(t *testing.T) {
	ta, alice, chat := videoSetup(t)
	small := ta.upload(alice, chat, "small.mp4", landscapeMP4(t, "640x360"))
	// odd dimensions cannot be encoded as 4:2:0 and are rounded down
	odd := ta.upload(alice, chat, "odd.mkv", ffmpegFixture(t, "in.mkv",
		"-f", "lavfi", "-i", "testsrc2=size=641x361:rate=10:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv444p"))
	ta.runMedia()
	f := ta.file(small)
	ensure.DeepEqual(t, f.Status, statusReady)
	ensure.DeepEqual(t, []int64{f.Width, f.Height}, []int64{640, 360})
	f = ta.file(odd)
	ensure.DeepEqual(t, f.Status, statusReady)
	ensure.DeepEqual(t, []int64{f.Width, f.Height}, []int64{640, 360})
}

func TestVideoInputSafety(t *testing.T) {
	ta, alice, chat := videoSetup(t)
	ctx := t.Context()
	evil := []byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\n/etc/passwd\n#EXT-X-ENDLIST\n")

	// a playlist named like a video is just a file, and is never converted
	id := ta.upload(alice, chat, "evil.mp4", evil)
	ensure.DeepEqual(t, ta.file(id).Kind, kindOther)

	// even forced through the converter it is refused by the pinned demuxer
	dir := t.TempDir()
	ensure.Nil(t, os.WriteFile(filepath.Join(dir, "in"), evil, 0o600))
	for _, format := range []string{"mov", "matroska", "avi"} {
		_, err := ta.app.convertVideo(ctx, dir, format)
		ensure.True(t, isPermanent(err), format)
	}
	_, err := os.Stat(filepath.Join(dir, "out.mp4"))
	ensure.True(t, err != nil, "no output")

	// every ffmpeg call that opens the user's file pins the demuxer
	args := ta.app.videoArgs("mov")
	for i, a := range args {
		if a == "-i" {
			ensure.DeepEqual(t, args[i-2:i+2], []string{"-f", "mov", "-i", "in"})
		}
	}
	ensure.DeepEqual(t, args[0], "-nostdin")

	// formats outside the allow list never start a process
	t.Setenv("PATH", t.TempDir())
	for _, format := range []string{"hls", "concat", "lavfi", "mp4", "", "image2"} {
		_, err := ta.app.convertVideo(ctx, dir, format)
		ensure.True(t, isPermanent(err), format)
		ensure.StringContains(t, err.Error(), "unsupported video format")
	}
}

func TestVideoWithoutVideoStreamFails(t *testing.T) {
	ta, alice, chat := videoSetup(t)
	// audio only, in a container whose brand says video
	in := ffmpegFixture(t, "in.mp4", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "aac")
	id := ta.upload(alice, chat, "audio.mp4", in)
	ensure.DeepEqual(t, ta.file(id).Kind, kindVideo)
	ta.runMedia()
	ensure.DeepEqual(t, ta.file(id).Status, statusFailed)
	page := ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, page, `download="audio.mp4"`)
}

func TestVideoConversionCancelledOnShutdown(t *testing.T) {
	ta, alice, chat := videoSetup(t)
	id := ta.upload(alice, chat, "v.mp4", landscapeMP4(t, "640x360"))
	ctx, cancel := context.WithCancel(t.Context())
	ta.app.beforeConvert = func(File) { cancel() }
	ta.app.mediaOnce(ctx)
	// interrupted work is left as processing, with nothing stored
	f := ta.file(id)
	ensure.DeepEqual(t, f.Status, statusProcessing)
	ensure.True(t, ta.objectMissing(fileKey(f, "converted")))
	// and is picked up again after a restart
	ensure.Nil(t, ta.app.q.FilesResetProcessing(t.Context()))
	ensure.DeepEqual(t, ta.file(id).Status, statusPending)
	ta.app.beforeConvert = nil
	ta.runMedia()
	ensure.DeepEqual(t, ta.file(id).Status, statusReady)
}
