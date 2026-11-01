# Agent API & Memory Backend

A high-performance Go monolith backing an AI Agent service, providing vector similarity search, long-term memory consolidation, and standard chat streaming. It runs on the host (Windows is the primary target, Linux/macOS work too); PostgreSQL, the embedder, Prometheus and Grafana run in Docker.

## Tech Stack
* **Language**: Go 1.26.0
* **Database**: PostgreSQL 15 (requires `pgvector` and `pg_trgm` extensions; the bundled image is `ankane/pgvector:v0.5.1`)
* **SQL Query Builder**: `sqlc` (with `pgvector-go` overriding PostgreSQL vector types)
* **Migrations**: `goose`
* **Job Queue / Background Tasks**: River (PostgreSQL-backed, replacing Redis)
* **LLM Integration**: Standard OpenAI Go SDK pointing to a custom "Lemonade" provider (OpenAI-compatible server; the tuned setup is Qwen 3.5 9B with a 32k context split into two 16k slots)
* **Embedding Model**: `intfloat/multilingual-e5-small` via FastAPI Python sidecar (CPU only)
* **Chat front ends**: the Flet desktop client (separate repository) and an optional Telegram bot
* **Observability**: Prometheus & Grafana (JSON logging via standard `slog`)

## Architecture & Features

### Orchestrator Pipeline
The main agent orchestrator (`internal/agent`) implements a streaming pipeline using Server-Sent Events (SSE). It handles:
1. Fetching history from PostgreSQL and fitting it into a token budget (`LLM_HISTORY_TOKENS`, `LLM_HISTORY_MESSAGES`); older turns are replaced by a rolling summary.
2. Generating real-time embeddings for the user's prompt using the embedder sidecar.
3. Fetching semantically similar long-term memories and document chunks via Hybrid Search.
4. Calling the LLM with the context and streaming response chunks directly to the user. The model may call tools (search, web search/fetch, memory and rule proposals); the output is cleaned of `<think>` blocks and tool markup before it is shown or stored.

