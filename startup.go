package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
)

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
