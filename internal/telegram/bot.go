package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go-monolith/internal/agent"
	"go-monolith/internal/proposals"
)

const (
	// maxConcurrentChats bounds how many chat turns run at once across all
	// chats. The LLM has two slots; more would only queue up there.
	maxConcurrentChats = 2

	// chatTurnTimeout bounds one question-answer round.
	chatTurnTimeout = 3 * time.Minute

	// maxReplyRunes is below Telegram's 4096 character limit per message.
	maxReplyRunes = 4000

	// shutdownGrace is how long Start waits for in-flight turns after cancellation.
	shutdownGrace = 10 * time.Second
)

// sender is the part of the Telegram API the handlers write to.
type sender interface {
	Send(c tgbotapi.Chattable) (tgbotapi.Message, error)
}

// chatProcessor answers one user turn; *agent.Orchestrator implements it.
type chatProcessor interface {
	ProcessChatTurn(ctx context.Context, userID uuid.UUID, req agent.ChatRequest) (agent.ChatTurn, error)
}

// proposalDesk is where the user decides on the writes the model proposed (a
// global rule, a memory); *proposals.Service implements it.
type proposalDesk interface {
	Pending(ctx context.Context, userID uuid.UUID) ([]proposals.Proposal, error)
	Approve(ctx context.Context, userID uuid.UUID, id int64) (proposals.Proposal, error)
	Reject(ctx context.Context, userID uuid.UUID, id int64) (proposals.Proposal, error)
}

// userStore maps Telegram chats to backend users.
type userStore interface {
	// LinkedUser reports the user a chat is linked to. A chat that is not linked
	// is (uuid.Nil, false, nil); err is only set for real failures.
	LinkedUser(ctx context.Context, chatID int64) (uuid.UUID, bool, error)
	UpsertUser(ctx context.Context, id uuid.UUID) error
	LinkChat(ctx context.Context, chatID int64, userID uuid.UUID) error
}

type pgUserStore struct {
	pool *pgxpool.Pool
}

func (s pgUserStore) LinkedUser(ctx context.Context, chatID int64) (uuid.UUID, bool, error) {
	var userID uuid.UUID
	err := s.pool.QueryRow(ctx, "SELECT user_id FROM telegram_users WHERE telegram_chat_id = $1;", chatID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return userID, true, nil
}

func (s pgUserStore) UpsertUser(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO users (id) VALUES ($1) ON CONFLICT (id) DO NOTHING;", id)
	return err
}

func (s pgUserStore) LinkChat(ctx context.Context, chatID int64, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO telegram_users (telegram_chat_id, user_id) VALUES ($1, $2) ON CONFLICT (telegram_chat_id) DO UPDATE SET user_id = EXCLUDED.user_id;", chatID, userID)
	return err
}

type Bot struct {
	// api is only needed by Start; the handlers use sender.
	api       *tgbotapi.BotAPI
	sender    sender
	processor chatProcessor
	store     userStore
	desk      proposalDesk

	// allowed is the set of chats that may talk to the bot. Telegram only
	// authenticates the bot's token, not who writes to it, so everybody else is
	// ignored. An empty set denies everyone.
	allowed map[int64]struct{}

	// slots is the global limit on concurrent chat turns.
	slots chan struct{}

	// chatLocks serialises the turns of one chat so replies cannot overtake
	// each other. Only allowed chats get an entry, so the map stays small.
	locksMu   sync.Mutex
	chatLocks map[int64]chan struct{}

	wg sync.WaitGroup
}

func newBot(s sender, processor chatProcessor, store userStore, desk proposalDesk, allowedChatIDs []int64) *Bot {
	allowed := make(map[int64]struct{}, len(allowedChatIDs))
	for _, id := range allowedChatIDs {
		allowed[id] = struct{}{}
	}
	return &Bot{
		sender:    s,
		processor: processor,
		store:     store,
		desk:      desk,
		allowed:   allowed,
		slots:     make(chan struct{}, maxConcurrentChats),
		chatLocks: make(map[int64]chan struct{}),
	}
}

func NewBot(token string, allowedChatIDs []int64, orch *agent.Orchestrator, pool *pgxpool.Pool, desk *proposals.Service) (*Bot, error) {
	if token == "" {
		return nil, fmt.Errorf("telegram token is empty")
	}

	// The client timeout must outlast the 60s long-poll in Start.
	api, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, &http.Client{Timeout: 90 * time.Second})
	if err != nil {
		// Transport errors embed the request URL, which contains the bot token.
		return nil, errors.New(strings.ReplaceAll(err.Error(), token, "<redacted>"))
	}

	bot := newBot(api, orch, pgUserStore{pool: pool}, desk, allowedChatIDs)
	bot.api = api
	return bot, nil
}

