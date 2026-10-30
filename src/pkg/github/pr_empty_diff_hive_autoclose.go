package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// Hive-authored empty-diff PR auto-close (hivecommons/hive#15499).
//
// #15499 was the hive's adoption of a contributor PR whose fix had already
// landed on v5 through #15427/#15449: every merge-from-base left its tree
// identical to the base (changed_files 0). The PR claimed no issue, so the
// superseded rule (pr_superseded_hive_autoclose.go) never fired, and the
// reviewer swarm asked the owner "close as a no-op?" on every new head —
// fifteen times. A PR with nothing in its diff is the one case where the
// person's answer is mechanical: there is nothing to merge. For the hive's
// own PR the hive closes it; a person's PR keeps the decision prompt, as
// the superseded rule does.
//
// The diff is measured, never inferred from the reviewer's text: the GET
// payload's changed_files is the cheap first signal (list payloads omit
// it and read 0, so it is only ever a reason to look closer), and the
// compare API base...head is the proof. A head younger than
// EmptyDiffMinHeadAge is left alone: a branch the author is still
// pushing to may be empty for a moment.
//
// Already-landed (hivecommons/hive#15647, #15648). The same rule, entered
// from the reviewer's verdict rather than from changed_files: when the
// review of THIS head says the change is already on the base (the
// `hive-redundant-of` marker, or "already landed via #N" in words), the
// hive closes its own PR as superseded by #N instead of parking it at 085
// for a person to say so. Two things differ from the plain empty-diff
// entry. The freshness guard is waived — the reviewer has already read the
// head, so it is not mid-push (on #15647 the guard measured from
// updated_at, which the hive's own labels and comments bump every pass,
// and the PR sat at 085). And an adoption whose contributor head was never
// merged from the base still lists files in the compare even though every
// one of them is byte-identical on the base tip; the closer proves that
// file by file (blob SHA at head == blob SHA on base) before closing.
// Redundancy is always measured: the reviewer's text only names the
// superseding PR, and when it names none the close says "the base itself".

const (
	// SupersessionActionClosedEmptyDiff: a hive-authored PR was closed
	// because its tree is identical to its base (the compare lists no
	// files); the change it carried is already on the base.
	SupersessionActionClosedEmptyDiff = "closed-empty-diff"

	// SupersessionActionClosedAlreadyLanded: a hive-authored PR was closed
	// because every file its compare lists is already identical on the
	// base tip — the change landed through another PR and the head was
	// simply never merged from the base.
	SupersessionActionClosedAlreadyLanded = "closed-already-landed"

	// EmptyDiffMinHeadAge is how old a PR must be before an empty diff
	// closes it: a just-opened PR whose commits are still arriving is not
	// yet empty by intent.
	EmptyDiffMinHeadAge = 15 * time.Minute

	// alreadyLandedMaxFiles bounds the file-by-file base comparison: one
	// contents GET per file, so a wide PR is not proved this way (it
	// stays with the reviewer's question).
	alreadyLandedMaxFiles = 25

	hiveEmptyDiffReasonHasDiff    = "has-diff"
	hiveEmptyDiffReasonUnknown    = "diff-unknown"
	hiveEmptyDiffReasonTooFresh   = "head-too-fresh"
	hiveEmptyDiffReasonGated      = "writeback-gated"
	hiveEmptyDiffReasonDraft      = "draft"
	hiveEmptyDiffReasonNotHive    = hiveSupersededReasonNotHive
	hiveEmptyDiffReasonHeld       = hiveSupersededReasonHeld
	hiveEmptyDiffReasonKeepOpen   = hiveSupersededReasonKeepOpen
	hiveEmptyDiffReasonPerson     = hiveSupersededReasonPerson
	hiveEmptyDiffReasonLevel      = hiveSupersededReasonLevel
	hiveEmptyDiffReasonDisabled   = hiveSupersededReasonDisabled
	hiveEmptyDiffReasonMerged     = hiveSupersededReasonMerged
	hiveEmptyDiffVerdictClose     = hiveSupersededVerdictClose
	hiveEmptyDiffVerdictKeep      = hiveSupersededVerdictKeep
	hiveEmptyDiffVerdictHumanNote = hiveSupersededVerdictHumanNote
)

