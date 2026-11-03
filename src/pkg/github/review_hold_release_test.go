package github

import (
	"path/filepath"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/review"
)

var reviewHoldVerdictAt12962 = time.Date(2026, 10, 19, 2, 0, 0, 0, time.UTC)

func TestReleasesReviewHold12962(t *testing.T) {
	after := reviewHoldVerdictAt12962.Add(time.Hour)
	before := reviewHoldVerdictAt12962.Add(-time.Hour)
	for _, tc := range []struct {
		name string
		r    ReviewHoldRelease
		want bool
	}{
		{"person approved after the verdict", ReviewHoldRelease{Via: ReviewHoldReleaseViaReview, State: "APPROVED", By: "owner", Person: true, At: after}, true},
		{"review state is case-insensitive", ReviewHoldRelease{Via: ReviewHoldReleaseViaReview, State: "approved", By: "owner", Person: true, At: after}, true},
		{"person replied /hive approve after the verdict", ReviewHoldRelease{Via: ReviewHoldReleaseViaComment, Body: "Checked it.\n/hive approve", By: "owner", Person: true, At: after}, true},
		{"approval before the verdict", ReviewHoldRelease{Via: ReviewHoldReleaseViaReview, State: "APPROVED", By: "owner", Person: true, At: before}, false},
		{"approval at the verdict's instant", ReviewHoldRelease{Via: ReviewHoldReleaseViaReview, State: "APPROVED", By: "owner", Person: true, At: reviewHoldVerdictAt12962}, false},
		{"/hive approve before the verdict", ReviewHoldRelease{Via: ReviewHoldReleaseViaComment, Body: "/hive approve", By: "owner", Person: true, At: before}, false},
		{"bot approval", ReviewHoldRelease{Via: ReviewHoldReleaseViaReview, State: "APPROVED", By: "hive[bot]", At: after}, false},
		{"bot /hive approve", ReviewHoldRelease{Via: ReviewHoldReleaseViaComment, Body: "/hive approve", By: "renovate[bot]", At: after}, false},
		{"commented review", ReviewHoldRelease{Via: ReviewHoldReleaseViaReview, State: "COMMENTED", By: "owner", Person: true, At: after}, false},
		{"plain comment", ReviewHoldRelease{Via: ReviewHoldReleaseViaComment, Body: "do not merge this", By: "owner", Person: true, At: after}, false},
		{"no login", ReviewHoldRelease{Via: ReviewHoldReleaseViaReview, State: "APPROVED", Person: true, At: after}, false},
	} {
		if got := ReleasesReviewHold(reviewHoldVerdictAt12962, tc.r); got != tc.want {
			t.Errorf("%s: ReleasesReviewHold = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsReviewHoldReleaseCommand(t *testing.T) {
	for body, want := range map[string]bool{
		"/hive approve":                  true,
		"  /HIVE   Approve  please":      true,
		"lgtm\n/hive approve\n":          true,
		"I would not /hive approve this": false,
		"/hive decision A":               false,
		"looks fine":                     false,
	} {
		if got := IsReviewHoldReleaseCommand(body); got != want {
			t.Errorf("IsReviewHoldReleaseCommand(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestReviewHoldReleasesRoundTrip12962(t *testing.T) {
	path := filepath.Join(t.TempDir(), ReviewHoldReleasesFile)
	releases, err := LoadReviewHoldReleases(path)
	if err != nil || len(releases) != 0 {
		t.Fatalf("missing ledger = %v, %v; want empty, nil", releases, err)
	}

	at := reviewHoldVerdictAt12962.Add(time.Hour)
	comment := ReviewHoldRelease{Via: ReviewHoldReleaseViaComment, Body: "Looked at it.\n/hive approve", By: "owner", Person: true, At: at}
	if err := RecordReviewHoldRelease(path, "Acme/Widget", 7, comment); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Nothing ReleasesReviewHold would refuse is recorded.
	if err := RecordReviewHoldRelease(path, "acme/widget", 8, ReviewHoldRelease{Via: ReviewHoldReleaseViaReview, State: "APPROVED", By: "hive[bot]", At: at}); err != nil {
		t.Fatalf("record bot: %v", err)
	}

	releases, err = LoadReviewHoldReleases(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := ReviewHoldReleaseFor(releases, "acme/widget", 7)
	if !ok {
		t.Fatalf("release not recorded, have %v", releases)
	}
	if got.Body != ReviewHoldReleaseCommand || got.By != "owner" || !got.At.Equal(at) {
		t.Fatalf("release = %+v, want the command only, by owner at %s", got, at)
	}
	if !ReleasesReviewHold(reviewHoldVerdictAt12962, got) {
		t.Fatalf("recorded release %+v no longer releases the hold", got)
	}
	if _, ok := ReviewHoldReleaseFor(releases, "acme/widget", 8); ok {
		t.Fatalf("a bot's approval was recorded")
	}

	// A later release replaces the earlier one.
	review := ReviewHoldRelease{Via: ReviewHoldReleaseViaReview, State: "APPROVED", By: "maintainer", Person: true, At: at.Add(time.Hour)}
	if err := RecordReviewHoldRelease(path, "acme/widget", 7, review); err != nil {
		t.Fatalf("record review: %v", err)
	}
	releases, _ = LoadReviewHoldReleases(path)
	if got, _ := ReviewHoldReleaseFor(releases, "ACME/widget", 7); got.By != "maintainer" {
		t.Fatalf("release = %+v, want the newer review", got)
	}
}

func prCommit13086(sha string, parents ...string) *gh.RepositoryCommit {
	c := &gh.RepositoryCommit{SHA: gh.Ptr(sha)}
	for _, p := range parents {
		c.Parents = append(c.Parents, &gh.Commit{SHA: gh.Ptr(p)})
	}
	return c
}

func allClean13086(string, string, string) bool { return true }

// A base-branch merge moves the head and draws a fresh requires_human verdict
// without changing the PR's own commits; the hold keeps starting at the first
// verdict on that content, so a person's release after it still stands. A
// new non-merge commit or a rewritten history starts a new hold
// (hivecommons/hive#13086).
func TestReviewHoldSinceForCommits13086(t *testing.T) {
	t1 := reviewHoldVerdictAt12962
	t2, t3 := t1.Add(time.Hour), t1.Add(2*time.Hour)
	hold := func(heads ...string) ReviewVerdictHold {
		h := ReviewVerdictHold{Held: true, Verdict: "requires_human"}
		for i, head := range heads {
			at := t1.Add(time.Duration(i) * time.Hour)
			h.Verdicts = append(h.Verdicts, review.HeldVerdict{HeadSHA: head, RecordedAt: at})
			h.HeadSHA, h.RecordedAt = head, at
		}
		return h
	}
	// a: the PR's own commit; m1, m2: `Merge branch 'main'` updates whose
	// second parent is on the base branch; b: a further change.
	mergesOnly := []*gh.RepositoryCommit{prCommit13086("a", "base0"), prCommit13086("m1", "a", "base1"), prCommit13086("m2", "m1", "base2")}
	withChange := []*gh.RepositoryCommit{prCommit13086("a", "base0"), prCommit13086("b", "a"), prCommit13086("m1", "b", "base1")}
	rebased := []*gh.RepositoryCommit{prCommit13086("a2", "base1"), prCommit13086("m1", "a2", "base2")}

	for _, tc := range []struct {
		name    string
		hold    ReviewVerdictHold
		commits []*gh.RepositoryCommit
		want    time.Time
	}{
		{"single verdict", hold("a"), mergesOnly, t1},
		{"base merges only keep the first verdict", hold("a", "m1", "m2"), mergesOnly, t1},
		{"a new commit starts a new hold", hold("a", "m1"), withChange, t2},
		{"a rewritten history starts a new hold", hold("a", "m1"), rebased, t2},
		{"run stops at the first changed content", hold("x", "a", "m1", "m2"), mergesOnly, t2},
		{"the same head again (a changed description) starts a new hold", hold("m1", "m1"), mergesOnly, t2},
		{"unknown latest head keeps the latest verdict", hold("a", "zz"), mergesOnly, t2},
		{"no commits keeps the latest verdict", hold("a", "m1", "m2"), nil, t3},
	} {
		if got := reviewHoldSinceForCommits(tc.hold, tc.commits, allClean13086); !got.Equal(tc.want) {
			t.Errorf("%s: since = %s, want %s", tc.name, got, tc.want)
		}
	}

	// A release after the first verdict but before the base-merge verdicts
	// releases the hold; one before the first verdict does not.
	h := hold("a", "m1", "m2")
	since := reviewHoldSinceForCommits(h, mergesOnly, allClean13086)
	release := ReviewHoldRelease{Via: ReviewHoldReleaseViaComment, Body: "/hive approve", By: "owner", Person: true, At: t1.Add(30 * time.Minute)}
	if !ReleasesReviewHold(since, release) {
		t.Fatalf("release after the first verdict on unchanged content no longer releases the hold")
	}
	if ReleasesReviewHold(reviewHoldSinceForCommits(hold("a", "m1"), withChange, allClean13086), release) {
		t.Fatalf("release before a content change still releases the hold")
	}
}

// A review-hold release survives a base-branch merge only when the merge is
// clean: the PR's patch against its merge-base is unchanged. A merge that
// carries conflict resolution, or one whose cleanliness cannot be read,
// starts a new hold like a new non-merge commit (hivecommons/hive#13086).
func TestReviewHoldSinceCleanMerge13086(t *testing.T) {
	t1 := reviewHoldVerdictAt12962
	t2 := t1.Add(time.Hour)
	h := ReviewVerdictHold{Held: true, Verdict: "requires_human", HeadSHA: "m1", RecordedAt: t2}
	h.Verdicts = []review.HeldVerdict{{HeadSHA: "a", RecordedAt: t1}, {HeadSHA: "m1", RecordedAt: t2}}
	commits := []*gh.RepositoryCommit{prCommit13086("a", "base0"), prCommit13086("m1", "a", "base1")}
	release := ReviewHoldRelease{Via: ReviewHoldReleaseViaComment, Body: "/hive approve", By: "owner", Person: true, At: t1.Add(30 * time.Minute)}

	// Patches as the compare API returns them: base1...a is the PR's patch
	// before the merge, base1...m1 after it.
	prPatch := []*gh.CommitFile{{Filename: gh.Ptr("main.go"), Status: gh.Ptr("modified"), Patch: gh.Ptr("@@ -10,3 +10,4 @@ func main() {\n \tx := 1\n+\ty := 2\n-\tz := 3\n }")}}
	shifted := []*gh.CommitFile{{Filename: gh.Ptr("main.go"), Status: gh.Ptr("modified"), Patch: gh.Ptr("@@ -14,3 +14,4 @@ func main() {\n \tx := 1\n+\ty  :=  2\n-\tz := 3\n }")}}
	resolved := []*gh.CommitFile{{Filename: gh.Ptr("main.go"), Status: gh.Ptr("modified"), Patch: gh.Ptr("@@ -14,3 +14,4 @@ func main() {\n \tx := 1\n+\ty := 4\n-\tz := 3\n }")}}
	cleanFor := func(after []*gh.CommitFile, readErr bool) cleanMergeFunc {
		return func(merge, prParent, baseParent string) bool {
			if merge != "m1" || prParent != "a" || baseParent != "base1" || readErr {
				return false
			}
			before, ok := comparePatchID(prPatch)
			if !ok {
				return false
			}
			got, ok := comparePatchID(after)
			return ok && before == got
		}
	}

	for _, tc := range []struct {
		name  string
		clean cleanMergeFunc
		want  time.Time
	}{
		{"a clean Merge branch 'main' keeps the earlier release", cleanFor(shifted, false), t1},
		{"a conflict-resolving merge starts a new hold", cleanFor(resolved, false), t2},
		{"a merge whose patch cannot be read starts a new hold", cleanFor(shifted, true), t2},
		{"no cleanliness check starts a new hold", nil, t2},
	} {
		since := reviewHoldSinceForCommits(h, commits, tc.clean)
		if !since.Equal(tc.want) {
			t.Errorf("%s: since = %s, want %s", tc.name, since, tc.want)
		}
		if got, want := ReleasesReviewHold(since, release), tc.want.Equal(t1); got != want {
			t.Errorf("%s: release honoured = %t, want %t", tc.name, got, want)
		}
	}

	if _, ok := comparePatchID([]*gh.CommitFile{{Filename: gh.Ptr("big.bin"), Status: gh.Ptr("modified"), Changes: gh.Ptr(5)}}); ok {
		t.Error("a file without a patch or blob SHA produced a patch-id")
	}
	if _, ok := comparePatchID(make([]*gh.CommitFile, 300)); ok {
		t.Error("a possibly truncated compare produced a patch-id")
	}
}

// The review pipeline honours the start the finder recorded with the release
// only for the verdict it was found against (hivecommons/hive#13086).
func TestReleasesRecordedReviewHold13086(t *testing.T) {
	first := reviewHoldVerdictAt12962
	latest := first.Add(2 * time.Hour)
	r := ReviewHoldRelease{Via: ReviewHoldReleaseViaReview, State: "APPROVED", By: "owner", Person: true, At: first.Add(time.Hour), HoldHeadSHA: "m2", HeldSince: first}
	if !ReleasesRecordedReviewHold("m2", latest, r) {
		t.Fatal("release recorded against the hold on m2 does not release it")
	}
	if ReleasesRecordedReviewHold("m3", latest.Add(time.Hour), r) {
		t.Fatal("release recorded against m2 released a newer verdict on m3")
	}
	plain := r
	plain.HoldHeadSHA, plain.HeldSince = "", time.Time{}
	if ReleasesRecordedReviewHold("m2", latest, plain) {
		t.Fatal("a release without a recorded start released a later verdict")
	}
	later := r
	later.HeldSince = latest.Add(time.Hour)
	if ReleasesRecordedReviewHold("m2", latest, later) {
		t.Fatal("a recorded start after the verdict moved the hold's start later")
	}
}
