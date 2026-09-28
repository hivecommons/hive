package github

import (
	"net/http"
	"testing"
	"time"
)

func resetRESTAccountingForTest(t *testing.T, now time.Time) func(time.Time) {
	t.Helper()
	old := sharedRESTAccounting
	store := &restAccountingStore{now: func() time.Time { return now }, coreRemaining: -1}
	sharedRESTAccounting = store
	t.Cleanup(func() { sharedRESTAccounting = old })
	return func(next time.Time) { store.now = func() time.Time { return next } }
}

func TestRESTAccountingTopConsumersTemplatesAndChargedRequests(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	advance := resetRESTAccountingForTest(t, now)
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "4999")
	h.Set("X-RateLimit-Reset", "1800000000")
	RecordRESTRequest("hive", http.MethodGet, "/repos/hivecommons/hive/issues", http.StatusOK, h)
	RecordRESTRequest("hive", http.MethodGet, "/repos/hivecommons/docs/issues", http.StatusOK, h)
	etag := http.Header{}
	etag.Set(etagCacheHeader, "revalidated")
	RecordRESTRequest("hive", http.MethodGet, "/repos/hivecommons/hive/issues", http.StatusOK, etag)
	RecordRESTRequest("agent:reviewer", http.MethodGet, "/repos/hivecommons/hive/pulls/123", http.StatusForbidden, h)

	top := RESTTopConsumers(10)
	if len(top) < 2 {
		t.Fatalf("top consumers length = %d, want at least 2", len(top))
	}
	if got := top[0].Endpoint; got != "/repos/{owner}/{repo}/issues" {
		t.Fatalf("top endpoint = %q, want issues template", got)
	}
	if top[0].Charged != 2 || top[0].Requests != 3 || top[0].NotModified != 1 {
		t.Fatalf("issues accounting = charged %d requests %d 304 %d, want 2/3/1", top[0].Charged, top[0].Requests, top[0].NotModified)
	}
	if !LowValueRESTWorkAllowed() {
		t.Fatal("4999 remaining should allow low-value work")
	}

	low := http.Header{}
	low.Set("X-RateLimit-Remaining", "500")
	RecordRESTRequest("hive", http.MethodGet, "/repos/hivecommons/hive/issues/1", http.StatusOK, low)
	if LowValueRESTWorkAllowed() {
		t.Fatal("remaining at reserve should shed low-value work")
	}

	advance(now.Add(restAccountingWindow + time.Minute))
	if got := RESTTopConsumers(10); len(got) != 0 {
		t.Fatalf("expired consumers = %#v, want none", got)
	}
}
