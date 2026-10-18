@echo off
setlocal

rem PlcBridge hidden starter (Windows batch - v2, direct powershell -File).
rem
rem Why this exists: PowerShell 5.1 has known quirks with env var
rem inheritance across nested Start-Process / powershell -Command calls.
rem This script:
rem   1) Sets PLC_* env vars in *this* cmd process.
rem   2) Calls powershell -File start-hidden-v4.ps1 directly.
rem   3) The launcher inherits env vars (same process tree).
rem   4) Reads PID from stdout via for /f.
rem
rem Pure ASCII. No cmd /c nesting. No PowerShell orchestrator.

set "ROOT=%~dp0.."
pushd "%ROOT%" >nul
set "ROOT=%CD%"
popd >nul

set "BIN=%ROOT%\bin"
set "CFG=%ROOT%\config"
set "LOG=%ROOT%\logs"
set "LAUNCHER=%~dp0start-hidden-v6.ps1"

if not exist "%LOG%" mkdir "%LOG%" >nul 2>&1

if not exist "%BIN%\plcbridge-web.exe" (
    echo [ERROR] missing %BIN%\plcbridge-web.exe
    echo         build: go build -o bin\plcbridge-web.exe .\cmd\plcbridge-web
    exit /b 1
)
if not exist "%BIN%\plcbridge.exe" (
    echo [ERROR] missing %BIN%\plcbridge.exe
    echo         build: go build -o bin\plcbridge.exe .\cmd\plcbridge
    exit /b 1
)

rem Kill any previous instances.
taskkill /IM plcbridge.exe /F      >nul 2>&1
taskkill /IM plcbridge-web.exe /F >nul 2>&1
ping -n 2 127.0.0.1 >nul

rem ---- web ----
set "PLC_EXE=%BIN%\plcbridge-web.exe"
set "PLC_ARGS=--config %CFG% --addr :8080"
set "PLC_OUT=%LOG%\plcbridge-web.out.log"
set "PLC_ERR=%LOG%\plcbridge-web.err.log"
set "PLC_CWD=%ROOT%"
del /q "%PLC_OUT%" "%PLC_ERR%" 2>nul

echo [INFO] launching plcbridge-web ...
for /f "usebackq delims=" %%p in (`powershell -NoProfile -ExecutionPolicy Bypass -File "%LAUNCHER%"`) do (
    set "WEB_PID=%%p"
)
if not defined WEB_PID (
    echo [ERROR] no PID returned for plcbridge-web
    type "%LOG%\plcbridge-web.err.log" 2>nul
    type "%LOG%\plcbridge-web.out.log" 2>nul
    exit /b 1
)
echo [INFO] plcbridge-web PID=%WEB_PID%  log=%PLC_OUT%  err=%PLC_ERR%

rem Give web time to bind :8080.
ping -n 3 127.0.0.1 >nul

rem ---- core ----
set "PLC_EXE=%BIN%\plcbridge.exe"
set "PLC_ARGS=--config %CFG%"
set "PLC_OUT=%LOG%\plcbridge.out.log"
set "PLC_ERR=%LOG%\plcbridge.err.log"
set "PLC_CWD=%ROOT%"
del /q "%PLC_OUT%" "%PLC_ERR%" 2>nul

echo [INFO] launching plcbridge ...
for /f "usebackq delims=" %%p in (`powershell -NoProfile -ExecutionPolicy Bypass -File "%LAUNCHER%"`) do (
    set "CORE_PID=%%p"
)
if not defined CORE_PID (
    echo [ERROR] no PID returned for plcbridge
    type "%LOG%\plcbridge.err.log" 2>nul
    type "%LOG%\plcbridge.out.log" 2>nul
    exit /b 1
)
echo [INFO] plcbridge PID=%CORE_PID%  log=%PLC_OUT%  err=%PLC_ERR%

echo.
echo ================================================================
echo  PlcBridge started hidden.
echo    web:   http://127.0.0.1:8080/
echo    core:  prom http://127.0.0.1:9091/metrics
echo.
echo  Tail logs (PowerShell):
echo    Get-Content "%LOG%\plcbridge-web.out.log" -Wait
echo    Get-Content "%LOG%\plcbridge.out.log"     -Wait
echo.
echo  Stop:
echo    taskkill /IM plcbridge.exe /IM plcbridge-web.exe /F
echo ================================================================
exit /b 0
