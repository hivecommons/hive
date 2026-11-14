package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

// seedRegistryEntry puts a bare registry row for id in place, standing in for
// the row a first accepted heartbeat would have created.
func seedRegistryEntry(s *HubServer, id string) {
	s.mu.Lock()
	s.registry.Hives = append(s.registry.Hives, RegistryEntry{ID: id, Online: true, HiveType: SelfServiceHiveType})
	s.mu.Unlock()
}

func TestSelfServiceExpirySweepRemovesPastExpiryAndKeepsWithin(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(selfServiceExpiryEnv, "")
	s, logBuf := newSelfServiceHeartbeatHub(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s.selfService.now = func() time.Time { return now }

	const stale, fresh = "byo-alice-stale", "byo-alice-fresh"
	staleKey, err := s.selfService.register(SelfServiceHive{ID: stale, Owner: "alice"})
	if err != nil {
		t.Fatalf("register stale: %v", err)
	}
	freshKey, err := s.selfService.register(SelfServiceHive{ID: fresh, Owner: "alice"})
	if err != nil {
		t.Fatalf("register fresh: %v", err)
	}
	staleBeat := now.Add(-selfServiceExpiryDefault - time.Minute)
	if err := s.selfService.noteAcceptedHeartbeat(stale, staleBeat); err != nil {
		t.Fatalf("stamp stale: %v", err)
	}
	if err := s.selfService.noteAcceptedHeartbeat(fresh, now.Add(-selfServiceExpiryDefault+time.Minute)); err != nil {
		t.Fatalf("stamp fresh: %v", err)
	}
	seedRegistryEntry(s, stale)
	seedRegistryEntry(s, fresh)

	removed := s.sweepExpiredSelfServiceHives()
	if len(removed) != 1 || removed[0] != stale {
		t.Fatalf("removed=%v, want [%s]", removed, stale)
	}
	if _, ok := s.selfService.get(stale); ok {
		t.Fatal("expired hive still in the self-service store")
	}
	if s.selfServiceRegistryHas(stale) {
		t.Fatal("expired hive still in the registry")
	}
	if _, ok := s.selfService.get(fresh); !ok {
		t.Fatal("hive within the window was removed from the store")
	}
	if !s.selfServiceRegistryHas(fresh) {
		t.Fatal("hive within the window was removed from the registry")
	}

	// Snapshot the sweep's log output before the heartbeats below add their own
	// lines.
	logs := logBuf.String()

	// The revoked key is dead on the real heartbeat handler; the kept one is
	// not.
	if rec := postHeartbeatWithBearer(t, s, stale, staleKey); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired key still authenticates: status=%d", rec.Code)
	}
	if rec := postHeartbeatWithBearer(t, s, fresh, freshKey); rec.Code != http.StatusOK {
		t.Fatalf("kept key stopped authenticating: status=%d body=%s", rec.Code, rec.Body.String())
	}

	wantLine := "audit: selfservice_hive_expired"
	if !strings.Contains(logs, wantLine) {
		t.Fatalf("no %q audit line in logs:\n%s", wantLine, logs)
	}
	for _, kv := range []string{"id=" + stale, "owner=alice", "last_heartbeat=" + staleBeat.Format(time.RFC3339), "by=" + selfServiceExpiredBy} {
		if !strings.Contains(logs, kv) {
			t.Errorf("audit line missing %q:\n%s", kv, logs)
		}
	}
	if strings.Contains(logs, "id="+fresh) {
		t.Errorf("kept hive appeared in an audit line:\n%s", logs)
	}
	for _, secret := range []string{staleKey, freshKey, s.hubSecret} {
		if strings.Contains(logBuf.String(), secret) {
			t.Fatal("key material appeared in hub logs")
		}
	}

	// Idempotent: a second sweep finds nothing.
	if again := s.sweepExpiredSelfServiceHives(); len(again) != 0 {
		t.Fatalf("second sweep removed %v", again)
	}
}

