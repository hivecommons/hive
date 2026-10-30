package github

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// One backlog item per underlying finding per PR, across perspectives and
// heads (hivecommons/hive#15627, #15628, #15629 were three issues for one
// finding on one PR). Two mechanisms:
//
//   - a perspective that reports an earlier head's or another perspective's
//     note ("Previous style finding still present (non-blocking)") is making
//     a REFERENCE, not a finding. Such a report never files; at most it
//     leaves one short "still present at <head>" comment on the item it
//     refers to, once per head;
//   - a genuinely new report that cites the same file as an item already
//     filed from the same PR, and the same line, a shared symbol or the same
//     (normalised) title, is the same finding seen from another perspective:
//     it is tracked against that item instead of filed again.
//
// Reviewers that want to point at a prior note cite it with the
// ReviewRefersToMarker; the hive reads it as a reference too.

// ReviewRefersToMarker is the comment a reviewer puts in a finding to say it
// restates an existing backlog item rather than raising a new one:
// `<!-- hive-refers-to: #N -->`.
const ReviewRefersToMarker = review.ReviewRefersToMarker

var (
	reviewRefersToRE = regexp.MustCompile(`<!--\s*hive-refers-to:\s*(?:[\w.-]+/[\w.-]+)?#(\d+)\s*-->`)
	// A role tag such as "[reviewer]" ahead of the backlog stem (another
	// filing path adds it; dedupe must read both forms).
	reviewBacklogRolePrefixRE = regexp.MustCompile(`^\s*\[[^\]\s]+\]\s*`)
	reviewBacklogStemRE       = regexp.MustCompile(`(?i)^\s*review backlog\s*\([^)]*\)\s*:\s*`)
	// Carry-over wrappers: "Previous style note still present (non-blocking):",
	// "Prior finding still present:", "Earlier … unchanged at new head",
	// "Previous intent-alignment note (…) still structurally true".
	reviewBacklogCarryOverRE = regexp.MustCompile(`(?i)^\s*(?:re-?check(?:\s+of)?\s+(?:the\s+)?)?(?:previous|prior|earlier)\b[^:]*?\b(?:finding|note|concern|item|issue|observation)s?\b`)
	reviewBacklogStillRE     = regexp.MustCompile(`(?i)\b(?:still\s+(?:present|true|structurally\s+true|applies|holds|stands|open|unaddressed|unchanged|not\s+(?:addressed|met|fixed))|unchanged|carried\s+over|not\s+yet\s+(?:addressed|met|fixed)|persists|remains)\b`)
	// Trailing advisory qualifiers that vary per pass: "(non-blocking)",
	// "(still present, advisory)", "(advisory, unchanged)".
	reviewBacklogQualifierRE = regexp.MustCompile(`(?i)\s*\((?:[\s,;/-]*(?:non-?blocking|advisory|still\s+present|unchanged|info(?:rmational)?|nit|minor|low|carried\s+over|at\s+new\s+head|new\s+head))+[\s,;/-]*\)\s*`)
	reviewBacklogWrapperTail = regexp.MustCompile(`(?i)\s*(?:at\s+(?:the\s+)?new\s+head|at\s+head\s+[0-9a-f]{7,40}|since\s+(?:head\s+)?[0-9a-f]{7,40})\s*$`)
)

