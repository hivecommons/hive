package github

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDecideHiveSupersededAutoClose pins the rule the hive applies to its
// own superseded PRs (hivecommons/hive#14965 → #14957 for #14952): every
// closing target closed by a different merged PR, and the head either
// conflicting or fully contained in the closer, closes without a person;
// anything a person wrote, held, parked or exempted keeps.
func TestDecideHiveSupersededAutoClose(t *testing.T) {
	base := HiveSupersededAutoCloseFacts{Enabled: true, HiveAuthored: true, Superseded: true, Level: acmmLevelAutonomous}
	with := func(mut func(*HiveSupersededAutoCloseFacts)) HiveSupersededAutoCloseFacts {
		f := base
		mut(&f)
		return f
	}
	cases := []struct {
		name        string
		facts       HiveSupersededAutoCloseFacts
		wantVerdict string
		wantReason  string
	}{
		{"disabled keeps", with(func(f *HiveSupersededAutoCloseFacts) { f.Enabled = false; f.Conflicting = true }), hiveSupersededVerdictKeep, hiveSupersededReasonDisabled},
		{"merged keeps", with(func(f *HiveSupersededAutoCloseFacts) { f.Merged = true; f.Conflicting = true }), hiveSupersededVerdictKeep, hiveSupersededReasonMerged},
		{"not superseded keeps", with(func(f *HiveSupersededAutoCloseFacts) { f.Superseded = false; f.Conflicting = true }), hiveSupersededVerdictKeep, hiveSupersededReasonNotSupers},
		{"human-authored gets a note", with(func(f *HiveSupersededAutoCloseFacts) { f.HiveAuthored = false; f.Conflicting = true }), hiveSupersededVerdictHumanNote, hiveSupersededReasonNotHive},
		{"held keeps", with(func(f *HiveSupersededAutoCloseFacts) { f.Held = true; f.Conflicting = true }), hiveSupersededVerdictKeep, hiveSupersededReasonHeld},
		{"keep-open keeps", with(func(f *HiveSupersededAutoCloseFacts) { f.KeptOpen = true; f.Conflicting = true }), hiveSupersededVerdictKeep, hiveSupersededReasonKeepOpen},
		{"parked by a person keeps", with(func(f *HiveSupersededAutoCloseFacts) { f.ParkedByPerson = true; f.Conflicting = true }), hiveSupersededVerdictKeep, hiveSupersededReasonPerson},
		{"below L6 keeps", with(func(f *HiveSupersededAutoCloseFacts) { f.Level = acmmLevelAutonomous - 1; f.Conflicting = true }), hiveSupersededVerdictKeep, hiveSupersededReasonLevel},
		{"conflicting closes", with(func(f *HiveSupersededAutoCloseFacts) { f.Conflicting = true }), hiveSupersededVerdictClose, SupersessionActionClosedConflicting},
		{"contained closes", with(func(f *HiveSupersededAutoCloseFacts) { f.FilesSubset = true }), hiveSupersededVerdictClose, SupersessionActionClosedContained},
		{"clean head with extra files keeps", base, hiveSupersededVerdictKeep, hiveSupersededReasonStillAdds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, reason := DecideHiveSupersededAutoClose(tc.facts)
			if verdict != tc.wantVerdict || reason != tc.wantReason {
				t.Fatalf("got (%s, %s), want (%s, %s)", verdict, reason, tc.wantVerdict, tc.wantReason)
			}
		})
	}
}

func TestPRSupersedes(t *testing.T) {
	cases := []struct {
		body string
		n    int
		want bool
	}{
		{"Supersedes #14947 (adopted by the hive: contributor offline).\n\nFixes #14938", 14947, true},
		{"## Summary\n\n  supersedes #14975 (contributor-offline)\nFixes #14972", 14975, true},
		{"Supersedes #149470", 14947, false},
		{"Fixes #14947", 14947, false},
		{"This does not supersede #14947 in body text", 14947, false},
		{"", 14947, false},
	}
	for _, tc := range cases {
		if got := PRSupersedes(tc.body, tc.n); got != tc.want {
			t.Errorf("PRSupersedes(%q, %d) = %v, want %v", tc.body, tc.n, got, tc.want)
		}
	}
}

