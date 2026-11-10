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

	"github.com/daaku/ensure"
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
	chats, err := ta.app.q.ChatsForUser(ctx, alice.ID)
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