// HiveEmptyDiffAutoCloseFacts is everything DecideHiveEmptyDiffAutoClose
// needs about one PR.
type HiveEmptyDiffAutoCloseFacts struct {
	// Enabled is governor.auto_close_superseded, the same switch as the
	// superseded rule: both close the hive's own PR because a merged PR
	// already carries its change.
	Enabled bool
	// HiveAuthored: this hive's App (or a login HiveIdentity matches)
	// opened the PR. A person's PR is closed here only when LandingProven.
	HiveAuthored bool
	// LandingProven (hivecommons/hive#15732): a merged PR is known to carry
	// this PR's change — the reviewer named one and it is merged, the issue
	// this PR closes was closed by a merged PR, or every commit on the head
	// is already on the base. With an empty diff that is unambiguous for
	// any author kind; trust changes nothing.
	LandingProven bool
	// Merged / Draft / Held / KeptOpen / ParkedByPerson are the guards every
	// sweep honours.
	Merged, Draft, Held, KeptOpen, ParkedByPerson bool
	// DiffKnown is true when the compare base...head succeeded with a
	// complete file list; DiffFiles is its length.
	DiffKnown bool
	DiffFiles int
	// HeadAge is now minus the head commit's date when the compare
	// reported one, else minus the PR's last update (or creation). It
	// never exceeds the PR's own age: a PR just opened from an old branch
	// is still a just-opened PR.
	HeadAge time.Duration
	// ReviewerConfirmed: the reviewer verdict for this very head says the
	// change is already on the base. The head has been read, so the
	// freshness guard (a branch mid-push) does not apply.
	ReviewerConfirmed bool
	// TreeMatchesBase is true when the compare lists files but every one
	// of them is byte-identical on the base tip (prFilesIdenticalOnBase):
	// the diff GitHub shows is history, not content.
	TreeMatchesBase bool
	// Level is the repo's ACMM level; own-PR closes need L6 like the
	// superseded rule.
	Level int
}

// DecideHiveEmptyDiffAutoClose is the pure rule. It returns "close" when the
// hive's own PR measurably adds nothing to its base — or anyone's PR does
// and a merged landing is proven (hivecommons/hive#15732) — "human" when
// the PR is a person's, empty, and no landing is proven (the caller leaves
// the decision prompt in place), or "keep" with the guard that held.
func DecideHiveEmptyDiffAutoClose(f HiveEmptyDiffAutoCloseFacts) (verdict, reason string) {
	switch {
	case !f.Enabled:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonDisabled
	case f.Merged:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonMerged
	case f.Draft:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonDraft
	case !f.DiffKnown:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonUnknown
	case f.DiffFiles > 0 && !f.TreeMatchesBase:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonHasDiff
	case !f.HiveAuthored && !f.LandingProven:
		return hiveEmptyDiffVerdictHumanNote, hiveEmptyDiffReasonNotHive
	case f.Held:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonHeld
	case f.KeptOpen:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonKeepOpen
	case f.ParkedByPerson:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonPerson
	case f.Level < acmmLevelAutonomous:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonLevel
	case f.HeadAge < EmptyDiffMinHeadAge && !f.ReviewerConfirmed:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonTooFresh
	}
	if f.DiffFiles > 0 {
		return hiveEmptyDiffVerdictClose, SupersessionActionClosedAlreadyLanded
	}
	return hiveEmptyDiffVerdictClose, SupersessionActionClosedEmptyDiff
}

// HiveEmptyDiffClose is what CloseEmptyDiffHivePRIfEligible did.
type HiveEmptyDiffClose struct {
	// Closed is true when the PR was closed by this call.
	Closed bool
	// Verdict / Reason are DecideHiveEmptyDiffAutoClose's answer (or
	// "writeback-gated" when the gate refused the close).
	Verdict, Reason string
	// Empty is true when the compare proved the diff empty.
	Empty bool
	// RedundantOf is the PR the close comment names as carrying the
	// change, when the caller knew one.
	RedundantOf int
	// Supersedes are the PRs the closed PR's body declared it supersedes
	// (an adoption's original); the caller settles their lineage.
	Supersedes []int
	// IssuesClosed are the issues the PR's closing references named that
	// the close also closed (through CloseIssue's usual gates).
	IssuesClosed []int
}

