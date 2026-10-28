package github

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

func TestSupersessionCloseDecisionIsExplicit(t *testing.T) {
	for _, text := range []string{"reject", "close superseded by #22"} {
		cmd, ok := parseUnparkCommand("/hive decision " + text)
		if !ok {
			t.Fatal(text)
		}
		if _, ok := supersessionCloseDecision(cmd); !ok {
			t.Fatal(text)
		}
	}
	for _, text := range []string{"reject the old design and fix it", "close superseded by #0", "close superseded by #22\nkeep tests", "approve"} {
		cmd, _ := parseUnparkCommand("/hive decision " + text)
		if _, ok := supersessionCloseDecision(cmd); ok {
			t.Fatalf("treated instruction as close: %q", text)
		}
	}
}

func TestContributorSupersessionDecisionClosesWithoutPush(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		requested   int
		labels      []string
		closes      bool
	}{
		{name: "reject", state: "closed", closes: true},
		{name: "explicit closer", state: "closed", requested: 22, closes: true},
		{name: "wrong closer", state: "closed", requested: 23},
		{name: "reopened issue", state: "open"},
		{name: "held", state: "closed", labels: []string{"hold"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prs := autoClosePRs(supersessionPRFixture{Body: "Fixes #3", Labels: tc.labels})
			server, obs := supersessionSweepServer(t, autoCloseOrg, autoCloseRepo, prs, []supersessionIssueFixture{{Number: 3, State: tc.state, GraphQLCloserPR: 22}})
			defer server.Close()
			c := newTestClient(t, server, autoCloseOrg, []string{autoCloseRepo})
			c.SetAppBotLogin(autoCloseBot)
			pr := &gh.PullRequest{Number: gh.Ptr(12), Body: gh.Ptr("Fixes #3"), User: &gh.User{Login: gh.Ptr("contributor")}}
			for _, l := range tc.labels {
				pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr(l)})
			}
			reply, err := c.closeSupersededPRDecision(context.Background(), autoCloseOrg+"/"+autoCloseRepo, autoCloseOrg, autoCloseRepo, pr, "maintainer", tc.requested)
			if err != nil {
				t.Fatal(err)
			}
			if (len(obs.closed) == 1) != tc.closes {
				t.Fatalf("closed=%v reply=%s", obs.closed, reply)
			}
			if tc.closes && !strings.Contains(reply, "#22") {
				t.Fatal(reply)
			}
			if len(obs.updated) != 0 || len(obs.bodies) != 0 {
				t.Fatalf("contributor branch mutated: %+v", obs)
			}
		})
	}
}

func TestSupersessionSupplementalFilesNeedImplementationOverlap(t *testing.T) {
	closer := map[string]bool{"pkg/fix.go": true}
	for _, tc := range []struct {
		name string
		open map[string]bool
		want int
	}{
		{"ancillary only extras", map[string]bool{"pkg/fix.go": true, "pkg/fix_test.go": true, "src/docs/fix.md": true, "changelog.d/fixed-fix.md": true}, 0},
		{"unrelated implementation", map[string]bool{"pkg/other.go": true, "pkg/other_test.go": true}, 2},
		{"extra implementation", map[string]bool{"pkg/fix.go": true, "pkg/other.go": true, "pkg/fix_test.go": true}, 1},
		{"docs only", map[string]bool{"docs/fix.md": true}, 1},
	} {
		if got := supersessionBlockingExtraFiles(tc.open, closer); len(got) != tc.want {
			t.Fatalf("%s: %v", tc.name, got)
		}
	}
}

func TestUntrustedSupersessionRejectCannotCloseContributorPR(t *testing.T) {
	issue := parkedPR(12, "contributor", []string{PRNeedsDecisionStageLabel}, humanComment(4242, "reader", "/hive decision reject"))
	c, rec := newUnparkPRClient(t, []unparkWireIssue{issue}, map[string]string{"reader": "read"})
	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{TrustedMerger: trustedMergerSet("reader")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Replies != 0 || len(rec.posted[12]) != 0 || len(rec.patched[12]) != 0 || len(rec.added[12]) != 0 {
		t.Fatalf("unauthorized command acted: %+v %+v", result, rec)
	}
}

func TestHeldSupersessionRemainsObservableWithoutWrites(t *testing.T) {
	pr := supersessionPRFixture{Body: "Fixes #3", Labels: []string{"sentinel-alert"}}
	server, obs := supersessionSweepServer(t, autoCloseOrg, autoCloseRepo, autoClosePRs(pr), []supersessionIssueFixture{{Number: 3, State: "closed", GraphQLCloserPR: 22}})
	defer server.Close()
	c := newTestClient(t, server, autoCloseOrg, []string{autoCloseRepo})
	c.SetAppBotLogin(autoCloseBot)
	c.SetHoldLabels([]string{"sentinel-alert"})
	var events []SupersessionSweepEvent
	opts := autoCloseEnabled
	opts.Audit = func(e SupersessionSweepEvent) { events = append(events, e) }
	if _, err := c.SweepSupersededOpenPRs(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].CloserPR != 22 || events[0].Action != SupersessionActionDetectedContributor {
		t.Fatalf("events=%+v", events)
	}
	if len(obs.closed) != 0 || len(obs.comments[12]) != 0 || len(obs.labels[12]) != 0 {
		t.Fatalf("hold bypassed: %+v", obs)
	}
}

func TestSupplementalFilesPermitGraceCloseButNotLargePRClose(t *testing.T) {
	body := graceNoticeBody(t)
	old := time.Now().Add(-25 * time.Hour)
	for _, size := range []string{"size/S", "size/L"} {
		res, obs := runAutoCloseSweep(t, supersessionPRFixture{Body: "Fixes #3", Labels: []string{size}, Files: []string{"pkg/a.go", "pkg/a_test.go", "changelog.d/fixed-a.md"}, Comments: []supersessionCommentFixture{noticeComment(body, old)}}, autoCloseEnabled)
		if size == "size/S" {
			if len(obs.closed) != 1 {
				t.Fatalf("ancillary files blocked closure: %+v", res)
			}
		} else if len(obs.closed) != 0 || len(res.Commented) != 1 || res.Commented[0].Action != SupersessionActionEscalated {
			t.Fatalf("large PR must need a decision: %+v closed=%v", res, obs.closed)
		}
	}
}

func TestReopenedIssueClearsSupersessionCauseWithoutClearingHumanPark(t *testing.T) {
	pr := supersessionPRFixture{Body: "Fixes #3", Labels: []string{issueNeedsHumanLabel, prstage.SupersessionPendingLabel}}
	server, obs := supersessionSweepServer(t, autoCloseOrg, autoCloseRepo, autoClosePRs(pr), []supersessionIssueFixture{{Number: 3, State: "open"}})
	defer server.Close()
	c := newTestClient(t, server, autoCloseOrg, []string{autoCloseRepo})
	c.SetAppBotLogin(autoCloseBot)
	result, err := c.SweepSupersededOpenPRs(context.Background(), autoCloseEnabled)
	if err != nil {
		t.Fatal(err)
	}
	removed := obs.removedLabels[12]
	if len(removed) != 1 || removed[0] != prstage.SupersessionPendingLabel || len(result.Resolved) != 1 {
		t.Fatalf("removed=%v result=%+v", removed, result)
	}
}
