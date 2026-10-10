package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

type Database struct {
	Pool    *pgxpool.Pool
	Queries *Queries
}

func Connect(ctx context.Context, connString string) (*Database, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("unable to ping database: %w", err)
	}

	return &Database{
		Pool:    pool,
		Queries: New(stdlib.OpenDBFromPool(pool)),
	}, nil
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

	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	return nil
}

// GenerateHash generates a SHA256 hash for the given content.
func GenerateHash(content string) string {
	h := sha256.New()
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

var ErrMemoryExists = errors.New("memory already exists")

// CreateMemoryWithCheck creates a memory after checking if it already exists by content hash.
func (db *Database) CreateMemoryWithCheck(ctx context.Context, params CreateMemoryParams) (Memory, error) {
	params.ContentHash = GenerateHash(params.Content)

	// Check for duplicate
	existing, err := db.Queries.GetMemoryByHash(ctx, GetMemoryByHashParams{
		UserID:      params.UserID,
		ContentHash: params.ContentHash,
	})
	if err == nil {
		// Memory exists
		return existing, ErrMemoryExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Memory{}, fmt.Errorf("failed to check existing memory: %w", err)
	}

	// Create new memory
	return db.Queries.CreateMemory(ctx, params)
}

// UpdateMemoryWithCheck updates a memory after regenerating the content hash.
func (db *Database) UpdateMemoryWithCheck(ctx context.Context, params UpdateMemoryParams) (Memory, error) {
	params.ContentHash = GenerateHash(params.Content)
	return db.Queries.UpdateMemory(ctx, params)
}
