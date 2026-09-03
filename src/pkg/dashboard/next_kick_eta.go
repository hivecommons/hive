package dashboard

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Next-kick ETA (#5594). The agent card's one-line state summary answers "how
// long until this agent does something". computeNextKickFromCadence returns a
// wall-clock stamp and leaves the subtraction to the reader; this is the same
// instant expressed as a wait.

// computeNextKickETA is empty exactly when computeNextKickFromCadence is empty,
// so the card can never show an ETA beside a schedule its own fields row calls
// "paused".
func computeNextKickETA(lastKick *time.Time, cadence config.Cadence) string {
	if cadence == "" || cadence.IsPaused() {
		return ""
	}
	now := time.Now()
	base := now
	if lastKick != nil && cadence.Mode() == config.CadenceModeInterval {
		base = *lastKick
	}
	next, ok := cadence.NextAfter(base)
	if !ok {
		return ""
	}
	return formatETA(next.Sub(now))
}

// formatETA humanises a wait. A time that has already passed reads "due now"
// rather than a negative duration: the governor fires it on the next tick, and
// a leading minus sign read as a bug.
func formatETA(d time.Duration) string {
	if d.Seconds() <= 0 {
		return "due now"
	}
	if d.Seconds() < 60 {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	if d.Minutes() < 60 {
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}
