# PlcBridge supervisor (Windows / PowerShell 5.1) -- shell-based, no Start-Process tricks
#
# - Spawn plcbridge-web and plcbridge via cmd /c start (detached, hidden)
# - Port-based watchdog every 3s
# - Read runtime/control.pending.json for control commands
# - Hidden window, parent dies won't kill children (cmd /c start detaches)
#
# Usage:
#   Foreground (dev/debug, real-time logs):
#     .\scripts\supervisor.ps1
#
#   Background + auto-start on boot (prod, recommended):
#     # Register as Windows scheduled task, launch as SYSTEM at boot
#     .\scripts\install-service.ps1 -Install        # install
#     .\scripts\install-service.ps1 -Uninstall      # uninstall
#     .\scripts\install-service.ps1 -Status         # show status
#     .\scripts\install-service.ps1 -Restart        # restart

[CmdletBinding()]
param(
    [string]$Root = '',
    [string]$ConfigDir = '',
    [int]$PollIntervalSec = 3,
    [int]$WebStartupTimeoutSec = 30,
    # Daemon mode: detach from stdin, no console read, log to file
    [switch]$Daemon
)

$ErrorActionPreference = 'Continue'

if (-not $Root) { $Root = Resolve-Path (Join-Path $PSScriptRoot '..') }
Set-Location $Root

$BinDir  = Join-Path $Root 'bin'
$LogDir  = Join-Path $Root 'logs'
# runtime/state files live under <configDir>/runtime (matches web's runtimeDirFromConfigDir),
# so that web started with --config <configDir> can read supervisor.state.json directly.
if (-not $ConfigDir) {
    # Fallback: default to <Root>/config; can be overridden via PLCBRIDGE_CONFIG_DIR env or -ConfigDir param
    $envCD = $env:PLCBRIDGE_CONFIG_DIR
    if ($envCD) { $ConfigDir = $envCD } else { $ConfigDir = Join-Path $Root 'config' }
}
$RunDir  = Join-Path $ConfigDir 'runtime'
$WebExe  = Join-Path $BinDir 'plcbridge-web.exe'
$CoreExe = Join-Path $BinDir 'plcbridge.exe'

if (-not (Test-Path $BinDir))  { throw "missing $BinDir" }
foreach ($d in @($LogDir, $RunDir)) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Force -Path $d | Out-Null }
}

$StateFile       = Join-Path $RunDir 'supervisor.state.json'
$FlagsFile      = Join-Path $RunDir 'supervisor.flags.json'
$ControlPending  = Join-Path $RunDir 'control.pending.json'
$ControlDoneDir  = Join-Path $RunDir 'control.done'
if (-not (Test-Path $ControlDoneDir)) { New-Item -ItemType Directory -Force -Path $ControlDoneDir | Out-Null }
if (-not (Test-Path $FlagsFile)) {
    '{"pause_core":false,"pause_web":false,"updated_at":"1970-01-01T00:00:00Z"}' | Set-Content -Path $FlagsFile -Encoding UTF8
}

# ============== Helpers ==============

function Write-State {
    param([int]$W = 0, [int]$C = 0, [string]$Msg = 'ok', [string]$Status = 'running')
    $supId = [System.Diagnostics.Process]::GetCurrentProcess().Id
    $obj = [ordered]@{
        status         = $Status
        supervisor_pid = $supId
        web_pid        = $W
        core_pid       = $C
        message        = $Msg
        updated_at     = (Get-Date).ToString('o')
    }
    $tmp = "$StateFile.tmp"
    try {
        $json = ($obj | ConvertTo-Json -Depth 3 -Compress)
        [System.IO.File]::WriteAllText($tmp, $json, [System.Text.UTF8Encoding]::new($false))
        Move-Item -Path $tmp -Destination $StateFile -Force
    } catch {
        Write-Host "[supervisor] Write-State failed: $($_.Exception.Message)" -ForegroundColor Red
    }
}

