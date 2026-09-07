package main

import (
	"golang.org/x/sys/windows"
	"unsafe"
)

var (
	user32             = windows.NewLazySystemDLL("user32.dll")
	procSendInput      = user32.NewProc("SendInput")
	procMapVirtualKeyW = user32.NewProc("MapVirtualKeyW")
)

const (
	INPUT_MOUSE           = 0
	INPUT_KEYBOARD        = 1
	KEYEVENTF_EXTENDEDKEY = 0x0001
	KEYEVENTF_KEYUP       = 0x0002
	KEYEVENTF_SCANCODE    = 0x0008

	MOUSEEVENTF_LEFTDOWN   = 0x0002
	MOUSEEVENTF_LEFTUP     = 0x0004
	MOUSEEVENTF_RIGHTDOWN  = 0x0008
	MOUSEEVENTF_RIGHTUP    = 0x0010
	MOUSEEVENTF_MIDDLEDOWN = 0x0020
	MOUSEEVENTF_MIDDLEUP   = 0x0040
	MOUSEEVENTF_XDOWN      = 0x0080
	MOUSEEVENTF_XUP        = 0x0100

	XBUTTON1 = 0x0001
	XBUTTON2 = 0x0002
)

type KEYBDINPUT struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type MOUSEINPUT struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

// C의 공용체(union) 구조체를 에뮬레이션하기 위해 패딩 포함
type INPUT_KBD struct {
	Type uint32
	_    uint32
	Ki   KEYBDINPUT
	_    uint64 // Padding for union size match
}

type INPUT_MOUSE_STRUCT struct {
	Type uint32
	_    uint32
	Mi   MOUSEINPUT
}

func SendKey(vkCode uint16, scanCode uint16, flags uint32, extraInfo uintptr) error {
	var input INPUT_KBD
	input.Type = INPUT_KEYBOARD
	input.Ki.WVk = vkCode
	input.Ki.WScan = scanCode
	input.Ki.DwFlags = flags
	input.Ki.DwExtraInfo = extraInfo

	ret, _, err := procSendInput.Call(
		uintptr(1),
		uintptr(unsafe.Pointer(&input)),
		uintptr(unsafe.Sizeof(input)),
	)
	if ret == 0 {
		return err
	}
	return nil
}

func SendMouse(dwFlags uint32, extraInfo uintptr) error {
	var input INPUT_MOUSE_STRUCT
	input.Type = INPUT_MOUSE
	input.Mi.DwFlags = dwFlags
	input.Mi.DwExtraInfo = extraInfo

	ret, _, err := procSendInput.Call(
		uintptr(1),
		uintptr(unsafe.Pointer(&input)),
		uintptr(unsafe.Sizeof(input)),
	)
	if ret == 0 {
		return err
	}
	return nil
}

// SendMouseX: X1/X2 버튼 입력. MouseData에 어떤 X버튼인지 지정해야 한다.
func SendMouseX(dwFlags uint32, xbutton uint32, extraInfo uintptr) error {
	var input INPUT_MOUSE_STRUCT
	input.Type = INPUT_MOUSE
	input.Mi.MouseData = xbutton
	input.Mi.DwFlags = dwFlags
	input.Mi.DwExtraInfo = extraInfo

	ret, _, err := procSendInput.Call(
		uintptr(1),
		uintptr(unsafe.Pointer(&input)),
		uintptr(unsafe.Sizeof(input)),
	)
	if ret == 0 {
		return err
	}
	return nil
}

// 가상 키 코드를 스캔 코드로 변환
func getMapVirtualKey(vk uint32) uint16 {
	ret, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0)
	return uint16(ret)
}

// isExtendedVK: E0 프리픽스가 필요한 확장 키인지 판별한다.
// 방향키/Home/End/PgUp/PgDn/Ins/Del, 넘패드 Divide, 우측 Ctrl/Alt, NumLock 등.
func isExtendedVK(vk uint32) bool {
	switch vk {
	case 0x21, 0x22, 0x23, 0x24, // PgUp, PgDn, End, Home
		0x25, 0x26, 0x27, 0x28, // ← ↑ → ↓
		0x2D, 0x2E, // Insert, Delete
		0x6F,       // 넘패드 Divide (/)
		0x90,       // NumLock
		0xA3, 0xA5: // 우측 Ctrl, 우측 Alt
		return true
	}
	return false
}
