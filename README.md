# Agent API & Memory Backend

A high-performance Go monolith backing an AI Agent service, providing vector similarity search, long-term memory consolidation, and standard chat streaming.

## Tech Stack
* **Language**: Go 1.26.0
* **Database**: PostgreSQL (requires `pgvector` and `pg_trgm` extensions)
* **SQL Query Builder**: `sqlc` (with `pgvector-go` overriding PostgreSQL vector types)
* **Migrations**: `goose`
* **Job Queue / Background Tasks**: River (PostgreSQL-backed, replacing Redis)
* **LLM Integration**: Standard OpenAI Go SDK pointing to a custom "Lemonade" provider
* **Embedding Model**: `intfloat/multilingual-e5-small` via FastAPI Python sidecar (CPU only)
* **Observability**: Prometheus & Grafana (JSON logging via standard `slog`)

## Architecture & Features

### Orchestrator Pipeline
The main agent orchestrator (`internal/agent`) implements a standard streaming pipeline using Server-Sent Events (SSE). It handles:
1. Fetching history from PostgreSQL, applying a sliding-window character count prune to prevent token limits.
2. Generating real-time embeddings for the user's prompt using the embedder sidecar.
3. Fetching semantically similar long-term memories via Hybrid Search.
4. Calling the LLM with the context and streaming response chunks directly to the user.

### Memory Consolidation
Background memory consolidation runs asynchronously via River queues (`internal/jobs`).
The pipeline implements:
1. **Fact Extraction**: Calls the LLM specifying a JSON schema to extract structured facts (`fact_key`, `value`, `statement`, `is_explicit_change`) from unanalyzed messages.
2. **State Management**: Facts transition atomically within a single SQL transaction between statuses (`candidate`, `active`, `superseded`). If a new fact contradicts an older one explicitly, the older one is `superseded` and the new one becomes `active`.
3. **Concurrency**: Implements PostgreSQL advisory locks using explicit `conn` handles and `crc32(UUID)` keys to prevent concurrent workers processing the same user's memory tree.
   *Warning: Session-level advisory locks do not work properly when used with PgBouncer in transaction pooling mode.*

### Vector Search
Memory retrieval operates via **Hybrid Search**, combining:
* `pgvector` cosine similarity (`<=>`)
* `pg_trgm` text similarity (used to support Bulgarian language nuances)

Results are merged directly inside optimized SQL CTEs using Reciprocal Rank Fusion (RRF) with `k=60` and returned to the caller.

To avoid repeated computation of static text vectors, embeddings are cached inside an `embedding_cache` table using SHA256 hashes of the content text. Background workers routinely clear old unaccessed caches.

## Observability
- Centralized JSON logging using standard `slog` with enforced `request_id` and `user_id` context tracing via HTTP middlewares.
- Before returning any HTTP 500 Internal Server Error, explicitly log the error using `slog.Error` so it is not lost.
- Prometheus metrics are exposed at `/metrics` tracking QPS, token usage, cache hits/misses, and pipeline stage latencies.

*Security Note: The `user_id` context tracing is critical for tenant isolation and must be securely validated. Furthermore, the `/metrics` endpoint is exposed unauthenticated by default. In a production environment, ensure it is protected via internal network routing or firewall rules.*
- Visualize metrics easily using the pre-configured Grafana dashboard available via `docker-compose.yml`.

## How to Run

1. **Start the Infrastructure**
Ensure you have Docker and Docker Compose installed.
```bash
docker-compose up -d
```
This boots up the FastAPI Python embedder service, Prometheus, and Grafana. Make sure you provide your own running PostgreSQL instance with `pgvector` and `pg_trgm` enabled.

2. **Run the API**
Make sure required environment variables are set:
```bash
export DATABASE_URL="postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
export LLM_BASE_URL="http://localhost:13305/v1" # Lemonade LLM URL
export EMBEDDER_URL="http://localhost:8000"

# Run the Go API
go run main.go
```

3. **Tools & Migrations**
If you make changes to the DB schema, jobs or SQL queries, you need to run:
- **Database Migrations**: `goose -dir migrations postgres "$DATABASE_URL" up`
- **River Migrations**: `river db migrate-v5 --database-url "$DATABASE_URL"`
- **Generate SQLC**: `sqlc generate`

4. **Scripts**
A suite of backup, restore, and reindex scripts with `set -euo pipefail` are available in the `scripts/` directory.
- `./scripts/backup.sh` uses `pg_dump -Fc`; `BACKUP_DIR` and `BACKUP_KEEP` (default `backups` / `14` dumps) control where dumps go and how many are kept.
- `scripts/backup.ps1` is the Windows equivalent and falls back to `docker compose exec db pg_dump` when the host has no PostgreSQL client.
- `./scripts/restore.sh` uses `pg_restore --clean --if-exists`
- `./scripts/reindex.sh` rebuilds concurrent HNSW indices.
- `scripts/backup-task.ps1` registers a daily Windows Scheduled Task (`go-monolith-backup`) that runs `backup.ps1` and keeps the dumps outside the repository.

> **Take backups.** On 2026-10-30 a `DELETE FROM users` cascade wiped the ingested files, the chunks and their embeddings. Nothing could be restored because no dump had ever been taken — see `docs/incident-2026-10-30/REPORT.md`.

## Tests

Integration tests **never** read `DATABASE_URL`. They require `TEST_DATABASE_URL`, and they refuse to run unless the database name contains `test`:

```bash
# one-time: create the throwaway database
docker compose exec db psql -U postgres -c "CREATE DATABASE app_test"

export TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/app_test?sslmode=disable"
go test -p 1 ./...
```

`-p 1` matters: `internal/db` runs the migrations down to zero and back up, so the packages that share the test database must not run at the same time. Without `TEST_DATABASE_URL` the database-backed tests skip; the pure unit tests still run. Some of them execute `goose.DownTo(..., 0)` and `DELETE FROM users`, which is exactly why they are fenced off from the live connection string.

### File Ingestion Pipeline
The system supports uploading external knowledge documents via the `POST /v1/files` endpoint.
1. **Upload & Verification**: Enforces a 50MB limit, hashes the file for deduplication, securely saves it to a local `storage/` directory, and writes a tracking record.
2. **Extraction**: The `internal/ingest` module detects formats strictly by magic bytes via `mimetype`. It includes robust parsers for plain text formats, HTML (tag stripping), PDF (`pdfcpu`), DOCX, and XLSX (`excelize`).
3. **Background Chunking**: An async River job (`IngestFileWorker`) extracts the raw text, splits it into chunks, generates vector embeddings, and stores them in the `document_chunks` table for retrieval.
4. **Embedding Status**: `GET /v1/files` (also available as `GET /files`) reports `chunk_count`, `has_embeddings` and `embedding_status` per file. Embeddings live in PostgreSQL via `pgvector` — there is no external vector store. `embedding_status` mirrors the ingest status but maps a `completed` file with zero chunks to `empty`, so the UI can distinguish "indexed" from "ingested but nothing to search".

### Known limitations and considerations
* **Vector Dimensions Validation:** When verifying embedder outputs, avoid hardcoding the dimensions logic like `len(vectors[0]) == 384` across the codebase. Since the embedding model config could change (e.g. from MiniLM with 384 dimensions to a model with 768 dimensions), hardcoding this check will break functionality. A constant or environment variable should be used if dimension validation is needed.
* **Embedder Fallback:** When the embedder fails or returns zero-dimension vectors, the fallback logic should not pad or insert zeroed-out arrays of a fixed size, as this pollutes the database with useless embeddings and degrades hybrid search quality.
