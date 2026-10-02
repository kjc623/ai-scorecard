@echo off
REM ---------------------------------------------------------------------------
REM Project Cockpit - finish the install, then start DeepSeek Harness.
REM
REM Why this exists: `pnpm add file:` COPIES this workspace into the DSH profile,
REM and Windows keeps those copied files open while DSH runs. The copy therefore
REM cannot be refreshed from inside a session - not by the agent, not by you.
REM The only way is with DSH closed, which is what this script does.
REM
REM Run it by double-clicking, or from a terminal:
REM     .cockpit\tools\relaunch.cmd
REM ---------------------------------------------------------------------------

setlocal
set "WORKSPACE=%~dp0..\.."
set "NODE=%USERPROFILE%\.dsh\dsh-runtimes\dsh-primary-runtime\dependencies\node\bin\node.exe"

echo.
echo Project Cockpit - syncing the install from the workspace
echo   workspace: %WORKSPACE%
echo.

if not exist "%NODE%" (
  echo ERROR: could not find the bundled Node runtime at:
  echo   %NODE%
  echo.
  echo Sync the install by hand instead: copy
  echo   %WORKSPACE%\*
  echo over
  echo   %USERPROFILE%\.dsh\profiles\desktop\node_modules\dsh-project-cockpit\
  echo keeping the folder structure, then start DeepSeek Harness.
  echo.
  pause
  exit /b 1
)

"%NODE%" "%WORKSPACE%\.cockpit\tools\sync-install.mjs" --apply
if errorlevel 1 (
  echo.
  echo The sync did not fully succeed. If DeepSeek Harness is still running,
  echo close it completely and run this again.
  echo.
  pause
  exit /b 1
)

echo.
echo Starting DeepSeek Harness...
echo Once it is up, open this session and click the Project Cockpit icon in the
echo left sidebar toolbar.
echo.

set "APP=%LOCALAPPDATA%\Programs\DeepSeek Harness\DeepSeek Harness.exe"
if exist "%APP%" (
  start "" "%APP%"
) else (
  echo Could not find DeepSeek Harness automatically. Start it yourself.
)
endlocal
