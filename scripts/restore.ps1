<#
.SYNOPSIS
Restores a pg_dump archive into a database (Windows counterpart of restore.sh).

.DESCRIPTION
This machine has no pg_restore on the host, so the archive is copied into the
docker compose database container and restored there.

The restore replaces everything in the target database (pg_restore --clean), so
the script asks you to type the database name before it starts. It also refuses
to run while other sessions are connected to the target: stop the backend
(main.exe / go run) first.

It runs pg_restore in one transaction (--single-transaction): either the whole
archive is restored or the database is left exactly as it was. The script exits
with pg_restore's exit code and prints "Restore complete." only when that code
is 0.

.PARAMETER BackupFile
The .dump file made by backup.ps1 / backup.sh.

.PARAMETER Database
Target database. Defaults to the one in DATABASE_URL (.env or the environment).

.PARAMETER ConfirmDatabase
Skip the prompt by passing the target database name again. Used by other
scripts; when you run this by hand just answer the prompt.

.PARAMETER Create
Create the target database first when it does not exist.

.PARAMETER Service
docker compose service name of the database. Defaults to db.

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\restore.ps1 "$env:USERPROFILE\go-monolith-backups\postgres_20261031_215229.dump"

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\restore.ps1 .\backups\postgres_20261031_215229.dump -Database app_test -Create
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory, Position = 0)][string]$BackupFile,
    [string]$Database = "",
    [string]$ConfirmDatabase = "",
    [switch]$Create,
    [string]$Service = "db"
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
. (Join-Path $PSScriptRoot "lib\db-container.ps1")

if (-not (Test-Path -LiteralPath $BackupFile -PathType Leaf)) {
    throw "Backup file '$BackupFile' not found."
}
$BackupFile = (Resolve-Path -LiteralPath $BackupFile).Path
$size = (Get-Item -LiteralPath $BackupFile).Length
if ($size -lt 1000) {
    throw "'$BackupFile' is only $size bytes; that is not a valid dump."
}
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw "docker is not available."
}

$target = Get-DatabaseTarget (Get-DatabaseUrl $repoRoot)
if (-not $Database) { $Database = $target.Name }
Assert-SafeDatabaseName $Database
$dbUser = $target.User

if (-not $ConfirmDatabase) {
    Write-Host "Restoring '$BackupFile' ($size bytes) will REPLACE everything in database '$Database'."
    $ConfirmDatabase = Read-Host "Type the database name to continue"
}
if ($ConfirmDatabase -ne $Database) {
    throw "Confirmation '$ConfirmDatabase' does not match the database '$Database'; nothing was changed."
}

Push-Location $repoRoot
$containerFile = "/tmp/go-monolith-restore-$(Get-Date -Format 'yyyyMMddHHmmssfff').dump"
try {
    $exists = @(Invoke-DbSql -Service $Service -User $dbUser -Database "postgres" `
        -Sql "SELECT 1 FROM pg_database WHERE datname = '$Database';")
    if ($exists.Count -eq 0) {
        if (-not $Create) {
            throw "Database '$Database' does not exist. Create it first or pass -Create."
        }
        Write-Host "Creating database '$Database' ..."
        Invoke-DbSql -Service $Service -User $dbUser -Database "postgres" -Sql "CREATE DATABASE $Database;" | Out-Null
    } else {
        $others = @(Invoke-DbSql -Service $Service -User $dbUser -Database "postgres" `
            -Sql "SELECT count(*) FROM pg_stat_activity WHERE datname = '$Database' AND pid <> pg_backend_pid();")
        if ([int]$others[0] -gt 0) {
            throw "$($others[0]) other session(s) are connected to '$Database'. Stop the backend (main.exe / go run) and anything else that uses it, then run this again."
        }
    }

    Write-Host "Copying the archive into the '$Service' container ..."
    & docker compose cp $BackupFile ("{0}:{1}" -f $Service, $containerFile)
    if ($LASTEXITCODE -ne 0) { throw "docker compose cp failed." }

    Write-Host "Restoring into '$Database' (one transaction; this can take minutes) ..."
    $command = 'pg_restore --clean --if-exists --single-transaction --no-owner --no-privileges -U {0} -d {1} {2}' -f `
        $dbUser, $Database, $containerFile
    $result = Invoke-DbDetached -Service $Service -Command $command

    if ($result.Log.Count -gt 0) {
        Write-Host "--- pg_restore output (last 40 lines) ---"
        $result.Log | Select-Object -Last 40 | ForEach-Object { Write-Host $_ }
        Write-Host "-----------------------------------------"
    }
    if ($result.ExitCode -ne 0) {
        [Console]::Error.WriteLine("pg_restore failed with exit code $($result.ExitCode); '$Database' was left as it was (single transaction).")
        exit $result.ExitCode
    }

    # pg_restore does not carry planner statistics over.
    Invoke-DbSql -Service $Service -User $dbUser -Database $Database -Sql "ANALYZE;" | Out-Null
    Write-Host "Restore complete: '$BackupFile' -> database '$Database'."
} finally {
    & docker compose exec -T $Service rm -f $containerFile
    Pop-Location
}
