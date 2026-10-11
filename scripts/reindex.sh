#!/bin/bash
set -euo pipefail

if [[ -z "${DATABASE_URL:-}" ]]; then
    echo "Error: DATABASE_URL is not set." >&2
    exit 1
fi

echo "Reindexing HNSW index concurrently..."
psql "$DATABASE_URL" -c "REINDEX INDEX CONCURRENTLY idx_memories_embedding;"
echo "Reindex complete."
