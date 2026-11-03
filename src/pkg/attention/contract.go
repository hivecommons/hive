package attention

import (
	"errors"
	"fmt"
	"strings"
)

// NotApproval is the contract wording every attention event carries so a
// recipient never mistakes `/hive ack` for a release.
const NotApproval = "Acknowledgement is not approval: `/hive ack` and `/hive snooze` only quiet reminders. They release no security, merge or review hold, change no label and open no merge gate."

// Event is the attention action contract. Every admitted attention event must
// say who is asked, what changed and why now, the requested action, the
// evidence, what Hive already tried, what happens next, and how to ack,
// snooze or resolve it. Producers build Observation bodies with NewObservation
// so an incomplete event is refused rather than paged.
type Event struct {
	// Who is the person or team asked to act (normally Key.Recipient).
	Who string
	// WhyNow is what changed and why it needs attention now.
	WhyNow string
	// Action is the single concrete thing the recipient is asked to do.
	Action string
	// Evidence links or names what the condition was observed from.
	Evidence string
	// Tried is what Hive already attempted on its own.
	Tried string
	// Next is what Hive will do if nobody acts.
	Next string
	// Resolve says what clears the condition (so the episode resolves).
	Resolve string
}

// Validate refuses an event missing any contract field.
func (e Event) Validate() error {
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"who", e.Who}, {"why now", e.WhyNow}, {"requested action", e.Action}, {"evidence", e.Evidence},
		{"what Hive tried", e.Tried}, {"what happens next", e.Next}, {"how to resolve", e.Resolve},
	} {
		if strings.TrimSpace(f.v) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return errors.New("attention: event missing contract fields: " + strings.Join(missing, ", "))
	}
	return nil
}

// Render emits every contract field, the ack/snooze/resolve instructions and
// the NotApproval wording.
func (e Event) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Who:** %s\n", oneLine(e.Who))
	fmt.Fprintf(&b, "**What changed / why now:** %s\n", oneLine(e.WhyNow))
	fmt.Fprintf(&b, "**Requested action:** %s\n", oneLine(e.Action))
	fmt.Fprintf(&b, "**Evidence:** %s\n", oneLine(e.Evidence))
	fmt.Fprintf(&b, "**What Hive tried:** %s\n", oneLine(e.Tried))
	fmt.Fprintf(&b, "**What happens next:** %s\n", oneLine(e.Next))
	b.WriteString("**How to ack, snooze or resolve:**\n")
	b.WriteString("- `/hive ack` — you have seen this; reminders stop for this episode.\n")
	b.WriteString("- `/hive snooze <duration>` (for example `4h`, `2d`; at most 30d) — reminders pause until then.\n")
	fmt.Fprintf(&b, "- Resolve: %s\n", oneLine(e.Resolve))
	b.WriteString("\n" + NotApproval + "\n")
	return b.String()
}

// NewObservation builds an observation whose body satisfies the contract.
func NewObservation(key Key, actionable bool, evidence string, ev Event) (Observation, error) {
	if err := ev.Validate(); err != nil {
		return Observation{}, err
	}
	return Observation{Key: key, Actionable: actionable, Evidence: evidence, Body: ev.Render()}, nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
