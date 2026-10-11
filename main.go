package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
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

	mux.HandleFunc("/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			apiService.CreateFileHandler(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/v1/files/", apiService.DeleteFileHandler)

	handler := api.Middleware(logger)(mux)

	logger.Info("server listening on :8080")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		logger.Error("server failed", slog.Any("error", err))
		os.Exit(1)
	}
}
