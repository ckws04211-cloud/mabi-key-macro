package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
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

// ---------- 채팅 중 감지 (Enter 입력으로 추정) ----------
//
// 게임이 채팅창을 자체 UI로 그려서 OS에서 입력창 포커스를 직접 알 수 없으므로,
// 실제 키 입력만 보고 상태를 추정한다. 마비노기 채팅 규칙:
//   - 닫힌 상태에서 Enter → 열림
//   - 열린 상태에서 내용 없이 Enter → 닫힘
//   - 열린 상태에서 내용을 입력하고 Enter → 전송되지만 입력창은 열린 채 유지
//   - 열린 상태에서 Esc → 닫힘
//
// 그래서 열린 뒤 입력된 글자 수를 세어 "내용이 있는지"를 판단한다. 추정이 어긋날 수
// 있으므로(NPC 대화 Enter 등) Esc, 게임 포커스 이탈, 매크로 켜고 끄기, UI의 초기화
// 버튼이 모두 상태를 "닫힘"으로 되돌린다.

const (
	VK_RETURN = 0x0D
	VK_ESCAPE = 0x1B
	VK_BACK   = 0x08
)

var (
	chatOpen      atomic.Bool
	chatChars     atomic.Int32 // 열린 뒤(또는 마지막 전송 뒤) 입력된 글자 수 추정치
	chatEnterHeld atomic.Bool  // Enter 오토리핏 keydown을 한 번으로만 세기 위함

	// ChatStateCallback: 채팅 감지 상태가 바뀔 때 호출된다(UI 갱신용).
	ChatStateCallback func(open bool)
)

// ---------- 입력 데스크톱 전환 감시 (키 업 유실 방지) ----------
//
// UAC 승인창·Ctrl+Alt+Del·화면 잠금은 "보안 데스크톱"에서 열려서, 그동안 우리 훅은
// 키 입력을 전혀 받지 못한다. 트리거를 누른 채 이런 화면으로 넘어가면 키 업이 유실돼
// 연타 워커가 계속 입력을 보낸다. 입력 데스크톱을 열 수 없게 되면(=전환됨) 돌고 있는
// 워커·채팅 추정·수정키 상태를 한꺼번에 정리한다.
// (GetAsyncKeyState로 "아직 눌려 있나"를 확인하는 방식은 쓰지 않는다 — 훅이 삼킨(억제한)
//  트리거 키는 시스템 키 상태에 눌림으로 기록되지 않아 정상 상황에서도 "뗐다"고 나오기 때문)

var (
	procOpenInputDesktop = user32.NewProc("OpenInputDesktop")
	procCloseDesktop     = user32.NewProc("CloseDesktop")

	inputDesktopLost atomic.Bool
)

const desktopSwitchDesktop = 0x0100 // DESKTOP_SWITCHDESKTOP

// inputDesktopAccessible: 지금 입력을 받는 데스크톱을 열 수 있는지(=일반 데스크톱인지).
func inputDesktopAccessible() bool {
	h, _, _ := procOpenInputDesktop.Call(0, 0, desktopSwitchDesktop)
	if h == 0 {
		return false
	}
	procCloseDesktop.Call(h)
	return true
}

// watchInputDesktop: 주기적으로(포커스 감시 루프에서) 호출한다. 일반 데스크톱에서
// 보안 데스크톱으로 넘어가는 순간 한 번만 정리한다.
func watchInputDesktop() {
	if inputDesktopAccessible() {
		inputDesktopLost.Store(false)
		return
	}
	if !inputDesktopLost.Swap(true) {
		StopAllWorkers()
		ResetChatState()
		physKeys.Store(0) // 그동안의 키 업을 못 봤으니 수정키 추적도 초기화
	}
}

// IsChatOpen: 지금 채팅 중으로 추정되는지.
func IsChatOpen() bool { return chatOpen.Load() }

func setChatOpen(open bool) {
	chatChars.Store(0)
	if chatOpen.Swap(open) == open {
		return
	}
	if open {
		// 채팅을 열 때 이미 돌고 있던 연타는 멈춘다(글자가 채팅창에 섞여 들어가지 않게).
		StopAllWorkers()
	}
	if ChatStateCallback != nil {
		go ChatStateCallback(open)
	}
}

