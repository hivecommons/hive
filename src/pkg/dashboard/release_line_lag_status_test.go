package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// buildReleaseLineStatus drives the REAL dashboard entry point
// (BuildFrontendStatus) so the test catches a regression in whether the call
// site actually wires ReleaseLineLag into the payload — not merely whether the
// helper returns the right shape.
func buildReleaseLineStatus(t *testing.T) *StatusPayload {
	t.Helper()
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}},
		GitHub:  config.GitHubConfig{Token: "tok"},
	}
	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	statuses := map[string]*agent.AgentProcess{}
	return BuildFrontendStatus(gov.GetState(), nil, statuses, cfg, nil, gov, nil, nil, nil, nil)
}

// A wired provider's exceeded-drift signal must reach the status payload
// through BuildFrontendStatus verbatim (#6960).
func TestBuildFrontendStatusSurfacesReleaseLineLag(t *testing.T) {
	prev := getReleaseLineLagFn()
	t.Cleanup(func() { SetReleaseLineLagProvider(prev) })

	SetReleaseLineLagProvider(func() *FrontendReleaseLineLag {
		return &FrontendReleaseLineLag{
			EdgeBranch:   "v5",
			StableBranch: "v4",
			EdgeSHA:      "v5aaaaa",
			StableSHA:    "v4bbbbb",
			BehindBy:     27,
			Known:        true,
			Threshold:    5,
			Exceeded:     true,
		}
	})

	payload := buildReleaseLineStatus(t)
	if payload.ReleaseLineLag == nil {
		t.Fatal("BuildFrontendStatus dropped ReleaseLineLag — the drift surface never reaches the dashboard")
	}
	lag := payload.ReleaseLineLag
	if !lag.Known || lag.BehindBy != 27 || !lag.Exceeded {
		t.Errorf("ReleaseLineLag = %+v, want known 27-behind exceeded=true", lag)
	}
	if lag.EdgeBranch != "v5" || lag.StableBranch != "v4" {
		t.Errorf("branches = %s behind %s, want v5 behind v4", lag.EdgeBranch, lag.StableBranch)
	}
}

// With NO provider wired, the payload must still carry an UNKNOWN lag — never
// nil-that-reads-as-healthy and never a healthy zero. This is the fail-open
// guard #6960 exists to close.
func TestBuildFrontendStatusReleaseLineLagUnknownWithoutProvider(t *testing.T) {
	prev := getReleaseLineLagFn()
	t.Cleanup(func() { SetReleaseLineLagProvider(prev) })
	SetReleaseLineLagProvider(nil)

	payload := buildReleaseLineStatus(t)
	if payload.ReleaseLineLag == nil {
		t.Fatal("ReleaseLineLag must be present even with no provider, so the UI shows 'unknown'")
	}
	if payload.ReleaseLineLag.Known {
		t.Errorf("no provider must render unknown, got Known=true: %+v", payload.ReleaseLineLag)
	}
	if payload.ReleaseLineLag.Exceeded {
		t.Error("an unknown lag must never be Exceeded")
	}
	if payload.ReleaseLineLag.EdgeBranch != releaseLineEdgeBranch ||
		payload.ReleaseLineLag.StableBranch != releaseLineStableBranch {
		t.Errorf("nil-provider fallback must still name the lines, got %+v", payload.ReleaseLineLag)
	}
}
