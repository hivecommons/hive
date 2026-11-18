package dashboard

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// A required check that has been queued or in progress far longer than a
// check run normally takes is stuck, not running: its reporter died or
// could not write the result (hivecommons/hive#18764 left `PR Type` and
// `PR Issue` in progress for hours), and nothing finishes it without a
// re-run. Only check runs are judged (their start time is known; a pending
// commit status carries when it was posted). The CI track names such checks instead of presenting them as
// motion (hivecommons/hive#18769).

// trackerCheckStuckAfterEnv overrides trackerCheckStuckAfterDefault with a
// Go duration ("45m", "3h").
const trackerCheckStuckAfterEnv = "HIVE_TRACKER_CHECK_STUCK_AFTER"

// trackerCheckStuckAfterDefault is how long a required check may stay
// queued or in progress before the tracker calls it stuck. It sits well
// past the slowest pipeline the fleet runs, so a long image build is never
// called stuck.
const trackerCheckStuckAfterDefault = 2 * time.Hour

// trackerCheckStuckAfter resolves the stuck threshold.
func trackerCheckStuckAfter() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(trackerCheckStuckAfterEnv)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return trackerCheckStuckAfterDefault
}

// stageTrackerMarkStuckChecks names the pending checks whose queued or
// running observation began more than the stuck threshold ago and, when
// every check the head still waits on is one of them, says so in the
// caption: the head is not running, it is stuck. pendingSince maps a
// pending check to when that observation began.
func stageTrackerMarkStuckChecks(out *StageTrackerActivity, pendingSince map[string]time.Time, now time.Time) {
	if out == nil || (out.State != "running" && out.State != "queued") || len(pendingSince) == 0 {
		return
	}
	after := trackerCheckStuckAfter()
	for _, name := range out.ChecksPending {
		since, ok := pendingSince[name]
		if !ok || since.IsZero() || now.Sub(since) <= after {
			continue
		}
		out.ChecksStuck = append(out.ChecksStuck, name)
		if out.StuckSince == nil || since.Before(*out.StuckSince) {
			at := since
			out.StuckSince = &at
		}
	}
	if len(out.ChecksStuck) == 0 {
		return
	}
	names := make([]string, 0, len(out.ChecksStuck))
	for _, name := range out.ChecksStuck {
		names = append(names, stageTrackerCheckName(name))
	}
	span := stageTrackerHumanGateSpan(now.Sub(*out.StuckSince))
	out.Detail = strings.TrimSpace(out.Detail + fmt.Sprintf(" Stuck: %s %s been queued or in progress for %s, far longer than a check normally runs. Nothing finishes a stuck check on its own: re-run it, or fix whatever reports its result.",
		strings.Join(names, ", "), pluralVerb(len(names), "has", "have"), span))
	if len(out.ChecksStuck) < len(out.ChecksPending) {
		out.Caption += fmt.Sprintf(" · %d stuck", len(out.ChecksStuck))
		return
	}
	// Every check the head waits on is stuck: no motion to show.
	out.Caption = "CI stuck · " + strings.Join(names, ", ") + " in progress " + span
	out.Active = false
}

// stageTrackerAllChecksStuck reports a head whose every pending check is
// stuck: nothing on it is in motion.
func stageTrackerAllChecksStuck(a *StageTrackerActivity) bool {
	return a != nil && len(a.ChecksStuck) > 0 && len(a.ChecksStuck) >= len(a.ChecksPending)
}
