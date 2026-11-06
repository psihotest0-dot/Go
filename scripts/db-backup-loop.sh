#!/bin/sh
# Database backup loop of the `backup` service (docker-compose.override.yml).
#
# Every DB_BACKUP_INTERVAL_SECONDS (default 6 h; the first round runs at start) it:
#   1. waits until the database accepts connections (after a reboot all containers start
#      together and `depends_on` is not applied, so db may still be recovering);
#   2. writes a dump WITHOUT the rows of embedding_cache to
#      /backups/auto/<db>_<date>_<time>_nocache.dump - small, and holds everything that cannot
#      be rebuilt (chats, memories, documents, chunks, vectors, Oracle decisions);
#   3. writes a FULL dump as well when the newest full one is older than a day (the cache is
#      ~80 % of a full dump but makes a re-ingest of the same files fast);
#   4. copies the uploaded originals /storage -> /backups/storage. Additive, like
#      scripts/backup.ps1: nothing is ever deleted from the copy;
#   5. prunes: the newest DB_BACKUP_KEEP nocache dumps and DB_BACKUP_KEEP_FULL full dumps stay.
#      Only /backups/auto is touched, never the dumps of scripts/backup.ps1 above it.
#
# A dump is written as *.part, checked (size, readable table of contents) and only then
# renamed, so a half-written file never looks like a backup. A failed round logs
# "Backup FAILED" (docker compose logs backup) and is retried after DB_BACKUP_RETRY_SECONDS
# (default 10 min), not after a whole interval.
#
# DB_BACKUP_ONCE=1 runs one round and exits with its status (tests, or by hand:
#   docker compose run --rm -e DB_BACKUP_ONCE=1 backup).
#
# The connection comes from the standard PG* variables (PGHOST, PGUSER, PGPASSWORD,
# PGDATABASE), set by the compose service.

set -u

PGDATABASE="${PGDATABASE:-postgres}"
export PGDATABASE

OUT="${DB_BACKUP_OUT:-/backups/auto}"
STORAGE_SRC="${DB_BACKUP_STORAGE_SRC:-/storage}"
STORAGE_DST="${DB_BACKUP_STORAGE_DST:-/backups/storage}"
INTERVAL="${DB_BACKUP_INTERVAL_SECONDS:-21600}"
RETRY="${DB_BACKUP_RETRY_SECONDS:-600}"
KEEP="${DB_BACKUP_KEEP:-12}"
KEEP_FULL="${DB_BACKUP_KEEP_FULL:-7}"
# A full dump is due when the newest one is older than this (minutes): ~ once a day.
FULL_AFTER_MIN="${DB_BACKUP_FULL_AFTER_MINUTES:-1380}"
# Same floor as scripts/backup.ps1: a smaller file is a broken run, not a backup.
MIN_BYTES=1000

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') $*"
}

need_int() { # need_int <name> <value> <minimum>
    case $2 in
    '' | *[!0-9]*)
        echo "$1 must be a whole number, got '$2'" >&2
        exit 2
        ;;
    esac
    if [ "$2" -lt "$3" ]; then
        echo "$1 must be at least $3, got '$2'" >&2
        exit 2
    fi
}

need_int DB_BACKUP_INTERVAL_SECONDS "$INTERVAL" 60
need_int DB_BACKUP_RETRY_SECONDS "$RETRY" 10
need_int DB_BACKUP_KEEP "$KEEP" 1
need_int DB_BACKUP_KEEP_FULL "$KEEP_FULL" 1
need_int DB_BACKUP_FULL_AFTER_MINUTES "$FULL_AFTER_MIN" 1

wait_db() {
    tries=0
    until pg_isready -q -t 5; do
        tries=$((tries + 1))
        if [ "$tries" -ge 60 ]; then
            log "Backup FAILED: the database did not accept connections within 5 minutes"
            return 1
        fi
        sleep 5
    done
}

# dump <name suffix> [pg_dump arguments...]: 0 only for a checked archive.
dump() {
    suffix=$1
    shift
    file="$OUT/${PGDATABASE}_$(date +%Y%m%d_%H%M%S)${suffix}.dump"
    part="$file.part"

    if ! pg_dump -Fc --no-owner --no-privileges "$@" -f "$part"; then
        rm -f "$part"
        log "Backup FAILED: pg_dump did not finish ($file)"
        return 1
    fi
    size=$(wc -c <"$part")
    if [ "$size" -lt "$MIN_BYTES" ]; then
        rm -f "$part"
        log "Backup FAILED: $file would be only $size bytes"
        return 1
    fi
    if ! pg_restore --list "$part" >/dev/null; then
        rm -f "$part"
        log "Backup FAILED: pg_restore cannot read the archive $file"
        return 1
    fi
    if ! mv "$part" "$file"; then
        rm -f "$part"
        log "Backup FAILED: could not rename the finished archive to $file"
        return 1
    fi
    log "Backup written: $file ($size bytes)"
}

# prune <glob pattern> <how many to keep>: the glob is left unquoted on purpose.
prune() {
    # shellcheck disable=SC2086
    ls -1t $1 2>/dev/null | tail -n +"$(($2 + 1))" | while IFS= read -r old; do
        if rm -f -- "$old"; then
            log "Pruned old backup: $old"
        fi
    done
}

full_is_due() {
    recent=$(find "$OUT" -maxdepth 1 -name "${PGDATABASE}_????????_??????.dump" -mmin "-$FULL_AFTER_MIN" 2>/dev/null | head -n 1)
    [ -z "$recent" ]
}

copy_storage() {
    if [ ! -d "$STORAGE_SRC" ]; then
        log "Backup FAILED: $STORAGE_SRC is not mounted, the uploaded files were not copied"
        return 1
    fi
    mkdir -p "$STORAGE_DST" || return 1
    # -u: only what is new or changed; -p keeps the time stamps so the next round skips it.
    if ! cp -rpu "$STORAGE_SRC"/. "$STORAGE_DST"/; then
        log "Backup FAILED: copying $STORAGE_SRC to $STORAGE_DST"
        return 1
    fi
    files=$(find "$STORAGE_DST" -type f | wc -l)
    log "Storage copy complete: $files files in $STORAGE_DST"
}

round() {
    status=0
    wait_db || return 1
    mkdir -p "$OUT" || return 1
    rm -f "$OUT"/*.part

    if dump _nocache --exclude-table-data=embedding_cache; then
        prune "$OUT/${PGDATABASE}_????????_??????_nocache.dump" "$KEEP"
    else
        status=1
    fi

    if full_is_due; then
        if dump ""; then
            prune "$OUT/${PGDATABASE}_????????_??????.dump" "$KEEP_FULL"
        else
            status=1
        fi
    fi

    # After the dumps, like scripts/backup.ps1: uploads are only ever added, so every file a
    # dump refers to is already in the copy.
    copy_storage || status=1
    return "$status"
}

log "Backup service started: every ${INTERVAL}s, keeping ${KEEP} nocache and ${KEEP_FULL} full dumps in ${OUT}"
while true; do
    round
    rc=$?
    if [ "$rc" -eq 0 ]; then
        pause=$INTERVAL
    else
        pause=$RETRY
        log "The round failed; trying again in ${RETRY}s"
    fi
    if [ "${DB_BACKUP_ONCE:-}" = 1 ]; then
        exit "$rc"
    fi
    sleep "$pause"
done
