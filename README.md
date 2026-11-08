# Agent API & Memory Backend

***English** · [Български](README.bg.md)*

A high-performance Go monolith backing an AI Agent service, providing vector similarity search, long-term memory consolidation, and standard chat streaming. It runs on the host (Windows is the primary target, Linux/macOS work too); PostgreSQL, the embedder, Prometheus and Grafana run in Docker.

## Tech Stack
* **Language**: Go 1.26.0
* **Database**: PostgreSQL 15 (requires `pgvector` and `pg_trgm` extensions; the bundled image is `ankane/pgvector:v0.5.1`)
* **SQL Query Builder**: `sqlc` (with `pgvector-go` overriding PostgreSQL vector types)
* **Migrations**: `goose`
* **Job Queue / Background Tasks**: River (PostgreSQL-backed, replacing Redis)
* **LLM Integration**: Standard OpenAI Go SDK pointing to a custom "Lemonade" provider (OpenAI-compatible server; the tuned setup is Gemma 4 12B instruction-tuned, `Gemma-4-12B-it-GGUF`, with a 32k context split into two 16k slots; Qwen 3.5 9B was used before and still works)
* **Embedding Model**: `intfloat/multilingual-e5-small` via FastAPI Python sidecar (CPU only)
* **Chat front ends**: the Flet desktop client (separate repository) and an optional Telegram bot
* **Observability**: Prometheus & Grafana (JSON logging via standard `slog`)

## Architecture & Features

### Orchestrator Pipeline
The main agent orchestrator (`internal/agent`) implements a streaming pipeline using Server-Sent Events (SSE). It handles:
1. Fetching history from PostgreSQL and fitting it into a token budget (`LLM_HISTORY_TOKENS`, `LLM_HISTORY_MESSAGES`); older turns are replaced by a rolling summary.
2. Generating real-time embeddings for the user's prompt using the embedder sidecar.
3. Fetching semantically similar long-term memories and document chunks via Hybrid Search.
4. Calling the LLM with the context and streaming response chunks directly to the user. The model may call tools (search, web search/fetch, saving a memory, proposing a global rule); the output is cleaned of `<think>` blocks and tool markup before it is shown or stored.

