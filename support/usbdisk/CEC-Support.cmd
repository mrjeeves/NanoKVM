@echo off
REM Double-clickable launcher for cecsupport.ps1.
REM
REM A .ps1 on a removable drive is not double-clickable: Windows opens it in
REM Notepad, and even Run-with-PowerShell is blocked by the default execution
REM policy on files that came off removable media. A .cmd has neither problem,
REM so this is what the customer actually clicks.
REM
REM -ExecutionPolicy Bypass applies to this one invocation only; it changes
REM nothing on the machine.
setlocal
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0cecsupport.ps1"
if errorlevel 1 pause
