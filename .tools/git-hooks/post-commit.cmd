@echo off
REM Native Windows twin of .tools/git-hooks/post-commit.
REM
REM Git for Windows also runs .cmd hooks, which matters in one case the shell
REM version cannot cover: when the commit is made from inside a DSH session, its
REM sandbox denies MSYS sh.exe the kernel objects it needs, so the sh hook dies
REM before it can push. cmd.exe has no such problem.
REM
REM Like the sh version it never blocks a commit.

setlocal
set "REMOTE=backup"

git remote get-url %REMOTE% >nul 2>&1
if errorlevel 1 exit /b 0

for /f "delims=" %%b in ('git symbolic-ref --short -q HEAD 2^>nul') do set "BRANCH=%%b"

if defined BRANCH (
  git push --quiet %REMOTE% %BRANCH% >nul 2>&1
  if errorlevel 1 (
    echo.
    echo   note: the backup at '%REMOTE%' was not updated ^(push failed^).
    echo         the commit is safe locally; run: git push %REMOTE% --all
    echo.
  )
) else (
  git push --quiet %REMOTE% --all >nul 2>&1
)

git push --quiet %REMOTE% --tags >nul 2>&1
exit /b 0
