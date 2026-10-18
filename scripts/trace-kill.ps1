# PlcBridge 实时监控 kill: 杀掉目标后立即列进程树, 找出真正的拉起源
$ErrorActionPreference = 'Continue'

function Kill([int]$pid, [string]$label) {
    $p = Get-WmiObject Win32_Process -Filter ("ProcessId=" + $pid) -ErrorAction SilentlyContinue
    if ($p) {
        Write-Host ("  WMI Terminate " + $label + " PID=" + $pid)
        $r = $p.Terminate()
        Write-Host ("    ReturnValue=" + $r.ReturnValue)
    } else {
        Write-Host ("  already gone: " + $label)
    }
}

function Show-Tree([string]$rootName) {
    $roots = Get-WmiObject Win32_Process -Filter ("Name='" + $rootName + "'") -ErrorAction SilentlyContinue
    foreach ($root in $roots) {
        Write-Host ""
        Write-Host ("=== 树 (root=" + $root.Name + " PID=" + $root.ProcessId + ") ===") -ForegroundColor Cyan
        $stack = New-Object System.Collections.Stack
        $stack.Push(@{ Proc = $root; Depth = 0 })
        while ($stack.Count -gt 0) {
            $cur = $stack.Pop()
            $p = $cur.Proc
            $prefix = "  " * $cur.Depth
            Write-Host ($prefix + "PID=" + $p.ProcessId + " PPID=" + $p.ParentProcessId + " Name=" + $p.Name)
            if ($p.CommandLine) {
                $cl = $p.CommandLine
                if ($cl.Length -gt 200) { $cl = $cl.Substring(0, 200) + "..." }
                Write-Host ($prefix + "  Cmd: " + $cl)
            }
            $children = Get-WmiObject Win32_Process -Filter ("ParentProcessId=" + $p.ProcessId) -ErrorAction SilentlyContinue
            foreach ($c in $children) {
                $stack.Push(@{ Proc = $c; Depth = $cur.Depth + 1 })
            }
        }
    }
}

Write-Host "=== 1. 杀 plcbridge / plcbridge-web / supervisor ===" -ForegroundColor Cyan
$targets = Get-WmiObject Win32_Process -Filter "Name='plcbridge.exe' or Name='plcbridge-web.exe'" -ErrorAction SilentlyContinue
foreach ($t in $targets) { Kill $t.ProcessId $t.Name }
$pws = Get-WmiObject Win32_Process -Filter "Name='powershell.exe'" -ErrorAction SilentlyContinue | Where-Object { $_.CommandLine -match 'supervisor' }
foreach ($p in $pws) { Kill $p.ProcessId 'supervisor' }

Start-Sleep -Seconds 2

Write-Host ""
Write-Host "=== 2. 残留 (按 plcbridge / plcbridge-web 名) ===" -ForegroundColor Cyan
$left = Get-WmiObject Win32_Process -ErrorAction SilentlyContinue | Where-Object { $_.Name -match 'plcbridge' }
if ($left) {
    $left | Select-Object ProcessId,ParentProcessId,Name,CommandLine | Format-List | Out-String | Write-Host
} else {
    Write-Host "  (无)" -ForegroundColor Green
}

Write-Host ""
Write-Host "=== 3. plcbridge-web.exe 进程树 (若有残留) ===" -ForegroundColor Cyan
Show-Tree 'plcbridge-web.exe'

Write-Host ""
Write-Host "=== 4. 端口 8080 LISTENING ===" -ForegroundColor Cyan
$conn = netstat -ano | Select-String "LISTENING" | Select-String ":8080"
if ($conn) { $conn | ForEach-Object { Write-Host ("  " + $_.Line) } }
else { Write-Host "  (无)" -ForegroundColor Green }

Write-Host ""
Write-Host "=== 5. WMI 子进程链 (找 PLC) ===" -ForegroundColor Cyan
# 找出 PID=1 (System Idle) 之外所有 plcbridge 相关
Get-WmiObject Win32_Process -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -match 'plcbridge' -or ($_.CommandLine -match 'plcbridge') } |
    Select-Object ProcessId,ParentProcessId,Name,CommandLine |
    Sort-Object ProcessId |
    Format-Table -AutoSize -Wrap
