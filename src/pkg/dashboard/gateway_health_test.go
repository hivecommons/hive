package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/inferencehealth"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return at
}

// gatewayStatusAfter is the "newest failure wins" tie-breaker for
// GatewayHealthState. Cover both the parsed-time path and the string-compare
// fallback used when either timestamp is not RFC3339.
func TestGatewayStatusAfter(t *testing.T) {
	older := inferencehealth.GatewayStatus{Name: "g", ErrorClass: "5xx", LastErrorAt: "2026-01-01T00:00:00Z"}
	newer := inferencehealth.GatewayStatus{Name: "g", ErrorClass: "5xx", LastErrorAt: "2026-01-02T00:00:00Z"}

	if !gatewayStatusAfter(newer, older) {
		t.Error("newer RFC3339 timestamp should be after older")
	}
	if gatewayStatusAfter(older, newer) {
		t.Error("older RFC3339 timestamp should not be after newer")
	}
	if gatewayStatusAfter(older, older) {
		t.Error("equal timestamps: neither is after the other")
	}

	// Invalid timestamp on either side falls back to lexicographic compare.
	invalid := inferencehealth.GatewayStatus{Name: "g", ErrorClass: "5xx", LastErrorAt: "not-a-time"}
	if !gatewayStatusAfter(invalid, older) {
		t.Error("fallback string compare: 'not-a-time' > '2026-01-01T00:00:00Z'")
	}
	if gatewayStatusAfter(older, invalid) {
		t.Error("fallback string compare: '2026-01-01T00:00:00Z' < 'not-a-time'")
	}
}

func TestSetGatewayHealthProvider_RegisterAndClear(t *testing.T) {
	t.Cleanup(func() { SetGatewayHealthProvider(nil) })

	SetGatewayHealthProvider(func() []inferencehealth.GatewayStatus {
		return []inferencehealth.GatewayStatus{{Name: "openrouter", ErrorClass: inferencehealth.ClassAuth, LastErrorAt: "2026-01-01T00:00:00Z"}}
	})
	if fn := getGatewayHealthFn(); fn == nil {
		t.Fatal("provider not registered")
	} else if got := fn(); len(got) != 1 || got[0].Name != "openrouter" {
		t.Fatalf("provider snapshot = %+v, want one openrouter entry", got)
	}

	SetGatewayHealthProvider(nil)
	if getGatewayHealthFn() != nil {
		t.Error("provider should be cleared after SetGatewayHealthProvider(nil)")
	}
}

func TestGatewayHealthState_MergesStoreAndProvider(t *testing.T) {
	t.Cleanup(func() { SetGatewayHealthProvider(nil) })

	s := &Server{}
	store := s.gatewayHealthStore()
	if store == nil {
		t.Fatal("gatewayHealthStore returned nil for non-nil server")
	}
	// Store knows an older fault for "shared" and a fault for "storeonly".
	store.RecordHTTPError("shared", 502, "old store fault", mustTime(t, "2026-01-01T00:00:00Z"))
	store.RecordHTTPError("storeonly", 503, "store fault", mustTime(t, "2026-01-03T00:00:00Z"))

	SetGatewayHealthProvider(func() []inferencehealth.GatewayStatus {
		return []inferencehealth.GatewayStatus{
			// Newer fault for the same gateway: must win over the store's.
			{Name: "Shared", ErrorClass: inferencehealth.ClassConnect, Detail: "new proxy fault", LastErrorAt: "2026-01-02T00:00:00Z"},
			{Name: "proxyonly", ErrorClass: inferencehealth.ClassDNS, LastErrorAt: "2026-01-04T00:00:00Z"},
			// Blank name and blank error class are both dropped.
			{Name: "   ", ErrorClass: inferencehealth.ClassDNS, LastErrorAt: "2026-01-04T00:00:00Z"},
			{Name: "noclass", ErrorClass: "  ", LastErrorAt: "2026-01-04T00:00:00Z"},
		}
	})

	got := s.GatewayHealthState()
	if len(got) != 3 {
		t.Fatalf("GatewayHealthState returned %d entries (%+v), want 3", len(got), got)
	}
	// inferencehealth.Sort orders by lowercase name.
	if got[0].Name != "proxyonly" || got[2].Name != "storeonly" {
		t.Errorf("unexpected sort order: %+v", got)
	}
	shared := got[1]
	if shared.ErrorClass != inferencehealth.ClassConnect || shared.Detail != "new proxy fault" {
		t.Errorf("newest fault should win for shared gateway, got %+v", shared)
	}
}

func TestGatewayHealthState_EmptyWhenNothingKnown(t *testing.T) {
	t.Cleanup(func() { SetGatewayHealthProvider(nil) })
	SetGatewayHealthProvider(nil)
	s := &Server{}
	if got := s.GatewayHealthState(); len(got) != 0 {
		t.Errorf("expected no faults, got %+v", got)
	}
}

func TestGatewayHealthStore_NilServer(t *testing.T) {
	var s *Server
	if s.gatewayHealthStore() != nil {
		t.Error("nil server must yield a nil store")
	}
}
