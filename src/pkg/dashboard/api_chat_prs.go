package dashboard

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// chatPRListCap is how many PR rows a chat answer prints before it folds the
// rest into an "… and N more" footer. The chat panel is a narrow column; a
// 300-row dump is as useless as the old per-repo count was (hivecommons/hive#14408).
const chatPRListCap = 25

// chatPRStaleAge is what `stale` means when the query gives no number: a PR
// nobody has touched for a week.
const chatPRStaleAge = 7 * 24 * time.Hour

// chatPRFilterHint lists the filters `/prs` understands; printed whenever the
// list is folded or the query carried words the parser did not recognise.
const chatPRFilterHint = "`waiting on review`, `needs decision`, `hold`, `failing ci`, `mine`, `by <login>`, `repo <owner/repo>`, `stale`, `older than 7d`"

// chatPRFilter is the parsed remainder of a `/prs …` query. Every field is a
// conjunct: a PR must satisfy all of the set ones.
type chatPRFilter struct {
	NeedsReview   bool
	NeedsDecision bool
	Hold          bool
	FailingCI     bool
	// Author is the lower-cased login the PRs must be opened by. Mine is set
	// when the query said `mine`/`my` and no viewer identity was available,
	// so the answer can say why that filter did nothing.
	Author string
	Mine   bool
	// Repo matches either the `owner/name` or the bare `name` of a repo,
	// lower-cased.
	Repo string
	// OlderThan keeps PRs whose last update is at least this long ago.
	OlderThan time.Duration
	// Unknown collects the words the parser could not place.
	Unknown []string
}

// Active reports whether any filter conjunct is set.
func (f chatPRFilter) Active() bool {
	return f.NeedsReview || f.NeedsDecision || f.Hold || f.FailingCI || f.Author != "" || f.Repo != "" || f.OlderThan > 0
}

// Describe renders the active filters the way the hint spells them, for the
// answer header ("… matching waiting on review, by alice").
func (f chatPRFilter) Describe() string {
	parts := []string{}
	if f.NeedsReview {
		parts = append(parts, "waiting on review")
	}
	if f.NeedsDecision {
		parts = append(parts, "needs decision")
	}
	if f.Hold {
		parts = append(parts, "hold")
	}
	if f.FailingCI {
		parts = append(parts, "failing ci")
	}
	if f.Author != "" {
		parts = append(parts, "by "+f.Author)
	}
	if f.Repo != "" {
		parts = append(parts, "repo "+f.Repo)
	}
	if f.OlderThan > 0 {
		parts = append(parts, "older than "+chatPRAge(f.OlderThan))
	}
	return strings.Join(parts, ", ")
}

