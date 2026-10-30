package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/rebasemerge"
)

// "Rebase, then merge as maintainer" (hivecommons/hive: #14822 sat 14h at
// 085 behind v5 and "Approve & merge as maintainer" on it would have been a
// GitHub 405). When the operator's merge-now lands on a PR GitHub reports
// behind or dirty, the dashboard records the approval, arms a follow-through
// in pkg/rebasemerge and answers 202 rebase-queued. The hive's merge-
// eligibility router then routes the PR to its fixer whoever authored it
// (an armed PR is routed like an in-place adoption), a hive-authored PR that
// is merely behind gets GitHub's update-branch straight away, and the
// ticker below watches the new head: green → merge with the approval on
// record, conflict / red / timeout → the action fails with the conflicting
// files and the two ways out.

const (
	rowActionRebaseQueued  = "rebase-queued"
	repoItemStatusRebaseQd = "rebase-queued"
	// rebaseMergeTickInterval is how often the follow-through polls GitHub
	// for every live PR; a PR event refetch does not drive it, so this is
	// the latency ceiling between "green" and "merged".
	rebaseMergeTickInterval = 2 * time.Minute
	rebaseMergeFollowedMax  = 6
)

// rebaseMergeStore is the follow-through store (swapped in tests).
var rebaseMergeStore = func() *rebasemerge.Store { return rebasemerge.Shared() }

// rebaseAttemptsStore is the fixer's rebase loop-guard ledger (the router
// in cmd/hive writes it); tests swap it.
var rebaseAttemptsStore = func() *rebasemerge.Attempts { return rebasemerge.SharedAttempts() }

// rebaseMergeNow is the clock (swapped in tests).
var rebaseMergeNow = time.Now

// rebaseMergeFollowThroughLabel / Consequence are the option text the
// decision menu shows instead of "Approve & merge as maintainer" when the
// PR needs a rebase first.
const rebaseMergeFollowThroughLabel = "Rebase, then merge as maintainer"

func rebaseMergeFollowThroughConsequence(number int, base string) string {
	return fmt.Sprintf("hive rebases #%d onto %s, waits for CI, then merges it with your approval on record; you'll get a toast when it lands or if the rebase conflicts", number, nonEmptyString(base, "its base branch"))
}

