package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- 0.14 regression tests ---

// 首轮同时触发告警和 OnStart 基线时，只能发一封（告警）。
func TestNotifyOnStartAlertSendsOnlyOneMail(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	srv := newSMTPTestServer(t, false)
	conf := regConf(srv.port(), true)
	conf.Email.OnStart = true

	fetchTrackLock.Lock()
	fetchTracker = fetchFailureTracker{consecutive: fetchAlertThreshold - 1}
	fetchTrackerLoaded = true
	fetchTrackLock.Unlock()
	defer func() {
		fetchTrackLock.Lock()
		fetchTracker = fetchFailureTracker{}
		fetchTrackerLoaded = false
		fetchTrackLock.Unlock()
	}()

	status := FetchStatus{IPv4Expected: true, IPv4OK: true, IPv6Expected: true, IPv6OK: false}
	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "g", Label: "g", IPv4: "5.5.5.5"}}, status)
	time.Sleep(300 * time.Millisecond)

	if n, _ := srv.stats(); n != 1 {
		t.Fatalf("first-run alert round sent %d mails, want 1", n)
	}
	if st := loadEmailState(); st.LastIPv4 != "5.5.5.5" {
		t.Fatalf("baseline was not committed after alert: %+v", st)
	}
	if _, _, _, notified := GetEmailStateSnapshotEx(); notified {
		t.Fatal("an alert-only round must not mark ChangeNotified")
	}
}

// 多源基线邮件要列出全部源；变更邮件只列本次变更源的域名。
func TestComposeBaselineListsAllSourcesAndChangedDomainsOnly(t *testing.T) {
	conf := &Config{Lang: "zh"}
	baseline := []sourceChange{
		{Key: "a", Label: "a", Domains: []string{"a.example.com"}, IPv4: "1.1.1.1"},
		{Key: "b", Label: "b", Domains: []string{"b.example.com"}, IPv6: "2001:db8::1"},
	}
	_, body := composeIPChangeMail(conf, baseline, true)
	for _, want := range []string{"配置: a", "配置: b", "a.example.com", "b.example.com"} {
		if !strings.Contains(body, want) {
			t.Fatalf("baseline body missing %q:\n%s", want, body)
		}
	}

	changes := []sourceChange{
		{Key: "a", Label: "a", Domains: []string{"a.example.com"}, PrevIPv4: "1.1.1.1", IPv4: "2.2.2.2"},
	}
	_, changeBody := composeIPChangeMail(conf, changes, false)
	if strings.Contains(changeBody, "b.example.com") {
		t.Fatalf("change body must not list other configs' domains:\n%s", changeBody)
	}
}

// 告警正文应优先使用本轮取到的 IP，而不是状态里的旧值。
func TestComposeFetchAlertUsesCurrentIP(t *testing.T) {
	conf := &Config{Lang: "zh"}
	_, body := composeFetchAlertMail(conf, 3,
		FetchStatus{IPv4Expected: true, IPv4OK: true, IPv6Expected: true, IPv6OK: false},
		"5.5.5.5", "")
	if !strings.Contains(body, "5.5.5.5") {
		t.Fatalf("alert body missing current IPv4:\n%s", body)
	}
}

// 并发写状态（后台发信 + 保存/测试触发 Reset）后，文件必须仍是合法 JSON。
func TestStoreEmailStateConcurrentWritesRemainValid(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			storeEmailState(emailState{LastIPv4: fmt.Sprintf("10.0.0.%d", i%250)})
		}(i)
	}
	wg.Wait()

	byt, err := os.ReadFile(emailStatePath())
	if err != nil {
		t.Fatal(err)
	}
	var state emailState
	if err := json.Unmarshal(byt, &state); err != nil {
		t.Fatalf("state file is not valid JSON after concurrent writes: %v\n%s", err, byt)
	}
}

// 恢复通知成功发送后，状态里的 lastIPv4/lastIPv6 必须是当前值。
func TestNotifyRecoveryUpdatesLastIP(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	srv := newSMTPTestServer(t, false)
	conf := regConf(srv.port(), true)
	storeEmailState(emailState{
		LastIPv4:       "1.1.1.1",
		ChangeNotified: true,
		Sources:        map[string]string{"s1|v4": "1.1.1.1"},
	})
	fetchTrackLock.Lock()
	fetchTracker = fetchFailureTracker{alerted: true}
	fetchTrackerLoaded = true
	fetchTrackLock.Unlock()
	defer func() {
		fetchTrackLock.Lock()
		fetchTracker = fetchFailureTracker{}
		fetchTrackerLoaded = false
		fetchTrackLock.Unlock()
	}()

	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv4: "2.2.2.2"}}, FetchStatus{IPv4Expected: true, IPv4OK: true})
	time.Sleep(300 * time.Millisecond)

	st := loadEmailState()
	if st.LastIPv4 != "2.2.2.2" {
		t.Fatalf("after recovery lastIPv4=%q, want 2.2.2.2", st.LastIPv4)
	}
	if !st.ChangeNotified {
		t.Fatal("recovery should preserve/not downgrade ChangeNotified")
	}
}

// 同一 IPv6 的大小写/压缩形式变化不能触发变更邮件。
func TestNotifyIPv6TextVariantIsNotAChange(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	srv := newSMTPTestServer(t, false)
	conf := regConf(srv.port(), false)
	storeEmailState(emailState{
		LastIPv6: "2001:db8::1",
		Sources:  map[string]string{"s1|v6": "2001:db8::1"},
	})

	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:DB8::1"}}, FetchStatus{IPv6Expected: true, IPv6OK: true})
	time.Sleep(300 * time.Millisecond)

	if n, _ := srv.stats(); n != 0 {
		t.Fatalf("same IPv6 address with different case sent %d mails", n)
	}
}

// 异常远期退避不应显示在状态接口。
func TestGetEmailBackoffUntilRejectsFarFuture(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	storeEmailState(emailState{
		FailCount:   3,
		NextRetryAt: time.Now().Add(10 * 365 * 24 * time.Hour).Unix(),
	})
	if got := GetEmailBackoffUntil(); got != 0 {
		t.Fatalf("GetEmailBackoffUntil returned absurd value %d", got)
	}
}

// 关闭取 IP 失败告警时，落盘的 alerted 必须清掉，避免重开后补发假恢复。
func TestDisablingAlertClearsAlerted(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	storeEmailState(emailState{
		Alerted: true,
		Sources: map[string]string{"s1|v4": "1.1.1.1"},
	})
	conf := regConf(2525, false)
	conf.Email.AlertOnFetchFailure = false

	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv4: "1.1.1.1"}}, FetchStatus{IPv4Expected: true, IPv4OK: true})
	if loadEmailState().Alerted {
		t.Fatal("alerted still true after disabling fetch-failure alerts")
	}
}

// 大收件人列表的 To 头必须折叠到 RFC 行宽以内。
func TestBuildMailMessageFoldsLongHeaders(t *testing.T) {
	to := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		to = append(to, "user000@example.com")
	}
	msg := string(buildMailMessage("sender@example.com", to, "subject", "body"))
	for _, line := range strings.Split(msg, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("header line length %d exceeds RFC limit", len(line))
		}
	}
}
