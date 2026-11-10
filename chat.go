package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/daaku/serr"
	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

const maxChatName = 64

var msgChatName = fmt.Sprintf("A group name is 1 to %d characters.", maxChatName)

var errNotFound = httpErr(http.StatusNotFound, "Not found.")

func chatPath(id int64) string {
	return pChat + strconv.FormatInt(id, 10)
}

// access

// chatAccess loads a chat and decides what the user may do with it. A user
// who may not view it gets a 404, so its existence is not revealed.
func (a *app) chatAccess(ctx context.Context, user *User, chatID int64) (chat Chat, member bool, manage bool, err error) {
	chat, err = a.q.ChatByID(ctx, chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return chat, false, false, errNotFound
	}
	if err != nil {
		return chat, false, false, serr.Wrap(err)
	}
	_, err = a.q.MemberGet(ctx, MemberGetParams{ChatID: chatID, UserID: user.ID})
	switch {
	case err == nil:
		member = true
	case !errors.Is(err, sql.ErrNoRows):
		return chat, false, false, serr.Wrap(err)
	}
	if chat.IsDm {
		// Only the two participants can see a direct message, not even an
		// admin, and there is nothing to manage.
		if !member {
			return chat, false, false, errNotFound
		}
		return chat, true, false, nil
	}
	admin := allowed(user, accAdmin)
	if !member && !admin {
		return chat, false, false, errNotFound
	}
	manage = admin || (chat.CreatedBy == user.ID && user.PermGroup)
	return chat, member, manage, nil
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

// notifyChat tells everyone who can see the chat that it changed.
func (a *app) notifyChat(ctx context.Context, chatID int64) {
	// Task 011 implements this: ping the members and viewers over SSE.
}

// chatView is a chat as seen by the signed in user.
type chatView struct {
	Chat   Chat
	User   *User
	Member bool
	Manage bool
	Title  string
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
	chat, member, manage, err := a.chatAccess(ctx, user, id)
	if err != nil {
		return chatView{}, err
	}
	title, err := a.chatName(ctx, chat, user)
	if err != nil {
		return chatView{}, err
	}
	return chatView{Chat: chat, User: user, Member: member, Manage: manage, Title: title}, nil
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
	ctx := r.Context()
	chats, err := a.q.ChatsForUser(ctx, user.ID)
	if err != nil {
		return serr.Wrap(err)
	}
	title := "Home"
	var activeID int64
	appAttrs := g.Group{h.Class("chat-layout")}
	if active != nil {
		activeID = active.ID
		if title, err = a.chatName(ctx, *active, user); err != nil {
			return err
		}
		appAttrs = g.Group{h.Class("chat-layout has-chat"),
			h.Data("chat", strconv.FormatInt(active.ID, 10))}
	}

	headerLink := func(href, label, iconName string) g.Node {
		return h.A(h.Class("btn ghost icon-btn"), h.Href(href), h.Title(label),
			h.Aria("label", label), icon(iconName))
	}
	var list g.Node
	if len(chats) == 0 {
		var group g.Node = g.Text(" Someone needs to add you to a group.")
		if allowed(user, accGroup) {
			group = g.Group{g.Text(" or "), h.A(h.Href(pNew), g.Text("create a group")), g.Text(".")}
		}
		list = h.Div(h.ID("chats-empty"), h.Class("empty stack"),
			h.P(h.Strong(g.Text("No chats yet."))),
			h.P(h.A(h.Href(pDM), g.Text("Start a direct message")), group))
	} else {
		list = h.Div(h.ID("chats"), h.Class("chat-list"), g.Map(chats, func(row ChatsForUserRow) g.Node {
			c := row.Chat
			kind, kindIcon := "group", "people"
			if c.IsDm {
				kind, kindIcon = "dm", "person"
			}
			return h.A(h.ID(fmt.Sprintf("chat-%d", c.ID)), h.Class("chat-item "+kind),
				h.Href(chatPath(c.ID)),
				g.If(c.ID == activeID, h.Aria("current", "page")),
				icon(kindIcon),
				h.Span(h.Class("chat-name"), g.Text(a.chatTitle(c, user, row.Other))))
		}))
	}
	sidebar := h.Nav(h.ID("sidebar"),
		h.Header(h.Class("sidebar-head"),
			h.H1(g.Text(a.cfg.AppName)),
			h.Div(h.Class("cluster"),
				headerLink(pDM, "New message", "pencil"),
				g.If(allowed(user, accGroup), headerLink(pNew, "New group", "plus")),
				g.If(allowed(user, accInvite), headerLink(pInvites, "Invites", "person-plus")),
				g.If(allowed(user, accAdmin), headerLink(pAdmin, "Admin", "shield")),
				headerLink(pSettings, "Settings", "gear"))),
		list)
	return a.shell(w, r, title, "chat", appAttrs, sidebar,
		h.Section(h.ID("pane"), pane)).Render(w)
}

// paneHead is the header of a chat pane. The back link to home is only
// shown on narrow screens, where the chat list is not visible.
func paneHead(v chatView) g.Node {
	return h.Header(h.ID("pane-head"), h.Class("pane-head"),
		h.A(h.Class("back narrow-only"), h.Href(pHome), h.Aria("label", "Back"), icon("back")),
		h.A(h.Class("pane-title"), h.Href(chatPath(v.Chat.ID)+"/info"), g.Text(v.Title)))
}

func (a *app) home(w http.ResponseWriter, r *http.Request) error {
	return a.chatPage(w, r, userFrom(r.Context()), nil,
		h.Div(h.Class("placeholder"), h.P(g.Text("Select a group or a direct message"))))
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
	v, err := a.chatView(r)
	if err != nil {
		return err
	}
	return a.chatPage(w, r, v.User, &v.Chat, g.Group{
		paneHead(v),
		h.Div(h.ID("scroll"), h.Class("scroll"),
			h.Div(h.ID("messages"))),
	})
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
			h.P(g.Text("Direct message with "+v.Title+"."))))
	}
	return a.chatPage(w, r, v.User, &v.Chat, h.Div(h.ID("info"), h.Class("pane-body stack"),
		pageHeader(v.Title, backLink(base, "Back")),
		g.If(!v.Member, h.Form(h.ID("join"), h.Method("post"), h.Action(base+"/members/add"),
			h.Input(h.Type("hidden"), h.Name("user"), h.Value(strconv.FormatInt(v.User.ID, 10))),
			h.Button(h.Class("btn"), g.Text("Join")))),
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