All token budgets of one request derive from the context window of one slot (`LLM_CONTEXT_TOKENS`, e.g. `16384` for a 32k server with two slots); every budget can also be set explicitly. See [Configuration](#configuration).

### Writes need the user's confirmation
`save_global_rule` and `create_memory` do not write directly. The model's request becomes a **proposal** that waits for the user (`GET /proposals`, `POST /proposals/{id}/approve|reject`, or `/pending`, `/approve`, `/reject` in Telegram). A proposal that rests on text from the web or from a document says so. Global rules (they apply to every session) are capped in number and can be listed and deleted (`GET /rules`, `DELETE /rules/{id}`). The chat stream announces new proposals with an `event: proposals` frame.

### Memory Consolidation
Background memory consolidation runs asynchronously via River queues (`internal/jobs`).
The pipeline implements:
1. **Fact Extraction**: Calls the LLM specifying a JSON schema to extract structured facts (`fact_key`, `value`, `statement`, `is_explicit_change`) from unanalyzed messages.
2. **State Management**: Facts transition atomically within a single SQL transaction between statuses (`candidate`, `active`, `superseded`). If a new fact contradicts an older one explicitly, the older one is `superseded` and the new one becomes `active`.
3. **Concurrency**: Implements PostgreSQL advisory locks using explicit `conn` handles and `crc32(UUID)` keys to prevent concurrent workers processing the same user's memory tree.
   *Warning: Session-level advisory locks do not work properly when used with PgBouncer in transaction pooling mode.*
4. **Distillation** (`ENABLE_DISTILLATION`, on by default): after a book is ingested, the LLM writes concept summaries of it in the background. This is slow (it can take days for a large library) and shares the model with the chat through the background share of the slots (`LLM_BACKGROUND_SLOTS`).

### Vector Search
Retrieval operates via **Hybrid Search**, combining:
* `pgvector` cosine similarity (`<=>`, HNSW indexes)
* a lexical side: PostgreSQL full-text search with prefix matching (`to_tsvector('simple', …)` with a GIN index) over document chunks, which stands in for Bulgarian stemming; `pg_trgm` `word_similarity` over memories

Results are merged directly inside optimized SQL CTEs using Reciprocal Rank Fusion (RRF) with `k=60` and returned to the caller.

### Embedding model and cache
Every vector in the database (chunks, memories, the cache) comes from the one model named by `config.DefaultEmbedModel`. The backend checks at start that the embedder runs exactly that model and refuses to store vectors from any other.

To avoid repeated computation, embeddings are cached in the `embedding_cache` table under a SHA-256 of the **model name and the text**, so a change of model never returns a vector of the old one. A batch of texts costs one cache read and one write. **Cache rule:** an entry is removed when it has not been used for **30 days** (`last_accessed_at`; a hit renews it, at most once a day). The `embedding_cache_gc` job runs daily and deletes in batches of 1000 until nothing old is left.

**Changing the embedding model** is a deliberate operation, not a setting: vectors of two models are not comparable.
1. Change `DefaultEmbedModel` (and `VectorDimension` plus the `vector(...)` columns in a migration if the dimension changes) and the model loaded by `services/embedder`; rebuild the embedder image.
2. Stop the backend and run `go run ./cmd/reembed -force`. It recomputes the vectors of **memories and document chunks** (`-memories=false` / `-chunks=false` limit it). It does not need the original files.
3. Back up first (`scripts\backup.ps1`); a full re-embed of a large library takes hours of CPU. The old cache entries are no longer read and disappear with the 30-day rule.

If the extraction itself changed (a better PDF parser, say), use `go run ./cmd/reingest` instead: it queues the ingest of the stored originals again.

## File Ingestion Pipeline
The system supports uploading external knowledge documents via the `POST /v1/files` endpoint (also served as `POST /files`).
1. **Upload & Verification**: Enforces a 100 MB limit (`MAX_UPLOAD_MB` changes it; larger files get `413 file too large`), hashes the file for deduplication (a duplicate is answered with `409` and the existing record), securely saves it to the local `storage/` directory, and writes a tracking record. The upload itself accepts any file type; the format is judged by the ingest job.
2. **Extraction**: The `internal/ingest` module picks the parser by the **MIME type sniffed from the file's first 3072 bytes (`mimetype`) or by the file extension** — either one is enough. Supported: plain text (`.txt`, `.md`, `.csv`, `.json`, `.yaml`, `.yml`, `.log`), HTML, PDF, DOCX, XLSX, EPUB and images. PDF text comes from Poppler's `pdftotext`; pages with almost no text are treated as scans and OCR'd (`pdftoppm` + Tesseract with the `bul` and `eng` languages). Images go through Tesseract directly. A file that cannot be read (unsupported, empty, password protected, no text) ends in status `failed` and is not retried.
3. **Background Chunking**: An async River job (`IngestFileWorker`) extracts the raw text, splits it into chunks, generates vector embeddings, and stores them in the `document_chunks` table for retrieval.
4. **Embedding Status**: `GET /v1/files` (also available as `GET /files`) reports `chunk_count`, `has_embeddings` and `embedding_status` per file. Embeddings live in PostgreSQL via `pgvector` — there is no external vector store. `embedding_status` mirrors the ingest status but maps a `completed` file with zero chunks to `empty`, so the UI can distinguish "indexed" from "ingested but nothing to search".

## Telegram bot
The bot starts when `TELEGRAM_BOT_TOKEN` is set. **It answers nobody until `TELEGRAM_ALLOWED_CHAT_IDS` lists the chat ids it may talk to** (comma separated); the token only controls the bot, not who may write to it.
1. Write `/id` to the bot — it answers with your chat id (the only command open to everyone).
2. Put the id in `.env` as `TELEGRAM_ALLOWED_CHAT_IDS=<id>` and restart the backend.
3. Commands: `/link <user-uuid>` ties the chat to an existing backend user (by default the chat gets a user of its own); `/pending` lists the proposals waiting for confirmation, `/approve <n>` and `/reject <n>` decide them. Any other text is a chat message. The bot works in private chats and understands text only (files go through the web client).

## Configuration
The backend reads the environment, and a `.env` file in the working directory is loaded at start (see `.env.example`). Unset means the default; an unparsable number or flag in the `LLM_*`, `MAX_UPLOAD_MB`, `AUTO_MIGRATE` and `EMBEDDER_TIMEOUT_SECONDS` variables is logged as a warning and the default is used.

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable` | Main database. |
| `LISTEN_ADDR` | `127.0.0.1:8080` | Address of the API. The default is loopback on purpose: identity is a bare `X-User-ID` header and `/metrics` is unauthenticated. |
| `MAX_UPLOAD_MB` | `100` | Upload size limit. |
| `AUTO_MIGRATE` | `true` | Apply pending migrations at start. When `false`, a database that is behind makes the start fail and print the command to run (`go run ./cmd/migrate`), so a backup can come first. |
| `PROMPTS_DIR` | empty | Directory of prompt templates (`*.tmpl`) that override the ones compiled into the binary (`internal/prompts`). Empty means no override. |
| `LLM_BASE_URL` | – | OpenAI-compatible URL of the model server (Lemonade), e.g. `http://localhost:13305/v1`. |
| `LLM_MODEL` | empty | Pins the model name sent to the server. Empty: ask the server which model it serves. |
| `LLM_PROVIDER` | empty | `lemonade` makes the client replace any model name chosen elsewhere with the pinned (`LLM_MODEL`) or discovered one. |
| `LLM_SLOTS` | `2` | Parallel completions the model server runs. |
| `LLM_BACKGROUND_SLOTS` | `1` | How many of those slots background work (summaries, consolidation, distillation) may use. |
| `LLM_CONTEXT_TOKENS` | `0` (unknown) | Context window of **one request** = server context / slots (`32768 / 2 = 16384`). While it is unknown the agent keeps conservative budgets sized for ~8k; set it and every budget below is derived from it. |
| `LLM_ANSWER_TOKENS`, `LLM_PROMPT_CONTEXT_TOKENS`, `LLM_HISTORY_TOKENS`, `LLM_HISTORY_MESSAGES`, `LLM_TOOL_RESULT_TOKENS` | `0` (derive) | Override a single budget: the answer reserve, the dynamic part of the system prompt, the replayed history (tokens / messages) and all tool results of a round. |
| `LLM_ENABLE_THINKING` | `true` | Let the model reason before it answers. The reasoning is hidden in the clients; turning it off saves time and tokens. |
| `ENABLE_ADAPTIVE_SKIP` | `true` | Skip retrieval for messages that obviously do not need it. |
| `ENABLE_DISTILLATION` | `true` | Background concept summaries of ingested files. |
| `EMBEDDER_URL` | – | URL of the embedder service, e.g. `http://localhost:8000`. |
| `EMBEDDER_TIMEOUT_SECONDS` | `30` | Limit of one call to the embedder, with its cache reads and writes. |
| `TELEGRAM_BOT_TOKEN` | empty | Starts the Telegram bot. |
| `TELEGRAM_ALLOWED_CHAT_IDS` | empty | Chats the bot may talk to; empty means nobody. |
| `DEEPSEEK_API_KEY` | empty | Key of the paid search service behind `web_search` / `web_fetch`. Without it the web tools report that they are unavailable. |
| `PDFTOTEXT_PATH`, `PDFTOPPM_PATH`, `PDFINFO_PATH` | auto | Poppler executables. On Windows an installed Poppler (`C:\poppler*\Library\bin`) is found by itself and preferred over whatever is first in `PATH` — Git for Windows ships Xpdf's `pdftotext`, which is a different program. Set these when Poppler lives elsewhere. |
| `OCR_DPI`, `OCR_CONCURRENCY` | `250`, `6` | Resolution and number of pages recognized at once when OCR'ing scans. |

Scripts and tests: `TEST_DATABASE_URL` (integration tests; the database name must contain `test`), `BACKUP_DIR`, `BACKUP_KEEP` (backups), `GRAFANA_ADMIN_PASSWORD` (required by `docker compose`).

## Observability
- Centralized JSON logging using standard `slog` with enforced `request_id` and `user_id` context tracing via HTTP middlewares.
- Before returning any HTTP 500 Internal Server Error, explicitly log the error using `slog.Error` so it is not lost.
- Prometheus metrics are exposed at `/metrics`: `api_requests_total` and `api_request_duration_seconds` (labelled by route pattern, not by raw path), `stage_duration_seconds` (pipeline stages), `llm_tokens_total`, `llm_queue_wait_seconds` (waiting for a model slot), `cache_hits_total` / `cache_misses_total`, `ingest_jobs_total` / `ingest_job_duration_seconds`, `distillation_summaries_total`, `agent_proposals_total`, `agent_web_tool_requests_total` (paid), plus gauges for the job queue and for files by ingest status.
- Visualize them with the pre-configured Grafana dashboard (`grafana/dashboards/main.json`). Alert rules are in `prometheus/alerts.yml` (there is no Alertmanager: they show in Prometheus and Grafana only). Their unit tests run with `promtool`; the command is at the top of `prometheus/alerts_test.yml`.

*Security Note: The `user_id` context tracing is critical for tenant isolation and must be securely validated. Furthermore, the `/metrics` endpoint is exposed unauthenticated. The API listens on loopback by default; if you set `LISTEN_ADDR` to a public address, protect it via internal network routing or firewall rules.*

## How to Run

### Windows (PowerShell, the primary setup)
Prerequisites: Go, Docker Desktop, [Tesseract](https://github.com/UB-Mannheim/tesseract/wiki) with the `bul` and `eng` language data, and [Poppler for Windows](https://github.com/oschwartz10612/poppler-windows) (for example in `C:\poppler-<version>\Library\bin`); a model server (Lemonade) listening on `LLM_BASE_URL`.

```powershell
# 1. Settings: copy the template and fill it in (GRAFANA_ADMIN_PASSWORD is required)
Copy-Item .env.example .env

# 2. Infrastructure: PostgreSQL (docker-compose.override.yml), embedder, Prometheus, Grafana
docker compose up -d

# 3. Build the backend (main.exe is a build artifact and is not in git) and start it
powershell -ExecutionPolicy Bypass -File scripts\build.ps1
.\main.exe            # or, without building: go run main.go
```

Start the backend **from the repository root**: `migrations`, `prompts` overrides, `storage` and `.env` are relative to the working directory (the start logs their absolute paths). `main.exe` loads `.env` by itself; for a single run you can also set a variable in the shell: `$env:LISTEN_ADDR = "127.0.0.1:8081"`. To pick up a new build, stop the old `main.exe` first (it keeps the file locked). A port that is already taken is reported at start: `netstat -ano | findstr :8080`.

### Linux / macOS
```bash
docker compose up -d   # the first build downloads the embedding model
export DATABASE_URL="postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
export LLM_BASE_URL="http://localhost:13305/v1" # Lemonade LLM URL
export EMBEDDER_URL="http://localhost:8000"

go run main.go
```
`docker compose up -d` starts the whole infrastructure: `db` (from `docker-compose.override.yml`, with a named volume `go_pgdata`), the FastAPI embedder, Prometheus and Grafana. Their ports are published on `127.0.0.1` only (5432, 8000, 9090, 3000). You can use your own PostgreSQL instead if it has `pgvector` and `pg_trgm`.

### Tools & Migrations
Pending database migrations are applied when the backend starts (`AUTO_MIGRATE`, River's own tables as well). To control when that happens — for instance to take a backup first — set `AUTO_MIGRATE=false` and run:
- **Database Migrations**: `go run ./cmd/migrate` (`-status` only lists what is pending), or `goose -dir migrations postgres "$DATABASE_URL" up`
- **River Migrations**: `river db migrate-v5 --database-url "$DATABASE_URL"`
- **Generate SQLC** (after changing `internal/db/queries` or a migration): `sqlc generate`
- **Library maintenance**: `go run ./cmd/reingest -list | -file <id> | -all` (extract and embed the stored originals again; the running backend does the work), `go run ./cmd/reembed [-force]` (see [Embedding model and cache](#embedding-model-and-cache)).

### Scripts
Backup, restore and reindex scripts are in `scripts/`; `Backup and Restore.md` has the full guide (in Bulgarian).
- `./scripts/backup.sh` uses `pg_dump -Fc`; `BACKUP_DIR` and `BACKUP_KEEP` (default `backups` / `14` dumps) control where dumps go and how many are kept.
- `scripts\backup.ps1` is the Windows equivalent and falls back to `docker compose exec db pg_dump` when the host has no PostgreSQL client; it also copies `storage\` (the uploaded originals). `scripts\restore.ps1` and `scripts\verify-restore.ps1` restore a dump / prove that a dump restores.
- `./scripts/restore.sh` uses `pg_restore --clean --if-exists`
- `./scripts/reindex.sh` rebuilds concurrent HNSW indices.
- `scripts\backup-task.ps1` registers a daily Windows Scheduled Task (`go-monolith-backup`) that runs `backup.ps1` and keeps the dumps outside the repository.
- `scripts\build.ps1` builds `main.exe`.

> **Take backups.** On 2026-10-30 a `DELETE FROM users` cascade wiped the ingested files, the chunks and their embeddings. Nothing could be restored because no dump had ever been taken — see `docs/incident-2026-10-30/REPORT.md`.

## API
The HTTP API is described in [`docs/openapi.yaml`](docs/openapi.yaml). Every route except `/healthz` and `/metrics` needs the `X-User-ID` header (a UUID; the user is created on first use). The Flet client keeps a copy of the file in its own `docs/openapi.yaml`; change both together.

## Tests

Integration tests **never** read `DATABASE_URL`. They require `TEST_DATABASE_URL`, and they refuse to run unless the database name contains `test`:

```bash
# one-time: create the throwaway database
docker compose exec db psql -U postgres -c "CREATE DATABASE app_test"

export TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/app_test?sslmode=disable"
go test -p 1 ./...
```

In PowerShell set the variable with `$env:TEST_DATABASE_URL = "postgres://postgres:postgres@localhost:5432/app_test?sslmode=disable"`.

`-p 1` matters: `internal/db` runs the migrations down to zero and back up, so the packages that share the test database must not run at the same time. Without `TEST_DATABASE_URL` the database-backed tests skip; the pure unit tests still run. Some of them execute `goose.DownTo(..., 0)` and `DELETE FROM users`, which is exactly why they are fenced off from the live connection string. Some tests also need the embedder (`EMBEDDER_URL`). `go test -race` needs a C compiler and does not work on a stock Windows setup; the CI workflow (`.github/workflows/go.yml`) has a separate job that runs it on Linux.

## Known limitations and considerations
* **Vector Dimensions Validation:** When verifying embedder outputs, avoid hardcoding the dimensions logic like `len(vectors[0]) == 384` across the codebase. The dimension is `config.VectorDimension`; a model with another dimension also needs the `vector(...)` columns migrated and everything re-embedded.
* **Embedder Fallback:** When the embedder fails or returns zero-dimension vectors, the fallback logic should not pad or insert zeroed-out arrays of a fixed size, as this pollutes the database with useless embeddings and degrades hybrid search quality.
* **Web tools:** `web_search` / `web_fetch` can send text from the conversation to external services (the paid search API and the fetched URL). URLs are limited to public `http(s)` addresses, but there is no allowlist of domains.
