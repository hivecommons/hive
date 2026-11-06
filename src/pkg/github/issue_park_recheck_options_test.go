package github

import (
	"strings"
	"testing"
	"time"
)

// projectbluefin/server#614 parked on #583 (both options merge it); #584 and
// #588 appear only in the evidence ("red on `main` and on PRs #583, #584,
// #588, #594, #613"). The re-check reported #584 and #588 merging as "the
// park reason names it" while #583 stayed open.
const park614 = "**Summary:** prow keeps failing because it tries to merge #583, which edits the build automation.\n\n" +
	"- Check: `prow / prow`, red on `main` (cron) and on PRs #583, #584, #588, #594, #613.\n" +
	"- Separately, #588's last red run was `HttpError: Service Unavailable` (a GitHub 503), not this cause.\n\n" +
	"Option A: Add the `workflow` scope to the token prow uses for merges, then let the cron merge #583.\n" +
	"Option B: Merge #583 by hand once; the cron stops retrying it.\n\n" +
	"Recommended: B, because it unblocks today."

func TestParkRecheckOptionsNameWhatTheParkWaitsOn(t *testing.T) {
	t.Run("collateral PRs merge, the option's PR is open: no note", func(t *testing.T) {
		f := prParkedIssue(614, park614)
		f.timeline = append(f.timeline,
			prCrossRef("org/repo", 584, true, "closed", parkT0.Add(16*24*time.Hour), parkT0.Add(16*24*time.Hour), "chore: bump"),
			prCrossRef("org/repo", 588, true, "closed", parkT0.Add(16*24*time.Hour), parkT0.Add(16*24*time.Hour), "fix: retry"),
			prCrossRef("org/repo", 583, true, "open", time.Time{}, time.Time{}, "ci: build.yml"))
		recheck(t, f)
		if len(f.posted) != 0 {
			t.Fatalf("posted %q, want no note while #583 is open", f.posted)
		}
	})
	t.Run("the option's PR merges: note names it", func(t *testing.T) {
		f := prParkedIssue(614, park614)
		f.timeline = append(f.timeline,
			prCrossRef("org/repo", 588, true, "closed", parkT0.Add(16*24*time.Hour), parkT0.Add(16*24*time.Hour), "fix: retry"),
			prCrossRef("org/repo", 583, true, "closed", parkT0.Add(17*24*time.Hour), parkT0.Add(17*24*time.Hour), "ci: build.yml"))
		recheck(t, f)
		if len(f.posted) != 1 {
			t.Fatalf("posted %q", f.posted)
		}
		wantNote(t, f.posted[0], "- #583 merged on 2026-10-20 (the park reason names it).")
		if strings.Contains(f.posted[0], "#588") {
			t.Fatalf("collateral #588 listed:\n%s", f.posted[0])
		}
	})
}

// Options that name no reference leave every named reference in play.
func TestParkRecheckOptionsWithoutRefsKeepNamedRefs(t *testing.T) {
	f := prParkedIssue(13, "Open PR #722 finishes most of the gate.\n\nOption A: wait for it\nOption B: close this issue")
	f.timeline = append(f.timeline, prCrossRef("org/repo", 722, true, "closed", parkT0.Add(21*24*time.Hour), time.Time{}, "ci: gate"))
	recheck(t, f)
	if len(f.posted) != 1 {
		t.Fatalf("posted %q", f.posted)
	}
	wantNote(t, f.posted[0], "- #722 was closed without merging")
}

func TestParkOptionRefs(t *testing.T) {
	if got := parkOptionRefs(park614, "org/repo", 614); len(got) != 1 || got[0] != "org/repo#583" {
		t.Fatalf("option refs = %v, want only #583", got)
	}
	// An option's own continuation lines count; a blank line ends it.
	text := "**A. Pin the tag.**\nWait for #12 to land.\n\nSee also #99.\n**B. Drop it.**"
	if got := parkOptionRefs(text, "org/repo", 1); len(got) != 1 || got[0] != "org/repo#12" {
		t.Fatalf("option refs = %v, want #12 from the continuation line", got)
	}
	if got := parkOptionRefs("Waits on #12 and #13.", "org/repo", 1); got != nil {
		t.Fatalf("no options: %v, want nil (every named ref counts)", got)
	}
}
