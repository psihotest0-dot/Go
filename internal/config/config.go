package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"unicode"
)

const VectorDimension = 384

// DefaultEmbedModel is the embedding model recorded on rows written by the
// embedder sidecar's default configuration.
const DefaultEmbedModel = "intfloat/multilingual-e5-small"

// DefaultListenAddr keeps the API on the loopback interface: identity is a bare
// X-User-ID header and /metrics is unauthenticated, so the server must not be
// reachable from other machines unless LISTEN_ADDR says so explicitly.
const DefaultListenAddr = "127.0.0.1:8080"

// DefaultMaxUploadMB is the upload size limit when MAX_UPLOAD_MB is not set.
const DefaultMaxUploadMB = 100

type Config struct {
	ListenAddr       string
	MaxUploadBytes   int64
	LLMBaseURL       string
	EmbedderURL      string
	EmbedModel       string
	DatabaseURL      string
	TelegramBotToken string
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
		LLMBaseURL:         os.Getenv("LLM_BASE_URL"),
		EmbedderURL:        os.Getenv("EMBEDDER_URL"),
		EmbedModel:         os.Getenv("EMBED_MODEL"),
		DatabaseURL:        dbURL,
		TelegramBotToken:   os.Getenv("TELEGRAM_BOT_TOKEN"),
		EnableAdaptiveSkip: adaptiveSkip,
		EnableDistillation: distillation,

		TelegramAllowedChatIDs: parseChatIDs(os.Getenv("TELEGRAM_ALLOWED_CHAT_IDS")),
	}
}
