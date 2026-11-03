package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// renovateRebaseNeverMarker is the line Renovate writes into every PR body it
// opens when the repo's config sets `rebaseWhen: never`. Such a PR is never
// rebased on its own: only ticking the rebase/retry checkbox refreshes it.
const renovateRebaseNeverMarker = "♻ **Rebasing**: Never, or you tick the rebase/retry checkbox."

// renovateBotAuthor is the only dependency bot whose rebase policy this sweep
// reads from the PR body (hivecommons/hive#11558). Other bots, and Renovate
// PRs without the never marker, are handed off only after depBotConflictGrace.
const renovateBotAuthor = "renovate[bot]"

// depBotConflictLedgerPath sits beside the fix-loop ledger on the PVC so a
// restart never re-posts the conflict comment (hivecommons/hive#11558).
const depBotConflictLedgerPath = "/data/metrics/dependency-bot-conflicts.json"

// depBotPRReader is the read side the sweep needs beyond the enumerated
// snapshot: the PR body (github.PullRequest keeps none) and, for a tracked PR
// that left the snapshot, whether it was closed. *github.Client satisfies it.
type depBotPRReader interface {
	GetPRBody(ctx context.Context, repo string, number int) (string, error)
	GetPRState(ctx context.Context, repo string, number int) (github.PRState, error)
}

// depBotConflictRecord is the durable record for one PR. HeadSHA is the head
// the hand-off comment was posted for and Reason why (depBotHandoff*; empty
// is a conflict): at most one comment per head SHA and reason.
// LabelApplied means needs-human was added BY THIS SWEEP and is believed
// present; it is the only needs-human label the undo path may remove.
type depBotConflictRecord struct {
	HeadSHA      string    `json:"head_sha"`
	Reason       string    `json:"reason,omitempty"`
	CommentedAt  time.Time `json:"commented_at"`
	LabelApplied bool      `json:"label_applied,omitempty"`
}

type depBotConflictStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]*depBotConflictRecord

	// noMarker remembers, in memory only, the head SHA whose body lacked the
	// rebaseWhen-never marker, so an unrelated conflicting bot PR costs one
	// body GET per head rather than one per pass.
	noMarker map[string]string

	// observed holds the persisted grace clocks (dependency_bot_handoffs.go);
	// holds, in memory only, the automerge sweep's terminal required-check
	// holds per PR head.
	observed map[string]*depBotObservation
	holds    map[string]depBotHold
}

func loadDepBotConflictStore(path string) *depBotConflictStore {
	s := &depBotConflictStore{
		path:     path,
		entries:  map[string]*depBotConflictRecord{},
		noMarker: map[string]string{},
		observed: loadDepBotObservations(depBotObservedPath(path)),
		holds:    map[string]depBotHold{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var m map[string]*depBotConflictRecord
	if json.Unmarshal(data, &m) == nil && m != nil {
		s.entries = m
	}
	return s
}

var (
	depBotConflictStoreOnce sync.Once
	depBotConflictStoreVal  *depBotConflictStore
)

func getDepBotConflictStore() *depBotConflictStore {
	depBotConflictStoreOnce.Do(func() {
		depBotConflictStoreVal = loadDepBotConflictStore(depBotConflictLedgerPath)
	})
	return depBotConflictStoreVal
}

func (s *depBotConflictStore) get(key string) (depBotConflictRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key]
	if e == nil {
		return depBotConflictRecord{}, false
	}
	return *e, true
}

func (s *depBotConflictStore) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for k := range s.entries {
		out = append(out, k)
	}
	return out
}

// markCommented records that the hand-off comment for reason is on the PR for
// headSHA. Called BEFORE the label call, mirroring escalation.Store.MarkEscalated:
// once the comment is posted nothing may post it again, whatever the label does.
func (s *depBotConflictStore) markCommented(key, headSHA, reason string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = &depBotConflictRecord{HeadSHA: headSHA, Reason: reason, CommentedAt: at}
	delete(s.observed, key)
	s.saveLocked()
	s.saveObservedLocked()
}

// setLabelApplied mirrors escalation.Store.MarkLabelApplied (true) and records
// a completed undo (false).
func (s *depBotConflictStore) setLabelApplied(key string, applied bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[key]; e != nil {
		e.LabelApplied = applied
	}
	s.saveLocked()
}

