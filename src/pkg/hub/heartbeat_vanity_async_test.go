package hub

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// server.go / saas_provision.go — the vanity-URL repair must NEVER block the
// heartbeat response.
//
// Live failure this guards against (vllmd-14, and vllmd-13 / EPM before it):
// the repair's kubectl calls hang ~90s per beat against an unreachable
// (heartbeat-only) cluster, the spoke's heartbeat client times out at 10s
// (heartbeatTimeout), so the response carrying the claimed-project config —
// the only channel that reaches such a spoke — is written to a dead
// connection. The claim never lands and the assignStuckResetTimeout sweep
// resets the assignment at 15m, forever.
// ============================================================

// waitTimeout bounds how long these tests wait for what should be an
// immediate response; generous so slow CI never flakes it, tiny next to the
// minutes a blocked kubectl would take.
const waitTimeout = 5 * time.Second

// newBlockedVanityHub builds a hub whose vanity-servability seam blocks until
// the returned release func is called — standing in for kubectl hanging
// against an unreachable cluster. The cluster has a Domain so the repair has
// a host to derive (i.e. it genuinely reaches the blocking call).
func newBlockedVanityHub(t *testing.T) (s *HubServer, entered *atomic.Int32, release func()) {
	t.Helper()
	s = newHeartbeatHub()
	s.clusters["vllm-d"] = ClusterConfig{
		ID:          "vllm-d",
		Name:        "vllm-d",
		Domain:      "apps.fmaas-vllm-d.example.com",
		IngressType: "nginx",
	}
	blocked := make(chan struct{})
	var once atomic.Bool
	release = func() {
		if once.CompareAndSwap(false, true) {
			close(blocked)
		}
	}
	t.Cleanup(release)
	entered = &atomic.Int32{}
	s.vanityHostServable = func(_, _ string, _ *ClusterConfig) error {
		entered.Add(1)
		<-blocked
		// After release the repair must treat the attempt as "not servable"
		// and keep the placeholder (errNotServableForTest lives in
		// url_reachability_test.go).
		return errNotServableForTest
	}
	return s, entered, release
}

// assignedVllmdHive is a freshly assigned placeholder on the unreachable
// cluster: complete claim, no vanity URL — exactly the record whose repair
// used to stall the heartbeat.
func assignedVllmdHive(id string) *SaaSHive {
	return &SaaSHive{
		ID: id, Owner: "AKippins", Status: statusAssigned,
		Org: "z-innersource", Repos: []string{"AutoIPL"}, PrimaryRepo: "AutoIPL",
		ACMMLevel: 1, RequestedACMMLevel: 1, ClusterID: "vllm-d",
	}
}

// The heartbeat response — carrying the claimed-project config — must come
// back within the spoke's client timeout even while the vanity repair is
// blocked on an unreachable cluster. With the old synchronous call this test
// hangs on the seam and times out.
func TestHeartbeatDeliversClaimWhileVanityRepairBlocked(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, _, release := newBlockedVanityHub(t)
	defer release()

	const id = "hosted-available-vllmd-14"
	if err := saveSaaSHive(assignedVllmdHive(id)); err != nil {
		t.Fatal(err)
	}

	// The spoke still reports its placeholder identity, so the hub must push
	// the claim in the response.
	body := `{"hive_id":"` + id + `","org":"available-vllmd-14","acmm_level":2}`

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postHeartbeat(t, s, body) }()

	select {
	case rec := <-done:
		if rec.Code != 200 {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		var resp HeartbeatResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.ProjectConfig == nil {
			t.Fatal("response carried no project config for a fresh assignment")
		}
		if resp.ProjectConfig.Org != "z-innersource" || resp.ProjectConfig.PrimaryRepo != "AutoIPL" {
			t.Errorf("project config = %+v, want org z-innersource primary AutoIPL", resp.ProjectConfig)
		}
	case <-time.After(waitTimeout):
		t.Fatal("heartbeat response blocked behind the vanity repair — the spoke's 10s client timeout would drop it and the claim would never be delivered")
	}
}

