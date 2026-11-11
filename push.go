package main

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/daaku/serr"
	"github.com/daaku/webpush"
	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

const (
	pushTimeout     = 30 * time.Second
	pushConcurrency = 8
	pushSnippetLen  = 120
)

// pushSender delivers one push message to one subscription. It is a
// *webpush.Client, and a fake in tests.
type pushSender interface {
	Send(ctx context.Context, message []byte, s *webpush.Subscription) error
}

// pushSubscriber is the VAPID subscriber claim. Push services only accept an
// https URL there (mailto would be contact information), so a plain http
// development origin is named by its host under https.
func pushSubscriber(publicURL string) string {
	if strings.HasPrefix(publicURL, "https://") {
		return publicURL
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return publicURL
	}
	return "https://" + u.Host
}

// loadVAPID loads the VAPID key, generating and storing it on first start,
// and sets up the real sender. Reading it back after the insert makes two
// racing starts agree.
func (a *app) loadVAPID(ctx context.Context) error {
	fresh, err := webpush.GenerateVAPIDKey()
	if err != nil {
		return serr.Wrap(err)
	}
	err = a.q.ConfigSetDefault(ctx, ConfigSetDefaultParams{Key: "vapid", Value: []byte(fresh)})
	if err != nil {
		return serr.Wrap(err)
	}
	stored, err := a.q.ConfigGet(ctx, "vapid")
	if err != nil {
		return serr.Wrap(err)
	}
	key, err := webpush.ParseVAPIDKey(string(stored))
	if err != nil {
		return serr.Wrap(err)
	}
	pub, err := vapidPublic(key)
	if err != nil {
		return err
	}
	a.vapidPublic = pub
	a.push = &webpush.Client{
		Client:     &http.Client{Timeout: pushTimeout},
		VAPIDKey:   key,
		Subscriber: pushSubscriber(a.cfg.PublicURL),
		TTL:        24 * time.Hour,
		Urgency:    webpush.UrgencyHigh,
	}
	return nil
}

