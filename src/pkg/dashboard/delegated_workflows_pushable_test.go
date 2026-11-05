package dashboard

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Danathar/goodreads-mcp#440: a sec-check issue said "the hive's token
// cannot change workflow files" (true of its own contributor-tier lane), and
// "Let hive decide" answered recommend-only `capability-gap`, though the App
// held `workflows: write` and the scanner lane had merged workflow PRs in
// that repository. The claim is a gap only when the hive cannot push
// workflow files there.
const issue440Body = "**Summary:** The nightly check holds an issue-writing key while it runs downloaded code.\n\n" +
	"**What I need from you:** Pick A, B or C. Option A needs a person to apply it, because the hive's token cannot change workflow files.\n\n" +
	"Option A: Move the scope to a `report` job.\n" +
	"Option B: Only take the token off disk.\n" +
	"Option C: Accept the risk.\n\n" +
	"Recommended: A, because it takes the write scope out of the job that runs downloaded code.\n"

func countingInstallationProbe(t *testing.T, perms map[string]string, err error) *int {
	t.Helper()
	calls := 0
	prev := installationPermissionProbe
	installationPermissionProbe = func(context.Context, *ghpkg.AppAuth) (*ghpkg.InstallationInfo, error) {
		calls++
		if err != nil {
			return nil, err
		}
		return &ghpkg.InstallationInfo{Permissions: perms}, nil
	}
	t.Cleanup(func() { installationPermissionProbe = prev })
	return &calls
}

func workflowsFleet(mergeLane bool) map[string]config.AgentConfig {
	agents := map[string]config.AgentConfig{"sec-check": {Mode: "ISSUES_AND_PRS", Enabled: true}}
	if mergeLane {
		agents["scanner"] = config.AgentConfig{Mode: "ISSUES_PRS_MERGE", Enabled: true}
	}
	return agents
}

func TestDelegatedWorkflowsClaimIsGapOnlyWhenHiveCannotPush(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mergeLane bool
		perms     map[string]string
		probeErr  error
		wantGap   bool
		wantProbe int
	}{
		{name: "App grants workflows and a merge lane serves the repo", mergeLane: true, perms: map[string]string{"workflows": "write"}, wantGap: false, wantProbe: 1},
		{name: "installation lacks workflows: write", mergeLane: true, perms: map[string]string{"workflows": "read"}, wantGap: true, wantProbe: 1},
		{name: "probe fails", mergeLane: true, probeErr: errors.New("no App auth"), wantGap: true, wantProbe: 1},
		{name: "no lane can push workflows: no probe", mergeLane: false, perms: map[string]string{"workflows": "write"}, wantGap: true, wantProbe: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := countingInstallationProbe(t, tc.perms, tc.probeErr)
			mock := &repoItemDecisionMock{issueTitle: "[sec-check] Nightly live-site check holds an issue-writing key", issueBody: issue440Body, issueLabels: []string{"security", "stage/045-needs-decision"}}
			srv := newDelegatedServer(t, mock)
			srv.deps.Config.Agents = workflowsFleet(tc.mergeLane)
			w := postDelegated(t, srv, 7, "writer", `{"type":"issue"}`, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			job := decodeDelegatedJob(t, w)
			if job.Decision == nil {
				t.Fatalf("no decision: %+v", job)
			}
			if gap := job.Decision.Rule == delegatedecision.RuleCapabilityGap; gap != tc.wantGap {
				t.Fatalf("capability gap = %v, want %v: %+v", gap, tc.wantGap, job.Decision)
			}
			if !tc.wantGap && job.Decision.KeepsPark {
				t.Fatalf("a pushable workflow change must be decided, not held: %+v", job.Decision)
			}
			if *calls != tc.wantProbe {
				t.Fatalf("probe calls = %d, want %d", *calls, tc.wantProbe)
			}
		})
	}
}

// The probe is a GitHub call: an item without capability prose never pays it.
func TestDelegatedWorkflowsProbeOnlyForCapabilityClaims(t *testing.T) {
	calls := countingInstallationProbe(t, map[string]string{"workflows": "write"}, nil)
	mock := &repoItemDecisionMock{issueTitle: "Pick a retry policy", issueBody: "Option A: retry twice\nOption B: never retry\n", issueLabels: []string{"stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)
	srv.deps.Config.Agents = workflowsFleet(true)
	if w := postDelegated(t, srv, 7, "writer", `{"type":"issue"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if *calls != 0 {
		t.Fatalf("probe calls = %d, want 0", *calls)
	}
}