// TestSweepDuplicateFixPRs_AdoptionPairIsNotADuplicate is the
// hivecommons/hive#14977 → #14947 case: the hive adopted a contributor's PR
// by opening an App-authored PR with the same head whose body says
// "Supersedes #14947". The duplicate-fix sweep must not close the adoptee
// as a duplicate of the PR it supersedes.
func TestSweepDuplicateFixPRs_AdoptionPairIsNotADuplicate(t *testing.T) {
	srv, obs := dupFixServer(t, []dupFixPR{
		{Number: 14947, Body: "Fixes #14938", Author: "thatguywob", Human: true, CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "green"},
		{Number: 14977, Body: "Supersedes #14947 (adopted by the hive: contributor offline).\n\nFixes #14938", Author: "hive-app[bot]", CreatedAt: dupT0.Add(40 * time.Minute), UpdatedAt: dupT0.Add(40 * time.Minute), Checks: "pending"},
	}, "open")
	defer srv.Close()
	c, rec := newSupersessionTestClient(t, srv, "o", "r")

	res, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow})
	if err != nil {
		t.Fatalf("SweepDuplicateFixPRs: %v", err)
	}
	if res.Actions() != 0 || len(obs.closed) != 0 {
		t.Fatalf("adoption pair must not be acted on: res=%+v closed=%v", res, obs.closed)
	}
	if len(obs.comments[14977])+len(obs.comments[14947]) != 0 {
		t.Fatalf("adoption pair must not be commented on: %v / %v", obs.comments[14977], obs.comments[14947])
	}
	if len(parkedPRs(rec)) != 0 {
		t.Fatalf("parked = %v, want none", parkedPRs(rec))
	}
}

func TestSupersessionSweepOptions_AutoCloseDefaultsOn(t *testing.T) {
	var opts SupersessionSweepOptions
	if !opts.autoCloseSupersededOn() {
		t.Fatal("nil AutoCloseSuperseded must mean on (governor.auto_close_superseded defaults to true)")
	}
	opts.AutoCloseSuperseded = func() bool { return false }
	if opts.autoCloseSupersededOn() {
		t.Fatal("explicit false must be honoured")
	}
}

func TestSupersededAutoClosePolicyFor_DefaultsToAutonomous(t *testing.T) {
	c := &Client{}
	on, level := c.supersededAutoClosePolicyFor("o/r")
	if !on || level != acmmLevelAutonomous {
		t.Fatalf("default policy = (%v, %d), want (true, L6)", on, level)
	}
	c.SetSupersededAutoClosePolicy(func(repo string) (bool, int) {
		if repo != "o/r" {
			t.Fatalf("repo = %q", repo)
		}
		return false, 3
	})
	on, level = c.supersededAutoClosePolicyFor("o/r")
	if on || level != 3 {
		t.Fatalf("configured policy = (%v, %d), want (false, 3)", on, level)
	}
	if !strings.Contains(SupersessionActionClosedConflicting, "conflicting") {
		t.Fatalf("action name %q should say why", SupersessionActionClosedConflicting)
	}
}

// TestCloseSupersededHivePRsForMergedPR_ContainedHiveOpenPRIsClosed covers
// the close-on-merge hook: #13760 merged closing #13780, the hive's own
// #13782 still closes #13780 and touches only files #13760 touched, so it
// closes as superseded in the same breath as the merge.
func TestCloseSupersededHivePRsForMergedPR_ContainedHiveOpenPRIsClosed(t *testing.T) {
	open := resolveFixtures(supersessionPRFixture{Files: []string{"pkg/dashboard/label_presets.go"}})
	server, obs := supersessionSweepServer(t, resolveOrg, resolveRepo, open,
		[]supersessionIssueFixture{{Number: 13780, State: "closed", GraphQLCloserPR: 13760}})
	defer server.Close()
	c, rec := newSupersessionTestClient(t, server, resolveOrg, resolveRepo)

	closed, err := c.CloseSupersededHivePRsForMergedPR(context.Background(), resolveOrg+"/"+resolveRepo, 13760, []int{13780})
	if err != nil {
		t.Fatalf("CloseSupersededHivePRsForMergedPR: %v", err)
	}
	if len(closed) != 1 || closed[0] != 13782 || len(obs.closed) != 1 || obs.closed[0] != 13782 {
		t.Fatalf("closed = %v (wire %v), want [13782]", closed, obs.closed)
	}
	if !slices.Contains(obs.labels[13782], SupersededLabel) {
		t.Fatalf("labels = %v, want %s", obs.labels[13782], SupersededLabel)
	}
	if cs := obs.comments[13782]; len(cs) != 1 || !strings.Contains(cs[0], "#13760") || !strings.Contains(cs[0], "superseded") {
		t.Fatalf("comment = %v, want one naming #13760", cs)
	}
	if len(parkedPRs(rec)) != 0 {
		t.Fatalf("parked = %v, want none", parkedPRs(rec))
	}

	// Unrelated issue: nothing to do, no writes.
	closed, err = c.CloseSupersededHivePRsForMergedPR(context.Background(), resolveOrg+"/"+resolveRepo, 13760, []int{999})
	if err != nil || len(closed) != 0 || len(obs.closed) != 1 {
		t.Fatalf("unrelated issue: closed=%v err=%v wire=%v", closed, err, obs.closed)
	}
}

