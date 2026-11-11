package main

import (
	"context"
	"crypto/ecdsa"
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
