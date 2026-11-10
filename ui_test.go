package main

import (
	"strings"
	"testing"
	"time"

	"github.com/daaku/ensure"
)

func TestFmtBytes(t *testing.T) {
	cases := map[int64]string{
		0:         "0 B",
		1023:      "1023 B",
		1024:      "1 KB",
		1536:      "1.5 KB",
		1 << 20:   "1 MB",
		3 << 19:   "1.5 MB",
		100 << 20: "100 MB",
		1<<30 - 1: "1 GB",
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
	ensure.StringContains(t, body, `<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">`)
	ensure.StringContains(t, body, `href="`+assetPath("main.css")+`"`)
	ensure.StringContains(t, body, `src="`+assetPath("main.js")+`" defer`)
	ensure.StringDoesNotContain(t, body, "livereload")
}
