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
//
// In rich mode an RFC document proposed as a pull request is tracked too:
// a PR is an RFC when its title starts with `📝 proposal:` or `📝 RFC`, or
// it adds or changes docs/rfc-*.md or src/docs/rfc-*.md (IsRFCPR).
package rfcnotify

import (
	"fmt"
	"path"
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
	// KindDocAccepted: an RFC document pull request was merged.
	KindDocAccepted Kind = "doc_accepted"
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
	// IsPR marks a pull request (an RFC document proposal). PRs share the
	// repo#N key space with issues.
	IsPR bool
	// Body is the PR description, read for the linked RFC issue.
	Body string
	// Files are the PR's added or changed paths. Only filled when the
	// title alone does not mark the PR as an RFC.
	Files []string
	// DocTitle is the proposed document's first heading, when known.
	DocTitle string
	// MergedAt is when the PR merged; zero when it has not.
	MergedAt time.Time
}

// Key is the per-issue identity the state is keyed by.
func (i Issue) Key() string { return fmt.Sprintf("%s#%d", i.Repo, i.Number) }

// RFC reports whether the issue (IsRFC) or pull request (IsRFCPR) is an RFC.
func (i Issue) RFC() bool {
	if i.IsPR {
		return IsRFCPR(i.Title, i.Files)
	}
	return IsRFC(i.Title, i.Labels)
}

// Merged reports whether the pull request merged.
func (i Issue) Merged() bool { return i.IsPR && !i.MergedAt.IsZero() }

// Open reports whether the issue is open.
func (i Issue) Open() bool { return strings.EqualFold(strings.TrimSpace(i.State), "open") }

// Event is one transition to announce.
type Event struct {
	Kind  Kind
	Issue Issue
	// Thread is the key of the RFC whose thread the event belongs in: the
	// issue's own key, or for KindDocAccepted the linked RFC issue when the
	// detector knows it.
	Thread string
	// LinkedRFC is the linked RFC issue number behind Thread, or 0.
	LinkedRFC int
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

// IsRFCPR reports whether a pull request proposes an RFC document: its title
// starts with `📝 proposal:` or `📝 RFC`, or files add or change
// docs/rfc-*.md or src/docs/rfc-*.md.
func IsRFCPR(title string, files []string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	if strings.HasPrefix(t, "📝 proposal:") || strings.HasPrefix(t, "📝 rfc") {
		return true
	}
	for _, f := range files {
		if IsRFCDoc(f) {
			return true
		}
	}
	return false
}

// IsRFCDoc reports whether a repository path is an RFC document.
func IsRFCDoc(p string) bool {
	for _, pattern := range []string{"docs/rfc-*.md", "src/docs/rfc-*.md"} {
		if ok, _ := path.Match(pattern, strings.TrimSpace(p)); ok {
			return true
		}
	}
	return false
}

var headingRE = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*#*\s*$`)

// FirstHeading returns the first Markdown ATX heading's text, or "".
func FirstHeading(markdown string) string {
	for _, line := range strings.Split(markdown, "\n") {
		if m := headingRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return ""
}

// linkRE finds issue references in a PR body: closing keywords and Refs /
// Part of with #N or owner/repo#N, and GitHub issue URLs.
var linkRE = regexp.MustCompile(`(?i)(?:\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?|refs?|part of)\s*:?\s+([\w.-]+/[\w.-]+)?#(\d+))|(?:https?://github\.com/([\w.-]+/[\w.-]+)/issues/(\d+))`)

// LinkedIssueKeys returns the repo#N keys a PR body references, in order
// of appearance, without duplicates. A bare #N belongs to repo.
func LinkedIssueKeys(repo, body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range linkRE.FindAllStringSubmatch(body, -1) {
		r, n := m[1], m[2]
		if m[4] != "" {
			r, n = m[3], m[4]
		}
		if r == "" {
			r = repo
		}
		key := r + "#" + n
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
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
	title := strings.TrimSpace(e.Issue.Title)
	if d := strings.TrimSpace(e.Issue.DocTitle); d != "" {
		title = d
	}
	tail := fmt.Sprintf("%s %s - <%s>", ref, title, strings.TrimSpace(e.Issue.URL))
	switch e.Kind {
	case KindOpened:
		if e.Issue.IsPR {
			if a := strings.TrimSpace(e.Issue.Author); a != "" {
				return fmt.Sprintf("RFC doc proposed: %s (by @%s)", tail, a)
			}
			return "RFC doc proposed: " + tail
		}
		if a := strings.TrimSpace(e.Issue.Author); a != "" {
			return fmt.Sprintf("RFC opened: %s (by @%s)", tail, a)
		}
		return "RFC opened: " + tail
	case KindClosed:
		return fmt.Sprintf("RFC closed (%s): %s", ClosedReason(e.Issue.StateReason), tail)
	case KindReady:
		return "RFC ready to implement (hold removed): " + tail
	case KindDocAccepted:
		if e.LinkedRFC > 0 {
			return fmt.Sprintf("RFC doc accepted (RFC #%d): %s", e.LinkedRFC, tail)
		}
		return "RFC doc accepted: " + tail
	}
	return "RFC " + string(e.Kind) + ": " + tail
}
