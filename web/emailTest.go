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
	if err := json.NewDecoder(request.Body).Decode(&data); err != nil {
		util.Log("数据解析失败, 请刷新页面重试")
		emailTestResult(writer, false, util.LogStr("数据解析失败, 请刷新页面重试"))
		return
	}

	// 简单限频：这个接口会真的发信、占用一个最长 45 秒的连接，
	// 不限制的话一次会话被盗就能把邮箱配额打光或触发风控。
	if !acquireEmailTestSlot() {
		emailTestResult(writer, false, util.LogStr("操作过于频繁，请稍后再试"))
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

	subject := "[ddns-go] 测试邮件 / Test email"
	body := "这是一封来自 ddns-go 的测试邮件。\n\n" +
		"如果你收到这封邮件，说明 SMTP 配置正确，IP 变更提醒可以正常工作。\n\n" +
		"This is a test email from ddns-go. If you received it, your SMTP settings work.\n"

	if err := email.Send(subject, body); err != nil {
		util.Log("邮件测试失败: %s", err)
		emailTestResult(writer, false, err.Error())
		return
	}

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

var (
	emailTestLock sync.Mutex
	emailTestLast time.Time
)

// acquireEmailTestSlot 限频；返回 false 表示本次应被拒绝
func acquireEmailTestSlot() bool {
	emailTestLock.Lock()
	defer emailTestLock.Unlock()
	if !emailTestLast.IsZero() && time.Since(emailTestLast) < emailTestMinInterval {
		return false
	}
	emailTestLast = time.Now()
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
