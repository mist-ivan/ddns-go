package dns

import "testing"

// 邮件比对只用主地址：多配置取到不同 IP 时，拼接串会因单源抖动误报变更
func TestPrimaryAddr(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"空列表", nil, ""},
		{"单值", []string{"1.2.3.4"}, "1.2.3.4"},
		{"取第一个", []string{"1.2.3.4", "5.6.7.8"}, "1.2.3.4"},
		{"跳过空串", []string{"", "  ", "5.6.7.8"}, "5.6.7.8"},
		{"去空白", []string{"  9.9.9.9  "}, "9.9.9.9"},
	}
	for _, tc := range cases {
		if got := primaryAddr(tc.in); got != tc.want {
			t.Errorf("%s: primaryAddr(%v) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestUniqueAddr(t *testing.T) {
	if got := uniqueAddr([]string{"1.2.3.4", "1.2.3.4", "", "5.6.7.8"}); got != "1.2.3.4,5.6.7.8" {
		t.Fatalf("uniqueAddr = %q", got)
	}
}
