# PlcBridge launcher — equivalent to scripts/start-hidden.bat but inlined.
#
# Launches plcbridge-web.exe + plcbridge.exe as detached hidden processes,
# waits briefly, prints PIDs and ports status. Mirrors scripts/start-hidden-v6.ps1
# behavior (ProcessStartInfo hidden + detached) without going through cmd /c wrappers.

$ErrorActionPreference = 'Stop'
$root = 'F:\Project\PlcBridge'
$bin  = Join-Path $root 'bin'
$cfg  = Join-Path $root 'config'
$log  = Join-Path $root 'logs'

foreach ($d in @($bin, $cfg, $log)) {
    if (-not (Test-Path $d)) { throw "missing $d" }
}

function Start-Hidden {
    param(
        [string]$Exe,
        [string]$ArgString,
        [string]$OutLog,
        [string]$ErrLog
    )
    # 日志文件可能被上一个实例还持有句柄（Windows 上文件未释放时删除会失败）。
    # 删除失败不应中断启动流程 —— Start-Process 的重定向本身就是"追加/覆盖"语义，
    # 删不掉只是这次日志会追加到旧文件里，不影响服务可用性。
    foreach ($f in @($OutLog, $ErrLog)) {
        if (Test-Path $f) {
            try { Remove-Item $f -Force -ErrorAction Stop }
            catch { Write-Host "[WARN] 无法删除日志 $f（可能被占用），继续启动" -ForegroundColor Yellow }
        }
    }
    # 关键：ProcessStartInfo.RedirectStandardOutput 是 **bool** 属性，不是路径。
    # 直接赋值路径字符串会把它置为 $true —— 管道开启但无人读取，
    # 一旦 64KB 管道缓冲写满，进程会被永久阻塞（现场表现为"服务假死"）。
    # Start-Process -RedirectStandardOutput 走的是 CreateProcess 的文件句柄直传，
    # 不经过管道，既不阻塞又能拿到真实子进程 PID。
    $p = Start-Process -FilePath $Exe `
                      -ArgumentList $ArgString `
                      -WorkingDirectory $root `
                      -WindowStyle Hidden `
                      -RedirectStandardOutput $OutLog `
                      -RedirectStandardError $ErrLog `
                      -PassThru
    return $p.Id
}

# 0) InfluxDB 1.8 (可选，但历史趋势/诊断依赖它)
#
# 现场 config.json 里 forward channel "influxdb" 的 address 是 http://localhost:8086，
# version = "1.x"（InfluxQL 方言）。没有它，/api/v1/targets/:id/history 会返回
# 500 history_query_failed（连接被拒绝）。
$influxExe = Join-Path $root 'tools\influxdb\influxdb-1.8.10-1\influxd.exe'
$influxConf = Join-Path $cfg 'runtime\influxdb\influxdb.conf'
$influxPid = $null

