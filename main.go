package main

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

type (
	C = layout.Context
	D = layout.Dimensions
)

const windowTitle = "마비노기 키 다운 - v" + appVersion

// autoSaveDelay: 설정 입력이 멈춘 뒤 자동 저장하기까지 기다리는 시간.
const autoSaveDelay = 600 * time.Millisecond

// scanLineFrameInterval:매크로 작동 중 스캔 라인 애니메이션의 프레임 간격(≈30fps).
const scanLineFrameInterval = 33 * time.Millisecond

// 색상 팔레트 (다크 럭셔리 · 웜 블랙 + 골드)
var (
	colBg          = color.NRGBA{R: 0x0B, G: 0x09, B: 0x06, A: 0xFF} // 딥 웜 블랙 배경
	colCard        = color.NRGBA{R: 0x15, G: 0x11, B: 0x0B, A: 0xFF} // 다크 패널
	colCardBorder  = color.NRGBA{R: 0x3A, G: 0x2F, B: 0x1D, A: 0xFF} // 은은한 골드 프레임
	colPrimary     = color.NRGBA{R: 0xCB, G: 0xB0, B: 0x7E, A: 0xFF} // 샴페인 골드 (포인트)
	colOnGold      = color.NRGBA{R: 0x14, G: 0x10, B: 0x07, A: 0xFF} // 앰버 위 텍스트(잉크)
	colText        = color.NRGBA{R: 0xEE, G: 0xE8, B: 0xDA, A: 0xFF} // 웜 오프화이트
	colSub         = color.NRGBA{R: 0x9C, G: 0x91, B: 0x7B, A: 0xFF} // 뮤트 골드그레이
	colFieldBg     = color.NRGBA{R: 0x19, G: 0x15, B: 0x0E, A: 0xFF}
	colFieldBorder = color.NRGBA{R: 0x3C, G: 0x33, B: 0x23, A: 0xFF}
	colEmptyBg     = color.NRGBA{R: 0x20, G: 0x1B, B: 0x13, A: 0xFF} // 미설정 키 버튼
	colGreen       = color.NRGBA{R: 0x6C, G: 0xC7, B: 0x92, A: 0xFF}
	colRed         = color.NRGBA{R: 0xDB, G: 0x5A, B: 0x4E, A: 0xFF}
	colAmber       = color.NRGBA{R: 0xE0, G: 0xB2, B: 0x4C, A: 0xFF}
	colChip        = color.NRGBA{R: 0x2A, G: 0x22, B: 0x14, A: 0xFF} // 다크 골드 칩 (추가 버튼)
	colWhite       = color.NRGBA{R: 0xF5, G: 0xF3, B: 0xEC, A: 0xFF}
)

// 페이지: 상태(모니터링 + on/off만) / 설정(키·슬롯·옵션 편집)
type appPage int

const (
	pageStatus appPage = iota
	pageSettings
)

const pageAnimDur = 220 * time.Millisecond

// easeOutCubic: 감속하며 끝나는 자연스러운 전환 곡선.
func easeOutCubic(t float32) float32 {
	f := t - 1
	return f*f*f + 1
}

// 키 바인딩 대상 종류
type bindKind int

const (
	bindNone bindKind = iota
	bindTrigger
	bindTarget
	bindAuto
	bindHotkey
)

type bindRef struct {
	kind bindKind
	slot int
}

type bindResult struct {
	vk        uint32
	mods      uint32
	name      string
	cancelled bool
	seq       int // 어느 바인딩 요청에서 온 결과인지(낡은 취소 무시용)
}

type slotState struct {
	trigBtn, targBtn, clearBtn, delBtn widget.Clickable
	delay, hold                        widget.Editor
	trigVK, targVK                     uint32
	trigMods                           uint32
	trigName, targName                 string
}

func newSlotState(delayMs, downMs int) *slotState {
	s := &slotState{}
	s.delay.SingleLine, s.delay.Filter = true, "0123456789"
	s.hold.SingleLine, s.hold.Filter = true, "0123456789"
	s.delay.SetText(strconv.Itoa(delayMs))
	s.hold.SetText(strconv.Itoa(downMs))
	return s
}

