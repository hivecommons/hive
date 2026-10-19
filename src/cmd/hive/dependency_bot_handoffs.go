package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

// Hand-off reasons the dependency-bot sweep records (hivecommons/hive#12379).
// The empty string is the conflict reason so ledger records written before
// the sweep handled more than conflicts keep their meaning.
const (
	depBotHandoffConflict = ""
	depBotHandoffRedCI    = "red-ci"
	depBotHandoffHeld     = "check-held"
)

// depBotConflictGrace is how long a dependency-bot PR may conflict with its
// base before a person is told. Renovate (default rebaseWhen) and Dependabot
// rebase conflicting PRs on their own; this is long enough for that, so only
// a PR the bot has stopped updating is handed off. A Renovate PR whose body
// says it rebases never is handed off at once.
const depBotConflictGrace = 24 * time.Hour

// depBotRedCIGrace is how long a failing required check may stay red on the
// same dependency-bot head before a person is told: long enough for a flaky
// re-run or a bot update, short enough that a broken bump is not left alone.
const depBotRedCIGrace = 4 * time.Hour

// Terminal required-check hold prefixes reported by the automerge sweep
// (automerge.isTerminalRequiredCheckHold).
const (
	depBotHoldHeldPrefix         = "required-check-held:"
	depBotHoldNeverStartedPrefix = "required-check-never-started:"
	depBotHoldNeverRanPrefix     = "required-check-never-ran:"
)

// depBotObservation is when a hand-off reason was first seen on a PR head.
// It is persisted so a restart does not restart the grace period.
type depBotObservation struct {
	HeadSHA string    `json:"head_sha"`
	Reason  string    `json:"reason,omitempty"`
	Since   time.Time `json:"since"`
}

// depBotHold is the latest terminal required-check hold the automerge sweep
// reported for a PR head.
type depBotHold struct {
	HeadSHA string
	Reason  string
}

// dependencyBotRedCI reports a red required check on the PR's current head
// that the bot PR itself must answer for: CI has settled, and the failure was
// not triaged as an upstream or shared-CI incident. A failure triaged as
// transient (TransientCITriaged) is still handed off, because nothing in Hive
// re-runs a single PR's check, but dependencyBotRedCIComment then asks for a
// re-run instead of blaming the bump (hivecommons/hive#12977).
func dependencyBotRedCI(pr github.PullRequest) bool {
	return pr.HasFailingRequiredCheck() && !pr.CIChecksRunning && !pr.ExternalCIDeferred && pr.SharedCIIncident == 0
}

// dependencyBotHandoffReason picks the one reason a candidate PR needs a
// person now, or "" with ok=false. heldReason is the automerge sweep's
// terminal hold for the PR's current head, if any.
func dependencyBotHandoffReason(pr github.PullRequest, heldReason string) (string, bool) {
	switch {
	case heldReason != "":
		return depBotHandoffHeld, true
	case conflictMergeState(pr):
		return depBotHandoffConflict, true
	case dependencyBotRedCI(pr):
		return depBotHandoffRedCI, true
	}
	return "", false
}

// depBotHandoffResolved reports that the condition a record was posted for is
// over, so the needs-human label this sweep added may come off.
func depBotHandoffResolved(rec depBotConflictRecord, pr github.PullRequest) bool {
	switch rec.Reason {
	case depBotHandoffRedCI:
		return strings.EqualFold(strings.TrimSpace(pr.CIStatus), "success")
	case depBotHandoffHeld:
		// A new head re-creates the checks; a check that completes green
		// lets automerge merge (closed is handled by the caller).
		return pr.HeadSHA != "" && pr.HeadSHA != rec.HeadSHA
	default:
		return depBotConflictResolved(pr)
	}
}

func dependencyBotDisplayName(author string) string {
	switch author {
	case renovateBotAuthor:
		return "Renovate"
	case "dependabot[bot]":
		return "Dependabot"
	}
	return strings.TrimSuffix(author, "[bot]")
}

func depBotBaseTarget(base string) string {
	if b := strings.TrimSpace(base); b != "" {
		return "`" + b + "`"
	}
	return "the base branch"
}

func depBotHours(d time.Duration) string {
	h := int(d / time.Hour)
	if h == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}

// dependencyBotWaitedConflictComment is posted when a bot that normally
// rebases on its own has left the PR conflicting past depBotConflictGrace.
func dependencyBotWaitedConflictComment(author, base string) string {
	name := dependencyBotDisplayName(author)
	target := depBotBaseTarget(base)
	waited := depBotHours(depBotConflictGrace)
	switch author {
	case renovateBotAuthor:
		return fmt.Sprintf("This Renovate PR has conflicted with %s for over %s and Renovate has not rebased it; "+
			"Renovate stops updating a branch once someone else has pushed to it. "+
			"To fix it, tick 'If you want to rebase/retry this PR, check this box' in the description; "+
			"Renovate regenerates the branch on its next run.", target, waited)
	case "dependabot[bot]":
		return fmt.Sprintf("This Dependabot PR has conflicted with %s for over %s and Dependabot has not rebased it; "+
			"Dependabot stops rebasing when `rebase-strategy: disabled` is set or the PR is more than 30 days old. "+
			"To fix it, comment `@dependabot rebase` on this PR, or rebase the branch by hand.", target, waited)
	}
	return fmt.Sprintf("This %s PR has conflicted with %s for over %s and the bot has not rebased it. "+
		"To fix it, rebase the branch by hand, or close the PR so the bot can open a fresh one.", name, target, waited)
}

