package main

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

// 자동 실행은 별도 config.json 필드 없이 레지스트리 값 존재 여부를
// 그대로 진실의 원천으로 삼는다(재설치·경로 변경 시에도 항상 일관됨).
const autoStartRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const autoStartValueName = "MabinogiMacro"

// IsAutoStartEnabled: 현재 실행 파일 경로로 등록된 자동 실행 항목이 있는지 확인한다.
func IsAutoStartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autoStartRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()

	val, _, err := k.GetStringValue(autoStartValueName)
	if err != nil {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return val == autoStartCommand(exe)
}

// SetAutoStart: 자동 실행을 켜거나 끈다.
func SetAutoStart(enable bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autoStartRunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	if !enable {
		if err := k.DeleteValue(autoStartValueName); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(autoStartValueName, autoStartCommand(exe))
}

func autoStartCommand(exePath string) string {
	return `"` + exePath + `"`
}
