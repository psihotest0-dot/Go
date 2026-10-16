package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"

	"go-monolith/internal/agent"
	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/jobs"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/internal/retrieval"
)

type LevelFilterHandler struct {
	fileHandler   slog.Handler
	stdoutHandler slog.Handler
}

func (h *LevelFilterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.fileHandler.Enabled(ctx, level)
}

func (h *LevelFilterHandler) Handle(ctx context.Context, r slog.Record) error {
	if err := h.fileHandler.Handle(ctx, r); err != nil {
		return err
	}
	if r.Level >= slog.LevelError || r.Message == "server listening on :8080" {
		return h.stdoutHandler.Handle(ctx, r)
	}
	return nil
}

func (h *LevelFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LevelFilterHandler{
		fileHandler:   h.fileHandler.WithAttrs(attrs),
		stdoutHandler: h.stdoutHandler.WithAttrs(attrs),
	}
}

func (h *LevelFilterHandler) WithGroup(name string) slog.Handler {
	return &LevelFilterHandler{
		fileHandler:   h.fileHandler.WithGroup(name),
		stdoutHandler: h.stdoutHandler.WithGroup(name),
	}
}

func main() {
	logFile := &lumberjack.Logger{
		Filename:   "backend.log",
		MaxSize:    5, // megabytes
		MaxBackups: 1,
		Compress:   true,
	}
	defer logFile.Close()

	fileHandler := slog.NewJSONHandler(logFile, nil)
	stdoutHandler := slog.NewJSONHandler(os.Stdout, nil)

	logger := slog.New(&LevelFilterHandler{
		fileHandler:   fileHandler,
		stdoutHandler: stdoutHandler,
	})
	slog.SetDefault(logger)

	fmt.Println("Starting server...")

	cfg := config.Load()
	logger.Info("loaded config", slog.String("db_url", cfg.DatabaseURL))

	prompts.Init("prompts")

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

	catMgr, err := retrieval.NewCategoryManager(ctx, embedder)
	if err != nil {
		logger.Error("failed to create category manager", slog.Any("error", err))
		// don't exit, just continue without it if it fails
	}

	riverClient, err := jobs.NewRiverClient(ctx, database.Pool, database, llmClient, embedder, catMgr)
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

	apiService := api.New(database, embedder)

	retrievalService := retrieval.NewRetrievalService(database, embedder, catMgr)
	toolRegistry := agent.NewDefaultToolRegistry(retrievalService, database)
	orchestrator := agent.NewOrchestrator(database, retrievalService, llmClient, toolRegistry)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", apiService.HealthzHandler)
	mux.HandleFunc("/metrics", apiService.MetricsHandler)

	protectedMux := http.NewServeMux()
	protectedMux.HandleFunc("/chat/stream", orchestrator.HandleStreamChat)

	protectedMux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			apiService.CreateMessageHandler(w, r)
		} else if r.Method == http.MethodGet {
			apiService.GetMessagesHandler(w, r)
		} else {
			slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	protectedMux.HandleFunc("/memories", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			apiService.CreateMemoryHandler(w, r)
		} else if r.Method == http.MethodGet {
			apiService.GetMemoriesHandler(w, r)
		} else {
			slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	protectedMux.HandleFunc("/memories/", apiService.MemoryByIDHandler)

	protectedMux.HandleFunc("/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			apiService.CreateFileHandler(riverClient)(w, r)
		} else if r.Method == http.MethodGet {
			apiService.GetFilesHandler(w, r)
		} else {
			slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	protectedMux.HandleFunc("/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/category") {
			apiService.FileCategoryHandler(w, r)
		} else {
			apiService.FileByIDHandler(w, r)
		}
	})

	userMiddleware := api.UserMiddleware(database.Pool)

	mux.Handle("/", userMiddleware(protectedMux))

	handler := api.Middleware(logger)(api.RecoveryMiddleware(logger)(mux))

	logger.Info("server listening on :8080")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		logger.Error("server failed", slog.Any("error", err))
		os.Exit(1)
	}
}
