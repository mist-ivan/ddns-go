package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

// emailTestMaxBody 限制测试接口请求体，避免认证会话被用来塞超大 JSON。
const emailTestMaxBody = 64 << 10

// EmailTest 发送一封测试邮件，用于验证 SMTP 配置是否正确
func EmailTest(writer http.ResponseWriter, request *http.Request) {
	var data struct {
		SMTPHost string `json:"SMTPHost"`
		SMTPPort string `json:"SMTPPort"`
		Username string `json:"Username"`
		Password string `json:"Password"`
		From     string `json:"From"`
		To       string `json:"To"`
		Encrypt  string `json:"Encrypt"`
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, emailTestMaxBody)
	if err := json.NewDecoder(request.Body).Decode(&data); err != nil {
		util.Log("数据解析失败, 请刷新页面重试")
		emailTestResult(writer, false, util.LogStr("数据解析失败, 请刷新页面重试"))
		return
	}

	port, err := parseSMTPPort(data.SMTPPort)
	if err != nil {
		util.Log("邮件测试失败: %s", err)
		emailTestResult(writer, false, err.Error())
		return
	}

	email := config.Email{
		Enable:   true,
		SMTPHost: strings.TrimSpace(data.SMTPHost),
		SMTPPort: port,
		Username: strings.TrimSpace(data.Username),
		Password: data.Password,
		From:     strings.TrimSpace(data.From),
		To:       strings.TrimSpace(data.To),
		Encrypt:  strings.TrimSpace(data.Encrypt),
	}

	// 密码留空时沿用已保存的密码，方便在页面上直接点测试。
	// 但仅当目标服务器/端口/账号与已保存配置一致时才复用：
	// 否则等于把已保存的密码交给请求里指定的任意 SMTP 服务器（凭据外泄）。
	if email.Password == "" {
		conf, _ := config.GetConfigCached()
		saved := conf.Email
		savedPort := saved.SMTPPort
		if savedPort == 0 {
			savedPort = 465
		}
		if saved.Password != "" &&
			sameSMTPHost(saved.SMTPHost, email.SMTPHost) &&
			savedPort == email.SMTPPort &&
			strings.EqualFold(strings.TrimSpace(saved.Username), email.Username) {
			email.Password = saved.Password
		}
	}

	if err := email.Validate(); err != nil {
		util.Log("邮件测试失败: %s", err)
		emailTestResult(writer, false, err.Error())
		return
	}

	// 先占“正在发送”的并发名额，再占 5 秒启动间隔：
	// 只限开始间隔的话，45 秒会话可以叠加多个并发 SMTP 连接。
	if !acquireEmailTestRun() {
		emailTestResult(writer, false, util.LogStr("已有测试邮件正在发送，请稍后再试"))
		return
	}
	defer releaseEmailTestRun()

	// 简单限频：这个接口会真的发信、占用一个最长 45 秒的连接，
	// 不限制的话一次会话被盗就能把邮箱配额打光或触发风控。
	// 名额必须在参数校验通过之后再占用，否则填错端口也要等 5 秒才能重试。
	if !acquireEmailTestSlot() {
		emailTestResult(writer, false, util.LogStr("操作过于频繁，请稍后再试"))
		return
	}

	subject := "[ddns-go] 测试邮件 / Test email"
	body := "这是一封来自 ddns-go 的测试邮件。\n\n" +
		"如果你收到这封邮件，说明 SMTP 配置正确，IP 变更提醒可以正常工作。\n\n" +
		"This is a test email from ddns-go. If you received it, your SMTP settings work.\n"

	if err := email.Send(subject, body); err != nil {
		util.Log("邮件测试失败: %s", err)
		emailTestResult(writer, false, err.Error())
		return
	}

	// 测试邮件能发出去，说明故障已排除：顺手解除退避，
	// 否则真实通知还要继续等退避期结束，用户会以为"测试成功但通知还是坏的"。
	config.ResetEmailBackoff()

	util.Log("邮件测试成功，已发送至: %s", email.To)
	emailTestResult(writer, true, "")
}

// emailTestResult 返回简单的结果对象
func emailTestResult(writer http.ResponseWriter, ok bool, msg string) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	byt, _ := json.Marshal(map[string]interface{}{"ok": ok, "msg": msg})
	_, _ = writer.Write(byt)
}

// emailTestMinInterval 两次测试邮件之间的最小间隔
const emailTestMinInterval = 5 * time.Second

// emailTestMaxPerHour 每小时最多允许的测试邮件数，防止会话被盗后大量发信。
const emailTestMaxPerHour = 30

var (
	emailTestLock sync.Mutex
	emailTestLast time.Time

	emailTestQuotaStart time.Time
	emailTestQuotaCount int

	// emailTestRun 是容量 1 的非阻塞信号量：同一时刻只允许一个测试邮件
	// 真正建立 SMTP 会话，避免每 5 秒启动一个 45 秒的连接。
	emailTestRun = make(chan struct{}, 1)
)

// acquireEmailTestRun 非阻塞地占用“正在发送”名额。
func acquireEmailTestRun() bool {
	select {
	case emailTestRun <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseEmailTestRun() {
	<-emailTestRun
}

// acquireEmailTestSlot 限频+小时配额；返回 false 表示本次应被拒绝
func acquireEmailTestSlot() bool {
	emailTestLock.Lock()
	defer emailTestLock.Unlock()

	now := time.Now()
	if emailTestQuotaStart.IsZero() || now.Sub(emailTestQuotaStart) >= time.Hour {
		emailTestQuotaStart = now
		emailTestQuotaCount = 0
	}
	if emailTestQuotaCount >= emailTestMaxPerHour {
		return false
	}
	if !emailTestLast.IsZero() && now.Sub(emailTestLast) < emailTestMinInterval {
		return false
	}
	emailTestQuotaCount++
	emailTestLast = now
	return true
}

// sameSMTPHost 比较两个 SMTP 主机是否等价（忽略大小写与 IPv6 方括号）
func sameSMTPHost(a, b string) bool {
	norm := func(h string) string {
		h = strings.ToLower(strings.TrimSpace(h))
		return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	}
	return norm(a) == norm(b)
}
