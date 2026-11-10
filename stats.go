package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/daaku/serr"
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
