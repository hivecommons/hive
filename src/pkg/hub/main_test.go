package hub

import (
	"fmt"
	"log/slog"
	"os"
	"testing"
)

// realFetchCommitCompareStatus preserves the production compare fetcher so its
// HTTP behaviour stays directly testable (TestFetchCommitCompareStatusHTTP)
// after TestMain swaps the package var for the offline default below.
var realFetchCommitCompareStatus func(base, head string, logger *slog.Logger) (string, error)

// TestMain isolates the hub test suite from any real Kubernetes cluster.
//
// Why this exists: many hub tests exercise handlers that shell out to `kubectl`
// (rolloutHubToSHA, handleSwitchBranch, triggerAutoUpgrades, ...). Those tests
// usually install a scripted fake kubectl on PATH, but several deliberately run
// a "kubectl fails" branch FIRST, with the real binary still on PATH.
//
// kubectlArgsForCluster builds in-cluster credentials from KUBERNETES_SERVICE_HOST
// and KUBERNETES_SERVICE_PORT. When the suite runs anywhere those are set — most
// importantly inside a hive pod, where agents run the tests — a real kubectl
// reaches the REAL API server using the pod's ServiceAccount, which holds patch
// on the hub Deployment.
//
// That is exactly how the 2026-07-27 outage happened: TestHandleHubSelfUpgrade_Edge
// seeds the SHA cache with the fixture value "target1" and calls
// handleHubSelfUpgrade, producing a literal
//
//	kubectl set image deployment/hive-hub hub=ghcr.io/hivecommons/hive-hub:target1 -n hive-hub
//
// against the live cluster. `target1` was never a published tag, so the new
// ReplicaSet went ImagePullBackOff while the old one kept serving — the hub
// looked healthy for ~20 minutes while silently running stale code.
//
// Clearing these two variables makes kubectlArgsForCluster omit --server/--token,
// so a stray real kubectl has no cluster to talk to and fails locally instead of
// mutating production. Tests that WANT kubectl behavior still get it from their
// scripted fake on PATH, which ignores these flags entirely.
func TestMain(m *testing.M) {
	os.Unsetenv("KUBERNETES_SERVICE_HOST")
	os.Unsetenv("KUBERNETES_SERVICE_PORT")
	// KUBECONFIG could equally point a real kubectl at a real cluster for the
	// non-InCluster branch, so neutralize it to a path that cannot exist.
	os.Setenv("KUBECONFIG", os.DevNull)
	k8sTokenPath = "testdata/no-such-serviceaccount-token"
	k8sCACertPath = "testdata/no-such-serviceaccount-ca.crt"

	// Commit-order ancestry resolves are kicked as BACKGROUND goroutines from
	// the heartbeat completion chain, the orphan sweep and triggerAutoUpgrades
	// (commitAtOrAheadOfTarget). Any test that crosses those paths with an
	// unresolved SHA pair — most don't even know they do — would otherwise
	// launch a REAL GitHub compare call with a 10s budget
	// (commitCompareTimeout). One slow call is exactly the 2026-08-17 coverage
	// red at bb2d554d: a leaked resolver outlived three consecutive 2s
	// waitForCommitOrderResolvers deadlines in sha_poll_coverage_test.go, so
	// TestListRepoBranches{,Error} and TestFetchBranchSHAImageReady failed on
	// an otherwise green branch. Default the fetcher to an instant offline
	// error: errors are never cached, so semantics match "resolution pending"
	// (fail-closed — an unknown pair never reads as complete), and tests that
	// need definitive answers already stub via stubCommitCompare or
	// seedCommitOrder. Held under commitOrderMu per the var's contract.
	commitOrderMu.Lock()
	realFetchCommitCompareStatus = fetchCommitCompareStatus
	fetchCommitCompareStatus = func(base, head string, _ *slog.Logger) (string, error) {
		return "", fmt.Errorf("commit-order compare %s...%s: network disabled in unit tests (stub via stubCommitCompare)", base, head)
	}
	commitOrderMu.Unlock()

	os.Exit(m.Run())
}
