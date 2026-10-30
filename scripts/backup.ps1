<#
.SYNOPSIS
Creates a compressed pg_dump archive of the database (Windows front-end).

.DESCRIPTION
The 2026-10-30 incident left nothing to restore from because no dump had ever
been taken (see docs/incident-2026-10-30/REPORT.md). This script is the Windows
counterpart of scripts/backup.sh:

* it uses a locally installed pg_dump when there is one, otherwise it runs
  pg_dump inside the docker compose database container, whose client always
  matches the server version (this machine has no pg_dump on the host);
* it reads DATABASE_URL from the environment or from the repository .env;
* it refuses to keep a dump smaller than 1000 bytes, so a broken run cannot
  look like a valid backup;
* it prunes old dumps so the directory does not grow forever.

.PARAMETER BackupDir
Where the dumps go. Defaults to <repo>\backups.

.PARAMETER Keep
How many dumps to keep, oldest removed first. Defaults to 14. 0 disables pruning.

.PARAMETER Service
docker compose service name of the database. Defaults to db.

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\backup.ps1 -BackupDir D:\backups\go-monolith -Keep 30
#>
[CmdletBinding()]
param(
    [string]$BackupDir = "",
    [int]$Keep = 14,
    [string]$Service = "db"
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot

# Load .env when DATABASE_URL is not already exported.
if (-not $env:DATABASE_URL) {
    $envFile = Join-Path $repoRoot ".env"
    if (-not (Test-Path $envFile)) {
        throw "DATABASE_URL is not set and $envFile does not exist."
    }
    foreach ($line in Get-Content $envFile) {
        if ($line -match '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') {
            $name = $matches[1]
            $value = $matches[2].Trim().Trim('"').Trim("'")
            if (-not (Test-Path "Env:$name")) {
                Set-Item -Path "Env:$name" -Value $value
            }
        }
    }
}
if (-not $env:DATABASE_URL) {
    # Mirror internal/config.Load: the application itself falls back to this URL.
    $env:DATABASE_URL = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
    Write-Warning "DATABASE_URL is not set in the environment or .env; using the application default ($env:DATABASE_URL)."
}

$dbName = "db"
if ($env:DATABASE_URL -match '://[^/]+/([^?]+)') { $dbName = $matches[1] }
$dbUser = "postgres"
if ($env:DATABASE_URL -match '://([^:@/]+)') { $dbUser = $matches[1] }

if (-not $BackupDir) {
    $BackupDir = Join-Path $repoRoot "backups"
}
New-Item -ItemType Directory -Force -Path $BackupDir | Out-Null

$backupFile = Join-Path $BackupDir ("{0}_{1}.dump" -f $dbName, (Get-Date -Format "yyyyMMdd_HHmmss"))

$pgDump = Get-Command pg_dump -ErrorAction SilentlyContinue
if ($pgDump) {
    Write-Host "Backing up to $backupFile with $($pgDump.Source) ..."
    & $pgDump.Source -Fc --no-owner --no-privileges $env:DATABASE_URL -f $backupFile
    if ($LASTEXITCODE -ne 0) {
        throw "pg_dump failed with exit code $LASTEXITCODE."
    }
} else {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        throw "Neither pg_dump nor docker is available: install the PostgreSQL client tools or Docker."
    }

    # pg_dump writes inside the container and the finished archive is copied out.
    #
    # The exec is detached on purpose: waiting for a long running
    # `docker compose exec` can block for ever after pg_dump has already
    # finished (reproduced from Task Scheduler and from a redirected host
    # stdout), and streaming the archive through the CLI's stdout then leaves a
    # zero byte file behind. The detached shell writes its exit code to a
    # sentinel file, which the script polls; short commands do not hang.
    $stamp = Get-Date -Format "yyyyMMddHHmmss"
    $containerFile = "/tmp/go-monolith-$stamp.dump"
    $containerExit = "/tmp/go-monolith-$stamp.exit"
    $dumpScript = 'pg_dump -Fc --no-owner --no-privileges -U {0} -d {1} -f {2}; echo $? > {3}' -f `
        $dbUser, $dbName, $containerFile, $containerExit

    Write-Host "No local pg_dump; dumping inside the '$Service' container ..."
    Push-Location $repoRoot
    try {
        & docker compose exec -d $Service sh -c $dumpScript
        if ($LASTEXITCODE -ne 0) {
            throw "Could not start pg_dump inside the container."
        }

        $exitCode = $null
        $deadline = (Get-Date).AddMinutes(30)
        while ((Get-Date) -lt $deadline) {
            Start-Sleep -Seconds 5
            & docker compose exec -T $Service sh -c "test -f $containerExit"
            if ($LASTEXITCODE -ne 0) {
                continue
            }
            $recorded = & docker compose exec -T $Service cat $containerExit
            if ($LASTEXITCODE -eq 0 -and $recorded) {
                $exitCode = [int](($recorded -join "").Trim())
                break
            }
        }

        if ($null -eq $exitCode) {
            throw "pg_dump inside the container did not finish within 30 minutes."
        }
        if ($exitCode -ne 0) {
            throw "pg_dump inside the container failed with exit code $exitCode."
        }

        Write-Host "Copying $containerFile to $backupFile ..."
        & docker compose cp ("{0}:{1}" -f $Service, $containerFile) $backupFile
        if ($LASTEXITCODE -ne 0) {
            throw "docker compose cp failed."
        }
    } finally {
        # Best effort; a leftover file in /tmp only wastes container space.
        & docker compose exec -T $Service rm -f $containerFile $containerExit
        Pop-Location
    }
}

$size = (Get-Item $backupFile).Length
if ($size -lt 1000) {
    Remove-Item $backupFile -Force
    throw "The dump was only $size bytes; refusing to keep it."
}
Write-Host "Backup complete: $backupFile ($size bytes)"

if ($Keep -gt 0) {
    $old = Get-ChildItem -Path $BackupDir -Filter ("{0}_*.dump" -f $dbName) |
        Sort-Object LastWriteTime -Descending |
        Select-Object -Skip $Keep
    foreach ($file in $old) {
        Write-Host "Pruning old backup: $($file.FullName)"
        Remove-Item $file.FullName -Force
    }
}
