package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/jobs"
	"go-monolith/internal/llm"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg := config.Load()
	logger.Info("loaded config", slog.String("db_url", cfg.DatabaseURL))

	ctx := context.Background()

	if err := db.RunMigrations(cfg.DatabaseURL); err != nil {
		logger.Error("failed to run migrations", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("migrations ran successfully")

	database, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to connect to database", slog.Any("error", err))
		os.Exit(1)
	}
	defer database.Pool.Close()

	llmClient := llm.NewLemonadeClient()
	embedder := llm.NewHTTPEmbedder(database)

	riverClient, err := jobs.NewRiverClient(ctx, database.Pool, database, llmClient, embedder)
	if err != nil {
		logger.Error("failed to create river client", slog.Any("error", err))
		os.Exit(1)
	}

	if err := riverClient.Start(ctx); err != nil {
		logger.Error("failed to start river client", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		if err := riverClient.Stop(ctx); err != nil {
			logger.Error("failed to stop river client safely", slog.Any("error", err))
		}
	}()

	apiService := api.New(database)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", apiService.HealthzHandler)
	mux.HandleFunc("/metrics", apiService.MetricsHandler)

	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			apiService.CreateMessageHandler(w, r)
		} else if r.Method == http.MethodGet {
			apiService.GetMessagesHandler(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/memories", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			apiService.CreateMemoryHandler(w, r)
		} else if r.Method == http.MethodGet {
			apiService.GetMemoriesHandler(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/memories/", apiService.MemoryByIDHandler)

	mux.HandleFunc("/v1/files", apiService.CreateFileHandler(riverClient))

	handler := api.Middleware(logger)(mux)

	logger.Info("server listening on :8080")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		logger.Error("server failed", slog.Any("error", err))
		os.Exit(1)
	}
}