var (
	chatPRQuerySplitRE = regexp.MustCompile(`[^a-z0-9#/@._-]+`)
	chatPRDurationRE   = regexp.MustCompile(`^(\d+)(h|d|w)$`)
	chatPRRepoRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*/[a-z0-9][a-z0-9_.-]*$`)
	chatPRLeadingRE    = regexp.MustCompile(`^(?:/?(?:prs?|pulls?|pull[ -]requests?)\s*:?\s*)+`)
)

// chatPRFillerWords are query words that carry no filter meaning: the command
// name the frontend prefixes, the "status" default argument it sends for a
// bare `/prs`, and English glue.
var chatPRFillerWords = map[string]bool{
	"prs": true, "pr": true, "pull": true, "pulls": true, "request": true, "requests": true,
	"status": true, "open": true, "list": true, "show": true, "me": true, "the": true, "all": true,
	"that": true, "which": true, "are": true, "is": true, "with": true, "and": true, "please": true,
	"of": true, "for": true, "a": true, "an": true, "any": true, "currently": true, "what": true,
	"on": true, "in": true, "than": true, "to": true, "still": true, "those": true,
}

// parseChatPRFilter reads the remainder of a `/prs …` query. viewer is the
// caller's login, used to resolve `mine`.
func parseChatPRFilter(query, viewer string) chatPRFilter {
	var f chatPRFilter
	normalized := strings.ToLower(strings.TrimSpace(query))
	normalized = chatPRLeadingRE.ReplaceAllString(normalized, "")
	normalized = chatPRQuerySplitRE.ReplaceAllString(normalized, " ")
	words := strings.Fields(normalized)

	next := func(i int) string {
		for j := i + 1; j < len(words); j++ {
			if !chatPRFillerWords[words[j]] {
				return words[j]
			}
		}
		return ""
	}
	skipTo := func(i int, target string) int {
		for j := i + 1; j < len(words); j++ {
			if words[j] == target {
				return j
			}
		}
		return i
	}

	for i := 0; i < len(words); i++ {
		w := strings.Trim(words[i], ".,:;")
		switch {
		case w == "":
		case w == "waiting", w == "awaiting", w == "needs", w == "need", w == "needing", w == "pending":
			switch target := next(i); {
			case strings.HasPrefix(target, "review"):
				f.NeedsReview = true
				i = skipTo(i, target)
			case strings.HasPrefix(target, "decision"), target == "direction":
				f.NeedsDecision = true
				i = skipTo(i, target)
			case w == "pending":
				f.NeedsReview = true
			default:
				f.Unknown = append(f.Unknown, w)
			}
		case strings.HasPrefix(w, "review"), w == "unreviewed", w == "unapproved":
			f.NeedsReview = true
		case strings.HasPrefix(w, "decision"), w == "undecided", w == "045", w == "085",
			strings.HasPrefix(w, "stage/045"), strings.HasPrefix(w, "stage/085"), w == "needs-decision":
			f.NeedsDecision = true
		case w == "hold", w == "held", w == "holds", w == "on-hold", w == "hold/review", w == "parked":
			f.Hold = true
		case w == "failing", w == "failed", w == "failures", w == "red", w == "broken":
			f.FailingCI = true
			if target := next(i); target == "ci" || target == "checks" || target == "check" || target == "build" || target == "tests" {
				i = skipTo(i, target)
			}
		case w == "ci":
			f.FailingCI = true
		case w == "mine", w == "my", w == "own":
			if login := strings.ToLower(strings.TrimSpace(viewer)); login != "" {
				f.Author = login
			} else {
				f.Mine = true
			}
		case w == "by", w == "from", w == "author", w == "opened":
			target := next(i)
			if target == "by" {
				// "opened by <login>": the next iteration handles "by".
				continue
			}
			if target == "" {
				f.Unknown = append(f.Unknown, w)
				continue
			}
			f.Author = strings.TrimPrefix(target, "@")
			i = skipTo(i, target)
		case strings.HasPrefix(w, "@") && len(w) > 1:
			f.Author = strings.TrimPrefix(w, "@")
		case w == "repo", w == "repository":
			if target := next(i); target != "" {
				f.Repo = target
				i = skipTo(i, target)
			} else {
				f.Unknown = append(f.Unknown, w)
			}
		case chatPRRepoRE.MatchString(w) && !strings.HasPrefix(w, "stage/"):
			f.Repo = w
		case w == "stale", w == "old", w == "aging", w == "idle":
			if f.OlderThan == 0 {
				f.OlderThan = chatPRStaleAge
			}
		case w == "older", w == "over", w == "more":
			if d, ok := chatPRParseAge(next(i)); ok {
				f.OlderThan = d
				i = skipTo(i, next(i))
			} else {
				f.Unknown = append(f.Unknown, w)
			}
		case chatPRDurationRE.MatchString(w):
			if d, ok := chatPRParseAge(w); ok {
				f.OlderThan = d
			}
		case chatPRFillerWords[w]:
		default:
			f.Unknown = append(f.Unknown, w)
		}
	}
	return f
}

// chatPRParseAge reads `3d`, `12h` or `2w`.
func chatPRParseAge(word string) (time.Duration, bool) {
	m := chatPRDurationRE.FindStringSubmatch(word)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch m[2] {
	case "h":
		return time.Duration(n) * time.Hour, true
	case "d":
		return time.Duration(n) * 24 * time.Hour, true
	case "w":
		return time.Duration(n) * 7 * 24 * time.Hour, true
	}
	return 0, false
}

// chatPRAge renders a duration the way the list does: whole days past 48h,
// otherwise hours, otherwise minutes.
func chatPRAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	case d >= time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	case d >= time.Minute:
		return strconv.Itoa(int(d.Minutes())) + "m"
	default:
		return "now"
	}
}

// chatPRRow is one PR in the status snapshot, with the repo it sits under and
// whether it came from the held set.
type chatPRRow struct {
	Repo    string
	PR      github.PullRequest
	Verdict *github.MergeVerdict
	Held    bool
}

// chatPRRows flattens the snapshot's actionable and held PR sets.
func chatPRRows(repos []FrontendRepo) []chatPRRow {
	rows := []chatPRRow{}
	for _, repo := range repos {
		name := overviewRepoName(repo)
		for _, held := range []bool{false, true} {
			source := repo.OpenPrs
			if held {
				source = repo.HeldPrs
			}
			for _, raw := range source {
				if pr, verdict, ok := chatPRFromEntry(raw); ok {
					rows = append(rows, chatPRRow{Repo: name, PR: pr, Verdict: verdict, Held: held})
				}
			}
		}
	}
	return rows
}

// chatPRNeedsDecision is true for a PR parked on a needs-decision stage label
// (stage/085 for PRs, stage/045 as the issue-side successor).
func chatPRNeedsDecision(labels []string) bool {
	for _, label := range labels {
		l := strings.ToLower(strings.TrimSpace(label))
		if l == strings.ToLower(github.PRNeedsDecisionStageLabel) || l == strings.ToLower(github.NeedsDecisionStageLabel) {
			return true
		}
	}
	return false
}

// chatPRHeld is true for a PR in the held set or carrying a hold label.
func chatPRHeld(row chatPRRow) bool {
	return row.Held || github.HasHoldLabel(row.PR.Labels)
}

// chatPRApproved is true when GitHub's review decision, or the hive's count
// of approving reviewers, says the PR has an approval.
func chatPRApproved(pr github.PullRequest) bool {
	if pr.Protection == nil {
		return false
	}
	return pr.Protection.ReviewDecision == github.ReviewDecisionApproved || pr.Protection.ApprovalsGiven > 0
}

// chatPRChangesRequested is true when GitHub's review decision asks the author
// for changes.
func chatPRChangesRequested(pr github.PullRequest) bool {
	return pr.Protection != nil && pr.Protection.ReviewDecision == github.ReviewDecisionChangesRequested
}

// chatPRWaitingOnReview is the `waiting on review` predicate: not a draft, no
// approving review yet, and no outstanding change request — a PR with changes
// requested is waiting on its author, not on a reviewer.
func chatPRWaitingOnReview(pr github.PullRequest) bool {
	return !pr.Draft && !chatPRApproved(pr) && !chatPRChangesRequested(pr)
}

func (f chatPRFilter) matches(row chatPRRow, now time.Time) bool {
	pr := row.PR
	if f.NeedsReview && !chatPRWaitingOnReview(pr) {
		return false
	}
	if f.NeedsDecision && !chatPRNeedsDecision(pr.Labels) {
		return false
	}
	if f.Hold && !chatPRHeld(row) {
		return false
	}
	if f.FailingCI && pr.CIStatus != "failure" {
		return false
	}
	if f.Author != "" && !strings.EqualFold(strings.TrimSpace(pr.Author), f.Author) {
		return false
	}
	if f.Repo != "" {
		full := strings.ToLower(row.Repo)
		short := full
		if idx := strings.LastIndex(full, "/"); idx >= 0 {
			short = full[idx+1:]
		}
		if f.Repo != full && f.Repo != short {
			return false
		}
	}
	if f.OlderThan > 0 {
		ref := pr.UpdatedAt
		if ref.IsZero() {
			ref = pr.CreatedAt
		}
		if ref.IsZero() || now.Sub(ref) < f.OlderThan {
			return false
		}
	}
	return true
}

// chatPRCI is the one-glyph CI column.
func chatPRCI(pr github.PullRequest) string {
	switch pr.CIStatus {
	case "success":
		return "✅ CI"
	case "failure":
		return "❌ CI"
	default:
		return "⏳ CI"
	}
}

// chatPRLine renders one PR row. The `owner/repo#N` token is what the chat
// panel auto-links (formatChatHtml's OWNER_REPO_REF_RE), so the URL is only
// appended when it lives somewhere github.com's link would not reach.
func chatPRLine(row chatPRRow, now time.Time) string {
	pr := row.PR
	meta := []string{}
	if author := strings.TrimSpace(pr.Author); author != "" {
		meta = append(meta, "@"+author)
	}
	if pr.Draft {
		meta = append(meta, "draft")
	}
	for _, label := range pr.Labels {
		if strings.HasPrefix(strings.ToLower(label), "stage/") {
			meta = append(meta, label)
		}
	}
	if ref := pr.UpdatedAt; !ref.IsZero() {
		meta = append(meta, chatPRAge(now.Sub(ref)))
	} else if !pr.CreatedAt.IsZero() {
		meta = append(meta, chatPRAge(now.Sub(pr.CreatedAt)))
	}
	meta = append(meta, chatPRCI(pr))
	if chatPRApproved(pr) {
		meta = append(meta, "approved")
	} else if chatPRChangesRequested(pr) {
		meta = append(meta, "changes requested")
	}
	if chatPRNeedsDecision(pr.Labels) {
		meta = append(meta, "needs decision")
	}
	if chatPRHeld(row) {
		meta = append(meta, "hold")
	}
	if row.Verdict != nil && row.Verdict.State == github.MergeVerdictEligible {
		meta = append(meta, "mergeable")
	}
	title := strings.TrimSpace(pr.Title)
	if title == "" {
		title = "(untitled)"
	}
	line := fmt.Sprintf("- %s#%d — %s (%s)", row.Repo, pr.Number, title, strings.Join(meta, " · "))
	if link := chatPRExternalLink(pr.URL); link != "" {
		line += " " + link
	}
	return line
}

// chatPRExternalLink returns pr.URL when the chat panel's github.com autolink
// would point at the wrong host, else "".
func chatPRExternalLink(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Host)
	if host == "github.com" || host == "www.github.com" || host == "api.github.com" {
		return ""
	}
	return raw
}

