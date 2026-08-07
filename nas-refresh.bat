@echo off
setlocal

rem Rebuild and restart Model Manager on the NAS, from the current main.
rem
rem     nas-refresh.bat                          run the usual update script
rem     nas-refresh.bat ~/other/nas-update.sh    run a different one
rem
rem Set the destination once and reopen the terminal:
rem
rem     setx MM_NAS_HOST "user@nas-hostname"
rem
rem It is an environment variable rather than a line in this file because this
rem repo is public, and the account and host name of somebody's NAS do not
rem belong in it.
rem
rem This runs the update over SSH's exec channel. SFTP on a TerraMaster box is
rem a separate mechanism and is known to be broken there -- a firmware update
rem removed the sftp-server binary and the built-in one faults on write. That
rem does not affect this. If the exec channel ever stops working too, run
rem `sh ~/model-manager/nas-update.sh` from a terminal session on the NAS: only
rem the one-command trigger is lost, not the deployment.

if not defined MM_NAS_HOST (
    echo.
    echo   MM_NAS_HOST is not set. Point it at the NAS and reopen this terminal:
    echo.
    echo       setx MM_NAS_HOST "user@nas-hostname"
    echo.
    exit /b 1
)

set "NAS_PATH=~/model-manager/nas-update.sh"
if not "%~1"=="" set "NAS_PATH=%~1"

where ssh >nul 2>&1
if errorlevel 1 (
    echo.
    echo   No ssh client found. Add it with:
    echo     Settings ^> Apps ^> Optional Features ^> Add ^> OpenSSH Client
    echo.
    exit /b 1
)

echo.
echo   Refreshing Model Manager on %MM_NAS_HOST%
echo   Running %NAS_PATH%
echo.

rem Output streams back as it happens, so a build failure is visible where it
rem occurs rather than as a non-zero exit code after two silent minutes.
ssh %MM_NAS_HOST% "sh %NAS_PATH%"

if errorlevel 1 (
    echo.
    echo   Refresh failed. The daemon on the NAS is untouched if the build never
    echo   completed; check with:  ssh %MM_NAS_HOST% "docker logs model-manager"
    echo.
    exit /b 1
)

endlocal
exit /b 0
