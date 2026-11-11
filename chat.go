package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the zone database for the tz cookie
	"unicode/utf8"

	"github.com/daaku/serr"
	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

const (
	maxChatName    = 64
	maxMessageBody = 8000
	defaultPage    = 100
	maxPage        = 5000
	snippetLength  = 80
)

var msgChatName = fmt.Sprintf("A group name is 1 to %d characters.", maxChatName)

var errNotFound = httpErr(http.StatusNotFound, "Not found.")

func chatPath(id int64) string {
	return pChat + strconv.FormatInt(id, 10)
}

// access

// chatAccess loads a chat and decides what the user may do with it. A user
// who may not view it gets a 404, so its existence is not revealed.
func (a *app) chatAccess(ctx context.Context, user *User, chatID int64) (chat Chat, member bool, manage bool, err error) {
	chat, row, manage, err := a.chatAccessRow(ctx, user, chatID)
	return chat, row != nil, manage, err
}

// chatAccessRow is chatAccess that also returns the user's members row, nil
// when they are not a member.
func (a *app) chatAccessRow(ctx context.Context, user *User, chatID int64) (chat Chat, row *Member, manage bool, err error) {
	chat, err = a.q.ChatByID(ctx, chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return chat, nil, false, errNotFound
	}
	if err != nil {
		return chat, nil, false, serr.Wrap(err)
	}
	m, err := a.q.MemberGet(ctx, MemberGetParams{ChatID: chatID, UserID: user.ID})
	switch {
	case err == nil:
		row = &m
	case !errors.Is(err, sql.ErrNoRows):
		return chat, nil, false, serr.Wrap(err)
	}
	member := row != nil
	if chat.IsDm {
		// Only the two participants can see a direct message, not even an
		// admin, and there is nothing to manage.
		if !member {
			return chat, nil, false, errNotFound
		}
		return chat, row, false, nil
	}
	admin := allowed(user, accAdmin)
	if !member && !admin {
		return chat, nil, false, errNotFound
	}
	manage = admin || (chat.CreatedBy == user.ID && user.PermGroup)
	return chat, row, manage, nil
}

// chatTitle is the name a chat is shown with: the group's name, or for a
// direct message the other participant's username.
func (a *app) chatTitle(chat Chat, viewer *User, other string) string {
	if chat.IsDm {
		return other
	}
	return chat.Name
}

// chatName is chatTitle for a single chat, loading the other participant of
// a direct message.
func (a *app) chatName(ctx context.Context, chat Chat, viewer *User) (string, error) {
	var other string
	if chat.IsDm {
		members, err := a.q.MembersOfChat(ctx, chat.ID)
		if err != nil {
			return "", serr.Wrap(err)
		}
		for _, m := range members {
			if m.UserID != viewer.ID {
				other = m.Username
			}
		}
	}
	return a.chatTitle(chat, viewer, other), nil
}

// live updates

// hubConn is one open /events stream.
type hubConn struct {
	ch     chan struct{} // buffered, size 1: a pending ping
	chatID int64         // the chat this tab is showing, 0 if none
}

// hub fans pings out to the open streams of this process. A ping carries no
// data: it means "something you can see changed, fetch the page again".
type hub struct {
	mu    sync.Mutex
	conns map[int64]map[*hubConn]struct{} // by user id
}

func newHub() *hub {
	return &hub{conns: map[int64]map[*hubConn]struct{}{}}
}

// subscribe registers a stream. cancel must be called when it ends.
func (h *hub) subscribe(userID, chatID int64) (c *hubConn, cancel func()) {
	c = &hubConn{ch: make(chan struct{}, 1), chatID: chatID}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[userID] == nil {
		h.conns[userID] = map[*hubConn]struct{}{}
	}
	h.conns[userID][c] = struct{}{}
	return c, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.conns[userID], c)
		if len(h.conns[userID]) == 0 {
			delete(h.conns, userID)
		}
	}
}

// ping never blocks: a full channel already has a ping pending, and one
// refresh shows everything.
func (c *hubConn) ping() {
	select {
	case c.ch <- struct{}{}:
	default:
	}
}

// notifyUsers pings every stream of the given users.
func (h *hub) notifyUsers(userIDs ...int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range userIDs {
		for c := range h.conns[id] {
			c.ping()
		}
	}
}

// notifyViewers pings every stream that is showing the chat, whoever it
// belongs to. This reaches an admin watching a group they are not in.
func (h *hub) notifyViewers(chatID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, conns := range h.conns {
		for c := range conns {
			if c.chatID == chatID {
				c.ping()
			}
		}
	}
}

// viewing reports whether the user has a stream open on the chat.
func (h *hub) viewing(userID, chatID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns[userID] {
		if c.chatID == chatID {
			return true
		}
	}
	return false
}

// notifyChat tells everyone who can see the chat that it changed.
func (a *app) notifyChat(ctx context.Context, chatID int64) {
	// The request that caused the change may already be over.
	ids, err := a.q.MemberIDs(context.WithoutCancel(ctx), chatID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "notifyChat %d: %+v\n", chatID, serr.Wrap(err))
	}
	a.hub.notifyUsers(ids...)
	a.hub.notifyViewers(chatID)
}

const eventsHeartbeat = 25 * time.Second

// events is the Server-Sent Events stream. The optional chat parameter says
// which chat the tab is showing.
func (a *app) events(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	var chatID int64
	if s := r.URL.Query().Get("chat"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return errNotFound
		}
		if _, _, _, err := a.chatAccess(ctx, user, id); err != nil {
			return err
		}
		chatID = id
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(s string) bool {
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	// Subscribe before the first byte, so nothing after it can be missed.
	c, cancel := a.hub.subscribe(user.ID, chatID)
	defer cancel()
	if !send("retry: 3000\n\n") {
		return nil
	}
	tick := time.NewTicker(eventsHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-c.ch:
			if !send("data: 1\n\n") {
				return nil
			}
		case <-tick.C:
			if !send(": hb\n\n") {
				return nil
			}
		case <-ctx.Done():
			return nil
		case <-a.ctx.Done(): // shutdown must not wait for open streams
			return nil
		}
	}
}

