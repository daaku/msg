package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"

	"github.com/daaku/ensure"
)

func TestStatsZeroWithoutRow(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	us, err := ta.app.userStats(ctx, 42)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, us, UserStat{UserID: 42})
	cs, err := ta.app.chatStats(ctx, 7)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, cs, ChatStat{ChatID: 7})
}

func TestStatsAccumulate(t *testing.T) {
	ta := newTestApp(t)
	a := ta.app
	ctx := t.Context()
	db := a.db
	_, err := db.ExecContext(ctx,
		"INSERT INTO users (id, username, handle, created_at) VALUES (1, 'a', x'01', 1)")
	ensure.Nil(t, err)
	_, err = db.ExecContext(ctx,
		"INSERT INTO chats (id, name, created_by, created_at, last_message_at) VALUES (5, 'g', 1, 1, 1)")
	ensure.Nil(t, err)

	ensure.Nil(t, a.statUser(ctx, a.q, 1, UserStatsAddParams{
		MessagesSent: 1, ImagesUploaded: 2, VideosUploaded: 3, FilesUploaded: 4,
		BytesUploaded: 5, BytesDownloaded: 6, PushesSent: 7, PushesOpened: 8,
		InvitesSent: 9,
	}))
	ensure.Nil(t, a.tx(ctx, func(q *Queries) error {
		return a.statUser(ctx, q, 1, UserStatsAddParams{
			UserID:       999, // ignored, the userID argument wins
			MessagesSent: 10, ImagesUploaded: 20, VideosUploaded: 30, FilesUploaded: 40,
			BytesUploaded: 50, BytesDownloaded: 60, PushesSent: 70, PushesOpened: 80,
			InvitesSent: 90,
		})
	}))
	us, err := a.userStats(ctx, 1)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, us, UserStat{
		UserID: 1, MessagesSent: 11, ImagesUploaded: 22, VideosUploaded: 33,
		FilesUploaded: 44, BytesUploaded: 55, BytesDownloaded: 66, PushesSent: 77,
		PushesOpened: 88, InvitesSent: 99,
	})

	for range 2 {
		ensure.Nil(t, a.statChat(ctx, a.q, 5, ChatStatsAddParams{
			ImagesUploaded: 1, VideosUploaded: 2, FilesUploaded: 3,
			BytesUploaded: 4, BytesDownloaded: 5,
		}))
	}
	cs, err := a.chatStats(ctx, 5)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, cs, ChatStat{
		ChatID: 5, ImagesUploaded: 2, VideosUploaded: 4, FilesUploaded: 6,
		BytesUploaded: 8, BytesDownloaded: 10,
	})
}

// statsSnapshot reads every counter of every user and chat, keyed as
// "user <name> <column>" and "chat <id> <column>".
func statsSnapshot(t *testing.T, ta *testApp) map[string]int64 {
	t.Helper()
	ta.app.bg.Wait() // pushes are counted in the background
	out := map[string]int64{}
	rows, err := ta.app.db.QueryContext(t.Context(), `SELECT users.username, messages_sent,
		images_uploaded, videos_uploaded, files_uploaded, bytes_uploaded, bytes_downloaded,
		pushes_sent, pushes_opened, invites_sent
		FROM user_stats JOIN users ON users.id = user_stats.user_id`)
	ensure.Nil(t, err)
	userCols := []string{"messages_sent", "images_uploaded", "videos_uploaded", "files_uploaded",
		"bytes_uploaded", "bytes_downloaded", "pushes_sent", "pushes_opened", "invites_sent"}
	for rows.Next() {
		var name string
		v := make([]int64, len(userCols))
		ensure.Nil(t, rows.Scan(&name, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7], &v[8]))
		for i, c := range userCols {
			out["user "+name+" "+c] = v[i]
		}
	}
	ensure.Nil(t, rows.Err())
	rows.Close()
	rows, err = ta.app.db.QueryContext(t.Context(), `SELECT chat_id, images_uploaded,
		videos_uploaded, files_uploaded, bytes_uploaded, bytes_downloaded FROM chat_stats`)
	ensure.Nil(t, err)
	chatCols := []string{"images_uploaded", "videos_uploaded", "files_uploaded", "bytes_uploaded", "bytes_downloaded"}
	for rows.Next() {
		var id int64
		v := make([]int64, len(chatCols))
		ensure.Nil(t, rows.Scan(&id, &v[0], &v[1], &v[2], &v[3], &v[4]))
		for i, c := range chatCols {
			out[fmt.Sprintf("chat %d %s", id, c)] = v[i]
		}
	}
	ensure.Nil(t, rows.Err())
	rows.Close()
	return out
}

