@echo off
setlocal

rem Model Manager launcher for Windows.
rem
rem Double-click it, or run it from a terminal:
rem
rem     start.bat              app window, downloads enabled
rem     start.bat browser      open in a normal browser tab instead
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

rem API keys are optional, and only needed for models gated behind a login.
rem Browsing and public downloads work without them.
rem
rem Prefer keeping keys out of this file. Set them once in your environment
rem and they are picked up automatically:
rem
rem     setx CIVITAI_API_KEY "your-key"
rem     setx HF_TOKEN "your-token"
rem
rem then reopen the terminal. To set one here instead, uncomment and fill in:
rem
rem     set "CIVITAI_API_KEY=your-key"
rem     set "HF_TOKEN=your-token"

rem Which Civitai catalogue to search. civitai.com is the main site, civitai.red
rem the adult split, civitai.green the safe-for-work one. They share one API, so
rem this only decides what a search can return. Example:
rem
rem     set "MM_CIVITAI_API=https://civitai.red/api/v1"

rem Another Model Manager holding the main collection -- usually the machine the
rem library lives on. Setting this makes it searchable in Browse alongside the
rem public providers, and lets you pull models from it onto this machine.
rem
rem The token is that daemon's api-token file, needed whenever it is reachable
rem from anywhere but its own loopback. Prefer setx over writing it here:
rem
rem     setx MM_UPSTREAM_URL "http://library:8737"
rem     setx MM_UPSTREAM_TOKEN "the-token-from-its-api-token-file"
rem     setx MM_UPSTREAM_NAME "Library"
rem
rem That machine also needs to be started with --serve-files before it will hand
rem any model files over.

rem ------------------------------------------------------- locate the binary

cd /d "%~dp0"

rem The release asset is published under its platform name, mm-windows-amd64.exe.
rem Renaming it to mm.exe is one step in the README and an easy one to skip, so
rem the download is accepted as-is rather than met with "could not find mm.exe"
rem while the binary sits right there.
set "MM_EXE="
if exist "%~dp0mm.exe" set "MM_EXE=%~dp0mm.exe"
if not defined MM_EXE if exist "%~dp0bin\mm.exe" set "MM_EXE=%~dp0bin\mm.exe"
if not defined MM_EXE if exist "%~dp0mm-windows-amd64.exe" set "MM_EXE=%~dp0mm-windows-amd64.exe"
if not defined MM_EXE if exist "%~dp0bin\mm-windows-amd64.exe" set "MM_EXE=%~dp0bin\mm-windows-amd64.exe"

