package telegram

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

	userID := b.generateDeterministicUUID(msg.Chat.ID)
	sessionID := fmt.Sprintf("tg-%d", msg.Chat.ID)

	if err := b.upsertUser(ctx, userID); err != nil {
		slog.Error("Failed to upsert telegram user", slog.Any("error", err), slog.Int64("chat_id", msg.Chat.ID))
		b.sendReply(msg.Chat.ID, "Възникна грешка при обработка на потребителя.")
		return
	}

	req := agent.ChatRequest{
		SessionID: sessionID,
		Content:   msg.Text,
	}

	// Send an initial "typing" action to make it feel responsive
	action := tgbotapi.NewChatAction(msg.Chat.ID, tgbotapi.ChatTyping)
	b.api.Send(action)

	answer, err := b.orchestrator.ProcessChat(ctx, userID, req)
	if err != nil {
		slog.Error("Failed to process telegram chat", slog.Any("error", err), slog.Int64("chat_id", msg.Chat.ID))
		b.sendReply(msg.Chat.ID, "Възникна грешка при генерирането на отговор.")
		return
	}

	b.sendReply(msg.Chat.ID, answer)
}

func (b *Bot) sendReply(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		slog.Error("Failed to send telegram reply", slog.Any("error", err), slog.Int64("chat_id", chatID))
	}
}
