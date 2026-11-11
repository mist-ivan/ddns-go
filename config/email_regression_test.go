package config

import (
	"bufio"
	"encoding/base64"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件是 v0.11 针对以下缺陷的回归测试：
//   1. 变更快照按地址族各追加一次，IPv4 那一条的 IPv6 字段为空，
//      导致邮件里 IPv6 恒为 "-"；双栈同轮变更时 IPv6 事件被整条丢弃。
//   2. 发信成功后写回发信前的旧快照，回滚了 FailCount / Alerted / LastSent，
//      造成退避等级永久抬高、重启后重复告警且恢复通知丢失。
//   3. 告警同轮压制变更邮件时顺手推进了基线，变更事件永久丢失。

// regSink 是一个最小 SMTP sink，可切换"拒绝 MAIL FROM"，并记录 DATA 原文。
type regSink struct {
	port     int
	mails    chan string
	failMail *atomic.Bool
}

func newRegSink(t *testing.T) *regSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := &regSink{
		port:     ln.Addr().(*net.TCPAddr).Port,
		mails:    make(chan string, 16),
		failMail: &atomic.Bool{},
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *regSink) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	_, _ = w.WriteString("220 reg ESMTP\r\n")
	_ = w.Flush()

	inData := false
	var body strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tr := strings.TrimRight(line, "\r\n")
		if inData {
			if tr == "." {
				inData = false
				s.mails <- body.String()
				body.Reset()
				_, _ = w.WriteString("250 queued\r\n")
				_ = w.Flush()
				continue
			}
			body.WriteString(line)
			continue
		}
		switch {
		case strings.HasPrefix(tr, "EHLO"), strings.HasPrefix(tr, "HELO"):
			_, _ = w.WriteString("250 reg\r\n")
		case strings.HasPrefix(tr, "MAIL FROM"):
			if s.failMail.Load() {
				_, _ = w.WriteString("550 mailbox unavailable\r\n")
			} else {
				_, _ = w.WriteString("250 ok\r\n")
			}
		case strings.HasPrefix(tr, "DATA"):
			inData = true
			_, _ = w.WriteString("354 go ahead\r\n")
		case strings.HasPrefix(tr, "QUIT"):
			_, _ = w.WriteString("221 bye\r\n")
			_ = w.Flush()
			return
		default:
			_, _ = w.WriteString("250 ok\r\n")
		}
		_ = w.Flush()
	}
}

// await 等待一封邮件；超时返回 ok=false
func (s *regSink) await(d time.Duration) (string, bool) {
	select {
	case m := <-s.mails:
		return m, true
	case <-time.After(d):
		return "", false
	}
}

// drain 取走当前所有已投递邮件
func (s *regSink) drain(d time.Duration) int {
	n := 0
	for {
		select {
		case <-s.mails:
			n++
		case <-time.After(d):
			return n
		}
	}
}

// bodyOf 解出邮件正文（正文固定 base64，逐行 76 字符）
func bodyOf(raw string) string {
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	if len(parts) != 2 {
		return raw
	}
	clean := strings.ReplaceAll(parts[1], "\r\n", "")
	if dec, err := base64.StdEncoding.DecodeString(clean); err == nil {
		return string(dec)
	}
	return parts[1]
}

func regConf(port int, alert bool) *Config {
	return &Config{Lang: "zh", Email: Email{
		Enable:              true,
		SMTPHost:            "127.0.0.1",
		SMTPPort:            port,
		From:                "ddns-go <a@example.com>",
		To:                  "b@example.com",
		Encrypt:             "none",
		AlertOnFetchFailure: alert,
	}}
}

// resetRegTracker 让告警 tracker 从状态文件重新加载（等价于新进程启动）
func resetRegTracker() {
	fetchTrackLock.Lock()
	fetchTracker = fetchFailureTracker{}
	fetchTrackerLoaded = false
	fetchTrackLock.Unlock()
}

func resetRegTrackerClean() {
	fetchTrackLock.Lock()
	fetchTracker = fetchFailureTracker{}
	fetchTrackerLoaded = true
	fetchTrackLock.Unlock()
}

var regOK = FetchStatus{IPv4Expected: true, IPv4OK: true, IPv6Expected: true, IPv6OK: true}
var regIPv4Failed = FetchStatus{IPv4Expected: true, IPv6Expected: true, IPv6OK: true}

