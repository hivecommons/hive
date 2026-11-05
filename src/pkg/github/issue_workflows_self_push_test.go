package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	itemblocker "github.com/hivecommons/hive/pkg/blocker"
)

// projectbluefin/testsuite#1059, scanner (ISSUES_PRS_MERGE, merger tier) on
// 2026-11-01 05:09Z. The App pushed .github/workflows/e2e.yml in #1089 the
// same day.
const selfPush1059 = "## Workflow change needed (the hive app cannot push `.github/workflows/`)\n\n" +
	"A test-side PR on `scanner/fix-1059` fixes most of the 16 failures. One related cause is in `e2e.yml`, and the hive app can't push it.\n\n" +
	"**Proposed diff** (`.github/workflows/e2e.yml`, \"Install gnome-ponytail-daemon\" step): run the daemon under `systemd-run --user`."

// stubInstallation swaps the installation read and counts calls.
func stubInstallation(t *testing.T, perms map[string]string, err error) *int {
	t.Helper()
	calls := 0
	prev := installationProbe
	installationProbe = func(context.Context, *AppAuth) (*InstallationInfo, error) {
		calls++
		if err != nil {
			return nil, err
		}
		return &InstallationInfo{Permissions: perms}, nil
	}
	t.Cleanup(func() { installationProbe = prev })
	return &calls
}

// fleet installs the two lookups the relay asks: scanner is the merger-tier
// lane, and it serves the repo only when lane is true.
func fleet(appAuth, lane bool) func(*Client) {
	return func(c *Client) {
		if appAuth {
			c.appAuth = &AppAuth{}
		}
		c.SetWorkflowsSelfPushFunc(func(agent string) bool { return agent == "scanner" })
		c.SetWorkflowsLaneFunc(func(agent, repo string) string {
			if !lane || agent == "scanner" {
				return ""
			}
			return "scanner"
		})
	}
}

// agent tier × installation grant × capable lane: a merger-tier lane is
// refused the park when the installation grants `workflows: write`; a
// contributor-tier lane hands it to the capable lane unless the installation
// is known to lack the permission; everything else parks as before.
func TestWorkflowsParkCheckedAgainstTierAndInstallation(t *testing.T) {
	const (
		self    = "comment"
		handoff = "label:" + WorkflowsLaneLabel + ",comment"
		park    = "label:owner-call,comment"
	)
	write := map[string]string{"workflows": "write"}
	read := map[string]string{"workflows": "read"}
	for _, tc := range []struct {
		name    string
		agent   string
		appAuth bool
		lane    bool
		perms   map[string]string
		err     error
		body    string
		want    string
	}{
		{name: "merger tier, installation grants: back to the agent", agent: "scanner", appAuth: true, lane: true, perms: write, want: self},
		{name: "merger tier, no other lane needed", agent: "scanner", appAuth: true, lane: false, perms: write, want: self},
		{name: "merger tier, installation has read only: parks", agent: "scanner", appAuth: true, lane: true, perms: read, want: park},
		{name: "merger tier, installation read fails: parks", agent: "scanner", appAuth: true, lane: true, err: errors.New("502"), want: park},
		{name: "merger tier, token-authenticated client: parks", agent: "scanner", appAuth: false, lane: true, perms: write, want: park},
		{name: "merger tier quoting GitHub's rejection: parks", agent: "scanner", appAuth: true, lane: true, perms: write, body: selfPush1059 + "\n\nThe push bounced: `! [remote rejected] scanner/fix-1059 (refusing to allow a GitHub App to create or update workflow `.github/workflows/e2e.yml` without `workflows` permission)`", want: park},
		{name: "contributor tier, installation grants: hand-off", agent: "ci-maintainer", appAuth: true, lane: true, perms: write, want: handoff},
		{name: "contributor tier, installation lacks it: parks", agent: "ci-maintainer", appAuth: true, lane: true, perms: read, want: park},
		{name: "contributor tier, installation unknown: hand-off as before", agent: "ci-maintainer", appAuth: true, lane: true, err: errors.New("502"), want: handoff},
		{name: "contributor tier, no capable lane: parks", agent: "ci-maintainer", appAuth: true, lane: false, perms: write, want: park},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubInstallation(t, tc.perms, tc.err)
			body := tc.body
			if body == "" {
				body = selfPush1059
			}
			writes, bodies, resp := runNeedsDecisionCommentCapture(t, needsDecisionFilter{label: "owner-call"}, IssueRequest{
				Kind: "comment", Repo: "o/r", Number: 1059, Agent: tc.agent, NeedsDecision: true, Body: body,
			}, false, fleet(tc.appAuth, tc.lane))
			if got := strings.Join(writes, ","); got != tc.want {
				t.Fatalf("writes = %q, want %q", got, tc.want)
			}
			if !resp.OK || len(bodies) != 1 {
				t.Fatalf("result = %+v bodies = %v", resp, bodies)
			}
			selfNote := strings.Contains(bodies[0], "Not parked: `scanner`'s token can push")
			if selfNote != (tc.want == self) {
				t.Fatalf("self-push note present = %v, want %v:\n%s", selfNote, tc.want == self, bodies[0])
			}
			if tc.want != park && strings.Contains(bodies[0], unparkOwnerStepMarker) {
				t.Fatalf("an unparked comment must not carry the owner-step marker:\n%s", bodies[0])
			}
		})
	}
}

