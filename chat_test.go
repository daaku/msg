package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daaku/ensure"
	g "maragu.dev/gomponents"
)

func idStr(id int64) string { return strconv.FormatInt(id, 10) }

func isMember(t *testing.T, ta *testApp, chatID, userID int64) bool {
	t.Helper()
	var n int
	ensure.Nil(t, ta.app.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM members WHERE chat_id = ? AND user_id = ?", chatID, userID).Scan(&n))
	return n == 1
}

func memberCount(t *testing.T, ta *testApp, chatID int64) int {
	t.Helper()
	var n int
	ensure.Nil(t, ta.app.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM members WHERE chat_id = ?", chatID).Scan(&n))
	return n
}

func setLastMessageAt(t *testing.T, ta *testApp, chatID, at int64) {
	t.Helper()
	_, err := ta.app.db.ExecContext(t.Context(),
		"UPDATE chats SET last_message_at = ? WHERE id = ?", at, chatID)
	ensure.Nil(t, err)
}

func TestGroupCreate(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")

	// the form lists the other users
	w := ta.get(pNew, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	body := w.Body.String()
	ensure.StringContains(t, body, `href="/"`)
	ensure.StringContains(t, body, `value="`+idStr(bob.ID)+`"`)
	ensure.StringContains(t, body, `value="`+idStr(carol.ID)+`"`)
	ensure.StringDoesNotContain(t, body, `value="`+idStr(alice.ID)+`"`)

	// needs the permission
	ensure.DeepEqual(t, ta.get(pNew, ta.as(bob)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.post(pNew, url.Values{"name": {"x"}}, ta.as(bob)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.post(pNew, url.Values{"name": {"x"}}).Code, http.StatusUnauthorized)
	ensure.DeepEqual(t, countRows(t, ta, "chats"), 0)

	// empty and overlong names are refused
	for _, name := range []string{"", "   ", strings.Repeat("é", 65)} {
		w = ta.post(pNew, url.Values{"name": {name}}, ta.as(alice))
		ensure.StringContains(t, ta.flashOf(w, ta.as(alice)), msgChatName)
	}
	ensure.DeepEqual(t, countRows(t, ta, "chats"), 0)

	// creator and selected users become members, unknown ids are ignored
	w = ta.post(pNew, url.Values{
		"name": {"  " + strings.Repeat("é", 64) + " "},
		"user": {idStr(bob.ID), "9999"},
	}, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther, w.Body.String())
	chats, err := ta.app.q.ChatsForUserWithLast(ctx, alice.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, len(chats), 1)
	chat := chats[0].Chat
	ensure.DeepEqual(t, w.Header().Get("Location"), chatPath(chat.ID))
	ensure.DeepEqual(t, chat.Name, strings.Repeat("é", 64))
	ensure.False(t, chat.IsDm)
	ensure.DeepEqual(t, chat.CreatedBy, alice.ID)
	ensure.DeepEqual(t, chat.LastMessageAt, testNow.Unix())
	ensure.True(t, isMember(t, ta, chat.ID, alice.ID))
	ensure.True(t, isMember(t, ta, chat.ID, bob.ID))
	ensure.False(t, isMember(t, ta, chat.ID, carol.ID))
	ensure.DeepEqual(t, memberCount(t, ta, chat.ID), 2)
	m, err := ta.app.q.MemberGet(ctx, MemberGetParams{ChatID: chat.ID, UserID: bob.ID})
	ensure.Nil(t, err)
	ensure.DeepEqual(t, m.AddedBy, alice.ID)

	// a malformed id is a bad request
	w = ta.post(pNew, url.Values{"name": {"x"}, "user": {"bob"}}, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusBadRequest)

	// an admin can create groups too
	admin := ta.newUser("boss", "admin")
	ensure.DeepEqual(t, ta.post(pNew, url.Values{"name": {"y"}}, ta.as(admin)).Code, http.StatusSeeOther)
}

func TestHomeListsOwnGroups(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	g1 := ta.newChat(alice, "first", bob)
	g2 := ta.newChat(alice, "second")
	g3 := ta.newChat(alice, "third", bob)
	other := ta.newChat(bob, "bobs own")
	setLastMessageAt(t, ta, g1.ID, testNow.Unix()+30)
	setLastMessageAt(t, ta, g2.ID, testNow.Unix()+10)
	setLastMessageAt(t, ta, g3.ID, testNow.Unix()+20)

	w := ta.get(pHome, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	body := w.Body.String()
	ensure.StringContains(t, body, `id="app"`)
	ensure.StringContains(t, body, `id="sidebar"`)
	ensure.StringContains(t, body, `id="pane"`)
	ensure.StringDoesNotContain(t, body, `data-chat=`)
	ensure.StringDoesNotContain(t, body, "has-chat")
	ensure.StringContains(t, body, "Select a group or a direct message")
	i1 := strings.Index(body, fmt.Sprintf(`id="chat-%d"`, g1.ID))
	i2 := strings.Index(body, fmt.Sprintf(`id="chat-%d"`, g2.ID))
	i3 := strings.Index(body, fmt.Sprintf(`id="chat-%d"`, g3.ID))
	ensure.True(t, i1 > 0 && i3 > i1 && i2 > i3, i1, i2, i3)
	ensure.StringDoesNotContain(t, body, fmt.Sprintf(`id="chat-%d"`, other.ID))
	ensure.StringContains(t, body, `href="/new"`)
	// sign out lives on the settings page
	ensure.StringDoesNotContain(t, body, `action="/sign-out"`)

	// ties are broken by id, newest first
	setLastMessageAt(t, ta, g1.ID, testNow.Unix())
	setLastMessageAt(t, ta, g2.ID, testNow.Unix())
	setLastMessageAt(t, ta, g3.ID, testNow.Unix())
	body = ta.get(pHome, ta.as(alice)).Body.String()
	i1 = strings.Index(body, fmt.Sprintf(`id="chat-%d"`, g1.ID))
	i2 = strings.Index(body, fmt.Sprintf(`id="chat-%d"`, g2.ID))
	i3 = strings.Index(body, fmt.Sprintf(`id="chat-%d"`, g3.ID))
	ensure.True(t, i3 < i2 && i2 < i1, i1, i2, i3)

	// bob sees his two and no create link
	body = ta.get(pHome, ta.as(bob)).Body.String()
	ensure.StringContains(t, body, fmt.Sprintf(`id="chat-%d"`, g1.ID))
	ensure.StringContains(t, body, fmt.Sprintf(`id="chat-%d"`, other.ID))
	ensure.StringDoesNotContain(t, body, fmt.Sprintf(`id="chat-%d"`, g2.ID))
	ensure.StringDoesNotContain(t, body, `href="/new"`)
}

func TestHomeEmptyState(t *testing.T) {
	ta := newTestApp(t)
	creator := ta.newUser("creator", "group")
	plain := ta.newUser("plain")
	body := ta.get(pHome, ta.as(creator)).Body.String()
	ensure.StringContains(t, body, `id="chats-empty"`)
	ensure.StringContains(t, body, `<a href="/new">`)
	body = ta.get(pHome, ta.as(plain)).Body.String()
	ensure.StringContains(t, body, `id="chats-empty"`)
	ensure.StringContains(t, body, `<a href="/dm">`)
	ensure.StringContains(t, body, "Someone needs to add you to a group.")
	ensure.StringDoesNotContain(t, body, `href="/new"`)
}

func TestGroupPageAccess(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	outsider := ta.newUser("outsider")
	admin := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "Team <b>", bob)

	for _, p := range []string{chatPath(chat.ID), chatPath(chat.ID) + "/info"} {
		for _, u := range []*testUser{alice, bob, admin} {
			w := ta.get(p, ta.as(u))
			ensure.DeepEqual(t, w.Code, http.StatusOK, p, u.Username)
			body := w.Body.String()
			ensure.StringContains(t, body, `data-chat="`+idStr(chat.ID)+`"`)
			ensure.StringContains(t, body, "has-chat")
			ensure.StringContains(t, body, `id="pane"`)
			ensure.StringContains(t, body, "Team &lt;b&gt;")
			ensure.StringDoesNotContain(t, body, "Team <b>")
		}
		ensure.DeepEqual(t, ta.get(p, ta.as(outsider)).Code, http.StatusNotFound, p)
		ensure.DeepEqual(t, ta.get(p).Code, http.StatusSeeOther, p)
	}
	for _, p := range []string{"/c/9999", "/c/9999/info", "/c/abc", "/c/abc/info", "/c/-1"} {
		ensure.DeepEqual(t, ta.get(p, ta.as(admin)).Code, http.StatusNotFound, p)
	}

	// the pane header links to the info page and home
	body := ta.get(chatPath(chat.ID), ta.as(bob)).Body.String()
	ensure.StringContains(t, body, `id="pane-head"`)
	ensure.StringContains(t, body, `href="`+chatPath(chat.ID)+`/info"`)
	ensure.StringContains(t, body, fmt.Sprintf(`id="chat-%d" class="chat-item group" href="%s" aria-current="page"`,
		chat.ID, chatPath(chat.ID)))
	ensure.StringContains(t, body, "<title>Team &lt;b&gt; · msg</title>")

	// the info page has a back link to the group and lists the members
	body = ta.get(chatPath(chat.ID)+"/info", ta.as(bob)).Body.String()
	ensure.StringContains(t, body, `class="back" href="`+chatPath(chat.ID)+`"`)
	ensure.StringContains(t, body, fmt.Sprintf(`id="member-%d"`, alice.ID))
	ensure.StringContains(t, body, fmt.Sprintf(`id="member-%d"`, bob.ID))
	ensure.StringDoesNotContain(t, body, fmt.Sprintf(`id="member-%d"`, outsider.ID))
	// a plain member manages nothing
	for _, s := range []string{"/rename", "/members/add", "/members/remove"} {
		ensure.StringDoesNotContain(t, body, `action="`+chatPath(chat.ID)+s+`"`)
	}

	// a manager sees the controls; the creator has no remove button
	body = ta.get(chatPath(chat.ID)+"/info", ta.as(alice)).Body.String()
	ensure.StringContains(t, body, `action="`+chatPath(chat.ID)+`/rename"`)
	ensure.StringContains(t, body, `id="add-members"`)
	ensure.StringContains(t, body, `value="`+idStr(outsider.ID)+`"`)
	ensure.StringContains(t, body, `name="user" value="`+idStr(bob.ID)+`"`)
	ensure.StringDoesNotContain(t, body, `name="user" value="`+idStr(alice.ID)+`"`)
	ensure.StringDoesNotContain(t, body, `id="join"`)

	// an admin who is not a member can join
	body = ta.get(chatPath(chat.ID)+"/info", ta.as(admin)).Body.String()
	ensure.StringContains(t, body, `id="join"`)
	w := ta.post(chatPath(chat.ID)+"/members/add", url.Values{"user": {idStr(admin.ID)}}, ta.as(admin))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.True(t, isMember(t, ta, chat.ID, admin.ID))
	ensure.StringDoesNotContain(t, ta.get(chatPath(chat.ID)+"/info", ta.as(admin)).Body.String(), `id="join"`)
}

func TestGroupManage(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	admin := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "team", bob)
	base := chatPath(chat.ID)
	info := base + "/info"

	name := func() string {
		c, err := ta.app.q.ChatByID(ctx, chat.ID)
		ensure.Nil(t, err)
		return c.Name
	}

	// creator with the permission can
	w := ta.post(base+"/rename", url.Values{"name": {" renamed "}}, ta.as(alice))
	ensure.DeepEqual(t, w.Header().Get("Location"), info)
	ensure.DeepEqual(t, name(), "renamed")
	w = ta.post(base+"/members/add", url.Values{"user": {idStr(carol.ID)}}, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, w.Header().Get("Location"), info)
	ensure.True(t, isMember(t, ta, chat.ID, carol.ID))
	w = ta.post(base+"/members/remove", url.Values{"user": {idStr(carol.ID)}}, ta.as(alice))
	ensure.DeepEqual(t, w.Header().Get("Location"), info)
	ensure.False(t, isMember(t, ta, chat.ID, carol.ID))

	// a bad name is refused
	w = ta.post(base+"/rename", url.Values{"name": {""}}, ta.as(alice))
	ensure.StringContains(t, ta.flashOf(w, ta.as(alice)), msgChatName)
	ensure.DeepEqual(t, name(), "renamed")

	// a plain member cannot
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{base + "/rename", url.Values{"name": {"bob was here"}}},
		{base + "/members/add", url.Values{"user": {idStr(carol.ID)}}},
		{base + "/members/remove", url.Values{"user": {idStr(alice.ID)}}},
	} {
		ensure.DeepEqual(t, ta.post(c.path, c.form, ta.as(bob)).Code, http.StatusNotFound, c.path)
		ensure.DeepEqual(t, ta.post(c.path, c.form, ta.as(carol)).Code, http.StatusNotFound, c.path)
		ensure.DeepEqual(t, ta.post(c.path, c.form).Code, http.StatusUnauthorized, c.path)
		ensure.DeepEqual(t, ta.post("/c/9999"+strings.TrimPrefix(c.path, base), c.form, ta.as(admin)).Code,
			http.StatusNotFound, c.path)
	}
	ensure.DeepEqual(t, name(), "renamed")
	ensure.False(t, isMember(t, ta, chat.ID, carol.ID))

	// admin can
	ensure.DeepEqual(t, ta.post(base+"/rename", url.Values{"name": {"by admin"}}, ta.as(admin)).Code,
		http.StatusSeeOther)
	ensure.DeepEqual(t, name(), "by admin")
	ensure.DeepEqual(t, ta.post(base+"/members/add", url.Values{"user": {idStr(carol.ID)}}, ta.as(admin)).Code,
		http.StatusSeeOther)
	ensure.True(t, isMember(t, ta, chat.ID, carol.ID))

	// add is idempotent and ignores unknown users
	w = ta.post(base+"/members/add", url.Values{"user": {idStr(carol.ID), idStr(bob.ID), "9999"}}, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, memberCount(t, ta, chat.ID), 3)

	// the creator cannot be removed, not even by an admin
	w = ta.post(base+"/members/remove", url.Values{"user": {idStr(alice.ID)}}, ta.as(admin))
	ensure.StringContains(t, ta.flashOf(w, ta.as(admin)), "cannot be removed")
	ensure.True(t, isMember(t, ta, chat.ID, alice.ID))

	// a removed member loses access
	ensure.DeepEqual(t, ta.get(base, ta.as(bob)).Code, http.StatusOK)
	ensure.DeepEqual(t, ta.post(base+"/members/remove", url.Values{"user": {idStr(bob.ID)}}, ta.as(alice)).Code,
		http.StatusSeeOther)
	ensure.DeepEqual(t, ta.get(base, ta.as(bob)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.get(info, ta.as(bob)).Code, http.StatusNotFound)
	ensure.StringDoesNotContain(t, ta.get(pHome, ta.as(bob)).Body.String(), fmt.Sprintf(`id="chat-%d"`, chat.ID))

	// a creator whose permission was removed cannot manage any more
	_, err := ta.app.db.ExecContext(ctx, "UPDATE users SET perm_group = FALSE WHERE id = ?", alice.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, ta.post(base+"/rename", url.Values{"name": {"nope"}}, ta.as(alice)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.post(base+"/members/add", url.Values{"user": {idStr(bob.ID)}}, ta.as(alice)).Code,
		http.StatusNotFound)
	ensure.DeepEqual(t, ta.post(base+"/members/remove", url.Values{"user": {idStr(carol.ID)}}, ta.as(alice)).Code,
		http.StatusNotFound)
	ensure.DeepEqual(t, name(), "by admin")
	// but can still view
	ensure.DeepEqual(t, ta.get(base, ta.as(alice)).Code, http.StatusOK)
	ensure.StringDoesNotContain(t, ta.get(info, ta.as(alice)).Body.String(), `action="`+base+`/rename"`)

	// the group permission alone does not let a member manage someone
	// else's group
	other := ta.newUser("other", "group")
	ta.post(base+"/members/add", url.Values{"user": {idStr(other.ID)}}, ta.as(admin))
	ensure.DeepEqual(t, ta.post(base+"/rename", url.Values{"name": {"nope"}}, ta.as(other)).Code, http.StatusNotFound)
}

func TestHeaderLinks(t *testing.T) {
	ta := newTestApp(t)
	plain := ta.newUser("plain")
	body := ta.get(pHome, ta.as(plain)).Body.String()
	ensure.StringContains(t, body, `href="/settings"`)
	for _, href := range []string{"/new", "/invites", "/admin"} {
		ensure.StringDoesNotContain(t, body, `href="`+href+`"`)
	}
	admin := ta.newUser("boss", "admin")
	body = ta.get(pHome, ta.as(admin)).Body.String()
	for _, href := range []string{"/new", "/invites", "/admin", "/settings"} {
		ensure.StringContains(t, body, `href="`+href+`"`)
	}
}

func TestDMStart(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	// no permissions at all
	alice := ta.newUser("alice")
	bob := ta.newUser("bob")

	// the picker lists the other users
	w := ta.get(pDM, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	body := w.Body.String()
	ensure.StringContains(t, body, "New message")
	ensure.StringContains(t, body, `href="/"`)
	ensure.StringContains(t, body, `name="user" value="`+idStr(bob.ID)+`"`)
	ensure.StringDoesNotContain(t, body, `name="user" value="`+idStr(alice.ID)+`"`)
	ensure.DeepEqual(t, ta.get(pDM).Code, http.StatusSeeOther)

	w = ta.post(pDM, url.Values{"user": {idStr(bob.ID)}}, ta.as(alice))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	chat, err := ta.app.q.ChatByDMKey(ctx, sql.NullString{
		String: fmt.Sprintf("%d:%d", alice.ID, bob.ID), Valid: true})
	ensure.Nil(t, err)
	ensure.DeepEqual(t, w.Header().Get("Location"), chatPath(chat.ID))
	ensure.True(t, chat.IsDm)
	ensure.DeepEqual(t, chat.Name, "")
	ensure.DeepEqual(t, chat.CreatedBy, alice.ID)
	ensure.DeepEqual(t, chat.LastMessageAt, testNow.Unix())
	ensure.DeepEqual(t, memberCount(t, ta, chat.ID), 2)
	ensure.True(t, isMember(t, ta, chat.ID, alice.ID))
	ensure.True(t, isMember(t, ta, chat.ID, bob.ID))

	// again from either side is the same chat
	for _, c := range []struct{ from, to *testUser }{{alice, bob}, {bob, alice}} {
		w = ta.post(pDM, url.Values{"user": {idStr(c.to.ID)}}, ta.as(c.from))
		ensure.DeepEqual(t, w.Header().Get("Location"), chatPath(chat.ID))
	}
	ensure.DeepEqual(t, countRows(t, ta, "chats"), 1)
	ensure.DeepEqual(t, ta.newDM(bob, alice).ID, chat.ID)

	// with yourself, a missing user or garbage is 404; signed out is 401
	for _, v := range []string{idStr(alice.ID), "9999", "", "bob"} {
		ensure.DeepEqual(t, ta.post(pDM, url.Values{"user": {v}}, ta.as(alice)).Code, http.StatusNotFound, v)
	}
	ensure.DeepEqual(t, ta.post(pDM, url.Values{"user": {idStr(bob.ID)}}).Code, http.StatusUnauthorized)
	ensure.DeepEqual(t, countRows(t, ta, "chats"), 1)
}

func TestDMConcurrentStart(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	bob := ta.newUser("bob")
	var wg sync.WaitGroup
	locs := make([]string, 20)
	for i := range locs {
		wg.Go(func() {
			from, to := alice, bob
			if i%2 == 1 {
				from, to = bob, alice
			}
			w := ta.post(pDM, url.Values{"user": {idStr(to.ID)}}, ta.as(from))
			locs[i] = fmt.Sprint(w.Code, " ", w.Header().Get("Location"))
		})
	}
	wg.Wait()
	ensure.DeepEqual(t, countRows(t, ta, "chats"), 1)
	ensure.DeepEqual(t, countRows(t, ta, "members"), 2)
	for _, l := range locs {
		ensure.DeepEqual(t, l, locs[0])
	}
	ensure.StringContains(t, locs[0], "303 /c/")
}

func TestDMAccess(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	admin := ta.newUser("boss", "admin")
	dm := ta.newDM(alice, bob)
	base := chatPath(dm.ID)

	for _, p := range []string{base, base + "/info"} {
		ensure.DeepEqual(t, ta.get(p, ta.as(alice)).Code, http.StatusOK, p)
		ensure.DeepEqual(t, ta.get(p, ta.as(bob)).Code, http.StatusOK, p)
		ensure.DeepEqual(t, ta.get(p, ta.as(carol)).Code, http.StatusNotFound, p)
		ensure.DeepEqual(t, ta.get(p, ta.as(admin)).Code, http.StatusNotFound, p)
		ensure.DeepEqual(t, ta.get(p).Code, http.StatusSeeOther, p)
	}

	// nothing to manage, for a participant or an admin
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{base + "/rename", url.Values{"name": {"x"}}},
		{base + "/members/add", url.Values{"user": {idStr(carol.ID)}}},
		{base + "/members/add", url.Values{"user": {idStr(admin.ID)}}},
		{base + "/members/remove", url.Values{"user": {idStr(bob.ID)}}},
	} {
		for _, u := range []*testUser{alice, bob, admin} {
			ensure.DeepEqual(t, ta.post(c.path, c.form, ta.as(u)).Code, http.StatusNotFound, c.path, u.Username)
		}
	}
	ensure.DeepEqual(t, memberCount(t, ta, dm.ID), 2)
	got, err := ta.app.q.ChatByID(t.Context(), dm.ID)
	ensure.Nil(t, err)
	ensure.DeepEqual(t, got.Name, "")

	// the info page shows the other user and no controls
	body := ta.get(base+"/info", ta.as(alice)).Body.String()
	ensure.StringContains(t, body, "<h1>bob</h1>")
	for _, s := range []string{"/rename", "/members/add", "/members/remove"} {
		ensure.StringDoesNotContain(t, body, `action="`+base+s+`"`)
	}
	ensure.StringDoesNotContain(t, body, `id="join"`)
}

func TestDMTitles(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	carol := ta.newUser("carol")
	g1 := ta.newChat(alice, "older group", bob)
	dm := ta.newDM(alice, bob)
	g2 := ta.newChat(alice, "newest group", bob)
	dm2 := ta.newDM(carol, alice)
	setLastMessageAt(t, ta, g1.ID, testNow.Unix()+10)
	setLastMessageAt(t, ta, dm.ID, testNow.Unix()+20)
	setLastMessageAt(t, ta, g2.ID, testNow.Unix()+30)
	setLastMessageAt(t, ta, dm2.ID, testNow.Unix()+5)

	row := func(c Chat, kind string) string {
		return fmt.Sprintf(`id="chat-%d" class="chat-item %s" href="%s"`, c.ID, kind, chatPath(c.ID)) +
			`><svg class="icon icon-` + map[string]string{"dm": "person", "group": "people"}[kind] +
			`"`
	}
	body := ta.get(pHome, ta.as(alice)).Body.String()
	ensure.StringContains(t, body, row(dm, "dm"))
	ensure.StringContains(t, body, row(g1, "group"))
	order := []int{
		strings.Index(body, fmt.Sprintf(`id="chat-%d"`, g2.ID)),
		strings.Index(body, fmt.Sprintf(`id="chat-%d"`, dm.ID)),
		strings.Index(body, fmt.Sprintf(`id="chat-%d"`, g1.ID)),
		strings.Index(body, fmt.Sprintf(`id="chat-%d"`, dm2.ID)),
	}
	for i := 1; i < len(order); i++ {
		ensure.True(t, order[i-1] > 0 && order[i] > order[i-1], order)
	}
	// each viewer sees the other participant's name
	nameOf := func(body string, c Chat) string {
		i := strings.Index(body, fmt.Sprintf(`id="chat-%d"`, c.ID))
		rest := body[i:]
		j := strings.Index(rest, `<span class="chat-name">`) + len(`<span class="chat-name">`)
		return rest[j : j+strings.Index(rest[j:], "<")]
	}
	ensure.DeepEqual(t, nameOf(body, dm), "bob")
	ensure.DeepEqual(t, nameOf(body, dm2), "carol")
	bobHome := ta.get(pHome, ta.as(bob)).Body.String()
	ensure.DeepEqual(t, nameOf(bobHome, dm), "alice")
	ensure.StringDoesNotContain(t, bobHome, fmt.Sprintf(`id="chat-%d"`, dm2.ID))

	// the pane header and page title
	body = ta.get(chatPath(dm.ID), ta.as(bob)).Body.String()
	ensure.StringContains(t, body, "<title>alice · msg</title>")
	ensure.StringContains(t, body, `class="pane-title" href="`+chatPath(dm.ID)+`/info">alice</a>`)
	body = ta.get(chatPath(dm.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, body, "<title>bob · msg</title>")
	ensure.StringContains(t, body, `class="pane-title" href="`+chatPath(dm.ID)+`/info">bob</a>`)
	body = ta.get(chatPath(dm.ID)+"/info", ta.as(alice)).Body.String()
	ensure.StringContains(t, body, "<title>bob · msg</title>")

	// everyone has the new message link
	ensure.StringContains(t, ta.get(pHome, ta.as(carol)).Body.String(), `href="/dm"`)
}

func messagesSent(t *testing.T, ta *testApp, userID int64) int64 {
	t.Helper()
	s, err := ta.app.userStats(t.Context(), userID)
	ensure.Nil(t, err)
	return s.MessagesSent
}

func lastMessageAt(t *testing.T, ta *testApp, chatID int64) int64 {
	t.Helper()
	c, err := ta.app.q.ChatByID(t.Context(), chatID)
	ensure.Nil(t, err)
	return c.LastMessageAt
}

func TestSend(t *testing.T) {
	ta := newTestApp(t)
	ctx := t.Context()
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	outsider := ta.newUser("outsider")
	admin := ta.newUser("boss", "admin")
	chat := ta.newChat(alice, "team", bob)
	send := chatPath(chat.ID) + "/send"

	later := testNow.Add(time.Hour)
	ta.app.now = func() time.Time { return later }
	w := ta.post(send, url.Values{"body": {"  hello\r\nthere  "}}, ta.as(bob))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, w.Header().Get("Location"), chatPath(chat.ID))
	rows, err := ta.app.q.MessagesLatest(ctx, MessagesLatestParams{ChatID: chat.ID, N: 10})
	ensure.Nil(t, err)
	ensure.DeepEqual(t, len(rows), 1)
	m := rows[0].Message
	ensure.DeepEqual(t, m.Body, "hello\nthere")
	ensure.DeepEqual(t, m.UserID, bob.ID)
	ensure.DeepEqual(t, m.CreatedAt, later.Unix())
	ensure.DeepEqual(t, rows[0].Username, "bob")
	ensure.DeepEqual(t, lastMessageAt(t, ta, chat.ID), later.Unix())
	ensure.DeepEqual(t, messagesSent(t, ta, bob.ID), int64(1))

	// non member 404, admin non member 404 on send but 200 on view
	ensure.DeepEqual(t, ta.post(send, url.Values{"body": {"x"}}, ta.as(outsider)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.post(send, url.Values{"body": {"x"}}, ta.as(admin)).Code, http.StatusNotFound)
	ensure.DeepEqual(t, ta.post(send, url.Values{"body": {"x"}}).Code, http.StatusUnauthorized)
	ensure.DeepEqual(t, ta.post("/c/9999/send", url.Values{"body": {"x"}}, ta.as(admin)).Code, http.StatusNotFound)
	w = ta.get(chatPath(chat.ID), ta.as(admin))
	ensure.DeepEqual(t, w.Code, http.StatusOK)
	ensure.StringDoesNotContain(t, w.Body.String(), `id="composer"`)
	ensure.StringContains(t, w.Body.String(), `id="composer-note"`)
	ensure.StringContains(t, w.Body.String(), `href="`+chatPath(chat.ID)+`/info"`)

	// empty creates nothing, too long is 400, exactly the limit is fine
	w = ta.post(send, url.Values{"body": {" \r\n\t "}}, ta.as(bob))
	ensure.DeepEqual(t, w.Code, http.StatusSeeOther)
	ensure.DeepEqual(t, w.Header().Get("Location"), chatPath(chat.ID))
	ensure.DeepEqual(t, ta.post(send, url.Values{"body": {strings.Repeat("a", 8001)}}, ta.as(bob)).Code,
		http.StatusBadRequest)
	ensure.DeepEqual(t, countRows(t, ta, "messages"), 1)
	ensure.DeepEqual(t, ta.post(send, url.Values{"body": {strings.Repeat("a", 8000)}}, ta.as(bob)).Code,
		http.StatusSeeOther)
	ensure.DeepEqual(t, countRows(t, ta, "messages"), 2)
	ensure.DeepEqual(t, messagesSent(t, ta, bob.ID), int64(2))

	// a member's page has the composer
	body := ta.get(chatPath(chat.ID), ta.as(bob)).Body.String()
	ensure.StringContains(t, body, `<form id="composer" method="post" action="`+send+`" data-morph>`)
	ensure.StringContains(t, body, `id="composer-body" name="body" rows="1" required maxlength="8000"`)
	ensure.StringContains(t, body, `id="composer-send"`)
}

func TestMessagesRender(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	chat := ta.newChat(alice, "team", bob)
	at := func(ts time.Time) { ta.app.now = func() time.Time { return ts } }

	day1 := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 11, 10, 14, 3, 0, 0, time.UTC)
	at(day1)
	m1 := ta.newMessage(chat, alice, "one")
	m2 := ta.newMessage(chat, alice, "two")
	m3 := ta.newMessage(chat, bob, "three")
	at(day2)
	m4 := ta.newMessage(chat, bob, "four")
	m5 := ta.newMessage(chat, alice, "<script>alert(1)</script>")

	body := ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	for _, id := range []string{`id="pane-head"`, `id="scroll" class="scroll"`, `id="messages"`} {
		ensure.StringContains(t, body, id)
	}
	art := func(m Message) string {
		i := strings.Index(body, fmt.Sprintf(`<article id="m%d"`, m.ID))
		ensure.True(t, i > 0, m.ID)
		j := strings.Index(body[i:], "</article>")
		return body[i : i+j]
	}
	// oldest first
	pos := func(m Message) int { return strings.Index(body, fmt.Sprintf(`id="m%d"`, m.ID)) }
	ensure.True(t, pos(m1) < pos(m2) && pos(m2) < pos(m3) && pos(m3) < pos(m4) && pos(m4) < pos(m5))
	for _, m := range []Message{m1, m2, m3, m4, m5} {
		a := art(m)
		ensure.StringContains(t, a, fmt.Sprintf(`id="m%d-body" class="msg-body"`, m.ID))
		ensure.StringContains(t, a, fmt.Sprintf(`id="m%d-time"`, m.ID))
	}
	ensure.StringContains(t, art(m1), `class="msg mine first"`)
	ensure.StringContains(t, art(m2), `class="msg mine"`)
	ensure.StringContains(t, art(m3), `class="msg first"`)
	ensure.StringContains(t, art(m4), `class="msg first"`) // new day, same user
	ensure.StringContains(t, art(m5), `class="msg mine first"`)
	// only first messages of other users name the author
	ensure.StringDoesNotContain(t, art(m1), "msg-who")
	ensure.StringContains(t, art(m3), fmt.Sprintf(`<div class="msg-who hue-%d">bob</div>`, bob.ID%12))
	ensure.StringContains(t, art(m4), "msg-who")
	ensure.StringContains(t, art(m4), `<time id="m`+idStr(m4.ID)+`-time" datetime="2026-11-10T14:03:00Z">14:03</time>`)
	// day separators
	ensure.StringContains(t, body, `<div id="day-2026-11-09" class="day">9 November 2026</div>`)
	ensure.StringContains(t, body, `<div id="day-2026-11-10" class="day">10 November 2026</div>`)
	ensure.True(t, strings.Index(body, "day-2026-11-10") < pos(m4) && strings.Index(body, "day-2026-11-10") > pos(m3))
	// HTML is escaped
	ensure.StringDoesNotContain(t, body, "<script>alert(1)</script>")
	ensure.StringContains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;")

	// bob's view flips mine
	body = ta.get(chatPath(chat.ID), ta.as(bob)).Body.String()
	ensure.StringContains(t, art(m1), `class="msg first"`)
	ensure.StringContains(t, art(m1), "alice")
	ensure.StringContains(t, art(m3), `class="msg mine first"`)
	ensure.StringContains(t, art(m4), `class="msg mine first"`)
	ensure.StringDoesNotContain(t, art(m4), "msg-who")

	// no earlier link with only a few messages
	ensure.StringDoesNotContain(t, body, `id="earlier"`)
}

func TestLinkify(t *testing.T) {
	render := func(nodes []g.Node) string {
		var sb strings.Builder
		ensure.Nil(t, g.Group(nodes).Render(&sb))
		return sb.String()
	}
	link := func(u string) string {
		return `<a href="` + u + `" target="_blank" rel="noopener noreferrer nofollow">` + u + `</a>`
	}
	cases := []struct{ in, want, first string }{
		{"plain text", "plain text", ""},
		{"", "", ""},
		{"see https://example.com/a?b=c", "see " + link("https://example.com/a?b=c"), "https://example.com/a?b=c"},
		{"go to http://example.com.", "go to " + link("http://example.com") + ".", "http://example.com"},
		{"(https://example.com/x)", "(" + link("https://example.com/x") + ")", "https://example.com/x"},
		{"[https://example.com]!", "[" + link("https://example.com") + "]!", "https://example.com"},
		{"https://a.com and https://b.com/p", link("https://a.com") + " and " + link("https://b.com/p"), "https://a.com"},
		{"javascript:alert(1)", "javascript:alert(1)", ""},
		{`https://a.com/"onmouseover="x`, link("https://a.com/") + "&#34;onmouseover=&#34;x", "https://a.com/"},
		{"https://a.com/<b>", link("https://a.com/") + "&lt;b&gt;", "https://a.com/"},
		{"https://a.com/'x", link("https://a.com/") + "&#39;x", "https://a.com/"},
		{"https://.", "https://.", ""},
		{"line\nhttps://a.com\nnext", "line\n" + link("https://a.com") + "\nnext", "https://a.com"},
	}
	for _, c := range cases {
		ensure.DeepEqual(t, render(linkify(c.in)), c.want, c.in)
		ensure.DeepEqual(t, firstURL(c.in), c.first, c.in)
	}
}

func TestMessagesPagination(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	chat := ta.newChat(alice, "team")
	var msgs []Message
	for i := range 150 {
		msgs = append(msgs, ta.newMessage(chat, alice, fmt.Sprint("message ", i)))
	}
	count := func(body string) int { return strings.Count(body, `<article id="m`) }

	body := ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.DeepEqual(t, count(body), 100)
	ensure.StringContains(t, body, fmt.Sprintf(`<a id="earlier" href="%s?n=200#m%d" data-morph>Show earlier messages</a>`,
		chatPath(chat.ID), msgs[50].ID))
	ensure.StringDoesNotContain(t, body, fmt.Sprintf(`id="m%d"`, msgs[49].ID))
	ensure.StringContains(t, body, fmt.Sprintf(`id="m%d"`, msgs[149].ID))
	// the link sits before the messages
	ensure.True(t, strings.Index(body, `id="earlier"`) < strings.Index(body, fmt.Sprintf(`id="m%d"`, msgs[50].ID)))

	body = ta.get(chatPath(chat.ID)+"?n=200", ta.as(alice)).Body.String()
	ensure.DeepEqual(t, count(body), 150)
	ensure.StringDoesNotContain(t, body, `id="earlier"`)

	body = ta.get(chatPath(chat.ID)+"?n=150", ta.as(alice)).Body.String()
	ensure.DeepEqual(t, count(body), 150)
	ensure.StringDoesNotContain(t, body, `id="earlier"`)

	for in, want := range map[string]int{"0": 1, "-5": 1, "x": 100, "": 100, "149": 149, "99999": 150} {
		ensure.DeepEqual(t, count(ta.get(chatPath(chat.ID)+"?n="+in, ta.as(alice)).Body.String()), want, in)
	}
	ensure.StringContains(t, ta.get(chatPath(chat.ID)+"?n=1", ta.as(alice)).Body.String(), "?n=2#m")
	ensure.DeepEqual(t, pageSize("4000"), int64(4000))
	ensure.DeepEqual(t, pageSize("5001"), int64(5000))
}

func TestMessagesTimeZone(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	chat := ta.newChat(alice, "team")
	ta.app.now = func() time.Time { return time.Date(2026, 11, 10, 22, 30, 0, 0, time.UTC) }
	m := ta.newMessage(chat, alice, "late")
	tz := func(v string) reqOpt {
		return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "tz", Value: v}) }
	}
	timeTag := func(clock string) string {
		return fmt.Sprintf(`<time id="m%d-time" datetime="2026-11-10T22:30:00Z">%s</time>`, m.ID, clock)
	}

	body := ta.get(chatPath(chat.ID), ta.as(alice), tz("Asia/Dubai")).Body.String()
	ensure.StringContains(t, body, timeTag("02:30"))
	ensure.StringContains(t, body, `<div id="day-2026-11-11" class="day">11 November 2026</div>`)
	ensure.StringContains(t, body, "11 Nov 02:30") // the chat list

	for _, v := range []string{"", "Not/AZone", "Local", "../../etc/passwd"} {
		body = ta.get(chatPath(chat.ID), ta.as(alice), tz(v)).Body.String()
		ensure.StringContains(t, body, timeTag("22:30"))
		ensure.StringContains(t, body, `<div id="day-2026-11-10" class="day">10 November 2026</div>`)
	}
	body = ta.get(chatPath(chat.ID), ta.as(alice)).Body.String()
	ensure.StringContains(t, body, timeTag("22:30"))
}

func TestSidebarSnippet(t *testing.T) {
	ta := newTestApp(t)
	alice := ta.newUser("alice", "group")
	bob := ta.newUser("bob")
	g1 := ta.newChat(alice, "first", bob)
	g2 := ta.newChat(alice, "second", bob)
	dm := ta.newDM(alice, bob)
	at := func(min int) {
		ta.app.now = func() time.Time { return testNow.Add(time.Duration(min) * time.Minute) }
	}
	order := func(body string, chats ...Chat) bool {
		prev := -1
		for _, c := range chats {
			i := strings.Index(body, fmt.Sprintf(`id="chat-%d"`, c.ID))
			if i <= prev {
				return false
			}
			prev = i
		}
		return true
	}
	at(1)
	ta.newMessage(g1, bob, "older")
	at(2)
	ta.newMessage(g1, bob, strings.Repeat("é", 100)+"\nmore")
	body := ta.get(pHome, ta.as(alice)).Body.String()
	ensure.True(t, order(body, g1, dm, g2))
	ensure.StringContains(t, body, `<span class="chat-snippet">bob: `+strings.Repeat("é", 80)+`</span>`)
	ensure.StringContains(t, body, `<time class="chat-time" datetime="2026-01-02T03:06:05Z">2 Jan 03:06</time>`)
	ensure.StringDoesNotContain(t, body, "bob: older")

	// a send through the form reorders the list
	at(3)
	ta.post(chatPath(g2.ID)+"/send", url.Values{"body": {"<b>hi</b>"}}, ta.as(alice))
	body = ta.get(pHome, ta.as(bob)).Body.String()
	ensure.True(t, order(body, g2, g1, dm))
	ensure.StringContains(t, body, `<span class="chat-snippet">alice: &lt;b&gt;hi&lt;/b&gt;</span>`)
	at(4)
	ta.newMessage(dm, bob, "direct")
	body = ta.get(pHome, ta.as(alice)).Body.String()
	ensure.True(t, order(body, dm, g2, g1))
	ensure.StringContains(t, body, `<span class="chat-snippet">bob: direct</span>`)
}