func main() {
	// 이미 실행 중이면 그 창을 앞으로 가져오고 종료한다(훅이 두 벌 설치되는 것을 막는다).
	if !ensureSingleInstance() {
		activateExistingInstance()
		return
	}

	LoadConfig()
	initCrashLog()      // 설정 폴더가 정해진 뒤, 다른 어떤 고루틴보다 먼저 열어 둔다
	MaintainAutoStart() // 예전 Run 키 항목 이전, 버전 교체로 사라진 경로 재등록

	if err := StartHookSystem(); err != nil {
		logEvent("훅 설치 실패", err)
		os.Exit(1)
	}

	safeGo("창 아이콘 적용", func() { applyWindowIcon(windowTitle) })
	StartFocusWatcher()

	go func() {
		// UI 루프에서 패닉이 나면 기록하고 종료한다(창이 멈춘 채 훅만 남아 키를 삼키는 것보다 낫다).
		defer func() {
			if r := recover(); r != nil {
				logCrash("UI 루프", r)
				StopHookSystem()
				os.Exit(1)
			}
		}()
		w := new(app.Window)
		w.Option(
			app.Title(windowTitle),
			app.Size(unit.Dp(360), unit.Dp(560)),
			app.MinSize(unit.Dp(360), unit.Dp(560)),
			app.Decorated(false), // OS 타이틀바를 끄고 자체 타이틀바(프레임리스)를 그린다
		)
		err := loop(w)
		StopHookSystem()
		if err != nil {
			logEvent("창 루프 오류 종료", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
	app.Main()
}

func loop(w *app.Window) error {
	th := material.NewTheme()
	// 한글 렌더링: gofont에는 한글 글리프가 없으므로 시스템 맑은 고딕을 최우선 로드.
	coll := gofont.Collection()
	if data, err := os.ReadFile(`C:\Windows\Fonts\malgun.ttf`); err == nil {
		if faces, err := opentype.ParseCollection(data); err == nil {
			coll = append(faces, coll...)
		}
	}
	th.Shaper = text.NewShaper(text.WithCollection(coll))
	th.Palette.Bg = colBg
	th.Palette.Fg = colText
	th.Palette.ContrastBg = colPrimary
	th.Palette.ContrastFg = colOnGold

	// 로고 이미지를 한 번만 디코딩해 재사용한다(프레임마다 디코딩하면 낭비).
	var logoOp paint.ImageOp
	haveLogo := false
	if img, err := decodeLogoImage(); err == nil {
		logoOp = paint.NewImageOp(img)
		haveLogo = true
	}

	// 슬롯 상태 초기화 (config에서 로드, 개수 가변)
	data := AppConfig.GetSlots()
	slots := make([]*slotState, len(data))
	for i := range data {
		s := newSlotState(data[i].DelayMs, data[i].DownMs)
		s.trigVK, s.trigName, s.trigMods = data[i].TriggerVK, data[i].TriggerName, data[i].TriggerMods
		s.targVK, s.targName = data[i].TargetVK, data[i].TargetName
		slots[i] = s
	}

	// 오토클릭 상태
	ac := AppConfig.GetAutoClicker()
	var (
		acTrigBtn, acClearBtn widget.Clickable
		acDelay, acHold       widget.Editor
		acTrigVK              = ac.TriggerVK
		acTrigName            = ac.TriggerName
		acRunning             atomic.Bool
	)
	acDelay.SingleLine, acDelay.Filter = true, "0123456789"
	acHold.SingleLine, acHold.Filter = true, "0123456789"
	acDelay.SetText(strconv.Itoa(ac.DelayMs))
	acHold.SetText(strconv.Itoa(ac.DownMs))

	AutoClickerStateCallback = func(running bool) {
		acRunning.Store(running)
		w.Invalidate()
	}

	// 매크로 on/off 단축키 (항상 Ctrl과 함께)
	var hotkeyBtn widget.Clickable
	hotkeyVK, hotkeyName := AppConfig.GetToggleHotkey()

	// 포커스 가드: 마비노기가 전경(포커스) 창일 때만 매크로가 반응하도록 하는 옵션.
	var focusGuardSwitch widget.Bool
	focusGuardSwitch.Value = AppConfig.GetFocusGuard()
	FocusStateCallback = func(bool) {
		w.Invalidate()
	}

	// 자동 실행: 레지스트리 값 존재 여부를 그대로 초기값으로 사용(기본 꺼짐).
	var autoStartSwitch widget.Bool
	autoStartSwitch.Value = IsAutoStartEnabled()

	// 채팅 중 일시정지: Enter 입력으로 채팅을 추정하는 방식이라 오감지 가능성이 있어
	// 끌 수 있게 하고, 상태 페이지에서 클릭해 초기화할 수 있게 한다.
	var chatGuardSwitch widget.Bool
	chatGuardSwitch.Value = AppConfig.GetChatGuard()
	var chatResetBtn widget.Clickable
	ChatStateCallback = func(bool) {
		w.Invalidate()
	}

	// 새 버전 알림: 공개 GitHub 릴리즈를 주기적으로 확인한다(끌 수 있음).
	var updateSwitch widget.Bool
	updateSwitch.Value = AppConfig.GetUpdateCheck()
	var updateBtn widget.Clickable
	UpdateStateCallback = func() {
		w.Invalidate()
	}
	StartUpdateChecker()

	focusGuardAnim := newSwitchAnim(focusGuardSwitch.Value)
	autoStartAnim := newSwitchAnim(autoStartSwitch.Value)
	chatGuardAnim := newSwitchAnim(chatGuardSwitch.Value)
	updateAnim := newSwitchAnim(updateSwitch.Value)

	var startBtn, addBtn widget.Clickable
	// 설정은 바뀌면 잠깐(autoSaveDelay) 뒤 자동 저장한다. 저장 직후엔 "저장됨 ✓"을 잠깐 보여준다.
	var savedFlashUntil time.Time
	savedFlash := false
	var saveDueAt time.Time
	var lastSig string
	// 설정 페이지 스크롤 목록의 상태. 프레임마다 새로 만들면 스크롤 위치가 매번
	// 리셋되어 스크롤이 안 되는 것처럼 보이므로, 반드시 loop() 수명 동안 유지한다.
	settingsList := &widget.List{}
	settingsList.Axis = layout.Vertical
	var deco widget.Decorations // 프레임리스 창의 최소화/닫기(+드래그 이동)

	macroActive := false
	warn := ""

	// --- 페이지 전환 상태 ---
	currentPage := pageStatus
	var navStatusBtn, navSettingsBtn widget.Clickable
	animating := false
	var animFrom, animTo appPage
	var animStart time.Time
	var animT float32
	animDir := 1

	navigateTo := func(target appPage, now time.Time) {
		if target == currentPage || animating {
			return
		}
		animFrom = currentPage
		animTo = target
		if target == pageSettings {
			animDir = 1
		} else {
			animDir = -1
		}
		currentPage = target
		animStart = now
		animT = 0 // 지난 전환이 남긴 값(보통 1)이 아니라 항상 0부터 다시 시작해야 한다
		animating = true
		w.Invalidate() // 다음 프레임을 바로 요청해 애니메이션 체인을 즉시 시작시킨다
	}

	var activeBind bindRef
	resultCh := make(chan bindResult, 8)

	// 바인딩 요청 번호. 바인딩 도중 다른 칸을 눌러 새로 시작하면 이전 요청의 "취소"
	// 결과가 새 요청 시작 뒤에 도착하는데, 번호를 비교하지 않으면 그 취소가 새 바인딩의
	// 상태(activeBind)까지 지워서 이후 눌린 키가 어디에도 저장되지 않고 사라진다.
	bindSeq := 0

	startBind := func(ref bindRef) {
		bindSeq++
		seq := bindSeq
		activeBind = ref
		bChan, cancel := StartBinding()
		go func() {
			select {
			case res := <-bChan:
				resultCh <- bindResult{vk: res.VK, mods: res.Mods, name: res.Name, seq: seq}
			case <-cancel:
				resultCh <- bindResult{cancelled: true, seq: seq}
			}
			w.Invalidate()
		}()
	}

	applyBind := func(r bindResult) {
		if r.cancelled {
			// 이미 더 새로운 바인딩이 시작됐다면(번호가 다르면) 낡은 취소는 무시한다.
			if r.seq == bindSeq {
				activeBind = bindRef{}
			}
			return
		}
		switch activeBind.kind {
		case bindTrigger:
			if activeBind.slot < len(slots) {
				// 조합키(Shift+R 등)는 트리거에서만 지원한다.
				slots[activeBind.slot].trigVK = r.vk
				slots[activeBind.slot].trigMods = r.mods
				slots[activeBind.slot].trigName = ModPrefix(r.mods) + r.name
			}
		case bindTarget:
			if activeBind.slot < len(slots) {
				slots[activeBind.slot].targVK = r.vk
				slots[activeBind.slot].targName = r.name
			}
		case bindAuto:
			acTrigVK = r.vk
			acTrigName = r.name
		case bindHotkey:
			hotkeyVK = r.vk
			hotkeyName = r.name
			AppConfig.SetToggleHotkey(hotkeyVK, hotkeyName)
			SaveConfig()
		}
		activeBind = bindRef{}
	}

	// settingsSig: 저장 대상 입력값(슬롯 + 오토클릭)을 한 문자열로 요약한다. 이 값이
	// 마지막 저장 때와 달라졌을 때만 저장하면 되므로, 입력창 글자 하나하나를 추적할 필요가 없다.
	settingsSig := func() string {
		var b strings.Builder
		for _, s := range slots {
			fmt.Fprintf(&b, "%d/%d/%s|%d/%s|%s|%s;", s.trigVK, s.trigMods, s.trigName, s.targVK, s.targName, s.delay.Text(), s.hold.Text())
		}
		fmt.Fprintf(&b, "A%d/%s|%s|%s", acTrigVK, acTrigName, acDelay.Text(), acHold.Text())
		return b.String()
	}

	// 현재 UI 슬롯 상태를 config에 반영하고 저장
	commit := func() {
		built := make([]MacroSlot, len(slots))
		for i, s := range slots {
			built[i] = MacroSlot{
				TriggerVK: s.trigVK, TriggerMods: s.trigMods, TriggerName: s.trigName,
				TargetVK: s.targVK, TargetName: s.targName,
				DelayMs: atoiDef(s.delay.Text(), 100),
				DownMs:  atoiDef(s.hold.Text(), DefaultDownMs),
			}
		}
		AppConfig.ReplaceSlots(built)
		AppConfig.UpdateAutoClicker(acTrigVK, acTrigName,
			atoiDef(acDelay.Text(), 100), atoiDef(acHold.Text(), DefaultDownMs))
		SaveConfig()
		lastSig = settingsSig() // 방금 저장한 내용 기준으로 맞춰 중복 저장을 막는다
		saveDueAt = time.Time{}
	}
	lastSig = settingsSig() // 시작 시점의 값 = 이미 저장돼 있는 값

	// 토글 펄스 애니메이션: on/off가 바뀔 때 상태 문구가 살짝 커졌다 작아지며 색이 전환되고,
	// 로고도 같은 진행률을 타고 그려지거나(on) 지워지는(off) 애니메이션을 재생한다.
	const toggleAnimDur = 480 * time.Millisecond
	toggleAnimActive := false
	var toggleAnimStart time.Time
	toggleAnimT := float32(1) // 1 = 애니메이션 없이 정착된 상태(올바른 색만 표시)

	// 매크로 시작/정지: 상태 페이지 텍스트 클릭과 전역 단축키(Ctrl+지정키) 둘 다 여기로 모인다.
	toggleMacro := func(now time.Time) {
		if macroActive {
			AppConfig.SetActive(false)
			StopAllWorkers()
			ResetChatState()
			macroActive = false
			toggleAnimStart, toggleAnimT, toggleAnimActive = now, 0, true
			w.Invalidate()
			return
		}
		hasAny := false
		conflict := false
		for _, s := range slots {
			if s.trigVK != 0 && s.targVK != 0 {
				hasAny = true
				// 오토클릭 트리거와 겹치면 훅에서 오토클릭이 우선 → 이 슬롯은 안 먹음
				if acTrigVK != 0 && s.trigVK == acTrigVK {
					conflict = true
				}
			}
		}
		if !hasAny {
			warn = "누르는 키 / 발동 키를 최소 1쌍 설정하세요 (설정 탭)."
		} else if conflict {
			warn = "오토클릭 트리거와 같은 키가 슬롯 트리거에 있어요 (오토클릭 우선)."
		} else {
			warn = ""
			commit()
			ResetChatState() // 켜는 시점엔 채팅이 닫혀 있다고 가정하고 시작
			AppConfig.SetActive(true)
			macroActive = true
			toggleAnimStart, toggleAnimT, toggleAnimActive = now, 0, true
			w.Invalidate()
		}
	}

	// 훅 스레드(백그라운드)에서 눌린 단축키를 UI 프레임 루프로 안전하게 전달한다.
	hotkeyToggleCh := make(chan struct{}, 1)
	ToggleHotkeyCallback = func() {
		select {
		case hotkeyToggleCh <- struct{}{}:
		default:
		}
		w.Invalidate()
	}
	drainHotkeyToggle := func() bool {
		select {
		case <-hotkeyToggleCh:
			return true
		default:
			return false
		}
	}

	var frameNow time.Time

	// 페이지 콘텐츠 렌더러 (애니메이션 중엔 두 페이지를 동시에 그리는 데도 재사용된다)
	renderPage := func(gtx C, page appPage) D {
		switch page {
		case pageStatus:
			hasAnyPair := false
			for _, s := range slots {
				if s.trigVK != 0 && s.targVK != 0 {
					hasAnyPair = true
					break
				}
			}
			return statusPageContent(gtx, th, &startBtn, macroActive, hasAnyPair,
				acTrigVK != 0, acRunning.Load(), focusGuardSwitch.Value, IsGameFocused(), warn, logoOp, haveLogo, toggleAnimT, hotkeyName, frameNow,
				AppConfig.GetChatGuard(), macroActive && AppConfig.GetChatGuard() && IsChatOpen(), &chatResetBtn,
				AvailableUpdate(), &updateBtn)
		default:
			return settingsPageContent(gtx, th, macroActive, slots, &addBtn,
				&acTrigBtn, &acClearBtn, &acDelay, &acHold, acTrigName, activeBind,
				&focusGuardSwitch, &autoStartSwitch, &chatGuardSwitch, &updateSwitch,
				focusGuardAnim.pos, autoStartAnim.pos, chatGuardAnim.pos, updateAnim.pos, IsGameFocused(),
				&hotkeyBtn, hotkeyName, settingsList, savedFlash)
		}
	}

	var ops op.Ops
	minimized := false // 창이 최소화돼 화면에 안 보이는 동안은 무한 애니메이션을 멈춘다
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			if !saveDueAt.IsZero() {
				commit() // 저장 대기 중이던 변경이 있으면 닫기 전에 반영한다
			}
			return e.Err
		case app.ConfigEvent:
			wasMin := minimized
			minimized = e.Config.Mode == app.Minimized
			if wasMin && !minimized {
				w.Invalidate() // 복원되면 멈춰 있던 애니메이션 체인을 다시 시작
			}
		case app.FrameEvent:
			frameNow = e.Now
			gtx := app.NewContext(&ops, e)

			// 바인딩 결과 반영
			for {
				select {
				case r := <-resultCh:
					applyBind(r)
					continue
				default:
				}
				break
			}

			// 프레임리스 타이틀바의 최소화/닫기 버튼 처리 (페이지/애니메이션 상태와 무관하게 항상 반응)
			if winActs := deco.Update(gtx); winActs != 0 {
				w.Perform(winActs)
			}

			// --- 애니메이션 진행 계산 (렌더 전에 확정) ---
			if animating {
				t := float32(e.Now.Sub(animStart)) / float32(pageAnimDur)
				if t >= 1 {
					t = 1
					animating = false
				}
				animT = easeOutCubic(t)
				if animating {
					w.Invalidate() // 다음 프레임을 즉시 요청해 애니메이션을 이어간다
				}
			}

			// 네비게이션 클릭은 애니메이션 중이 아닐 때만 반응한다.
			if !animating {
				if navStatusBtn.Clicked(gtx) {
					navigateTo(pageStatus, e.Now)
				}
				if navSettingsBtn.Clicked(gtx) {
					navigateTo(pageSettings, e.Now)
				}
			}

			// 포커스 가드 토글은 매크로 활성 여부와 무관하게 언제든 바꿀 수 있다.
			if !animating && focusGuardSwitch.Update(gtx) {
				AppConfig.SetFocusGuard(focusGuardSwitch.Value)
				SaveConfig()
			}

			if !animating && chatGuardSwitch.Update(gtx) {
				AppConfig.SetChatGuard(chatGuardSwitch.Value)
				SaveConfig()
				if !chatGuardSwitch.Value {
					ResetChatState() // 끄면 일시정지 상태도 즉시 해제
				}
			}

			// 상태 페이지의 "채팅 중" 줄을 클릭하면 추정 상태를 초기화한다.
			if chatResetBtn.Clicked(gtx) {
				ResetChatState()
			}

			if !animating && updateSwitch.Update(gtx) {
				AppConfig.SetUpdateCheck(updateSwitch.Value)
				SaveConfig()
				if updateSwitch.Value {
					TriggerUpdateCheck() // 켜자마자 한 번 확인
				} else {
					ClearUpdateNotice()
				}
			}

			// 새 버전 알림 줄을 클릭하면 릴리즈 페이지를 연다.
			if updateBtn.Clicked(gtx) {
				if u := AvailableUpdate(); u != nil {
					OpenReleasePage(u.URL)
				}
			}

			if !animating && autoStartSwitch.Update(gtx) {
				if err := SetAutoStart(autoStartSwitch.Value); err != nil {
					autoStartSwitch.Value = !autoStartSwitch.Value // 실패 시 되돌림
				}
			}

			// 스위치 썸(동그라미) 이동 애니메이션 진행
			if focusGuardAnim.tick(&focusGuardSwitch, e.Now) {
				w.Invalidate()
			}
			if autoStartAnim.tick(&autoStartSwitch, e.Now) {
				w.Invalidate()
			}
			if chatGuardAnim.tick(&chatGuardSwitch, e.Now) {
				w.Invalidate()
			}
			if updateAnim.tick(&updateSwitch, e.Now) {
				w.Invalidate()
			}

			// --- 설정 페이지 전용 이벤트 처리 (편집 가능 상태에서만) ---
			if !animating && currentPage == pageSettings && !macroActive {
				removeIdx := -1
				for i, s := range slots {
					if s.trigBtn.Clicked(gtx) {
						startBind(bindRef{bindTrigger, i})
					}
					if s.targBtn.Clicked(gtx) {
						startBind(bindRef{bindTarget, i})
					}
					if s.clearBtn.Clicked(gtx) {
						s.trigVK, s.trigMods, s.trigName = 0, 0, ""
						s.targVK, s.targName = 0, ""
						s.delay.SetText("100")
						s.hold.SetText(strconv.Itoa(DefaultDownMs))
					}
					if s.delBtn.Clicked(gtx) && len(slots) > 1 {
						removeIdx = i
					}
				}
				if removeIdx >= 0 {
					slots = append(slots[:removeIdx], slots[removeIdx+1:]...)
					activeBind = bindRef{} // 인덱스 변동 → 진행 중 바인딩 무효화
					commit()
				}
				if addBtn.Clicked(gtx) && len(slots) < MaxSlots {
					slots = append(slots, newSlotState(100, DefaultDownMs))
					commit()
				}
				if acTrigBtn.Clicked(gtx) {
					startBind(bindRef{bindAuto, -1})
				}
				if hotkeyBtn.Clicked(gtx) {
					startBind(bindRef{bindHotkey, -1})
				}
				if acClearBtn.Clicked(gtx) {
					acTrigVK, acTrigName = 0, ""
					acDelay.SetText("100")
					acHold.SetText(strconv.Itoa(DefaultDownMs))
				}
			}

			// --- 상태 페이지(상태 문구 클릭) 또는 단축키로만 매크로를 켜고 끈다 ---
			if !animating && (startBtn.Clicked(gtx) || drainHotkeyToggle()) {
				toggleMacro(e.Now)
			}

			// 토글 펄스 애니메이션 진행 계산
			if toggleAnimActive {
				t := float32(e.Now.Sub(toggleAnimStart)) / float32(toggleAnimDur)
				if t >= 1 {
					t = 1
					toggleAnimActive = false
				}
				toggleAnimT = t // sin() 펄스는 렌더 쪽에서 직접 만들므로 선형 값을 그대로 넘긴다
				if toggleAnimActive {
					w.Invalidate()
				}
			}

			// 매크로 작동 중에는 상태 문구 아래 라인 스캔 애니메이션이 계속 흘러가야 한다.
			// 다만 화면에 보이지 않는 최소화 상태에서는 그릴 필요가 없으니 멈추고, 보일 때도
			// 매 프레임(60fps)이 아니라 약 30fps로만 다시 그려 CPU/GPU 사용을 줄인다.
			// (최소화에서 복원되면 ConfigEvent → 프레임이 다시 오면서 이 요청이 재개된다)
			if macroActive && currentPage == pageStatus && !minimized {
				gtx.Execute(op.InvalidateCmd{At: e.Now.Add(scanLineFrameInterval)})
			}

			// 자동 저장: 설정 페이지에서 값이 바뀌면 입력이 멈춘 뒤 autoSaveDelay 후에 저장한다
			// (숫자를 한 글자씩 칠 때마다 디스크에 쓰지 않도록). 페이지를 떠나거나 매크로를
			// 켜는 경우엔 기다리지 않고 바로 저장한다.
			if currentPage == pageSettings && !macroActive {
				if sig := settingsSig(); sig != lastSig {
					lastSig = sig
					saveDueAt = e.Now.Add(autoSaveDelay)
				}
				if !saveDueAt.IsZero() {
					if !e.Now.Before(saveDueAt) {
						commit()
						savedFlashUntil = e.Now.Add(1200 * time.Millisecond)
					} else {
						gtx.Execute(op.InvalidateCmd{At: saveDueAt})
					}
				}
			} else if !saveDueAt.IsZero() {
				commit()
			}

			// "저장됨" 표시가 떠 있는 동안은 계속 다음 프레임을 요청해 시간이 지나면 저절로 사라지게 한다.
			savedFlash = !savedFlashUntil.IsZero() && e.Now.Before(savedFlashUntil)
			if savedFlash {
				w.Invalidate()
			} else {
				savedFlashUntil = time.Time{}
			}

			// 편집 잠금 (활성 중)
			for _, s := range slots {
				s.delay.ReadOnly = macroActive
				s.hold.ReadOnly = macroActive
			}

			// --- 렌더 ---
			paint.Fill(gtx.Ops, colBg)
			layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					return titleBar(gtx, th, &deco, &navStatusBtn, &navSettingsBtn, currentPage)
				}),
				layout.Flexed(1, func(gtx C) D {
					return layout.UniformInset(unit.Dp(12)).Layout(gtx, func(gtx C) D {
						if animating {
							return renderSlide(gtx, renderPage, animFrom, animTo, animT, animDir)
						}
						return renderPage(gtx, currentPage)
					})
				}),
			)

			e.Frame(gtx.Ops)
		}
	}
}

