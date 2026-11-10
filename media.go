package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/daaku/serr"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// Uploads go from the browser straight to S3 as a multipart upload with
// presigned part URLs. This file is a trust boundary: nothing the client
// says about a file's size or type is believed. The size is checked against
// what S3 actually holds, and the type comes from the first bytes.

const (
	partSize       = 8 << 20 // S3 needs at least 5 MiB for all but the last part
	partURLExpiry  = 6 * time.Hour
	staleUploadAge = 24 * time.Hour
	sniffLength    = 4096
	maxFileName    = 200

	kindImage   = "image"
	kindVideo   = "video"
	kindOther   = "other"
	kindPreview = "preview"

	statusUploading  = "uploading"
	statusPending    = "pending"
	statusProcessing = "processing"
	statusReady      = "ready"
	statusFailed     = "failed"

	cacheForever = "private, max-age=31536000, immutable"
)

func fileKey(f File, variant string) string { return f.S3Prefix + "/" + variant }

// sniffMedia decides how a file is treated from its first bytes. It returns
// the kind and the explicit format name the converters are told to use. The
// file name and the browser's content type play no part.
func sniffMedia(head []byte) (kind, format string) {
	has := func(off int, s string) bool {
		return len(head) >= off+len(s) && string(head[off:off+len(s)]) == s
	}
	switch {
	case has(0, "\xff\xd8\xff"):
		return kindImage, "jpeg"
	case has(0, "\x89PNG\r\n\x1a\n"):
		return kindImage, "png"
	case has(0, "GIF87a"), has(0, "GIF89a"):
		return kindImage, "gif"
	case has(0, "RIFF") && has(8, "WEBP"):
		return kindImage, "webp"
	case has(0, "RIFF") && has(8, "AVI "):
		return kindVideo, "avi"
	case has(0, "\x1a\x45\xdf\xa3"):
		return kindVideo, "matroska"
	case has(4, "ftyp") && len(head) >= 12:
		switch string(head[8:12]) {
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1":
			return kindImage, "heic"
		case "avif", "avis":
			return kindImage, "avif"
		case "M4A ", "M4B ":
			return kindOther, ""
		}
		return kindVideo, "mov"
	}
	return kindOther, ""
}

// cleanFileName keeps the base name, drops control characters and cuts the
// result to maxFileName bytes on a rune boundary.
func cleanFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	for len(name) > maxFileName {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "file"
	}
	return name
}

// fileSnippet is the one line description of a file message.
func fileSnippet(kind, name string) string {
	switch kind {
	case kindImage:
		return "📷 Photo"
	case kindVideo:
		return "🎬 Video"
	}
	return "📎 " + name
}

// JSON endpoints

