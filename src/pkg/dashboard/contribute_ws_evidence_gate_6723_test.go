package dashboard

import (
	"testing"
	"time"
)

// #6723 suite: evidence-gated completion.
//
// #6717 showed the contributor relay's chrome_idle fallback reporting a task
// COMPLETE when the prompt was never submitted to the CLI — no commit, no
// branch, no PR, no HIVE_VERDICT. The hub treated that exactly like a genuine
// "nothing to ship" completion: it escalated the no-PR streak (4h → 8h → … →
// the full with-PR window) and cleared the issue's failure history. A handful
// of false completions therefore parked live work for a week.
//
// A false completion is strictly worse than a failure: failures are re-offered,
// false completions silently strand the issue until someone audits the queue.

// completeEvidenceLess reports a completion the way a #5376-or-later relay does
// when it never saw a HIVE_VERDICT line and fell back to idle terminal chrome.
func completeEvidenceLess(hub *ContributeWSHub, key string) {
	hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictIdle, "relay", "", completionSignalChromeIdle)
}

// TestEvidenceLessCompletionDoesNotEscalateCooldown is the core guarantee: no
// number of chrome_idle completions can walk an issue's cooldown up toward the
// with-PR cap. Compare TestNoPRCooldownEscalatesGeometrically, which pins the
// doubling this case must NOT get.
func TestEvidenceLessCompletionDoesNotEscalateCooldown(t *testing.T) {
	hub, _ := covK2Hub(t)
	key := noPRKey("evidence-gate/no-escalation", 1)

	base := completedNoPRCooldownHours * time.Hour
	for i := 0; i < 8; i++ {
		completeEvidenceLess(hub, key)
		if got := recordedCooldown(t, hub, key); got != base {
			t.Fatalf("completion %d: evidence-less cooldown = %v, want a flat %v — "+
				"escalation is what parks the issue (#6723)", i+1, got, base)
		}
	}
}

// TestEvidenceLessCompletionPreservesFailureHistory: clearing the
// consecutive-failure counter on a signal that indicates nothing was attempted
// lets an issue alternate fail → false-complete → fail and never reach
// quarantine.
func TestEvidenceLessCompletionPreservesFailureHistory(t *testing.T) {
	hub, _ := covK2Hub(t)
	key := noPRKey("evidence-gate/failure-history", 2)

	hub.completedMu.Lock()
	hub.failedTasks[key] = time.Now()
	hub.consecutiveFailures[key] = 3
	hub.completedMu.Unlock()

	completeEvidenceLess(hub, key)

	hub.completedMu.Lock()
	defer hub.completedMu.Unlock()
	if _, ok := hub.failedTasks[key]; !ok {
		t.Error("evidence-less completion cleared failedTasks; the quarantine window must survive it")
	}
	if got := hub.consecutiveFailures[key]; got != 3 {
		t.Errorf("consecutiveFailures = %d, want 3 preserved — resetting it on a "+
			"chrome_idle completion lets an issue evade quarantine forever", got)
	}
}

// TestCompletionEvidenceIsHonoured: each of the three accepted kinds of
// evidence must switch the normal (pre-#6723) path back on, including when the
// relay also reports chrome_idle.
func TestCompletionEvidenceIsHonoured(t *testing.T) {
	t.Run("verified PR", func(t *testing.T) {
		hub, _ := covK2Hub(t)
		key := noPRKey("evidence-gate/pr", 3)
		hub.markTaskCompletedVerdictKeySignal(key, "https://github.com/o/r/pull/1",
			completionVerdictShipped, "relay", "", completionSignalChromeIdle)
		if got, want := recordedCooldown(t, hub, key), hub.configuredWithPRCooldown(); got != want {
			t.Fatalf("cooldown = %v, want the full with-PR window %v: a verified PR is "+
				"the strongest evidence there is and outranks the signal", got, want)
		}
	})

	t.Run("no_work_needed verdict", func(t *testing.T) {
		hub, _ := covK2Hub(t)
		key := noPRKey("evidence-gate/verdict", 4)
		hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictNoWorkNeeded,
			"relay", "maintainer_gated", completionSignalChromeIdle)
		hub.completedMu.Lock()
		_, booked := hub.noWorkVerdicts[key]
		hub.completedMu.Unlock()
		if !booked {
			t.Fatal("an affirmative no_work_needed verdict is evidence of a conclusion " +
				"and must still book the offer-suppression ledger")
		}
	})

	// The compatibility guarantee, and the reason the predicate keys off
	// chrome_idle specifically rather than "not verdict": every relay predating
	// #5376 and the whole headless path omit the field, normalizing to unknown.
	// Those must behave EXACTLY as before, or upgrading the hub silently changes
	// cooldown behaviour for every existing deployment.
	t.Run("absent signal still escalates", func(t *testing.T) {
		hub, _ := covK2Hub(t)
		key := noPRKey("evidence-gate/legacy-relay", 5)
		base := completedNoPRCooldownHours * time.Hour

		hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictIdle, "relay", "", "")
		if got := recordedCooldown(t, hub, key); got != base {
			t.Fatalf("first completion: cooldown = %v, want %v", got, base)
		}
		hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictIdle, "relay", "", "")
		if got := recordedCooldown(t, hub, key); got != base*2 {
			t.Fatalf("second completion: cooldown = %v, want %v — a relay that does not "+
				"report the signal must keep the pre-#6723 escalation", got, base*2)
		}
	})
}

// TestIsEvidenceLessCompletion pins the predicate directly, including the
// unknown/unrecognised cases that must NOT be treated as evidence-less.
func TestIsEvidenceLessCompletion(t *testing.T) {
	for _, tc := range []struct {
		name           string
		prURL, verdict string
		signal         string
		want           bool
	}{
		{"chrome_idle with nothing else", "", completionVerdictIdle, completionSignalChromeIdle, true},
		{"chrome_idle is case-insensitive", "", completionVerdictIdle, "Chrome_Idle", true},
		{"verified PR outranks signal", "https://github.com/o/r/pull/9", completionVerdictShipped, completionSignalChromeIdle, false},
		{"whitespace-only PR is no PR", "   ", completionVerdictIdle, completionSignalChromeIdle, true},
		{"no_work_needed is a conclusion", "", completionVerdictNoWorkNeeded, completionSignalChromeIdle, false},
		{"explicit verdict signal", "", completionVerdictIdle, completionSignalVerdict, false},
		{"absent signal (pre-#5376 relay)", "", completionVerdictIdle, "", false},
		{"unrecognised signal", "", completionVerdictIdle, "something-new", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEvidenceLessCompletion(tc.prURL, tc.verdict, tc.signal); got != tc.want {
				t.Errorf("isEvidenceLessCompletion(%q, %q, %q) = %v, want %v",
					tc.prURL, tc.verdict, tc.signal, got, tc.want)
			}
		})
	}
}
