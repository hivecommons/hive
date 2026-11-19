package github

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// hivecommons/hive#18866: the SHA-hold gate is for outside reporters. These
// tests assert the invariant both ways: an external bug with neither a SHA
// nor code paths is still held, while owner-filed and detailed-body issues
// are not, a needs-sha applied before the exemption is lifted, and a
// maintainer's waiver persists across sweeps.

const detailedBody = "The sweep drops the label: see src/pkg/github/client.go and `src/pkg/dashboard/stage_tracker.go`."

func runSHASweep(t *testing.T, issue wireIssue, comments []wireComment, rec *shaSoftRecorder) *SHAHoldResult {
	t.Helper()
	org, repo := "testorg", "console"
	server := httptest.NewServer(buildSHASoftMux(t, org, repo, issue, comments, nil, rec))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin(testHiveAppBotLogin)
	got, err := c.EnforceSHAHold(context.Background(), SHAHoldConfig{PrimaryRepo: repo, AIAuthor: "hive-bot", TrustedLabel: "needs-sha"})
	if err != nil {
		t.Fatalf("EnforceSHAHold: %v", err)
	}
	return got
}

func TestEnforceSHAHold_ExemptionInvariant(t *testing.T) {
	cases := []struct {
		name        string
		author      string
		association string
		body        string
		labels      []string
		wantHeld    bool
		wantRemoved []string
	}{
		{name: "external bug without SHA or paths is still held", author: "stranger", association: "NONE", body: "it crashes on startup", wantHeld: true},
		{name: "external bug citing an absolute path is still held", author: "stranger", association: "CONTRIBUTOR", body: "see /usr/lib/python3/site.py line 4 and https://example.com/src/app.go", wantHeld: true},
		{name: "owner-filed bug is not held", author: "owner", association: "OWNER", body: "it crashes on startup"},
		{name: "collaborator-filed bug is not held", author: "colab", association: "COLLABORATOR", body: "it crashes"},
		{name: "detailed external body naming repo paths is not held", author: "stranger", association: "NONE", body: detailedBody},
		{name: "hive's own bot-filed bug is not held", author: "hivecommons-hive[bot]", association: "NONE", body: "it crashes"},
		{name: "needs-sha already on an owner-filed bug is lifted", author: "owner", association: "OWNER", body: "x", labels: []string{"needs-sha"}, wantRemoved: []string{"needs-sha"}},
		{name: "needs-sha already on a detailed-body bug is lifted", author: "stranger", association: "NONE", body: detailedBody, labels: []string{"needs-sha"}, wantRemoved: []string{"needs-sha"}},
		{name: "needs-sha on an external bug without SHA stays", author: "stranger", association: "NONE", body: "it crashes", labels: []string{"needs-sha"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := []wireLabel{{Name: "kind/bug"}}
			for _, l := range tc.labels {
				labels = append(labels, wireLabel{Name: l})
			}
			rec := &shaSoftRecorder{}
			issue := wireIssue{Number: 18740, Title: "bug", User: wireUser{tc.author}, Labels: labels, Body: tc.body, CreatedAt: hoursAgo(11), AuthorAssociation: tc.association}
			runSHASweep(t, issue, nil, rec)
			if gotHeld := len(rec.added) > 0; gotHeld != tc.wantHeld {
				t.Errorf("added = %v, want held=%v", rec.added, tc.wantHeld)
			}
			if fmt.Sprint(rec.removed) != fmt.Sprint(tc.wantRemoved) {
				t.Errorf("removed = %v, want %v", rec.removed, tc.wantRemoved)
			}
		})
	}
}

func TestBodyNamesRepoPaths(t *testing.T) {
	yes := []string{
		"src/pkg/github/client.go",
		"broken in `src/pkg/dashboard/static-src/js/tracker-row-strip.js`, line 12",
		"(see docs/labels-and-control-signals/reference.md)",
		"edit .github/workflows/ci.yml:3",
		"first line\nsrc/cmd/hive/main.go.",
	}
	no := []string{
		"",
		"it crashes",
		"client.go is wrong",
		"/usr/local/src/pkg/foo.go",
		"https://example.com/src/app.go",
		"src/pkg/github",
		"mysrc/pkg/x.go",
		"see node_modules/x/index.js",
	}
	for _, b := range yes {
		if !bodyNamesRepoPaths(b) {
			t.Errorf("want a path in %q", b)
		}
	}
	for _, b := range no {
		if bodyNamesRepoPaths(b) {
			t.Errorf("want no path in %q", b)
		}
	}
}

