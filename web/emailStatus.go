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
}

// HandleEmailStatus 返回邮件通知模块的运行状态，供页面状态面板轮询
func HandleEmailStatus(writer http.ResponseWriter, request *http.Request) {
	conf, _ := config.GetConfigCached()
	ipv4, ipv6, lastSent := config.GetEmailStateSnapshot()
	summary := config.GetEmailHistorySummary(10, conf.Lang)

	payload := emailStatusPayload{
		IPv4:            ipv4,
		IPv6:            ipv6,
		LastSent:        lastSent,
		LastRunAt:       dns.LastRunAt(),
		IntervalSeconds: dns.IntervalSeconds(),
		HistoryTotal:    summary.Total,
		Recent:          summary.Recent,
		EmailEnabled:    conf.Email.Enable,
	}

	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(writer).Encode(payload)
}

// HandleEmailHistoryClear 清空通知历史
func HandleEmailHistoryClear(writer http.ResponseWriter, request *http.Request) {
	if err := config.ClearEmailHistory(); err != nil {
		util.Log("邮件通知: 清空历史记录失败: %s", err)
		emailTestResult(writer, false, util.LogStr("清空历史记录失败: %s", err.Error()))
		return
	}
	util.Log("邮件通知: 历史记录已清空")
	emailTestResult(writer, true, "")
}
