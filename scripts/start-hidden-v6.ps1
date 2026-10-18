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

    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $exe
    # PS 5.1 / .NET Framework: no ArgumentList collection. Use Arguments string.
    $psi.Arguments = $argsJoined
    $psi.WorkingDirectory = $cwd
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow  = $true
    $psi.WindowStyle     = 'Hidden'
    if ($out) { $psi.RedirectStandardOutput = $out }
    if ($err) { $psi.RedirectStandardError  = $err }

    Dbg ("ProcessStartInfo: " + $exe + " args=[" + $argsJoined + "] cwd=" + $cwd)

    $proc = New-Object System.Diagnostics.Process
    $proc.StartInfo = $psi
    [void]$proc.Start()

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