// renderSlide: from 페이지가 밀려나가고 to 페이지가 밀려들어오는 가로 슬라이드 전환.
// dir=+1: 오른쪽에서 들어옴(상태→설정), dir=-1: 왼쪽에서 들어옴(설정→상태).
// renderSlide: from 페이지가 옅어지며 살짝 위로, to 페이지가 짙어지며 제자리로
// 내려오는 오퍼시티 기반 크로스페이드. (기존 가로 슬라이드 대신 "스르륵" 느낌)
func renderSlide(gtx C, renderPage func(C, appPage) D, from, to appPage, eased float32, dir int) D {
	size := gtx.Constraints.Max
	defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()

	sub := gtx
	sub.Constraints = layout.Exact(size)

	drift := gtx.Dp(unit.Dp(10))

	oldStack := paint.PushOpacity(gtx.Ops, 1-eased)
	t1 := op.Offset(image.Pt(0, -int(eased*float32(drift)))).Push(gtx.Ops)
	renderPage(sub, from)
	t1.Pop()
	oldStack.Pop()

	newStack := paint.PushOpacity(gtx.Ops, eased)
	t2 := op.Offset(image.Pt(0, int((1-eased)*float32(drift)))).Push(gtx.Ops)
	renderPage(sub, to)
	t2.Pop()
	newStack.Pop()

	return D{Size: size}
}

