package github

import (
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// hivecommons/hive#15499: the hive's own adoption whose change had already
// landed through another PR sat at "needs decision" for a day while
// reviewers asked a person "close as a no-op?" six times. The rule closes
// it; a person's empty PR still gets the prompt.
func TestDecideHiveEmptyDiffAutoClose(t *testing.T) {
	base := HiveEmptyDiffAutoCloseFacts{Enabled: true, HiveAuthored: true, DiffKnown: true, Level: acmmLevelAutonomous, HeadAge: time.Hour}
	with := func(mut func(*HiveEmptyDiffAutoCloseFacts)) HiveEmptyDiffAutoCloseFacts {
		f := base
		mut(&f)
		return f
	}
	cases := []struct {
		name        string
		facts       HiveEmptyDiffAutoCloseFacts
		wantVerdict string
		wantReason  string
	}{
		{"empty hive PR closes", base, hiveEmptyDiffVerdictClose, SupersessionActionClosedEmptyDiff},
		{"disabled keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.Enabled = false }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonDisabled},
		{"merged keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.Merged = true }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonMerged},
		{"draft keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.Draft = true }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonDraft},
		{"unknown compare keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.DiffKnown = false }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonUnknown},
		{"real diff keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.DiffFiles = 1 }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonHasDiff},
		{"person's empty PR is a prompt, never a close", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.HiveAuthored = false }), hiveEmptyDiffVerdictHumanNote, hiveEmptyDiffReasonNotHive},
		{"held keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.Held = true }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonHeld},
		{"keep-open keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.KeptOpen = true }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonKeepOpen},
		{"parked by a person keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.ParkedByPerson = true }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonPerson},
		{"below L6 keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.Level = acmmLevelAutonomous - 1 }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonLevel},
		{"fresh head keeps", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.HeadAge = EmptyDiffMinHeadAge / 2 }), hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonTooFresh},
		// hivecommons/hive#15647: the reviewer read this head and said it
		// is already landed; a fresh head is not mid-push, and files that
		// are all identical on the base tip are no diff.
		{"reviewer-confirmed fresh head closes", with(func(f *HiveEmptyDiffAutoCloseFacts) { f.HeadAge = 0; f.ReviewerConfirmed = true }), hiveEmptyDiffVerdictClose, SupersessionActionClosedEmptyDiff},
		{"files identical on base close as already landed", with(func(f *HiveEmptyDiffAutoCloseFacts) {
			f.DiffFiles = 2
			f.TreeMatchesBase = true
			f.ReviewerConfirmed = true
		}), hiveEmptyDiffVerdictClose, SupersessionActionClosedAlreadyLanded},
		{"files identical on base but a person's PR is a prompt", with(func(f *HiveEmptyDiffAutoCloseFacts) {
			f.DiffFiles = 2
			f.TreeMatchesBase = true
			f.HiveAuthored = false
		}), hiveEmptyDiffVerdictHumanNote, hiveEmptyDiffReasonNotHive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, r := DecideHiveEmptyDiffAutoClose(tc.facts)
			if v != tc.wantVerdict || r != tc.wantReason {
				t.Fatalf("got (%s, %s), want (%s, %s)", v, r, tc.wantVerdict, tc.wantReason)
			}
		})
	}
}

func TestPRChangedFilesClaimsEmpty(t *testing.T) {
	if PRChangedFilesClaimsEmpty(nil) || PRChangedFilesClaimsEmpty(&gh.PullRequest{}) {
		t.Fatal("missing counts (list payload) must not read as empty")
	}
	if !PRChangedFilesClaimsEmpty(&gh.PullRequest{ChangedFiles: gh.Ptr(0), Additions: gh.Ptr(0), Deletions: gh.Ptr(0)}) {
		t.Fatal("0/0/0 is empty")
	}
	if PRChangedFilesClaimsEmpty(&gh.PullRequest{ChangedFiles: gh.Ptr(0), Additions: gh.Ptr(2), Deletions: gh.Ptr(0)}) {
		t.Fatal("any count above zero is not empty")
	}
}

func TestRenderEmptyDiffCloseCommentCarriesMarkers(t *testing.T) {
	body := renderEmptyDiffCloseComment(supersessionCloser{Repo: "hivecommons/hive", Number: 15427, URL: "https://github.com/hivecommons/hive/pull/15427"}, "closed: empty diff", false)
	for _, want := range []string{supersessionSweepMarker, reviewRedundantOfMarker + " #15427 -->", "[#15427](https://github.com/hivecommons/hive/pull/15427)", "edited in place"} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, body)
		}
	}
	if reviewRedundantOfNumber(body) != 15427 {
		t.Fatalf("redundant-of not readable back: %d", reviewRedundantOfNumber(body))
	}
}

// hivecommons/hive#15499: the adoption's dispatch records carried no
// author_agent, so the relay judged the hive's own PR non-hive and routed
// its decision to the contributor lane.
func TestDispatchRecordHiveAuthored(t *testing.T) {
	if !dispatchRecordHiveAuthored("", true) || !dispatchRecordHiveAuthored("author-1", false) || dispatchRecordHiveAuthored("  ", false) {
		t.Fatal("hive_authored on the record, or an author agent, marks the PR hive-authored; neither does not")
	}
}

func TestReviewRedundantOfNumber(t *testing.T) {
	cases := map[string]int{
		"<!-- hive-verdict: needs-decision -->\n<!-- hive-redundant-of: #15427 -->":                     15427,
		"<!-- hive-redundant-of: hivecommons/hive#15427 -->":                                            15427,
		"<!-- hive-redundant-of: https://github.com/hivecommons/hive/pull/15427 -->\nclose as a no-op?": 15427,
		"<!-- hive-redundant-of: none -->":                                                              0,
		"no marker at all":                                                                              0,
	}
	for body, want := range cases {
		if got := reviewRedundantOfNumber(body); got != want {
			t.Fatalf("%q → %d, want %d", body, got, want)
		}
	}
}
