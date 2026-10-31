<#
.SYNOPSIS
Proves that the newest backup can be restored, by restoring it into a scratch
database and comparing row counts with the live database.

.DESCRIPTION
A backup nobody has restored is a hope, not a backup. This script restores the
newest full dump into a throw-away database (verify_restore_test, created and
dropped by the script; the live database is only read) and compares the number
of rows of every table in the public schema with the live database.

Exit code 0: the restore worked and every count matches.
River's leader-election row (river_leader) is a lease that comes and goes, so it
is shown but never compared.

Exit code 2: the restore worked but some counts differ. That is expected when
the dump is older than the live data; run with -Fresh to dump first.
Any other non-zero code: the restore itself failed.

.PARAMETER BackupFile
Verify this dump instead of the newest full one in BackupDir.

.PARAMETER BackupDir
Where the dumps are. Defaults to %USERPROFILE%\go-monolith-backups.

.PARAMETER Fresh
Take a new full dump first (backup.ps1 -SkipStorage -Keep 0), then verify that
one. Counts then match unless the backend writes during the run.

.PARAMETER KeepDatabase
Do not drop the scratch database afterwards.

.PARAMETER Service
docker compose service name of the database. Defaults to db.

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\verify-restore.ps1 -Fresh
#>
[CmdletBinding()]
param(
    [string]$BackupFile = "",
    [string]$BackupDir = "",
    [switch]$Fresh,
    [switch]$KeepDatabase,
    [string]$Service = "db"
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
. (Join-Path $PSScriptRoot "lib\db-container.ps1")

$scratchDb = "verify_restore_test"
$target = Get-DatabaseTarget (Get-DatabaseUrl $repoRoot)
$liveDb = $target.Name
$dbUser = $target.User

if (-not $BackupDir) {
    $BackupDir = Join-Path $env:USERPROFILE "go-monolith-backups"
}

if ($Fresh) {
    & (Join-Path $PSScriptRoot "backup.ps1") -BackupDir $BackupDir -SkipStorage -Keep 0
}

if (-not $BackupFile) {
    $fullDump = '^' + [regex]::Escape($liveDb) + '_\d{8}_\d{6}\.dump$'
    $newest = Get-ChildItem -Path $BackupDir -Filter "*.dump" -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -match $fullDump } |
        Sort-Object LastWriteTime -Descending |
        Select-Object -First 1
    if (-not $newest) {
        throw "No full dump of '$liveDb' found in $BackupDir."
    }
    $BackupFile = $newest.FullName
}
if (-not (Test-Path -LiteralPath $BackupFile -PathType Leaf)) {
    throw "Backup file '$BackupFile' not found."
}
$cacheIsEmpty = (Split-Path -Leaf $BackupFile) -like "*_nocache.dump"

Write-Host "Verifying $BackupFile"
Write-Host "  live database:    $liveDb (read only)"
Write-Host "  scratch database: $scratchDb (dropped afterwards)"

Push-Location $repoRoot
$failed = $false
try {
    Invoke-DbSql -Service $Service -User $dbUser -Database "postgres" `
        -Sql "SET client_min_messages = warning; DROP DATABASE IF EXISTS $scratchDb WITH (FORCE);" | Out-Null

    & (Join-Path $PSScriptRoot "restore.ps1") -BackupFile $BackupFile -Database $scratchDb `
        -ConfirmDatabase $scratchDb -Create -Service $Service
    if ($LASTEXITCODE -ne 0) {
        $code = $LASTEXITCODE
        [Console]::Error.WriteLine("The restore failed (exit code $code).")
        exit $code
    }

    $live = Get-TableCounts -Service $Service -User $dbUser -Database $liveDb
    $restored = Get-TableCounts -Service $Service -User $dbUser -Database $scratchDb

    $names = @($live.Keys) + @($restored.Keys) | Select-Object -Unique | Sort-Object
    $rows = foreach ($name in $names) {
        $l = $live[$name]
        $r = $restored[$name]
        $status = "ok"
        if ($null -eq $l) { $status = "only in the dump" }
        elseif ($null -eq $r) { $status = "MISSING in the restore" }
        elseif ($cacheIsEmpty -and $name -eq "embedding_cache") { $status = "skipped (nocache dump)" }
        elseif ($name -eq "river_leader") { $status = "skipped (leader lease)" }
        elseif ($l -ne $r) { $status = "DIFFERENT" }
        [pscustomobject]@{ Table = $name; Live = $l; Restored = $r; Status = $status }
    }
    $rows | Format-Table -AutoSize | Out-String | Write-Host

    $bad = @($rows | Where-Object { $_.Status -ne "ok" -and $_.Status -notlike "skipped*" })
    if ($bad.Count -gt 0) {
        $failed = $true
        [Console]::Error.WriteLine("$($bad.Count) table(s) differ between the live database and the restored dump. If the dump is older than the live data this is expected: run again with -Fresh.")
    } else {
        Write-Host "Restore verified: every table has the same number of rows as the live database."
    }
} finally {
    if (-not $KeepDatabase) {
        try {
            Invoke-DbSql -Service $Service -User $dbUser -Database "postgres" `
                -Sql "SET client_min_messages = warning; DROP DATABASE IF EXISTS $scratchDb WITH (FORCE);" | Out-Null
        } catch {
            [Console]::Error.WriteLine("Could not drop the scratch database ${scratchDb}: $($_.Exception.Message)")
        }
    }
    Pop-Location
}
if ($failed) { exit 2 }
