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
