package dns

import (
	"strings"
	"testing"

	"github.com/jeessy2/ddns-go/v6/config"
)

// 0.17 回归：Ipv6Reg 也是源身份的一部分，不能和其他取 IP 参数相同的
// 无名配置共用状态键。
func TestSourceIdentityIncludesIpv6Reg(t *testing.T) {
	base := config.DnsConfig{}
	base.Ipv6.Enable = true
	base.Ipv6.GetType = "netInterface"
	base.Ipv6.NetInterface = "eth0"
	base.Ipv6.Domains = []string{"a.example.com"}
	base.Ipv6.Ipv6Reg = "@1"

	other := base
	other.Ipv6.Ipv6Reg = "@2"

	nameCount := map[string]int{}
	keyA, _, _ := sourceIdentity(base, 0, nameCount)
	keyB, _, _ := sourceIdentity(other, 1, nameCount)
	if keyA == keyB {
		t.Fatalf("different Ipv6Reg values share source key %q", keyA)
	}
}

// 0.17 回归：告警明细只应列出失败地址族的域名。
func TestSourceDomainsForFailedFamilyOnly(t *testing.T) {
	dc := config.DnsConfig{}
	dc.Ipv4.Domains = []string{"v4.example.com"}
	dc.Ipv6.Domains = []string{"v6.example.com"}

	got := strings.Join(sourceDomainsForFamilies(dc, []string{"IPv4"}), ",")
	if got != "v4.example.com" {
		t.Fatalf("IPv4-only failure domains = %q", got)
	}
	if strings.Contains(got, "v6.example.com") {
		t.Fatal("IPv4 failure must not list IPv6 domains")
	}

	both := strings.Join(sourceDomainsForFamilies(dc, []string{"IPv4", "IPv6"}), ",")
	if !strings.Contains(both, "v4.example.com") || !strings.Contains(both, "v6.example.com") {
		t.Fatalf("dual-family failure domains = %q", both)
	}
}
