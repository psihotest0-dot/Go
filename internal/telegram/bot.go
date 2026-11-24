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
	"go-monolith/internal/api"
	"go-monolith/internal/proposals"
	"go-monolith/metrics"
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

	// typingInterval is how often the "typing..." action is repeated while a turn
	// runs. Telegram shows it for about 5 seconds, and a turn with tools and a
	// large prompt takes much longer.
	typingInterval = 4 * time.Second

	// typingStopWait bounds how long the end of a turn waits for a "typing"
	// request that is still on its way, so that it cannot arrive after the reply.
	typingStopWait = 2 * time.Second

	// The chats that are not allowed. Anybody who finds the bot can write to it, so
	// what they cost must not grow with how much they write: /id is answered once per
	// chat every idReplyEvery, at most maxStrangerReplies of those replies are in
	// flight at once, other text from them gets no goroutine at all, and the log
	// line about it is written once per chat every strangerLogEvery.
	idReplyEvery       = 10 * time.Second
	strangerLogEvery   = time.Minute
	maxStrangerReplies = 8
	maxThrottledChats  = 1024
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

	// bills reads a bill sent as a document or a picture (nil: the bills are off); download fetches
	// the file of a message.
	bills    billReader
	download func(ctx context.Context, fileID string) ([]byte, error)

	// typingEvery is typingInterval; a field so that tests can shorten it.
	typingEvery time.Duration

	// chatTails serialises the turns of one chat, in the order the messages
	// arrived, so replies cannot overtake each other. It holds the "done" channel
	// of the last turn queued for the chat (see reserveTurn). Only allowed chats
	// get an entry, so the map stays small.
	tailsMu   sync.Mutex
	chatTails map[int64]<-chan struct{}

	// usePaid is what every turn tells the model router (agent.ChatRequest.UsePaid): nil -
	// the default, false - the local model even for a question to the library
	// (TELEGRAM_LLM=local).
	usePaid *bool

	// idReplies and strangerLog throttle the chats that are not allowed;
	// strangerSlots bounds the /id replies in flight (see dispatch).
	idReplies     *throttle
	strangerLog   *throttle
	strangerSlots chan struct{}

	wg sync.WaitGroup
}

func newBot(s sender, processor chatProcessor, store userStore, desk proposalDesk, allowedChatIDs []int64) *Bot {
	allowed := make(map[int64]struct{}, len(allowedChatIDs))
	for _, id := range allowedChatIDs {
		allowed[id] = struct{}{}
	}
	return &Bot{
		sender:      s,
		processor:   processor,
		store:       store,
		desk:        desk,
		allowed:     allowed,
		slots:       make(chan struct{}, maxConcurrentChats),
		typingEvery: typingInterval,
		chatTails:   make(map[int64]<-chan struct{}),

		idReplies:     newThrottle(idReplyEvery, maxThrottledChats),
		strangerLog:   newThrottle(strangerLogEvery, maxThrottledChats),
		strangerSlots: make(chan struct{}, maxStrangerReplies),
	}
}

func NewBot(token string, allowedChatIDs []int64, orch *agent.Orchestrator, pool *pgxpool.Pool, desk *proposals.Service, localOnly bool) (*Bot, error) {
	if token == "" {
		return nil, fmt.Errorf("telegram token is empty")
	}

	api, err := newAPI(token, tgbotapi.APIEndpoint)
	if err != nil {
		return nil, err
	}

	bot := newBot(api, orch, pgUserStore{pool: pool}, desk, allowedChatIDs)
	bot.api = api
	if svc := orch.Bills(); svc != nil {
		bot.bills = svc
		bot.download = func(ctx context.Context, fileID string) ([]byte, error) { return downloadFile(ctx, api, token, fileID) }
	}
	bot.usePaid = usePaidFor(localOnly)
	return bot, nil
}

// usePaidFor is the choice every turn tells the model router: nothing by default (the
// router decides), false - the local model - when TELEGRAM_LLM=local.
func usePaidFor(localOnly bool) *bool {
	if !localOnly {
		return nil
	}
	local := false
	return &local
}

