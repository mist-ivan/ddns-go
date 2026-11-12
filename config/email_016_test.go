package config

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 没有任何有效检测任务时，不能把旧告警当成“恢复”发信。
func TestNoDetectionTasksDoesNotSendRecovery(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTracker()

	s := newRegSink(t)
	conf := regConf(s.port, true)
	storeEmailState(emailState{
		Alerted:         true,
		AlertedFamilies: []string{"IPv6"},
		LastIPv6:        "2001:db8::1",
	})

	NotifyIPChangeEmail(conf, nil, FetchStatus{})
	if mail, ok := s.await(400 * time.Millisecond); ok {
		t.Fatalf("no-task round sent a recovery mail:\n%s", bodyOf(mail))
	}
	st := loadEmailState()
	if st.Alerted || len(st.AlertedFamilies) != 0 {
		t.Fatalf("stale alert state not cleared: %+v", st)
	}
}

// 原先失败的是 IPv6，用户禁用 IPv6、只保留健康 IPv4，不应发“IPv4 已恢复”。
func TestDisablingFailedFamilyDoesNotSendRecovery(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTracker()

	s := newRegSink(t)
	conf := regConf(s.port, true)
	storeEmailState(emailState{
		Alerted:         true,
		AlertedFamilies: []string{"IPv6"},
		LastIPv4:        "1.1.1.1",
		Sources:         map[string]string{"s1|v4": "1.1.1.1"},
	})

	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv4: "1.1.1.1"}},
		FetchStatus{IPv4Expected: true, IPv4OK: true, IPv6Expected: false, IPv6OK: false})
	if mail, ok := s.await(400 * time.Millisecond); ok {
		t.Fatalf("disabled failed family sent a recovery mail:\n%s", bodyOf(mail))
	}
	st := loadEmailState()
	if st.Alerted || len(st.AlertedFamilies) != 0 {
		t.Fatalf("alert state not cleared: %+v", st)
	}
}

// 原先失败的 IPv6 仍启用且已恢复，恢复邮件只应提 IPv6。
func TestRecoveryMentionsOriginallyFailedFamily(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTracker()

	s := newRegSink(t)
	conf := regConf(s.port, true)
	storeEmailState(emailState{
		Alerted:         true,
		AlertedFamilies: []string{"IPv6"},
		AlertedSources:  []string{"s1"},
		LastIPv6:        "2001:db8::1",
		Sources:         map[string]string{"s1|v6": "2001:db8::1"},
	})

	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::1"}},
		FetchStatus{IPv4Expected: false, IPv4OK: false, IPv6Expected: true, IPv6OK: true, ConfiguredSources: []string{"s1"}})
	mail, ok := s.await(2 * time.Second)
	if !ok {
		t.Fatal("expected a recovery mail")
	}
	body := bodyOf(mail)
	if !strings.Contains(body, "IPv6") {
		t.Fatalf("recovery mail does not mention IPv6:\n%s", body)
	}
	if strings.Contains(body, "IPv4 检测已恢复") {
		t.Fatalf("recovery mail incorrectly mentions IPv4:\n%s", body)
	}
	if loadEmailState().Alerted {
		t.Fatal("alerted state not cleared after recovery")
	}
}

// 告警时应把失败地址族和失败源一起落盘，供重启后的恢复判断使用。
func TestAlertPersistsFailedFamiliesAndSources(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTracker()

	s := newRegSink(t)
	conf := regConf(s.port, true)
	fetchTrackLock.Lock()
	fetchTracker = fetchFailureTracker{consecutive: fetchAlertThreshold - 1}
	fetchTrackerLoaded = true
	fetchTrackLock.Unlock()

	status := FetchStatus{
		IPv6Expected:  true,
		FailedSources: []FailedSource{{Key: "a", Label: "家里", Families: []string{"IPv6"}}},
	}
	handleFetchHealth(conf, status, "", "", nil)
	if _, ok := s.await(2 * time.Second); !ok {
		t.Fatal("expected an alert mail")
	}
	st := loadEmailState()
	if !st.Alerted || len(st.AlertedFamilies) != 1 || st.AlertedFamilies[0] != "IPv6" {
		t.Fatalf("failed families not persisted: %+v", st)
	}
	if len(st.AlertedSources) != 1 || st.AlertedSources[0] != "a" {
		t.Fatalf("failed sources not persisted: %+v", st)
	}
}

// 原先失败的源被删除，但同地址族的另一个源健康：不能发“恢复”。
func TestRemovedFailedSourceDoesNotRecover(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTracker()

	s := newRegSink(t)
	conf := regConf(s.port, true)
	storeEmailState(emailState{
		Alerted:         true,
		AlertedFamilies: []string{"IPv6"},
		AlertedSources:  []string{"a"},
		LastIPv6:        "2001:db8::1",
		Sources:         map[string]string{"a|v6": "2001:db8::1"},
	})

	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "b", IPv6: "2001:db8::2"}},
		FetchStatus{IPv6Expected: true, IPv6OK: true, ConfiguredSources: []string{"b"}})
	if mail, ok := s.await(400 * time.Millisecond); ok {
		t.Fatalf("removed failed source sent a recovery mail:\n%s", bodyOf(mail))
	}
	st := loadEmailState()
	if st.Alerted || len(st.AlertedSources) != 0 {
		t.Fatalf("alert state not cleared: %+v", st)
	}
}

// 告警正文应列出具体失败源的名称/地址族/域名。
func TestFetchAlertListsFailedSources(t *testing.T) {
	conf := &Config{Lang: "zh"}
	status := FetchStatus{
		IPv6Expected: true,
		FailedSources: []FailedSource{{
			Key:      "home",
			Label:    "家里",
			Domains:  []string{"a.example.com"},
			Families: []string{"IPv6"},
		}},
	}
	_, body := composeFetchAlertMail(conf, fetchAlertThreshold, status, "", "")
	for _, want := range []string{"家里", "IPv6", "a.example.com"} {
		if !strings.Contains(body, want) {
			t.Fatalf("alert body missing %q:\n%s", want, body)
		}
	}
}

// 退避重置发生在发送进行中时，旧请求的失败不能重新设置退避。
func TestResetBackoffDuringSendDoesNotRearm(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		<-release
		_ = conn.Close()
	}()

	storeEmailState(emailState{
		FailCount: 1,
		LastIPv4:  "1.1.1.1",
	})
	conf := regConf(ln.Addr().(*net.TCPAddr).Port, false)
	errCh := make(chan error, 1)
	go func() {
		errCh <- conf.sendMail("subject", "body")
	}()

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("SMTP server did not accept connection")
	}

	// 模拟 HTTP 保存配置/测试成功：在旧 SMTP 会话仍卡住时解除退避。
	ResetEmailBackoff()
	close(release)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected send to fail after connection close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendMail did not return")
	}

	st := loadEmailState()
	if st.FailCount != 0 || st.NextRetryAt != 0 {
		t.Fatalf("old in-flight failure re-armed backoff: failCount=%d nextRetryAt=%d",
			st.FailCount, st.NextRetryAt)
	}
}
