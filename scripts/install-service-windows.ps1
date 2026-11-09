<# Compatibility wrapper forwarding to unified scripts\install-service.ps1 #>
$ScriptRoot = if ($PSScriptRoot) { $PSScriptRoot } else { Split-Path -Parent $MyInvocation.MyCommand.Definition }
$global:LASTEXITCODE = 0
& (Join-Path $ScriptRoot "install-service.ps1") @args
if ($LASTEXITCODE -ne 0) {
  exit $LASTEXITCODE
}
