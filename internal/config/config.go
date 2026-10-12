package config

import (
	"os"
)

type Config struct {
	LLMBaseURL  string
	EmbedderURL string
	EmbedModel  string
	DatabaseURL string
}

func Load() *Config {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	}
	return &Config{
		LLMBaseURL:  os.Getenv("LLM_BASE_URL"),
		EmbedderURL: os.Getenv("EMBEDDER_URL"),
		EmbedModel:  os.Getenv("EMBED_MODEL"),
		DatabaseURL: dbURL,
	}
}
