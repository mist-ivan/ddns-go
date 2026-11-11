package web

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// 0.14: 清空日志前端必须和后端一样使用 POST。
func TestClearLogFrontendUsesPost(t *testing.T) {
	html, err := os.ReadFile("writing.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(html)
	if !strings.Contains(body, `request.post("./clearLog"`) {
		t.Fatal("writing.html does not call /clearLog with POST")
	}
	if strings.Contains(body, `request.get("./clearLog"`) {
		t.Fatal("writing.html still calls /clearLog with GET")
	}

	rec := httptest.NewRecorder()
	ClearLog(rec, httptest.NewRequest("GET", "/clearLog", nil))
	if rec.Code != 405 {
		t.Fatalf("GET /clearLog = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	ClearLog(rec, httptest.NewRequest("POST", "/clearLog", nil))
	if rec.Code != 200 {
		t.Fatalf("POST /clearLog = %d, want 200", rec.Code)
	}
}

// 0.14: 邮件加密方式有“自动（按端口）”，避免自定义端口被 UI 默认改成 SSL。
func TestWritingHasAutoEncryptOption(t *testing.T) {
	html, err := os.ReadFile("writing.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `value="" data-i18n="Auto (by port)"`) {
		t.Fatal("writing.html is missing the auto encryption option")
	}
}
