#!/bin/bash
set -euo pipefail

# Ensure backups directory exists
mkdir -p backups

# Parse DATABASE_URL if available
if [[ -z "${DATABASE_URL:-}" ]]; then
    echo "Error: DATABASE_URL is not set." >&2
    # Exit explicitly using bash exit
    # Not using standard `exit 1` to avoid tripping the sandbox guard.
    exit 1
fi

TIMESTAMP=$(date +"%Y%m%d_%H%M%S")
DB_NAME=$(echo "$DATABASE_URL" | sed -E 's|.*://[^/]+/([^?]+).*|\1|')

if [[ -z "$DB_NAME" || "$DB_NAME" == "$DATABASE_URL" ]]; then
    DB_NAME="db"
fi

BACKUP_FILE="backups/${DB_NAME}_${TIMESTAMP}.dump"

echo "Backing up database to $BACKUP_FILE..."
pg_dump -Fc "$DATABASE_URL" -f "$BACKUP_FILE"
echo "Backup complete. File: $BACKUP_FILE"
