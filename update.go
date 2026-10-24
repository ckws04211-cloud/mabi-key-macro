package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// 새 버전 알림.
//
// 별도 배포 서버 없이 GitHub 릴리즈를 그대로 쓴다: 시작 직후(그리고 이후 주기적으로)
// 공개 저장소의 "최신 릴리즈" API를 한 번 조회해 지금 버전과 비교한다. 자동으로 내려받아
// 교체하지는 않는다 — 관리자 권한 앱이 스스로 바뀌는 건 위험하고 백신/안티치트가 의심할 수
// 있어서, 새 버전이 있다고 알리고 릴리즈 페이지만 열어 준다. 실패(오프라인, 속도 제한 등)는
// 조용히 넘기고, 설정에서 끌 수 있다.

const (
	appVersion = "13.0.0" // 릴리즈 태그(v13.0.0)와 같은 형식의 숫자 버전

	updateRepoURL   = "https://github.com/ckws04211-cloud/mabi-key-macro/"
	updateLatestAPI = "https://api.github.com/repos/ckws04211-cloud/mabi-key-macro/releases/latest"

	updateFirstDelay = 15 * time.Second // 시작 직후 부하를 피하려고 잠깐 뒤에 첫 확인
	updateInterval   = 12 * time.Hour
)

// UpdateInfo: 지금 버전보다 새로운 릴리즈 정보.
type UpdateInfo struct {
	Version string // 예: "14.0.0"
	URL     string // 릴리즈 페이지
}

var (
	updateAvail atomic.Pointer[UpdateInfo]
	// UpdateStateCallback: 알림 상태가 바뀔 때 호출된다(UI 갱신용).
	UpdateStateCallback func()
	updateNudge         = make(chan struct{}, 1)
)

// AvailableUpdate: 새 버전이 있으면 그 정보, 없으면 nil.
func AvailableUpdate() *UpdateInfo { return updateAvail.Load() }

// ClearUpdateNotice: 알림을 지운다(설정에서 끌 때).
func ClearUpdateNotice() {
	if updateAvail.Swap(nil) != nil && UpdateStateCallback != nil {
		go UpdateStateCallback()
	}
}

// TriggerUpdateCheck: 기다리지 않고 지금 확인하도록 깨운다(설정에서 켤 때).
func TriggerUpdateCheck() {
	select {
	case updateNudge <- struct{}{}:
	default:
	}
}

// StartUpdateChecker: 백그라운드에서 주기적으로 새 버전을 확인한다.
func StartUpdateChecker() {
	safeGo("업데이트 확인", func() {
		wait := updateFirstDelay
		for {
			select {
			case <-time.After(wait):
			case <-updateNudge:
			}
			wait = updateInterval

			if !AppConfig.GetUpdateCheck() {
				continue
			}
			info, err := fetchLatestRelease()
			if err != nil {
				continue // 오프라인/제한 등은 다음 주기에 다시 시도
			}
			var next *UpdateInfo
			if info != nil && isNewerVersion(info.Version, appVersion) {
				next = info
			}
			prev := updateAvail.Swap(next)
			if (prev == nil) != (next == nil) || (prev != nil && next != nil && prev.Version != next.Version) {
				if UpdateStateCallback != nil {
					go UpdateStateCallback()
				}
			}
		}
	})
}

func fetchLatestRelease() (*UpdateInfo, error) {
	req, err := http.NewRequest("GET", updateLatestAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MabinogiKeyDown/"+appVersion)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("릴리즈 조회 실패: HTTP %d", resp.StatusCode)
	}

	var rel struct {
		TagName    string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.Draft || rel.Prerelease || rel.TagName == "" {
		return nil, nil
	}
	// 응답이 이상해도 엉뚱한 주소를 열지 않도록, 우리 저장소 주소만 인정한다.
	if !strings.HasPrefix(rel.HTMLURL, updateRepoURL) {
		return nil, fmt.Errorf("예상 밖의 릴리즈 주소: %q", rel.HTMLURL)
	}
	return &UpdateInfo{Version: strings.TrimPrefix(rel.TagName, "v"), URL: rel.HTMLURL}, nil
}

// parseVersion: "v13.0.0", "13.0", "13.0.0-beta" 같은 문자열에서 숫자 3개를 뽑는다.
func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return v, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// isNewerVersion: latest가 current보다 확실히 높을 때만 true(해석 못 하면 false).
func isNewerVersion(latest, current string) bool {
	l, ok1 := parseVersion(latest)
	c, ok2 := parseVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// OpenReleasePage: 기본 브라우저로 릴리즈 페이지를 연다. explorer를 거치면 관리자 권한을
// 그대로 물려받지 않은 일반 권한 브라우저로 열린다.
func OpenReleasePage(url string) {
	if !strings.HasPrefix(url, updateRepoURL) {
		return
	}
	cmd := exec.Command("explorer.exe", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	_ = cmd.Start()
	go cmd.Wait() // 좀비 프로세스 방지(explorer는 보통 바로 끝난다)
}
