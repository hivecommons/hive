package hub

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// perHiveEnvTestMaster is a fixed master secret for derivation in these tests.
// Never a real value — the derived keys are compared to each other, never to a
// production constant.
const perHiveEnvTestMaster = "perhive-env-reconcile-test-master"

// withTestMaster points provisionMasterSecret() at a known master for the
// duration of a test. provisionMasterSecret prefers $HIVE_HUB_SECRET, so
// setting it is sufficient and t.Setenv restores it automatically.
func withTestMaster(t *testing.T, master string) {
	t.Helper()
	t.Setenv("HIVE_HUB_SECRET", master)
	// Guard against the /data/saas/hub-secret.key fallback being picked up when
	// we deliberately test the EMPTY-master case: an unset env var would fall
	// through to that file if it happened to exist on the test machine.
	if master == "" {
		if _, err := os.Stat("/data/saas/hub-secret.key"); err == nil {
			t.Skip("host has /data/saas/hub-secret.key; cannot exercise the empty-master path here")
		}
	}
}

// envList is a convenience for building a live Deployment env list.
func envList(pairs ...string) []deploymentEnvVar {
	out := make([]deploymentEnvVar, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, deploymentEnvVar{Name: pairs[i], Value: pairs[i+1]})
	}
	return out
}

// TestDesiredPerHiveEnvMatchesProvisioningTemplate is the anti-divergence test.
// The reconcile MUST derive byte-identical values to the provisioning template
// block in saas_provision.go. If they ever disagreed, provisioning and this
// sweep would fight: every cycle would see "drift", patch, and roll the pod
// forever. So pin each of the five to the exact expression the template uses.
func TestDesiredPerHiveEnvMatchesProvisioningTemplate(t *testing.T) {
	withTestMaster(t, perHiveEnvTestMaster)
	const hiveID = "hive-alpha"

	want := desiredPerHiveEnv(hiveID)
	if want == nil {
		t.Fatal("desiredPerHiveEnv returned nil for a valid master + hive ID")
	}

	// These right-hand sides are copied verbatim from the template's data map
	// (saas_provision.go: HeartbeatKey / SessionKey / SSOPublicKey /
	// SessionPublicKey / TerminalKey).
	expect := map[string]string{
		EnvHeartbeatKey:     provisionHeartbeatKey(hiveID),
		EnvSessionKey:       deriveDomainKey(provisionMasterSecret(), infoSessionKey),
		EnvSSOPublicKey:     ssoPublicKeyFromSeed(deriveDomainKey(provisionMasterSecret(), infoSSOEd25519Seed)),
		envSessionPublicKey: provisionSessionPublicKey(),
		EnvTerminalKey:      provisionTerminalKey(hiveID),
	}
	for name, v := range expect {
		if v == "" {
			t.Fatalf("template-side derivation for %s produced an empty value; test setup is wrong", name)
		}
		if want[name] != v {
			t.Errorf("%s: reconcile derivation diverges from the provisioning template", name)
		}
	}
	if len(want) != len(expect) {
		t.Errorf("desiredPerHiveEnv returned %d vars, want %d", len(want), len(expect))
	}

	// The two PER-HIVE keys must actually differ between hives — otherwise the
	// reconcile would be re-installing the fleet-uniform sharing N1/N3 removed.
	other := desiredPerHiveEnv("hive-bravo")
	if other[EnvHeartbeatKey] == want[EnvHeartbeatKey] {
		t.Error("heartbeat key is identical across hives — not per-hive")
	}
	if other[EnvTerminalKey] == want[EnvTerminalKey] {
		t.Error("terminal key is identical across hives — not per-hive")
	}
}

