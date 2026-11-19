package github

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Verified close (hivecommons/hive#19299, the #17157 / #19177 pattern).
//
// A multi-phase PR says `Refs #N`; on merge the claim sync and
// close-on-merge label the issue hive/partially-addressed and the stage
// engine sends it back to 010. The scanner's verify-once kick then reads
// the issue and the merged work and writes its verdict. Until now that
// verdict was free text ("This can be closed"), nothing acted on it, and a
// finished issue sat at 010 until a person closed it.
//
// The scanner now ends its verification note with a machine-readable
// marker:
//
//	<!-- hive:verification verdict=complete prs=19172,19179 -->
//	<!-- hive:verification verdict=partial prs=19172 -->
//
// verdict=complete means nothing is left on the issue; prs names the
// merged PRs that delivered it. This sweep reads the marker from the
// hive's own comments (an agent's note is relayed under the hive identity;
// a marker from anyone else is ignored) and:
//
//   - closes the issue as completed, with an evidence comment citing the
//     merged PRs, when every guard holds on two reads
//     verifyCloseSettleInterval apart;
//   - parks it for a person (VerifyCloseLabel, the 045 park the dashboard
//     shows with a one-click Close) when the verdict is complete but a
//     confidence guard does not hold;
//   - labels it hive/verified-open on verdict=partial, so later kicks
//     implement the remainder instead of re-verifying.
//
// Never touched: a held or hive/tracking issue, an umbrella or tracker
// (closeDoneTracker owns those), a decomposed parent with an open child, an
// issue a person reopened, and an issue a person took out of the
// verify-close lane by removing its label. The settle bookkeeping is the
// tracker auto-close's (trackerReadDue / trackerSettled, hivecommons/hive#19151).

const (
	// VerifyCloseLabel is the verify-close lane: an issue the scanner
	// verified complete that the hive will not close on its own. It is a
	// hard-suppress park (stage/045-needs-decision): no agent is offered
	// it, and the dashboard leads its row with Close.
	VerifyCloseLabel = config.VerifyCloseLabel
	// VerifyCloseLabelColor is the label colour (the likely-done green).
	VerifyCloseLabelColor = "0e8a16"
	// VerifyCloseLabelDescription is the description the label is created with.
	VerifyCloseLabelDescription = "Scanner verified this complete; a person confirms the close"

	// verifyCloseSettleInterval is how far apart the two reads that must see
	// the same evidence are, so a close never lands between two PRs or
	// before a second note can disagree. It is the tracker auto-close's.
	verifyCloseSettleInterval = trackerSettleInterval
	// verifyCloseMaxPRs bounds the PR reads one issue can cost per read. An
	// issue that cites or links more is parked for a person instead.
	verifyCloseMaxPRs = 20
	// verifyCloseKeyPrefix namespaces this sweep's settle records in the
	// tracker's read cache.
	verifyCloseKeyPrefix = "verify-close:"
	// verifyCloseEvidenceMarker identifies the close comment for one set of
	// evidence, so a close that failed after its comment is not repeated.
	verifyCloseEvidenceMarker = "<!-- hive:verify-close evidence="
	// VerificationVerdictComplete / VerificationVerdictPartial are the
	// marker's verdict values.
	VerificationVerdictComplete = "complete"
	VerificationVerdictPartial  = "partial"
	// verifyCloseStatusKey is the status-card section of the lane note.
	verifyCloseStatusKey = "verify-close"
)

var (
	verificationMarkerRe = regexp.MustCompile(`<!--\s*hive:verification\s+([^>]*?)\s*-->`)
	verificationAttrRe   = regexp.MustCompile(`([a-z]+)=([^\s]+)`)
	verificationPRRe     = regexp.MustCompile(`^(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#|#)?(\d+)$`)
)

// VerificationMarker renders the marker an agent ends its verification note
// with. verdict is VerificationVerdictComplete or VerificationVerdictPartial.
func VerificationMarker(verdict string, prs ...int) string {
	parts := make([]string, 0, len(prs))
	for _, n := range prs {
		if n > 0 {
			parts = append(parts, strconv.Itoa(n))
		}
	}
	s := "<!-- hive:verification verdict=" + verdict
	if len(parts) > 0 {
		s += " prs=" + strings.Join(parts, ",")
	}
	return s + " -->"
}

// verification is one parsed marker.
type verification struct {
	Verdict string
	PRs     []ChildRef
}

// parseVerificationMarkers returns every well-formed marker in body, in
// order. A marker with an unknown verdict is ignored; a PR token that is
// not a number (or owner/repo#number) is dropped.
func parseVerificationMarkers(body string) []verification {
	var out []verification
	for _, m := range verificationMarkerRe.FindAllStringSubmatch(body, -1) {
		v := verification{}
		for _, a := range verificationAttrRe.FindAllStringSubmatch(m[1], -1) {
			switch strings.ToLower(a[1]) {
			case "verdict":
				v.Verdict = strings.ToLower(a[2])
			case "prs":
				for _, tok := range strings.Split(a[2], ",") {
					pm := verificationPRRe.FindStringSubmatch(strings.TrimSpace(tok))
					if pm == nil {
						continue
					}
					if n, err := strconv.Atoi(pm[2]); err == nil && n > 0 {
						v.PRs = append(v.PRs, ChildRef{Repo: strings.ToLower(pm[1]), Number: n})
					}
				}
			}
		}
		if v.Verdict == VerificationVerdictComplete || v.Verdict == VerificationVerdictPartial {
			out = append(out, v)
		}
	}
	return out
}

// verifyCloseAction is what the sweep does with one issue.
type verifyCloseAction int

const (
	verifyCloseSkip verifyCloseAction = iota
	verifyClosePartial
	verifyClosePark
	verifyCloseClose
)

// verifyClosePR is one PR the evidence names.
type verifyClosePR struct {
	Ref ChildRef
	// Cited: the verification marker or the merged claim names it; it
	// must be merged. A PR only linked from the timeline must not be open.
	Cited  bool
	Merged bool
	Open   bool
}

func (p verifyClosePR) state() string {
	switch {
	case p.Merged:
		return "merged"
	case p.Open:
		return "open"
	}
	return "closed unmerged"
}

// verifyCloseFacts is everything the decision reads, gathered first so the
// rules are one pure function.
type verifyCloseFacts struct {
	// Hard exemptions: never closed, never parked.
	Held             bool
	Tracking         string
	Umbrella         bool
	OpenChildren     bool
	ReopenedBy       string
	LaneRemovedBy    string
	NoVerdict        bool
	LatestVerdict    string
	VerdictsDisagree bool
	// PRs are the cited and linked PRs, sorted.
	PRs []verifyClosePR
	// TooManyPRs: more than verifyCloseMaxPRs; nothing was read.
	TooManyPRs bool
	// ReporterGate is the reporter-confirmation close gate's reason ("" =
	// not gated): a person confirms such a close.
	ReporterGate string
}

// decideVerifyClose applies the guards. The reason names the guard that
// parked or skipped the issue.
func decideVerifyClose(f verifyCloseFacts) (verifyCloseAction, string) {
	switch {
	case f.Held:
		return verifyCloseSkip, "held"
	case f.Tracking != "":
		return verifyCloseSkip, "tracking issue (" + f.Tracking + ")"
	case f.Umbrella:
		return verifyCloseSkip, "umbrella or tracker; the tracker auto-close owns it"
	case f.OpenChildren:
		return verifyCloseSkip, "a sub-issue is still open"
	case f.ReopenedBy != "":
		return verifyCloseSkip, "reopened by " + f.ReopenedBy
	case f.LaneRemovedBy != "":
		return verifyCloseSkip, VerifyCloseLabel + " removed by " + f.LaneRemovedBy
	case f.NoVerdict:
		return verifyCloseSkip, "no verification marker"
	}
	if f.VerdictsDisagree {
		return verifyClosePark, "the verification notes disagree (complete and partial)"
	}
	if f.LatestVerdict == VerificationVerdictPartial {
		return verifyClosePartial, "verified partial: remainder still open"
	}
	if f.TooManyPRs {
		return verifyClosePark, fmt.Sprintf("more than %d PRs to check", verifyCloseMaxPRs)
	}
	cited := 0
	for _, p := range f.PRs {
		switch {
		case p.Cited && !p.Merged:
			return verifyClosePark, "PR " + p.Ref.String() + " is " + p.state()
		case !p.Cited && p.Open:
			return verifyClosePark, "linked PR " + p.Ref.String() + " is still open"
		}
		if p.Cited {
			cited++
		}
	}
	if cited == 0 {
		return verifyClosePark, "the verification cites no merged PR"
	}
	if f.ReporterGate != "" {
		return verifyClosePark, "the reporter confirms this close"
	}
	return verifyCloseClose, "verified complete; every cited PR merged, no open linked PR"
}

// verifyCloseCandidate is the cheap pre-filter: only an issue a merged PR
// partly addressed, or one already in the lane, is ever read.
func verifyCloseCandidate(in IssueStageInput) bool {
	if in.Held || HasHoldLabel(in.Labels) || in.TrackerSignal || in.SpekUmbrella {
		return false
	}
	if in.Children != nil && in.Children.Total > 0 && !in.Children.AllClosed() {
		return false
	}
	return issueHasLabel(in.Labels, PartiallyAddressedLabel) || issueHasLabel(in.Labels, VerifyCloseLabel)
}

// verifyCloseTimelineFacts reads the verdict, the reopen / lane-removal
// guards and the linked PRs from the issue timeline (oldest first).
func (c *Client) verifyCloseTimelineFacts(events []IssueEvent, repoFull string, f *verifyCloseFacts) (linked []ChildRef) {
	var verdicts []string
	seen := map[ChildRef]bool{}
	for _, ev := range events {
		switch {
		case ev.Event == "commented" && ev.Body != "" && c.IsHiveAuthorLogin(ev.Actor):
			for _, v := range parseVerificationMarkers(ev.Body) {
				verdicts = append(verdicts, v.Verdict)
				for _, ref := range v.PRs {
					if strings.EqualFold(ref.Repo, repoFull) {
						ref.Repo = ""
					}
					if !seen[ref] {
						seen[ref] = true
						f.PRs = append(f.PRs, verifyClosePR{Ref: ref, Cited: true})
					}
				}
			}
		case ev.Event == "reopened" && !ev.ActorIsBot && ev.Actor != "":
			f.ReopenedBy = ev.Actor
		case ev.Event == "unlabeled" && strings.EqualFold(ev.Label, VerifyCloseLabel) && !ev.ActorIsBot && ev.Actor != "":
			f.LaneRemovedBy = ev.Actor
		case ev.Event == "cross-referenced" && ev.SourcePR > 0 && (ev.SourceRepo == "" || strings.EqualFold(ev.SourceRepo, repoFull)):
			linked = append(linked, ChildRef{Number: ev.SourcePR})
		}
	}
	if len(verdicts) == 0 {
		f.NoVerdict = true
		return linked
	}
	f.LatestVerdict = verdicts[len(verdicts)-1]
	for _, v := range verdicts {
		if v != f.LatestVerdict {
			f.VerdictsDisagree = true
		}
	}
	return linked
}

// verifyCloseEvidenceHash fingerprints the decision and the PR states so
// two reads can be compared.
func verifyCloseEvidenceHash(action verifyCloseAction, reason string, prs []verifyClosePR) string {
	items := make([]trackerItem, 0, len(prs)+1)
	items = append(items, trackerItem{Text: strconv.Itoa(int(action)) + " " + reason, State: "decision"})
	for _, p := range prs {
		items = append(items, trackerItem{Ref: p.Ref, PR: true, State: p.state() + " cited=" + strconv.FormatBool(p.Cited)})
	}
	return trackerEvidenceHash(items)
}

// verifyCloseComment is the evidence comment left before the close.
func verifyCloseComment(prs []verifyClosePR, hash string) string {
	var b strings.Builder
	b.WriteString(parentAutoCloseMarker + "\n" + verifyCloseEvidenceMarker + hash + " -->\n")
	b.WriteString("The scanner verified this issue complete with nothing left, and the merged work it cites is in:\n\n")
	for _, p := range prs {
		if p.Cited {
			b.WriteString("- PR " + p.Ref.String() + ": merged\n")
		}
	}
	b.WriteString("\nNo linked PR is still open, so the hive is closing it as completed. Reopen it if something is still missing; a reopen by a person is never overridden.")
	return b.String()
}

// verifyCloseParkNote is the status-card note on a parked issue.
func verifyCloseParkNote(reason string) string {
	return "**Verify and close:** the scanner verified this issue complete, but the hive did not close it on its own: " + reason +
		". Close it from the dashboard row (one click), or remove `" + VerifyCloseLabel + "` if work remains."
}

// closeVerifiedComplete runs the verified-close sweep for one issue. It
// reports whether the issue was closed. Two reads verifyCloseSettleInterval
// apart must see the same evidence before it writes anything.
func (c *Client) closeVerifiedComplete(ctx context.Context, in IssueStageInput, store *stagelabels.Store, key string, now time.Time, logger *slog.Logger) bool {
	if !verifyCloseCandidate(in) {
		return false
	}
	settleKey := verifyCloseKeyPrefix + key
	if !trackerReadDue(settleKey, now) {
		return false
	}
	owner, name := c.splitRepo(in.Repo)
	repoFull := strings.ToLower(owner + "/" + name)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, in.Number)
	if err != nil || !strings.EqualFold(issue.GetState(), "open") || issue.IsPullRequest() {
		if err != nil && logger != nil {
			logger.Warn("issue stage labels: verify-close issue unreadable, not closing", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		trackerSettled(settleKey, now, "", false)
		return false
	}
	labels := issueLabelNames(issue.Labels)
	f := verifyCloseFacts{Held: HasHoldLabel(labels), ReporterGate: c.reporterConfirmationCloseGateReason(issue)}
	if why, tracking := isTrackingIssue(issue); tracking {
		f.Tracking = why
	}
	_, f.Umbrella = gfi.UmbrellaPolicy{}.UmbrellaReason(issue.GetTitle(), labels, issue.GetBody(), 0)
	f.OpenChildren = in.Children != nil && in.Children.Total > 0 && !in.Children.AllClosed()
	if f.Held || f.Tracking != "" || f.Umbrella || f.OpenChildren {
		// Exempt whatever else it says: no further reads.
		trackerSettled(settleKey, now, "", false)
		return false
	}
	events, err := c.IssueTimelineSince(ctx, owner+"/"+name, in.Number, time.Time{})
	if err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: verify-close timeline unreadable, not closing", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		trackerSettled(settleKey, now, "", false)
		return false
	}
	linked := c.verifyCloseTimelineFacts(events, repoFull, &f)
	if in.Claim != nil && in.Claim.MergedPR && in.Claim.PRNumber > 0 {
		ref := ChildRef{Repo: strings.ToLower(in.Claim.PRRepo), Number: in.Claim.PRNumber}
		if strings.EqualFold(ref.Repo, repoFull) {
			ref.Repo = ""
		}
		f.PRs = appendVerifyClosePR(f.PRs, verifyClosePR{Ref: ref, Cited: true})
	}
	if !f.NoVerdict && f.LatestVerdict == VerificationVerdictComplete {
		for _, ref := range linked {
			f.PRs = appendVerifyClosePR(f.PRs, verifyClosePR{Ref: ref})
		}
		if len(f.PRs) > verifyCloseMaxPRs {
			f.TooManyPRs = true
		} else if err := c.readVerifyClosePRs(ctx, owner, name, f.PRs); err != nil {
			if logger != nil {
				logger.Warn("issue stage labels: verify-close PR unreadable, not closing", "repo", in.Repo, "issue", in.Number, "error", err)
			}
			trackerSettled(settleKey, now, "", false)
			return false
		}
	}
	sort.SliceStable(f.PRs, func(i, j int) bool {
		a, b := f.PRs[i].Ref, f.PRs[j].Ref
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Number < b.Number
	})
	action, reason := decideVerifyClose(f)
	hash := verifyCloseEvidenceHash(action, reason, f.PRs)
	if action == verifyCloseSkip || !trackerSettled(settleKey, now, hash, true) {
		if action == verifyCloseSkip {
			trackerSettled(settleKey, now, "", false)
		}
		return false
	}
	switch action {
	case verifyClosePartial:
		if !issueHasLabel(labels, VerifiedOpenLabel) {
			if err := c.AddLabels(ctx, in.Repo, in.Number, []string{VerifiedOpenLabel}); err != nil && logger != nil {
				logger.Warn("issue stage labels: labelling verified-open failed", "repo", in.Repo, "issue", in.Number, "error", err)
			}
		}
		return false
	case verifyClosePark:
		c.parkVerifiedForClose(ctx, in, labels, reason, logger)
		return false
	}
	return c.closeVerified(ctx, in, labels, f.PRs, hash, store, key, logger)
}

func appendVerifyClosePR(prs []verifyClosePR, p verifyClosePR) []verifyClosePR {
	for i := range prs {
		if prs[i].Ref == p.Ref {
			prs[i].Cited = prs[i].Cited || p.Cited
			return prs
		}
	}
	return append(prs, p)
}

// readVerifyClosePRs fills Merged / Open for each PR from the issues API
// (pull_request.merged_at). An unreadable PR is an error: no write that
// cycle.
func (c *Client) readVerifyClosePRs(ctx context.Context, owner, name string, prs []verifyClosePR) error {
	for i := range prs {
		_, issue, err := c.readTrackerItem(ctx, owner, name, prs[i].Ref)
		if err != nil {
			return err
		}
		prs[i].Open = strings.EqualFold(issue.GetState(), "open")
		prs[i].Merged = issue.IsPullRequest() && !issue.GetPullRequestLinks().GetMergedAt().IsZero()
	}
	return nil
}

// parkVerifiedForClose puts the issue in the verify-close lane: the label
// (a 045 park, out of the actionable set) and one status note saying why
// the hive did not close it.
func (c *Client) parkVerifiedForClose(ctx context.Context, in IssueStageInput, labels []string, reason string, logger *slog.Logger) {
	if issueHasLabel(labels, VerifyCloseLabel) {
		// Already in the lane: the note was posted when it entered.
		return
	}
	if err := c.EnsureIssueLabel(ctx, in.Repo, VerifyCloseLabel, VerifyCloseLabelColor, VerifyCloseLabelDescription); err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: ensuring verify-close label failed", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		return
	}
	if err := c.AddLabels(ctx, in.Repo, in.Number, []string{VerifyCloseLabel}); err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: adding verify-close label failed", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		return
	}
	note := verifyCloseParkNote(reason)
	if _, err := c.PostInformationalIssueNote(ctx, in.Repo, in.Number, verifyCloseStatusKey, note, func(ctx context.Context) error {
		return c.CreateIssueCommentBudgeted(ctx, in.Repo, in.Number, note)
	}); err != nil && logger != nil {
		logger.Warn("issue stage labels: verify-close note failed", "repo", in.Repo, "issue", in.Number, "error", err)
	}
	if logger != nil {
		logger.Info("issue stage labels: verified complete, parked for a person to close", "repo", in.Repo, "issue", in.Number, "reason", reason)
	}
}

