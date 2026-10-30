#!/bin/bash
set -euo pipefail

# Dumps the database to a compressed pg_dump archive.
#
# BACKUP_DIR   where the dumps are written (default: backups/, relative to the
#              repository root). Point it outside the repository in production.
# BACKUP_KEEP  how many dumps to keep, oldest removed first (default: 14,
#              0 disables pruning).
#
# The 2026-10-30 incident (docs/incident-2026-10-30/REPORT.md) left nothing to
# restore from because no dump had ever been taken. Run this on a schedule -
# scripts/backup-task.ps1 registers a daily Windows task - and verify the files
# are actually reachable from another machine.

if [[ -z "${DATABASE_URL:-}" ]]; then
    # Mirror internal/config.Load: the application itself falls back to this URL.
    DATABASE_URL="postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
    echo "Warning: DATABASE_URL is not set; using the application default ($DATABASE_URL)." >&2
fi

BACKUP_DIR="${BACKUP_DIR:-backups}"
BACKUP_KEEP="${BACKUP_KEEP:-14}"

# Ensure backups directory exists
mkdir -p "$BACKUP_DIR"

TIMESTAMP=$(date +"%Y%m%d_%H%M%S")
DB_NAME=$(echo "$DATABASE_URL" | sed -E 's|.*://[^/]+/([^?]+).*|\1|')

if [[ -z "$DB_NAME" || "$DB_NAME" == "$DATABASE_URL" ]]; then
    DB_NAME="db"
fi

BACKUP_FILE="${BACKUP_DIR}/${DB_NAME}_${TIMESTAMP}.dump"

echo "Backing up database to $BACKUP_FILE..."
# --no-owner/--no-privileges keep the dump restorable under a different role.
pg_dump -Fc --no-owner --no-privileges "$DATABASE_URL" -f "$BACKUP_FILE"

SIZE=$(wc -c < "$BACKUP_FILE")
if [[ "$SIZE" -lt 1000 ]]; then
    echo "Error: dump is only ${SIZE} bytes, refusing to keep it." >&2
    rm -f "$BACKUP_FILE"
    exit 1
fi

echo "Backup complete. File: $BACKUP_FILE (${SIZE} bytes)"

if [[ "$BACKUP_KEEP" -gt 0 ]]; then
    # Sort by name: the timestamp suffix makes the order chronological.
    KEPT=0
    while IFS= read -r FILE; do
        KEPT=$((KEPT + 1))
        if [[ "$KEPT" -gt "$BACKUP_KEEP" ]]; then
            echo "Pruning old backup: $FILE"
            rm -f "$FILE"
        fi
    done < <(ls -1t "${BACKUP_DIR}/${DB_NAME}"_*.dump 2>/dev/null || true)
fi

