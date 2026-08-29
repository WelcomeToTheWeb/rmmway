# RMMWay one-line bootstrap installer (W1-3) - Windows.
#
#   iwr -useb https://raw.githubusercontent.com/welcometotheweb/rmmway/Phase-1/scripts/install.ps1 | iex
#
# With arguments (recommended):
#   iwr -useb https://raw.githubusercontent.com/welcometotheweb/rmmway/Phase-1/scripts/install.ps1 -OutFile install.ps1
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Server https://rmm.example.com -Bootstrap <TOKEN>
#
# What it does: detect arch (amd64), download the static agent from the GitHub
# release, install to %ProgramFiles%\RMMWay\ (or %LOCALAPPDATA%\RmmWay when not
# elevated), write a 0600-style ACL'd config, and register a Windows Service
# via sc.exe.
#
# Only -Server and -Bootstrap are required. The agent enrolls over the
# server's HTTPS origin (POST {server}/agent/enroll), then streams over the
# mTLS gRPC port (default 50052 on the server host) - so from the device you
# only need the server host + that port reachable (the plain gRPC bootstrap
# port, 50051, stays internal).

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
    Log "not elevated - installing to $installDir"
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
if (-not $isAdmin -and ($env:PATH -notmatch [regex]::Escape($installDir))) {
    $env:PATH = "$installDir;$env:PATH"
    [Environment]::SetEnvironmentVariable("PATH", "$installDir;$env:PATH", "User")
    Log "added $installDir to the user PATH"
}

# --- write config (restricted ACL) ------------------------------------------
$cfg  = Join-Path $installDir "agent.env"
# NB: the device id is minted at enroll - the agent does not read an
# RMMWAY_DEVICE_ID key, so writing one here only invites drift.
$lines = @(
    "RMMWAY_SERVER={0}"          -f $(if ($Server) { $Server } else { "https://rmm.local" })
    "RMMWAY_BOOTSTRAP_TOKEN={0}" -f $Bootstrap
)
Set-Content -Path $cfg -Value $lines -Encoding ascii
# Restrict the config to the current user + Administrators (the token lives here).
# Best-effort hardening: a failure here (e.g. the AccessControl assembly is not
# loaded on this PowerShell build) must not abort the install - the config is
# already written and readable by the service.
try {
    # Resolve the type in a cast context first so PowerShell loads the containing
    # assembly (System.AccessControl on .NET/PS7, System on .NET FW/PS5.1). New-Object
    # with the bare string name does not always trigger that load.
    $facr = [System.AccessControl.FileSystemAccessRule]
    $acl = Get-Acl $cfg
    $acl.SetAccessRuleProtection($true, $false)
    # NB: a bare command (New-Object) is NOT valid as a direct argument inside a
    # .NET method call ( ... ). Build each rule into a variable first, then add it.
    $userRule  = New-Object $facr "$env:USERDOMAIN\$env:USERNAME", "FullControl", "Allow"
    $adminRule = New-Object $facr "Administrators", "FullControl", "Allow"
    $acl.AddAccessRule($userRule)
    $acl.AddAccessRule($adminRule)
    Set-Acl -Path $cfg -AclObject $acl
    Log "config -> $cfg (ACL restricted)"
} catch {
    Log "config -> $cfg (written; ACL restriction skipped: $($_.Exception.Message))"
}

# --- register + start the Windows service -----------------------------------
# The agent is a real Windows service (it reports the SCM handshake on start),
# so Start-Service succeeds even before the agent has enrolled/connected.
$svc = "RmmWayAgent"
# sc.exe parses the binPath value itself: quote the two paths, leave the
# arguments unquoted -> "`"$bin`" run --config `"$cfg`"". The command is then
# passed as an ARRAY below: each element becomes a separate argv entry
# (a single joined string would fail with an invalid parameter).
$binPath = "`"$bin`" run --config `"$cfg`""
if (Get-Service -Name $svc -ErrorAction SilentlyContinue) {
    # Re-run with a new binary/config path: update binPath, not just restart,
    # or the change never takes effect.
    & sc.exe config $svc binPath= $binPath | Out-Null
    if ($LASTEXITCODE -ne 0) { Log "WARNING: sc.exe config failed: binPath unchanged" }
    Log "service $svc already exists - updating binPath + restarting"
    try { Restart-Service $svc -ErrorAction Stop } catch { Log "WARNING: restart failed: $($_.Exception.Message)" }
} else {
    # & with an array splats each element as its own argument; passing the
    # whole command as one string makes sc.exe fail with an invalid parameter.
    $scArgs = @("create", $svc, "binPath=", $binPath, "start=", "auto")
    & sc.exe @scArgs | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Die "sc.exe create failed (re-run as Administrator)"
    }
    Log "service $svc registered"
    try {
        Start-Service $svc -ErrorAction Stop
    } catch {
        Log "WARNING: service did not start: $($_.Exception.Message)"
        Log "  run the agent in the foreground to see the real error:"
        Log "    & `"$bin`" run --config `"$cfg`""
        Log "  and check the Application log (eventvwr.msc)."
    }
}
# If the agent process ever crashes, auto-restart it (retry in 30s, reset the
# failure counter after a day). Best-effort hardening.
& sc.exe failure $svc reset= 86400 actions= restart/30000 | Out-Null
$st = Get-Service -Name $svc -ErrorAction SilentlyContinue
if ($st) { Log "service status: $($st.Status)" }

Log "done. agent $verOut installed."
Log "  binary : $bin"
Log "  config : $cfg"
Log "  service: $svc"
Log "note: the agent enrolls over the server's HTTPS origin, then streams over"
Log "      the mTLS gRPC port (default 50052 on the server host). Only that"
Log "      host + port need to be reachable from this machine."
