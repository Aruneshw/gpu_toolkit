@echo off
setlocal
rem Started from PowerShell 7, Windows PowerShell 5.1 would inherit 7s module
rem path and fail to load built-in cmdlets (Get-FileHash). Use its own defaults.
set "PSModulePath="
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0installers\install-windows.ps1" -Join %*
exit /b %ERRORLEVEL%
