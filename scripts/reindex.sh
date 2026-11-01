#!/bin/bash
set -euo pipefail

if [[ -z "${DATABASE_URL:-}" ]]; then
    echo "Error: DATABASE_URL is not set." >&2
    exit 1
fi

# The HNSW indexes. Run it after bulk deletes or a big re-ingest: HNSW does not
# shrink on its own (99 MB for 216 rows was seen on 2026-10-31).
for index in idx_memories_embedding idx_document_chunks_raw_embedding idx_document_chunks_concept_embedding; do
    echo "Reindexing $index concurrently..."
    psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c "REINDEX INDEX CONCURRENTLY $index;"
done
echo "Reindex complete."
