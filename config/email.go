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

// Validate 检查配置是否足以发信
func (e *Email) Validate() error {
	if strings.TrimSpace(e.SMTPHost) == "" {
		return errors.New("SMTP 服务器不能为空")
	}
	if len(splitRecipients(e.To)) == 0 {
		return errors.New("收件人不能为空")
	}
	if strings.TrimSpace(e.From) == "" && strings.TrimSpace(e.Username) == "" {
		return errors.New("发件人不能为空")
	}
	return nil
}

// Send 发送一封纯文本邮件
func (e *Email) Send(subject, body string) error {
	if err := e.Validate(); err != nil {
		return err
	}

	host := strings.TrimSpace(e.SMTPHost)
	port := e.SMTPPort
	if port <= 0 {
		port = 465
	}
	from := strings.TrimSpace(e.From)
	if from == "" {
		from = strings.TrimSpace(e.Username)
	}
	to := splitRecipients(e.To)
	msg := buildMailMessage(from, to, subject, body)
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var auth smtp.Auth
	if e.Username != "" {
		auth = smtp.PlainAuth("", e.Username, e.Password, host)
	}

	err := e.deliver(addr, host, port, auth, from, to, msg)
	if err != nil && auth != nil {
		// 部分服务商不支持 AUTH PLAIN，回退到 AUTH LOGIN 再试一次
		util.Log("邮件发送失败，尝试使用 LOGIN 方式重试: %s", err)
		err = e.deliver(addr, host, port, &loginAuth{e.Username, e.Password}, from, to, msg)
	}
	return err
}

// deliver 建立一次 SMTP 会话并投递
func (e *Email) deliver(addr, host string, port int, auth smtp.Auth, from string, to []string, msg []byte) error {
	encrypt := strings.ToLower(strings.TrimSpace(e.Encrypt))
	if encrypt == "" {
		// 未显式配置时按端口推断
		switch port {
		case 465:
			encrypt = "ssl"
		case 587:
			encrypt = "starttls"
		default:
			encrypt = "none"
		}
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var (
		conn net.Conn
		err  error
	)
	if encrypt == "ssl" || encrypt == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	defer conn.Close()

	// 整段会话必须有硬超时：SMTP 服务器半死时，读写会无限阻塞，
	// 而本函数是在 DNS 更新循环里同步调用的，不能把主循环一起拖死。
	_ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout))

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("初始化 SMTP 会话失败: %w", err)
	}
	defer client.Close()

	if encrypt == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP 服务器不支持 STARTTLS")
		}
		if err = client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("STARTTLS 失败: %w", err)
		}
	}

	if auth != nil {
		if ok, _ := client.Extension("AUTH"); ok {
			if err = client.Auth(auth); err != nil {
				return fmt.Errorf("SMTP 认证失败: %w", err)
			}
		}
	}

	if err = client.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM 被拒绝: %w", err)
	}
	for _, rcpt := range to {
		if err = client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("RCPT TO(%s) 被拒绝: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA 被拒绝: %w", err)
	}
	if _, err = w.Write(msg); err != nil {
		return fmt.Errorf("写入邮件内容失败: %w", err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("提交邮件内容失败: %w", err)
	}
	return client.Quit()
}

// NotifyIPChangeEmail 当公网 IP 与上次通知过的 IP 不同时发信。
// 首次运行（没有历史记录）时，若开启 OnStart 则发送一封基线邮件。
func NotifyIPChangeEmail(conf *Config, ipv4, ipv6 string) {
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

	subject, body := composeIPChangeMail(conf, state, ipv4, ipv6, firstRun)
	if err := conf.Email.Send(subject, body); err != nil {
		util.Log("邮件通知发送失败: %s", err)
		// 不更新状态，下一轮继续重试
		return
	}

	next.LastSent = time.Now().Unix()
	storeEmailState(next)
	if firstRun {
		util.Log("邮件通知: 已发送初始 IP 基线邮件 (%s / %s)", displayOrDash(ipv4), displayOrDash(ipv6))
	} else {
		util.Log("邮件通知: IP 已变更，通知邮件发送成功 (%s / %s)", displayOrDash(ipv4), displayOrDash(ipv6))
	}
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

	var b strings.Builder
	if zh {
		if firstRun {
			subject = fmt.Sprintf("[ddns-go] 启动基线: 当前 IP %s", displayOrDash(ipv4))
			b.WriteString("ddns-go 已启动，这是首次记录的 IP 基线。\n\n")
		} else {
			subject = fmt.Sprintf("[ddns-go] IP 地址变更: %s -> %s", displayOrDash(state.LastIPv4), displayOrDash(ipv4))
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
			subject = fmt.Sprintf("[ddns-go] Baseline IP: %s", displayOrDash(ipv4))
			b.WriteString("ddns-go has started. This is the first recorded IP baseline.\n\n")
		} else {
			subject = fmt.Sprintf("[ddns-go] IP changed: %s -> %s", displayOrDash(state.LastIPv4), displayOrDash(ipv4))
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

// splitRecipients 拆分收件人，支持逗号、分号和换行
func splitRecipients(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
	var out []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
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
	// emailStateLock 同时保护内存态和日志去重标记，也顺带避免"保存配置触发的
	// RunOnce"与定时器并发读写状态导致的重复发送
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
