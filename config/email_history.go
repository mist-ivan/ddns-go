package config

import (
	"bufio"
	"encoding/json"
	"io"
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
}

// EmailHistorySummary 历史摘要：总条数 + 最近若干条（新的在前）
type EmailHistorySummary struct {
	Total  int
	Recent []EmailHistoryEntry
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

// 正常上限 200 条时文件只有几十 KB；超过这个体积说明裁剪失效（例如重命名被拒），
// 此时只读尾部，避免每次页面轮询都全量读一个不断变大的文件。
const (
	emailHistoryFullReadLimit = 256 * 1024
	emailHistoryTailBytes     = 128 * 1024
)

// readEmailHistoryLines 读取非空行（不加锁，调用方负责并发控制）
func readEmailHistoryLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	skipFirstPartial := false
	if info, err := f.Stat(); err == nil && info.Size() > emailHistoryFullReadLimit {
		if _, err := f.Seek(info.Size()-emailHistoryTailBytes, io.SeekStart); err == nil {
			skipFirstPartial = true
		}
	}

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if first && skipFirstPartial {
			// 从中间开始读，第一条大概率是被截断的半行
			first = false
			continue
		}
		first = false
		if line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		util.Log("邮件通知: 读取历史记录失败: %s", err)
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

// GetEmailHistorySummary 返回历史概要：总条数 + 最近 limit 条（新的在前）。
// 展示文案（时间格式、类型标签）统一交给前端按当前语言渲染，避免两份文案分叉。
func GetEmailHistorySummary(limit int) EmailHistorySummary {
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
		summary.Recent = append(summary.Recent, entry)
	}
	return summary
}
