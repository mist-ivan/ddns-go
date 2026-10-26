package config

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeessy2/ddns-go/v6/util"
)

func TestParseRecipients(t *testing.T) {
	got, err := parseRecipients(" a@x.com, b@y.com;c@z.com\n\nd@w.com ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a@x.com", "b@y.com", "c@z.com", "d@w.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].Address != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if _, err := parseRecipients(""); err == nil {
		t.Fatal("empty input should be rejected")
	}
}

func TestParseRecipientsWithDisplayName(t *testing.T) {
	got, err := parseRecipients(`"Doe, John" <john@example.com>, jane@example.com`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Address != "john@example.com" || got[1].Address != "jane@example.com" {
		t.Fatalf("unexpected parsed recipients: %+v", got)
	}
}

func TestParseSenderKeepsDisplayNameForHeader(t *testing.T) {
	envelope, header, err := parseSender("ddns-go <you@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	if envelope != "you@example.com" {
		t.Fatalf("envelope = %q, want you@example.com", envelope)
	}
	if !strings.Contains(header, "ddns-go") || !strings.Contains(header, "<you@example.com>") {
		t.Fatalf("header = %q", header)
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

func TestComposeIPChangeMailIPv6Only(t *testing.T) {
	conf := &Config{Lang: "zh"}
	subject, _ := composeIPChangeMail(conf, emailState{LastIPv6: "2001:db8::1"}, "", "2001:db8::2", false)
	if !strings.Contains(subject, "IPv6") || !strings.Contains(subject, "2001:db8::2") {
		t.Fatalf("IPv6-only subject is wrong: %s", subject)
	}
	if strings.Contains(subject, "- -> -") {
		t.Fatalf("IPv6-only subject must not contain '- -> -': %s", subject)
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

	// 常见误填：把 host:port 整体填进服务器框
	hostPort := &Email{SMTPHost: "smtp.x.com:465", To: "a@x.com", From: "b@x.com"}
	if err := hostPort.Validate(); err == nil {
		t.Fatal("host:port should be rejected")
	}

	// 未知加密方式不能静默降级为明文
	badEncrypt := &Email{SMTPHost: "smtp.x.com", To: "a@x.com", From: "b@x.com", Encrypt: "foo"}
	if err := badEncrypt.Validate(); err == nil {
		t.Fatal("unknown encrypt should be rejected")
	}

	// 带显示名的 From/To 是合法配置
	displayName := &Email{SMTPHost: "smtp.x.com", To: `"Alice" <a@x.com>`, From: "ddns-go <b@x.com>"}
	if err := displayName.Validate(); err != nil {
		t.Fatalf("display-name address rejected: %v", err)
	}

	// 非 localhost 的明文连接不允许携带 SMTP 凭据
	plain := &Email{
		SMTPHost: "smtp.x.com", SMTPPort: 25, To: "a@x.com", From: "b@x.com",
		Username: "user@x.com", Password: "secret", Encrypt: "none",
	}
	if err := plain.Validate(); err == nil {
		t.Fatal("cleartext SMTP auth should be rejected")
	}

	// localhost 的明文连接只用于本地测试，允许
	local := &Email{
		SMTPHost: "127.0.0.1", SMTPPort: 2525, To: "a@x.com", From: "b@x.com",
		Username: "user@x.com", Password: "secret", Encrypt: "none",
	}
	if err := local.Validate(); err != nil {
		t.Fatalf("localhost cleartext test config rejected: %v", err)
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

// TestSendUsesBareEnvelopeAndDoesNotRetryAfterQuit 覆盖两个回归点：
// 1. From 的显示名只能出现在邮件头，MAIL FROM 必须是裸地址；
// 2. DATA 已被 250 接收后，QUIT 断连不能触发整封重发。
func TestSendUsesBareEnvelopeAndDoesNotRetryAfterQuit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer ln.Close()

	var (
		mu           sync.Mutex
		mailFrom     []string
		messageCount int
	)
	quitSeen := make(chan struct{})
	var quitOnce sync.Once

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				_, _ = fmt.Fprint(c, "220 test-smtp\r\n")
				inData := false
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")
					if inData {
						if line == "." {
							inData = false
							mu.Lock()
							messageCount++
							mu.Unlock()
							_, _ = fmt.Fprint(c, "250 queued\r\n")
						}
						continue
					}
					upper := strings.ToUpper(line)
					switch {
					case strings.HasPrefix(upper, "EHLO"):
						_, _ = fmt.Fprint(c, "250-test\r\n250-AUTH PLAIN LOGIN\r\n250 OK\r\n")
					case strings.HasPrefix(upper, "AUTH"):
						_, _ = fmt.Fprint(c, "235 ok\r\n")
					case strings.HasPrefix(upper, "MAIL FROM"):
						mu.Lock()
						mailFrom = append(mailFrom, line)
						mu.Unlock()
						_, _ = fmt.Fprint(c, "250 ok\r\n")
					case strings.HasPrefix(upper, "RCPT TO"):
						_, _ = fmt.Fprint(c, "250 ok\r\n")
					case upper == "DATA":
						inData = true
						_, _ = fmt.Fprint(c, "354 go\r\n")
					case upper == "QUIT":
						// 模拟服务器收到 QUIT 后直接断开，不回 221
						quitOnce.Do(func() { close(quitSeen) })
						return
					default:
						_, _ = fmt.Fprint(c, "250 ok\r\n")
					}
				}
			}(conn)
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	email := &Email{
		Enable:   true,
		SMTPHost: "127.0.0.1",
		SMTPPort: addr.Port,
		Username: "test@example.com",
		Password: "secret",
		From:     "ddns-go <test@example.com>",
		To:       `"Alice" <alice@example.com>`,
		Encrypt:  "none",
	}
	if err := email.Send("subject", "body"); err != nil {
		t.Fatalf("Send returned error after successful DATA: %v", err)
	}

	select {
	case <-quitSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive QUIT")
	}

	mu.Lock()
	defer mu.Unlock()
	if messageCount != 1 {
		t.Fatalf("message accepted %d times, want 1 (QUIT failure must not resend)", messageCount)
	}
	if len(mailFrom) != 1 || mailFrom[0] != "MAIL FROM:<test@example.com>" {
		t.Fatalf("unexpected MAIL FROM: %v", mailFrom)
	}
}

// --- 邮件状态机 ---

type smtpTestServer struct {
	ln        net.Listener
	failFirst bool
	mu        sync.Mutex
	conns     int
	messages  int
	mailFrom  []string
}

func newSMTPTestServer(t *testing.T, failFirst bool) *smtpTestServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	s := &smtpTestServer{ln: ln, failFirst: failFirst}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *smtpTestServer) port() int {
	return s.ln.Addr().(*net.TCPAddr).Port
}

func (s *smtpTestServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *smtpTestServer) handle(c net.Conn) {
	defer c.Close()
	s.mu.Lock()
	s.conns++
	connNo := s.conns
	s.mu.Unlock()
	if s.failFirst && connNo == 1 {
		return
	}

	br := bufio.NewReader(c)
	_, _ = fmt.Fprint(c, "220 test-smtp\r\n")
	inData := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				s.mu.Lock()
				s.messages++
				s.mu.Unlock()
				_, _ = fmt.Fprint(c, "250 queued\r\n")
			}
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			_, _ = fmt.Fprint(c, "250-test\r\n250-AUTH PLAIN LOGIN\r\n250 OK\r\n")
		case strings.HasPrefix(upper, "AUTH"):
			_, _ = fmt.Fprint(c, "235 ok\r\n")
		case strings.HasPrefix(upper, "MAIL FROM"):
			s.mu.Lock()
			s.mailFrom = append(s.mailFrom, line)
			s.mu.Unlock()
			_, _ = fmt.Fprint(c, "250 ok\r\n")
		case strings.HasPrefix(upper, "RCPT TO"):
			_, _ = fmt.Fprint(c, "250 ok\r\n")
		case upper == "DATA":
			inData = true
			_, _ = fmt.Fprint(c, "354 go\r\n")
		case upper == "QUIT":
			_, _ = fmt.Fprint(c, "221 bye\r\n")
			return
		default:
			_, _ = fmt.Fprint(c, "250 ok\r\n")
		}
	}
}

