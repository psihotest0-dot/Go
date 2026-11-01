<#
.SYNOPSIS
Creates a compressed pg_dump archive of the database (Windows front-end).

.DESCRIPTION
The 2026-10-30 incident left nothing to restore from because no dump had ever
been taken (the 2026-10-30 incident). This script is the Windows
counterpart of scripts/backup.sh:

* it uses a locally installed pg_dump when there is one, otherwise it runs
  pg_dump inside the docker compose database container, whose client always
  matches the server version (this machine has no pg_dump on the host);
* it reads DATABASE_URL from the environment or from the repository .env;
* it refuses to keep a dump smaller than 1000 bytes, so a broken run cannot
  look like a valid backup;
* it prunes old dumps so the directory does not grow forever;
* it also copies the uploaded originals (storage\) next to the dumps, because the
  database only holds extracted text and vectors: the files are the only source
  for a re-ingest (task 18 of the project plan).

.PARAMETER BackupDir
Where the dumps go. Defaults to <repo>\backups.

.PARAMETER Keep
How many dumps to keep, oldest removed first. Defaults to 14. 0 disables pruning.

.PARAMETER Service
docker compose service name of the database. Defaults to db.

.PARAMETER ExcludeCache
Leave the rows of embedding_cache out of the dump (--exclude-table-data). That
table is ~80 % of a full dump but can be rebuilt; keeping it makes a re-ingest
of the same files fast, so keep at least one full dump. Such dumps are named
<db>_<stamp>_nocache.dump and are pruned separately from full ones, so a nocache
run can never delete your last full dump (and the other way round).

.PARAMETER SkipStorage
Do not copy the storage directory.

.PARAMETER StorageDir
The uploaded files. Defaults to <repo>\storage (the backend stores uploads in
./storage relative to its working directory). They are copied additively to
<BackupDir>\storage with robocopy /E: nothing is ever deleted from the copy.

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\backup.ps1 -BackupDir D:\backups\go-monolith -Keep 30

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\backup.ps1 -ExcludeCache
#>
[CmdletBinding()]
param(
    [string]$BackupDir = "",
    [int]$Keep = 14,
    [string]$Service = "db",
    [switch]$ExcludeCache,
    [switch]$SkipStorage,
    [string]$StorageDir = ""
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

$suffix = ""
$dumpArgs = @("-Fc", "--no-owner", "--no-privileges")
if ($ExcludeCache) {
    $suffix = "_nocache"
    $dumpArgs += "--exclude-table-data=embedding_cache"
}

$backupFile = Join-Path $BackupDir ("{0}_{1}{2}.dump" -f $dbName, (Get-Date -Format "yyyyMMdd_HHmmss"), $suffix)

$pgDump = Get-Command pg_dump -ErrorAction SilentlyContinue
if ($pgDump) {
    Write-Host "Backing up to $backupFile with $($pgDump.Source) ..."
    & $pgDump.Source @dumpArgs $env:DATABASE_URL -f $backupFile
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
    $dumpScript = 'pg_dump {4} -U {0} -d {1} -f {2}; echo $? > {3}' -f `
        $dbUser, $dbName, $containerFile, $containerExit, ($dumpArgs -join " ")

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

# The dump comes first and the copy second on purpose: uploads are only ever
# added, so every file the dump refers to is already in the copy.
if (-not $SkipStorage) {
    if (-not $StorageDir) {
        $StorageDir = Join-Path $repoRoot "storage"
    }
    if (Test-Path -LiteralPath $StorageDir -PathType Container) {
        $storageCopy = Join-Path $BackupDir "storage"
        Write-Host "Copying uploaded files $StorageDir -> $storageCopy ..."
        # /E copies sub-directories and never deletes anything in the target
        # (no /MIR, no /PURGE). Exit codes below 8 mean success.
        & robocopy $StorageDir $storageCopy /E /COPY:DAT /DCOPY:T /R:2 /W:2 /NFL /NDL /NJH /NJS /NP | Out-Null
        if ($LASTEXITCODE -ge 8) {
            throw "robocopy failed with exit code $LASTEXITCODE while copying $StorageDir."
        }
        $copied = Get-ChildItem -LiteralPath $storageCopy -Recurse -File -ErrorAction SilentlyContinue |
            Measure-Object -Property Length -Sum
        Write-Host ("Storage copy complete: {0} files, {1} bytes in {2}" -f $copied.Count, [int64]$copied.Sum, $storageCopy)
    } else {
        Write-Warning "Storage directory $StorageDir does not exist; the uploaded files were not copied."
    }
}

if ($Keep -gt 0) {
    # Full and -ExcludeCache dumps are pruned separately: a run of one kind must
    # never delete the last dump of the other.
    $namePattern = '^' + [regex]::Escape($dbName) + '_\d{8}_\d{6}' + $suffix + '\.dump$'
    $old = Get-ChildItem -Path $BackupDir -Filter ("{0}_*.dump" -f $dbName) |
        Where-Object { $_.Name -match $namePattern } |
        Sort-Object LastWriteTime -Descending |
        Select-Object -Skip $Keep
    foreach ($file in $old) {
        Write-Host "Pruning old backup: $($file.FullName)"
        Remove-Item $file.FullName -Force
    }
}
