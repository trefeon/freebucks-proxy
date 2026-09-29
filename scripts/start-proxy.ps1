# start-proxy.ps1 - Launch freebuff-proxy from the install root or repo.
# Right-click this folder -> "Open in Terminal" -> .\start-proxy.cmd
# (or double-click start-proxy.cmd - it bypasses the execution policy)
param(
  [string]$EnvFile = ""   # explicit .env path (advanced; default: %APPDATA%\freebuff-proxy\.env, then .env next to the exe)
)
$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
# Per-user paths; fall back to home-based paths when the env vars are unset
# (the same fallback the runtime uses in backend/internal/config).
$localAppData = $env:LOCALAPPDATA
if (-not $localAppData) { $localAppData = Join-Path $env:USERPROFILE "AppData\Local" }
$appData = $env:APPDATA
if (-not $appData) { $appData = Join-Path $env:USERPROFILE "AppData\Roaming" }

# --- 1. locate the binary ----------------------------------------------------
# Installed per-user location first (%LOCALAPPDATA%\Programs\freebuff-proxy),
# then next to this script (dev checkout / extracted release folder).
$exe = Join-Path (Join-Path $localAppData "Programs\freebuff-proxy") "freebuff-proxy.exe"
if (-not (Test-Path -LiteralPath $exe)) {
    $exe = Join-Path $root "freebuff-proxy.exe"
    if (-not (Test-Path -LiteralPath $exe)) {
        $parentExe = Join-Path (Split-Path -Parent $root) "freebuff-proxy.exe"
        if (Test-Path -LiteralPath $parentExe) {
            $root = Split-Path -Parent $root
            $exe = $parentExe
        }
    }
}

if (-not (Test-Path -LiteralPath $exe)) {
    Write-Host "freebuff-proxy.exe not found." -ForegroundColor Red
    Write-Host "  Expected:  $localAppData\Programs\freebuff-proxy\freebuff-proxy.exe (run the installer: scripts\install.ps1 or install.cmd)" -ForegroundColor Yellow
    Write-Host "  or next to this script (dev checkout)." -ForegroundColor Yellow
    exit 1
}
$exeDir = Split-Path -Parent $exe

# --- 2. locate the config file ----------------------------------------------
# The runtime resolves .env per-platform: %APPDATA%\freebuff-proxy\.env (or
# ./.env in the working directory, which wins). We never create or copy .env
# here - install.ps1 owns that. Just resolve the path for the token check, the
# banner, and the token-generator offer. Fall back to .env next to the exe
# (legacy/dev layout).
$envFile = $EnvFile
if (-not $envFile) {
    $platformEnv = Join-Path (Join-Path $appData "freebuff-proxy") ".env"
    if (Test-Path -LiteralPath $platformEnv) {
        $envFile = $platformEnv
    } else {
        $legacyEnv = Join-Path $exeDir ".env"
        if (Test-Path -LiteralPath $legacyEnv) { $envFile = $legacyEnv }
    }
}
if (-not $envFile) {
    Write-Host "No .env config found (expected $appData\freebuff-proxy\.env or a .env next to the exe)." -ForegroundColor Yellow
    Write-Host "Run scripts\install.ps1 to set up the proxy, or pass -EnvFile <path> to this script." -ForegroundColor Yellow
    Write-Host "Starting with runtime defaults (bridge mode, LISTEN_ADDR=127.0.0.1:3457)." -ForegroundColor Yellow
} elseif ($EnvFile -and -not (Test-Path -LiteralPath $EnvFile)) {
    Write-Host "Env file not found: $EnvFile (starting with runtime defaults)." -ForegroundColor Yellow
}

# --- 3. If no token, offer to generate one (skipped when piped/CI) -----------
if ($envFile -and (Test-Path -LiteralPath $envFile)) {
    $envText = [System.IO.File]::ReadAllText($envFile, [System.Text.Encoding]::UTF8)
    if ($envText -notmatch '(?m)^AUTH_TOKENS=\S') {
        $genCandidates = @(
            (Join-Path $root "gen-token.ps1"),
            (Join-Path (Join-Path $root "scripts") "gen-token.ps1"),
            (Join-Path $root "gen-freebuff-token.ps1"),
            (Join-Path (Join-Path $root "scripts") "gen-freebuff-token.ps1"),
            (Join-Path $exeDir "gen-token.ps1"),
            (Join-Path (Join-Path $exeDir "scripts") "gen-token.ps1"),
            (Join-Path $exeDir "gen-freebuff-token.ps1"),
            (Join-Path (Join-Path $exeDir "scripts") "gen-freebuff-token.ps1")
        )
        $genScript = ""
        foreach ($c in $genCandidates) { if (Test-Path -LiteralPath $c) { $genScript = $c; break } }
        if ($genScript) {
            if ([Console]::IsInputRedirected) {
                Write-Host "  No token in AUTH_TOKENS - running in bridge mode (clients send their own token)." -ForegroundColor Yellow
            } else {
                Write-Host "No token found in the config" -ForegroundColor Yellow
                $ans = Read-Host "Generate one now via browser login? [Y/n]"
                if ($ans -notmatch '^(n|no)$') {
                    & $genScript -Append -EnvFile $envFile
                } else {
                    Write-Host "  Skipped; running in bridge mode (clients send their own token)." -ForegroundColor Yellow
                }
            }
        }
    }
}

# --- 4. Banner with the real listen address ---------------------------------
$addr = "127.0.0.1:3457"
if ($envFile -and (Test-Path -LiteralPath $envFile)) {
    $line = [System.IO.File]::ReadAllText($envFile, [System.Text.Encoding]::UTF8) -split "`r?`n" |
        Where-Object { $_ -match '^LISTEN_ADDR=' } | Select-Object -First 1
    if ($line) { $addr = ($line -split '=', 2)[1].Trim() }
}
$base = "http://$addr"
Write-Host ""
Write-Host "Starting freebuff-proxy from $exeDir" -ForegroundColor Cyan
if ($envFile) { Write-Host "  Config:       $envFile" -ForegroundColor DarkGray }
Write-Host "  OpenAI API:  $base/v1" -ForegroundColor Green
Write-Host "  Health:      $base/healthz" -ForegroundColor Green
Write-Host "  Stop:        Ctrl+C" -ForegroundColor Green
Write-Host ""

& $exe
$code = $LASTEXITCODE
if ($code -ne 0) {
    Write-Host "freebuff-proxy exited with code $code" -ForegroundColor Red
    exit $code
}