All token budgets of one request derive from the context window of one slot (`LLM_CONTEXT_TOKENS`, e.g. `16384` for a 32k server with two slots); every budget can also be set explicitly. See [Configuration](#configuration).

### What the model saves, and what waits for the user
* **Memories** (`create_memory`) are saved at once: what to remember is the model's decision, as in a conversation between people. A memory written in a turn that read a web page or a document carries `metadata.source = "chat_tool"` and `metadata.external_sources`, so a doubtful one can be found and deleted later (`GET /memories`, `DELETE /memories/{id}`).
* **Global rules** (`save_global_rule`) do not write directly, because a rule is the first thing in the system prompt of every session. The model's request becomes a **proposal** that waits for the user (`GET /proposals`, `POST /proposals/{id}/approve|reject`, or `/pending`, `/approve`, `/reject` in Telegram). A proposal that rests on text from the web or from a document says so. Global rules (they apply to every session) are capped in number and can be listed and deleted (`GET /rules`, `DELETE /rules/{id}`). The chat stream announces new proposals with an `event: proposals` frame.

### Files attached to a conversation
A text file (code, a log, a long paste) can be attached to **one conversation** (`POST /chat/attachments`, see `docs/openapi.yaml`). It is not the library: nothing is split, embedded or summarised, and other conversations never see it. The prompt shows the newest files in full (every line with its number) within about 4 000 tokens, which are taken from the history and the retrieved context; the model looks into the rest, or into any file, with the `read_attachment` tool (a part by line numbers, or the lines that contain a text, so a function can be found in a file of thousands of lines). Limits: 256 KB a file, 20 files and 1 MB a conversation; a file nobody attaches again for 30 days is removed. The text of a file is untrusted: a memory written after reading it carries `external_sources: ["attachments"]`, and a global rule waits for the user as always.

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

Two details matter for a library with books in more than one language:
* **Associative search.** The `search_documents` tool first finds the concept summaries nearest to the question and lends their themes to the text side, so a Bulgarian question also finds passages of an English book (on 11 questions with a known answer, a chunk of the right book was among the first five results 11 times, against 6 before).
* **Candidate pool and result count.** The vector scan keeps the 400 nearest chunks (not 200) before filtering, and `search_documents` returns up to 8 passages (`search_memories` 5). On 33 questions with a known answer this put a chunk of the right book among the results 27 times instead of 25, and a relevant passage 22 times instead of 19, at the same search time (about 10 ms) and about 1 000 more tokens per call.
* **Language-aware distance limit.** A question is farther from passages in another language than from passages in its own, so a chunk whose language differs from the question's gets 0.03 more distance before it is dropped (`chunkCrossLanguageMargin`).

No distance limit can tell an answer from an off-topic hit with this embedding model (nearest relevant chunks at 0.10-0.21, nearest chunks of questions the library cannot answer at 0.18-0.21), so the model that reads the results is the one that judges relevance.

### Embedding model and cache
Every vector in the database (chunks, memories, the cache) comes from the one model named by `config.DefaultEmbedModel`. The backend checks at start that the embedder runs exactly that model and refuses to store vectors from any other.

To avoid repeated computation, embeddings are cached in the `embedding_cache` table under a SHA-256 of the **model name and the text**, so a change of model never returns a vector of the old one. A batch of texts costs one cache read and one write. **Cache rule:** an entry is removed when it has not been used for **30 days** (`last_accessed_at`; a hit renews it, at most once a day). The `embedding_cache_gc` job runs daily and deletes in batches of 1000 until nothing old is left.

**Changing the embedding model** is a deliberate operation, not a setting: vectors of two models are not comparable.
1. Change `DefaultEmbedModel` (and `VectorDimension` plus the `vector(...)` columns in a migration if the dimension changes) and the model loaded by `services/embedder`; rebuild the embedder image.
2. Stop the backend and run `go run ./cmd/reembed -force`. It recomputes the vectors of **memories and document chunks** (`-memories=false` / `-chunks=false` limit it). It does not need the original files.
3. Back up first (`scripts\backup.ps1`); a full re-embed of a large library takes hours of CPU. The old cache entries are no longer read and disappear with the 30-day rule.

If the extraction itself changed (a better PDF parser, say), use `go run ./cmd/reingest` instead: it queues the ingest of the stored originals again. It can only do that for files whose original is still in `storage/` (see `KEEP_ORIGINALS`); the others are left alone, with their text and vectors untouched.

## File Ingestion Pipeline
The system supports uploading external knowledge documents via the `POST /v1/files` endpoint (also served as `POST /files`).
1. **Upload & Verification**: Enforces a 100 MB limit (`MAX_UPLOAD_MB` changes it; larger files get `413 file too large`), hashes the file for deduplication (a duplicate is answered with `409` and the existing record), securely saves it to the local `storage/` directory, and writes a tracking record. The original stays there after the ingest unless `KEEP_ORIGINALS=false`. The upload itself accepts any file type; the format is judged by the ingest job.
2. **Extraction**: The `internal/ingest` module picks the parser by the **MIME type sniffed from the file's first 3072 bytes (`mimetype`) or by the file extension** — either one is enough. Supported: plain text (`.txt`, `.md`, `.csv`, `.json`, `.yaml`, `.yml`, `.log`), HTML, PDF, DOCX, XLSX, EPUB and images. PDF text comes from Poppler's `pdftotext`; pages with almost no text are treated as scans and OCR'd (`pdftoppm` + Tesseract with the `bul` and `eng` languages). Images go through Tesseract directly. A file that cannot be read (unsupported, empty, password protected, no text) ends in status `failed` and is not retried.
3. **Background Chunking**: An async River job (`IngestFileWorker`) extracts the raw text, splits it into chunks, generates vector embeddings, and stores them in the `document_chunks` table for retrieval.
4. **Embedding Status**: `GET /v1/files` (also available as `GET /files`) reports `chunk_count`, `has_embeddings` and `embedding_status` per file. Embeddings live in PostgreSQL via `pgvector` — there is no external vector store. `embedding_status` mirrors the ingest status but maps a `completed` file with zero chunks to `empty`, so the UI can distinguish "indexed" from "ingested but nothing to search".

## Telegram bot
The bot starts when `TELEGRAM_BOT_TOKEN` is set. **It answers nobody until `TELEGRAM_ALLOWED_CHAT_IDS` lists the chat ids it may talk to** (comma separated); the token only controls the bot, not who may write to it.
1. Write `/id` to the bot — it answers with your chat id (the only command open to everyone).
2. Put the id in `.env` as `TELEGRAM_ALLOWED_CHAT_IDS=<id>` and restart the backend.
3. Commands: `/link <user-uuid>` ties the chat to an existing backend user (by default the chat gets a user of its own); `/pending` lists the proposals waiting for confirmation, `/approve <n>` and `/reject <n>` decide them. Any other text is a chat message. The bot works in private chats and understands text only (files go through the web client).

## The Oracle (second opinion for the trading bot)
The trading bot ([`alpaca-roundtable-trader`](https://github.com/psihotest0-dot/alpaca-roundtable-trader), a separate project, paper trading only) asks this backend for a second opinion on each signal of its strategy. It sends the signal with the numbers around it (indicators, the last bars, volume, the account and its limits) and up to five cleaned news headlines; the model answers `APPROVE`, `VETO` or `HOLD` with one short sentence of reason in Bulgarian. The bot's risk engine still decides every order. The model never proposes an order, a size or a price.

* **Routes.** `POST /oracle/evaluate` and `GET /oracle/ping` need `Authorization: Bearer <ORACLE_TOKEN>` and no `X-User-ID`. They are served only when `ORACLE_TOKEN` is at least 32 characters; otherwise they do not exist. `GET /oracle/decisions` (the journal, below) is an ordinary route with `X-User-ID`. The contract is in [`docs/openapi.yaml`](docs/openapi.yaml).
* **What bounds a planted sentence.** The model has no tools, no memory, no history and no access to the database. The news is cleaned twice (the bot and here: one line, no addresses, markup or invisible characters, the length cut) and shown as quoted data. The reply is held to a schema whose verdict can only be one of the three words; anything else, a reply cut off at the token limit, or a model that does not answer is **not a verdict**: the call is asked once more, then fails with `502`/`504`, and the bot reads that as `HOLD`, never as a yes. Cleaning cannot recognise an instruction in plain words, so the design relies on this limit, not on the cleaning.
* **Repeats.** Asking again about the same `signal_id` returns the earlier verdict with `cached: true` (a `HOLD` for 3 minutes, a final verdict for 30), and two questions about one signal at once share one call to the model. The work goes on when the bot has stopped waiting, so that its next question finds the answer.
* **The journal.** Every call, answered or failed, is a row of `oracle_evaluations` (verdict, reason, model, tokens, time, the request as received, and two labels: `signal_version`, the strategy version the bot sent, where `oracle-probe` marks the bot's own test questions, and `prompt_version`, a 12-character hash of the question the model was asked — the system prompt, the answer schema, the call settings and the layout of the facts — which is also returned in the reply of `/oracle/evaluate`, so that a changed prompt starts a new period of the evaluation instead of being guessed from dates). The bot first runs in **shadow mode**: it ignores the verdicts, and the journal is the whole result, to be compared later with what the signals went on to do (the bot's backtest can replay an exported record of verdicts: `export-oracle-decisions`, `--replay-oracle`). Judge it only on signals that come after the day it was switched on, never on old news (the model has seen the past).
* **Metrics.** `oracle_requests_total{outcome}` (`answered`, `cached`, `failed`, `unauthorized`, `bad_request`), `oracle_verdicts_total{verdict}`, `oracle_latency_seconds`. On the development machine a call takes about 1.5 to 2.5 seconds with `Gemma-4-12B-it-GGUF`. Three alerts in `prometheus/alerts.yml` (group `oracle`) watch them: `OracleGivesNoVerdict` (2 or more calls without a verdict in 30 minutes), `OracleCallsRefused` (a wrong token or an unusable body) and `OracleSlow` (an average over 8 seconds). Nothing is sent anywhere: an alert is something to look at in Prometheus or Grafana. The prompt is `internal/prompts/oracle_system.tmpl`.
* **Turning it on.** Make a token (`python -c "import secrets; print(secrets.token_hex(24))"`), put it in this backend's `.env` as `ORACLE_TOKEN` and in the bot's `.env`, and restart the backend; the start log says whether the route is on. The new table is created by the migration at the start.

## Configuration
The backend reads the environment, and a `.env` file in the working directory is loaded at start (see `.env.example`). Unset means the default; an unparsable number or flag in the `LLM_*`, `MAX_UPLOAD_MB`, `AUTO_MIGRATE` and `EMBEDDER_TIMEOUT_SECONDS` variables is logged as a warning and the default is used.

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable` | Main database. |
| `LISTEN_ADDR` | `127.0.0.1:8080` | Address of the API. The default is loopback on purpose: identity is a bare `X-User-ID` header and `/metrics` is unauthenticated. |
| `MAX_UPLOAD_MB` | `100` | Upload size limit. |
| `AUTO_MIGRATE` | `true` | Apply pending migrations at start. When `false`, a database that is behind makes the start fail and print the command to run (`go run ./cmd/migrate`), so a backup can come first. |
| `KEEP_ORIGINALS` | `true` | Keep the uploaded file in `storage/` after a successful ingest. The original is only needed to ingest the file again (`cmd/reingest`); search and chat use the text and vectors in the database. With `false` it is deleted once its ingest has succeeded (a failed ingest keeps it, so it can be tried again), and `cmd/reingest` leaves such files alone. A new value needs a restart of the backend. |
| `LOG_MAX_MB` | `10` | Size at which `backend.log` is rotated: the old file is compressed and kept as one backup (`backend-<time>.log.gz`), so the logs take about this much and no more. A file that is already larger is rotated at the first write after the start. |
| `METRICS_LOG_MAX_MB` | `5` | The `incoming request` lines of the polling go to their own `metrics.log`, not to `backend.log`: the Prometheus scrapes (`GET /metrics`, every 15 s) and the client's reads of the Oracle journal (`GET /oracle/decisions`, every 10 s while the "Бот" tab is open). It is emptied when it reaches this size and keeps no backup. |
| `PROMPTS_DIR` | empty | Directory of prompt templates (`*.tmpl`) that override the ones compiled into the binary (`internal/prompts`). Empty means no override. |
| `LLM_BASE_URL` | – | OpenAI-compatible URL of the model server (Lemonade), e.g. `http://localhost:13305/v1`. |
| `LLM_MODEL` | empty | Pins the model name sent to the server. Empty: ask the server which model it serves. |
| `LLM_PROVIDER` | empty | `lemonade` makes the client replace any model name chosen elsewhere with the pinned (`LLM_MODEL`) or discovered one. |
| `LLM_SLOTS` | `2` | Parallel completions the model server runs. |
| `LLM_BACKGROUND_SLOTS` | `1` | How many of those slots background work (summaries, consolidation, distillation) may use. |
| `LLM_CONTEXT_TOKENS` | `0` (unknown) | Context window of **one request** = server context / slots (`32768 / 2 = 16384`). While it is unknown the agent keeps conservative budgets sized for ~8k; set it and every budget below is derived from it. |
| `LLM_ANSWER_TOKENS`, `LLM_PROMPT_CONTEXT_TOKENS`, `LLM_HISTORY_TOKENS`, `LLM_HISTORY_MESSAGES`, `LLM_TOOL_RESULT_TOKENS` | `0` (derive) | Override a single budget: the answer reserve, the dynamic part of the system prompt, the replayed history (tokens / messages) and all tool results of a round. |
| `LLM_ENABLE_THINKING` | `false` | Let the model reason before it answers. Off by default: Qwen 3.5 9B leaked its reasoning into the chat with it on. The reasoning is hidden in the clients; turning it on costs time and tokens. |
| `LLM_TEMPERATURE` | `0.7` | Temperature of every chat request (0 to 2). Lemonade starts Qwen 3.5 and Gemma 4 with 1.0; measured on 2026-11-02 in Bulgarian conversations, 0.7 with `LLM_TOP_P=0.8` gave far fewer non-words (Qwen 3.5 9B: about 2.5 times fewer; Gemma 4 12B: about 5 times fewer) and fewer closing questions. |
| `LLM_TOP_P` | `0.8` | Top-p of every chat request (0.01 to 1). |
| `LLM_PRESENCE_PENALTY` | `0` (off) | Presence penalty sent with every chat request (0 to 2); `0` is not sent. Tried at 0.5 and 1.0 on 2026-11-02: no clear effect on the replies, so it stays off. |
| `ENABLE_ADAPTIVE_SKIP` | `true` | Skip retrieval for messages that obviously do not need it. |
| `ENABLE_DISTILLATION` | `true` | Background concept summaries of ingested files. |
| `EMBEDDER_URL` | – | URL of the embedder service, e.g. `http://localhost:8000`. |
| `EMBEDDER_TIMEOUT_SECONDS` | `30` | Limit of one call to the embedder, with its cache reads and writes. |
| `TELEGRAM_BOT_TOKEN` | empty | Starts the Telegram bot. |
| `TELEGRAM_ALLOWED_CHAT_IDS` | empty | Chats the bot may talk to; empty means nobody. |
| `DEEPSEEK_API_KEY` | empty | Key of the paid search service behind `web_search` / `web_fetch`. Without it the web tools report that they are unavailable. |
| `ORACLE_TOKEN` | empty | Bearer token of the Oracle routes the trading bot calls (`/oracle/evaluate`, `/oracle/ping`). Empty or shorter than 32 characters: the routes are not served. Keep it out of git; the same value goes into the bot's `.env`. |
| `PDFTOTEXT_PATH`, `PDFTOPPM_PATH`, `PDFINFO_PATH` | auto | Poppler executables. On Windows an installed Poppler (`C:\poppler*\Library\bin`) is found by itself and preferred over whatever is first in `PATH` — Git for Windows ships Xpdf's `pdftotext`, which is a different program. Set these when Poppler lives elsewhere. |
| `OCR_DPI`, `OCR_CONCURRENCY` | `250`, `6` | Resolution and number of pages recognized at once when OCR'ing scans. |

### The model server (Lemonade)

These settings live in Lemonade, not in this backend. They are what the chat was tuned and measured with on 2026-11-02: `Gemma-4-12B-it-GGUF` on a 16 GB GPU with 32 GB of RAM.

* **Context.** `ctx_size` 32768 with `--parallel 2` gives 16384 tokens per slot, which is what `LLM_CONTEXT_TOKENS` assumes. Lemonade's automatic size can come out smaller (31022 gave 15616 per slot). After loading, look for `n_ctx_slot` in Lemonade's log (`%TEMP%\lemonade-server.log`).
* **Options per model** go through `POST /v1/models/<id>/options`, for example `{"ctx_size": 32768, "llamacpp_args": "--no-mmproj-offload --cache-ram 0 --ctx-checkpoints 2"}`. The model's own sampler defaults from Lemonade stay in force that way; a `llamacpp_args` sent with the load request replaces them.
* `--no-mmproj-offload` keeps the vision projector off the GPU (the chat sends no images).
* `--cache-ram 0 --ctx-checkpoints 2` stop llama.cpp from keeping prompt snapshots in RAM. Without them the server grew from 7.8 to 15-17 GB of RAM during a short test; with them it stays at 8.5 GB and the speed is the same (about 62 tokens/s, a reply in about 1.7 s).
* **One model at a time.** The 12B takes 11.9 GB of the GPU and the 9B 9.7 GB, so they do not fit together. Set `LLM_MODEL`: when it is empty the backend takes the first model by name from `/v1/models`, and a newly downloaded model can change that.

Scripts and tests: `TEST_DATABASE_URL` (integration tests; the database name must contain `test`), `BACKUP_DIR`, `BACKUP_KEEP` (backups), `GRAFANA_ADMIN_PASSWORD` (required by `docker compose`).

## Observability
- Centralized JSON logging using standard `slog` with enforced `request_id` and `user_id` context tracing via HTTP middlewares.
- Before returning any HTTP 500 Internal Server Error, explicitly log the error using `slog.Error` so it is not lost.
- Prometheus metrics are exposed at `/metrics`: `api_requests_total` and `api_request_duration_seconds` (labelled by route pattern, not by raw path), `stage_duration_seconds` (pipeline stages), `llm_tokens_total`, `llm_queue_wait_seconds` (waiting for a model slot), `cache_hits_total` / `cache_misses_total`, `ingest_jobs_total` / `ingest_job_duration_seconds`, `distillation_summaries_total`, `agent_proposals_total`, `agent_web_tool_requests_total` (paid), plus gauges for the job queue and for files by ingest status.
- Visualize them with the pre-configured Grafana dashboard (`grafana/dashboards/main.json`). Alert rules are in `prometheus/alerts.yml` (there is no Alertmanager: they show in Prometheus and Grafana only). Their unit tests run with `promtool`; the command is at the top of `prometheus/alerts_test.yml`.
- The same Prometheus also scrapes the trading bot (another project, with its own compose): the job `alpaca-trader` in `prometheus/prometheus.yml` reads `host.docker.internal:9101` (trader) and `:9102` (observer), which the bot publishes on the host loopback. Its series all start with `alpaca_`. Its alert rules are the group `alpaca-trader` in `prometheus/alerts.yml` (with 17 cases in `alerts_test.yml`; a target that stays down for 5 minutes shows as `AlpacaTargetDown`) and its dashboard is `grafana/dashboards/alpaca-trader.json`. The two targets carry the label `service` (`trader`, `observer`): the observer also exports the trader-only gauges (always 0), so the rules and panels for them select `{service="trader"}`. Grafana does not notice a new or changed dashboard file on a Windows bind mount by itself: run `docker compose restart grafana` after editing one.
- Prometheus keeps its time series (15 days, the default) in the named volume `go_prometheus_data`: they survive `docker compose down` and a rebuilt container, and are deleted only by `docker compose down -v` or by removing the volume.

*Security Note: The `user_id` context tracing is critical for tenant isolation and must be securely validated. Furthermore, the `/metrics` endpoint is exposed unauthenticated. The API listens on loopback by default; if you set `LISTEN_ADDR` to a public address, protect it via internal network routing or firewall rules. Because there is no token, the server also refuses (421, logged as a warning) every request whose `Host` header is not `localhost`, `127.0.0.1`, `[::1]`, `host.docker.internal` or the address in `LISTEN_ADDR`: a web page that makes its own domain resolve to `127.0.0.1` (DNS rebinding) still sends its own name as `Host`, so it cannot use the API as if it were the client.*

## How to Run

### Windows (PowerShell, the primary setup)
Prerequisites: Go, Docker Desktop, [Tesseract](https://github.com/UB-Mannheim/tesseract/wiki) with the `bul` and `eng` language data, and [Poppler for Windows](https://github.com/oschwartz10612/poppler-windows) (for example in `C:\poppler-<version>\Library\bin`) — the exact steps are in [Poppler and Tesseract on Windows](#poppler-and-tesseract-on-windows-pdf-text-and-ocr) below; a model server (Lemonade) listening on `LLM_BASE_URL`.

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

### Poppler and Tesseract on Windows (PDF text and OCR)
Two programs outside Go and Docker read the PDFs and the images: **Poppler** (`pdftotext`, `pdfinfo`, `pdftoppm`) and **Tesseract**. The backend starts them as child processes of the host, so they must be installed on the machine that runs `main.exe`; the Docker containers do not need them. This is how the development machine is set up:

| | Version | Where | Where it comes from |
|---|---|---|---|
| Poppler | 26.09.0 | `C:\poppler-26.09.0\` (the programs are in `Library\bin`) | the zip of a [poppler-windows release](https://github.com/oschwartz10612/poppler-windows/releases), unpacked; there is no installer |
| Tesseract | 5.4.0.20240606 | `C:\Program Files\Tesseract-OCR\` | the Windows installer of the [UB Mannheim build](https://github.com/UB-Mannheim/tesseract/wiki) |
| Language data | `bul`, `eng` (the installer adds `osd` and `script\Cyrillic`) | `C:\Program Files\Tesseract-OCR\tessdata\` | `eng` from the installer, `bul.traineddata` from [tessdata_fast](https://github.com/tesseract-ocr/tessdata_fast) |

**Poppler**
1. Download the release zip (`Release-<version>.zip`) and unpack it so that the `poppler-<version>` folder it contains sits directly under `C:\`. The result must be `C:\poppler-26.09.0\Library\bin\pdftotext.exe`. Keep one Poppler folder only: when several match, the last one by name is used.
2. That is all the backend needs. It looks in `C:\poppler*\Library\bin` (then `C:\poppler*\bin` and `C:\Program Files\poppler*\...`) **before** `PATH`, and logs the programs it picked at startup (`PDF tool … version`). The order matters: Git for Windows ships `pdftotext` from Xpdf, a different program, and it can come first in `PATH`. If Poppler lives elsewhere, set `PDFTOTEXT_PATH`, `PDFTOPPM_PATH` and `PDFINFO_PATH` in `.env`.
3. On the development machine `C:\poppler-26.09.0\Library\bin` is also in the system `PATH`, so the tools can be run by hand; the backend does not depend on it.

**Tesseract**
1. Run the installer (administrator rights) and keep the default folder, `C:\Program Files\Tesseract-OCR`.
2. Bulgarian is not installed by default. Either tick it under "Additional language data" in the installer, or download `bul.traineddata` from [tessdata_fast](https://raw.githubusercontent.com/tesseract-ocr/tessdata_fast/main/bul.traineddata) and copy it to `C:\Program Files\Tesseract-OCR\tessdata\` (administrator rights). The development machine has the `tessdata_fast` file (1 675 212 bytes) next to the installer's own `eng.traineddata`, which is a `tessdata_fast` file too; the larger `tessdata_best` models have not been tried here.
3. Put `C:\Program Files\Tesseract-OCR` into the system `PATH` (on the development machine this was done by hand). The backend runs the bare command `tesseract` and there is no setting for its location. A running process keeps the environment it started with, so open a new terminal and restart `main.exe` afterwards.
4. Check both from a new PowerShell window:

```powershell
tesseract --version        # first line: tesseract v5.4.0.20240606
tesseract --list-langs     # must list bul and eng
& (Get-ChildItem C:\poppler*\Library\bin\pdftotext.exe | Select-Object -Last 1).FullName -v   # pdftotext version 26.09.0
```

**How the backend uses them** (to read a failure):
- A PDF is read with `pdftotext`, its page count comes from `pdfinfo`. Pages with almost no text are treated as scans: `pdftoppm` renders the page at `OCR_DPI` (250 by default) and `tesseract stdin stdout -l bul+eng --psm 3 -c preserve_interword_spaces=1` reads it. An image file goes to the same command directly.
- Tesseract is started single-threaded (`OMP_THREAD_LIMIT=1`), `OCR_CONCURRENCY` pages (6 by default) at a time, and one run is cut off after 2 minutes. These numbers were measured on the development machine (16 threads); see the comments in `internal/ingest/ocr.go`.
- Only Poppler is reported at startup. A missing or broken Tesseract shows up when the first scan or image is ingested: every page fails, the backend gives up after about ten failed pages, and `backend.log` has `tesseract OCR failed`. Run the three commands above to find out which part is missing.
- On Linux the same programs come from the packages `poppler-utils`, `tesseract-ocr`, `tesseract-ocr-bul` and `tesseract-ocr-eng` (not tried with this project).

### Linux / macOS
```bash
docker compose up -d   # the first build downloads the embedding model
export DATABASE_URL="postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
export LLM_BASE_URL="http://localhost:13305/v1" # Lemonade LLM URL
export EMBEDDER_URL="http://localhost:8000"

go run main.go
```
`docker compose up -d` starts the whole infrastructure: `db` (from `docker-compose.override.yml`, with a named volume `go_pgdata`), the FastAPI embedder, Prometheus, Grafana and `backup` (the database archive, see below). Their ports are published on `127.0.0.1` only (5432, 8000, 9090, 3000). Every service has `restart: unless-stopped`, so after a reboot (Docker Desktop must start with Windows) they come back by themselves; the backend is not one of them: `main.exe` runs on the host and has to be started by hand. You can use your own PostgreSQL instead if it has `pgvector` and `pg_trgm`.

### Tools & Migrations
Pending database migrations are applied when the backend starts (`AUTO_MIGRATE`, River's own tables as well). To control when that happens — for instance to take a backup first — set `AUTO_MIGRATE=false` and run:
- **Database Migrations**: `go run ./cmd/migrate` (`-status` only lists what is pending), or `goose -dir migrations postgres "$DATABASE_URL" up`
- **River Migrations**: `river db migrate-v5 --database-url "$DATABASE_URL"`
- **Generate SQLC** (after changing `internal/db/queries` or a migration): `sqlc generate`
- **Library maintenance**: `go run ./cmd/reingest -list | -file <id> | -all` (extract and embed the stored originals again; the running backend does the work), `go run ./cmd/reembed [-force]` (see [Embedding model and cache](#embedding-model-and-cache)), `go run ./cmd/recategorize [-apply]` (work out the category of the files again from the stored vectors; it only shows the differences without `-apply`).

### Scripts
Backup, restore and reindex scripts are in `scripts/`; `Backup and Restore.md` has the full guide (in Bulgarian).
- `./scripts/backup.sh` uses `pg_dump -Fc`; `BACKUP_DIR` and `BACKUP_KEEP` (default `backups` / `14` dumps) control where dumps go and how many are kept.
- `scripts\backup.ps1` is the Windows equivalent and falls back to `docker compose exec db pg_dump` when the host has no PostgreSQL client; it also copies `storage\` (the uploaded originals). `scripts\restore.ps1` and `scripts\verify-restore.ps1` restore a dump / prove that a dump restores.
- `./scripts/restore.sh` uses `pg_restore --clean --if-exists`
- `./scripts/reindex.sh` rebuilds concurrent HNSW indices.
- `scripts\backup-task.ps1` registers a daily Windows Scheduled Task (`go-monolith-backup`) that runs `backup.ps1` and keeps the dumps outside the repository.
- The `backup` compose service (`scripts/db-backup-loop.sh`) archives the database while Docker runs, the way the trading bot does: at start and every 6 hours it writes a checked dump without `embedding_cache` (~250 MB) and, about once a day, a full one (~470 MB) to `$BACKUP_DIR/auto`, and copies `storage/` to `$BACKUP_DIR/storage`. Failures show as `Backup FAILED` in `docker compose logs backup` and as `unhealthy` in `docker ps`. Settings: `BACKUP_DIR`, `DB_BACKUP_*`, `TZ` in `.env.example`; details in `Backup and Restore.md`.
- `scripts\build.ps1` builds `main.exe`.

> **Take backups.** On 2026-10-30 a `DELETE FROM users` cascade wiped the ingested files, the chunks and their embeddings. Nothing could be restored because no dump had ever been taken — the 2026-10-30 incident.

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

`-p 1` matters: `internal/db` runs the migrations down to zero and back up, so the packages that share the test database must not run at the same time. Without `TEST_DATABASE_URL` the database-backed tests skip; the pure unit tests still run. Some of them execute `goose.DownTo(..., 0)` and `DELETE FROM users`, which is exactly why they are fenced off from the live connection string. Some tests also need the embedder (`EMBEDDER_URL`). `go test -race` needs a C compiler and does not work on a stock Windows setup; the CI workflow (`.github/workflows/go.yml`) has a separate job that runs it on Linux. A second workflow, `.github/workflows/vulncheck.yml`, runs `govulncheck` (Linux and Windows) on every push to `main` and every Monday, so a new advisory against the Go version in `go.mod` or a dependency the code calls shows up as a red run without any commit.

## Known limitations and considerations
* **Vector Dimensions Validation:** When verifying embedder outputs, avoid hardcoding the dimensions logic like `len(vectors[0]) == 384` across the codebase. The dimension is `config.VectorDimension`; a model with another dimension also needs the `vector(...)` columns migrated and everything re-embedded.
* **Embedder Fallback:** When the embedder fails or returns zero-dimension vectors, the fallback logic should not pad or insert zeroed-out arrays of a fixed size, as this pollutes the database with useless embeddings and degrades hybrid search quality.
* **Web tools:** `web_search` / `web_fetch` can send text from the conversation to external services (the paid search API and the fetched URL). URLs are limited to public `http(s)` addresses, but there is no allowlist of domains.

## License

MIT - see [LICENSE](LICENSE).
