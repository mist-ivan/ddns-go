package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 0.13：多源同轮变更必须全部出现在同一封邮件里。
// 旧实现只取 changes[0]，却把每个源的基线都推进，其余变更永久丢失。
func TestComposeIPChangeMailMultiSource(t *testing.T) {
	conf := &Config{Lang: "zh"}
	changes := []sourceChange{
		{Key: "home", PrevIPv4: "1.1.1.1", IPv4: "9.9.9.1"},
		{Key: "office", PrevIPv4: "2.2.2.2", IPv4: "9.9.9.2"},
	}
	subject, body := composeIPChangeMail(conf, changes, false)

	if !strings.Contains(subject, "2") {
		t.Fatalf("subject should mention the number of changed sources: %s", subject)
	}
	for _, want := range []string{
		"9.9.9.1", "9.9.9.2", // 两个新 IP 都要在
		"配置: home", "配置: office", // 且都能定位到来源
		"1.1.1.1", "2.2.2.2", // 旧 IP 保留
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestComposeIPChangeMailMultiSourceEN(t *testing.T) {
	conf := &Config{Lang: "en"}
	_, body := composeIPChangeMail(conf, []sourceChange{
		{Key: "home", PrevIPv4: "1.1.1.1", IPv4: "9.9.9.1"},
		{Key: "office", PrevIPv4: "2.2.2.2", IPv4: "9.9.9.2"},
	}, false)
	for _, want := range []string{"Config: home", "Config: office", "previous: 1.1.1.1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

// 单源时不该退化：仍应只有一个「配置」段和原来格式的新旧 IP。
func TestComposeIPChangeMailSingleSourceKeepsFormat(t *testing.T) {
	conf := &Config{Lang: "zh"}
	subject, body := composeIPChangeMail(conf, []sourceChange{
		{Key: "home", PrevIPv4: "1.1.1.1", IPv4: "2.2.2.2"},
	}, false)
	if !strings.Contains(subject, "1.1.1.1 -> 2.2.2.2") {
		t.Fatalf("single-source subject changed unexpectedly: %s", subject)
	}
	if !strings.Contains(body, "上次: 1.1.1.1") {
		t.Fatalf("single-source body should keep previous IP:\n%s", body)
	}
}

func TestComposeRecoveredMailMentionsChanges(t *testing.T) {
	conf := &Config{Lang: "zh"}
	status := FetchStatus{IPv4Expected: true, IPv4OK: true}
	_, body := composeRecoveredMail(conf, status, "9.9.9.1", "", []sourceChange{
		{Key: "home", PrevIPv4: "1.1.1.1", IPv4: "9.9.9.1"},
	})
	if !strings.Contains(body, "故障期间检测到 IP 变更") {
		t.Fatalf("recovery mail should have a change section:\n%s", body)
	}
	if !strings.Contains(body, "1.1.1.1 -> 9.9.9.1") {
		t.Fatalf("recovery mail should show the change:\n%s", body)
	}
	if !strings.Contains(body, "home") {
		t.Fatalf("recovery mail should name the source:\n%s", body)
	}

	// 没有变更时不应该出现该段落
	_, plain := composeRecoveredMail(conf, status, "9.9.9.1", "", nil)
	if strings.Contains(plain, "故障期间检测到 IP 变更") {
		t.Fatalf("recovery mail without changes must not add the section:\n%s", plain)
	}
}

func TestBuildMessageIDIsValidAndUnique(t *testing.T) {
	a := buildMessageID()
	b := buildMessageID()

	if !strings.HasPrefix(a, "<") || !strings.HasSuffix(a, ">") {
		t.Fatalf("bad Message-ID format: %s", a)
	}
	if strings.ContainsAny(a, "\r\n ") {
		t.Fatalf("Message-ID must not contain CR/LF/space: %q", a)
	}
	if strings.Count(a, "@") != 1 {
		t.Fatalf("Message-ID should contain exactly one @: %s", a)
	}
	if a == b {
		t.Fatalf("Message-ID must be unique per message: %s", a)
	}

	msg := string(buildMailMessage("a@x.com", []string{"b@x.com"}, "subject", "body"))
	if !strings.Contains(msg, "\r\nMessage-ID: <") {
		t.Fatalf("mail is missing the Message-ID header:\n%s", msg)
	}
	if strings.Count(msg, "Message-ID:") != 1 {
		t.Fatalf("mail should carry exactly one Message-ID:\n%s", msg)
	}
}

func TestMsgIDDomainStripsIllegalChars(t *testing.T) {
	host := msgIDDomain()
	if host == "" {
		t.Fatal("msgIDDomain must never return empty")
	}
	for _, r := range host {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-'
		if !ok {
			t.Fatalf("msgIDDomain returned illegal char %q in %q", r, host)
		}
	}
	if strings.HasPrefix(host, "-") || strings.HasPrefix(host, ".") ||
		strings.HasSuffix(host, "-") || strings.HasSuffix(host, ".") {
		t.Fatalf("msgIDDomain must not start/end with - or .: %q", host)
	}
}

func TestResetEmailBackoffClearsState(t *testing.T) {
	withConfigPath(t, filepath.Join(t.TempDir(), "conf.yaml"))

	storeEmailState(emailState{
		LastIPv4:    "1.1.1.1",
		FailCount:   3,
		NextRetryAt: time.Now().Add(time.Hour).Unix(),
	})
	if GetEmailBackoffUntil() == 0 {
		t.Fatal("expected an active backoff before reset")
	}

	ResetEmailBackoff()

	if got := GetEmailBackoffUntil(); got != 0 {
		t.Fatalf("backoff should be cleared, got until=%d", got)
	}
	state := loadEmailState()
	if state.FailCount != 0 || state.NextRetryAt != 0 {
		t.Fatalf("state should be reset: %+v", state)
	}
	// IP 基线不能被重置连坐清掉
	if state.LastIPv4 != "1.1.1.1" {
		t.Fatalf("reset must keep the IP baseline: %+v", state)
	}

	// 幂等：再次调用不应报错或改回任何东西
	ResetEmailBackoff()
	if got := GetEmailBackoffUntil(); got != 0 {
		t.Fatalf("unexpected backoff after second reset: %d", got)
	}
}

func TestGetEmailBackoffUntilExpired(t *testing.T) {
	withConfigPath(t, filepath.Join(t.TempDir(), "conf.yaml"))

	storeEmailState(emailState{FailCount: 1, NextRetryAt: time.Now().Add(-time.Minute).Unix()})
	if got := GetEmailBackoffUntil(); got != 0 {
		t.Fatalf("an expired backoff must report 0, got %d", got)
	}
}
