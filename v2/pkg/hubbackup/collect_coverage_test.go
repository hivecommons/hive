package hubbackup

import (
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// spokeStream renders files the way buildSpokeReadScript's output looks coming
// back over `kubectl exec`, so collectOne is driven through its real parser.
func spokeStream(files map[string]string) string {
	var sb strings.Builder
	for name, content := range files {
		fmt.Fprintf(&sb, "%s%s%s\n%s\n",
			spokeStreamPrefix, name, spokeStreamDelim,
			base64.StdEncoding.EncodeToString([]byte(content)))
	}
	return sb.String()
}

// ---- KubectlSpokeCollector.Collect / collectOne ----

// A spoke whose pod is Evicted must be recorded as an error entry, NOT dropped:
// losing the entry silently shrinks the fleet in the backup, which is how a
// 50/50 backup became 49/50.
func TestCollectEvictedSpokeIsRecordedNotDropped(t *testing.T) {
	fakeKubectl(t, map[string]string{
		// No Running pod: the field-selector query returns empty stdout.
		"get pods -n " + spokeNamespacePrefix + "alpha": "",
		"get pods -n " + spokeNamespacePrefix + "beta":  "beta-pod-1",
		"exec -n " + spokeNamespacePrefix + "beta":      spokeStream(map[string]string{"hive.yaml.bak": "beta: config"}),
	})

	k := KubectlSpokeCollector{
		Clusters: map[string]ClusterTarget{"c1": {ID: "c1", InCluster: true}},
		Hives:    map[string]string{"alpha": "c1", "beta": "c1"},
	}
	got, err := k.Collect(quietLogger())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Both hives must still be present in the returned set.
	if len(got) != 2 {
		t.Fatalf("want 2 spoke entries (no hive dropped), got %d: %+v", len(got), got)
	}
	byID := map[string]SpokeConfig{}
	for _, sc := range got {
		byID[sc.ID] = sc
	}
	alpha, ok := byID["alpha"]
	if !ok {
		t.Fatal("hive 'alpha' was dropped from the result set entirely")
	}
	if alpha.Err == "" {
		t.Fatal("evicted-only spoke must carry an Err explaining the gap")
	}
	if !strings.Contains(alpha.Err, "no running pod") {
		t.Errorf("Err should explain no running pod, got %q", alpha.Err)
	}
	beta := byID["beta"]
	if beta.Err != "" {
		t.Fatalf("healthy spoke should have no Err, got %q", beta.Err)
	}
	if string(beta.Files["hive.yaml.bak"]) != "beta: config" {
		t.Errorf("healthy spoke content wrong: %q", beta.Files["hive.yaml.bak"])
	}
}

// The pod query must filter to Running, otherwise an Evicted pod can be picked
// by .items[0] and that hive's config is silently lost.
func TestCollectRequestsOnlyRunningPods(t *testing.T) {
	var seen string
	fakeKubectl(t, map[string]string{"get pods": ""})
	orig := execCommandContext
	t.Cleanup(func() { execCommandContext = orig })
	inner := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "get pods") {
			seen = joined
		}
		return inner(ctx, name, args...)
	}

	k := KubectlSpokeCollector{
		Clusters: map[string]ClusterTarget{"c1": {ID: "c1", InCluster: true}},
		Hives:    map[string]string{"alpha": "c1"},
	}
	if _, err := k.Collect(quietLogger()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !strings.Contains(seen, "--field-selector=status.phase=Running") {
		t.Fatalf("pod query must filter to Running pods, got %q", seen)
	}
}

// A hive pointing at a cluster that is not in the Clusters map must still
// appear, carrying the reason.
func TestCollectUnknownClusterIsRecordedNotDropped(t *testing.T) {
	fakeKubectl(t, map[string]string{})
	k := KubectlSpokeCollector{
		Clusters: map[string]ClusterTarget{"c1": {ID: "c1", InCluster: true}},
		Hives:    map[string]string{"orphan": "does-not-exist"},
	}
	got, err := k.Collect(quietLogger())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	if !strings.Contains(got[0].Err, "unknown cluster") {
		t.Fatalf("want unknown-cluster error, got %q", got[0].Err)
	}
}

// A spoke that returns hive.yaml but NOT hive.yaml.bak must be flagged: the
// .bak file is the authoritative config, and a live spoke carries both with
// DIFFERENT contents, so accepting hive.yaml backs up stale config.
func TestCollectRejectsSpokeMissingAuthoritativeConfig(t *testing.T) {
	fakeKubectl(t, map[string]string{
		"get pods": "pod-1",
		"exec":     spokeStream(map[string]string{"hive.yaml": "stale: true"}),
	})
	k := KubectlSpokeCollector{
		Clusters: map[string]ClusterTarget{"c1": {ID: "c1", InCluster: true}},
		Hives:    map[string]string{"alpha": "c1"},
	}
	got, _ := k.Collect(quietLogger())
	if got[0].Err == "" {
		t.Fatal("a spoke without hive.yaml.bak must be flagged, not accepted silently")
	}
	if !strings.Contains(got[0].Err, "hive.yaml.bak") {
		t.Errorf("error should name hive.yaml.bak, got %q", got[0].Err)
	}
}

// exec failure is tolerated per-spoke rather than aborting the fleet backup.
func TestCollectExecFailureRecordedPerSpoke(t *testing.T) {
	fakeKubectl(t, map[string]string{
		"get pods": "pod-1",
		"exec":     failMarker + "container not ready",
	})
	k := KubectlSpokeCollector{
		Clusters: map[string]ClusterTarget{"c1": {ID: "c1", InCluster: true}},
		Hives:    map[string]string{"alpha": "c1"},
	}
	got, err := k.Collect(quietLogger())
	if err != nil {
		t.Fatalf("one bad spoke must not fail the whole collection: %v", err)
	}
	if !strings.Contains(got[0].Err, "exec into") {
		t.Fatalf("want exec error recorded, got %q", got[0].Err)
	}
}

