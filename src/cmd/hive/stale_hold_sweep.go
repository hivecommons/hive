package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/stalehold"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/tracing"
)

// The stale-hold sweep: a PR that sits behind a hold label after every check
// went green is raised to a person once, and a hold whose setter added the
// opt-in release-when-green label is released when CI is green. A plain hold
// is never removed here, and nothing (no lgtm, no approval) is ever added.

const staleHoldLedgerPath = "/data/metrics/stale-holds.json"

// staleHoldBlockedReason is the lifecycle "blocked" reason the dashboard
// journey shows for a raised stale hold.
const staleHoldBlockedReason = "stale-hold"

// staleHoldShortSHA is how many characters of a head the notification and
// release comment print.
const staleHoldShortSHA = 7

// CI states as EnrichCIStatus records them on github.PullRequest.CIStatus.
const (
	staleHoldCIStatusSuccess = "success"
	staleHoldCIStatusFailure = "failure"
)

var (
	staleHoldStoreOnce sync.Once
	staleHoldStore     *stalehold.Store
)

func getStaleHoldStore() *stalehold.Store {
	staleHoldStoreOnce.Do(func() {
		staleHoldStore = stalehold.Load(staleHoldLedgerPath)
	})
	return staleHoldStore
}

// staleHoldCleanConclusions are the completed-check conclusions that count
// as green. Anything else completed (cancelled, timed_out, stale, ...) is not
// green, so a hold is never released or reported over it.
var staleHoldCleanConclusions = map[string]bool{"success": true, "skipped": true, "neutral": true}

// staleHoldCI classifies one PR's CI for the stale-hold ledger. Green needs
// the enumeration's own verdict to be success, no required check failing or
// missing, and no completed check on the head with an unclean conclusion.
func staleHoldCI(pr github.PullRequest) string {
	switch pr.CIStatus {
	case staleHoldCIStatusSuccess:
	case staleHoldCIStatusFailure:
		return stalehold.CIRed
	default:
		return stalehold.CIUnknown
	}
	if p := pr.Protection; p != nil {
		if len(p.FailingRequiredChecks) > 0 {
			return stalehold.CIRed
		}
		if len(p.MissingRequiredChecks) > 0 {
			return stalehold.CIUnknown
		}
	}
	if a := pr.CIActivity; a != nil {
		for _, c := range a.Checks {
			if c.State == "completed" && !staleHoldCleanConclusions[strings.ToLower(c.Conclusion)] {
				return stalehold.CIRed
			}
		}
	}
	return stalehold.CIGreen
}

// staleHoldObservations projects every enumerated open PR (held or not, so
// a configured hold label the hive does not itself treat as a hold is still
// seen) into ledger observations.
func staleHoldObservations(cfg *config.Config, actionable *github.ActionableResult) []stalehold.Observation {
	if actionable == nil {
		return nil
	}
	org := ""
	if cfg != nil {
		org = cfg.Project.Org
	}
	out := make([]stalehold.Observation, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held))
	for _, list := range [][]github.PullRequest{actionable.PRs.Held, actionable.PRs.Items} {
		for _, pr := range list {
			out = append(out, stalehold.Observation{
				Repo: fullRepoName(pr.Repo, org), Number: pr.Number, HeadSHA: pr.HeadSHA,
				Labels: pr.Labels, CI: staleHoldCI(pr),
			})
		}
	}
	return out
}

func staleHoldOptions(cfg *config.Config, getenv func(string) string) stalehold.Options {
	var e config.EscalationConfig
	if cfg != nil {
		e = cfg.Escalation
	}
	return stalehold.Options{
		After:        e.EffectiveStaleHoldAfter(getenv),
		HoldLabels:   e.EffectiveStaleHoldLabels(),
		ReleaseLabel: e.EffectiveReleaseWhenGreenLabel(),
	}
}

func shortStaleHoldSHA(sha string) string {
	if len(sha) > staleHoldShortSHA {
		return sha[:staleHoldShortSHA]
	}
	return sha
}

// staleHoldDuration renders a wait in whole minutes ("2h0m", "11h20m"), or
// in seconds below a minute.
func staleHoldDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Truncate(time.Second).String()
	}
	return strings.TrimSuffix(d.Truncate(time.Minute).String(), "0s")
}

// staleHoldReleaseComment is the one comment a release leaves on the PR.
func staleHoldReleaseComment(a stalehold.Action) string {
	quoted := make([]string, 0, len(a.HoldLabels))
	for _, l := range a.HoldLabels {
		quoted = append(quoted, "`"+l+"`")
	}
	return fmt.Sprintf("Released the hold (%s): this PR carries `%s` and every check on %s is green. "+
		"No approval or lgtm label was added; merging still follows the repository's normal rules.",
		strings.Join(quoted, ", "), a.ReleaseLabel, shortStaleHoldSHA(a.HeadSHA))
}

// staleHoldSweepDeps carries the sweep's side effects so tests can observe
// them. send nil means no notification channel; rec nil records nothing.
type staleHoldSweepDeps struct {
	writer forge.IssueWriter
	send   func(title, message string)
	rec    lifecycleRecorder
	store  *stalehold.Store
	now    time.Time
	getenv func(string) string
}

