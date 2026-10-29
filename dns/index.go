package dns

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

// DNS interface
type DNS interface {
	Init(dnsConf *config.DnsConfig, ipv4cache *util.IpCache, ipv6cache *util.IpCache)
	// 添加或更新IPv4/IPv6记录
	AddUpdateDomainRecords() (domains config.Domains)
}

var (
	Addresses = []string{
		alidnsEndpoint,
		aliesaEndpoint,
		baiduEndpoint,
		zonesAPI,
		recordListAPI,
		huaweicloudEndpoint,
		nameCheapEndpoint,
		nameSiloListRecordEndpoint,
		porkbunEndpoint,
		tencentCloudEndPoint,
		dynadotEndpoint,
		dynv6Endpoint,
		gcoreAPIEndpoint,
		edgeoneEndPoint,
		rainyunEndpoint,
		CloudnsEndpoint,
		desecEndpoint,
	}

	Ipcache = [][2]util.IpCache{}

	// runOnceMu 串行化 RunOnce：定时器与“保存配置后立即执行”可能同时触发，
	// 并发运行会让邮件状态判断重复、Ipcache 竞争。
	runOnceMu sync.Mutex

	// lastRunUnix 最近一次检测完成的时间（unix 秒），供页面状态面板展示
	lastRunUnix atomic.Int64
	// intervalSec 配置的检测间隔（秒）
	intervalSec atomic.Int64
	// nextRunUnix 定时器下一次触发的预计时间（unix 秒）；手动触发的 RunOnce 不会改它
	nextRunUnix atomic.Int64

	// 最近一轮实际检测到的公网 IP，和"上次成功通知的 IP"区分开：
	// 后者可能因为发送失败或告警场景停留在旧值。
	lastDetectedIPv4 atomic.Value
	lastDetectedIPv6 atomic.Value
)

// SetInterval 记录检测间隔，供页面计算“下次检测”时间
func SetInterval(d time.Duration) {
	intervalSec.Store(int64(d.Seconds()))
}

// LastRunAt 返回最近一次检测完成的时间（unix 秒），0 表示尚未执行过
func LastRunAt() int64 { return lastRunUnix.Load() }

// IntervalSeconds 返回配置的检测间隔（秒），未知时为 0
func IntervalSeconds() int64 { return intervalSec.Load() }

// NextRunAt 返回定时器下一次触发的预计时间（unix 秒），0 表示未知
func NextRunAt() int64 { return nextRunUnix.Load() }

// LastDetected 返回最近一轮实际检测到的公网 IP（可能为空字符串）
func LastDetected() (ipv4, ipv6 string) {
	if v, ok := lastDetectedIPv4.Load().(string); ok {
		ipv4 = v
	}
	if v, ok := lastDetectedIPv6.Load().(string); ok {
		ipv6 = v
	}
	return ipv4, ipv6
}

// RunTimer 定时运行
func RunTimer(delay time.Duration) {
	for {
		RunOnce()
		// 记录"下一次定时触发"的时间：只在这里写，手动触发的 RunOnce 不会污染它
		nextRunUnix.Store(time.Now().Add(delay).Unix())
		time.Sleep(delay)
	}
}

