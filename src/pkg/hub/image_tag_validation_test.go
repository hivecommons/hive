package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateImageTagRefusesMalformed(t *testing.T) {
	// Every one of these would have been written straight onto a live
	// Deployment before this change, stranding the component on
	// ImagePullBackOff behind a still-serving old ReplicaSet.
	bad := []struct {
		tag string
		why string
	}{
		{"target1", "the literal value from the 2026-07-27 hub outage: a test fixture SHA, not a tag"},
		{"", "empty"},
		{"$target", "unsubstituted shell variable"},
		{"${target}", "unsubstituted shell variable with braces"},
		{"$target1", "unsubstituted shell variable with index"},
		{"target", "bare identifier"},
		{"latest", "not a channel tag we publish per-branch"},
		{"abc", "hex but too short to be a git SHA"},
		{"g45d2e9", "'g' is not a hex digit"},
		{"F45D2E9", "uppercase hex is not how git prints short SHAs"},
		{"  f45d2e9", "leading whitespace"},
		{"f45d2e9\n", "trailing newline"},
		{"-latest", "channel tag with no branch part"},
		{strings.Repeat("a", maxImageTagLen+1), "over the OCI tag length limit"},
	}
	for _, tc := range bad {
		if err := validateImageTag(tc.tag); err == nil {
			t.Errorf("validateImageTag(%q) = nil, want error (%s)", tc.tag, tc.why)
		} else if !strings.Contains(err.Error(), "refusing to patch") {
			t.Errorf("validateImageTag(%q) error = %q, want an actionable 'refusing to patch' message", tc.tag, err)
		}
	}
}

func TestValidateImageTagAcceptsValid(t *testing.T) {
	good := []string{
		"f45d2e9",  // canonical 7-char short SHA (the manual recovery tag)
		"438a9f2b", // 8-char abbreviation
		"f45d2e9b1c3a4e5f6789012345678901234567ab", // full 40-char SHA
		"v2-latest",
		"v3-latest",
		"mk-latest",
		"dd-latest",
		"feat-vanity-url-latest", // branchToTag("feat/vanity-url") + "-latest"
		// Release channels ARE the tag verbatim — the regression behind the
		// live "branch does not map to a valid image tag" toast on channel
		// switches: upgradeTargetTag returned "stable" and this validator,
		// which predates channels, refused it.
		"stable",
		"candidate",
		"edge",
	}
	for _, tag := range good {
		if err := validateImageTag(tag); err != nil {
			t.Errorf("validateImageTag(%q) = %v, want nil", tag, err)
		}
	}
}

func TestValidateImageRef(t *testing.T) {
	if err := validateImageRef("ghcr.io/hivecommons/hive-hub:target1"); err == nil {
		t.Error("validateImageRef accepted the outage image reference, want refusal")
	}
	if err := validateImageRef("ghcr.io/hivecommons/hive-hub:f45d2e9"); err != nil {
		t.Errorf("validateImageRef(valid SHA ref) = %v, want nil", err)
	}
	if err := validateImageRef("ghcr.io/hivecommons/hive:v2-latest"); err != nil {
		t.Errorf("validateImageRef(valid channel ref) = %v, want nil", err)
	}
	// A registry port must not be mistaken for a tag.
	if err := validateImageRef("registry.local:5000/kubestellar/hive"); err == nil {
		t.Error("validateImageRef accepted an untagged image, want refusal")
	}
	// Digest pins are already fully resolved.
	if err := validateImageRef("ghcr.io/hivecommons/hive-hub@sha256:" + strings.Repeat("a", 64)); err != nil {
		t.Errorf("validateImageRef(digest pin) = %v, want nil", err)
	}
	if err := validateImageRef(""); err == nil {
		t.Error("validateImageRef(\"\") = nil, want error")
	}
}

