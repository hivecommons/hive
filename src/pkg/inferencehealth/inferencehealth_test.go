package inferencehealth

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{401, ClassAuth},
		{403, ClassAuth},
		{429, ClassBudget},
		{500, Class5xx},
		{503, Class5xx},
		{599, Class5xx},
		{200, ClassOther},
		{404, ClassOther},
		{600, ClassOther},
		{0, ClassOther},
	}
	for _, tc := range cases {
		if got := ClassifyHTTPStatus(tc.status); got != tc.want {
			t.Errorf("ClassifyHTTPStatus(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantClass  string
		wantStatus int
	}{
		{"nil", nil, "", 0},
		{"dns typed", &net.DNSError{Err: "lookup failed", Name: "gw.example"}, ClassDNS, 0},
		{"op typed", &net.OpError{Op: "dial", Err: errors.New("timeout")}, ClassConnect, 0},
		{"dns unwrapped from url.Error", &url.Error{Op: "Get", URL: "http://x", Err: &net.DNSError{Err: "nope", Name: "x"}}, ClassDNS, 0},
		{"op unwrapped from url.Error", &url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "dial", Err: errors.New("refused")}}, ClassConnect, 0},
		{"no such host string", errors.New("lookup gw: no such host"), ClassDNS, 0},
		{"server misbehaving string", errors.New("lookup gw: server misbehaving"), ClassDNS, 0},
		{"connection refused string", errors.New("dial tcp: connection refused"), ClassConnect, 0},
		{"connect prefix string", errors.New("connect: network unreachable"), ClassConnect, 0},
		{"io timeout string", errors.New("read tcp: i/o timeout"), ClassConnect, 0},
		{"http 401", errors.New("gateway said HTTP 401"), ClassAuth, 401},
		{"http 403", errors.New("gateway said http 403 forbidden"), ClassAuth, 403},
		{"http 429", errors.New("http 429 too many requests"), ClassBudget, 429},
		{"http 5xx", errors.New("http 502 bad gateway"), Class5xx, 0},
		{"other", errors.New("something odd"), ClassOther, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, status := ClassifyError(tc.err)
			if class != tc.wantClass || status != tc.wantStatus {
				t.Errorf("ClassifyError(%v) = (%q, %d), want (%q, %d)", tc.err, class, status, tc.wantClass, tc.wantStatus)
			}
		})
	}
}

func TestStoreRecordErrorAndSnapshot(t *testing.T) {
	s := NewStore()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.RecordError("GW-One", errors.New("dial tcp: connection refused"), at)

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot() len = %d, want 1", len(snap))
	}
	got := snap[0]
	if got.Name != "GW-One" {
		t.Errorf("Name = %q, want %q", got.Name, "GW-One")
	}
	if got.ErrorClass != ClassConnect {
		t.Errorf("ErrorClass = %q, want %q", got.ErrorClass, ClassConnect)
	}
	if got.LastErrorAt != "2026-09-01T12:00:00Z" {
		t.Errorf("LastErrorAt = %q, want RFC3339 UTC", got.LastErrorAt)
	}
}

func TestStoreRecordErrorIgnoresInvalid(t *testing.T) {
	s := NewStore()
	s.RecordError("", errors.New("boom"), time.Now())
	s.RecordError("  ", errors.New("boom"), time.Now())
	s.RecordError("gw", nil, time.Now())
	if got := s.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot() len = %d, want 0", len(got))
	}

	var nilStore *Store
	nilStore.RecordError("gw", errors.New("boom"), time.Now()) // must not panic
	nilStore.RecordHTTPError("gw", 500, "boom", time.Now())    // must not panic
	nilStore.Clear("gw")                                       // must not panic
	if got := nilStore.Snapshot(); got != nil {
		t.Errorf("nil Store Snapshot() = %v, want nil", got)
	}
}

