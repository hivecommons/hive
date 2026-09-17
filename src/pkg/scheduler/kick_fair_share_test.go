package scheduler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type fsItem struct {
	repo string
	n    int
}

func fsRepo(i fsItem) string { return i.repo }

func fsItems(spec map[string]int, order ...string) []fsItem {
	var out []fsItem
	for _, repo := range order {
		for i := 1; i <= spec[repo]; i++ {
			out = append(out, fsItem{repo: repo, n: i})
		}
	}
	return out
}

func fsCountByRepo(items []fsItem) map[string]int {
	got := map[string]int{}
	for _, it := range items {
		got[it.repo]++
	}
	return got
}

// The budget is spread across repos rather than consumed by whichever repos
// sort first. This is the real bluefin shape, measured 2026-09-17: 423 open
// PRs across 16 repos. Under the old flat prefix cut a cap of 50 was exhausted
// inside "bluefin", the SECOND repo, so the remaining fourteen contributed
// nothing to any kick — utah sat at cumulative positions 275-312 and was never
// once visible, which is how utah#108 and utah#110 became the same change
// twice (hivecommons/hive#7455).
func TestFairShareByRepo_SpreadsAcrossRepos(t *testing.T) {
	order := []string{"common", "bluefin", "bluefin-lts", "actions", "testsuite",
		"server", "fsdk-containers", "finpilot", "dakota-iso", "utah",
		"utah-packages", "documentation", "website", "review", "bluefin-bling",
		"chairlift"}
	items := fsItems(map[string]int{
		"common": 32, "bluefin": 37, "bluefin-lts": 32, "actions": 10,
		"testsuite": 40, "server": 33, "fsdk-containers": 26, "finpilot": 19,
		"dakota-iso": 45, "utah": 38, "utah-packages": 35, "documentation": 11,
		"website": 16, "review": 12, "bluefin-bling": 15, "chairlift": 22,
	}, order...)
	if len(items) != 423 {
		t.Fatalf("fixture has %d items, want the measured 423", len(items))
	}

	got := fairShareByRepo(items, 50, fsRepo)
	if len(got) != 50 {
		t.Fatalf("selected %d items, want the full budget of 50", len(got))
	}
	counts := fsCountByRepo(got)
	if len(counts) != len(order) {
		t.Fatalf("only %d of %d repos represented (%v) — the prefix-cut blind spot is back",
			len(counts), len(order), counts)
	}
	// Every repo has far more than its share here, so the split is even.
	for _, repo := range order {
		if n := counts[repo]; n < 3 || n > 4 {
			t.Errorf("repo %s got %d of 50 (%v), want an even ~3 share", repo, n, counts)
		}
	}
	// The specific repo the flat cut hid. Named so a regression names its victim.
	if counts["utah"] == 0 {
		t.Errorf("utah got no rows — this is exactly the #7455 duplicate-PR bug")
	}
}

// A repo holding fewer items than its share must not waste the slots it cannot
// use — they go to repos that still have work. "10 from each unless there
// aren't 10 to retrieve."
func TestFairShareByRepo_RedistributesUnusedSlots(t *testing.T) {
	items := fsItems(map[string]int{"tiny": 2, "big": 60}, "tiny", "big")

	got := fairShareByRepo(items, 20, fsRepo)
	if len(got) != 20 {
		t.Fatalf("selected %d, want 20 — unused slots must be redistributed", len(got))
	}
	counts := fsCountByRepo(got)
	if counts["tiny"] != 2 {
		t.Errorf("tiny contributed %d, want all 2 of its items", counts["tiny"])
	}
	if counts["big"] != 18 {
		t.Errorf("big contributed %d, want the remaining 18", counts["big"])
	}
}

// Membership changes; ordering does not. The rendered list must still group by
// repo the way the caller assembled it.
func TestFairShareByRepo_PreservesInputOrder(t *testing.T) {
	items := fsItems(map[string]int{"a": 5, "b": 5}, "a", "b")

	got := fairShareByRepo(items, 6, fsRepo)
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if prev.repo == cur.repo && prev.n >= cur.n {
			t.Fatalf("within-repo order broken at %d: %+v then %+v", i, prev, cur)
		}
	}
	if got[0].repo != "a" {
		t.Errorf("first item = %+v, want the input's first repo", got[0])
	}
}

// Under the cap nothing is dropped, and an unlimited cap returns everything.
func TestFairShareByRepo_UnderCapAndUnlimited(t *testing.T) {
	items := fsItems(map[string]int{"a": 3, "b": 4}, "a", "b")

	if got := fairShareByRepo(items, 100, fsRepo); len(got) != 7 {
		t.Errorf("under the cap: got %d, want all 7", len(got))
	}
	if got := fairShareByRepo(items, config.KickListUnlimited, fsRepo); len(got) != 7 {
		t.Errorf("unlimited: got %d, want all 7", len(got))
	}
	if got := fairShareByRepo(items, -5, fsRepo); len(got) != 7 {
		t.Errorf("negative: got %d, want all 7", len(got))
	}
	if got := fairShareByRepo([]fsItem(nil), 10, fsRepo); len(got) != 0 {
		t.Errorf("empty input: got %d, want 0", len(got))
	}
}

// The rendered PR list must carry every repo, not just the first few, and must
// still announce the cut.
func TestFormatPRList_FairSharesAcrossRepos(t *testing.T) {
	s := newScheduler()
	s.cfg.Governor.KickLimits = config.KickLimitsConfig{MaxPRs: ptr(6)}

	var prs []github.PullRequest
	for _, repo := range []string{"first", "second", "third", "fourth"} {
		for i := 1; i <= 10; i++ {
			prs = append(prs, github.PullRequest{
				Repo: repo, Number: i, Title: fmt.Sprintf("%s pr %d", repo, i), Author: "someone",
			})
		}
	}
	out := s.formatPRList(&github.ActionableResult{PRs: github.PRResult{Count: len(prs), Items: prs}})

	for _, repo := range []string{"first", "second", "third", "fourth"} {
		if !strings.Contains(out, repo+"#") {
			t.Errorf("repo %s absent from the PR list:\n%s", repo, out)
		}
	}
	if !strings.Contains(out, "… and 34 more open PRs") {
		t.Errorf("overflow marker missing or miscounted:\n%s", out)
	}
}

// An explicit max_prs: 0 lists every PR instead of emptying the list — the
// failure mode of treating the unlimited sentinel as a literal cap.
func TestFormatPRList_UnlimitedListsEverything(t *testing.T) {
	s := newScheduler()
	s.cfg.Governor.KickLimits = config.KickLimitsConfig{MaxPRs: ptr(0)}

	prs := manyPRs(120)
	out := s.formatPRList(&github.ActionableResult{PRs: github.PRResult{Count: len(prs), Items: prs}})

	if got := prLineCount(out); got != 120 {
		t.Errorf("unlimited rendered %d PR lines, want all 120", got)
	}
	if strings.Contains(out, "more open PRs not listed") {
		t.Errorf("unlimited must not emit an overflow marker:\n%s", out)
	}
}