const (
	initialConnectBackoff = 5 * time.Second
	maxConnectBackoff     = 5 * time.Minute
)

// Run connects to Telegram and serves updates until ctx is cancelled. A failed
// connection (network down, connection reset, ...) is retried with exponential
// backoff instead of leaving the bot disabled until the next restart.
func Run(ctx context.Context, token string, allowedChatIDs []int64, orch *agent.Orchestrator, pool *pgxpool.Pool, desk *proposals.Service) {
	if len(allowedChatIDs) == 0 {
		slog.Warn("TELEGRAM_ALLOWED_CHAT_IDS is empty: the bot answers nobody except /id, which prints the chat id to put there")
	}

	backoff := initialConnectBackoff
	for {
		bot, err := NewBot(token, allowedChatIDs, orch, pool, desk)
		if err == nil {
			bot.Start(ctx)
			return
		}

		slog.Error("failed to initialize telegram bot", slog.Any("error", err), slog.Duration("retry_in", backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxConnectBackoff)
	}
}

func (b *Bot) generateDeterministicUUID(chatID int64) uuid.UUID {
	namespace := uuid.Must(uuid.Parse("00000000-0000-0000-0000-000000000000"))
	data := fmt.Appendf(nil, "telegram-chat-%d", chatID)
	// Using NewSHA1 to generate version 5 UUID
	return uuid.NewSHA1(namespace, data)
}

func (b *Bot) Start(ctx context.Context) {
	slog.Info("Starting Telegram bot", slog.String("username", b.api.Self.UserName))

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)
	// Without this the long-poll goroutine keeps running after Start returns.
	defer b.api.StopReceivingUpdates()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Stopping Telegram bot")
			b.waitForTurns()
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			if update.Message != nil {
				b.wg.Add(1)
				go func() {
					defer b.wg.Done()
					defer b.recoverTurn(update.Message.Chat.ID)
					b.handleMessage(ctx, update.Message)
				}()
			}
		}
	}
}

// recoverTurn stops a panic in one conversation from taking the whole backend
// down: the goroutine of a turn is not covered by the HTTP recovery middleware
// or by River. It must be deferred directly so that recover works.
func (b *Bot) recoverTurn(chatID int64) {
	rcv := recover()
	if rcv == nil {
		return
	}
	slog.Error("panic while handling a Telegram message", "panic", rcv, "chat_id", chatID, "stack", string(debug.Stack()))
	b.sendReply(chatID, "Възникна грешка. Опитай отново.")
}

// waitForTurns gives in-flight turns a short time to notice the cancelled context.
func (b *Bot) waitForTurns() {
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		slog.Warn("telegram turns still running at shutdown")
	}
}

func (b *Bot) isAllowed(chatID int64) bool {
	_, ok := b.allowed[chatID]
	return ok
}

