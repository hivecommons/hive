package claims

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The claim ledger is the one comment per issue that records who holds it.
// Before it existed every claim, takeover and release was its own comment,
// and a relaunch/revoke cycle appended a fresh release+claim pair each time
// (334 comments in 48h, a quarter of the hive's output). The ledger is
// edited in place instead: editing notifies nobody, while the facts stay on
// the issue.
//
// Compatibility with marker readers (pkg/issueclaim.Latest, the dashboard's
// contribute claims view, the contributor lane) rests on two rules:
//
//   - the ledger body carries EXACTLY ONE machine-readable marker for the
//     current state — a `hive:claim` marker while held, a `hive:release`
//     marker once released — so a reader that scans comment bodies sees the
//     same thing it saw when each transition was its own comment;
//   - the history lines never contain a marker prefix, and LedgerMarker
//     itself does not start with `<!-- hive:claim` or `<!-- hive:release`
//     (`<!-- hive:claims-ledger` would, and would be read as a malformed
//     claim).
const (
	// LedgerMarker identifies the per-issue claim ledger comment.
	LedgerMarker = "<!-- hive:issue-claim-ledger -->"
	// LedgerHistoryLimit is how many transitions the ledger keeps.
	LedgerHistoryLimit = 8

	ledgerHistoryOpen  = "<details><summary>Claim history</summary>"
	ledgerHistoryClose = "</details>"
	ledgerLinePrefix   = "- "
)

// LedgerEvent is one transition the ledger records.
type LedgerEvent struct {
	// Claim is the claim the event concerns: the new holder for a claim or
	// takeover, the released holder for a release.
	Claim Claim
	// Previous is the displaced holder on a takeover; zero otherwise.
	Previous Claim
	// Released is set for a release; Reason is its reason.
	Released bool
	Reason   string
	// At is when the transition happened.
	At time.Time
}

