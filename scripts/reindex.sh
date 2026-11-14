#!/bin/bash
set -euo pipefail

if [[ -z "${DATABASE_URL:-}" ]]; then
    echo "Error: DATABASE_URL is not set." >&2
    exit 1
fi

# The HNSW indexes. Run it after bulk deletes or a big re-ingest: HNSW does not
# shrink on its own (99 MB for 216 rows was seen on 2026-10-31).
for index in idx_memories_embedding idx_document_chunks_raw_cyrillic_embedding idx_document_chunks_raw_other_embedding idx_document_chunks_concept_embedding; do
    echo "Reindexing $index concurrently..."
    # pgvector 0.8 builds in parallel with the whole maintenance_work_mem in one
    # shared-memory segment; the server's 1 GB leaves ~2 MiB of the 1 GB /dev/shm.
    PGOPTIONS="-c maintenance_work_mem=512MB" psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c "REINDEX INDEX CONCURRENTLY $index;"
done
echo "Reindex complete."
