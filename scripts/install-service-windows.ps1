<# Compatibility wrapper forwarding to unified scripts\install-service.ps1 #>
& (Join-Path $PSScriptRoot "install-service.ps1") @args
