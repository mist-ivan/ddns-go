package web

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/dns"
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
		`id="emailStatus"`,
		`id="emailHistory"`,
		`id="emailRefreshBtn"`,
		`id="emailClearHistoryBtn"`,
		`id="emailTestResult"`,
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
	// 后台登录用户名不能被 SMTP 用户名覆盖
	if !strings.Contains(body, `id="Username" value="admin"`) {
		t.Fatal("admin username field must render the login username, not the SMTP username")
	}
	if !strings.Contains(body, "checked") {
		t.Fatal("EmailEnable / EmailOnStart should render as checked")
	}
	// 状态面板与历史由前端异步渲染：页面里应有容器和轮询接口路径
	if !strings.Contains(body, "./emailStatus") {
		t.Fatal("page should poll ./emailStatus for the status panel")
	}
	if !strings.Contains(body, "./emailHistoryClear") {
		t.Fatal("page should reference ./emailHistoryClear")
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
    smtphost: smtp.qq.com
    smtpport: 587
    username: me@qq.com
    password: saved-secret
    from: me@qq.com
    to: a@x.com, b@y.com
    encrypt: starttls
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

// 改 SMTP 主机/账号但留空密码时，必须拒绝保存，不能把旧密码发给新目标。
func TestSaveRejectsOldPasswordForNewSMTPServer(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	initial := `dnsconf: []
user:
    username: admin
    password: ""
email:
    enable: false
    smtphost: old.example.com
    smtpport: 587
    username: me@old.example.com
    password: old-smtp-secret
    from: me@old.example.com
    to: a@x.com
    encrypt: starttls
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
      "EmailTo": "a@x.com",
      "EmailEncrypt": "starttls",
      "EmailAlertOnFetchFailure": true,
      "DnsConf": []
    }`
	rec := httptest.NewRecorder()
	Save(rec, httptest.NewRequest("POST", "/save", strings.NewReader(payload)))
	if strings.Contains(rec.Body.String(), `"result":"ok"`) {
		t.Fatalf("save should have been rejected: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "重新输入") {
		t.Fatalf("unexpected error: %s", rec.Body.String())
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "smtphost: old.example.com") ||
		!strings.Contains(string(saved), "password: old-smtp-secret") {
		t.Fatalf("rejected save must not alter the old config:\n%s", saved)
	}
}

// 状态接口应返回状态面板需要的字段
func TestEmailStatusEndpoint(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("dnsconf: []\nuser:\n    username: admin\n    password: \"\"\nlang: zh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(util.ConfigFilePathENV, cfgPath)
	config.ResetConfigCache()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/emailStatus", nil)
	HandleEmailStatus(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status code = %d", rec.Code)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not valid json: %v (body=%s)", err, rec.Body.String())
	}
	for _, key := range []string{"ipv4", "ipv6", "lastSent", "lastRunAt", "intervalSeconds", "historyTotal", "recent", "emailEnabled"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("status payload missing %q: %s", key, rec.Body.String())
		}
	}
}

// 保存时可以通过 EmailPasswordClear 清空已存的 SMTP 密码
func TestSaveCanClearEmailPassword(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	initial := `dnsconf: []
user:
    username: admin
    password: ""
email:
    enable: false
    smtphost: smtp.qq.com
    password: saved-secret
    to: a@x.com
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
      "EmailEnable": false,
      "EmailSMTPHost": "smtp.qq.com",
      "EmailSMTPPort": "465",
      "EmailPassword": "",
      "EmailPasswordClear": true,
      "EmailTo": "a@x.com",
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
	if strings.Contains(string(saved), "saved-secret") {
		t.Fatalf("password should have been cleared:\n%s", saved)
	}
}

// 同时提交“清除密码”与新密码时，新密码必须生效（v0.7 修复）
func TestSaveNewPasswordWinsOverClear(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	initial := `dnsconf: []
user:
    username: admin
    password: ""
email:
    enable: false
    smtphost: smtp.qq.com
    password: old-secret
    to: a@x.com
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
      "EmailEnable": false,
      "EmailSMTPHost": "smtp.qq.com",
      "EmailSMTPPort": "465",
      "EmailPassword": "new-secret",
      "EmailPasswordClear": true,
      "EmailTo": "a@x.com",
      "DnsConf": []
    }`
	rec := httptest.NewRecorder()
	Save(rec, httptest.NewRequest("POST", "/save", strings.NewReader(payload)))

	if !strings.Contains(rec.Body.String(), `"result":"ok"`) {
		t.Fatalf("save failed: %s", rec.Body.String())
	}
	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "password: new-secret") {
		t.Fatalf("new password should win over the clear flag:\n%s", saved)
	}
}

// 保存失败时不应该触发 DNS 检测/邮件检查（v0.7 修复）
func TestSaveFailureSkipsRunOnce(t *testing.T) {
	// 先等上一个用例遗留的异步检测跑完：它的 RunOnce 会读配置，
	// 而本用例马上把配置路径改成不存在的目录，读失败会把该标志回写成 true。
	dns.WaitPendingRun(5 * time.Second)

	// 指向不存在的目录，SaveConfig 必然失败
	t.Setenv(util.ConfigFilePathENV, filepath.Join(t.TempDir(), "missing-dir", "config.yaml"))
	config.ResetConfigCache()

	util.ForceCompareGlobal.Store(false)
	defer func() { util.ForceCompareGlobal.Store(false) }()

	payload := `{
      "Username": "admin",
      "Password": "V3ry-Str0ng-Passphrase!2026",
      "Lang": "zh",
      "EmailEnable": false,
      "DnsConf": []
    }`
	rec := httptest.NewRecorder()
	Save(rec, httptest.NewRequest("POST", "/save", strings.NewReader(payload)))

	if strings.Contains(rec.Body.String(), `"result":"ok"`) {
		t.Fatalf("save should have failed: %s", rec.Body.String())
	}
	if util.ForceCompareGlobal.Load() {
		t.Fatal("RunOnce must not be triggered when saving fails")
	}
}

// 清空历史只接受 POST：GET 会被浏览器顶层导航带上 cookie，可被跨站触发
func TestEmailHistoryClearRejectsGet(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("dnsconf: []\nlang: zh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(util.ConfigFilePathENV, cfgPath)
	config.ResetConfigCache()

	rec := httptest.NewRecorder()
	HandleEmailHistoryClear(rec, httptest.NewRequest("GET", "/emailHistoryClear", nil))
	if rec.Code != 405 {
		t.Fatalf("GET should be rejected with 405, got %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	HandleEmailHistoryClear(rec2, httptest.NewRequest("POST", "/emailHistoryClear", strings.NewReader("{}")))
	if rec2.Code != 200 {
		t.Fatalf("POST should be accepted, got %d", rec2.Code)
	}
}

// 只有与已保存配置一致时才复用密码（否则等于把密码发给请求指定的任意服务器）
func TestSameSMTPHost(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"smtp.qq.com", "SMTP.QQ.COM", true},
		{"[::1]", "::1", true},
		{"smtp.qq.com", "evil.example.com", false},
	}
	for _, tc := range cases {
		if got := sameSMTPHost(tc.a, tc.b); got != tc.want {
			t.Errorf("sameSMTPHost(%q,%q) = %v", tc.a, tc.b, got)
		}
	}
}
