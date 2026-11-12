package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 0.17 回归：旧的 IPv4 告警仍在时，新出现的 IPv6 故障必须单独告警，
// 而不是被旧的 alerted 布尔值永久压住。
func TestNewFamilyFailureAfterAlertAlertsAgain(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTracker()

	s := newRegSink(t)
	conf := regConf(s.port, true)

	storeEmailState(emailState{
		Alerted:            true,
		AlertedFamilies:    []string{"IPv4"},
		AlertedSources:     []string{"s1"},
		AlertedFailureKeys: []string{"s1|IPv4"},
		LastIPv4:           "1.1.1.1",
		Sources:            map[string]string{"s1|v4": "1.1.1.1"},
	})
	resetRegTracker()

	status := FetchStatus{
		IPv4Expected: true, IPv4OK: true,
		IPv6Expected: true, IPv6OK: false,
		FailedSources: []FailedSource{{
			Key: "s1", Label: "s1", Families: []string{"IPv6"},
		}},
		ConfiguredSources:    []string{"s1"},
		FailedFamilyKeys:     []string{"s1|IPv6"},
		ConfiguredFamilyKeys: []string{"s1|IPv4", "s1|IPv6"},
		OKFamilyKeys:         []string{"s1|IPv4"},
	}

	for i := 0; i < fetchAlertThreshold-1; i++ {
		handleFetchHealth(conf, status, "1.1.1.1", "", nil)
	}
	if raw, ok := s.await(200 * time.Millisecond); ok {
		t.Fatalf("IPv6 故障未达到阈值就发了邮件:\n%s", bodyOf(raw))
	}

	handleFetchHealth(conf, status, "1.1.1.1", "", nil)
	raw, ok := s.await(2 * time.Second)
	if !ok {
		t.Fatal("旧 IPv4 告警存在时，新出现的 IPv6 故障没有告警")
	}
	body := bodyOf(raw)
	if !strings.Contains(body, "IPv6") {
		t.Fatalf("新告警正文没有 IPv6:\n%s", body)
	}

	st := loadEmailState()
	if !st.Alerted {
		t.Fatal("新告警后 alerted 未落盘")
	}
	keys := strings.Join(st.AlertedFailureKeys, ",")
	if !strings.Contains(keys, "s1|IPv6") {
		t.Fatalf("新告警没有记录 IPv6 失败项: %v", st.AlertedFailureKeys)
	}
}

// 0.17 回归：原失败源只关闭了自己坏掉的 IPv4、但同族还有另一个健康源时，
// 不能把“b 一直健康”冒充成“a 的 IPv4 已恢复”。
func TestFailedSourceDisabledItsFamilyDoesNotRecover(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTracker()

	s := newRegSink(t)
	conf := regConf(s.port, true)

	storeEmailState(emailState{
		Alerted:            true,
		AlertedFamilies:    []string{"IPv4"},
		AlertedSources:     []string{"a"},
		AlertedFailureKeys: []string{"a|IPv4"},
		LastIPv4:           "1.1.1.1",
		Sources: map[string]string{
			"a|v6": "2001:db8::1",
			"b|v4": "2.2.2.2",
		},
	})
	resetRegTracker()

	status := FetchStatus{
		IPv4Expected: true, IPv4OK: true, // b 满足
		IPv6Expected: true, IPv6OK: true, // a 满足
		ConfiguredSources:    []string{"a", "b"},
		ConfiguredFamilyKeys: []string{"a|IPv6", "b|IPv4"},
		OKFamilyKeys:         []string{"a|IPv6", "b|IPv4"},
	}

	handleFetchHealth(conf, status, "2.2.2.2", "2001:db8::1", nil)
	if raw, ok := s.await(300 * time.Millisecond); ok {
		t.Fatalf("误发了恢复邮件:\n%s", bodyOf(raw))
	}
	st := loadEmailState()
	if st.Alerted || len(st.AlertedFailureKeys) != 0 {
		t.Fatalf("告警状态未清理: %+v", st)
	}
}

// 0.17 回归：密码只允许在同一 SMTP 凭据作用域内复用。
func TestCredentialScopeEqual(t *testing.T) {
	saved := Email{
		SMTPHost: "smtp.example.com",
		SMTPPort: 465,
		Username: "me@example.com",
		Encrypt:  "",
	}
	sameEffective := Email{
		SMTPHost: "SMTP.EXAMPLE.COM",
		SMTPPort: 465,
		Username: "me@example.com",
		Encrypt:  "ssl",
	}
	if !saved.CredentialScopeEqual(sameEffective) {
		t.Fatal("equivalent auto/SSL target should share credential scope")
	}

	cases := []Email{
		{SMTPHost: "evil.example.com", SMTPPort: 465, Username: "me@example.com", Encrypt: "ssl"},
		{SMTPHost: "smtp.example.com", SMTPPort: 587, Username: "me@example.com", Encrypt: "starttls"},
		{SMTPHost: "smtp.example.com", SMTPPort: 465, Username: "other@example.com", Encrypt: "ssl"},
		{SMTPHost: "smtp.example.com", SMTPPort: 465, Username: "ME@example.com", Encrypt: "ssl"},
		{SMTPHost: "smtp.example.com", SMTPPort: 465, Username: "me@example.com", Encrypt: "none"},
	}
	for _, other := range cases {
		if saved.CredentialScopeEqual(other) {
			t.Fatalf("credential scope should differ for %+v", other)
		}
	}
}

// 0.17 回归：两个族都确实恢复时，恢复邮件应包含两个族。
func TestRecoveryMentionsAllActuallyRecoveredFamilies(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTracker()

	s := newRegSink(t)
	conf := regConf(s.port, true)
	storeEmailState(emailState{
		Alerted:            true,
		AlertedFamilies:    []string{"IPv4", "IPv6"},
		AlertedSources:     []string{"s1"},
		AlertedFailureKeys: []string{"s1|IPv4", "s1|IPv6"},
		LastIPv4:           "1.1.1.1",
		LastIPv6:           "2001:db8::1",
		Sources: map[string]string{
			"s1|v4": "1.1.1.1",
			"s1|v6": "2001:db8::1",
		},
	})
	resetRegTracker()

	status := FetchStatus{
		IPv4Expected: true, IPv4OK: true,
		IPv6Expected: true, IPv6OK: true,
		ConfiguredSources:    []string{"s1"},
		ConfiguredFamilyKeys: []string{"s1|IPv4", "s1|IPv6"},
		OKFamilyKeys:         []string{"s1|IPv4", "s1|IPv6"},
	}
	handleFetchHealth(conf, status, "1.1.1.1", "2001:db8::1", nil)
	raw, ok := s.await(2 * time.Second)
	if !ok {
		t.Fatal("两族恢复但没有恢复邮件")
	}
	body := bodyOf(raw)
	for _, want := range []string{"IPv4", "IPv6"} {
		if !strings.Contains(body, want) {
			t.Fatalf("恢复邮件缺少 %s:\n%s", want, body)
		}
	}
}
