package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// saas.go / saas_provision.go — assign/approve must respond IMMEDIATELY.
//
// Live failure this guards against: the assign dialog sat on a pending fetch
// for ~a minute because handleAssignHive ran makeVanityHostServable (unbounded
// kubectl against the hive's cluster) and stampHostedNamespaceIdentity (2×15s
// bounded kubectl) BETWEEN persisting the assignment and writing the HTTP
// response. On the heartbeat-only vllm-d pool every kubectl call eats a ~45s
// TCP dial timeout, so the dashboard dialog — which awaits that fetch — hung
// until the cluster work gave up. Same disease #2730 cured on the heartbeat
// path; the cure is the same: persist, respond, and kick the cluster work into
// a guarded background goroutine (kickClaimClusterWorkAsync).
// ============================================================

// newBlockedClaimHub builds a hub whose vanity-servability seam blocks until
// release is called and whose kubectl is a fake that blocks until the returned
// unblockKubectl func is called — standing in for kubectl hanging against an
// unreachable cluster on BOTH claim-time cluster calls (namespace stamp and
// vanity mint). seamErr is what the seam returns once released.
func newBlockedClaimHub(t *testing.T, seamErr error) (s *HubServer, entered *atomic.Int32, release, unblockKubectl func()) {
	t.Helper()

	// Fake kubectl: block until the sentinel file exists, then succeed. The
	// stamp's own 15s context bounds it even if the sentinel never appears.
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "kubectl.release")
	script := `#!/bin/sh
while [ ! -f "` + sentinel + `" ]; do sleep 0.05; done
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	unblockKubectl = func() { _ = os.WriteFile(sentinel, nil, 0o644) }

	s = &HubServer{
		logger:                  slog.Default(),
		hubSecret:               testHubSecret,
		saveCh:                  make(chan struct{}, 1),
		clusters:                map[string]ClusterConfig{"hive-oke": *dynamicCluster()},
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
	}
	blocked := make(chan struct{})
	var once atomic.Bool
	release = func() {
		if once.CompareAndSwap(false, true) {
			close(blocked)
		}
	}
	entered = &atomic.Int32{}
	s.vanityHostServable = func(_, _ string, _ *ClusterConfig) error {
		entered.Add(1)
		<-blocked
		return seamErr
	}
	return s, entered, release, unblockKubectl
}

// The assign response must come back immediately — with the assignment fully
// persisted — while BOTH claim-time cluster calls (namespace stamp, vanity
// mint) are still blocked. With the old synchronous code this test hangs on
// the seam and times out. Once the cluster work is released, the background
// goroutine adopts the vanity host exactly as the inline version did.
func TestAssignRespondsWhileClusterWorkBlocked(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, _, release, unblockKubectl := newBlockedClaimHub(t, nil)
	// LIFO: these run BEFORE cleanup's provisionWG.Wait, so the drained
	// goroutine can finish and the Wait cannot deadlock on the blocked seam.
	defer release()
	defer unblockKubectl()
	mkUser(t, hubAdminUsername)

	const id = "hosted-avail-fast01"
	if err := saveSaaSHive(&SaaSHive{ID: id, Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"}); err != nil {
		t.Fatal(err)
	}

	body := `{"owner":"carol","org":"falcon-core","repos":"buddies","primary_repo":"buddies","acmm_level":2,"project_name":"TradingAsBuddies"}`
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/"+id+"/assign", body, hubAdminUsername), "id", id)
		s.handleAssignHive(rec, req)
		done <- rec
	}()

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("assign status = %d body=%s", rec.Code, rec.Body.String())
		}
	case <-time.After(waitTimeout):
		t.Fatal("assign response blocked behind cluster work — the dialog would hang on its fetch (the ~1min dialog-hostage bug)")
	}

	// The response was written with the cluster work still blocked, so the
	// assignment itself must already be fully persisted — that is what the
	// heartbeat channel delivers to the spoke, response semantics need nothing
	// from the cluster calls.
	h := loadSaaSHive(id)
	if h == nil {
		t.Fatal("hive missing after assign")
	}
	if h.Status != statusAssigned || h.Owner != "carol" || h.Org != "falcon-core" || h.PrimaryRepo != "buddies" {
		t.Errorf("assignment not persisted at response time: %+v", h)
	}
	if h.VanityURL != "" {
		t.Errorf("VanityURL = %q before the mint completed — must only be adopted once servable", h.VanityURL)
	}

	// Release the cluster work; the background goroutine must then adopt the
	// name-bearing vanity host exactly as the old inline code did.
	unblockKubectl()
	release()
	provisionWG.Wait()
	h = loadSaaSHive(id)
	if h == nil || h.VanityURL == "" {
		t.Fatalf("background mint did not adopt a vanity URL after release: %+v", h)
	}
}

// The approve response must likewise come back immediately while the claim's
// cluster work is blocked — approving assigns a placeholder through the same
// dialog-awaited fetch.
func TestApproveRespondsWhileClusterWorkBlocked(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, _, release, unblockKubectl := newBlockedClaimHub(t, errNotServableForTest)
	defer release()
	defer unblockKubectl()
	mkUser(t, hubAdminUsername)

	if err := saveProvisionRequest(&ProvisionRequest{
		Username: "bob", Org: "falcon-core", Repos: "buddies", PrimaryRepo: "buddies", ACMMLevel: 2,
		AuthMethod: "public", RequestedAt: time.Now().UTC().Format(time.RFC3339), Status: provisionStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	const id = "hosted-avail-fast02"
	if err := saveSaaSHive(&SaaSHive{ID: id, Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := setPathValue(reqWithUser(http.MethodPut, "/api/saas/approve-provision/bob", "", hubAdminUsername), "username", "bob")
		s.handleApproveProvision(rec, req)
		done <- rec
	}()

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("approve status = %d body=%s", rec.Code, rec.Body.String())
		}
	case <-time.After(waitTimeout):
		t.Fatal("approve response blocked behind cluster work — the dialog would hang on its fetch")
	}

	// Approval semantics are complete at response time, cluster work or not.
	h := loadSaaSHive(id)
	if h == nil || h.Status != statusAssigned || h.Owner != "bob" {
		t.Fatalf("approval not persisted at response time: %+v", h)
	}
	pr := loadProvisionRequest("bob")
	if pr == nil || pr.Status != provisionStatusApproved || pr.AssignedHive != id {
		t.Fatalf("provision request not marked approved at response time: %+v", pr)
	}
}

// While one claim work-set is stuck against the cluster, further kicks for the
// same hive must not stack goroutines; once it drains, a later kick may run.
func TestKickClaimClusterWorkDedupesPerHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, entered, release, unblockKubectl := newBlockedClaimHub(t, errNotServableForTest)
	defer release()
	unblockKubectl() // stamp is instant here; only the vanity seam blocks

	const id = "hosted-avail-dedupe01"
	if err := saveSaaSHive(&SaaSHive{
		ID: id, Owner: "carol", Status: statusAssigned, ClusterID: "hive-oke",
		Org: "falcon-core", Repos: []string{"buddies"}, PrimaryRepo: "buddies", ACMMLevel: 2,
	}); err != nil {
		t.Fatal(err)
	}

	s.kickClaimClusterWorkAsync(id)
	deadline := time.Now().Add(waitTimeout)
	for entered.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if entered.Load() != 1 {
		t.Fatalf("first kick: seam entered %d times, want 1", entered.Load())
	}

	// Retries while the attempt is stuck — none may stack.
	s.kickClaimClusterWorkAsync(id)
	s.kickClaimClusterWorkAsync(id)
	time.Sleep(50 * time.Millisecond)
	if got := entered.Load(); got != 1 {
		t.Fatalf("in-flight dedupe failed: seam entered %d times, want 1", got)
	}

	// Release; the failed (not-servable) attempt leaves VanityURL empty, so a
	// fresh kick after the drain must reach the seam again.
	release()
	provisionWG.Wait()
	s.kickClaimClusterWorkAsync(id)
	provisionWG.Wait()
	if got := entered.Load(); got != 2 {
		t.Fatalf("post-drain kick: seam entered %d times, want 2", got)
	}
}
