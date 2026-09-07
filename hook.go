package main

import (
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procSetWindowsHookEx    = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procGetKeyNameTextW     = user32.NewProc("GetKeyNameTextW")
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")

	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")

	winmm               = windows.NewLazySystemDLL("winmm.dll")
	procTimeBeginPeriod = winmm.NewProc("timeBeginPeriod")
	procTimeEndPeriod   = winmm.NewProc("timeEndPeriod")
)

const WM_QUIT = 0x0012

// Win32 MSG 구조체
type winMSG struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

const (
	WH_KEYBOARD_LL = 13
	WH_MOUSE_LL    = 14
	WM_KEYDOWN     = 0x0100
	WM_KEYUP       = 0x0101
	WM_SYSKEYDOWN  = 0x0104
	WM_SYSKEYUP    = 0x0105

	WM_LBUTTONDOWN = 0x0201
	WM_LBUTTONUP   = 0x0202
	WM_RBUTTONDOWN = 0x0204
	WM_RBUTTONUP   = 0x0205
	WM_MBUTTONDOWN = 0x0207
	WM_MBUTTONUP   = 0x0208
	WM_XBUTTONDOWN = 0x020B
	WM_XBUTTONUP   = 0x020C

	LLKHF_INJECTED = 0x00000010

	VK_CONTROL = 0x11
)

var MyInjectionFlag uintptr = 0xDEADBEEF

type KBDLLHOOKSTRUCT struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type MSLLHOOKSTRUCT struct {
	Pt          struct{ X, Y int32 }
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

var keyboardHookHandle uintptr
var mouseHookHandle uintptr
var hookThreadID uint32

var (
	stateMu    sync.Mutex
	keyStates  map[uint32]*WorkerContext
	workerDone chan struct{}

	BindingMode   bool
	BindingChan   chan BindingResult
	BindingCancel chan struct{}

	autoClickerMu      sync.Mutex
	autoClickerRunning bool
	autoClickerStopCh  chan struct{}
	autoClickerKeyDown bool

	AutoClickerStateCallback func(bool)

	toggleHotkeyDown bool
	// ToggleHotkeyCallback: Ctrl+단축키 조합이 눌렸을 때 호출된다(매크로 on/off 전환용).
	ToggleHotkeyCallback func()
)

type BindingResult struct {
	VK   uint32
	Name string
}

type WorkerContext struct {
	TargetVK  uint32
	DelayMs   int
	DownMs    int
	StopCh    chan struct{}
	IsRunning bool
}

// StartHookSystem은 LL 훅을 전용 OS 스레드에 설치하고, 그 스레드에서 Win32
// 메시지 루프를 돌려 콜백이 안정적으로 디스패치되도록 한다. GUI 툴킷(Gio)의
// 이벤트 루프와 완전히 분리되어, 어떤 GUI를 쓰든 훅 동작이 보장된다.
func StartHookSystem() error {
	workerDone = make(chan struct{})
	keyStates = make(map[uint32]*WorkerContext)
	BindingChan = make(chan BindingResult)

	ready := make(chan error, 1)
	go hookThreadLoop(ready)
	return <-ready
}

func hookThreadLoop(ready chan error) {
	// LL 훅 콜백은 훅을 설치한 스레드의 메시지 큐로만 전달되므로 이 스레드에 고정한다.
	runtime.LockOSThread()
	procTimeBeginPeriod.Call(1) // 타이머 해상도 1ms

	tid, _, _ := procGetCurrentThreadId.Call()
	hookThreadID = uint32(tid)

	kh, _, errK := procSetWindowsHookEx.Call(
		uintptr(WH_KEYBOARD_LL),
		windows.NewCallback(keyboardHookCallback),
		0,
		0,
	)
	if kh == 0 {
		procTimeEndPeriod.Call(1)
		ready <- fmt.Errorf("failed to install keyboard hook: %v", errK)
		return
	}
	keyboardHookHandle = kh

	mh, _, errM := procSetWindowsHookEx.Call(
		uintptr(WH_MOUSE_LL),
		windows.NewCallback(mouseHookCallback),
		0,
		0,
	)
	if mh == 0 {
		procUnhookWindowsHookEx.Call(keyboardHookHandle)
		keyboardHookHandle = 0
		procTimeEndPeriod.Call(1)
		ready <- fmt.Errorf("failed to install mouse hook: %v", errM)
		return
	}
	mouseHookHandle = mh

	ready <- nil // 설치 성공을 메인에 알림

	// 메시지 루프: GetMessage가 큐를 펌프하면서 훅 콜백을 디스패치한다.
	// StopHookSystem이 WM_QUIT를 보내면 GetMessage가 0을 반환하며 루프를 종료한다.
	var msg winMSG
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = error
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	if keyboardHookHandle != 0 {
		procUnhookWindowsHookEx.Call(keyboardHookHandle)
		keyboardHookHandle = 0
	}
	if mouseHookHandle != 0 {
		procUnhookWindowsHookEx.Call(mouseHookHandle)
		mouseHookHandle = 0
	}
	procTimeEndPeriod.Call(1) // 타이머 해상도 복원
}

func StopHookSystem() {
	StopAllWorkers()

	autoClickerMu.Lock()
	if autoClickerRunning {
		close(autoClickerStopCh)
		autoClickerRunning = false
	}
	autoClickerMu.Unlock()

	close(workerDone)

	// 훅 스레드의 메시지 루프를 깨워 종료시킨다.
	if hookThreadID != 0 {
		procPostThreadMessageW.Call(uintptr(hookThreadID), WM_QUIT, 0, 0)
		hookThreadID = 0
	}
}

// StopAllWorkers: 실행 중인 모든 연타 워커를 멈추고 상태 맵을 비운다.
// 매크로를 비활성화할 때 호출한다(예전에는 훅 콜백이 매 입력마다 처리했음).
func StopAllWorkers() {
	stateMu.Lock()
	for vk, ctx := range keyStates {
		if ctx.IsRunning {
			close(ctx.StopCh)
			ctx.IsRunning = false
		}
		delete(keyStates, vk)
	}
	stateMu.Unlock()
}

func StartBinding() (<-chan BindingResult, <-chan struct{}) {
	stateMu.Lock()
	defer stateMu.Unlock()

	if BindingMode && BindingCancel != nil {
		close(BindingCancel)
	}

	BindingMode = true
	BindingCancel = make(chan struct{})
	return BindingChan, BindingCancel
}

func TryBindKey(vk uint32, name string) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	if BindingMode {
		BindingMode = false
		go func() {
			select {
			case BindingChan <- BindingResult{VK: vk, Name: name}:
			case <-BindingCancel:
			}
		}()
		return true
	}
	return false
}

