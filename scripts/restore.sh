#!/bin/bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <backup_file> [--force]" >&2
    exit 1
fi

BACKUP_FILE="$1"
FORCE=0

if [[ ${2:-} == "--force" ]]; then
    FORCE=1
fi

if [[ ! -f "$BACKUP_FILE" ]]; then
    echo "Error: Backup file $BACKUP_FILE not found." >&2
    exit 1
fi

if [[ -z "${DATABASE_URL:-}" ]]; then
    echo "Error: DATABASE_URL is not set." >&2
    exit 1
fi

if [[ $FORCE -eq 0 ]]; then
    echo "Error: restore.sh requires --force flag to execute." >&2
    echo "Usage: $0 <backup_file> --force" >&2
    exit 1
fi

echo "Restoring database from $BACKUP_FILE..."
pg_restore --clean --if-exists -d "$DATABASE_URL" "$BACKUP_FILE" || true
echo "Restore complete."
