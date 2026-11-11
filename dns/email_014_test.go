package dns

import (
	"testing"

	"github.com/jeessy2/ddns-go/v6/config"
)

// 无名称配置的 key 必须由内容派生，不能随数组下标变化。
func TestSourceIdentityStableForUnnamedConfig(t *testing.T) {
	dc := config.DnsConfig{}
	dc.Ipv4.GetType = "url"
	dc.Ipv4.URL = "http://127.0.0.1/ip"
	dc.Ipv4.Domains = []string{"b.example.com"}
	nameCount := map[string]int{}

	key0, label0, domains := sourceIdentity(dc, 0, nameCount)
	key1, label1, _ := sourceIdentity(dc, 1, nameCount)
	if key0 != key1 {
		t.Fatalf("unnamed source key changed with index: %q vs %q", key0, key1)
	}
	if label0 != "b.example.com" || label1 != "b.example.com" {
		t.Fatalf("unexpected labels: %q %q", label0, label1)
	}
	if len(domains) != 1 || domains[0] != "b.example.com" {
		t.Fatalf("unexpected domains: %#v", domains)
	}
}

// 重名配置也必须靠内容区分，而不是靠遍历顺序。
func TestSourceIdentityDuplicateNamesStable(t *testing.T) {
	base := config.DnsConfig{Name: "home"}
	base.Ipv4.GetType = "url"
	base.Ipv4.URL = "http://127.0.0.1/ip1"
	base.Ipv4.Domains = []string{"a.example.com"}

	other := config.DnsConfig{Name: "home"}
	other.Ipv4.GetType = "url"
	other.Ipv4.URL = "http://127.0.0.1/ip2"
	other.Ipv4.Domains = []string{"b.example.com"}

	nameCount := map[string]int{"home": 2}
	keyA, _, _ := sourceIdentity(base, 0, nameCount)
	keyB, _, _ := sourceIdentity(other, 1, nameCount)
	if keyA == keyB {
		t.Fatalf("duplicate-name configs share key %q", keyA)
	}
	if again, _, _ := sourceIdentity(base, 5, nameCount); again != keyA {
		t.Fatalf("duplicate-name key changed with index: %q vs %q", keyA, again)
	}
}

// 非法域名不应被算作“期望取 IP”：GetNewIp 不会发起请求，Configured 必须为 false。
func TestDomainsConfiguredUsesParsedDomains(t *testing.T) {
	dc := config.DnsConfig{}
	dc.Ipv4.Enable = true
	dc.Ipv4.GetType = "url"
	dc.Ipv4.URL = "http://127.0.0.1/ip"
	dc.Ipv4.Domains = []string{"not-a-domain"}

	var d config.Domains
	d.GetNewIp(&dc)
	if d.Ipv4Configured {
		t.Fatal("invalid-only domain reported as configured")
	}
	if len(d.Ipv4Domains) != 0 || d.Ipv4Addr != "" {
		t.Fatalf("unexpected parsed/fetched result: domains=%d addr=%q", len(d.Ipv4Domains), d.Ipv4Addr)
	}
}