func TestSelfServiceExpirySweepNeverHeartbeatedCountsFromCreatedAt(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(selfServiceExpiryEnv, "")
	s, logBuf := newSelfServiceHeartbeatHub(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s.selfService.now = func() time.Time { return now }

	const id = "byo-bob-silent"
	if _, err := s.selfService.register(SelfServiceHive{ID: id, Owner: "bob"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if h, _ := s.selfService.get(id); !h.LastAcceptedHeartbeatAt.IsZero() || h.CreatedAt.IsZero() {
		t.Fatalf("precondition: CreatedAt set, no heartbeat; got %+v", h)
	}

	// Inside the window: kept.
	now = now.Add(selfServiceExpiryDefault - time.Minute)
	if removed := s.sweepExpiredSelfServiceHives(); len(removed) != 0 {
		t.Fatalf("sweep inside the window removed %v", removed)
	}
	// Past it: gone, audited as never heartbeated.
	now = now.Add(2 * time.Minute)
	if removed := s.sweepExpiredSelfServiceHives(); len(removed) != 1 || removed[0] != id {
		t.Fatalf("sweep past the window removed %v, want [%s]", removed, id)
	}
	if _, ok := s.selfService.get(id); ok {
		t.Fatal("never-heartbeated hive survived the window")
	}
	if !strings.Contains(logBuf.String(), "last_heartbeat="+selfServiceNeverHeartbeated) {
		t.Fatalf("audit line did not record last_heartbeat=%s:\n%s", selfServiceNeverHeartbeated, logBuf.String())
	}
}

func TestSelfServiceExpiryWindowHonoursEnv(t *testing.T) {
	t.Setenv(selfServiceExpiryEnv, "2h")
	t.Setenv(selfServiceSweepIntervalEnv, "30s")
	if got := selfServiceExpiry(); got != 2*time.Hour {
		t.Fatalf("expiry=%s, want 2h", got)
	}
	if got := selfServiceSweepInterval(); got != 30*time.Second {
		t.Fatalf("sweep interval=%s, want 30s", got)
	}
	t.Setenv(selfServiceSweepIntervalEnv, "not-a-duration")
	if got := selfServiceSweepInterval(); got != selfServiceSweepIntervalDefault {
		t.Fatalf("invalid interval fell back to %s, want %s", got, selfServiceSweepIntervalDefault)
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	h := SelfServiceHive{ID: "byo-x-y", CreatedAt: now.Add(-3 * time.Hour)}
	if !selfServiceExpired(h, now, selfServiceExpiry()) {
		t.Fatal("3h-old registration not expired by a 2h window")
	}
	if selfServiceExpired(SelfServiceHive{ID: "byo-x-z"}, now, selfServiceExpiry()) {
		t.Fatal("a record with no timestamps must never expire")
	}
}

// TestSelfServiceExpirySweepLoopRunsOnTicker drives the real poller with a
// short interval and waits for the removal with testutil.Eventually; the clock
// the window is measured on is still the injected one.
func TestSelfServiceExpirySweepLoopRunsOnTicker(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(selfServiceExpiryEnv, "")
	t.Setenv(selfServiceSweepIntervalEnv, "5ms")
	s, _ := newSelfServiceHeartbeatHub(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s.selfService.now = func() time.Time { return now }

	const id = "byo-carol-lab"
	if _, err := s.selfService.register(SelfServiceHive{ID: id, Owner: "carol"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Registration happened at `now`; the sweep measures against a clock
	// already past the window, so the first tick removes it.
	s.selfService.now = func() time.Time { return now.Add(selfServiceExpiryDefault + time.Minute) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.startSelfServiceExpirySweep(ctx)
	}()
	testutil.Eventually(t, 5*time.Second, func() bool {
		_, ok := s.selfService.get(id)
		return !ok
	}, "sweep loop did not remove the expired hive")
	cancel()
	<-done
}

// postTypedHeartbeat sends a heartbeat whose body claims hive_type for hiveID
// and returns the HiveType the registry stored.
func postTypedHeartbeat(t *testing.T, s *HubServer, hiveID, bearer, claimedType string) (string, int) {
	t.Helper()
	body := `{"hive_id":"` + hiveID + `","org":"alice"`
	if claimedType != "" {
		body += `,"hive_type":"` + claimedType + `"`
	}
	body += `}`
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	s.handleHeartbeat(rec, req)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID {
			return h.HiveType, rec.Code
		}
	}
	return "", rec.Code
}

func TestSelfServiceHiveTypeIsHubAssigned(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, _ := newSelfServiceHeartbeatHub(t)

	const byo = "byo-alice-lab"
	raw, err := s.selfService.register(SelfServiceHive{ID: byo, Owner: "alice"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// A byo- payload claiming to be hosted is still listed as self-hosted.
	if got, code := postTypedHeartbeat(t, s, byo, raw, "hosted"); code != http.StatusOK || got != SelfServiceHiveType {
		t.Fatalf("byo- claiming hosted: status=%d hiveType=%q, want %q", code, got, SelfServiceHiveType)
	}
	if got, code := postTypedHeartbeat(t, s, byo, raw, ""); code != http.StatusOK || got != SelfServiceHiveType {
		t.Fatalf("byo- with no claim: status=%d hiveType=%q, want %q", code, got, SelfServiceHiveType)
	}

	// Hosted and operator-listed lanes are unchanged.
	const hosted, operator = "hosted-alice-lab", "alice-lab"
	if got, code := postTypedHeartbeat(t, s, hosted, derivePerHiveKey(s.hubSecret, infoHeartbeatKey, hosted), ""); code != http.StatusOK || got != "hosted" {
		t.Fatalf("hosted- default: status=%d hiveType=%q, want hosted", code, got)
	}
	if got, code := postTypedHeartbeat(t, s, operator, derivePerHiveKey(s.hubSecret, infoHeartbeatKey, operator), ""); code != http.StatusOK || got != "local" {
		t.Fatalf("operator default: status=%d hiveType=%q, want local", code, got)
	}
	if got, code := postTypedHeartbeat(t, s, operator, derivePerHiveKey(s.hubSecret, infoHeartbeatKey, operator), "lite"); code != http.StatusOK || got != "lite" {
		t.Fatalf("operator claimed type: status=%d hiveType=%q, want lite", code, got)
	}
}

func TestHubAssignedHiveType(t *testing.T) {
	cases := []struct {
		id, claimed, want string
	}{
		{"byo-alice-lab", "", SelfServiceHiveType},
		{"byo-alice-lab", "hosted", SelfServiceHiveType},
		{"byo-alice-lab", "local", SelfServiceHiveType},
		{"hosted-x", "", "hosted"},
		{"saas-x", "", "hosted"},
		{"alice-lab", "", "local"},
		{"alice-lab", "lite", "lite"},
	}
	for _, c := range cases {
		if got := hubAssignedHiveType(c.id, c.claimed); got != c.want {
			t.Errorf("hubAssignedHiveType(%q, %q)=%q, want %q", c.id, c.claimed, got, c.want)
		}
	}
}