// While one background repair attempt is stuck against the cluster, further
// heartbeats must not stack additional attempts; once it finishes, a later
// beat may try again.
func TestVanityRepairKickDedupesInFlightAttempts(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(vanityRepairFailureBackoffEnv, "1ms")
	s, entered, release := newBlockedVanityHub(t)

	const id = "hosted-available-vllmd-15"
	if err := saveSaaSHive(assignedVllmdHive(id)); err != nil {
		t.Fatal(err)
	}

	s.kickVanityURLRepairAsync(id)
	// Wait for the first attempt to reach the blocking seam.
	deadline := time.Now().Add(waitTimeout)
	for entered.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if entered.Load() != 1 {
		t.Fatalf("first kick: seam entered %d times, want 1", entered.Load())
	}

	// Beats keep arriving while the attempt is stuck — none may stack.
	s.kickVanityURLRepairAsync(id)
	s.kickVanityURLRepairAsync(id)
	time.Sleep(50 * time.Millisecond)
	if got := entered.Load(); got != 1 {
		t.Fatalf("in-flight dedupe failed: seam entered %d times, want 1", got)
	}

	// Release the stuck attempt; once it drains, a new kick may run again.
	release()
	deadline = time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if _, inFlight := s.vanityRepairInFlight.Load(id); !inFlight {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.kickVanityURLRepairAsync(id)
	deadline = time.Now().Add(waitTimeout)
	for entered.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := entered.Load(); got != 2 {
		t.Fatalf("post-release kick: seam entered %d times, want 2", got)
	}
}

// The kick must spawn nothing for records the repair would no-op on: the
// common case stays goroutine-free, and unclaimed placeholders stay untouched.
func TestVanityRepairKickInlineGuards(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, entered, _ := newBlockedVanityHub(t)

	cases := []*SaaSHive{
		nil, // no record at all
		{ID: "hosted-unclaimed", Status: statusAvailable, Org: "available-x", ClusterID: "vllm-d"},
		// NOTE: "already has a vanity URL" is deliberately NOT in this list any
		// more. It used to be, and that short-circuit is exactly why a stale
		// vanity host could never be corrected — the only code that reads the
		// live Route sat behind it. Such a hive must now reach the repair so it
		// can reconcile against reality; see
		// TestVanityRepairKickRunsForHiveWithExistingVanityURL below.
		func() *SaaSHive { // incomplete claim — nothing to derive a host from
			h := assignedVllmdHive("hosted-no-primary")
			h.PrimaryRepo = ""
			return h
		}(),
	}
	for _, h := range cases {
		id := "hosted-missing"
		if h != nil {
			id = h.ID
			if err := saveSaaSHive(h); err != nil {
				t.Fatal(err)
			}
		}
		s.kickVanityURLRepairAsync(id)
		if _, inFlight := s.vanityRepairInFlight.Load(id); inFlight {
			t.Errorf("%s: kick spawned a background repair for a no-op record", id)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := entered.Load(); got != 0 {
		t.Errorf("no-op kicks reached the servability seam %d times, want 0", got)
	}
}

// A claimed hive that ALREADY has a vanity URL must still be kicked, because
// the stored host is not self-validating: it can drift from the live Route
// (the suffix is random, so any re-mint changes it) and the reconcile inside
// the repair is the only thing that compares the two. This is the inverse of
// the guard list above, and it fails against the pre-fix short-circuit.
func TestVanityRepairKickRunsForHiveWithExistingVanityURL(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, _, _ := newBlockedVanityHub(t)

	h := assignedVllmdHive("hosted-has-vanity")
	h.VanityURL = "https://already.example.com"
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	s.kickVanityURLRepairAsync(h.ID)

	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if _, inFlight := s.vanityRepairInFlight.Load(h.ID); inFlight {
			return // the reconcile pass was spawned, which is the contract here
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("kick spawned no repair for a hive with a vanity URL, so a stale host could never be reconciled")
}

// A successful repair must not write through a record loaded before its slow
// kubectl window: fields the heartbeat path latched meanwhile (ClaimDelivered
// here) survive the vanity save.
func TestVanityRepairReloadsHiveBeforeSave(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.clusters["vllm-d"] = ClusterConfig{
		ID: "vllm-d", Name: "vllm-d",
		Domain: "apps.fmaas-vllm-d.example.com", IngressType: "nginx",
	}

	const id = "hosted-available-vllmd-16"
	if err := saveSaaSHive(assignedVllmdHive(id)); err != nil {
		t.Fatal(err)
	}

	// The seam stands in for the minutes-long kubectl window: while the repair
	// is "making the host servable", the heartbeat path latches the claim.
	s.vanityHostServable = func(hiveID, _ string, _ *ClusterConfig) error {
		h := loadSaaSHive(hiveID)
		h.ClaimDelivered = true
		if err := saveSaaSHive(h); err != nil {
			t.Errorf("mid-repair save: %v", err)
		}
		return nil
	}

	if !s.repairVanityURLForHive(id) {
		t.Fatal("repair reported no change for a claimed hive without a vanity URL")
	}
	got := loadSaaSHive(id)
	if got.VanityURL == "" {
		t.Error("repair did not mint a vanity URL")
	}
	if !got.ClaimDelivered {
		t.Error("vanity save reverted ClaimDelivered latched during the repair's kubectl window (stale load-modify-write)")
	}
	if !strings.HasPrefix(got.VanityURL, "https://hosted-z-innersource-autoipl-") {
		t.Errorf("vanity URL %q not derived from the claimed project", got.VanityURL)
	}
}
