package web

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

// 确保邮件配置区能正常渲染（模板字段名写错时这里会失败）
func TestWritingRendersEmailSection(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `dnsconf: []
user:
    username: admin
    password: ""
email:
    enable: true
    smtphost: smtp.example.com
    smtpport: 465
    username: me@example.com
    password: secret
    from: ddns-go <me@example.com>
    to: you@example.com
    encrypt: ssl
    onstart: true
notallowwanaccess: true
lang: zh
`
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(util.ConfigFilePathENV, cfgPath)
	config.ResetConfigCache()

	// 预置一条历史，验证页面能渲染出来
	history := `{"time":1700000000,"kind":"change","ipv4":"2.2.2.2","prevIpv4":"1.1.1.1"}` + "\n"
	if err := os.WriteFile(cfgPath+".email-history.jsonl", []byte(history), 0600); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	Writing(rec, req)

	body := rec.Body.String()
	checks := []string{
		`id="EmailEnable"`,
		`id="EmailSMTPHost"`,
		`value="smtp.example.com"`,
		`value="465"`,
		`value="me@example.com"`,
		`id="EmailEncrypt"`,
		`id="EmailTo"`,
		`value="you@example.com"`,
		`id="emailTestBtn"`,
		`id="EmailAlertOnFetchFailure"`,
		`id="emailHistory"`,
	}
	for _, want := range checks {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered page is missing %q", want)
		}
	}
	// 密码不应回显到页面上
	if strings.Contains(body, `value="secret"`) {
		t.Fatal("SMTP password must not be rendered into the page")
	}
	if !strings.Contains(body, "checked") {
		t.Fatal("EmailEnable / EmailOnStart should render as checked")
	}
	// 历史记录应渲染出预置的那条
	if !strings.Contains(body, "2.2.2.2") || !strings.Contains(body, "IP 变更") {
		t.Fatal("email history entry should be rendered in the page")
	}
}

// 保存配置时应写入邮件字段，且空密码表示保留原密码
func TestSavePersistsEmailConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	initial := `dnsconf: []
user:
    username: admin
    password: ""
email:
    enable: false
    smtphost: old.example.com
    password: saved-secret
    to: old@example.com
notallowwanaccess: true
lang: zh
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(util.ConfigFilePathENV, cfgPath)
	config.ResetConfigCache()

	payload := `{
      "Username": "admin",
      "Password": "V3ry-Str0ng-Passphrase!2026",
      "Lang": "zh",
      "NotAllowWanAccess": true,
      "EmailEnable": true,
      "EmailSMTPHost": "smtp.qq.com",
      "EmailSMTPPort": "587",
      "EmailUsername": "me@qq.com",
      "EmailPassword": "",
      "EmailFrom": "me@qq.com",
      "EmailTo": "a@x.com, b@y.com",
      "EmailEncrypt": "starttls",
      "EmailOnStart": true,
      "EmailAlertOnFetchFailure": true,
      "DnsConf": []
    }`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/save", strings.NewReader(payload))
	Save(rec, req)

	if !strings.Contains(rec.Body.String(), `"result":"ok"`) {
		t.Fatalf("save failed: %s", rec.Body.String())
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	for _, want := range []string{
		"smtphost: smtp.qq.com",
		"smtpport: 587",
		"encrypt: starttls",
		"onstart: true",
		"alertonfetchfailure: true",
		"enable: true",
		"to: a@x.com, b@y.com",
		"password: saved-secret",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("saved config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "V3ry-Str0ng-Passphrase!2026") {
		t.Fatal("user password must be hashed, not stored in plain text")
	}
}