// ---------- 네비게이션 ----------

// navBar: 상태/설정 2단 세그먼트 탭. 창 폭에 맞춰 절반씩 균등 분할되므로
// 어떤 창 크기에서도 두 탭 모두 항상 잘리지 않고 클릭 가능하다.
// ---------- 커스텀 타이틀바 (프레임리스) ----------

const titleBarHeight = 34

// titleBar: OS 타이틀바 대신 그리는 자체 상단바. 로고+제목 영역을 드래그로
// 창 이동(system.ActionMove)에 매핑하고, 최소화/닫기 버튼만 오른쪽에 둔다.
// (창을 최대화할 필요가 없는 작은 유틸리티 창이라 최대화 버튼은 넣지 않았다)
// titleBar: 드래그 이동 영역 + (상태/설정 아이콘) + 최소화/닫기만 있는 최소한의 바.
// 제목·로고는 빼서 최대한 심플하게 유지한다(브랜딩은 각 페이지 콘텐츠에서 보여줌).
func titleBar(gtx C, th *material.Theme, deco *widget.Decorations, navStatusBtn, navSettingsBtn *widget.Clickable, page appPage) D {
	gtx.Constraints.Min.Y = gtx.Dp(unit.Dp(titleBarHeight))
	gtx.Constraints.Max.Y = gtx.Dp(unit.Dp(titleBarHeight))

	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			paint.Fill(gtx.Ops, colCard)
			return D{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx C) D {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
				layout.Flexed(1, func(gtx C) D {
					return deco.LayoutMove(gtx, func(gtx C) D { return D{Size: gtx.Constraints.Max} })
				}),
				layout.Rigid(func(gtx C) D {
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx C) D {
							// 메인(상태) 페이지에서는 설정 아이콘만, 설정 페이지에서는 홈 아이콘만 보인다.
							if page == pageStatus {
								return navIconBtn(gtx, navSettingsBtn, drawSlidersIcon)
							}
							return navIconBtn(gtx, navStatusBtn, drawHomeIcon)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
						layout.Rigid(func(gtx C) D { return titleBarBtn(gtx, deco, system.ActionMinimize, colSub, false) }),
						layout.Rigid(func(gtx C) D { return titleBarBtn(gtx, deco, system.ActionClose, colRed, true) }),
					)
				}),
			)
		}),
	)
}

// navIconBtn: 상태(●)/설정(○) 페이지 이동용 작은 아이콘 버튼. 채운 점=상태, 링=설정.
// 현재 페이지는 골드로, 아니면 뮤트 컬러로 표시한다.
// navIconBtn: 현재 페이지에 맞는 단일 아이콘 버튼(상태 페이지=설정 아이콘,
// 설정 페이지=홈 아이콘)만 그린다. 항상 뮤트 컬러 — 이동 액션이라 강조가 필요 없다.
func navIconBtn(gtx C, btn *widget.Clickable, draw func(C, color.NRGBA, int)) D {
	sz := gtx.Dp(unit.Dp(titleBarHeight))
	gtx.Constraints = layout.Exact(image.Pt(sz, sz))
	return btn.Layout(gtx, func(gtx C) D {
		return layout.Center.Layout(gtx, func(gtx C) D {
			d := gtx.Dp(unit.Dp(15))
			draw(gtx, colSub, d)
			return D{Size: image.Pt(d, d)}
		})
	})
}

// drawSlidersIcon: 톱니바퀴 대신 쓰는 "설정" 아이콘 — 굵기가 다른 가로선 3개 +
// 각 선의 작은 손잡이(동그라미). 회전 도형이 없어 작은 크기에서도 또렷하다.
func drawSlidersIcon(gtx C, col color.NRGBA, d int) {
	fd := float32(d)
	// 정수 나눗셈으로 0이 되어 선이 안 보이는 일이 없도록 최소값을 보장한다.
	lineW := gtx.Dp(unit.Dp(1.6))
	if lineW < 1 {
		lineW = 1
	}
	knob := gtx.Dp(unit.Dp(4))
	if knob < 3 {
		knob = 3
	}
	ys := [3]float32{fd * 0.2, fd * 0.5, fd * 0.8}
	knobX := [3]float32{fd * 0.7, fd * 0.3, fd * 0.6}

	for i, y := range ys {
		yi := int(y)
		r := image.Rect(0, yi, d, yi+lineW)
		paint.FillShape(gtx.Ops, col, clip.Rect(r).Op())
		cx := int(knobX[i])
		cy := yi + lineW/2
		paint.FillShape(gtx.Ops, col, clip.Ellipse{
			Min: image.Pt(cx-knob/2, cy-knob/2),
			Max: image.Pt(cx+knob/2, cy+knob/2),
		}.Op(gtx.Ops))
	}
}

// drawHomeIcon: 단순한 집(오각형) 실루엣. 상태 페이지로 돌아가는 버튼에 쓴다.
func drawHomeIcon(gtx C, col color.NRGBA, d int) {
	fd := float32(d)
	pts := []f32.Point{
		{X: fd * 0.5, Y: fd * 0.06},
		{X: fd * 0.92, Y: fd * 0.44},
		{X: fd * 0.92, Y: fd * 0.94},
		{X: fd * 0.08, Y: fd * 0.94},
		{X: fd * 0.08, Y: fd * 0.44},
	}
	var p clip.Path
	p.Begin(gtx.Ops)
	p.MoveTo(pts[0])
	for _, pt := range pts[1:] {
		p.LineTo(pt)
	}
	p.Close()
	paint.FillShape(gtx.Ops, col, clip.Outline{Path: p.End()}.Op())
}

// titleBarBtn: 최소화/닫기 버튼. 각자 자기 사각형 영역에만 ActionInputOp을 걸어
// 위 드래그 영역과 절대 겹치지 않게 한다(겹치면 클릭이 드래그로 먹혀버림).
func titleBarBtn(gtx C, deco *widget.Decorations, action system.Action, iconCol color.NRGBA, isClose bool) D {
	sz := gtx.Dp(unit.Dp(titleBarHeight))
	gtx.Constraints = layout.Exact(image.Pt(sz, sz))
	cl := deco.Clickable(action)
	return cl.Layout(gtx, func(gtx C) D {
		system.ActionInputOp(action).Add(gtx.Ops)
		hovered := cl.Hovered()
		if hovered {
			bg := color.NRGBA{R: 0x2A, G: 0x22, B: 0x14, A: 0xFF}
			if isClose {
				bg = color.NRGBA{R: 0x5A, G: 0x22, B: 0x1E, A: 0xFF}
			}
			paint.FillShape(gtx.Ops, bg, clip.Rect{Max: gtx.Constraints.Min}.Op())
		}
		return layout.Center.Layout(gtx, func(gtx C) D {
			if action == system.ActionClose {
				return drawXIcon(gtx, iconCol)
			}
			return drawMinusIcon(gtx, iconCol)
		})
	})
}

func drawMinusIcon(gtx C, col color.NRGBA) D {
	w := gtx.Dp(unit.Dp(10))
	th := gtx.Dp(unit.Dp(1.4))
	r := image.Rect(0, 0, w, th)
	paint.FillShape(gtx.Ops, col, clip.Rect(r).Op())
	return D{Size: image.Pt(w, th)}
}

func drawXIcon(gtx C, col color.NRGBA) D {
	sz := gtx.Dp(unit.Dp(10))
	strokeW := float32(gtx.Dp(unit.Dp(1.3)))
	var p clip.Path
	p.Begin(gtx.Ops)
	p.MoveTo(f32.Pt(0, 0))
	p.LineTo(f32.Pt(float32(sz), float32(sz)))
	p.MoveTo(f32.Pt(float32(sz), 0))
	p.LineTo(f32.Pt(0, float32(sz)))
	st := clip.Stroke{Path: p.End(), Width: strokeW}.Op().Push(gtx.Ops)
	paint.ColorOp{Color: col}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	st.Pop()
	return D{Size: image.Pt(sz, sz)}
}

// ---------- 상태 페이지 ----------

