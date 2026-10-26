package dns

import (
	"sort"
	"strings"
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
)

// RunTimer 定时运行
func RunTimer(delay time.Duration) {
	for {
		RunOnce()
		time.Sleep(delay)
	}
}

// RunOnce RunOnce
func RunOnce() {
	conf, err := config.GetConfigCached()
	if err != nil {
		return
	}
	if util.ForceCompareGlobal || len(Ipcache) != len(conf.DnsConf) {
		Ipcache = [][2]util.IpCache{}
		for range conf.DnsConf {
			Ipcache = append(Ipcache, [2]util.IpCache{{}, {}})
		}
	}

	// 收集本轮所有配置检测到的公网 IP，用于邮件提醒
	var (
		detectedIPv4, detectedIPv6                   []string
		ipv4Expected, ipv6Expected, ipv4Got, ipv6Got bool
	)

	for i, dc := range conf.DnsConf {
		if dc.Ipv4.Enable {
			ipv4Expected = true
		}
		if dc.Ipv6.Enable {
			ipv6Expected = true
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
			ipv4Got = true
		}
		if domains.Ipv6Addr != "" {
			detectedIPv6 = append(detectedIPv6, domains.Ipv6Addr)
			ipv6Got = true
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

	// IP 变更与获取失败告警（与 webhook 相互独立）
	// 配置了某一族地址、但整轮都没取到，视为本轮检测失败
	fetchFailed := (ipv4Expected && !ipv4Got) || (ipv6Expected && !ipv6Got)
	config.NotifyIPChangeEmail(&conf, uniqueAddr(detectedIPv4), uniqueAddr(detectedIPv6), fetchFailed)

	util.ForceCompareGlobal = false
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
