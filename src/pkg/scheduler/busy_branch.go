package scheduler

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Busy-branch guard for the repair queues (hivecommons/hive#18488).
//
// The FIX-BEFORE-NEW block and the shared CI_FAILING queue list red or
// conflicted PRs as push targets. A PR the hive App did not open — the
// owner's own session PRs, a same-repo contributor branch — reached the
// scanner as "yours" (unattributed rows default to scanner) and the scanner
// pushed its own commits onto a branch another worker was still pushing to:
// the "Sign-off survives the squash" check went red (#18419), the worker's
// next push raced, and it re-made the same fix. The decision point is the
// same one that already keeps fork PRs comment-only: such a row is listed
// as "suggest, do not push" when the branch is busy, i.e. when
//
//   - a live claim held by someone other than the kicked agent covers the
//     PR or one of its linked issues (the claims ledger), or
//   - the head was pushed less than busyBranchWindow() ago.
//
// Rows of App-authored PRs and in-place adoptions are never checked: the
// first are the hive's own lane branches, the second are owned by the hive
// for as long as the adoption holds. A row with no claim and no recent push
// is idle and keeps its push instruction exactly as before.

// DefaultBusyBranchMinutes is how recent a head push makes a non-App branch
// busy. BusyBranchMinutesEnv overrides it; a missing, unparsable or
// non-positive value keeps the default.
const (
	DefaultBusyBranchMinutes = 60
	BusyBranchMinutesEnv     = "HIVE_SCANNER_BUSY_BRANCH_MINUTES"
)

// busyBranchNow is the busy guard's clock; tests swap it.
var busyBranchNow = time.Now

// busyBranchWindow is the recent-push window in effect.
func busyBranchWindow() time.Duration {
	minutes := DefaultBusyBranchMinutes
	if raw := strings.TrimSpace(os.Getenv(BusyBranchMinutesEnv)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			minutes = n
		}
	}
	return time.Duration(minutes) * time.Minute
}

// PRLeaseLookup reports a live claim on repo#number. holder is a short
// display string ("clubanderson (human) until …"); holderAgent is the hive
// agent name when a hive agent holds it, else "".
type PRLeaseLookup func(repo string, number int) (holder, holderAgent string, held bool)

// SetPRLeaseLookup installs (or with nil, removes) the claim lookup the busy
// guard consults.
func (s *Scheduler) SetPRLeaseLookup(fn PRLeaseLookup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prLease = fn
}

// busyBranchRow is the part of a ci-failing.json row the guard reads.
type busyBranchRow struct {
	Repo         string
	Number       int
	Issues       []int
	AppAuthored  bool
	Adopted      bool
	HeadPushedAt *time.Time
	// Own marks a row the audit trail attributes to the kicked agent itself
	// (a lane PR opened on a person's credentials): its recent pushes are
	// that agent's own, so only another worker's claim makes it busy.
	Own bool
}

// ownRepairRow reports a ci-failing.json row attributed to agent by the
// audit trail itself. A trusted-author rebase or a reroute from a paused lane
// names its fixer explicitly but is still someone else's branch.
func ownRepairRow(rowAgent, agent string, trustedAuthor bool, reroutedFrom string) bool {
	rowAgent = strings.TrimSpace(rowAgent)
	return rowAgent != "" && agent != "" && strings.EqualFold(rowAgent, agent) && !trustedAuthor && strings.TrimSpace(reroutedFrom) == ""
}

// busyBranchCheck returns why a row's branch is busy for agent, or "" when it
// is free to push to. A nil check treats every branch as free.
type busyBranchCheck func(row busyBranchRow) string

// busyBranchChecker builds the guard for agent (a base agent name; "" means
// any hive agent's claim counts as another worker's).
func (s *Scheduler) busyBranchChecker(agent string) busyBranchCheck {
	var lease PRLeaseLookup
	if s != nil {
		s.mu.RLock()
		lease = s.prLease
		s.mu.RUnlock()
	}
	baseName := func(name string) string {
		if s == nil || s.cfg == nil {
			return name
		}
		return s.cfg.BaseAgentName(name)
	}
	window := busyBranchWindow()
	now := busyBranchNow()
	return func(row busyBranchRow) string {
		return busyBranchReason(row, agent, lease, baseName, window, now)
	}
}

// busyBranchReason is the guard's decision, separated from the scheduler so
// tests drive it with fixed inputs.
func busyBranchReason(row busyBranchRow, agent string, lease PRLeaseLookup, baseName func(string) string, window time.Duration, now time.Time) string {
	if row.AppAuthored || row.Adopted {
		return ""
	}
	if lease != nil {
		for _, n := range append([]int{row.Number}, row.Issues...) {
			if n <= 0 {
				continue
			}
			holder, holderAgent, held := lease(row.Repo, n)
			if !held {
				continue
			}
			if holderAgent != "" && agent != "" && strings.EqualFold(baseName(holderAgent), agent) {
				continue // the kicked agent's own claim is not another worker
			}
			what := "this PR"
			if n != row.Number {
				what = fmt.Sprintf("linked issue #%d", n)
			}
			return fmt.Sprintf("%s is claimed by %s", what, holder)
		}
	}
	if !row.Own && row.HeadPushedAt != nil && !row.HeadPushedAt.IsZero() {
		if age := now.Sub(*row.HeadPushedAt); age < window {
			if age < 0 {
				age = 0
			}
			return fmt.Sprintf("head pushed %s ago (busy window %s)", age.Round(time.Minute), window)
		}
	}
	return ""
}

