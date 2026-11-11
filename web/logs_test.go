package web

import (
	"sync"
	"testing"
)

// 日志缓冲区会被后台 RunOnce 与 Web 请求处理器并发写入（都经 log.Println ->
// MemoryLogs.Write）。必须加锁：裸切片 append 与裁剪用的重新切片在并发下
// 是数据竞争，裁剪还可能越界 panic，而 panic 出现在 RunOnce goroutine 里
// 会直接终止进程。用 `go test -race ./web/` 跑本用例可复现修复前的竞争。
func TestMemoryLogsConcurrentSafety(t *testing.T) {
	ml := &MemoryLogs{MaxNum: 50}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				_, _ = ml.Write([]byte("line\n"))
			}
		}()
	}
	// 与写并发地清空/读取
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			ml.Clear()
			_ = ml.Snapshot()
		}
	}()
	wg.Wait()

	if n := len(ml.Snapshot()); n > ml.MaxNum {
		t.Fatalf("日志缓冲超过上限: %d > %d", n, ml.MaxNum)
	}
}
