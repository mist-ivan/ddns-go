package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEmailTestRunSemaphore(t *testing.T) {
	if !acquireEmailTestRun() {
		t.Fatal("first test slot should be available")
	}
	if acquireEmailTestRun() {
		t.Fatal("second concurrent test slot must be rejected")
	}
	releaseEmailTestRun()
	if !acquireEmailTestRun() {
		t.Fatal("slot should be available after release")
	}
	releaseEmailTestRun()
}

func TestEmailTestHourlyQuota(t *testing.T) {
	emailTestLock.Lock()
	oldStart, oldCount, oldLast := emailTestQuotaStart, emailTestQuotaCount, emailTestLast
	emailTestQuotaStart = time.Now()
	emailTestQuotaCount = emailTestMaxPerHour
	emailTestLast = time.Time{}
	emailTestLock.Unlock()
	defer func() {
		emailTestLock.Lock()
		emailTestQuotaStart, emailTestQuotaCount, emailTestLast = oldStart, oldCount, oldLast
		emailTestLock.Unlock()
	}()

	if acquireEmailTestSlot() {
		t.Fatal("quota-exhausted request should be rejected")
	}

	emailTestLock.Lock()
	emailTestQuotaCount = 0
	emailTestLast = time.Time{}
	emailTestLock.Unlock()
	if !acquireEmailTestSlot() {
		t.Fatal("request should be accepted after quota reset")
	}
}

func TestEmailTestRejectsOversizedBody(t *testing.T) {
	body := strings.NewReader(strings.Repeat("x", emailTestMaxBody+1))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/emailTest", body)
	EmailTest(rec, req)

	if rec.Code != 200 {
		t.Fatalf("unexpected status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("oversized body should be rejected: %s", rec.Body.String())
	}
}
