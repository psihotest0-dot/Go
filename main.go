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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/rs/cors"
	"gopkg.in/natefinch/lumberjack.v2"

	"go-monolith/internal/agent"
	"go-monolith/internal/api"
	"go-monolith/internal/attachments"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/jobs"
	"go-monolith/internal/llm"
	"go-monolith/internal/logfile"
	"go-monolith/internal/oracle"
	"go-monolith/internal/prompts"
	"go-monolith/internal/proposals"
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

// newLogFile is the rotating backend.log. When it reaches maxMB megabytes it is
// compressed and kept as the one backup (backend-<time>.log.gz), so the logs
// never take much more than maxMB. A file that is already larger than the limit
// is rotated on the first write.
//
// The name is made absolute here: lumberjack archives and prunes the old files
// in a goroutine of its own and looks the name up against the current directory
// each time, so a relative name followed a later change of directory and could
// write or delete a backup in the wrong folder.
func newLogFile(name string, maxMB int) *lumberjack.Logger {
	if abs, err := filepath.Abs(name); err == nil {
		name = abs
	}
	return &lumberjack.Logger{
		Filename:   name,
		MaxSize:    maxMB, // megabytes
		MaxBackups: 1,
		Compress:   true,
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

	// Read once, right after .env: the size limit of the log file is part of it.
	cfg := config.Load()

	logFile := newLogFile("backend.log", cfg.LogMaxMB)
	defer logFile.Close()

	// The Prometheus scrapes ask /metrics every 15 s, and the "Бот" tab of the client
	// reads /oracle/decisions every 10 s (api.polledPaths); their "incoming request"
	// lines go to their own file, which is emptied at METRICS_LOG_MAX_MB, instead of
	// burying backend.log.
	metricsLog := logfile.NewTruncating("metrics.log", int64(cfg.MetricsLogMaxMB)<<20)
	defer metricsLog.Close()
	metricsLogger := slog.New(slog.NewJSONHandler(metricsLog, nil))

	fileHandler := slog.NewJSONHandler(logFile, nil)
	stdoutHandler := slog.NewJSONHandler(os.Stdout, nil)

	// The context handler adds request_id and user_id to every line logged with a
	// request context, so a failure can be found by the X-Request-ID the client saw.
	logger := slog.New(api.NewContextHandler(&LevelFilterHandler{
		fileHandler:   fileHandler,
		stdoutHandler: stdoutHandler,
	}))
	slog.SetDefault(logger)

	fmt.Println("Starting server...")

	dbURLForLog := cfg.DatabaseURL
	if u, err := url.Parse(cfg.DatabaseURL); err == nil {
		dbURLForLog = u.Redacted()
	}
	logger.Info("loaded config", slog.String("db_url", dbURLForLog))

	prompts.Init(cfg.PromptsDir)
	if err := prompts.GlobalManager.Preload(prompts.TemplateNames()...); err != nil {
		return fmt.Errorf("failed to preload prompt templates: %w", err)
	}
	if cfg.PromptsDir != "" {
		promptsDir, absErr := filepath.Abs(cfg.PromptsDir)
		if absErr != nil {
			promptsDir = cfg.PromptsDir
		}
		logger.Info("prompt templates: files in PROMPTS_DIR override the built-in ones", "dir", promptsDir)
	}

	logWorkingPaths(logger)
	if err := prepareSchema(logger, cfg); err != nil {
		return err
	}

	startupCtx, startupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startupCancel()

	database, err := db.Connect(startupCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer database.Close()

	llmClient := llm.NewLemonadeClient()
	embedder := llm.NewHTTPEmbedder(database).WithTimeout(cfg.EmbedderTimeout)
	// A wrong model is loud here and refused in EmbedTexts; an embedder that is
	// merely not up yet is not fatal, the backend retries on every request.
	if err := embedder.CheckModel(ctx); err != nil {
		logger.Error("embedding service check failed", slog.Any("error", err))
	} else {
		logger.Info("embedding service ready", slog.String("model", config.DefaultEmbedModel))
	}

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

	// The job queue and the files by ingest status, for the Grafana dashboard.
	prometheus.MustRegister(jobs.NewStateCollector(database.SqlDB))

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

	retrievalService := retrieval.NewRetrievalService(database, embedder)
	proposalService := proposals.NewService(database, retrievalService)
	// Files the user attaches to a conversation (code, a long paste): kept per session,
	// shown in the prompt within a small budget and read in parts by the model.
	attachmentService := attachments.NewService(database)
	toolRegistry := agent.NewDefaultToolRegistry(retrievalService, database, proposalService).WithAttachments(attachmentService)
	orchestrator := agent.NewOrchestrator(database, retrievalService, llmClient, toolRegistry).WithAttachments(attachmentService)

	if cfg.TelegramBotToken != "" {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("telegram bot panicked", slog.Any("error", r))
				}
			}()
			telegram.Run(ctx, cfg.TelegramBotToken, cfg.TelegramAllowedChatIDs, orchestrator, database.Pool, proposalService)
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
			slog.WarnContext(r.Context(), "method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
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
			slog.WarnContext(r.Context(), "method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	protectedMux.HandleFunc("/memories/", apiService.MemoryByIDHandler)

	// What the model proposes to remember waits here for the user, and the saved
	// global rules can be read and deleted.
	proposalService.RegisterRoutes(protectedMux)
	apiService.RegisterRuleRoutes(protectedMux)
	attachmentService.RegisterRoutes(protectedMux)

	// The Oracle: the trading bot's second opinion on a signal (APPROVE / VETO / HOLD).
	// The bot's routes carry their own token and sit outside the user check; without a
	// usable ORACLE_TOKEN they are not served at all. The journal of the answers is read
	// through the ordinary user check.
	oracleStore := oracle.NewDBStore(database.Queries)
	oracleHandler := oracle.NewHandler(oracle.NewService(llmClient, oracleStore), oracleStore, cfg.OracleToken)
	oracleHandler.RegisterReadRoutes(protectedMux)
	switch {
	case oracle.TokenUsable(cfg.OracleToken):
		oracleHandler.RegisterBotRoutes(mux)
		logger.Info("Oracle route is on", slog.String("path", "/oracle/evaluate"))
	case cfg.OracleToken != "":
		logger.Warn("ORACLE_TOKEN is too short: the Oracle route is off", slog.Int("min_length", oracle.MinTokenLength))
	default:
		logger.Info("Oracle route is off (ORACLE_TOKEN is not set)")
	}

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

	handler := api.Middleware(logger, api.WithMetricsLogger(metricsLogger))(api.RecoveryMiddleware(logger)(c.Handler(api.RecordRoute(mux))))

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

// absPath is the absolute form of a path for the log; the path itself when it
// cannot be resolved.
func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// logWorkingPaths says where the backend is looking for the folders that are
// given relative to the working directory. A backend started from another folder
// (a scheduled task, a shortcut) would otherwise read other migrations and save
// uploads somewhere else without a word.
func logWorkingPaths(logger *slog.Logger) {
	cwd, _ := os.Getwd()
	storage := absPath(api.UploadDir())
	logger.Info("working directory and the folders resolved against it",
		slog.String("cwd", cwd),
		slog.String("migrations", absPath(db.MigrationsDir)),
		slog.String("storage", storage))

	if _, err := os.Stat(api.UploadDir()); err != nil {
		logger.Warn("the storage folder does not exist yet; uploads will create it here. If files were uploaded before, the backend was started from another folder",
			slog.String("storage", storage))
	}
}

// ensureSchema brings the database to the version of the migration files, or
// stops the start when it must not do that by itself. It is separate from the
// database calls so the decision can be tested.
func ensureSchema(logger *slog.Logger, autoMigrate bool, status db.MigrationStatus, apply func() error) error {
	if len(status.Pending) == 0 {
		logger.Info("database schema is up to date", slog.Int64("version", status.Current))
		return nil
	}

	if !autoMigrate {
		return fmt.Errorf("the database is at version %d and %d migrations are pending (%s), but AUTO_MIGRATE is off: "+
			"take a backup (scripts\\backup.ps1), run `go run ./cmd/migrate` from the repository root, then start the backend again",
			status.Current, len(status.Pending), strings.Join(status.Pending, ", "))
	}

	logger.Warn("applying pending database migrations; a backup first is a good idea (AUTO_MIGRATE=false lets you apply them by hand)",
		slog.Int64("from_version", status.Current),
		slog.String("pending", strings.Join(status.Pending, ", ")))
	if err := apply(); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	logger.Info("migrations ran successfully", slog.Int("applied", len(status.Pending)))
	return nil
}

// prepareSchema applies the pending migrations when AUTO_MIGRATE allows it.
func prepareSchema(logger *slog.Logger, cfg *config.Config) error {
	if _, err := os.Stat(db.MigrationsDir); err != nil {
		return fmt.Errorf("the migrations folder %s does not exist: start the backend from the repository root (%w)",
			absPath(db.MigrationsDir), err)
	}

	status, err := db.PendingMigrations(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	return ensureSchema(logger, cfg.AutoMigrate, status, func() error {
		return db.RunMigrations(cfg.DatabaseURL)
	})
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", slog.Any("error", err))
		os.Exit(1)
	}
}