// newAPI connects to the Bot API. Transport errors embed the request URL, which
// contains the bot token, so nothing the library returns or prints may reach a
// log as it is (redact.go). endpoint has the form of tgbotapi.APIEndpoint.
func newAPI(token, endpoint string) (*tgbotapi.BotAPI, error) {
	// The library prints its own long-poll failures. The logger is a package
	// global: set it before the first call, never while a bot is polling.
	if err := tgbotapi.SetLogger(newLibraryLogger(token)); err != nil {
		return nil, err
	}

	// The client timeout must outlast the 60s long-poll in Start.
	client := newRedactingClient(&http.Client{Timeout: 90 * time.Second}, token)
	api, err := tgbotapi.NewBotAPIWithClient(token, endpoint, client)
	if err != nil {
		// Not every error comes from the client (e.g. a bad response body).
		return nil, redactToken(err, token)
	}
	return api, nil
}

const (
	initialConnectBackoff = 5 * time.Second
	maxConnectBackoff     = 5 * time.Minute
)

// Run connects to Telegram and serves updates until ctx is cancelled. A failed
// connection (network down, connection reset, ...) is retried with exponential
// backoff instead of leaving the bot disabled until the next restart.
func Run(ctx context.Context, token string, allowedChatIDs []int64, orch *agent.Orchestrator, pool *pgxpool.Pool, desk *proposals.Service, localOnly bool) {
	if len(allowedChatIDs) == 0 {
		slog.Warn("TELEGRAM_ALLOWED_CHAT_IDS is empty: the bot answers nobody except /id, which prints the chat id to put there")
	}
	if localOnly {
		slog.Info("Telegram answers from the local model only (TELEGRAM_LLM=local)")
	}

	backoff := initialConnectBackoff
	for {
		bot, err := NewBot(token, allowedChatIDs, orch, pool, desk, localOnly)
		if err == nil {
			// What is answered in the app to a conversation shared with this bot is repeated
			// here; the repeat stops with the bot.
			orch.SetReplyMirror(bot.Mirror)
			defer orch.SetReplyMirror(nil)
			// So is the report of a task the server walked in the background (package taskrun).
			orch.SetOutsideNotifier(bot.Notify)
			defer orch.SetOutsideNotifier(nil)
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
				b.dispatch(ctx, update.Message)
			}
		}
	}
}

// dispatch hands a message to a goroutine of its own. The place in the chat's
// queue is taken here, in the loop that reads the updates, and not in the
// goroutine: goroutines do not reach a lock in the order they were started, so
// two quick messages could be answered the wrong way round.
func (b *Bot) dispatch(ctx context.Context, msg *tgbotapi.Message) {
	if !b.isAllowed(msg.Chat.ID) {
		release, ok := b.admitStranger(msg)
		if !ok {
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer release()
			defer b.recoverTurn(msg.Chat.ID)
			b.handleMessageIn(ctx, msg, nil)
		}()
		return
	}

	t := b.reserveTurn(msg)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.recoverTurn(msg.Chat.ID)
		b.handleMessageIn(ctx, msg, t)
	}()
}

