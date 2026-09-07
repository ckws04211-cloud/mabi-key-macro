@echo off
echo ==============================================
echo Building Mabinogi Macro (GUI Mode)...
echo Flag '-ldflags="-H windowsgui"' is applied
echo to prevent the CMD window from showing up.
echo ==============================================

set OUTPUT_NAME=mabinogi_macro_v11.exe
if not "%~1"=="" set OUTPUT_NAME=%~1

go build -ldflags="-H windowsgui" -o %OUTPUT_NAME%

if %errorlevel% neq 0 (
    echo [ERROR] Build failed!
    pause
    exit /b %errorlevel%
)

echo [SUCCESS] Build completed: %OUTPUT_NAME%
pause