// armRebaseThenMerge is the merge-now branch for a PR that needs a rebase.
// Order matters: the approval is posted first (the durable record the
// merge carries), then the follow-through is armed, then the rebase is
// kicked.
func (s *Server) armRebaseThenMerge(ctx context.Context, w http.ResponseWriter, repoFull string, number int, user, comment string, st ghpkg.PRMergeState) {
	gh := s.deps.GHClient
	now := rebaseMergeNow()
	hiveAuthored := gh.IsHiveAuthorLogin(st.Author)
	// Workflow paths refuse every author: the App token cannot push them
	// even to the hive's own fork. Plain reachability keeps the hive-authored
	// bypass — the hive pushes its own heads without maintainer edits.
	refusal := ""
	switch {
	case len(st.WorkflowPushPaths) > 0:
		refusal = rebasemerge.FailWorkflowPush
	case !hiveAuthored && !st.PushReachable():
		refusal = rebasemerge.FailNotReachable
	}
	if refusal != "" {
		f := rebasemerge.Arm(repoFull, number, user, st.BaseRef, st.HeadSHA, false, now)
		f.FailNow(now, refusal, st.WorkflowPushPaths)
		// A 200 with a refused result, not an error: the toast shows the
		// reason and a recovery step instead of "HTTP 409".
		jsonStatusResponse(w, http.StatusOK, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: repoItemDecisionMergeNow, Status: rowActionRefused, Head: st.HeadSHA,
			Result: rebaseMergeFailedResult(f)})
		return
	}
	event, err := gh.RecordOperatorPullRequestApprovalWith(ctx, repoFull, number, user, ghpkg.PullRequestApprovalOptions{
		HumanChangeRequests: repoItemDecisionChangeRequests(),
		LiftNeedsHuman:      true,
		SentinelLabel:       s.sentinelLabel(),
	})
	if err != nil {
		jsonError(w, "recording your approval: "+err.Error(), http.StatusBadGateway)
		return
	}
	f := rebasemerge.Arm(repoFull, number, user, st.BaseRef, nonEmptyString(event.HeadSHA, st.HeadSHA), hiveAuthored, now)
	if err := rebaseMergeStore().Put(f); err != nil {
		if errors.Is(err, rebasemerge.ErrAlreadyArmed) {
			prev, _ := rebaseMergeStore().Get(repoFull, number)
			jsonStatusResponse(w, http.StatusAccepted, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: repoItemDecisionMergeNow, Status: repoItemStatusRebaseQd, Head: prev.ApprovedHead,
				Result: rebaseMergeQueuedResult(prev, true)})
			return
		}
		jsonError(w, "arming the rebase-then-merge follow-through: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Solicited: the operator asked for this from the dashboard, so the
	// write-back gate does not apply (pkg/writeback doc; ratchet baseline).
	if err := gh.CreateIssueComment(ctx, repoFull, number, renderRebaseThenMergeComment(user, comment, number, st.BaseRef)); err != nil && s.logger != nil {
		s.logger.Warn("rebase-then-merge: posting note failed", "repo", repoFull, "number", number, "error", err)
	}
	how := "fixer rebase (merge-eligibility conflict route)"
	if hiveAuthored && !st.Conflicting() {
		// Behind, not conflicting: GitHub's own update-branch is the whole
		// rebase for the hive's own PR.
		if err := gh.UpdateBranch(ctx, repoFull, number); err == nil {
			how = "update-branch"
		} else if s.logger != nil {
			s.logger.Warn("rebase-then-merge: update-branch failed; leaving the rebase to the fixer", "repo", repoFull, "number", number, "error", err)
		}
	}
	f.Kicked(now, how)
	_ = rebaseMergeStore().Update(f)
	s.audit.LogRecord(user, auditRepoItemDecisionMerge, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "type", "pr", "head", f.ApprovedHead, "author", st.Author, "base", st.BaseRef, "status", repoItemStatusRebaseQd, "mergeable_state", st.MergeableState, "kick", how), "", repoFull, number)
	if s.logger != nil {
		s.logger.Info("rebase-then-merge armed", "repo", repoFull, "number", number, "actor", user, "head", f.ApprovedHead, "base", st.BaseRef, "mergeable_state", st.MergeableState, "kick", how)
	}
	s.invalidateStageTrackerForWebhook()
	jsonStatusResponse(w, http.StatusAccepted, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: repoItemDecisionMergeNow, Status: repoItemStatusRebaseQd, Head: f.ApprovedHead,
		Released: repoItemApprovalReleased(event), Stage: event.Stage, Result: rebaseMergeQueuedResult(f, false)})
}

func rebaseMergeQueuedResult(f rebasemerge.FollowThrough, already bool) *RowActionResult {
	out := &RowActionResult{Action: rowActionRebaseQueued, Target: rowActionTarget(f.Repo, rowActionTargetPR, f.Number)}
	base := nonEmptyString(f.Base, "its base branch")
	if already {
		out.Outcome = fmt.Sprintf("Already queued: @%s's approval is on record and the hive is rebasing #%d onto %s.", f.Actor, f.Number, base)
	} else {
		out.Outcome = fmt.Sprintf("Your approval is on record; the hive is rebasing #%d onto %s.", f.Number, base)
	}
	out.Next = "Once CI is green on the rebased head the hive merges it with your approval attributed; you'll get a toast when it lands or if the rebase conflicts."
	return out
}

func rebaseMergeFailedResult(f rebasemerge.FollowThrough) *RowActionResult {
	out := &RowActionResult{Action: rowActionRefused, Target: rowActionTarget(f.Repo, rowActionTargetPR, f.Number), Outcome: f.FailureText(), Why: f.FailReason}
	out.Next = "Pick a way out: adopt the commits into a fresh branch, or close it as superseded."
	if f.FailReason == rebasemerge.FailWorkflowPush {
		out.Next = "Rebase the fork branch with credentials that can push workflow changes, then approve and merge again."
	}
	if f.FailReason == rebasemerge.FailForeignHead {
		out.Next = "Read the new head; if it is what you meant to merge, approve it again — or close the PR as superseded."
	}
	out.Options = rebaseMergeFailureOptions(f)
	return out
}