// chatView is a chat as seen by the signed in user.
type chatView struct {
	Chat     Chat
	User     *User
	Member   bool
	Manage   bool
	Archived bool // by this user, private to them
	Title    string
}

// chatView loads the chat named by the {id} path value for a user who may
// view it.
func (a *app) chatView(r *http.Request) (chatView, error) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return chatView{}, errNotFound
	}
	user := userFrom(ctx)
	chat, row, manage, err := a.chatAccessRow(ctx, user, id)
	if err != nil {
		return chatView{}, err
	}
	title, err := a.chatName(ctx, chat, user)
	if err != nil {
		return chatView{}, err
	}
	return chatView{Chat: chat, User: user, Member: row != nil, Manage: manage,
		Archived: row != nil && row.Archived, Title: title}, nil
}

// chatMember is chatView for a user who must be a member.
func (a *app) chatMember(r *http.Request) (chatView, error) {
	v, err := a.chatView(r)
	if err == nil && !v.Member {
		return v, errNotFound
	}
	return v, err
}

// chatManage is chatView for a user who must be allowed to manage the chat.
func (a *app) chatManage(r *http.Request) (chatView, error) {
	v, err := a.chatView(r)
	if err == nil && !v.Manage {
		return v, errNotFound
	}
	return v, err
}

// layout

// chatPage renders the two pane shell shared by home and every chat page.
// active is nil on home.
func (a *app) chatPage(w http.ResponseWriter, r *http.Request, user *User, active *Chat, pane g.Node) error {
	return a.chatShell(w, r, user, active, false, pane)
}

// chatShell is chatPage with the sidebar showing either the main chat list
// or, when archived is true, the user's archived chats.
func (a *app) chatShell(w http.ResponseWriter, r *http.Request, user *User, active *Chat, archived bool, pane g.Node) error {
	ctx := r.Context()
	chats, err := a.q.ChatsForUserWithLast(ctx, ChatsForUserWithLastParams{UserID: user.ID, Archived: archived})
	if err != nil {
		return serr.Wrap(err)
	}
	var archivedCount int64
	if !archived {
		if archivedCount, err = a.q.ArchivedCount(ctx, user.ID); err != nil {
			return serr.Wrap(err)
		}
	}
	loc := viewerLoc(r)
	title := "Home"
	if archived {
		title = "Archived"
	}
	var activeID int64
	appAttrs := g.Group{h.Class("chat-layout"), h.Data("live", "")}
	if active != nil {
		activeID = active.ID
		if title, err = a.chatName(ctx, *active, user); err != nil {
			return err
		}
		appAttrs = g.Group{h.Class("chat-layout has-chat"), h.Data("live", ""),
			h.Data("chat", strconv.FormatInt(active.ID, 10))}
	}

	headerLink := func(href, label, iconName string) g.Node {
		return h.A(h.Class("btn ghost icon-btn"), h.Href(href), h.Title(label),
			h.Aria("label", label), icon(iconName))
	}
	var list g.Node
	if archived && len(chats) == 0 {
		list = h.P(h.ID("chats-empty"), h.Class("empty"), g.Text("No archived chats."))
	} else if len(chats) == 0 && archivedCount == 0 {
		var group g.Node = g.Text(" Someone needs to add you to a group.")
		if allowed(user, accGroup) {
			group = g.Group{g.Text(" or "), h.A(h.Href(pNew), g.Text("create a group")), g.Text(".")}
		}
		list = h.Div(h.ID("chats-empty"), h.Class("empty stack"),
			h.P(h.Strong(g.Text("No chats yet."))),
			h.P(h.A(h.Href(pDM), g.Text("Start a direct message")), group))
	} else {
		list = h.Div(h.ID("chats"), h.Class("chat-list"), g.Map(chats, func(row ChatsForUserWithLastRow) g.Node {
			c := row.Chat
			kind, kindIcon := "group", "people"
			if c.IsDm {
				kind, kindIcon = "dm", "person"
			}
			var last g.Node
			if row.LastCreatedAt > 0 {
				at := time.Unix(row.LastCreatedAt, 0)
				last = g.Group{
					h.Time(h.Class("chat-time"), h.DateTime(at.UTC().Format(time.RFC3339)),
						g.Text(at.In(loc).Format("2 Jan 15:04"))),
					h.Span(h.Class("chat-snippet"), g.Text(row.LastUsername+": "+lastSnippet(row))),
				}
			}
			return h.A(h.ID(fmt.Sprintf("chat-%d", c.ID)), h.Class("chat-item "+kind),
				h.Href(chatPath(c.ID)),
				g.If(c.ID == activeID, h.Aria("current", "page")),
				icon(kindIcon),
				h.Span(h.Class("chat-name"), g.Text(a.chatTitle(c, user, row.Other))),
				last)
		}), g.If(!archived, archivedLink(archivedCount)))
	}
	head := h.Header(h.Class("sidebar-head"),
		h.H1(g.Text(a.cfg.AppName)),
		h.Div(h.Class("cluster"),
			headerLink(pDM, "New message", "pencil"),
			g.If(allowed(user, accGroup), headerLink(pNew, "New group", "plus")),
			g.If(allowed(user, accInvite), headerLink(pInvites, "Invites", "person-plus")),
			g.If(allowed(user, accAdmin), headerLink(pAdmin, "Admin", "shield")),
			headerLink(pSettings, "Settings", "gear")))
	if archived {
		head = h.Header(h.Class("sidebar-head start"),
			headerLink(pHome, "Back", "back"),
			h.H1(g.Text("Archived")))
	}
	// Shown by CSS only while the body has push-prompt.
	prompt := h.A(h.ID("push-prompt"), h.Class("push-prompt-link"), h.Href(pSettings+"#notifications"),
		icon("bell"), h.Span(g.Text("Turn on notifications for this device")))
	sidebar := h.Nav(h.ID("sidebar"), head, prompt, list)
	return a.shell(w, r, title, "chat", appAttrs, sidebar,
		h.Section(h.ID("pane"), pane),
		// Upload progress and errors, driven by main.js. The ring draws from
		// state on <html>, which the morph never touches.
		h.Div(h.ID("upload-ring"), h.Class("upload-ring"), h.Role("progressbar"),
			h.Aria("label", "Uploading"), h.Aria("valuemin", "0"), h.Aria("valuemax", "100")),
		h.Div(h.ID("upload-error"), h.Class("upload-error"), h.Role("alert"))).Render(w)
}