// PRChangedFilesClaimsEmpty reports whether a single-PR GET payload says the
// PR changes no files. List payloads omit changed_files and read false
// here, so this is only ever the cheap reason to run the compare.
func PRChangedFilesClaimsEmpty(pr *gh.PullRequest) bool {
	return pr != nil && pr.ChangedFiles != nil && *pr.ChangedFiles == 0 && pr.Additions != nil && *pr.Additions == 0 && pr.Deletions != nil && *pr.Deletions == 0
}

// prDiffAgainstBase measures the PR's diff as GitHub shows it: the compare
// base...head. known is false when the compare failed or the file list
// was cut short. headAt is the newest commit's committer date when the
// compare listed commits; when it listed none the head is already on the
// base, so it is the merge base's date. Zero when neither is readable.
// headOnBase is true when the compare listed no commits: every commit of
// the head is contained in the base.
func (c *Client) prDiffAgainstBase(ctx context.Context, owner, name string, pr *gh.PullRequest) (files []*gh.CommitFile, headAt time.Time, known, headOnBase bool) {
	base := strings.TrimSpace(pr.GetBase().GetRef())
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if base == "" || head == "" {
		return nil, time.Time{}, false, false
	}
	comparison, resp, err := c.client.Repositories.CompareCommits(WithRESTCaller(ctx, "hive:empty_diff_autoclose"), owner, name, base, head, &gh.ListOptions{PerPage: githubCompareFileLimit})
	if err != nil || comparison == nil {
		return nil, time.Time{}, false, false
	}
	for _, commit := range comparison.Commits {
		if at := commit.GetCommit().GetCommitter().GetDate().Time; at.After(headAt) {
			headAt = at
		}
	}
	headOnBase = len(comparison.Commits) == 0 && comparison.GetMergeBaseCommit() != nil
	if mb := comparison.GetMergeBaseCommit(); len(comparison.Commits) == 0 && strings.EqualFold(mb.GetSHA(), head) {
		headAt = mb.GetCommit().GetCommitter().GetDate().Time
	}
	if (resp != nil && resp.NextPage != 0) || len(comparison.Files) >= githubCompareFileLimit {
		return comparison.Files, headAt, false, headOnBase
	}
	return comparison.Files, headAt, true, headOnBase
}

// alreadyLandedProof looks for the merged PR that carries pr's change
// (hivecommons/hive#15732), cheapest evidence first: the PR the reviewer
// named, when it is merged; else the merged PR that closed the issue this
// PR claims (detectSupersessionDetail); else the compare having listed no
// commits (the head is already on the base — no single PR named). landed
// is the PR to credit, 0 when the base itself is the landing.
func (c *Client) alreadyLandedProof(ctx context.Context, owner, name string, pr *gh.PullRequest, redundantOf int, headOnBase bool, identity HiveIdentity, now time.Time) (landed int, proven bool) {
	if redundantOf > 0 && redundantOf != pr.GetNumber() {
		named, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:empty_diff_autoclose"), owner, name, redundantOf)
		if err == nil && named != nil && (named.GetMerged() || !named.GetMergedAt().IsZero()) {
			return redundantOf, true
		}
	}
	if det, err := c.detectSupersessionDetail(ctx, owner+"/"+name, pr, identity, now); err == nil && det.candidate.Issue > 0 && det.closer.Number > 0 {
		return det.closer.Number, true
	}
	if headOnBase {
		return 0, true
	}
	return 0, false
}