function Test-Port {
    param([int]$Port)
    try {
        $client = New-Object System.Net.Sockets.TcpClient
        $iar = $client.BeginConnect('127.0.0.1', $Port, $null, $null)
        $ok = $iar.AsyncWaitHandle.WaitOne(500)
        if ($ok) { $client.EndConnect($iar); $client.Close(); return $true }
        $client.Close()
    } catch {}
    return $false
}

# Read-Flags / Set-Flag -- pause marker: dashboard "stop plcbridge" sets pause_core=true.
# supervisor tick checks port 9091 not listening -> restart; **but** if pause_core=true then skip restart.
function Read-Flags {
    $raw = ''
    try { $raw = Get-Content -Path $FlagsFile -Raw -ErrorAction Stop } catch { return @{ pause_core = $false; pause_web = $false } }
    try { return ($raw | ConvertFrom-Json) } catch { return @{ pause_core = $false; pause_web = $false } }
}
function Set-Flag {
    param([string]$Name, [bool]$Value)
    $f = Read-Flags
    $f.$Name = $Value
    $f.updated_at = (Get-Date).ToString('o')
    $tmp = [System.IO.Path]::GetTempFileName()
    try {
        [System.IO.File]::WriteAllText($tmp, ($f | ConvertTo-Json -Depth 3 -Compress), [System.Text.UTF8Encoding]::new($false))
        Move-Item -Path $tmp -Destination $FlagsFile -Force
    } catch {
        Write-Host "[supervisor] Set-Flag failed: $($_.Exception.Message)" -ForegroundColor Red
    }
}

# Spawn child via cmd /c start. The 'start /B' runs in background and the
# call returns immediately. We use cmd so that even if our shell exits,
# the cmd process keeps the child alive long enough to detach.
function Start-Detached {
    param(
        [string]$ExePath,
        [string]$ArgsString,
        [string]$StdoutLog,
        [string]$StderrLog
    )
    # Build: start "" "<exe>" <args> > <stdout> 2> <stderr>
    # Use cmd /c so the child inherits an environment + is fully detached.
    $cmdLine = "cmd /c `"$ExePath`" $ArgsString > `"$StdoutLog`" 2> `"$StderrLog`""
    Start-Process -FilePath 'cmd.exe' -ArgumentList '/c', $cmdLine -WorkingDirectory $Root -WindowStyle Hidden
}

function Start-Web {
    # IMPORTANT: do not overwrite an existing web (dashboard "start supervisor" usually runs while old web is up).
    # If 8080 is already listening, reuse existing instance, do not restart.
    # This keeps already-logged-in user sessions alive.
    if (Test-Port 8080) {
        $existingW = Get-PidByName 'plcbridge-web'
        Write-Host "[supervisor] plcbridge-web already listening on :8080 (pid=$existingW), skip start" -ForegroundColor DarkGreen
        return
    }
    Write-Host "[supervisor] starting plcbridge-web ..." -ForegroundColor Cyan
    Start-Detached `
        -ExePath $WebExe `
        -ArgsString "--config $ConfigDir --addr :8080" `
        -StdoutLog (Join-Path $LogDir 'plcbridge-web.out.log') `
        -StderrLog (Join-Path $LogDir 'plcbridge-web.err.log')
    $deadline = (Get-Date).AddSeconds($WebStartupTimeoutSec)
    while ((Get-Date) -lt $deadline) {
        if (Test-Port 8080) {
            Write-Host "[supervisor] plcbridge-web listening on :8080" -ForegroundColor Green
            return
        }
        Start-Sleep -Milliseconds 500
    }
    Write-Host "[supervisor] plcbridge-web did not start on :8080 in ${WebStartupTimeoutSec}s" -ForegroundColor Red
}

function Start-Core {
    if (-not (Test-Path $CoreExe)) { return }
    # IMPORTANT: do not overwrite an existing plcbridge (dashboard "start supervisor" usually runs while old core is up).
    if (Test-Port 9091) {
        $existingC = Get-PidByName 'plcbridge'
        Write-Host "[supervisor] plcbridge already listening on :9091 (pid=$existingC), skip start" -ForegroundColor DarkGreen
        return
    }
    Write-Host "[supervisor] starting plcbridge ..." -ForegroundColor Cyan
    Start-Detached `
        -ExePath $CoreExe `
        -ArgsString "--config $ConfigDir" `
        -StdoutLog (Join-Path $LogDir 'plcbridge.out.log') `
        -StderrLog (Join-Path $LogDir 'plcbridge.err.log')
    Start-Sleep -Seconds 2
    if (Test-Port 9091) {
        Write-Host "[supervisor] plcbridge listening on :9091" -ForegroundColor Green
    } else {
        Write-Host "[supervisor] plcbridge may not have started (port 9091 not listening)" -ForegroundColor Yellow
    }
}