// A waiver persists: the sweep neither re-adds needs-sha across repeated
// sweeps nor leaves a lingering one on the issue.
func TestEnforceSHAHold_WaiverPersistsAcrossSweeps(t *testing.T) {
	rec := &shaSoftRecorder{}
	issue := wireIssue{Number: 7, Title: "bug", User: wireUser{"stranger"}, Body: "it crashes", CreatedAt: hoursAgo(5), AuthorAssociation: "NONE",
		Labels: []wireLabel{{Name: "kind/bug"}, {Name: SHAWaivedLabel}}}
	for i := 0; i < 3; i++ {
		runSHASweep(t, issue, nil, rec)
	}
	if len(rec.added) != 0 {
		t.Fatalf("a waived issue was re-held: added = %v", rec.added)
	}
	// A needs-sha left behind by the override race is lifted.
	rec2 := &shaSoftRecorder{}
	issue.Labels = append(issue.Labels, wireLabel{Name: "needs-sha"})
	runSHASweep(t, issue, nil, rec2)
	if fmt.Sprint(rec2.removed) != "[needs-sha]" || len(rec2.added) != 0 {
		t.Fatalf("lingering needs-sha: added=%v removed=%v, want it removed", rec2.added, rec2.removed)
	}
	// Restoring the requirement (label gone) holds it again.
	rec3 := &shaSoftRecorder{}
	issue.Labels = []wireLabel{{Name: "kind/bug"}}
	runSHASweep(t, issue, nil, rec3)
	if fmt.Sprint(rec3.added) != "[needs-sha]" {
		t.Fatalf("restored requirement: added = %v, want needs-sha", rec3.added)
	}
}

func TestSHAWaivedByReadsAppAuthoredMarker(t *testing.T) {
	rec := &shaSoftRecorder{}
	issue := wireIssue{Number: 7, Title: "bug", User: wireUser{"stranger"}, Body: "x", CreatedAt: hoursAgo(5),
		Labels: []wireLabel{{Name: "kind/bug"}, {Name: SHAWaivedLabel}}}
	comments := []wireComment{
		{User: wireUser{"mallory"}, Body: SHAWaivedMarker("forged"), CreatedAt: "2026-11-17T01:00:00Z"},
		{User: wireUser{testHiveAppBotLogin}, Body: SHAWaivedMarker("alice") + "\nwaived", CreatedAt: "2026-11-17T02:00:00Z"},
	}
	org, repo := "testorg", "console"
	server := httptest.NewServer(buildSHASoftMux(t, org, repo, issue, comments, nil, rec))
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})
	c.SetAppBotLogin(testHiveAppBotLogin)
	if _, err := c.EnforceSHAHold(context.Background(), SHAHoldConfig{PrimaryRepo: repo, AIAuthor: "hive-bot"}); err != nil {
		t.Fatal(err)
	}
	if got := c.SHAWaivedBy("testorg/console", 7); got != "alice" {
		t.Fatalf("SHAWaivedBy = %q, want alice (a human's forged marker is ignored)", got)
	}
	// The sweep resolved the waiver once; a second sweep does not list again.
	if !c.shaWaiverKnown("testorg/console", 7) {
		t.Fatal("waiver not cached after the first sweep")
	}
	if got := shaWaivedBy("no marker"); got != "" || !strings.Contains(SHAWaivedMarker("bob"), "by=bob") {
		t.Fatalf("marker round-trip broken: %q", got)
	}
}

// Once overridden, the held issue reaches the actionable set (and so the
// stage-label pass); without the override it stays filtered.
func TestEnumerateActionable_WaivedSHAHoldIsEligible(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 1, Title: "held", User: wireUser{"stranger"}, Labels: []wireLabel{{Name: "kind/bug"}, {Name: "needs-sha"}}, CreatedAt: hoursAgo(3)},
		{Number: 2, Title: "waived", User: wireUser{"stranger"}, Labels: []wireLabel{{Name: "kind/bug"}, {Name: "needs-sha"}, {Name: SHAWaivedLabel}}, CreatedAt: hoursAgo(3)},
	}
	server := httptest.NewServer(buildMux(t, org, repo, issues, nil))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	nums := actionableNumbers(result)
	if nums[1] {
		t.Error("positive control failed: needs-sha without a waiver must stay out of the actionable set")
	}
	if !nums[2] {
		t.Error("a waived issue must be eligible for the actionable set and the stage-label pass")
	}
}