// reviewRefersTo lists the backlog item numbers a finding cites with the
// ReviewRefersToMarker in its title or summary.
func reviewRefersTo(f outputschema.Finding) []int {
	var out []int
	for _, m := range reviewRefersToRE.FindAllStringSubmatch(f.Title+"\n"+f.Summary, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// reviewBacklogCarryOver reports whether a finding is a carry-over
// reference — a restatement of an earlier head's or another perspective's
// note — rather than a finding of its own: its title is wrapped in
// "Previous/Prior/Earlier … still present/unchanged", or it cites an item
// with the refers-to marker.
func reviewBacklogCarryOver(f outputschema.Finding) bool {
	if len(reviewRefersTo(f)) > 0 {
		return true
	}
	title := reviewBacklogBareTitle(f.Title)
	if !reviewBacklogCarryOverRE.MatchString(title) {
		return false
	}
	head := reviewBacklogCarryOverRE.FindString(title)
	rest := title[len(head):]
	// "Previous … note" alone, or followed by a still/unchanged clause
	// before the first colon, is the wrapper; "Previous finding leaks the
	// token" (a new finding about earlier code) is not.
	clause := rest
	if i := strings.Index(rest, ":"); i >= 0 {
		clause = rest[:i]
	}
	return strings.TrimSpace(strings.Trim(clause, " .-—–()")) == "" || reviewBacklogStillRE.MatchString(clause)
}

// reviewBacklogBareTitle drops a leading role tag and the "Review backlog
// (<perspective>):" stem, so titles from either filing path compare equal.
func reviewBacklogBareTitle(title string) string {
	t := reviewBacklogRolePrefixRE.ReplaceAllString(strings.TrimSpace(title), "")
	t = reviewBacklogStemRE.ReplaceAllString(t, "")
	return strings.TrimSpace(t)
}

// reviewBacklogNormalisedTitle is the finding a title names once its
// per-pass wrapping is stripped: role tag, backlog stem, carry-over wrapper
// ("Previous style note still present (non-blocking):"), trailing advisory
// qualifiers and "at new head" tails, lower-cased with whitespace collapsed.
// Titles of the same finding reported by different perspectives or at
// different heads normalise to the same string; a pure wrapper with no
// finding of its own normalises to "".
func reviewBacklogNormalisedTitle(title string) string {
	t := reviewBacklogQualifierRE.ReplaceAllString(reviewBacklogBareTitle(title), " ")
	if m := reviewBacklogCarryOverRE.FindStringIndex(t); m != nil {
		rest := t[m[1]:]
		if i := strings.Index(rest, ":"); i >= 0 && (strings.TrimSpace(strings.Trim(rest[:i], " .-—–()")) == "" || reviewBacklogStillRE.MatchString(rest[:i])) {
			t = rest[i+1:]
		} else if strings.TrimSpace(strings.Trim(rest, " .-—–()")) == "" || reviewBacklogStillRE.MatchString(rest) {
			// "Previous security finding (merge/trust policy expansion)
			// unchanged at new head": the parenthetical is the finding.
			t = ""
			if open := strings.Index(rest, "("); open >= 0 {
				if close := strings.Index(rest[open:], ")"); close > 0 {
					t = rest[open+1 : open+close]
				}
			}
		}
	}
	t = reviewBacklogQualifierRE.ReplaceAllString(t, " ")
	t = reviewBacklogWrapperTail.ReplaceAllString(t, "")
	t = strings.Trim(t, " \t:.-—–")
	return normaliseFingerprintText(t)
}

// reviewBacklogPRFinding is one finding filed (or tracked) from a PR: enough
// of it to tell a later report of the same finding from a new one.
type reviewBacklogPRFinding struct {
	File       string `json:"file"`
	Line       int    `json:"line,omitempty"`
	Title      string `json:"title"`
	ItemID     string `json:"item_id,omitempty"`
	ItemKey    string `json:"item_key,omitempty"`
	ItemNumber int    `json:"item_number,omitempty"`
	ItemURL    string `json:"item_url,omitempty"`
}

func (r reviewBacklogPRFinding) ref() ReviewBacklogRef {
	return ReviewBacklogRef{ID: r.ItemID, Key: r.ItemKey, Number: r.ItemNumber, URL: r.ItemURL}
}

// reviewBacklogSameFinding reports whether a report is the finding an
// item already tracks for the same PR: same file, and the same line, a
// shared code symbol, or the same normalised title.
func reviewBacklogSameFinding(have reviewBacklogPRFinding, f outputschema.Finding) bool {
	file := normaliseFingerprintPath(f.File)
	if file == "" || file != normaliseFingerprintPath(have.File) {
		return false
	}
	if have.Line > 0 && f.Line == have.Line {
		return true
	}
	want, got := reviewBacklogNormalisedTitle(have.Title), reviewBacklogNormalisedTitle(f.Title)
	if want != "" && want == got {
		return true
	}
	return reviewBacklogSharesSymbol(have.Title, f.Title)
}

// prFinding returns the item already filed from pr for the same finding.
func (l *reviewBacklogLedger) prFinding(repo string, pr int, f outputschema.Finding) (reviewBacklogPRFinding, bool) {
	for _, have := range l.PRFindings[reviewBacklogPRKey(repo, pr)] {
		if reviewBacklogSameFinding(have, f) {
			return have, true
		}
	}
	return reviewBacklogPRFinding{}, false
}

// prFindingByItem returns the finding pr filed as item number n, for a
// refers-to citation.
func (l *reviewBacklogLedger) prFindingByItem(repo string, pr, n int) (reviewBacklogPRFinding, bool) {
	for _, have := range l.PRFindings[reviewBacklogPRKey(repo, pr)] {
		if have.ItemNumber == n && n > 0 {
			return have, true
		}
	}
	return reviewBacklogPRFinding{}, false
}

func (l *reviewBacklogLedger) addPRFinding(repo string, pr int, f outputschema.Finding, ref ReviewBacklogRef, head string) {
	key := reviewBacklogPRKey(repo, pr)
	// The head the finding was filed at needs no "still present" note.
	if head = strings.TrimSpace(head); head != "" && ref.ID != "" {
		l.StillPresentHeads[reviewBacklogStillPresentKey(repo, pr, ref)] = head
	}
	for _, have := range l.PRFindings[key] {
		if have.ItemID == ref.ID && normaliseFingerprintPath(have.File) == normaliseFingerprintPath(f.File) && have.Line == f.Line {
			return
		}
	}
	l.PRFindings[key] = append(l.PRFindings[key], reviewBacklogPRFinding{
		File: normaliseFingerprintPath(f.File), Line: f.Line, Title: strings.TrimSpace(f.Title),
		ItemID: ref.ID, ItemKey: ref.Key, ItemNumber: ref.Number, ItemURL: ref.URL,
	})
}

// reviewBacklogStillPresentKey is per PR and item: an item several PRs
// re-raise keeps a last-noted head for each of them.
func reviewBacklogStillPresentKey(repo string, pr int, ref ReviewBacklogRef) string {
	return reviewBacklogPRKey(repo, pr) + ":" + ref.ID
}

func reviewBacklogPRKey(repo string, pr int) string {
	return reviewBacklogRepoKey(repo) + "#" + strconv.Itoa(pr)
}

// reviewBacklogTitlesEqual reports whether two review-backlog titles name
// the same item once a leading role tag ("[reviewer]") is ignored, so a
// filing dedupes against an existing title with or without the prefix.
// Titles outside the backlog stem never match this way.
func reviewBacklogTitlesEqual(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b || !reviewBacklogTitleCategoryRE.MatchString(a) || !reviewBacklogTitleCategoryRE.MatchString(b) {
		return a == b
	}
	return reviewBacklogRolePrefixRE.ReplaceAllString(a, "") == reviewBacklogRolePrefixRE.ReplaceAllString(b, "")
}

// stillPresentDue reports whether a "still present" note for item at head
// has not been posted yet; it records the head when it has not. Rate-limited
// to one note per item per head, so three perspectives re-reporting the same
// note at one head leave one line, not three.
func (l *reviewBacklogLedger) stillPresentDue(repo string, pr int, ref ReviewBacklogRef, head string) bool {
	head = strings.TrimSpace(head)
	if head == "" || ref.ID == "" {
		return false
	}
	key := reviewBacklogStillPresentKey(repo, pr, ref)
	if l.StillPresentHeads[key] == head {
		return false
	}
	l.StillPresentHeads[key] = head
	return true
}

// reviewBacklogStillPresentMarker tags the once-per-head note.
const reviewBacklogStillPresentMarker = "<!-- hive:review-backlog-still-present "

func reviewBacklogStillPresentComment(head string, perspectives []review.Perspective) string {
	names := make([]string, 0, len(perspectives))
	seen := map[string]bool{}
	for _, p := range perspectives {
		if s := strings.TrimSpace(string(p)); s != "" && !seen[s] {
			seen[s] = true
			names = append(names, s)
		}
	}
	sort.Strings(names)
	short := head
	if len(short) > 7 {
		short = short[:7]
	}
	return fmt.Sprintf("%s%s -->\nStill present at %s (%s).", reviewBacklogStillPresentMarker, head, short, strings.Join(names, ", "))
}
