package hub

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Coverage for the reconcileContributeIngress sweep loop itself
// (contribute_ingress_reconcile.go). The pure helpers — annotation set, patch
// computation, eligibility, throttle — are pinned in
// contribute_ingress_reconcile_test.go; these tests drive the loop end to end
// through the same fake-kubectl fixture the NET_ADMIN sweep uses
// (netadmin_sweep_coverage_test.go), so the get/patch shelling, the breaker
// interplay, and the sweep accounting are all exercised.

// contributeSweepHubURL keeps hubPublicURL() deterministic for these tests.
const contributeSweepHubURL = "https://hub.sweep.test"

// driftedContributeIngressJSON is a hive-contribute Ingress as a pre-#7457
// spoke serves it: cert-manager issuer present, neither auth annotation.
func driftedContributeIngressJSON(t *testing.T) string {
	t.Helper()
	return string(liveIngressJSON(t, map[string]string{
		"cert-manager.io/cluster-issuer": "letsencrypt-prod",
	}))
}

// TestReconcileContributeIngressPatchesDriftedSpoke is the issue's remediation
// path: a hosted spoke whose live hive-contribute Ingress carries no auth-url
// gets exactly one merge patch with both #7457 annotations, in its hosted
// namespace, and the successful kubectl round-trip clears any unreachable
// suppression armed for the cluster.
func TestReconcileContributeIngressPatchesDriftedSpoke(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", contributeSweepHubURL)
	s := netAdminSweepServer(t)
	logPath := installNetAdminKubectl(t, driftedContributeIngressJSON(t), 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-drift-ci01", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	// Pre-arm an expired suppression entry so the success path visibly clears it.
	s.clusterUnreachableUntil[defaultClusterID] = time.Now().Add(-time.Minute)

	s.reconcileContributeIngress()

	logged := readKubectlLog(t, logPath)
	ns := hiveHostedNamespacePrefix + "hosted-drift-ci01"
	if !strings.Contains(logged, "get ingress "+contributeIngressName+" -n "+ns) {
		t.Errorf("expected a live-Ingress read in %s, got:\n%s", ns, logged)
	}
	if !strings.Contains(logged, "patch ingress "+contributeIngressName+" -n "+ns) {
		t.Errorf("expected a merge patch in %s, got:\n%s", ns, logged)
	}
	// The patch body rides the log as the argument after `-p`; decode it
	// rather than substring-match, because json.Marshal escapes `&` in the
	// auth-url as \u0026 on the wire.
	// The fake kubectl serves the same drifted document for every Ingress,
	// so the sweep patches hive-contribute AND hive-terminal — one patch
	// each, carrying that Ingress's own annotation set.
	patches := patchBodiesByIngress(t, logged)
	for _, target := range gatedIngressTargets() {
		got, ok := patches[target.name]
		if !ok {
			t.Fatalf("expected a merge patch on %s, got:\n%s", target.name, logged)
		}
		for key, value := range target.want(contributeSweepHubURL, "hosted-drift-ci01") {
			if got[key] != value {
				t.Errorf("%s patch should carry %s = %q, got %q", target.name, key, value, got[key])
			}
		}
	}
	if _, suppressed := s.clusterUnreachableUntil[defaultClusterID]; suppressed {
		t.Error("successful get+patch should mark the cluster reachable again")
	}
}

// TestReconcileContributeIngressConvergedIsNoOp: a spoke already carrying both
// annotations is read but never patched — the idempotence that lets the sweep
// run every interval without churning converged fleets.
func TestReconcileContributeIngressConvergedIsNoOp(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", contributeSweepHubURL)
	s := netAdminSweepServer(t)
	// hive-terminal's set is a superset of hive-contribute's, so one document
	// carrying it converges both reads the fake kubectl serves.
	converged := string(liveIngressJSON(t,
		terminalIngressAuthAnnotations(contributeSweepHubURL, "hosted-ok-ci02")))
	logPath := installNetAdminKubectl(t, converged, 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-ok-ci02", Status: "assigned"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileContributeIngress()

	logged := readKubectlLog(t, logPath)
	for _, target := range gatedIngressTargets() {
		if !strings.Contains(logged, "get ingress "+target.name) {
			t.Errorf("expected the live-Ingress read of %s, got:\n%s", target.name, logged)
		}
	}
	if strings.Contains(logged, "patch") {
		t.Errorf("converged Ingress must not be patched, got:\n%s", logged)
	}
}

// patchBodiesByIngress decodes every `kubectl patch ingress <name> ... -p
// <body>` line in the fake-kubectl log into name → annotations. Decoded rather
// than substring-matched because json.Marshal escapes `&` in the auth-url.
func patchBodiesByIngress(t *testing.T, logged string) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, line := range strings.Split(logged, "\n") {
		_, rest, found := strings.Cut(line, "patch ingress ")
		if !found {
			continue
		}
		name, _, _ := strings.Cut(rest, " ")
		_, body, found := strings.Cut(line, " -p ")
		if !found {
			t.Fatalf("patch line without -p body: %s", line)
		}
		out[name] = decodePatch(t, strings.TrimSpace(body))
	}
	return out
}

// TestReconcileTerminalIngressRepointsStaleHubHost is the /terminal 500 on
// spokes provisioned under the hub's previous public host: hive-terminal's
// auth-url and auth-signin still name that host (which now 301s, and nginx
// turns a 301 auth_request answer into a 500). The sweep must patch exactly
// the two stale values onto hive-terminal, and leave the headers alone.
func TestReconcileTerminalIngressRepointsStaleHubHost(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", contributeSweepHubURL)
	s := netAdminSweepServer(t)
	stale := terminalIngressAuthAnnotations("https://hub.old-host.test", "hosted-stale-ci09")
	logPath := installNetAdminKubectl(t, string(liveIngressJSON(t, stale)), 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-stale-ci09", Status: "assigned"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileContributeIngress()

	patches := patchBodiesByIngress(t, readKubectlLog(t, logPath))
	got, ok := patches[terminalIngressName]
	if !ok {
		t.Fatalf("expected a merge patch on %s, got patches for %v", terminalIngressName, patches)
	}
	want := terminalIngressAuthAnnotations(contributeSweepHubURL, "hosted-stale-ci09")
	if got[ingressAuthURLAnnotation] != want[ingressAuthURLAnnotation] {
		t.Errorf("auth-url = %q, want %q", got[ingressAuthURLAnnotation], want[ingressAuthURLAnnotation])
	}
	if got[ingressAuthSigninAnnotation] != want[ingressAuthSigninAnnotation] {
		t.Errorf("auth-signin = %q, want %q", got[ingressAuthSigninAnnotation], want[ingressAuthSigninAnnotation])
	}
	if _, present := got[ingressAuthResponseHeadersAnnotation]; present {
		t.Errorf("auth-response-headers was not stale and must not be in the patch: %v", got)
	}
}

// TestReconcileContributeIngressGetFailureIsNonFatal: a failing `kubectl get`
// (Ingress missing on a pre-hive-contribute spoke, mid-teardown, transient
// error) skips the hive without patching and without arming the
// cluster-unreachable breaker — the next sweep simply retries.
func TestReconcileContributeIngressGetFailureIsNonFatal(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", contributeSweepHubURL)
	s := netAdminSweepServer(t)
	logPath := installNetAdminKubectl(t, "", 1, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-gone-ci03", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileContributeIngress()

	logged := readKubectlLog(t, logPath)
	if strings.Contains(logged, "patch") {
		t.Errorf("failed read must not lead to a patch, got:\n%s", logged)
	}
	if s.clusterRecentlyUnreachable(defaultClusterID) {
		t.Error("a failed get must not arm the unreachable breaker")
	}
}

// TestReconcileContributeIngressUnparseableIngressIsNotPatchedBlind: `kubectl
// get` succeeding with output that does not parse is a failure, not a patch —
// patching blind onto an object the sweep could not read is the outage the
// code comment warns about. The breaker stays disarmed: the cluster answered.
func TestReconcileContributeIngressUnparseableIngressIsNotPatchedBlind(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", contributeSweepHubURL)
	s := netAdminSweepServer(t)
	logPath := installNetAdminKubectl(t, "not-json{{{", 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-mangled-ci04", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileContributeIngress()

	logged := readKubectlLog(t, logPath)
	if strings.Contains(logged, "patch") {
		t.Errorf("unparseable Ingress must never be patched, got:\n%s", logged)
	}
	if s.clusterRecentlyUnreachable(defaultClusterID) {
		t.Error("a parse failure is not a dial failure — breaker must stay disarmed")
	}
}

// TestReconcileContributeIngressPatchFailureArmsBreaker: a failing patch is
// retried next sweep AND arms the unreachable suppression, so one down cluster
// does not cost a kubectl timeout per hive on the very next tick.
func TestReconcileContributeIngressPatchFailureArmsBreaker(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", contributeSweepHubURL)
	s := netAdminSweepServer(t)
	logPath := installNetAdminKubectl(t, driftedContributeIngressJSON(t), 0, 1)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-flaky-ci05", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileContributeIngress()

	logged := readKubectlLog(t, logPath)
	if !strings.Contains(logged, "patch ingress "+contributeIngressName) {
		t.Errorf("expected the patch attempt, got:\n%s", logged)
	}
	if !s.clusterRecentlyUnreachable(defaultClusterID) {
		t.Error("a failed patch must arm the unreachable breaker for the cluster")
	}
}

// TestReconcileContributeIngressSkipsClustersItCannotOrMustNotTouch drives the
// three per-cluster skip lanes — OpenShift Route (no nginx Ingress exists),
// pull-only (no kubectl path), recently-unreachable (suppressed) — and pins
// that none of them shells out at all.
func TestReconcileContributeIngressSkipsClustersItCannotOrMustNotTouch(t *testing.T) {
	cases := []struct {
		name    string
		cluster ClusterConfig
		arm     bool
	}{
		{"openshift route cluster has no nginx ingress",
			ClusterConfig{ID: defaultClusterID, InCluster: true, IngressType: ingressTypeOpenShiftRoute}, false},
		{"pull-only cluster has no kubectl path",
			ClusterConfig{ID: defaultClusterID, PullOnly: true}, false},
		{"recently unreachable cluster is suppressed",
			ClusterConfig{ID: defaultClusterID, InCluster: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_HUB_PUBLIC_URL", contributeSweepHubURL)
			s := netAdminSweepServer(t)
			s.clusters[defaultClusterID] = tc.cluster
			if tc.arm {
				s.clusterUnreachableUntil[defaultClusterID] = time.Now().Add(time.Hour)
			}
			logPath := installNetAdminKubectl(t, driftedContributeIngressJSON(t), 0, 0)
			if err := saveSaaSHive(&SaaSHive{ID: "hosted-skip-ci06", Status: "available"}); err != nil {
				t.Fatal(err)
			}

			s.reconcileContributeIngress()

			if logged := readKubectlLog(t, logPath); logged != "" {
				t.Errorf("skipped cluster must never be dialled, got:\n%s", logged)
			}
		})
	}
}

// TestReconcileContributeIngressWarnsWhenSweepSelectsNobody pins the
// dead-sweep tripwire: a registry with hives where the filter admits none of
// them must say so loudly (the NET_ADMIN sweep shipped exactly this bug and
// nobody could tell), and a provisioning hive alone must trip it.
func TestReconcileContributeIngressWarnsWhenSweepSelectsNobody(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", contributeSweepHubURL)
	s := netAdminSweepServer(t)
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))
	logPath := installNetAdminKubectl(t, driftedContributeIngressJSON(t), 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-fresh-ci07", Status: "provisioning"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileContributeIngress()

	if logged := readKubectlLog(t, logPath); logged != "" {
		t.Errorf("provisioning hive must be skipped before kubectl, got:\n%s", logged)
	}
	if out := buf.String(); !strings.Contains(out, "selected NO hives") {
		t.Errorf("a sweep that admits nobody on a hosting hub must warn, got:\n%s", out)
	}
}
