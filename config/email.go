package config

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jeessy2/ddns-go/v6/util"
)

// Email 邮件通知配置。随 Config 一起序列化到 ddns-go 的 yaml 配置文件。
type Email struct {
	// Enable 是否启用邮件通知
	Enable bool
	// SMTPHost SMTP 服务器地址，如 smtp.qq.com
	SMTPHost string
	// SMTPPort SMTP 端口，465/587/25
	SMTPPort int
	// Username SMTP 登录账号，留空表示匿名发送
	Username string
	// Password SMTP 密码或授权码
	Password string
	// From 发件人地址，留空时使用 Username
	From string
	// To 收件人，多个用逗号或分号分隔
	To string
	// Encrypt 加密方式：ssl(隐式 TLS) / starttls / none
	Encrypt string
	// OnStart 首次运行（没有历史 IP 记录）时也发送一封基线邮件
	OnStart bool
	// AlertOnFetchFailure 连续多轮取不到公网 IP 时发送告警邮件
	AlertOnFetchFailure bool
}

// FetchStatus 本轮公网 IP 的期望与获取结果。
// 分开记录 IPv4/IPv6，避免“只启用/只配置了一族”时误报整个公网 IP 获取失败。
type FetchStatus struct {
	IPv4Expected bool
	IPv4OK       bool
	IPv6Expected bool
	IPv6OK       bool
}

// Failed 是否至少有一个已配置的地址族在本轮获取失败
func (s FetchStatus) Failed() bool {
	return (s.IPv4Expected && !s.IPv4OK) || (s.IPv6Expected && !s.IPv6OK)
}

func (s FetchStatus) failedFamilies() []string {
	var out []string
	if s.IPv4Expected && !s.IPv4OK {
		out = append(out, "IPv4")
	}
	if s.IPv6Expected && !s.IPv6OK {
		out = append(out, "IPv6")
	}
	return out
}

func (s FetchStatus) expectedFamilies() []string {
	var out []string
	if s.IPv4Expected {
		out = append(out, "IPv4")
	}
	if s.IPv6Expected {
		out = append(out, "IPv6")
	}
	return out
}

// smtpSessionTimeout 单次 SMTP 会话的总超时（从连接建立起算到会话结束）
var smtpSessionTimeout = 45 * time.Second

// emailState 记录上次成功通知过的 IP，用于判断"IP 是否真的变了"
type emailState struct {
	LastIPv4 string `json:"lastIPv4"`
	LastIPv6 string `json:"lastIPv6"`
	LastSent int64  `json:"lastSent"`
}

// loginAuth 实现 SMTP AUTH LOGIN，部分邮件服务商只支持该方式
type loginAuth struct {
	username string
	password string
}

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	challenge := strings.ToLower(strings.TrimSpace(string(fromServer)))
	switch {
	case strings.HasPrefix(challenge, "username"):
		return []byte(a.username), nil
	case strings.HasPrefix(challenge, "password"):
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("unexpected SMTP auth challenge: %s", fromServer)
	}
}

// smtpAuthError 标记“认证阶段”的错误，只有这类错误才值得换一种 AUTH 机制重试。
type smtpAuthError struct {
	err error
}

func (e *smtpAuthError) Error() string { return e.err.Error() }
func (e *smtpAuthError) Unwrap() error { return e.err }

func isAuthError(err error) bool {
	var authErr *smtpAuthError
	return errors.As(err, &authErr)
}

// normalizeSMTPHost 去掉 IPv6 字面量外层的方括号，便于 JoinHostPort/证书校验。
func normalizeSMTPHost(raw string) string {
	host := strings.TrimSpace(raw)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	return host
}

// normalizeEncrypt 校验/推断加密方式。留空时按端口推断。
func normalizeEncrypt(raw string, port int) (string, error) {
	encrypt := strings.ToLower(strings.TrimSpace(raw))
	if encrypt == "" {
		switch port {
		case 465:
			encrypt = "ssl"
		case 587:
			encrypt = "starttls"
		default:
			encrypt = "none"
		}
	}
	switch encrypt {
	case "ssl", "tls", "starttls", "none":
		return encrypt, nil
	default:
		return "", fmt.Errorf("不支持的加密方式: %s", raw)
	}
}

