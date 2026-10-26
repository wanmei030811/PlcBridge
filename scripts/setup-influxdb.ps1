# 下载并安装 InfluxDB 1.8（Windows, amd64）到 tools\influxdb\
#
# 为什么需要：
#   现场 config.json 的 forward channel "influxdb" 指向 http://localhost:8086，
#   version = "1.x"（走 InfluxQL 方言）。没有 InfluxDB 实例时，
#   /api/v1/targets/:id/history 会返回 500 history_query_failed（连接被拒绝）。
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File scripts\setup-influxdb.ps1
#
# 说明：
#   - tools/ 已在 .gitignore 中，下载的二进制不入库
#   - 选 1.8.10 而不是 2.x：现场 config 是 "1.x"，1.8 兼容且不强制鉴权
#   - 数据目录在 config\runtime\influxdb\（同样不入库）

$ErrorActionPreference = 'Stop'
$root   = 'F:\Project\PlcBridge'
$tools  = Join-Path $root 'tools'
$dest   = Join-Path $tools 'influxdb'
$zipUrl = 'https://dl.influxdata.com/influxdb/releases/influxdb-1.8.10_windows_amd64.zip'
$zip    = Join-Path $tools 'influxdb-1.8.10.zip'

$influxd = Join-Path $dest 'influxdb-1.8.10-1\influxd.exe'
if (Test-Path $influxd) {
    Write-Host "[OK] InfluxDB 已安装: $influxd" -ForegroundColor Green
    exit 0
}

New-Item -ItemType Directory -Force -Path $tools | Out-Null

if (-not (Test-Path $zip)) {
    Write-Host "[..] 下载 $zipUrl" -ForegroundColor Cyan
    # 50MB 左右，网络慢时可能要几分钟
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -Uri $zipUrl -OutFile $zip -UseBasicParsing -TimeoutSec 900
}
Write-Host ("[OK] 下载完成 ({0:N1} MB)" -f ((Get-Item $zip).Length / 1MB)) -ForegroundColor Green

Write-Host "[..] 解压到 $dest" -ForegroundColor Cyan
if (Test-Path $dest) { Remove-Item $dest -Recurse -Force }
Expand-Archive -Path $zip -DestinationPath $dest -Force

if (-not (Test-Path $influxd)) {
    Write-Host "[FAIL] 未找到 influxd.exe，解压结果：" -ForegroundColor Red
    Get-ChildItem $dest -Recurse -Filter '*.exe' | ForEach-Object { Write-Host "    $($_.FullName)" }
    exit 1
}

Write-Host "[OK] 安装完成: $influxd" -ForegroundColor Green
Write-Host ""
Write-Host "下一步："
Write-Host "  1. 确认配置已存在: config\runtime\influxdb\influxdb.conf"
Write-Host "  2. 启动全部服务:    .\scripts\run-direct.ps1  (会自动拉起 influxd)"
Write-Host "  3. 创建数据库:      http://127.0.0.1:8086/query?q=CREATE%20DATABASE%20plcbridge"
Write-Host ""
Write-Host "注意：run-direct.ps1 不会自动建库。若 plcbridge 日志报"
Write-Host "  'database not found'，请执行上面第 3 步。"