// archivedLink is the last row of the main chat list. It is always
// rendered, hidden when nothing is archived, so it never shifts the rows.
func archivedLink(n int64) g.Node {
	if n == 0 {
		return h.A(h.ID("archived-link"), h.Class("archived-link"), h.Hidden(""))
	}
	return h.A(h.ID("archived-link"), h.Class("archived-link"), h.Href(pArchived),
		g.Textf("Archived (%d)", n))
}

// paneHead is the header of a chat pane. The back link to home is only
// shown on narrow screens, where the chat list is not visible. A chat the
// viewer archived carries a label and an Unarchive button at the end.
func paneHead(v chatView) g.Node {
	return h.Header(h.ID("pane-head"), h.Class("pane-head"),
		h.A(h.Class("back narrow-only"), h.Href(pHome), h.Aria("label", "Back"), icon("back")),
		h.A(h.Class("pane-title"), h.Href(chatPath(v.Chat.ID)+"/info"), g.Text(v.Title)),
		g.If(v.Archived, h.Form(h.ID("pane-archived"), h.Class("pane-archived"), h.Method("post"),
			h.Action(chatPath(v.Chat.ID)+"/unarchive"),
			h.Span(h.Class("badge"), g.Text("Archived")),
			h.Button(h.Class("btn secondary"), g.Text("Unarchive")))))
}

func homePlaceholder() g.Node {
	return h.Div(h.Class("placeholder"), h.P(g.Text("Select a group or a direct message")))
}

func (a *app) home(w http.ResponseWriter, r *http.Request) error {
	return a.chatPage(w, r, userFrom(r.Context()), nil, homePlaceholder())
}

// archived is home with the user's archived chats in the sidebar.
func (a *app) archived(w http.ResponseWriter, r *http.Request) error {
	return a.chatShell(w, r, userFrom(r.Context()), nil, true, homePlaceholder())
}

func (a *app) chatArchive(w http.ResponseWriter, r *http.Request) error {
	return a.setArchived(w, r, true)
}

func (a *app) chatUnarchive(w http.ResponseWriter, r *http.Request) error {
	return a.setArchived(w, r, false)
}

// setArchived sets the flag on the user's own members row. Only the user's
// own tabs are told, because nobody else may see it.
func (a *app) setArchived(w http.ResponseWriter, r *http.Request, archived bool) error {
	v, err := a.chatMember(r)
	if err != nil {
		return err
	}
	err = a.q.MemberSetArchived(r.Context(), MemberSetArchivedParams{
		Archived: archived,
		ChatID:   v.Chat.ID,
		UserID:   v.User.ID,
	})
	if err != nil {
		return serr.Wrap(err)
	}
	a.hub.notifyUsers(v.User.ID)
	to := chatPath(v.Chat.ID)
	if archived {
		to = pHome
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
	return nil
}

// archiveForm is the Archive or Unarchive button on the chat info page.
func archiveForm(v chatView) g.Node {
	if !v.Member {
		return nil
	}
	action, label := "/archive", "Archive"
	if v.Archived {
		action, label = "/unarchive", "Unarchive"
	}
	return h.Form(h.ID("archive"), h.Method("post"), h.Action(chatPath(v.Chat.ID)+action),
		h.Button(h.Class("btn secondary"), g.Text(label)))
}

// groups

// validChatName trims a group name and checks its length.
func validChatName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > maxChatName {
		return "", errors.New(msgChatName)
	}
	return s, nil
}