// isLocalhost 判断 SMTP 主机是否为本机；只有本机明文连接才允许携带凭据（便于本地测试）。
func isLocalhost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// parseSender 解析发件人，返回 SMTP 信封地址（裸地址）和邮件头显示值。
func parseSender(raw string) (envelope, header string, err error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("发件人地址不合法: %w", err)
	}
	return addr.Address, addr.String(), nil
}

// parseRecipients 解析收件人，兼容逗号、分号、换行分隔以及带显示名的地址。
func parseRecipients(raw string) ([]*mail.Address, error) {
	normalized := strings.NewReplacer("\r\n", ",", "\r", ",", "\n", ",", ";", ",").Replace(raw)
	normalized = strings.TrimSpace(normalized)
	for strings.Contains(normalized, ",,") {
		normalized = strings.ReplaceAll(normalized, ",,", ",")
	}
	normalized = strings.Trim(normalized, " \t,")
	if normalized == "" {
		return nil, errors.New("收件人不能为空")
	}
	list, err := mail.ParseAddressList(normalized)
	if err != nil {
		return nil, fmt.Errorf("收件人地址不合法: %w", err)
	}
	if len(list) == 0 {
		return nil, errors.New("收件人不能为空")
	}
	return list, nil
}

// Validate 检查配置是否足以发信
func (e *Email) Validate() error {
	host := normalizeSMTPHost(e.SMTPHost)
	if host == "" {
		return errors.New("SMTP 服务器不能为空")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return errors.New("SMTP 服务器不能包含空白字符")
	}
	// 常见误填：把 host:port 整体填进“服务器”输入框
	if strings.Count(host, ":") == 1 {
		if _, _, err := net.SplitHostPort(host); err == nil {
			return errors.New("SMTP 服务器只填主机名，端口请填在端口输入框")
		}
	}
	if e.SMTPPort < 0 || e.SMTPPort > 65535 {
		return fmt.Errorf("SMTP 端口不合法: %d", e.SMTPPort)
	}
	port := e.SMTPPort
	if port == 0 {
		port = 465
	}
	if _, err := normalizeEncrypt(e.Encrypt, port); err != nil {
		return err
	}

	from := strings.TrimSpace(e.From)
	if from == "" {
		from = strings.TrimSpace(e.Username)
	}
	if from == "" {
		return errors.New("发件人不能为空")
	}
	if _, _, err := parseSender(from); err != nil {
		return err
	}
	if _, err := parseRecipients(e.To); err != nil {
		return err
	}
	return nil
}

// Send 发送一封纯文本邮件
func (e *Email) Send(subject, body string) error {
	if err := e.Validate(); err != nil {
		return err
	}

	host := normalizeSMTPHost(e.SMTPHost)
	port := e.SMTPPort
	if port <= 0 {
		port = 465
	}
	encrypt, err := normalizeEncrypt(e.Encrypt, port)
	if err != nil {
		return err
	}

	fromRaw := strings.TrimSpace(e.From)
	if fromRaw == "" {
		fromRaw = strings.TrimSpace(e.Username)
	}
	envelopeFrom, headerFrom, err := parseSender(fromRaw)
	if err != nil {
		return err
	}
	toAddrs, err := parseRecipients(e.To)
	if err != nil {
		return err
	}
	envelopeTo := make([]string, 0, len(toAddrs))
	headerTo := make([]string, 0, len(toAddrs))
	for _, addr := range toAddrs {
		envelopeTo = append(envelopeTo, addr.Address)
		headerTo = append(headerTo, addr.String())
	}

	msg := buildMailMessage(headerFrom, headerTo, subject, body)
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var auth smtp.Auth
	if e.Username != "" {
		if encrypt == "none" && !isLocalhost(host) {
			return errors.New("拒绝在未加密的 SMTP 连接上发送账号密码，请改用 SSL 或 STARTTLS")
		}
		auth = smtp.PlainAuth("", e.Username, e.Password, host)
	}

	accepted, err := e.deliver(addr, host, encrypt, auth, envelopeFrom, envelopeTo, msg)
	if err != nil && !accepted && auth != nil && isAuthError(err) {
		// 仅认证机制不被接受时回退 AUTH LOGIN；其他错误不重发，避免重复投递。
		util.Log("SMTP AUTH PLAIN 失败，尝试使用 LOGIN 方式重试: %s", err)
		_, err = e.deliver(addr, host, encrypt, &loginAuth{e.Username, e.Password}, envelopeFrom, envelopeTo, msg)
	}
	return err
}