// isCtrlHeld: 좌/우 Ctrl 중 하나라도 현재 눌려있는지 확인한다.
func isCtrlHeld() bool {
	ret, _, _ := procGetAsyncKeyState.Call(uintptr(VK_CONTROL))
	return ret&0x8000 != 0
}

func GetKeyName(vkCode uint32, scanCode uint32) string {
	buf := make([]uint16, 256)
	lParam := (scanCode << 16)
	if vkCode == 0x21 || vkCode == 0x22 || vkCode == 0x23 || vkCode == 0x24 ||
		vkCode == 0x25 || vkCode == 0x26 || vkCode == 0x27 || vkCode == 0x28 ||
		vkCode == 0x2D || vkCode == 0x2E || vkCode == 0x2F || vkCode == 0x6F { // Extended keys like arrows/pageup/etc
		lParam |= (1 << 24)
	}
	ret, _, _ := procGetKeyNameTextW.Call(uintptr(lParam), uintptr(unsafe.Pointer(&buf[0])), 256)
	if ret > 0 {
		return windows.UTF16ToString(buf[:ret])
	}
	return fmt.Sprintf("Key 0x%X", vkCode)
}

func keyboardHookCallback(nCode int, wParam uintptr, lParam uintptr) uintptr {
	if nCode >= 0 {
		kbd := (*KBDLLHOOKSTRUCT)(unsafe.Pointer(lParam))
		if kbd.DwExtraInfo == MyInjectionFlag || (kbd.Flags&LLKHF_INJECTED) != 0 {
			return callNextHookEx(keyboardHookHandle, nCode, wParam, lParam)
		}

		// 포커스 가드: 켜져 있으면 마비노기가 전경 창일 때만 트리거를 반응시킨다.
		// (게임이 아닌 다른 창, 예: 디스코드에서는 설정한 키가 평소처럼 그대로 입력된다)
		gameOk := !AppConfig.GetFocusGuard() || IsGameFocused()

		if wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN {
			stateMu.Lock()
			isBinding := BindingMode
			stateMu.Unlock()

			if isBinding {
				if TryBindKey(kbd.VkCode, GetKeyName(kbd.VkCode, kbd.ScanCode)) {
					return 1
				}
			} else {
				// 매크로 on/off 단축키(기본 Ctrl+F12): 포커스 가드와 무관하게 항상 반응한다
				// (다른 프로그램에 있어도 켜고 끌 수 있어야 실용적이다).
				toggleVK, _ := AppConfig.GetToggleHotkey()
				if toggleVK != 0 && kbd.VkCode == toggleVK && isCtrlHeld() {
					if !toggleHotkeyDown {
						toggleHotkeyDown = true
						if ToggleHotkeyCallback != nil {
							go ToggleHotkeyCallback()
						}
					}
					return 1
				}
				if gameOk {
					acConfig := AppConfig.GetAutoClicker()
					if acConfig.TriggerVK != 0 && kbd.VkCode == acConfig.TriggerVK {
						if !autoClickerKeyDown {
							autoClickerKeyDown = true
							toggleAutoClicker()
						}
						return 1
					}
				}
			}
		} else if wParam == WM_KEYUP || wParam == WM_SYSKEYUP {
			stateMu.Lock()
			isBinding := BindingMode
			stateMu.Unlock()
			if isBinding {
				return 1
			} else {
				toggleVK, _ := AppConfig.GetToggleHotkey()
				if toggleVK != 0 && kbd.VkCode == toggleVK && toggleHotkeyDown {
					toggleHotkeyDown = false
					return 1
				}
				acConfig := AppConfig.GetAutoClicker()
				// keydown 때 실제로 억제(suppress)했을 때만 keyup도 억제한다.
				// (autoClickerKeyDown이 이미 눌림 상태를 의미하므로 focus 변화로 인한
				//  "억제 안 된 채 계속 눌림"으로 꼬이는 상황을 만들지 않는다)
				if acConfig.TriggerVK != 0 && kbd.VkCode == acConfig.TriggerVK && autoClickerKeyDown {
					autoClickerKeyDown = false
					return 1
				}
			}
		}

		// 매크로 비활성 상태면 여기서 즉시 통과시켜 시스템 전체 입력 오버헤드를 없앤다.
		// (실행 중이던 워커는 비활성화 시 StopAllWorkers로 이미 정리된다)
		if !AppConfig.GetActive() {
			return callNextHookEx(keyboardHookHandle, nCode, wParam, lParam)
		}

		var slot MacroSlot
		var exists bool

		ActiveTriggersMu.RLock()
		if ActiveTriggers != nil {
			slot, exists = ActiveTriggers[kbd.VkCode]
		}
		ActiveTriggersMu.RUnlock()

		if exists {
			if wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN {
				if gameOk {
					stateMu.Lock()
					if ctx, ok := keyStates[kbd.VkCode]; !ok || !ctx.IsRunning {
						stopCh := make(chan struct{})
						ctx = &WorkerContext{
							TargetVK:  slot.TargetVK,
							DelayMs:   slot.DelayMs,
							DownMs:    slot.DownMs,
							StopCh:    stopCh,
							IsRunning: true,
						}
						keyStates[kbd.VkCode] = ctx
						go runMacroWorker(ctx)
					}
					stateMu.Unlock()
					return 1
				}
				// 게임 밖: 실제 키를 그대로 통과시킨다(디스코드 등에서 정상 타이핑).
			} else if wParam == WM_KEYUP || wParam == WM_SYSKEYUP {
				// 다운 시 워커를 띄워 억제했던 경우에만(=keyStates에 존재) 업도 억제한다.
				// 이 체크는 gameOk와 무관하게 항상 수행 — 눌린 채로 창을 전환해도
				// 워커가 멈추지 않고 계속 입력을 스팸하는 상황을 방지한다.
				stateMu.Lock()
				ctx, ok := keyStates[kbd.VkCode]
				if ok {
					if ctx.IsRunning {
						close(ctx.StopCh)
						ctx.IsRunning = false
					}
					delete(keyStates, kbd.VkCode)
				}
				stateMu.Unlock()
				if ok {
					return 1
				}
			}
		}
	}
	return callNextHookEx(keyboardHookHandle, nCode, wParam, lParam)
}

