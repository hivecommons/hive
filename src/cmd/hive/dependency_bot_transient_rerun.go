package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

// Transient CI re-run for dependency-bot PRs.
//
// An agent can triage a red check on a dependency-bot PR as transient or
// infrastructure (`hive-ci-transient` markers, github.PullRequest.
// TransientCITriaged), for example a mirror outage during a package install
// while the same check passes on the base branch. Before this, only a
// hive-authored PR got the one re-run (rerunTransientTriagedPRs), so such a
// bot PR stayed red and the red-CI hand-off gave it to a person, who then
// only had to click "Re-run".
//
// Now a PR whose author is in auto_merge.trusted_bot_authors (the bots the
// hive already merges like its own PRs) gets the same once-per-head re-run
// through the same ledger and the same write-back gate (kind ci_rerun), and
// the red-CI hand-off waits for it: it holds while the re-run is owed or
// running, and tells a person only when the head is still red after the
// re-run, or when the re-run re-ran nothing (refused by the write-back gate,
// forbidden, or no failed run to re-run), saying which.

// dbrTrustedBotAuthor reports whether author is in
// auto_merge.trusted_bot_authors (config.AutoMergeConfig.TrustedBotAuthorSet,
// the same set the trusted-bot merge lane reads).
func dbrTrustedBotAuthor(cfg *config.Config, author string) bool {
	login := strings.ToLower(strings.TrimSpace(author))
	return cfg != nil && login != "" && cfg.AutoMerge.TrustedBotAuthorSet()[login]
}

// dbrTransientRerunAuthor reports whether rerunTransientTriagedPRs may re-run
// a PR by author: one of the hive's agents, or a trusted bot.
func dbrTransientRerunAuthor(cfg *config.Config, author string) bool {
	return isHiveAgentAuthor(cfg, author) || dbrTrustedBotAuthor(cfg, author)
}

// dbrTrustedBotObservations is the ledger view of the trusted-bot PRs in this
// pass that are not already hive-agent PRs. recordRedStaleness folds them
// into the ledger with ObserveRed only, so their transient-triaged head is
// recorded (and cleared when it goes green or moves) and the re-run can be
// owed; they never enter the fix-loop sweep, the reaper or the red hooks.
func dbrTrustedBotObservations(cfg *config.Config, actionable *github.ActionableResult) []escalation.Observation {
	if cfg == nil || actionable == nil {
		return nil
	}
	var idx []int
	for i, pr := range actionable.PRs.Items {
		if !isHiveAgentAuthor(cfg, pr.Author) && dbrTrustedBotAuthor(cfg, pr.Author) {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return nil
	}
	all := allEscalationObservations(cfg, actionable, nil)
	out := make([]escalation.Observation, 0, len(idx))
	for _, i := range idx {
		out = append(out, all[i])
	}
	return out
}

// dbrTransientLedger is the part of the escalation ledger the dependency-bot
// hand-off reads. *escalation.Store satisfies it.
type dbrTransientLedger interface {
	TransientRerunOwed(repo string, number int, headSHA string) bool
	TransientRerunOutcome(repo string, number int, headSHA string) (spent bool, skipped string)
}

var _ dbrTransientLedger = (*escalation.Store)(nil)

// dbrLedger is the ledger the dependency-bot sweep reads: the shared
// escalation store rerunTransientTriagedPRs writes.
func dbrLedger() dbrTransientLedger {
	return getEscalationStore()
}

// depBotRerunState is where a dependency-bot PR's current head stands with
// the hive's one transient re-run.
type depBotRerunState int

const (
	// The zero value: no re-run is owed or spent on this head (not
	// triaged as transient, or the author is not a trusted bot).
	_ depBotRerunState = iota
	// depBotRerunOwed: the head is transient-triaged and the re-run has
	// not been attempted yet.
	depBotRerunOwed
	// depBotRerunDone: the hive re-ran the head's failed jobs.
	depBotRerunDone
	// depBotRerunNotRun: the re-run is spent but re-ran nothing (Skipped
	// says why: a write-back gate reason, "rerun-forbidden",
	// "no-failed-runs", "rerun-failed").
	depBotRerunNotRun
)

// depBotTransientRerun is depBotRerunState plus the reason a spent re-run
// re-ran nothing.
type depBotTransientRerun struct {
	State   depBotRerunState
	Skipped string
}

// depBotTransientRerunBackstop is how long past depBotRedCIGrace the red-CI
// hand-off keeps waiting on a re-run that is owed but has not happened (the
// re-run step keeps failing to list the runs); after that a person is told
// anyway, so the PR is never left red with nobody told.
const depBotTransientRerunBackstop = depBotRedCIGrace

// depBotTransientRerunFor reads the ledger for pr's current head. repo is
// the fully qualified repository.
func depBotTransientRerunFor(ledger dbrTransientLedger, repo string, pr github.PullRequest) depBotTransientRerun {
	if ledger == nil || pr.HeadSHA == "" {
		return depBotTransientRerun{}
	}
	if spent, skipped := ledger.TransientRerunOutcome(repo, pr.Number, pr.HeadSHA); spent {
		if skipped == "" {
			return depBotTransientRerun{State: depBotRerunDone}
		}
		return depBotTransientRerun{State: depBotRerunNotRun, Skipped: skipped}
	}
	if pr.TransientCITriaged && ledger.TransientRerunOwed(repo, pr.Number, pr.HeadSHA) {
		return depBotTransientRerun{State: depBotRerunOwed}
	}
	return depBotTransientRerun{}
}

// holdsHandoff reports that the red-CI hand-off must wait: the re-run is
// still owed, and either the head has not been red past the backstop or the
// re-run has been owed for less than depBotTransientRerunBackstop. The second
// clause covers a triage that lands late: without it a head red for longer
// than the backstop was handed off (needs-human) in the same pass that first
// saw the re-run owed, and the label then kept the re-run from ever running.
func (r depBotTransientRerun) holdsHandoff(red, owed time.Duration) bool {
	return r.State == depBotRerunOwed && (red < depBotRedCIGrace+depBotTransientRerunBackstop || owed < depBotTransientRerunBackstop)
}

// depBotTransientRerunRunning reports that the hive re-ran this head's
// failed jobs and CI has not settled since (no failing required check yet,
// not green). The sweep keeps the red-CI grace clock running through it, so
// a head that fails again is handed off on its first red sighting's clock
// instead of a fresh one.
func depBotTransientRerunRunning(ledger dbrTransientLedger, repo string, pr github.PullRequest) bool {
	if ledger == nil || strings.EqualFold(strings.TrimSpace(pr.CIStatus), "success") {
		return false
	}
	spent, skipped := ledger.TransientRerunOutcome(repo, pr.Number, pr.HeadSHA)
	return spent && skipped == ""
}

// depBotRerunNotRunText says, in plain words, why the hive's one re-run of
// a transient-triaged head re-ran nothing.
func depBotRerunNotRunText(skipped string) string {
	switch skipped {
	case "no-failed-runs":
		return "Hive found no failed GitHub Actions run on this head that it could re-run"
	case "rerun-failed":
		return "GitHub did not accept Hive's request to re-run it"
	}
	return fmt.Sprintf("Hive was not allowed to re-run it (`%s`)", skipped)
}