// closeVerified posts the evidence comment (once per evidence) and closes
// the issue as completed. A failed or refused comment means no close.
func (c *Client) closeVerified(ctx context.Context, in IssueStageInput, labels []string, prs []verifyClosePR, hash string, store *stagelabels.Store, key string, logger *slog.Logger) bool {
	owner, name := c.splitRepo(in.Repo)
	needle := verifyCloseEvidenceMarker + hash + " -->"
	posted, err := c.IssueCommentsContain(ctx, in.Repo, in.Number, needle)
	if err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: verify-close comments unreadable, not closing", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		return false
	}
	if !posted {
		if err := c.CreateIssueCommentBudgeted(ctx, in.Repo, in.Number, verifyCloseComment(prs, hash)); err != nil {
			if logger != nil {
				logger.Warn("issue stage labels: verify-close evidence comment failed, not closing", "repo", in.Repo, "issue", in.Number, "error", err)
			}
			return false
		}
	}
	if _, err := stagelabels.Enforce(ctx, c.StageLabeler(), stagelabels.IssueItem, owner, name, in.Number, labels, stagelabels.Desired{}); err != nil && logger != nil {
		logger.Warn("issue stage labels: clearing stage labels before verified close failed", "repo", in.Repo, "issue", in.Number, "error", err)
	}
	if err := c.CloseIssue(ctx, in.Repo, in.Number, IssueCloseOptions{StateReason: IssueStateReasonCompleted}); err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: verified close failed", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		return false
	}
	if issueHasLabel(labels, VerifyCloseLabel) {
		if err := c.RemoveLabel(ctx, in.Repo, in.Number, VerifyCloseLabel); err != nil && logger != nil {
			logger.Warn("issue stage labels: removing verify-close label after close failed", "repo", in.Repo, "issue", in.Number, "error", err)
		}
	}
	if store != nil {
		store.Forget(key)
	}
	if logger != nil {
		logger.Info("issue stage labels: issue closed, scanner verified it complete", "repo", in.Repo, "issue", in.Number, "evidence", hash, "prs", len(prs))
	}
	return true
}
