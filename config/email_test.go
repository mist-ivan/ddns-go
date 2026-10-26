package config

import (
	"strings"
	"testing"
)

func TestSplitRecipients(t *testing.T) {
	got := splitRecipients(" a@x.com, b@y.com;c@z.com\n\nd@w.com ")
	want := []string{"a@x.com", "b@y.com", "c@z.com", "d@w.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if n := len(splitRecipients("")); n != 0 {
		t.Fatalf("empty input should yield no recipients, got %d", n)
	}
}

func TestBuildMailMessageStripsHeaderInjection(t *testing.T) {
	msg := string(buildMailMessage(
		"a@x.com",
		[]string{"b@y.com"},
		"hello\r\nBcc: evil@x.com",
		"line1\r\nline2",
	))
	if strings.Contains(msg, "\r\nBcc:") {
		t.Fatal("subject must not be able to inject extra headers")
	}
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatal("subject must not be able to inject a Bcc header")
		}
	}
	if !strings.Contains(msg, "Content-Transfer-Encoding: base64") {
		t.Fatal("body should be base64 encoded")
	}
	if !strings.HasPrefix(msg, "From: a@x.com\r\n") {
		t.Fatalf("unexpected message start: %q", msg[:40])
	}
}

func TestComposeIPChangeMailChangeZH(t *testing.T) {
	conf := &Config{Lang: "zh"}
	subject, body := composeIPChangeMail(conf, emailState{LastIPv4: "1.1.1.1"}, "2.2.2.2", "", false)

	if !strings.Contains(subject, "1.1.1.1") || !strings.Contains(subject, "2.2.2.2") {
		t.Fatalf("subject should contain old and new IP: %s", subject)
	}
	if !strings.Contains(body, "上次: 1.1.1.1") {
		t.Fatalf("body should contain previous IP: %s", body)
	}
}

func TestComposeIPChangeMailBaselineEN(t *testing.T) {
	conf := &Config{Lang: "en"}
	_, body := composeIPChangeMail(conf, emailState{}, "2.2.2.2", "", true)

	if !strings.Contains(body, "first recorded IP baseline") {
		t.Fatalf("english baseline body missing: %s", body)
	}
	if !strings.Contains(body, "IPv4: 2.2.2.2") {
		t.Fatalf("body should contain the current IP: %s", body)
	}
}

func TestEmailValidate(t *testing.T) {
	if err := (&Email{}).Validate(); err == nil {
		t.Fatal("empty config should not validate")
	}
	e := &Email{SMTPHost: "smtp.x.com", To: "a@x.com", From: "b@x.com"}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
