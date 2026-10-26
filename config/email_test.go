package config

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeessy2/ddns-go/v6/util"
)

func TestSplitRecipients(t *testing.T) {
	got := splitRecipients(" a@x.com, b@y.com;c@z.com\n\nd@w.com ")
	want := []string{"a@x.com", "b@y.com", "c@z.com", "d@w.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if n := len(splitRecipients("")); n != 0 {
		t.Fatalf("empty input should yield no recipients, got %d", n)
	}
}

func TestBuildMailMessageStripsHeaderInjection(t *testing.T) {
	msg := string(buildMailMessage(
		"a@x.com",
		[]string{"b@y.com"},
		"hello\r\nBcc: evil@x.com",
		"line1\r\nline2",
	))
	if strings.Contains(msg, "\r\nBcc:") {
		t.Fatal("subject must not be able to inject extra headers")
	}
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatal("subject must not be able to inject a Bcc header")
		}
	}
	if !strings.Contains(msg, "Content-Transfer-Encoding: base64") {
		t.Fatal("body should be base64 encoded")
	}
	if !strings.HasPrefix(msg, "From: a@x.com\r\n") {
		t.Fatalf("unexpected message start: %q", msg[:40])
	}
}

func TestComposeIPChangeMailChangeZH(t *testing.T) {
	conf := &Config{Lang: "zh"}
	subject, body := composeIPChangeMail(conf, emailState{LastIPv4: "1.1.1.1"}, "2.2.2.2", "", false)

	if !strings.Contains(subject, "1.1.1.1") || !strings.Contains(subject, "2.2.2.2") {
		t.Fatalf("subject should contain old and new IP: %s", subject)
	}
	if !strings.Contains(body, "上次: 1.1.1.1") {
		t.Fatalf("body should contain previous IP: %s", body)
	}
}

func TestComposeIPChangeMailBaselineEN(t *testing.T) {
	conf := &Config{Lang: "en"}
	_, body := composeIPChangeMail(conf, emailState{}, "2.2.2.2", "", true)

	if !strings.Contains(body, "first recorded IP baseline") {
		t.Fatalf("english baseline body missing: %s", body)
	}
	if !strings.Contains(body, "IPv4: 2.2.2.2") {
		t.Fatalf("body should contain the current IP: %s", body)
	}
}

func TestEmailValidate(t *testing.T) {
	if err := (&Email{}).Validate(); err == nil {
		t.Fatal("empty config should not validate")
	}
	e := &Email{SMTPHost: "smtp.x.com", To: "a@x.com", From: "b@x.com"}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// --- 状态持久化 ---

// resetEmailStateForTest 清空包内状态，避免用例之间互相污染
func resetEmailStateForTest() {
	emailStateLock.Lock()
	defer emailStateLock.Unlock()
	emailStateMem = nil
	emailStateWarn = false
	lastIgnoredIPv6 = ""
}

func withConfigPath(t *testing.T, path string) {
	t.Helper()
	t.Setenv(util.ConfigFilePathENV, path)
	resetEmailStateForTest()
}

func TestStoreEmailStateWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))

	storeEmailState(emailState{LastIPv4: "1.1.1.1", LastIPv6: "2001:db8::1"})

	path := emailStatePath()
	byt, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	if !strings.Contains(string(byt), `"lastIPv4": "1.1.1.1"`) {
		t.Fatalf("unexpected state content: %s", byt)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file should be removed after rename")
	}

	// 重新读取（先清空内存态，模拟进程重启）
	resetEmailStateForTest()
	if got := loadEmailState(); got.LastIPv4 != "1.1.1.1" {
		t.Fatalf("state not reloaded from disk: %+v", got)
	}
}

func TestStoreEmailStateFallsBackToMemory(t *testing.T) {
	dir := t.TempDir()
	// 父目录不存在 -> 写盘必然失败，用来模拟只读挂载或权限问题
	withConfigPath(t, filepath.Join(dir, "missing-dir", "config.yaml"))

	storeEmailState(emailState{LastIPv4: "9.9.9.9"})

	// 内存态必须生效，否则下一轮会重复发送同一封邮件
	if got := loadEmailState(); got.LastIPv4 != "9.9.9.9" {
		t.Fatalf("in-memory fallback not used: %+v", got)
	}
}

func TestLoadEmailStateToleratesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	if err := os.WriteFile(emailStatePath(), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadEmailState(); got.LastIPv4 != "" || got.LastIPv6 != "" {
		t.Fatalf("corrupt state should be treated as empty: %+v", got)
	}
}

// --- IPv6 临时地址过滤 ---

const ifInet6Sample = `20010db8000000000000000000000001 02 40 00 00     eth0
20010db80000000000000000000000ab 02 40 20 01     eth0
20010db8000000000000000000000002 02 40 20 20     eth0
20010db8000000000000000000000003 02 40 20 40     eth0
fe800000000000000000000000000001 02 40 20 80     eth0
`

func TestParseIfInet6(t *testing.T) {
	got := parseIfInet6(strings.NewReader(ifInet6Sample))

	cases := map[string]uint32{
		"2001:db8::1":  0x00,
		"2001:db8::ab": 0x01, // temporary
		"2001:db8::2":  0x20, // deprecated
		"2001:db8::3":  0x40, // tentative
		"fe80::1":      0x80, // permanent link-local
	}
	for addr, want := range cases {
		flags, ok := got[addr]
		if !ok {
			t.Fatalf("address %s not parsed, got %v", addr, got)
		}
		if flags != want {
			t.Fatalf("%s: flags = %#x, want %#x", addr, flags, want)
		}
	}
}

func TestFilterUnstableIPv6(t *testing.T) {
	dir := t.TempDir()
	procPath := filepath.Join(dir, "if_inet6")
	if err := os.WriteFile(procPath, []byte(ifInet6Sample), 0600); err != nil {
		t.Fatal(err)
	}
	old := ifInet6Path
	ifInet6Path = procPath
	defer func() { ifInet6Path = old }()
	resetEmailStateForTest()

	// 稳定地址保留，临时/弃用/待定的剔除
	got := filterUnstableIPv6("2001:db8::ab,2001:db8::2,2001:db8::1,2001:db8::3")
	if got != "2001:db8::1" {
		t.Fatalf("filterUnstableIPv6 = %q, want %q", got, "2001:db8::1")
	}

	// 全是临时地址时返回空串
	if got := filterUnstableIPv6("2001:db8::ab"); got != "" {
		t.Fatalf("expected empty result, got %q", got)
	}
}

func TestFilterUnstableIPv6WithoutProc(t *testing.T) {
	old := ifInet6Path
	ifInet6Path = filepath.Join(t.TempDir(), "not-exists")
	defer func() { ifInet6Path = old }()

	raw := "2001:db8::1,2001:db8::ab"
	if got := filterUnstableIPv6(raw); got != raw {
		t.Fatalf("without /proc the input should pass through, got %q", got)
	}
}

// --- SMTP 超时 ---

func TestSendHonorsSessionTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer ln.Close()

	// 接受连接、发完问候语后彻底不回包，模拟"半死"的 SMTP 服务器
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("220 stall.example ESMTP\r\n"))
		time.Sleep(5 * time.Second)
		_ = conn.Close()
	}()

	old := smtpSessionTimeout
	smtpSessionTimeout = 600 * time.Millisecond
	defer func() { smtpSessionTimeout = old }()

	addr := ln.Addr().(*net.TCPAddr)
	email := &Email{
		SMTPHost: "127.0.0.1",
		SMTPPort: addr.Port,
		From:     "a@x.com",
		To:       "b@y.com",
		Encrypt:  "none",
	}

	start := time.Now()
	err = email.Send("subject", "body")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error from the stalled server")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("send blocked for %v, session deadline did not apply", elapsed)
	}
}

// --- 多源 fallback ---

func TestGetIpv4AddrFromUrlFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/html":
			_, _ = w.Write([]byte("<html><body>maintenance</body></html>"))
		case "/good":
			_, _ = w.Write([]byte("203.0.113.7\n"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	conf := &DnsConfig{}
	conf.Ipv4.URL = srv.URL + "/html, " + srv.URL + "/good"

	if got := conf.getIpv4AddrFromUrl(); got != "203.0.113.7" {
		t.Fatalf("expected fallback to the second source, got %q", got)
	}
}

func TestGetIpv4AddrFromUrlKeepsTryingAfterHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/good" {
			_, _ = w.Write([]byte("198.51.100.9"))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	conf := &DnsConfig{}
	conf.Ipv4.URL = srv.URL + "/broken, " + srv.URL + "/good"

	if got := conf.getIpv4AddrFromUrl(); got != "198.51.100.9" {
		t.Fatalf("expected fallback after HTTP error, got %q", got)
	}
}