// A burst of parks costs one installation read.
func TestInstallationGrantIsCached(t *testing.T) {
	calls := stubInstallation(t, map[string]string{"workflows": "write"}, nil)
	c := &Client{appAuth: &AppAuth{}}
	for i := 0; i < 3; i++ {
		if granted, known := c.installationGrantsWorkflows(context.Background()); !granted || !known {
			t.Fatalf("granted=%v known=%v", granted, known)
		}
	}
	if *calls != 1 {
		t.Fatalf("installation reads = %d, want 1", *calls)
	}
	c.workflowsGrant.at = time.Now().Add(-workflowsGrantTTL - time.Second)
	c.installationGrantsWorkflows(context.Background())
	if *calls != 2 {
		t.Fatalf("installation reads after the TTL = %d, want 2", *calls)
	}
}

// The scheduled pass lifts a recorded workflows blocker once the hive can
// push workflow files on that repo, and routes the item to the capable lane.
// A repo no capable lane serves keeps its blocker.
func TestSweepWorkflowsBlockersReleasesGrantedBlockers(t *testing.T) {
	stubInstallation(t, map[string]string{"workflows": "write"}, nil)
	l := itemblocker.NewLedger("")
	prev := itemblocker.SetShared(l)
	t.Cleanup(func() { itemblocker.SetShared(prev) })
	since := time.Date(2026, 11, 1, 5, 9, 13, 0, time.UTC)
	l.Record("o/testsuite", 1059, itemblocker.Blocker{Kind: itemblocker.CapabilityWorkflowsPermission, Since: since}, "the hive app cannot push .github/workflows/")
	l.Record("o/other", 7, itemblocker.Blocker{Kind: itemblocker.CapabilityWorkflowsPermission, Since: since}, "the hive app cannot push .github/workflows/")
	l.Record("o/testsuite", 1073, itemblocker.Blocker{Kind: itemblocker.HumanOnlyCLA, Since: since}, "sign the CLA")
	l.Record("o/testsuite", 1080, itemblocker.Blocker{Kind: itemblocker.CapabilityBranchProtection, Since: since}, "cannot edit the ruleset")
	// testsuite#1037's shape: a fork PR, whose branch takes no workflow push
	// from the App whatever the grant.
	l.Record("o/testsuite", 1037, itemblocker.Blocker{Kind: itemblocker.CapabilityWorkflowsPermission, Since: since}, "the fixer's push was refused")
	// A same-repo PR is pushable like an issue.
	l.Record("o/testsuite", 1089, itemblocker.Blocker{Kind: itemblocker.CapabilityWorkflowsPermission, Since: since}, "the hive app cannot push .github/workflows/")

	var mu sync.Mutex
	var labelled []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/testsuite/issues/1059":
			_, _ = w.Write([]byte(`{"number":1059}`))
			return
		case "/repos/o/testsuite/issues/1037", "/repos/o/testsuite/issues/1089":
			_, _ = w.Write([]byte(`{"number":1,"pull_request":{"url":"x"}}`))
			return
		case "/repos/o/testsuite/pulls/1037":
			_, _ = w.Write([]byte(`{"number":1037,"head":{"repo":{"full_name":"arteeh/testsuite"}}}`))
			return
		case "/repos/o/testsuite/pulls/1089":
			_, _ = w.Write([]byte(`{"number":1089,"head":{"repo":{"full_name":"o/testsuite"}}}`))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels") {
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			labelled = append(labelled, r.URL.Path+"="+strings.Join(body, ","))
			mu.Unlock()
			_, _ = w.Write([]byte(`[{"name":"x"}]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appAuth = &AppAuth{}
	c.SetWorkflowsLaneFunc(func(agent, repo string) string {
		if repo == "o/testsuite" {
			return "scanner"
		}
		return ""
	})

	released, err := c.SweepWorkflowsBlockers(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got := []int{}
	for _, e := range released {
		got = append(got, e.Number)
	}
	if len(got) != 2 || got[0] != 1059 || got[1] != 1089 {
		t.Fatalf("released = %v, want o/testsuite#1059 (issue) and #1089 (same-repo PR)", got)
	}
	if _, blocked := l.Blocking("o/testsuite", 1037); !blocked {
		t.Fatal("a fork PR's workflows blocker must stay: the App cannot push workflow files to a fork")
	}
	if _, blocked := l.Blocking("o/testsuite", 1059); blocked {
		t.Fatal("#1059 still blocked")
	}
	if _, blocked := l.Blocking("o/other", 7); !blocked {
		t.Fatal("o/other#7 has no capable lane and must stay blocked")
	}
	if _, blocked := l.Blocking("o/testsuite", 1073); !blocked {
		t.Fatal("a human-only blocker is never re-checked away")
	}
	if _, blocked := l.Blocking("o/testsuite", 1080); !blocked {
		t.Fatal("a workflows grant must not release another capability's blocker")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(labelled) != 2 || labelled[0] != "/repos/o/testsuite/issues/1059/labels="+WorkflowsLaneLabel {
		t.Fatalf("labels written = %v", labelled)
	}

	// Once the hive can push, a fresh report of the same claim is a reason,
	// not a blocker.
	l.SetCapabilityCheck(c.CapabilityAvailable)
	if e, _ := l.Observe("o/testsuite", 1059, "needs-human: the hive app cannot push `.github/workflows/e2e.yml`", time.Now().Add(time.Hour)); e.Blocks() {
		t.Fatalf("re-report recorded a blocker: %+v", e)
	}
}

// A merger-tier lane filing a workflow-only issue gets it created unparked,
// actionable, with no routing label (it stays its own) and the note.
func TestCreateWorkflowsParkRefusedForMergerTier(t *testing.T) {
	stubInstallation(t, map[string]string{"workflows": "write"}, nil)
	var mu sync.Mutex
	var got []issueCreatePayload
	srv := newIssueCreateLabelServer(t, &mu, &got)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetAgentIssueDecisionPolicy("escalate")
	fleet(true, true)(c)
	dir := withIssueDir(t)
	if _, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] ponytail daemon dies with the SSH session",
		Body: selfPush1059, Labels: []string{"agent/scanner"}, Agent: "scanner", NeedsDecision: true,
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("creates = %d, want 1", len(got))
	}
	p := got[0]
	if hasLabelFold(p.Labels, NeedsDecisionStageLabel) || hasLabelFold(p.Labels, WorkflowsLaneLabel) || !hasLabelFold(p.Labels, "stage/010-actionable") {
		t.Fatalf("labels = %v, want actionable, not parked, not handed off", p.Labels)
	}
	if strings.Contains(p.Body, unparkOwnerStepMarker) || !strings.Contains(p.Body, "Not parked: `scanner`'s token can push") {
		t.Fatalf("body must carry the self-push note and no owner-step marker:\n%s", p.Body)
	}
}

func TestWorkflowsRejectionProvesByReporterTier(t *testing.T) {
	c := &Client{}
	c.SetWorkflowsSelfPushFunc(func(agent string) bool { return agent == "scanner" })
	quote := "refusing to allow a GitHub App to create or update workflow `.github/workflows/e2e.yml` without `workflows` permission"
	for text, want := range map[string]bool{
		quote + "\n\n— hive: agent=scanner backend=claude":                           true,
		quote + "\n\n— hive: agent=scanner backend=claude\n<!-- hive:owner-step -->": true,
		quote + "\n\n— hive: agent=ci-maintainer backend=claude":                     false,
		quote: true,
	} {
		if got := c.WorkflowsRejectionProves(text); got != want {
			t.Errorf("WorkflowsRejectionProves(%q) = %v, want %v", text, got, want)
		}
	}
}
