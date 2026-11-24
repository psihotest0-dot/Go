package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"

	"go-monolith/internal/api"
	"go-monolith/internal/bills"
)

// billReader reads a bill into the conversation (*bills.Service).
type billReader interface {
	Read(ctx context.Context, userID uuid.UUID, session, name string, data []byte) (string, error)
}

// handleBill reads the PDF or the picture of a message as a bill (an e-mail's PDF forwarded to the
// bot, a photo of a paper one) and answers with what was read and the question whether to keep it;
// the "да" that follows is a chat turn of the same conversation, answered by the code.
func (b *Bot) handleBill(ctx context.Context, chatID int64, userID uuid.UUID, msg *tgbotapi.Message) {
	var fileID, name string
	var size int
	switch {
	case msg.Document != nil:
		fileID, name, size = msg.Document.FileID, msg.Document.FileName, msg.Document.FileSize
		mime := strings.ToLower(msg.Document.MimeType)
		if mime != "application/pdf" && !strings.HasPrefix(mime, "image/") && !strings.HasSuffix(strings.ToLower(name), ".pdf") {
			b.sendReply(chatID, "Мога да чета като сметка PDF или снимка. Другите файлове — през уеб приложението.")
			return
		}
	case len(msg.Photo) > 0:
		// The largest of the sizes Telegram keeps of a photo.
		p := msg.Photo[len(msg.Photo)-1]
		fileID, name, size = p.FileID, "снимка.jpg", p.FileSize
	}
	if size > bills.MaxFileBytes {
		b.sendReply(chatID, fmt.Sprintf("Файлът е над %d MB — не мога да го прочета.", bills.MaxFileBytes>>20))
		return
	}
	stopTyping := b.keepTyping(ctx, chatID)
	defer stopTyping()
	data, err := b.download(ctx, fileID)
	if err != nil {
		slog.Error("the file of a bill could not be downloaded from Telegram", slog.Any("error", err))
		b.sendReply(chatID, "Не успях да сваля файла от Telegram. Опитай пак.")
		return
	}
	text, err := b.bills.Read(ctx, userID, api.TelegramSessionID(chatID), name, data)
	if err != nil {
		slog.Error("a bill was read but not written into the conversation", slog.Any("error", err))
	}
	b.sendReply(chatID, text)
}

// downloadFile fetches the file of a message. Its address holds the token: the client is the bot's
// own (it takes the token out of any error), and the address is never logged.
func downloadFile(ctx context.Context, botAPI *tgbotapi.BotAPI, token, fileID string) ([]byte, error) {
	file, err := botAPI.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.Link(token), nil)
	if err != nil {
		return nil, fmt.Errorf("the request of the file could not be made")
	}
	resp, err := botAPI.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram answered %d for the file", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, bills.MaxFileBytes+1))
}