// TestPerHiveEnvDriftMissingAll covers the four spokes found running purely on
// master fallbacks: a Deployment with the pre-cutover env shape and none of the
// five reconciled vars. All five must be reported as drift.
func TestPerHiveEnvDriftMissingAll(t *testing.T) {
	withTestMaster(t, perHiveEnvTestMaster)
	want := desiredPerHiveEnv("hive-alpha")

	live := envList(
		"HIVE_GITHUB_TOKEN", "tok",
		"DASHBOARD_AUTH_TOKEN", "dash",
		"HIVE_ID", "hive-alpha",
		"HIVE_LEVEL", "3",
		"HIVE_HUB_URL", "https://hive.kubestellar.io",
		envHubSecretMaster, perHiveEnvTestMaster,
	)

	drift := perHiveEnvDrift(live, want)
	if len(drift) != 5 {
		t.Fatalf("drift = %v (%d), want all 5 vars reported missing", drift, len(drift))
	}
	for _, name := range perHiveEnvNames() {
		found := false
		for _, d := range drift {
			if d == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s not reported as drift on a Deployment that lacks it", name)
		}
	}

	// And the patch built from that drift must install all five with the
	// CORRECT values, while preserving every pre-existing var — including the
	// master, which this lane must never remove.
	patchBody, err := perHiveEnvPatchJSON(live, want)
	if err != nil {
		t.Fatalf("perHiveEnvPatchJSON: %v", err)
	}
	merged := decodePatchEnv(t, patchBody)
	for name, v := range want {
		if merged[name] != v {
			t.Errorf("%s: patched value does not match the desired derivation", name)
		}
	}
	if _, ok := merged[envHubSecretMaster]; !ok {
		t.Error("patch dropped HIVE_HUB_SECRET — this lane is additive only, removal is a separate step")
	}
	for _, keep := range []string{"HIVE_GITHUB_TOKEN", "DASHBOARD_AUTH_TOKEN", "HIVE_ID", "HIVE_LEVEL", "HIVE_HUB_URL"} {
		if _, ok := merged[keep]; !ok {
			t.Errorf("patch dropped pre-existing var %s", keep)
		}
	}
}

// TestPerHiveEnvDriftConvergedIsNoOp is the anti-rollout-storm test. A
// Deployment that already carries all five correct values must report ZERO
// drift, because the sweep issues a patch only when drift is non-empty and any
// patch rolls the pod. If this regressed, every sweep would restart every pod
// in the fleet and interrupt every running agent.
func TestPerHiveEnvDriftConvergedIsNoOp(t *testing.T) {
	withTestMaster(t, perHiveEnvTestMaster)
	const hiveID = "hive-alpha"
	want := desiredPerHiveEnv(hiveID)

	live := envList("HIVE_ID", hiveID, envHubSecretMaster, perHiveEnvTestMaster)
	for _, name := range perHiveEnvNames() {
		live = append(live, deploymentEnvVar{Name: name, Value: want[name]})
	}

	if drift := perHiveEnvDrift(live, want); len(drift) != 0 {
		t.Fatalf("converged Deployment reported drift %v — the sweep would patch and roll the pod", drift)
	}

	// Stability: feeding the patch output back in must ALSO report no drift, so
	// a hive converged by this lane is never re-patched on the next cycle.
	patchBody, err := perHiveEnvPatchJSON(live, want)
	if err != nil {
		t.Fatalf("perHiveEnvPatchJSON: %v", err)
	}
	if drift := perHiveEnvDrift(decodePatchEnvSlice(t, patchBody), want); len(drift) != 0 {
		t.Fatalf("re-reading a patched Deployment reported drift %v — patch loop", drift)
	}
}

// TestPerHiveEnvDriftWrongValueCorrected covers the drift shape a stale hive ID
// or a rotated master produces: the var is PRESENT but carries the wrong value.
// Present-but-wrong is worse than absent (the spoke does not fall back to the
// master; it authenticates with a key the hub does not expect), so it must be
// detected and corrected.
func TestPerHiveEnvDriftWrongValueCorrected(t *testing.T) {
	withTestMaster(t, perHiveEnvTestMaster)
	const hiveID = "hive-alpha"
	want := desiredPerHiveEnv(hiveID)

	// Every var correct EXCEPT the heartbeat key, which carries another hive's
	// value — exactly what a copied/stale Deployment looks like.
	stale := desiredPerHiveEnv("hive-bravo")[EnvHeartbeatKey]
	live := envList("HIVE_ID", hiveID)
	for _, name := range perHiveEnvNames() {
		v := want[name]
		if name == EnvHeartbeatKey {
			v = stale
		}
		live = append(live, deploymentEnvVar{Name: name, Value: v})
	}

	drift := perHiveEnvDrift(live, want)
	if len(drift) != 1 || drift[0] != EnvHeartbeatKey {
		t.Fatalf("drift = %v, want exactly [%s] for a present-but-wrong value", drift, EnvHeartbeatKey)
	}

	patchBody, err := perHiveEnvPatchJSON(live, want)
	if err != nil {
		t.Fatalf("perHiveEnvPatchJSON: %v", err)
	}
	merged := decodePatchEnv(t, patchBody)
	if merged[EnvHeartbeatKey] != want[EnvHeartbeatKey] {
		t.Error("wrong heartbeat key was not corrected by the patch")
	}
	if merged[EnvHeartbeatKey] == stale {
		t.Error("patch left the stale heartbeat key in place")
	}
	// Correcting in place must not duplicate the var — a duplicate name in a
	// container env list is an API-server error on apply.
	if n := countEnvName(t, patchBody, EnvHeartbeatKey); n != 1 {
		t.Errorf("patched env list contains %s %d times, want exactly 1", EnvHeartbeatKey, n)
	}
}