// TestCloseSupersededHivePRsForMergedPR_HumanPRKept: a person's PR is never
// auto-closed, even when it is just as superseded.
func TestCloseSupersededHivePRsForMergedPR_HumanPRKept(t *testing.T) {
	open := resolveFixtures(supersessionPRFixture{Author: "thatguywob", Files: []string{"pkg/dashboard/label_presets.go"}})
	server, obs := supersessionSweepServer(t, resolveOrg, resolveRepo, open,
		[]supersessionIssueFixture{{Number: 13780, State: "closed", GraphQLCloserPR: 13760}})
	defer server.Close()
	c, _ := newSupersessionTestClient(t, server, resolveOrg, resolveRepo)

	closed, err := c.CloseSupersededHivePRsForMergedPR(context.Background(), resolveOrg+"/"+resolveRepo, 13760, []int{13780})
	if err != nil || len(closed) != 0 || len(obs.closed) != 0 {
		t.Fatalf("human PR: closed=%v err=%v wire=%v", closed, err, obs.closed)
	}
}

// TestCloseSupersededHivePRsForMergedPR_KnobOff: governor.auto_close_superseded
// false leaves the hive's own superseded PR to the sweep's report path.
func TestCloseSupersededHivePRsForMergedPR_KnobOff(t *testing.T) {
	open := resolveFixtures(supersessionPRFixture{Files: []string{"pkg/dashboard/label_presets.go"}})
	server, obs := supersessionSweepServer(t, resolveOrg, resolveRepo, open,
		[]supersessionIssueFixture{{Number: 13780, State: "closed", GraphQLCloserPR: 13760}})
	defer server.Close()
	c, _ := newSupersessionTestClient(t, server, resolveOrg, resolveRepo)
	c.SetSupersededAutoClosePolicy(func(string) (bool, int) { return false, acmmLevelAutonomous })

	closed, err := c.CloseSupersededHivePRsForMergedPR(context.Background(), resolveOrg+"/"+resolveRepo, 13760, []int{13780})
	if err != nil || len(closed) != 0 || len(obs.closed) != 0 {
		t.Fatalf("knob off: closed=%v err=%v wire=%v", closed, err, obs.closed)
	}
}

// TestDecideHiveSupersededAutoClose_DeclaredAndIssueClosed pins the two
// hivecommons/hive#15208 additions: a merged PR that declares it supersedes
// this one closes it whatever the head measures, and a PR whose every
// closing target is closed with no merged closer (closed as not planned)
// has nothing left to deliver.
func TestDecideHiveSupersededAutoClose_DeclaredAndIssueClosed(t *testing.T) {
	base := HiveSupersededAutoCloseFacts{Enabled: true, HiveAuthored: true, Superseded: true, Level: acmmLevelAutonomous}
	declared := base
	declared.Declared = true
	if v, r := DecideHiveSupersededAutoClose(declared); v != hiveSupersededVerdictClose || r != SupersessionActionClosedDeclared {
		t.Fatalf("declared clean head: got (%s, %s), want close/%s", v, r, SupersessionActionClosedDeclared)
	}
	issueClosed := base
	issueClosed.IssuesClosedNoCloser = true
	if v, r := DecideHiveSupersededAutoClose(issueClosed); v != hiveSupersededVerdictClose || r != SupersessionActionClosedIssueClosed {
		t.Fatalf("issue closed, no closer: got (%s, %s), want close/%s", v, r, SupersessionActionClosedIssueClosed)
	}
	parked := declared
	parked.ParkedByPerson = true
	if v, r := DecideHiveSupersededAutoClose(parked); v != hiveSupersededVerdictKeep || r != hiveSupersededReasonPerson {
		t.Fatalf("a person's park still wins over a declaration: got (%s, %s)", v, r)
	}
}