func TestStoreRecordHTTPError(t *testing.T) {
	s := NewStore()
	at := time.Date(2026, 9, 2, 8, 30, 0, 0, time.UTC)
	s.RecordHTTPError(" gw ", 429, "quota exceeded", at)

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot() len = %d, want 1", len(snap))
	}
	got := snap[0]
	if got.Name != "gw" {
		t.Errorf("Name = %q, want trimmed %q", got.Name, "gw")
	}
	if got.ErrorClass != ClassBudget || got.HTTPStatus != 429 {
		t.Errorf("got class %q status %d, want %q 429", got.ErrorClass, got.HTTPStatus, ClassBudget)
	}
	if got.Detail != "quota exceeded" {
		t.Errorf("Detail = %q", got.Detail)
	}
}

func TestStoreRecordDedupCaseInsensitive(t *testing.T) {
	s := NewStore()
	s.RecordHTTPError("Gateway", 500, "first", time.Now())
	s.RecordHTTPError("gateway", 503, "second", time.Now())

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot() len = %d, want 1 (case-insensitive dedup)", len(snap))
	}
	if snap[0].HTTPStatus != 503 || snap[0].Detail != "second" {
		t.Errorf("latest record should win, got %+v", snap[0])
	}
}

func TestStoreClear(t *testing.T) {
	s := NewStore()
	s.RecordHTTPError("gw", 500, "boom", time.Now())
	s.Clear("GW") // case-insensitive
	if got := s.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot() after Clear len = %d, want 0", len(got))
	}
	s.Clear("") // no-op, must not panic
}

func TestStoreSnapshotSorted(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.RecordHTTPError("zeta", 500, "", now)
	s.RecordHTTPError("Alpha", 500, "", now)
	s.RecordHTTPError("mid", 500, "", now)

	snap := s.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot() len = %d, want 3", len(snap))
	}
	if snap[0].Name != "Alpha" || snap[1].Name != "mid" || snap[2].Name != "zeta" {
		t.Errorf("Snapshot not sorted case-insensitively by name: %v", []string{snap[0].Name, snap[1].Name, snap[2].Name})
	}
}

func TestRecordDefaultsEmptyClassToOther(t *testing.T) {
	s := NewStore()
	s.record(GatewayStatus{Name: "gw", ErrorClass: "  "})
	snap := s.Snapshot()
	if len(snap) != 1 || snap[0].ErrorClass != ClassOther {
		t.Errorf("record with blank class = %+v, want ErrorClass %q", snap, ClassOther)
	}
}

func TestZeroValueStoreLazilyInitializes(t *testing.T) {
	var s Store
	s.RecordHTTPError("gw", 500, "boom", time.Now())
	if got := s.Snapshot(); len(got) != 1 {
		t.Errorf("zero-value Store Snapshot() len = %d, want 1", len(got))
	}
}

func TestReason(t *testing.T) {
	cases := []struct {
		name string
		st   GatewayStatus
		want string
	}{
		{"dns", GatewayStatus{Name: "gw", ErrorClass: ClassDNS}, "inference gateway 'gw' unreachable (dns)"},
		{"dns with host", GatewayStatus{Name: "vllm", Host: "hive-vllm-svc.hive-inference.svc.cluster.local", ErrorClass: ClassDNS}, "vllm endpoint hive-vllm-svc.hive-inference.svc.cluster.local not resolvable on this cluster — set inference.vllm.endpoint or disable"},
		{"connect", GatewayStatus{Name: "gw", ErrorClass: ClassConnect}, "inference gateway 'gw' unreachable (connect)"},
		{"auth with status", GatewayStatus{Name: "gw", ErrorClass: ClassAuth, HTTPStatus: 401}, "inference gateway 'gw' rejected key (401)"},
		{"auth without status", GatewayStatus{Name: "gw", ErrorClass: ClassAuth}, "inference gateway 'gw' rejected key"},
		{"budget", GatewayStatus{Name: "gw", ErrorClass: ClassBudget}, "inference gateway 'gw' budget/rate limited (429)"},
		{"5xx", GatewayStatus{Name: "gw", ErrorClass: Class5xx}, "inference gateway 'gw' returned 5xx"},
		{"other", GatewayStatus{Name: "gw", ErrorClass: ClassOther}, "inference gateway 'gw' failing"},
		{"blank name", GatewayStatus{Name: " ", ErrorClass: ClassOther}, "inference gateway 'unknown' failing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Reason(tc.st); got != tc.want {
				t.Errorf("Reason(%+v) = %q, want %q", tc.st, got, tc.want)
			}
		})
	}
}