// deliver 建立一次 SMTP 会话并投递。
// accepted 表示服务器已对 DATA 内容返回 250（邮件已接收）；此后即使 QUIT 失败也不应重发。
func (e *Email) deliver(addr, host, encrypt string, auth smtp.Auth, from string, to []string, msg []byte) (accepted bool, err error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	if encrypt == "ssl" || encrypt == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return false, fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	defer conn.Close()

	// 单次会话硬超时：SMTP 服务器半死时读写不能无限阻塞主循环。
	_ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout))

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return false, fmt.Errorf("初始化 SMTP 会话失败: %w", err)
	}
	defer client.Close()

	if encrypt == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return false, errors.New("SMTP 服务器不支持 STARTTLS")
		}
		if err = client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return false, fmt.Errorf("STARTTLS 失败: %w", err)
		}
	}

	if auth != nil {
		if ok, _ := client.Extension("AUTH"); ok {
			if err = client.Auth(auth); err != nil {
				return false, &smtpAuthError{err: err}
			}
		}
	}

	if err = client.Mail(from); err != nil {
		return false, fmt.Errorf("MAIL FROM 被拒绝: %w", err)
	}
	for _, rcpt := range to {
		if err = client.Rcpt(rcpt); err != nil {
			return false, fmt.Errorf("RCPT TO(%s) 被拒绝: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return false, fmt.Errorf("DATA 被拒绝: %w", err)
	}
	if _, err = w.Write(msg); err != nil {
		return false, fmt.Errorf("写入邮件内容失败: %w", err)
	}
	if err = w.Close(); err != nil {
		return false, fmt.Errorf("提交邮件内容失败: %w", err)
	}
	accepted = true

	if err = client.Quit(); err != nil {
		// 邮件已被服务器接收，QUIT 只是关闭会话；不能判为发送失败，否则会重复投递。
		util.Log("邮件已投递，关闭 SMTP 会话时出错（忽略）: %s", err)
	}
	return true, nil
}

// NotifyIPChangeEmail 当公网 IP 与上次通知过的 IP 不同时发信。
// 首次运行（没有历史记录）时，若开启 OnStart 则发送一封基线邮件。
func NotifyIPChangeEmail(conf *Config, ipv4, ipv6 string, fetchStatus FetchStatus) {
	if conf == nil || !conf.Email.Enable {
		return
	}
	if strings.TrimSpace(conf.Email.SMTPHost) == "" || strings.TrimSpace(conf.Email.To) == "" {
		return
	}

	ipv4 = strings.TrimSpace(ipv4)
	ipv6 = strings.TrimSpace(ipv6)
	// 剔除临时/已弃用的 IPv6（SLAAC 隐私地址），它们会按小时级轮换，
	// 若参与比对会造成大量无意义的变更邮件
	if ipv6 != "" {
		ipv6 = filterUnstableIPv6(ipv6)
	}

	// 取 IP 失败的告警独立于变更通知：即使本轮一个地址都没拿到，也要能报出去
	recoveredNotice := handleFetchHealth(conf, fetchStatus, ipv4, ipv6)

	if ipv4 == "" && ipv6 == "" {
		return
	}

	state := loadEmailState()
	firstRun := state.LastIPv4 == "" && state.LastIPv6 == ""

	// 空值表示本轮没取到地址（可能是瞬时失败），不参与比对也不覆盖历史值
	changed := false
	if ipv4 != "" && state.LastIPv4 != ipv4 {
		changed = true
	}
	if ipv6 != "" && state.LastIPv6 != ipv6 {
		changed = true
	}
	if !changed {
		return
	}

	next := state
	if ipv4 != "" {
		next.LastIPv4 = ipv4
	}
	if ipv6 != "" {
		next.LastIPv6 = ipv6
	}

	if firstRun && !conf.Email.OnStart {
		// 仅记录基线，不发信
		storeEmailState(next)
		util.Log("邮件通知: 已记录初始 IP 基线 (%s / %s)，本次不发送", displayOrDash(ipv4), displayOrDash(ipv6))
		return
	}

	if firstRun && recoveredNotice {
		// 恢复通知里已经带了当前 IP，不再重复发一封基线邮件
		next.LastSent = time.Now().Unix()
		storeEmailState(next)
		return
	}

	subject, body := composeIPChangeMail(conf, state, ipv4, ipv6, firstRun)
	if err := conf.Email.Send(subject, body); err != nil {
		util.Log("邮件通知发送失败: %s", err)
		// 不更新状态，下一轮继续重试
		return
	}

	next.LastSent = time.Now().Unix()
	storeEmailState(next)

	kind := historyKindChange
	if firstRun {
		kind = historyKindBaseline
	}
	AppendEmailHistory(EmailHistoryEntry{
		Kind:     kind,
		IPv4:     ipv4,
		PrevIPv4: state.LastIPv4,
		IPv6:     ipv6,
		PrevIPv6: state.LastIPv6,
	})
	if firstRun {
		util.Log("邮件通知: 已发送初始 IP 基线邮件 (%s / %s)", displayOrDash(ipv4), displayOrDash(ipv6))
	} else {
		util.Log("邮件通知: IP 已变更，通知邮件发送成功 (%s / %s)", displayOrDash(ipv4), displayOrDash(ipv6))
	}
}

// fetchAlertThreshold 连续多少轮取不到 IP 才告警，避免网络抖动造成误报
const fetchAlertThreshold = 3

// fetchFailureTracker 记录连续失败次数，保证一次故障只告警一次
type fetchFailureTracker struct {
	consecutive int
	alerted     bool
}

func (t *fetchFailureTracker) observe(failed bool) (alert bool, recovered bool) {
	if !failed {
		recovered = t.alerted
		t.consecutive = 0
		t.alerted = false
		return false, recovered
	}

	t.consecutive++
	if t.consecutive >= fetchAlertThreshold && !t.alerted {
		t.alerted = true
		return true, false
	}
	return false, false
}

var (
	fetchTrackLock sync.Mutex
	fetchTracker   fetchFailureTracker
)

// handleFetchHealth 处理"取不到公网 IP"的告警与恢复通知。
// 返回值表示本轮是否成功发出了"已恢复"通知（调用方据此避免重复发基线邮件）。
func handleFetchHealth(conf *Config, status FetchStatus, ipv4, ipv6 string) bool {
	if !conf.Email.AlertOnFetchFailure {
		fetchTrackLock.Lock()
		fetchTracker = fetchFailureTracker{}
		fetchTrackLock.Unlock()
		return false
	}

	fetchTrackLock.Lock()
	alert, recovered := fetchTracker.observe(status.Failed())
	consecutive := fetchTracker.consecutive
	fetchTrackLock.Unlock()

	if alert {
		subject, body := composeFetchAlertMail(conf, consecutive, status)
		if err := conf.Email.Send(subject, body); err != nil {
			util.Log("邮件通知: IP 获取失败告警发送失败: %s", err)
			// 允许下一轮重试，避免一次发送失败就把告警吞掉
			fetchTrackLock.Lock()
			fetchTracker.alerted = false
			fetchTrackLock.Unlock()
			return false
		}
		AppendEmailHistory(EmailHistoryEntry{Kind: historyKindFetchFailed, IPv4: ipv4, IPv6: ipv6})
		util.Log("邮件通知: 连续 %d 轮未能获取 IP, 已发送告警邮件", consecutive)
	}

	if recovered {
		subject, body := composeRecoveredMail(conf, status, ipv4, ipv6)
		if err := conf.Email.Send(subject, body); err != nil {
			util.Log("邮件通知: IP 获取恢复通知发送失败: %s", err)
			// observe(false) 刚刚清掉了 alerted；这里必须还原，
			// 否则下一轮健康时不会再触发恢复通知，通知就永久丢失了。
			fetchTrackLock.Lock()
			fetchTracker.alerted = true
			fetchTracker.consecutive = 0
			fetchTrackLock.Unlock()
			return false
		}
		AppendEmailHistory(EmailHistoryEntry{Kind: historyKindRecovered, IPv4: ipv4, IPv6: ipv6})
		util.Log("邮件通知: IP 获取已恢复, 已发送恢复通知")
		return true
	}

	return false
}

// composeFetchAlertMail 生成"取不到 IP"的告警邮件
func composeFetchAlertMail(conf *Config, consecutive int, status FetchStatus) (subject, body string) {
	zh := strings.HasPrefix(strings.ToLower(conf.Lang), "zh")
	now := time.Now().Format("2006-01-02 15:04:05 -07:00")
	hostname, _ := os.Hostname()
	state := loadEmailState()
	families := status.failedFamilies()
	familyText := strings.Join(families, " / ")
	if familyText == "" {
		familyText = "公网 IP"
	}

	var b strings.Builder
	if zh {
		subject = fmt.Sprintf("[ddns-go] 连续 %d 轮未能获取 %s 地址", consecutive, familyText)
		fmt.Fprintf(&b, "ddns-go 已连续 %d 轮未能检测到 %s 地址，相关域名解析可能已经停止更新。\n\n", consecutive, familyText)
		fmt.Fprintf(&b, "时间: %s\n", now)
		fmt.Fprintf(&b, "主机: %s\n", hostname)
		fmt.Fprintf(&b, "最后已知 IPv4: %s\n", displayOrDash(state.LastIPv4))
		fmt.Fprintf(&b, "最后已知 IPv6: %s\n", displayOrDash(state.LastIPv6))
		fmt.Fprintf(&b, "\n请检查容器网络、%s 的 IP 查询接口是否可访问。恢复之后会再发一封通知。\n", familyText)
	} else {
		subject = fmt.Sprintf("[ddns-go] Failed to detect %s address for %d rounds", familyText, consecutive)
		fmt.Fprintf(&b, "ddns-go has failed to detect the %s address for %d consecutive rounds; related DNS records may be stale.\n\n", familyText, consecutive)
		fmt.Fprintf(&b, "Time: %s\n", now)
		fmt.Fprintf(&b, "Host: %s\n", hostname)
		fmt.Fprintf(&b, "Last known IPv4: %s\n", displayOrDash(state.LastIPv4))
		fmt.Fprintf(&b, "Last known IPv6: %s\n", displayOrDash(state.LastIPv6))
		fmt.Fprintf(&b, "\nCheck the container network and whether the %s IP lookup endpoints are reachable. A recovery notice will follow.\n", familyText)
	}
	b.WriteString("\n-- \nddns-go email notification\n")
	return subject, b.String()
}

// composeRecoveredMail 生成"已恢复"的邮件
func composeRecoveredMail(conf *Config, status FetchStatus, ipv4, ipv6 string) (subject, body string) {
	zh := strings.HasPrefix(strings.ToLower(conf.Lang), "zh")
	now := time.Now().Format("2006-01-02 15:04:05 -07:00")
	hostname, _ := os.Hostname()
	familyText := strings.Join(status.expectedFamilies(), " / ")
	if familyText == "" {
		familyText = "公网 IP"
	}

	var b strings.Builder
	if zh {
		subject = fmt.Sprintf("[ddns-go] %s 检测已恢复", familyText)
		fmt.Fprintf(&b, "ddns-go 已重新拿到 %s 地址，检测恢复正常。\n\n", familyText)
		fmt.Fprintf(&b, "时间: %s\n", now)
		fmt.Fprintf(&b, "主机: %s\n", hostname)
		fmt.Fprintf(&b, "当前 IPv4: %s\n", displayOrDash(ipv4))
		fmt.Fprintf(&b, "当前 IPv6: %s\n", displayOrDash(ipv6))
	} else {
		subject = fmt.Sprintf("[ddns-go] %s detection recovered", familyText)
		fmt.Fprintf(&b, "ddns-go can detect the %s address again.\n\n", familyText)
		fmt.Fprintf(&b, "Time: %s\n", now)
		fmt.Fprintf(&b, "Host: %s\n", hostname)
		fmt.Fprintf(&b, "IPv4: %s\n", displayOrDash(ipv4))
		fmt.Fprintf(&b, "IPv6: %s\n", displayOrDash(ipv6))
	}
	b.WriteString("\n-- \nddns-go email notification\n")
	return subject, b.String()
}

// filterUnstableIPv6 从逗号分隔的地址串里剔除本机标记为临时/已弃用/待定的 IPv6。
// 解析不到 /proc/net/if_inet6 时原样返回（例如容器内用 URL 获取 WAN 地址的场景）。
func filterUnstableIPv6(raw string) string {
	unstable := unstableIPv6Addrs()
	if len(unstable) == 0 {
		return raw
	}

	kept := make([]string, 0, 4)
	var dropped []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if ip := net.ParseIP(item); ip != nil && unstable[ip.String()] {
			dropped = append(dropped, item)
			continue
		}
		kept = append(kept, item)
	}

	if len(dropped) > 0 {
		logIgnoredIPv6(dropped)
	}
	sort.Strings(kept)
	return strings.Join(kept, ",")
}

