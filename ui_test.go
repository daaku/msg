package main

import (
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/daaku/ensure"
)

func TestFmtBytes(t *testing.T) {
	cases := map[int64]string{
		0:         "0 B",
		999:       "999 B",
		1000:      "1000 B",
		1023:      "1023 B",
		1024:      "1 KB",
		1536:      "1.5 KB",
		1 << 20:   "1 MB",
		3 << 19:   "1.5 MB",
		100 << 20: "100 MB",
		1<<30 - 1: "1 GB",
		5 << 30:   "5 GB",
		5 << 40:   "5 TB",
	}
	for in, want := range cases {
		ensure.DeepEqual(t, fmtBytes(in), want, in)
	}
}

func TestFmtTime(t *testing.T) {
	ts := time.Date(2026, 3, 4, 23, 30, 0, 0, time.UTC)
	ensure.DeepEqual(t, fmtTime(ts, nil), "4 Mar 2026 23:30")
	loc := time.FixedZone("x", 2*60*60)
	ensure.DeepEqual(t, fmtTime(ts, loc), "5 Mar 2026 01:30")
}

func TestIcon(t *testing.T) {
	for name := range icons {
		var sb strings.Builder
		ensure.Nil(t, icon(name).Render(&sb))
		s := sb.String()
		ensure.StringContains(t, s, `class="icon icon-`+name+`"`)
		ensure.StringContains(t, s, "<path ")
	}
	defer ensure.PanicDeepEqual(t, "unknown icon: nope")
	icon("nope")
}

func TestPageShell(t *testing.T) {
	ta := newTestApp(t)
	body := ta.get(pSignIn).Body.String()
	ensure.StringContains(t, body, `<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content">`)
	ensure.StringContains(t, body, `href="`+assetPath("main.css")+`"`)
	ensure.StringContains(t, body, `src="`+assetPath("main.js")+`" defer`)
	ensure.StringDoesNotContain(t, body, "livereload")
}

func TestManifest(t *testing.T) {
	ta := newTestApp(t)
	w := ta.get(pAppWebManifest) // signed out
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.DeepEqual(t, w.Header().Get("Content-Type"), "application/manifest+json")
	var m struct {
		Name, ShortName, ID, StartURL, Scope, Display string
		Icons                                         []struct{ Src, Sizes, Type, Purpose string }
	}
	var raw map[string]any
	ensure.Nil(t, json.Unmarshal(w.Body.Bytes(), &raw))
	ensure.Nil(t, json.Unmarshal(w.Body.Bytes(), &m))
	ensure.DeepEqual(t, raw["display"], "standalone")
	ensure.DeepEqual(t, raw["start_url"], "/")
	ensure.DeepEqual(t, raw["scope"], "/")
	ensure.DeepEqual(t, raw["id"], "/")
	ensure.DeepEqual(t, raw["name"], "msg")
	ensure.DeepEqual(t, raw["short_name"], "msg")
	ensure.True(t, raw["theme_color"] != "" && raw["background_color"] != "")
	ensure.DeepEqual(t, len(m.Icons), 3)
	ensure.DeepEqual(t, m.Icons[2].Purpose, "maskable")
	for _, ic := range m.Icons {
		ensure.DeepEqual(t, ic.Type, "image/png")
		r := ta.get(ic.Src)
		ensure.DeepEqual(t, r.Code, http.StatusOK, ic.Src)
		ensure.DeepEqual(t, r.Header().Get("Content-Type"), "image/png", ic.Src)
		img, err := png.Decode(r.Body)
		ensure.Nil(t, err)
		ensure.DeepEqual(t, fmt.Sprintf("%dx%d", img.Bounds().Dx(), img.Bounds().Dy()), ic.Sizes)
	}
}

func TestPageShellInstall(t *testing.T) {
	ta := newTestApp(t)
	body := ta.get(pSignIn).Body.String()
	ensure.StringContains(t, body, `<link rel="manifest" href="/app.webmanifest">`)
	ensure.StringContains(t, body, `<link rel="apple-touch-icon" href="`+assetPath("apple-touch-icon.png")+`">`)
	ensure.StringContains(t, body, `<link rel="icon" type="image/png" href="`+assetPath("favicon.png")+`">`)
	ensure.StringContains(t, body, `<meta name="mobile-web-app-capable" content="yes">`)
	ensure.StringContains(t, body, `<meta name="apple-mobile-web-app-capable" content="yes">`)
	ensure.StringContains(t, body, `<meta name="apple-mobile-web-app-title" content="msg">`)
	ensure.StringContains(t, body, `<meta name="theme-color" content="#ffffff" media="(prefers-color-scheme: light)">`)
	ensure.StringContains(t, body, `<meta name="theme-color" content="#1e1e1c" media="(prefers-color-scheme: dark)">`)
}

func TestIconSizes(t *testing.T) {
	for name, size := range map[string]int{
		"icon-512.png": 512, "icon-192.png": 192, "apple-touch-icon.png": 180, "favicon.png": 32,
	} {
		f, err := assets.Open("assets/" + name)
		ensure.Nil(t, err, name)
		img, err := png.Decode(f)
		f.Close()
		ensure.Nil(t, err, name)
		ensure.DeepEqual(t, img.Bounds().Dx(), size, name)
		ensure.DeepEqual(t, img.Bounds().Dy(), size, name)
		// edge to edge: the corners are opaque
		_, _, _, alpha := img.At(0, 0).RGBA()
		ensure.DeepEqual(t, alpha, uint32(0xffff), name)
	}
}

func TestInstallHint(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	ensure.StringContains(t, ta.get(pSettings, ta.as(alice)).Body.String(),
		`<p class="install-hint muted">On iPhone and iPad: tap Share, then Add to Home Screen, and open the app from there to enable notifications.</p>`)
}
