# RMMWay one-line bootstrap installer (W1-3) - Windows.
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

# --- integrity: the asset must match the release's published SHA256SUMS ---
# The v0.4.0 windows asset was once hot-swapped without re-signing (the
# .minisig and SHA256SUMS stayed stale) - refuse to install any asset whose
# hash does not match the published sums, so an unsigned swap fails loud
# instead of reaching the endpoint. (Full minisign verification needs the
# minisign tool and remains the gold standard; the checksum is a cheap guard.)
try {
    $sumsText = (Invoke-WebRequest -Uri "$rawdl/$Version/SHA256SUMS" -UseBasicParsing).Content
    $wantLine = ($sumsText -split "`n") | Where-Object { $_ -match [regex]::Escape("rmmway-agent-windows-$arch.exe") } | Select-Object -First 1
    if ($wantLine) {
        $want = ($wantLine -split "\s+")[0].Trim()
        $hasher = [System.Security.Cryptography.SHA256]::Create()
        $got = [BitConverter]::ToString($hasher.ComputeHash([System.IO.File]::ReadAllBytes($tmp))).ToLower().Replace("-", "")
        if ($want -cne $got) {
            Remove-Item $tmp -ErrorAction SilentlyContinue
            Die "SHA256 mismatch for rmmway-agent-windows-$arch.exe: release says $want, download is $got. The release asset does not match its published sums (unsigned hot-swap?) - not installing. Ask the operator to cut a fresh signed release."
        }
        Log "sha256 verified: $got"
    } else {
        Log "no SHA256SUMS entry for this asset - skipping checksum check"
    }
} catch {
    Log "WARNING: could not verify SHA256SUMS ($($_.Exception.Message)) - continuing without the checksum check"
}

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
# binPath is written DIRECTLY to the registry (not via sc.exe): sc.exe +
# PowerShell native-argument quoting mangles the embedded quotes (the path
# "C:\Program Files\..." loses its quotes and the SCM cannot find the exe,
# so Start-Service fails with a generic "Cannot start service" error). The
# registry value is the source of truth - set it verbatim, then verify it
# round-trips before we let the SCM use it.
$binPath = "`"$bin`" run --config `"$cfg`""
$svcKey  = "HKLM:\SYSTEM\CurrentControlSet\Services\$svc"
function Set-SvcBinPath {
    param([string]$Value)
    Set-ItemProperty -Path $svcKey -Name BinPath -Value $Value
    $stored = (Get-ItemProperty -Path $svcKey -Name BinPath).BinPath
    if ($stored -cne $Value) {
        Die "binPath round-trip mismatch - stored [$stored], want [$Value]"
    }
    Log "binPath set + verified: $stored"
}
# Dump-StartFailure surfaces what the SCM actually saw: Start-Service swallows
# the real error code (1053 timeout / 1066 bad binPath / 1067 process died
# before the handshake / ...), so a generic "Cannot start service" tells you
# nothing. `net start` prints the exact code, `sc qc` shows the binPath the
# SCM registered, and the Application log carries the service's exit reason.
function Dump-StartFailure {
    param([string]$Name)
    Log "collecting service start diagnostics for $Name ..."
    try { & net start $Name 2>&1 | ForEach-Object { Log "  net start: $_" } }
    catch { Log "  net start: ($($_.Exception.Message))" }
    try { & sc.exe qc $Name 2>&1 | ForEach-Object { Log "  sc qc:   $_" } }
    catch { Log "  sc qc:   ($($_.Exception.Message))" }
    try {
        $evts = Get-WinEvent -LogName Application -MaxEvents 60 -ErrorAction Stop |
            Where-Object { $_.Message -match $Name }
        if ($evts) {
            $evts | ForEach-Object {
                Log ("  event {0} @ {1}: {2}" -f $_.Id, $_.TimeCreated, ($_.Message -replace "\s+", " ").Trim())
            }
        } else {
            Log "  no recent Application events mention $Name"
        }
    } catch {
        Log "  (could not read the Application event log: $($_.Exception.Message))"
    }
}

if (Get-Service -Name $svc -ErrorAction SilentlyContinue) {
    # Re-run with a new binary/config path: update binPath, not just restart,
    # or the change never takes effect.
    Set-SvcBinPath $binPath
    # Any `sc config` call forces the SCM to re-read the service record from
    # the registry - without it the SCM can keep using the CACHED (stale)
    # binPath it loaded when the service was first registered, which is a
    # classic cause of "Cannot start service" after a registry-level update.
    & sc.exe config $svc start= auto | Out-Null
    Log "service $svc already exists - updating binPath + restarting"
    try { Restart-Service $svc -ErrorAction Stop } catch { Dump-StartFailure $svc; Log "WARNING: restart failed: $($_.Exception.Message)" }
} else {
    # Register with a placeholder binPath (no spaces -> no quoting involved),
    # set the real quoted binPath via the registry, then enable auto-start.
    & sc.exe create $svc binPath= C:\Windows\system32\cmd.exe start= disabled | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Die "sc.exe create failed (re-run as Administrator)"
    }
    Log "service $svc registered"
    Set-SvcBinPath $binPath
    & sc.exe config $svc start= auto | Out-Null
    try {
        Start-Service $svc -ErrorAction Stop
    } catch {
        Log "WARNING: service did not start: $($_.Exception.Message)"
        Dump-StartFailure $svc
        Log "  run the agent in the foreground (note the & call operator) to see the real error:"
        Log "    & `"$bin`" run --config `"$cfg`""
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
