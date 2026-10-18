# PlcBridge 服务安装（生产部署 / 开机自启）
#
# 策略：
#   - 用 Windows 计划任务（schtasks）后台跑 .\scripts\supervisor.ps1
#   - 触发器：开机；操作：立刻停止；账号：SYSTEM（不需要用户登录）
#   - 自动重启：每 1 分钟（崩溃自愈）
#   - 不依赖 NSSM / 任何第三方
#   - 不复制任何文件，工作目录 = 当前仓库（你编译在哪就在哪跑）
#   - 同时保留端口守护：若 plcbridge / plcbridge-web 崩了，supervisor 自愈
#
# 用法（管理员 PowerShell）：
#   .\scripts\install-service.ps1 -Install        # 安装 + 立刻启动
#   .\scripts\install-service.ps1 -Uninstall      # 卸载 + 清理
#   .\scripts\install-service.ps1 -Status         # 看状态
#   .\scripts\install-service.ps1 -Start          # 立刻启动
#   .\scripts\install-service.ps1 -Stop           # 立刻停止
#   .\scripts\install-service.ps1 -Restart        # 重启

[CmdletBinding()]
param(
    [switch]$Install,
    [switch]$Uninstall,
    [switch]$Status,
    [switch]$Start,
    [switch]$Stop,
    [switch]$Restart,

    # 高级：自定义任务名（默认 PlcBridgeSupervisor）
    [string]$TaskName = 'PlcBridgeSupervisor',

    # 高级：自定义仓库根（默认当前脚本上一级）
    [string]$Root = ''
)

$ErrorActionPreference = 'Stop'

# ---------- 路径解析 ----------
$ScriptDir = if ($PSScriptRoot) { $PSScriptRoot } else { Split-Path -Parent $MyInvocation.MyCommand.Path }
if (-not $Root) { $Root = (Resolve-Path (Join-Path $ScriptDir '..')).Path }

$SupervisorScript = Join-Path $ScriptDir 'supervisor.ps1'
if (-not (Test-Path $SupervisorScript)) {
    throw "missing supervisor script: $SupervisorScript"
}

# ---------- 工具函数 ----------
function Assert-Admin {
    $p = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
    if (-not $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw "Need admin rights. Run this script in an Administrator PowerShell."
    }
}

function Get-StatePath {
    Join-Path $Root 'runtime\supervisor.state.json'
}

function Out-Line {
    param([string]$Text, [string]$Color = 'White')
    Write-Host $Text -ForegroundColor $Color
}

$SCHTASKS = Join-Path $env:WINDIR 'System32\schtasks.exe'
if (-not (Test-Path $SCHTASKS)) {
    Out-Line "WARNING: $SCHTASKS not found, falling back to PATH lookup" 'Yellow'
    $SCHTASKS = 'schtasks.exe'
}
Out-Line ("[debug] schtasks path: " + $SCHTASKS) 'DarkGray'

function Get-TaskInfo {
    # schtasks 会输出一些非 UTF-8 字符 + 大量列表，用 Start-Process 干净地收 stdout
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $SCHTASKS
    $psi.Arguments = '/QUERY /TN ' + $TaskName + ' /FO LIST /V'
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError  = $true
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.StandardOutputEncoding = [System.Text.Encoding]::UTF8
    $psi.StandardErrorEncoding  = [System.Text.Encoding]::UTF8
    try {
        $p = [System.Diagnostics.Process]::Start($psi)
        $p.WaitForExit(10000)
        $stdout = $p.StandardOutput.ReadToEnd()
        $stderr = $p.StandardError.ReadToEnd()
        $p.Dispose()
        if ($p.ExitCode -ne 0 -and -not $stdout) {
            Write-Warning "schtasks exit $($p.ExitCode): $stderr"
            return $null
        }
        return $stdout
    } catch {
        Write-Warning "Get-TaskInfo failed: $($_.Exception.Message)"
        return $null
    }
}