func rebaseMergeFailureOptions(f rebasemerge.FollowThrough) []StageTrackerDecisionOption {
	var out []StageTrackerDecisionOption
	for _, id := range f.Options {
		switch id {
		case rebasemerge.OptionAdoptFresh:
			out = append(out, decisionOption(decisionActionAdopt, rebasemerge.OptionAdoptFreshLabel, true, ""))
		case rebasemerge.OptionCloseSuperseded:
			out = append(out, decisionOption(decisionActionCloseSuperseded, rebasemerge.OptionCloseSupersLabel, true, ""))
		case rebasemerge.OptionReapprove:
			opt := decisionOption(decisionActionMergeNow, rebasemerge.OptionReapproveLabel, true, "")
			opt.Consequence = "records your approval for the head as it is now and has the hive rebase and merge it"
			out = append(out, opt)
		}
	}
	return out
}

func renderRebaseThenMergeComment(user, comment string, number int, base string) string {
	body := fmt.Sprintf("Rebase, then merge as maintainer — @%s approved #%d from the Hive dashboard; the hive rebases it onto `%s`, waits for CI, then merges it with that approval on record.", strings.TrimSpace(user), number, nonEmptyString(base, "its base branch"))
	if c := strings.TrimSpace(comment); c != "" {
		body += "\n\n> " + strings.ReplaceAll(c, "\n", "\n> ")
	}
	return body
}

func renderRebaseThenMergeFailedComment(f rebasemerge.FollowThrough) string {
	body := "Rebase, then merge as maintainer did not land: " + f.FailureText()
	if len(f.ConflictFiles) > 0 {
		if f.FailReason == rebasemerge.FailWorkflowPush {
			body += "\n\nWorkflow paths blocking the push:\n"
		} else {
			body += "\n\nFiles to resolve:\n"
		}
		for _, file := range f.ConflictFiles {
			body += "- `" + file + "`\n"
		}
	}
	if f.FailReason == rebasemerge.FailWorkflowPush {
		return body
	}
	if f.FailReason == rebasemerge.FailForeignHead {
		body += fmt.Sprintf("\n@%s: from the dashboard row, **%s** or **%s**.", f.Actor, rebasemerge.OptionReapproveLabel, rebasemerge.OptionCloseSupersLabel)
		return body
	}
	body += fmt.Sprintf("\n@%s: from the dashboard row, **%s** or **%s**.", f.Actor, rebasemerge.OptionAdoptFreshLabel, rebasemerge.OptionCloseSupersLabel)
	return body
}

// StartRebaseMergeFollowThrough runs the ticker until ctx ends.
func (s *Server) StartRebaseMergeFollowThrough(ctx context.Context) {
	go func() {
		t := time.NewTicker(rebaseMergeTickInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.tickRebaseMerge(ctx)
			}
		}
	}()
}

// tickRebaseMerge drives every live follow-through one observation forward.
func (s *Server) tickRebaseMerge(ctx context.Context) {
	if s == nil || s.deps == nil || s.deps.GHClient == nil {
		return
	}
	gh := s.deps.GHClient
	store := rebaseMergeStore()
	for _, f := range store.Live() {
		st, err := gh.GetPRMergeState(ctx, f.Repo, f.Number)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("rebase-then-merge: reading PR failed", "repo", f.Repo, "number", f.Number, "error", err)
			}
			continue
		}
		s.driveRebaseMerge(ctx, store, f, st)
	}
}

