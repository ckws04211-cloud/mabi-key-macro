package main

import (
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32           = windows.NewLazySystemDLL("shell32.dll")
	procExtractIconEx = shell32.NewProc("ExtractIconExW")

	procSendMessageW             = user32.NewProc("SendMessageW")
	procSetClassLongPtrW         = user32.NewProc("SetClassLongPtrW")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
)

const (
	wmSetIcon = 0x0080
	iconSmall = 0
	iconBig   = 1
)

// 음수 클래스 인덱스는 상수→uintptr 변환이 컴파일 오류라 런타임 변수로 둔다.
var (
	gclpHIcon   int32 = -14
	gclpHIconSm int32 = -34
)

// extractExeIcons: 현재 실행 파일에 임베드된 아이콘에서 큰/작은 핸들을 추출한다.
// (타이틀바·트레이 아이콘에 공용으로 사용)
func extractExeIcons() (large, small uintptr) {
	exePath, err := os.Executable()
	if err != nil {
		return 0, 0
	}
	pPath, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return 0, 0
	}
	var hLarge, hSmall uintptr
	procExtractIconEx.Call(
		uintptr(unsafe.Pointer(pPath)), 0,
		uintptr(unsafe.Pointer(&hLarge)),
		uintptr(unsafe.Pointer(&hSmall)),
		1,
	)
	if hLarge == 0 && hSmall == 0 {
		return 0, 0
	}
	if hSmall == 0 {
		hSmall = hLarge
	}
	if hLarge == 0 {
		hLarge = hSmall
	}
	return hLarge, hSmall
}

// findMyWindow: 이 프로세스가 소유한, 제목이 정확히 일치하는 최상위 창의 HWND를 찾는다
// (다중 인스턴스 안전). 창이 아직 안 떴을 수 있어 짧게 재시도한다.
func findMyWindow(title string) uintptr {
	myPID := windows.GetCurrentProcessId()
	var hwnd uintptr
	cb := windows.NewCallback(func(h uintptr, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadProcessId.Call(h, uintptr(unsafe.Pointer(&pid)))
		if pid != myPID {
			return 1 // 계속
		}
		var buf [256]uint16
		n, _, _ := procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n > 0 && windows.UTF16ToString(buf[:n]) == title {
			hwnd = h
			return 0 // 중단
		}
		return 1
	})

	for i := 0; i < 100; i++ { // 창 생성 대기(최대 ~5초)
		hwnd = 0
		procEnumWindows.Call(cb, 0)
		if hwnd != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return hwnd
}

// applyWindowIcon은 exe에 임베드된 아이콘을 이 프로세스의 창 타이틀바/작업표시줄에 설정한다.
// Gio는 리소스 ID 1인 아이콘만 자동 로드하므로(대부분의 리소스 툴은 다른 ID를 부여),
// 창이 뜨면 WM_SETICON + 클래스 아이콘으로 직접 적용한다.
func applyWindowIcon(title string) {
	hLarge, hSmall := extractExeIcons()
	if hLarge == 0 && hSmall == 0 {
		return
	}
	hwnd := findMyWindow(title)
	if hwnd == 0 {
		return
	}

	procSendMessageW.Call(hwnd, wmSetIcon, iconSmall, hSmall)
	procSendMessageW.Call(hwnd, wmSetIcon, iconBig, hLarge)
	procSetClassLongPtrW.Call(hwnd, uintptr(gclpHIcon), hLarge)
	procSetClassLongPtrW.Call(hwnd, uintptr(gclpHIconSm), hSmall)
}
