package dashboard

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Stuck-decision detection for "Let hive decide" (hivecommons/hive#14822).
//
// The live case: the delegate decided `supersede` on a contributor PR at
// 13:01Z and posted the `/hive decision …` comment. The merge gate read
// that comment as a reviewer's change request (`contributor-lane:
// changes-requested` every cycle), so the decision blocked the PR it was
// meant to advance, and the supersede itself never ran (the author was not
// offline). Forty minutes later the owner clicked "Let hive decide" again
// and got "No change — hive already recorded this decision (rule
// `contributor-offline`)". He opened the PR and could not tell why the hive
// had not decided or what "supersede" meant.
//
// Now the dedup path checks whether the recorded decision's expected
// effect has materialised. Past `decisions.delegate.effect_timeout`
// (default 30 min) the row action answers `no-effect` — "Hive decided
// <plain action> at <T>, but it hasn't taken effect: <plain why>" — with
// the concrete next actions, and posts one plain-words follow-up comment
// on the item. Before the deadline the answer still says what was decided
// and by when it should show.

// delegatedDecisionNow is the clock the deadline check reads; tests pin it.
var delegatedDecisionNow = time.Now

// decisionActionDelegateRerun re-runs the delegate past the "already
// recorded" dedup (the request carries force=true).
const decisionActionDelegateRerun = "decide:delegate:rerun"

// delegatedPriorState is what the hive already wrote on the item under
// one rule: when it last recorded the decision, the decision text it
// posted, and whether the stalled follow-up is already there.
type delegatedPriorState struct {
	At      time.Time
	Text    string
	Stalled bool
	Found   bool
}

// delegatedPriorDecisionState reads the item's comments once for both the
// prior decision under rule and its stalled follow-up. A listing failure
// reads as "no prior decision": deciding twice is the cheaper mistake.
func (s *Server) delegatedPriorDecisionState(ctx context.Context, repoFull string, number int, rule string) delegatedPriorState {
	var st delegatedPriorState
	if rule == "" || s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		return st
	}
	owner, repoName, _ := strings.Cut(repoFull, "/")
	comments, _, err := s.deps.GHClient.GoGitHub().Issues.ListComments(ctx, owner, repoName, number, &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: delegatedDecisionGatherLimit}})
	if err != nil {
		return st
	}
	tag := "(rule `" + rule + "`)"
	for _, c := range comments {
		body := c.GetBody()
		if delegatedecision.IsStalledComment(body, rule) {
			st.Stalled = true
			continue
		}
		if !delegatedecision.IsDelegatedComment(body) || !strings.Contains(body, tag) {
			continue
		}
		if t := c.GetCreatedAt().Time; !st.Found || t.After(st.At) {
			st.At, st.Found = t, true
			st.Text = strings.TrimSpace(strings.SplitN(body, "\n", 2)[0])
		}
	}
	return st
}

// delegatedParkLabelsAsked are the labels whose (re)application after a
// recorded decision means someone asked a new question: the needs-human
// ask and the 045 / 085 needs-decision stage labels.
var delegatedParkLabelsAsked = []string{config.DefaultNeedsHumanLabel, config.DefaultNeedsDecisionLabel, config.PRNeedsDecisionStageLabel}

// delegatedRepark is the latest park label applied after a point in time.
type delegatedRepark struct {
	At    time.Time
	Label string
}

// delegatedReparkedAfter reads the item's timeline for a park label
// applied after since. ok is false when none was, or when the timeline
// cannot be read (the recorded decision then stands as the dedup's
// answer, the cheaper mistake).
func (s *Server) delegatedReparkedAfter(ctx context.Context, repoFull string, number int, since time.Time) (delegatedRepark, bool) {
	if since.IsZero() || s.deps == nil || s.deps.GHClient == nil {
		return delegatedRepark{}, false
	}
	events, err := s.deps.GHClient.IssueTimelineSince(ctx, repoFull, number, since)
	if err != nil {
		return delegatedRepark{}, false
	}
	return reparkFromEvents(events)
}

// reparkFromEvents picks the latest "labeled" event naming a park label.
func reparkFromEvents(events []ghpkg.IssueEvent) (delegatedRepark, bool) {
	var out delegatedRepark
	for _, ev := range events {
		if ev.Event != "labeled" || !slices.ContainsFunc(delegatedParkLabelsAsked, func(l string) bool { return strings.EqualFold(l, ev.Label) }) {
			continue
		}
		if ev.At.After(out.At) {
			out = delegatedRepark{At: ev.At, Label: ev.Label}
		}
	}
	return out, !out.At.IsZero()
}

// delegatedCommentTexts lists the item's recent comments as body + time
// for the blocker ledger; nil when the client is missing or the read
// fails (the ledger then answers from what it already knows).
func (s *Server) delegatedCommentTexts(ctx context.Context, repoFull string, number int) []commentText {
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		return nil
	}
	owner, repoName, _ := strings.Cut(repoFull, "/")
	comments, _, err := s.deps.GHClient.GoGitHub().Issues.ListComments(ctx, owner, repoName, number, &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: delegatedDecisionGatherLimit}})
	if err != nil {
		return nil
	}
	out := make([]commentText, 0, len(comments))
	for _, c := range comments {
		out = append(out, commentText{Body: c.GetBody(), At: c.GetCreatedAt().Time})
	}
	return out
}