// statusPageContent: 로고 + (클릭하면 on/off 되는) 상태 문구 + 체크리스트.
// 별도 큰 버튼 없이 상태 문구 자체가 토글 버튼이다(펄스+색 전환 애니메이션 포함).
func statusPageContent(gtx C, th *material.Theme, toggleArea *widget.Clickable, active, hasAnyPair, acConfigured, acRunning, focusGuardOn, gameFocused bool, warn string, logoOp paint.ImageOp, haveLogo bool, toggleAnimT float32, hotkeyName string, now time.Time, chatGuardOn, chatPaused bool, chatBtn *widget.Clickable, upd *UpdateInfo, updBtn *widget.Clickable) D {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Flexed(1, func(gtx C) D {
			return statusCenterContent(gtx, th, toggleArea, active, hasAnyPair, acConfigured, acRunning, focusGuardOn, gameFocused, logoOp, haveLogo, toggleAnimT, now, chatGuardOn, chatPaused, chatBtn)
		}),
		layout.Rigid(func(gtx C) D {
			if warn == "" {
				return D{}
			}
			l := material.Label(th, unit.Sp(11), warn)
			l.Color = colRed
			l.Alignment = text.Middle
			return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return l.Layout(gtx)
			})
		}),
		layout.Rigid(func(gtx C) D {
			if upd == nil {
				return D{}
			}
			// 새 버전이 있으면 한 줄로 알리고, 클릭하면 릴리즈 페이지를 연다.
			return updBtn.Layout(gtx, func(gtx C) D {
				l := material.Label(th, unit.Sp(11), "새 버전 v"+upd.Version+" 이 나왔어요 · 클릭해서 보기")
				l.Color = colPrimary
				l.Font.Weight = 600
				l.Alignment = text.Middle
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(6)}.Layout(gtx, l.Layout)
			})
		}),
		layout.Rigid(func(gtx C) D {
			l := material.Label(th, unit.Sp(10), "Ctrl + "+pick(hotkeyName, "F12")+"로 켜고 끌 수 있어요")
			l.Color = colFieldBorder
			l.Alignment = text.Middle
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{Bottom: unit.Dp(4)}.Layout(gtx, l.Layout)
		}),
	)
}

// statusCenterContent: 로고·제목·체크리스트 등 상태 페이지의 중앙 콘텐츠.
func statusCenterContent(gtx C, th *material.Theme, toggleArea *widget.Clickable, active, hasAnyPair, acConfigured, acRunning, focusGuardOn, gameFocused bool, logoOp paint.ImageOp, haveLogo bool, toggleAnimT float32, now time.Time, chatGuardOn, chatPaused bool, chatBtn *widget.Clickable) D {
	return layout.Center.Layout(gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return toggleTitleArea(gtx, th, toggleArea, active, toggleAnimT, logoOp, haveLogo, now)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx C) D {
				return checklistRow(gtx, th, hasAnyPair, "트리거 슬롯 설정됨")
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx C) D {
				txt := "오토 좌클릭 미설정"
				if acConfigured {
					txt = "오토 좌클릭 설정됨"
				}
				if acRunning {
					txt = "오토 좌클릭 작동 중"
				}
				return checklistRow(gtx, th, acRunning, txt)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx C) D {
				txt := "포커스 가드 꺼짐"
				if focusGuardOn {
					txt = "포커스 가드 켜짐"
				}
				return checklistRow(gtx, th, focusGuardOn, txt)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx C) D {
				txt := "다른 프로그램 사용 중"
				if !focusGuardOn {
					txt = "게임 포커스 감지 안 함(가드 꺼짐)"
				} else if gameFocused {
					txt = "마비노기 포커스 중"
				}
				return checklistRow(gtx, th, focusGuardOn && gameFocused, txt)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx C) D {
				// 채팅 감지 상태는 항상 한 줄로 보여준다. 채팅 중으로 추정되면 주황색
				// 안내로 바뀌고, 클릭하면 감지 상태를 초기화한다(오감지 대비).
				switch {
				case chatPaused:
					return chatBtn.Layout(gtx, func(gtx C) D {
						return checklistRowCol(gtx, th, colAmber, "채팅 중 · 매크로 일시정지 (클릭해 초기화)")
					})
				case chatGuardOn:
					return checklistRow(gtx, th, true, "채팅 감지 켜짐")
				default:
					return checklistRow(gtx, th, false, "채팅 감지 꺼짐")
				}
			}),
		)
	})
}

func pick(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// toggleTitleArea: 제목 + 상태 문구를 감싸는 클릭 영역. 클릭 시 toggleMacro가 실행되고,
// toggleAnimT(0→1)에 따라 짧게 확대되었다 돌아오는 펄스와 상태색 전환이 함께 재생된다.
// toggleTitleArea: 로고+제목+상태문구를 한데 묶은 큰 클릭 영역. 이 영역 전체가
// on/off 토글 버튼이다 — 호버 시 은은한 배경으로 "누를 수 있음"을 알려주고,
// 클릭 시 펄스(살짝 커졌다 작아짐) + 색 전환 애니메이션이 재생된다.
func toggleTitleArea(gtx C, th *material.Theme, area *widget.Clickable, active bool, animT float32, logoOp paint.ImageOp, haveLogo bool, now time.Time) D {
	return area.Layout(gtx, func(gtx C) D {
		// widget.Clickable는 리턴된(스케일 적용 전) Dimensions 크기로 클립을 건다.
		// 그래서 실제 콘텐츠 주위에 여유 패딩을 둬 그 크기가 클립 영역에 포함되게 하고,
		// 확대(pulse) 시 이 패딩 안에서만 커지도록 해 가장자리가 잘리지 않게 한다.
		pad := unit.Dp(14)

		// 로고 애니메이션 진행률(0~1): "완성된(on) 상태로부터의 거리"를 의미한다.
		// on으로 켤 때는 animT(0→1)를 그대로 쓰고, off로 끌 때는 이를 뒤집어(1→0)
		// 그리기 진행을 거꾸로 재생해 지워지는 것처럼 보이게 한다.
		logoD := animT
		if !active {
			logoD = 1 - animT
		}

		macro := op.Record(gtx.Ops)
		dims := layout.UniformInset(pad).Layout(gtx, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					return logoMarkAnim(gtx, unit.Dp(92), logoOp, haveLogo, logoD)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
				layout.Rigid(func(gtx C) D {
					l := material.Label(th, unit.Sp(17), "마비노기 키 다운")
					l.Color = colText
					l.Font.Weight = 600
					l.Alignment = text.Middle
					return l.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(3)}.Layout),
				layout.Rigid(func(gtx C) D {
					txt := "매크로 대기 중"
					from, to := colPrimary, colSub
					if active {
						txt, from, to = "매크로 작동 중", colSub, colPrimary
					}
					l := material.Label(th, unit.Sp(13), txt)
					l.Color = lerpColor(from, to, animT)
					l.Font.Weight = 600
					l.Alignment = text.Middle
					return l.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(7)}.Layout),
				layout.Rigid(func(gtx C) D {
					return scanLine(gtx, active, now)
				}),
			)
		})
		call := macro.Stop()

		// 호버 시 클릭 가능함을 알리는 은은한 배경(스케일과 무관하게 항상 이 크기).
		if area.Hovered() {
			r := clip.RRect{Rect: image.Rectangle{Max: dims.Size}, NW: 16, NE: 16, SE: 16, SW: 16}
			paint.FillShape(gtx.Ops, color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0x0C}, r.Op(gtx.Ops))
		}

		pulse := float32(0)
		if animT > 0 && animT < 1 {
			pulse = float32(math.Sin(float64(animT) * math.Pi))
		}
		scale := 1 + 0.05*pulse
		center := f32.Pt(float32(dims.Size.X)/2, float32(dims.Size.Y)/2)
		t := op.Affine(f32.Affine2D{}.Scale(center, f32.Pt(scale, scale))).Push(gtx.Ops)
		call.Add(gtx.Ops)
		t.Pop()

		return dims
	})
}

const (
	scanLineWidthDp  = 130
	scanLineHeightDp = 10
	scanLinePeriod   = 1400 * time.Millisecond
)

// scanLine: 매크로 작동 중일 때 상태 문구 아래에 가로선을 그리고, 그 위를 빛 덩어리가
// 무한히 왼쪽→오른쪽으로 흘러가는 애니메이션을 재생한다. 비활성 상태에서는 선 자체를
// 그리지 않지만(투명), 자리 차지 크기는 항상 동일해 on/off 전환 시 높이가 흔들리지 않는다.
func scanLine(gtx C, active bool, now time.Time) D {
	w := gtx.Dp(unit.Dp(scanLineWidthDp))
	h := gtx.Dp(unit.Dp(scanLineHeightDp))
	size := image.Pt(w, h)
	if !active || w <= 0 || h <= 0 {
		return D{Size: size}
	}

	midY := h / 2

	// 은은한 바탕 선.
	baseCol := color.NRGBA{R: colPrimary.R, G: colPrimary.G, B: colPrimary.B, A: 0x30}
	paint.FillShape(gtx.Ops, baseCol, clip.Rect(image.Rect(0, midY, w, midY+1)).Op())

	// 무한 루프 진행률(0~1): period마다 처음으로 돌아온다.
	periodMs := int64(scanLinePeriod / time.Millisecond)
	phase := float64(now.UnixMilli()%periodMs) / float64(periodMs)

	cometW := w / 3
	if cometW < 6 {
		cometW = 6
	}
	travel := float64(w + cometW)
	headX := int(phase*travel) - cometW

	for i := 0; i < cometW; i++ {
		px := headX + i
		if px < 0 || px >= w {
			continue
		}
		t := float32(i) / float32(cometW) // 꼬리(0) → 머리(1)로 갈수록 밝아진다
		a := uint8(0xE0 * t * t)
		col := color.NRGBA{R: colPrimary.R, G: colPrimary.G, B: colPrimary.B, A: a}
		paint.FillShape(gtx.Ops, col, clip.Rect(image.Rect(px, midY-1, px+1, midY+2)).Op())
	}

	return D{Size: size}
}