func TestBodyDeclaresSupersedes(t *testing.T) {
	cases := []struct {
		body string
		n    int
		want bool
	}{
		{"## Supersedes #15208 · layers on #15209\n\nFixes #15188", 15208, true},
		{"## Supersedes #15208 · layers on #15209\n\nFixes #15188", 15209, false},
		{"supersedes: #12, #13", 13, true},
		{"Supersedes #152080", 15208, false},
		{"Fixes #15208", 15208, false},
		{"Supersedes hivecommons/hive#15208", 15208, true},
		{"", 15208, false},
	}
	for _, tc := range cases {
		if got := bodyDeclaresSupersedes(tc.body, tc.n); got != tc.want {
			t.Errorf("bodyDeclaresSupersedes(%q, %d) = %v, want %v", tc.body, tc.n, got, tc.want)
		}
	}
}

// TestCloseSupersededHivePRIfEligible_DeclaredByMergedPR is
// hivecommons/hive#15208: the hive's PR says `Closes #15203` (a child
// issue still open) and `Part of #15188`; #15206 merged with "## Supersedes
// #15208" and closed #15188. The merged-closer rule reads #15208 as live
// work (an issue it closes is open); the declaration closes it anyway,
// with a comment naming #15206 and #15188, no conflict or file read.
func TestCloseSupersededHivePRIfEligible_DeclaredByMergedPR(t *testing.T) {
	open := supersessionPRFixture{Number: 15208, Title: "child (b)", Body: "Solves child (b).\n\nCloses #15203\nPart of #15188", Author: resolveBot, Files: []string{"pkg/a.go", "pkg/b.go"},
		CrossRefs: []supersessionCrossRefFixture{
			{Number: 15209, Body: "Refs #15208 (not a supersession)", Merged: true},
			{Number: 15206, Body: "## Supersedes #15208 · layers on #15209\n\nFixes #15188", Merged: true},
		}}
	server, obs := supersessionSweepServer(t, resolveOrg, resolveRepo, []supersessionPRFixture{open},
		[]supersessionIssueFixture{{Number: 15203, State: "open"}, {Number: 15188, State: "closed", GraphQLCloserPR: 15206}})
	defer server.Close()
	c, rec := newSupersessionTestClient(t, server, resolveOrg, resolveRepo)

	prs, err := c.listOpenPRsForSupersessionSweep(context.Background(), resolveOrg, resolveRepo)
	if err != nil || len(prs) != 1 {
		t.Fatalf("list: %v %d", err, len(prs))
	}
	res, err := c.CloseSupersededHivePRIfEligible(context.Background(), resolveOrg+"/"+resolveRepo, prs[0])
	if err != nil {
		t.Fatalf("CloseSupersededHivePRIfEligible: %v", err)
	}
	if !res.Closed || res.Reason != SupersessionActionClosedDeclared || res.CloserPR != 15206 || res.Issue != 15188 {
		t.Fatalf("res = %+v, want closed-declared by #15206 for #15188", res)
	}
	if len(obs.closed) != 1 || obs.closed[0] != 15208 || !slices.Contains(obs.labels[15208], SupersededLabel) {
		t.Fatalf("closed=%v labels=%v", obs.closed, obs.labels)
	}
	if cs := obs.comments[15208]; len(cs) != 1 || !strings.Contains(cs[0], "Superseded by #15206 which closed #15188") {
		t.Fatalf("comment = %v", cs)
	}
	closedFacts := 0
	for _, f := range rec.All() {
		if f.Method == "Closed" && f.Ref.Number == 15208 {
			closedFacts++
		}
	}
	if closedFacts != 1 {
		t.Fatalf("stage Closed facts = %d, want 1", closedFacts)
	}
}

