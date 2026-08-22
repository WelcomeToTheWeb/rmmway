# RMMWay one-line bootstrap installer (W1-3) — Windows.
#
#   iwr -useb https://raw.githubusercontent.com/welcometotheweb/rmmway/main/scripts/install.ps1 | iex
#
# With arguments (recommended):
#   iwr -useb https://raw.githubusercontent.com/welcometotheweb/rmmway/main/scripts/install.ps1 -OutFile install.ps1
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Server https://rmm.example.com -Bootstrap <TOKEN>
#
# What it does: detect arch (amd64), download the static agent from the GitHub
# release, install to %ProgramFiles%\RMMWay\ (or %LOCALAPPDATA%\RmmWay when not
# elevated), write a 0600-style ACL'd config, and register a Windows Service
# via sc.exe. Enrollment + transport land in W1-4.

[CmdletBinding()]
param(
    [string]$Server = "",
    [string]$Bootstrap = "",
    [string]$Version = "latest",
    [string]$Repo = "welcometotheweb/rmmway"
)

$ErrorActionPreference = "Stop"

function Log($msg) { Write-Host "==> $msg" }
function Die($msg) { Write-Error "ERROR: $msg"; exit 1 }

# --- detect arch ------------------------------------------------------------
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
Log "OS=windows ARCH=$arch release=$Version"

# --- resolve release tag ----------------------------------------------------
$github = "https://api.github.com"
$rawdl  = "https://github.com/$Repo/releases/download"
if ($Version -eq "latest") {
    $rel = Invoke-RestMethod "$github/repos/$Repo/releases/latest"
    $Version = $rel.tag_name
    Log "resolved latest: $Version"
}
$asset = "rmmway-agent-windows-$arch.exe"
$url   = "$rawdl/$Version/$asset"
Log "asset: $url"

# --- pick install dir (elevated -> ProgramFiles, else LocalAppData) ---------
$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()
    ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if ($isAdmin) {
    $installDir = "C:\Program Files\RMMWay"
    $bin        = "$installDir\rmmway-agent.exe"
} else {
    $installDir = "$env:LOCALAPPDATA\RmmWay"
    $bin        = "$installDir\rmmway-agent.exe"
    Log "not elevated — installing to $installDir"
}
New-Item -ItemType Directory -Force -Path $installDir | Out-Null

# --- download (temp first, atomic move) -------------------------------------
$tmp = Join-Path $env:TEMP ("rmmway-agent-" + [guid]::NewGuid().ToString("N") + ".exe")
Log "downloading agent ..."
try {
    Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing
} catch {
    Remove-Item $tmp -ErrorAction SilentlyContinue
    Die "download failed from $url ($($_.Exception.Message))"
}

# sanity: run --version before replacing any existing binary
try {
    $verOut = & $tmp --version
} catch {
    Remove-Item $tmp -ErrorAction SilentlyContinue
    Die "downloaded binary will not run: $($_.Exception.Message)"
}
Log "verified: $verOut"
Copy-Item $tmp $bin -Force
Remove-Item $tmp
Log "installed -> $bin"
if (-not $isAdmin -and -not (Test-Path "env:PATH") -and ($env:PATH -notmatch [regex]::Escape($installDir))) {
    $env:PATH = "$installDir;$env:PATH"
    [Environment]::SetEnvironmentVariable("PATH", "$installDir;$env:PATH", "User")
    Log "added $installDir to the user PATH"
}

# --- write config (restricted ACL) ------------------------------------------
$cfg  = Join-Path $installDir "agent.env"
$lines = @(
    "RMMWAY_SERVER={0}"          -f $(if ($Server) { $Server } else { "https://rmm.local" })
    "RMMWAY_BOOTSTRAP_TOKEN={0}" -f $Bootstrap
    "RMMWAY_DEVICE_ID={0}"       -f $env:COMPUTERNAME
)
Set-Content -Path $cfg -Value $lines -Encoding ascii
# Restrict the config to the current user + Administrators (the token lives here).
$acl = Get-Acl $cfg
$acl.SetAccessRuleProtection($true, $false)
$rule = New-Object System.AccessControl.FileSystemAccessRule("$env:USERDOMAIN\$env:USERNAME", "FullControl", "Allow")
$admins = New-Object System.AccessControl.FileSystemAccessRule("Administrators", "FullControl", "Allow")
$acl.SetAccessRule($rule)
$acl.SetAccessRule($admins)
Set-Acl -Path $cfg -AclObject $acl
Log "config -> $cfg (ACL restricted)"

# --- register + start the Windows service -----------------------------------
$svc = "RmmWayAgent"
$exe = "`"$bin`" run --config `"$cfg`""
if (Get-Service -Name $svc -ErrorAction SilentlyContinue) {
    Log "service $svc already exists — restarting"
    Restart-Service $svc -ErrorAction SilentlyContinue
} else {
    $scArgs = "create $svc binPath= $exe start= auto"
    & sc.exe $scArgs | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Die "sc.exe create failed (re-run as Administrator)"
    }
    Log "service $svc registered"
}
Start-Service $svc -ErrorAction SilentlyContinue
$st = Get-Service -Name $svc -ErrorAction SilentlyContinue
if ($st) { Log "service status: $($st.Status)" }

Log "done. agent $verOut installed."
Log "  binary : $bin"
Log "  config : $cfg"
Log "  service: $svc"
Log "note: enrollment + the connect/run loop land in W1-4; the binary and"
Log "      service are in place now."