// lerpColor: 두 색 사이를 t(0~1)만큼 선형 보간한다.
func lerpColor(a, b color.NRGBA, t float32) color.NRGBA {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	lp := func(x, y uint8) uint8 { return uint8(float32(x) + (float32(y)-float32(x))*t) }
	return color.NRGBA{R: lp(a.R, b.R), G: lp(a.G, b.G), B: lp(a.B, b.B), A: lp(a.A, b.A)}
}

// checklistRow: 점 표시(초록=충족/회색=미충족) + 설명 텍스트 한 줄.
func checklistRow(gtx C, th *material.Theme, ok bool, txt string) D {
	col := colSub
	if ok {
		col = colGreen
	}
	return checklistRowCol(gtx, th, col, txt)
}

// checklistRowCol: checklistRow와 같은 모양에 점 색을 직접 지정한다.
func checklistRowCol(gtx C, th *material.Theme, col color.NRGBA, txt string) D {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			d := gtx.Dp(unit.Dp(6))
			r := clip.Ellipse{Max: image.Pt(d, d)}
			paint.FillShape(gtx.Ops, col, r.Op(gtx.Ops))
			return D{Size: image.Pt(d, d)}
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(7)}.Layout),
		layout.Rigid(func(gtx C) D {
			l := material.Label(th, unit.Sp(12), txt)
			l.Color = colSub
			return l.Layout(gtx)
		}),
	)
}

// logoMark: 골드 원형 배지 위에 마비노기 팬사이트 키트의 공식 심볼(logo.png)을
// 원본 그대로(색 변형 없이) 얹는다. 이미지 디코딩에 실패하면 골드 배지만 그린다.
// logoDimAlpha: 매크로가 꺼져 있을 때(정착 상태) 로고를 아주 흐리게 깔아두는 투명도.
const logoDimAlpha = 0.16

// logoMarkAnim: 로고를 "완성된(on) 상태로부터의 거리" d(0=완전히 꺼진 정착 상태,
// 1=완전히 켜진 정착 상태)에 따라 그린다. 항상 맨 뒤(z-index 가장 아래)에 아주
// 흐린(logoDimAlpha) 원본 이미지를 깔아두고, 그 위에 중심에서부터 원(iris) 모양
// 으로 커지는 마스크로 같은 원본 이미지를 온전한 밝기로 겹쳐 그린다 — 벡터로
// 다시 그리는 게 아니라 항상 같은 원본 픽셀을 마스킹만 하는 것이므로, 어느
// 시점에 봐도 최종 완성본과 완벽히 동일한 선이다. 드러난 경계에는 옅은 골드
// 링을 얹어 "그려지는" 느낌을 더한다.
//
// off→on(켜기)에는 d를 0→1로, on→off(끄기)에는 반대로 1→0으로 재생해 같은 로직으로
// "나타남"과 "사라짐"을 모두 표현한다.
func logoMarkAnim(gtx C, dp unit.Dp, img paint.ImageOp, haveLogo bool, d float32) D {
	sz := gtx.Dp(dp)
	if !haveLogo {
		return D{Size: image.Pt(sz, sz)}
	}
	if d < 0 {
		d = 0
	} else if d > 1 {
		d = 1
	}

	drawFull := func() {
		sub := gtx
		sub.Constraints = layout.Exact(image.Pt(sz, sz))
		widget.Image{Src: img, Fit: widget.Contain, Position: layout.Center}.Layout(sub)
	}

	// 뒷배경: 항상 흐리게 깔아둔다(z-index 가장 아래).
	st := paint.PushOpacity(gtx.Ops, logoDimAlpha)
	drawFull()
	st.Pop()

	if d <= 0 {
		return D{Size: image.Pt(sz, sz)}
	}

	center := f32.Pt(float32(sz)/2, float32(sz)/2)
	maxRadius := float32(sz) * 0.72 // 모서리까지 넉넉히 덮는 반경
	radius := maxRadius * d

	circle := clip.Ellipse{
		Min: image.Pt(int(center.X-radius), int(center.Y-radius)),
		Max: image.Pt(int(center.X+radius), int(center.Y+radius)),
	}

	// 앞배경: 원 안쪽만 온전한 밝기로 겹쳐 그린다.
	if radius > 0.5 {
		st := circle.Push(gtx.Ops)
		drawFull()
		st.Pop()
	}

	// 드러나는 경계를 은은한 골드 링으로 강조한다(끝날 무렵엔 서서히 사라짐).
	if d < 1 {
		ringAlpha := float32(1)
		if d > 0.8 {
			ringAlpha = 1 - (d-0.8)/0.2
		}
		ringW := float32(gtx.Dp(unit.Dp(1.5)))
		st := paint.PushOpacity(gtx.Ops, ringAlpha)
		strokeOp := clip.Stroke{Path: circle.Path(gtx.Ops), Width: ringW}.Op().Push(gtx.Ops)
		paint.ColorOp{Color: colPrimary}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		strokeOp.Pop()
		st.Pop()
	}

	return D{Size: image.Pt(sz, sz)}
}

// ---------- 설정 페이지 ----------

func settingsPageContent(gtx C, th *material.Theme, macroActive bool, slots []*slotState, addBtn *widget.Clickable,
	acTrigBtn, acClearBtn *widget.Clickable, acDelay, acHold *widget.Editor, acTrigName string, ab bindRef,
	focusGuardSwitch, autoStartSwitch, chatGuardSwitch, updateSwitch *widget.Bool, focusGuardPos, autoStartPos, chatGuardPos, updatePos float32, gameFocused bool,
	hotkeyBtn *widget.Clickable, hotkeyName string, list *widget.List, saved bool) D {

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return focusGuardRow(gtx, th, focusGuardSwitch, focusGuardPos, gameFocused)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx C) D {
			return simpleToggleRow(gtx, th, chatGuardSwitch, chatGuardPos, "채팅 중에는 일시정지", "Enter로 채팅을 열면 매크로 키가 그대로 입력돼요 (Esc·상태 화면 클릭으로 초기화)")
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx C) D {
			return simpleToggleRow(gtx, th, autoStartSwitch, autoStartPos, "윈도우 시작 시 자동 실행", "컴퓨터를 켜면 이 프로그램이 자동으로 실행돼요")
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx C) D {
			return simpleToggleRow(gtx, th, updateSwitch, updatePos, "새 버전 알림", "새 버전이 나오면 상태 화면에 알려줘요 (자동 설치는 하지 않아요)")
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx C) D {
			return hotkeyRow(gtx, th, hotkeyBtn, hotkeyName, ab.kind == bindHotkey)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
		layout.Flexed(1, func(gtx C) D {
			items := make([]layout.Widget, 0, 3)
			items = append(items, func(gtx C) D {
				return slotsPanel(gtx, th, slots, ab, macroActive)
			})
			if !macroActive && len(slots) < MaxSlots {
				items = append(items, func(gtx C) D {
					return addSlotButton(gtx, th, addBtn, len(slots))
				})
			}
			items = append(items, func(gtx C) D {
				return autoCard(gtx, th, acTrigBtn, acClearBtn,
					acDelay, acHold, acTrigName, ab, macroActive)
			})
			return material.List(th, list).Layout(gtx, len(items), func(gtx C, i int) D {
				return layout.Inset{Bottom: unit.Dp(7)}.Layout(gtx, items[i])
			})
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
		layout.Rigid(func(gtx C) D {
			return settingsSaveHint(gtx, th, saved)
		}),
	)
}

// settingsSaveHint: 설정은 바뀔 때 자동 저장된다. 저장 버튼 자리에는 안내 문구를 두고,
// 저장 직후 잠깐만 "저장됨 ✓"로 바꿔 저장됐음을 알려준다(줄 높이는 그대로).
func settingsSaveHint(gtx C, th *material.Theme, saved bool) D {
	txt, col := "변경 사항은 자동으로 저장돼요", colSub
	if saved {
		txt, col = "저장됨 ✓", colPrimary
	}
	l := material.Label(th, unit.Sp(11), txt)
	l.Color = col
	l.Alignment = text.Middle
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(4)}.Layout(gtx, l.Layout)
}

func statusPill(gtx C, th *material.Theme, active bool) D {
	txt, dot := "정지됨", colSub
	bg := color.NRGBA{R: 0x24, G: 0x1F, B: 0x16, A: 0xFF}
	if active {
		txt, dot = "실행 중", colGreen
		bg = color.NRGBA{R: 0x18, G: 0x2A, B: 0x1E, A: 0xFF}
	}
	return layout.Stack{}.Layout(gtx,
		layout.Expanded(func(gtx C) D {
			fillRRect(gtx, bg, gtx.Constraints.Min.Y/2)
			return D{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx C) D {
			return layout.Inset{Top: unit.Dp(7), Bottom: unit.Dp(7), Left: unit.Dp(11), Right: unit.Dp(11)}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						d := 8
						r := clip.Ellipse{Max: image.Pt(d, d)}
						paint.FillShape(gtx.Ops, dot, r.Op(gtx.Ops))
						return D{Size: image.Pt(d, d)}
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(func(gtx C) D {
						l := material.Label(th, unit.Sp(13), txt)
						l.Color = colText
						l.Font.Weight = 600
						return l.Layout(gtx)
					}),
				)
			})
		}),
	)
}