// statsMoved runs action and checks that exactly the expected counters
// moved, each by exactly the expected amount.
func statsMoved(t *testing.T, ta *testApp, name string, want map[string]int64, action func()) {
	t.Helper()
	before := statsSnapshot(t, ta)
	action()
	after := statsSnapshot(t, ta)
	got := map[string]int64{}
	for k, v := range after {
		if d := v - before[k]; d != 0 {
			got[k] = d
		}
	}
	if want == nil {
		want = map[string]int64{}
	}
	ensure.DeepEqual(t, got, want, name)
}

func TestStatsWiring(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group", "invite")
	bob := ta.newUser("bob")
	boss := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "Team", bob)
	c := func(col string) string { return fmt.Sprintf("chat %d %s", chat.ID, col) }
	ok := func(w interface{ Result() *http.Response }, code int) {
		t.Helper()
		ensure.DeepEqual(t, w.Result().StatusCode, code)
	}

	statsMoved(t, ta, "text message", map[string]int64{"user alice messages_sent": 1}, func() {
		ok(ta.post(chatPath(chat.ID)+"/send", url.Values{"body": {"hi"}}, ta.as(alice)), http.StatusSeeOther)
	})

	pngData := pngOf(t, 64, 48)
	s := int64(len(pngData))
	var pngID int64
	statsMoved(t, ta, "png upload", map[string]int64{
		"user alice messages_sent": 1, "user alice images_uploaded": 1, "user alice bytes_uploaded": s,
		c("images_uploaded"): 1, c("bytes_uploaded"): s,
	}, func() { pngID = ta.upload(alice, chat, "a.png", pngData) })

	mp4 := int64(len(tinyMP4))
	statsMoved(t, ta, "mp4 upload", map[string]int64{
		"user alice messages_sent": 1, "user alice videos_uploaded": 1, "user alice bytes_uploaded": mp4,
		c("videos_uploaded"): 1, c("bytes_uploaded"): mp4,
	}, func() { ta.upload(alice, chat, "a.mp4", tinyMP4) })

	other := randData(t, 1000)
	statsMoved(t, ta, "other upload", map[string]int64{
		"user alice messages_sent": 1, "user alice files_uploaded": 1, "user alice bytes_uploaded": 1000,
		c("files_uploaded"): 1, c("bytes_uploaded"): 1000,
	}, func() { ta.upload(alice, chat, "a.bin", other) })

	statsMoved(t, ta, "original download", map[string]int64{
		"user bob bytes_downloaded": s, c("bytes_downloaded"): s,
	}, func() { ok(ta.get(filePath(pngID, "o"), ta.as(bob)), http.StatusFound) })

	if _, err := exec.LookPath("magick"); err == nil {
		statsMoved(t, ta, "conversion", nil, func() { ta.runMedia() })
		conv := ta.file(pngID).ConvSize
		ensure.True(t, conv > 0)
		statsMoved(t, ta, "converted download", map[string]int64{
			"user bob bytes_downloaded": conv, c("bytes_downloaded"): conv,
		}, func() { ok(ta.get(filePath(pngID, "c"), ta.as(bob)), http.StatusFound) })
	} else {
		t.Log("magick is not installed: the converted download row is not checked")
	}

	ta.subscribe(bob, "https://push.example/bob")
	statsMoved(t, ta, "push delivered", map[string]int64{
		"user alice messages_sent": 1, "user bob pushes_sent": 1,
	}, func() {
		ok(ta.post(chatPath(chat.ID)+"/send", url.Values{"body": {"ping"}}, ta.as(alice)), http.StatusSeeOther)
	})

	statsMoved(t, ta, "push opened", map[string]int64{"user bob pushes_opened": 1}, func() {
		ok(ta.post(pPushOpened, nil, ta.as(bob)), http.StatusNoContent)
	})

	statsMoved(t, ta, "invite", map[string]int64{"user alice invites_sent": 1}, func() {
		ok(ta.post(pInvites, url.Values{"username": {"newbie"}}, ta.as(alice)), http.StatusSeeOther)
	})

	statsMoved(t, ta, "failures", nil, func() {
		// an upload whose bytes do not match
		out := ta.uploadInit(alice, chat, "bad", 5)
		ta.uploadParts(out.URLs, []byte("1234"))
		ok(ta.apiPost(pUploadComplete, uploadFileIn{File: out.File}, ta.as(alice)), http.StatusBadRequest)
		// invites for a taken and a pending username
		ok(ta.post(pInvites, url.Values{"username": {"bob"}}, ta.as(alice)), http.StatusSeeOther)
		ok(ta.post(pInvites, url.Values{"username": {"newbie"}}, ta.as(alice)), http.StatusSeeOther)
		ok(ta.post(pInvites, url.Values{"username": {"Bad Name"}}, ta.as(alice)), http.StatusSeeOther)
		// 404s on /f/
		ok(ta.get(filePath(99999, "o"), ta.as(bob)), http.StatusNotFound)
		ok(ta.get(filePath(pngID, "x"), ta.as(bob)), http.StatusNotFound)
		ok(ta.get(filePath(pngID, "p"), ta.as(bob)), http.StatusNotFound)
		ok(ta.get(filePath(pngID, "o"), ta.as(ta.newUser("carol"))), http.StatusNotFound)
	})

	m := ta.newMessage(chat, bob, "react to me")
	ta.sends()
	statsMoved(t, ta, "reaction and links", nil, func() {
		ensure.DeepEqual(t, react(ta, chat, m, "👍", alice), http.StatusSeeOther)
		ok(ta.post(pSettingsLink, nil, ta.as(alice)), http.StatusSeeOther)
		ok(ta.post(adminUserPath(bob.ID)+"/link", nil, ta.as(boss)), http.StatusSeeOther)
	})
}