func (s *smtpTestServer) stats() (int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messages, append([]string{}, s.mailFrom...)
}

func waitForMessages(t *testing.T, s *smtpTestServer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := s.stats(); n >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	n, _ := s.stats()
	t.Fatalf("got %d messages, want at least %d", n, want)
}

func TestNotifyIPChangeEmailSendsOncePerChange(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	srv := newSMTPTestServer(t, false)
	conf := &Config{Lang: "zh", Email: Email{
		Enable:   true,
		SMTPHost: "127.0.0.1",
		SMTPPort: srv.port(),
		From:     "ddns-go <test@example.com>",
		To:       "alice@example.com",
		Encrypt:  "none",
		OnStart:  true,
	}}
	status := FetchStatus{IPv4Expected: true, IPv4OK: true}

	NotifyIPChangeEmail(conf, "1.2.3.4", "", status)
	waitForMessages(t, srv, 1)

	// IP 未变，静默
	NotifyIPChangeEmail(conf, "1.2.3.4", "", status)
	time.Sleep(100 * time.Millisecond)
	if n, _ := srv.stats(); n != 1 {
		t.Fatalf("unchanged IP sent %d messages, want 1", n)
	}

	// IP 变化，只发一封
	NotifyIPChangeEmail(conf, "5.6.7.8", "", status)
	waitForMessages(t, srv, 2)
	time.Sleep(100 * time.Millisecond)
	n, mailFrom := srv.stats()
	if n != 2 {
		t.Fatalf("got %d messages, want 2", n)
	}
	if len(mailFrom) != 2 || mailFrom[0] != "MAIL FROM:<test@example.com>" {
		t.Fatalf("unexpected MAIL FROM: %v", mailFrom)
	}
}