// TestPerHiveEnvFailsSafeOnEmptyDerivation is THE fail-safe. derivePerHiveKey
// returns "" for an empty master or an empty hive ID by design. Writing that
// through would set e.g. HIVE_HEARTBEAT_KEY="" on a live spoke — strictly worse
// than absent, because the spoke falls back to the master exactly as if the var
// were missing while the readiness count reports the hive as converged.
// desiredPerHiveEnv must return nil so the sweep SKIPS instead of patching.
func TestPerHiveEnvFailsSafeOnEmptyDerivation(t *testing.T) {
	t.Run("empty hive ID", func(t *testing.T) {
		withTestMaster(t, perHiveEnvTestMaster)
		if got := desiredPerHiveEnv(""); got != nil {
			t.Fatalf("desiredPerHiveEnv(\"\") = %v, want nil — would patch empty values", got)
		}
		if got := desiredPerHiveEnv("   "); got != nil {
			t.Fatalf("desiredPerHiveEnv(whitespace) = %v, want nil", got)
		}
	})

	t.Run("empty master", func(t *testing.T) {
		withTestMaster(t, "")
		if got := desiredPerHiveEnv("hive-alpha"); got != nil {
			t.Fatalf("desiredPerHiveEnv with no master = %v, want nil — would patch empty values", got)
		}
	})

	// And with a nil desired map, the drift function must report NOTHING to do
	// rather than reporting all five as missing — otherwise a keyless hub would
	// see the whole fleet as drifted and try to patch it with empty values.
	t.Run("nil desired reports no drift", func(t *testing.T) {
		live := envList("HIVE_ID", "hive-alpha")
		if drift := perHiveEnvDrift(live, nil); len(drift) != 0 {
			t.Fatalf("perHiveEnvDrift(_, nil) = %v, want empty", drift)
		}
	})
}

// TestPerHiveEnvPositiveControl is the control for the no-op test above.
// "Never patch anything" would satisfy TestPerHiveEnvDriftConvergedIsNoOp
// perfectly, so this asserts the opposite direction: a legitimately drifted
// Deployment DOES produce a patch, and that patch is well-formed JSON-Patch
// targeting the hive container's env list.
func TestPerHiveEnvPositiveControl(t *testing.T) {
	withTestMaster(t, perHiveEnvTestMaster)
	want := desiredPerHiveEnv("hive-alpha")

	// Missing exactly one var — the minimal legitimate reconcile.
	live := envList("HIVE_ID", "hive-alpha", envHubSecretMaster, perHiveEnvTestMaster)
	for _, name := range perHiveEnvNames() {
		if name == envSessionPublicKey {
			continue
		}
		live = append(live, deploymentEnvVar{Name: name, Value: want[name]})
	}

	drift := perHiveEnvDrift(live, want)
	if len(drift) != 1 || drift[0] != envSessionPublicKey {
		t.Fatalf("drift = %v, want exactly [%s] — the reconcile must still fire on real drift",
			drift, envSessionPublicKey)
	}

	patchBody, err := perHiveEnvPatchJSON(live, want)
	if err != nil {
		t.Fatalf("perHiveEnvPatchJSON: %v", err)
	}
	if !strings.Contains(patchBody, hiveContainerEnvPath) {
		t.Errorf("patch %q does not target the hive container env path %q", patchBody, hiveContainerEnvPath)
	}
	if !strings.Contains(patchBody, `"op":"replace"`) {
		t.Errorf("patch %q is not a replace op", patchBody)
	}
	if merged := decodePatchEnv(t, patchBody); merged[envSessionPublicKey] != want[envSessionPublicKey] {
		t.Errorf("%s was not installed by the patch", envSessionPublicKey)
	}
}