func mouseHookCallback(nCode int, wParam uintptr, lParam uintptr) uintptr {
	if nCode >= 0 {
		// 마우스 이동(WM_MOUSEMOVE: 0x0200)은 가장 빈번하므로 최우선으로 패스하여 리소스 낭비 차단
		if wParam == 0x0200 {
			return callNextHookEx(mouseHookHandle, nCode, wParam, lParam)
		}

		ms := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lParam))
		if ms.DwExtraInfo == MyInjectionFlag || (ms.Flags&1) != 0 { // LLMHF_INJECTED
			return callNextHookEx(mouseHookHandle, nCode, wParam, lParam)
		}

		var vk uint32
		var name string
		isDown := false
		isUp := false

		switch wParam {
		case WM_LBUTTONDOWN:
			vk = 0x01
			name = "좌클릭"
			isDown = true
		case WM_LBUTTONUP:
			vk = 0x01
			isUp = true
		case WM_RBUTTONDOWN:
			vk = 0x02
			name = "우클릭"
			isDown = true
		case WM_RBUTTONUP:
			vk = 0x02
			isUp = true
		case WM_MBUTTONDOWN:
			vk = 0x04
			name = "휠클릭"
			isDown = true
		case WM_MBUTTONUP:
			vk = 0x04
			isUp = true
		case WM_XBUTTONDOWN:
			xbtn := (ms.MouseData >> 16) & 0xFFFF
			if xbtn == 1 {
				vk = 0x05
				name = "Mouse X1"
			} else {
				vk = 0x06
				name = "Mouse X2"
			}
			isDown = true
		case WM_XBUTTONUP:
			xbtn := (ms.MouseData >> 16) & 0xFFFF
			if xbtn == 1 {
				vk = 0x05
			} else {
				vk = 0x06
			}
			isUp = true
		}

		if vk != 0 {
			gameOk := !AppConfig.GetFocusGuard() || IsGameFocused()

			if isDown {
				stateMu.Lock()
				isBinding := BindingMode
				stateMu.Unlock()
				if isBinding {
					if TryBindKey(vk, name) {
						return 1
					}
				} else if gameOk {
					acConfig := AppConfig.GetAutoClicker()
					if acConfig.TriggerVK != 0 && vk == acConfig.TriggerVK {
						if !autoClickerKeyDown {
							autoClickerKeyDown = true
							toggleAutoClicker()
						}
						return 1
					}
				}
			} else if isUp {
				stateMu.Lock()
				isBinding := BindingMode
				stateMu.Unlock()
				if isBinding {
					// We might return 1 here if we want to block up events during binding,
					// but it wasn't blocked before.
				} else {
					acConfig := AppConfig.GetAutoClicker()
					if acConfig.TriggerVK != 0 && vk == acConfig.TriggerVK && autoClickerKeyDown {
						autoClickerKeyDown = false
						return 1
					}
				}
			}

			if AppConfig.GetActive() {
				var slot MacroSlot
				var exists bool

				ActiveTriggersMu.RLock()
				if ActiveTriggers != nil {
					slot, exists = ActiveTriggers[vk]
				}
				ActiveTriggersMu.RUnlock()

				if exists {
					if isDown {
						if gameOk {
							stateMu.Lock()
							if ctx, ok := keyStates[vk]; !ok || !ctx.IsRunning {
								stopCh := make(chan struct{})
								ctx = &WorkerContext{
									TargetVK:  slot.TargetVK,
									DelayMs:   slot.DelayMs,
									DownMs:    slot.DownMs,
									StopCh:    stopCh,
									IsRunning: true,
								}
								keyStates[vk] = ctx
								go runMacroWorker(ctx)
							}
							stateMu.Unlock()
							return 1
						}
						// 게임 밖: 실제 클릭을 그대로 통과시킨다.
					} else if isUp {
						stateMu.Lock()
						ctx, ok := keyStates[vk]
						if ok {
							if ctx.IsRunning {
								close(ctx.StopCh)
								ctx.IsRunning = false
							}
							delete(keyStates, vk)
						}
						stateMu.Unlock()
						if ok {
							return 1
						}
					}
				}
			}
		}
	}
	return callNextHookEx(mouseHookHandle, nCode, wParam, lParam)
}

