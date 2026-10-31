// Package prstatus renders the one hive status comment a pull request
// carries in github.notifications.mode: quiet. Every new issue comment
// GitHub sends emails the author and every watcher; editing one sends
// nothing. So instead of one comment per note (stage stalled, stage
// regressed, the trusted-human 071 note, the duplicate-PR redirect, …) the
// hive keeps a single comment tagged Marker and rewrites one keyed section
// of it in place. The package is pure string handling; the GitHub client
// and the auto-merge engine each own the list/edit/create calls around it.
package prstatus

import (
	"strings"
)

// Marker tags the status comment so a later pass finds it. Exactly one
// comment per PR carries it.
const Marker = "<!-- hive:status -->"

// Heading opens the comment body. It names the contract so a reader
// understands why the comment keeps changing under them.
const Heading = "**Hive status** — updated in place; edits do not notify. Full history is in the Activity Monitor."

const (
	sectionOpenPrefix = "<!-- hive:status:section="
	sectionOpenSuffix = " -->"
	sectionClose      = "<!-- /hive:status:section -->"
)

// Section is one keyed block of the status comment.
type Section struct {
	Key  string
	Body string
}

// Parse splits a status comment body into its sections, in order. Text
// outside a section (the heading, anything a person typed) is dropped: the
// comment is the hive's, and Render rebuilds the frame.
func Parse(body string) []Section {
	var out []Section
	rest := body
	for {
		i := strings.Index(rest, sectionOpenPrefix)
		if i < 0 {
			return out
		}
		rest = rest[i+len(sectionOpenPrefix):]
		j := strings.Index(rest, sectionOpenSuffix)
		if j < 0 {
			return out
		}
		key := strings.TrimSpace(rest[:j])
		rest = rest[j+len(sectionOpenSuffix):]
		k := strings.Index(rest, sectionClose)
		if k < 0 {
			out = append(out, Section{Key: key, Body: strings.TrimSpace(rest)})
			return out
		}
		out = append(out, Section{Key: key, Body: strings.TrimSpace(rest[:k])})
		rest = rest[k+len(sectionClose):]
	}
}

// Render builds the comment body from its sections.
func Render(sections []Section) string {
	var b strings.Builder
	b.WriteString(Marker)
	b.WriteString("\n")
	b.WriteString(Heading)
	for _, s := range sections {
		b.WriteString("\n\n")
		b.WriteString(sectionOpenPrefix)
		b.WriteString(s.Key)
		b.WriteString(sectionOpenSuffix)
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(s.Body))
		b.WriteString("\n")
		b.WriteString(sectionClose)
	}
	b.WriteString("\n")
	return b.String()
}

// Upsert returns existing with the section key set to text: replaced in
// place when present, appended otherwise. An empty text removes the
// section. existing may be "" (no status comment yet).
func Upsert(existing, key, text string) string {
	sections := Parse(existing)
	text = strings.TrimSpace(text)
	out := make([]Section, 0, len(sections)+1)
	replaced := false
	for _, s := range sections {
		if s.Key != key {
			out = append(out, s)
			continue
		}
		if text == "" || replaced {
			continue
		}
		out = append(out, Section{Key: key, Body: text})
		replaced = true
	}
	if !replaced && text != "" {
		out = append(out, Section{Key: key, Body: text})
	}
	return Render(out)
}

// Get returns the body of section key and whether it exists.
func Get(existing, key string) (string, bool) {
	for _, s := range Parse(existing) {
		if s.Key == key {
			return s.Body, true
		}
	}
	return "", false
}

// IsStatusComment reports whether body is the hive status comment.
func IsStatusComment(body string) bool {
	return strings.Contains(body, Marker)
}

// Section keys the hive writes. Each replaces what used to be a standalone
// comment carrying the legacy `<!-- hive:… -->` marker named alongside it;
// the legacy marker stays inside the section text so existing readers that
// grep for it still find it.
const (
	KeyStageStalled   = "stage-stalled"             // <!-- hive:stage-stalled -->
	KeyStageRegressed = "stage-regressed"           // <!-- hive:stage-regressed -->
	KeyHuman071Note   = "human-071-note"            // <!-- hive:human-071-note … -->
	KeyDuplicateGuard = "create-pr-duplicate-guard" // <!-- hive:create-pr-duplicate-guard -->
	// KeyReviewFindingsPrefix + perspective holds a perspective's findings
	// when the head already carries the hive review it may have, so the
	// findings are kept without a second review submission.
	KeyReviewFindingsPrefix = "review-findings:"
	// KeyEvidence is the post-merge pointer at the PR's evidence bundle
	// (<!-- hive-review-evidence -->).
	KeyEvidence = "evidence"
	// KeyUnblocked is the note on a blocked issue that its blocking PR
	// settled (<!-- hive:unblocked -->).
	KeyUnblocked = "unblocked"
	// KeyReviewBacklog lists the non-blocking review findings filed as
	// backlog issues (<!-- hive:review-backlog-summary v1 -->).
	KeyReviewBacklog = "review-backlog"
	// KeyClaim holds a contributor's `<!-- hive:claim … -->` marker; claim
	// readers scan every hive comment body for it, so a section carries it
	// as well as a comment of its own did.
	KeyClaim = "claim"
	// KeyAgentNotePrefix + kind holds an agent's relayed status note (the
	// `hive/awaiting-ci:` deferral, a stage/045 parking note, an
	// "Already fixed" verdict) that used to be its own comment.
	KeyAgentNotePrefix = "agent-note:"
)