// TestPerHiveEnvRateLimitRespected asserts the per-cycle patch cap. Each patch
// rolls a pod and interrupts that hive's running agents, so a sweep over a
// fully-drifted ~70-hive fleet must NOT patch them all at once. This replays
// the sweep's cap arithmetic over a drifted fleet fixture.
func TestPerHiveEnvRateLimitRespected(t *testing.T) {
	withTestMaster(t, perHiveEnvTestMaster)

	const fleetSize = 70
	patched, deferredCount := 0, 0
	for i := 0; i < fleetSize; i++ {
		// Every hive is drifted (missing all five).
		want := desiredPerHiveEnv("hive-" + string(rune('a'+i%26)) + itoa(i))
		if want == nil {
			t.Fatal("fixture hive derived to nil")
		}
		if drift := perHiveEnvDrift(nil, want); len(drift) == 0 {
			t.Fatal("fixture hive should be drifted")
		}
		// Calls the SAME predicate reconcilePerHiveEnv gates on, so removing
		// the sweep's rate limit fails this test rather than leaving it green.
		if !perHiveEnvPatchAllowed(patched) {
			deferredCount++
			continue
		}
		patched++
	}

	if patched != perHiveEnvMaxPatchesPerCycle {
		t.Errorf("patched %d hives in one cycle, want the cap of %d — a bigger batch rolls that many pods at once",
			patched, perHiveEnvMaxPatchesPerCycle)
	}
	if deferredCount != fleetSize-perHiveEnvMaxPatchesPerCycle {
		t.Errorf("deferred %d, want %d", deferredCount, fleetSize-perHiveEnvMaxPatchesPerCycle)
	}
	if perHiveEnvMaxPatchesPerCycle >= fleetSize {
		t.Error("cap is not actually limiting: it would patch the whole fleet in one cycle")
	}
	// The cap must be a small positive number: 0 would never converge, and a
	// large one defeats the purpose.
	if perHiveEnvMaxPatchesPerCycle < 1 || perHiveEnvMaxPatchesPerCycle > 10 {
		t.Errorf("perHiveEnvMaxPatchesPerCycle = %d, want a small positive batch", perHiveEnvMaxPatchesPerCycle)
	}
}

// TestPerHiveEnvReconcileThrottle verifies the poller-loop throttle, so the
// 2-min SHA poller can call the sweep every tick without hammering kubectl.
func TestPerHiveEnvReconcileThrottle(t *testing.T) {
	s := &HubServer{clusterUnreachableUntil: map[string]time.Time{}}

	s.clusterUnreachableMu.Lock()
	due := s.lastPerHiveEnvReconcile.IsZero()
	if due {
		s.lastPerHiveEnvReconcile = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		t.Fatal("first reconcile should be due (zero timestamp)")
	}

	s.clusterUnreachableMu.Lock()
	due2 := time.Since(s.lastPerHiveEnvReconcile) >= perHiveEnvReconcileInterval
	s.clusterUnreachableMu.Unlock()
	if due2 {
		t.Fatal("second reconcile immediately after should be throttled")
	}

	s.clusterUnreachableMu.Lock()
	s.lastPerHiveEnvReconcile = time.Now().Add(-perHiveEnvReconcileInterval - time.Minute)
	due3 := time.Since(s.lastPerHiveEnvReconcile) >= perHiveEnvReconcileInterval
	s.clusterUnreachableMu.Unlock()
	if !due3 {
		t.Fatal("reconcile should be due again after the interval elapses")
	}
}