func TestStatsVisibility(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	boss := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "Team", bob)
	for range 3 {
		ta.newMessage(chat, alice, "hi")
	}
	ta.upload(alice, chat, "a.bin", randData(t, 1536))
	msgs := "<dt>Messages sent</dt><dd>4</dd>"

	// own stats on settings, nobody else's
	body := ta.get(pSettings, ta.as(alice)).Body.String()
	ensure.StringContains(t, body, `<section id="stats"`)
	ensure.StringContains(t, body, msgs)
	ensure.StringContains(t, body, "<dt>Uploaded</dt><dd>1.5 KB</dd>")
	ensure.StringContains(t, body, "Estimated from media links opened")
	ensure.StringContains(t, ta.get(pSettings, ta.as(bob)).Body.String(), "<dt>Messages sent</dt><dd>0</dd>")

	// a user's stats page is admin only
	ensure.DeepEqual(t, ta.get(adminUserPath(alice.ID), ta.as(bob)).Code, http.StatusNotFound)
	body = ta.get(adminUserPath(alice.ID), ta.as(boss)).Body.String()
	ensure.StringContains(t, body, msgs)
	// and the admin list has the columns
	body = ta.get(pAdmin, ta.as(boss)).Body.String()
	ensure.StringContains(t, body, `<td data-label="Messages sent">4</td>`)
	ensure.StringContains(t, body, `<td data-label="Uploaded">1.5 KB</td>`)
	ensure.StringContains(t, body, `<td data-label="Downloaded">0 B</td>`)
	ensure.StringContains(t, body, `href="/admin/groups"`)

	// group info: members and admins
	info := chatPath(chat.ID) + "/info"
	files := "<dt>Other files uploaded</dt><dd>1</dd>"
	for _, u := range []*testUser{alice, bob, boss} {
		body := ta.get(info, ta.as(u)).Body.String()
		ensure.StringContains(t, body, `<section id="stats"`, u.Username)
		ensure.StringContains(t, body, files, u.Username)
		ensure.StringDoesNotContain(t, body, "Messages sent", u.Username)
	}
	ensure.DeepEqual(t, ta.get(info, ta.as(carol)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(info).Code, http.StatusSeeOther)

	// all groups: admin only, never direct messages
	dm := ta.newDM(alice, bob)
	ta.upload(bob, dm, "d.bin", randData(t, 10))
	ensure.DeepEqual(t, ta.get(pAdminGroups, ta.as(alice)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(pAdminGroups).Code, http.StatusSeeOther)
	w := ta.get(pAdminGroups, ta.as(boss))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	body = w.Body.String()
	ensure.StringContains(t, body, `href="/admin"`)
	row := body[strings.Index(body, fmt.Sprintf(`id="group-%d"`, chat.ID)):]
	ensure.StringContains(t, row, `<a href="`+info+`">Team</a>`)
	ensure.StringContains(t, row, `<td data-label="Members">2</td>`)
	ensure.StringContains(t, row, `<td data-label="Creator">alice</td>`)
	ensure.StringContains(t, row, `<td data-label="Other files">1</td>`)
	ensure.StringContains(t, row, `<td data-label="Uploaded">1.5 KB</td>`)
	ensure.StringDoesNotContain(t, body, fmt.Sprintf(`id="group-%d"`, dm.ID))

	// direct messages: both participants, not an admin
	dmInfo := chatPath(dm.ID) + "/info"
	for _, u := range []*testUser{alice, bob} {
		body := ta.get(dmInfo, ta.as(u)).Body.String()
		ensure.StringContains(t, body, `<section id="stats"`, u.Username)
		ensure.StringContains(t, body, "<dt>Uploaded</dt><dd>10 B</dd>", u.Username)
	}
	ensure.DeepEqual(t, ta.get(dmInfo, ta.as(boss)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(dmInfo, ta.as(carol)).Code, http.StatusNotFound)
}

func TestStatsZeroRender(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	boss := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "Quiet")
	zeros := []string{"<dt>Images uploaded</dt><dd>0</dd>", "<dt>Videos uploaded</dt><dd>0</dd>",
		"<dt>Other files uploaded</dt><dd>0</dd>", "<dt>Uploaded</dt><dd>0 B</dd>", "<dt>Downloaded</dt><dd>0 B"}
	user := append([]string{"<dt>Messages sent</dt><dd>0</dd>", "<dt>Notifications sent</dt><dd>0</dd>",
		"<dt>Notifications opened</dt><dd>0</dd>", "<dt>Invites sent</dt><dd>0</dd>"}, zeros...)
	for _, s := range user {
		ensure.StringContains(t, ta.get(adminUserPath(boss.ID), ta.as(boss)).Body.String(), s)
	}
	body := ta.get(chatPath(chat.ID)+"/info", ta.as(alice)).Body.String()
	for _, s := range zeros {
		ensure.StringContains(t, body, s)
	}
	body = ta.get(pAdminGroups, ta.as(boss)).Body.String()
	ensure.StringContains(t, body, `<td data-label="Images">0</td>`)
	ensure.StringContains(t, body, `<td data-label="Downloaded">0 B</td>`)
	// the labels are in the stated order
	body = ta.get(pSettings, ta.as(alice)).Body.String()
	last := -1
	for _, l := range []string{"Messages sent", "Images uploaded", "Videos uploaded", "Other files uploaded",
		"<dt>Uploaded", "<dt>Downloaded", "Notifications sent", "Notifications opened", "Invites sent"} {
		i := strings.Index(body, l)
		ensure.True(t, i > last, l)
		last = i
	}
}
