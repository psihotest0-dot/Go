<#
.SYNOPSIS
Lists, and with -Apply deletes, the throwaway users that live checks leave in the database.

.DESCRIPTION
The API creates a user the first time it sees an X-User-ID, so every check against
the running backend that uses a random UUID leaves a row in "users" behind (18 of
them on 2026-11-13, 4 more on 2026-11-19). To make them easy to find, a live check
uses ids from one reserved range:

    7e570000-0000-4000-8000-<12 hex digits>      ("7e57" reads "test")

for example 7e570000-0000-4000-8000-0000000000a1. This script touches exactly that
range and nothing else: the real user (123e4567-...) and the users of Telegram chats
are never matched.

Without -Apply it only lists the users and what they own. With -Apply it deletes
them, and with them everything that cascades from "users" (messages, memories,
traces, files, chunks...), in one transaction.

Migration 00018 makes a plain DELETE FROM users fail (the incident of 2026-10-30).
The transaction switches app.allow_user_delete on for itself only, after the range
and the guards below have been checked.

Guards (the transaction rolls back and nothing is deleted):
* a user of the range with a Telegram link is left alone and reported;
* a user of the range that still owns a file that is not deleted stops the run:
  its stored original on disk would be left behind. Delete the file first
  (DELETE /v1/files/{id}), then run the script again.

The folder of the assistant's own files (FILES_DIR\<user-id>) is not touched; a
check that used the file tools removes its files through the API.

No backup is taken: the users of this range are disposable by definition. Take one
(scripts\backup.ps1) before using -Apply on anything else.

.PARAMETER Service
docker compose service name of the database. Defaults to db.

.PARAMETER Apply
Delete the users. Without it the script only lists them.

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\cleanup-test-users.ps1

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\cleanup-test-users.ps1 -Apply
#>
[CmdletBinding()]
param(
    [string]$Service = "db",
    [switch]$Apply
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
. (Join-Path $PSScriptRoot "lib\db-container.ps1")

# The reserved range. A literal, not a parameter: a mistyped value must not be able
# to widen the delete to real users.
$likePattern = "7e570000-0000-4000-8000-%"

$url = Get-DatabaseUrl -RepoRoot $repoRoot
$target = Get-DatabaseTarget -Url $url
Assert-SafeDatabaseName -Name $target.Name

Push-Location $repoRoot
try {
    # Every table that points at users(id) is counted, found from the catalog, so a
    # table added by a later migration is included without a change here.
    $listSql = @'
SELECT u.id::text,
       to_char(u.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI'),
       (SELECT COALESCE(sum((xpath('/row/c/text()', query_to_xml(
                 format('SELECT count(*) AS c FROM %s WHERE %I = %L', c.conrelid::regclass, a.attname, u.id),
                 false, true, '')))[1]::text::bigint), 0)
          FROM pg_constraint c
          JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
         WHERE c.confrelid = 'users'::regclass AND c.contype = 'f'),
       (SELECT count(*) FROM ingested_files f WHERE f.user_id = u.id AND f.deleted_at IS NULL),
       (SELECT count(*) FROM telegram_users t WHERE t.user_id = u.id)
  FROM users u
 WHERE u.id::text LIKE '7e570000-0000-4000-8000-%'
 ORDER BY u.created_at, u.id;
'@
    $rows = @(Invoke-DbSql -Service $Service -User $target.User -Database $target.Name -Sql $listSql)

    if ($rows.Count -eq 0) {
        Write-Host "No test users (ids 7e570000-0000-4000-8000-...) in '$($target.Name)'."
        return
    }

    Write-Host ("{0,-36}  {1,-16}  {2,6}  {3,10}  {4,8}" -f "id", "created (UTC)", "rows", "live files", "telegram")
    $liveFiles = 0
    $linked = 0
    foreach ($row in $rows) {
        $id, $created, $owned, $files, $tg = $row -split '\|'
        Write-Host ("{0,-36}  {1,-16}  {2,6}  {3,10}  {4,8}" -f $id, $created, $owned, $files, $tg)
        $liveFiles += [int]$files
        if ([int]$tg -gt 0) { $linked++ }
    }
    Write-Host ""

    if (-not $Apply) {
        Write-Host "$($rows.Count) test user(s). Nothing was changed; run again with -Apply to delete them."
        return
    }

    if ($liveFiles -gt 0) {
        throw "$liveFiles file(s) of the test users are not deleted yet; their stored originals would be left on disk. Delete them with DELETE /v1/files/{id} first. Nothing was changed."
    }
    if ($linked -gt 0) {
        Write-Warning "$linked user(s) are linked to a Telegram chat and are left alone."
    }

    $deleteSql = @'
BEGIN;
SET LOCAL app.allow_user_delete = 'on';
CREATE TEMP TABLE victims AS
  SELECT u.id FROM users u
   WHERE u.id::text LIKE '7e570000-0000-4000-8000-%'
     AND NOT EXISTS (SELECT 1 FROM telegram_users t WHERE t.user_id = u.id);
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM ingested_files f WHERE f.user_id IN (SELECT id FROM victims) AND f.deleted_at IS NULL) THEN
    RAISE EXCEPTION 'a test user still owns a file that is not deleted';
  END IF;
  IF EXISTS (SELECT 1 FROM victims WHERE id::text NOT LIKE '7e570000-0000-4000-8000-%') THEN
    RAISE EXCEPTION 'a user outside the test range is among the victims';
  END IF;
END $$;
WITH d AS (DELETE FROM users WHERE id IN (SELECT id FROM victims) RETURNING 1)
SELECT 'deleted|' || count(*) FROM d;
COMMIT;
SELECT 'left|' || count(*) FROM users;
'@
    $out = @(Invoke-DbSql -Service $Service -User $target.User -Database $target.Name -Sql $deleteSql)
    $deleted = ($out | Where-Object { $_ -like 'deleted|*' } | Select-Object -First 1) -replace '^deleted\|', ''
    $left = ($out | Where-Object { $_ -like 'left|*' } | Select-Object -First 1) -replace '^left\|', ''
    Write-Host "Deleted $deleted test user(s) and what cascaded from them; $left user(s) are left in '$($target.Name)'."
} finally {
    Pop-Location
}