// 双栈同轮变更：邮件必须同时包含新 IPv4 和新 IPv6，且基线正常推进。
func TestEmailDualFamilyChangeReportsBoth(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	s := newRegSink(t)
	conf := regConf(s.port, false)

	storeEmailState(emailState{
		LastIPv4: "1.1.1.1",
		LastIPv6: "2001:db8::1",
		Sources:  map[string]string{"s1|v4": "1.1.1.1", "s1|v6": "2001:db8::1"},
	})

	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv4: "2.2.2.2", IPv6: "2001:db8::2"}}, regOK)

	raw, ok := s.await(5 * time.Second)
	if !ok {
		t.Fatal("没有收到通知邮件")
	}
	body := bodyOf(raw)
	if !strings.Contains(body, "2.2.2.2") {
		t.Errorf("邮件缺少新 IPv4，正文:\n%s", body)
	}
	if !strings.Contains(body, "2001:db8::2") {
		t.Errorf("邮件缺少新 IPv6，正文:\n%s", body)
	}
	if !strings.Contains(body, "2001:db8::1") {
		t.Errorf("邮件缺少 IPv6 的上次值，正文:\n%s", body)
	}
}

// 只变 IPv4 时，邮件里的 IPv6 应当显示当前值，而不是 "-"。
func TestEmailIPv4ChangeKeepsIPv6InMail(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	s := newRegSink(t)
	conf := regConf(s.port, false)

	storeEmailState(emailState{
		LastIPv4: "1.1.1.1",
		LastIPv6: "2001:db8::1",
		Sources:  map[string]string{"s1|v4": "1.1.1.1", "s1|v6": "2001:db8::1"},
	})

	// IPv6 未变
	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv4: "2.2.2.2", IPv6: "2001:db8::1"}}, regOK)

	raw, ok := s.await(5 * time.Second)
	if !ok {
		t.Fatal("没有收到通知邮件")
	}
	body := bodyOf(raw)
	if !strings.Contains(body, "2.2.2.2") {
		t.Errorf("邮件缺少新 IPv4，正文:\n%s", body)
	}
	if !strings.Contains(body, "2001:db8::1") {
		t.Errorf("IPv4 变更时邮件把 IPv6 显示成了空值，正文:\n%s", body)
	}
}

// 告警与变更同轮：alerted / lastSent 必须真正落盘（重启后行为依赖它们）。
func TestEmailAlertAndChangePersistState(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTrackerClean()

	s := newRegSink(t)
	conf := regConf(s.port, true)

	storeEmailState(emailState{
		LastIPv6: "2001:db8::1",
		Sources:  map[string]string{"s1|v6": "2001:db8::1"},
	})
	resetRegTrackerClean()

	for i := 0; i < 2; i++ {
		NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::1"}}, regIPv4Failed)
	}
	// 第 3 轮：触发告警，同时 IPv6 变化
	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::2"}}, regIPv4Failed)

	if n := s.drain(400 * time.Millisecond); n != 1 {
		t.Fatalf("期望 1 封告警邮件，实际 %d 封", n)
	}

	st := loadEmailState()
	if !st.Alerted {
		t.Error("告警已发出，但落盘的 alerted 为 false：重启后会重复告警、恢复通知丢失")
	}
	if st.LastSent == 0 {
		t.Error("告警已发出，但落盘的 lastSent 为 0：状态面板会显示错误的“上次通知”")
	}
}

// 告警同轮压制变更邮件时，不能推进基线；下一轮必须把这次变更补发出去。
func TestEmailChangeDeferredNotDropped(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTrackerClean()

	s := newRegSink(t)
	conf := regConf(s.port, true)

	storeEmailState(emailState{
		LastIPv6: "2001:db8::1",
		Sources:  map[string]string{"s1|v6": "2001:db8::1"},
	})
	resetRegTrackerClean()

	for i := 0; i < 2; i++ {
		NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::1"}}, regIPv4Failed)
	}
	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::2"}}, regIPv4Failed)
	if n := s.drain(400 * time.Millisecond); n != 1 {
		t.Fatalf("第 3 轮应只发 1 封告警，实际 %d 封", n)
	}

	// 基线不能被推进，否则这次变更就永久丢了
	if got := loadEmailState().Sources["s1|v6"]; got != "2001:db8::1" {
		t.Fatalf("告警同轮时基线被推进到 %q，变更事件会永久丢失", got)
	}

	// 下一轮地址不变、故障持续（不再告警）：应补发变更邮件
	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::2"}}, regIPv4Failed)
	raw, ok := s.await(5 * time.Second)
	if !ok {
		t.Fatal("被顺延的变更通知没有在下一轮补发")
	}
	if body := bodyOf(raw); !strings.Contains(body, "2001:db8::2") {
		t.Errorf("补发的邮件内容不正确，正文:\n%s", body)
	}
}

