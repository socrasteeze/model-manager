@echo off
setlocal

rem Build Model Manager from source on Windows.
rem
rem     build.bat              build the binary
rem     build.bat ui           rebuild the web UI first, then the binary
rem
rem The output is named the same as the published release asset,
rem mm-windows-amd64.exe, so start.bat finds it with no renaming.
rem
rem Nothing here needs a network connection except the very first build, which
rem downloads the Go module dependencies.

cd /d "%~dp0"

rem -------------------------------------------------------------- locate Go

rem Checked in three places rather than just the PATH. The Windows installer
rem adds Go to the PATH for *new* shells only, so a terminal that was already
rem open when Go was installed will not find it -- which looks like "Go is not
rem installed" while it sits in Program Files.
set "MM_GO="
where go >nul 2>&1
if not errorlevel 1 set "MM_GO=go"
if not defined MM_GO if exist "%ProgramFiles%\Go\bin\go.exe" set "MM_GO=%ProgramFiles%\Go\bin\go.exe"
if not defined MM_GO if exist "%LOCALAPPDATA%\Programs\Go\bin\go.exe" set "MM_GO=%LOCALAPPDATA%\Programs\Go\bin\go.exe"

if not defined MM_GO (
    echo.
    echo   Go is not installed, or not where this looked:
    echo     on the PATH
    echo     "%ProgramFiles%\Go\bin\go.exe"
    echo     "%LOCALAPPDATA%\Programs\Go\bin\go.exe"
    echo.
    echo   Install it with:  winget install GoLang.Go
    echo   ...then open a new terminal, or just download a prebuilt binary:
    echo     https://github.com/socrasteeze/model-manager/releases/latest
    goto :fail
)

rem ---------------------------------------------------------------- version

rem Stamped into the binary the same way the release workflow does it, so
rem `mm version` reports something meaningful instead of "dev".
rem
rem An exact tag gives a clean "v0.4.2". Anything else gives the nearest tag
rem plus how far past it you are and the commit -- "v0.4.2-3-gabc1234" -- with
rem "-dirty" appended when the tree has uncommitted changes, which is worth
rem knowing when a build behaves unlike the release it claims to be.
set "MM_VERSION="
where git >nul 2>&1
if errorlevel 1 goto :noversion
for /f "delims=" %%v in ('git describe --tags --exact-match HEAD 2^>nul') do set "MM_VERSION=%%v"
if defined MM_VERSION goto :haveversion
for /f "delims=" %%v in ('git describe --tags --always --dirty 2^>nul') do set "MM_VERSION=%%v"
if defined MM_VERSION goto :haveversion
:noversion
set "MM_VERSION=dev"
:haveversion

rem ------------------------------------------------------------------- arch

rem Named to match the release assets. Windows on ARM is a real target; every
rem other machine is amd64.
set "MM_ARCH=amd64"
if /i "%PROCESSOR_ARCHITECTURE%"=="ARM64" set "MM_ARCH=arm64"
set "MM_OUT=mm-windows-%MM_ARCH%.exe"

rem --------------------------------------------------------------- web UI

rem Skipped by default, and that is not a shortcut: the built UI is committed
rem under internal/webui/dist and embedded into the binary, so a source build
rem needs no Node at all. Only worth running after changing something in web/.
if /i not "%~1"=="ui" goto :build

where npm >nul 2>&1
if errorlevel 1 (
    echo.
    echo   Node is not installed, so the UI cannot be rebuilt.
    echo   The committed UI is used instead -- which is correct unless you
    echo   have edited something under web\.
    echo.
    goto :build
)
echo.
echo   Rebuilding the web UI...
call npm --prefix web install --no-audit --no-fund
if errorlevel 1 goto :uifail
call npm --prefix web run build
if errorlevel 1 goto :uifail
goto :build

:uifail
echo.
echo   The UI build failed; see the errors above.
goto :fail

rem ------------------------------------------------------------------ build

:build
echo.
echo   Model Manager
echo   -------------
echo   version   %MM_VERSION%
echo   target    windows/%MM_ARCH%
echo   output    %MM_OUT%
echo.
echo   Building...

rem CGO off because the SQLite driver is pure Go. That is what makes this a
rem single self-contained .exe with no C toolchain and no DLLs to ship.
set "CGO_ENABLED=0"
set "GOOS=windows"
set "GOARCH=%MM_ARCH%"

rem -trimpath keeps local absolute paths out of the binary; -s -w drop the
rem symbol table and DWARF data, which is most of the file size. Both match
rem the release workflow so a local build is comparable to a downloaded one.
"%MM_GO%" build -trimpath -ldflags "-s -w -X main.version=%MM_VERSION%" -o "%MM_OUT%" ".\cmd\mm"
if errorlevel 1 (
    echo.
    echo   Build failed; see the errors above.
    goto :fail
)

echo   Built %MM_OUT%
echo.

rem A locally built binary carries no mark-of-the-web, so unlike a downloaded
rem release it does not need Properties -^> Unblock before it will run.

rem ------------------------------------------------------- shadowing check

rem start.bat looks for mm.exe before the release name, so an older mm.exe
rem sitting here would silently keep being launched and this build would never
rem run. Reported rather than deleted: removing a file the operator did not ask
rem about is not this script's call.
if exist "%~dp0mm.exe" (
    echo   Note: mm.exe is also present, and start.bat prefers it over
    echo   %MM_OUT%. Delete it so this build is the one that runs:
    echo.
    echo       del mm.exe
    echo.
)
if exist "%~dp0bin\mm.exe" (
    echo   Note: bin\mm.exe is also present and takes precedence over
    echo   %MM_OUT%. Delete it so this build is the one that runs:
    echo.
    echo       del bin\mm.exe
    echo.
)

echo   Start it with:  start.bat
echo.

endlocal
exit /b 0

:fail
echo.
echo   Nothing was built.
echo.
pause
exit /b 1
