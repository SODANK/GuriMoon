@echo off
rem Build gurimoon.exe from source. Needs Go 1.24 or newer.
rem All logic lives in rebuild.ps1.
rem   rebuild          - build WITH every module in modules\, replace gurimoon.exe
rem   rebuild check    - compile check WITH modules (build\gurimoon-check.exe;
rem                      the live gurimoon.exe is not touched)
rem   rebuild core     - plain module-less build, replaces gurimoon.exe
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0rebuild.ps1" %1
exit /b %ERRORLEVEL%
