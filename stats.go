package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/daaku/serr"
	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// statUser adds the deltas in d to the stats of userID. Pass the q of an
// a.tx to count inside a transaction.
func (a *app) statUser(ctx context.Context, q *Queries, userID int64, d UserStatsAddParams) error {
	d.UserID = userID
	return serr.Wrap(q.UserStatsAdd(ctx, d))
}

// statChat adds the deltas in d to the stats of chatID.
func (a *app) statChat(ctx context.Context, q *Queries, chatID int64, d ChatStatsAddParams) error {
	d.ChatID = chatID
	return serr.Wrap(q.ChatStatsAdd(ctx, d))
}

// userStats returns the stats of userID, all zero when nothing was counted.
func (a *app) userStats(ctx context.Context, userID int64) (UserStat, error) {
	s, err := a.q.UserStatsGet(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return UserStat{UserID: userID}, nil
	}
	return s, serr.Wrap(err)
}

// chatStats returns the stats of chatID, all zero when nothing was counted.
func (a *app) chatStats(ctx context.Context, chatID int64) (ChatStat, error) {
	s, err := a.q.ChatStatsGet(ctx, chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatStat{ChatID: chatID}, nil
	}
	return s, serr.Wrap(err)
}

// rendering

const downloadNote = "Estimated from media links opened"

func statCount(label string, n int64) g.Node {
	return g.Group{h.Dt(g.Text(label)), h.Dd(g.Text(strconv.FormatInt(n, 10)))}
}

// statDownloaded is the downloaded figure with its note: the bytes never
// pass through this server, so it counts the media links that were opened.
func statDownloaded(n int64) g.Node {
	return g.Group{h.Dt(g.Text("Downloaded")),
		h.Dd(g.Text(fmtBytes(n)), h.Small(h.Class("stats-note"), g.Text(downloadNote)))}
}

// userStatsList renders a user's stats.
func userStatsList(s UserStat) g.Node {
	return h.Dl(h.Class("stats"),
		statCount("Messages sent", s.MessagesSent),
		statCount("Images uploaded", s.ImagesUploaded),
		statCount("Videos uploaded", s.VideosUploaded),
		statCount("Other files uploaded", s.FilesUploaded),
		h.Dt(g.Text("Uploaded")), h.Dd(g.Text(fmtBytes(s.BytesUploaded))),
		statDownloaded(s.BytesDownloaded),
		statCount("Notifications sent", s.PushesSent),
		statCount("Notifications opened", s.PushesOpened),
		statCount("Invites sent", s.InvitesSent))
}

// chatStatsList renders a group's or a direct message's stats.
func chatStatsList(s ChatStat) g.Node {
	return h.Dl(h.Class("stats"),
		statCount("Images uploaded", s.ImagesUploaded),
		statCount("Videos uploaded", s.VideosUploaded),
		statCount("Other files uploaded", s.FilesUploaded),
		h.Dt(g.Text("Uploaded")), h.Dd(g.Text(fmtBytes(s.BytesUploaded))),
		statDownloaded(s.BytesDownloaded))
}

// userStatsSection is the Stats section for one user, on the settings page
// and the admin's page for that user.
func (a *app) userStatsSection(ctx context.Context, userID int64) (g.Node, error) {
	s, err := a.userStats(ctx, userID)
	if err != nil {
		return nil, err
	}
	return h.Section(h.ID("stats"), h.Class("stack"), h.H2(g.Text("Stats")), userStatsList(s)), nil
}

// settingsStats is the viewer's own stats on the settings page.
func (a *app) settingsStats(r *http.Request, u *User) (g.Node, error) {
	return a.userStatsSection(r.Context(), u.ID)
}

// chatStatsSection is the Stats section of a chat's info page. The info page
// is only shown to those who may see the chat's stats.
func (a *app) chatStatsSection(ctx context.Context, chatID int64) (g.Node, error) {
	s, err := a.chatStats(ctx, chatID)
	if err != nil {
		return nil, err
	}
	return h.Section(h.ID("stats"), h.Class("stack"), h.H2(g.Text("Stats")), chatStatsList(s)), nil
}

// adminGroups lists every group, never direct messages, with its stats.
func (a *app) adminGroups(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.q.GroupsAllWithStats(r.Context())
	if err != nil {
		return serr.Wrap(err)
	}
	cell := func(label string, children ...g.Node) g.Node {
		return h.Td(h.Data("label", label), g.Group(children))
	}
	num := func(n int64) g.Node { return g.Text(strconv.FormatInt(n, 10)) }
	var list g.Node = h.P(h.Class("muted"), g.Text("No groups yet."))
	if len(rows) > 0 {
		list = h.Table(h.ID("groups"), h.Class("table"),
			h.THead(h.Tr(
				h.Th(g.Text("Name")), h.Th(g.Text("Members")), h.Th(g.Text("Created")),
				h.Th(g.Text("Creator")), h.Th(g.Text("Images")), h.Th(g.Text("Videos")),
				h.Th(g.Text("Other files")), h.Th(g.Text("Uploaded")), h.Th(g.Text("Downloaded")))),
			h.TBody(g.Map(rows, func(row GroupsAllWithStatsRow) g.Node {
				c := row.Chat
				return h.Tr(h.ID(fmt.Sprintf("group-%d", c.ID)),
					cell("Name", h.A(h.Href(chatPath(c.ID)+"/info"), g.Text(c.Name))),
					cell("Members", num(row.MemberCount)),
					cell("Created", g.Text(fmtTime(time.Unix(c.CreatedAt, 0), nil))),
					cell("Creator", g.Text(row.Creator)),
					cell("Images", num(row.ImagesUploaded)),
					cell("Videos", num(row.VideosUploaded)),
					cell("Other files", num(row.FilesUploaded)),
					cell("Uploaded", g.Text(fmtBytes(row.BytesUploaded))),
					cell("Downloaded", g.Text(fmtBytes(row.BytesDownloaded))))
			})))
	}
	return a.page(w, r, "Groups",
		h.Div(h.Class("stack"),
			pageHeader("Groups", backLink(pAdmin, "Admin")),
			list,
			h.P(h.Class("muted"), g.Text("Downloaded: "+downloadNote+".")))).Render(w)
}