// logIgnoredIPv6 同一组被忽略的地址只记一次日志，避免每轮刷屏
func logIgnoredIPv6(dropped []string) {
	sort.Strings(dropped)
	key := strings.Join(dropped, ",")

	emailStateLock.Lock()
	repeated := key == lastIgnoredIPv6
	lastIgnoredIPv6 = key
	emailStateLock.Unlock()

	if !repeated {
		util.Log("邮件通知: 忽略临时/已弃用的 IPv6 地址: %s", key)
	}
}

// composeIPChangeMail 生成通知邮件的标题和正文
func composeIPChangeMail(conf *Config, state emailState, ipv4, ipv6 string, firstRun bool) (subject, body string) {
	zh := strings.HasPrefix(strings.ToLower(conf.Lang), "zh")
	now := time.Now().Format("2006-01-02 15:04:05 -07:00")
	hostname, _ := os.Hostname()

	v4Changed := ipv4 != "" && state.LastIPv4 != ipv4
	v6Changed := ipv6 != "" && state.LastIPv6 != ipv6
	firstIP := ipv4
	if firstIP == "" {
		firstIP = ipv6
	}

	var b strings.Builder
	if zh {
		if firstRun {
			subject = fmt.Sprintf("[ddns-go] 启动基线: 当前 IP %s", displayOrDash(firstIP))
			b.WriteString("ddns-go 已启动，这是首次记录的 IP 基线。\n\n")
		} else {
			switch {
			case v4Changed && v6Changed:
				subject = fmt.Sprintf("[ddns-go] IP 地址变更: %s -> %s / IPv6 %s -> %s",
					displayOrDash(state.LastIPv4), displayOrDash(ipv4),
					displayOrDash(state.LastIPv6), displayOrDash(ipv6))
			case v6Changed:
				subject = fmt.Sprintf("[ddns-go] IPv6 地址变更: %s -> %s",
					displayOrDash(state.LastIPv6), displayOrDash(ipv6))
			default:
				subject = fmt.Sprintf("[ddns-go] IP 地址变更: %s -> %s",
					displayOrDash(state.LastIPv4), displayOrDash(ipv4))
			}
			b.WriteString("ddns-go 检测到公网 IP 地址发生变化。\n\n")
		}
		fmt.Fprintf(&b, "时间: %s\n", now)
		fmt.Fprintf(&b, "主机: %s\n", hostname)
		fmt.Fprintf(&b, "IPv4: %s\n", displayOrDash(ipv4))
		if !firstRun {
			fmt.Fprintf(&b, "  上次: %s\n", displayOrDash(state.LastIPv4))
		}
		fmt.Fprintf(&b, "IPv6: %s\n", displayOrDash(ipv6))
		if !firstRun {
			fmt.Fprintf(&b, "  上次: %s\n", displayOrDash(state.LastIPv6))
		}
	} else {
		if firstRun {
			subject = fmt.Sprintf("[ddns-go] Baseline IP: %s", displayOrDash(firstIP))
			b.WriteString("ddns-go has started. This is the first recorded IP baseline.\n\n")
		} else {
			switch {
			case v4Changed && v6Changed:
				subject = fmt.Sprintf("[ddns-go] IP changed: %s -> %s / IPv6 %s -> %s",
					displayOrDash(state.LastIPv4), displayOrDash(ipv4),
					displayOrDash(state.LastIPv6), displayOrDash(ipv6))
			case v6Changed:
				subject = fmt.Sprintf("[ddns-go] IPv6 changed: %s -> %s",
					displayOrDash(state.LastIPv6), displayOrDash(ipv6))
			default:
				subject = fmt.Sprintf("[ddns-go] IP changed: %s -> %s",
					displayOrDash(state.LastIPv4), displayOrDash(ipv4))
			}
			b.WriteString("ddns-go detected that the public IP address has changed.\n\n")
		}
		fmt.Fprintf(&b, "Time: %s\n", now)
		fmt.Fprintf(&b, "Host: %s\n", hostname)
		fmt.Fprintf(&b, "IPv4: %s\n", displayOrDash(ipv4))
		if !firstRun {
			fmt.Fprintf(&b, "  previous: %s\n", displayOrDash(state.LastIPv4))
		}
		fmt.Fprintf(&b, "IPv6: %s\n", displayOrDash(ipv6))
		if !firstRun {
			fmt.Fprintf(&b, "  previous: %s\n", displayOrDash(state.LastIPv6))
		}
	}

	domains := configuredDomains(conf)
	if len(domains) > 0 {
		if zh {
			fmt.Fprintf(&b, "域名: %s\n", strings.Join(domains, ", "))
		} else {
			fmt.Fprintf(&b, "Domains: %s\n", strings.Join(domains, ", "))
		}
	}
	b.WriteString("\n-- \nddns-go email notification\n")
	return subject, b.String()
}

