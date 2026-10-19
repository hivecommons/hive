package github

import (
	"path/filepath"
	"testing"
	"time"
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