// formIDs reads the row ids posted in the named form field.
func formIDs(r *http.Request, name string) ([]int64, error) {
	if err := r.ParseForm(); err != nil {
		return nil, httpErr(http.StatusBadRequest, "Bad request.")
	}
	var ids []int64
	for _, v := range r.PostForm[name] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return nil, httpErr(http.StatusBadRequest, "Bad request.")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// addMembers adds the given users to a chat, ignoring ids of users that do
// not exist and users who are already members.
func (a *app) addMembers(ctx context.Context, q *Queries, chatID, addedBy int64, userIDs []int64) error {
	for _, id := range userIDs {
		err := q.MemberAdd(ctx, MemberAddParams{
			ChatID:    chatID,
			UserID:    id,
			AddedBy:   addedBy,
			CreatedAt: a.now().Unix(),
		})
		if err != nil {
			return serr.Wrap(err)
		}
	}
	return nil
}

// createGroup creates a group with the creator and the given users as
// members.
func (a *app) createGroup(ctx context.Context, creatorID int64, name string, userIDs []int64) (Chat, error) {
	var chat Chat
	err := a.tx(ctx, func(q *Queries) error {
		now := a.now().Unix()
		var err error
		chat, err = q.ChatCreate(ctx, ChatCreateParams{
			Name:          name,
			CreatedBy:     creatorID,
			CreatedAt:     now,
			LastMessageAt: now,
		})
		if err != nil {
			return serr.Wrap(err)
		}
		return a.addMembers(ctx, q, chat.ID, creatorID, append([]int64{creatorID}, userIDs...))
	})
	return chat, err
}

// userChecks renders a checkbox per user, posted as "user".
func userChecks(users []UsersAllBasicRow) g.Node {
	return h.Div(h.Class("stack-tight"), g.Map(users, func(u UsersAllBasicRow) g.Node {
		return h.Label(h.Class("check"),
			h.Input(h.Type("checkbox"), h.Name("user"), h.Value(strconv.FormatInt(u.ID, 10))),
			g.Text(u.Username))
	}))
}

func nameInput(value string) g.Node {
	return h.Input(h.Type("text"), h.Name("name"), h.Value(value), h.Required(),
		h.MaxLength(strconv.Itoa(maxChatName)), h.AutoComplete("off"))
}

func (a *app) newGroup(w http.ResponseWriter, r *http.Request) error {
	user := userFrom(r.Context())
	all, err := a.q.UsersAllBasic(r.Context())
	if err != nil {
		return serr.Wrap(err)
	}
	var others []UsersAllBasicRow
	for _, u := range all {
		if u.ID != user.ID {
			others = append(others, u)
		}
	}
	var members g.Node = h.P(h.Class("muted"), g.Text("There is nobody else to add yet."))
	if len(others) > 0 {
		members = h.FieldSet(h.Class("stack-tight"),
			h.Legend(g.Text("Members")), userChecks(others))
	}
	return a.pageSimple(w, r, "New group", backLink(pHome, "Home"),
		h.Form(h.Method("post"), h.Action(pNew), h.Class("stack card"),
			h.Label(g.Text("Name"), nameInput(r.URL.Query().Get("name"))),
			members,
			h.Button(h.Class("btn"), g.Text("Create group"))),
	).Render(w)
}

func (a *app) newGroupPost(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	ids, err := formIDs(r, "user")
	if err != nil {
		return err
	}
	name, err := validChatName(r.PostFormValue("name"))
	if err != nil {
		return a.flash(w, r, pNew, Flash{Kind: flashError, Content: err.Error()})
	}
	chat, err := a.createGroup(ctx, user.ID, name, ids)
	if err != nil {
		return err
	}
	http.Redirect(w, r, chatPath(chat.ID), http.StatusSeeOther)
	return nil
}

func (a *app) chat(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	v, err := a.chatView(r)
	if err != nil {
		return err
	}
	n := pageSize(r.URL.Query().Get("n"))
	rows, err := a.q.MessagesLatest(ctx, MessagesLatestParams{ChatID: v.Chat.ID, N: n})
	if err != nil {
		return serr.Wrap(err)
	}
	slices.Reverse(rows)
	total, err := a.q.MessageCount(ctx, v.Chat.ID)
	if err != nil {
		return serr.Wrap(err)
	}
	base := chatPath(v.Chat.ID)
	reactions, err := a.reactionsFor(ctx, v.User.ID, rows)
	if err != nil {
		return err
	}
	files, err := a.filesFor(ctx, rows)
	if err != nil {
		return err
	}

	// The link is always rendered, hidden when there is nothing earlier, so
	// that the first child of #messages lines up between renders. Without
	// it the morph would move every message when the link goes away.
	earlier := h.A(h.ID("earlier"), h.Hidden(""))
	if total > n && n < maxPage && len(rows) > 0 {
		// The fragment keeps the reading position when the link is followed
		// as a plain navigation: the browser scrolls to the message that was
		// the oldest one shown.
		href := fmt.Sprintf("%s?n=%d#m%d", base, min(2*n, maxPage), rows[0].Message.ID)
		earlier = h.A(h.ID("earlier"), h.Href(href), g.Attr("data-morph"),
			g.Text("Show earlier messages"))
	}

	var composer g.Node
	if v.Member {
		composer = h.Form(h.ID("composer"), h.Method("post"), h.Action(base+"/send"),
			g.Attr("data-morph"),
			h.Label(h.ID("composer-attach"), h.Class("btn ghost icon-btn"), h.Aria("label", "Attach files"),
				icon("paperclip"),
				h.Input(h.ID("composer-files"), h.Type("file"), h.Multiple(), h.Hidden(""),
					h.Data("upload", strconv.FormatInt(v.Chat.ID, 10)),
					h.Data("max", strconv.FormatInt(a.cfg.MaxUploadSize, 10)))),
			h.Textarea(h.ID("composer-body"), h.Name("body"), h.Rows("1"), h.Required(),
				h.MaxLength(strconv.Itoa(maxMessageBody)), h.Placeholder("Message"),
				h.AutoComplete("off"), h.Aria("label", "Message")),
			h.Button(h.ID("composer-send"), h.Class("btn icon-btn"), h.Type("submit"),
				h.Aria("label", "Send"), icon("send")))
	} else {
		composer = h.P(h.ID("composer-note"), h.Class("composer-note"),
			g.Text("You are viewing this group as an admin. "),
			h.A(h.Href(base+"/info"), g.Text("Join it")), g.Text(" to post."))
	}

	return a.chatPage(w, r, v.User, &v.Chat, g.Group{
		paneHead(v),
		h.Div(h.ID("scroll"), h.Class("scroll"),
			h.Div(h.ID("messages"), earlier, a.messageNodes(v, rows, reactions, files, viewerLoc(r)))),
		composer,
	})
}

// pageSize parses the n query parameter: how many of the newest messages to
// show.
func pageSize(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return defaultPage
	}
	return min(max(n, 1), maxPage)
}

// viewerLoc is the time zone the browser reported in the tz cookie, UTC when
// it is missing or unknown.
func viewerLoc(r *http.Request) *time.Location {
	c, err := r.Cookie("tz")
	if err != nil || c.Value == "" || c.Value == "Local" {
		return time.UTC
	}
	loc, err := time.LoadLocation(c.Value)
	if err != nil {
		return time.UTC
	}
	return loc
}

// messageNodes renders messages, oldest first, with a separator before the
// first message of each day.
func (a *app) messageNodes(v chatView, rows []MessagesLatestRow, reactions map[int64][]reactionGroup, files map[int64]File, loc *time.Location) g.Node {
	var nodes g.Group
	var prevUser int64
	var prevDay string
	for _, row := range rows {
		m := row.Message
		at := time.Unix(m.CreatedAt, 0).In(loc)
		day := at.Format("2006-01-02")
		if day != prevDay {
			nodes = append(nodes, h.Div(h.ID("day-"+day), h.Class("day"),
				g.Text(at.Format("2 January 2006"))))
		}
		first := day != prevDay || m.UserID != prevUser
		prevUser, prevDay = m.UserID, day
		mine := m.UserID == v.User.ID
		class := "msg"
		if mine {
			class += " mine"
		}
		if first {
			class += " first"
		}
		id := fmt.Sprintf("m%d", m.ID)
		content := h.Div(h.ID(id+"-body"), h.Class("msg-body"), g.Group(linkify(m.Body)))
		if f, ok := files[m.FileID.Int64]; ok && m.FileID.Valid {
			content = h.Div(h.ID(id+"-file"), h.Class("msg-file"), fileNode(f))
		}
		nodes = append(nodes, h.Article(h.ID(id), h.Class(class),
			g.If(first && !mine, h.Div(h.Class(fmt.Sprintf("msg-who hue-%d", m.UserID%12)),
				g.Text(row.Username))),
			content,
			a.reactionChips(v, m, reactions[m.ID]),
			g.If(m.EditedAt > 0, h.Span(h.ID(id+"-edited"), h.Class("msg-edited"), g.Text("edited"))),
			h.Time(h.ID(id+"-time"), h.DateTime(at.UTC().Format(time.RFC3339)),
				g.Text(at.Format("15:04"))),
			a.messageMenu(v, m)))
	}
	return nodes
}