// prFilesIdenticalOnBase proves an already-landed head: every file the
// compare lists is, on the base tip, exactly what the head has (added or
// modified → same blob SHA; removed → absent). known is false when a
// contents read failed for a reason other than 404, or the PR is wider
// than alreadyLandedMaxFiles; a rename or copy is not proved (match
// false, known true).
func (c *Client) prFilesIdenticalOnBase(ctx context.Context, owner, name, base string, files []*gh.CommitFile) (match, known bool) {
	if len(files) == 0 {
		return true, true
	}
	if len(files) > alreadyLandedMaxFiles || strings.TrimSpace(base) == "" {
		return false, false
	}
	ctx = WithRESTCaller(ctx, "hive:already_landed_autoclose")
	for _, f := range files {
		path := strings.TrimSpace(f.GetFilename())
		if path == "" {
			return false, false
		}
		content, _, resp, err := c.client.Repositories.GetContents(ctx, owner, name, path, &gh.RepositoryContentGetOptions{Ref: base})
		notFound := err != nil && resp != nil && resp.StatusCode == http.StatusNotFound
		switch strings.ToLower(strings.TrimSpace(f.GetStatus())) {
		case "added", "modified", "changed":
			if notFound {
				return false, true
			}
			if err != nil || content == nil {
				return false, false
			}
			if !strings.EqualFold(strings.TrimSpace(content.GetSHA()), strings.TrimSpace(f.GetSHA())) {
				return false, true
			}
		case "removed":
			if notFound {
				continue
			}
			if err != nil {
				return false, false
			}
			return false, true
		default:
			return false, true
		}
	}
	return true, true
}

// CloseEmptyDiffHivePRIfEligible closes pr when the hive opened it and the
// compare against its base lists no files: the change it carried is
// already on the base. redundantOf, when known (the reviewer's
// `hive-redundant-of` marker), names the PR that carries it in the close
// comment. The close goes through the writeback gate as a supersession
// close. Idempotent: a closed PR, a PR with a diff, or a person's PR is a
// no-op. repo is "owner/name".
func (c *Client) CloseEmptyDiffHivePRIfEligible(ctx context.Context, repo string, pr *gh.PullRequest, redundantOf int) (HiveEmptyDiffClose, error) {
	return c.closeHiveRedundantPR(ctx, repo, pr, redundantOf, false)
}

// CloseAlreadyLandedHivePRIfEligible is CloseEmptyDiffHivePRIfEligible
// entered from the reviewer's verdict on this head (the change is already
// on the base; redundantOf is the PR it named, 0 when it named none). The
// freshness guard is waived and a head whose compare lists files is still
// closed when every one of them is identical on the base tip. The PR's
// closing references are closed through CloseIssue's usual gates, and the
// originals its body declares it supersedes are reported for the caller's
// lineage settlement. Redundancy is measured, never taken from the text.
//
// Any author's PR is closed this way (hivecommons/hive#15732: #15544 by a
// trusted contributor sat at 085 for 9h although #15545 had merged the
// same change) when, besides the empty diff, the landing is proven: the
// named PR is merged, the issue this PR closes was closed by a merged PR,
// or the head's commits are all on the base. A person's empty PR with no
// proven landing keeps the reviewer's question.
func (c *Client) CloseAlreadyLandedHivePRIfEligible(ctx context.Context, repo string, pr *gh.PullRequest, redundantOf int) (HiveEmptyDiffClose, error) {
	return c.closeHiveRedundantPR(ctx, repo, pr, redundantOf, true)
}