// driveRebaseMerge folds one PR read into its follow-through and performs
// the action the state machine asks for.
func (s *Server) driveRebaseMerge(ctx context.Context, store *rebasemerge.Store, f rebasemerge.FollowThrough, st ghpkg.PRMergeState) {
	gh := s.deps.GHClient
	now := rebaseMergeNow()
	if st.NeedsRebase() && len(st.WorkflowPushPaths) > 0 && !st.Merged {
		f.FailNow(now, rebasemerge.FailWorkflowPush, st.WorkflowPushPaths)
		_ = store.Update(f)
		s.settleRebaseMergeLedger(ctx, f)
		s.invalidateStageTrackerForWebhook()
		return
	}
	obs := rebasemerge.Observation{Now: now, Head: st.HeadSHA, State: st.State, Merged: st.Merged, MergeableState: st.MergeableState, CIRed: st.CIRed}
	if head := strings.TrimSpace(st.HeadSHA); head != "" && head != f.ApprovedHead && head != f.RebasedHead && !f.HiveAuthored && !st.Merged {
		// The approval was bound to ApprovedHead. Before the new head can
		// count as "the rebase", make sure the hive produced it; a read
		// error leaves the decision to the next tick rather than guessing.
		byHive, err := gh.CommitByHive(ctx, f.Repo, head)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("rebase-then-merge: cannot tell who pushed the new head; waiting", "repo", f.Repo, "number", f.Number, "head", head, "error", err)
			}
			return
		}
		obs.HeadByHive = byHive
	}
	if st.Conflicting() {
		if files, err := gh.PRConflictCandidates(ctx, f.Repo, st.BaseRef, st.HeadSHA); err == nil {
			obs.ConflictFiles = files
		}
		if attempts := rebaseAttemptsStore(); attempts != nil {
			if a, ok := attempts.Get(f.Repo, f.Number); ok {
				obs.RebaseAttempt, obs.RebaseExhausted = a.Count(), a.Exhausted()
			}
		}
	}
	wasFailed := f.State == rebasemerge.StateFailed
	action := rebasemerge.Advance(&f, obs, rebasemerge.Deadlines{})
	if !wasFailed && f.State == rebasemerge.StateFailed && f.FailReason == rebasemerge.FailConflict && len(f.ConflictFiles) > 0 {
		if src := s.hotspotSourceRef(); src != nil {
			src.ObserveConflict(f.Repo, f.Number, f.ConflictFiles)
		}
	}
	switch action {
	case rebasemerge.ActionMerge:
		head := nonEmptyString(f.RebasedHead, f.ApprovedHead)
		res, err := gh.MergePRVia(ctx, f.Repo, f.Number, "", head, ghpkg.PRAuditPathDashboard)
		switch {
		case err != nil:
			f.FailNow(now, "merge-refused: "+err.Error(), nil)
		case !res.Merged:
			f.FailNow(now, "merge-refused: "+strings.TrimSpace(res.Message), nil)
		default:
			f.Merged(now, res.SHA)
		}
	}
	_ = store.Update(f)
	s.settleRebaseMergeLedger(ctx, f)
	if !f.Live() {
		s.invalidateStageTrackerForWebhook()
	}
	if s.logger != nil {
		s.logger.Info("rebase-then-merge advanced", "repo", f.Repo, "number", f.Number, "state", f.State, "head", st.HeadSHA, "mergeable_state", st.MergeableState, "action", string(action), "fail_reason", f.FailReason)
	}
}

// settleRebaseMergeLedger closes the operator's pending merge-now record
// when the follow-through ends, and leaves the failure note on the PR.
func (s *Server) settleRebaseMergeLedger(ctx context.Context, f rebasemerge.FollowThrough) {
	switch f.State {
	case rebasemerge.StateMerged:
		_, _ = s.operatorActions().Settle(f.Repo, f.Number, operatorActionMergeNow, operatorActionOutcomeApplied, "merged "+rebaseMergeShortSHA(f.MergeSHA)+" after rebase")
	case rebasemerge.StateFailed:
		_, _ = s.operatorActions().Settle(f.Repo, f.Number, operatorActionMergeNow, operatorActionOutcomeFailed, f.FailureText())
		// The outcome note of that same operator request: solicited, ungated.
		if f.FailReason != rebasemerge.FailClosed && s.deps != nil && s.deps.GHClient != nil {
			if err := s.deps.GHClient.CreateIssueComment(ctx, f.Repo, f.Number, renderRebaseThenMergeFailedComment(f)); err != nil && s.logger != nil {
				s.logger.Warn("rebase-then-merge: posting failure note failed", "repo", f.Repo, "number", f.Number, "error", err)
			}
		}
	}
}

func rebaseMergeShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// rebaseMergeFollowedHops renders the follow-through's steps as ledger hops
// (Number 0, Slug = step) so last_operator_action.followed[] tells the
// trail: approval recorded → rebase kicked → rebased → CI green → merged.
func rebaseMergeFollowedHops(f rebasemerge.FollowThrough) []StageTrackerHop {
	out := make([]StageTrackerHop, 0, len(f.Steps))
	for _, st := range f.Steps {
		out = append(out, StageTrackerHop{Slug: st.Step, Reason: st.Detail, At: st.At})
	}
	if len(out) > rebaseMergeFollowedMax {
		out = out[len(out)-rebaseMergeFollowedMax:]
	}
	return out
}

