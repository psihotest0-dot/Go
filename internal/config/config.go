package config

import (
	"os"
)

const VectorDimension = 384

// DefaultEmbedModel is the embedding model recorded on rows written by the
// embedder sidecar's default configuration.
const DefaultEmbedModel = "intfloat/multilingual-e5-small"

type Config struct {
	LLMBaseURL         string
	EmbedderURL        string
	EmbedModel         string
	DatabaseURL        string
	TelegramBotToken   string
	EnableAdaptiveSkip bool
	EnableDistillation bool
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

	return &Config{
		LLMBaseURL:         os.Getenv("LLM_BASE_URL"),
		EmbedderURL:        os.Getenv("EMBEDDER_URL"),
		EmbedModel:         os.Getenv("EMBED_MODEL"),
		DatabaseURL:        dbURL,
		TelegramBotToken:   os.Getenv("TELEGRAM_BOT_TOKEN"),
		EnableAdaptiveSkip: adaptiveSkip,
		EnableDistillation: distillation,
	}
}