// busyBranchInstruction is what an agent does with a busy branch instead of
// pushing: hand the fix to the worker so it lands as that worker's own
// signed commit.
const busyBranchInstruction = "Do NOT push, rebase or force-push these branches: someone else is working on them, and a commit from you would break their sign-off check and race their next push. " +
	"Post the fix as a suggestion instead — a GitHub suggestion block, a diff, or exact steps — in ONE comment per PR: if you already left a note on that PR, edit it rather than adding another. " +
	"The worker applies it as its own signed commit. A branch that stays idle returns to the push list once its head is older than the busy window and no claim covers it."

// busyBranchAnnotation is the ${PR_LIST} marker for a busy branch, the
// counterpart of forkAnnotation's "comment only, cannot push": the agent
// learns from its work list that it must not push there. Empty for a fork PR
// (already comment-only) and for a free branch.
func busyBranchAnnotation(pr github.PullRequest, agent string, busy busyBranchCheck) string {
	if busy == nil || pr.FromFork {
		return ""
	}
	why := busy(busyBranchRow{
		Repo:         pr.Repo,
		Number:       pr.Number,
		Issues:       PRIssueNumbers(pr),
		AppAuthored:  PRAppAuthored(pr),
		Adopted:      prHasLabel(pr.Labels, stagelabels.AdoptedByHive.Label()),
		HeadPushedAt: PRHeadPushedAt(pr),
		Own:          ownRepairRow(pr.HiveAgent, agent, false, ""),
	})
	if why == "" {
		return ""
	}
	return " [busy: " + why + " — do not push; post the fix as one suggestion comment]"
}

// busyRedPR is one busy row as the kick renders it.
type busyRedPR struct {
	Number        int
	Repo          string
	Title         string
	FailingChecks []string
	Excerpt       string
	Why           string
}

// writeBusyRedPRs renders the FIX-BEFORE-NEW companion block for busy rows:
// the evidence an agent needs to write the suggestion, under a heading that
// says the branch is not theirs to push to. n rows are read through row.
func writeBusyRedPRs(b *strings.Builder, n int, row func(i int) busyRedPR) {
	if n == 0 {
		return
	}
	fmt.Fprintf(b, "\n## ⏸ BUSY BRANCHES — red PRs you must NOT push to (%d)\n\n", n)
	b.WriteString(busyBranchInstruction + "\n\n")
	for i := 0; i < n; i++ {
		if i >= redPRFixMaxDetailed {
			fmt.Fprintf(b, "  … and %d more (full list: %s)\n", n-i, ciFailingPath)
			break
		}
		pr := row(i)
		fmt.Fprintf(b, "  #%d %s — %s\n", pr.Number, pr.Repo, pr.Title)
		b.WriteString("    busy: " + pr.Why + " — suggest, do not push\n")
		if len(pr.FailingChecks) > 0 {
			fmt.Fprintf(b, "    failing: %s\n", strings.Join(pr.FailingChecks, ", "))
		}
		if excerpt := strings.TrimSpace(pr.Excerpt); excerpt != "" {
			if runes := []rune(excerpt); len(runes) > redPRFixExcerptRunes {
				excerpt = string(runes[:redPRFixExcerptRunes]) + "…"
			}
			b.WriteString("    evidence: " + strings.ReplaceAll(excerpt, "\n", "\n              ") + "\n")
		}
	}
	b.WriteString("\n")
}

// Busy-branch evidence from a PR record. cmd/hive stamps these on
// ci-failing.json rows and the PR list reads them directly; all come from data
// the eval tick already holds, so no GitHub call is added.

// PRAppAuthored reports a PR the hive App itself opened. Only such a branch
// is the hive's own to push to without asking whether someone else is working
// on it; a PR opened on a person's credentials (HiveAttributed) is that
// person's branch.
func PRAppAuthored(pr github.PullRequest) bool {
	return pr.AppAuthored || strings.HasPrefix(strings.ToLower(strings.TrimSpace(pr.Author)), "app/")
}

// PRHeadPushedAt approximates when the current head was pushed: the
// earliest CI activity GitHub reported on that head (a check run starting or
// finishing, a commit status being posted). CI reacts to a push within
// moments, so this is a close upper bound on the push time; nil when no CI
// activity was observed, which the consumer reads as "no recent push known".
func PRHeadPushedAt(pr github.PullRequest) *time.Time {
	if pr.CIActivity == nil {
		return nil
	}
	head := strings.TrimSpace(pr.HeadSHA)
	var earliest *time.Time
	for _, check := range pr.CIActivity.Checks {
		if check.Since == nil || check.Since.IsZero() {
			continue
		}
		if check.Head != "" && head != "" && !strings.EqualFold(check.Head, head) {
			continue
		}
		if earliest == nil || check.Since.Before(*earliest) {
			at := check.Since.UTC()
			earliest = &at
		}
	}
	return earliest
}

// PRIssueNumbers lists the same-repo issues the PR is linked to (GitHub's
// closing references and the PR's own references), sorted and de-duplicated,
// so a consumer can ask the claims ledger whether a worker holds one.
func PRIssueNumbers(pr github.PullRequest) []int {
	bare := strings.ToLower(strings.TrimSpace(pr.Repo))
	if i := strings.LastIndex(bare, "/"); i >= 0 {
		bare = bare[i+1:]
	}
	sameRepo := func(repo string) bool {
		repo = strings.ToLower(strings.TrimSpace(repo))
		return repo == "" || repo == bare || strings.HasSuffix(repo, "/"+bare)
	}
	seen := map[int]bool{}
	var out []int
	for _, list := range [][]github.PRLinkedIssue{pr.LinkedIssues, pr.ReferencedIssues} {
		for _, issue := range list {
			if issue.Number <= 0 || issue.Number == pr.Number || seen[issue.Number] || !sameRepo(issue.Repo) {
				continue
			}
			seen[issue.Number] = true
			out = append(out, issue.Number)
		}
	}
	sort.Ints(out)
	return out
}
