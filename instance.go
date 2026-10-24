package main

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 중복 실행 방어.
//
// 앱이 둘 떠 있으면 훅이 두 벌 설치돼, 먼저 호출되는 쪽이 키를 삼켜 다른 쪽은 입력을
// 못 보는 등 동작이 꼬인다(자동 시작과 수동 실행이 겹치면 쉽게 생김). 이름 있는
// 뮤텍스로 하나만 실행되게 하고, 두 번째 실행은 이미 떠 있는 창을 앞으로 가져온 뒤 끝낸다.

var (
	singleInstanceMutexName = `Local\MabinogiKeyDown_SingleInstance`

	instanceMutex windows.Handle // 프로세스 수명 동안 열어 둔다(닫으면 뮤텍스가 풀림)

	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

const swRestore = 9

// ensureSingleInstance: 이 프로세스가 첫 번째 인스턴스면 true.
// 뮤텍스를 만들 수 없는 예외 상황에서는 실행을 막지 않고 true(허용)를 돌려준다.
func ensureSingleInstance() bool {
	name, err := windows.UTF16PtrFromString(singleInstanceMutexName)
	if err != nil {
		return true
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return false
	}
	if h != 0 {
		instanceMutex = h
	}
	return true
}

// isAppWindowTitle: "마비노기 키 다운 - v13.0.0" 형태의 이 앱 창 제목인지(브라우저 탭
// 제목 등 우연히 비슷한 창을 건드리지 않도록 접두어 + 버전 형식까지 확인).
func isAppWindowTitle(title string) bool {
	const prefix = "마비노기 키 다운 - v"
	if !strings.HasPrefix(title, prefix) {
		return false
	}
	rest := title[len(prefix):]
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if !(r >= '0' && r <= '9') && r != '.' {
			return false
		}
	}
	return true
}

// activateExistingInstance: 이미 실행 중인 인스턴스의 창을 복원하고 앞으로 가져온다.
func activateExistingInstance() {
	var found uintptr
	cb := windows.NewCallback(func(h uintptr, _ uintptr) uintptr {
		var buf [256]uint16
		n, _, _ := procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n > 0 && isAppWindowTitle(windows.UTF16ToString(buf[:n])) {
			found = h
			return 0 // 중단
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	if found == 0 {
		return
	}
	procShowWindow.Call(found, swRestore)
	procSetForegroundWindow.Call(found)
}
