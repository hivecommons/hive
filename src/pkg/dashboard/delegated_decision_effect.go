package dashboard

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/delegatedecision"
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

// decisionActionOpenGitHub is a link-style option: the SPA opens the target
// on GitHub instead of calling an endpoint (merging stays a human act).
const decisionActionOpenGitHub = "open:github"

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
	case in.Kind == delegatedecision.KindPR && !in.CIGreen && d.Verdict != delegatedecision.VerdictCloseDuplicate && d.Verdict != delegatedecision.VerdictCloseSuperseded:
		return delegatedecision.GlossaryCIRed
	}
	switch {
	case d.Verdict == delegatedecision.VerdictCloseDuplicate, d.Verdict == delegatedecision.VerdictCloseSuperseded:
		return delegatedecision.GlossaryNotClosedYet
	case d.Verdict == delegatedecision.VerdictDirection:
		return delegatedecision.GlossaryNoNewCommit
	case in.Kind == delegatedecision.KindIssue:
		return delegatedecision.GlossaryStillParked
	default:
		return delegatedecision.GlossaryNotMergedYet
	}
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
// them as buttons. Merging is never done by the dashboard: that option
// opens the PR on GitHub.
func delegatedStuckOptions(in delegatedecision.Input, d delegatedecision.Decision, mergerRole bool) []StageTrackerDecisionOption {
	var opts []StageTrackerDecisionOption
	if in.Kind == delegatedecision.KindPR && d.Verdict != delegatedecision.VerdictCloseDuplicate && d.Verdict != delegatedecision.VerdictCloseSuperseded {
		o := StageTrackerDecisionOption{Action: decisionActionOpenGitHub, Label: "Merge as maintainer (on GitHub)", Enabled: mergerRole}
		if !mergerRole {
			o.ReasonIfDisabled = "merger role required"
		}
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
	plainAction := delegatedecision.ActionPlain(d)
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
	out.Outcome = "Hive decided " + plainAction + " at " + stamp + ", but it hasn't taken effect: " + delegatedecision.Plain(why) + "."
	next := "Expected: " + eff.What + ". " + delegatedecision.Detail(why)
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
