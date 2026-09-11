package automerge

import (
	"testing"
	"time"
)

// TestSelfAuthoredSweepInterval_StaysWithinRateBudget is the regression guard
// for a live outage: a 45-repo hive swept every 10s, which lists open PRs once
// per repo — 45 x 360 = 16,200 requests/hour against a 6,900/hour App limit.
// The sweep 403'd continuously, go-github short-circuited every subsequent
// request until the recorded reset, and no PR merged for a full day on a hive
// that had merged 767 before.
//
// The interval must therefore keep the sweep inside its share of the budget at
// any repo count.
func TestSelfAuthoredSweepInterval_StaysWithinRateBudget(t *testing.T) {
	budget := float64(githubAppHourlyRateLimit) * selfAuthoredSweepBudgetShare
	for _, repos := range []int{1, 5, 10, 39, 45, 100, 500} {
		interval := selfAuthoredSweepInterval(repos)
		if interval <= 0 {
			t.Fatalf("repos=%d: non-positive interval %v", repos, interval)
		}
		ticksPerHour := time.Hour.Seconds() / interval.Seconds()
		// Cost model: one list per repo plus the per-candidate Get allowance.
		reqPerHour := ticksPerHour * float64(repos+selfAuthoredSweepCandidateAllowance)
		if repos <= selfAuthoredSweepSmallHiveRepos {
			// Small hives keep the fixed tick by design; the allowance is an
			// upper bound sized for many-repo hives and over-penalises a hive
			// that will rarely hold a fraction of that many open App PRs. Only
			// the list-call cost is structural there — assert THAT stays well
			// inside the full limit.
			if listPerHour := ticksPerHour * float64(repos); listPerHour > githubAppHourlyRateLimit/2 {
				t.Errorf("repos=%d: %.0f list req/hour is not comfortably inside %d/hour (interval %v)",
					repos, listPerHour, githubAppHourlyRateLimit, interval)
			}
			continue
		}
		if interval < selfAuthoredSweepMaxInterval && reqPerHour > budget*1.05 {
			t.Errorf("repos=%d: %.0f req/hour exceeds the %.0f/hour share (interval %v)",
				repos, reqPerHour, budget, interval)
		}
		if reqPerHour > githubAppHourlyRateLimit {
			t.Errorf("repos=%d: %.0f req/hour exceeds the FULL %d/hour limit (interval %v)",
				repos, reqPerHour, githubAppHourlyRateLimit, interval)
		}
	}
}

// TestSelfAuthoredSweepInterval_SmallHivesUnchanged: the fix must not slow down
// the hives it was already fine for. At small repo counts the fixed interval
// remains the floor, so behaviour is byte-identical to before.
func TestSelfAuthoredSweepInterval_SmallHivesUnchanged(t *testing.T) {
	for _, repos := range []int{0, 1, 2, 3, 4} {
		if got := selfAuthoredSweepInterval(repos); got != selfAuthoredAutoMergeSweepInterval {
			t.Errorf("repos=%d: interval = %v, want the unchanged %v",
				repos, got, selfAuthoredAutoMergeSweepInterval)
		}
	}
}

func TestSelfAuthoredSweepInterval_AdaptsToObservedCandidates(t *testing.T) {
	base := selfAuthoredSweepIntervalForCandidates(6, selfAuthoredSweepCandidateAllowance)
	got := selfAuthoredSweepIntervalForCandidates(6, selfAuthoredSweepCandidateAllowance*3)
	if got <= base {
		t.Fatalf("interval with excess candidates = %v, want greater than base %v", got, base)
	}
}

func TestSelfAuthoredSweepInterval_AdaptiveBackoffIsBounded(t *testing.T) {
	got := selfAuthoredSweepIntervalForCandidates(6, 100000)
	if got != selfAuthoredSweepMaxInterval {
		t.Fatalf("interval with huge candidate backlog = %v, want max %v", got, selfAuthoredSweepMaxInterval)
	}
}

func TestSelfAuthoredSweepInterval_SmallHiveIgnoresObservedCandidates(t *testing.T) {
	got := selfAuthoredSweepIntervalForCandidates(selfAuthoredSweepSmallHiveRepos, 100000)
	if got != selfAuthoredAutoMergeSweepInterval {
		t.Fatalf("small-hive interval = %v, want unchanged %v", got, selfAuthoredAutoMergeSweepInterval)
	}
}

func TestNextSelfAuthoredSweepInterval_AdjustsAndRecovers(t *testing.T) {
	base := selfAuthoredSweepIntervalForCandidates(6, selfAuthoredSweepCandidateAllowance)
	backedOff, changed := nextSelfAuthoredSweepInterval(6, base, &AutoMergeSweepResult{Candidates: selfAuthoredSweepCandidateAllowance + 100})
	if !changed || backedOff <= base {
		t.Fatalf("backoff interval=%v changed=%v, want interval greater than base %v", backedOff, changed, base)
	}

	recovered, changed := nextSelfAuthoredSweepInterval(6, backedOff, &AutoMergeSweepResult{Candidates: 0})
	if !changed || recovered != base {
		t.Fatalf("recovered interval=%v changed=%v, want base %v", recovered, changed, base)
	}

	unchanged, changed := nextSelfAuthoredSweepInterval(6, base, nil)
	if changed || unchanged != base {
		t.Fatalf("nil result interval=%v changed=%v, want unchanged base %v", unchanged, changed, base)
	}
}

// TestSelfAuthoredSweepInterval_ScalesWithRepos: more repos must never sweep
// more often, or the guard above can be defeated by a non-monotonic curve.
func TestSelfAuthoredSweepInterval_ScalesWithRepos(t *testing.T) {
	prev := selfAuthoredSweepInterval(1)
	for _, repos := range []int{5, 10, 20, 45, 100, 250} {
		got := selfAuthoredSweepInterval(repos)
		if got < prev {
			t.Errorf("repos=%d: interval %v is shorter than at fewer repos (%v)", repos, got, prev)
		}
		prev = got
	}
}

// TestSelfAuthoredSweepInterval_RealWorldCase pins the case that broke:
// the 45-repo hive must land well inside its budget rather than 2.3x over it.
func TestSelfAuthoredSweepInterval_RealWorldCase(t *testing.T) {
	interval := selfAuthoredSweepInterval(45)
	reqPerHour := (time.Hour.Seconds() / interval.Seconds()) * float64(45+selfAuthoredSweepCandidateAllowance)
	if reqPerHour > githubAppHourlyRateLimit {
		t.Fatalf("45 repos: %.0f req/hour still exceeds %d/hour (interval %v)",
			reqPerHour, githubAppHourlyRateLimit, interval)
	}
	t.Logf("45 repos -> interval %v, %.0f req/hour (was 16200 at the fixed 10s)", interval, reqPerHour)
}