// vapidPublic is the public half of the key in base64url, as the browser's
// pushManager.subscribe wants it.
func vapidPublic(key *ecdsa.PrivateKey) (string, error) {
	b, err := key.PublicKey.Bytes()
	if err != nil {
		return "", serr.Wrap(err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// subscriptions

// parseSubscription reads a browser's PushSubscription JSON and checks it.
func parseSubscription(raw []byte) (*webpush.Subscription, error) {
	var s webpush.Subscription
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errors.New("not a push subscription")
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the endpoint must be https")
	}
	if s.Keys.Auth == "" || s.Keys.P256dh == "" {
		return nil, errors.New("the keys are missing")
	}
	return &s, nil
}

// pushSubscribe stores the browser's subscription for the signed in user. A
// known endpoint is taken over: the device re-subscribed, or someone else
// signed in on the same browser.
func (a *app) pushSubscribe(w http.ResponseWriter, r *http.Request) error {
	user := userFrom(r.Context())
	s, err := parseSubscription([]byte(r.PostFormValue(inputJSON)))
	if err != nil {
		return httpErr(http.StatusBadRequest, "The push subscription was not understood.")
	}
	stored, err := json.Marshal(s)
	if err != nil {
		return serr.Wrap(err)
	}
	err = a.q.PushSubUpsert(r.Context(), PushSubUpsertParams{
		UserID:       user.ID,
		Endpoint:     s.Endpoint,
		Subscription: stored,
		Label:        deviceLabel(r),
		CreatedAt:    a.now().Unix(),
	})
	if err != nil {
		return serr.Wrap(err)
	}
	return a.flash(w, r, pSettings, Flash{Kind: flashSuccess,
		Content: "Notifications are on for this device."})
}

func (a *app) pushUnsubscribe(w http.ResponseWriter, r *http.Request) error {
	user := userFrom(r.Context())
	id, err := formID(r, "id")
	if err != nil {
		return err
	}
	n, err := a.q.PushSubDelete(r.Context(), PushSubDeleteParams{ID: id, UserID: user.ID})
	if err != nil {
		return serr.Wrap(err)
	}
	if n == 0 {
		return errNotFound
	}
	return a.flash(w, r, pSettings+"#notifications", Flash{Kind: flashSuccess,
		Content: "Device removed."})
}

// pushOpened counts a notification the user clicked. The service worker
// posts it.
func (a *app) pushOpened(w http.ResponseWriter, r *http.Request) error {
	user := userFrom(r.Context())
	if err := a.statUser(r.Context(), a.q, user.ID, UserStatsAddParams{PushesOpened: 1}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// serviceWorker serves sw.js at the root, so that it controls the whole
// origin. It is never cached, so an update is picked up on the next load.
func (a *app) serviceWorker(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Cache-Control", "no-cache")
	return a.static("sw.js")(w, r)
}

// settingsNotifications is the Notifications section of the settings page.
// Which hint and controls show depends on the push-* class main.js puts on
// <body>.
func (a *app) settingsNotifications(r *http.Request, u *User) (g.Node, error) {
	subs, err := a.q.PushSubsByUser(r.Context(), u.ID)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	var list g.Node = h.P(h.Class("muted"), g.Text("No devices get notifications."))
	if len(subs) > 0 {
		list = h.Ul(h.ID("push-subs"), h.Class("list"), g.Map(subs, func(s PushSubscription) g.Node {
			return h.Li(h.ID(fmt.Sprintf("push-sub-%d", s.ID)), h.Class("row"),
				h.Div(h.Class("stack-tight"),
					h.Strong(g.Text(s.Label)),
					dateCell("Added", s.CreatedAt)),
				h.Form(h.Method("post"), h.Action(pPushUnsubscribe),
					h.Input(h.Type("hidden"), h.Name("id"), h.Value(strconv.FormatInt(s.ID, 10))),
					h.Button(h.Class("btn secondary"), g.Text("Remove"))))
		}))
	}
	return h.Section(h.ID("notifications"), h.Class("stack"),
		h.H2(g.Text("Notifications")),
		h.P(h.Class("push-hint denied"), g.Text("Notifications are blocked on this device. "+
			"Allow them for this site in the browser's site settings, then reload this page.")),
		h.P(h.Class("push-hint unavailable"), g.Text("This browser cannot show notifications. "+
			"On an iPhone or iPad, add the app to the home screen first and open it from there.")),
		h.P(h.Class("install-hint muted"), g.Text("On iPhone and iPad: tap Share, then Add to Home Screen, "+
			"and open the app from there to enable notifications.")),
		h.Form(h.Method("post"), h.Action(pPushSubscribe), h.Class("push-subscribe stack"),
			h.Input(h.Type("hidden"), h.Name(inputJSON)),
			h.Button(h.Class("btn"), h.Type("button"), h.Data("push-subscribe", a.vapidPublic),
				icon("bell"), h.Span(g.Text("Enable notifications on this device"))),
			h.P(h.Class("form-error"), h.Role("alert"))),
		list,
	), nil
}

// sending

// pushPayload is what the service worker shows.
type pushPayload struct {
	Title string          `json:"title"`
	Body  string          `json:"body"`
	Tag   string          `json:"tag"`
	Data  pushPayloadData `json:"data"`
}

type pushPayloadData struct {
	URL string `json:"url"`
}

// pushSnippet describes a message in a notification: the start of its
// text, or what kind of file it is.
func (a *app) pushSnippet(ctx context.Context, msg Message) (string, error) {
	if msg.FileID.Valid {
		f, err := a.q.FileByID(ctx, msg.FileID.Int64)
		if err != nil {
			return "", serr.Wrap(err)
		}
		return fileSnippet(f.Kind, f.Name), nil
	}
	if utf8.RuneCountInString(msg.Body) <= pushSnippetLen {
		return msg.Body, nil
	}
	return string([]rune(msg.Body)[:pushSnippetLen]), nil
}

// messagePayload is the notification for a new message.
func (a *app) messagePayload(ctx context.Context, chat Chat, msg Message) ([]byte, error) {
	author, err := a.q.UserByID(ctx, msg.UserID)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	text, err := a.pushSnippet(ctx, msg)
	if err != nil {
		return nil, err
	}
	p := pushPayload{
		Title: chat.Name,
		Body:  author.Username + ": " + text,
		Tag:   fmt.Sprintf("chat-%d", chat.ID),
		Data:  pushPayloadData{URL: chatPath(chat.ID)},
	}
	if chat.IsDm {
		p.Title, p.Body = author.Username, text
	}
	b, err := json.Marshal(p)
	return b, serr.Wrap(err)
}

// pushMessage sends a new message to the devices of the other members who
// are not looking at the chat right now. It does not block the caller.
func (a *app) pushMessage(chat Chat, msg Message) {
	a.bg.Go(func() {
		ctx := a.ctx
		if err := a.pushMessageNow(ctx, chat, msg); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "push message %d: %+v\n", msg.ID, err)
		}
	})
}

func (a *app) pushMessageNow(ctx context.Context, chat Chat, msg Message) error {
	ids, err := a.q.MemberIDs(ctx, chat.ID)
	if err != nil {
		return serr.Wrap(err)
	}
	var to []int64
	for _, id := range ids {
		if id != msg.UserID && !a.hub.viewing(id, chat.ID) {
			to = append(to, id)
		}
	}
	if len(to) == 0 {
		return nil
	}
	subs, err := a.q.PushSubsByUsers(ctx, to)
	if err != nil {
		return serr.Wrap(err)
	}
	if len(subs) == 0 {
		return nil
	}
	payload, err := a.messagePayload(ctx, chat, msg)
	if err != nil {
		return err
	}
	a.pushSubs(subs, payload)
	return nil
}

// pushTo sends payload to every device of the user. It does not block.
func (a *app) pushTo(userID int64, payload []byte) {
	subs, err := a.q.PushSubsByUser(a.ctx, userID)
	if err != nil {
		if a.ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "push to user %d: %+v\n", userID, serr.Wrap(err))
		}
		return
	}
	a.pushSubs(subs, payload)
}

