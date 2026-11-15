@echo off
rem Build gurimoon.exe from source. Needs Go 1.24 or newer.
go build -o gurimoon.exe .\cmd\gurimoon\
if errorlevel 1 (
    echo === BUILD FAILED ===
    exit /b 1
)
echo === Build OK: gurimoon.exe ===
