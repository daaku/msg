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
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/daaku/serr"
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
	case converts && f.Status == statusReady:
		// Task 018 renders the converted video here.
		return fileDownload(f)
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

func (a *app) convertVideo(ctx context.Context, dir, inFormat string) (mediaResult, error) {
	// Task 018 implements this.
	return mediaResult{}, permanent("video conversion is not available")
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
