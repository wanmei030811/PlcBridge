# check-no-root-exe.ps1
# 强约束：根目录不能出现 plcbridge.exe / plcbridge-web.exe（必须放 bin/）。
# 退出码 0 = 通过；1 = 发现违规。
# 用法：
#   powershell -File scripts\check-no-root-exe.ps1
#   或在 CI 里：失败就拒合并。

$ErrorActionPreference = 'Stop'
# 强制 UTF-8 读 / 写，避免 PowerShell 5.1 默认 ANSI 把中文搞坏
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8
$root = Resolve-Path (Join-Path $PSScriptRoot '..')
$forbidden = @('plcbridge.exe', 'plcbridge-web.exe')
$bad = @()
foreach ($name in $forbidden) {
    $p = Join-Path $root $name
    if (Test-Path $p) { $bad += $p }
}
if ($bad.Count -gt 0) {
    Write-Host '[FAIL] Root directory has forbidden binaries (use `make build` to put them under bin/):' -ForegroundColor Red
    foreach ($p in $bad) {
        $alt = Join-Path (Join-Path $root 'bin') (Split-Path $p -Leaf)
        Write-Host ('  - {0}' -f $p) -ForegroundColor Red
        if (Test-Path $alt) {
            Write-Host ('      identical copy exists at: {0}' -f $alt) -ForegroundColor Yellow
        } else {
            Write-Host '      missing in bin/; run `make build` first.' -ForegroundColor Yellow
        }
    }
    $quoted = ($bad | ForEach-Object { "'$_'" }) -join ','
    Write-Host ''
    Write-Host ('Fix: powershell -Command "Remove-Item -Force {0} -ErrorAction SilentlyContinue"' -f $quoted) -ForegroundColor Cyan
    exit 1
}
Write-Host '[OK] No plcbridge.exe / plcbridge-web.exe in project root.' -ForegroundColor Green
exit 0