// simpleToggleRow: 스위치 + 라벨만 있는 단순 옵션 행(상태 표시 없음).
// settingRow: 저쪽 참고 앱처럼 굵은 제목 + 작은 설명 + 우측 컨트롤 한 줄 구성.
// 사이드바 대신 이 패턴만 우리 규모(좁은 창, 적은 항목 수)에 맞게 채택한다.
func settingRow(gtx C, th *material.Theme, title, desc string, control layout.Widget) D {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					l := material.Label(th, unit.Sp(12), title)
					l.Color = colText
					l.Font.Weight = 600
					return l.Layout(gtx)
				}),
				layout.Rigid(func(gtx C) D {
					if desc == "" {
						return D{}
					}
					l := material.Label(th, unit.Sp(10), desc)
					l.Color = colSub
					return layout.Inset{Top: unit.Dp(1)}.Layout(gtx, l.Layout)
				}),
			)
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
		layout.Rigid(control),
	)
}

// switchControl: 작고 심플한 커스텀 스위치. 호버 시 트랙이 살짝 밝아진다.
// switchAnim: 스위치 하나의 썸(동그라미) 위치를 0(꺼짐)~1(켜짐) 사이로 애니메이션한다.
type switchAnim struct {
	pos    float32
	target bool
	from   float32
	start  time.Time
	active bool
}

const switchAnimDur = 140 * time.Millisecond

func newSwitchAnim(v bool) *switchAnim {
	p := float32(0)
	if v {
		p = 1
	}
	return &switchAnim{pos: p, target: v}
}

// tick: 매 프레임 호출한다. sw.Value가 바뀌면 애니메이션을 새로 시작하고, 진행 중이면
// 진행률을 갱신한다. 다음 프레임이 더 필요하면(=아직 애니메이션 중이면) true를 반환한다.
func (a *switchAnim) tick(sw *widget.Bool, now time.Time) bool {
	if sw.Value != a.target {
		a.from = a.pos
		a.target = sw.Value
		a.start = now
		a.active = true
	}
	if !a.active {
		return false
	}
	d := float32(now.Sub(a.start)) / float32(switchAnimDur)
	if d >= 1 {
		d = 1
		a.active = false
	}
	to := float32(0)
	if a.target {
		to = 1
	}
	a.pos = a.from + (to-a.from)*easeOutCubic(d)
	return a.active
}

// switchControl: 작고 심플한 커스텀 스위치. 호버 시 트랙이 살짝 밝아지고,
// pos(0~1)로 썸 위치·트랙 색을 부드럽게 움직인다(즉시 뚝 바뀌지 않는다).
func switchControl(th *material.Theme, sw *widget.Bool, pos float32) layout.Widget {
	return func(gtx C) D {
		trackW := gtx.Dp(unit.Dp(30))
		trackH := gtx.Dp(unit.Dp(15))
		thumbD := gtx.Dp(unit.Dp(11))
		pad := (trackH - thumbD) / 2

		return sw.Layout(gtx, func(gtx C) D {
			gtx.Constraints = layout.Exact(image.Pt(trackW, trackH))

			trackCol := lerpColor(colFieldBorder, colPrimary, pos)
			if sw.Hovered() {
				trackCol = lerpColor(trackCol, colText, 0.18)
			}
			fillRRect(gtx, trackCol, trackH/2)

			travel := trackW - thumbD - 2*pad
			thumbX := pad + int(pos*float32(travel))
			r := clip.Ellipse{Min: image.Pt(thumbX, pad), Max: image.Pt(thumbX+thumbD, pad+thumbD)}
			paint.FillShape(gtx.Ops, colBg, r.Op(gtx.Ops))

			return D{Size: image.Pt(trackW, trackH)}
		})
	}
}

func simpleToggleRow(gtx C, th *material.Theme, sw *widget.Bool, pos float32, title, desc string) D {
	return settingRow(gtx, th, title, desc, switchControl(th, sw, pos))
}

// hotkeyRow: 매크로 on/off 단축키(항상 Ctrl+지정키) 표시/재지정 행.
func hotkeyRow(gtx C, th *material.Theme, btn *widget.Clickable, name string, active bool) D {
	control := func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				l := material.Label(th, unit.Sp(12), "Ctrl +")
				l.Color = colSub
				return layout.Inset{Right: unit.Dp(6)}.Layout(gtx, l.Layout)
			}),
			layout.Rigid(func(gtx C) D {
				return keyPicker(gtx, th, "키 선택", btn, name, active, false)
			}),
		)
	}
	return settingRow(gtx, th, "매크로 on/off 단축키", "Ctrl과 함께 눌러 어디서든 켜고 꺼요", control)
}

// focusGuardRow: 실시간 포커스 상태는 상태 페이지에서만 보여준다(여기선 항상
// 고정된 기능 설명만 표시해 혼동을 줄인다).
func focusGuardRow(gtx C, th *material.Theme, sw *widget.Bool, pos float32, gameFocused bool) D {
	return settingRow(gtx, th, "마비노기에서만 작동",
		"다른 프로그램에서는 설정한 키가 매크로 없이 평소처럼 입력돼요",
		switchControl(th, sw, pos))
}

// 표(row) 구조 상수 — 헤더와 각 행의 열 폭을 맞추기 위해 공유한다.
const (
	numColDp = 40 // 주기/유지 숫자칸 폭
	delColDp = 22 // 삭제(×) 열 폭 — 너무 좁으면 ✕가 거의 안 보여서 22dp로 확보
)

// slotsPanel: 모든 슬롯을 카드 하나에 표처럼 나열(행당 한 줄 + 구분선).
func slotsPanel(gtx C, th *material.Theme, slots []*slotState, ab bindRef, locked bool) D {
	return card(gtx, func(gtx C) D {
		children := make([]layout.FlexChild, 0, len(slots)*2+3)
		children = append(children,
			layout.Rigid(func(gtx C) D {
				l := material.Label(th, unit.Sp(13), "매크로 키")
				l.Color = colText
				l.Font.Weight = 600
				return l.Layout(gtx)
			}),
			layout.Rigid(func(gtx C) D {
				l := material.Label(th, unit.Sp(10), "누르는 키를 누르고 있으면 발동 키가 반복 입력돼요 (Shift·Ctrl·Alt 조합 가능)")
				l.Color = colSub
				return layout.Inset{Top: unit.Dp(1), Bottom: unit.Dp(7)}.Layout(gtx, l.Layout)
			}),
			layout.Rigid(func(gtx C) D {
				return slotHeader(gtx, th)
			}),
		)
		for i := range slots {
			i := i
			children = append(children,
				layout.Rigid(divider),
				layout.Rigid(func(gtx C) D {
					return slotRow(gtx, th, i, len(slots), slots[i], ab, locked)
				}),
			)
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

// slotHeader: 열 제목. 숫자 열 폭을 행과 동일하게 잡아 정렬을 맞춘다.
func slotHeader(gtx C, th *material.Theme) D {
	cap := func(txt string) layout.Widget {
		return func(gtx C) D {
			l := material.Label(th, unit.Sp(10), txt)
			l.Color = colSub
			l.Font.Weight = 600
			l.Alignment = text.Middle
			return l.Layout(gtx)
		}
	}
	return layout.Inset{Bottom: unit.Dp(5)}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx C) D {
				l := material.Label(th, unit.Sp(10), "누르는 키  →  발동 키")
				l.Color = colSub
				l.Font.Weight = 600
				return layout.Inset{Left: unit.Dp(2)}.Layout(gtx, l.Layout)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx C) D { return fixedW(gtx, numColDp, cap("주기")) }),
			layout.Rigid(layout.Spacer{Width: unit.Dp(5)}.Layout),
			layout.Rigid(func(gtx C) D { return fixedW(gtx, numColDp, cap("유지")) }),
			layout.Rigid(layout.Spacer{Width: unit.Dp(3)}.Layout),
			layout.Rigid(func(gtx C) D { return fixedW(gtx, delColDp, func(gtx C) D { return D{} }) }),
		)
	})
}

