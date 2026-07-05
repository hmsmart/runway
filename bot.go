package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/hmsmart/runway/database"
)

type TelegramBot struct {
	bot    *bot.Bot
	chatID int64
}

func NewTelegramBot(token string, chatID int64) (*TelegramBot, error) {
	b, err := bot.New(token)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}
	return &TelegramBot{bot: b, chatID: chatID}, nil
}

func (t *TelegramBot) userFilter(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, update *models.Update) {
		chatID := int64(0)
		if update.Message != nil {
			chatID = update.Message.Chat.ID
		} else if update.CallbackQuery != nil && update.CallbackQuery.Message.Message != nil {
			chatID = update.CallbackQuery.Message.Message.Chat.ID
		}
		if chatID != t.chatID {
			return
		}
		next(ctx, b, update)
	}
}

func (t *TelegramBot) RegisterHandlers(store *database.Store) {
	t.bot.RegisterHandler(bot.HandlerTypeMessageText, "/ping", bot.MatchTypeExact,
		t.userFilter(func(ctx context.Context, b *bot.Bot, update *models.Update) {
			slog.Info("got ping", "chatID", update.Message.Chat.ID)
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: update.Message.Chat.ID,
				Text:   "pong",
			})
		}),
	)
}
