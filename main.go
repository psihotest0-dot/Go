package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/rs/cors"
	"gopkg.in/natefinch/lumberjack.v2"

	"go-monolith/internal/agent"
	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/jobs"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/internal/retrieval"
	"go-monolith/internal/telegram"
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

func enqueueIncompleteDistillations(ctx context.Context, client *river.Client[pgx.Tx], database *db.Database) {
	rows, err := database.SqlDB.QueryContext(ctx, `
		SELECT f.id FROM ingested_files f
		WHERE f.status = 'completed'
		  AND EXISTS (SELECT 1 FROM document_chunks c1
					  WHERE c1.file_id = f.id AND c1.chunk_type = 'raw_text')
		  AND (
			  SELECT COUNT(*) FROM document_chunks c2
			  WHERE c2.file_id = f.id AND c2.chunk_type = 'concept_summary'
		  ) < (
			  SELECT COUNT(*) FROM document_chunks c3
			  WHERE c3.file_id = f.id AND c3.chunk_type = 'raw_text'
		  ) / 5 * 8 / 10
	`)
	if err != nil {
		slog.Error("startup distillation scan failed", "error", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var fileID uuid.UUID
		if err := rows.Scan(&fileID); err != nil {
			continue
		}
		_, err := client.Insert(ctx, jobs.DistillationWorkerArgs{
			FileID: fileID,
		}, &river.InsertOpts{Queue: "distillation"})
		if err != nil {
			slog.Error("failed to enqueue", "file_id", fileID, "error", err)
			continue
		}
		count++
	}
	slog.Info("Startup distillation scan complete", "enqueued", count)
}

func run() error {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) && !errors.Is(err, os.ErrNotExist) {
		fmt.Printf("warning: failed to load .env file: %v\n", err)
	}

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

	dbURLForLog := cfg.DatabaseURL
	if u, err := url.Parse(cfg.DatabaseURL); err == nil {
		dbURLForLog = u.Redacted()
	}
	logger.Info("loaded config", slog.String("db_url", dbURLForLog))

	prompts.Init("prompts")
	if err := prompts.GlobalManager.Preload("chat_system.tmpl"); err != nil {
		return fmt.Errorf("failed to preload critical templates: %w", err)
	}

	ctx := context.Background()

	if err := db.RunMigrations(cfg.DatabaseURL); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	logger.Info("migrations ran successfully")

	startupCtx, startupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startupCancel()

	database, err := db.Connect(startupCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer database.Close()

	llmClient := llm.NewLemonadeClient()
	embedder := llm.NewHTTPEmbedder(database)

	catMgr, err := retrieval.NewCategoryManager(ctx, embedder)
	if err != nil {
		logger.Error("failed to create category manager", slog.Any("error", err))
		// don't exit, just continue without it if it fails
	}

	riverClient, err := jobs.NewRiverClient(startupCtx, database.Pool, database, llmClient, embedder, catMgr)
	if err != nil {
		return fmt.Errorf("failed to create river client: %w", err)
	}

	migrator, err := rivermigrate.New(riverClient.Driver(), nil)
	if err != nil {
		return fmt.Errorf("failed to create river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("failed to migrate river: %w", err)
	}

	enqueueIncompleteDistillations(ctx, riverClient, database)

	if err := riverClient.Start(ctx); err != nil {
		return fmt.Errorf("failed to start river client: %w", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := riverClient.Stop(stopCtx); err != nil {
			logger.Error("failed to stop river client safely", slog.Any("error", err))
		}
	}()

	apiService := api.New(database, embedder)

	retrievalService := retrieval.NewRetrievalService(database, embedder, catMgr)
	toolRegistry := agent.NewDefaultToolRegistry(retrievalService, database)
	orchestrator := agent.NewOrchestrator(database, retrievalService, llmClient, toolRegistry)

	if cfg.TelegramBotToken != "" {
		tgBot, err := telegram.NewBot(cfg.TelegramBotToken, orchestrator, database.Pool)
		if err != nil {
			logger.Error("failed to initialize telegram bot", slog.Any("error", err))
		} else {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Error("telegram bot panicked", slog.Any("error", r))
					}
				}()
				tgBot.Start(ctx)
			}()
		}
	}


	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", apiService.HealthzHandler)
	mux.HandleFunc("/metrics", apiService.MetricsHandler)

	protectedMux := http.NewServeMux()
	protectedMux.HandleFunc("/chat/stream", orchestrator.HandleStreamChat)
	protectedMux.HandleFunc("/chat/regenerate", orchestrator.HandleRegenerate)

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

	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"http://localhost:*"},
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowCredentials: true,
	})

	handler := api.Middleware(logger)(api.RecoveryMiddleware(logger)(c.Handler(mux)))

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srvErr := make(chan error, 1)

	go func() {
		logger.Info("server listening on :8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	select {
	case err := <-srvErr:
		return fmt.Errorf("server failed: %w", err)
	case <-shutdownCtx.Done():
		logger.Info("shutting down server gracefully...")
	}

	shutdownTimeoutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownTimeoutCtx); err != nil {
		logger.Error("server shutdown failed", slog.Any("error", err))
	} else {
		logger.Info("server stopped gracefully")
	}

	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", slog.Any("error", err))
		os.Exit(1)
	}
}
