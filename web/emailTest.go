package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

// EmailTest 发送一封测试邮件，用于验证 SMTP 配置是否正确
func EmailTest(writer http.ResponseWriter, request *http.Request) {
	var data struct {
		Enable   bool   `json:"Enable"`
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

	// 密码留空时沿用已保存的密码，方便页面上直接点测试
	if email.Password == "" {
		conf, _ := config.GetConfigCached()
		email.Password = conf.Email.Password
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