// 故障期间重启：恢复后必须还能发出恢复通知。
func TestEmailRecoveryNoticeSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTrackerClean()

	s := newRegSink(t)
	conf := regConf(s.port, true)

	storeEmailState(emailState{
		LastIPv6: "2001:db8::1",
		Sources:  map[string]string{"s1|v6": "2001:db8::1"},
	})
	resetRegTrackerClean()

	for i := 0; i < 3; i++ {
		NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::1"}}, regIPv4Failed)
	}
	if n := s.drain(400 * time.Millisecond); n != 1 {
		t.Fatalf("期望 1 封告警，实际 %d 封", n)
	}

	// 模拟进程重启：清内存态，tracker 重新从文件加载
	resetEmailStateForTest()
	resetRegTracker()

	// 恢复
	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::1"}}, regOK)
	if _, ok := s.await(5 * time.Second); !ok {
		t.Error("故障期间重启后恢复，恢复通知没有发出")
	}
}

// 故障期间重启：同一故障不能重复告警。
func TestEmailNoDuplicateAlertAfterRestart(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer resetRegTrackerClean()

	s := newRegSink(t)
	conf := regConf(s.port, true)

	storeEmailState(emailState{
		LastIPv6: "2001:db8::1",
		Sources:  map[string]string{"s1|v6": "2001:db8::1"},
	})
	resetRegTrackerClean()

	for i := 0; i < 3; i++ {
		NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::1"}}, regIPv4Failed)
	}
	if n := s.drain(400 * time.Millisecond); n != 1 {
		t.Fatalf("期望 1 封告警，实际 %d 封", n)
	}

	resetEmailStateForTest()
	resetRegTracker()

	// 同一故障继续
	for i := 0; i < 3; i++ {
		NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::1"}}, regIPv4Failed)
	}
	if n := s.drain(400 * time.Millisecond); n != 0 {
		t.Errorf("同一次故障在重启后又告警了 %d 次", n)
	}
}

// 发信成功后退避计数必须清零，否则退避等级会被永久抬高。
func TestEmailFailCountResetsAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	s := newRegSink(t)
	conf := regConf(s.port, false)

	storeEmailState(emailState{
		LastIPv4: "1.1.1.1",
		Sources:  map[string]string{"s1|v4": "1.1.1.1"},
	})

	status := FetchStatus{IPv4Expected: true, IPv4OK: true}

	s.failMail.Store(true)
	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv4: "2.2.2.2"}}, status)
	st := loadEmailState()
	if st.FailCount != 1 {
		t.Fatalf("失败一次后 failCount 应为 1，实际 %d", st.FailCount)
	}

	// 只清 nextRetryAt，模拟退避期已过
	st.NextRetryAt = 0
	storeEmailState(st)

	s.failMail.Store(false)
	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv4: "2.2.2.2"}}, status)
	if _, ok := s.await(5 * time.Second); !ok {
		t.Fatal("第二次没有发出邮件")
	}

	if final := loadEmailState(); final.FailCount != 0 {
		t.Errorf("发信成功后 failCount 仍为 %d，退避等级会被永久抬高", final.FailCount)
	}
}

// 状态文件里的 nextRetryAt 落在很远的未来时必须自愈，而不是永久静默跳过。
// 触发源：系统时钟被大幅回拨、状态文件被改坏或异常写入。
func TestEmailStateFarFutureRetrySelfHeals(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	s := newRegSink(t)
	conf := regConf(s.port, false)

	storeEmailState(emailState{
		LastIPv4:    "1.1.1.1",
		Sources:     map[string]string{"s1|v4": "1.1.1.1"},
		NextRetryAt: time.Now().Add(10 * 365 * 24 * time.Hour).Unix(),
		FailCount:   999999,
	})

	NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv4: "2.2.2.2"}}, regOK)

	if _, ok := s.await(5 * time.Second); !ok {
		t.Fatal("远期 nextRetryAt 导致通知被永久跳过（未自愈）")
	}
	st := loadEmailState()
	if st.NextRetryAt != 0 || st.FailCount != 0 {
		t.Errorf("异常状态未被重置: nextRetryAt=%d failCount=%d", st.NextRetryAt, st.FailCount)
	}
}
