package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const VectorDimension = 384

// DefaultEmbedModel is the embedding model of the whole system: every vector in
// the database (chunks, memories, the embedding cache) comes from it, and the
// embedding service must run exactly this one - llm.HTTPEmbedder refuses
// answers from any other. It is also the name services/embedder loads. Changing
// it means changing the service, re-embedding everything (cmd/reembed) and, if
// the dimension differs, VectorDimension and the vector(...) columns.
const DefaultEmbedModel = "intfloat/multilingual-e5-small"

// DefaultListenAddr keeps the API on the loopback interface: identity is a bare
// X-User-ID header and /metrics is unauthenticated, so the server must not be
// reachable from other machines unless LISTEN_ADDR says so explicitly.
const DefaultListenAddr = "127.0.0.1:8080"

// DefaultMaxUploadMB is the upload size limit when MAX_UPLOAD_MB is not set.
const DefaultMaxUploadMB = 100

// DefaultLLMSlots is how many completions the model server runs in parallel
// (Lemonade is started with two slots). DefaultLLMBackgroundSlots is the share
// of them background work (summaries, consolidation, distillation) may use.
const (
	DefaultLLMSlots           = 2
	DefaultLLMBackgroundSlots = 1
)

// DefaultEmbedderTimeout is the limit of one call to the embedding service. A
// batch of 64 texts takes 1-3 s on an idle CPU, so this is a wide margin.
const DefaultEmbedderTimeout = 30 * time.Second

type Config struct {
	ListenAddr     string
	MaxUploadBytes int64
	// PromptsDir is an optional directory of prompt templates that override the
	// ones compiled into the binary (internal/prompts). Empty means no override.
	PromptsDir string
	LLMBaseURL string
	// LLMModel pins the model name sent to the LLM server. Empty means the
	// client asks the server which model it serves.
	LLMModel string
	// LLMSlots is the number of parallel completions the LLM server handles;
	// LLMBackgroundSlots is how many of them background work may occupy.
	LLMSlots           int
	LLMBackgroundSlots int
	// LLMContextTokens is the context window of ONE request, i.e. the server's
	// context divided by its slots (32768 / 2 = 16384). Zero means unknown: the
	// agent then keeps its conservative built-in budgets. The remaining LLM*
	// token fields override a single budget each; zero means "derive it from
	// LLMContextTokens" (see internal/agent/budgets.go).
	LLMContextTokens       int
	LLMAnswerTokens        int
	LLMPromptContextTokens int
	LLMHistoryTokens       int
	LLMHistoryMessages     int
	LLMToolResultTokens    int
	// LLMEnableThinking lets the model reason before it answers in the chat. It
	// is on by default because that is what the model does when asked nothing.
	LLMEnableThinking bool
	EmbedderURL       string
	// EmbedderTimeout bounds one call to the embedding service, with its cache reads
	// and writes (EMBEDDER_TIMEOUT_SECONDS).
	EmbedderTimeout time.Duration
	EmbedModel        string
	DatabaseURL       string
	TelegramBotToken  string
	// TelegramAllowedChatIDs lists the chats the bot may talk to. Empty means
	// nobody: the bot token only controls the bot, not who can write to it.
	TelegramAllowedChatIDs []int64
	EnableAdaptiveSkip     bool
	EnableDistillation     bool
}

// parseChatIDs reads a comma, semicolon or space separated list of Telegram chat
// ids. Entries that are not integers are logged and skipped.
func parseChatIDs(raw string) []int64 {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})

	ids := make([]int64, 0, len(fields))
	for _, field := range fields {
		id, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			slog.Warn("ignoring invalid entry in TELEGRAM_ALLOWED_CHAT_IDS", slog.String("entry", field))
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// positiveIntEnv reads a positive integer from the environment. An unset
// variable gives def silently; an invalid or non-positive one gives def with a
// warning.
func positiveIntEnv(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		slog.Warn("ignoring invalid "+name, slog.String("value", raw), slog.Int("using", def))
		return def
	}
	return n
}

// boolEnv reads a boolean from the environment (true/false/1/0, any case). An
// unset variable gives def silently; an unparsable one gives def with a warning.
func boolEnv(name string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Warn("ignoring invalid "+name, slog.String("value", raw), slog.Bool("using", def))
		return def
	}
	return value
}

func Load() *Config {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	}

	adaptiveSkipStr := os.Getenv("ENABLE_ADAPTIVE_SKIP")
	adaptiveSkip := true
	if adaptiveSkipStr == "false" || adaptiveSkipStr == "0" {
		adaptiveSkip = false
	}

	distillationStr := os.Getenv("ENABLE_DISTILLATION")
	distillation := true
	if distillationStr == "false" || distillationStr == "0" {
		distillation = false
	}

	listenAddr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = DefaultListenAddr
	}

	maxUploadMB := int64(DefaultMaxUploadMB)
	if raw := strings.TrimSpace(os.Getenv("MAX_UPLOAD_MB")); raw != "" {
		if mb, err := strconv.ParseInt(raw, 10, 64); err == nil && mb > 0 {
			maxUploadMB = mb
		} else {
			slog.Warn("ignoring invalid MAX_UPLOAD_MB", slog.String("value", raw), slog.Int64("using_mb", maxUploadMB))
		}
	}

	return &Config{
		ListenAddr:         listenAddr,
		MaxUploadBytes:     maxUploadMB << 20,
		PromptsDir:         strings.TrimSpace(os.Getenv("PROMPTS_DIR")),
		LLMBaseURL:         os.Getenv("LLM_BASE_URL"),
		LLMModel:           strings.TrimSpace(os.Getenv("LLM_MODEL")),
		LLMSlots:           positiveIntEnv("LLM_SLOTS", DefaultLLMSlots),
		LLMBackgroundSlots: positiveIntEnv("LLM_BACKGROUND_SLOTS", DefaultLLMBackgroundSlots),

		LLMContextTokens:       positiveIntEnv("LLM_CONTEXT_TOKENS", 0),
		LLMAnswerTokens:        positiveIntEnv("LLM_ANSWER_TOKENS", 0),
		LLMPromptContextTokens: positiveIntEnv("LLM_PROMPT_CONTEXT_TOKENS", 0),
		LLMHistoryTokens:       positiveIntEnv("LLM_HISTORY_TOKENS", 0),
		LLMHistoryMessages:     positiveIntEnv("LLM_HISTORY_MESSAGES", 0),
		LLMToolResultTokens:    positiveIntEnv("LLM_TOOL_RESULT_TOKENS", 0),
		LLMEnableThinking:      boolEnv("LLM_ENABLE_THINKING", true),

		EmbedderURL:        os.Getenv("EMBEDDER_URL"),
		EmbedderTimeout:    time.Duration(positiveIntEnv("EMBEDDER_TIMEOUT_SECONDS", int(DefaultEmbedderTimeout/time.Second))) * time.Second,
		EmbedModel:         os.Getenv("EMBED_MODEL"),
		DatabaseURL:        dbURL,
		TelegramBotToken:   os.Getenv("TELEGRAM_BOT_TOKEN"),
		EnableAdaptiveSkip: adaptiveSkip,
		EnableDistillation: distillation,

		TelegramAllowedChatIDs: parseChatIDs(os.Getenv("TELEGRAM_ALLOWED_CHAT_IDS")),
	}
}