// Undecodable base64 in the stream must surface as a parse error on that spoke.
func TestCollectParseFailureRecorded(t *testing.T) {
	fakeKubectl(t, map[string]string{
		"get pods": "pod-1",
		"exec":     spokeStreamPrefix + "hive.yaml.bak" + spokeStreamDelim + "\n!!!not-base64!!!\n",
	})
	k := KubectlSpokeCollector{
		Clusters: map[string]ClusterTarget{"c1": {ID: "c1", InCluster: true}},
		Hives:    map[string]string{"alpha": "c1"},
	}
	got, _ := k.Collect(quietLogger())
	if !strings.Contains(got[0].Err, "parse spoke stream") {
		t.Fatalf("want parse error, got %q", got[0].Err)
	}
}

// Results must be ordered deterministically so two runs of the same fleet
// produce comparable manifests.
func TestCollectIsDeterministicallyOrdered(t *testing.T) {
	fakeKubectl(t, map[string]string{"get pods": ""})
	k := KubectlSpokeCollector{
		Clusters: map[string]ClusterTarget{"c1": {ID: "c1", InCluster: true}},
		Hives:    map[string]string{"zeta": "c1", "alpha": "c1", "mid": "c1"},
	}
	got, _ := k.Collect(quietLogger())
	var ids []string
	for _, sc := range got {
		ids = append(ids, sc.ID)
	}
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("spokes must be sorted by ID, got %v", ids)
		}
	}
}

// ---- KubectlSecretCollector ----

func TestSecretCollectorCapturesAndStrips(t *testing.T) {
	raw := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"hive-hub-secrets",` +
		`"uid":"abc","resourceVersion":"991","creationTimestamp":"2020-01-01T00:00:00Z"},` +
		`"data":{"k":"dg=="}}`
	fakeKubectl(t, map[string]string{"get secret": raw})

	k := KubectlSecretCollector{
		Cluster:   ClusterTarget{ID: "hub", InCluster: true},
		Namespace: "hive-hub",
		Names:     []string{"hive-hub-secrets"},
	}
	items, err := k.Collect(quietLogger())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 secret, got %d", len(items))
	}
	body := string(items[0].JSON)
	for _, noise := range []string{"resourceVersion", "uid", "creationTimestamp"} {
		if strings.Contains(body, noise) {
			t.Errorf("%s must be stripped so the secret re-applies to a new cluster", noise)
		}
	}
	if !strings.Contains(body, `"data"`) {
		t.Error("secret payload must be preserved")
	}
}

// A missing TLS secret is survivable (cert-manager reissues); any OTHER missing
// secret makes the archive unrestorable and must be fatal.
func TestSecretCollectorTLSOptionalOthersFatal(t *testing.T) {
	t.Run("tls missing is tolerated", func(t *testing.T) {
		fakeKubectl(t, map[string]string{"get secret": failMarker + "NotFound"})
		k := KubectlSecretCollector{
			Cluster: ClusterTarget{ID: "hub", InCluster: true},
			Names:   []string{"hive-hub-tls"},
		}
		if _, err := k.Collect(quietLogger()); err != nil {
			t.Fatalf("missing TLS secret must not fail the backup: %v", err)
		}
	})

	t.Run("oauth secret missing is fatal", func(t *testing.T) {
		fakeKubectl(t, map[string]string{"get secret": failMarker + "NotFound"})
		k := KubectlSecretCollector{
			Cluster: ClusterTarget{ID: "hub", InCluster: true},
			Names:   []string{"hive-hub-secrets"},
		}
		if _, err := k.Collect(quietLogger()); err == nil {
			t.Fatal("a missing required secret must be fatal — the archive would not restore")
		}
	})
}

// Malformed JSON from kubectl must be an error, not a silently empty secret.
func TestSecretCollectorRejectsMalformedJSON(t *testing.T) {
	fakeKubectl(t, map[string]string{"get secret": "{not json"})
	k := KubectlSecretCollector{
		Cluster: ClusterTarget{ID: "hub", InCluster: true},
		Names:   []string{"hive-hub-secrets"},
	}
	if _, err := k.Collect(quietLogger()); err == nil {
		t.Fatal("malformed secret JSON must be rejected")
	}
}

// Remote clusters must be addressed with their kubeconfig and context, or the
// backup silently reads the WRONG cluster.
func TestRemoteClusterFlagsReachRemoteCluster(t *testing.T) {
	var seen string
	fakeKubectl(t, map[string]string{"get pods": ""})
	inner := execCommandContext
	t.Cleanup(func() { execCommandContext = inner })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		seen = strings.Join(args, " ")
		return inner(ctx, name, args...)
	}

	k := KubectlSpokeCollector{
		Clusters: map[string]ClusterTarget{"remote": {
			ID: "remote", InCluster: false,
			KubeconfigPath: "/etc/hive/kubeconfigs/vllm-d.yaml", Context: "vllm-d",
		}},
		Hives: map[string]string{"alpha": "remote"},
	}
	if _, err := k.Collect(quietLogger()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !strings.Contains(seen, "--kubeconfig /etc/hive/kubeconfigs/vllm-d.yaml") {
		t.Errorf("remote cluster must be addressed via --kubeconfig, got %q", seen)
	}
	if !strings.Contains(seen, "--context vllm-d") {
		t.Errorf("remote cluster must be addressed via --context, got %q", seen)
	}
}
