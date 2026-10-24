package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// DefaultDownMs: 키를 눌러 유지하는 기본 시간(ms). 연타 주기(DelayMs)와 분리된 값.
const DefaultDownMs = 15

// 슬롯 개수 관련 상수
const (
	DefaultSlotCount = 3
	MaxSlots         = 10
)

type MacroSlot struct {
	TriggerVK uint32 `json:"trigger_vk"`
	// TriggerMods: 트리거에 함께 눌러야 하는 조합키(ModShift|ModCtrl|ModAlt). 0이면 단일 키.
	// 구버전 config에는 없으므로 omitempty — 없으면 0(단일 키)으로 읽힌다.
	TriggerMods uint32 `json:"trigger_mods,omitempty"`
	TriggerName string `json:"trigger_name"`
	TargetVK    uint32 `json:"target_vk"`
	TargetName  string `json:"target_name"`
	DelayMs     int    `json:"delay_ms"` // 한 사이클 전체 주기(down+up)
	DownMs      int    `json:"down_ms"`  // 그 중 키를 눌러 유지하는 시간
}

type AutoClickerConfig struct {
	TriggerVK   uint32 `json:"trigger_vk"`
	TriggerName string `json:"trigger_name"`
	DelayMs     int    `json:"delay_ms"`
	DownMs      int    `json:"down_ms"`
}

// DefaultToggleHotkeyVK: 매크로 on/off 단축키 기본값(F12). 항상 Ctrl과 함께 눌러야 한다.
const DefaultToggleHotkeyVK = 0x7B // VK_F12

type Config struct {
	Slots              []MacroSlot       `json:"slots"`
	AutoClicker        AutoClickerConfig `json:"auto_clicker"`
	FocusGuardEnabled  bool              `json:"focus_guard_enabled"`
	ChatGuardEnabled   bool              `json:"chat_guard_enabled"`
	UpdateCheckEnabled bool              `json:"update_check_enabled"`
	ToggleHotkeyVK     uint32            `json:"toggle_hotkey_vk"`
	ToggleHotkeyName   string            `json:"toggle_hotkey_name"`
	IsActive           bool              `json:"-"` // UI 동작 여부는 저장하지 않음
	mu                 sync.RWMutex
}

var AppConfig *Config
var configFilePath string

// trigKey: 트리거 식별자 = 키(VK) + 조합키 마스크. R과 Shift+R은 서로 다른 트리거다.
type trigKey struct {
	VK   uint32
	Mods uint32
}

var (
	ActiveTriggers   map[trigKey]MacroSlot
	ActiveTriggersMu sync.RWMutex
)

func init() {
	// C 드라이브 AppData/Local 경로 탐색
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		localAppData = os.TempDir()
	}
	appDir := filepath.Join(localAppData, "MabinogiMacro")
	// 폴더가 없으면 생성
	os.MkdirAll(appDir, 0755)
	configFilePath = filepath.Join(appDir, "config.json")
}

// normalizeDownMs: down 값을 유효 범위로 보정한다.
// 0 이하(미설정/구버전 config)면 기본값, 최소 1, 주기보다 크면 주기-1로 클램프한다.
func normalizeDownMs(down int, delay int) int {
	if down <= 0 {
		down = DefaultDownMs
	}
	if down < 1 {
		down = 1
	}
	if down >= delay {
		down = delay - 1
		if down < 1 {
			down = 1
		}
	}
	return down
}

func defaultSlots(n int) []MacroSlot {
	s := make([]MacroSlot, n)
	for i := range s {
		s[i].DelayMs = 100
		s[i].DownMs = DefaultDownMs
	}
	return s
}

