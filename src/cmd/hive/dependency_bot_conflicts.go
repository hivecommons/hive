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
)

// renovateRebaseNeverMarker is the line Renovate writes into every PR body it
// opens when the repo's config sets `rebaseWhen: never`. Such a PR is never
// rebased on its own: only ticking the rebase/retry checkbox refreshes it.
const renovateRebaseNeverMarker = "♻ **Rebasing**: Never, or you tick the rebase/retry checkbox."

// renovateBotAuthor is the only dependency bot whose rebase policy this sweep
// reads today; dependabot rebases on conflict by default (hivecommons/hive#11558).
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
// the conflict comment was posted for: at most one comment per head SHA.
// LabelApplied means needs-human was added BY THIS SWEEP and is believed
// present; it is the only needs-human label the undo path may remove.
type depBotConflictRecord struct {
	HeadSHA      string    `json:"head_sha"`
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
}

func loadDepBotConflictStore(path string) *depBotConflictStore {
	s := &depBotConflictStore{path: path, entries: map[string]*depBotConflictRecord{}, noMarker: map[string]string{}}
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

// markCommented records that the conflict comment is on the PR for headSHA.
// Called BEFORE the label call, mirroring escalation.Store.MarkEscalated: once
// the comment is posted nothing may post it again, whatever the label does.
func (s *depBotConflictStore) markCommented(key, headSHA string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = &depBotConflictRecord{HeadSHA: headSHA, CommentedAt: at}
	s.saveLocked()
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

// dependencyBotConflictCandidate is the cheap, call-free half of selection:
// an open, non-draft Renovate PR that conflicts with its base and is not
// already parked (needs-human, hold, do-not-merge).
func dependencyBotConflictCandidate(pr github.PullRequest) bool {
	if pr.Draft || (pr.State != "" && !strings.EqualFold(pr.State, "open")) {
		return false
	}
	if !dependencyBots[pr.Author] || pr.Author != renovateBotAuthor || !conflictMergeState(pr) {
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

// dependencyBotConflictSelected is the whole selection rule: a candidate whose
// body says Renovate will not rebase it without a person.
func dependencyBotConflictSelected(pr github.PullRequest, body string) bool {
	return dependencyBotConflictCandidate(pr) && renovateRebasesNever(body)
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

// runDependencyBotConflictSweep closes hivecommons/hive#11558. A dependency-bot
// PR is outside the fix-loop escalation sweep (isHiveAgentAuthor excludes
// dependencyBots) and the hive never pushes to bot branches, so a conflicting
// Renovate PR in a `rebaseWhen: never` repo had nobody to rebase it and
// nothing saying a person must act. Per pass this deterministic sweep posts
// one comment naming the checkbox to tick and adds needs-human, and takes
// that label back off once the PR stops conflicting or closes — only if this
// sweep is the one that added it. It never pushes, rebases, closes or ticks.
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

	for _, pr := range actionable.PRs.Items {
		if pr.HeadSHA == "" || !dependencyBotConflictCandidate(pr) {
			continue
		}
		repo := qualify(pr.Repo)
		key := escalation.Key(repo, pr.Number)
		if rec, ok := store.get(key); ok && rec.HeadSHA == pr.HeadSHA {
			if !rec.LabelApplied {
				// The comment for this head is already on the PR. Retry the
				// LABEL ONLY: a failed AddLabels on an earlier pass, or the
				// same head conflicting again after an undo.
				applyDepBotConflictLabel(ctx, store, writer, repo, pr.Number, key, logger)
			}
			continue
		}
		if store.lacksMarker(key, pr.HeadSHA) {
			continue
		}
		body, err := reader.GetPRBody(ctx, repo, pr.Number)
		if err != nil {
			logger.Warn("dependency-bot conflict sweep: PR body fetch failed; will retry next pass",
				"repo", repo, "pr", pr.Number, "error", err)
			continue
		}
		if !dependencyBotConflictSelected(pr, body) {
			store.rememberNoMarker(key, pr.HeadSHA)
			continue
		}
		if err := writer.CreateIssueComment(ctx, repo, pr.Number, dependencyBotConflictComment(pr.BaseRef)); err != nil {
			logger.Warn("dependency-bot conflict comment failed; will retry next pass",
				"repo", repo, "pr", pr.Number, "error", err)
			continue
		}
		store.markCommented(key, pr.HeadSHA, now())
		applyDepBotConflictLabel(ctx, store, writer, repo, pr.Number, key, logger)
		logger.Info("conflicting rebaseWhen-never dependency PR handed to a human",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA)
	}
}

func applyDepBotConflictLabel(ctx context.Context, store *depBotConflictStore, writer forge.IssueWriter, repo string, number int, key string, logger *slog.Logger) {
	if err := writer.AddLabels(ctx, repo, number, []string{escalation.NeedsHumanLabel}); err != nil {
		logger.Warn("dependency-bot conflict label failed; will retry next pass",
			"repo", repo, "pr", number, "error", err)
		return
	}
	store.setLabelApplied(key, true)
}

// undoDependencyBotConflicts takes needs-human back off PRs this sweep
// labeled once they stop conflicting or close, following unparkEscalatedPR:
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
			if !depBotConflictResolved(pr) {
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
		if !removeDepBotConflictLabel(ctx, writer, repo, number, logger) {
			continue
		}
		if closed {
			store.forget(key)
		} else {
			// Keep the record: the comment for this head stays the only one.
			store.setLabelApplied(key, false)
		}
		logger.Info("dependency-bot conflict resolved; needs-human removed",
			"repo", repo, "pr", number, "closed", closed)
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

func removeDepBotConflictLabel(ctx context.Context, writer forge.IssueWriter, repo string, number int, logger *slog.Logger) bool {
	remover, ok := writer.(labelRemover)
	if !ok {
		logger.Warn("cannot remove dependency-bot conflict label: forge writer cannot remove labels",
			"repo", repo, "pr", number)
		return false
	}
	if err := remover.RemoveLabel(ctx, repo, number, escalation.NeedsHumanLabel); err != nil {
		logger.Warn("dependency-bot conflict label removal failed; will retry next pass",
			"repo", repo, "pr", number, "error", err)
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
