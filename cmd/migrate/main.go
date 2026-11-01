// Command migrate applies the database migrations by hand, for the times the
// backend runs with AUTO_MIGRATE=false so a schema change can be preceded by a
// backup:
//
//	scripts\backup.ps1
//	go run ./cmd/migrate            # apply what is pending
//	go run ./cmd/migrate -status    # only show what is pending
//
// Run it from the repository root (the migrations folder is relative to it) with
// the same DATABASE_URL as the backend.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
)

func run() error {
	statusOnly := flag.Bool("status", false, "only show the database version and the pending migrations")
	flag.Parse()

	// The backend reads the same file; a missing one is fine.
	_ = godotenv.Load()
	cfg := config.Load()

	if _, err := os.Stat(db.MigrationsDir); err != nil {
		abs, _ := filepath.Abs(db.MigrationsDir)
		return fmt.Errorf("the migrations folder %s does not exist: run this from the repository root (%w)", abs, err)
	}

	status, err := db.PendingMigrations(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	fmt.Printf("database version: %d\n", status.Current)
	if len(status.Pending) == 0 {
		fmt.Println("nothing to apply: the database is up to date")
		return nil
	}
	fmt.Printf("pending (%d): %s\n", len(status.Pending), strings.Join(status.Pending, ", "))
	if *statusOnly {
		return nil
	}

	if err := db.RunMigrations(cfg.DatabaseURL); err != nil {
		return err
	}
	fmt.Println("applied")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
