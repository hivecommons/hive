package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	cruxruntime "github.com/hivecommons/hive/crux/runtime"
)

func serveSharedPoolRequest(t *testing.T, pool *sharedAgentPool, s *HubServer, method, bearerHive string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, "/api/shared-agent-pool/leases", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+s.heartbeatKeyFor(bearerHive))
	rec := httptest.NewRecorder()
	pool.handle(rec, req, s)
	return rec
}

func TestSharedAgentPoolTenantAuthCapacityAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared-agent-pool.json")
	pool := &sharedAgentPool{path: path, max: 1, ttl: time.Minute, leases: make(map[string]sharedAgentLease)}
	s := &HubServer{logger: slog.Default()}
	s.setHubSecret("test-shared-agent-pool-master")

	school := map[string]string{"hive_id": "school", "agent_name": "scanner"}
	firstRec := serveSharedPoolRequest(t, pool, s, http.MethodPost, "school", school)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first acquire status = %d, body %q", firstRec.Code, firstRec.Body.String())
	}
	var first struct {
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal(firstRec.Body.Bytes(), &first); err != nil || first.LeaseID == "" {
		t.Fatalf("first lease response = %q, err %v", firstRec.Body.String(), err)
	}

	spoof := map[string]string{"hive_id": "reef", "agent_name": "scanner"}
	spoofRec := serveSharedPoolRequest(t, pool, s, http.MethodPost, "school", spoof)
	if spoofRec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-tenant acquire status = %d, want 401", spoofRec.Code)
	}

	reef := map[string]string{"hive_id": "reef", "agent_name": "reviewer"}
	busyRec := serveSharedPoolRequest(t, pool, s, http.MethodPost, "reef", reef)
	if busyRec.Code != http.StatusConflict {
		t.Fatalf("second tenant acquire status = %d, want 409", busyRec.Code)
	}

	restored := &sharedAgentPool{path: path, max: 1, ttl: time.Minute, leases: make(map[string]sharedAgentLease)}
	restored.load()
	if restored.loadError != nil || len(restored.leases) != 1 {
		t.Fatalf("restored pool: leases=%d loadError=%v", len(restored.leases), restored.loadError)
	}
	lease := restored.leases[sharedPoolOwnerKey("school", "scanner")]
	if lease.ID != first.LeaseID {
		t.Fatalf("restored lease ID = %q, want %q", lease.ID, first.LeaseID)
	}

	release := map[string]string{"hive_id": "school", "lease_id": first.LeaseID}
	releaseRec := serveSharedPoolRequest(t, restored, s, http.MethodDelete, "school", release)
	if releaseRec.Code != http.StatusNoContent {
		t.Fatalf("release status = %d, want 204 (%s)", releaseRec.Code, releaseRec.Body.String())
	}

	acquireRec := serveSharedPoolRequest(t, restored, s, http.MethodPost, "reef", reef)
	if acquireRec.Code != http.StatusOK {
		t.Fatalf("acquire after release status = %d, want 200 (%s)", acquireRec.Code, acquireRec.Body.String())
	}
}