// ResetChatState: 추정 상태를 "닫힘"으로 강제 초기화한다.
func ResetChatState() {
	chatEnterHeld.Store(false)
	setChatOpen(false)
}

// isTextVK: 채팅창에 글자를 입력하는 키인지(숫자/영문/스페이스/기호/넘패드/IME 처리 중 키).
func isTextVK(vk uint32) bool {
	switch {
	case vk >= 0x30 && vk <= 0x39, // 0-9
		vk >= 0x41 && vk <= 0x5A, // A-Z
		vk == 0x20,               // Space
		vk >= 0x60 && vk <= 0x6F, // 넘패드 숫자/연산자
		vk >= 0xBA && vk <= 0xC0, // ; = , - . / `
		vk >= 0xDB && vk <= 0xDF, // [ \ ] ' 등
		vk == 0xE5:               // VK_PROCESSKEY (한글 IME 조합 중)
		return true
	}
	return false
}

// chatObserveKeyDown: 실제(물리) 키 눌림을 보고 채팅 상태를 갱신한다.
func chatObserveKeyDown(vk uint32) {
	if vk == VK_RETURN {
		if chatEnterHeld.Swap(true) {
			return // 누르고 있는 동안의 오토리핏은 무시
		}
	}
	if !AppConfig.GetChatGuard() || !AppConfig.GetActive() || !IsGameFocused() {
		return
	}
	stateMu.Lock()
	binding := BindingMode
	stateMu.Unlock()
	if binding {
		return
	}

	open := chatOpen.Load()
	switch vk {
	case VK_RETURN:
		switch {
		case !open:
			setChatOpen(true)
		case chatChars.Load() > 0:
			chatChars.Store(0) // 전송됨 — 입력창은 열린 채 유지
		default:
			setChatOpen(false) // 내용 없이 Enter → 닫힘
		}
	case VK_ESCAPE:
		if open {
			setChatOpen(false)
		}
	case VK_BACK:
		if open {
			if n := chatChars.Load(); n > 0 {
				chatChars.Store(n - 1)
			}
		}
	default:
		if !open {
			return
		}
		held := modsOf(physKeys.Load())
		if held&(ModCtrl|ModAlt) != 0 && !(held&ModCtrl != 0 && vk == 'V') {
			return // 단축키 조합은 글자 입력이 아니다(Ctrl+V 붙여넣기는 내용으로 친다)
		}
		if isTextVK(vk) || (held&ModCtrl != 0 && vk == 'V') {
			chatChars.Add(1)
		}
	}
}

// ---------- 조합키(Shift/Ctrl/Alt) 지원 ----------

// 물리 수정키 비트(좌/우 구분). 훅이 본 "실제" 입력만 추적한다(주입 입력은 제외).
// 순서는 physBitVKs와 같다.
const (
	pkLShift uint32 = 1 << iota
	pkRShift
	pkLCtrl
	pkRCtrl
	pkLAlt
	pkRAlt
)

var physBitVKs = [6]uint32{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5}

// 트리거에 저장되는 조합키 마스크.
const (
	ModShift uint32 = 1 << iota
	ModCtrl
	ModAlt
)

var (
	physKeys      atomic.Uint32 // 지금 실제로 눌려 있는 수정키(pk* 비트)
	bindPendingVK atomic.Uint32 // 바인딩 중 눌린 채인 수정키 — 단독 수정키 바인딩 판정용

	// 조합 트리거 발동 중에는 물리 수정키를 게임에 안 보이게 "뗀 것처럼" 만든다
	// (Shift+R → '-'가 Shift+'-'로 들어가지 않도록). 워커가 여럿일 수 있어 참조 카운트.
	consumeMu    sync.Mutex
	consumeRef   [6]int
	consumedKeys atomic.Uint32
)

func physBitFor(vk uint32) uint32 {
	switch vk {
	case 0xA0, 0x10:
		return pkLShift
	case 0xA1:
		return pkRShift
	case 0xA2, 0x11:
		return pkLCtrl
	case 0xA3:
		return pkRCtrl
	case 0xA4, 0x12:
		return pkLAlt
	case 0xA5:
		return pkRAlt
	}
	return 0
}

