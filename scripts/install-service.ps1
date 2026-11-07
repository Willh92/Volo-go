<#
.SYNOPSIS
  Volo-go Windows Auto-Start Service Installer & Online Updater
  Repository: https://github.com/Willh92/Volo-go
  License: Apache-2.0

.DESCRIPTION
  Installs volo.exe under %ProgramFiles%\Volo, stores runtime configuration & data under
  %ProgramData%\Volo (locked down with NTFS ACLs via icacls), and registers an auto-start
  background service (NSSM when present on PATH, otherwise a zero-dependency Scheduled Task).

.EXAMPLE
  # Install auto-start service (uses local binary if present, otherwise auto-downloads from GitHub Releases)
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Install -PublicUrl "https://blog.example.com"

  # Zero-flag online upgrade (or -Action Update): downloads latest release, verifies SHA-256, smoke-tests, and restarts
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade

  # Manage service lifecycle
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Status
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Restart
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Uninstall
#>

param(
  [ValidateSet("Install", "Upgrade", "Update", "Uninstall", "Start", "Stop", "Restart", "Status")]
  [string]$Action = "Install",
  [string]$BinaryPath = "",
  [string]$ArchivePath = "",
  [string]$InstallDir = "",
  [switch]$Download,
  [switch]$FromGitHub,
  [string]$Version = $env:VOLO_VERSION,
  [ValidateSet("release", "preview")]
  [string]$Channel = $(if ($env:VOLO_CHANNEL) { $env:VOLO_CHANNEL } else { "release" }),
  [string]$Mirror = $env:VOLO_MIRROR,
  [string]$GitHubProxy = "",
  [string]$Repo = $(if ($env:VOLO_REPO) { $env:VOLO_REPO } else { "Willh92/Volo-go" }),
  [switch]$NoVerify,
  [switch]$SkipVerify,
  [string]$HostAddr = "127.0.0.1",
  [int]$Port = 8080,
  [string]$ListenAddr = "",
  [string]$PublicUrl = "http://localhost:8080",
  [string]$AdminPath = "/console",
  [string]$TrustedProxies = "127.0.0.1/32,::1/128",
  [string]$DataDir = "",
  [string]$ServiceName = "VoloBlog",
  [string]$ServiceAccount = "NT AUTHORITY\NetworkService",
  [string]$ServicePassword = "",
  [string]$PermissionGroup = "Administrators",
  [switch]$PurgeData,
  [switch]$DryRun
)

if ($Action -eq "Update") { $Action = "Upgrade" }
if ($FromGitHub) { $Download = $true }
if ($SkipVerify) { $NoVerify = $true }
if ((-not $Mirror) -and $GitHubProxy) { $Mirror = $GitHubProxy }

$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$ScriptDir = if ($MyInvocation.MyCommand.Path) { Split-Path -Parent $MyInvocation.MyCommand.Path } else { (Get-Location).Path }
$EffectiveListen = if ($ListenAddr) { $ListenAddr } else { "${HostAddr}:${Port}" }

if ($InstallDir) {
  $InstallDir = [System.IO.Path]::GetFullPath($InstallDir)
  if (-not $DataDir) { $DataDir = $InstallDir }
} else {
  $InstallDir = Join-Path $env:ProgramFiles "Volo"
  if (-not $DataDir) {
    $ProgramDataRoot = if ($env:ProgramData) { $env:ProgramData } else { "C:\ProgramData" }
    $DataDir = Join-Path $ProgramDataRoot "Volo"
  }
}
$DataDir = [System.IO.Path]::GetFullPath($DataDir)
$EnvFile = Join-Path $DataDir "volo.env"
$NssmCmd = Get-Command "nssm.exe" -ErrorAction SilentlyContinue
$Script:TempDownloadDir = ""
$Script:ResolvedBundleDir = ""

function New-RandomHex32 {
  $bytes = New-Object byte[] 32
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  return ($bytes | ForEach-Object { $_.ToString("x2") }) -join ""
}

function Get-ReleaseBaseUrl([string]$Ref) {
  $rawBase = if ($Ref -eq "latest") {
    "https://github.com/$Repo/releases/latest/download"
  } else {
    "https://github.com/$Repo/releases/download/$Ref"
  }
  if ($Mirror) {
    return ($Mirror.TrimEnd('/') + "/" + $rawBase)
  }
  return $rawBase
}

