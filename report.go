package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/hmsmart/runway/database/sqlcgen"
)

// clockFormat is the stored shape of a report time: zero-padded 24h, so
// string comparison is time-of-day order. All schedule times are interpreted
// in the server's timezone.
const clockFormat = "15:04"

// reportUsage is the help shown for a bare or malformed /report.
const reportUsage = "Send <code>/report</code> followed by a time to get your runway report every day, " +
	"e.g. <code>/report 8:00</code> or <code>/report 5pm</code>. <code>/report off</code> stops it."

// handleReport sets, shows, or clears the user's daily report schedule:
// "/report [daily] TIME" schedules, "/report off" clears, bare "/report"
// shows the current setting.
func (t *TelegramBot) handleReport(ctx context.Context, b *bot.Bot, update *models.Update) {
	user := UserFromContext(ctx)
	chatID := update.Message.Chat.ID
	slog.Info("called report", "chatID", chatID)
	args := strings.Fields(update.Message.Text)[1:]
	// "daily" is decorative — it's the only cadence there is.
	if len(args) > 0 && strings.EqualFold(args[0], "daily") {
		args = args[1:]
	}

	if len(args) == 0 {
		if cur := user.ReportTime(); cur != nil {
			t.sendText(ctx, b, chatID, fmt.Sprintf(
				"Your daily report is scheduled for <b>%s</b>. Send <code>/report</code> with a new time to move it, or <code>/report off</code> to stop it.", *cur))
			return
		}
		t.sendText(ctx, b, chatID, "You don't have a daily report scheduled. "+reportUsage)
		return
	}

	if strings.EqualFold(args[0], "off") || strings.EqualFold(args[0], "stop") {
		err := t.store.SetReportSchedule(ctx, sqlcgen.SetReportScheduleParams{
			ReportTime:   nil,
			ReportSentOn: nil,
			ID:           user.ID(),
		})
		if err != nil {
			slog.Error("failed to clear report schedule", "user", user.ID(), "err", err)
			t.sendText(ctx, b, chatID, errTryLater)
			return
		}
		t.sendText(ctx, b, chatID, "Daily report stopped. Schedule a new one anytime, e.g. <code>/report 8:00</code>.")
		return
	}

	// Join the remaining tokens so "8:00 am" parses the same as "8:00am".
	hhmm, err := parseClockTime(strings.Join(args, ""))
	if err != nil {
		slog.Info("invalid report time", "chatID", chatID, "message", update.Message.Text)
		t.sendText(ctx, b, chatID, "I couldn't read that time. "+reportUsage)
		return
	}

	// If the chosen time is still ahead today, leave the sent marker clear so
	// the first report lands today; otherwise stamp today so it starts
	// tomorrow instead of firing the moment it's set.
	now := time.Now()
	var sentOn *string
	if hhmm <= now.Format(clockFormat) {
		today := now.Format(time.DateOnly)
		sentOn = &today
	}
	err = t.store.SetReportSchedule(ctx, sqlcgen.SetReportScheduleParams{
		ReportTime:   &hhmm,
		ReportSentOn: sentOn,
		ID:           user.ID(),
	})
	if err != nil {
		slog.Error("failed to set report schedule", "user", user.ID(), "err", err)
		t.sendText(ctx, b, chatID, errTryLater)
		return
	}
	slog.Info("report scheduled", "chatID", chatID, "time", hhmm)
	first := "tomorrow"
	if sentOn == nil {
		first = "today"
	}
	t.sendText(ctx, b, chatID, fmt.Sprintf(
		"📬 Daily report scheduled for <b>%s</b>, starting %s. <code>/report off</code> stops it.", hhmm, first))
}

// parseClockTime normalizes user time input — "8", "8:30", "08:30", "8am",
// "8:30pm", "noon"-adjacent 12am/12pm — to zero-padded 24h "HH:MM".
func parseClockTime(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	meridiem := ""
	for _, m := range []string{"am", "pm"} {
		if strings.HasSuffix(s, m) {
			meridiem = m
			s = strings.TrimSpace(strings.TrimSuffix(s, m))
			break
		}
	}
	hs, ms, hasMinutes := strings.Cut(s, ":")
	if !hasMinutes {
		ms = "00"
	}
	h, err := strconv.Atoi(hs)
	if err != nil {
		return "", fmt.Errorf("bad hour %q", hs)
	}
	m, err := strconv.Atoi(ms)
	if err != nil || m < 0 || m > 59 || len(ms) > 2 {
		return "", fmt.Errorf("bad minutes %q", ms)
	}
	switch meridiem {
	case "am":
		if h < 1 || h > 12 {
			return "", fmt.Errorf("bad 12h hour %d", h)
		}
		if h == 12 { // 12am is midnight
			h = 0
		}
	case "pm":
		if h < 1 || h > 12 {
			return "", fmt.Errorf("bad 12h hour %d", h)
		}
		if h != 12 { // 12pm is noon and stays 12
			h += 12
		}
	default:
		if h < 0 || h > 23 {
			return "", fmt.Errorf("bad 24h hour %d", h)
		}
	}
	return fmt.Sprintf("%02d:%02d", h, m), nil
}

// sendDueReports delivers the runway report to every user whose scheduled
// time has passed without a report yet today. Because dueness is "time has
// passed" plus a per-day sent marker (not an exact-minute match), a slot
// missed to downtime is delivered late on the next tick rather than never.
func (t *TelegramBot) sendDueReports(ctx context.Context) {
	now := time.Now()
	today := now.Format(time.DateOnly)
	hhmm := now.Format(clockFormat)
	due, err := t.store.ListDueReports(ctx, sqlcgen.ListDueReportsParams{
		ReportTime:   &hhmm,
		ReportSentOn: &today,
	})
	if err != nil {
		slog.Error("failed to list due reports", "err", err)
		return
	}
	for _, u := range due {
		msg, err := t.buildRunwayReport(ctx, u.ID)
		if err != nil && !errors.Is(err, errNoSpendHistory) {
			// Transient failure: leave the sent marker unset so the next
			// tick retries.
			slog.Error("failed to build scheduled report", "user", u.ID, "err", err)
			continue
		}
		if err == nil {
			slog.Info("sending scheduled report", "user", u.ID)
			t.sendText(ctx, t.bot, *u.TgID, msg)
		}
		// No-history users are marked without a send: nothing to report
		// today, and retrying every tick wouldn't change that.
		err = t.store.MarkReportSent(ctx, sqlcgen.MarkReportSentParams{
			ReportSentOn: &today,
			ID:           u.ID,
		})
		if err != nil {
			slog.Error("failed to mark report sent", "user", u.ID, "err", err)
		}
	}
}