// renderChatPRList is the `/prs` answer: the aggregate header the old answer
// printed, then the matching PRs grouped by repo, capped at chatPRListCap.
func renderChatPRList(repos []FrontendRepo, f chatPRFilter, now time.Time) string {
	rows := chatPRRows(repos)
	total := len(rows)
	if total == 0 {
		return "No open pull requests are currently reported."
	}

	matched := make([]chatPRRow, 0, len(rows))
	for _, row := range rows {
		if f.matches(row, now) {
			matched = append(matched, row)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Repo != matched[j].Repo {
			return matched[i].Repo < matched[j].Repo
		}
		return matched[i].PR.Number < matched[j].PR.Number
	})

	var b strings.Builder
	if len(f.Unknown) > 0 {
		fmt.Fprintf(&b, "Ignored `%s`; filters are %s.\n", strings.Join(f.Unknown, " "), chatPRFilterHint)
	}
	if f.Mine {
		b.WriteString("`mine` needs a signed-in viewer; showing every author.\n")
	}
	if f.Active() {
		fmt.Fprintf(&b, "%d of %d open pull request(s) match %s", len(matched), total, f.Describe())
	} else {
		fmt.Fprintf(&b, "There are %d open pull request(s)", total)
	}
	if len(matched) == 0 {
		b.WriteString("; nothing to list.")
		return b.String()
	}
	b.WriteString(":\n")

	shown := 0
	currentRepo := ""
	for _, row := range matched {
		if shown >= chatPRListCap {
			break
		}
		if row.Repo != currentRepo {
			currentRepo = row.Repo
			count := 0
			for _, r := range matched {
				if r.Repo == currentRepo {
					count++
				}
			}
			fmt.Fprintf(&b, "**%s** (%d):\n", currentRepo, count)
		}
		b.WriteString(chatPRLine(row, now))
		b.WriteString("\n")
		shown++
	}
	if rest := len(matched) - shown; rest > 0 {
		fmt.Fprintf(&b, "… and %d more — narrow with a filter: %s.", rest, chatPRFilterHint)
	}
	return strings.TrimRight(b.String(), "\n")
}
