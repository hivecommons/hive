package recommend

import (
	"fmt"
	"strings"
)

// startHere names a single next action.
//
// A digest that opens with five sections is still a reading task, and a
// maintainer looking at several hundred open items has already learned that
// opening the queue costs more time than they have. One sentence naming one
// pull request is the difference between a page that gets skimmed and one
// that gets acted on.
//
// The pick is the oldest item in the highest-value actionable bucket, which
// is also the one whose author has been waiting longest.
func (r Report) startHere() string {
	for _, bucket := range []Bucket{BucketReady, BucketDecision, BucketConflicts} {
		items := r.Buckets[bucket]
		if len(items) == 0 {
			continue
		}
		it := items[0]
		switch bucket {
		case BucketReady:
			return fmt.Sprintf("**Start here:** %s — ready to merge%s.", link(it), waitedFor(it))
		case BucketDecision:
			return fmt.Sprintf("**Start here:** %s — waiting on your decision%s.", link(it), waitedFor(it))
		case BucketConflicts:
			return fmt.Sprintf("**Start here:** %s — %s%s.", link(it), it.Note, waitedFor(it))
		}
	}
	return ""
}

func waitedFor(it Item) string {
	if it.AgeDays <= 0 {
		return ""
	}
	return ", open " + humanDays(it.AgeDays)
}

// autonomyNote is the trust statement.
//
// The hive on this fleet does not merge, approve, or close anything, and the
// reason a maintainer would ever trust it to is that it spent a long time
// being useful without asking to. Saying so plainly, every cycle, is what
// keeps a digest full of merge commands from reading like an automation
// lobbying for the keys. The commands are the reader's to run or ignore.
const autonomyNote = "The hive does not merge, approve, or close anything here — every command above is yours to run, edit, or ignore."

func withAutonomyNote(b *strings.Builder) {
	b.WriteString(autonomyNote)
	b.WriteString("\n")
}