// TestPerHiveEnvSnapshotMixedFleet asserts the readiness counts reflect reality
// for a mixed fleet, and — critically — that they are sourced from Deployment
// reads rather than heartbeat recency. A hive that has not heartbeated in weeks
// (paused) must STILL appear in the denominator and still block convergence:
// that is the exact failure mode authRolloutStaleAfter has and this must not.
func TestPerHiveEnvSnapshotMixedFleet(t *testing.T) {
	s := &HubServer{}

	// Converged, beating normally.
	s.recordPerHiveEnvObservation("hive-good", perHiveEnvObservation{
		HasMasterSecret: true, Observed: time.Now(),
	})
	// Converged, but its Deployment no longer carries the master — the end state.
	s.recordPerHiveEnvObservation("hive-clean", perHiveEnvObservation{
		HasMasterSecret: false, Observed: time.Now(),
	})
	// Drifted: missing everything (one of the four real spokes).
	s.recordPerHiveEnvObservation("hive-bare", perHiveEnvObservation{
		MissingVars: perHiveEnvNames(), HasMasterSecret: true, Observed: time.Now(),
	})
	// PAUSED and drifted: its last Deployment read was weeks ago. A
	// heartbeat-sourced signal would have dropped this hive from the totals and
	// reported the fleet ready. Deployment-sourced counts must keep it.
	s.recordPerHiveEnvObservation("hive-paused", perHiveEnvObservation{
		MissingVars:     []string{envSessionPublicKey},
		HasMasterSecret: true,
		Observed:        time.Now().Add(-30 * 24 * time.Hour),
	})

	got := s.PerHiveEnvSnapshot()
	if got.ObservedHives != 4 {
		t.Errorf("ObservedHives = %d, want 4", got.ObservedHives)
	}
	if got.MissingPerHiveEnv != 2 {
		t.Errorf("MissingPerHiveEnv = %d, want 2", got.MissingPerHiveEnv)
	}
	if got.StillCarryingMaster != 3 {
		t.Errorf("StillCarryingMaster = %d, want 3", got.StillCarryingMaster)
	}
	if got.PerHiveEnvConverged {
		t.Error("PerHiveEnvConverged = true with 2 drifted hives")
	}
	// The paused hive must be NAMED, not silently dropped.
	if !hasString(got.MissingHives, "hive-paused") {
		t.Errorf("MissingHives = %v, must include the long-unseen paused hive — "+
			"dropping it by age is the authRolloutStaleAfter failure mode this signal avoids",
			got.MissingHives)
	}
	if !hasString(got.MissingHives, "hive-bare") {
		t.Errorf("MissingHives = %v, must include hive-bare", got.MissingHives)
	}

	// Fix both laggards → converged.
	s.recordPerHiveEnvObservation("hive-bare", perHiveEnvObservation{HasMasterSecret: true, Observed: time.Now()})
	s.recordPerHiveEnvObservation("hive-paused", perHiveEnvObservation{HasMasterSecret: true, Observed: time.Now()})
	got = s.PerHiveEnvSnapshot()
	if !got.PerHiveEnvConverged || got.MissingPerHiveEnv != 0 {
		t.Errorf("after repair: converged=%v missing=%d, want true/0", got.PerHiveEnvConverged, got.MissingPerHiveEnv)
	}
	if got.StillCarryingMaster != 3 {
		t.Errorf("StillCarryingMaster = %d, want 3 — env convergence must not imply the master is gone",
			got.StillCarryingMaster)
	}

	// Deprovisioned hives are forgotten, so they cannot pin the counts forever.
	s.forgetPerHiveEnvObservation("hive-clean")
	if got := s.PerHiveEnvSnapshot(); got.ObservedHives != 3 {
		t.Errorf("after forget: ObservedHives = %d, want 3", got.ObservedHives)
	}
}

// TestPerHiveEnvSnapshotFailsClosed: with no observations at all the fleet must
// NOT read converged. "No evidence" must never gate the master-secret removal.
func TestPerHiveEnvSnapshotFailsClosed(t *testing.T) {
	s := &HubServer{}
	if got := s.PerHiveEnvSnapshot(); got.PerHiveEnvConverged {
		t.Error("PerHiveEnvConverged = true with zero observations; must fail closed")
	}
	var nilServer *HubServer
	if got := nilServer.PerHiveEnvSnapshot(); got.PerHiveEnvConverged {
		t.Error("nil HubServer must not report converged")
	}
}

