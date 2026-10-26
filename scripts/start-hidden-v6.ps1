# PlcBridge hidden starter (v6: pure .NET, no Start-Process quirks).
#
# Sets PLC_EXE, PLC_ARGS, PLC_CWD, PLC_OUT, PLC_ERR, then starts the
# exe via [System.Diagnostics.Process] and disowns from this PS.
# Prints child PID to stdout, exits 0.

$ErrorActionPreference = 'Stop'

$dbg = Join-Path $env:TEMP "start-hidden-v6.dbg.txt"
function Dbg([string]$s) {
    "[" + (Get-Date -Format 'HH:mm:ss.fff') + "] " + $s | Out-File -LiteralPath $dbg -Append -Encoding ascii
}

Dbg ("v6 start. PLC_EXE=[$env:PLC_EXE] PLC_ARGS=[$env:PLC_ARGS] PLC_CWD=[$env:PLC_CWD]")

try {
    if (-not $env:PLC_EXE)  { [Console]::Error.WriteLine("PLC_EXE not set");  exit 2 }
    if (-not $env:PLC_ARGS) { [Console]::Error.WriteLine("PLC_ARGS not set"); exit 2 }

    $exe = $env:PLC_EXE
    $cwd = if ($env:PLC_CWD) { $env:PLC_CWD } else { Split-Path -Parent $exe }
    $out = $env:PLC_OUT
    $err = $env:PLC_ERR

    if (-not (Test-Path -LiteralPath $exe)) { [Console]::Error.WriteLine("not found: $exe"); exit 2 }
    if (-not (Test-Path -LiteralPath $cwd)) { [Console]::Error.WriteLine("cwd not found: $cwd"); exit 2 }

    $argList = @($env:PLC_ARGS -split '\s+' | Where-Object { $_ -ne '' })
    $argsJoined = ($argList -join ' ')

    Dbg ("launch: " + $exe + " args=[" + $argsJoined + "] cwd=" + $cwd)

    # 关键：ProcessStartInfo.RedirectStandardOutput 是 **bool** 属性，不是路径。
    # 直接赋值路径字符串只会把它置为 $true —— 管道开启但无人读取，
    # 一旦 64KB 管道缓冲写满，子进程会被永久阻塞（现场表现为"服务假死"）。
    # Start-Process -RedirectStandardOutput 走 CreateProcess 的文件句柄直传，
    # 不经过管道，既不阻塞又能拿到真实子进程 PID（cmd /c 只会拿到 cmd 的 PID）。
    $spArgs = @()
    foreach ($a in $argList) { $spArgs += ('"' + $a + '"') }

    $spParams = @{
        FileName               = $exe
        WorkingDirectory       = $cwd
        WindowStyle            = 'Hidden'
        PassThru               = $true
    }
    if ($spArgs.Count -gt 0) { $spParams['ArgumentList'] = $spArgs }
    if ($out) { $spParams['RedirectStandardOutput'] = $out }
    if ($err) { $spParams['RedirectStandardError'] = $err }

    $proc = Start-Process @spParams

    Dbg ("Started PID=" + $proc.Id)
    [Console]::Out.WriteLine($proc.Id)
    # Do NOT WaitForExit. Detach.
    Dbg "v6 done, exiting 0"
    exit 0
} catch {
    Dbg ("EXCEPTION: " + $_)
    [Console]::Error.WriteLine("v6 exception: " + $_)
    exit 4
}