// snippet is the start of a message body for the chat list.
// lastSnippet describes a chat's newest message in the chat list.
func lastSnippet(row ChatsForUserWithLastRow) string {
	if row.LastFileKind != "" {
		return fileSnippet(row.LastFileKind, row.LastFileName)
	}
	return snippet(row.LastBody)
}

func snippet(body string) string {
	if utf8.RuneCountInString(body) <= snippetLength {
		return body
	}
	return string([]rune(body)[:snippetLength])
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"']+`)

// findURLs returns the start and end of each http and https URL in s.
func findURLs(s string) [][2]int {
	var out [][2]int
	for _, loc := range urlRe.FindAllStringIndex(s, -1) {
		start := loc[0]
		u := strings.TrimRight(s[start:loc[1]], ".,;:!?)]}")
		if _, rest, _ := strings.Cut(u, "://"); rest != "" {
			out = append(out, [2]int{start, start + len(u)})
		}
	}
	return out
}

// linkify renders a message body as text with its http and https URLs as
// links.
func linkify(body string) []g.Node {
	var nodes []g.Node
	pos := 0
	for _, u := range findURLs(body) {
		if u[0] > pos {
			nodes = append(nodes, g.Text(body[pos:u[0]]))
		}
		href := body[u[0]:u[1]]
		nodes = append(nodes, h.A(h.Href(href), h.Target("_blank"),
			h.Rel("noopener noreferrer nofollow"), g.Text(href)))
		pos = u[1]
	}
	if pos < len(body) {
		nodes = append(nodes, g.Text(body[pos:]))
	}
	return nodes
}

// firstURL returns the first http or https URL in body, or "".
func firstURL(body string) string {
	if urls := findURLs(body); len(urls) > 0 {
		return body[urls[0][0]:urls[0][1]]
	}
	return ""
}

// messages

// normalizeBody normalises line endings and trims a message body.
func normalizeBody(s string) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len(s) > maxMessageBody {
		return "", httpErr(http.StatusBadRequest,
			fmt.Sprintf("A message is at most %d bytes.", maxMessageBody))
	}
	return s, nil
}

// createMessage stores a new message, bumps the chat and the author's
// counter, and runs afterMessage once it is committed.
func (a *app) createMessage(ctx context.Context, chat Chat, userID int64, body string, fileID sql.NullInt64) (Message, error) {
	var msg Message
	err := a.tx(ctx, func(q *Queries) error {
		now := a.now().Unix()
		var err error
		msg, err = q.MessageCreate(ctx, MessageCreateParams{
			ChatID:    chat.ID,
			UserID:    userID,
			Body:      body,
			FileID:    fileID,
			CreatedAt: now,
		})
		if err != nil {
			return serr.Wrap(err)
		}
		// New activity is never hidden: everyone else who archived the chat
		// gets it back. The author's own flag is left alone.
		err = q.MembersUnarchive(ctx, MembersUnarchiveParams{ChatID: chat.ID, AuthorID: userID})
		if err != nil {
			return serr.Wrap(err)
		}
		err = q.ChatTouch(ctx, ChatTouchParams{ID: chat.ID, LastMessageAt: now})
		if err != nil {
			return serr.Wrap(err)
		}
		return a.statUser(ctx, q, userID, UserStatsAddParams{MessagesSent: 1})
	})
	if err != nil {
		return msg, err
	}
	a.afterMessage(ctx, chat, msg)
	return msg, nil
}

// afterMessage runs after a new message is committed. It is the only place
// that fans out the side effects of a new message.
func (a *app) afterMessage(ctx context.Context, chat Chat, msg Message) {
	a.notifyChat(ctx, chat.ID)
	a.pushMessage(chat, msg)
	// Task 019 adds the link preview and task 020 the push notification.
}

func (a *app) chatSend(w http.ResponseWriter, r *http.Request) error {
	v, err := a.chatMember(r)
	if err != nil {
		return err
	}
	body, err := normalizeBody(r.PostFormValue("body"))
	if err != nil {
		return err
	}
	if body != "" {
		_, err = a.createMessage(r.Context(), v.Chat, v.User.ID, body, sql.NullInt64{})
		if err != nil {
			return err
		}
	}
	http.Redirect(w, r, chatPath(v.Chat.ID), http.StatusSeeOther)
	return nil
}