// slotRow: 슬롯 한 줄. [번호] [트리거] → [발동] [주기] [유지] [×]
func slotRow(gtx C, th *material.Theme, idx, total int, s *slotState, ab bindRef, locked bool) D {
	return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						l := material.Label(th, unit.Sp(12), strconv.Itoa(idx+1))
						l.Color = colSub
						l.Font.Weight = 600
						return layout.Inset{Right: unit.Dp(5)}.Layout(gtx, l.Layout)
					}),
					layout.Flexed(1, func(gtx C) D {
						active := ab.kind == bindTrigger && ab.slot == idx
						return keyPicker(gtx, th, "누르는 키", &s.trigBtn, s.trigName, active, locked)
					}),
					layout.Rigid(func(gtx C) D {
						return layout.Inset{Left: unit.Dp(3), Right: unit.Dp(3)}.Layout(gtx, func(gtx C) D {
							l := material.Label(th, unit.Sp(13), "→")
							l.Color = colSub
							return l.Layout(gtx)
						})
					}),
					layout.Flexed(1, func(gtx C) D {
						active := ab.kind == bindTarget && ab.slot == idx
						return keyPicker(gtx, th, "발동 키", &s.targBtn, s.targName, active, locked)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx C) D { return fixedW(gtx, numColDp, func(gtx C) D { return numCompact(gtx, th, &s.delay) }) }),
			layout.Rigid(layout.Spacer{Width: unit.Dp(5)}.Layout),
			layout.Rigid(func(gtx C) D { return fixedW(gtx, numColDp, func(gtx C) D { return numCompact(gtx, th, &s.hold) }) }),
			layout.Rigid(layout.Spacer{Width: unit.Dp(3)}.Layout),
			layout.Rigid(func(gtx C) D {
				return fixedW(gtx, delColDp, func(gtx C) D {
					if locked || total <= 1 {
						return D{Size: image.Pt(gtx.Constraints.Min.X, 0)}
					}
					b := material.Button(th, &s.delBtn, "✕")
					b.Background = color.NRGBA{}
					b.Color = colRed
					b.TextSize = unit.Sp(13)
					b.Font.Weight = 600
					b.Inset = layout.UniformInset(unit.Dp(4))
					return b.Layout(gtx)
				})
			}),
		)
	})
}

// numCompact: 라벨·단위 없는 좁은 숫자 입력칸(테두리). 폭은 호출측에서 고정.
func numCompact(gtx C, th *material.Theme, ed *widget.Editor) D {
	return widget.Border{Color: colFieldBorder, Width: unit.Dp(1), CornerRadius: unit.Dp(8)}.Layout(gtx, func(gtx C) D {
		return layout.Stack{}.Layout(gtx,
			layout.Expanded(func(gtx C) D {
				fillRRect(gtx, colFieldBg, 8)
				return D{Size: gtx.Constraints.Min}
			}),
			layout.Stacked(func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(5), Right: unit.Dp(5)}.Layout(gtx, func(gtx C) D {
					ed.Alignment = text.Middle
					e := material.Editor(th, ed, "0")
					e.TextSize = unit.Sp(13)
					e.Color = colText
					if ed.ReadOnly {
						e.Color = colSub
					}
					return e.Layout(gtx)
				})
			}),
		)
	})
}

// fixedW: 자식을 지정한 Dp 폭으로 고정해 배치한다.
func fixedW(gtx C, dp int, w layout.Widget) D {
	px := gtx.Dp(unit.Dp(dp))
	gtx.Constraints.Min.X = px
	gtx.Constraints.Max.X = px
	return w(gtx)
}

// divider: 슬롯 사이 얇은 구분선.
func divider(gtx C) D {
	h := gtx.Dp(unit.Dp(1))
	sz := image.Pt(gtx.Constraints.Max.X, h)
	defer clip.Rect{Max: sz}.Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, colCardBorder)
	return D{Size: sz}
}

func autoCard(gtx C, th *material.Theme, trig, clear *widget.Clickable,
	delay, hold *widget.Editor, trigName string, ab bindRef, locked bool) D {
	return card(gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				l := material.Label(th, unit.Sp(13), "오토 좌클릭")
				l.Color = colText
				l.Font.Weight = 600
				return l.Layout(gtx)
			}),
			layout.Rigid(func(gtx C) D {
				l := material.Label(th, unit.Sp(10), "트리거 키를 누르면 켜고 꺼지는 토글이에요 (켜지면 1초 후 시작)")
				l.Color = colSub
				return layout.Inset{Top: unit.Dp(1), Bottom: unit.Dp(6)}.Layout(gtx, l.Layout)
			}),
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx C) D {
						active := ab.kind == bindAuto
						return keyPicker(gtx, th, "트리거 키", trig, trigName, active, locked)
					}),
					layout.Rigid(func(gtx C) D {
						if locked {
							return D{}
						}
						return layout.Inset{Left: unit.Dp(3)}.Layout(gtx, func(gtx C) D {
							return textButton(gtx, th, clear, "지우기", colSub)
						})
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx C) D { return numField(gtx, th, "주기", delay) }),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Flexed(1, func(gtx C) D { return numField(gtx, th, "유지", hold) }),
				)
			}),
		)
	})
}

func addSlotButton(gtx C, th *material.Theme, btn *widget.Clickable, count int) D {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return outlineBtn(gtx, th, btn, "＋ 슬롯 추가", colCardBorder, colPrimary, unit.Sp(12), true, unit.Dp(9), unit.Dp(1), unit.Dp(8))
}

// keyPicker: 키 선택 버튼 (라벨 없이 한 줄). 미설정 시 placeholder로 용도를 안내.
func keyPicker(gtx C, th *material.Theme, placeholder string, btn *widget.Clickable, name string, active, locked bool) D {
	switch {
	case active:
		// 입력 대기 = 앰버 솔리드 (주의 환기, 잠깐 상태)
		b := material.Button(th, btn, "키 입력… (Esc 취소)")
		b.Background, b.Color = colAmber, colOnGold
		b.CornerRadius = unit.Dp(8)
		b.TextSize = unit.Sp(13)
		b.Font.Weight = 600
		b.Inset = layout.UniformInset(unit.Dp(6))
		return b.Layout(gtx)
	case name == "":
		// 미설정 = 은은한 아웃라인
		return outlineBtn(gtx, th, btn, placeholder, colFieldBorder, colSub, unit.Sp(12), false, unit.Dp(7), unit.Dp(1), unit.Dp(6))
	default:
		// 설정됨 = 골드 아웃라인
		return outlineBtn(gtx, th, btn, name, colPrimary, colPrimary, unit.Sp(12), true, unit.Dp(7), unit.Dp(1.5), unit.Dp(6))
	}
}

// numField: 왼쪽 인라인 라벨 + 숫자 입력 박스(테두리 + ms). 한 줄로 컴팩트.
func numField(gtx C, th *material.Theme, label string, ed *widget.Editor) D {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			l := material.Label(th, unit.Sp(11), label)
			l.Color = colSub
			l.Font.Weight = 600
			return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, l.Layout)
		}),
		layout.Flexed(1, func(gtx C) D {
			return widget.Border{Color: colFieldBorder, Width: unit.Dp(1), CornerRadius: unit.Dp(7)}.Layout(gtx, func(gtx C) D {
				return layout.Stack{}.Layout(gtx,
					layout.Expanded(func(gtx C) D {
						fillRRect(gtx, colFieldBg, 7)
						return D{Size: gtx.Constraints.Min}
					}),
					layout.Stacked(func(gtx C) D {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						return layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(7), Right: unit.Dp(7)}.Layout(gtx, func(gtx C) D {
							return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
								layout.Flexed(1, func(gtx C) D {
									e := material.Editor(th, ed, "0")
									e.TextSize = unit.Sp(13)
									e.Color = colText
									if ed.ReadOnly {
										e.Color = colSub
									}
									return e.Layout(gtx)
								}),
								layout.Rigid(func(gtx C) D {
									l := material.Label(th, unit.Sp(11), "ms")
									l.Color = colSub
									return l.Layout(gtx)
								}),
							)
						})
					}),
				)
			})
		}),
	)
}

// ---------- 공통 헬퍼 ----------

func card(gtx C, content layout.Widget) D {
	return widget.Border{Color: colCardBorder, Width: unit.Dp(1), CornerRadius: unit.Dp(14)}.Layout(gtx, func(gtx C) D {
		return layout.Stack{}.Layout(gtx,
			layout.Expanded(func(gtx C) D {
				fillRRect(gtx, colCard, 14)
				return D{Size: gtx.Constraints.Min}
			}),
			layout.Stacked(func(gtx C) D {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return layout.UniformInset(unit.Dp(8)).Layout(gtx, content)
			}),
		)
	})
}

func fillRRect(gtx C, col color.NRGBA, radius int) {
	r := clip.RRect{
		Rect: image.Rectangle{Max: gtx.Constraints.Min},
		NW:   radius, NE: radius, SW: radius, SE: radius,
	}
	paint.FillShape(gtx.Ops, col, r.Op(gtx.Ops))
}

func textButton(gtx C, th *material.Theme, btn *widget.Clickable, label string, col color.NRGBA) D {
	b := material.Button(th, btn, label)
	b.CornerRadius = unit.Dp(8)
	b.TextSize = unit.Sp(13)
	b.Font.Weight = 600
	b.Inset = layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(9), Right: unit.Dp(9)}
	b.Background = color.NRGBA{}
	b.Color = col
	return b.Layout(gtx)
}

// outlineBtn: 배경 없이 테두리만 있는 아웃라인 버튼(고급 톤). 테두리/글자 색 분리 지정.
func outlineBtn(gtx C, th *material.Theme, btn *widget.Clickable, label string,
	borderCol, textCol color.NRGBA, size unit.Sp, bold bool, radius, borderW, inset unit.Dp) D {
	return widget.Border{Color: borderCol, Width: borderW, CornerRadius: radius}.Layout(gtx, func(gtx C) D {
		b := material.Button(th, btn, label)
		b.Background = color.NRGBA{} // 투명
		b.Color = textCol
		b.TextSize = size
		if bold {
			b.Font.Weight = 600
		}
		b.CornerRadius = radius
		b.Inset = layout.UniformInset(inset)
		return b.Layout(gtx)
	})
}

func atoiDef(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
