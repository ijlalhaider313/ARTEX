@echo off
rem Switch console to UTF-8, otherwise non-ASCII text in this file is garbled under a GBK terminal.
chcp 65001 >nul 2>&1
rem ARTEX daemon launcher (Windows)
rem
rem Usage:
rem   start.bat                  Run in foreground (Ctrl-C to stop)
rem   start.bat -addr :9000      Extra args are passed through to artex as-is
rem
rem It does one thing: run artex.exe, and after the process exits, decide whether
rem to relaunch based on the exit code.
rem
rem   0      Normal stop        -> exit loop
rem   75     Program requests restart -> relaunch immediately (user clicked "Update" or "Rollback" in the UI)
rem   Other  Crash              -> back off and relaunch (1->2->4... up to 60 seconds)
rem
rem Download, SHA256 verification, and binary swap are NOT handled here;
rem artex does all of that at startup (selfupdate package).
rem The script stays simple — see the top of start.sh for details.

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] Executable not found: %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] Normal exit
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem Update/rollback is ready: after relaunch, artex will complete the swap at startup.
	echo [artex] Restart requested (applying new version)...
	set /a delay=1
	goto loop
)

echo [artex] Abnormal exit ^(code=!code!^), restarting in !delay!s 1>&2
rem timeout fails in a redirected console; fall back to ping (N+1 pings gives ~N second delay).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop