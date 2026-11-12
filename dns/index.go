package dns

import (
	"fmt"
	"hash/fnv"
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

// pendingRuns 记录"保存后立即执行"的排队数量：1 表示正在跑，2 表示还有一个在等。
// 用计数而非布尔值：原实现里"已有任务在跑"时新的触发会被直接丢弃，
// 用户保存配置后可能要等下一个定时周期（默认 10 分钟）才生效。
var pendingRuns atomic.Int32

// maxPendingRuns 允许 1 个在跑 + 1 个在等，再多就合并掉，避免连点保存堆 goroutine
const maxPendingRuns = 2

// RunOnceAsync 异步触发一次检测；排队已满时跳过（合并为下一轮）。
func RunOnceAsync() {
	if pendingRuns.Add(1) > maxPendingRuns {
		pendingRuns.Add(-1)
		return
	}
	go func() {
		defer pendingRuns.Add(-1)
		RunOnce()
	}()
}

// WaitPendingRun 等待已排队的异步检测结束。
//
// 仅用于测试：RunOnceAsync 起的 goroutine 会跨测试用例存活，若紧接着的用例
// 改了配置路径，那个 goroutine 里 RunOnce 读配置失败会回写 ForceCompareGlobal，
// 污染下一个用例的断言（实测 HEAD 上 8 次全量测试偶发失败 2 次）。
func WaitPendingRun(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for pendingRuns.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
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
		detectedIPv4, detectedIPv6           []string
		ipv4ExpectedCount, ipv6ExpectedCount int
		ipv4SuccessCount, ipv6SuccessCount   int
		// sources 按配置源记录本轮地址，供邮件模块"按源比对"
		sources []config.SourceAddr
		// failedSources 记录具体哪个配置源/地址族失败，供告警正文明细
		failedSources []config.FailedSource
		// configuredSources 是本轮仍然有效的源 Key 集合
		configuredSources []string
	)

	// 重名配置需要内容 hash 才能区分；先统计全量名称，避免“第一个叫 A、
	// 第二个也叫 A”时第二个的 key 依赖遍历顺序。
	nameCount := make(map[string]int)
	for _, dc := range conf.DnsConf {
		if n := strings.TrimSpace(dc.Name); n != "" {
			nameCount[n]++
		}
	}

	for i, dc := range conf.DnsConf {
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
		// 按配置粒度统计期望/成功数：Expected 由 Domains.GetNewIp 根据
		// 解析后的合法域名设置，不能再拿原始字符串判断，否则非法域名
		// 会被误算成“取 IP 失败”。
		if domains.Ipv4Configured {
			ipv4ExpectedCount++
			if domains.Ipv4Addr != "" {
				ipv4SuccessCount++
			}
		}
		if domains.Ipv6Configured {
			ipv6ExpectedCount++
			if domains.Ipv6Addr != "" {
				ipv6SuccessCount++
			}
		}
		key, label, sourceDomains := sourceIdentity(dc, i, nameCount)
		if domains.Ipv4Configured || domains.Ipv6Configured {
			configuredSources = append(configuredSources, key)
		}
		var failedFamilies []string
		if domains.Ipv4Configured && domains.Ipv4Addr == "" {
			failedFamilies = append(failedFamilies, "IPv4")
		}
		if domains.Ipv6Configured && domains.Ipv6Addr == "" {
			failedFamilies = append(failedFamilies, "IPv6")
		}
		if len(failedFamilies) > 0 {
			failedSources = append(failedSources, config.FailedSource{
				Key:      key,
				Label:    label,
				Domains:  sourceDomains,
				Families: failedFamilies,
			})
		}
		sources = append(sources, config.SourceAddr{
			Key:     key,
			Label:   label,
			Domains: sourceDomains,
			IPv4:    domains.Ipv4Addr,
			IPv6:    domains.Ipv6Addr,
		})
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
	config.NotifyIPChangeEmail(&conf, sources, config.FetchStatus{
		IPv4Expected:      ipv4Expected,
		IPv4OK:            ipv4Got,
		IPv6Expected:      ipv6Expected,
		IPv6OK:            ipv6Got,
		FailedSources:     failedSources,
		ConfiguredSources: configuredSources,
	})
}

// sourceDomains 返回配置里全部非空域名（去重、排序）。
func sourceDomains(dc config.DnsConfig) []string {
	seen := make(map[string]bool)
	var domains []string
	for _, list := range [][]string{dc.Ipv4.Domains, dc.Ipv6.Domains} {
		for _, d := range list {
			d = strings.TrimSpace(d)
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			domains = append(domains, d)
		}
	}
	sort.Strings(domains)
	return domains
}

// sourceIdentity 生成跨轮次稳定的配置源标识。
//
// 旧实现无名称时直接用 "#索引"：删除/调换前面的配置会让后面的配置复用
// 旧索引，从而拿已删除配置的基线比较，误发 IP 变更邮件。
// 现在：
//   - 唯一名称仍用名称（兼容已有状态文件）；
//   - 无名称/重名时用 "域名/提供方/取 IP 方式" 的 hash，删除/重排不会变；
//   - label 只用于邮件展示，不参与状态键。
func sourceIdentity(dc config.DnsConfig, index int, nameCount map[string]int) (key, label string, domains []string) {
	domains = sourceDomains(dc)
	name := strings.TrimSpace(dc.Name)

	if name != "" {
		label = name
		if nameCount[name] <= 1 {
			return name, label, domains
		}
	} else if len(domains) > 0 {
		label = domains[0]
	} else {
		label = fmt.Sprintf("#%d", index+1)
	}

	h := fnv.New64a()
	writeHashPart := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	writeHashPart(dc.DNS.Name)
	writeHashPart(dc.DNS.ID)
	writeHashPart(dc.DNS.ExtParam)
	writeHashPart(dc.Ipv4.GetType)
	writeHashPart(dc.Ipv4.URL)
	writeHashPart(dc.Ipv4.NetInterface)
	writeHashPart(dc.Ipv4.Cmd)
	writeHashPart(dc.Ipv6.GetType)
	writeHashPart(dc.Ipv6.URL)
	writeHashPart(dc.Ipv6.NetInterface)
	writeHashPart(dc.Ipv6.Cmd)
	writeHashPart(dc.HttpInterface)
	for _, d := range domains {
		writeHashPart(d)
	}
	suffix := fmt.Sprintf("%x", h.Sum64())
	key = label + "#" + suffix
	if name != "" && nameCount[name] > 1 {
		// 重名配置在邮件里也要能区分，显示短 hash。
		short := suffix
		if len(short) > 6 {
			short = short[:6]
		}
		label = fmt.Sprintf("%s (%s)", name, short)
	}
	return key, label, domains
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