func apiError(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := http.StatusInternalServerError, "Something went wrong."
	if he, ok := errors.AsType[*httpError](err); ok {
		code, msg = he.code, he.msg
	} else {
		fmt.Fprintf(os.Stderr, "%s %s: %+v\n", r.Method, r.URL.Path, err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// api adapts a handler that reads a JSON body into in and answers JSON.
func api[In any](f func(r *http.Request, in In) (any, error)) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		var in In
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		if err := dec.Decode(&in); err != nil {
			apiError(w, r, httpErr(http.StatusBadRequest, "The request was not understood."))
			return nil
		}
		out, err := f(r, in)
		if err != nil {
			apiError(w, r, err)
			return nil
		}
		return writeJSON(w, out)
	}
}

func (a *app) wakeMedia() {
	select {
	case a.mediaWake <- struct{}{}:
	default:
	}
}

// lockFile serialises work on one file within this process, so two
// concurrent completes of the same upload cannot both run.
func (a *app) lockFile(id int64) (unlock func()) {
	v, _ := a.fileLocks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

type uploadInitIn struct {
	Chat int64  `json:"chat"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type uploadInitOut struct {
	File     int64    `json:"file"`
	PartSize int64    `json:"partSize"`
	URLs     []string `json:"urls"`
}

func partCount(size int64) int { return int((size + partSize - 1) / partSize) }

func (a *app) uploadInit(r *http.Request, in uploadInitIn) (any, error) {
	ctx := r.Context()
	user := userFrom(ctx)
	chat, member, _, err := a.chatAccess(ctx, user, in.Chat)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, errNotFound
	}
	if in.Size < 1 {
		return nil, httpErr(http.StatusBadRequest, "The file is empty.")
	}
	if in.Size > a.cfg.MaxUploadSize {
		return nil, httpErr(http.StatusRequestEntityTooLarge,
			"The file is larger than the "+fmtBytes(a.cfg.MaxUploadSize)+" limit.")
	}
	name := cleanFileName(in.Name)
	prefix := hex.EncodeToString(randBytes(16))
	key := prefix + "/original"
	// Originals are only ever served as opaque downloads, whatever they are.
	uploadID, err := a.s3.createMultipart(ctx, key, http.Header{
		"Content-Type":        {"application/octet-stream"},
		"Content-Disposition": {"attachment; filename*=UTF-8''" + s3Escape(name, false)},
		"Cache-Control":       {cacheForever},
	})
	if err != nil {
		return nil, err
	}
	file, err := a.q.FileCreate(ctx, FileCreateParams{
		ChatID:    chat.ID,
		UserID:    user.ID,
		Name:      name,
		S3Prefix:  prefix,
		UploadID:  uploadID,
		Size:      in.Size,
		CreatedAt: a.now().Unix(),
	})
	if err != nil {
		_ = a.s3.abortMultipart(context.WithoutCancel(ctx), key, uploadID)
		return nil, serr.Wrap(err)
	}
	out := uploadInitOut{File: file.ID, PartSize: partSize}
	for n := 1; n <= partCount(in.Size); n++ {
		out.URLs = append(out.URLs, a.s3.presignPart(key, uploadID, n, partURLExpiry))
	}
	return out, nil
}

type uploadFileIn struct {
	File int64 `json:"file"`
}

var okJSON = map[string]bool{"ok": true}

// dropUpload aborts the multipart upload and removes the row.
func (a *app) dropUpload(ctx context.Context, f File) {
	ctx = context.WithoutCancel(ctx)
	if err := a.s3.abortMultipart(ctx, fileKey(f, "original"), f.UploadID); err != nil {
		fmt.Fprintf(os.Stderr, "abort upload %d: %v\n", f.ID, err)
	}
	if _, err := a.q.FileDeleteUploading(ctx, f.ID); err != nil {
		fmt.Fprintf(os.Stderr, "delete upload %d: %v\n", f.ID, err)
	}
}

// checkParts verifies that S3 holds exactly the parts a file of the declared
// size is made of.
func checkParts(parts []s3Part, size int64) bool {
	n := partCount(size)
	if len(parts) != n {
		return false
	}
	var sum int64
	for i, p := range parts {
		if p.Number != i+1 || p.Size < 1 || (i < n-1 && p.Size != partSize) {
			return false
		}
		sum += p.Size
	}
	return sum == size
}

func (a *app) uploadComplete(r *http.Request, in uploadFileIn) (any, error) {
	ctx := r.Context()
	user := userFrom(ctx)
	defer a.lockFile(in.File)()

	file, err := a.q.FileByID(ctx, in.File)
	if errors.Is(err, sql.ErrNoRows) || (err == nil &&
		(file.UserID != user.ID || file.Status != statusUploading)) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, serr.Wrap(err)
	}
	// The user must still be a member now, not only when the upload began.
	chat, member, _, err := a.chatAccess(ctx, user, file.ChatID)
	if err != nil || !member {
		a.dropUpload(ctx, file)
		return nil, errNotFound
	}

	key := fileKey(file, "original")
	parts, err := a.s3.listParts(ctx, key, file.UploadID)
	if err != nil {
		return nil, err
	}
	if !checkParts(parts, file.Size) {
		a.dropUpload(ctx, file)
		return nil, httpErr(http.StatusBadRequest, "The upload does not match the file.")
	}
	if err := a.s3.completeMultipart(ctx, key, file.UploadID, parts); err != nil {
		return nil, err
	}

	rc, err := a.s3.get(ctx, key, fmt.Sprintf("bytes=0-%d", sniffLength-1))
	if err != nil {
		return nil, err
	}
	head, err := io.ReadAll(io.LimitReader(rc, sniffLength))
	rc.Close()
	if err != nil {
		return nil, serr.Wrap(err)
	}
	kind, _ := sniffMedia(head)
	status := statusReady
	if kind == kindImage || kind == kindVideo {
		status = statusPending
	}

	err = a.tx(ctx, func(q *Queries) error {
		n, err := q.FileCompleteUpload(ctx, FileCompleteUploadParams{
			ID: file.ID, Kind: kind, Status: status,
		})
		if err != nil {
			return serr.Wrap(err)
		}
		if n == 0 {
			return errNotFound
		}
		us := UserStatsAddParams{BytesUploaded: file.Size}
		chatStats := ChatStatsAddParams{BytesUploaded: file.Size}
		switch kind {
		case kindImage:
			us.ImagesUploaded, chatStats.ImagesUploaded = 1, 1
		case kindVideo:
			us.VideosUploaded, chatStats.VideosUploaded = 1, 1
		default:
			us.FilesUploaded, chatStats.FilesUploaded = 1, 1
		}
		if err := a.statUser(ctx, q, file.UserID, us); err != nil {
			return err
		}
		return a.statChat(ctx, q, file.ChatID, chatStats)
	})
	if err != nil {
		return nil, err
	}
	_, err = a.createMessage(ctx, chat, user.ID, "", sql.NullInt64{Int64: file.ID, Valid: true})
	if err != nil {
		return nil, err
	}
	a.wakeMedia()
	return okJSON, nil
}

func (a *app) uploadAbort(r *http.Request, in uploadFileIn) (any, error) {
	ctx := r.Context()
	defer a.lockFile(in.File)()
	file, err := a.q.FileByID(ctx, in.File)
	if err == nil && file.UserID == userFrom(ctx).ID && file.Status == statusUploading {
		a.dropUpload(ctx, file)
	}
	return okJSON, nil
}

// serving

// fileRedirect sends the browser to a signed S3 URL for one variant of a
// file and counts the bytes. The browser caches the redirect, so a device
// asks once per window per file and the count approximates real downloads.
func (a *app) fileRedirect(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return errNotFound
	}
	file, err := a.q.FileByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return serr.Wrap(err)
	}
	if _, _, _, err := a.chatAccess(ctx, user, file.ChatID); err != nil {
		return err
	}
	var size int64
	variant := ""
	switch v := r.PathValue("variant"); {
	case v == "o" && file.Status != statusUploading && file.Size > 0:
		variant, size = "original", file.Size
	case v == "c" && file.Status == statusReady && file.ConvSize > 0:
		variant, size = "converted", file.ConvSize
	case v == "p" && file.Status == statusReady && file.PosterSize > 0:
		variant, size = "poster", file.PosterSize
	default:
		return errNotFound
	}
	u, maxAge := a.s3.presignStable(fileKey(file, variant), a.cfg.S3URLValidity)
	err = a.tx(ctx, func(q *Queries) error {
		if err := a.statUser(ctx, q, user.ID, UserStatsAddParams{BytesDownloaded: size}); err != nil {
			return err
		}
		return a.statChat(ctx, q, file.ChatID, ChatStatsAddParams{BytesDownloaded: size})
	})
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int64(maxAge.Seconds())))
	http.Redirect(w, r, u, http.StatusFound)
	return nil
}

func filePath(id int64, variant string) string {
	return fmt.Sprintf("%s%d/%s", pFile, id, variant)
}

// filesFor loads the files attached to the given messages in one query.
func (a *app) filesFor(ctx context.Context, rows []MessagesLatestRow) (map[int64]File, error) {
	var ids []int64
	for _, row := range rows {
		if row.Message.FileID.Valid {
			ids = append(ids, row.Message.FileID.Int64)
		}
	}
	out := map[int64]File{}
	if len(ids) == 0 {
		return out, nil
	}
	files, err := a.q.FilesByIDs(ctx, ids)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	for _, f := range files {
		out[f.ID] = f
	}
	return out, nil
}

// fileNode renders the attachment of a message.
func fileNode(f File) g.Node {
	converts := f.Kind == kindImage || f.Kind == kindVideo
	switch {
	case converts && (f.Status == statusPending || f.Status == statusProcessing):
		return h.Div(h.Class("file-processing"),
			h.Span(h.Class("file-name"), g.Text(f.Name)),
			h.Span(h.Class("file-note"), g.Text("Processing…")))
	case f.Kind == kindImage && f.Status == statusReady:
		// width and height reserve the space, so nothing shifts on load
		src := filePath(f.ID, "c")
		return h.A(h.Class("media"), h.Href(src), h.Target("_blank"), h.Rel("noopener"),
			h.Img(h.ID(fmt.Sprintf("f%d", f.ID)), h.Src(src),
				h.Width(strconv.FormatInt(f.Width, 10)), h.Height(strconv.FormatInt(f.Height, 10)),
				h.Alt(f.Name), h.Loading("lazy"), g.Attr("decoding", "async")))
	case f.Kind == kindVideo && f.Status == statusReady:
		// preload none: nothing is downloaded until it is played
		return h.Video(h.ID(fmt.Sprintf("f%d", f.ID)), h.Class("media"),
			h.Controls(), h.PlaysInline(), h.Preload("none"),
			h.Poster(filePath(f.ID, "p")),
			h.Width(strconv.FormatInt(f.Width, 10)), h.Height(strconv.FormatInt(f.Height, 10)),
			h.Src(filePath(f.ID, "c")))
	}
	return fileDownload(f)
}

// fileDownload is a link to the original: for other files, and for media
// whose conversion failed.
func fileDownload(f File) g.Node {
	return h.A(h.Class("file-download"), h.Href(filePath(f.ID, "o")), h.Download(f.Name),
		icon("download"),
		h.Span(h.Class("file-name"), g.Text(f.Name)),
		h.Span(h.Class("file-note"), g.Text(fmtBytes(f.Size))))
}

// deleteFile removes a file row and then its objects. The database is the
// source of truth: an S3 failure is logged and leaves a harmless orphan.
func (a *app) deleteFile(ctx context.Context, q *Queries, f File) (cleanup func(), err error) {
	if err := q.FileDelete(ctx, f.ID); err != nil {
		return nil, serr.Wrap(err)
	}
	return func() {
		ctx := context.WithoutCancel(ctx)
		for _, variant := range []string{"original", "converted", "poster"} {
			if err := a.s3.del(ctx, fileKey(f, variant)); err != nil {
				fmt.Fprintf(os.Stderr, "delete object %s: %v\n", fileKey(f, variant), err)
			}
		}
	}, nil
}

// cleanStaleUploads drops uploads that were started but never finished.
func (a *app) cleanStaleUploads(ctx context.Context) {
	files, err := a.q.FilesStaleUploads(ctx, a.now().Add(-staleUploadAge).Unix())
	if err != nil {
		fmt.Fprintf(os.Stderr, "stale uploads: %+v\n", serr.Wrap(err))
		return
	}
	for _, f := range files {
		unlock := a.lockFile(f.ID)
		a.dropUpload(ctx, f)
		unlock()
	}
}

// startJanitor runs the periodic cleanup until shutdown.
func (a *app) startJanitor() {
	a.bg.Go(func() {
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()
		for {
			a.cleanStaleUploads(a.ctx)
			select {
			case <-tick.C:
			case <-a.ctx.Done():
				return
			}
		}
	})
}

// conversion worker

const (
	mediaWorkers     = 2
	mediaMaxAttempts = 3
	magickTimeout    = 2 * time.Minute
	execStderrCap    = 64 << 10
)

// permanentError marks a failure that retrying cannot fix: the converter
// rejected the input, or timed out on it.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(format string, args ...any) error {
	return &permanentError{serr.WrapSkip(fmt.Errorf(format, args...), 1)}
}

func isPermanent(err error) bool {
	_, ok := errors.AsType[*permanentError](err)
	return ok
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf []byte
	max int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

// exec runs an external converter and returns its standard output. It is
// the only way such a program is started: arguments are an argv slice, never
// a shell line, the environment is minimal, the working directory is the
// job's temp directory and there is always a timeout. Any failure is
// permanent for the file being converted.
func (a *app) exec(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"TMPDIR=" + dir,
		"LC_ALL=C",
	}
	cmd.WaitDelay = 5 * time.Second
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: execStderrCap}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(string(stderr.buf))
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, permanent("%s: %w: %s", name, err, tail)
	}
	return stdout.Bytes(), nil
}

// mediaOutput is one converted object to store.
type mediaOutput struct {
	variant     string // "converted" or "poster"
	path        string
	contentType string
}

type mediaResult struct {
	outputs  []mediaOutput
	width    int
	height   int
	duration int // seconds, video only
}

var imageTypes = map[string]string{
	"webp": "image/webp",
	"avif": "image/avif",
	"jpeg": "image/jpeg",
}

// magickInputs are the only formats ImageMagick is ever asked to decode.
var magickInputs = map[string]bool{
	"jpeg": true, "png": true, "gif": true, "webp": true, "heic": true, "avif": true,
}

// magickConvert converts dir/inName, which must be of inFormat, to the
// configured image format at dir/outName, shrinking it to maxEdge on the
// long side. The input format is always given explicitly: letting
// ImageMagick pick a decoder from the file's content is how its dangerous
// coders are reached.
func (a *app) magickConvert(ctx context.Context, dir, inFormat, inName, outName string, maxEdge int, firstFrame bool) (w, h int, err error) {
	if !magickInputs[inFormat] {
		return 0, 0, permanent("unsupported image format %q", inFormat)
	}
	outFormat := a.cfg.ImageFormat
	animated := !firstFrame && outFormat == "webp" && (inFormat == "gif" || inFormat == "webp")
	in := inFormat + ":" + inName
	if !animated {
		in += "[0]" // only webp output can hold more than one frame
	}
	args := []string{
		"-limit", "memory", "512MiB", "-limit", "map", "1GiB",
		"-limit", "disk", "4GiB", "-limit", "time", "120",
		in,
	}
	if animated {
		args = append(args, "-coalesce")
	}
	args = append(args,
		"-auto-orient",
		"-resize", fmt.Sprintf("%dx%d>", maxEdge, maxEdge),
		"+profile", "!icc,*", // drop EXIF, GPS and XMP, keep the colour profile
		"-quality", "82",
		outFormat+":"+outName)
	if _, err := a.exec(ctx, dir, magickTimeout, "magick", args...); err != nil {
		return 0, 0, err
	}
	out, err := a.exec(ctx, dir, magickTimeout, "magick", "identify",
		"-format", "%w %h\n", outFormat+":"+outName+"[0]")
	if err != nil {
		return 0, 0, err
	}
	if _, err := fmt.Sscanf(string(out), "%d %d", &w, &h); err != nil || w < 1 || h < 1 {
		return 0, 0, permanent("unreadable image size %q", out)
	}
	return w, h, nil
}

func (a *app) convertImage(ctx context.Context, dir, inFormat string) (mediaResult, error) {
	w, h, err := a.magickConvert(ctx, dir, inFormat, "in", "out", a.cfg.ImageResolution, false)
	if err != nil {
		return mediaResult{}, err
	}
	return mediaResult{
		outputs: []mediaOutput{{"converted", filepath.Join(dir, "out"), imageTypes[a.cfg.ImageFormat]}},
		width:   w,
		height:  h,
	}, nil
}

// videoInputs are the only demuxers ffmpeg is ever told to use. ffmpeg never
// chooses one itself: playlist formats (HLS, concat) can make it read local
// files or fetch URLs, and forcing the demuxer keeps them out of reach.
var videoInputs = map[string]bool{"mov": true, "matroska": true, "avi": true}

const ffmpegTimeout = 60 * time.Minute

// videoArgs is the ffmpeg command line that converts dir/in to out.mp4.
func (a *app) videoArgs(inFormat string) []string {
	n := a.cfg.VideoResolution
	// Cap the long edge, keep the aspect ratio, never upscale, and keep
	// both dimensions even. Rotation is applied before this filter.
	scale := fmt.Sprintf(
		"scale='if(gt(iw,ih),trunc(min(%d,iw)/2)*2,-2)':'if(gt(iw,ih),-2,trunc(min(%d,ih)/2)*2)'", n, n)
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", inFormat, "-i", "in",
		"-map", "0:v:0", "-map", "0:a:0?", // audio is optional
		"-vf", scale,
	}
	if a.cfg.VideoFormat == "av1" {
		args = append(args, "-c:v", "libsvtav1", "-preset", "8", "-crf", "35", "-pix_fmt", "yuv420p10le")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "medium", "-crf", "23", "-pix_fmt", "yuv420p")
	}
	return append(args,
		"-c:a", "aac", "-b:a", "128k", "-ac", "2",
		"-map_metadata", "-1", // drops container metadata such as the GPS location
		"-movflags", "+faststart", // playback can start before the download ends
		"-f", "mp4", "out.mp4")
}

func (a *app) convertVideo(ctx context.Context, dir, inFormat string) (mediaResult, error) {
	if !videoInputs[inFormat] {
		return mediaResult{}, permanent("unsupported video format %q", inFormat)
	}
	if _, err := a.exec(ctx, dir, ffmpegTimeout, "ffmpeg", a.videoArgs(inFormat)...); err != nil {
		return mediaResult{}, err
	}

	// Probe our own output, not the user's file.
	out, err := a.exec(ctx, dir, time.Minute, "ffprobe", "-v", "error", "-f", "mov",
		"-select_streams", "v:0", "-show_entries", "stream=width,height:format=duration",
		"-of", "json", "out.mp4")
	if err != nil {
		return mediaResult{}, err
	}
	var probe struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil || len(probe.Streams) != 1 ||
		probe.Streams[0].Width < 1 || probe.Streams[0].Height < 1 {
		return mediaResult{}, permanent("unreadable video details %q", out)
	}
	seconds, _ := strconv.ParseFloat(probe.Format.Duration, 64)
	duration := max(int(seconds+0.5), 1)

	// The poster is a frame of the converted video, in the image format.
	_, err = a.exec(ctx, dir, time.Minute, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-y", "-f", "mov", "-i", "out.mp4", "-frames:v", "1", "-c:v", "png", "-f", "image2", "poster.png")
	if err != nil {
		return mediaResult{}, err
	}
	_, _, err = a.magickConvert(ctx, dir, "png", "poster.png", "poster", a.cfg.ImageResolution, true)
	if err != nil {
		return mediaResult{}, err
	}
	return mediaResult{
		outputs: []mediaOutput{
			{"converted", filepath.Join(dir, "out.mp4"), "video/mp4"},
			{"poster", filepath.Join(dir, "poster"), imageTypes[a.cfg.ImageFormat]},
		},
		width:    probe.Streams[0].Width,
		height:   probe.Streams[0].Height,
		duration: duration,
	}, nil
}

func (a *app) tmpDir() string { return filepath.Join(a.cfg.StateDir, "tmp") }

// processFile converts one claimed file and stores the results.
func (a *app) processFile(ctx context.Context, f File) error {
	if err := os.MkdirAll(a.tmpDir(), 0o700); err != nil {
		return serr.Wrap(err)
	}
	dir, err := os.MkdirTemp(a.tmpDir(), fmt.Sprintf("f%d-", f.ID))
	if err != nil {
		return serr.Wrap(err)
	}
	defer os.RemoveAll(dir)

	// download the original
	rc, err := a.s3.get(ctx, fileKey(f, "original"), "")
	if err != nil {
		return err
	}
	in, err := os.OpenFile(filepath.Join(dir, "in"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		rc.Close()
		return serr.Wrap(err)
	}
	_, err = io.Copy(in, rc)
	rc.Close()
	if cerr := in.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return serr.Wrap(err)
	}

	// decide the format from the bytes, never from the name
	head := make([]byte, sniffLength)
	file, err := os.Open(filepath.Join(dir, "in"))
	if err != nil {
		return serr.Wrap(err)
	}
	n, _ := io.ReadFull(file, head)
	file.Close()
	kind, format := sniffMedia(head[:n])
	if kind != f.Kind {
		return permanent("file %d is %s, not %s", f.ID, kind, f.Kind)
	}

	if a.beforeConvert != nil {
		a.beforeConvert(f)
	}
	var res mediaResult
	if kind == kindImage {
		res, err = a.convertImage(ctx, dir, format)
	} else {
		res, err = a.convertVideo(ctx, dir, format)
	}
	if err != nil {
		return err
	}

	// store the outputs
	arg := FileSetReadyParams{
		ID:       f.ID,
		Width:    int64(res.width),
		Height:   int64(res.height),
		Duration: int64(res.duration),
	}
	var stored []string
	removeStored := func() {
		for _, key := range stored {
			if err := a.s3.del(context.WithoutCancel(ctx), key); err != nil {
				fmt.Fprintf(os.Stderr, "delete object %s: %v\n", key, err)
			}
		}
	}
	for _, out := range res.outputs {
		file, err := os.Open(out.path)
		if err != nil {
			removeStored()
			return serr.Wrap(err)
		}
		stat, err := file.Stat()
		if err == nil {
			key := fileKey(f, out.variant)
			stored = append(stored, key)
			err = a.s3.put(ctx, key, file, stat.Size(), http.Header{
				"Content-Type":        {out.contentType},
				"Cache-Control":       {cacheForever},
				"Content-Disposition": {"inline"},
			})
		}
		file.Close()
		if err != nil {
			removeStored()
			return serr.Wrap(err)
		}
		if out.variant == "poster" {
			arg.PosterType, arg.PosterSize = out.contentType, stat.Size()
		} else {
			arg.ConvType, arg.ConvSize = out.contentType, stat.Size()
		}
	}
	changed, err := a.q.FileSetReady(ctx, arg)
	if err != nil {
		removeStored()
		return serr.Wrap(err)
	}
	if changed == 0 {
		// The message was deleted while we worked. Leave nothing behind.
		removeStored()
		return nil
	}
	a.notifyChat(ctx, f.ChatID)
	return nil
}

// mediaOnce claims and processes one pending file. It reports whether there
// was one.
func (a *app) mediaOnce(ctx context.Context) bool {
	f, err := a.q.FileClaim(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "media claim: %+v\n", serr.Wrap(err))
		}
		return false
	}
	err = a.processFile(ctx, f)
	if err == nil {
		return true
	}
	if ctx.Err() != nil {
		// Shutting down: the file stays processing and is reset at the
		// next start.
		return false
	}
	status := statusFailed
	if !isPermanent(err) && f.Attempts < mediaMaxAttempts {
		status = statusPending
	}
	fmt.Fprintf(os.Stderr, "media file %d (attempt %d, now %s): %v\n", f.ID, f.Attempts, status, err)
	if _, err := a.q.FileSetStatus(ctx, FileSetStatusParams{ID: f.ID, Status: status}); err != nil {
		fmt.Fprintf(os.Stderr, "media file %d: %+v\n", f.ID, serr.Wrap(err))
	}
	if status == statusFailed {
		a.notifyChat(ctx, f.ChatID) // the placeholder becomes a download
	}
	return true
}

// startMedia recovers from a previous run and starts the workers.
func (a *app) startMedia() error {
	if err := os.RemoveAll(a.tmpDir()); err != nil {
		return serr.Wrap(err)
	}
	if err := a.q.FilesResetProcessing(a.ctx); err != nil {
		return serr.Wrap(err)
	}
	for _, name := range []string{"magick", "ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			fmt.Printf("warning: %s not found; media that needs it will not be converted\n", name)
		}
	}
	for range mediaWorkers {
		a.bg.Go(func() {
			tick := time.NewTicker(time.Minute)
			defer tick.Stop()
			for {
				for a.mediaOnce(a.ctx) {
				}
				select {
				case <-a.mediaWake:
				case <-tick.C:
				case <-a.ctx.Done():
					return
				}
			}
		})
	}
	return nil
}

// link previews

const (
	previewTimeout    = 15 * time.Second
	previewMaxURL     = 2000
	previewMaxHTML    = 1 << 20
	previewMaxImage   = 10 << 20
	previewImageEdge  = 1200
	previewRedirects  = 5
	previewUserAgent  = "Mozilla/5.0 (compatible; msg-preview/1.0)"
	previewTitleRunes = 200
	previewDescRunes  = 300
	previewSiteRunes  = 60
)

// Addresses the preview fetcher must never connect to, on top of what the
// netip.Addr predicates below cover.
var previewBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 reaches IPv4 addresses
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), // 6to4 embeds IPv4 addresses
}

// publicAddr reports whether ip is an ordinary public internet address.
func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, p := range previewBlocked {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// previewAllowStrict is the production rule: public addresses, web ports.
func previewAllowStrict(ap netip.AddrPort) bool {
	return publicAddr(ap.Addr()) && (ap.Port() == 80 || ap.Port() == 443)
}

var errPreviewBlocked = errors.New("preview: address not allowed")

// newPreviewClient builds the only HTTP client that may fetch a URL chosen
// by a user. The check runs in the dialer's Control function, on the address
// actually being connected to, after DNS resolution and for every attempt
// and redirect. That is what defeats DNS rebinding; checking the host name
// before the request would not.
func (a *app) newPreviewClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !a.previewAllow(ap) {
				return errPreviewBlocked
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: previewTimeout,
		Transport: &http.Transport{
			Proxy:                  nil, // never through a proxy: it would dial for us
			DialContext:            dialer.DialContext,
			DisableKeepAlives:      true,
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  10 * time.Second,
			MaxResponseHeaderBytes: 64 << 10,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= previewRedirects {
				return errors.New("preview: too many redirects")
			}
			_, err := checkPreviewURL(req.URL.String())
			return err
		},
	}
}

// checkPreviewURL accepts only plain http and https URLs.
func checkPreviewURL(raw string) (*url.URL, error) {
	if len(raw) > previewMaxURL {
		return nil, errors.New("preview: URL too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("preview: bad URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("preview: URL not allowed")
	}
	return u, nil
}

// previewGet fetches u and returns the response when it is a 2xx.
func (a *app) previewGet(ctx context.Context, u *url.URL, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", previewUserAgent)
	req.Header.Set("Accept", accept)
	res, err := a.previewClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		res.Body.Close()
		return nil, fmt.Errorf("preview: status %d", res.StatusCode)
	}
	return res, nil
}

type previewData struct {
	Title       string
	Description string
	SiteName    string
	ImageURL    string
}

// cleanText collapses whitespace and cuts to max runes.
func cleanText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = strings.TrimSpace(string([]rune(s)[:max]))
	}
	return s
}

// parsePreview reads the head of an HTML document.
func parsePreview(r io.Reader, final *url.URL) previewData {
	meta := map[string]string{}
	title := ""
	z := html.NewTokenizer(r)
loop:
	for {
		switch z.Next() {
		case html.ErrorToken:
			break loop
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "head" {
				break loop
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "body":
				break loop
			case "title":
				if title == "" && z.Next() == html.TextToken {
					title = string(z.Text())
				}
			case "meta":
				var key, content string
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					switch string(k) {
					case "property", "name":
						if key == "" {
							key = strings.ToLower(strings.TrimSpace(string(v)))
						}
					case "content":
						content = string(v)
					}
				}
				if _, seen := meta[key]; key != "" && !seen && strings.TrimSpace(content) != "" {
					meta[key] = content
				}
			}
		}
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := meta[k]; v != "" {
				return v
			}
		}
		return ""
	}
	d := previewData{
		Title:       cleanText(first("og:title", "twitter:title"), previewTitleRunes),
		Description: cleanText(first("og:description", "twitter:description", "description"), previewDescRunes),
		SiteName:    cleanText(first("og:site_name"), previewSiteRunes),
	}
	if d.Title == "" {
		d.Title = cleanText(title, previewTitleRunes)
	}
	if d.SiteName == "" {
		d.SiteName = cleanText(strings.TrimPrefix(final.Hostname(), "www."), previewSiteRunes)
	}
	if img := strings.TrimSpace(first("og:image", "twitter:image")); img != "" {
		if ref, err := url.Parse(img); err == nil {
			if u, err := checkPreviewURL(final.ResolveReference(ref).String()); err == nil {
				d.ImageURL = u.String()
			}
		}
	}
	return d
}

// fetchPreview loads the page at rawURL and extracts its preview data.
func (a *app) fetchPreview(ctx context.Context, rawURL string) (previewData, error) {
	u, err := checkPreviewURL(rawURL)
	if err != nil {
		return previewData{}, err
	}
	res, err := a.previewGet(ctx, u, "text/html,application/xhtml+xml")
	if err != nil {
		return previewData{}, err
	}
	defer res.Body.Close()
	contentType := res.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType != "text/html" && mediaType != "application/xhtml+xml" {
		return previewData{}, fmt.Errorf("preview: not HTML: %q", mediaType)
	}
	body, err := charset.NewReader(io.LimitReader(res.Body, previewMaxHTML), contentType)
	if err != nil {
		return previewData{}, err
	}
	return parsePreview(body, res.Request.URL), nil
}

type previewImage struct {
	prefix      string
	contentType string
	size        int64
	width       int
	height      int
}

// fetchPreviewImage downloads, converts and stores a preview image. Any
// problem means the preview is shown without an image.
func (a *app) fetchPreviewImage(ctx context.Context, rawURL string) (*previewImage, error) {
	u, err := checkPreviewURL(rawURL)
	if err != nil {
		return nil, err
	}
	res, err := a.previewGet(ctx, u, "image/*")
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, previewMaxImage+1))
	res.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(data) > previewMaxImage {
		return nil, errors.New("preview: image too large")
	}
	// The bytes decide what it is. The response content type is ignored.
	kind, format := sniffMedia(data)
	switch {
	case kind != kindImage:
		return nil, errors.New("preview: not an image")
	case format != "jpeg" && format != "png" && format != "gif" && format != "webp":
		return nil, fmt.Errorf("preview: image format %s not accepted", format)
	}
	if err := os.MkdirAll(a.tmpDir(), 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(a.tmpDir(), "p-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "in"), data, 0o600); err != nil {
		return nil, err
	}
	w, h, err := a.magickConvert(ctx, dir, format, "in", "out", previewImageEdge, true)
	if err != nil {
		return nil, err
	}
	out, err := os.Open(filepath.Join(dir, "out"))
	if err != nil {
		return nil, err
	}
	defer out.Close()
	stat, err := out.Stat()
	if err != nil {
		return nil, err
	}
	img := &previewImage{
		prefix:      hex.EncodeToString(randBytes(16)),
		contentType: imageTypes[a.cfg.ImageFormat],
		size:        stat.Size(),
		width:       w,
		height:      h,
	}
	err = a.s3.put(ctx, img.prefix+"/converted", out, img.size, http.Header{
		"Content-Type":        {img.contentType},
		"Cache-Control":       {cacheForever},
		"Content-Disposition": {"inline"},
	})
	if err != nil {
		return nil, err
	}
	return img, nil
}

var errPreviewGone = errors.New("preview: no longer wanted")

// processPreview fetches one claimed preview. The row is already marked
// failed, so every early return leaves it that way.
func (a *app) processPreview(ctx context.Context, p Preview) {
	fail := func(err error) {
		if ctx.Err() == nil {
			host := ""
			if u, perr := url.Parse(p.Url); perr == nil {
				host = u.Hostname()
			}
			fmt.Fprintf(os.Stderr, "preview for message %d (%s): %v\n", p.MessageID, host, err)
		}
	}
	msg, err := a.q.MessageByID(ctx, p.MessageID)
	if err != nil {
		return // deleted meanwhile
	}
	data, err := a.fetchPreview(ctx, p.Url)
	if err != nil {
		fail(err)
		return
	}
	var img *previewImage
	if data.ImageURL != "" {
		if img, err = a.fetchPreviewImage(ctx, data.ImageURL); err != nil {
			fail(err)
		}
	}
	removeImage := func() {
		if img != nil {
			_ = a.s3.del(context.WithoutCancel(ctx), img.prefix+"/converted")
		}
	}
	if data.Title == "" && img == nil {
		fail(errors.New("nothing to show"))
		return
	}
	err = a.tx(ctx, func(q *Queries) error {
		arg := PreviewSetReadyParams{
			MessageID:   p.MessageID,
			Url:         p.Url,
			Title:       data.Title,
			Description: data.Description,
			SiteName:    data.SiteName,
		}
		if img != nil {
			f, err := q.FileCreatePreview(ctx, FileCreatePreviewParams{
				ChatID:    msg.ChatID,
				UserID:    msg.UserID,
				S3Prefix:  img.prefix,
				ConvType:  img.contentType,
				ConvSize:  img.size,
				Width:     int64(img.width),
				Height:    int64(img.height),
				CreatedAt: a.now().Unix(),
			})
			if err != nil {
				return serr.Wrap(err)
			}
			arg.FileID = sql.NullInt64{Int64: f.ID, Valid: true}
		}
		n, err := q.PreviewSetReady(ctx, arg)
		if err != nil {
			return serr.Wrap(err)
		}
		if n == 0 {
			// The message was deleted or edited to another URL meanwhile.
			return errPreviewGone
		}
		return nil
	})
	if err != nil {
		removeImage()
		if !errors.Is(err, errPreviewGone) {
			fail(err)
		}
		return
	}
	a.notifyChat(ctx, msg.ChatID)
}

// previewOnce claims and processes one pending preview. It reports whether
// there was one.
func (a *app) previewOnce(ctx context.Context) bool {
	p, err := a.q.PreviewClaim(ctx)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "preview claim: %+v\n", serr.Wrap(err))
		}
		return false
	}
	a.processPreview(ctx, p)
	return true
}

func (a *app) wakePreviews() {
	select {
	case a.previewWake <- struct{}{}:
	default:
	}
}

// startPreviews runs the preview worker until shutdown.
func (a *app) startPreviews() {
	a.bg.Go(func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			for a.previewOnce(a.ctx) {
			}
			select {
			case <-a.previewWake:
			case <-tick.C:
			case <-a.ctx.Done():
				return
			}
		}
	})
}

// requestPreview queues a preview for the first URL of a text message.
func (a *app) requestPreview(ctx context.Context, msg Message) {
	u := firstURL(msg.Body)
	if u == "" || msg.FileID.Valid {
		return
	}
	if _, err := checkPreviewURL(u); err != nil {
		return
	}
	err := a.q.PreviewCreate(ctx, PreviewCreateParams{
		MessageID: msg.ID, Url: u, CreatedAt: a.now().Unix(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview create: %+v\n", serr.Wrap(err))
		return
	}
	a.wakePreviews()
}

// dropPreview removes a message's preview and its image, inside q. The
// returned cleanup deletes the image object and must run after commit.
func (a *app) dropPreview(ctx context.Context, q *Queries, messageID int64) (cleanup func(), err error) {
	cleanup = func() {}
	p, err := q.PreviewByMessage(ctx, messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return cleanup, nil
	}
	if err != nil {
		return cleanup, serr.Wrap(err)
	}
	if err := q.PreviewDelete(ctx, messageID); err != nil {
		return cleanup, serr.Wrap(err)
	}
	if !p.FileID.Valid {
		return cleanup, nil
	}
	f, err := q.FileByID(ctx, p.FileID.Int64)
	if err != nil {
		return cleanup, serr.Wrap(err)
	}
	return a.deleteFile(ctx, q, f)
}

// refreshPreview brings the preview in line with an edited message.
func (a *app) refreshPreview(ctx context.Context, msg Message) {
	cleanup := func() {}
	changed := false
	err := a.tx(ctx, func(q *Queries) error {
		p, err := q.PreviewByMessage(ctx, msg.ID)
		if err == nil && p.Url == firstURL(msg.Body) {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return serr.Wrap(err)
		}
		changed = true
		cleanup, err = a.dropPreview(ctx, q, msg.ID)
		return err
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview refresh: %+v\n", err)
		return
	}
	cleanup()
	if changed {
		a.requestPreview(ctx, msg)
	}
}

// previewsFor loads the ready previews of the given messages in one query.
func (a *app) previewsFor(ctx context.Context, rows []MessagesLatestRow) (map[int64]PreviewsForMessagesRow, error) {
	out := map[int64]PreviewsForMessagesRow{}
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.Message.ID
	}
	previews, err := a.q.PreviewsForMessages(ctx, ids)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	for _, p := range previews {
		out[p.Preview.MessageID] = p
	}
	return out, nil
}

// previewNode renders a ready preview card. The link is the URL from the
// message, never anything taken from the fetched page.
func previewNode(id string, p PreviewsForMessagesRow) g.Node {
	var img g.Node
	if p.Preview.FileID.Valid && p.Width > 0 {
		img = h.Img(h.ID(fmt.Sprintf("f%d", p.Preview.FileID.Int64)),
			h.Src(filePath(p.Preview.FileID.Int64, "c")),
			h.Width(strconv.FormatInt(p.Width, 10)), h.Height(strconv.FormatInt(p.Height, 10)),
			h.Alt(""), h.Loading("lazy"), g.Attr("decoding", "async"))
	}
	return h.A(h.ID(id+"-preview"), h.Class("preview"), h.Href(p.Preview.Url),
		h.Target("_blank"), h.Rel("noopener noreferrer nofollow"),
		img,
		h.Span(h.Class("preview-site"), g.Text(p.Preview.SiteName)),
		g.If(p.Preview.Title != "", h.Strong(h.Class("preview-title"), g.Text(p.Preview.Title))),
		g.If(p.Preview.Description != "", h.Span(h.Class("preview-desc"), g.Text(p.Preview.Description))))
}
