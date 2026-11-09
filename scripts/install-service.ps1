<#
.SYNOPSIS
  Volo-go Windows Auto-Start Service Installer & Online Updater
  Repository: https://github.com/Willh92/Volo-go
  License: Apache-2.0

.DESCRIPTION
  Installs volo.exe under %ProgramW6432%\Volo (%ProgramFiles%\Volo), stores runtime
  configuration & data under %ProgramData%\Volo (locked down with NTFS ACLs via icacls),
  and registers a native Windows Service via sc.exe by default (volo.exe natively
  implements the Windows Service Control Manager protocol). NSSM and Scheduled Task
  modes remain supported when explicitly selected or already registered.

.EXAMPLE
  # Install native Windows auto-start service (auto-downloads from GitHub Releases if needed)
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Install -PublicUrl "https://blog.example.com"

  # Safe online upgrade: backs up DB, downloads & verifies SHA-256, smoke-tests, replaces binary,
  # refreshes bundled official themes/plugins/locales, polls healthcheck, and auto-rolls back on failure
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade

  # Manage service lifecycle & inspect logs
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Status
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Logs
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Restart
  powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Uninstall
#>

param(
  [ValidateSet("Install", "Upgrade", "Update", "Uninstall", "Start", "Stop", "Restart", "Status", "Logs")]
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
  [switch]$SkipBackup,
  [switch]$NoBackup,
  [switch]$NoStart,
  [string]$HostAddr = "127.0.0.1",
  [int]$Port = 8080,
  [string]$ListenAddr = "",
  [string]$PublicUrl = "http://localhost:8080",
  [string]$AdminPath = "/console",
  [string]$TrustedProxies = "127.0.0.1/32,::1/128",
  [string]$DataDir = "",
  [string]$EnvFile = "",
  [string]$ServiceName = "VoloBlog",
  [ValidateSet("Auto", "Native", "Nssm", "ScheduledTask")]
  [Alias("ServiceMode")]
  [string]$ServiceManager = "Auto",
  [switch]$UseNssm,
  [string]$ServiceAccount = "NT AUTHORITY\NetworkService",
  [string]$ServicePassword = "",
  [string]$PermissionGroup = "Administrators",
  [int]$LogLines = 100,
  [switch]$Follow,
  [switch]$PurgeData,
  [switch]$DryRun
)

if ($Action -eq "Update") { $Action = "Upgrade" }
if ($FromGitHub) { $Download = $true }
if ($SkipVerify) { $NoVerify = $true }
if ($NoBackup) { $SkipBackup = $true }
if ((-not $Mirror) -and $GitHubProxy) { $Mirror = $GitHubProxy }
if ($UseNssm -and ($ServiceManager -eq "Auto")) { $ServiceManager = "Nssm" }
if ($LogLines -lt 1) { $LogLines = 100 }

$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

if ($ServiceName -notmatch '^[A-Za-z0-9._-]{1,64}$') {
  throw "Invalid ServiceName '$ServiceName': must match ^[A-Za-z0-9._-]{1,64}$."
}

if (-not $DryRun) {
  $CurrentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
  if (-not $CurrentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Administrator privileges are required to manage Windows services/tasks and ProgramData ACLs. Re-run in an elevated PowerShell prompt (or pass -DryRun)."
  }
}

# WOW64-safe CPU architecture detection: check PROCESSOR_ARCHITEW6432 before PROCESSOR_ARCHITECTURE
# so a 32-bit PowerShell host on 64-bit x64/ARM64 Windows resolves the true native architecture.
$RawArch = if (-not [string]::IsNullOrWhiteSpace($env:PROCESSOR_ARCHITEW6432)) {
  $env:PROCESSOR_ARCHITEW6432.Trim()
} elseif (-not [string]::IsNullOrWhiteSpace($env:PROCESSOR_ARCHITECTURE)) {
  $env:PROCESSOR_ARCHITECTURE.Trim()
} else {
  "AMD64"
}

$Arch = switch -Regex ($RawArch) {
  '^(?i)(arm64|aarch64)$'    { "arm64" }
  '^(?i)(amd64|x86_64|x64)$' { "amd64" }
  default {
    throw "Unsupported Windows CPU architecture '$RawArch' (Volo-go requires 64-bit amd64 or arm64)."
  }
}

# Prefer ProgramW6432 so WOW64 32-bit PowerShell installs into 64-bit Program Files rather than Program Files (x86).
$ProgramFiles64 = if (-not [string]::IsNullOrWhiteSpace($env:ProgramW6432)) {
  $env:ProgramW6432
} elseif (-not [string]::IsNullOrWhiteSpace($env:ProgramFiles)) {
  $env:ProgramFiles
} else {
  "C:\Program Files"
}

$ScriptDir = if ($MyInvocation.MyCommand.Path) { Split-Path -Parent $MyInvocation.MyCommand.Path } else { (Get-Location).Path }
$EffectiveListen = if ($ListenAddr) { $ListenAddr } else { "${HostAddr}:${Port}" }

$Script:HasExplicitInstallDir = $PSBoundParameters.ContainsKey("InstallDir") -and (-not [string]::IsNullOrWhiteSpace($InstallDir))
$Script:HasExplicitDataDir = $PSBoundParameters.ContainsKey("DataDir") -and (-not [string]::IsNullOrWhiteSpace($DataDir))
$Script:HasExplicitEnvFile = $PSBoundParameters.ContainsKey("EnvFile") -and (-not [string]::IsNullOrWhiteSpace($EnvFile))
$Script:HasExplicitListen = $PSBoundParameters.ContainsKey("ListenAddr") -or $PSBoundParameters.ContainsKey("HostAddr") -or $PSBoundParameters.ContainsKey("Port")

if ($InstallDir) {
  $InstallDir = [System.IO.Path]::GetFullPath($InstallDir)
  if (-not $DataDir) { $DataDir = $InstallDir }
} else {
  $InstallDir = Join-Path $ProgramFiles64 "Volo"
  if (-not $DataDir) {
    $ProgramDataRoot = if ($env:ProgramData) { $env:ProgramData } else { "C:\ProgramData" }
    $DataDir = Join-Path $ProgramDataRoot "Volo"
  }
}
$DataDir = [System.IO.Path]::GetFullPath($DataDir)
if ($EnvFile) {
  $EnvFile = [System.IO.Path]::GetFullPath($EnvFile)
} else {
  $EnvFile = Join-Path $DataDir "volo.env"
}

$NssmCmd = Get-Command "nssm.exe" -ErrorAction SilentlyContinue
$CurlCmd = Get-Command "curl.exe" -ErrorAction SilentlyContinue
$Script:TempDownloadDir = ""
$Script:ResolvedBundleDir = ""

function New-RandomHex32 {
  $bytes = New-Object byte[] 32
  $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
  try {
    $rng.GetBytes($bytes)
  } finally {
    if ($rng -is [System.IDisposable]) { $rng.Dispose() }
  }
  return ($bytes | ForEach-Object { $_.ToString("x2") }) -join ""
}

