@echo off
chcp 65001 >nul
echo ==============================================
echo 마비노기 키 다운 빌드 중... (GUI 모드)
echo '-ldflags="-H windowsgui"' 플래그를 적용해
echo CMD 창이 뜨지 않도록 합니다.
echo ==============================================

set OUTPUT_NAME=마비노기 키 다운 - v14.0.0.exe
if not "%~1"=="" set OUTPUT_NAME=%~1

go build -ldflags="-H windowsgui" -o "%OUTPUT_NAME%"

if %errorlevel% neq 0 (
    echo [오류] 빌드 실패!
    pause
    exit /b %errorlevel%
)

echo [성공] 빌드 완료: %OUTPUT_NAME%
pause
