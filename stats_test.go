package main

import (
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
