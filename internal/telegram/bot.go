package telegram

import (
	"context"
	"fmt"
	"log/slog"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"

	"go-monolith/internal/agent"
)

type Bot struct {
	api          *tgbotapi.BotAPI
	orchestrator *agent.Orchestrator
	dbPool       *pgxpool.Pool
}

func NewBot(token string, orch *agent.Orchestrator, pool *pgxpool.Pool) (*Bot, error) {
	if token == "" {
		return nil, fmt.Errorf("telegram token is empty")
	}

	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}

	return &Bot{
		api:          api,
		orchestrator: orch,
		dbPool:       pool,
	}, nil
}

func (b *Bot) generateDeterministicUUID(chatID int64) uuid.UUID {
	namespace := uuid.Must(uuid.Parse("00000000-0000-0000-0000-000000000000"))
	data := []byte(fmt.Sprintf("telegram-chat-%d", chatID))
	// Using NewSHA1 to generate version 5 UUID
	return uuid.NewSHA1(namespace, data)
}

func (b *Bot) upsertUser(ctx context.Context, userID uuid.UUID) error {
	_, err := b.dbPool.Exec(ctx, "INSERT INTO users (id) VALUES ($1) ON CONFLICT (id) DO NOTHING;", userID)
	return err
}

func (b *Bot) Start(ctx context.Context) {
	slog.Info("Starting Telegram bot", slog.String("username", b.api.Self.UserName))

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			slog.Info("Stopping Telegram bot")
			return
		case update := <-updates:
			if update.Message != nil {
				go b.handleMessage(ctx, update.Message)
			}
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	if msg.Text == "" {
		return
	}

	if len(msg.Text) > 6 && msg.Text[:6] == "/link " {
		uidStr := strings.TrimSpace(msg.Text[6:])
		uid, err := uuid.Parse(uidStr)
		if err != nil {
			b.sendReply(msg.Chat.ID, "Невалиден UUID.")
			return
		}

		if err := b.upsertUser(ctx, uid); err != nil {
			slog.Error("Failed to upsert user", slog.Any("error", err))
			b.sendReply(msg.Chat.ID, "Грешка при запазване на потребителя.")
			return
		}

		_, err = b.dbPool.Exec(ctx, "INSERT INTO telegram_users (telegram_chat_id, user_id) VALUES ($1, $2) ON CONFLICT (telegram_chat_id) DO UPDATE SET user_id = EXCLUDED.user_id;", msg.Chat.ID, uid)
		if err != nil {
			slog.Error("Failed to link telegram user", slog.Any("error", err))
			b.sendReply(msg.Chat.ID, "Възникна грешка при свързването.")
			return
		}

		b.sendReply(msg.Chat.ID, "Успешно свързан с бекенда! Вече имам достъп до твоите документи и памет.")
		return
	}

	var userID uuid.UUID
	err := b.dbPool.QueryRow(ctx, "SELECT user_id FROM telegram_users WHERE telegram_chat_id = $1;", msg.Chat.ID).Scan(&userID)
	if err != nil {
		userID = b.generateDeterministicUUID(msg.Chat.ID)
		if err := b.upsertUser(ctx, userID); err != nil {
			slog.Error("Failed to upsert telegram user", slog.Any("error", err), slog.Int64("chat_id", msg.Chat.ID))
			b.sendReply(msg.Chat.ID, "Възникна грешка при обработка на потребителя.")
			return
		}
	}

	sessionID := fmt.Sprintf("tg-%d", msg.Chat.ID)

	req := agent.ChatRequest{
		SessionID: sessionID,
		Content:   msg.Text,
	}

	// Send an initial "typing" action to make it feel responsive
	action := tgbotapi.NewChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	b.api.Send(action)

	answer, reasoning, err := b.orchestrator.ProcessChat(ctx, userID, req)
	if err != nil {
		slog.Error("Failed to process telegram chat", slog.Any("error", err), slog.Int64("chat_id", msg.Chat.ID))
		b.sendReply(msg.Chat.ID, "Възникна грешка при генерирането на отговор.")
		return
	}

	finalReply := answer
	if reasoning != "" {
		finalReply = "*Лили мисли:*\n" + reasoning + "\n\n" + answer
	}

	b.sendReply(msg.Chat.ID, finalReply)
}

func (b *Bot) sendReply(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		slog.Error("Failed to send telegram reply", slog.Any("error", err), slog.Int64("chat_id", chatID))
	}
}