// admitStranger decides what a message from a chat that is not allowed costs. Only
// /id is ever answered (it is how the owner finds the id to allow), and then once per
// chat every idReplyEvery, with at most maxStrangerReplies replies in flight; the
// release function frees the place. Anything else is dropped here, without a goroutine,
// and logged at most once per chat every strangerLogEvery.
func (b *Bot) admitStranger(msg *tgbotapi.Message) (release func(), ok bool) {
	chatID := msg.Chat.ID
	now := time.Now()
	if command, _ := parseCommand(msg.Text); command != "id" {
		if b.strangerLog.allow(chatID, now) {
			slog.Warn("unauthorized chat", slog.Int64("id", chatID))
		}
		return nil, false
	}
	if !b.idReplies.allow(chatID, now) {
		return nil, false
	}
	select {
	case b.strangerSlots <- struct{}{}:
		return func() { <-b.strangerSlots }, true
	default:
		return nil, false
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

// modeCommands are the commands of the modes, the buttons of the app (agent/turn_mode.go).
var modeCommands = map[string]string{
	"books":  agent.ModeBooks,
	"web":    agent.ModeWeb,
	"add":    agent.ModeLibrary,
	"notes":  agent.ModeFiles,
	"memory": agent.ModeMemory,
	"style":  agent.ModeStyle,
	"remind": agent.ModeReminder,
}

// modeCommandExample is what the reply to a command with nothing after it shows.
var modeCommandExample = map[string]string{
	"books":  "какво казва Сенека за гнева?",
	"web":    "цената на златото днес",
	"add":    "https://…",
	"notes":  "запиши в бележки.md: …",
	"memory": "запомни, че …",
	"style":  "отсега нататък отговаряй по-кратко",
	"remind": "утре в 9 да платя тока",
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

// handleMessageIn serves one message in the place t that reserveTurn gave it
// (nil for a message that needs no place: /id and chats that are not allowed).
func (b *Bot) handleMessageIn(ctx context.Context, msg *tgbotapi.Message, t *turn) {
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

	// t is never nil here: reserveTurn gives a place to every allowed chat's
	// message that is not /id, and those are the only ones that get this far.
	if !t.wait(ctx) {
		return
	}
	defer t.finish()

	if !msg.Chat.IsPrivate() {
		b.sendReply(chatID, "Мога да комуникирам само в лични чатове.")
		return
	}
	// A bill: a PDF or a picture, when the bills are on (bill.go).
	bill := b.bills != nil && (msg.Document != nil || len(msg.Photo) > 0)
	if !bill && (msg.Voice != nil || msg.Audio != nil || len(msg.Photo) > 0 || msg.Document != nil) {
		b.sendReply(chatID, "Засега разбирам само текст. Моля, качвай файлове през уеб приложението.")
		return
	}
	if !bill && (msg.Sticker != nil || msg.Text == "") {
		return
	}

	if command == "link" {
		b.handleLink(ctx, chatID, args)
		return
	}

	if err := agent.ValidateUserMessage(msg.Text); err != nil && !bill {
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

	if bill {
		b.handleBill(ctx, chatID, userID, msg)
		return
	}

	switch command {
	case "pending", "approve", "reject":
		b.handleProposalCommand(ctx, chatID, userID, command, args)
		return
	}

	// A command of a mode ("/web цената на златото"): the text after it goes down that road
	// (agent/turn_mode.go), as the buttons of the app send it.
	content, mode := msg.Text, ""
	if command == "reminders" {
		// The list of the reminders: the code answers it (agent/reminder_answer.go).
		content = "какви напомняния имам?"
	}
	if m, ok := modeCommands[command]; ok {
		if args == "" {
			b.sendReply(chatID, "Напиши след командата какво искаш, например „/"+command+" "+modeCommandExample[command]+"“.")
			return
		}
		content, mode = args, m
	}

	if !b.acquireSlot(ctx) {
		return
	}
	defer b.releaseSlot()

	turnCtx, cancel := context.WithTimeout(ctx, chatTurnTimeout)
	defer cancel()
	stopTyping := b.keepTyping(turnCtx, chatID)
	defer stopTyping()
	turn, err := b.processor.ProcessChatTurn(turnCtx, userID, agent.ChatRequest{
		SessionID: api.TelegramSessionID(chatID),
		Content:   content,
		UsePaid:   b.usePaid,
		Mode:      mode,
	})
	// Before anything is sent: a "typing" action that arrives after the reply
	// would show the indicator for another five seconds with nothing coming.
	stopTyping()
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}
		if errors.Is(err, agent.ErrConversationBusy) {
			// The same conversation is open in the app and a long answer there is not
			// finished; nothing of this message was stored.
			slog.Warn("Telegram message dropped: the conversation stayed busy", slog.Int64("chat_id", chatID))
			b.sendReply(chatID, "Разговорът още пише отговор другаде (например във Flet). Опитай пак след малко.")
			return
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
		b.sendAnswer(chatID, withRemembered(answer, turn.Remembered))
	}
	if len(turn.Proposals) > 0 {
		b.sendReply(chatID, proposalNotice(turn.Proposals))
	}
}

// keepTyping shows "typing..." in the chat at once and repeats it every
// typingEvery until the returned stop function is called or ctx ends. stop may
// be called more than once. It waits (for at most typingStopWait) for a request
// that is already on its way, so nothing is sent after stop returns.
func (b *Bot) keepTyping(ctx context.Context, chatID int64) (stop func()) {
	send := func() {
		_, _ = b.sender.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))
	}
	send()

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(b.typingEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				send()
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		select {
		case <-finished:
		case <-time.After(typingStopWait):
		}
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
	var applyErr *proposals.ApplyError
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
	case errors.Is(err, proposals.ErrTooManyStyleNotes):
		b.sendReply(chatID, fmt.Sprintf("Вече има %d бележки „как говорим“ — първо изтрий някоя от приложението.", proposals.MaxStyleNotes))
	case errors.As(err, &applyErr):
		b.sendReply(chatID, applyErr.Reason)
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

// turn is a place in the queue of one chat. The turns of a chat run one at a
// time, in the order their places were taken.
type turn struct {
	prev <-chan struct{} // closed when the turn before this one has finished
	done chan struct{}   // closed when this turn has finished
	once sync.Once
}

// closedChan is the "turn before" of the first turn of a chat.
var closedChan = func() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// reserveTurn takes the next place in the queue of msg's chat. It must be called
// in the order the messages arrived, and the place must be used: every turn after
// it waits for it. It returns nil for a message that needs no place - /id, which
// is answered at once, and a chat that is not allowed, which is ignored (and
// gets no entry, so strangers cannot grow the map).
func (b *Bot) reserveTurn(msg *tgbotapi.Message) *turn {
	chatID := msg.Chat.ID
	if !b.isAllowed(chatID) {
		return nil
	}
	if command, _ := parseCommand(msg.Text); command == "id" {
		return nil
	}

	t := &turn{done: make(chan struct{})}
	b.tailsMu.Lock()
	t.prev = b.chatTails[chatID]
	if t.prev == nil {
		t.prev = closedChan
	}
	b.chatTails[chatID] = t.done
	b.tailsMu.Unlock()
	return t
}

// wait blocks until the turn before this one has finished. It gives up when ctx
// is cancelled and then returns false; the place is still handed on, but only
// after the turn before it has finished, so the order of the others holds.
func (t *turn) wait(ctx context.Context) bool {
	select {
	case <-t.prev:
		return true
	case <-ctx.Done():
		go func() {
			<-t.prev
			t.finish()
		}()
		return false
	}
}

// finish hands the place on to the next turn. It may be called more than once.
func (t *turn) finish() { t.once.Do(func() { close(t.done) }) }

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

// maxMirroredQuestionRunes cuts the question that is repeated in the chat: it is there to say what
// the answer is about, and a pasted page of text would fill the screen.
const maxMirroredQuestionRunes = 500

// Mirror repeats in the Telegram chat an answer that was given in the app to the conversation
// shared with that chat ("tg-<chat id>"): what the person wrote there, then the answer as any
// other answer of the bot. Without it the Telegram side of the conversation would have gaps -
// only what was written in Telegram - while the app shows everything. agent.ReplyMirror.
//
// Nothing is sent to a chat the bot may not talk to, or that is not linked to this user: the
// session id comes from a request, and "tg-<any number>" must not be a way to write to a
// stranger's chat.
func (b *Bot) Mirror(ctx context.Context, userID uuid.UUID, sessionID, question, answer string, regenerated bool) {
	chatID, ok := api.ParseTelegramSession(sessionID)
	if !ok || !b.isAllowed(chatID) {
		metrics.TelegramMirror.WithLabelValues("not_allowed").Inc()
		return
	}
	linked, isLinked, err := b.store.LinkedUser(ctx, chatID)
	if err != nil {
		metrics.TelegramMirror.WithLabelValues("failed").Inc()
		slog.Error("Failed to look up the telegram link for a mirrored answer", slog.Any("error", err), slog.Int64("chat_id", chatID))
		return
	}
	if !isLinked || linked != userID {
		metrics.TelegramMirror.WithLabelValues("not_linked").Inc()
		return
	}

	if regenerated {
		b.sendReply(chatID, "🔄 Нов отговор на последния въпрос (от приложението):")
	} else {
		b.sendReply(chatID, "📱 Ти (в приложението):\n"+cutRunes(question, maxMirroredQuestionRunes))
	}
	b.sendAnswer(chatID, answer)
	metrics.TelegramMirror.WithLabelValues("sent").Inc()
}

// Notify sends to the chat of a shared conversation a text the server wrote there on its own (the
// report of a task it walked), with the same checks as Mirror: the chat is allowed and linked to
// the user.
func (b *Bot) Notify(ctx context.Context, userID uuid.UUID, sessionID, text string) {
	chatID, ok := api.ParseTelegramSession(sessionID)
	if !ok || !b.isAllowed(chatID) {
		metrics.TelegramMirror.WithLabelValues("not_allowed").Inc()
		return
	}
	linked, isLinked, err := b.store.LinkedUser(ctx, chatID)
	if err != nil {
		metrics.TelegramMirror.WithLabelValues("failed").Inc()
		slog.Error("Failed to look up the telegram link for a report of a task", slog.Any("error", err), slog.Int64("chat_id", chatID))
		return
	}
	if !isLinked || linked != userID {
		metrics.TelegramMirror.WithLabelValues("not_linked").Inc()
		return
	}
	b.sendAnswer(chatID, text)
	metrics.TelegramMirror.WithLabelValues("sent").Inc()
}

// cutRunes cuts s to at most n characters, with an ellipsis when it cut.
func cutRunes(s string, n int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n]) + "…"
}

// sendAnswer sends an answer of the model: its Markdown as the HTML Telegram shows (bold, italics,
// code, links), in pieces that each convert on their own. When Telegram refuses a piece it
// cannot parse, the piece is sent again as the plain text it was; any other failure is logged
// and the piece is not repeated (it may have arrived).
func (b *Bot) sendAnswer(chatID int64, text string) {
	for _, piece := range splitReply(text, maxReplyRunes) {
		if strings.TrimSpace(piece) == "" {
			continue
		}
		formatted := markdownToHTML(piece)
		if strings.TrimSpace(formatted) == "" {
			b.send(chatID, piece)
			continue
		}
		msg := tgbotapi.NewMessage(chatID, formatted)
		msg.ParseMode = tgbotapi.ModeHTML
		_, err := b.sender.Send(msg)
		switch {
		case err == nil:
		case refusedMarkup(err):
			slog.Warn("Telegram refused the formatted reply; sending it as plain text", slog.Int64("chat_id", chatID))
			b.send(chatID, piece)
		default:
			slog.Error("Failed to send telegram reply", slog.Any("error", err), slog.Int64("chat_id", chatID))
		}
	}
}

// refusedMarkup reports whether Telegram answered "can't parse entities": the HTML of a message
// was not accepted, and the same text without a parse mode would be.
func refusedMarkup(err error) bool {
	var apiErr *tgbotapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Message), "parse entities")
}

func (b *Bot) send(chatID int64, text string) {
	if utf8.RuneCountInString(text) == 0 {
		return
	}
	if _, err := b.sender.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		slog.Error("Failed to send telegram reply", slog.Any("error", err), slog.Int64("chat_id", chatID))
	}
}

// withRemembered adds under a planned answer the line "Лили си спомни: …" in italics (П55, step
// 2.5) with the memories the answer used. The memories are text of the model: their Markdown
// marks are made literal, so that a star in a memory cannot open a tag.
func withRemembered(answer string, remembered []string) string {
	if len(remembered) == 0 {
		return answer
	}
	escaped := make([]string, len(remembered))
	for i, m := range remembered {
		var sb strings.Builder
		for _, r := range m {
			if strings.ContainsRune(escapable, r) {
				sb.WriteRune('\\')
			}
			sb.WriteRune(r)
		}
		escaped[i] = sb.String()
	}
	return strings.TrimRight(answer, " \n\t") + "\n\n_Лили си спомни: " + strings.Join(escaped, "; ") + "_"
}