// pushSubs sends payload to each subscription in the background, at most
// pushConcurrency at a time across the process. A delivered push is counted
// for the subscription's user. A subscription the push service says is gone
// for good, or one that cannot be read, is deleted. Other failures are
// logged and not retried.
func (a *app) pushSubs(subs []PushSubscription, payload []byte) {
	for _, row := range subs {
		a.bg.Go(func() {
			select {
			case a.pushSem <- struct{}{}:
			case <-a.ctx.Done():
				return
			}
			defer func() { <-a.pushSem }()
			ctx, cancel := context.WithTimeout(a.ctx, pushTimeout)
			defer cancel()

			var s webpush.Subscription
			if err := json.Unmarshal(row.Subscription, &s); err != nil {
				a.dropSub(ctx, row)
				return
			}
			err := a.push.Send(ctx, payload, &s)
			we, ok := errors.AsType[*webpush.Error](err)
			if ok && we.Permanent {
				a.dropSub(ctx, row)
				return
			}
			if err != nil {
				// Only the host: the rest of the endpoint is a secret, and
				// other errors can quote it.
				host := ""
				if u, perr := url.Parse(row.Endpoint); perr == nil {
					host = u.Host
				}
				status := ""
				if ok {
					status = fmt.Sprintf(" with status %d", we.StatusCode)
				}
				fmt.Fprintf(os.Stderr, "push to %s failed%s\n", host, status)
				return
			}
			if err := a.statUser(ctx, a.q, row.UserID, UserStatsAddParams{PushesSent: 1}); err != nil {
				fmt.Fprintf(os.Stderr, "push stat: %+v\n", err)
			}
		})
	}
}

func (a *app) dropSub(ctx context.Context, row PushSubscription) {
	if err := a.q.PushSubDeleteByID(ctx, row.ID); err != nil {
		fmt.Fprintf(os.Stderr, "delete push subscription %d: %+v\n", row.ID, serr.Wrap(err))
	}
}

// snoozes

const (
	snoozeInterval = 30 * time.Second
	snoozeHour     = 9 // "morning", in the viewer's time zone
)

// snoozeChoices are the times a message can be snoozed until, in the order
// they are offered.
var snoozeChoices = []struct{ Value, Label string }{
	{"1h", "In 1 hour"},
	{"3h", "In 3 hours"},
	{"tomorrow", "Tomorrow morning"},
	{"week", "Next week"},
}

// snoozeDue is when a snooze chosen at now in loc comes due. Calendar days
// are counted with time.Date in loc, so a daylight saving change in between
// still gives 09:00 local time.
func snoozeDue(when string, now time.Time, loc *time.Location) (time.Time, bool) {
	local := now.In(loc)
	y, m, d := local.Date()
	switch when {
	case "1h":
		return now.Add(time.Hour), true
	case "3h":
		return now.Add(3 * time.Hour), true
	case "tomorrow":
		return time.Date(y, m, d+1, snoozeHour, 0, 0, 0, loc), true
	case "week":
		return time.Date(y, m, d+7, snoozeHour, 0, 0, 0, loc), true
	}
	return time.Time{}, false
}

