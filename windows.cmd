@echo off
title MeshCompute
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0installers\menu-windows.ps1" %*
if errorlevel 1 (
  echo.
  echo MeshCompute stopped with an error. Read the message above.
  pause
)