func (c *Client) closeHiveRedundantPR(ctx context.Context, repo string, pr *gh.PullRequest, redundantOf int, reviewed bool) (HiveEmptyDiffClose, error) {
	out := HiveEmptyDiffClose{RedundantOf: redundantOf}
	if c == nil || c.client == nil {
		return out, ErrNoGitHubClient
	}
	if pr == nil {
		return out, nil
	}
	owner, name := c.splitRepo(repo)
	repo = owner + "/" + name
	enabled, level := c.supersededAutoClosePolicyFor(repo)
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	labels := labelNames(pr.Labels)
	now := time.Now()
	facts := HiveEmptyDiffAutoCloseFacts{
		Enabled:           enabled,
		Level:             level,
		Merged:            pr.GetMerged() || !pr.GetMergedAt().IsZero() || strings.EqualFold(pr.GetState(), "closed"),
		Draft:             pr.GetDraft(),
		HiveAuthored:      identity.Matches(safeGetLogin(pr.GetUser())),
		Held:              c.isHeld(labels) || c.isExempt(labels),
		KeptOpen:          supersessionHasLabel(pr, SupersessionKeepOpenLabel),
		HeadAge:           emptyDiffHeadAge(pr, time.Time{}, now),
		ReviewerConfirmed: reviewed,
	}
	if pr.ChangedFiles != nil && *pr.ChangedFiles > 0 && !reviewed {
		// The GET payload already shows a diff: no compare needed. On the
		// reviewer's word the compare still runs — the files may all be
		// identical on the base.
		facts.DiffKnown, facts.DiffFiles = true, *pr.ChangedFiles
	}
	if !enabled || facts.Merged || facts.Draft || facts.Held || facts.KeptOpen || facts.DiffFiles > 0 || (!reviewed && !facts.HiveAuthored && facts.DiffKnown) {
		// Cheap guards first: no compare for a PR the rule cannot touch.
		out.Verdict, out.Reason = DecideHiveEmptyDiffAutoClose(facts)
		return out, nil
	}
	if !reviewed && !facts.HiveAuthored {
		// The sweep's empty-diff rule never closes a person's PR; only
		// the reviewer's already-landed entry proves a landing.
		out.Verdict, out.Reason = hiveEmptyDiffVerdictHumanNote, hiveEmptyDiffReasonNotHive
		return out, nil
	}
	headOnBase := false
	if !facts.DiffKnown {
		files, headAt, known, onBase := c.prDiffAgainstBase(ctx, owner, name, pr)
		facts.DiffFiles, facts.DiffKnown, headOnBase = len(files), known, onBase
		facts.HeadAge = emptyDiffHeadAge(pr, headAt, now)
		if known && len(files) > 0 && reviewed {
			match, matchKnown := c.prFilesIdenticalOnBase(ctx, owner, name, strings.TrimSpace(pr.GetBase().GetRef()), files)
			facts.TreeMatchesBase = match && matchKnown
		}
	}
	out.Empty = facts.DiffKnown && (facts.DiffFiles == 0 || facts.TreeMatchesBase)
	if out.Empty {
		facts.ParkedByPerson = c.supersessionNeedsHumanByPerson(ctx, owner, name, pr)
	}
	if out.Empty && !facts.HiveAuthored {
		// A person's empty PR closes only on proof that the change
		// landed; the compare alone could be a branch mid-push.
		landed, proven := c.alreadyLandedProof(ctx, owner, name, pr, redundantOf, headOnBase, identity, now)
		facts.LandingProven = proven
		if proven && landed > 0 {
			redundantOf = landed
			out.RedundantOf = landed
		}
	}
	out.Verdict, out.Reason = DecideHiveEmptyDiffAutoClose(facts)
	if out.Verdict != hiveEmptyDiffVerdictClose {
		return out, nil
	}
	if d := c.WritebackAllowed(repo, c.sentinelWritebackItem(pr), writeback.KindSupersessionClose, "close empty-diff PR"); !d.Allowed {
		out.Verdict, out.Reason = hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonGated
		return out, nil
	}
	event := SupersessionSweepEvent{
		Repo: repo, Number: pr.GetNumber(), Author: safeGetLogin(pr.GetUser()),
		IssueRepo: repo, CloserPR: redundantOf, Action: out.Reason,
	}
	closer := supersessionCloser{Repo: repo, Number: redundantOf}
	why := hiveEmptyDiffCloseReason(redundantOf, shortHeadSHA(pr), strings.TrimSpace(pr.GetBase().GetRef()), facts.TreeMatchesBase)
	if !facts.HiveAuthored {
		why = alreadyLandedCloseReason(redundantOf, event.Author, shortHeadSHA(pr), strings.TrimSpace(pr.GetBase().GetRef()), facts.TreeMatchesBase)
	}
	if _, err := c.ensureSupersessionComment(ctx, owner, name, pr.GetNumber(), renderEmptyDiffCloseComment(closer, why, facts.TreeMatchesBase)); err != nil {
		return out, err
	}
	if err := c.labelAndCloseSupersededPR(ctx, owner, name, pr.GetNumber()); err != nil {
		return out, err
	}
	if sink := c.StageFacts(); sink != nil {
		if err := sink.Closed(ctx, c.PRStageRef(repo, pr.GetNumber())); err != nil && c.logger != nil {
			c.logger.Warn("empty-diff auto-close: stage close deferred to next sweep", slog.String("repo", repo), slog.Int("pr", pr.GetNumber()), slog.String("error", err.Error()))
		}
	}
	if c.logger != nil {
		c.logger.Info("empty-diff auto-close: closed PR with no diff against its base",
			slog.String("repo", repo), slog.Int("pr", pr.GetNumber()), slog.Int("redundant_of", redundantOf), slog.String("action", event.Action),
			slog.Bool("hive_authored", facts.HiveAuthored), slog.Bool("landing_proven", facts.LandingProven))
	}
	out.Closed = true
	if reviewed {
		out.Supersedes = SupersedesDeclared(pr.GetBody())
		out.IssuesClosed = c.closeIssuesAlreadyLanded(ctx, owner, name, pr, redundantOf, facts.HiveAuthored)
	}
	return out, nil
}

