package config

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jeessy2/ddns-go/v6/util"
)

// maxEmailHistory 历史文件最多保留的条数，超出后从头丢弃
const maxEmailHistory = 200

// 历史记录类型
const (
	historyKindBaseline    = "baseline"
	historyKindChange      = "change"
	historyKindFetchFailed = "fetch_failed"
	historyKindRecovered   = "recovered"
)

// EmailHistoryEntry 一次通知的历史记录（一行 JSON）
type EmailHistoryEntry struct {
	Time     int64  `json:"time"`
	Kind     string `json:"kind"`
	IPv4     string `json:"ipv4,omitempty"`
	PrevIPv4 string `json:"prevIpv4,omitempty"`
	IPv6     string `json:"ipv6,omitempty"`
	PrevIPv6 string `json:"prevIpv6,omitempty"`

	// 以下字段只用于页面展示，不写入文件
	TimeText string `json:"-"`
	KindText string `json:"-"`
}

// EmailHistorySummary 页面展示用的历史摘要
type EmailHistorySummary struct {
	Total    int
	LastTime string
	Recent   []EmailHistoryEntry
}

var emailHistoryLock sync.Mutex

func emailHistoryPath() string {
	return util.GetConfigFilePath() + ".email-history.jsonl"
}

// AppendEmailHistory 追加一条历史，并在超过上限时裁剪
func AppendEmailHistory(entry EmailHistoryEntry) {
	if entry.Time == 0 {
		entry.Time = time.Now().Unix()
	}
	byt, err := json.Marshal(entry)
	if err != nil {
		return
	}

	emailHistoryLock.Lock()
	defer emailHistoryLock.Unlock()

	path := emailHistoryPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		util.Log("邮件通知: 写入历史记录失败: %s", err)
		return
	}
	_, writeErr := f.Write(append(byt, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		util.Log("邮件通知: 写入历史记录失败: %v %v", writeErr, closeErr)
		return
	}

	trimEmailHistoryLocked(path)
}

// readEmailHistoryLines 读取全部非空行（不加锁，调用方负责并发控制）
func readEmailHistoryLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// trimEmailHistoryLocked 超过上限时只保留最新的部分（调用方已持锁）
func trimEmailHistoryLocked(path string) {
	lines := readEmailHistoryLines(path)
	if len(lines) <= maxEmailHistory {
		return
	}
	lines = lines[len(lines)-maxEmailHistory:]

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// ClearEmailHistory 清空历史记录文件
func ClearEmailHistory() error {
	emailHistoryLock.Lock()
	defer emailHistoryLock.Unlock()

	path := emailHistoryPath()
	_ = os.Remove(path + ".tmp")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// GetEmailHistorySummary 返回历史概要：总条数、最近一次时间、最近若干条（新的在前）
func GetEmailHistorySummary(limit int, lang string) EmailHistorySummary {
	emailHistoryLock.Lock()
	lines := readEmailHistoryLines(emailHistoryPath())
	emailHistoryLock.Unlock()

	summary := EmailHistorySummary{Total: len(lines)}
	if len(lines) == 0 {
		return summary
	}

	for i := len(lines) - 1; i >= 0 && len(summary.Recent) < limit; i-- {
		var entry EmailHistoryEntry
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			continue
		}
		entry.TimeText = formatHistoryTime(entry.Time)
		entry.KindText = historyKindText(entry.Kind, lang)
		summary.Recent = append(summary.Recent, entry)
	}
	if len(summary.Recent) > 0 {
		summary.LastTime = summary.Recent[0].TimeText
	}
	return summary
}

func formatHistoryTime(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

func historyKindText(kind, lang string) string {
	zh := strings.HasPrefix(strings.ToLower(lang), "zh")
	switch kind {
	case historyKindBaseline:
		if zh {
			return "首次基线"
		}
		return "baseline"
	case historyKindChange:
		if zh {
			return "IP 变更"
		}
		return "IP changed"
	case historyKindFetchFailed:
		if zh {
			return "获取 IP 失败"
		}
		return "fetch failed"
	case historyKindRecovered:
		if zh {
			return "获取已恢复"
		}
		return "recovered"
	default:
		return kind
	}
}
