package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/dns"
	"github.com/jeessy2/ddns-go/v6/util"
)

// Save 保存
func Save(writer http.ResponseWriter, request *http.Request) {
	result := checkAndSave(request)
	dnsConfJsonStr := "[]"
	if result == "ok" {
		conf, _ := config.GetConfigCached()
		dnsConfJsonStr = getDnsConfStr(conf.DnsConf)
	}
	byt, _ := json.Marshal(map[string]string{"result": result, "dnsConf": dnsConfJsonStr})

	writer.Write(byt)
}

func checkAndSave(request *http.Request) string {
	conf, _ := config.GetConfigCached()

	// 从请求中读取 JSON 数据
	var data struct {
		Username           string       `json:"Username"`
		Password           string       `json:"Password"`
		Lang               string       `json:"Lang"`
		NotAllowWanAccess  bool         `json:"NotAllowWanAccess"`
		WebhookURL         string       `json:"WebhookURL"`
		WebhookRequestBody string       `json:"WebhookRequestBody"`
		WebhookHeaders     string       `json:"WebhookHeaders"`
		EmailEnable        bool         `json:"EmailEnable"`
		EmailSMTPHost      string       `json:"EmailSMTPHost"`
		EmailSMTPPort      string       `json:"EmailSMTPPort"`
		EmailUsername      string       `json:"EmailUsername"`
		EmailPassword      string       `json:"EmailPassword"`
		EmailFrom          string       `json:"EmailFrom"`
		EmailTo            string       `json:"EmailTo"`
		EmailEncrypt       string       `json:"EmailEncrypt"`
		EmailOnStart       bool         `json:"EmailOnStart"`
		EmailAlertOnFetch  bool         `json:"EmailAlertOnFetchFailure"`
		EmailPasswordClear bool         `json:"EmailPasswordClear"`
		DnsConf            []dnsConf4JS `json:"DnsConf"`
	}

	// 解析请求中的 JSON 数据
	err := json.NewDecoder(request.Body).Decode(&data)
	if err != nil {
		return util.LogStr("数据解析失败, 请刷新页面重试")
	}
	usernameNew := strings.TrimSpace(data.Username)
	passwordNew := data.Password

	if strings.TrimSpace(data.Lang) != "" {
		conf.Lang = util.InitLogLang(data.Lang)
	} else {
		conf.Lang = util.InitLogLang(conf.Lang)
	}

	// 记住保存前的值：同一次请求里把它改成 true 会放松密码强度要求
	wasLanOnly := conf.NotAllowWanAccess
	conf.NotAllowWanAccess = data.NotAllowWanAccess
	conf.WebhookURL = strings.TrimSpace(data.WebhookURL)
	conf.WebhookRequestBody = strings.TrimSpace(data.WebhookRequestBody)
	conf.WebhookHeaders = strings.TrimSpace(data.WebhookHeaders)

	// 邮件通知
	conf.Email.Enable = data.EmailEnable
	conf.Email.SMTPHost = strings.TrimSpace(data.EmailSMTPHost)
	if data.EmailEnable || strings.TrimSpace(data.EmailSMTPPort) != "" {
		port, portErr := parseSMTPPort(data.EmailSMTPPort)
		if portErr != nil {
			// 只要给了端口就校验：以前邮件关闭时会静默吞掉错误、保留旧端口，
			// 界面上显示的却是用户刚填的非法值，下次启用时行为和界面不一致。
			return util.LogStr("邮件通知配置错误: %s", portErr.Error())
		}
		conf.Email.SMTPPort = port
	}
	conf.Email.Username = strings.TrimSpace(data.EmailUsername)
	// 密码留空表示保持原值，避免前端回显明文。
	// 同时提交了"清除"标记和新密码时，以新密码为准（清除标记只是前端的临时状态）。
	switch {
	case data.EmailPassword != "":
		conf.Email.Password = data.EmailPassword
	case data.EmailPasswordClear:
		conf.Email.Password = ""
	}
	conf.Email.From = strings.TrimSpace(data.EmailFrom)
	conf.Email.To = strings.TrimSpace(data.EmailTo)
	conf.Email.Encrypt = strings.TrimSpace(data.EmailEncrypt)
	conf.Email.OnStart = data.EmailOnStart
	conf.Email.AlertOnFetchFailure = data.EmailAlertOnFetch
	if data.EmailEnable {
		if err := conf.Email.Validate(); err != nil {
			return util.LogStr("邮件通知配置错误: %s", err.Error())
		}
	}

	// 如果新密码不为空则检查是否够强, 内/外网要求强度不同
	conf.Username = usernameNew
	if passwordNew != "" {
		// 两次判定取"更严格"的结果：把 NotAllowWanAccess 从 false 改成 true 不能顺便降低强度要求
		hashedPwd, err := conf.CheckPasswordWithLimit(passwordNew, wasLanOnly && conf.NotAllowWanAccess)
		if err != nil {
			return err.Error()
		}
		conf.Password = hashedPwd
	}

	// 帐号密码不能为空
	if conf.Username == "" || conf.Password == "" {
		return util.LogStr("必须输入用户名/密码")
	}

	dnsConfFromJS := data.DnsConf
	var dnsConfArray []config.DnsConfig
	empty := dnsConf4JS{}
	for k, v := range dnsConfFromJS {
		if v == empty {
			continue
		}
		dnsConf := config.DnsConfig{Name: v.Name, TTL: v.TTL}
		// 覆盖以前的配置
		dnsConf.DNS.Name = v.DnsName
		dnsConf.DNS.ID = strings.TrimSpace(v.DnsID)
		dnsConf.DNS.Secret = strings.TrimSpace(v.DnsSecret)
		dnsConf.DNS.ExtParam = strings.TrimSpace(v.DnsExtParam)

		if v.Ipv4Domains == "" && v.Ipv6Domains == "" {
			util.Log("第 %s 个配置未填写域名", util.Ordinal(k+1, conf.Lang))
		}

		dnsConf.Ipv4.Enable = v.Ipv4Enable
		dnsConf.Ipv4.GetType = v.Ipv4GetType
		dnsConf.Ipv4.URL = strings.TrimSpace(v.Ipv4Url)
		dnsConf.Ipv4.NetInterface = v.Ipv4NetInterface
		dnsConf.Ipv4.Cmd = strings.TrimSpace(v.Ipv4Cmd)
		dnsConf.Ipv4.Domains = util.SplitLines(v.Ipv4Domains)

		dnsConf.Ipv6.Enable = v.Ipv6Enable
		dnsConf.Ipv6.GetType = v.Ipv6GetType
		dnsConf.Ipv6.URL = strings.TrimSpace(v.Ipv6Url)
		dnsConf.Ipv6.NetInterface = v.Ipv6NetInterface
		dnsConf.Ipv6.Cmd = strings.TrimSpace(v.Ipv6Cmd)
		dnsConf.Ipv6.Ipv6Reg = strings.TrimSpace(v.Ipv6Reg)
		dnsConf.Ipv6.Domains = util.SplitLines(v.Ipv6Domains)
		dnsConf.HttpInterface = strings.TrimSpace(v.HttpInterface)

		if k < len(conf.DnsConf) {
			c := &conf.DnsConf[k]
			idHide, secretHide := getHideIDSecret(c)
			if dnsConf.DNS.ID == idHide {
				dnsConf.DNS.ID = c.DNS.ID
			}
			if dnsConf.DNS.Secret == secretHide {
				dnsConf.DNS.Secret = c.DNS.Secret
			}
		}

		dnsConfArray = append(dnsConfArray, dnsConf)
	}
	conf.DnsConf = dnsConfArray

	// 保存到用户目录。保存失败时直接返回，不触发检测：
	// 否则界面提示保存失败，后台却已经用新配置跑了一轮（可能发信、改 DNS）。
	if err = conf.SaveConfig(); err != nil {
		return err.Error()
	}

	// 只运行一次
	util.ForceCompareGlobal.Store(true)
	dns.RunOnceAsync()
	return "ok"
}

// parseSMTPPort 解析端口；留空按 465，非法值返回错误（不再静默改成 465）
func parseSMTPPort(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 465, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s", util.LogStr("SMTP 端口不合法: %s", raw))
	}
	return port, nil
}
