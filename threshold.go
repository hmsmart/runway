package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type spendLevel int

const (
	levelNone   spendLevel = iota
	levelOuter             // 50% consumed
	levelMiddle            // 75% consumed
	levelInner             // 90% consumed
	levelDanger            // 100%+ consumed
)

var spendThresholds = [4]struct {
	level spendLevel
	pct   float64
	label string
	emoji string
}{
	{levelOuter, 0.50, "OUTER MARKER", "🔵"},
	{levelMiddle, 0.75, "MIDDLE MARKER", "🟡"},
	{levelInner, 0.90, "INNER MARKER", "🔴"},
	{levelDanger, 1.00, "OVER BUDGET", "🚨"},
}

type thresholdTracker struct {
	mu    sync.Mutex
	state map[string]spendLevel // key: "userID:2006-01-02"
}

func newThresholdTracker() *thresholdTracker {
	return &thresholdTracker{state: make(map[string]spendLevel)}
}

func (tt *thresholdTracker) highestNotified(userID, date string) spendLevel {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.state[userID+":"+date]
}

func (tt *thresholdTracker) record(userID, date string, level spendLevel) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	key := userID + ":" + date
	if level > tt.state[key] {
		tt.state[key] = level
	}
}

func (t *TelegramBot) checkSpendThresholdsForChat(ctx context.Context, chatID int64) {
	userRow, err := t.store.GetUserByTelegram(ctx, &chatID)
	if err != nil {
		return
	}
	t.checkSpendThresholds(ctx, chatID, userRow.ID)
}

func (t *TelegramBot) checkSpendThresholds(ctx context.Context, chatID int64, userID string) {
	today := time.Now().Format(time.DateOnly)

	db := computeDailyBudget(ctx, t.store, userID)
	if !db.HasBudget {
		return
	}

	crossed := levelNone
	for _, th := range spendThresholds {
		if db.Consumed >= th.pct {
			crossed = th.level
		}
	}
	if crossed == levelNone {
		return
	}

	prev := t.thresholds.highestNotified(userID, today)
	if crossed <= prev {
		return
	}
	t.thresholds.record(userID, today, crossed)

	for _, th := range spendThresholds {
		if th.level == crossed {
			msg := fmt.Sprintf(
				"%s <b>%s — %.0f%% of daily budget</b>\n\n"+
					"Allowance  %s\n"+
					"Committed  -%s\n"+
					"Spent      -%s\n",
				th.emoji, th.label, db.Consumed*100,
				formatDollarsCents(db.Allowance),
				formatDollarsCents(db.Committed),
				formatDollarsCents(db.Spent),
			)
			if db.Correction > 0 {
				msg += fmt.Sprintf("Correction -%s\n", formatDollarsCents(db.Correction))
			}
			availStr := formatDollarsCents(db.Available)
			if db.Available < 0 {
				availStr = "-" + formatDollarsCents(-db.Available)
			}
			msg += fmt.Sprintf("Available   %s", availStr)

			if _, err := t.bot.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:    chatID,
				Text:      msg,
				ParseMode: models.ParseModeHTML,
			}); err != nil {
				slog.Error("failed to send threshold notification", "chatID", chatID, "level", th.label, "err", err)
			}
			break
		}
	}
}
