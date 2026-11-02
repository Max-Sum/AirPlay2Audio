@echo off
cd /d "%~dp0"
echo Starting AirPlay 2 Audio. Press Ctrl+C to stop.
start "" /realtime /b /wait AirPlay2Engine.exe -n "AirPlay 2 Audio"
pause
