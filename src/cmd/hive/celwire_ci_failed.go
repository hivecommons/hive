package main

import (
	"context"
	"fmt"

	"github.com/hivecommons/hive/pkg/celtrigger"
	"github.com/hivecommons/hive/pkg/github"
)

// ciFailedCheckNameMax bounds the check name quoted in a ci.failed kick.
const ciFailedCheckNameMax = 200

// ciFailedEvent maps a default-branch CI failure from the red-main sweep onto
// a NormalizedEvent. It reuses existing fields: repo (configured spelling),
// title (check name), head_branch (default branch), state (conclusion), and
// number 0, since no issue or PR is involved.
func ciFailedEvent(run github.CIFailedRun) celtrigger.NormalizedEvent {
	return celtrigger.NormalizedEvent{
		Kind:       celtrigger.KindCIFailed,
		Repo:       run.Repo,
		Title:      run.Check,
		HeadBranch: run.Branch,
		State:      run.Conclusion,
	}
}

// ciFailedKickReason is the kick message a matched agent receives.
func ciFailedKickReason(run github.CIFailedRun) string {
	check := run.Check
	if r := []rune(check); len(r) > ciFailedCheckNameMax {
		check = string(r[:ciFailedCheckNameMax]) + "…"
	}
	sha := run.HeadSHA
	if len(sha) > 12 {
		sha = sha[:12]
	}
	msg := fmt.Sprintf("celtrigger %s: check %q concluded %s on the default branch %s of %s at %s",
		celtrigger.KindCIFailed, check, run.Conclusion, run.Branch, run.Repo, sha)
	if run.URL != "" {
		msg += " — " + run.URL
	}
	return msg
}

// ciFailedTriggerWanted reports whether any compiled trigger rule names
// ci.failed. The red-main sweep consults it once per pass; false keeps the
// sweep exactly as it was (no extra reads, nothing emitted).
func (b *boot) ciFailedTriggerWanted() bool {
	if b == nil || b.cfg == nil || b.logger == nil {
		return false
	}
	return celEngineFor(b.cfg, b.logger).ForKind(celtrigger.KindCIFailed).Len() > 0
}

// emitCIFailedTrigger kicks the agents whose ci.failed rules match run,
// through the same gates as every other CEL kick (celTriggerKickAgents:
// agent enabled, not paused, governor-eligible). Only rules that name
// ci.failed are evaluated, so a rule written for issues or PRs never fires
// on a CI failure.
func (b *boot) emitCIFailedTrigger(ctx context.Context, run github.CIFailedRun) {
	if b == nil || b.cfg == nil || b.logger == nil || b.agentMgr == nil {
		return
	}
	engine := celEngineFor(b.cfg, b.logger).ForKind(celtrigger.KindCIFailed)
	if engine.Len() == 0 {
		return
	}
	kicked := celTriggerKickAgents(ctx, engine, ciFailedEvent(run), b.cfg, b.gov, b.agentMgr.IsPaused, b.agentMgr, ciFailedKickReason(run), b.logger)
	if len(kicked) > 0 {
		b.logger.Info("celtrigger: ci.failed kicked agents", "repo", run.Repo, "check", run.Check, "sha", run.HeadSHA, "agents", kicked)
	}
}
