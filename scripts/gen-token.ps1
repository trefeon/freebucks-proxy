# gen-token.ps1 - Headless token generator alias
param(
    [switch]$Save,
    [switch]$ToClipboard,
    [switch]$Incognito,
    [switch]$Append,
    [switch]$Verify,
    [string]$EnvFile = "",
    [string]$BaseUrl = "https://freebuff.com",
    [int]$TimeoutSeconds = 300,
    [int]$PollIntervalMs = 5000
)

$scriptPath = Join-Path $PSScriptRoot "gen-freebuff-token.ps1"
& $scriptPath @PSBoundParameters
