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

	handler := api.Middleware(logger)(mux)

	logger.Info("server listening on :8080")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		logger.Error("server failed", slog.Any("error", err))
		os.Exit(1)
	}
}