// delegatedEffectTimeout is decisions.delegate.effect_timeout (default 30m).
func (s *Server) delegatedEffectTimeout() time.Duration {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return delegatedecision.DefaultEffectTimeout
	}
	return s.deps.Config.Decisions.Delegate.EffectTimeoutDuration()
}

// delegatedStuckWhy names, as a glossary slug, why the decision d recorded
// on in has not taken effect. It reads the park reason the tracker row
// showed (req.Reason / Caption), the gathered PR state, and the decision
// itself; the fallback is the verdict's own "hasn't happened yet" slug.
func (s *Server) delegatedStuckWhy(in delegatedecision.Input, d delegatedecision.Decision, req delegatedDecisionRequest, prior delegatedPriorState) string {
	reason := strings.ToLower(strings.TrimSpace(req.Reason + " " + req.Caption))
	has := func(subs ...string) bool {
		for _, sub := range subs {
			if strings.Contains(reason, sub) {
				return true
			}
		}
		return false
	}
	if d.Verdict == delegatedecision.VerdictAdoptSupersede {
		if in.WorkflowPath {
			return delegatedecision.GlossaryWorkflowPath
		}
		if in.ContributorOnline {
			return delegatedecision.GlossaryAuthorOnline
		}
		if !s.stageAdoptionEnabled() {
			return delegatedecision.GlossaryAdoptionOff
		}
	}
	switch {
	case has("changes-requested", "changes_requested", "changes requested"):
		// The hive's own non-approve `/hive decision` read as a reviewer's
		// change request is the #14822 class; a bare approve is not.
		if prior.Found && !isApproveDecisionText(prior.Text) {
			return delegatedecision.GlossarySelfDecision
		}
		return delegatedecision.GlossaryChangesRequest
	case in.SentinelAlert || has("sentinel"):
		return delegatedecision.GlossarySentinel
	case in.Conflicting || has("conflict"):
		return delegatedecision.GlossaryConflicting
	case in.DCOMissing || has("dco"):
		return delegatedecision.GlossaryDCO
	case has("hold"):
		return delegatedecision.GlossaryHold
	case has("behind"):
		return delegatedecision.GlossaryBehind
	case in.Kind == delegatedecision.KindPR && in.CIRed() && d.Verdict != delegatedecision.VerdictCloseDuplicate && d.Verdict != delegatedecision.VerdictCloseSuperseded:
		return delegatedecision.GlossaryCIRed
	}
	switch {
	case d.Verdict == delegatedecision.VerdictCloseDuplicate, d.Verdict == delegatedecision.VerdictCloseSuperseded:
		return delegatedecision.GlossaryNotClosedYet
	case d.Verdict == delegatedecision.VerdictDirection:
		return delegatedecision.GlossaryNoNewCommit
	case issueParkLabelStuck(in, d):
		return delegatedecision.GlossaryParkLabelStuck
	case in.Kind == delegatedecision.KindIssue:
		return delegatedecision.GlossaryStillParked
	default:
		return delegatedecision.GlossaryNotMergedYet
	}
}

// issueParkLabelStuck reports an approved issue that still carries both
// approved-direction and stage/045-needs-decision. The labels alone do not
// show whether the park was never lifted or was added again. The caller
// handles later re-parks first (hivecommons/hive#15021: approve lifted 045,
// then the scanner re-parked), so those become a new ask rather than a
// stuck-label diagnosis. The toast names the label and offers the lift.
func issueParkLabelStuck(in delegatedecision.Input, d delegatedecision.Decision) bool {
	if in.Kind != delegatedecision.KindIssue || d.Verdict != delegatedecision.VerdictApprove {
		return false
	}
	park, ack := false, false
	for _, l := range in.Labels {
		switch {
		case strings.EqualFold(strings.TrimSpace(l), stagelabels.IssueNeedsDecision.Label()):
			park = true
		case strings.EqualFold(strings.TrimSpace(l), ghpkg.HumanAckLabel):
			ack = true
		}
	}
	return park && ack
}

// isApproveDecisionText reports a bare `/hive decision approve[d]`.
func isApproveDecisionText(line string) bool {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/hive decision"))
	word, _, _ := strings.Cut(rest, " ")
	switch strings.ToLower(strings.Trim(word, ".,:;!")) {
	case "approve", "approved", "lgtm":
		return true
	}
	return false
}