// runStaleHoldSweep folds this enumeration pass into the stale-hold ledger
// and performs the actions due: a one-time notification plus a blocked
// lifecycle event for a PR held and green past the threshold, and the
// removal of the hold labels and the opt-in label for a PR that asked to be
// released when green.
func runStaleHoldSweep(ctx context.Context, cfg *config.Config, actionable *github.ActionableResult, deps staleHoldSweepDeps, logger *slog.Logger) {
	if deps.store == nil || actionable == nil {
		return
	}
	if deps.now.IsZero() {
		deps.now = time.Now().UTC()
	}
	org := ""
	if cfg != nil {
		org = cfg.Project.Org
	}
	for _, a := range deps.store.Sweep(staleHoldObservations(cfg, actionable), deps.now, staleHoldOptions(cfg, deps.getenv)) {
		switch a.Kind {
		case stalehold.ActionNotify:
			raiseStaleHold(ctx, org, a, deps, logger)
		case stalehold.ActionRelease:
			releaseGreenHold(ctx, a, deps.writer, logger)
		}
	}
}

func raiseStaleHold(ctx context.Context, org string, a stalehold.Action, deps staleHoldSweepDeps, logger *slog.Logger) {
	waited := staleHoldDuration(a.GreenFor)
	labels := strings.Join(a.HoldLabels, ", ")
	if deps.send != nil {
		deps.send("PR held with all checks green",
			fmt.Sprintf("%s#%d has been held by %s for %s with every check green on %s. Release the hold or say why it stays.",
				a.Repo, a.Number, labels, waited, shortStaleHoldSHA(a.HeadSHA)))
	}
	recordStaleHoldBlocked(ctx, deps.rec, org, a, waited)
	deps.store.MarkNotified(a.Repo, a.Number, a.HeadSHA)
	logger.Info("stale hold raised to a person", "repo", a.Repo, "pr", a.Number,
		"head_sha", a.HeadSHA, "hold_labels", labels, "green_for", waited)
}

// recordStaleHoldBlocked records the dashboard's blocked lifecycle stage
// for a raised stale hold.
func recordStaleHoldBlocked(ctx context.Context, rec lifecycleRecorder, org string, a stalehold.Action, waited string) {
	if rec == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	ref := timelineRef(org, a.Repo, a.Number)
	if ref == "" {
		return
	}
	event := timeline.Event{IssueRef: ref, Kind: timeline.KindBlocked, Attrs: map[string]string{
		"reason":      staleHoldBlockedReason,
		"hold_labels": strings.Join(a.HoldLabels, ","),
		"green_for":   waited,
		"head_sha":    a.HeadSHA,
	}}
	_, span := tracing.StartTimelineSpan(ctx, event)
	store.Record(event)
	span.End()
}

// releaseGreenHold removes the hold labels, then the opt-in label. Any
// failure stops the release; the opt-in label is still on the PR, so the
// next pass proposes it again.
func releaseGreenHold(ctx context.Context, a stalehold.Action, writer forge.IssueWriter, logger *slog.Logger) {
	remover, ok := writer.(labelRemover)
	if !ok {
		logger.Warn("cannot release a green hold: forge writer cannot remove labels", "repo", a.Repo, "pr", a.Number)
		return
	}
	for _, l := range a.HoldLabels {
		if err := remover.RemoveLabel(ctx, a.Repo, a.Number, l); err != nil { // writeback:solicited the PR carries the opt-in release-when-green label
			logger.Warn("release-when-green: hold label removal failed; will retry next pass",
				"repo", a.Repo, "pr", a.Number, "label", l, "error", err)
			return
		}
	}
	if len(a.HoldLabels) > 0 {
		// Said once, on the pass that removed the hold: a retry that only
		// removes the opt-in label has no hold left to report.
		if err := writer.CreateIssueComment(ctx, a.Repo, a.Number, staleHoldReleaseComment(a)); err != nil { // writeback:solicited the PR carries the opt-in release-when-green label
			logger.Warn("release-when-green: comment failed", "repo", a.Repo, "pr", a.Number, "error", err)
		}
	}
	if err := remover.RemoveLabel(ctx, a.Repo, a.Number, a.ReleaseLabel); err != nil { // writeback:solicited the PR carries the opt-in release-when-green label
		logger.Warn("release-when-green: opt-in label removal failed; will retry next pass",
			"repo", a.Repo, "pr", a.Number, "label", a.ReleaseLabel, "error", err)
		return
	}
	logger.Info("released a green hold on request", "repo", a.Repo, "pr", a.Number,
		"head_sha", a.HeadSHA, "hold_labels", strings.Join(a.HoldLabels, ","), "label", a.ReleaseLabel)
}

// staleHoldNotifierSend adapts a notifier for the sweep; nil stays nil.
func staleHoldNotifierSend(n *notify.Notifier) func(title, message string) {
	if n == nil {
		return nil
	}
	return func(title, message string) { n.Send(title, message, notify.PriorityHigh) }
}
