<#
.SYNOPSIS
Registers (or removes) the Windows Scheduled Task that dumps the database daily.

.DESCRIPTION
The 2026-10-30 incident left nothing to restore from: no dump had ever been
taken, so the deleted rows and vectors were unrecoverable (see
docs/incident-2026-10-30/REPORT.md). This script creates the missing piece - a
daily, unattended backup run.

The task starts scripts/backup.ps1 with powershell.exe, so it needs neither bash
nor a PostgreSQL client on the host: backup.ps1 falls back to pg_dump inside the
docker compose database container. Dumps land outside the repository by default
(%USERPROFILE%\go-monolith-backups) so a `git clean` or an accident in the
project folder cannot take them.

.PARAMETER TaskName
Name of the scheduled task. Defaults to go-monolith-backup.

.PARAMETER Time
Daily start time, 24h "HH:mm". Defaults to 03:00.

.PARAMETER BackupDir
Directory for the dumps. Defaults to %USERPROFILE%\go-monolith-backups.

.PARAMETER Keep
How many dumps to keep. Defaults to 14.

.PARAMETER Remove
Unregisters the task instead of creating it.

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\backup-task.ps1

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\backup-task.ps1 -Time 02:30 -Keep 30

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\backup-task.ps1 -Remove
#>
[CmdletBinding()]
param(
    [string]$TaskName = "go-monolith-backup",
    [string]$Time = "03:00",
    [string]$BackupDir = "",
    [int]$Keep = 14,
    [switch]$Remove
)

$ErrorActionPreference = "Stop"

if ($Remove) {
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
        Write-Host "Removed scheduled task '$TaskName'."
    } else {
        Write-Host "Scheduled task '$TaskName' does not exist."
    }
    return
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$envFile = Join-Path $repoRoot ".env"
$backupScript = Join-Path $repoRoot "scripts\backup.ps1"

if (-not (Test-Path $envFile)) {
    throw ".env not found at $envFile; it must define DATABASE_URL."
}
if (-not (Test-Path $backupScript)) {
    throw "backup script not found at $backupScript."
}

if (-not $BackupDir) {
    $BackupDir = Join-Path $env:USERPROFILE "go-monolith-backups"
}

$arguments = '-NoProfile -ExecutionPolicy Bypass -File "{0}" -BackupDir "{1}" -Keep {2}' -f `
    $backupScript, $BackupDir, $Keep

$action = New-ScheduledTaskAction `
    -Execute (Join-Path $env:WINDIR "System32\WindowsPowerShell\v1.0\powershell.exe") `
    -Argument $arguments `
    -WorkingDirectory $repoRoot
$trigger = New-ScheduledTaskTrigger -Daily -At $Time
$settings = New-ScheduledTaskSettingsSet `
    -StartWhenAvailable `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit (New-TimeSpan -Hours 1)

Register-ScheduledTask `
    -TaskName $TaskName `
    -Action $action `
    -Trigger $trigger `
    -Settings $settings `
    -Description "Daily pg_dump backup of the go-monolith database (see docs/incident-2026-10-30)." `
    -Force | Out-Null

Write-Host "Registered scheduled task '$TaskName':"
Write-Host "  action:   powershell.exe $arguments"
Write-Host "  schedule: daily at $Time"
Write-Host "  dumps:    $BackupDir (keep $Keep)"
Write-Host ""
Write-Host "Run it once now to prove it works:"
Write-Host "  Start-ScheduledTask -TaskName '$TaskName'"
Write-Host "  Get-ScheduledTaskInfo -TaskName '$TaskName'   # LastTaskResult must be 0"
