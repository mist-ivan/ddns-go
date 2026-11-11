package web

import (
	"encoding/json"
	"net/http"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/dns"
	"github.com/jeessy2/ddns-go/v6/util"
)

// emailStatusPayload 页面状态面板所需的数据
type emailStatusPayload struct {
	IPv4            string                     `json:"ipv4"`
	IPv6            string                     `json:"ipv6"`
	LastSent        int64                      `json:"lastSent"`
	LastRunAt       int64                      `json:"lastRunAt"`
	IntervalSeconds int64                      `json:"intervalSeconds"`
	HistoryTotal    int                        `json:"historyTotal"`
	Recent          []config.EmailHistoryEntry `json:"recent"`
	EmailEnabled    bool                       `json:"emailEnabled"`
	// HasNotified 表示是否真的发出过通知（用来区分"基线 IP"和"上次通知的 IP"）
	HasNotified bool `json:"hasNotified"`
	// NextRunAt 定时器下一次触发的预计时间（unix 秒），0 表示未知
	NextRunAt int64 `json:"nextRunAt"`
	// 最近一轮实际检测到的公网 IP（区别于"上次成功通知的 IP"）
	DetectedIPv4 string `json:"detectedIpv4"`
	DetectedIPv6 string `json:"detectedIpv6"`
	// BackoffUntil 发信失败后的退避截止时间（unix 秒），0 表示不在退避期
	BackoffUntil int64 `json:"backoffUntil"`
}

// HandleEmailStatus 返回邮件通知模块的运行状态，供页面状态面板轮询
func HandleEmailStatus(writer http.ResponseWriter, request *http.Request) {
	// 只读接口，限定 GET：避免被当成跨站 POST 的落点（虽无副作用，但语义应明确）
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	conf, _ := config.GetConfigCached()
	ipv4, ipv6, lastSent := config.GetEmailStateSnapshot()
	detectedIPv4, detectedIPv6 := dns.LastDetected()
	summary := config.GetEmailHistorySummary(10)

	payload := emailStatusPayload{
		IPv4:            ipv4,
		IPv6:            ipv6,
		LastSent:        lastSent,
		LastRunAt:       dns.LastRunAt(),
		IntervalSeconds: dns.IntervalSeconds(),
		HistoryTotal:    summary.Total,
		Recent:          summary.Recent,
		EmailEnabled:    conf.Email.Enable,
		HasNotified:     lastSent > 0,
		NextRunAt:       dns.NextRunAt(),
		DetectedIPv4:    detectedIPv4,
		DetectedIPv6:    detectedIPv6,
		BackoffUntil:    config.GetEmailBackoffUntil(),
	}

	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(writer).Encode(payload)
}

// HandleEmailHistoryClear 清空通知历史。
// 只接受 POST：GET 会被浏览器当作顶层导航带上 cookie，跨站一个链接就能清空历史。
func HandleEmailHistoryClear(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := config.ClearEmailHistory(); err != nil {
		util.Log("邮件通知: 清空历史记录失败: %s", err)
		emailTestResult(writer, false, util.LogStr("清空历史记录失败: %s", err.Error()))
		return
	}
	util.Log("邮件通知: 历史记录已清空")
	emailTestResult(writer, true, "")
}