# ---------- 命令实现 ----------
function Install-Service {
    Assert-Admin
    Out-Line "[install] task name:    $TaskName" 'Cyan'
    Out-Line "[install] working dir:  $Root" 'Cyan'
    Out-Line "[install] supervisor:   $SupervisorScript" 'Cyan'

    Uninstall-Service -Quiet

    $psExe = (Get-Command powershell.exe).Source
    $arg   = "-NoProfile -ExecutionPolicy Bypass -File `"$SupervisorScript`" -Root `"$Root`" -Daemon"

    $xml = @"
<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>PlcBridge supervisor (auto-restart on crash, port-based watchdog)</Description>
    <Author>PlcBridge</Author>
  </RegistrationInfo>
  <Triggers>
    <BootTrigger>
      <Enabled>true</Enabled>
    </BootTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-18</UserId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>5</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>$psExe</Command>
      <Arguments>$arg</Arguments>
      <WorkingDirectory>$Root</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
"@

    $xmlPath = Join-Path $env:TEMP "plcbridge-task-$PID.xml"
    [System.IO.File]::WriteAllText($xmlPath, $xml, [System.Text.UnicodeEncoding]::new($false, $true))

    try {
        $r = Run-Schtasks ('/CREATE /TN ' + $TaskName + ' /F /XML "' + $xmlPath + '"')
        if ($r.ExitCode -ne 0) {
            throw ('schtasks /CREATE failed (exit=' + $r.ExitCode + '): ' + $r.StdErr + ' / ' + $r.StdOut)
        }
        Out-Line "[install] task created" 'Green'
    } finally {
        Remove-Item -Force -ErrorAction SilentlyContinue $xmlPath
    }

    Out-Line "[install] starting task..." 'Cyan'
    Start-Service

    Out-Line ""
    Out-Line "==> Installation done." 'Green'
    Out-Line "    Status:  .\scripts\install-service.ps1 -Status" 'White'
    Out-Line "    Stop:    .\scripts\install-service.ps1 -Stop" 'White'
    Out-Line "    Start:   .\scripts\install-service.ps1 -Start" 'White'
    Out-Line "    Restart: .\scripts\install-service.ps1 -Restart" 'White'
    Out-Line "    Remove:  .\scripts\install-service.ps1 -Uninstall" 'White'
}

function Run-Schtasks {
    param([string]$Args)
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $SCHTASKS
    $psi.Arguments = $Args
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError  = $true
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    # note: schtasks output is GBK, do NOT force UTF-8 encoding
    try {
        $p = [System.Diagnostics.Process]::Start($psi)
        $p.WaitForExit(15000)
        $stdout = $p.StandardOutput.ReadToEnd()
        $stderr = $p.StandardError.ReadToEnd()
        $ec = $p.ExitCode
        $p.Dispose()
        return [pscustomobject]@{ ExitCode = $ec; StdOut = $stdout; StdErr = $stderr }
    } catch {
        return [pscustomobject]@{ ExitCode = 999; StdOut = ''; StdErr = $_.Exception.Message }
    }
}

function Get-TaskInfo {
    $args = '/QUERY /TN ' + $TaskName + ' /FO LIST /V'
    Out-Line ('[debug] Run-Schtasks ' + $SCHTASKS + ' ' + $args) 'DarkGray'
    $r = Run-Schtasks $args
    Write-Verbose ("schtasks exit=" + $r.ExitCode + " stdout=" + $r.StdOut.Length + " chars")
    if ($r.ExitCode -ne 0) {
        return $null
    }
    return $r.StdOut
}

function Uninstall-Service {
    param([switch]$Quiet)

    $r = Run-Schtasks ('/DELETE /TN ' + $TaskName + ' /F')
    if ($r.ExitCode -eq 0) {
        if (-not $Quiet) { Out-Line "[uninstall] task deleted" 'Yellow' }
    } elseif (-not $Quiet) {
        Out-Line ('[uninstall] task did not exist, skipped (exit=' + $r.ExitCode + ')') 'DarkGray'
    }

    $procNames = @('plcbridge','plcbridge-web')
    foreach ($n in $procNames) {
        Get-Process -Name $n -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    }
    # kill cmd.exe parent of plcbridge child (only those spawned by supervisor)
    Get-CimInstance Win32_Process -Filter "Name='cmd.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -match 'plcbridge(-web)?\.exe' } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }

    $stateFile = Get-StatePath
    if (Test-Path $stateFile) {
        $obj = [ordered]@{
            status         = 'uninstalled'
            supervisor_pid = 0
            web_pid        = 0
            core_pid       = 0
            message        = 'removed by install-service -Uninstall'
            updated_at     = (Get-Date).ToString('o')
        }
        try {
            $json = ($obj | ConvertTo-Json -Depth 3 -Compress)
            [System.IO.File]::WriteAllText($stateFile, $json, [System.Text.UTF8Encoding]::new($false))
        } catch {}
    }
}

function Start-Service {
    Out-Line "[start] running task $TaskName ..." 'Cyan'
    $r = Run-Schtasks ('/RUN /TN ' + $TaskName)
    if ($r.ExitCode -ne 0) { throw ('schtasks /RUN failed: ' + $r.StdErr) }
    Start-Sleep -Seconds 3
    Status-Service
}

function Stop-Service {
    Out-Line "[stop] stopping task $TaskName ..." 'Cyan'
    # first send stop_supervisor for clean shutdown
    $ctrlPath = Join-Path $Root 'runtime\control.pending.json'
    $ctrlDir  = Split-Path $ctrlPath
    if (-not (Test-Path $ctrlDir)) { New-Item -ItemType Directory -Force -Path $ctrlDir | Out-Null }
    try {
        [System.IO.File]::WriteAllText($ctrlPath, '{"action":"stop_supervisor"}', [System.Text.UTF8Encoding]::new($false))
    } catch {}
    # wait
    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-Date) -lt $deadline) {
        $state = Get-ServiceState
        if ($state.status -eq 'stopped' -or $state.supervisor_pid -eq 0) { break }
        Start-Sleep -Seconds 1
    }
    # fallback
    $taskPid = Get-TaskPid
    if ($taskPid -gt 0) {
        Stop-Process -Id $taskPid -Force -ErrorAction SilentlyContinue
    }
    Get-Process -Name plcbridge-web,plcbridge -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Out-Line "[stop] stopped" 'Green'
}

