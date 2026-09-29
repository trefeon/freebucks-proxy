# install-freebuff-proxy.ps1 - Backward compatibility wrapper for install.ps1
& (Join-Path $PSScriptRoot "install.ps1") @PSBoundParameters