// modClassOf: 수정키면 해당 조합키 마스크(ModShift 등), 아니면 0.
func modClassOf(vk uint32) uint32 {
	switch physBitFor(vk) {
	case pkLShift, pkRShift:
		return ModShift
	case pkLCtrl, pkRCtrl:
		return ModCtrl
	case pkLAlt, pkRAlt:
		return ModAlt
	}
	return 0
}

func modsOf(bits uint32) uint32 {
	var m uint32
	if bits&(pkLShift|pkRShift) != 0 {
		m |= ModShift
	}
	if bits&(pkLCtrl|pkRCtrl) != 0 {
		m |= ModCtrl
	}
	if bits&(pkLAlt|pkRAlt) != 0 {
		m |= ModAlt
	}
	return m
}

// heldModsFor: vk 자신은 제외하고 지금 함께 눌려 있는 조합키 마스크.
func heldModsFor(vk uint32) uint32 {
	return modsOf(physKeys.Load()) &^ modClassOf(vk)
}

// ModPrefix: 표시용 접두어. 예) Ctrl+Shift+
func ModPrefix(mods uint32) string {
	s := ""
	if mods&ModCtrl != 0 {
		s += "Ctrl+"
	}
	if mods&ModAlt != 0 {
		s += "Alt+"
	}
	if mods&ModShift != 0 {
		s += "Shift+"
	}
	return s
}

// lookupTrigger: (키+조합키) 정확히 일치하는 트리거를 먼저 찾고, 없으면 단일 키 트리거로
// 폴백한다. 그래서 R과 Shift+R을 둘 다 등록하면 서로 안 겹치고, R만 등록한 기존 설정은
// Shift를 누르고 있어도 예전처럼 동작한다. consume은 발동 중 게임에서 숨길 조합키.
func lookupTrigger(vk, held uint32) (slot MacroSlot, consume uint32, ok bool) {
	ActiveTriggersMu.RLock()
	defer ActiveTriggersMu.RUnlock()
	if ActiveTriggers == nil {
		return
	}
	if held != 0 {
		if s, found := ActiveTriggers[trigKey{vk, held}]; found {
			return s, held, true
		}
	}
	if s, found := ActiveTriggers[trigKey{vk, 0}]; found {
		return s, 0, true
	}
	return
}

// tryStartMacro: 이 입력이 매크로 트리거면 워커를 띄우고(이미 돌고 있으면 유지) true.
// true면 호출한 훅은 실제 입력을 억제해야 한다.
func tryStartMacro(vk, held uint32) bool {
	stateMu.Lock()
	if ctx, ok := keyStates[vk]; ok && ctx.IsRunning {
		// 오토리핏 등 — 도중에 수정키가 바뀌어도 눌린 키 기준으로 계속 억제한다.
		stateMu.Unlock()
		return true
	}
	stateMu.Unlock()

	slot, consume, exists := lookupTrigger(vk, held)
	if !exists {
		return false
	}

	stateMu.Lock()
	defer stateMu.Unlock()
	if ctx, ok := keyStates[vk]; ok && ctx.IsRunning {
		return true
	}
	ctx := &WorkerContext{
		TargetVK:    slot.TargetVK,
		DelayMs:     slot.DelayMs,
		DownMs:      slot.DownMs,
		StopCh:      make(chan struct{}),
		IsRunning:   true,
		ConsumeMods: consume,
	}
	keyStates[vk] = ctx
	safeGo("연타 워커", func() { runMacroWorker(ctx) })
	return true
}

// stopMacroKey: 이 키로 워커를 띄워 억제했던 경우에만 멈추고 true(=업도 억제).
// 게임 포커스와 무관하게 항상 수행 — 눌린 채로 창을 전환해도 스팸이 이어지지 않게 한다.
func stopMacroKey(vk uint32) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	ctx, ok := keyStates[vk]
	if !ok {
		return false
	}
	if ctx.IsRunning {
		close(ctx.StopCh)
		ctx.IsRunning = false
	}
	delete(keyStates, vk)
	return true
}

