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
$ApiSecretFile   = Join-Path $ConfigDir 'api.secret'
if (-not (Test-Path $ControlDoneDir)) { New-Item -ItemType Directory -Force -Path $ControlDoneDir | Out-Null }
if (-not (Test-Path $FlagsFile)) {
    '{"pause_core":false,"pause_web":false,"updated_at":"1970-01-01T00:00:00Z"}' | Set-Content -Path $FlagsFile -Encoding UTF8
}

# ============== Helpers ==============

# Get-OrCreateApiSecret 返回 web/core 共享的 HMAC 签名密钥。
#
# 优先级：
#   1. 环境变量 PLCBRIDGE_API_SECRET（外部注入，如 CI / 容器 secret）
#   2. <ConfigDir>/api.secret 文件（已存在 → 直接用）
#   3. 都没有 → 生成 32 字节随机密钥并**独占创建**该文件
#
# 为什么需要它：
#   MVP 时代 HMAC 密钥写死在源码（auth.DefaultSecret），所有部署共用同一个
#   公开密钥 → 任何人可伪造 Bearer Token 调回写 API。
#
# 与 Go 侧的关系（internal/infra/secconfig）：
#   web / core 进程本身也能读/生成这个文件（secconfig.EnsureAPISecretWithFallback）。
#   supervisor 提前生成只是为了让「两个子进程从一开始就同密钥」，
#   并把密钥写进环境变量（子进程继承），避免各自生成的风险窗口。
#
# 并发安全：
#   用 FileMode.CreateNew（等价于 O_EXCL）独占创建 —— 内核保证原子性。
#   并发时只有一个进程写入成功，其他进程发现文件已存在 → 读文件里赢家的密钥。
#   绝不能两个进程各生成一个（web 与 core 密钥不一致 → 内部调用全 401）。
#
# 返回 $null 表示生成失败（此时应由调用方报错退出，而不是用弱默认值兜底）。
function Get-OrCreateApiSecret {
    if ($env:PLCBRIDGE_API_SECRET) { return $env:PLCBRIDGE_API_SECRET }

    if (Test-Path $ApiSecretFile) {
        $existing = (Get-Content -Path $ApiSecretFile -Raw -ErrorAction SilentlyContinue)
        if ($existing) { return $existing.Trim() }
    }

    # 生成 32 字节（256 bit）随机密钥，base64 编码后 44 字符。
    try {
        $bytes = New-Object 'System.Byte[]' 32
        [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
        $secret = [System.Convert]::ToBase64String($bytes)
    } catch {
        Write-Host "[supervisor] 生成 API 密钥失败: $($_.Exception.Message)" -ForegroundColor Red
        return $null
    }

    try {
        # CreateNew = 独占创建（O_EXCL）：文件已存在就抛异常，不会覆盖别人的密钥。
        $fs = [System.IO.File]::Open($ApiSecretFile, [System.IO.FileMode]::CreateNew,
                                     [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)
        try {
            # 结尾必须带换行 —— 它是"写入完成"的标记。
            # Go 侧 secconfig.loadSecretFile 靠它判断文件是否已写完，
            # 避免读到半截密钥（半截密钥长度也能通过强度检查，
            # 会导致 web 与 core 缓存到不同密钥 → 内部调用全 401）。
            $bytesOut = [System.Text.Encoding]::UTF8.GetBytes($secret + "`n")
            $fs.Write($bytesOut, 0, $bytesOut.Length)
        } finally {
            $fs.Close()
        }
        Write-Host "[supervisor] 已生成 API 密钥: $ApiSecretFile" -ForegroundColor DarkGreen
    } catch [System.IO.IOException] {
        # 文件已存在（并发时另一个进程赢了）→ 读它的内容。
        $existing = (Get-Content -Path $ApiSecretFile -Raw -ErrorAction SilentlyContinue)
        if ($existing) {
            Write-Host "[supervisor] API 密钥已由其它进程生成，复用: $ApiSecretFile" -ForegroundColor DarkYellow
            return $existing.Trim()
        }
        Write-Host "[supervisor] 读 API 密钥文件失败: $($_.Exception.Message)" -ForegroundColor Red
        return $null
    } catch {
        Write-Host "[supervisor] 写 API 密钥文件失败: $($_.Exception.Message)" -ForegroundColor Red
        return $null
    }
    return $secret
}

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
        [string]$StderrLog,
        [string]$ApiSecret = ''
    )
    # Build: start "" "<exe>" <args> > <stdout> 2> <stderr>
    # Use cmd /c so the child inherits an environment + is fully detached.
    #
    # PLCBRIDGE_API_SECRET 不写进命令行，而是由 supervisor 进程自身持有
    # （$env:PLCBRIDGE_API_SECRET），Start-Process 的子进程自动继承。
    #
    # 为什么不用 cmd 的 `set VAR=...&&` 前缀注入：
    #   1. 引号地狱 —— 密钥里若含引号 / & / | 会破坏命令解析，甚至变成命令注入。
    #   2. `cmd /c` 的命令行对同机其它进程可见（进程列表 / wmic），
    #      密钥会泄露给无关进程；环境变量继承没有这个问题。
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
        -StderrLog (Join-Path $LogDir 'plcbridge-web.err.log') `
        -ApiSecret $ApiSecret
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
        -StderrLog (Join-Path $LogDir 'plcbridge.err.log') `
        -ApiSecret $ApiSecret
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

# === API 密钥（web / core 共享的 HMAC 签名密钥） ===
# 缺失则生成并落盘到 <ConfigDir>/api.secret；生成失败直接退出，
# 绝不用弱默认值兜底（弱密钥 = 全网可伪造的 Bearer Token）。
$ApiSecret = Get-OrCreateApiSecret
if (-not $ApiSecret) {
    Write-Host "[supervisor] FATAL: 无法获取 API 密钥，拒绝启动两个进程" -ForegroundColor Red
    Write-State -Status 'stopped' -Msg 'api secret unavailable'
    exit 1
}
# 先记录来源（必须在写 $env: 之前判断，否则永远是 'env'）。
$ApiSecretSource = if ($env:PLCBRIDGE_API_SECRET) { 'env' } else { 'file' }
# 写进 supervisor 自身的环境 —— Start-Process 的子进程会自动继承。
# web 与 core 因此共享同一密钥，且密钥不出现在任何命令行里。
$env:PLCBRIDGE_API_SECRET = $ApiSecret
Write-Host "[supervisor] API 密钥就绪 (source: $ApiSecretSource)" -ForegroundColor DarkGray

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
