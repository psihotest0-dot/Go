# Shared helpers for restore.ps1 and verify-restore.ps1 (dot-source this file).
#
# They talk to the database through the docker compose "db" container, because
# this machine has no PostgreSQL client tools on the host (see backup.ps1).
# Keep this file ASCII-only: Windows PowerShell 5.1 reads BOM-less files as ANSI.

function Get-DatabaseUrl {
    param([string]$RepoRoot)

    if ($env:DATABASE_URL) { return $env:DATABASE_URL }

    $envFile = Join-Path $RepoRoot ".env"
    if (Test-Path $envFile) {
        foreach ($line in Get-Content $envFile) {
            if ($line -match '^\s*DATABASE_URL\s*=\s*(.*)$') {
                return $matches[1].Trim().Trim('"').Trim("'")
            }
        }
    }

    # Mirror internal/config.Load: the application itself falls back to this URL.
    return "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
}

function Get-DatabaseTarget {
    param([string]$Url)

    $name = "postgres"
    $user = "postgres"
    if ($Url -match '://([^:@/]+)') { $user = $matches[1] }
    if ($Url -match '://[^/]+/([^?]+)') { $name = $matches[1] }
    return [pscustomobject]@{ Name = $name; User = $user }
}

# Database names end up in SQL and in a shell command inside the container.
function Assert-SafeDatabaseName {
    param([string]$Name)

    if ($Name -notmatch '^[A-Za-z][A-Za-z0-9_]{0,62}$') {
        throw "Refusing database name '$Name': use letters, digits and underscores only."
    }
}

# Runs SQL (given on stdin) with psql inside the container and returns stdout
# lines, unaligned and without headers: fields are separated by '|'. Throws when
# psql fails; its own error text is already on the console (stderr).
function Invoke-DbSql {
    param(
        [Parameter(Mandatory)][string]$Service,
        [Parameter(Mandatory)][string]$User,
        [Parameter(Mandatory)][string]$Database,
        [Parameter(Mandatory)][string]$Sql
    )

    $out = $Sql | & docker compose exec -T $Service psql -U $User -d $Database -X -A -t -F '|' -v ON_ERROR_STOP=1 -f -
    if ($LASTEXITCODE -ne 0) {
        throw "psql failed with exit code $LASTEXITCODE (database '$Database')."
    }
    # Callers wrap the call in @(...): PowerShell unrolls a one-element result.
    return @($out | Where-Object { "$_".Length -gt 0 })
}

# Runs a shell command inside the container, detached, and waits for it.
#
# A plain `docker compose exec` can block for ever after a long command has
# already finished (reproduced from Task Scheduler and from a redirected host
# stdout - see backup.ps1). The command therefore runs detached, writes its
# output and exit code to files, and this function polls for the exit code.
# Returns an object with ExitCode and Log (the command's stdout+stderr).
function Invoke-DbDetached {
    param(
        [Parameter(Mandatory)][string]$Service,
        [Parameter(Mandatory)][string]$Command,
        [int]$TimeoutMinutes = 120
    )

    $stamp = Get-Date -Format "yyyyMMddHHmmssfff"
    $logFile = "/tmp/go-monolith-$stamp.log"
    $exitFile = "/tmp/go-monolith-$stamp.exit"
    # The command must not contain double quotes: Windows PowerShell 5.1 does not
    # escape them when it starts docker.exe.
    $wrapped = '{0} > {1} 2>&1; echo $? > {2}' -f $Command, $logFile, $exitFile

    try {
        & docker compose exec -d $Service sh -c $wrapped
        if ($LASTEXITCODE -ne 0) {
            throw "Could not start the command inside the '$Service' container."
        }

        $exitCode = $null
        $deadline = (Get-Date).AddMinutes($TimeoutMinutes)
        while ((Get-Date) -lt $deadline) {
            Start-Sleep -Seconds 2
            & docker compose exec -T $Service sh -c "test -f $exitFile"
            if ($LASTEXITCODE -ne 0) { continue }
            $recorded = & docker compose exec -T $Service cat $exitFile
            if ($LASTEXITCODE -eq 0 -and $recorded) {
                $exitCode = [int](($recorded -join "").Trim())
                break
            }
        }
        if ($null -eq $exitCode) {
            throw "The command inside the '$Service' container did not finish within $TimeoutMinutes minutes."
        }

        $log = & docker compose exec -T $Service cat $logFile
        return [pscustomobject]@{ ExitCode = $exitCode; Log = @($log | Where-Object { $null -ne $_ }) }
    } finally {
        # Best effort; leftovers in /tmp only waste container space.
        & docker compose exec -T $Service rm -f $logFile $exitFile
    }
}

# Exact row counts of every table in the public schema: table name -> rows.
# (query_to_xml runs one count(*) per table; no table list is hard-coded.)
function Get-TableCounts {
    param(
        [Parameter(Mandatory)][string]$Service,
        [Parameter(Mandatory)][string]$User,
        [Parameter(Mandatory)][string]$Database
    )

    $sql = @'
SELECT table_name,
       (xpath('/row/c/text()', query_to_xml(format('SELECT count(*) AS c FROM %I.%I', table_schema, table_name), false, true, '')))[1]::text
FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
ORDER BY 1;
'@
    $counts = [ordered]@{}
    foreach ($line in @(Invoke-DbSql -Service $Service -User $User -Database $Database -Sql $sql)) {
        $name, $rows = $line -split '\|', 2
        $counts[$name] = [int64]$rows
    }
    return $counts
}