func (s *depBotConflictStore) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
	s.saveLocked()
}

func (s *depBotConflictStore) lacksMarker(key, headSHA string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sha, ok := s.noMarker[key]
	return ok && sha == headSHA
}

func (s *depBotConflictStore) rememberNoMarker(key, headSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noMarker[key] = headSHA
}

func (s *depBotConflictStore) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// dependencyBotHandoffCandidate is the cheap, call-free half of selection: an
// open, non-draft dependency-bot PR (dependencyBots) that is not already
// parked (needs-human, hold, do-not-merge). dependencyBotHandoffReason says
// whether it needs a person.
func dependencyBotHandoffCandidate(pr github.PullRequest) bool {
	if pr.Draft || (pr.State != "" && !strings.EqualFold(pr.State, "open")) {
		return false
	}
	if !dependencyBots[pr.Author] {
		return false
	}
	if escalation.HasNeedsHumanLabel(pr.Labels) || github.HasHoldLabel(pr.Labels) || hasLabel(pr.Labels, "do-not-merge") {
		return false
	}
	return true
}

// renovateRebasesNever reports whether a PR body carries Renovate's
// `rebaseWhen: never` policy line.
func renovateRebasesNever(body string) bool {
	return strings.Contains(body, renovateRebaseNeverMarker)
}

// dependencyBotConflictCandidate is a conflicting hand-off candidate.
func dependencyBotConflictCandidate(pr github.PullRequest) bool {
	return dependencyBotHandoffCandidate(pr) && conflictMergeState(pr)
}

// dependencyBotConflictSelected is the immediate conflict hand-off: a
// conflicting Renovate PR whose body says Renovate will not rebase it without
// a person. Every other conflicting candidate waits depBotConflictGrace.
func dependencyBotConflictSelected(pr github.PullRequest, body string) bool {
	return dependencyBotConflictCandidate(pr) && pr.Author == renovateBotAuthor && renovateRebasesNever(body)
}

// dependencyBotConflictComment is the one plain-words comment the sweep posts.
func dependencyBotConflictComment(base string) string {
	target := "the base branch"
	if b := strings.TrimSpace(base); b != "" {
		target = "`" + b + "`"
	}
	return fmt.Sprintf("This Renovate PR conflicts with %s and this repo's Renovate config only rebases when the box is ticked. "+
		"To fix it, tick 'If you want to rebase/retry this PR, check this box' in the description; "+
		"Renovate regenerates the branch on its next run.", target)
}

// runDependencyBotConflictSweep closes hivecommons/hive#11558 and #12379. A
// dependency-bot PR is outside the fix-loop escalation sweep
// (isHiveAgentAuthor excludes dependencyBots) and the hive never pushes to bot
// branches, so a bot PR stuck on something only a person can clear had
// nobody saying so. Per pass this deterministic sweep hands such a PR to a
// person with one comment naming the action and needs-human:
//   - a conflict: at once for a Renovate `rebaseWhen: never` PR, otherwise
//     once it has conflicted past depBotConflictGrace;
//   - a failing required check that stayed red on the same head past
//     depBotRedCIGrace (the PR stays out of the fix loop);
//   - a terminal required-check hold the automerge sweep reported (held for
//     approval, never started, never ran).
//
// It takes that label back off once the reason is over (see
// depBotHandoffResolved) or the PR closes — only if this sweep is the one
// that added it. It never pushes, rebases, closes, approves or ticks.
func runDependencyBotConflictSweep(ctx context.Context, cfg *config.Config, writer forge.IssueWriter, reader depBotPRReader, actionable *github.ActionableResult, logger *slog.Logger) {
	org := ""
	if cfg != nil {
		org = cfg.Project.Org
	}
	sweepDependencyBotConflicts(ctx, getDepBotConflictStore(), org, writer, reader, actionable, time.Now, logger)
}

