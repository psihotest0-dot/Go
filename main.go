package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
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
	if r.Level >= slog.LevelError || strings.HasPrefix(r.Message, "server listening on") {
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

func run() error {
	// One signal-aware context for the whole process: Ctrl+C / SIGTERM cancels
	// it, which stops River and the Telegram bot and starts the HTTP shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	jobs.EnqueueIncompleteDistillations(ctx, riverClient, database)

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

	apiService := api.New(database, embedder).WithUploadLimit(cfg.MaxUploadBytes)

	retrievalService := retrieval.NewRetrievalService(database, embedder, catMgr)
	toolRegistry := agent.NewDefaultToolRegistry(retrievalService, database)
	orchestrator := agent.NewOrchestrator(database, retrievalService, llmClient, toolRegistry)

	if cfg.TelegramBotToken != "" {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("telegram bot panicked", slog.Any("error", r))
				}
			}()
			telegram.Run(ctx, cfg.TelegramBotToken, cfg.TelegramAllowedChatIDs, orchestrator, database.Pool)
		}()
	}


	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", apiService.HealthzHandler)
	mux.HandleFunc("/metrics", apiService.MetricsHandler)

	protectedMux := http.NewServeMux()
	protectedMux.HandleFunc("/chat/stream", orchestrator.HandleStreamChat)
	protectedMux.HandleFunc("/chat/regenerate", orchestrator.HandleRegenerate)

	protectedMux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			apiService.CreateMessageHandler(w, r)
		case http.MethodGet:
			apiService.GetMessagesHandler(w, r)
		default:
			slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	protectedMux.HandleFunc("/memories", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			apiService.CreateMemoryHandler(w, r)
		case http.MethodGet:
			apiService.GetMemoriesHandler(w, r)
		default:
			slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	protectedMux.HandleFunc("/memories/", apiService.MemoryByIDHandler)

	// File endpoints are served under both /v1/files and /files; see
	// api.RegisterFileRoutes for why both prefixes exist.
	apiService.RegisterFileRoutes(protectedMux, riverClient)

	userMiddleware := api.UserMiddleware(database.Pool)

	mux.Handle("/", userMiddleware(api.RecordRoute(protectedMux)))

	c := cors.New(cors.Options{
		AllowedOrigins: []string{"http://localhost:*", "http://127.0.0.1:*"},
		// PATCH is required by the file category endpoint; without it the
		// preflight for that call fails and the UI never sends the request.
		AllowedMethods: []string{
			http.MethodGet,
			http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
			http.MethodOptions,
		},
		// X-User-ID is not a CORS-safelisted header, so the browser preflights
		// every file/memory call. Omitting it here makes the preflight fail and
		// the client reports the endpoint as unreachable even though the handler
		// answers correctly.
		AllowedHeaders: []string{
			"Accept",
			"Content-Type",
			"Authorization",
			"X-Request-ID",
			"X-User-ID",
		},
		ExposedHeaders:   []string{"X-Request-ID"},
		AllowCredentials: true,
	})

	handler := api.Middleware(logger)(api.RecoveryMiddleware(logger)(c.Handler(api.RecordRoute(mux))))

	// LISTEN_ADDR defaults to loopback only (config.DefaultListenAddr). Bind
	// before announcing the address so that a port already taken by a stale
	// instance is reported as a startup error, not as a fake "listening" line.
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s (is another instance of the server already running, or is LISTEN_ADDR invalid? check the port with: netstat -ano | findstr :8080): %w", cfg.ListenAddr, err)
	}
	addr := listener.Addr().String()

	// No ReadTimeout: it would cut off slow large uploads. Body deadlines are set
	// per request where the body is read (see api.NewHTTPServer).
	srv := api.NewHTTPServer(handler)
	srv.Addr = addr

	srvErr := make(chan error, 1)

	go func() {
		logger.Info("server listening on " + addr)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	select {
	case err := <-srvErr:
		return fmt.Errorf("server failed: %w", err)
	case <-ctx.Done():
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
