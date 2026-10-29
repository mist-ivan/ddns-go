package util

import (
	"os"
	"strconv"
	"sync/atomic"
)

const IPCacheTimesENV = "DDNS_IP_CACHE_TIMES"

// IpCache 上次IP缓存
type IpCache struct {
	Addr          string // 缓存地址
	Times         int    // 剩余次数
	TimesFailedIP int    // 获取ip失败的次数
}

// ForceCompareGlobal 请求下一轮忽略 Ipcache 强制比对。
// 会被 HTTP 处理器（保存配置后）和 RunOnce 的 goroutine 同时访问，必须用原子类型。
var ForceCompareGlobal atomic.Bool

func init() {
	ForceCompareGlobal.Store(true)
}

func (d *IpCache) Check(newAddr string) bool {
	if newAddr == "" {
		return true
	}
	// 地址改变 或 达到剩余次数
	if d.Addr != newAddr || d.Times <= 1 {
		IPCacheTimes, err := strconv.Atoi(os.Getenv(IPCacheTimesENV))
		if err != nil {
			IPCacheTimes = 5
		}
		d.Addr = newAddr
		d.Times = IPCacheTimes + 1
		return true
	}
	d.Addr = newAddr
	d.Times--
	return false
}