// delegatedStuckOptions are the concrete next actions for a decision that
// has not taken effect, in the popover's option shape so the SPA renders
// them as buttons. Merging is the in-tracker merge-now action
// (hivecommons/hive#15087) for a merger, not a link out. When why is the stuck 045 park
// label, the first option lifts it: `decide:approve` on an issue runs
// RecordOperatorIssueDecision, the audited path that removes the park
// label and tells the stage engine the park lifted.
func delegatedStuckOptions(in delegatedecision.Input, d delegatedecision.Decision, mergerRole bool, why string) []StageTrackerDecisionOption {
	var opts []StageTrackerDecisionOption
	if why == delegatedecision.GlossaryParkLabelStuck {
		opts = append(opts,
			StageTrackerDecisionOption{Action: decisionActionApprove, Label: "Lift the park (remove " + stagelabels.IssueNeedsDecision.Label() + ")", Enabled: true},
			StageTrackerDecisionOption{Action: decisionActionRetry, Label: "Re-evaluate now", Enabled: true})
	}
	if in.Kind == delegatedecision.KindPR && d.Verdict != delegatedecision.VerdictCloseDuplicate && d.Verdict != delegatedecision.VerdictCloseSuperseded {
		o := decisionOption(decisionActionMergeNow, decisionMergeNowLabel, mergerRole, decisionWhyMergerRole)
		o.Consequence = "merges #" + strconv.Itoa(in.Number) + " now via hive's identity, attributed to you"
		opts = append(opts, o)
	}
	opts = append(opts, StageTrackerDecisionOption{Action: decisionActionDelegateRerun, Label: "Re-run decision", Enabled: true})
	if in.Kind == delegatedecision.KindPR {
		opts = append(opts,
			StageTrackerDecisionOption{Action: decisionActionCloseSuperseded, Label: "Close as superseded", Enabled: true},
			StageTrackerDecisionOption{Action: decisionActionDirection, Label: "Send back for a new commit", Enabled: true})
	} else {
		opts = append(opts, StageTrackerDecisionOption{Action: decisionActionDirection, Label: "Give direction", Enabled: true})
	}
	return opts
}

func optionLabels(opts []StageTrackerDecisionOption) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Label)
	}
	return out
}

// delegatedDecisionEffectResult is the dedup path's answer. Before the
// deadline it is a no-op that still says, in plain words, what the hive
// decided and by when it should show; past it, `no-effect` with the why
// and the options.
func delegatedDecisionEffectResult(target RowActionTarget, in delegatedecision.Input, d delegatedecision.Decision, prior time.Time, now time.Time, timeout time.Duration, why string, options []StageTrackerDecisionOption) RowActionResult {
	eff := delegatedecision.ExpectedEffect(d, in.Kind)
	at := prior.UTC().Truncate(time.Second)
	stamp := at.Format(time.RFC3339)
	plainAction := delegatedecision.ActionPlainOn(d, in.Kind)
	out := RowActionResult{Target: target, Rule: d.Rule, AppliedAt: &at, Why: why}
	if !eff.Waits {
		out.Action = rowActionNoop
		out.Outcome = "No change — hive already recorded this decision at " + stamp + ": " + plainAction + " (" + delegatedecision.RulePlain(d.Rule) + ")."
		out.Next = eff.IfNot
		return out
	}
	deadline := at.Add(timeout)
	if now.Before(deadline) {
		out.Action = rowActionNoop
		out.Outcome = "No change — hive already recorded this decision at " + stamp + ": " + plainAction + " (" + delegatedecision.RulePlain(d.Rule) + ")."
		out.Next = "Expected by " + deadline.UTC().Truncate(time.Second).Format(time.RFC3339) + ": " + eff.What + ". If it hasn't happened by then, this action will say why and offer options."
		return out
	}
	out.Action = rowActionNoEffect
	out.Options = options
	// The toast headline already says "Hive decided on <item>"; the body
	// does not say "decided" again. The SPA rewrites the RFC3339 stamp into
	// the operator's clock (stageTrackerStampWords).
	out.Outcome = delegatedecision.CapitalizeFirst(plainAction) + " at " + stamp + ", but it hasn't taken effect: " + delegatedecision.PlainOn(why, in.Kind) + "."
	next := "Expected: " + eff.What + ". " + delegatedecision.DetailOn(why, in.Kind)
	if labels := optionLabels(options); len(labels) > 0 {
		next += " Options: " + strings.Join(labels, " · ") + "."
	}
	out.Next = next
	return out
}

// postDelegatedStalledComment posts the one plain-words follow-up for a
// decision past its deadline. Posting failures are reported in the notes,
// never fatal: the toast already carries the explanation.
func (s *Server) postDelegatedStalledComment(ctx context.Context, in delegatedecision.Input, d delegatedecision.Decision, prior delegatedPriorState, why string, options []StageTrackerDecisionOption) string {
	if prior.Stalled {
		return "stalled follow-up already posted"
	}
	owner, repoName, _ := strings.Cut(in.Repo, "/")
	body := delegatedecision.StalledCommentBody(d, in.Kind, prior.At, why, optionLabels(options), s.delegateHiveID())
	if _, _, err := s.deps.GHClient.GoGitHub().Issues.CreateComment(ctx, owner, repoName, in.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return "stalled follow-up not posted: " + err.Error()
	}
	return "posted the stalled follow-up comment"
}