// TestRolloutHubToSHARefusesBogusTagWithoutPatching is the regression test for
// the outage: a malformed target must be refused BEFORE kubectl runs, and must
// leave the deployment untouched.
func TestRolloutHubToSHARefusesBogusTagWithoutPatching(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Record every kubectl invocation so we can assert none happened.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logPath + "\nexit 0\n"
	kubectlPath := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(kubectlPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// If validation were bypassed, hubImageExists would be consulted next —
	// make it loudly fail the test rather than silently allow the patch.
	oldImgCheck := hubImageExists
	hubImageExists = func(sha string, _ *slog.Logger) bool {
		t.Errorf("hubImageExists called for %q — validation should have refused first", sha)
		return true
	}
	t.Cleanup(func() { hubImageExists = oldImgCheck })

	s := &HubServer{
		logger:       slog.Default(),
		hubSecret:    testHubSecret,
		hubGitBranch: "v2",
		clusters:     map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID, InCluster: true}},
	}

	for _, bogus := range []string{"target1", "$target", "target"} {
		err := s.rolloutHubToSHA(bogus)
		if err == nil {
			t.Fatalf("rolloutHubToSHA(%q) = nil, want refusal", bogus)
		}
		if !strings.Contains(err.Error(), "refusing to patch") {
			t.Errorf("rolloutHubToSHA(%q) error = %q, want an actionable refusal", bogus, err)
		}
		// The refusal must be visible in hub status, not just in logs.
		if fault := s.HubUpgradeFault(); !strings.Contains(fault, bogus) {
			t.Errorf("HubUpgradeFault() = %q, want it to name the refused tag %q", fault, bogus)
		}
	}

	// Nothing may have reached kubectl.
	if data, err := os.ReadFile(logPath); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		t.Errorf("kubectl was invoked during a refused rollout:\n%s", data)
	}
}

// TestHubUpgradeStateSurfacesFault asserts a refused/stuck upgrade shows as
// "failed" rather than a reassuring "queued".
func TestHubUpgradeStateSurfacesFault(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	setV2Latest(t, "f45d2e9")

	s := &HubServer{
		logger:       slog.Default(),
		hubSecret:    testHubSecret,
		hubGitBranch: "v2",
		hubGitHash:   "0000000", // behind latest
		clusters:     map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID, InCluster: true}},
	}
	if got := s.hubUpgradeState(); got == hubUpgradeStateFailed {
		t.Fatalf("hubUpgradeState() = %q before any fault, want a non-failed state", got)
	}
	s.setHubUpgradeFault("refused invalid image tag \"target1\"")
	if got := s.hubUpgradeState(); got != hubUpgradeStateFailed {
		t.Errorf("hubUpgradeState() = %q after a fault, want %q", got, hubUpgradeStateFailed)
	}
}

// TestSwitchImageSelfRefusesBogusTag covers the SHARED spoke helper. This is the
// path that would strand spokes, which is worse than the hub outage: a spoke
// stuck on an old ReplicaSet keeps reporting its OLD git hash, so the hub
// re-sends the same doomed target on every heartbeat, forever.
func TestSwitchImageSelfRefusesBogusTag(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/hivecommons/hive:target1",
		"ghcr.io/hivecommons/hive:$target",
		"ghcr.io/hivecommons/hive:",
		"",
	} {
		err := SwitchImageSelf(slog.Default(), image)
		if err == nil {
			t.Errorf("SwitchImageSelf(%q) = nil, want refusal", image)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to patch") {
			t.Errorf("SwitchImageSelf(%q) error = %q, want an actionable refusal", image, err)
		}
	}
}

// TestHandleSwitchBranchRefusesBogusTag asserts the hub-side spoke writer
// refuses a branch that does not sanitize into a valid channel tag.
func TestHandleSwitchBranchRefusesBogusTag(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")

	h := &SaaSHive{ID: "h1", Owner: "alice"}
	saveSaaSHive(h)

	s := &HubServer{
		logger:             slog.Default(),
		hubSecret:          testHubSecret,
		saveCh:             make(chan struct{}, 1),
		heartbeatUpgrade:   make(map[string]string),
		heartbeatSwitchTag: make(map[string]string),
		clusters:           map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID, InCluster: true}},
	}

	// A branch whose sanitized tag starts with '-' cannot form a valid channel
	// tag. It must be rejected, not written to the spoke's Deployment.
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodPost, "/api/saas/hives/h1/switch-branch", `{"branch":"-bad"}`, "alice")
	req.SetPathValue("id", "h1")
	s.handleSwitchBranch(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("handleSwitchBranch accepted a branch that maps to an invalid tag: %s", rec.Body.String())
	}
}