// closeIssuesAlreadyLanded closes the issues pr's closing references name
// once the PR itself is closed as already landed: the work is on the base,
// so the issue gets the usual close (CloseIssue's tracking-issue and
// reporter-confirmation gates included), then a note naming the PR that
// landed it — only once the close went through, so a refused close leaves
// no "done" note on an open issue. Failures are logged, never fatal: the
// PR close stands.
func (c *Client) closeIssuesAlreadyLanded(ctx context.Context, owner, name string, pr *gh.PullRequest, redundantOf int, hiveAuthored bool) []int {
	repo := owner + "/" + name
	var closed []int
	for _, number := range ClosingIssueRefs(pr.GetBody(), repo) {
		issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
		if err != nil || issue == nil || issue.IsPullRequest() || !strings.EqualFold(issue.GetState(), "open") {
			continue
		}
		item := writeback.Item{Number: number, Author: safeGetLogin(issue.GetUser()), Labels: labelNames(issue.Labels), CreatedAt: issue.GetCreatedAt().Time, HiveAuthored: c.getHiveIdentity().Matches(safeGetLogin(issue.GetUser()))}
		if d := c.WritebackAllowed(repo, item, writeback.KindIssueClose, "close issue already landed"); !d.Allowed {
			continue
		}
		// Close first: CloseIssue's gates may refuse, and a "this issue is
		// done" note must never sit on an issue that stayed open.
		if err := c.CloseIssue(ctx, repo, number, IssueCloseOptions{StateReason: IssueStateReasonCompleted}); err != nil { // writeback:gated
			if c.logger != nil {
				c.logger.Warn("already-landed auto-close: issue left open", slog.String("repo", repo), slog.Int("issue", number), slog.String("error", err.Error()))
			}
			continue
		}
		closed = append(closed, number)
		note := alreadyLandedIssueNote(pr.GetNumber(), redundantOf, strings.TrimSpace(pr.GetBase().GetRef()), hiveAuthored)
		if _, _, err := c.client.Issues.CreateComment(ctx, owner, name, number, &gh.IssueComment{Body: gh.Ptr(note)}); err != nil && c.logger != nil { // writeback:gated
			c.logger.Warn("already-landed auto-close: issue note failed", slog.String("repo", repo), slog.Int("issue", number), slog.String("error", err.Error()))
		}
	}
	return closed
}

// alreadyLandedIssueNote is the one-line note on an issue closed because
// the PR that claimed it was already landed through another PR.
func alreadyLandedIssueNote(pr, redundantOf int, base string, hiveAuthored bool) string {
	whose := "#" + strconv.Itoa(pr)
	if hiveAuthored {
		whose = "its own " + whose
	}
	if redundantOf > 0 {
		return fmt.Sprintf("%s #%d -->\nThe hive closed %s as superseded: the change it carried is already on `%s` via #%d, so this issue is done. Reopen if that's wrong.", reviewRedundantOfMarker, redundantOf, whose, base, redundantOf)
	}
	return fmt.Sprintf("The hive closed %s as superseded: the change it carried is already on `%s`, so this issue is done. Reopen if that's wrong.", whose, base)
}

