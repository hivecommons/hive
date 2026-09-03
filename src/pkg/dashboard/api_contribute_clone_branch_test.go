package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The onboarding page hardcoded `git clone -b v2`, so every contributor who
// copy-pasted it cloned a branch the hub is not built from
// (kubestellar/hive#3990). That is not a cosmetic drift: the v2 relay speaks
// contributor protocol 1.1 while the hub speaks 1.2, so the copied command
// produced a client the hub had to flag as a protocol-drifted peer.
//
// The clone branch must track the branch THIS hub was built from — not a
// constant, and deliberately not "whatever branch is newest upstream": a
// contributor's relay has to match the hub it is joining, which is the same
// reasoning pkg/hub's upgradeBranchOrDefault records for upgrade targets.

// withGitBranch sets the built-from branch for one test and restores it after,
// so these cases cannot leak into the rest of the package.
func withGitBranch(t *testing.T, branch string) {
	t.Helper()
	prev := versionBranch
	SetGitBranch(branch)
	t.Cleanup(func() { SetGitBranch(prev) })
}

func renderContributeLanding(t *testing.T) string {
	t.Helper()
	srv := newFullServer(t)
	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	rec := httptest.NewRecorder()
	srv.handleContributeLanding(rec, req)
	return rec.Body.String()
}

// TestContributeCloneBranchFollowsBuildBranch: a hub built from v4 must hand
// out `-b v4`, and must never emit the old hardcoded v2 or a raw sentinel.
func TestContributeCloneBranchFollowsBuildBranch(t *testing.T) {
	withGitBranch(t, "v4")
	html := renderContributeLanding(t)

	if !strings.Contains(html, "git clone -b v4 https://github.com/hivecommons/hive") {
		t.Error("onboarding page does not clone the branch the hub was built from (v4)")
	}
	if strings.Contains(html, "clone -b v2 ") {
		t.Error("onboarding page still hands out the unmaintained v2 branch")
	}
	if strings.Contains(html, "{{HIVE_BRANCH}}") {
		t.Error("unsubstituted {{HIVE_BRANCH}} sentinel leaked into the rendered page")
	}
}

// TestContributeCloneBranchTracksADifferentBranch pins that the value is really
// derived, not just re-hardcoded to v4: a hub built from another branch hands
// out that branch instead.
func TestContributeCloneBranchTracksADifferentBranch(t *testing.T) {
	withGitBranch(t, "v5-experimental")
	html := renderContributeLanding(t)

	if !strings.Contains(html, "git clone -b v5-experimental https://github.com/hivecommons/hive") {
		t.Error("clone branch did not follow the hub's own build branch")
	}
	if strings.Contains(html, "git clone -b v4 https://github.com/hivecommons/hive") {
		t.Error("clone branch is still pinned to v4 rather than derived")
	}
}

// TestContributeCloneBranchUnknownBuildFallsBack: a build with no injected
// branch (local `go run`, where gitBranch stays "unknown") must fall back to the
// maintained default rather than emitting "unknown" into a copy-paste command.
func TestContributeCloneBranchUnknownBuildFallsBack(t *testing.T) {
	for _, branch := range []string{"", "unknown"} {
		t.Run("branch="+branch, func(t *testing.T) {
			withGitBranch(t, branch)
			html := renderContributeLanding(t)

			if strings.Contains(html, "git clone -b unknown") || strings.Contains(html, "git clone -b  ") {
				t.Fatal("a build with no injected branch emitted an unusable clone command")
			}
			if !strings.Contains(html, "git clone -b "+defaultUpstreamBranch+" https://github.com/hivecommons/hive") {
				t.Errorf("fallback did not use defaultUpstreamBranch (%q)", defaultUpstreamBranch)
			}
		})
	}
}

// TestDefaultUpstreamBranchIsMaintained guards the constant itself. v2 is no
// longer maintained (see pkg/hub upgradeBranchOrDefault), so a fallback pointing
// at it is the exact stale-constant defect this fix removes.
func TestDefaultUpstreamBranchIsMaintained(t *testing.T) {
	if defaultUpstreamBranch == "v2" {
		t.Fatal("defaultUpstreamBranch is back to the unmaintained v2 branch")
	}
}

// TestContributeCloneBranchAppliesToEveryCopyPasteSurface: the page carries the
// clone line five times — the default <pre>, the container/host/k8s JS
// templates, and the prefilled assistant prompt. A fix that reached only the
// visible one would still mislead anyone who switched Mode or copied the prompt.
func TestContributeCloneBranchAppliesToEveryCopyPasteSurface(t *testing.T) {
	withGitBranch(t, "v4")
	html := renderContributeLanding(t)

	if got := strings.Count(html, "git clone -b v4 https://github.com/hivecommons/hive"); got != 5 {
		t.Errorf("clone command appears with the right branch %d times; want 5 (pre block, container/host/k8s templates, prompt)", got)
	}
}