// parseCommand splits "/cmd@botname args" into ("cmd", "args"). Text that is not
// a command yields an empty command.
func parseCommand(text string) (command, args string) {
	if !strings.HasPrefix(text, "/") {
		return "", text
	}
	head, rest := text, ""
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		head, rest = text[:i], text[i:]
	}
	command = strings.ToLower(strings.TrimPrefix(head, "/"))
	if at := strings.IndexByte(command, '@'); at >= 0 {
		command = command[:at]
	}
	return command, strings.TrimSpace(rest)
}

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	command, args := parseCommand(msg.Text)

	// /id is open to everyone: it is how the owner finds the id to allow.
	if command == "id" {
		if !b.isAllowed(chatID) {
			slog.Warn("unauthorized chat", slog.Int64("id", chatID), slog.String("command", "/id"))
		}
		b.sendReply(chatID, fmt.Sprintf("Chat id: %d", chatID))
		return
	}

	if !b.isAllowed(chatID) {
		slog.Warn("unauthorized chat", slog.Int64("id", chatID))
		return
	}

	release, ok := b.lockChat(ctx, chatID)
	if !ok {
		return
	}
	defer release()

	if !msg.Chat.IsPrivate() {
		b.sendReply(chatID, "Мога да комуникирам само в лични чатове.")
		return
	}
	if msg.Voice != nil || msg.Audio != nil || len(msg.Photo) > 0 || msg.Document != nil {
		b.sendReply(chatID, "Засега разбирам само текст. Моля, качвай файлове през уеб приложението.")
		return
	}
	if msg.Sticker != nil || msg.Text == "" {
		return
	}

	if command == "link" {
		b.handleLink(ctx, chatID, args)
		return
	}

	if err := agent.ValidateUserMessage(msg.Text); err != nil {
		b.sendReply(chatID, "Съобщението е твърде дълго. Моля, изпрати по-кратко.")
		return
	}

	userID, linked, err := b.store.LinkedUser(ctx, chatID)
	if err != nil {
		slog.Error("Failed to look up telegram user", slog.Any("error", err), slog.Int64("chat_id", chatID))
		b.sendReply(chatID, "Възникна грешка при обработка на потребителя.")
		return
	}
	if !linked {
		userID = b.generateDeterministicUUID(chatID)
		if err := b.store.UpsertUser(ctx, userID); err != nil {
			slog.Error("Failed to upsert telegram user", slog.Any("error", err), slog.Int64("chat_id", chatID))
			b.sendReply(chatID, "Възникна грешка при обработка на потребителя.")
			return
		}
	}

	switch command {
	case "pending", "approve", "reject":
		b.handleProposalCommand(ctx, chatID, userID, command, args)
		return
	}

	if !b.acquireSlot(ctx) {
		return
	}
	defer b.releaseSlot()

	// Send an initial "typing" action to make it feel responsive
	_, _ = b.sender.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))

	turnCtx, cancel := context.WithTimeout(ctx, chatTurnTimeout)
	defer cancel()
	turn, err := b.processor.ProcessChatTurn(turnCtx, userID, agent.ChatRequest{
		SessionID: fmt.Sprintf("tg-%d", chatID),
		Content:   msg.Text,
	})
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}
		slog.Error("Failed to process telegram chat", slog.Any("error", err), slog.Int64("chat_id", chatID))
		b.sendReply(chatID, "Възникна грешка при генерирането на отговор.")
		return
	}
	answer := turn.Answer
	if strings.TrimSpace(answer) == "" && len(turn.Proposals) == 0 {
		// Telegram rejects an empty message.
		b.sendReply(chatID, "Не успях да формулирам отговор. Опитай отново.")
		return
	}

	if strings.TrimSpace(answer) != "" {
		b.sendReply(chatID, answer)
	}
	if len(turn.Proposals) > 0 {
		b.sendReply(chatID, proposalNotice(turn.Proposals))
	}
}

// handleProposalCommand serves /pending, /approve <id> and /reject <id>.
func (b *Bot) handleProposalCommand(ctx context.Context, chatID int64, userID uuid.UUID, command, args string) {
	if b.desk == nil {
		b.sendReply(chatID, "Предложенията не са налични.")
		return
	}

	if command == "pending" {
		list, err := b.desk.Pending(ctx, userID)
		if err != nil {
			slog.Error("Failed to list proposals", slog.Any("error", err), slog.Int64("chat_id", chatID))
			b.sendReply(chatID, "Възникна грешка при четенето на предложенията.")
			return
		}
		if len(list) == 0 {
			b.sendReply(chatID, "Няма предложения, които чакат потвърждение.")
			return
		}
		b.sendReply(chatID, proposalNotice(list))
		return
	}

	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || id < 1 {
		b.sendReply(chatID, fmt.Sprintf("Кажи кое предложение: /%s <номер>. Списъкът е с /pending.", command))
		return
	}

	decide := b.desk.Approve
	if command == "reject" {
		decide = b.desk.Reject
	}
	p, err := decide(ctx, userID, id)
	switch {
	case err == nil:
		if command == "approve" {
			b.sendReply(chatID, "✅ Запазено: "+p.Summary)
		} else {
			b.sendReply(chatID, "🗑 Отхвърлено: "+p.Summary)
		}
	case errors.Is(err, proposals.ErrNotFound):
		b.sendReply(chatID, "Няма такова предложение.")
	case errors.Is(err, proposals.ErrNotPending):
		b.sendReply(chatID, "Това предложение вече е решено.")
	case errors.Is(err, proposals.ErrTooManyRules):
		b.sendReply(chatID, fmt.Sprintf("Вече има %d глобални правила — първо изтрий някое от приложението.", proposals.MaxActiveRules))
	default:
		slog.Error("Failed to decide a proposal", slog.Any("error", err), slog.Int64("chat_id", chatID), slog.Int64("proposal_id", id))
		b.sendReply(chatID, "Възникна грешка. Опитай отново.")
	}
}

