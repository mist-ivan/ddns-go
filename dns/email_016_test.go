package dns

import (
	"testing"

	"github.com/jeessy2/ddns-go/v6/config"
)

// 同一 provider/域名/URL、不同 DNS 账号 ID 的两个无名配置，不能共用源 key。
func TestSourceIdentityIncludesDNSAccount(t *testing.T) {
	base := config.DnsConfig{}
	base.DNS.Name = "callback"
	base.DNS.ID = "account-a"
	base.Ipv4.GetType = "url"
	base.Ipv4.URL = "http://127.0.0.1/ip"
	base.Ipv4.Domains = []string{"a.example.com"}

	other := base
	other.DNS.ID = "account-b"

	nameCount := map[string]int{}
	keyA, _, _ := sourceIdentity(base, 0, nameCount)
	keyB, _, _ := sourceIdentity(other, 1, nameCount)
	if keyA == keyB {
		t.Fatalf("different DNS accounts share source key %q", keyA)
	}
}