// fmtDue describes a due time for the viewer: only the time when it is
// today, "Tomorrow 09:00" for tomorrow, otherwise the date and the time.
func fmtDue(due, now time.Time, loc *time.Location) string {
	due, now = due.In(loc), now.In(loc)
	y, m, d := now.Date()
	switch dy, dm, dd := due.Date(); {
	case dy == y && dm == m && dd == d:
		return due.Format("15:04")
	case due.Format("2006-01-02") == time.Date(y, m, d+1, 0, 0, 0, 0, loc).Format("2006-01-02"):
		return "Tomorrow " + due.Format("15:04")
	case dy == y:
		return due.Format("Mon 2 Jan 15:04")
	}
	return due.Format("2 Jan 2006 15:04")
}

// snoozeView is what the chat page needs to render the viewer's snoozes.
type snoozeView struct {
	due     map[int64]string // message id to the formatted due time
	hasPush bool             // the viewer has at least one push subscription
}

// snoozesFor loads the viewer's snoozes on the rendered messages and whether
// they can get a reminder at all, with one query each.
func (a *app) snoozesFor(ctx context.Context, userID int64, rows []MessagesLatestRow, loc *time.Location) (snoozeView, error) {
	sv := snoozeView{due: map[int64]string{}}
	n, err := a.q.PushSubCountByUser(ctx, userID)
	if err != nil {
		return sv, serr.Wrap(err)
	}
	sv.hasPush = n > 0
	if len(rows) == 0 {
		return sv, nil
	}
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.Message.ID
	}
	snoozes, err := a.q.SnoozesForUserMessages(ctx, SnoozesForUserMessagesParams{UserID: userID, Ids: ids})
	if err != nil {
		return sv, serr.Wrap(err)
	}
	now := a.now()
	for _, s := range snoozes {
		sv.due[s.MessageID] = fmtDue(time.Unix(s.DueAt, 0), now, loc)
	}
	return sv, nil
}

// snoozeMarker is the clock on a message the viewer snoozed, nil otherwise.
func snoozeMarker(v chatView, m Message) g.Node {
	due, ok := v.Snoozes.due[m.ID]
	if !ok {
		return nil
	}
	return h.Span(h.ID(fmt.Sprintf("m%d-snooze", m.ID)), h.Class("msg-snooze"),
		h.Title("Reminder "+due), icon("clock"), g.Text(due))
}

// snoozeMenu is the "Remind me" section of a message's menu.
func snoozeMenu(v chatView, m Message) g.Node {
	if !v.Member {
		return nil
	}
	id := fmt.Sprintf("m%d", m.ID)
	base := messagePath(v.Chat.ID, m.ID)
	due, snoozed := v.Snoozes.due[m.ID]
	var current g.Node
	if snoozed {
		current = h.Form(h.ID(id+"-unsnooze"), h.Class("row"), h.Method("post"), h.Action(base+"/unsnooze"),
			g.Attr("data-morph"),
			h.Span(h.Class("muted"), g.Text("Reminder "+due)),
			h.Button(h.Class("btn secondary"), g.Text("Cancel reminder")))
	}
	return h.Div(h.ID(id+"-remind"), h.Class("sheet-remind"),
		h.Div(h.Class("sheet-label"), g.Text("Remind me")),
		current,
		g.If(!v.Snoozes.hasPush, h.P(h.Class("muted"),
			h.A(h.Href(pSettings+"#notifications"), g.Text("Enable notifications in Settings")),
			g.Text(" to get reminders."))),
		h.Form(h.ID(id+"-snooze-pick"), h.Method("post"), h.Action(base+"/snooze"), g.Attr("data-morph"),
			g.Map(snoozeChoices, func(c struct{ Value, Label string }) g.Node {
				return h.Button(h.Class("sheet-item"), h.Name("when"), h.Value(c.Value), g.Text(c.Label))
			})))
}

// snoozedMessage is chatMessage for a member, who may snooze.
func (a *app) snoozedMessage(r *http.Request) (chatView, Message, error) {
	v, m, err := a.chatMessage(r)
	if err == nil && !v.Member {
		return v, m, errNotFound
	}
	return v, m, err
}

func (a *app) messageSnooze(w http.ResponseWriter, r *http.Request) error {
	v, m, err := a.snoozedMessage(r)
	if err != nil {
		return err
	}
	now := a.now()
	due, ok := snoozeDue(r.PostFormValue("when"), now, viewerLoc(r))
	if !ok {
		return httpErr(http.StatusBadRequest, "Unknown reminder time.")
	}
	err = a.q.SnoozeUpsert(r.Context(), SnoozeUpsertParams{
		MessageID: m.ID,
		UserID:    v.User.ID,
		DueAt:     due.Unix(),
		CreatedAt: now.Unix(),
	})
	if err != nil {
		return serr.Wrap(err)
	}
	a.hub.notifyUsers(v.User.ID) // private: nobody else is told
	http.Redirect(w, r, chatPath(v.Chat.ID), http.StatusSeeOther)
	return nil
}