// proposalNotice lists proposals with the commands that decide them.
func proposalNotice(list []proposals.Proposal) string {
	var sb strings.Builder
	sb.WriteString("📝 Чака твоето потвърждение:")
	for _, p := range list {
		fmt.Fprintf(&sb, "\n\n%d. %s", p.ID, p.Summary)
		if warning := externalWarning(p.ExternalSources); warning != "" {
			sb.WriteString("\n" + warning)
		}
		fmt.Fprintf(&sb, "\n/approve %d  ·  /reject %d", p.ID, p.ID)
	}
	return sb.String()
}

// externalWarning tells where the text came from that the proposal may rest on.
func externalWarning(sources []string) string {
	var web, docs bool
	for _, source := range sources {
		switch source {
		case "web_search", "web_fetch":
			web = true
		default:
			docs = true
		}
	}
	switch {
	case web && docs:
		return "⚠️ Предложено след четене на интернет и документи — провери го."
	case web:
		return "⚠️ Предложено след четене на интернет — провери го."
	case docs:
		return "⚠️ Предложено след четене на документи — провери го."
	}
	return ""
}

// handleLink connects the (already authorised) chat to a backend user id.
func (b *Bot) handleLink(ctx context.Context, chatID int64, arg string) {
	uid, err := uuid.Parse(arg)
	if err != nil {
		b.sendReply(chatID, "Невалиден UUID.")
		return
	}

	// A failed lookup must not be read as "not linked yet": relinking on a
	// database hiccup would silently move the chat to another user.
	if _, linked, err := b.store.LinkedUser(ctx, chatID); err != nil {
		slog.Error("Failed to look up telegram link", slog.Any("error", err), slog.Int64("chat_id", chatID))
		b.sendReply(chatID, "Възникна грешка при свързването.")
		return
	} else if linked {
		b.sendReply(chatID, "Вече си свързан.")
		return
	}

	if err := b.store.UpsertUser(ctx, uid); err != nil {
		slog.Error("Failed to upsert user", slog.Any("error", err))
		b.sendReply(chatID, "Грешка при запазване на потребителя.")
		return
	}
	if err := b.store.LinkChat(ctx, chatID, uid); err != nil {
		slog.Error("Failed to link telegram user", slog.Any("error", err))
		b.sendReply(chatID, "Възникна грешка при свързването.")
		return
	}

	slog.Info("telegram chat linked", slog.Int64("chat_id", chatID), slog.String("user_id", uid.String()))
	b.sendReply(chatID, "Успешно свързан с бекенда! Вече имам достъп до твоите документи и памет.")
}

// lockChat serialises the turns of one chat. It gives up when ctx is cancelled.
func (b *Bot) lockChat(ctx context.Context, chatID int64) (release func(), ok bool) {
	b.locksMu.Lock()
	lock, exists := b.chatLocks[chatID]
	if !exists {
		lock = make(chan struct{}, 1)
		b.chatLocks[chatID] = lock
	}
	b.locksMu.Unlock()

	select {
	case lock <- struct{}{}:
		return func() { <-lock }, true
	case <-ctx.Done():
		return nil, false
	}
}

func (b *Bot) acquireSlot(ctx context.Context) bool {
	select {
	case b.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (b *Bot) releaseSlot() { <-b.slots }

func (b *Bot) sendReply(chatID int64, text string) {
	runes := []rune(text)
	for len(runes) > 0 {
		if len(runes) <= maxReplyRunes {
			b.send(chatID, string(runes))
			return
		}

		splitIndex := maxReplyRunes
		for i := maxReplyRunes; i > 0; i-- {
			if runes[i] == ' ' || runes[i] == '\n' || runes[i] == '\t' {
				splitIndex = i
				break
			}
		}

		b.send(chatID, string(runes[:splitIndex]))

		runes = runes[splitIndex:]
		if len(runes) > 0 && (runes[0] == ' ' || runes[0] == '\n' || runes[0] == '\t') {
			runes = runes[1:]
		}
	}
}

func (b *Bot) send(chatID int64, text string) {
	if utf8.RuneCountInString(text) == 0 {
		return
	}
	if _, err := b.sender.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		slog.Error("Failed to send telegram reply", slog.Any("error", err), slog.Int64("chat_id", chatID))
	}
}
