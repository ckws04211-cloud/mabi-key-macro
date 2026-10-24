package main

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

// 윈도우 시작 시 자동 실행.
//
// 이 앱은 관리자 권한(requireAdministrator)이 필요한데, 레지스트리 Run 키로 등록한
// 프로그램은 UAC 때문에 로그온 시 실행이 막힌다. 그래서 작업 스케줄러에 "로그온 시,
// 가장 높은 권한으로 실행" 작업을 등록하는 방식을 쓴다(UAC 창 없이 관리자로 시작됨).
//
// 파일명에 버전이 들어가서(예: "... - v13.0.0.exe") 새 버전으로 바꾸면 등록된 경로가
// 사라질 수 있다. 등록한 경로를 HKCU\Software\MabinogiKeyDown에 따로 적어 두었다가,
// 그 파일이 없어졌으면 지금 실행 중인 exe로 다시 등록한다(MaintainAutoStart).

var (
	autoStartTaskName = "MabinogiKeyDown"
	// 테스트에서 일반 권한으로도 등록/삭제를 검증할 수 있게 바꿀 수 있도록 변수로 둔다.
	autoStartRunLevel = "HighestAvailable"
)

const (
	// 예전 버전이 쓰던 Run 키 항목. 관리자 권한 앱에는 동작하지 않으므로 발견하면 이전한다.
	legacyRunKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	legacyRunValue = "MabinogiMacro"

	appRegKey          = `Software\MabinogiKeyDown`
	appRegAutoStartExe = "AutoStartExe"

	createNoWindow = 0x08000000
)

// schtasks: 콘솔 창이 번쩍이지 않게 숨겨서 실행한다.
func schtasks(args ...string) ([]byte, error) {
	cmd := exec.Command("schtasks", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd.CombinedOutput()
}

func autoStartTaskExists() bool {
	_, err := schtasks("/Query", "/TN", autoStartTaskName)
	return err == nil
}

func registeredAutoStartExe() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, appRegKey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(appRegAutoStartExe)
	if err != nil {
		return ""
	}
	return v
}

func setRegisteredAutoStartExe(path string) {
	if path == "" {
		if k, err := registry.OpenKey(registry.CURRENT_USER, appRegKey, registry.SET_VALUE); err == nil {
			k.DeleteValue(appRegAutoStartExe)
			k.Close()
		}
		return
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, appRegKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	k.SetStringValue(appRegAutoStartExe, path)
}

func legacyRunEntryExists() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, legacyRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(legacyRunValue)
	return err == nil
}

func deleteLegacyRunEntry() {
	k, err := registry.OpenKey(registry.CURRENT_USER, legacyRunKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	k.DeleteValue(legacyRunValue)
}

// IsAutoStartEnabled: 자동 실행 작업이 등록돼 있는지. 등록한 적이 없으면(대부분의
// 사용자) schtasks를 띄우지 않고 바로 false를 돌려 시작 속도에 영향을 주지 않는다.
func IsAutoStartEnabled() bool {
	if registeredAutoStartExe() == "" {
		return false
	}
	return autoStartTaskExists()
}

// SetAutoStart: 자동 실행을 켜거나 끈다.
func SetAutoStart(enable bool) error {
	if !enable {
		if autoStartTaskExists() {
			if out, err := schtasks("/Delete", "/TN", autoStartTaskName, "/F"); err != nil {
				return fmt.Errorf("작업 삭제 실패: %v (%s)", err, bytes.TrimSpace(out))
			}
		}
		deleteLegacyRunEntry()
		setRegisteredAutoStartExe("")
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := createAutoStartTask(exe); err != nil {
		return err
	}
	deleteLegacyRunEntry()
	setRegisteredAutoStartExe(exe)
	return nil
}

// MaintainAutoStart: 시작할 때 한 번 호출해 자동 실행 등록을 최신 상태로 맞춘다.
//   - 예전 Run 키 항목이 있으면 작업 스케줄러 방식으로 옮긴다.
//   - 등록해 둔 exe 파일이 없어졌다면(버전 교체 등) 지금 exe로 다시 등록한다.
func MaintainAutoStart() {
	if legacyRunEntryExists() {
		SetAutoStart(true)
		return
	}
	reg := registeredAutoStartExe()
	if reg == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil || reg == exe {
		return
	}
	if _, statErr := os.Stat(reg); os.IsNotExist(statErr) {
		SetAutoStart(true)
	}
}

// createAutoStartTask: 작업 XML을 만들어 schtasks /XML 로 등록한다. 한글 경로가 깨지지
// 않도록 XML은 UTF-16(LE, BOM 포함)으로 임시 파일에 써서 넘긴다.
func createAutoStartTask(exe string) error {
	userName := ""
	if u, err := user.Current(); err == nil {
		userName = u.Username // "도메인\사용자"
	}
	data := encodeUTF16LEWithBOM(buildAutoStartTaskXML(exe, userName, autoStartRunLevel))

	tmp, err := os.CreateTemp("", "mabinogi-task-*.xml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if out, err := schtasks("/Create", "/TN", autoStartTaskName, "/XML", tmpPath, "/F"); err != nil {
		return fmt.Errorf("작업 등록 실패: %v (%s)", err, bytes.TrimSpace(out))
	}
	return nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func buildAutoStartTaskXML(exe, userName, runLevel string) string {
	userXML := ""
	if userName != "" {
		userXML = "<UserId>" + xmlEscape(userName) + "</UserId>"
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>마비노기 키 다운 - 로그온 시 자동 실행</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      ` + userXML + `
      <Delay>PT5S</Delay>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      ` + userXML + `
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>` + xmlEscape(runLevel) + `</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>"` + xmlEscape(exe) + `"</Command>
      <WorkingDirectory>` + xmlEscape(filepath.Dir(exe)) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
}

func encodeUTF16LEWithBOM(s string) []byte {
	u := utf16.Encode([]rune(s))
	buf := make([]byte, 0, 2+len(u)*2)
	buf = append(buf, 0xFF, 0xFE) // BOM (little endian)
	b2 := make([]byte, 2)
	for _, c := range u {
		binary.LittleEndian.PutUint16(b2, c)
		buf = append(buf, b2...)
	}
	return buf
}