// releasePhysMods: mods에 해당하는 물리 수정키 중 눌려 있는 것을 게임엔 뗀 것으로 만든다.
func releasePhysMods(mods uint32) (released uint32) {
	if mods == 0 {
		return 0
	}
	consumeMu.Lock()
	defer consumeMu.Unlock()
	held := physKeys.Load()
	for i, vk := range physBitVKs {
		pb := uint32(1) << i
		if held&pb == 0 || modClassOf(vk)&mods == 0 {
			continue
		}
		if consumeRef[i] == 0 {
			consumedKeys.Or(pb)
			_, up := makeEmitters(vk)
			up()
		}
		consumeRef[i]++
		released |= pb
	}
	return released
}

// restorePhysMods: releasePhysMods로 뗐던 수정키를, 아직 물리적으로 눌려 있으면 다시 누른다.
func restorePhysMods(released uint32) {
	if released == 0 {
		return
	}
	consumeMu.Lock()
	defer consumeMu.Unlock()
	for i, vk := range physBitVKs {
		pb := uint32(1) << i
		if released&pb == 0 {
			continue
		}
		consumeRef[i]--
		if consumeRef[i] <= 0 {
			consumeRef[i] = 0
			consumedKeys.And(^pb)
			if physKeys.Load()&pb != 0 {
				down, _ := makeEmitters(vk)
				down()
			}
		}
	}
}

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
	Mods uint32 // 바인딩 순간 함께 눌려 있던 조합키(트리거에서만 의미 있음)
}

type WorkerContext struct {
	TargetVK    uint32
	DelayMs     int
	DownMs      int
	StopCh      chan struct{}
	IsRunning   bool
	ConsumeMods uint32 // 발동 중 게임에서 숨길 조합키(조합 트리거일 때만)
}

// StartHookSystem은 LL 훅을 전용 OS 스레드에 설치하고, 그 스레드에서 Win32
// 메시지 루프를 돌려 콜백이 안정적으로 디스패치되도록 한다. GUI 툴킷(Gio)의
// 이벤트 루프와 완전히 분리되어, 어떤 GUI를 쓰든 훅 동작이 보장된다.
func StartHookSystem() error {
	workerDone = make(chan struct{})
	keyStates = make(map[uint32]*WorkerContext)
	BindingChan = make(chan BindingResult)

	ready := make(chan error, 1)
	safeGo("훅 스레드", func() { hookThreadLoop(ready) })
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
	bindPendingVK.Store(0)
	BindingCancel = make(chan struct{})
	return BindingChan, BindingCancel
}