func (a *app) chatInfo(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	v, err := a.chatView(r)
	if err != nil {
		return err
	}
	chatID := v.Chat.ID
	base := chatPath(chatID)
	members, err := a.q.MembersOfChat(ctx, chatID)
	if err != nil {
		return serr.Wrap(err)
	}
	memberList := h.Ul(h.ID("members"), h.Class("list"), g.Map(members, func(m MembersOfChatRow) g.Node {
		return h.Li(h.ID(fmt.Sprintf("member-%d", m.UserID)), h.Class("row"),
			h.Span(h.Class("cluster"), g.Text(m.Username),
				g.If(m.UserID == v.Chat.CreatedBy, h.Span(h.Class("badge"), g.Text("creator")))),
			g.If(v.Manage && m.UserID != v.Chat.CreatedBy,
				h.Form(h.Method("post"), h.Action(base+"/members/remove"),
					h.Input(h.Type("hidden"), h.Name("user"), h.Value(strconv.FormatInt(m.UserID, 10))),
					h.Button(h.Class("btn secondary"), g.Text("Remove")))))
	}))

	var manage g.Node
	if v.Manage {
		others, err := a.q.UsersNotInChat(ctx, chatID)
		if err != nil {
			return serr.Wrap(err)
		}
		var add g.Node
		if len(others) > 0 {
			checks := make([]UsersAllBasicRow, len(others))
			for i, u := range others {
				checks[i] = UsersAllBasicRow(u)
			}
			add = h.Form(h.ID("add-members"), h.Method("post"), h.Action(base+"/members/add"),
				h.Class("stack card"),
				h.FieldSet(h.Class("stack-tight"), h.Legend(g.Text("Add members")), userChecks(checks)),
				h.Button(h.Class("btn"), g.Text("Add")))
		}
		manage = g.Group{
			h.Form(h.ID("rename"), h.Method("post"), h.Action(base+"/rename"), h.Class("stack card"),
				h.Label(g.Text("Name"), nameInput(v.Chat.Name)),
				h.Button(h.Class("btn"), g.Text("Rename"))),
			add,
		}
	}

	if v.Chat.IsDm {
		return a.chatPage(w, r, v.User, &v.Chat, h.Div(h.ID("info"), h.Class("pane-body stack"),
			pageHeader(v.Title, backLink(base, "Back")),
			h.P(g.Text("Direct message with "+v.Title+".")),
			archiveForm(v)))
	}
	return a.chatPage(w, r, v.User, &v.Chat, h.Div(h.ID("info"), h.Class("pane-body stack"),
		pageHeader(v.Title, backLink(base, "Back")),
		g.If(!v.Member, h.Form(h.ID("join"), h.Method("post"), h.Action(base+"/members/add"),
			h.Input(h.Type("hidden"), h.Name("user"), h.Value(strconv.FormatInt(v.User.ID, 10))),
			h.Button(h.Class("btn"), g.Text("Join")))),
		archiveForm(v),
		h.Section(h.Class("stack"),
			h.H2(g.Text("Members")),
			memberList),
		manage))
}

func (a *app) chatRename(w http.ResponseWriter, r *http.Request) error {
	v, err := a.chatManage(r)
	if err != nil {
		return err
	}
	back := chatPath(v.Chat.ID) + "/info"
	name, err := validChatName(r.PostFormValue("name"))
	if err != nil {
		return a.flash(w, r, back, Flash{Kind: flashError, Content: err.Error()})
	}
	if err := a.q.ChatRename(r.Context(), ChatRenameParams{ID: v.Chat.ID, Name: name}); err != nil {
		return serr.Wrap(err)
	}
	a.notifyChat(r.Context(), v.Chat.ID)
	return a.flash(w, r, back, Flash{Kind: flashSuccess, Content: "Group renamed."})
}

func (a *app) chatMembersAdd(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	v, err := a.chatManage(r)
	if err != nil {
		return err
	}
	ids, err := formIDs(r, "user")
	if err != nil {
		return err
	}
	err = a.tx(ctx, func(q *Queries) error {
		return a.addMembers(ctx, q, v.Chat.ID, v.User.ID, ids)
	})
	if err != nil {
		return err
	}
	a.notifyChat(ctx, v.Chat.ID)
	http.Redirect(w, r, chatPath(v.Chat.ID)+"/info", http.StatusSeeOther)
	return nil
}

func (a *app) chatMembersRemove(w http.ResponseWriter, r *http.Request) error {
	v, err := a.chatManage(r)
	if err != nil {
		return err
	}
	id, err := formID(r, "user")
	if err != nil {
		return err
	}
	back := chatPath(v.Chat.ID) + "/info"
	if id == v.Chat.CreatedBy {
		return a.flash(w, r, back, Flash{Kind: flashError,
			Content: "The creator of a group cannot be removed."})
	}
	err = a.q.MemberRemove(r.Context(), MemberRemoveParams{ChatID: v.Chat.ID, UserID: id})
	if err != nil {
		return serr.Wrap(err)
	}
	// The removed user is no longer among the members notifyChat pings.
	a.notifyChat(r.Context(), v.Chat.ID)
	a.hub.notifyUsers(id)
	http.Redirect(w, r, back, http.StatusSeeOther)
	return nil
}

// direct messages

func dmKey(a, b int64) string {
	return fmt.Sprintf("%d:%d", min(a, b), max(a, b))
}

// startDM returns the direct message between two users, creating it when
// there is none yet. created reports whether this call created it.
func (a *app) startDM(ctx context.Context, userID, otherID int64) (chat Chat, created bool, err error) {
	key := sql.NullString{String: dmKey(userID, otherID), Valid: true}
	err = a.tx(ctx, func(q *Queries) error {
		var err error
		chat, err = q.ChatByDMKey(ctx, key)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return serr.Wrap(err)
		}
		now := a.now().Unix()
		chat, err = q.ChatCreateDM(ctx, ChatCreateDMParams{
			DmKey:         key,
			CreatedBy:     userID,
			CreatedAt:     now,
			LastMessageAt: now,
		})
		if err != nil {
			return err // not wrapped: a unique violation is handled below
		}
		created = true
		return a.addMembers(ctx, q, chat.ID, userID, []int64{userID, otherID})
	})
	if isUniqueErr(err) {
		// A concurrent request created it first.
		chat, err = a.q.ChatByDMKey(ctx, key)
		return chat, false, serr.Wrap(err)
	}
	return chat, created, serr.Wrap(err)
}

func (a *app) dm(w http.ResponseWriter, r *http.Request) error {
	user := userFrom(r.Context())
	all, err := a.q.UsersAllBasic(r.Context())
	if err != nil {
		return serr.Wrap(err)
	}
	var rows []g.Node
	for _, u := range all {
		if u.ID == user.ID {
			continue
		}
		rows = append(rows, h.Li(h.ID(fmt.Sprintf("dm-user-%d", u.ID)),
			h.Form(h.Method("post"), h.Action(pDM),
				h.Input(h.Type("hidden"), h.Name("user"), h.Value(strconv.FormatInt(u.ID, 10))),
				h.Button(h.Class("btn ghost user-pick"), icon("person"), h.Span(g.Text(u.Username))))))
	}
	var list g.Node = h.P(h.Class("muted"), g.Text("There is nobody else here yet."))
	if len(rows) > 0 {
		list = h.Ul(h.ID("dm-users"), h.Class("list"), g.Group(rows))
	}
	return a.pageSimple(w, r, "New message", backLink(pHome, "Home"), list).Render(w)
}