func sweepDependencyBotConflicts(
	ctx context.Context,
	store *depBotConflictStore,
	org string,
	writer forge.IssueWriter,
	reader depBotPRReader,
	actionable *github.ActionableResult,
	now func() time.Time,
	logger *slog.Logger,
) {
	if store == nil || writer == nil || reader == nil || actionable == nil {
		return
	}
	qualify := func(repo string) string {
		if !strings.Contains(repo, "/") && org != "" {
			return org + "/" + repo
		}
		return repo
	}

	seen := map[string]github.PullRequest{}
	for _, pr := range actionable.PRs.Items {
		seen[escalation.Key(qualify(pr.Repo), pr.Number)] = pr
	}
	for _, pr := range actionable.PRs.Held {
		key := escalation.Key(qualify(pr.Repo), pr.Number)
		if _, ok := seen[key]; !ok {
			seen[key] = pr
		}
	}

	undoDependencyBotConflicts(ctx, store, writer, reader, seen, logger)

	waiting := map[string]bool{}
	defer store.pruneObservations(waiting)
	for _, pr := range actionable.PRs.Items {
		if pr.HeadSHA == "" || !dependencyBotHandoffCandidate(pr) {
			continue
		}
		repo := qualify(pr.Repo)
		key := escalation.Key(repo, pr.Number)
		heldReason := store.heldReason(key, pr.HeadSHA)
		reason, ok := dependencyBotHandoffReason(pr, heldReason)
		if !ok {
			continue
		}
		if rec, ok := store.get(key); ok && rec.HeadSHA == pr.HeadSHA && rec.Reason == reason {
			if !rec.LabelApplied {
				// The comment for this head is already on the PR. Retry the
				// LABEL ONLY: a failed AddLabels on an earlier pass, or the
				// same head stuck again after an undo.
				applyDepBotConflictLabel(ctx, store, writer, reader, repo, pr.Number, key, logger)
			}
			continue
		}
		var comment string
		switch reason {
		case depBotHandoffHeld:
			comment = dependencyBotHeldCheckComment(pr.Author, heldReason)
		case depBotHandoffConflict:
			if pr.Author == renovateBotAuthor && !store.lacksMarker(key, pr.HeadSHA) {
				body, err := reader.GetPRBody(ctx, repo, pr.Number)
				if err != nil {
					logger.Warn("dependency-bot conflict sweep: PR body fetch failed; will retry next pass",
						"repo", repo, "pr", pr.Number, "error", err)
					waiting[key] = true
					continue
				}
				if dependencyBotConflictSelected(pr, body) {
					comment = dependencyBotConflictComment(pr.BaseRef)
				} else {
					store.rememberNoMarker(key, pr.HeadSHA)
				}
			}
			if comment == "" {
				waiting[key] = true
				if now().Sub(store.observe(key, pr.HeadSHA, reason, now())) < depBotConflictGrace {
					continue
				}
				comment = dependencyBotWaitedConflictComment(pr.Author, pr.BaseRef)
			}
		case depBotHandoffRedCI:
			waiting[key] = true
			if now().Sub(store.observe(key, pr.HeadSHA, reason, now())) < depBotRedCIGrace {
				continue
			}
			comment = dependencyBotRedCIComment(pr)
		}
		if !depBotWritebackAllowed(ctx, reader, repo, pr.Number, writeback.KindNeedsDecisionComment, comment, logger) {
			continue
		}
		if err := writer.CreateIssueComment(ctx, repo, pr.Number, comment); err != nil { // writeback:gated
			logger.Warn("dependency-bot hand-off comment failed; will retry next pass",
				"repo", repo, "pr", pr.Number, "reason", depBotHandoffLogReason(reason), "error", err)
			continue
		}
		store.markCommented(key, pr.HeadSHA, reason, now())
		delete(waiting, key)
		applyDepBotConflictLabel(ctx, store, writer, reader, repo, pr.Number, key, logger)
		logger.Info("stuck dependency-bot PR handed to a human",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "reason", depBotHandoffLogReason(reason))
	}
}

func depBotHandoffLogReason(reason string) string {
	if reason == depBotHandoffConflict {
		return "conflict"
	}
	return reason
}

func applyDepBotConflictLabel(ctx context.Context, store *depBotConflictStore, writer forge.IssueWriter, reader depBotPRReader, repo string, number int, key string, logger *slog.Logger) {
	if !depBotWritebackAllowed(ctx, reader, repo, number, writeback.KindStageLabel, escalation.NeedsHumanLabel, logger) {
		return
	}
	if err := writer.AddLabels(ctx, repo, number, []string{escalation.NeedsHumanLabel}); err != nil { // writeback:gated
		logger.Warn("dependency-bot conflict label failed; will retry next pass",
			"repo", repo, "pr", number, "error", err)
		return
	}
	store.setLabelApplied(key, true)
}