func callNextHookEx(hhk uintptr, nCode int, wParam uintptr, lParam uintptr) uintptr {
	ret, _, _ := procCallNextHookEx.Call(hhk, uintptr(nCode), wParam, lParam)
	return ret
}

// makeEmitters: 대상 VK에 맞는 down/up 입력 방출 함수를 만든다.
// 마우스 좌(0x01)/우(0x02)/휠(0x04)/X1(0x05)/X2(0x06)과 그 외 키보드 키를 모두 처리한다.
func makeEmitters(targetVK uint32) (emitDown, emitUp func()) {
	switch targetVK {
	case 0x01:
		return func() { SendMouse(MOUSEEVENTF_LEFTDOWN, MyInjectionFlag) },
			func() { SendMouse(MOUSEEVENTF_LEFTUP, MyInjectionFlag) }
	case 0x02:
		return func() { SendMouse(MOUSEEVENTF_RIGHTDOWN, MyInjectionFlag) },
			func() { SendMouse(MOUSEEVENTF_RIGHTUP, MyInjectionFlag) }
	case 0x04:
		return func() { SendMouse(MOUSEEVENTF_MIDDLEDOWN, MyInjectionFlag) },
			func() { SendMouse(MOUSEEVENTF_MIDDLEUP, MyInjectionFlag) }
	case 0x05, 0x06:
		xbtn := uint32(XBUTTON1)
		if targetVK == 0x06 {
			xbtn = XBUTTON2
		}
		return func() { SendMouseX(MOUSEEVENTF_XDOWN, xbtn, MyInjectionFlag) },
			func() { SendMouseX(MOUSEEVENTF_XUP, xbtn, MyInjectionFlag) }
	default:
		scan := getMapVirtualKey(targetVK)
		var ext uint32
		if isExtendedVK(targetVK) {
			ext = KEYEVENTF_EXTENDEDKEY
		}
		dflags := uint32(KEYEVENTF_SCANCODE) | ext
		uflags := uint32(KEYEVENTF_KEYUP|KEYEVENTF_SCANCODE) | ext
		vk := uint16(targetVK)
		return func() { SendKey(vk, scan, dflags, MyInjectionFlag) },
			func() { SendKey(vk, scan, uflags, MyInjectionFlag) }
	}
}