if ((Test-Path $influxExe) -and (Test-Path $influxConf)) {
    # 已经在跑就别重复启动
    $alreadyUp = $false
    try {
        $c = New-Object System.Net.Sockets.TcpClient
        $iar = $c.BeginConnect('127.0.0.1', 8086, $null, $null)
        if ($iar.AsyncWaitHandle.WaitOne(300)) { $c.EndConnect($iar); $alreadyUp = $true }
        $c.Close()
    } catch {}
    if ($alreadyUp) {
        Write-Host "[OK] InfluxDB already listening on :8086"
    } else {
        $influxPid = Start-Hidden `
            -Exe $influxExe `
            -ArgString "-config `"$influxConf`"" `
            -OutLog (Join-Path $log 'influxd.out.log') `
            -ErrLog (Join-Path $log 'influxd.err.log')
        Write-Host "[OK] influxd started PID=$influxPid"
        for ($i = 0; $i -lt 40; $i++) {
            Start-Sleep -Milliseconds 500
            try {
                $c = New-Object System.Net.Sockets.TcpClient
                $iar = $c.BeginConnect('127.0.0.1', 8086, $null, $null)
                if ($iar.AsyncWaitHandle.WaitOne(300)) { $c.EndConnect($iar); $c.Close(); break }
                $c.Close()
            } catch {}
        }
        Write-Host "[OK] :8086 listening"
    }
} else {
    Write-Host "[SKIP] InfluxDB not found (tools\influxdb\...) -- 历史趋势功能不可用" -ForegroundColor Yellow
}

# 1) plcbridge-web first (plcbridge core needs config.json from web)
$webPid = Start-Hidden `
    -Exe (Join-Path $bin 'plcbridge-web.exe') `
    -ArgString "--config $cfg --addr :8080" `
    -OutLog (Join-Path $log 'plcbridge-web.out.log') `
    -ErrLog (Join-Path $log 'plcbridge-web.err.log')
Write-Host "[OK] plcbridge-web started PID=$webPid"

# Wait up to 30s for :8080
$ready = $false
for ($i = 0; $i -lt 60; $i++) {
    Start-Sleep -Milliseconds 500
    try {
        $c = New-Object System.Net.Sockets.TcpClient
        $iar = $c.BeginConnect('127.0.0.1', 8080, $null, $null)
        if ($iar.AsyncWaitHandle.WaitOne(300)) {
            $c.EndConnect($iar); $c.Close(); $ready = $true; break
        }
        $c.Close()
    } catch {}
}

if (-not $ready) {
    Write-Host "[FAIL] plcbridge-web did not bind :8080 in 30s" -ForegroundColor Red
    Get-Content (Join-Path $log 'plcbridge-web.err.log') -ErrorAction SilentlyContinue | Write-Host
    exit 2
}
Write-Host "[OK] :8080 listening"

# 2) plcbridge core
$corePid = Start-Hidden `
    -Exe (Join-Path $bin 'plcbridge.exe') `
    -ArgString "--config $cfg" `
    -OutLog (Join-Path $log 'plcbridge.out.log') `
    -ErrLog (Join-Path $log 'plcbridge.err.log')
Write-Host "[OK] plcbridge started PID=$corePid"

# Wait up to 20s for :9091 (Prometheus metrics)
for ($i = 0; $i -lt 40; $i++) {
    Start-Sleep -Milliseconds 500
    try {
        $c = New-Object System.Net.Sockets.TcpClient
        $iar = $c.BeginConnect('127.0.0.1', 9091, $null, $null)
        if ($iar.AsyncWaitHandle.WaitOne(300)) {
            $c.EndConnect($iar); $c.Close()
            Write-Host "[OK] :9091 listening"; break
        }
        $c.Close()
    } catch {}
}

# :8082 API may take a bit longer; give it a few more seconds
for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Milliseconds 500
    try {
        $c = New-Object System.Net.Sockets.TcpClient
        $iar = $c.BeginConnect('127.0.0.1', 8082, $null, $null)
        if ($iar.AsyncWaitHandle.WaitOne(300)) {
            $c.EndConnect($iar); $c.Close()
            Write-Host "[OK] :8082 listening"; break
        }
        $c.Close()
    } catch {}
}

Write-Host ""
Write-Host "=== PlcBridge is up ==="
Write-Host ("  web    PID=" + $webPid  + "  http://127.0.0.1:8080/")
Write-Host ("  core   PID=" + $corePid + "  prom http://127.0.0.1:9091/metrics  api http://127.0.0.1:8082/")
if ($influxPid) {
    Write-Host ("  influx PID=" + $influxPid + "  http://127.0.0.1:8086/  (InfluxDB 1.8, 历史查询依赖)")
} else {
    Write-Host "  influx (not started) -- 历史趋势/诊断会返回 503 plcbridge_unavailable" -ForegroundColor Yellow
}
Write-Host ""
Write-Host "Tail logs:"
Write-Host ("  Get-Content '" + (Join-Path $log 'plcbridge-web.out.log')     + "' -Wait")
Write-Host ("  Get-Content '" + (Join-Path $log 'plcbridge.out.log')         + "' -Wait")
if ($influxPid) {
    Write-Host ("  Get-Content '" + (Join-Path $log 'influxd.out.log')         + "' -Wait")
}
Write-Host ""
Write-Host "Stop:"
Write-Host "  .\scripts\hard-kill.ps1"