// StageTrackerRebaseMerge is the row's view of a live or just-settled
// follow-through.
type StageTrackerRebaseMerge struct {
	State string `json:"state"`
	Actor string `json:"actor"`
	Base  string `json:"base,omitempty"`
	// Text is the row caption while it runs: "Rebasing → merge queued
	// (approved by @you)".
	Text          string   `json:"text"`
	FailReason    string   `json:"fail_reason,omitempty"`
	ConflictFiles []string `json:"conflict_files,omitempty"`
	ArmedAt       string   `json:"armed_at"`
}

// stageTrackerRebaseMergeText is the row caption for a follow-through.
func stageTrackerRebaseMergeText(f rebasemerge.FollowThrough) string {
	by := "approved by @" + f.Actor
	switch f.State {
	case rebasemerge.StateApproved, rebasemerge.StateRebasing:
		return "Rebasing → merge queued (" + by + ")"
	case rebasemerge.StateCIPending:
		return "Rebased → waiting for CI, then merge (" + by + ")"
	case rebasemerge.StateMerged:
		return "Merged after rebase (" + by + ")"
	case rebasemerge.StateFailed:
		return "Rebase-then-merge failed: " + f.FailureText()
	}
	return ""
}

// StageTrackerRebase is the row's view of the fixer's autonomous
// rebase-resolve loop guard on a conflicting PR (hivecommons/hive#15103):
// "Rebasing · attempt 1/2", or "Rebase failed · 2/2 — needs a person".
type StageTrackerRebase struct {
	Attempt   int    `json:"attempt"`
	Max       int    `json:"max"`
	Exhausted bool   `json:"exhausted"`
	Head      string `json:"head,omitempty"`
	Text      string `json:"text"`
}

// applyRebaseAttempts stamps rebase onto PR rows with an open conflict
// episode in the fixer's attempts ledger.
func applyRebaseAttempts(items []StageTrackerItem) {
	attempts := rebaseAttemptsStore()
	if attempts == nil {
		return
	}
	for i := range items {
		it := &items[i]
		if it.Kind != "pr" {
			continue
		}
		a, ok := attempts.Get(it.Repo, it.Number)
		if !ok {
			continue
		}
		it.Rebase = &StageTrackerRebase{Attempt: min(max(a.Count(), 1), rebasemerge.MaxRebaseAttempts), Max: rebasemerge.MaxRebaseAttempts, Exhausted: a.Exhausted(), Head: rebaseMergeShortSHA(a.Head()), Text: rebasemerge.AttemptText(a)}
	}
}

// applyRebaseMergeFollowThrough stamps rebase_merge onto rows with a live
// follow-through (or one settled within the hour, so the toast's target
// row can still say what happened) and folds its steps into the ledger's
// followed[].
func (s *Server) applyRebaseMergeFollowThrough(items []StageTrackerItem, now time.Time) {
	store := rebaseMergeStore()
	if store == nil {
		return
	}
	for i := range items {
		it := &items[i]
		if it.Kind != "pr" {
			continue
		}
		f, ok := store.Get(it.Repo, it.Number)
		if !ok || (!f.Live() && now.Sub(f.UpdatedAt) > time.Hour) {
			continue
		}
		it.RebaseMerge = &StageTrackerRebaseMerge{State: f.State, Actor: f.Actor, Base: f.Base, Text: stageTrackerRebaseMergeText(f), FailReason: f.FailReason, ConflictFiles: f.ConflictFiles, ArmedAt: f.ArmedAt.UTC().Format(time.RFC3339)}
		if f.Live() {
			// The row reads the follow-through while it runs; the lane's
			// own caption ("Needs human decision") is what it replaces.
			it.Caption = it.RebaseMerge.Text
		}
		if it.LastOperatorAction != nil && it.LastOperatorAction.Action == operatorActionMergeNow && !it.LastOperatorAction.At.After(f.ArmedAt.Add(time.Minute)) {
			it.LastOperatorAction.Followed = append(rebaseMergeFollowedHops(f), it.LastOperatorAction.Followed...)
		}
	}
}