func runMacroWorker(ctx *WorkerContext) {
	// 키 유지 시간(down)과 연타 주기(delay)는 사용자가 각각 설정한 값을 사용한다.
	downDurationMs, upDurationMs := splitDurations(ctx.DelayMs, ctx.DownMs)

	downDuration := time.Duration(downDurationMs) * time.Millisecond
	upDuration := time.Duration(upDurationMs) * time.Millisecond

	// 대상 종류(키보드 / 마우스 좌·우·휠 / X1·X2)에 맞는 down/up 방출 함수를 준비.
	emitDown, emitUp := makeEmitters(ctx.TargetVK)

	// GC(가비지 컬렉터) 부하를 없애기 위해 재사용 가능한 타이머 생성
	sleepTimer := time.NewTimer(0)
	if !sleepTimer.Stop() {
		<-sleepTimer.C
	}

	for {
		// 우선순위 1: Stop 신호가 이미 들어왔는지 체크 (select의 랜덤성 방지)
		select {
		case <-workerDone:
			return
		case <-ctx.StopCh:
			return
		default:
		}

		// 입력 (DOWN)
		emitDown()

		// 프레임 인식을 위한 DOWN 대기
		sleepTimer.Reset(downDuration)
		select {
		case <-sleepTimer.C:
		case <-ctx.StopCh:
			if !sleepTimer.Stop() {
				<-sleepTimer.C
			}
			emitUp()
			return
		case <-workerDone:
			if !sleepTimer.Stop() {
				<-sleepTimer.C
			}
			emitUp()
			return
		}

		// 떼기 (UP)
		emitUp()

		// 다음 입력을 위한 UP 대기
		sleepTimer.Reset(upDuration)
		select {
		case <-sleepTimer.C:
		case <-ctx.StopCh:
			if !sleepTimer.Stop() {
				<-sleepTimer.C
			}
			return
		case <-workerDone:
			if !sleepTimer.Stop() {
				<-sleepTimer.C
			}
			return
		}
	}
}

