package hubbackup

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
)

// The backup ClusterRole grants pods/exec cluster-wide (issue #4062), so the
// binary itself must refuse any target outside the hive-hosted-* tree.
func TestValidateSpokeNamespace(t *testing.T) {
	valid := []string{"hive-hosted-a", "hive-hosted-acme", "hive-hosted-acme-01"}
	for _, ns := range valid {
		if err := validateSpokeNamespace(ns); err != nil {
			t.Errorf("validateSpokeNamespace(%q) = %v, want nil", ns, err)
		}
	}

	invalid := []string{
		"",
		"default",
		"kube-system",
		"hive-hosted-",
		"hive-hosted-../kube-system",
		"hive-hosted-UPPER",
		"xhive-hosted-a",
	}
	for _, ns := range invalid {
		if err := validateSpokeNamespace(ns); err == nil {
			t.Errorf("validateSpokeNamespace(%q) = nil, want error", ns)
		}
	}
}

// collectOne must fail the spoke rather than spawn kubectl exec when the
// derived namespace escapes the spoke tree.
func TestCollectOneRejectsOutOfTreeNamespace(t *testing.T) {
	called := false
	restore := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		called = true
		return restore(ctx, name, args...)
	}
	defer func() { execCommandContext = restore }()

	k := KubectlSpokeCollector{}
	sc := k.collectOne(ClusterTarget{ID: "hub"}, "../kube-system",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if called {
		t.Fatal("collectOne spawned kubectl for an out-of-tree namespace")
	}
	if !strings.Contains(sc.Err, "does not match expected prefix") {
		t.Fatalf("sc.Err = %q, want namespace-prefix rejection", sc.Err)
	}
}
