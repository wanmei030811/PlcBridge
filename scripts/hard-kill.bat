@echo off
rem Wrapper: powershell -File hard-kill.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0hard-kill.ps1"
exit /b %ERRORLEVEL%
