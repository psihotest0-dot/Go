package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

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
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("unable to ping database: %w", err)
	}

	sqlDb := stdlib.OpenDBFromPool(pool)
	return &Database{
		Pool:    pool,
		SqlDB:   sqlDb,
		Queries: New(sqlDb),
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

	// Create new memory
	mem, err := db.Queries.CreateMemory(ctx, params)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
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

// UpdateMemoryWithCheck updates a memory after regenerating the content hash.
func (db *Database) UpdateMemoryWithCheck(ctx context.Context, params UpdateMemoryParams) (Memory, error) {
	params.ContentHash = GenerateHash(params.Content)
	return db.Queries.UpdateMemory(ctx, params)
}