function Expand-VoloZip([string]$ZipFile, [string]$StageDir) {
  Expand-Archive -Path $ZipFile -DestinationPath $StageDir -Force
  $directExe = Join-Path $StageDir "volo.exe"
  if (Test-Path $directExe) {
    $Script:ResolvedBundleDir = $StageDir
    return $directExe
  }
  $subDir = Get-ChildItem -Path $StageDir -Directory | Select-Object -First 1
  if ($subDir -and (Test-Path (Join-Path $subDir.FullName "volo.exe"))) {
    $Script:ResolvedBundleDir = $subDir.FullName
    return (Join-Path $subDir.FullName "volo.exe")
  }
  throw "Archive '$ZipFile' does not contain volo.exe."
}

function Sync-BundledExtensions([string]$DestRoot) {
  if ((-not $Script:ResolvedBundleDir) -or (-not (Test-Path $Script:ResolvedBundleDir))) { return }
  foreach ($kind in @("plugins", "themes", "locales")) {
    $srcKind = Join-Path $Script:ResolvedBundleDir $kind
    if (Test-Path $srcKind) {
      $dstKind = Join-Path $DestRoot $kind
      New-Item -ItemType Directory -Force -Path $dstKind | Out-Null
      foreach ($child in (Get-ChildItem -Path $srcKind)) {
        $target = Join-Path $dstKind $child.Name
        if (-not (Test-Path $target)) {
          Copy-Item -Path $child.FullName -Destination $target -Recurse -Force
        }
      }
    }
  }
}

