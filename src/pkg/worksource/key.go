package worksource

import (
	"fmt"
	"strconv"
	"strings"
)

// externalKeySeparator joins a repository to a string-keyed source's native
// identifier. It is "!" because that character cannot appear in a GitHub issue
// number, a Linear key ("ENG-123"), or a Jira key ("ENG-42"), so a key from one
// source can never be mistaken for a key from another — and, critically, can
// never collide with the "#" form GitHub-backed work has always used.
const externalKeySeparator = "!"

// Ref is the canonical, source-aware identity of one work item: the single
// thing scheduler kicks, contributor admission, holds, cooldowns, claims, and
// active-work tracking key on.
//
// It exists because those sites each rebuilt `fmt.Sprintf("%s#%d", repo, num)`
// independently. That is correct for GitHub and silently wrong for everything
// else: a Linear or Jira item arrives with Number == 0, so every one of them
// formatted to "repo#0" and they all collapsed onto ONE identity — one hold,
// one cooldown, one active-task slot, shared between unrelated work.
//
// The zero value is not a usable identity; callers get "" from Key() and must
// treat that as "not identifiable", never as a key.
type Ref struct {
	// SourceType is the work source that produced the item ("github",
	// "github_projects", "linear", "jira"). Empty is treated as GitHub for
	// backward compatibility with envelopes written before this field existed.
	SourceType string
	// Repo is the GitHub repository (owner/name) work happens against. It scopes
	// every key, so the same external ID in two repositories stays distinct.
	Repo string
	// ExternalID is the source-native identifier ("ENG-123"). For GitHub-backed
	// items it is the decimal issue number as a string.
	ExternalID string
	// Number is the GitHub issue number, or 0 for a string-keyed source. It is
	// what makes an item GitHub-backed, and the only thing the GitHub-only
	// observers (PR claims, bead dependencies) can act on.
	Number int
	// URL is the canonical web URL of the item, carried so a contributor sees
	// where non-GitHub work actually lives.
	URL string
}

// Key returns the canonical identity string.
//
// For GitHub-backed work it is byte-identical to the "repo#number" form every
// call site used before this type existed. That is a hard compatibility
// requirement, not a stylistic one: operator hold lists, queue order, completion
// cooldowns, failure quarantine and the PR-claim ledger are all PERSISTED under
// these strings, so changing the GitHub spelling would silently discard every
// existing exclusion on the next restart and re-dispatch claimed or
// cooling-down work.
//
// For a string-keyed source it is "repo!externalID".
//
// A Ref with no repository, or with neither a positive number nor an external
// ID, has no identity and returns "". Callers must skip such an item rather
// than indexing it — a fabricated key is exactly the "#0" collapse this type
// exists to prevent.
func (r Ref) Key() string {
	if r.Repo == "" {
		return ""
	}
	if r.Number > 0 {
		return fmt.Sprintf("%s#%d", r.Repo, r.Number)
	}
	if r.ExternalID == "" {
		return ""
	}
	return r.Repo + externalKeySeparator + r.ExternalID
}

// IsGitHubIssue reports whether this item is backed by a real GitHub issue
// number, and is therefore a legitimate subject for the GitHub-only observers:
// the PR-claim ledger and the legacy bead dependency gate.
//
// A GitHub Projects item backed by an actual issue number answers true here and
// keeps its existing claim and dependency behavior. A Linear or Jira item
// answers false and must be SKIPPED by those observers — never handed to them
// under a fabricated "#0", which would match an unrelated record or invent one.
func (r Ref) IsGitHubIssue() bool {
	return r.Repo != "" && r.Number > 0
}

// Display returns the short human form used in kick messages and prompts:
// "#42" for GitHub-backed work, the native key ("ENG-123") otherwise. It never
// renders "#0".
func (r Ref) Display() string {
	if r.Number > 0 {
		return "#" + strconv.Itoa(r.Number)
	}
	return r.ExternalID
}

// RefFromIssue builds the canonical reference for an enumerated work item.
func RefFromIssue(issue Issue) Ref {
	return Ref{
		SourceType: issue.SourceType,
		Repo:       issue.Repo,
		ExternalID: issue.ExternalID,
		Number:     issue.Number,
		URL:        issue.URL,
	}
}

// TaskKey returns the canonical identity key for an enumerated work item.
//
// Retained as the package's original entry point; it is now one line over Ref
// so there is exactly ONE implementation of the key format in the tree. A
// second one is the failure mode this whole change is about: it passes every
// helper-level test while production keeps formatting keys its own way.
func TaskKey(issue Issue) string {
	return RefFromIssue(issue).Key()
}

// ParseKey recovers a Ref from a canonical key string. It is the reader half of
// Key(), needed because operator controls — the hold list and the queue order
// override — persist bare key strings in config and must be matched against
// live items without every handler re-deriving the format.
//
// It reports ok=false for anything that is not a well-formed key, so a
// malformed or hand-edited entry is skipped rather than silently matching
// nothing (or, worse, matching the wrong item).
//
// The "#" form is tried FIRST and its number must parse as a positive integer,
// which keeps "owner/repo#12" GitHub-backed and refuses "owner/repo#0" —
// exactly the fabricated identity that must never be admitted.
func ParseKey(raw string) (Ref, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Ref{}, false
	}
	if repo, num, found := strings.Cut(raw, "#"); found {
		repo = strings.TrimSpace(repo)
		n, err := strconv.Atoi(strings.TrimSpace(num))
		if repo == "" || err != nil || n <= 0 {
			return Ref{}, false
		}
		return Ref{Repo: repo, Number: n, ExternalID: strconv.Itoa(n)}, true
	}
	if repo, ext, found := strings.Cut(raw, externalKeySeparator); found {
		repo = strings.TrimSpace(repo)
		ext = strings.TrimSpace(ext)
		if repo == "" || ext == "" {
			return Ref{}, false
		}
		return Ref{Repo: repo, ExternalID: ext}, true
	}
	return Ref{}, false
}
