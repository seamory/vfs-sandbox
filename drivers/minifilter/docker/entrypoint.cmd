@echo off
setlocal
call C:\EWDK\BuildEnv\SetupBuildEnv.cmd
if errorlevel 1 exit /b %errorlevel%
where msbuild.exe >nul 2>&1
if errorlevel 1 (
  echo EWDK did not initialize MSBuild.
  exit /b 1
)
powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File C:\build\build-container.ps1 %*
exit /b %errorlevel%