// undoDependencyBotConflicts takes needs-human back off PRs this sweep
// labeled once their hand-off reason is over or they close, following unparkEscalatedPR:
// a failed removal is retried next pass, and a label this sweep did not add
// is never touched.
func undoDependencyBotConflicts(ctx context.Context, store *depBotConflictStore, writer forge.IssueWriter, reader depBotPRReader, seen map[string]github.PullRequest, logger *slog.Logger) {
	for _, key := range store.keys() {
		rec, ok := store.get(key)
		if !ok {
			continue
		}
		repo, number, ok := splitEscalationKey(key)
		if !ok {
			store.forget(key)
			continue
		}
		closed := false
		if pr, visible := seen[key]; visible {
			if !depBotHandoffResolved(rec, pr) {
				continue
			}
			if !rec.LabelApplied {
				continue
			}
			if !escalation.HasNeedsHumanLabel(pr.Labels) {
				// Someone already took it off; nothing left to undo.
				store.setLabelApplied(key, false)
				continue
			}
		} else {
			st, err := reader.GetPRState(ctx, repo, number)
			if err != nil || !strings.EqualFold(st.State, "closed") {
				// Still open (draft, exempt, or a partial enumeration) or
				// unknown: keep the record and look again next pass.
				continue
			}
			closed = true
			if !rec.LabelApplied {
				store.forget(key)
				continue
			}
		}
		if !removeDepBotConflictLabel(ctx, writer, reader, repo, number, logger) {
			continue
		}
		if closed {
			store.forget(key)
		} else {
			// Keep the record: the comment for this head stays the only one.
			store.setLabelApplied(key, false)
		}
		logger.Info("dependency-bot hand-off resolved; needs-human removed",
			"repo", repo, "pr", number, "reason", depBotHandoffLogReason(rec.Reason), "closed", closed)
	}
}

// depBotConflictResolved reports that GitHub has computed a merge state for
// the PR and it is not a conflict. An unknown state is not evidence of a
// rebase, so it never triggers the undo.
func depBotConflictResolved(pr github.PullRequest) bool {
	state := strings.ToLower(strings.TrimSpace(pr.MergeableState))
	if state == "" || state == "unknown" {
		return false
	}
	return !conflictMergeState(pr)
}

func removeDepBotConflictLabel(ctx context.Context, writer forge.IssueWriter, reader depBotPRReader, repo string, number int, logger *slog.Logger) bool {
	remover, ok := writer.(labelRemover)
	if !ok {
		logger.Warn("cannot remove dependency-bot conflict label: forge writer cannot remove labels",
			"repo", repo, "pr", number)
		return false
	}
	if !depBotWritebackAllowed(ctx, reader, repo, number, writeback.KindStageLabel, escalation.NeedsHumanLabel, logger) {
		return false
	}
	if err := remover.RemoveLabel(ctx, repo, number, escalation.NeedsHumanLabel); err != nil { // writeback:gated
		logger.Warn("dependency-bot conflict label removal failed; will retry next pass",
			"repo", repo, "pr", number, "error", err)
		return false
	}
	return true
}

func depBotWritebackAllowed(ctx context.Context, reader depBotPRReader, repo string, number int, kind writeback.Kind, preview string, logger *slog.Logger) bool {
	client, ok := reader.(*github.Client)
	if !ok {
		return true
	}
	item, err := client.WritebackItemForNumber(ctx, repo, number)
	if err != nil {
		logger.Warn("dependency-bot hand-off: writeback target lookup failed", "repo", repo, "pr", number, "error", err)
		return false
	}
	if d := client.WritebackAllowed(repo, item, kind, preview); !d.Allowed {
		logger.Info("dependency-bot hand-off withheld by write-back gate", "repo", repo, "pr", number, "reason", d.Reason)
		return false
	}
	return true
}

// splitEscalationKey inverts escalation.Key ("owner/repo#number").
func splitEscalationKey(key string) (string, int, bool) {
	i := strings.LastIndex(key, "#")
	if i <= 0 || i == len(key)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return key[:i], n, true
}
