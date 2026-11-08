<#
.SYNOPSIS
Writes the two files Alertmanager reads its Telegram settings from (alertmanager\secrets\).

.DESCRIPTION
The repository is public, so the bot token and the chat id are not in alertmanager.yml.
This script takes them from .env: TELEGRAM_BOT_TOKEN, and the first chat id in
TELEGRAM_ALLOWED_CHAT_IDS (or ALERT_TELEGRAM_CHAT_ID, if set, to send alerts to another chat).
It writes alertmanager\secrets\telegram_token and ...\telegram_chat_id (git ignores that folder)
and prints only the names and lengths, never the values. Run it once after cloning, and again if
the token is rotated; then `docker compose restart alertmanager`.
#>
$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$envFile = Join-Path $repoRoot ".env"
if (-not (Test-Path $envFile)) { throw "$envFile does not exist." }

$values = @{}
foreach ($line in Get-Content $envFile) {
    if ($line -match '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') {
        $values[$matches[1]] = $matches[2].Trim().Trim('"').Trim("'")
    }
}

$token = $values["TELEGRAM_BOT_TOKEN"]
if (-not $token) { throw "TELEGRAM_BOT_TOKEN is not set in .env." }
$chat = $values["ALERT_TELEGRAM_CHAT_ID"]
if (-not $chat) { $chat = @($values["TELEGRAM_ALLOWED_CHAT_IDS"] -split '[,; ]+' | Where-Object { $_ })[0] }  # @() keeps one id from being read as a string
if ($chat -notmatch '^-?\d+$') { throw "No numeric chat id: set TELEGRAM_ALLOWED_CHAT_IDS or ALERT_TELEGRAM_CHAT_ID in .env." }

$dir = Join-Path $repoRoot "alertmanager\secrets"
New-Item -ItemType Directory -Force $dir | Out-Null
# No trailing newline: Alertmanager would take it as part of the value.
[System.IO.File]::WriteAllText((Join-Path $dir "telegram_token"), $token)
[System.IO.File]::WriteAllText((Join-Path $dir "telegram_chat_id"), $chat)
Write-Output ("alertmanager\secrets\telegram_token: {0} characters" -f $token.Length)
Write-Output ("alertmanager\secrets\telegram_chat_id: {0} characters" -f $chat.Length)