func TestMostRecent(t *testing.T) {
	old := GatewayStatus{Name: "old", ErrorClass: Class5xx, LastErrorAt: "2026-01-01T00:00:00Z"}
	newer := GatewayStatus{Name: "newer", ErrorClass: ClassAuth, LastErrorAt: "2026-06-01T00:00:00Z"}
	blankName := GatewayStatus{Name: " ", ErrorClass: Class5xx, LastErrorAt: "2026-12-01T00:00:00Z"}
	blankClass := GatewayStatus{Name: "gw", ErrorClass: "", LastErrorAt: "2026-12-01T00:00:00Z"}

	got, ok := MostRecent([]GatewayStatus{old, newer, blankName, blankClass})
	if !ok || got.Name != "newer" {
		t.Errorf("MostRecent = (%+v, %v), want newer/true", got, ok)
	}

	if _, ok := MostRecent(nil); ok {
		t.Error("MostRecent(nil) ok = true, want false")
	}
	if _, ok := MostRecent([]GatewayStatus{blankName, blankClass}); ok {
		t.Error("MostRecent(all-invalid) ok = true, want false")
	}

	// Entries with unparseable timestamps are still eligible; first valid wins ties.
	noTS := GatewayStatus{Name: "no-ts", ErrorClass: Class5xx}
	got, ok = MostRecent([]GatewayStatus{noTS})
	if !ok || got.Name != "no-ts" {
		t.Errorf("MostRecent(no timestamp) = (%+v, %v), want no-ts/true", got, ok)
	}
}

func TestSafeDetailTruncation(t *testing.T) {
	long := strings.Repeat("x", detailLimit+50)
	if got := safeDetail(long); len(got) != detailLimit {
		t.Errorf("safeDetail(long) len = %d, want %d", len(got), detailLimit)
	}
	if got := safeDetail("  short  "); got != "short" {
		t.Errorf("safeDetail should trim, got %q", got)
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStore()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			s.RecordHTTPError("gw", 500, "boom", time.Now())
			s.Clear("gw")
		}
	}()
	for i := 0; i < 100; i++ {
		s.Snapshot()
	}
	<-done
}

func TestStoreRecordEndpointErrorIncludesHost(t *testing.T) {
	s := NewStore()
	err := &url.Error{Op: "Post", URL: "http://hive-vllm-svc.hive-inference.svc.cluster.local:8000/v1/chat/completions", Err: &net.DNSError{Name: "hive-vllm-svc.hive-inference.svc.cluster.local", Err: "no such host"}}
	s.RecordEndpointError("vllm", "http://hive-vllm-svc.hive-inference.svc.cluster.local:8000", err, time.Date(2026, 9, 4, 23, 0, 0, 0, time.UTC))
	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(snap))
	}
	if snap[0].Endpoint != "http://hive-vllm-svc.hive-inference.svc.cluster.local:8000" || snap[0].Host != "hive-vllm-svc.hive-inference.svc.cluster.local" {
		t.Fatalf("gateway status missing endpoint/host: %+v", snap[0])
	}
	if got := Reason(snap[0]); got != "vllm endpoint hive-vllm-svc.hive-inference.svc.cluster.local not resolvable on this cluster — set inference.vllm.endpoint or disable" {
		t.Fatalf("Reason = %q", got)
	}
}
