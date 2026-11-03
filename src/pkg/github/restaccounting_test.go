package github

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func resetRESTAccountingForTest(t *testing.T, now time.Time) func(time.Time) {
	t.Helper()
	store := &restAccountingStore{now: func() time.Time { return now }, coreRemaining: -1}
	old := sharedRESTAccountingPtr.Swap(store)
	t.Cleanup(func() { sharedRESTAccountingPtr.Store(old) })
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

func TestRESTAccountingIgnoresForeignCoreBucketObservations(t *testing.T) {
	now := time.Date(2026, 10, 15, 13, 7, 58, 0, time.UTC)
	resetRESTAccountingForTest(t, now)
	reset := strconv.FormatInt(now.Add(45*time.Minute).Unix(), 10)

	daemon := http.Header{}
	daemon.Set("X-RateLimit-Limit", "15000")
	daemon.Set("X-RateLimit-Remaining", "14321")
	daemon.Set("X-RateLimit-Reset", reset)
	RecordRESTRequest("hive", http.MethodGet, "/repos/hivecommons/hive/issues", http.StatusOK, daemon)
	if rem, _, ok := CoreRateLimitSnapshot(); !ok || rem != 14321 {
		t.Fatalf("snapshot after daemon response = %d/%v, want 14321", rem, ok)
	}

	// An agent's gh/curl running without the App token sees the per-IP
	// unauthenticated bucket (limit 60). It must not move the hive snapshot.
	unauth := http.Header{}
	unauth.Set("X-RateLimit-Limit", "60")
	unauth.Set("X-RateLimit-Remaining", "46")
	unauth.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	RecordRESTRequest("agent:scanner", http.MethodGet, "/repos/hivecommons/hive/pulls/1", http.StatusOK, unauth)
	if rem, _, _ := CoreRateLimitSnapshot(); rem != 14321 {
		t.Fatalf("snapshot after unauthenticated agent response = %d, want 14321 unchanged", rem)
	}
	if !LowValueRESTWorkAllowed() {
		t.Fatal("unauthenticated agent observation must not shed low-value work")
	}

	// Same for the daemon itself: a 60-limit reading is never our bucket.
	RecordRESTRequest("hive", http.MethodGet, "/repos/hivecommons/hive/pulls/2", http.StatusOK, unauth)
	if rem, _, _ := CoreRateLimitSnapshot(); rem != 14321 {
		t.Fatalf("snapshot after unauthenticated daemon response = %d, want 14321 unchanged", rem)
	}

	// An agent response from a different authenticated bucket (e.g. a user
	// PAT with limit 5000) is also foreign once the daemon limit is known.
	pat := http.Header{}
	pat.Set("X-RateLimit-Limit", "5000")
	pat.Set("X-RateLimit-Remaining", "120")
	pat.Set("X-RateLimit-Reset", reset)
	RecordRESTRequest("agent:reviewer", http.MethodGet, "/repos/hivecommons/hive/pulls/3", http.StatusOK, pat)
	if rem, _, _ := CoreRateLimitSnapshot(); rem != 14321 {
		t.Fatalf("snapshot after foreign-bucket agent response = %d, want 14321 unchanged", rem)
	}

	// Agent traffic on the hive's own bucket keeps the snapshot fresh.
	own := http.Header{}
	own.Set("X-RateLimit-Limit", "15000")
	own.Set("X-RateLimit-Remaining", "14200")
	own.Set("X-RateLimit-Reset", reset)
	RecordInjectedRESTRequest("agent:scanner", http.MethodGet, "/repos/hivecommons/hive/pulls/4", http.StatusOK, own)
	if rem, _, _ := CoreRateLimitSnapshot(); rem != 14200 {
		t.Fatalf("snapshot after same-bucket agent response = %d, want 14200", rem)
	}

	// Responses without a limit header (legacy test fixtures, 304s from the
	// cache) keep the old behaviour.
	nolimit := http.Header{}
	nolimit.Set("X-RateLimit-Remaining", "14100")
	RecordRESTRequest("hive", http.MethodGet, "/repos/hivecommons/hive/pulls/5", http.StatusOK, nolimit)
	if rem, _, _ := CoreRateLimitSnapshot(); rem != 14100 {
		t.Fatalf("snapshot after headerless response = %d, want 14100", rem)
	}
}

func TestRESTAccountingPassThroughAgentCredentialDoesNotMoveSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 15, 13, 7, 58, 0, time.UTC)
	resetRESTAccountingForTest(t, now)
	reset := strconv.FormatInt(now.Add(45*time.Minute).Unix(), 10)

	h := http.Header{}
	h.Set("X-RateLimit-Limit", "15000")
	h.Set("X-RateLimit-Remaining", "14000")
	h.Set("X-RateLimit-Reset", reset)
	RecordRESTRequest("hive", http.MethodGet, "/repos/hivecommons/hive/issues", http.StatusOK, h)

	// Same limit value but remaining=0, e.g. another App installation token.
	drained := http.Header{}
	drained.Set("X-RateLimit-Limit", "15000")
	drained.Set("X-RateLimit-Remaining", "0")
	drained.Set("X-RateLimit-Reset", reset)
	RecordRESTRequest("agent:x", http.MethodGet, "/repos/hivecommons/hive/pulls/1", http.StatusOK, drained)
	if rem, _, _ := CoreRateLimitSnapshot(); rem != 14000 {
		t.Fatalf("snapshot after pass-through agent response = %d, want 14000 unchanged", rem)
	}
	if !LowValueRESTWorkAllowed() {
		t.Fatal("pass-through agent observation must not shed low-value work")
	}
	if got := RESTTopConsumers(10); len(got) != 2 {
		t.Fatalf("agent observation must still be recorded per caller, got %+v", got)
	}

	RecordRESTRequest("hive", http.MethodGet, "/repos/hivecommons/hive/pulls/2", http.StatusOK, drained)
	if rem, _, _ := CoreRateLimitSnapshot(); rem != 0 {
		t.Fatalf("snapshot after daemon response = %d, want 0", rem)
	}
}
