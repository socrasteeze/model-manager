@echo off
setlocal

rem Model Manager launcher for Windows.
rem
rem Double-click it, or run it from a terminal:
rem
rem     start.bat              normal start, downloads enabled
rem     start.bat readonly     no writes at all
rem     start.bat offline      no outbound requests to any provider
rem
rem Edit the settings block below to change the port, the database location, or
rem your API keys. None of it is required; the defaults work.

rem ---------------------------------------------------------------- settings

set "MM_PORT=8737"
set "MM_HOST=127.0.0.1"

rem Leave empty to use the default: %APPDATA%\model-manager\master.db
set "MM_DB="

rem Optional, and only needed for models gated behind a Civitai login. Browsing
rem and public downloads work without one.
rem
rem Prefer keeping keys out of this file. Set them once in your environment
rem instead and leave these blank:
rem
rem     setx CIVITAI_API_KEY "your-key"
rem     setx HF_TOKEN "your-token"
rem
rem then reopen the terminal. Anything already in the environment is used as-is.
if not defined CIVITAI_API_KEY set "CIVITAI_API_KEY="
if not defined HF_TOKEN set "HF_TOKEN="

rem Which Civitai catalogue to search. civitai.com is the main site, civitai.red
rem the adult split, civitai.green the safe-for-work one. They share one API, so
rem this only decides what a search can return. Example:
rem
rem     set "MM_CIVITAI_API=https://civitai.red/api/v1"
if not defined MM_CIVITAI_API set "MM_CIVITAI_API="

rem ------------------------------------------------------- locate the binary

cd /d "%~dp0"

set "MM_EXE="
if exist "%~dp0mm.exe" set "MM_EXE=%~dp0mm.exe"
if not defined MM_EXE if exist "%~dp0bin\mm.exe" set "MM_EXE=%~dp0bin\mm.exe"

if not defined MM_EXE (
    echo.
    echo   Could not find mm.exe. Looked in:
    echo     %~dp0mm.exe
    echo     %~dp0bin\mm.exe
    echo.
    where go >nul 2>&1
    if errorlevel 1 (
        echo   Go is not installed, so this cannot build it for you.
        echo   Download a release build and put mm.exe next to this file.
        goto :fail
    )
    echo   Go is installed. Building...
    echo.
    go build -o "bin\mm.exe" ".\cmd\mm"
    if errorlevel 1 (
        echo.
        echo   Build failed; see the errors above.
        goto :fail
    )
    set "MM_EXE=%~dp0bin\mm.exe"
    echo   Built bin\mm.exe
)

rem -------------------------------------------------------------------- mode

rem Writable by default: you opened this to use the app, and downloading needs
rem it. The daemon itself defaults to read-only, so this is the single place
rem that choice gets made -- and it is printed rather than assumed.
set "MM_MODE=--writable"
set "MM_REMOTE="
set "MM_LABEL=writable (downloads enabled)"

if /i "%~1"=="readonly" (
    set "MM_MODE="
    set "MM_LABEL=read-only (no writes, no downloads)"
)
if /i "%~1"=="offline" (
    set "MM_MODE="
    set "MM_REMOTE=--no-remote"
    set "MM_LABEL=offline (no outbound requests, no downloads)"
)

set "MM_DBARG="
if not "%MM_DB%"=="" set MM_DBARG=--db "%MM_DB%"

rem ------------------------------------------------------------------ banner

echo.
echo   Model Manager
echo   -------------
echo   mode      %MM_LABEL%
echo   address   http://%MM_HOST%:%MM_PORT%
if not "%MM_DB%"=="" echo   database  %MM_DB%
if not "%MM_CIVITAI_API%"=="" echo   civitai   %MM_CIVITAI_API%
if "%CIVITAI_API_KEY%"=="" echo   note      no Civitai API key; gated models will not download
echo.
echo   Close this window to stop the server.
echo.

rem Open the browser once the port is actually accepting connections. A fixed
rem delay is either too short on a cold start or a pointless wait when warm.
rem This runs in its own process so the server keeps this console.
where powershell >nul 2>&1
if not errorlevel 1 (
    start "" /b powershell -NoProfile -WindowStyle Hidden -Command "for ($i=0; $i -lt 60; $i++) { try { $c = New-Object Net.Sockets.TcpClient('%MM_HOST%', %MM_PORT%); $c.Close(); Start-Process 'http://%MM_HOST%:%MM_PORT%/'; break } catch { Start-Sleep -Milliseconds 500 } }"
)

rem ------------------------------------------------------------------- serve

"%MM_EXE%" serve --host "%MM_HOST%" --port %MM_PORT% %MM_DBARG% %MM_MODE% %MM_REMOTE%
if errorlevel 1 goto :fail

endlocal
exit /b 0

:fail
echo.
echo   Model Manager did not start.
echo.
pause
exit /b 1