func toggleAutoClicker() {
	autoClickerMu.Lock()
	defer autoClickerMu.Unlock()

	if autoClickerRunning {
		close(autoClickerStopCh)
		autoClickerRunning = false
		if AutoClickerStateCallback != nil {
			go AutoClickerStateCallback(false)
		}
	} else {
		autoClickerStopCh = make(chan struct{})
		autoClickerRunning = true
		if AutoClickerStateCallback != nil {
			go AutoClickerStateCallback(true)
		}

		ac := AppConfig.GetAutoClicker()
		go runAutoClickerWorker(autoClickerStopCh, ac.DelayMs, ac.DownMs)
	}
}

// splitDurations: 전체 주기(delayMs)와 유지 시간(downMs)을 down/up 대기시간으로 나눈다.
// 각 값은 최소 1ms 이상이 되도록 보정한다.
func splitDurations(delayMs int, downMs int) (int, int) {
	if downMs < 1 {
		downMs = 1
	}
	if downMs >= delayMs {
		downMs = delayMs - 1
	}
	if downMs < 1 {
		downMs = 1
	}
	up := delayMs - downMs
	if up < 1 {
		up = 1
	}
	return downMs, up
}

func runAutoClickerWorker(stopCh chan struct{}, delayMs int, downMs int) {
	// 1초 딜레이
	timer := time.NewTimer(1 * time.Second)
	select {
	case <-timer.C:
	case <-stopCh:
		timer.Stop()
		return
	case <-workerDone:
		timer.Stop()
		return
	}

	downDurationMs, upDurationMs := splitDurations(delayMs, downMs)

	downDuration := time.Duration(downDurationMs) * time.Millisecond
	upDuration := time.Duration(upDurationMs) * time.Millisecond

	sleepTimer := time.NewTimer(0)
	if !sleepTimer.Stop() {
		<-sleepTimer.C
	}

	for {
		select {
		case <-workerDone:
			return
		case <-stopCh:
			return
		default:
		}

		SendMouse(MOUSEEVENTF_LEFTDOWN, MyInjectionFlag)

		sleepTimer.Reset(downDuration)
		select {
		case <-sleepTimer.C:
		case <-stopCh:
			if !sleepTimer.Stop() {
				<-sleepTimer.C
			}
			SendMouse(MOUSEEVENTF_LEFTUP, MyInjectionFlag)
			return
		case <-workerDone:
			if !sleepTimer.Stop() {
				<-sleepTimer.C
			}
			SendMouse(MOUSEEVENTF_LEFTUP, MyInjectionFlag)
			return
		}

		SendMouse(MOUSEEVENTF_LEFTUP, MyInjectionFlag)

		sleepTimer.Reset(upDuration)
		select {
		case <-sleepTimer.C:
		case <-stopCh:
			if !sleepTimer.Stop() {
				<-sleepTimer.C
			}
			return
		case <-workerDone:
			if !sleepTimer.Stop() {
				<-sleepTimer.C
			}
			return
		}
	}
}