func LoadConfig() {
	AppConfig = &Config{
		Slots:              defaultSlots(DefaultSlotCount),
		FocusGuardEnabled:  true, // 기본값: 마비노기 포커스 시에만 작동
		ChatGuardEnabled:   true, // 기본값: 채팅 중(Enter 감지)에는 트리거 일시정지
		UpdateCheckEnabled: true, // 기본값: 시작 시 새 버전 확인
		ToggleHotkeyVK:     DefaultToggleHotkeyVK,
		ToggleHotkeyName:   "F12",
	}
	AppConfig.AutoClicker.DelayMs = 100
	AppConfig.AutoClicker.DownMs = DefaultDownMs

	if data := readConfigFile(); data != nil {
		var loaded Config
		if err := json.Unmarshal(data, &loaded); err == nil {
			// 저장된 슬롯 개수를 그대로 복원 (1..MaxSlots로 클램프)
			n := len(loaded.Slots)
			if n > 0 {
				if n > MaxSlots {
					n = MaxSlots
				}
				slots := make([]MacroSlot, n)
				for i := 0; i < n; i++ {
					slots[i].TriggerVK = loaded.Slots[i].TriggerVK
					slots[i].TriggerMods = loaded.Slots[i].TriggerMods
					slots[i].TriggerName = loaded.Slots[i].TriggerName
					slots[i].TargetVK = loaded.Slots[i].TargetVK
					slots[i].TargetName = loaded.Slots[i].TargetName
					if loaded.Slots[i].DelayMs < 10 {
						slots[i].DelayMs = 10
					} else {
						slots[i].DelayMs = loaded.Slots[i].DelayMs
					}
					slots[i].DownMs = normalizeDownMs(loaded.Slots[i].DownMs, slots[i].DelayMs)
				}
				AppConfig.Slots = slots
			}

			if loaded.AutoClicker.DelayMs < 10 {
				AppConfig.AutoClicker.DelayMs = 100
			} else {
				AppConfig.AutoClicker.DelayMs = loaded.AutoClicker.DelayMs
			}
			AppConfig.AutoClicker.DownMs = normalizeDownMs(loaded.AutoClicker.DownMs, AppConfig.AutoClicker.DelayMs)
			AppConfig.AutoClicker.TriggerVK = loaded.AutoClicker.TriggerVK
			AppConfig.AutoClicker.TriggerName = loaded.AutoClicker.TriggerName

			// focus_guard_enabled는 구버전 config.json에 없을 수 있으므로,
			// 키가 아예 없을 때만 기본값(true)을 유지하고 명시된 값이 있으면 그대로 따른다.
			var probe struct {
				FocusGuardEnabled  *bool `json:"focus_guard_enabled"`
				ChatGuardEnabled   *bool `json:"chat_guard_enabled"`
				UpdateCheckEnabled *bool `json:"update_check_enabled"`
			}
			if json.Unmarshal(data, &probe) == nil {
				if probe.FocusGuardEnabled != nil {
					AppConfig.FocusGuardEnabled = *probe.FocusGuardEnabled
				}
				if probe.ChatGuardEnabled != nil {
					AppConfig.ChatGuardEnabled = *probe.ChatGuardEnabled
				}
				if probe.UpdateCheckEnabled != nil {
					AppConfig.UpdateCheckEnabled = *probe.UpdateCheckEnabled
				}
			}

			// toggle_hotkey_vk가 0이면(구버전 config 또는 명시적 해제) 기본값 유지.
			if loaded.ToggleHotkeyVK != 0 {
				AppConfig.ToggleHotkeyVK = loaded.ToggleHotkeyVK
				AppConfig.ToggleHotkeyName = loaded.ToggleHotkeyName
			}
		}
	}
}

// readConfigFile: 올바른 JSON인 설정 파일 내용을 돌려준다. 본 파일이 없거나 깨졌으면
// 저장할 때마다 남겨 둔 직전 정상본(.bak)으로 대신한다. 둘 다 못 쓰면 nil.
func readConfigFile() []byte {
	for _, p := range []string{configFilePath, configFilePath + ".bak"} {
		if data, err := os.ReadFile(p); err == nil && json.Valid(data) {
			return data
		}
	}
	return nil
}