func (a *app) dmPost(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	user := userFrom(ctx)
	otherID, err := strconv.ParseInt(r.PostFormValue("user"), 10, 64)
	if err != nil || otherID == user.ID {
		return errNotFound
	}
	if _, err := a.q.UserByID(ctx, otherID); errors.Is(err, sql.ErrNoRows) {
		return errNotFound
	} else if err != nil {
		return serr.Wrap(err)
	}
	chat, created, err := a.startDM(ctx, user.ID, otherID)
	if err != nil {
		return err
	}
	if created {
		a.notifyChat(ctx, chat.ID)
	}
	http.Redirect(w, r, chatPath(chat.ID), http.StatusSeeOther)
	return nil
}

// editing and deleting

func messagePath(chatID, msgID int64) string {
	return fmt.Sprintf("%s/m/%d", chatPath(chatID), msgID)
}

// canEdit reports whether the viewer may edit the message: the author, while
// still a member, and only a text message.
func canEdit(v chatView, m Message) bool {
	return m.UserID == v.User.ID && v.Member && !m.FileID.Valid
}

// canDelete reports whether the viewer may delete the message: the author
// while still a member, or an admin who can view the chat.
func canDelete(v chatView, m Message) bool {
	return (m.UserID == v.User.ID && v.Member) || allowed(v.User, accAdmin)
}

// messageMenu builds the per message popover with the actions the viewer may
// use. It renders nothing when there are none.
func (a *app) messageMenu(v chatView, m Message) g.Node {
	id := fmt.Sprintf("m%d", m.ID)
	base := messagePath(v.Chat.ID, m.ID)
	// Task 021 adds the snooze choices.
	var entries g.Group
	if v.Member {
		entries = append(entries, h.Form(h.ID(id+"-pick"), h.Class("picker"), h.Method("post"),
			h.Action(chatPath(v.Chat.ID)+"/react"), g.Attr("data-morph"),
			h.Input(h.Type("hidden"), h.Name("message"), h.Value(strconv.FormatInt(m.ID, 10))),
			g.Map(reactionPalette, func(e string) g.Node {
				return h.Button(h.Name("emoji"), h.Value(e), h.Aria("label", "React with "+e), g.Text(e))
			})))
	}
	if canEdit(v, m) {
		entries = append(entries, h.A(h.ID(id+"-edit"), h.Class("sheet-item"), h.Href(base+"/edit"),
			g.Text("Edit")))
	}
	if canDelete(v, m) {
		entries = append(entries, h.Details(h.ID(id+"-del"), h.Class("sheet-confirm"),
			h.Summary(h.Class("sheet-item"), g.Text("Delete")),
			h.Form(h.Method("post"), h.Action(base+"/delete"), g.Attr("data-morph"),
				h.Button(h.Class("btn danger"), g.Text("Delete this message")))))
	}
	if len(entries) == 0 {
		return nil
	}
	return g.Group{
		h.Button(h.ID(id+"-more"), h.Class("msg-more"), h.Type("button"),
			g.Attr("popovertarget", id+"-menu"), h.Aria("label", "Message actions"), icon("dots")),
		h.Div(h.ID(id+"-menu"), g.Attr("popover"), h.Class("sheet"), entries),
	}
}

// chatMessage loads the chat and the message named in the path. A message
// that is not in that chat is not found.
func (a *app) chatMessage(r *http.Request) (chatView, Message, error) {
	v, err := a.chatView(r)
	if err != nil {
		return v, Message{}, err
	}
	id, err := strconv.ParseInt(r.PathValue("mid"), 10, 64)
	if err != nil {
		return v, Message{}, errNotFound
	}
	m, err := a.q.MessageByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && m.ChatID != v.Chat.ID) {
		return v, m, errNotFound
	}
	return v, m, serr.Wrap(err)
}

// editableMessage is chatMessage for a message the viewer may edit.
func (a *app) editableMessage(r *http.Request) (chatView, Message, error) {
	v, m, err := a.chatMessage(r)
	if err == nil && !canEdit(v, m) {
		return v, m, errNotFound
	}
	return v, m, err
}

func (a *app) messageEdit(w http.ResponseWriter, r *http.Request) error {
	v, m, err := a.editableMessage(r)
	if err != nil {
		return err
	}
	return a.chatPage(w, r, v.User, &v.Chat, h.Div(h.ID("edit"), h.Class("pane-body stack"),
		pageHeader("Edit message", backLink(chatPath(v.Chat.ID), v.Title)),
		h.Form(h.Method("post"), h.Action(messagePath(v.Chat.ID, m.ID)+"/edit"), h.Class("stack"),
			h.Textarea(h.ID("edit-body"), h.Name("body"), h.Rows("6"), h.Required(),
				h.MaxLength(strconv.Itoa(maxMessageBody)), h.AutoComplete("off"),
				h.Aria("label", "Message"), g.Text(m.Body)),
			h.Button(h.Class("btn"), g.Text("Save")))))
}

func (a *app) messageEditPost(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	v, m, err := a.editableMessage(r)
	if err != nil {
		return err
	}
	body, err := normalizeBody(r.PostFormValue("body"))
	if err != nil {
		return err
	}
	if body == "" {
		return a.flash(w, r, messagePath(v.Chat.ID, m.ID)+"/edit", Flash{Kind: flashError,
			Content: "A message cannot be empty. Delete it instead."})
	}
	if body != m.Body {
		m.Body, m.EditedAt = body, a.now().Unix()
		err := a.q.MessageUpdateBody(ctx, MessageUpdateBodyParams{ID: m.ID, Body: m.Body, EditedAt: m.EditedAt})
		if err != nil {
			return serr.Wrap(err)
		}
		a.afterEdit(ctx, v.Chat, m)
	}
	http.Redirect(w, r, chatPath(v.Chat.ID), http.StatusSeeOther)
	return nil
}

func (a *app) messageDelete(w http.ResponseWriter, r *http.Request) error {
	v, m, err := a.chatMessage(r)
	if err != nil {
		return err
	}
	if !canDelete(v, m) {
		return errNotFound
	}
	if err := a.deleteMessage(r.Context(), v.Chat, m); err != nil {
		return err
	}
	http.Redirect(w, r, chatPath(v.Chat.ID), http.StatusSeeOther)
	return nil
}

