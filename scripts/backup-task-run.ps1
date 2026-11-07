<#
.SYNOPSIS
Runs scripts/backup.ps1 for the scheduled task and keeps everything it prints in a log file.

.DESCRIPTION
Registered by scripts/backup-task.ps1. On 2026-11-07 the task (started late, because
the PC was off at 03:00) opened a visible PowerShell window that closed by itself, so
nobody could read what it said, and a failed run would have left no trace at all
(powershell.exe without a log). This wrapper fixes the second half of that: every
line of backup.ps1 - its own messages, docker's progress lines, errors - goes to
<BackupDir>\backup-task.log with a time stamp, plus a "finished OK" or "FAILED with
exit code N" line at the end. The first half (no window) is the job of the task
action, see backup-task.ps1.

The log is rotated once it is larger than 1 MB: the old one becomes
backup-task.log.old (one generation only).

The exit code is the one of backup.ps1, so Task Scheduler's LastTaskResult stays
meaningful (0 = a good dump).

.PARAMETER BackupDir
Where the dumps go (also where the log goes).

.PARAMETER Keep
How many dumps to keep, passed on to backup.ps1.

.PARAMETER Script
The script to run. Defaults to backup.ps1 next to this file; a different one is only
for testing the wrapper itself.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$BackupDir,
    [int]$Keep = 14,
    [string]$Script = ""
)

if (-not $Script) {
    $Script = Join-Path $PSScriptRoot "backup.ps1"
}

New-Item -ItemType Directory -Force -Path $BackupDir | Out-Null
$log = Join-Path $BackupDir "backup-task.log"
if ((Test-Path -LiteralPath $log) -and ((Get-Item -LiteralPath $log).Length -gt 1MB)) {
    Move-Item -LiteralPath $log -Destination "$log.old" -Force
}

function Write-Log([string]$Text) {
    Add-Content -LiteralPath $log -Encoding UTF8 -Value ("{0} {1}" -f (Get-Date -Format "yyyy-MM-dd HH:mm:ss"), $Text)
}

$powershell = Join-Path $env:WINDIR "System32\WindowsPowerShell\v1.0\powershell.exe"

Write-Log "start: $Script -BackupDir $BackupDir -Keep $Keep"

# Windows PowerShell 5.1 turns the stderr lines of a native program into error records
# when they are merged with 2>&1; with "Stop" the first such line (docker prints its
# progress there) would abort this wrapper. "Continue" keeps them as plain text.
$ErrorActionPreference = "Continue"
& $powershell -NoProfile -ExecutionPolicy Bypass -File $Script -BackupDir $BackupDir -Keep $Keep 2>&1 |
    ForEach-Object {
        $line = "$_".TrimEnd()
        if ($line) { Write-Log $line }
    }
$code = $LASTEXITCODE

if ($code -eq 0) {
    Write-Log "finished OK"
} else {
    Write-Log "FAILED with exit code $code"
}
exit $code