func SaveConfig() {
	// 락은 직렬화하는 동안만 잡는다(디스크 쓰기 동안 훅 스레드의 읽기를 막지 않도록).
	AppConfig.mu.RLock()
	data, err := json.MarshalIndent(AppConfig, "", "  ")
	AppConfig.mu.RUnlock()
	if err != nil {
		return
	}
	writeFileAtomic(configFilePath, data)
}

// writeFileAtomic: 임시 파일에 다 쓰고 디스크에 flush한 뒤 이름을 바꿔 덮어쓴다.
// 쓰는 도중에 꺼져도 본 파일은 이전 내용 그대로이거나 새 내용 전체 둘 중 하나다
// (예전처럼 바로 덮어쓰면 중간에 끊긴 반쪽짜리 파일이 남아 설정이 통째로 날아갔다).
// 교체 직전의 정상 파일은 .bak으로 한 부 남겨 둔다.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	if old, err := os.ReadFile(path); err == nil && json.Valid(old) {
		os.WriteFile(path+".bak", old, 0644) // 최선 노력: 실패해도 저장은 계속한다
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (c *Config) SetActive(active bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.IsActive = active

	ActiveTriggersMu.Lock()
	defer ActiveTriggersMu.Unlock()
	if active {
		ActiveTriggers = make(map[trigKey]MacroSlot)
		for _, s := range c.Slots {
			if s.TriggerVK != 0 && s.TargetVK != 0 {
				k := trigKey{s.TriggerVK, s.TriggerMods}
				if existing, ok := ActiveTriggers[k]; !ok || s.DelayMs < existing.DelayMs {
					ActiveTriggers[k] = s
				}
			}
		}
	} else {
		ActiveTriggers = nil
	}
}

func (c *Config) GetActive() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IsActive
}

// ReplaceSlots는 전체 슬롯 배열을 교체한다(개수 가변). 값은 유효 범위로 보정한다.
func (c *Config) ReplaceSlots(slots []MacroSlot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]MacroSlot, len(slots))
	for i, s := range slots {
		if s.DelayMs < 10 {
			s.DelayMs = 10
		}
		s.DownMs = normalizeDownMs(s.DownMs, s.DelayMs)
		out[i] = s
	}
	c.Slots = out
}

func (c *Config) GetSlots() []MacroSlot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	res := make([]MacroSlot, len(c.Slots))
	copy(res, c.Slots)
	return res
}

func (c *Config) UpdateAutoClicker(triggerVK uint32, triggerName string, delay int, down int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if delay < 10 {
		delay = 10
	}
	c.AutoClicker.TriggerVK = triggerVK
	c.AutoClicker.TriggerName = triggerName
	c.AutoClicker.DelayMs = delay
	c.AutoClicker.DownMs = normalizeDownMs(down, delay)
}

func (c *Config) GetAutoClicker() AutoClickerConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AutoClicker
}

// SetFocusGuard: 마비노기가 포커스일 때만 매크로가 작동하도록 하는 옵션을 켜고 끈다.
func (c *Config) SetFocusGuard(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.FocusGuardEnabled = enabled
}

func (c *Config) GetFocusGuard() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.FocusGuardEnabled
}

// SetChatGuard: 채팅 중(Enter 감지)에는 매크로 트리거를 일시정지하는 옵션을 켜고 끈다.
func (c *Config) SetChatGuard(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ChatGuardEnabled = enabled
}

// SetUpdateCheck: 시작 시 새 버전 확인(알림)을 켜고 끈다.
func (c *Config) SetUpdateCheck(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.UpdateCheckEnabled = enabled
}

func (c *Config) GetUpdateCheck() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.UpdateCheckEnabled
}

func (c *Config) GetChatGuard() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ChatGuardEnabled
}

// SetToggleHotkey: 매크로 on/off 단축키를 바꾼다(항상 Ctrl과 함께 눌러야 함).
func (c *Config) SetToggleHotkey(vk uint32, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ToggleHotkeyVK = vk
	c.ToggleHotkeyName = name
}

func (c *Config) GetToggleHotkey() (uint32, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ToggleHotkeyVK, c.ToggleHotkeyName
}
