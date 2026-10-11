# Agent API & Memory Backend

A high-performance Go monolith backing an AI Agent service, providing vector similarity search, long-term memory consolidation, and standard chat streaming.

## Tech Stack
* **Language**: Go 1.26
* **Database**: PostgreSQL (requires `pgvector` and `pg_trgm` extensions)
* **SQL Query Builder**: `sqlc` (with `pgvector-go` overriding PostgreSQL vector types)
* **Migrations**: `goose`
* **Job Queue / Background Tasks**: River (PostgreSQL-backed, replacing Redis)
* **LLM Integration**: Standard OpenAI Go SDK pointing to a custom "Lemonade" provider
* **Embedding Model**: `paraphrase-multilingual-MiniLM-L12-v2` via FastAPI Python sidecar (CPU only)
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

### Vector Search
Memory retrieval operates via **Hybrid Search**, combining:
* `pgvector` cosine similarity (`<=>`)
* `pg_trgm` text similarity (used to support Bulgarian language nuances)

Results are merged directly inside optimized SQL CTEs using Reciprocal Rank Fusion (RRF) with `k=60` and returned to the caller.

To avoid repeated computation of static text vectors, embeddings are cached inside an `embedding_cache` table using SHA256 hashes of the content text. Background workers routinely clear old unaccessed caches.

## Observability
- Centralized JSON logging using standard `slog` with enforced `request_id` and `user_id` context tracing via HTTP middlewares.
- Prometheus metrics are exposed at `/metrics` tracking QPS, token usage, cache hits/misses, and pipeline stage latencies.
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
export GO111MODULE=on

# Run the Go API
go run main.go
```

3. **Scripts**
A suite of backup, restore, and reindex scripts with `set -euo pipefail` are available in the `scripts/` directory.
- `./scripts/backup.sh` uses `pg_dump -Fc`
- `./scripts/restore.sh` uses `pg_restore --clean --if-exists`
- `./scripts/reindex.sh` rebuilds concurrent HNSW indices.