// LedgerComment renders the ledger body after event, given the body of the
// existing ledger comment (empty when the issue has none yet). The current
// state is rendered exactly as the standalone claim / takeover / release
// comment used to be, so the marker a reader finds is unchanged; the history
// below it keeps the last LedgerHistoryLimit transitions, oldest first.
func LedgerComment(previous string, event LedgerEvent) string {
	if event.At.IsZero() {
		event.At = time.Now()
	}
	history := ParseLedgerHistory(previous)
	history = append(history, ledgerHistoryLine(event))
	if len(history) > LedgerHistoryLimit {
		history = history[len(history)-LedgerHistoryLimit:]
	}
	var b strings.Builder
	b.WriteString(LedgerMarker)
	b.WriteString("\n")
	switch {
	case event.Released:
		b.WriteString(releaseCommentAt(event.Claim, event.Reason, event.At))
	case event.Previous.Holder != "":
		b.WriteString(TakeoverComment(event.Claim, event.Previous))
	default:
		b.WriteString(ClaimComment(event.Claim))
	}
	b.WriteString("\n\n")
	b.WriteString(ledgerHistoryOpen)
	b.WriteString("\n\n")
	for _, line := range history {
		b.WriteString(ledgerLinePrefix)
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(ledgerHistoryClose)
	return b.String()
}

// ParseLedgerHistory returns the history lines of a ledger body, oldest
// first, without their bullet. A body that is not a ledger yields none.
func ParseLedgerHistory(body string) []string {
	if !strings.Contains(body, LedgerMarker) {
		return nil
	}
	start := strings.Index(body, ledgerHistoryOpen)
	if start < 0 {
		return nil
	}
	rest := body[start+len(ledgerHistoryOpen):]
	if end := strings.Index(rest, ledgerHistoryClose); end >= 0 {
		rest = rest[:end]
	}
	var lines []string
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, ledgerLinePrefix) {
			continue
		}
		if line = strings.TrimSpace(strings.TrimPrefix(line, ledgerLinePrefix)); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// LedgerMentions reports whether the ledger body already names holder:
// a human holder the ledger has never named is owed one notifying comment,
// since an edit emails nobody.
func LedgerMentions(body, holder string) bool {
	holder = strings.TrimPrefix(strings.TrimSpace(holder), "@")
	if body == "" || holder == "" {
		return false
	}
	lower := strings.ToLower(body)
	holder = strings.ToLower(holder)
	return strings.Contains(lower, "@"+holder) || strings.Contains(lower, "`"+holder+"`")
}

// LedgerNotifiesHuman reports whether a claim by c should also post a
// notifying comment: the holder is a person (the claim comment @-mentions
// them) and the ledger has not named them before.
func LedgerNotifiesHuman(previous string, c Claim) bool {
	switch c.Kind {
	case KindHuman, KindExternal, KindContributor:
	default:
		return false
	}
	return !LedgerMentions(previous, c.Holder)
}

// LedgerNoticeComment is the one notifying comment a newly named human
// holder gets. It carries no claim marker: the ledger is the record, this
// is only the email.
func LedgerNoticeComment(c Claim) string {
	return fmt.Sprintf("%s you now hold this issue (%s%s); the claim ledger comment above tracks it. Reply `/unclaim` to release.", mention(c), c.Kind, hiveSuffix(c))
}

// LedgerDisplacedNotice is the notifying comment a displaced human holder
// gets on a takeover (an edit would tell them nothing); "" when the
// displaced holder is not a person.
func LedgerDisplacedNotice(now, previous Claim) string {
	switch previous.Kind {
	case KindHuman, KindExternal, KindContributor:
	default:
		return ""
	}
	return fmt.Sprintf("%s: your claim on this issue was taken over by %s (%s). Stop work on this item and pick up something else; the claim ledger comment above has the details.", mention(previous), mention(now), now.Kind)
}

// ledgerHistoryLine renders one transition. It is prose only: the marker
// prefixes stay out of the history so a reader finds exactly one.
func ledgerHistoryLine(e LedgerEvent) string {
	at := e.At.UTC().Format("2006-01-02 15:04 UTC")
	who := ledgerName(e.Claim)
	var line string
	switch {
	case e.Released:
		reason := strings.TrimSpace(e.Reason)
		if reason == "" {
			reason = "released"
		}
		line = fmt.Sprintf("%s — released by %s (%s; %s)", at, who, e.Claim.Kind, reason)
	case e.Previous.Holder != "":
		line = fmt.Sprintf("%s — taken over by %s (%s) from %s (%s)", at, who, e.Claim.Kind, ledgerName(e.Previous), e.Previous.Kind)
	default:
		line = fmt.Sprintf("%s — claimed by %s (%s%s)", at, who, e.Claim.Kind, hiveSuffix(e.Claim))
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(line, "<!--", "")), " ")
}

// ledgerName names a holder in history without an @-mention: the history
// must not re-notify everyone named in it on every edit.
func ledgerName(c Claim) string {
	return "`" + strings.TrimPrefix(c.Holder, "@") + "`"
}

// ledgerHistoryRe reads the holder and kind back out of a history line.
var ledgerHistoryRe = regexp.MustCompile("— (claimed|released|taken over) by `[^`]+` \\(([a-z]+)[^)]*\\)(?: from `[^`]+` \\(([a-z]+)\\))?")

// LedgerHumanReleased reports whether the ledger's history shows the last
// human principal who held the issue letting go (or being displaced by an
// agent). The standalone comments kept a human's claim and release markers
// side by side forever; the ledger rewrites them away, so the claimed-by
// label reconciler (github.ApplyClaimedByLabel) reads the history instead.
func LedgerHumanReleased(body string) bool {
	released := false
	for _, line := range ParseLedgerHistory(body) {
		m := ledgerHistoryRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch m[1] {
		case "claimed":
			if humanKind(m[2]) {
				released = false
			}
		case "released":
			if humanKind(m[2]) {
				released = true
			}
		case "taken over":
			switch {
			case humanKind(m[2]):
				released = false
			case humanKind(m[3]):
				released = true
			}
		}
	}
	return released
}

func humanKind(kind string) bool {
	switch Kind(kind) {
	case KindHuman, KindExternal, KindContributor:
		return true
	}
	return false
}