function Stop-ByName {
    param([string]$Name)
    Get-Process -Name $Name -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
}

function Get-PidByName {
    param([string]$Name)
    $p = Get-Process -Name $Name -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($p) { return $p.Id } else { return 0 }
}

function Consume-Control {
    if (-not (Test-Path $ControlPending)) { return }
    $raw = Get-Content -Path $ControlPending -Raw -ErrorAction SilentlyContinue
    if (-not $raw) { return }
    try {
        $obj = $raw | ConvertFrom-Json -ErrorAction Stop
    } catch {
        Move-Item -Path $ControlPending -Destination (Join-Path $ControlDoneDir ("bad_" + [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() + ".json")) -Force
        return
    }
    $archived = Join-Path $ControlDoneDir ("ctrl_" + [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() + ".json")
    Move-Item -Path $ControlPending -Destination $archived -Force
    return $obj
}

# ============== Main ==============

# Daemon mode: all console output goes to log file (cmd/scheduled-task caller)
if ($Daemon) {
    $daemonLog = Join-Path $LogDir 'supervisor.out.log'
    try {
        # /append: use UTF-8 BOM to avoid PowerShell 5.1 encoding issues
        [System.IO.File]::AppendAllText($daemonLog, "[$((Get-Date).ToString('o'))] supervisor in daemon`r`n", [System.Text.UTF8Encoding]::new($true))
    } catch {}
}

# === Cleanup stale control command ===
# supervisor at startup clears control.pending.json: previous process may have
# unexpectedly exited / stop_supervisor self-killed, leaving a stale
# stop_supervisor command; otherwise on the first tick this supervisor would
# consume it and kill plcbridge. We archive the residual pending file into
# control.done/ rather than directly delete: keep audit traceability.
if (Test-Path $ControlPending) {
    $staleTs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
    try {
        Move-Item -Path $ControlPending -Destination (Join-Path $ControlDoneDir ("stale_startup_" + $staleTs + ".json")) -Force
        Write-Host "[supervisor] archived stale control.pending.json (startup cleanup)" -ForegroundColor DarkYellow
    } catch {
        Write-Host "[supervisor] WARN: failed to archive stale control.pending.json: $_" -ForegroundColor Red
    }
}

# === Critical fix: supervisor must not unconditionally kill existing web/core on startup ===
# History bug: starting supervisor would unconditionally Stop-ByName plcbridge-web,
# kicking out logged-in user sessions. Now: only kill when port is not listening
# (meaning the process is dead/zombie), otherwise leave it untouched.
if (-not (Test-Port 8080)) {
    Stop-ByName 'plcbridge-web'
} else {
    Write-Host "[supervisor] plcbridge-web already listening on :8080, preserve existing web (keep user sessions)" -ForegroundColor DarkGreen
}
if (-not (Test-Port 9091)) {
    Stop-ByName 'plcbridge'
} else {
    Write-Host "[supervisor] plcbridge already listening on :9091, preserve existing plcbridge" -ForegroundColor DarkGreen
}
Start-Sleep -Milliseconds 800

# After supervisor start/restart, clear pause flags (let watchdog take over normal crash restart).
# Only dashboard "stop plcbridge" will re-set pause_core=true.
Set-Flag -Name 'pause_core' -Value $false
Set-Flag -Name 'pause_web'   -Value $false

Write-Host "[supervisor] PlcBridge supervisor starting  Root=$Root  ConfigDir=$ConfigDir" -ForegroundColor Green
Write-State -Status 'starting' -Msg 'initial boot'

Start-Web
Start-Core
$shouldExit = $false
$tickCount = 0

try {
    while (-not $shouldExit) {
        $tickCount++
        $ctrl = Consume-Control
        if ($ctrl) {
            $act = $ctrl.action
            Write-Host "[supervisor] control $tickCount : $act" -ForegroundColor Magenta
            switch ($act) {
                'stop_plcbridge' {
                    Set-Flag -Name 'pause_core' -Value $true
                    Stop-ByName 'plcbridge'
                }
                'start_plcbridge' {
                    Set-Flag -Name 'pause_core' -Value $false
                    if (-not (Test-Port 9091)) { Start-Core }
                }
                'restart_plcbridge-web' { Stop-ByName 'plcbridge-web' }
                'export_config' { Write-Host "[supervisor] export_config handled by web (reload_config)" -ForegroundColor DarkGray }
                'start_supervisor' { Write-Host "[supervisor] start_supervisor ignored (we are running)" -ForegroundColor DarkGray }
                'stop_supervisor' {
                    # IMPORTANT: web is the control panel and MUST stay online, otherwise dashboard "start supervisor" button is unreachable.
                    # On stop_supervisor: only stop plcbridge core + set pause_core;
                    # plcbridge-web keeps running; when supervisor self-exits, the web "start supervisor" button will restart it.
                    Write-Host "[supervisor] received stop_supervisor (web stays alive)" -ForegroundColor Magenta
                    Write-State -Status 'stopping' -Msg 'stopped by user (web preserved)'
                    Set-Flag -Name 'pause_core' -Value $true
                    # no Set-Flag pause_web -- web MUST keep running
                    Stop-ByName 'plcbridge'
                    # no Stop-ByName 'plcbridge-web' -- web is preserved for the "start supervisor" button
                    $shouldExit = $true
                }
                default { Write-Host "[supervisor] unknown action '$act'" -ForegroundColor Red }
            }
        }

        # Watch flags: do not restart under pause state (reset default is false, does not affect normal crash restart)
        $flags = Read-Flags
        if (-not (Test-Port 8080)) {
            if ($flags.pause_web) {
                Write-Host "[supervisor] tick $tickCount : plcbridge-web not listening, but pause_web=true -> skip" -ForegroundColor DarkYellow
            } else {
                Write-Host "[supervisor] tick $tickCount : plcbridge-web not listening, restarting" -ForegroundColor Yellow
                Stop-ByName 'plcbridge-web'
                Start-Web
            }
        }
        if (-not (Test-Port 9091)) {
            if ($flags.pause_core) {
                Write-Host "[supervisor] tick $tickCount : plcbridge not listening, but pause_core=true -> skip" -ForegroundColor DarkYellow
            } else {
                Write-Host "[supervisor] tick $tickCount : plcbridge not listening, restarting" -ForegroundColor Yellow
                Stop-ByName 'plcbridge'
                Start-Core
            }
        }

        $webStatus  = 'down'; if (Test-Port 8080) { $webStatus  = 'up' }
        $coreStatus = 'down'; if (Test-Port 9091) { $coreStatus = 'up' }
        $wPid = Get-PidByName 'plcbridge-web'
        $cPid = Get-PidByName 'plcbridge'
        Write-State -W $wPid -C $cPid

        if ($tickCount % 5 -eq 0) {
            Write-Host "[supervisor] tick $tickCount : web=$wPid($webStatus) core=$cPid($coreStatus)" -ForegroundColor DarkGray
        }

        Start-Sleep -Seconds $PollIntervalSec
    }
} finally {
    # IMPORTANT: web is the control panel and MUST stay online, otherwise dashboard "start supervisor" button is unreachable.
    # finally only stops plcbridge; web is always preserved (even if admin uses Ctrl+C or abnormal exit).
    Write-Host "[supervisor] cleanup" -ForegroundColor Magenta
    Stop-ByName 'plcbridge'
    # no Stop-ByName 'plcbridge-web' -- web is preserved for the "start supervisor" button
    Write-State -Status 'stopped' -Msg 'clean exit'
    Write-Host "[supervisor] bye" -ForegroundColor Green
}