// TestAuthRolloutIncludesPerHiveEnv asserts the readiness surface actually
// carries the new counts, so convergence is measurable over the existing
// endpoint without shelling out.
func TestAuthRolloutIncludesPerHiveEnv(t *testing.T) {
	s := &HubServer{}
	s.recordPerHiveEnvObservation("hive-bare", perHiveEnvObservation{
		MissingVars: perHiveEnvNames(), HasMasterSecret: true, Observed: time.Now(),
	})
	out := s.AuthRolloutReadiness()
	if out.PerHiveEnv.ObservedHives != 1 || out.PerHiveEnv.MissingPerHiveEnv != 1 {
		t.Errorf("AuthRolloutReadiness did not carry the per-hive env counts: %+v", out.PerHiveEnv)
	}
	if out.PerHiveEnv.StillCarryingMaster != 1 {
		t.Errorf("StillCarryingMaster not surfaced: %+v", out.PerHiveEnv)
	}
	// The JSON must actually serialize the nested block for operators.
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"per_hive_env", "missing_per_hive_env", "still_carrying_master", "per_hive_env_converged"} {
		if !strings.Contains(string(b), key) {
			t.Errorf("auth-rollout JSON missing %q", key)
		}
	}
}

// --- helpers ---

func decodePatchEnv(t *testing.T, patchBody string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range decodePatchEnvSlice(t, patchBody) {
		out[e.Name] = e.Value
	}
	return out
}

func decodePatchEnvSlice(t *testing.T, patchBody string) []deploymentEnvVar {
	t.Helper()
	var ops []struct {
		Op    string             `json:"op"`
		Path  string             `json:"path"`
		Value []deploymentEnvVar `json:"value"`
	}
	if err := json.Unmarshal([]byte(patchBody), &ops); err != nil {
		t.Fatalf("patch body is not valid JSON-Patch: %v (%s)", err, patchBody)
	}
	if len(ops) != 1 {
		t.Fatalf("patch has %d ops, want exactly 1", len(ops))
	}
	if ops[0].Path != hiveContainerEnvPath {
		t.Fatalf("patch path = %q, want %q", ops[0].Path, hiveContainerEnvPath)
	}
	return ops[0].Value
}

func countEnvName(t *testing.T, patchBody, name string) int {
	t.Helper()
	n := 0
	for _, e := range decodePatchEnvSlice(t, patchBody) {
		if e.Name == name {
			n++
		}
	}
	return n
}

func hasString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// realRegistryStatuses are the hive lifecycle statuses that actually occur,
// with the LIVE distribution measured across all 66 /data/saas/hives/*/meta.json
// records on the production hub at the time this test was written:
//
//	30 "available"   28 ""   8 "assigned"   0 "running"
//
// The zero is the whole point. The sweep originally guarded on
// `h.Status != "running"`, so it selected no hive on any cycle and had never
// patched a spoke in production. These fixtures are stated as real data, not
// invented values, so that a future change to the status vocabulary that
// invalidates them is a test failure rather than a silent regression.
var realRegistryStatuses = []struct {
	status string
	live   int
	want   bool
	why    string
}{
	{"", 28, true, "the common steady state: provisioned, deployed, no status ever written"},
	{"available", 30, true, "unclaimed placeholder; deployed, and must hold correct keys BEFORE it is claimed"},
	{"assigned", 8, true, "claimed and deployed"},
	{"running", 0, true, "never observed live, but the pre-existing intent must still select"},
	{"error", 0, true, "status is often stale and the Deployment usually still exists; drift may be the CAUSE"},
	{"provisioning", 0, false, "namespace/Deployment still being applied; the template renders all five vars at creation"},
}

// TestPerHiveEnvSweepEligibleOverRealStatuses pins the sweep's hive-selection
// predicate against the REAL status vocabulary. This is the test layer that was
// missing: the original suite exercised desiredPerHiveEnv, perHiveEnvDrift,
// perHiveEnvPatchJSON and the rate limiter directly, but nothing covered the
// filter that decides which hives reach any of them — so a filter matching
// NOTHING left every one of those tests green.
func TestPerHiveEnvSweepEligibleOverRealStatuses(t *testing.T) {
	for _, tc := range realRegistryStatuses {
		if got := perHiveEnvSweepEligible(tc.status); got != tc.want {
			t.Errorf("perHiveEnvSweepEligible(%q) = %v, want %v — %s", tc.status, got, tc.want, tc.why)
		}
	}
}