// configuredDomains 汇总配置中的所有域名
func configuredDomains(conf *Config) []string {
	seen := map[string]bool{}
	var domains []string
	for _, dc := range conf.DnsConf {
		for _, d := range append(append([]string{}, dc.Ipv4.Domains...), dc.Ipv6.Domains...) {
			d = strings.TrimSpace(d)
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			domains = append(domains, d)
		}
	}
	return domains
}

func displayOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// buildMailMessage 构造 MIME 报文（正文 base64，避免中文与换行问题）
func buildMailMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	writeHeaderLine(&b, "From", from)
	writeHeaderLine(&b, "To", strings.Join(to, ", "))
	writeHeaderLine(&b, "Subject", mime.QEncoding.Encode("utf-8", sanitizeHeaderValue(subject)))
	writeHeaderLine(&b, "Date", time.Now().Format(time.RFC1123Z))
	writeHeaderLine(&b, "MIME-Version", "1.0")
	writeHeaderLine(&b, "Content-Type", "text/plain; charset=UTF-8")
	writeHeaderLine(&b, "Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")

	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76])
		b.WriteString("\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded)
	b.WriteString("\r\n")
	return []byte(b.String())
}

// writeHeaderLine 写入一行头，禁止头注入
func writeHeaderLine(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(sanitizeHeaderValue(value))
	b.WriteString("\r\n")
}