function Restart-Service {
    Stop-Service
    Start-Sleep -Seconds 2
    Start-Service
}

function Get-TaskPid {
    Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -like '*supervisor.ps1*' -and $_.CommandLine -like '*-Daemon*' } |
        Select-Object -First 1 -ExpandProperty ProcessId
}

function Get-ServiceState {
    $stateFile = Get-StatePath
    if (-not (Test-Path $stateFile)) {
        return [pscustomobject]@{ status='unknown'; supervisor_pid=0; web_pid=0; core_pid=0; message='state.json missing' }
    }
    try {
        $raw = Get-Content $stateFile -Raw -ErrorAction Stop
        return ($raw | ConvertFrom-Json -ErrorAction Stop)
    } catch {
        return [pscustomobject]@{ status='unknown'; supervisor_pid=0; web_pid=0; core_pid=0; message=$_.Exception.Message }
    }
}

function Status-Service {
    Out-Line ""
    Out-Line "============ PlcBridge service status ============" 'Cyan'

    $info = Get-TaskInfo
    if ($info) {
        $tag = '[' + $TaskName + ']  registered'
        Out-Line $tag 'Green'
        $statusLine = $info -split "`r?`n" | Where-Object { $_ -match '^Status:' } | Select-Object -First 1
        if ($statusLine) { Out-Line ('             ' + $statusLine.Trim()) 'White' }
    } else {
        $tag = '[' + $TaskName + ']  NOT installed'
        Out-Line $tag 'Red'
    }

    $taskPid = Get-TaskPid
    if ($taskPid -gt 0) {
        Out-Line ('[supervisor]  PID ' + $taskPid + '  running') 'Green'
    } else {
        Out-Line '[supervisor]  NOT running' 'Red'
    }

    $s = Get-ServiceState
    $stateLine = '[state]      status=' + $s.status + '  web=' + $s.web_pid + '  core=' + $s.core_pid + '  msg=' + $s.message
    if ($s.status -eq 'running') {
        Out-Line $stateLine 'Green'
    } else {
        Out-Line $stateLine 'Yellow'
    }

    function _testPort($p) {
        try {
            $c = New-Object System.Net.Sockets.TcpClient
            $iar = $c.BeginConnect('127.0.0.1', $p, $null, $null)
            $ok = $iar.AsyncWaitHandle.WaitOne(500)
            if ($ok) { $c.EndConnect($iar); $c.Close(); return $true }
            $c.Close()
        } catch {}
        return $false
    }
    $webPort  = _testPort 8080
    $corePort = _testPort 9091
    $webText  = if ($webPort)  { 'listening' } else { 'NOT listening' }
    $coreText = if ($corePort) { 'listening' } else { 'NOT listening' }
    $webColor  = if ($webPort)  { 'Green' } else { 'Red' }
    $coreColor = if ($corePort) { 'Green' } else { 'Red' }
    Out-Line ('[port 8080] plcbridge-web   ' + $webText) $webColor
    Out-Line ('[port 9091] plcbridge       ' + $coreText) $coreColor

    Out-Line ""
    Out-Line ('logs: ' + $Root + '\logs\') 'DarkGray'
    Out-Line '      supervisor.out.log / plcbridge-web.out.log / plcbridge.out.log' 'DarkGray'
    Out-Line ""
}

# ---------- 路由 ----------
$installed = $Install -or $Uninstall -or $Start -or $Stop -or $Restart -or $Status
if (-not $installed) {
    Out-Line "Usage:" 'Yellow'
    Out-Line "  .\scripts\install-service.ps1 -Install        # install + autostart on boot" 'White'
    Out-Line "  .\scripts\install-service.ps1 -Uninstall      # remove" 'White'
    Out-Line "  .\scripts\install-service.ps1 -Start          # start now" 'White'
    Out-Line "  .\scripts\install-service.ps1 -Stop           # stop now" 'White'
    Out-Line "  .\scripts\install-service.ps1 -Restart        # restart" 'White'
    Out-Line "  .\scripts\install-service.ps1 -Status         # show status" 'White'
    exit 0
}

if ($Install)    { Install-Service }
if ($Uninstall)  { Uninstall-Service }
if ($Start)      { Start-Service }
if ($Stop)       { Stop-Service }
if ($Restart)    { Restart-Service }
if ($Status)     { Status-Service }