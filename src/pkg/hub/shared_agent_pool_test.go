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