// sanitizeHeaderValue 去掉换行，避免在头部注入额外的头
func sanitizeHeaderValue(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}

// emailStatePath 状态文件与配置文件放在同一目录，随容器挂载卷一起持久化
func emailStatePath() string {
	return util.GetConfigFilePath() + ".email-state.json"
}

var (
	// emailStateLock 只保护内存态、状态文件和日志去重标记的单次读写；
	// “读状态→发信→写状态”这一整段由 dns.RunOnce 的串行锁保证不并发。
	emailStateLock sync.Mutex
	// emailStateMem 是本次运行内的权威状态：即便落盘失败也不会重复提醒
	emailStateMem   *emailState
	emailStateWarn  bool
	lastIgnoredIPv6 string
)

// loadEmailState 优先返回内存态，其次读文件
func loadEmailState() emailState {
	emailStateLock.Lock()
	defer emailStateLock.Unlock()

	if emailStateMem != nil {
		return *emailStateMem
	}

	byt, err := os.ReadFile(emailStatePath())
	if err != nil {
		return emailState{}
	}
	var state emailState
	if err := json.Unmarshal(byt, &state); err != nil {
		util.Log("邮件通知: 状态文件无法解析, 按首次运行处理: %s", err)
		return emailState{}
	}
	return state
}

// storeEmailState 先更新内存态，再原子落盘。
// 落盘失败只告警一次：内存态已经记住了本次的 IP，下一轮不会重复发同一封邮件。
func storeEmailState(state emailState) {
	emailStateLock.Lock()
	emailStateMem = &state
	emailStateLock.Unlock()

	if err := writeEmailStateFile(state); err != nil {
		emailStateLock.Lock()
		shouldWarn := !emailStateWarn
		emailStateWarn = true
		emailStateLock.Unlock()
		if shouldWarn {
			util.Log("邮件通知: 状态写入失败 (%s), 已改用内存状态, 本次运行内不会重复提醒", err)
		}
		return
	}

	emailStateLock.Lock()
	emailStateWarn = false
	emailStateLock.Unlock()
}