// afterEdit runs after a message's text was changed. It is the only place
// for the side effects of an edit.
func (a *app) afterEdit(ctx context.Context, chat Chat, msg Message) {
	a.notifyChat(ctx, chat.ID)
	// Task 019 refreshes the link preview here.
}

// deleteMessage is the only place a message is removed. Deletes are hard
// deletes: reactions and snoozes cascade.
func (a *app) deleteMessage(ctx context.Context, chat Chat, msg Message) error {
	// Task 019 also removes the preview and its image here.
	cleanup := func() {}
	err := a.tx(ctx, func(q *Queries) error {
		// The message goes first: it holds the reference to the file.
		if err := q.MessageDelete(ctx, msg.ID); err != nil {
			return serr.Wrap(err)
		}
		if !msg.FileID.Valid {
			return nil
		}
		f, err := q.FileByID(ctx, msg.FileID.Int64)
		if err != nil {
			return serr.Wrap(err)
		}
		cleanup, err = a.deleteFile(ctx, q, f)
		return err
	})
	if err != nil {
		return err
	}
	cleanup()
	a.notifyChat(ctx, chat.ID)
	return nil
}

// reactions

// reactionPalette is the fixed list of emoji a message can be reacted with,
// in the order they are shown.
var reactionPalette = []string{
	"👍", "❤️", "😂", "😮", "😢", "🙏", "🎉", "🔥", "👏", "😍", "🤔", "👀", "💯", "✅", "❌", "😡",
}

// reactionGroup is one emoji on one message, with the users who used it in
// the order they reacted.
type reactionGroup struct {
	Emoji string
	Users []string
	Mine  bool
}

// emojiID is the emoji's code points in hex joined with "-", for element ids.
func emojiID(e string) string {
	var parts []string
	for _, r := range e {
		parts = append(parts, strconv.FormatInt(int64(r), 16))
	}
	return strings.Join(parts, "-")
}

// reactionsFor loads the reactions of the given messages with one query and
// groups them by message, each in palette order.
func (a *app) reactionsFor(ctx context.Context, viewerID int64, rows []MessagesLatestRow) (map[int64][]reactionGroup, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.Message.ID
	}
	all, err := a.q.ReactionsForMessages(ctx, ids)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	out := map[int64][]reactionGroup{}
	for _, rr := range all {
		i := slices.Index(reactionPalette, rr.Emoji)
		if i < 0 {
			continue // not offered any more
		}
		groups := out[rr.MessageID]
		if groups == nil {
			groups = make([]reactionGroup, len(reactionPalette))
			out[rr.MessageID] = groups
		}
		groups[i].Emoji = rr.Emoji
		groups[i].Users = append(groups[i].Users, rr.Username)
		if rr.UserID == viewerID {
			groups[i].Mine = true
		}
	}
	for id, groups := range out {
		out[id] = slices.DeleteFunc(groups, func(rg reactionGroup) bool { return len(rg.Users) == 0 })
	}
	return out, nil
}

// reactionChips renders the reactions of a message. The div is always
// rendered, so its id stays stable for the morph; CSS hides it when empty.
// Members get a button per emoji that toggles their own reaction.
func (a *app) reactionChips(v chatView, m Message, groups []reactionGroup) g.Node {
	id := fmt.Sprintf("m%d", m.ID)
	if len(groups) == 0 {
		return h.Div(h.ID(id+"-reactions"), h.Class("reactions"))
	}
	chips := g.Map(groups, func(rg reactionGroup) g.Node {
		class := "chip"
		if rg.Mine && v.Member {
			class += " on"
		}
		chipID := h.ID(id + "-r-" + emojiID(rg.Emoji))
		content := g.Group{h.Class(class), h.Title(strings.Join(rg.Users, ", ")),
			g.Text(rg.Emoji + " "), h.Span(g.Text(strconv.Itoa(len(rg.Users))))}
		if v.Member {
			return h.Button(chipID, h.Name("emoji"), h.Value(rg.Emoji), content)
		}
		return h.Span(chipID, content)
	})
	if !v.Member {
		return h.Div(h.ID(id+"-reactions"), h.Class("reactions"), chips)
	}
	return h.Div(h.ID(id+"-reactions"), h.Class("reactions"),
		h.Form(h.Method("post"), h.Action(chatPath(v.Chat.ID)+"/react"), g.Attr("data-morph"),
			h.Input(h.Type("hidden"), h.Name("message"), h.Value(strconv.FormatInt(m.ID, 10))),
			chips))
}

// chatReact toggles the user's reaction with one emoji on one message.
// Reactions do not bump the chat, unarchive it or send a push.
func (a *app) chatReact(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	v, err := a.chatMember(r)
	if err != nil {
		return err
	}
	emoji := r.PostFormValue("emoji")
	if !slices.Contains(reactionPalette, emoji) {
		return httpErr(http.StatusBadRequest, "Unknown emoji.")
	}
	msgID, err := strconv.ParseInt(r.PostFormValue("message"), 10, 64)
	if err != nil {
		return errNotFound
	}
	_, err = a.q.MessageInChat(ctx, MessageInChatParams{ID: msgID, ChatID: v.Chat.ID})
	if errors.Is(err, sql.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return serr.Wrap(err)
	}
	err = a.tx(ctx, func(q *Queries) error {
		n, err := q.ReactionDelete(ctx, ReactionDeleteParams{MessageID: msgID, UserID: v.User.ID, Emoji: emoji})
		if err != nil {
			return serr.Wrap(err)
		}
		if n > 0 {
			return nil
		}
		return serr.Wrap(q.ReactionAdd(ctx, ReactionAddParams{
			MessageID: msgID,
			UserID:    v.User.ID,
			Emoji:     emoji,
			CreatedAt: a.now().Unix(),
		}))
	})
	if err != nil {
		return err
	}
	a.notifyChat(ctx, v.Chat.ID)
	http.Redirect(w, r, chatPath(v.Chat.ID), http.StatusSeeOther)
	return nil
}
