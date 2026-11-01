<#
.SYNOPSIS
Builds the backend binary (main.exe) from the repository root.

.DESCRIPTION
main.exe is a build artifact: it is listed in .gitignore (*.exe) and was removed
from the git history (task 2 of the project plan). Run this script instead of
committing the binary.

.PARAMETER Output
Name of the binary, relative to the repository root. Defaults to main.exe.

.EXAMPLE
powershell -ExecutionPolicy Bypass -File scripts\build.ps1
#>
[CmdletBinding()]
param(
    [string]$Output = "main.exe"
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot

Push-Location $repoRoot
try {
    Write-Host "Building $Output ..."
    & go build -o $Output .
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE."
    }
    $size = (Get-Item (Join-Path $repoRoot $Output)).Length
    Write-Host "Build complete: $(Join-Path $repoRoot $Output) ($size bytes)"
} finally {
    Pop-Location
}