function Format-Win32Arg([string]$Value) {
  if ($null -eq $Value) { return '""' }
  if ($Value.Length -eq 0) { return '""' }
  if ($Value -notmatch '[\s"]') { return $Value }
  $sb = New-Object System.Text.StringBuilder
  [void]$sb.Append('"')
  $backslashes = 0
  foreach ($ch in $Value.ToCharArray()) {
    if ($ch -eq '\') {
      $backslashes++
    } elseif ($ch -eq '"') {
      if ($backslashes -gt 0) {
        [void]$sb.Append(('\' * ($backslashes * 2)))
        $backslashes = 0
      }
      [void]$sb.Append('\"')
    } else {
      if ($backslashes -gt 0) {
        [void]$sb.Append(('\' * $backslashes))
        $backslashes = 0
      }
      [void]$sb.Append($ch)
    }
  }
  if ($backslashes -gt 0) {
    [void]$sb.Append(('\' * ($backslashes * 2)))
  }
  [void]$sb.Append('"')
  return $sb.ToString()
}

function Invoke-ScExe([string[]]$ScArgs, [switch]$IgnoreExitCode) {
  $argLine = ($ScArgs | ForEach-Object { Format-Win32Arg $_ }) -join " "
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = "sc.exe"
  $psi.Arguments = $argLine
  $psi.UseShellExecute = $false
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.CreateNoWindow = $true

  $proc = [System.Diagnostics.Process]::Start($psi)
  $stdout = $proc.StandardOutput.ReadToEnd()
  $stderr = $proc.StandardError.ReadToEnd()
  $proc.WaitForExit()
  $combined = ($stdout + "`n" + $stderr).Trim()
  if ((-not $IgnoreExitCode) -and ($proc.ExitCode -ne 0)) {
    throw "sc.exe $($ScArgs[0]) failed (exit code $($proc.ExitCode)): $combined"
  }
  return [PSCustomObject]@{
    ExitCode = $proc.ExitCode
    Output   = $combined
  }
}

function Invoke-FileDownload([string]$Url, [string]$OutFile, [switch]$Quiet) {
  if ($CurlCmd) {
    if ($Quiet) {
      & $CurlCmd.Source -fsSL --retry 3 --connect-timeout 15 -o $OutFile $Url
    } else {
      & $CurlCmd.Source -fL --retry 3 --connect-timeout 15 -o $OutFile $Url
    }
    return ($LASTEXITCODE -eq 0 -and (Test-Path -LiteralPath $OutFile) -and ((Get-Item -LiteralPath $OutFile).Length -gt 0))
  }
  for ($attempt = 1; $attempt -le 3; $attempt++) {
    try {
      Invoke-WebRequest -Uri $Url -OutFile $OutFile -UseBasicParsing -TimeoutSec 60 -Headers @{ "User-Agent" = "Volo-go-Installer" }
      if ((Test-Path -LiteralPath $OutFile) -and ((Get-Item -LiteralPath $OutFile).Length -gt 0)) {
        return $true
      }
    } catch {
      if ($attempt -lt 3) { Start-Sleep -Seconds 2 }
    }
  }
  return $false
}

function Assert-SafePurgeDir([string]$TargetPath) {
  if ([string]::IsNullOrWhiteSpace($TargetPath)) {
    throw "Refusing to purge an empty directory path."
  }
  $full = [System.IO.Path]::GetFullPath($TargetPath).TrimEnd('\', '/')
  $root = [System.IO.Path]::GetPathRoot($full).TrimEnd('\', '/')
  if ($full.Length -le 3 -or [string]::Equals($full, $root, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to purge drive root: $full"
  }
  $protected = @(
    $env:SystemRoot,
    $env:ProgramW6432,
    $env:ProgramFiles,
    ${env:ProgramFiles(x86)},
    $env:ProgramData,
    $env:USERPROFILE,
    $env:PUBLIC,
    "C:\Windows",
    "C:\Program Files",
    "C:\Program Files (x86)",
    "C:\ProgramData",
    "C:\Users"
  ) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
  foreach ($p in $protected) {
    $normP = [System.IO.Path]::GetFullPath($p).TrimEnd('\', '/')
    if ([string]::Equals($full, $normP, [System.StringComparison]::OrdinalIgnoreCase)) {
      throw "Refusing to purge protected system directory: $full"
    }
  }
}

function Read-EnvFileValue([string]$FilePath, [string]$KeyName) {
  if ([string]::IsNullOrWhiteSpace($FilePath) -or (-not (Test-Path -LiteralPath $FilePath))) {
    return ""
  }
  $result = ""
  foreach ($rawLine in (Get-Content -LiteralPath $FilePath -ErrorAction SilentlyContinue)) {
    if ($null -eq $rawLine) { continue }
    $line = $rawLine.Trim()
    if ($line.Length -eq 0 -or $line.StartsWith("#")) { continue }
    if ($line.StartsWith("export ")) { $line = $line.Substring(7).TrimStart() }
    $eqIdx = $line.IndexOf('=')
    if ($eqIdx -le 0) { continue }
    $k = $line.Substring(0, $eqIdx).Trim()
    if ($k -ne $KeyName) { continue }
    $v = $line.Substring($eqIdx + 1).Trim()
    if ($v.Length -ge 2) {
      if (($v.StartsWith('"') -and $v.EndsWith('"')) -or ($v.StartsWith("'") -and $v.EndsWith("'"))) {
        $v = $v.Substring(1, $v.Length - 2)
      }
    }
    $result = $v
  }
  return $result
}

function Extract-EnvFileFromArgs([string]$ArgLine) {
  if ([string]::IsNullOrWhiteSpace($ArgLine)) { return "" }
  if ($ArgLine -match '--env-file(?:=|\s+)"([^"]+)"') { return $Matches[1] }
  if ($ArgLine -match '--env-file(?:=|\s+)([^\s"]+)') { return $Matches[1] }
  return ""
}

function Get-Win32ServiceRecord {
  if ($DryRun) {
    return (Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue)
  }
  return (Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction SilentlyContinue)
}

function Test-IsNssmService($WinSvcRecord = $null) {
  $svcRec = if ($null -ne $WinSvcRecord) { $WinSvcRecord } else { Get-Win32ServiceRecord }
  if (-not $svcRec) { return $false }
  if ($svcRec.PathName -and ($svcRec.PathName -match '(?i)(^|[\\/"\s])nssm(\.exe)?(["\s]|$)')) {
    return $true
  }
  return $false
}

function Resolve-EffectiveServiceManager([string]$RequestedMode, [switch]$ForUpgrade) {
  if ($RequestedMode -ne "Auto") {
    return $RequestedMode
  }
  $winSvc = Get-Win32ServiceRecord
  if ($winSvc) {
    if ((Test-IsNssmService $winSvc) -and $NssmCmd) {
      return "Nssm"
    }
    return "Native"
  }
  if ($ForUpgrade) {
    $existingTask = Get-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue
    if ($existingTask) {
      return "ScheduledTask"
    }
  }
  # First-class default: native Windows Service via sc.exe (cmd/volo/svc_windows.go speaks SCM natively)
  return "Native"
}

function Resolve-ExistingRegistration([ref]$RefBin, [ref]$RefDataDir, [ref]$RefEnvFile) {
  $hasExplicitInstallDir = $Script:HasExplicitInstallDir
  $hasExplicitDataDir = $Script:HasExplicitDataDir
  $hasExplicitEnvFile = $Script:HasExplicitEnvFile

  # 1. Inspect Win32_Service (native sc.exe or NSSM) registration first.
  $winSvc = Get-Win32ServiceRecord
  if ($winSvc) {
    if ((Test-IsNssmService $winSvc) -and $NssmCmd) {
      $nssmApp = (& $NssmCmd.Source get $ServiceName Application 2>$null | Out-String).Trim("`r", "`n", "`0", " ", '"')
      $nssmDir = (& $NssmCmd.Source get $ServiceName AppDirectory 2>$null | Out-String).Trim("`r", "`n", "`0", " ", '"')
      $nssmArgs = (& $NssmCmd.Source get $ServiceName AppParameters 2>$null | Out-String).Trim("`r", "`n", "`0", " ")
      if ((-not $hasExplicitInstallDir) -and $nssmApp) {
        $RefBin.Value = [System.IO.Path]::GetFullPath($nssmApp)
      }
      if ((-not $hasExplicitDataDir) -and $nssmDir) {
        $RefDataDir.Value = [System.IO.Path]::GetFullPath($nssmDir)
      }
      $parsedEnv = Extract-EnvFileFromArgs $nssmArgs
      if ((-not $hasExplicitEnvFile) -and $parsedEnv) {
        $RefEnvFile.Value = [System.IO.Path]::GetFullPath($parsedEnv)
        if (-not $hasExplicitDataDir -and (-not $nssmDir)) {
          $RefDataDir.Value = Split-Path -Parent $RefEnvFile.Value
        }
      } elseif ((-not $hasExplicitEnvFile) -and (-not $hasExplicitDataDir)) {
        $RefEnvFile.Value = Join-Path $RefDataDir.Value "volo.env"
      }
      return
    }

    if ($winSvc.PathName) {
      $cmdLine = [string]$winSvc.PathName
      $exeCandidate = ""
      if ($cmdLine -match '^"([^"]+)"') {
        $exeCandidate = $Matches[1]
      } elseif ($cmdLine -match '^([^\s]+)') {
        $exeCandidate = $Matches[1]
      }
      if ((-not $hasExplicitInstallDir) -and $exeCandidate -and ($exeCandidate -notmatch '(?i)nssm(\.exe)?$')) {
        $RefBin.Value = [System.IO.Path]::GetFullPath($exeCandidate)
      }
      $parsedEnv = Extract-EnvFileFromArgs $cmdLine
      if ($parsedEnv) {
        if (-not $hasExplicitEnvFile) {
          $RefEnvFile.Value = [System.IO.Path]::GetFullPath($parsedEnv)
        }
        if (-not $hasExplicitDataDir) {
          $envParent = Split-Path -Parent ([System.IO.Path]::GetFullPath($parsedEnv))
          $cfgDataDir = Read-EnvFileValue ([System.IO.Path]::GetFullPath($parsedEnv)) "VOLO_DATA_DIR"
          if ($cfgDataDir) {
            $normCfgData = [System.IO.Path]::GetFullPath($cfgDataDir.Replace('/', '\'))
            if ([System.IO.Path]::GetFileName($normCfgData.TrimEnd('\')) -ieq "data") {
              $RefDataDir.Value = Split-Path -Parent $normCfgData
            } else {
              $RefDataDir.Value = $envParent
            }
          } else {
            $RefDataDir.Value = $envParent
          }
        }
      }
      return
    }
  }

  # 2. Inspect legacy Scheduled Task registration.
  $task = Get-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue
  if ($task -and $task.Actions.Count -gt 0) {
    $act = $task.Actions[0]
    if ((-not $hasExplicitInstallDir) -and $act.Execute) {
      $RefBin.Value = [System.IO.Path]::GetFullPath($act.Execute.Trim('"'))
    }
    if ((-not $hasExplicitDataDir) -and $act.WorkingDirectory) {
      $RefDataDir.Value = [System.IO.Path]::GetFullPath($act.WorkingDirectory.Trim('"'))
    }
    $parsedEnv = Extract-EnvFileFromArgs ([string]$act.Arguments)
    if ((-not $hasExplicitEnvFile) -and $parsedEnv) {
      $RefEnvFile.Value = [System.IO.Path]::GetFullPath($parsedEnv)
    } elseif ((-not $hasExplicitEnvFile) -and (-not $hasExplicitDataDir)) {
      $RefEnvFile.Value = Join-Path $RefDataDir.Value "volo.env"
    }
  }
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

function Resolve-PreviewReleaseTag {
  $apiUrl = "https://api.github.com/repos/$Repo/releases?per_page=10"
  try {
    $releases = Invoke-RestMethod -Uri $apiUrl -Headers @{
      "Accept"     = "application/vnd.github+json"
      "User-Agent" = "Volo-go-Installer"
    } -TimeoutSec 10
    if ($releases) {
      foreach ($rel in @($releases)) {
        if (($null -ne $rel) -and (-not $rel.draft) -and (-not [string]::IsNullOrWhiteSpace([string]$rel.tag_name))) {
          return ([string]$rel.tag_name).Trim()
        }
      }
    }
  } catch {}
  return ""
}

function Resolve-LatestTagFromRedirect([string]$TargetUrl) {
  if ([string]::IsNullOrWhiteSpace($TargetUrl)) { return "" }

  if ($CurlCmd) {
    try {
      $hdrLines = & $CurlCmd.Source -fsSI --connect-timeout 10 $TargetUrl 2>$null
      foreach ($hl in @($hdrLines)) {
        if ([string]$hl -match '(?i)^location:\s*(.+)$') {
          $loc = $Matches[1].Trim()
          if ($loc -match '/releases/tag/([^/?#\s]+)') {
            return $Matches[1]
          }
        }
      }
    } catch {}
    try {
      $effUrl = (& $CurlCmd.Source -fsSL --connect-timeout 10 -o NUL -w "%{url_effective}" $TargetUrl 2>$null | Out-String).Trim()
      if ($effUrl -match '/releases/tag/([^/?#\s]+)') {
        return $Matches[1]
      }
    } catch {}
  }

  foreach ($autoRedir in @($false, $true)) {
    $resp = $null
    try {
      $req = [System.Net.HttpWebRequest]::Create($TargetUrl)
      $req.Method = "HEAD"
      $req.UserAgent = "Volo-go-Installer"
      $req.Timeout = 10000
      $req.AllowAutoRedirect = $autoRedir
      try {
        $resp = [System.Net.HttpWebResponse]$req.GetResponse()
      } catch [System.Net.WebException] {
        if ($_.Exception.Response) {
          $resp = [System.Net.HttpWebResponse]$_.Exception.Response
        }
      }
      if ($resp) {
        $locHdr = $resp.Headers["Location"]
        if ($locHdr -and ($locHdr -match '/releases/tag/([^/?#\s]+)')) {
          return $Matches[1]
        }
        if ($resp.ResponseUri -and ($resp.ResponseUri.AbsoluteUri -match '/releases/tag/([^/?#\s]+)')) {
          return $Matches[1]
        }
      }
    } catch {
    } finally {
      if ($resp) { $resp.Close() }
    }
  }
  return ""
}

function Resolve-LatestReleaseTag {
  # When -Mirror is NOT set, try GitHub REST API first, then fall back to /releases/latest redirect.
  # When -Mirror IS set (e.g. offline/China mirror where api.github.com is blocked), prefer resolving
  # redirect via "$Mirror/https://github.com/$Repo/releases/latest" if api.github.com is unreachable.
  if (-not $Mirror) {
    try {
      $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{
        "Accept"     = "application/vnd.github+json"
        "User-Agent" = "Volo-go-Installer"
      } -TimeoutSec 10
      if ($rel -and $rel.tag_name) {
        return ([string]$rel.tag_name).Trim()
      }
    } catch {}
    return (Resolve-LatestTagFromRedirect "https://github.com/$Repo/releases/latest")
  }

  $mirrorLatestUrl = $Mirror.TrimEnd('/') + "/https://github.com/$Repo/releases/latest"
  $tagFromMirror = Resolve-LatestTagFromRedirect $mirrorLatestUrl
  if ($tagFromMirror) {
    return $tagFromMirror
  }
  try {
    $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{
      "Accept"     = "application/vnd.github+json"
      "User-Agent" = "Volo-go-Installer"
    } -TimeoutSec 8
    if ($rel -and $rel.tag_name) {
      return ([string]$rel.tag_name).Trim()
    }
  } catch {}
  return (Resolve-LatestTagFromRedirect "https://github.com/$Repo/releases/latest")
}

function Expand-VoloZip([string]$ZipFile, [string]$StageDir) {
  if (Test-Path -LiteralPath $StageDir) {
    Remove-Item -LiteralPath $StageDir -Recurse -Force -ErrorAction SilentlyContinue
  }
  Expand-Archive -Path $ZipFile -DestinationPath $StageDir -Force
  $directExe = Join-Path $StageDir "volo.exe"
  if (Test-Path -LiteralPath $directExe) {
    $Script:ResolvedBundleDir = $StageDir
    return $directExe
  }
  $subDir = Get-ChildItem -LiteralPath $StageDir -Directory | Select-Object -First 1
  if ($subDir -and (Test-Path -LiteralPath (Join-Path $subDir.FullName "volo.exe"))) {
    $Script:ResolvedBundleDir = $subDir.FullName
    return (Join-Path $subDir.FullName "volo.exe")
  }
  throw "Archive '$ZipFile' does not contain volo.exe."
}

function Detect-SiblingBundleDir([string]$ExeFile) {
  if ([string]::IsNullOrWhiteSpace($ExeFile)) { return }
  $candDir = Split-Path -Parent ([System.IO.Path]::GetFullPath($ExeFile))
  if ((Test-Path -LiteralPath (Join-Path $candDir "themes")) -or
      (Test-Path -LiteralPath (Join-Path $candDir "plugins")) -or
      (Test-Path -LiteralPath (Join-Path $candDir "locales"))) {
    $Script:ResolvedBundleDir = $candDir
    return
  }
  $repoParent = [System.IO.Path]::GetFullPath((Join-Path $ScriptDir ".."))
  if ((Test-Path -LiteralPath (Join-Path $repoParent "themes")) -or
      (Test-Path -LiteralPath (Join-Path $repoParent "plugins")) -or
      (Test-Path -LiteralPath (Join-Path $repoParent "locales"))) {
    $Script:ResolvedBundleDir = $repoParent
  }
}

function Resolve-ExtensionTargetDirs([string]$DestRoot, [string]$CfgEnvFile) {
  $themesDir = ""
  $pluginsDir = ""
  $localesDir = ""
  if ($CfgEnvFile -and (Test-Path -LiteralPath $CfgEnvFile)) {
    $themesDir = Read-EnvFileValue $CfgEnvFile "VOLO_THEMES_DIR"
    $pluginsDir = Read-EnvFileValue $CfgEnvFile "VOLO_PLUGINS_DIR"
    $localesDir = Read-EnvFileValue $CfgEnvFile "VOLO_LOCALES_DIR"
  }
  if ([string]::IsNullOrWhiteSpace($themesDir)) { $themesDir = Join-Path $DestRoot "themes" } else { $themesDir = $themesDir.Replace('/', '\') }
  if ([string]::IsNullOrWhiteSpace($pluginsDir)) { $pluginsDir = Join-Path $DestRoot "plugins" } else { $pluginsDir = $pluginsDir.Replace('/', '\') }
  if ([string]::IsNullOrWhiteSpace($localesDir)) { $localesDir = Join-Path $DestRoot "locales" } else { $localesDir = $localesDir.Replace('/', '\') }
  return @{
    themes  = [System.IO.Path]::GetFullPath($themesDir)
    plugins = [System.IO.Path]::GetFullPath($pluginsDir)
    locales = [System.IO.Path]::GetFullPath($localesDir)
  }
}

function Sync-BundledExtensions([string]$DestRoot, [string]$CfgEnvFile = "") {
  if ((-not $Script:ResolvedBundleDir) -or (-not (Test-Path -LiteralPath $Script:ResolvedBundleDir))) {
    return
  }
  $extTargets = Resolve-ExtensionTargetDirs -DestRoot $DestRoot -CfgEnvFile $CfgEnvFile

  # Refresh official bundled subdirectories/files in themes/ and plugins/ without deleting user custom extensions or touching data/.
  foreach ($kind in @("plugins", "themes")) {
    $srcKind = Join-Path $Script:ResolvedBundleDir $kind
    if (-not (Test-Path -LiteralPath $srcKind)) { continue }
    $dstKind = $extTargets[$kind]
    $normSrc = [System.IO.Path]::GetFullPath($srcKind).TrimEnd('\', '/')
    $normDst = [System.IO.Path]::GetFullPath($dstKind).TrimEnd('\', '/')
    if ([string]::Equals($normSrc, $normDst, [System.StringComparison]::OrdinalIgnoreCase)) { continue }

    foreach ($child in @(Get-ChildItem -LiteralPath $srcKind -Force)) {
      if ($child.Name.StartsWith(".")) { continue }
      $target = Join-Path $dstKind $child.Name
      if ($DryRun) {
        Write-Host "[dry-run] Sync official $kind\$($child.Name) -> $target (preserving custom extensions)"
      } else {
        New-Item -ItemType Directory -Force -Path $dstKind | Out-Null
        $stagePath = Join-Path $dstKind (".sync-" + $child.Name + "-" + [Guid]::NewGuid().ToString("N").Substring(0, 8))
        if (Test-Path -LiteralPath $stagePath) {
          Remove-Item -LiteralPath $stagePath -Recurse -Force -ErrorAction SilentlyContinue
        }
        Copy-Item -LiteralPath $child.FullName -Destination $stagePath -Recurse -Force
        if (Test-Path -LiteralPath $target) {
          Remove-Item -LiteralPath $target -Recurse -Force
        }
        Move-Item -LiteralPath $stagePath -Destination $target -Force
      }
    }
  }

  $srcLocales = Join-Path $Script:ResolvedBundleDir "locales"
  if (Test-Path -LiteralPath $srcLocales) {
    $dstLocales = $extTargets["locales"]
    $normSrcLoc = [System.IO.Path]::GetFullPath($srcLocales).TrimEnd('\', '/')
    $normDstLoc = [System.IO.Path]::GetFullPath($dstLocales).TrimEnd('\', '/')
    if (-not [string]::Equals($normSrcLoc, $normDstLoc, [System.StringComparison]::OrdinalIgnoreCase)) {
      foreach ($locFile in @(Get-ChildItem -LiteralPath $srcLocales -Filter "*.json" -File -Force)) {
        $targetLoc = Join-Path $dstLocales $locFile.Name
        if ($DryRun) {
          Write-Host "[dry-run] Sync official locales\$($locFile.Name) -> $targetLoc"
        } else {
          New-Item -ItemType Directory -Force -Path $dstLocales | Out-Null
          $stageLoc = Join-Path $dstLocales (".sync-" + $locFile.Name + "-" + [Guid]::NewGuid().ToString("N").Substring(0, 8))
          Copy-Item -LiteralPath $locFile.FullName -Destination $stageLoc -Force
          Move-Item -LiteralPath $stageLoc -Destination $targetLoc -Force
        }
      }
    }
  }
}

function Test-VoloBinarySmoke([string]$CandidateExe) {
  if ($DryRun) {
    Write-Host "[dry-run] Smoke-test candidate binary: `"$CandidateExe`" version"
    return
  }
  if (-not (Test-Path -LiteralPath $CandidateExe)) {
    throw "Candidate binary not found: $CandidateExe"
  }
  $verOut = (& $CandidateExe version 2>&1 | Out-String).Trim()
  if ($LASTEXITCODE -ne 0) {
    throw "Candidate binary '$CandidateExe' failed smoke check (version): $verOut"
  }
}

function Download-ReleaseBundle {
  $pinned = $false
  $wantPreview = $false
  $primary = "latest"
  if ($Version) {
    if ($Version -eq "latest") {
      $primary = "latest"
    } elseif ($Version -eq "preview") {
      $primary = "preview"
      $wantPreview = $true
    } else {
      $primary = "v" + $Version.TrimStart('v', 'V')
      $pinned = $true
    }
  } elseif ($Channel -eq "preview") {
    $primary = "preview"
    $wantPreview = $true
  }

  $stableAsset = "volo-windows-$Arch.zip"
  if ($DryRun) {
    if ($wantPreview) {
      Write-Host "[dry-run] Query /repos/$Repo/releases?per_page=10 for newest release/prerelease (fallback: latest)"
    }
    Write-Host "[dry-run] Download $stableAsset from $Repo ($primary) + verify SHA256SUMS.txt"
    return "C:\volo-dry-run.exe"
  }

  if ($wantPreview) {
    $previewTag = Resolve-PreviewReleaseTag
    if ($previewTag) {
      Write-Host "[Volo] Preview channel resolved to newest release tag: $previewTag" -ForegroundColor Cyan
      $primary = $previewTag
      $pinned = $true
    } else {
      Write-Host "[Volo] No preview/prerelease tag returned from /releases?per_page=10 (or API rate-limited); falling back cleanly to 'latest'." -ForegroundColor Yellow
      $primary = "latest"
      $pinned = $false
    }
  }

  $tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("volo-dl-" + [Guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Force -Path $tmpDir | Out-Null
  $Script:TempDownloadDir = $tmpDir

  $baseUrl = Get-ReleaseBaseUrl $primary
  $assetName = $stableAsset
  $dlZip = Join-Path $tmpDir $assetName
  $dlSum = Join-Path $tmpDir "SHA256SUMS.txt"

  # 1. Try unversioned release asset first (e.g. $Mirror/https://github.com/$Repo/releases/latest/download/volo-windows-$Arch.zip)
  Write-Host "[Volo] Downloading $assetName from GitHub Releases ($Repo @ $primary)..." -ForegroundColor Cyan
  $ok = Invoke-FileDownload -Url "$baseUrl/$assetName" -OutFile $dlZip

  # 2. If pinned tag was requested and unversioned asset wasn't found, try versioned volo-$tag-windows-$Arch.zip
  if ((-not $ok) -and ($primary -ne "latest")) {
    $assetName = "volo-$primary-windows-$Arch.zip"
    $dlZip = Join-Path $tmpDir $assetName
    $ok = Invoke-FileDownload -Url "$baseUrl/$assetName" -OutFile $dlZip
  }

  # 3. If latest failed (e.g. mirror requires concrete tag or only versioned asset exists), resolve tag via mirror redirect / API
  if ((-not $ok) -and ($primary -eq "latest") -and (-not $pinned)) {
    $resolvedTag = Resolve-LatestReleaseTag
    if ($resolvedTag -and ($resolvedTag -ne "latest")) {
      $baseUrl = Get-ReleaseBaseUrl $resolvedTag
      $assetName = "volo-$resolvedTag-windows-$Arch.zip"
      $dlZip = Join-Path $tmpDir $assetName
      $ok = Invoke-FileDownload -Url "$baseUrl/$assetName" -OutFile $dlZip
      if (-not $ok) {
        $assetName = $stableAsset
        $dlZip = Join-Path $tmpDir $assetName
        $ok = Invoke-FileDownload -Url "$baseUrl/$assetName" -OutFile $dlZip
      }
    }
    # Final mirror safety net: ensure unversioned latest download URL was tried if baseUrl changed
    if ((-not $ok) -and $Mirror) {
      $baseUrl = $Mirror.TrimEnd('/') + "/https://github.com/$Repo/releases/latest/download"
      $assetName = $stableAsset
      $dlZip = Join-Path $tmpDir $assetName
      $ok = Invoke-FileDownload -Url "$baseUrl/$assetName" -OutFile $dlZip
    }
  }

  if (-not $ok) {
    throw "Failed to download release archive for windows/$Arch from $Repo ($primary)."
  }

  if (-not $NoVerify) {
    $sumOk = Invoke-FileDownload -Url "$baseUrl/SHA256SUMS.txt" -OutFile $dlSum -Quiet
    if (-not $sumOk) {
      throw "Failed to download SHA256SUMS.txt from $baseUrl/SHA256SUMS.txt (pass -NoVerify only for custom mirrors without checksums)."
    }
    $expected = $null
    foreach ($line in (Get-Content -LiteralPath $dlSum)) {
      if ($line -match "^([0-9a-fA-F]{64})\s+\*?$([Regex]::Escape($assetName))\s*$") {
        $expected = $Matches[1].ToLowerInvariant()
        break
      }
    }
    if (-not $expected) {
      throw "Entry for '$assetName' not found in SHA256SUMS.txt."
    }
    $actual = (Get-FileHash -LiteralPath $dlZip -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
      throw "SHA-256 mismatch for $assetName! Expected $expected, got $actual"
    }
    Write-Host "[Volo] Verified SHA-256 checksum ($actual)" -ForegroundColor Green
  } else {
    Write-Host "[Volo] WARNING: Skipping SHA-256 verification (-NoVerify specified)." -ForegroundColor Yellow
  }

  $exePath = Expand-VoloZip -ZipFile $dlZip -StageDir (Join-Path $tmpDir "extracted")
  return $exePath
}

function Resolve-VoloBinary {
  if ($BinaryPath) {
    if (-not $DryRun -and (-not (Test-Path -LiteralPath $BinaryPath))) {
      throw "Specified binary not found: $BinaryPath"
    }
    $resolvedBin = if (Test-Path -LiteralPath $BinaryPath) { (Resolve-Path -LiteralPath $BinaryPath).Path } else { [System.IO.Path]::GetFullPath($BinaryPath) }
    Detect-SiblingBundleDir $resolvedBin
    return $resolvedBin
  }
  if ($ArchivePath) {
    if ($DryRun -and (-not (Test-Path -LiteralPath $ArchivePath))) {
      Write-Host "[dry-run] Extract local archive '$ArchivePath'"
      return "C:\volo-dry-run-archive.exe"
    }
    if (-not (Test-Path -LiteralPath $ArchivePath)) {
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
    if (Test-Path -LiteralPath $c) {
      $resolvedLocal = (Resolve-Path -LiteralPath $c).Path
      Write-Host "[Volo] Using local binary: $resolvedLocal" -ForegroundColor Cyan
      Detect-SiblingBundleDir $resolvedLocal
      return $resolvedLocal
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
  $existingSvc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  if ($existingSvc) {
    if ((Test-IsNssmService) -and $NssmCmd) {
      & $NssmCmd.Source stop $ServiceName 2>$null | Out-Null
    }
    if ($existingSvc.Status -ne "Stopped") {
      Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    }
  }
  Stop-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue

  if ($TargetExe -and (Test-Path -LiteralPath $TargetExe)) {
    $fullTarget = [System.IO.Path]::GetFullPath($TargetExe)
    for ($i = 0; $i -lt 30; $i++) {
      $stillRunning = $false
      foreach ($proc in @(Get-Process -Name "volo" -ErrorAction SilentlyContinue)) {
        try {
          if ($proc.Path -and [string]::Equals([System.IO.Path]::GetFullPath($proc.Path), $fullTarget, [System.StringComparison]::OrdinalIgnoreCase)) {
            $stillRunning = $true
            break
          }
        } catch {}
      }
      if (-not $stillRunning) { break }
      Start-Sleep -Milliseconds 500
    }
    foreach ($proc in @(Get-Process -Name "volo" -ErrorAction SilentlyContinue)) {
      try {
        if ($proc.Path -and [string]::Equals([System.IO.Path]::GetFullPath($proc.Path), $fullTarget, [System.StringComparison]::OrdinalIgnoreCase)) {
          Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        }
      } catch {}
    }
  } else {
    Start-Sleep -Seconds 1
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
  Remove-Item -LiteralPath $oldExe -Force -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath $newExe -Force -ErrorAction SilentlyContinue
  Copy-Item -LiteralPath $fullSrc -Destination $newExe -Force
  try {
    Move-Item -LiteralPath $newExe -Destination $fullDst -Force
  } catch {
    if (Test-Path -LiteralPath $fullDst) { Move-Item -LiteralPath $fullDst -Destination $oldExe -Force }
    Move-Item -LiteralPath $newExe -Destination $fullDst -Force
    Remove-Item -LiteralPath $oldExe -Force -ErrorAction SilentlyContinue
  }
}

function Backup-LiveBinary([string]$LiveBin) {
  if ($DryRun) {
    Write-Host "[dry-run] Backup live binary '$LiveBin' -> '$LiveBin.bak'"
    return $true
  }
  if (Test-Path -LiteralPath $LiveBin) {
    Copy-Item -LiteralPath $LiveBin -Destination "$LiveBin.bak" -Force
    return $true
  }
  return $false
}

function Invoke-PreUpgradeBackup([string]$LiveBin, [string]$RootDataDir, [string]$CfgEnvFile) {
  if ($SkipBackup) {
    Write-Host "[Volo] Skipping pre-upgrade database backup (-SkipBackup specified)." -ForegroundColor Yellow
    return
  }

  $effDataDir = ""
  $effBackupDir = ""
  $cfgDbDsn = ""
  if ($CfgEnvFile -and (Test-Path -LiteralPath $CfgEnvFile)) {
    $effDataDir = Read-EnvFileValue $CfgEnvFile "VOLO_DATA_DIR"
    $effBackupDir = Read-EnvFileValue $CfgEnvFile "VOLO_BACKUP_DIR"
    $cfgDbDsn = Read-EnvFileValue $CfgEnvFile "VOLO_DB_DSN"
  }
  if ([string]::IsNullOrWhiteSpace($effDataDir)) {
    $subData = Join-Path $RootDataDir "data"
    if ($DryRun -or (Test-Path -LiteralPath $subData)) {
      $effDataDir = $subData
    } else {
      $effDataDir = $RootDataDir
    }
  } else {
    $effDataDir = [System.IO.Path]::GetFullPath($effDataDir.Replace('/', '\'))
  }
  if ([string]::IsNullOrWhiteSpace($effBackupDir)) {
    $effBackupDir = Join-Path $effDataDir "backups"
  } else {
    $effBackupDir = [System.IO.Path]::GetFullPath($effBackupDir.Replace('/', '\'))
  }

  if ($DryRun) {
    Write-Host "[dry-run] Pre-upgrade backup: `"$LiveBin`" backup create --env-file `"$CfgEnvFile`" --data-dir `"$effDataDir`" --backup-dir `"$effBackupDir`""
    return
  }

  if (-not (Test-Path -LiteralPath $LiveBin)) {
    Write-Host "[Volo] No existing binary at '$LiveBin'; skipping pre-upgrade backup." -ForegroundColor Cyan
    return
  }

  $hasSqliteDb = Test-Path -LiteralPath (Join-Path $effDataDir "volo.db")
  $hasDbJson = Test-Path -LiteralPath (Join-Path $effDataDir "db.json")
  if ((-not $hasSqliteDb) -and (-not $hasDbJson) -and [string]::IsNullOrWhiteSpace($cfgDbDsn)) {
    Write-Host "[Volo] Database in '$effDataDir' is not initialized yet; skipping pre-upgrade backup." -ForegroundColor Cyan
    return
  }

  New-Item -ItemType Directory -Force -Path $effBackupDir -ErrorAction SilentlyContinue | Out-Null
  Write-Host "[Volo] Creating pre-upgrade database backup in '$effBackupDir'..." -ForegroundColor Cyan
  $backupArgs = @("backup", "create", "--data-dir", $effDataDir, "--backup-dir", $effBackupDir)
  if ($CfgEnvFile -and (Test-Path -LiteralPath $CfgEnvFile)) {
    $backupArgs = @("backup", "create", "--env-file", $CfgEnvFile, "--data-dir", $effDataDir, "--backup-dir", $effBackupDir)
  }
  $backupOut = (& $LiveBin @backupArgs 2>&1 | Out-String).Trim()
  if ($LASTEXITCODE -eq 0) {
    Write-Host "[Volo] Pre-upgrade backup saved in '$effBackupDir'." -ForegroundColor Green
  } else {
    Write-Host "[Volo] WARNING: Pre-upgrade backup exited with code $LASTEXITCODE ($backupOut); preserving existing data and continuing." -ForegroundColor Yellow
  }
}

function Normalize-ProbeAddress([string]$RawListen) {
  if ([string]::IsNullOrWhiteSpace($RawListen)) { return "127.0.0.1:8080" }
  $trimmed = $RawListen.Trim()
  if ($trimmed.StartsWith(":")) { return "127.0.0.1" + $trimmed }
  if ($trimmed.StartsWith("0.0.0.0:")) { return "127.0.0.1:" + $trimmed.Substring(8) }
  if ($trimmed.StartsWith("[::]:")) { return "127.0.0.1:" + $trimmed.Substring(5) }
  if ($trimmed.StartsWith(":::")) { return "127.0.0.1:" + $trimmed.Substring(3) }
  if ($trimmed.StartsWith("*:")) { return "127.0.0.1:" + $trimmed.Substring(2) }
  return $trimmed
}

function Get-HealthcheckEndpointInfo([string]$CfgEnvFile) {
  $activeSock = ""
  if ($CfgEnvFile -and (Test-Path -LiteralPath $CfgEnvFile)) {
    $activeSock = Read-EnvFileValue $CfgEnvFile "VOLO_LISTEN_SOCKET"
    if ($activeSock -and $activeSock.Contains(",")) {
      $activeSock = $activeSock.Split(",")[0].Trim()
    }
  }
  $cfgListen = ""
  if ($Script:HasExplicitListen) {
    $cfgListen = $EffectiveListen
  } elseif ($CfgEnvFile -and (Test-Path -LiteralPath $CfgEnvFile)) {
    $cfgListen = Read-EnvFileValue $CfgEnvFile "VOLO_LISTEN_ADDR"
  }
  if ([string]::IsNullOrWhiteSpace($cfgListen)) {
    $cfgListen = $EffectiveListen
  }
  $probeAddr = Normalize-ProbeAddress $cfgListen
  return [PSCustomObject]@{
    Socket = $activeSock
    Url    = "http://$probeAddr/healthz"
  }
}

function Invoke-SingleHealthcheckProbe([string]$LiveBin, $EndpointInfo) {
  $hcArgs = if (-not [string]::IsNullOrWhiteSpace($EndpointInfo.Socket)) {
    @("healthcheck", "--listen-socket", $EndpointInfo.Socket)
  } else {
    @("healthcheck", "--url", $EndpointInfo.Url)
  }
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $LiveBin
  $psi.Arguments = ($hcArgs | ForEach-Object { Format-Win32Arg $_ }) -join " "
  $psi.UseShellExecute = $false
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.CreateNoWindow = $true
  foreach ($proxyKey in @("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy")) {
    if ($psi.EnvironmentVariables.ContainsKey($proxyKey)) {
      $psi.EnvironmentVariables[$proxyKey] = ""
    }
  }
  $psi.EnvironmentVariables["NO_PROXY"] = "*"
  $psi.EnvironmentVariables["no_proxy"] = "*"

  $proc = [System.Diagnostics.Process]::Start($psi)
  [void]$proc.StandardOutput.ReadToEnd()
  [void]$proc.StandardError.ReadToEnd()
  if (-not $proc.WaitForExit(5000)) {
    try { $proc.Kill() } catch {}
    return $false
  }
  return ($proc.ExitCode -eq 0)
}

function Test-VoloHealthcheck([string]$LiveBin, [string]$CfgEnvFile, [int]$TimeoutSeconds = 15) {
  $ep = Get-HealthcheckEndpointInfo -CfgEnvFile $CfgEnvFile
  if ($DryRun) {
    if ($ep.Socket) {
      Write-Host "[dry-run] Poll `"$LiveBin`" healthcheck --listen-socket `"$($ep.Socket)`" (up to ${TimeoutSeconds}s; auto-rollback '$LiveBin.bak' on failure)"
    } else {
      Write-Host "[dry-run] Poll `"$LiveBin`" healthcheck --url `"$($ep.Url)`" (up to ${TimeoutSeconds}s; auto-rollback '$LiveBin.bak' on failure)"
    }
    return $true
  }
  $attempts = [Math]::Max(1, $TimeoutSeconds)
  for ($i = 1; $i -le $attempts; $i++) {
    if (Invoke-SingleHealthcheckProbe -LiveBin $LiveBin -EndpointInfo $ep) {
      return $true
    }
    if ($i -lt $attempts) {
      Start-Sleep -Seconds 1
    }
  }
  return $false
}

function Register-OrUpdateNativeService([string]$TargetExe, [string]$CfgEnvFile) {
  if ($DryRun) {
    Write-Host "[dry-run] Register/update native Windows service '$ServiceName' via sc.exe (binPath: `"`"$TargetExe`" serve --env-file `"$CfgEnvFile`"`")"
    return
  }
  Unregister-ScheduledTask -TaskName $ServiceName -Confirm:$false -ErrorAction SilentlyContinue

  $svcCmdLine = "`"$TargetExe`" serve --env-file `"$CfgEnvFile`""
  $acctArgs = @()
  if ($ServiceAccount -match '^(?i)(LocalSystem|SYSTEM|NT AUTHORITY\\SYSTEM)$') {
    $acctArgs = @("obj=", "LocalSystem")
  } elseif ([string]::IsNullOrEmpty($ServicePassword)) {
    $acctArgs = @("obj=", $ServiceAccount, "password=", "")
  } else {
    $acctArgs = @("obj=", $ServiceAccount, "password=", $ServicePassword)
  }

  $existingSvc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  if (-not $existingSvc) {
    $createArgs = @("create", $ServiceName, "binPath=", $svcCmdLine, "start=", "auto", "DisplayName=", "Volo-go ($ServiceName)") + $acctArgs
    [void](Invoke-ScExe -ScArgs $createArgs)
  } else {
    $configArgs = @("config", $ServiceName, "binPath=", $svcCmdLine, "start=", "auto", "DisplayName=", "Volo-go ($ServiceName)") + $acctArgs
    [void](Invoke-ScExe -ScArgs $configArgs)
  }
  [void](Invoke-ScExe -ScArgs @("description", $ServiceName, "Volo-go Blog Service ($CfgEnvFile)") -IgnoreExitCode)
  [void](Invoke-ScExe -ScArgs @("failure", $ServiceName, "reset=", "86400", "actions=", "restart/5000/restart/5000/restart/10000") -IgnoreExitCode)
  [void](Invoke-ScExe -ScArgs @("failureflag", $ServiceName, "1") -IgnoreExitCode)
}

function Register-OrUpdateNssmService([string]$TargetExe, [string]$WorkDir, [string]$CfgEnvFile) {
  if ($DryRun) {
    Write-Host "[dry-run] Register/update NSSM Windows service '$ServiceName' (Application: '$TargetExe', AppParameters: 'serve --env-file `"$CfgEnvFile`"')"
    return
  }
  if (-not $NssmCmd) {
    throw "NSSM service mode was requested, but 'nssm.exe' was not found on PATH. Omit -UseNssm / -ServiceManager Nssm to use native sc.exe registration."
  }
  Unregister-ScheduledTask -TaskName $ServiceName -Confirm:$false -ErrorAction SilentlyContinue

  $existingSvc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  if ($existingSvc -and (-not (Test-IsNssmService))) {
    [void](Invoke-ScExe -ScArgs @("delete", $ServiceName) -IgnoreExitCode)
    Start-Sleep -Seconds 1
    $existingSvc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  }

  $appParams = "serve --env-file `"$CfgEnvFile`""
  if (-not $existingSvc) {
    & $NssmCmd.Source install $ServiceName $TargetExe "serve" "--env-file" $CfgEnvFile | Out-Null
    if ($LASTEXITCODE -ne 0) {
      throw "nssm install failed for service '$ServiceName' (exit code $LASTEXITCODE)."
    }
  }
  # Idempotently synchronize Application, AppParameters, AppDirectory, log files, start type, and account on both install & re-install/upgrade
  & $NssmCmd.Source set $ServiceName Application $TargetExe | Out-Null
  & $NssmCmd.Source set $ServiceName AppParameters $appParams | Out-Null
  & $NssmCmd.Source set $ServiceName AppDirectory $WorkDir | Out-Null
  $logDir = Join-Path $WorkDir "logs"
  New-Item -ItemType Directory -Force -Path $logDir | Out-Null
  & $NssmCmd.Source set $ServiceName AppStdout (Join-Path $logDir "volo.out.log") | Out-Null
  & $NssmCmd.Source set $ServiceName AppStderr (Join-Path $logDir "volo.err.log") | Out-Null
  & $NssmCmd.Source set $ServiceName AppRotateFiles 1 | Out-Null
  & $NssmCmd.Source set $ServiceName Start SERVICE_AUTO_START | Out-Null
  if ($ServiceAccount -and ($ServiceAccount -notmatch '^(?i)(LocalSystem|SYSTEM|NT AUTHORITY\\SYSTEM)$')) {
    if ([string]::IsNullOrEmpty($ServicePassword)) {
      & $NssmCmd.Source set $ServiceName ObjectName $ServiceAccount '""' | Out-Null
    } else {
      & $NssmCmd.Source set $ServiceName ObjectName $ServiceAccount $ServicePassword | Out-Null
    }
  }
}

function Register-OrUpdateScheduledTask([string]$TargetExe, [string]$WorkDir, [string]$CfgEnvFile) {
  if ($DryRun) {
    Write-Host "[dry-run] Register/update Windows Scheduled Task '$ServiceName'"
    return
  }
  $TaskAction = New-ScheduledTaskAction -Execute $TargetExe -Argument "serve --env-file `"$CfgEnvFile`"" -WorkingDirectory $WorkDir
  $TaskTrigger = New-ScheduledTaskTrigger -AtStartup
  $TaskSettings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
  $Principal = New-ScheduledTaskPrincipal -UserId $ServiceAccount -LogonType ServiceAccount -RunLevel Highest
  Register-ScheduledTask -TaskName $ServiceName -Action $TaskAction -Trigger $TaskTrigger -Settings $TaskSettings -Principal $Principal -Force | Out-Null
}

function Start-VoloRegisteredService {
  if ($DryRun) {
    Write-Host "[dry-run] Start service/task '$ServiceName'"
    return
  }
  if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    Start-Service -Name $ServiceName
    return
  }
  $existingTask = Get-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue
  if ($existingTask) {
    Start-ScheduledTask -TaskName $ServiceName
    return
  }
  throw "Service or Scheduled Task '$ServiceName' is not registered."
}

function Show-VoloLogs([string]$RootDataDir, [string]$CfgEnvFile) {
  if ($DryRun) {
    Write-Host "[dry-run] Inspect logs for '$ServiceName' ($RootDataDir\logs, NSSM stdout/stderr, and Windows System/Application Event Logs; tail $LogLines)"
    return
  }

  $candidateFiles = New-Object System.Collections.Generic.List[string]
  if ((Test-IsNssmService) -and $NssmCmd) {
    foreach ($field in @("AppStdout", "AppStderr")) {
      $p = (& $NssmCmd.Source get $ServiceName $field 2>$null | Out-String).Trim("`r", "`n", "`0", " ", '"')
      if ($p -and (Test-Path -LiteralPath $p) -and (-not $candidateFiles.Contains($p))) {
        $candidateFiles.Add($p)
      }
    }
  }

  $logDirs = New-Object System.Collections.Generic.List[string]
  $rootLogs = Join-Path $RootDataDir "logs"
  if (Test-Path -LiteralPath $rootLogs) { $logDirs.Add($rootLogs) }
  if ($CfgEnvFile -and (Test-Path -LiteralPath $CfgEnvFile)) {
    $cfgData = Read-EnvFileValue $CfgEnvFile "VOLO_DATA_DIR"
    if ($cfgData) {
      $cfgDataLogs = Join-Path ([System.IO.Path]::GetFullPath($cfgData.Replace('/', '\'))) "logs"
      if ((Test-Path -LiteralPath $cfgDataLogs) -and (-not $logDirs.Contains($cfgDataLogs))) {
        $logDirs.Add($cfgDataLogs)
      }
    }
  }

  foreach ($ld in $logDirs) {
    foreach ($lf in @(Get-ChildItem -LiteralPath $ld -File -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending)) {
      if (-not $candidateFiles.Contains($lf.FullName)) {
        $candidateFiles.Add($lf.FullName)
      }
    }
  }

  if ($Follow -and ($candidateFiles.Count -gt 0)) {
    $followTarget = $candidateFiles[0]
    Write-Host "[Volo] Following log file: $followTarget (Ctrl+C to stop)..." -ForegroundColor Cyan
    Get-Content -LiteralPath $followTarget -Tail $LogLines -Wait
    return
  }

  $printedAny = $false
  foreach ($fileItem in $candidateFiles) {
    Write-Host "=== File Log: $fileItem (last $LogLines lines) ===" -ForegroundColor Cyan
    Get-Content -LiteralPath $fileItem -Tail $LogLines -ErrorAction SilentlyContinue
    $printedAny = $true
  }

  Write-Host "=== Windows Event Log (Application & System for '$ServiceName') ===" -ForegroundColor Cyan
  $escapedSvc = [Regex]::Escape($ServiceName)
  $appEvents = @(Get-WinEvent -FilterHashtable @{ LogName = "Application" } -MaxEvents 250 -ErrorAction SilentlyContinue |
    Where-Object { ($_.ProviderName -match "(?i)^($escapedSvc|volo)$") -or ($_.Message -match "(?i)\b$escapedSvc\b") } |
    Select-Object -First $LogLines)
  if ($appEvents.Count -gt 0) {
    $appEvents | Format-Table TimeCreated, LevelDisplayName, ProviderName, Id, Message -Wrap -AutoSize
    $printedAny = $true
  }

  $sysEvents = @(Get-WinEvent -FilterHashtable @{ LogName = "System"; ProviderName = "Service Control Manager" } -MaxEvents 250 -ErrorAction SilentlyContinue |
    Where-Object { $_.Message -match "(?i)\b$escapedSvc\b" } |
    Select-Object -First ([Math]::Min($LogLines, 30)))
  if ($sysEvents.Count -gt 0) {
    $sysEvents | Format-Table TimeCreated, LevelDisplayName, Id, Message -Wrap -AutoSize
    $printedAny = $true
  }

  if (-not $printedAny) {
    Write-Host "[Volo] No log files or matching Windows Event Log entries found yet for '$ServiceName' (checked '$RootDataDir\logs' and Event Viewer)." -ForegroundColor Yellow
  }
}

function Resolve-IcaclsPrincipal([string]$Identity) {
  if ([string]::IsNullOrWhiteSpace($Identity)) { return "" }
  switch -Regex ($Identity.Trim()) {
    '^(NT AUTHORITY\\)?SYSTEM$|^LocalSystem$' { return "*S-1-5-18" }
    '^(BUILTIN\\)?Administrators$'            { return "*S-1-5-32-544" }
    '^(NT AUTHORITY\\)?NetworkService$'       { return "*S-1-5-20" }
    '^(NT AUTHORITY\\)?LocalService$'         { return "*S-1-5-19" }
    default                                   { return $Identity.Trim() }
  }
}

function Set-DataDirPermissions([string]$TargetDir, [string]$Account, [string]$Group) {
  if ($DryRun) {
    Write-Host "[dry-run] Restrict NTFS ACLs on $TargetDir (SID-based icacls)"
    return
  }
  & icacls $TargetDir /inheritance:r | Out-Null
  & icacls $TargetDir /grant:r "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F" | Out-Null
  $acctSpec = Resolve-IcaclsPrincipal $Account
  if ($acctSpec -and ($acctSpec -ne "*S-1-5-18") -and ($acctSpec -ne "*S-1-5-32-544")) {
    & icacls $TargetDir /grant:r "${acctSpec}:(OI)(CI)M" | Out-Null
  }
  $grpSpec = Resolve-IcaclsPrincipal $Group
  if ($grpSpec -and ($grpSpec -ne "*S-1-5-18") -and ($grpSpec -ne "*S-1-5-32-544")) {
    & icacls $TargetDir /grant:r "${grpSpec}:(OI)(CI)RX" | Out-Null
  }
}

function Set-SecretEnvFilePermissions([string]$TargetFile, [string]$Account) {
  if ($DryRun) {
    Write-Host "[dry-run] Restrict secret file NTFS ACLs on $TargetFile (SYSTEM, Administrators, and service account read-only)"
    return
  }
  if (-not (Test-Path -LiteralPath $TargetFile)) {
    return
  }
  & icacls $TargetFile /inheritance:r | Out-Null
  & icacls $TargetFile /grant:r "*S-1-5-18:F" "*S-1-5-32-544:F" | Out-Null
  $acctSpec = Resolve-IcaclsPrincipal $Account
  if ($acctSpec -and ($acctSpec -ne "*S-1-5-18") -and ($acctSpec -ne "*S-1-5-32-544")) {
    & icacls $TargetFile /grant:r "${acctSpec}:R" | Out-Null
  }
}

try {
  $DestBin = [System.IO.Path]::GetFullPath((Join-Path $InstallDir "volo.exe"))
  switch ($Action) {
    "Install" {
      $SrcBin = Resolve-VoloBinary
      Test-VoloBinarySmoke -CandidateExe $SrcBin
      Stop-VoloServiceAndWait -TargetExe $DestBin
      if (-not $DryRun) {
        foreach ($sub in @("data\uploads", "data\backups", "data\geoip", "plugins", "themes", "locales", "logs")) {
          New-Item -ItemType Directory -Force -Path (Join-Path $DataDir $sub) | Out-Null
        }
        $envParentDir = Split-Path -Parent $EnvFile
        if ($envParentDir) {
          New-Item -ItemType Directory -Force -Path $envParentDir | Out-Null
        }
      }
      Sync-BundledExtensions -DestRoot $DataDir -CfgEnvFile $EnvFile
      Set-DataDirPermissions -TargetDir $DataDir -Account $ServiceAccount -Group $PermissionGroup
      Install-BinarySafe -SourceExe $SrcBin -TargetExe $DestBin

      if (-not (Test-Path -LiteralPath $EnvFile)) {
        $SecretHex = if ($DryRun) { "<redacted>" } else { New-RandomHex32 }
        $EnvLines = @(
          "# Generated by Volo-go install-service.ps1",
          "VOLO_LISTEN_ADDR=$EffectiveListen",
          "VOLO_PUBLIC_URL=$PublicUrl",
          "VOLO_TRUSTED_PROXIES=$TrustedProxies",
          "VOLO_ADMIN_PATH=$AdminPath",
          "VOLO_SECRET_KEY=$SecretHex",
          "VOLO_GEO_ACCESS_ENABLED=true",
          "",
          "# Leave DB driver/DSN commented out so Setup Wizard (data/db.json) controls DB selection unless explicitly overridden:",
          "# VOLO_DB_DRIVER=sqlite",
          "# VOLO_DB_DSN=$($DataDir -replace '\\','/')/data/volo.db",
          "",
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
          Set-Content -LiteralPath $EnvFile -Value $EnvLines -Encoding UTF8
        }
      }
      Set-SecretEnvFilePermissions -TargetFile $EnvFile -Account $ServiceAccount

      $effMgr = Resolve-EffectiveServiceManager -RequestedMode $ServiceManager
      switch ($effMgr) {
        "Nssm"          { Register-OrUpdateNssmService -TargetExe $DestBin -WorkDir $DataDir -CfgEnvFile $EnvFile }
        "ScheduledTask" { Register-OrUpdateScheduledTask -TargetExe $DestBin -WorkDir $DataDir -CfgEnvFile $EnvFile }
        default         { Register-OrUpdateNativeService -TargetExe $DestBin -CfgEnvFile $EnvFile }
      }

      if (-not $NoStart) {
        Start-VoloRegisteredService
      }
      Write-Host "[Volo] Installed Windows service '$ServiceName' (mode: $effMgr)" -ForegroundColor Green
      Write-Host "  Binary   : $DestBin"
      Write-Host "  Config   : $EnvFile"
      Write-Host "  Data Dir : $DataDir"
      Write-Host "  Listen   : http://$EffectiveListen"
    }
    "Upgrade" {
      Resolve-ExistingRegistration ([ref]$DestBin) ([ref]$DataDir) ([ref]$EnvFile)
      $SrcBin = Resolve-VoloBinary
      Test-VoloBinarySmoke -CandidateExe $SrcBin
      Invoke-PreUpgradeBackup -LiveBin $DestBin -RootDataDir $DataDir -CfgEnvFile $EnvFile
      Stop-VoloServiceAndWait -TargetExe $DestBin
      $hadPrevBin = Backup-LiveBinary -LiveBin $DestBin
      if ($DryRun -or (Test-Path -LiteralPath $DataDir)) {
        Sync-BundledExtensions -DestRoot $DataDir -CfgEnvFile $EnvFile
      }
      Install-BinarySafe -SourceExe $SrcBin -TargetExe $DestBin

      # Keep service registration parameters synchronized (e.g. NSSM AppParameters or explicit ServiceManager migration)
      $effUpgradeMgr = Resolve-EffectiveServiceManager -RequestedMode $ServiceManager -ForUpgrade
      if (-not $DryRun) {
        if (($effUpgradeMgr -eq "Nssm") -and $NssmCmd -and (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) {
          Register-OrUpdateNssmService -TargetExe $DestBin -WorkDir $DataDir -CfgEnvFile $EnvFile
        } elseif (($ServiceManager -eq "Native") -or ((Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) -and (-not (Test-IsNssmService)) -and ($Script:HasExplicitInstallDir -or $Script:HasExplicitEnvFile))) {
          Register-OrUpdateNativeService -TargetExe $DestBin -CfgEnvFile $EnvFile
        } elseif (($ServiceManager -eq "ScheduledTask") -and (Get-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue)) {
          Register-OrUpdateScheduledTask -TargetExe $DestBin -WorkDir $DataDir -CfgEnvFile $EnvFile
        }
      }

      if ($DryRun) {
        if (-not $NoStart) {
          Start-VoloRegisteredService
          [void](Test-VoloHealthcheck -LiveBin $DestBin -CfgEnvFile $EnvFile -TimeoutSeconds 15)
        }
      } elseif (-not $NoStart) {
        $startOk = $false
        $startErr = ""
        try {
          Start-VoloRegisteredService
          $startOk = $true
        } catch {
          $startErr = $_.Exception.Message
        }

        $hcOk = $false
        if ($startOk) {
          $hcOk = Test-VoloHealthcheck -LiveBin $DestBin -CfgEnvFile $EnvFile -TimeoutSeconds 15
        }

        if ((-not $startOk) -or (-not $hcOk)) {
          $diagDetail = if ($startErr) { "start error: $startErr" } else { "healthcheck timed out after 15s" }
          Write-Host "[Volo ERROR] Upgraded service '$ServiceName' failed post-upgrade verification ($diagDetail)!" -ForegroundColor Red
          Stop-VoloServiceAndWait -TargetExe $DestBin
          if ($hadPrevBin -and (Test-Path -LiteralPath "$DestBin.bak")) {
            Write-Host "[Volo] Rolling back '$DestBin.bak' -> '$DestBin' and restarting previous version..." -ForegroundColor Yellow
            Install-BinarySafe -SourceExe "$DestBin.bak" -TargetExe $DestBin
            try {
              Start-VoloRegisteredService
              Write-Host "[Volo] Previous binary restored and service '$ServiceName' restarted." -ForegroundColor Yellow
            } catch {
              Write-Host "[Volo ERROR] Rollback restart also encountered an error: $($_.Exception.Message)" -ForegroundColor Red
            }
            throw "Upgrade aborted for '$ServiceName' ($diagDetail). Rolled back '$DestBin.bak' -> '$DestBin' and restarted previous version."
          }
          throw "Upgrade aborted for '$ServiceName' ($diagDetail), and no previous backup '$DestBin.bak' was found for rollback."
        }

        Write-Host "[Volo] Upgraded, verified healthcheck, and restarted '$ServiceName' ($DestBin)" -ForegroundColor Green
        & $DestBin version
      } else {
        Write-Host "[Volo] Upgraded binary '$DestBin' (-NoStart specified; skipped restart and healthcheck)." -ForegroundColor Green
      }
    }
    "Uninstall" {
      Resolve-ExistingRegistration ([ref]$DestBin) ([ref]$DataDir) ([ref]$EnvFile)
      Stop-VoloServiceAndWait -TargetExe $DestBin
      if ($DryRun) {
        Write-Host "[dry-run] Uninstall '$ServiceName' (Binary: $DestBin, DataDir: $DataDir, PurgeData: $PurgeData)"
      } else {
        if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
          if ((Test-IsNssmService) -and $NssmCmd) {
            & $NssmCmd.Source remove $ServiceName confirm 2>$null | Out-Null
          }
          if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
            [void](Invoke-ScExe -ScArgs @("delete", $ServiceName) -IgnoreExitCode)
          }
        }
        Unregister-ScheduledTask -TaskName $ServiceName -Confirm:$false -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $DestBin -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath "$DestBin.bak" -Force -ErrorAction SilentlyContinue
        if ($PurgeData) {
          Assert-SafePurgeDir $DataDir
          if ($EnvFile -and (Test-Path -LiteralPath $EnvFile)) {
            Remove-Item -LiteralPath $EnvFile -Force -ErrorAction SilentlyContinue
          }
          Remove-Item -LiteralPath $DataDir -Recurse -Force -ErrorAction SilentlyContinue
        }
        Write-Host "[Volo] Uninstalled service '$ServiceName' (DataDir: $DataDir)" -ForegroundColor Green
      }
    }
    "Start" {
      Start-VoloRegisteredService
    }
    "Stop" {
      Resolve-ExistingRegistration ([ref]$DestBin) ([ref]$DataDir) ([ref]$EnvFile)
      Stop-VoloServiceAndWait -TargetExe $DestBin
    }
    "Restart" {
      Resolve-ExistingRegistration ([ref]$DestBin) ([ref]$DataDir) ([ref]$EnvFile)
      Stop-VoloServiceAndWait -TargetExe $DestBin
      Start-VoloRegisteredService
    }
    "Status" {
      if ($DryRun) {
        Write-Host "[dry-run] Query Windows Service / Scheduled Task status for '$ServiceName'"
      } else {
        $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($svc) {
          $svc | Format-Table Name, Status, StartType -AutoSize
        } else {
          $task = Get-ScheduledTask -TaskName $ServiceName -ErrorAction SilentlyContinue
          if ($task) {
            $task | Select-Object TaskName, State | Format-Table -AutoSize
          } else {
            Write-Host "[Volo] Service '$ServiceName' is not registered." -ForegroundColor Yellow
          }
        }
      }
    }
    "Logs" {
      Resolve-ExistingRegistration ([ref]$DestBin) ([ref]$DataDir) ([ref]$EnvFile)
      Show-VoloLogs -RootDataDir $DataDir -CfgEnvFile $EnvFile
    }
  }
} finally {
  if ($Script:TempDownloadDir -and (Test-Path -LiteralPath $Script:TempDownloadDir)) {
    Remove-Item -LiteralPath $Script:TempDownloadDir -Recurse -Force -ErrorAction SilentlyContinue
  }
}
