@echo off
setlocal

echo === PlcBridge 启动链路诊断 ===
echo.

echo [1] 进程:
tasklist /FI "IMAGENAME eq plcbridge.exe"      2>nul | findstr /v "INFO:"
tasklist /FI "IMAGENAME eq plcbridge-web.exe"  2>nul | findstr /v "INFO:"
tasklist /FI "IMAGENAME eq powershell.exe"     2>nul | findstr /v "INFO:"

echo.
echo [2] 所有 powershell.exe 进程的父进程 + 命令行:
powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='powershell.exe'\" | Select-Object ProcessId,ParentProcessId,CommandLine | Format-List"

echo.
echo [3] 所有 cmd.exe 进程 + 命令行:
powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='cmd.exe'\" | Select-Object ProcessId,ParentProcessId,CommandLine | Format-List"

echo.
echo [4] 计划任务 (含 plcbridge):
schtasks /QUERY /FO LIST 2>nul | findstr /I /B "HostName TaskName Status"

echo.
echo [5] 端口 8080/9091 监听者:
netstat -ano | findstr "LISTENING" | findstr ":8080 "
netstat -ano | findstr "LISTENING" | findstr ":9091 "

echo.
echo [6] 注册表 Run / RunOnce (开机自启):
reg query "HKLM\Software\Microsoft\Windows\CurrentVersion\Run"     2>nul | findstr /I plcbridge
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Run"     2>nul | findstr /I plcbridge
reg query "HKLM\Software\Microsoft\Windows\CurrentVersion\RunOnce" 2>nul | findstr /I plcbridge

echo.
echo [7] WMI EventFilter / EventConsumer (持久化机制):
powershell -NoProfile -Command "Get-WmiObject -Namespace root\subscription -Class __EventFilter -ErrorAction SilentlyContinue | Where-Object { \$_.Name -match 'plcbridge' -or \$_.Query -match 'plcbridge' } | Format-List; Get-WmiObject -Namespace root\subscription -Class CommandLineEventConsumer -ErrorAction SilentlyContinue | Where-Object { \$_.Name -match 'plcbridge' -or \$_.CommandLineTemplate -match 'plcbridge' } | Format-List"

echo.
echo === 诊断结束 ===
endlocal
