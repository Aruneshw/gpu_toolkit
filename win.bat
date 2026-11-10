@echo off
rem MeshCompute for Windows: double-click this file.
rem It installs or updates MeshCompute, then lets you generate a key (host)
rem or join a friend with their key. Also: win.bat host ^| join KEY ^| reconnect ^| invite ^| gpu
setlocal
title MeshCompute
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0installers\menu-windows.ps1" %*
set "MC_RC=%ERRORLEVEL%"
if not "%MC_RC%"=="0" if not defined MESHCOMPUTE_NO_PAUSE (
  echo.
  echo MeshCompute stopped. Read the message above, or send a screenshot of this window.
  pause
)
exit /b %MC_RC%
