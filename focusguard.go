package main

import (
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetForegroundWindow        = user32.NewProc("GetForegroundWindow")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
)

const processQueryLimitedInformation = 0x1000

// targetProcessNames: 마비노기 클라이언트로 인식할 실행 파일명(대소문자 무시, 정확히 일치).
// 배포/버전에 따라 실행 파일명이 다를 수 있어 후보를 여럿 두고, 아래 창 제목
// 검사(역시 정확히 일치)와 함께 이중으로 판별한다.
var targetProcessNames = []string{"mabinogi.exe", "client.exe", "mabinogiclient.exe"}

// targetWindowTitles: 창 제목이 이 목록과 "정확히" 일치할 때만 게임으로 인정한다.
// 부분 포함(Contains) 매칭은 디스코드 채널명·브라우저 탭 제목 등 "마비노기"라는
// 단어가 들어간 무관한 창까지 오판하게 만들어 일부러 쓰지 않는다.
var targetWindowTitles = []string{"마비노기", "mabinogi"}

var gameFocused atomic.Bool

// FocusStateCallback: 포커스 판정이 바뀔 때(게임⇄기타) 호출된다. UI 갱신용.
var FocusStateCallback func(bool)

// IsGameFocused는 마지막으로 감지된 "현재 포커스 창 = 마비노기" 여부를 반환한다.
func IsGameFocused() bool {
	return gameFocused.Load()
}

// StartFocusWatcher는 짧은 주기로 전경(foreground) 창을 확인해 마비노기 여부를
// gameFocused에 갱신한다. 값이 실제로 바뀔 때만 FocusStateCallback을 호출한다.
func StartFocusWatcher() {
	go func() {
		first := true
		var last bool
		for {
			// 한 번의 점검에서 패닉이 나도 감시 루프 자체가 죽지 않게 점검 단위로 복구한다
			// (감시가 멈추면 포커스 상태가 낡은 채로 남아 엉뚱한 창에서 매크로가 동작한다).
			func() {
				defer recoverPanic("포커스 감시")
				watchInputDesktop() // UAC/잠금 화면 등 보안 데스크톱 전환 시 워커 정리
				cur := checkForegroundIsGame()
				if first || cur != last {
					gameFocused.Store(cur)
					if !cur {
						ResetChatState() // 게임을 벗어나면 채팅 추정 상태도 초기화
					}
					if FocusStateCallback != nil {
						go FocusStateCallback(cur)
					}
					last, first = cur, false
				}
			}()
			time.Sleep(200 * time.Millisecond)
		}
	}()
}

// checkForegroundIsGame: 현재 전경 창이 마비노기인지 두 가지 방법으로 판별한다.
// 둘 다 "정확히 일치"만 인정한다 — 부분 포함(Contains) 매칭은 디스코드 채널명이나
// 브라우저 탭 제목처럼 "마비노기"라는 단어만 들어간 무관한 창까지 게임으로
// 오판하게 만들 수 있어 의도적으로 배제했다.
//  1. 프로세스 실행 파일명이 후보 목록과 정확히 일치하는지
//  2. (보완) 창 제목이 후보 목록과 정확히 일치하는지 — 실행 파일명이
//     배포판마다 다를 수 있어, 하나만으로는 놓칠 수 있는 경우를 보완한다.
func checkForegroundIsGame() bool {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return false
	}

	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))

	// 이 매크로 앱 자신의 창은 절대 "게임 포커스"로 오판하지 않는다.
	// (앱 창 제목에도 "마비노기"가 들어있어 제목 매칭만으로는 자기 자신과 혼동될 수 있음 —
	//  이 상태에서 설정창에 숫자를 입력할 때 매크로가 오작동하는 것을 방지)
	if pid == windows.GetCurrentProcessId() {
		return false
	}

	if pid != 0 {
		if hProc, _, _ := procOpenProcess.Call(uintptr(processQueryLimitedInformation), 0, uintptr(pid)); hProc != 0 {
			defer procCloseHandle.Call(hProc)
			buf := make([]uint16, 260)
			size := uint32(len(buf))
			if ret, _, _ := procQueryFullProcessImageNameW.Call(
				hProc, 0,
				uintptr(unsafe.Pointer(&buf[0])),
				uintptr(unsafe.Pointer(&size)),
			); ret != 0 {
				full := windows.UTF16ToString(buf[:size])
				base := full
				if idx := strings.LastIndexAny(full, `\/`); idx >= 0 {
					base = full[idx+1:]
				}
				base = strings.ToLower(base)
				for _, name := range targetProcessNames {
					if base == name {
						return true
					}
				}
			}
		}
	}

	var tbuf [256]uint16
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&tbuf[0])), uintptr(len(tbuf)))
	if n > 0 {
		title := strings.ToLower(strings.TrimSpace(windows.UTF16ToString(tbuf[:n])))
		for _, cand := range targetWindowTitles {
			if title == strings.ToLower(cand) {
				return true
			}
		}
	}

	return false
}