// TestPerHiveEnvSweepSelectsTheLiveFleet replays the predicate over a fixture
// with the exact live status distribution and asserts the sweep would examine
// the whole 66-hive fleet.
//
// POSITIVE CONTROL, BOTH DIRECTIONS. The bug being fixed is "selects nothing",
// so a test that can only fail when the predicate becomes too permissive is
// insufficient — and the converse is equally true. This asserts a exact
// selected count, which is the only assertion that fails in BOTH directions:
//
//   - Neuter the predicate to `return false` (the shipped bug, and the
//     `!= "running"` guard is behaviourally identical against this fixture):
//     selected becomes 0, and the count assertion fails.
//   - Neuter it to `return true` (select everything unconditionally):
//     selected becomes 66, "provisioning" is no longer excluded, and both the
//     count assertion and the explicit skipped-bucket assertion fail.
//
// Neither direction can be satisfied by a predicate that ignores its argument.
func TestPerHiveEnvSweepSelectsTheLiveFleet(t *testing.T) {
	// The live registry: 66 hives, none "running".
	type hive struct{ id, status string }
	var fleet []hive
	for _, tc := range realRegistryStatuses {
		for i := 0; i < tc.live; i++ {
			fleet = append(fleet, hive{id: "hosted-" + tc.status + itoa(i), status: tc.status})
		}
	}
	// Add one hive in each status that has no live instances today, so the
	// predicate is exercised over the full vocabulary rather than only the
	// three statuses that happen to be populated right now.
	for _, tc := range realRegistryStatuses {
		if tc.live == 0 {
			fleet = append(fleet, hive{id: "hosted-synth-" + tc.status, status: tc.status})
		}
	}

	const liveFleetSize = 66 // 30 available + 28 "" + 8 assigned
	if got := len(fleet) - 3; got != liveFleetSize {
		t.Fatalf("fixture models %d live hives, want %d — update the measured distribution", got, liveFleetSize)
	}

	var selected, skipped int
	var selectedProvisioning int
	for _, h := range fleet {
		if !perHiveEnvSweepEligible(h.status) {
			skipped++
			continue
		}
		selected++
		if h.status == "provisioning" {
			selectedProvisioning++
		}
	}

	// Direction 1 — the shipped bug. A predicate that selects nobody (or that
	// keys off "running", which no live hive has) drives this to 0.
	if selected == 0 {
		t.Fatal("sweep selected NO hives from a 66-hive fleet — this is the production bug: " +
			"the lane runs every cycle and patches nothing, and drifted spokes never converge")
	}
	// The 66 live hives + the synthetic "running" and "error" rows are all
	// eligible; only the synthetic "provisioning" row is not.
	const wantSelected = liveFleetSize + 2
	if selected != wantSelected {
		t.Errorf("selected %d hives, want %d", selected, wantSelected)
	}
	// Direction 2 — over-selection. `return true` makes this 0 and trips here.
	if skipped != 1 {
		t.Errorf("skipped %d hives, want exactly 1 (the provisioning row) — "+
			"a predicate that selects everything unconditionally skips 0", skipped)
	}
	if selectedProvisioning != 0 {
		t.Error("a hive still provisioning was selected; its Deployment may not exist yet")
	}

	// The four spokes measured as drifted on the live fleet sit in "" and
	// "available". If either status were excluded, the spokes this lane exists
	// to repair would still never converge. Assert that explicitly rather than
	// relying on the aggregate count.
	for _, s := range []string{"", "available"} {
		if !perHiveEnvSweepEligible(s) {
			t.Errorf("status %q is not selected, but known-drifted production spokes carry it", s)
		}
	}
}

// TestPerHiveEnvSweepEligibleRejectsTheOldGuard is the regression replay,
// pinned as a permanent test. It reconstructs the exact predicate that shipped
// and asserts it selects nothing over the real distribution — so if anyone
// reintroduces a "running"-based filter, the reasoning is already written down
// next to the failure.
func TestPerHiveEnvSweepEligibleRejectsTheOldGuard(t *testing.T) {
	oldGuard := func(status string) bool { return status == "running" }

	var oldSelected, newSelected int
	for _, tc := range realRegistryStatuses {
		if oldGuard(tc.status) {
			oldSelected += tc.live
		}
		if perHiveEnvSweepEligible(tc.status) {
			newSelected += tc.live
		}
	}
	if oldSelected != 0 {
		t.Fatalf("fixture no longer reproduces the bug: old guard selected %d hives, expected 0", oldSelected)
	}
	if newSelected == 0 {
		t.Fatal("fixed predicate ALSO selects nothing — the bug is not fixed")
	}
	if newSelected != 66 {
		t.Errorf("fixed predicate selects %d of 66 live hives, want all 66", newSelected)
	}
}
