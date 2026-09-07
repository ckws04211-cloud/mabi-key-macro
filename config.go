package main

import (
	"encoding/json"
	"io/ioutil"
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
	TriggerVK   uint32 `json:"trigger_vk"`
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
	Slots             []MacroSlot       `json:"slots"`
	AutoClicker       AutoClickerConfig `json:"auto_clicker"`
	FocusGuardEnabled bool              `json:"focus_guard_enabled"`
	ToggleHotkeyVK    uint32            `json:"toggle_hotkey_vk"`
	ToggleHotkeyName  string            `json:"toggle_hotkey_name"`
	IsActive          bool              `json:"-"` // UI 동작 여부는 저장하지 않음
	mu                sync.RWMutex
}

var AppConfig *Config
var configFilePath string

var (
	ActiveTriggers   map[uint32]MacroSlot
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
		Slots:             defaultSlots(DefaultSlotCount),
		FocusGuardEnabled: true, // 기본값: 마비노기 포커스 시에만 작동
		ToggleHotkeyVK:    DefaultToggleHotkeyVK,
		ToggleHotkeyName:  "F12",
	}
	AppConfig.AutoClicker.DelayMs = 100
	AppConfig.AutoClicker.DownMs = DefaultDownMs

	data, err := ioutil.ReadFile(configFilePath)
	if err == nil {
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
				FocusGuardEnabled *bool `json:"focus_guard_enabled"`
			}
			if json.Unmarshal(data, &probe) == nil && probe.FocusGuardEnabled != nil {
				AppConfig.FocusGuardEnabled = *probe.FocusGuardEnabled
			}

			// toggle_hotkey_vk가 0이면(구버전 config 또는 명시적 해제) 기본값 유지.
			if loaded.ToggleHotkeyVK != 0 {
				AppConfig.ToggleHotkeyVK = loaded.ToggleHotkeyVK
				AppConfig.ToggleHotkeyName = loaded.ToggleHotkeyName
			}
		}
	}
}

func SaveConfig() {
	AppConfig.mu.RLock()
	defer AppConfig.mu.RUnlock()

	data, err := json.MarshalIndent(AppConfig, "", "  ")
	if err == nil {
		ioutil.WriteFile(configFilePath, data, 0644)
	}
}

func (c *Config) SetActive(active bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.IsActive = active

	ActiveTriggersMu.Lock()
	defer ActiveTriggersMu.Unlock()
	if active {
		ActiveTriggers = make(map[uint32]MacroSlot)
		for _, s := range c.Slots {
			if s.TriggerVK != 0 && s.TargetVK != 0 {
				if existing, ok := ActiveTriggers[s.TriggerVK]; !ok || s.DelayMs < existing.DelayMs {
					ActiveTriggers[s.TriggerVK] = s
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