func (a *app) messageUnsnooze(w http.ResponseWriter, r *http.Request) error {
	v, m, err := a.snoozedMessage(r)
	if err != nil {
		return err
	}
	err = a.q.SnoozeDelete(r.Context(), SnoozeDeleteParams{MessageID: m.ID, UserID: v.User.ID})
	if err != nil {
		return serr.Wrap(err)
	}
	a.hub.notifyUsers(v.User.ID)
	http.Redirect(w, r, chatPath(v.Chat.ID), http.StatusSeeOther)
	return nil
}

// startSnoozes fires due reminders now, which covers those that came due
// while the server was down, and then every snoozeInterval until shutdown.
func (a *app) startSnoozes() {
	a.bg.Go(func() {
		tick := time.NewTicker(snoozeInterval)
		defer tick.Stop()
		for {
			a.snoozeRound(a.ctx)
			select {
			case <-tick.C:
			case <-a.ctx.Done():
				return
			}
		}
	})
}

// snoozeRound fires the reminders that are due, oldest first.
func (a *app) snoozeRound(ctx context.Context) {
	due, err := a.q.SnoozesDue(ctx, a.now().Unix())
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "snoozes due: %+v\n", serr.Wrap(err))
		}
		return
	}
	for _, s := range due {
		if err := a.fireSnooze(ctx, s); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "snooze %d/%d: %+v\n", s.MessageID, s.UserID, err)
		}
	}
}

// fireSnooze sends one reminder. Taking the row first, guarded by its due
// time, makes a reminder fire at most once and leaves alone one that was
// just moved to a later time.
func (a *app) fireSnooze(ctx context.Context, s Snooze) error {
	n, err := a.q.SnoozeTake(ctx, SnoozeTakeParams{MessageID: s.MessageID, UserID: s.UserID, DueAt: s.DueAt})
	if err != nil {
		return serr.Wrap(err)
	}
	if n == 0 {
		return nil
	}
	msg, err := a.q.MessageByID(ctx, s.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return serr.Wrap(err)
	}
	_, err = a.q.MemberGet(ctx, MemberGetParams{ChatID: msg.ChatID, UserID: s.UserID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no longer a member: dropped silently
	}
	if err != nil {
		return serr.Wrap(err)
	}
	payload, err := a.reminderPayload(ctx, s.UserID, msg)
	if err != nil {
		return err
	}
	a.pushTo(s.UserID, payload)
	return nil
}

// reminderPayload is the notification for a snoozed message, titled with
// the chat as the user sees it.
func (a *app) reminderPayload(ctx context.Context, userID int64, msg Message) ([]byte, error) {
	chat, err := a.q.ChatByID(ctx, msg.ChatID)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	viewer, err := a.q.UserByID(ctx, userID)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	title, err := a.chatName(ctx, chat, &viewer)
	if err != nil {
		return nil, err
	}
	author, err := a.q.UserByID(ctx, msg.UserID)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	text, err := a.pushSnippet(ctx, msg)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(pushPayload{
		Title: "Reminder · " + title,
		Body:  author.Username + ": " + text,
		Tag:   fmt.Sprintf("snooze-%d", msg.ID),
		Data:  pushPayloadData{URL: fmt.Sprintf("%s?m=%d", chatPath(chat.ID), msg.ID)},
	})
	return b, serr.Wrap(err)
}

// jumpToMessage answers /c/{id}?m={mid}, the link in a reminder: it shows
// enough of the newest messages to include that one, and scrolls to it.
func (a *app) jumpToMessage(w http.ResponseWriter, r *http.Request, v chatView, mid string) error {
	ctx := r.Context()
	to := chatPath(v.Chat.ID)
	id, err := strconv.ParseInt(mid, 10, 64)
	if err == nil {
		_, err = a.q.MessageInChat(ctx, MessageInChatParams{ID: id, ChatID: v.Chat.ID})
		switch {
		case err == nil:
			n, err := a.q.MessagesCountFrom(ctx, MessagesCountFromParams{ChatID: v.Chat.ID, ID: id})
			if err != nil {
				return serr.Wrap(err)
			}
			to = fmt.Sprintf("%s?n=%d#m%d", to, max(defaultPage, n), id)
		case !errors.Is(err, sql.ErrNoRows):
			return serr.Wrap(err)
		}
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
	return nil
}