func TestSharedAgentPoolHTTPSerializesTwoHives(t *testing.T) {
	t.Setenv("HIVE_SHARED_AGENT_POOL_MAX_SESSIONS", "1")
	srv := newHubServerForTest(t)
	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)

	school, err := cruxruntime.NewSharedSessionPoolClient(ts.URL, "school", srv.heartbeatKeyFor("school"))
	if err != nil {
		t.Fatal(err)
	}
	reef, err := cruxruntime.NewSharedSessionPoolClient(ts.URL, "reef", srv.heartbeatKeyFor("reef"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lease, err := school.Acquire(ctx, "scanner")
	if err != nil {
		t.Fatalf("school acquire: %v", err)
	}

	busyCtx, busyCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer busyCancel()
	if _, err := reef.Acquire(busyCtx, "reviewer"); err == nil {
		t.Fatal("reef acquired a second live CLI slot while school held the only slot")
	}
	if err := school.Release(ctx, lease); err != nil {
		t.Fatalf("school release: %v", err)
	}
	reefCtx, reefCancel := context.WithTimeout(context.Background(), time.Second)
	defer reefCancel()
	if _, err := reef.Acquire(reefCtx, "reviewer"); err != nil {
		t.Fatalf("reef acquire after school release: %v", err)
	}
}

func TestSharedAgentPoolFIFOWaitersAndStaleExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared-agent-pool.json")
	pool := &sharedAgentPool{
		path:         path,
		max:          1,
		ttl:          time.Minute,
		leases:       make(map[string]sharedAgentLease),
		waitingHives: make(map[string]time.Time),
		lastSeen:     make(map[string]time.Time),
	}
	s := &HubServer{logger: slog.Default()}
	s.setHubSecret("test-fifo-waiters-master")

	h0 := map[string]string{"hive_id": "h0", "agent_name": "a"}
	h1 := map[string]string{"hive_id": "h1", "agent_name": "b"}
	h2 := map[string]string{"hive_id": "h2", "agent_name": "c"}

	// h0 acquires the slot.
	r0 := serveSharedPoolRequest(t, pool, s, http.MethodPost, "h0", h0)
	if r0.Code != http.StatusOK {
		t.Fatalf("h0 acquire status = %d", r0.Code)
	}
	var lease0 struct {
		LeaseID string `json:"lease_id"`
	}
	_ = json.Unmarshal(r0.Body.Bytes(), &lease0)

	// h1 tries and gets 409, becomes first waiter.
	r1 := serveSharedPoolRequest(t, pool, s, http.MethodPost, "h1", h1)
	if r1.Code != http.StatusConflict {
		t.Fatalf("h1 status = %d, want 409", r1.Code)
	}
	// Back-date h1's FIFO position so h2's 409 below is strictly later even
	// when both requests land in the same clock tick.
	pool.mu.Lock()
	pool.waitingHives["h1"] = pool.waitingHives["h1"].Add(-time.Second)
	pool.mu.Unlock()

	// h2 tries and gets 409, becomes second waiter.
	r2 := serveSharedPoolRequest(t, pool, s, http.MethodPost, "h2", h2)
	if r2.Code != http.StatusConflict {
		t.Fatalf("h2 status = %d, want 409", r2.Code)
	}

	// h0 releases.
	rel := map[string]string{"hive_id": "h0", "lease_id": lease0.LeaseID}
	rRel := serveSharedPoolRequest(t, pool, s, http.MethodDelete, "h0", rel)
	if rRel.Code != http.StatusNoContent {
		t.Fatalf("h0 release status = %d", rRel.Code)
	}

	// h2 tries to acquire before h1, but h1 is earlier in queue -> 409.
	r2Blocked := serveSharedPoolRequest(t, pool, s, http.MethodPost, "h2", h2)
	if r2Blocked.Code != http.StatusConflict {
		t.Fatalf("h2 status = %d, want 409 (h1 was earlier waiter)", r2Blocked.Code)
	}

	// h1 acquires -> 200 OK.
	r1Acq := serveSharedPoolRequest(t, pool, s, http.MethodPost, "h1", h1)
	if r1Acq.Code != http.StatusOK {
		t.Fatalf("h1 acquire status = %d, want 200", r1Acq.Code)
	}
	var lease1 struct {
		LeaseID string `json:"lease_id"`
	}
	_ = json.Unmarshal(r1Acq.Body.Bytes(), &lease1)

	// h1 releases.
	rel1 := map[string]string{"hive_id": "h1", "lease_id": lease1.LeaseID}
	serveSharedPoolRequest(t, pool, s, http.MethodDelete, "h1", rel1)

	// Now make h2 stale (simulate h2 crashing and not retrying for > 15s).
	pool.mu.Lock()
	pool.lastSeen["h2"] = time.Now().Add(-20 * time.Second)
	pool.mu.Unlock()

	// Fresh hive h3 can acquire because h2 is stale and pruned!
	h3 := map[string]string{"hive_id": "h3", "agent_name": "d"}
	r3 := serveSharedPoolRequest(t, pool, s, http.MethodPost, "h3", h3)
	if r3.Code != http.StatusOK {
		t.Fatalf("h3 acquire status = %d, want 200 after stale waiter pruned", r3.Code)
	}
}

func TestSharedAgentPoolPerHiveCapDoesNotStarveOtherTenants(t *testing.T) {
	pool := &sharedAgentPool{
		path:         filepath.Join(t.TempDir(), "shared-agent-pool.json"),
		max:          3,
		maxPerHive:   1,
		ttl:          time.Minute,
		leases:       make(map[string]sharedAgentLease),
		waitingHives: make(map[string]time.Time),
		lastSeen:     make(map[string]time.Time),
	}
	s := &HubServer{logger: slog.Default()}
	s.setHubSecret("test-per-hive-cap-master")

	if r := serveSharedPoolRequest(t, pool, s, http.MethodPost, "h0", map[string]string{"hive_id": "h0", "agent_name": "a"}); r.Code != http.StatusOK {
		t.Fatalf("h0 first acquire status = %d, want 200", r.Code)
	}
	// Second agent on the same tenant exceeds its cap even though the fleet
	// still has two free slots.
	r := serveSharedPoolRequest(t, pool, s, http.MethodPost, "h0", map[string]string{"hive_id": "h0", "agent_name": "b"})
	if r.Code != http.StatusConflict {
		t.Fatalf("h0 second acquire status = %d, want 409", r.Code)
	}
	pool.mu.Lock()
	_, waiting := pool.waitingHives["h0"]
	pool.mu.Unlock()
	if waiting {
		t.Fatal("tenant at its per-hive cap must not take a FIFO waiter position")
	}
	// Another tenant is not blocked by h0's capped request.
	if r := serveSharedPoolRequest(t, pool, s, http.MethodPost, "h1", map[string]string{"hive_id": "h1", "agent_name": "c"}); r.Code != http.StatusOK {
		t.Fatalf("h1 acquire status = %d, want 200", r.Code)
	}
	// Re-acquiring an existing lease (same hive+agent) is a renewal, not a new slot.
	if r := serveSharedPoolRequest(t, pool, s, http.MethodPost, "h0", map[string]string{"hive_id": "h0", "agent_name": "a"}); r.Code != http.StatusOK {
		t.Fatalf("h0 renew-by-acquire status = %d, want 200", r.Code)
	}
}

func TestNewSharedAgentPoolReadsPerHiveCap(t *testing.T) {
	t.Setenv("HIVE_SHARED_AGENT_POOL_MAX_SESSIONS", "4")
	t.Setenv("HIVE_SHARED_AGENT_POOL_MAX_PER_HIVE", "2")
	if got := newSharedAgentPool().maxPerHive; got != 2 {
		t.Fatalf("maxPerHive = %d, want 2", got)
	}
	t.Setenv("HIVE_SHARED_AGENT_POOL_MAX_PER_HIVE", "-1")
	if got := newSharedAgentPool().maxPerHive; got != 0 {
		t.Fatalf("maxPerHive = %d, want 0 for negative input", got)
	}
}
