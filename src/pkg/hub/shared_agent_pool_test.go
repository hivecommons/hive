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

	cruxagent "github.com/hivecommons/hive/crux/agent"
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

	school, err := cruxagent.NewSharedSessionPoolClient(ts.URL, "school", srv.heartbeatKeyFor("school"))
	if err != nil {
		t.Fatal(err)
	}
	reef, err := cruxagent.NewSharedSessionPoolClient(ts.URL, "reef", srv.heartbeatKeyFor("reef"))
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
	// Backdate h1's FIFO position so h2's entry below is unambiguously
	// later, without sleeping (sleep ratchet).
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

// TestSharedAgentPoolPerHiveCeiling pins the per-tenant cap: one spoke cannot
// take every slot by acquiring under distinct agent names, and a spoke refused
// at its ceiling takes no FIFO position (so it cannot starve other tenants).
func TestSharedAgentPoolPerHiveCeiling(t *testing.T) {
	pool := &sharedAgentPool{
		path:         filepath.Join(t.TempDir(), "shared-agent-pool.json"),
		max:          4,
		ttl:          time.Minute,
		leases:       make(map[string]sharedAgentLease),
		waitingHives: make(map[string]time.Time),
		lastSeen:     make(map[string]time.Time),
	}
	if got := pool.perHiveCap(); got != 2 {
		t.Fatalf("default perHiveCap = %d, want 2 (half of max 4)", got)
	}
	s := &HubServer{logger: slog.Default()}
	s.setHubSecret("test-per-hive-ceiling-master")

	for _, name := range []string{"a", "b"} {
		rec := serveSharedPoolRequest(t, pool, s, http.MethodPost, "greedy", map[string]string{"hive_id": "greedy", "agent_name": name})
		if rec.Code != http.StatusOK {
			t.Fatalf("greedy/%s acquire status = %d, want 200 (%s)", name, rec.Code, rec.Body.String())
		}
	}
	capped := serveSharedPoolRequest(t, pool, s, http.MethodPost, "greedy", map[string]string{"hive_id": "greedy", "agent_name": "c"})
	if capped.Code != http.StatusConflict {
		t.Fatalf("greedy third acquire status = %d, want 409 at per-hive ceiling", capped.Code)
	}
	if _, waiting := pool.waitingHives["greedy"]; waiting {
		t.Fatal("hive refused at its per-hive ceiling must not hold a FIFO waiter position")
	}
	// Re-acquiring an agent it already holds is still a renewal, not a new slot.
	again := serveSharedPoolRequest(t, pool, s, http.MethodPost, "greedy", map[string]string{"hive_id": "greedy", "agent_name": "a"})
	if again.Code != http.StatusOK {
		t.Fatalf("re-acquire of held lease status = %d, want 200", again.Code)
	}
	// The remaining fleet capacity stays available to another tenant.
	other := serveSharedPoolRequest(t, pool, s, http.MethodPost, "other", map[string]string{"hive_id": "other", "agent_name": "x"})
	if other.Code != http.StatusOK {
		t.Fatalf("other tenant acquire status = %d, want 200 (%s)", other.Code, other.Body.String())
	}

	explicit := &sharedAgentPool{max: 4, maxPerHive: 9}
	if got := explicit.perHiveCap(); got != 4 {
		t.Fatalf("perHiveCap with maxPerHive above max = %d, want clamp to 4", got)
	}
	single := &sharedAgentPool{max: 1}
	if got := single.perHiveCap(); got != 1 {
		t.Fatalf("perHiveCap with max 1 = %d, want 1", got)
	}
}
