package github

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DedupeChecklistFindings is what a dedupe-agent checklist body asks a
// maintainer to DO, read off the body alone: the verdict on line two, the
// counts the Summary sentence states, and the `- [ ] #N` task lines the
// policy requires for every recommended action. A checklist with no action
// in it — no near-duplicate pair, no item to close as resolved, no missing
// PR↔issue link, no task line, verdict "no" — is a triage pass that found
// nothing, and the policy says such a pass files nothing: a merge-order
// reminder already declared in the PR body and a "related but distinct"
// cross-link are notes, not work (hivecommons/hive#15381 sat open for hours
// as "Open · since 3h" with exactly that content).
type DedupeChecklistFindings struct {
	// Repo is the owner/repo the marker names ("" when malformed).
	Repo string
	// DecisionRequired is true when line two says "Decision required: yes".
	DecisionRequired bool
	// NearDuplicates, Resolved and MissingLinks are the Summary counts
	// ("Found <a> near-duplicate pairs, … <c> items that appear already
	// resolved", "<d> missing PR↔issue links"); 0 when absent.
	NearDuplicates int
	Resolved       int
	MissingLinks   int
	// Clusters is the same-root-cause cluster count. It is informational:
	// a cluster row that asks for a close carries a task line, and one
	// that only cross-links is not an action.
	Clusters int
	// TaskItems counts the `- [ ]` lines: every recommended action is one.
	TaskItems int
}

// dedupeChecklistCountPatterns read the Summary sentence's counts. Each
// pattern captures the integer that precedes the phrase.
var (
	dedupeCountNearDupPattern  = regexp.MustCompile(`(?i)\b(\d+)\s+near-duplicate`)
	dedupeCountClusterPattern  = regexp.MustCompile(`(?i)\b(\d+)\s+same-root-cause`)
	dedupeCountResolvedPattern = regexp.MustCompile(`(?i)\b(\d+)\s+(?:items?\s+(?:that\s+)?(?:appear\s+)?(?:to\s+be\s+)?)?(?:already\s+)?resolved`)
	dedupeCountLinksPattern    = regexp.MustCompile(`(?i)\b(\d+)\s+missing\s+PR\s*(?:↔|<->|->|/|-|to)\s*issue\s+links?`)
	dedupeTaskLinePattern      = regexp.MustCompile(`(?m)^\s*[-*]\s+\[ \]`)
)

// ParseDedupeChecklistFindings reports whether body is a dedupe-agent
// checklist (it opens with the `<!-- hive:dedupe-checklist repo=… -->`
// marker) and, when it is, what the checklist asks for. Fenced code blocks
// are ignored so a template quoted in the body does not count as a finding.
func ParseDedupeChecklistFindings(body string) (DedupeChecklistFindings, bool) {
	repo, verdict, ok := parseDedupeChecklist(body)
	if !ok {
		return DedupeChecklistFindings{}, false
	}
	f := DedupeChecklistFindings{Repo: repo, DecisionRequired: verdict == dedupeVerdictYes}
	text := stripFencedCode(htmlCommentPattern.ReplaceAllString(body, ""))
	f.NearDuplicates = dedupeChecklistCount(dedupeCountNearDupPattern, text)
	f.Clusters = dedupeChecklistCount(dedupeCountClusterPattern, text)
	f.Resolved = dedupeChecklistCount(dedupeCountResolvedPattern, text)
	f.MissingLinks = dedupeChecklistCount(dedupeCountLinksPattern, text)
	f.TaskItems = len(dedupeTaskLinePattern.FindAllStringIndex(text, -1))
	return f, true
}

// Actionable reports whether the checklist asks a maintainer to close,
// relabel or edit anything. A verdict of "yes", any task line, or a
// positive near-duplicate / resolved / missing-link count is an action;
// clusters, merge-order notes and cross-links are not.
func (f DedupeChecklistFindings) Actionable() bool {
	return f.DecisionRequired || f.TaskItems > 0 || f.NearDuplicates > 0 || f.Resolved > 0 || f.MissingLinks > 0
}

// Summary is the one-line digest of the counts, for logs and beads.
func (f DedupeChecklistFindings) Summary() string {
	verdict := "no"
	if f.DecisionRequired {
		verdict = "yes"
	}
	return fmt.Sprintf("decision required: %s; %d near-duplicate pair(s), %d same-root-cause cluster(s), %d resolved, %d missing PR↔issue link(s), %d task line(s)",
		verdict, f.NearDuplicates, f.Clusters, f.Resolved, f.MissingLinks, f.TaskItems)
}

// IssueAdvisory is the body-derived part of "this issue is a maintainer
// advisory, not work", carried on an enumerated issue so the stage tracker
// can draw it without the body (the sidelined work-detail rows have none).
type IssueAdvisory struct {
	// DedupeChecklist is true when the body opens with the dedupe-checklist
	// marker; DecisionNo when its verdict line says "Decision required: no".
	DedupeChecklist bool `json:"dedupe_checklist,omitempty"`
	DecisionNo      bool `json:"decision_no,omitempty"`
	// Empty is true when the body is a dedupe checklist with nothing
	// actionable in it (DedupeChecklistFindings.Actionable is false).
	Empty bool `json:"empty,omitempty"`
}

// dedupeDecisionNoLinePattern finds a "Decision required: no" line anywhere
// near the top of a body that lacks the marker (an advisory a different
// lane wrote by hand to the same convention).
var dedupeDecisionNoLinePattern = regexp.MustCompile(`(?im)^\s*decision required:\s*no\b`)

// IssueAdvisoryFromBody classifies body; nil when it is neither a dedupe
// checklist nor carries a "Decision required: no" line.
func IssueAdvisoryFromBody(body string) *IssueAdvisory {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	if f, ok := ParseDedupeChecklistFindings(body); ok {
		return &IssueAdvisory{DedupeChecklist: true, DecisionNo: !f.DecisionRequired, Empty: !f.Actionable()}
	}
	if dedupeDecisionNoLinePattern.MatchString(stripFencedCode(body)) {
		return &IssueAdvisory{DecisionNo: true}
	}
	return nil
}

func dedupeChecklistCount(re *regexp.Regexp, text string) int {
	total := 0
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		total += n
	}
	return total
}

var fencedCodePattern = regexp.MustCompile("(?s)```.*?```")

func stripFencedCode(s string) string {
	return fencedCodePattern.ReplaceAllString(s, "")
}
