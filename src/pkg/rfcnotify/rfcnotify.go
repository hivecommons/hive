// Package rfcnotify posts an RFC issue's lifecycle to a Discord channel:
// opened, closed (completed or not planned), and taken off hold so it can be
// implemented. The detector is a pure function over successive issue
// snapshots plus a small persisted per-issue record, so a restart never
// re-posts a transition it already announced and a polling hive needs no
// webhook to see the change.
//
// An issue is an RFC when it carries the `rfc` label or its title is
// prefixed with RFC (`RFC: ...`, `[RFC] ...`, `📝 RFC: ...`), which is the
// convention hivecommons/hive uses (for example #12062).
package rfcnotify

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// RFCLabel marks an issue as an RFC regardless of its title.
const RFCLabel = "rfc"

// Kind is one announced transition.
type Kind string

const (
	// KindOpened: a new RFC issue appeared.
	KindOpened Kind = "opened"
	// KindClosed: an RFC issue went from open to closed.
	KindClosed Kind = "closed"
	// KindReady: an open RFC that was on hold is no longer held, or moved
	// to the actionable stage while held.
	KindReady Kind = "ready"
)

const (
	closedCompleted  = "completed"
	closedNotPlanned = "not planned"
)

// Issue is the detector's view of one issue at one point in time.
type Issue struct {
	Repo        string
	Number      int
	Title       string
	URL         string
	Author      string
	State       string
	StateReason string
	Labels      []string
	CreatedAt   time.Time
	// Private is true when the issue's repository is private, or its
	// visibility could not be resolved (fail closed). Such issues are not
	// posted unless Options.IncludePrivate is set.
	Private bool
	// VisibilityUnknown is true when the lookup behind Private failed. The
	// sweeper retries such events next poll instead of dropping them.
	VisibilityUnknown bool
}

// Key is the per-issue identity the state is keyed by.
func (i Issue) Key() string { return fmt.Sprintf("%s#%d", i.Repo, i.Number) }

// Open reports whether the issue is open.
func (i Issue) Open() bool { return strings.EqualFold(strings.TrimSpace(i.State), "open") }

// Event is one transition to announce.
type Event struct {
	Kind  Kind
	Issue Issue
}

// rfcTitleRE matches a title whose first word, after any leading emoji,
// punctuation and bracketed tags such as [v6], is RFC. "RFCs" and a
// parenthetical "(RFC #13617)" later in the title do not match.
var rfcTitleRE = regexp.MustCompile(`(?i)^(?:[^\p{L}\p{N}]*(?:\[[^\]]*\])?)*\s*\[?RFC\b`)

// IsRFC reports whether the issue is an RFC by label or title prefix.
func IsRFC(title string, labels []string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), RFCLabel) {
			return true
		}
	}
	title = strings.TrimSpace(title)
	loc := rfcTitleRE.FindStringIndex(title)
	if loc == nil {
		return false
	}
	// "RFC #123 follow-up" references an RFC rather than being one.
	return !strings.HasPrefix(strings.TrimSpace(title[loc[1]:]), "#")
}

// Held reports whether labels carry a hold label (Prow's `hold`, plus the
// spellings pkg/github treats as holds).
func Held(labels []string) bool { return github.HasHoldLabel(labels) }

// Actionable reports whether labels carry the actionable stage label.
func Actionable(labels []string) bool {
	want := stagelabels.Actionable.Label()
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

// ClosedReason renders GitHub's state_reason for a closed issue.
func ClosedReason(stateReason string) string {
	switch strings.ToLower(strings.TrimSpace(stateReason)) {
	case "not_planned", "not planned":
		return closedNotPlanned
	default:
		return closedCompleted
	}
}

// Format renders the Discord line for an event. The URL is wrapped in angle
// brackets so Discord does not add a preview card. includeRepo prefixes
// the issue number with its repo, for a hive that manages several.
func Format(e Event, includeRepo bool) string {
	ref := fmt.Sprintf("#%d", e.Issue.Number)
	if includeRepo && e.Issue.Repo != "" {
		ref = e.Issue.Repo + ref
	}
	tail := fmt.Sprintf("%s %s - <%s>", ref, strings.TrimSpace(e.Issue.Title), strings.TrimSpace(e.Issue.URL))
	switch e.Kind {
	case KindOpened:
		if a := strings.TrimSpace(e.Issue.Author); a != "" {
			return fmt.Sprintf("RFC opened: %s (by @%s)", tail, a)
		}
		return "RFC opened: " + tail
	case KindClosed:
		return fmt.Sprintf("RFC closed (%s): %s", ClosedReason(e.Issue.StateReason), tail)
	case KindReady:
		return "RFC ready to implement (hold removed): " + tail
	}
	return "RFC " + string(e.Kind) + ": " + tail
}