func TryBindKey(vk uint32, name string, mods uint32) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	if BindingMode {
		BindingMode = false
		go func() {
			select {
			case BindingChan <- BindingResult{VK: vk, Name: name, Mods: mods}:
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

// keyboardHookCallback: 훅 콜백에서 패닉이 나면 스레드(=훅)가 통째로 죽어 키보드가 먹통이
// 되거나 앱이 꺼질 수 있다. 기록만 남기고 해당 입력은 그대로 시스템에 통과시킨다.
func keyboardHookCallback(nCode int, wParam uintptr, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			logCrash("키보드 훅", r)
			ret = callNextHookEx(keyboardHookHandle, nCode, wParam, lParam)
		}
	}()
	return keyboardHookImpl(nCode, wParam, lParam)
}

func keyboardHookImpl(nCode int, wParam uintptr, lParam uintptr) uintptr {
	if nCode >= 0 {
		kbd := (*KBDLLHOOKSTRUCT)(unsafe.Pointer(lParam))
		if kbd.DwExtraInfo == MyInjectionFlag || (kbd.Flags&LLKHF_INJECTED) != 0 {
			return callNextHookEx(keyboardHookHandle, nCode, wParam, lParam)
		}

		// 실제 수정키 상태 추적(조합키 트리거 판정용). 주입 입력은 위에서 이미 걸러졌다.
		isDownMsg := wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN
		if bit := physBitFor(kbd.VkCode); bit != 0 {
			if isDownMsg {
				physKeys.Or(bit)
				// 조합 트리거 발동 중 숨겨 둔 수정키는 오토리핏 keydown도 게임에 안 보낸다.
				if consumedKeys.Load()&bit != 0 {
					return 1
				}
			} else {
				physKeys.And(^bit)
			}
		}

		// 채팅 중 감지: 키를 가로채지는 않고 입력만 관찰한다.
		if isDownMsg {
			chatObserveKeyDown(kbd.VkCode)
		} else if kbd.VkCode == VK_RETURN {
			chatEnterHeld.Store(false)
		}
		chatPaused := chatOpen.Load()

		// 포커스 가드: 켜져 있으면 마비노기가 전경 창일 때만 트리거를 반응시킨다.
		// (게임이 아닌 다른 창, 예: 디스코드에서는 설정한 키가 평소처럼 그대로 입력된다)
		gameOk := !AppConfig.GetFocusGuard() || IsGameFocused()

		if wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN {
			stateMu.Lock()
			isBinding := BindingMode
			stateMu.Unlock()

			if isBinding {
				// 수정키는 바로 바인딩하지 않고 기다린다: 뒤이어 다른 키가 눌리면 "조합"이고,
				// 그냥 떼면 수정키 단독 바인딩이다(아래 keyup 처리).
				if modClassOf(kbd.VkCode) != 0 {
					bindPendingVK.Store(kbd.VkCode)
					return 1
				}
				bindPendingVK.Store(0)
				if TryBindKey(kbd.VkCode, GetKeyName(kbd.VkCode, kbd.ScanCode), heldModsFor(kbd.VkCode)) {
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
							safeGo("단축키 콜백", ToggleHotkeyCallback)
						}
					}
					return 1
				}
				if gameOk && !chatPaused {
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
				// 수정키를 눌렀다가 다른 키 없이 뗐다면 그 수정키 단독 바인딩.
				if pv := bindPendingVK.Load(); pv != 0 && pv == kbd.VkCode {
					bindPendingVK.Store(0)
					TryBindKey(kbd.VkCode, GetKeyName(kbd.VkCode, kbd.ScanCode), heldModsFor(kbd.VkCode))
				}
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

		if wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN {
			// 게임 밖에서는 실제 키를 그대로 통과시킨다(디스코드 등에서 정상 타이핑).
			// 채팅 중이면 트리거를 건드리지 않고 그대로 통과시킨다(글자가 정상 입력됨).
			if gameOk && !chatPaused && tryStartMacro(kbd.VkCode, heldModsFor(kbd.VkCode)) {
				return 1
			}
		} else if wParam == WM_KEYUP || wParam == WM_SYSKEYUP {
			// 다운 시 워커를 띄워 억제했던 경우에만 업도 억제한다(gameOk와 무관).
			if stopMacroKey(kbd.VkCode) {
				return 1
			}
		}
	}
	return callNextHookEx(keyboardHookHandle, nCode, wParam, lParam)
}

func mouseHookCallback(nCode int, wParam uintptr, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			logCrash("마우스 훅", r)
			ret = callNextHookEx(mouseHookHandle, nCode, wParam, lParam)
		}
	}()
	return mouseHookImpl(nCode, wParam, lParam)
}

func mouseHookImpl(nCode int, wParam uintptr, lParam uintptr) uintptr {
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
					bindPendingVK.Store(0)
					if TryBindKey(vk, name, heldModsFor(vk)) {
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
				if isDown {
					// 게임 밖에서는 실제 클릭을 그대로 통과시킨다.
					if gameOk && tryStartMacro(vk, heldModsFor(vk)) {
						return 1
					}
				} else if isUp {
					if stopMacroKey(vk) {
						return 1
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

	// 조합 트리거(예: Shift+R)면 눌려 있는 Shift를 게임에서 잠시 떼서, 대상 키가
	// 조합키 없이 그대로 들어가게 한다. 워커가 끝나면(어떤 경로로든) 원복한다.
	defer restorePhysMods(releasePhysMods(ctx.ConsumeMods))

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
		stopCh := autoClickerStopCh
		safeGo("오토클릭 워커", func() { runAutoClickerWorker(stopCh, ac.DelayMs, ac.DownMs) })
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
