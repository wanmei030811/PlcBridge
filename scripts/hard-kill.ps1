# PlcBridge hard-kill via WMI (cross-session safe).
# WMI Win32_Process.Terminate() RPC runs at SYSTEM level -- no session isolation.

$ErrorActionPreference = 'Continue'

function Kill-ByName([string]$name) {
    $procs = Get-WmiObject Win32_Process -Filter ("Name='" + $name + ".exe'") -ErrorAction SilentlyContinue
    foreach ($p in $procs) {
        Write-Host ("  WMI Terminate PID=" + $p.ProcessId + " Name=" + $p.Name)
        $r = $p.Terminate()
        if ($r.ReturnValue -eq 0) {
            Write-Host "    ok" -ForegroundColor Green
        } else {
            Write-Host ("    ReturnValue=" + $r.ReturnValue) -ForegroundColor Red
        }
    }
}

function Kill-Supervisor {
    $pws = Get-WmiObject Win32_Process -Filter "Name='powershell.exe'" -ErrorAction SilentlyContinue
    foreach ($p in $pws) {
        if ($p.CommandLine -and $p.CommandLine -match 'supervisor') {
            Write-Host ("  WMI Terminate supervisor PID=" + $p.ProcessId)
            $r = $p.Terminate()
            if ($r.ReturnValue -eq 0) {
                Write-Host "    ok" -ForegroundColor Green
            } else {
                Write-Host ("    ReturnValue=" + $r.ReturnValue) -ForegroundColor Red
            }
        }
    }
}

Write-Host "=== PlcBridge 硬杀 (WMI 跨会话) ===" -ForegroundColor Cyan
Write-Host ""
Write-Host "[1] 当前 PID:"
Get-Process -Name plcbridge,plcbridge-web -ErrorAction SilentlyContinue |
    Select-Object Id,Name | Format-Table -AutoSize
Get-WmiObject Win32_Process -Filter "Name='powershell.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -match 'supervisor' } |
    Select-Object ProcessId,CommandLine | Format-List

Write-Host "[2] WMI Terminate plcbridge,plcbridge-web..." -ForegroundColor Yellow
Kill-ByName 'plcbridge'
Kill-ByName 'plcbridge-web'

Write-Host ""
Write-Host "[2b] WMI Terminate influxd (InfluxDB 1.8, 历史趋势依赖)..." -ForegroundColor Yellow
Kill-ByName 'influxd'

Write-Host ""
Write-Host "[3] WMI Terminate supervisor (powershell 含 supervisor)..." -ForegroundColor Yellow
Kill-Supervisor

Write-Host ""
Write-Host "[4] 兜底 taskkill /F (同会话)..." -ForegroundColor Yellow
& taskkill.exe /IM plcbridge.exe /F 2>&1 | Out-Null
& taskkill.exe /IM plcbridge-web.exe /F 2>&1 | Out-Null
& taskkill.exe /IM influxd.exe /F 2>&1 | Out-Null

Start-Sleep -Seconds 3

Write-Host ""
Write-Host "[5] 残留确认:" -ForegroundColor Yellow
Write-Host "  进程:"
$left = Get-Process -Name plcbridge,plcbridge-web,influxd -ErrorAction SilentlyContinue
if ($left) { $left | Format-Table Id,Name | Out-String | Write-Host }
else { Write-Host "    (无)" -ForegroundColor Green }

$supLeft = Get-WmiObject Win32_Process -Filter "Name='powershell.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -match 'supervisor' }
if ($supLeft) {
    Write-Host "  supervisor 残留:" -ForegroundColor Red
    $supLeft | Select-Object ProcessId,CommandLine | Format-List | Out-String | Write-Host
} else {
    Write-Host "  supervisor: (无)" -ForegroundColor Green
}

Write-Host "  端口 8080/8082/8086/9091 LISTENING:"
$conns = netstat -ano | Select-String "LISTENING" | Select-String ":8080|:8082|:8086|:9091"
if ($conns) { $conns | ForEach-Object { Write-Host ("    " + $_.Line) } }
else { Write-Host "    (无)" -ForegroundColor Green }

Write-Host ""
Write-Host "=== 硬杀完成 ===" -ForegroundColor Cyan
