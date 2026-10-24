package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

// 크래시 로그.
//
// 이 앱은 콘솔 없는 GUI(-H windowsgui)라 오류가 사용자에게 전혀 안 보인다. 훅·워커·
// 포커스 감시에서 패닉이 나면 조용히 멈추거나 꺼지는데 원인을 알 방법이 없어서,
// 설정 폴더(%LOCALAPPDATA%\MabinogiMacro)의 crash.log에 시각·버전·스택을 남긴다.
//
//   - recover로 잡을 수 있는 패닉: 각 고루틴/훅 콜백에서 잡아 기록하고 앱은 계속 돈다.
//   - 못 잡는 치명적 런타임 오류(동시 맵 접근 등): debug.SetCrashOutput으로 같은 파일에 남긴다.

const (
	crashLogMaxBytes = 512 * 1024 // 이 크기를 넘으면 시작 시 crash.log.old로 교체
	crashLogMaxItems = 50         // 같은 실행에서 무한 반복되는 패닉이 로그를 도배하지 않게 상한
)

var (
	crashLogMu    sync.Mutex
	crashLogFile  *os.File
	crashLogCount int
)

func crashLogPath() string {
	return filepath.Join(filepath.Dir(configFilePath), "crash.log")
}

// initCrashLog: LoadConfig 이후(설정 폴더가 정해진 뒤) 한 번 호출한다.
func initCrashLog() {
	path := crashLogPath()
	if st, err := os.Stat(path); err == nil && st.Size() > crashLogMaxBytes {
		os.Remove(path + ".old")
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return // 로그를 못 남겨도 앱 동작에는 영향이 없어야 한다
	}
	crashLogFile = f
	debug.SetCrashOutput(f, debug.CrashOptions{})
}

func writeLogEntry(kind, where string, detail any, stack []byte) {
	crashLogMu.Lock()
	defer crashLogMu.Unlock()
	if crashLogFile == nil || crashLogCount >= crashLogMaxItems {
		return
	}
	crashLogCount++
	fmt.Fprintf(crashLogFile, "==== %s | %s | %s | %s/%s %s ====\n",
		time.Now().Format("2006-01-02 15:04:05"), kind, windowTitle,
		runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Fprintf(crashLogFile, "where: %s\n%v\n", where, detail)
	if len(stack) > 0 {
		crashLogFile.Write(stack)
		if stack[len(stack)-1] != '\n' {
			crashLogFile.WriteString("\n")
		}
	}
	crashLogFile.WriteString("\n")
	crashLogFile.Sync()
}

// logCrash: 복구한 패닉을 기록한다(호출한 곳의 스택 포함).
func logCrash(where string, r any) {
	writeLogEntry("PANIC", where, r, debug.Stack())
}

// logEvent: 패닉은 아니지만 남겨 둘 만한 오류(훅 설치 실패 등).
func logEvent(where string, detail any) {
	writeLogEntry("ERROR", where, detail, nil)
}

// recoverPanic: `defer recoverPanic("이름")` 형태로 쓴다. 패닉을 기록하고 삼킨다.
func recoverPanic(where string) {
	if r := recover(); r != nil {
		logCrash(where, r)
	}
}

// safeGo: 패닉이 나도 프로세스가 죽지 않고 기록만 남기는 고루틴.
func safeGo(where string, fn func()) {
	go func() {
		defer recoverPanic(where)
		fn()
	}()
}