// writeEmailStateFile 先写临时文件再 rename，避免进程中途被杀留下半个 JSON
func writeEmailStateFile(state emailState) error {
	byt, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	path := emailStatePath()
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, byt, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ifInet6Path 内核暴露的 IPv6 地址表，测试可覆盖
var ifInet6Path = "/proc/net/if_inet6"

// Linux 地址标志位，见 include/uapi/linux/if_addr.h
const (
	ipv6FlagTemporary  = 0x01 // IFA_F_SECONDARY，IPv6 下即 IFA_F_TEMPORARY
	ipv6FlagDeprecated = 0x20
	ipv6FlagTentative  = 0x40
)

// parseIfInet6 解析 /proc/net/if_inet6 内容，返回 规范化地址 -> 标志位
func parseIfInet6(r io.Reader) map[string]uint32 {
	out := make(map[string]uint32)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || len(fields[0]) != net.IPv6len*2 {
			continue
		}

		buf := make([]byte, net.IPv6len)
		valid := true
		for i := 0; i < net.IPv6len; i++ {
			v, err := strconv.ParseUint(fields[0][i*2:i*2+2], 16, 8)
			if err != nil {
				valid = false
				break
			}
			buf[i] = byte(v)
		}
		if !valid {
			continue
		}

		flags, err := strconv.ParseUint(fields[4], 16, 32)
		if err != nil {
			continue
		}
		out[net.IP(buf).String()] = uint32(flags)
	}
	return out
}

// unstableIPv6Addrs 返回本机上临时/已弃用/待定的 IPv6 地址集合
func unstableIPv6Addrs() map[string]bool {
	f, err := os.Open(ifInet6Path)
	if err != nil {
		return nil
	}
	defer f.Close()

	out := make(map[string]bool)
	for addr, flags := range parseIfInet6(f) {
		if flags&(ipv6FlagTemporary|ipv6FlagDeprecated|ipv6FlagTentative) != 0 {
			out[addr] = true
		}
	}
	return out
}