if not defined MM_EXE (
    echo.
    echo   Could not find mm.exe. Looked in:
    rem Quoted on purpose. An unquoted %~dp0 inside a parenthesized block ends
    rem the block early whenever the path contains a closing bracket -- the
    rem Program Files x86 folder, or a twice-unzipped "model-manager 1" copy.
    echo     "%~dp0mm.exe"
    echo     "%~dp0bin\mm.exe"
    echo   ...and under the release name, mm-windows-amd64.exe, in both.
    echo.
    where go >nul 2>&1
    if errorlevel 1 (
        echo   Go is not installed, so this cannot build it for you.
        echo   Download a release build and put mm.exe next to this file:
        echo     https://github.com/socrasteeze/model-manager/releases/latest
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
set "MM_WINDOW=app"

if /i "%~1"=="browser" (
    set "MM_WINDOW=browser"
)
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

set "MM_URL=http://%MM_HOST%:%MM_PORT%/"

rem ------------------------------------------------------- resolve the opener

rem App mode gives a real window with no tab strip or address bar, so it reads
rem as a desktop application rather than a browser tab. Edge ships with Windows
rem 11 so it is the usual path; Chrome is tried next; a plain browser tab is the
rem fallback and is what "start.bat browser" asks for outright.
rem
rem Either way it is the same local server being displayed, which is what keeps
rem the phone and tailnet access working at the same time.
rem
rem Resolved here, before anything is launched, so the waiter below can be told
rem exactly what to run. Re-invoking this script to do the opening would risk a
rem second server on a path that fell through.
set "MM_OPEN_EXE="
if /i not "%MM_WINDOW%"=="browser" (
    if exist "%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe" set "MM_OPEN_EXE=%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe"
    if not defined MM_OPEN_EXE if exist "%ProgramFiles%\Microsoft\Edge\Application\msedge.exe" set "MM_OPEN_EXE=%ProgramFiles%\Microsoft\Edge\Application\msedge.exe"
    if not defined MM_OPEN_EXE if exist "%ProgramFiles%\Google\Chrome\Application\chrome.exe" set "MM_OPEN_EXE=%ProgramFiles%\Google\Chrome\Application\chrome.exe"
    if not defined MM_OPEN_EXE if exist "%ProgramFiles(x86)%\Google\Chrome\Application\chrome.exe" set "MM_OPEN_EXE=%ProgramFiles(x86)%\Google\Chrome\Application\chrome.exe"
)

rem ------------------------------------------------- already running? attach

rem Checked before launching rather than after: starting the opener first meant
rem it would connect to whatever already held the port, open a window onto that
rem instance, and then this script would report "did not start" next to a
rem window that plainly had.
powershell -NoProfile -Command "try { $c = New-Object Net.Sockets.TcpClient('%MM_HOST%', %MM_PORT%); $c.Close(); exit 0 } catch { exit 1 }" >nul 2>&1
if not errorlevel 1 (
    echo.
    echo   Model Manager is already running on port %MM_PORT% — opening it.
    echo.
    call :open
    goto :eof
)

rem ------------------------------------------------------------------ banner

echo.
echo   Model Manager
echo   -------------
echo   mode      %MM_LABEL%
echo   address   %MM_URL%
if not "%MM_DB%"=="" echo   database  %MM_DB%
if not "%MM_CIVITAI_API%"=="" echo   civitai   %MM_CIVITAI_API%
if "%CIVITAI_API_KEY%"=="" echo   note      no Civitai API key; gated models will not download
echo.
echo   Close this window to stop the server.
echo.

rem Open once the port actually accepts a connection, rather than after a fixed
rem delay that is either too short on a cold start or a pointless wait when
rem warm. Runs in its own process so the server keeps this console.
if defined MM_OPEN_EXE (
    start "" /b powershell -NoProfile -WindowStyle Hidden -Command "for ($i=0; $i -lt 60; $i++) { try { $c = New-Object Net.Sockets.TcpClient('%MM_HOST%', %MM_PORT%); $c.Close(); Start-Process -FilePath '%MM_OPEN_EXE%' -ArgumentList '--app=%MM_URL%'; break } catch { Start-Sleep -Milliseconds 500 } }"
) else (
    start "" /b powershell -NoProfile -WindowStyle Hidden -Command "for ($i=0; $i -lt 60; $i++) { try { $c = New-Object Net.Sockets.TcpClient('%MM_HOST%', %MM_PORT%); $c.Close(); Start-Process '%MM_URL%'; break } catch { Start-Sleep -Milliseconds 500 } }"
)

rem ------------------------------------------------------------------- serve

rem No errorlevel check afterwards. Go exits non-zero on Ctrl+C, and on Windows
rem Ctrl+C in a batch file additionally prompts "Terminate batch job?" -- both
rem ordinary ways to stop the server, neither a failure worth a scary message.
rem A genuine bind failure prints its own error above and the port pre-check
rem catches the common cause.
"%MM_EXE%" serve --host "%MM_HOST%" --port %MM_PORT% %MM_DBARG% %MM_MODE% %MM_REMOTE%

endlocal
exit /b 0

rem ------------------------------------------------------------- open helper

:open
rem Uses the opener resolved above; see that block for the ordering.
if defined MM_OPEN_EXE (
    start "" "%MM_OPEN_EXE%" --app=%MM_URL%
) else (
    start "" "%MM_URL%"
)
exit /b 0

:fail
echo.
echo   Model Manager did not start.
echo.
pause
exit /b 1
