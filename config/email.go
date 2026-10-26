package config

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
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
	if ipv4 == "" && ipv6 == "" {
		return
	}

	state := loadEmailState()
	firstRun := state.LastIPv4 == "" && state.LastIPv6 == ""
	if state.LastIPv4 == ipv4 && state.LastIPv6 == ipv6 {
		return
	}
	if firstRun && !conf.Email.OnStart {
		// 仅记录基线，不发信
		saveEmailState(emailState{LastIPv4: ipv4, LastIPv6: ipv6})
		util.Log("邮件通知: 已记录初始 IP 基线 (%s / %s)，本次不发送", displayOrDash(ipv4), displayOrDash(ipv6))
		return
	}

	subject, body := composeIPChangeMail(conf, state, ipv4, ipv6, firstRun)
	if err := conf.Email.Send(subject, body); err != nil {
		util.Log("邮件通知发送失败: %s", err)
		// 不更新状态，下一轮继续重试
		return
	}

	saveEmailState(emailState{LastIPv4: ipv4, LastIPv6: ipv6, LastSent: time.Now().Unix()})
	if firstRun {
		util.Log("邮件通知: 已发送初始 IP 基线邮件 (%s / %s)", displayOrDash(ipv4), displayOrDash(ipv6))
	} else {
		util.Log("邮件通知: IP 已变更，通知邮件发送成功 (%s / %s)", displayOrDash(ipv4), displayOrDash(ipv6))
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

func loadEmailState() emailState {
	var state emailState
	byt, err := os.ReadFile(emailStatePath())
	if err != nil {
		return state
	}
	_ = json.Unmarshal(byt, &state)
	return state
}

func saveEmailState(state emailState) {
	byt, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		util.Log("邮件通知: 保存状态失败: %s", err)
		return
	}
	if err = os.WriteFile(emailStatePath(), byt, 0600); err != nil {
		util.Log("邮件通知: 保存状态失败: %s", err)
	}
}