// TestCloseSupersededHivePRIfEligible_UnmergedDeclarationKeeps: a PR that
// merely says it supersedes this one but has not merged decides nothing.
func TestCloseSupersededHivePRIfEligible_UnmergedDeclarationKeeps(t *testing.T) {
	open := supersessionPRFixture{Number: 15208, Body: "Closes #15203", Author: resolveBot, Files: []string{"pkg/a.go"},
		CrossRefs: []supersessionCrossRefFixture{{Number: 15206, Body: "Supersedes #15208", Merged: false}}}
	server, obs := supersessionSweepServer(t, resolveOrg, resolveRepo, []supersessionPRFixture{open},
		[]supersessionIssueFixture{{Number: 15203, State: "open"}})
	defer server.Close()
	c, _ := newSupersessionTestClient(t, server, resolveOrg, resolveRepo)
	prs, _ := c.listOpenPRsForSupersessionSweep(context.Background(), resolveOrg, resolveRepo)
	res, err := c.CloseSupersededHivePRIfEligible(context.Background(), resolveOrg+"/"+resolveRepo, prs[0])
	if err != nil || res.Closed || res.Reason != hiveSupersededReasonNotSupers || len(obs.closed) != 0 {
		t.Fatalf("res=%+v err=%v closed=%v, want kept not-superseded", res, err, obs.closed)
	}
}

// TestCloseSupersededHivePRIfEligible_IssueClosedWithoutCloser: the issue
// the hive's PR fixes was closed as not planned (no merged PR closed it);
// the PR has nothing left to deliver and is closed with the reason said.
// A "completed" close whose PR did not resolve is left alone (it may be a
// transient lookup failure), as is a bare reference to a closed issue.
func TestCloseSupersededHivePRIfEligible_IssueClosedWithoutCloser(t *testing.T) {
	open := supersessionPRFixture{Number: 41, Body: "Fixes #40", Author: resolveBot, Files: []string{"pkg/a.go"}}
	server, obs := supersessionSweepServer(t, resolveOrg, resolveRepo, []supersessionPRFixture{open},
		[]supersessionIssueFixture{{Number: 40, State: "closed", StateReason: "not_planned"}})
	defer server.Close()
	c, _ := newSupersessionTestClient(t, server, resolveOrg, resolveRepo)
	prs, _ := c.listOpenPRsForSupersessionSweep(context.Background(), resolveOrg, resolveRepo)
	res, err := c.CloseSupersededHivePRIfEligible(context.Background(), resolveOrg+"/"+resolveRepo, prs[0])
	if err != nil || !res.Closed || res.Reason != SupersessionActionClosedIssueClosed || res.Issue != 40 || res.CloserPR != 0 {
		t.Fatalf("res=%+v err=%v, want closed-issue-closed for #40", res, err)
	}
	if cs := obs.comments[41]; len(cs) != 1 || !strings.Contains(cs[0], "every issue it closes (#40) was closed as not planned") || !strings.Contains(cs[0], "Closing PR: none") {
		t.Fatalf("comment = %v", cs)
	}
	for name, tc := range map[string]struct {
		pr    supersessionPRFixture
		issue supersessionIssueFixture
	}{
		"bare reference is context, not delivery":   {supersessionPRFixture{Number: 42, Body: "Refs #40", Author: resolveBot, Files: []string{"pkg/a.go"}}, supersessionIssueFixture{Number: 40, State: "closed", StateReason: "not_planned"}},
		"completed without a resolved closer keeps": {supersessionPRFixture{Number: 43, Body: "Fixes #40", Author: resolveBot, Files: []string{"pkg/a.go"}}, supersessionIssueFixture{Number: 40, State: "closed", StateReason: "completed"}},
	} {
		t.Run(name, func(t *testing.T) {
			server, obs := supersessionSweepServer(t, resolveOrg, resolveRepo, []supersessionPRFixture{tc.pr}, []supersessionIssueFixture{tc.issue})
			defer server.Close()
			c, _ := newSupersessionTestClient(t, server, resolveOrg, resolveRepo)
			prs, _ := c.listOpenPRsForSupersessionSweep(context.Background(), resolveOrg, resolveRepo)
			if res, err := c.CloseSupersededHivePRIfEligible(context.Background(), resolveOrg+"/"+resolveRepo, prs[0]); err != nil || res.Closed || len(obs.closed) != 0 {
				t.Fatalf("res=%+v err=%v closed=%v, want kept", res, err, obs.closed)
			}
		})
	}
}