// RunOnce RunOnce
func RunOnce() {
	runOnceMu.Lock()
	defer runOnceMu.Unlock()
	// 用函数字面量：defer 的实参在注册时求值，写成 defer f(time.Now()) 记的是开始时间
	defer func() { lastRunUnix.Store(time.Now().Unix()) }()

	// 原子地取走"强制比对"标志：定时器那轮与保存后触发的那轮不会互相覆盖
	forceCompare := util.ForceCompareGlobal.Swap(false)

	conf, err := config.GetConfigCached()
	if err != nil {
		// 配置读不出来，没人消费这个标志，还回去
		util.ForceCompareGlobal.Store(true)
		return
	}
	if forceCompare || len(Ipcache) != len(conf.DnsConf) {
		Ipcache = [][2]util.IpCache{}
		for range conf.DnsConf {
			Ipcache = append(Ipcache, [2]util.IpCache{{}, {}})
		}
	}

	// 收集本轮所有配置检测到的公网 IP，用于邮件提醒
	var (
		detectedIPv4, detectedIPv6                   []string
		ipv4ExpectedCount, ipv6ExpectedCount         int
		ipv4SuccessCount, ipv6SuccessCount           int
	)

	for i, dc := range conf.DnsConf {
		// 只有“启用且确实配置了域名”的地址族才算期望；
		// 否则新增配置默认勾选 IPv6 但只填 IPv4 域名时，会误报 IPv6 获取失败。
		if dc.Ipv4.Enable && hasDomains(dc.Ipv4.Domains) {
			ipv4ExpectedCount++
		}
		if dc.Ipv6.Enable && hasDomains(dc.Ipv6.Domains) {
			ipv6ExpectedCount++
		}
		var dnsSelected DNS
		switch dc.DNS.Name {
		case "alidns":
			dnsSelected = &Alidns{}
		case "aliesa":
			dnsSelected = &Aliesa{}
		case "tencentcloud":
			dnsSelected = &TencentCloud{}
		case "trafficroute":
			dnsSelected = &TrafficRoute{}
		case "dnspod":
			dnsSelected = &Dnspod{}
		case "dnsla":
			dnsSelected = &Dnsla{}
		case "cloudflare":
			dnsSelected = &Cloudflare{}
		case "huaweicloud":
			dnsSelected = &Huaweicloud{}
		case "callback":
			dnsSelected = &Callback{}
		case "baiducloud":
			dnsSelected = &BaiduCloud{}
		case "porkbun":
			dnsSelected = &Porkbun{}
		case "godaddy":
			dnsSelected = &GoDaddyDNS{}
		case "namecheap":
			dnsSelected = &NameCheap{}
		case "namesilo":
			dnsSelected = &NameSilo{}
		case "vercel":
			dnsSelected = &Vercel{}
		case "dynadot":
			dnsSelected = &Dynadot{}
		case "dynv6":
			dnsSelected = &Dynv6{}
		case "spaceship":
			dnsSelected = &Spaceship{}
		case "nowcn":
			dnsSelected = &Nowcn{}
		case "eranet":
			dnsSelected = &Eranet{}
		case "tnethk":
			dnsSelected = &Tnethk{}
		case "gcore":
			dnsSelected = &Gcore{}
		case "edgeone":
			dnsSelected = &EdgeOne{}
		case "nsone":
			dnsSelected = &NSOne{}
		case "name_com":
			dnsSelected = &NameCom{}
		case "rainyun":
			dnsSelected = &Rainyun{}
		case "hipmdnsmgr":
			dnsSelected = &HiPMDnsMgr{}
		case "cloudns":
			dnsSelected = &ClouDNS{}
		case "desec":
			dnsSelected = &DeSEC{}
		default:
			dnsSelected = &Alidns{}
		}
		dnsSelected.Init(&dc, &Ipcache[i][0], &Ipcache[i][1])
		domains := dnsSelected.AddUpdateDomainRecords()
		if domains.Ipv4Addr != "" {
			detectedIPv4 = append(detectedIPv4, domains.Ipv4Addr)
		}
		if domains.Ipv6Addr != "" {
			detectedIPv6 = append(detectedIPv6, domains.Ipv6Addr)
		}
		// 按配置粒度统计成功数：任一"期望"的配置没取到地址都要算失败，
		// 否则多配置里坏掉一个永远不会触发告警。
		if dc.Ipv4.Enable && hasDomains(dc.Ipv4.Domains) && domains.Ipv4Addr != "" {
			ipv4SuccessCount++
		}
		if dc.Ipv6.Enable && hasDomains(dc.Ipv6.Domains) && domains.Ipv6Addr != "" {
			ipv6SuccessCount++
		}
		// webhook
		v4Status, v6Status := config.ExecWebhook(&domains, &conf)
		// 重置单个cache
		if v4Status == config.UpdatedFailed {
			Ipcache[i][0] = util.IpCache{}
		}
		if v6Status == config.UpdatedFailed {
			Ipcache[i][1] = util.IpCache{}
		}
	}

	ipv4Expected := ipv4ExpectedCount > 0
	ipv6Expected := ipv6ExpectedCount > 0
	ipv4Got := ipv4Expected && ipv4SuccessCount == ipv4ExpectedCount
	ipv6Got := ipv6Expected && ipv6SuccessCount == ipv6ExpectedCount

	// 面板展示用：所有配置检测到的地址
	lastDetectedIPv4.Store(uniqueAddr(detectedIPv4))
	lastDetectedIPv6.Store(uniqueAddr(detectedIPv6))

	// IP 变更与获取失败告警（与 webhook 相互独立）。
	// 邮件比对只用"主地址"（按配置顺序第一个非空值）：
	// 多个配置取到不同地址时，拼成 "a,b" 会因单个源抖动被误判为 IP 变更。
	config.NotifyIPChangeEmail(&conf, primaryAddr(detectedIPv4), primaryAddr(detectedIPv6), config.FetchStatus{
		IPv4Expected: ipv4Expected,
		IPv4OK:       ipv4Got,
		IPv6Expected: ipv6Expected,
		IPv6OK:       ipv6Got,
	})
}

// hasDomains 判断域名列表里是否有非空项
func hasDomains(domains []string) bool {
	for _, d := range domains {
		if strings.TrimSpace(d) != "" {
			return true
		}
	}
	return false
}

// primaryAddr 取第一个非空地址，作为"主地址"参与邮件比对。
// 不做排序：排序会让地址集合变化时主地址跳来跳去。
func primaryAddr(addrs []string) string {
	for _, addr := range addrs {
		if a := strings.TrimSpace(addr); a != "" {
			return a
		}
	}
	return ""
}

// uniqueAddr 去重并拼接地址，正常情况下只会有一个公网 IP
func uniqueAddr(addrs []string) string {
	seen := make(map[string]bool, len(addrs))
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		addr = strings.TrimSpace(addr)
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
