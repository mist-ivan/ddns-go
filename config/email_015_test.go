package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 0.15: 状态文件里的旧格式 IPv6 基线必须先规范化，再与当前值比较。
// 直接写原始 JSON，模拟 0.13/0.14 已落盘的状态。
func TestUpgradeNonCanonicalBaseline(t *testing.T) {
	for _, stored := range []string{
		"2001:0DB8:0000::0001",
		"2001:db8:0:0:0:0:0:1",
	} {
		dir := t.TempDir()
		withConfigPath(t, filepath.Join(dir, "config.yaml"))
		srv := newRegSink(t)
		conf := regConf(srv.port, false)

		raw := fmt.Sprintf(`{"schema":2,"lastIpv6":%q,"sources":{"s1|v6":%q}}`, stored, stored)
		if err := os.WriteFile(emailStatePath(), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		resetEmailStateForTest()

		NotifyIPChangeEmail(conf, []SourceAddr{{Key: "s1", IPv6: "2001:db8::1"}},
			FetchStatus{IPv6Expected: true, IPv6OK: true})

		if mail, ok := srv.await(400 * time.Millisecond); ok {
			t.Fatalf("baseline %q was treated as a change; body:\n%s", stored, bodyOf(mail))
		}
	}
}

// updateEmailState 必须把两次更新都保留下来，而不是后写者覆盖先写者。
func TestUpdateEmailStatePreservesBothUpdates(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	storeEmailState(emailState{
		FailCount: 2,
		LastIPv4:  "1.1.1.1",
		Sources:   map[string]string{"s1|v4": "1.1.1.1"},
	})

	updateEmailState(func(state *emailState) {
		state.FailCount = 0
		state.NextRetryAt = 0
	})
	updateEmailState(func(state *emailState) {
		state.LastSent = 123
	})

	st := loadEmailState()
	if st.FailCount != 0 || st.LastSent != 123 {
		t.Fatalf("lost update: failCount=%d lastSent=%d", st.FailCount, st.LastSent)
	}
}

// 并发更新只应串行化，不应互相覆盖。
func TestUpdateEmailStateConcurrent(t *testing.T) {
	dir := t.TempDir()
	withConfigPath(t, filepath.Join(dir, "config.yaml"))
	defer resetEmailStateForTest()

	storeEmailState(emailState{
		FailCount: 2,
		LastIPv4:  "1.1.1.1",
		Sources:   map[string]string{"s1|v4": "1.1.1.1"},
	})

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			updateEmailState(func(state *emailState) {
				state.FailCount = 0
				state.NextRetryAt = 0
			})
		}()
		go func() {
			defer wg.Done()
			updateEmailState(func(state *emailState) {
				state.LastSent = 456
			})
		}()
	}
	wg.Wait()

	st := loadEmailState()
	if st.FailCount != 0 || st.NextRetryAt != 0 || st.LastSent != 456 {
		t.Fatalf("concurrent update lost fields: failCount=%d nextRetryAt=%d lastSent=%d",
			st.FailCount, st.NextRetryAt, st.LastSent)
	}
}

// Subject 的 encoded-word 不能拆，因此不强制 76 字节；硬限制 998 和
// RFC 2047 单个 encoded-word 75 字节必须满足。
func TestSubjectEncodedWordLimits(t *testing.T) {
	var changes []sourceChange
	for i := 0; i < 3; i++ {
		changes = append(changes, sourceChange{
			Key:      fmt.Sprintf("s%d", i),
			IPv4:     fmt.Sprintf("2.2.%d.1", i),
			PrevIPv4: fmt.Sprintf("1.1.%d.1", i),
			IPv6:     fmt.Sprintf("240e:3b0:2c31:1:a%d::1", i),
			PrevIPv6: fmt.Sprintf("240e:3b0:2c31:1:b%d::1", i),
		})
	}
	subject := composeMultiChangeSubject(true, changes)
	msg := string(buildMailMessage("a@example.com", []string{"b@example.com"}, subject, "body"))

	var subjectLine string
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, "Subject: ") {
			subjectLine = line
			break
		}
	}
	if subjectLine == "" {
		t.Fatal("no Subject header")
	}
	if len(subjectLine) > 998 {
		t.Fatalf("Subject line %d exceeds RFC 5322 hard limit", len(subjectLine))
	}
	for _, word := range strings.Fields(strings.TrimPrefix(subjectLine, "Subject: ")) {
		if len(word) > 75 {
			t.Fatalf("encoded-word %d exceeds RFC 2047 limit: %q", len(word), word)
		}
	}
}