// emptyDiffHeadAge measures from the compare's newest commit (headAt) when
// it is known; updated_at, which labels and comments bump, stands in only
// when it is not. The age never exceeds the PR's own age: a PR just opened
// from an old branch is still a just-opened PR whose commits may be arriving.
func emptyDiffHeadAge(pr *gh.PullRequest, headAt, now time.Time) time.Duration {
	at := headAt
	if at.IsZero() {
		at = pr.GetUpdatedAt().Time
	}
	if created := pr.GetCreatedAt().Time; !created.IsZero() && created.After(at) {
		at = created
	}
	if at.IsZero() {
		return 0
	}
	return now.Sub(at)
}

func shortHeadSHA(pr *gh.PullRequest) string {
	sha := strings.TrimSpace(pr.GetHead().GetSHA())
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// hiveEmptyDiffCloseReason is the one-line why of the close comment.
func hiveEmptyDiffCloseReason(redundantOf int, head, base string, treeMatches bool) string {
	proof := "the compare lists no files"
	if treeMatches {
		proof = "every file it touches is already byte-identical on the base tip"
	}
	if redundantOf > 0 {
		return fmt.Sprintf("Closing this hive-authored PR as superseded by #%d: head %s is identical to `%s` (%s), so the change it carried is already on the base. Reopen if that's wrong.", redundantOf, head, base, proof)
	}
	return fmt.Sprintf("Closing this hive-authored PR: head %s is identical to `%s` (%s), so there is nothing left to merge. Reopen if that's wrong.", head, base, proof)
}

// alreadyLandedCloseReason is the one-line why of the close comment for a
// PR the hive did not author (hivecommons/hive#15732): the change landed
// via another PR, so the PR is closed as superseded with thanks.
func alreadyLandedCloseReason(redundantOf int, author, head, base string, treeMatches bool) string {
	proof := "the compare lists no files"
	if treeMatches {
		proof = "every file it touches is already byte-identical on the base tip"
	}
	thanks := "thank you for the work."
	if a := strings.TrimSpace(author); a != "" {
		thanks = "thank you @" + a + "."
	}
	if redundantOf > 0 {
		return fmt.Sprintf("Already landed via #%d — closing as superseded; %s Head %s is identical to `%s` (%s), so the change this PR carried is already on the base. Reopen if that's wrong.", redundantOf, thanks, head, base, proof)
	}
	return fmt.Sprintf("Already landed on `%s` — closing as superseded; %s Head %s is identical to the base (%s), so there is nothing left to merge. Reopen if that's wrong.", base, thanks, head, proof)
}

// renderEmptyDiffCloseComment is the close notice, under the supersession
// marker so ensureSupersessionComment edits it in place and the
// `hive-redundant-of` marker so the dashboard and the dedupe key read the
// superseding PR.
func renderEmptyDiffCloseComment(closer supersessionCloser, reason string, treeMatches bool) string {
	var b strings.Builder
	fmt.Fprintln(&b, supersessionSweepMarker)
	if closer.Number > 0 {
		fmt.Fprintf(&b, "%s #%d -->\n", reviewRedundantOfMarker, closer.Number)
	}
	if treeMatches {
		fmt.Fprintln(&b, "The hive found that this PR's change is already on its base.")
	} else {
		fmt.Fprintln(&b, "The hive found that this PR's diff against its base is empty.")
	}
	fmt.Fprintln(&b)
	switch {
	case closer.Number <= 0:
		fmt.Fprintln(&b, "- Superseded by: the base branch itself (no single PR named)")
	case closer.URL != "":
		fmt.Fprintf(&b, "- Superseded by: [#%d](%s)\n", closer.Number, closer.URL)
	default:
		fmt.Fprintf(&b, "- Superseded by: %s#%d\n", closer.Repo, closer.Number)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, reason)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "_This comment is edited in place by the hive; it is not duplicated._")
	return b.String()
}