// dependencyBotRedCIComment is posted when a required check has stayed red on
// the PR's current head past depBotRedCIGrace. It does not assume the bump is
// at fault: when a hive-ci-transient triage marker covers every failing run
// at this head, it says so and asks only for a re-run (hivecommons/hive#12977).
func dependencyBotRedCIComment(pr github.PullRequest) string {
	checks := "a required check"
	if len(pr.FailingChecks) > 0 {
		names := pr.FailingChecks
		more := ""
		if len(names) > 5 {
			more = fmt.Sprintf(" and %d more", len(names)-5)
			names = names[:5]
		}
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = "`" + n + "`"
		}
		checks = strings.Join(quoted, ", ") + more
	}
	name := dependencyBotDisplayName(pr.Author)
	waited := depBotHours(depBotRedCIGrace)
	if pr.TransientCITriaged {
		return fmt.Sprintf("This %s PR has failed %s on its current head for over %s. "+
			"Hive's triage of this head found the failure transient or infrastructure-related, not caused by the bump, "+
			"and nothing has re-run the check since. "+
			"To fix it, re-run the failed check once the cause has cleared.",
			name, checks, waited)
	}
	return fmt.Sprintf("This %s PR has failed %s on its current head for over %s. "+
		"Hive does not repair dependency-bot PRs. "+
		"If the failure was transient (a flaky job or an infrastructure problem), re-run the failed check. "+
		"If the bump breaks the build, fix the code or lockfile on this branch, pin or ignore the dependency in the bot's config, or close the PR; a rebase will not fix it.",
		name, checks, waited)
}

// dependencyBotHeldCheckComment is posted for a terminal required-check hold
// the automerge sweep reported (held for approval, never started, never ran).
func dependencyBotHeldCheckComment(author, holdReason string) string {
	name := dependencyBotDisplayName(author)
	check := func(prefix string) string {
		if c := strings.TrimSpace(strings.TrimPrefix(holdReason, prefix)); c != "" {
			return "`" + c + "`"
		}
		return "a required check"
	}
	switch {
	case strings.HasPrefix(holdReason, depBotHoldHeldPrefix):
		return fmt.Sprintf("The required check %s on this %s PR is waiting for approval to run, and Hive could not approve it. "+
			"To fix it, click 'Approve and run' on the held workflow runs (or run `gh run approve <run-id>`), or merge the PR by hand.",
			check(depBotHoldHeldPrefix), name)
	case strings.HasPrefix(holdReason, depBotHoldNeverRanPrefix):
		return fmt.Sprintf("The required check %s has not run on this %s PR's current head, even after a rebase was requested. "+
			"To fix it, push to the branch again or close and reopen the PR so the check is created, or merge the PR by hand.",
			check(depBotHoldNeverRanPrefix), name)
	}
	return fmt.Sprintf("The required check %s never started on this %s PR's current head. "+
		"To fix it, push to the branch again or close and reopen the PR so the check is created, or merge the PR by hand.",
		check(depBotHoldNeverStartedPrefix), name)
}

// noteDependencyBotTerminalHold is the automerge sweep's OnTerminalHold hook:
// it records the hold for the next dependency-bot hand-off pass. The pass
// itself decides whether the PR is a dependency-bot PR.
func noteDependencyBotTerminalHold(repo string, number int, headSHA, reason string) {
	getDepBotConflictStore().noteHold(escalation.Key(repo, number), headSHA, reason)
}

func (s *depBotConflictStore) noteHold(key, headSHA, reason string) {
	if headSHA == "" || reason == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds == nil {
		s.holds = map[string]depBotHold{}
	}
	s.holds[key] = depBotHold{HeadSHA: headSHA, Reason: reason}
}

// heldReason returns the terminal hold reported for exactly this head, and
// forgets a hold reported for an older head.
func (s *depBotConflictStore) heldReason(key, headSHA string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.holds[key]
	if !ok {
		return ""
	}
	if !strings.EqualFold(h.HeadSHA, headSHA) {
		delete(s.holds, key)
		return ""
	}
	return h.Reason
}

// observe returns when reason was first seen on headSHA for key, recording
// now if this is the first sighting of that head and reason.
func (s *depBotConflictStore) observe(key, headSHA, reason string, now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.observed == nil {
		s.observed = map[string]*depBotObservation{}
	}
	if o := s.observed[key]; o != nil && o.HeadSHA == headSHA && o.Reason == reason {
		return o.Since
	}
	s.observed[key] = &depBotObservation{HeadSHA: headSHA, Reason: reason, Since: now}
	s.saveObservedLocked()
	return now
}

// pruneObservations drops grace clocks for PRs that no longer show a waiting
// reason this pass, so a later sighting starts a fresh grace period.
func (s *depBotConflictStore) pruneObservations(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k := range s.observed {
		if !keep[k] {
			delete(s.observed, k)
			changed = true
		}
	}
	if changed {
		s.saveObservedLocked()
	}
}

func depBotObservedPath(path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimSuffix(path, ".json") + "-observed.json"
}

func loadDepBotObservations(path string) map[string]*depBotObservation {
	out := map[string]*depBotObservation{}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var m map[string]*depBotObservation
	if json.Unmarshal(data, &m) == nil && m != nil {
		out = m
	}
	return out
}

func (s *depBotConflictStore) saveObservedLocked() {
	path := depBotObservedPath(s.path)
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(s.observed, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}