function Download-ReleaseBundle {
  $pinned = $false
  $primary = "latest"
  if ($Version) {
    if ($Version -eq "latest") {
      $primary = "latest"
    } elseif ($Version -eq "preview") {
      $primary = "preview"; $pinned = $true
    } else {
      $primary = "v" + $Version.TrimStart('v', 'V')
      $pinned = $true
    }
  } elseif ($Channel -eq "preview") {
    $primary = "preview"; $pinned = $true
  }

  $stableAsset = "volo-windows-$Arch.zip"
  if ($DryRun) {
    Write-Host "[dry-run] Download $stableAsset from $Repo ($primary) + verify SHA256SUMS.txt"
    return "C:\volo-dry-run.exe"
  }

  $tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("volo-dl-" + [Guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Force -Path $tmpDir | Out-Null
  $Script:TempDownloadDir = $tmpDir

  $baseUrl = Get-ReleaseBaseUrl $primary
  $assetName = $stableAsset
  $dlZip = Join-Path $tmpDir $assetName
  $dlSum = Join-Path $tmpDir "SHA256SUMS.txt"

  Write-Host "[Volo] Downloading $assetName from GitHub Releases ($Repo @ $primary)..." -ForegroundColor Cyan
  $ok = $false
  & curl.exe -fL --retry 3 --connect-timeout 15 -o $dlZip "$baseUrl/$assetName"
  if ($LASTEXITCODE -eq 0 -and (Test-Path $dlZip) -and ((Get-Item $dlZip).Length -gt 0)) {
    $ok = $true
  }

  if (-not $ok -and $primary -ne "latest") {
    $assetName = "volo-$primary-windows-$Arch.zip"
    $dlZip = Join-Path $tmpDir $assetName
    & curl.exe -fL --retry 3 --connect-timeout 15 -o $dlZip "$baseUrl/$assetName"
    if ($LASTEXITCODE -eq 0 -and (Test-Path $dlZip) -and ((Get-Item $dlZip).Length -gt 0)) {
      $ok = $true
    }
  }

  if (-not $ok -and ($primary -eq "latest") -and (-not $pinned)) {
    try {
      $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ "User-Agent" = "Volo-go-Installer" } -TimeoutSec 15
      if ($rel.tag_name) {
        $baseUrl = Get-ReleaseBaseUrl ([string]$rel.tag_name)
        $assetName = "volo-$($rel.tag_name)-windows-$Arch.zip"
        $dlZip = Join-Path $tmpDir $assetName
        & curl.exe -fL --retry 3 --connect-timeout 15 -o $dlZip "$baseUrl/$assetName"
        if ($LASTEXITCODE -eq 0 -and (Test-Path $dlZip) -and ((Get-Item $dlZip).Length -gt 0)) { $ok = $true }
      }
    } catch {}
    if (-not $ok) {
      Write-Host "[Volo] No stable 'latest' release found; falling back to 'preview' release..." -ForegroundColor Yellow
      $baseUrl = Get-ReleaseBaseUrl "preview"
      $assetName = $stableAsset
      $dlZip = Join-Path $tmpDir $assetName
      & curl.exe -fL --retry 3 --connect-timeout 15 -o $dlZip "$baseUrl/$assetName"
      if ($LASTEXITCODE -eq 0 -and (Test-Path $dlZip) -and ((Get-Item $dlZip).Length -gt 0)) { $ok = $true }
    }
  }

  if (-not $ok) {
    throw "Failed to download release archive for windows/$Arch from $Repo."
  }

  if (-not $NoVerify) {
    & curl.exe -fsL --retry 3 --connect-timeout 15 -o $dlSum "$baseUrl/SHA256SUMS.txt"
    if (($LASTEXITCODE -ne 0) -or (-not (Test-Path $dlSum)) -or ((Get-Item $dlSum).Length -eq 0)) {
      throw "Failed to download SHA256SUMS.txt from $baseUrl/SHA256SUMS.txt (pass -NoVerify only for custom mirrors without checksums)."
    }
    $expected = $null
    foreach ($line in (Get-Content $dlSum)) {
      if ($line -match "^([0-9a-fA-F]{64})\s+\*?$([Regex]::Escape($assetName))\s*$") {
        $expected = $Matches[1].ToLowerInvariant()
        break
      }
    }
    if (-not $expected) {
      throw "Entry for '$assetName' not found in SHA256SUMS.txt."
    }
    $actual = (Get-FileHash -Path $dlZip -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
      throw "SHA-256 mismatch for $assetName! Expected $expected, got $actual"
    }
    Write-Host "[Volo] Verified SHA-256 checksum ($actual)" -ForegroundColor Green
  } else {
    Write-Host "[Volo] WARNING: Skipping SHA-256 verification (-NoVerify specified)." -ForegroundColor Yellow
  }

  $exePath = Expand-VoloZip -ZipFile $dlZip -StageDir (Join-Path $tmpDir "extracted")
  $verOut = (& $exePath version 2>&1 | Out-String).Trim()
  if ($LASTEXITCODE -ne 0) {
    throw "Downloaded binary failed smoke check (version): $verOut"
  }
  return $exePath
}

function Resolve-VoloBinary {
  if ($BinaryPath) {
    if (-not $DryRun -and (-not (Test-Path $BinaryPath))) {
      throw "Specified binary not found: $BinaryPath"
    }
    return $BinaryPath
  }
  if ($ArchivePath) {
    if (-not (Test-Path $ArchivePath)) {
      throw "Specified archive not found: $ArchivePath"
    }
    $tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("volo-arc-" + [Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Force -Path $tmpDir | Out-Null
    $Script:TempDownloadDir = $tmpDir
    return (Expand-VoloZip -ZipFile $ArchivePath -StageDir (Join-Path $tmpDir "extracted"))
  }
  if ($Download -or $Version -or ($Action -eq "Upgrade")) {
    return (Download-ReleaseBundle)
  }
  $Candidates = @(
    (Join-Path $ScriptDir "volo-windows-$Arch.exe"),
    (Join-Path $ScriptDir "volo.exe"),
    (Join-Path $ScriptDir "..\volo.exe"),
    (Join-Path $ScriptDir "..\dist\volo-windows-$Arch.exe")
  )
  foreach ($c in $Candidates) {
    if (Test-Path $c) {
      Write-Host "[Volo] Using local binary: $c" -ForegroundColor Cyan
      $Script:ResolvedBundleDir = Split-Path -Parent (Resolve-Path $c).Path
      return (Resolve-Path $c).Path
    }
  }
  Write-Host "[Volo] No local binary found; auto-downloading from GitHub Releases..." -ForegroundColor Cyan
  return (Download-ReleaseBundle)
}

function Stop-VoloServiceAndWait([string]$TargetExe = "") {
  if ($DryRun) {
    Write-Host "[dry-run] Stop service/task '$ServiceName'"
    return
  }
  if ($NssmCmd) { & $NssmCmd.Source stop $ServiceName 2>$null | Out-Null }
  Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
  Stop-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue
  Start-Sleep -Seconds 1
  if ($TargetExe -and (Test-Path $TargetExe)) {
    $fullTarget = [System.IO.Path]::GetFullPath($TargetExe)
    foreach ($proc in @(Get-Process -Name "volo" -ErrorAction SilentlyContinue)) {
      try {
        if ($proc.Path -and [string]::Equals([System.IO.Path]::GetFullPath($proc.Path), $fullTarget, [System.StringComparison]::OrdinalIgnoreCase)) {
          Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        }
      } catch {}
    }
  }
}

function Install-BinarySafe([string]$SourceExe, [string]$TargetExe) {
  if ($DryRun) {
    Write-Host "[dry-run] Atomic install $SourceExe -> $TargetExe"
    return
  }
  $fullSrc = [System.IO.Path]::GetFullPath($SourceExe)
  $fullDst = [System.IO.Path]::GetFullPath($TargetExe)
  if ([string]::Equals($fullSrc, $fullDst, [System.StringComparison]::OrdinalIgnoreCase)) { return }
  New-Item -ItemType Directory -Force -Path (Split-Path -Parent $fullDst) | Out-Null
  $oldExe = "$fullDst.old"
  $newExe = "$fullDst.new"
  Remove-Item -Force $oldExe -ErrorAction SilentlyContinue
  Remove-Item -Force $newExe -ErrorAction SilentlyContinue
  Copy-Item -Force $fullSrc $newExe
  try {
    Move-Item -Force $newExe $fullDst
  } catch {
    if (Test-Path $fullDst) { Move-Item -Force $fullDst $oldExe }
    Move-Item -Force $newExe $fullDst
    Remove-Item -Force $oldExe -ErrorAction SilentlyContinue
  }
}

function Set-DataDirPermissions([string]$TargetDir, [string]$Account, [string]$Group) {
  if ($DryRun) {
    Write-Host "[dry-run] Restrict NTFS ACLs on $TargetDir"
    return
  }
  & icacls $TargetDir /inheritance:r | Out-Null
  & icacls $TargetDir /grant:r "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F" | Out-Null
  if ($Account -and ($Account -ne "SYSTEM") -and ($Account -ne "NT AUTHORITY\SYSTEM")) {
    & icacls $TargetDir /grant:r "${Account}:(OI)(CI)M" | Out-Null
  }
  if ($Group -and ($Group -ne "Administrators") -and ($Group -ne "BUILTIN\Administrators")) {
    & icacls $TargetDir /grant:r "${Group}:(OI)(CI)RX" | Out-Null
  }
}

try {
  $DestBin = [System.IO.Path]::GetFullPath((Join-Path $InstallDir "volo.exe"))
  switch ($Action) {
    "Install" {
      $SrcBin = Resolve-VoloBinary
      Stop-VoloServiceAndWait -TargetExe $DestBin
      if (-not $DryRun) {
        foreach ($sub in @("data\uploads", "data\backups", "data\geoip", "plugins", "themes", "locales", "logs")) {
          New-Item -ItemType Directory -Force -Path (Join-Path $DataDir $sub) | Out-Null
        }
        Sync-BundledExtensions -DestRoot $DataDir
      }
      Install-BinarySafe -SourceExe $SrcBin -TargetExe $DestBin

      if (-not (Test-Path $EnvFile)) {
        $SecretHex = if ($DryRun) { "<redacted>" } else { New-RandomHex32 }
        $EnvLines = @(
          "# Generated by Volo-go install-service.ps1",
          "VOLO_LISTEN_ADDR=$EffectiveListen",
          "VOLO_PUBLIC_URL=$PublicUrl",
          "VOLO_TRUSTED_PROXIES=$TrustedProxies",
          "VOLO_ADMIN_PATH=$AdminPath",
          "VOLO_SECRET_KEY=$SecretHex",
          "VOLO_GEO_ACCESS_ENABLED=true",
          "VOLO_DB_DRIVER=sqlite",
          "VOLO_DB_DSN=$($DataDir -replace '\\','/')/data/volo.db",
          "VOLO_DATA_DIR=$($DataDir -replace '\\','/')/data",
          "VOLO_UPLOAD_DIR=$($DataDir -replace '\\','/')/data/uploads",
          "VOLO_BACKUP_DIR=$($DataDir -replace '\\','/')/data/backups",
          "VOLO_GEOIP_DIR=$($DataDir -replace '\\','/')/data/geoip",
          "VOLO_PLUGINS_DIR=$($DataDir -replace '\\','/')/plugins",
          "VOLO_THEMES_DIR=$($DataDir -replace '\\','/')/themes",
          "VOLO_LOCALES_DIR=$($DataDir -replace '\\','/')/locales"
        )
        if ($DryRun) {
          Write-Host "[dry-run] Write $EnvFile"
        } else {
          Set-Content -Path $EnvFile -Value $EnvLines -Encoding UTF8
        }
      }
      Set-DataDirPermissions -TargetDir $DataDir -Account $ServiceAccount -Group $PermissionGroup

      if ($DryRun) {
        Write-Host "[dry-run] Register and start Windows auto-start service '$ServiceName'"
      } elseif ($NssmCmd) {
        & $NssmCmd.Source install $ServiceName $DestBin "serve" "--env-file" $EnvFile | Out-Null
        & $NssmCmd.Source set $ServiceName AppDirectory $DataDir | Out-Null
        & $NssmCmd.Source set $ServiceName Start SERVICE_AUTO_START | Out-Null
        Start-Service -Name $ServiceName
      } else {
        $TaskAction = New-ScheduledTaskAction -Execute $DestBin -Argument "serve --env-file `"$EnvFile`"" -WorkingDirectory $DataDir
        $TaskTrigger = New-ScheduledTaskTrigger -AtStartup
        $TaskSettings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
        $Principal = New-ScheduledTaskPrincipal -UserId $ServiceAccount -LogonType ServiceAccount -RunLevel Highest
        Register-ScheduledTask -TaskName $ServiceName -Action $TaskAction -Trigger $TaskTrigger -Settings $TaskSettings -Principal $Principal -Force | Out-Null
        Start-ScheduledTask -TaskName $ServiceName
      }
      Write-Host "[Volo] Installed & started Windows service '$ServiceName'" -ForegroundColor Green
      Write-Host "  Binary   : $DestBin"
      Write-Host "  Config   : $EnvFile"
      Write-Host "  Data Dir : $DataDir"
      Write-Host "  Listen   : http://$EffectiveListen"
    }
    "Upgrade" {
      $ExistingTask = Get-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue
      if ($ExistingTask -and (-not $PSBoundParameters.ContainsKey("InstallDir")) -and $ExistingTask.Actions.Count -gt 0 -and $ExistingTask.Actions[0].Execute) {
        $DestBin = [System.IO.Path]::GetFullPath($ExistingTask.Actions[0].Execute.Trim('"'))
      }
      $SrcBin = Resolve-VoloBinary
      Stop-VoloServiceAndWait -TargetExe $DestBin
      if ((-not $DryRun) -and (Test-Path $DataDir)) { Sync-BundledExtensions -DestRoot $DataDir }
      Install-BinarySafe -SourceExe $SrcBin -TargetExe $DestBin
      if (-not $DryRun) {
        if ($NssmCmd -and (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) {
          Start-Service -Name $ServiceName
        } elseif ($ExistingTask) {
          Start-ScheduledTask -TaskName $ServiceName
        }
        Write-Host "[Volo] Upgraded & restarted '$ServiceName' ($DestBin)" -ForegroundColor Green
        & $DestBin version
      }
    }
    "Uninstall" {
      Stop-VoloServiceAndWait -TargetExe $DestBin
      if (-not $DryRun) {
        if ($NssmCmd -and (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) {
          & $NssmCmd.Source remove $ServiceName confirm | Out-Null
        }
        Unregister-ScheduledTask -TaskName $ServiceName -Confirm:$false -ErrorAction SilentlyContinue
        Remove-Item -Force $DestBin -ErrorAction SilentlyContinue
        if ($PurgeData) { Remove-Item -Recurse -Force $DataDir -ErrorAction SilentlyContinue }
        Write-Host "[Volo] Uninstalled service '$ServiceName' (DataDir: $DataDir)" -ForegroundColor Green
      }
    }
    "Start" {
      if ($NssmCmd -and (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) { Start-Service -Name $ServiceName }
      else { Start-ScheduledTask -TaskName $ServiceName }
    }
    "Stop" { Stop-VoloServiceAndWait -TargetExe $DestBin }
    "Restart" {
      Stop-VoloServiceAndWait -TargetExe $DestBin
      if ($NssmCmd -and (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) { Start-Service -Name $ServiceName }
      else { Start-ScheduledTask -TaskName $ServiceName }
    }
    "Status" {
      $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
      if ($svc) { $svc | Format-Table Name, Status, StartType -AutoSize }
      else { Get-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue | Select-Object TaskName, State | Format-Table -AutoSize }
    }
  }
} finally {
  if ($Script:TempDownloadDir -and (Test-Path $Script:TempDownloadDir)) {
    Remove-Item -Recurse -Force $Script:TempDownloadDir -ErrorAction SilentlyContinue
  }
}