func TestHandleFetchHealthRetriesRecovery(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()
	defer func() {
		fetchTrackLock.Lock()
		fetchTracker = fetchFailureTracker{}
		fetchTrackLock.Unlock()
	}()

	srv := newSMTPTestServer(t, true)
	conf := &Config{Lang: "zh", Email: Email{
		Enable:              true,
		SMTPHost:            "127.0.0.1",
		SMTPPort:            srv.port(),
		From:                "a@example.com",
		To:                  "b@example.com",
		Encrypt:             "none",
		AlertOnFetchFailure: true,
	}}
	status := FetchStatus{IPv4Expected: true, IPv4OK: true}

	fetchTrackLock.Lock()
	fetchTracker = fetchFailureTracker{consecutive: fetchAlertThreshold, alerted: true}
	fetchTrackLock.Unlock()

	if got := handleFetchHealth(conf, status, "1.2.3.4", ""); got {
		t.Fatal("first recovery send should fail")
	}
	fetchTrackLock.Lock()
	pending := fetchTracker.alerted
	fetchTrackLock.Unlock()
	if !pending {
		t.Fatal("failed recovery must keep alerted=true for retry")
	}

	if got := handleFetchHealth(conf, status, "1.2.3.4", ""); !got {
		t.Fatal("second recovery send should succeed")
	}
	waitForMessages(t, srv, 1)
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

// --- v0.4: 历史清空与状态快照 ---

func TestClearEmailHistory(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	_ = os.Remove(emailHistoryPath())

	AppendEmailHistory(EmailHistoryEntry{Kind: historyKindBaseline, IPv4: "1.1.1.1"})
	AppendEmailHistory(EmailHistoryEntry{Kind: historyKindChange, IPv4: "2.2.2.2"})
	if got := GetEmailHistorySummary(5, "zh").Total; got != 2 {
		t.Fatalf("total = %d, want 2", got)
	}

	if err := ClearEmailHistory(); err != nil {
		t.Fatalf("clear failed: %v", err)
	}
	if got := GetEmailHistorySummary(5, "zh").Total; got != 0 {
		t.Fatalf("after clear total = %d, want 0", got)
	}
	// 清空不存在的文件也应返回 nil
	if err := ClearEmailHistory(); err != nil {
		t.Fatalf("clearing twice should be fine: %v", err)
	}
}

func TestGetEmailStateSnapshot(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))

	storeEmailState(emailState{LastIPv4: "9.9.9.9", LastIPv6: "2001:db8::1", LastSent: 1700000000})

	ipv4, ipv6, lastSent := GetEmailStateSnapshot()
	if ipv4 != "9.9.9.9" || ipv6 != "2001:db8::1" || lastSent != 1700000000 {
		t.Fatalf("snapshot = %q %q %d", ipv4, ipv6, lastSent)
	}
}
