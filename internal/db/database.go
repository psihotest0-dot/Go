package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	goose "github.com/pressly/goose/v3"
)

type Database struct {
	Pool    *pgxpool.Pool
	SqlDB   *sql.DB
	Queries *Queries
}

func Connect(ctx context.Context, connString string) (*Database, error) {
	config, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("unable to parse database config: %w", err)
	}

	config.MaxConns = 50
	config.MinConns = 10
	config.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		// The pool already holds background goroutines and connections; without
		// closing it here they outlive this function and leak on every failed
		// startup attempt.
		pool.Close()
		return nil, fmt.Errorf("unable to ping database: %w", err)
	}

	sqlDb := stdlib.OpenDBFromPool(pool)
	sqlDb.SetMaxOpenConns(50)
	sqlDb.SetMaxIdleConns(10)
	sqlDb.SetConnMaxLifetime(time.Hour)

	return &Database{
		Pool:    pool,
		SqlDB:   sqlDb,
		Queries: New(sqlDb),
	}, nil
}

// Close releases both connection handles.
//
// The database/sql wrapper is closed before the pgx pool on purpose: closing
// the pool first blocks until every pooled connection has been returned, and
// the wrapper only returns them when it is closed itself. Reversing the order
// hangs the shutdown. Close is safe to call on a nil or partially initialised
// Database and tolerates being called twice.
func (d *Database) Close() {
	if d == nil {
		return
	}
	if d.SqlDB != nil {
		_ = d.SqlDB.Close()
	}
	if d.Pool != nil {
		d.Pool.Close()
	}
}

// MigrationsDir is where the migration files are read from. It is relative to the
// working directory, so the backend (and cmd/migrate) must be started from the
// repository root.
const MigrationsDir = "migrations"

// MigrationStatus is how far the database is from the migration files.
type MigrationStatus struct {
	// Current is the version the database is at (0 for a database never migrated).
	Current int64
	// Pending are the files of the migrations that have not been applied yet.
	Pending []string
}

// PendingMigrations compares the database with the migration files without
// changing the schema.
func PendingMigrations(connString string) (MigrationStatus, error) {
	conn, err := sql.Open("pgx", connString)
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("failed to open database for migrations: %w", err)
	}
	defer conn.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return MigrationStatus{}, fmt.Errorf("failed to set goose dialect: %w", err)
	}

	current, err := goose.GetDBVersion(conn)
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("failed to read the database version: %w", err)
	}
	files, err := goose.CollectMigrations(MigrationsDir, 0, goose.MaxVersion)
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("failed to read the migration files: %w", err)
	}

	status := MigrationStatus{Current: current}
	for _, m := range files {
		if m.Version > current {
			status.Pending = append(status.Pending, filepath.Base(m.Source))
		}
	}
	return status, nil
}

func RunMigrations(connString string) error {
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return fmt.Errorf("failed to open database for migrations: %w", err)
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("failed to set goose dialect: %w", err)
	}

	if err := goose.Up(db, MigrationsDir); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	return nil
}

// GenerateHash generates a SHA256 hash for the given content.
func GenerateHash(content string) string {
	normalized := strings.ToLower(strings.TrimSpace(content))
	h := sha256.New()
	h.Write([]byte(normalized))
	return hex.EncodeToString(h.Sum(nil))
}

var ErrMemoryExists = errors.New("memory already exists")

// CreateMemoryWithCheck creates a memory after checking if it already exists by content hash.
func (db *Database) CreateMemoryWithCheck(ctx context.Context, params CreateMemoryParams) (Memory, error) {
	params.ContentHash = GenerateHash(params.Content)

	// Create new memory
	mem, err := db.Queries.CreateMemory(ctx, params)
	if err != nil {
		var pgErr *pgconn.PgError
		var pqErr *pq.Error
		if (errors.As(err, &pgErr) && pgErr.Code == "23505") || (errors.As(err, &pqErr) && pqErr.Code == "23505") {
			// Memory exists, unique constraint violated
			existing, getErr := db.Queries.GetMemoryByHash(ctx, GetMemoryByHashParams{
				UserID:      params.UserID,
				ContentHash: params.ContentHash,
			})
			if getErr == nil {
				return existing, ErrMemoryExists
			}
			return Memory{}, fmt.Errorf("failed to get existing memory after unique constraint error: %w", getErr)
		}
		return Memory{}, fmt.Errorf("failed to create memory: %w", err)
	}

	return mem, nil
}

// UpdateMemoryWithCheck updates a memory after regenerating the content hash if content is provided.
func (db *Database) UpdateMemoryWithCheck(ctx context.Context, params UpdateMemoryParams) (Memory, error) {
	if params.Content.Valid && params.Content.String != "" {
		params.ContentHash = sql.NullString{String: GenerateHash(params.Content.String), Valid: true}
	}

	if params.Embedding != nil {
		if params.Embedding.Slice() == nil || len(params.Embedding.Slice()) == 0 {
			return Memory{}, fmt.Errorf("vector must have at least 1 dimension")
		}
	}

	mem, err := db.Queries.UpdateMemory(ctx, params)
	if err != nil {
		var pgErr *pgconn.PgError
		var pqErr *pq.Error
		if (errors.As(err, &pgErr) && pgErr.Code == "23505") || (errors.As(err, &pqErr) && pqErr.Code == "23505") {
			return Memory{}, ErrMemoryExists
		}
		return Memory{}, err
	}
	return mem, nil
}
