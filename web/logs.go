package web

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
)

// MemoryLogs 内存中的日志。
//
// 写入口是 log.Println：后台 RunOnce 与 Web 请求处理器都会并发调用。
// 旧实现直接对裸切片 append / 重新切片，没有任何同步——这是真实的数据竞争，
// 裁剪时的 Logs[len-MaxNum:] 在一次并发 append 之后还可能越界 panic，
// 而 panic 若发生在 RunOnce 的 goroutine 里会直接终止进程。
type MemoryLogs struct {
	mu     sync.Mutex
	MaxNum int      // 保存最大条数
	Logs   []string // 日志（仅在本类型的锁内读写）
}

func (mlogs *MemoryLogs) Write(p []byte) (n int, err error) {
	mlogs.mu.Lock()
	defer mlogs.mu.Unlock()

	mlogs.Logs = append(mlogs.Logs, string(p))
	if len(mlogs.Logs) > mlogs.MaxNum {
		// 复制新切片而不是原地重切：避免底层数组被无限向前推进
		mlogs.Logs = append([]string(nil), mlogs.Logs[len(mlogs.Logs)-mlogs.MaxNum:]...)
	}
	return len(p), nil
}

// Snapshot 返回日志副本，调用方可安全遍历/序列化
func (mlogs *MemoryLogs) Snapshot() []string {
	mlogs.mu.Lock()
	defer mlogs.mu.Unlock()
	return append([]string(nil), mlogs.Logs...)
}

// Clear 清空日志
func (mlogs *MemoryLogs) Clear() {
	mlogs.mu.Lock()
	defer mlogs.mu.Unlock()
	mlogs.Logs = mlogs.Logs[:0]
}

var mlogs = &MemoryLogs{MaxNum: 50}

// 初始化日志
func init() {
	log.SetOutput(io.MultiWriter(mlogs, os.Stdout))
	// log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

// Logs web
func Logs(writer http.ResponseWriter, request *http.Request) {
	logs, _ := json.Marshal(mlogs.Snapshot())
	writer.Write(logs)
}

// ClearLog 清空内存日志。只接受 POST，避免跨站 GET 导航顺手清掉日志。
func ClearLog(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mlogs.Clear()
}