func TestGetIpv4AddrFromUrlReturnsEmptyWhenAllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	conf := &DnsConfig{}
	conf.Ipv4.URL = srv.URL + "/a, " + srv.URL + "/b"

	if got := conf.getIpv4AddrFromUrl(); got != "" {
		t.Fatalf("expected empty result, got %q", got)
	}
}

// --- 变更历史 ---

func TestEmailHistoryAppendAndSummarize(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	_ = os.Remove(emailHistoryPath())

	AppendEmailHistory(EmailHistoryEntry{Kind: historyKindBaseline, IPv4: "1.1.1.1"})
	AppendEmailHistory(EmailHistoryEntry{Kind: historyKindChange, IPv4: "2.2.2.2", PrevIPv4: "1.1.1.1"})

	summary := GetEmailHistorySummary(5, "zh")
	if summary.Total != 2 {
		t.Fatalf("total = %d, want 2", summary.Total)
	}
	if len(summary.Recent) != 2 {
		t.Fatalf("recent = %d, want 2", len(summary.Recent))
	}
	// 新的在前
	if summary.Recent[0].IPv4 != "2.2.2.2" {
		t.Fatalf("newest entry first expected, got %q", summary.Recent[0].IPv4)
	}
	if summary.Recent[0].KindText != "IP 变更" {
		t.Fatalf("kind text = %q", summary.Recent[0].KindText)
	}
	if summary.Recent[0].TimeText == "" {
		t.Fatal("time text should be filled")
	}
	if summary.LastTime == "" {
		t.Fatal("last time should be filled")
	}
}

func TestEmailHistoryIsTrimmed(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	_ = os.Remove(emailHistoryPath())

	for i := 0; i < maxEmailHistory+5; i++ {
		AppendEmailHistory(EmailHistoryEntry{Kind: historyKindChange, IPv4: "10.0.0.1"})
	}

	lines := readEmailHistoryLines(emailHistoryPath())
	if len(lines) != maxEmailHistory {
		t.Fatalf("history kept %d lines, want %d", len(lines), maxEmailHistory)
	}
}

func TestEmailHistorySummaryOnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	_ = os.Remove(emailHistoryPath())

	summary := GetEmailHistorySummary(5, "en")
	if summary.Total != 0 || len(summary.Recent) != 0 || summary.LastTime != "" {
		t.Fatalf("expected empty summary, got %+v", summary)
	}
}

// --- 获取失败告警的状态机 ---

func TestFetchFailureTrackerAlertsOncePerOutage(t *testing.T) {
	var tr fetchFailureTracker

	for i := 1; i < fetchAlertThreshold; i++ {
		if alert, recovered := tr.observe(true); alert || recovered {
			t.Fatalf("round %d should not notify (alert=%v recovered=%v)", i, alert, recovered)
		}
	}

	alert, recovered := tr.observe(true)
	if !alert || recovered {
		t.Fatalf("round %d should alert (alert=%v recovered=%v)", fetchAlertThreshold, alert, recovered)
	}

	if alert, _ := tr.observe(true); alert {
		t.Fatal("a single outage must not alert twice")
	}

	if _, recovered := tr.observe(false); !recovered {
		t.Fatal("recovery should be reported once")
	}
	if alert, recovered := tr.observe(false); alert || recovered {
		t.Fatal("healthy rounds must stay silent")
	}
}

func TestFetchFailureTrackerResetsAfterRecovery(t *testing.T) {
	var tr fetchFailureTracker
	for i := 0; i < fetchAlertThreshold; i++ {
		tr.observe(true)
	}
	tr.observe(false) // 恢复

	// 新一轮故障需要重新累计到阈值
	for i := 1; i < fetchAlertThreshold; i++ {
		if alert, _ := tr.observe(true); alert {
			t.Fatalf("round %d of the second outage should not alert yet", i)
		}
	}
	if alert, _ := tr.observe(true); !alert {
		t.Fatal("second outage should alert again after the threshold")
	}
}
