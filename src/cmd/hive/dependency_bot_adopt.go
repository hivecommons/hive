package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Hive adoption of red dependency-bot PRs (auto_merge.dependency_bot_adopt).
//
// A dependency-bot PR whose bump needs a code change (a Go module major bump
// whose imports still name the old path, say) cannot go green on its own: the
// fix loop refuses another bot's branch (review.FixDispatchExcludedOtherBotAuthor,
// the bot regenerates it), so the red-CI hand-off only told a person. The
// person's one step was then the dashboard's Adopt decision, which adds
// `adoption/supersede`; the adoption path copies the PR to `hive/adopt-<n>`
// and the fix loop repairs the copy.
//
// With the switch on, the sweep takes that step itself at the moment the
// red-CI hand-off would fire, when the bot is a trusted bot author and the
// review swarm's own verdict on that exact head is changes_requested (the
// reviewer named a change; the bump is not merely flaky). It adds the label
// once per bot head and does not post the hand-off for that head. Every
// other case keeps the hand-off, and the adoption path itself is unchanged.

// depBotAdoptReason is the reason logged with an adoption request.
const depBotAdoptReason = "red-ci-changes-requested"

// Skip reasons the sweep logs when the switch is on but the request is not
// made, so the hand-off that follows says why.
const (
	depBotAdoptSkipNotTrustedBot  = "not-trusted-bot-author"
	depBotAdoptSkipAdoptionOff    = "stage-adoption-disabled"
	depBotAdoptSkipHumanMerge     = "human-merge-rule"
	depBotAdoptSkipHoldLabel      = "hold-label"
	depBotAdoptSkipAlreadyAsked   = "adoption-already-requested"
	depBotAdoptSkipNoVerdict      = "no-changes-requested-verdict"
	depBotAdoptSkipHumanMergePath = "human-merge-path"
	depBotAdoptSkipPathUnknown    = "human-merge-path-unreadable"
	depBotAdoptSkipWriteback      = "writeback-refused"
)

// depBotAdoptLabelColor and depBotAdoptLabelDesc are the label the dashboard's
// Adopt decision creates (pkg/dashboard adoptRequestLabelColor/Desc).
const (
	depBotAdoptLabelColor = "5319e7"
	depBotAdoptLabelDesc  = "Owner-delegate request: adopt now in supersede mode"
)

// depBotAdoptPolicy is what the red-CI adoption request reads. The zero value
// is the switch off: the sweep behaves exactly as without it.
type depBotAdoptPolicy struct {
	// Enabled is auto_merge.dependency_bot_adopt.enabled.
	Enabled bool
	// StageAdoption is governor.labels.stage with its adoption half on, the
	// gate the dashboard's Adopt decision checks (stageAdoptionEnabled):
	// without it nothing consumes the label.
	StageAdoption bool
	// TrustedBot reports an auto_merge.trusted_bot_authors login (the PR then
	// rides the self-authored sweep's trusted-bot lane, whose adoption path
	// handles a delegated supersede).
	TrustedBot func(author string) bool
	// HumanMerge reports the auto_merge.human_merge_title_prefixes /
	// human_merge_labels rule (needs-owner-merge, a ⚠️ title).
	HumanMerge func(pr github.PullRequest) bool
	// HeadVerdict is the review swarm's aggregate verdict on exactly this
	// head ("" when none is recorded): the raw swarm verdict, not the
	// trusted-bot lane's advisory downgrade.
	HeadVerdict func(repo string, number int, headSHA string) string
	// HumanMergePath reports that the head touches the repo's
	// auto_merge.human_merge_paths without its pin-only exception admitting
	// it. An error is read as "touches" (fail closed).
	HumanMergePath func(ctx context.Context, repo string, number int, headSHA string) (bool, error)
}

// depBotAdoptPolicyFromConfig builds the live policy. reader is the sweep's
// PR reader; *github.Client answers the human-merge-path check.
func depBotAdoptPolicyFromConfig(cfg *config.Config, reader depBotPRReader, logger *slog.Logger) depBotAdoptPolicy {
	if cfg == nil || !cfg.AutoMerge.DependencyBotAdopt.IsEnabled() {
		return depBotAdoptPolicy{}
	}
	stage := cfg.Governor.Labels.Stage
	rule := humanMergeRuleFor(cfg)
	p := depBotAdoptPolicy{
		Enabled:       true,
		StageAdoption: stage.IsEnabled() && stage.Adoption.IsEnabled(),
		TrustedBot:    func(author string) bool { return dbrTrustedBotAuthor(cfg, author) },
		HumanMerge: func(pr github.PullRequest) bool {
			held, _ := rule.RequiresHumanMerge(pr.Title, pr.Labels)
			return held
		},
		HeadVerdict: func(repo string, number int, headSHA string) string {
			return depBotHeadVerdictFromArtifact(repo, number, headSHA, logger)
		},
	}
	if checker, ok := reader.(interface {
		HumanMergePathHeld(ctx context.Context, repo string, number int, headSHA string) (bool, error)
	}); ok {
		p.HumanMergePath = checker.HumanMergePathHeld
	}
	return p
}

// depBotHeadVerdictFromArtifact reads the swarm's aggregate verdict for
// exactly headSHA from review-verdicts.json, the artifact the automerge
// trusted-bot lane reads before it downgrades the verdict to advisory. A
// missing or unreadable artifact, or no aggregate on this head, is "".
func depBotHeadVerdictFromArtifact(repo string, number int, headSHA string, logger *slog.Logger) string {
	art, err := review.LoadArtifact("")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && logger != nil {
			logger.Warn("dependency-bot adoption: review-verdicts.json unreadable; not adopting", "repo", repo, "pr", number, "error", err)
		}
		return ""
	}
	return depBotHeadVerdict(art, repo, number, headSHA)
}

// depBotHeadVerdict is the newest aggregate verdict on exactly headSHA (no
// fallback to an older head), matched the way the contributor-lane verdict
// gate matches it (contributorLaneVerdictFromArtifact): normalized items,
// case-insensitive repo, under the repo as given or its short name (the
// artifact records either form).
func depBotHeadVerdict(art review.Artifact, repo string, number int, headSHA string) string {
	if strings.TrimSpace(headSHA) == "" || number <= 0 {
		return ""
	}
	repo = strings.TrimSpace(repo)
	_, short, _ := strings.Cut(repo, "/")
	var match review.Aggregate
	found := false
	for _, item := range art.Items {
		item = item.Normalize()
		if item.Number != number || !review.SameHeadSHA(item.HeadSHA, headSHA) {
			continue
		}
		r := strings.TrimSpace(item.Repo)
		if !strings.EqualFold(r, repo) && (short == "" || !strings.EqualFold(r, short)) {
			continue
		}
		if !found || item.RecordedAt.After(match.RecordedAt) {
			match, found = item, true
		}
	}
	if !found {
		return ""
	}
	return string(match.Verdict)
}

// skipReason is why this head gets the person hand-off instead of an
// adoption request, or "" when the request may be made. The cheap checks run
// first; the human-merge-path check may read the PR's files.
func (p depBotAdoptPolicy) skipReason(ctx context.Context, repo string, pr github.PullRequest) string {
	switch {
	case p.TrustedBot == nil || !p.TrustedBot(pr.Author):
		return depBotAdoptSkipNotTrustedBot
	case !p.StageAdoption:
		return depBotAdoptSkipAdoptionOff
	case github.HasHoldLabel(pr.Labels) || hasLabel(pr.Labels, "do-not-merge") || hasLabel(pr.Labels, "do-not-merge/hold"):
		return depBotAdoptSkipHoldLabel
	case p.HumanMerge != nil && p.HumanMerge(pr):
		return depBotAdoptSkipHumanMerge
	case hasLabel(pr.Labels, delegatedecision.LabelAdoptSupersede):
		return depBotAdoptSkipAlreadyAsked
	}
	verdict := ""
	if p.HeadVerdict != nil {
		verdict = p.HeadVerdict(repo, pr.Number, pr.HeadSHA)
	}
	if !strings.EqualFold(strings.TrimSpace(verdict), string(review.VerdictChangesRequested)) {
		return depBotAdoptSkipNoVerdict
	}
	if p.HumanMergePath == nil {
		return depBotAdoptSkipPathUnknown
	}
	held, err := p.HumanMergePath(ctx, repo, pr.Number, pr.HeadSHA)
	switch {
	case err != nil:
		return depBotAdoptSkipPathUnknown
	case held:
		return depBotAdoptSkipHumanMergePath
	}
	return ""
}

// requestDependencyBotAdoption runs at the point the red-CI hand-off would
// be posted for pr's head. It reports true when the hand-off must not be
// posted this pass: the adoption was requested now, or earlier for this same
// head and the label write succeeded, or the write failed this pass (the
// hand-off follows on the next one). Every request is recorded per head
// before the write, so a restart or a failed write never repeats it.
func requestDependencyBotAdoption(ctx context.Context, p depBotAdoptPolicy, store *depBotConflictStore, writer forge.IssueWriter, reader depBotPRReader, repo, key string, pr github.PullRequest, now time.Time, logger *slog.Logger) bool {
	if !p.Enabled || pr.HeadSHA == "" {
		return false
	}
	if rec, ok := store.adoptRequest(key); ok && rec.HeadSHA == pr.HeadSHA {
		// One request per bot head: after a successful one the adoption
		// path owns the PR; after a failed one the person gets it.
		return !rec.Failed
	}
	if why := p.skipReason(ctx, repo, pr); why != "" {
		logger.Info("dependency-bot red CI: hive adoption not requested; handing to a person",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "reason", why)
		return false
	}
	if !depBotWritebackAllowed(ctx, reader, repo, pr.Number, writeback.KindContributorAdopt, delegatedecision.LabelAdoptSupersede, logger) {
		logger.Info("dependency-bot red CI: hive adoption not requested; handing to a person",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "reason", depBotAdoptSkipWriteback)
		return false
	}
	store.markAdoptRequested(key, pr.HeadSHA, now)
	if err := addDepBotAdoptLabel(ctx, writer, repo, pr.Number); err != nil {
		store.markAdoptFailed(key, pr.HeadSHA)
		logger.Warn("dependency-bot red CI: adoption label failed; a person gets the hand-off next pass",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "error", err)
		return true
	}
	logger.Info("dependency-bot red CI: hive adoption requested",
		"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "reason", depBotAdoptReason, "label", delegatedecision.LabelAdoptSupersede)
	return true
}

// depBotLabelEnsurer is the optional label-create call the dashboard's Adopt
// decision makes before adding the label. *github.Client satisfies it.
type depBotLabelEnsurer interface {
	EnsureIssueLabel(ctx context.Context, repo, name, color, description string) error
}

func addDepBotAdoptLabel(ctx context.Context, writer forge.IssueWriter, repo string, number int) error {
	if ensurer, ok := writer.(depBotLabelEnsurer); ok {
		if err := ensurer.EnsureIssueLabel(ctx, repo, delegatedecision.LabelAdoptSupersede, depBotAdoptLabelColor, depBotAdoptLabelDesc); err != nil {
			return err
		}
	}
	return writer.AddLabels(ctx, repo, number, []string{delegatedecision.LabelAdoptSupersede}) // writeback:gated
}

// depBotAdoptRecord is the persisted adoption request for one PR: the bot
// head it was made for and whether the label write failed.
type depBotAdoptRecord struct {
	HeadSHA     string    `json:"head_sha"`
	RequestedAt time.Time `json:"requested_at"`
	Failed      bool      `json:"failed,omitempty"`
}

func (s *depBotConflictStore) adoptRequest(key string) (depBotAdoptRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.adopt[key]
	if r == nil {
		return depBotAdoptRecord{}, false
	}
	return *r, true
}

func (s *depBotConflictStore) markAdoptRequested(key, headSHA string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adopt == nil {
		s.adopt = map[string]*depBotAdoptRecord{}
	}
	s.adopt[key] = &depBotAdoptRecord{HeadSHA: headSHA, RequestedAt: at}
	s.saveAdoptLocked()
}

func (s *depBotConflictStore) markAdoptFailed(key, headSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.adopt[key]; r != nil && r.HeadSHA == headSHA {
		r.Failed = true
		s.saveAdoptLocked()
	}
}

// pruneAdopt forgets PRs that left the open snapshot.
func (s *depBotConflictStore) pruneAdopt(open map[string]github.PullRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k := range s.adopt {
		if _, ok := open[k]; !ok {
			delete(s.adopt, k)
			changed = true
		}
	}
	if changed {
		s.saveAdoptLocked()
	}
}

func depBotAdoptPath(path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimSuffix(path, ".json") + "-adopt.json"
}

func loadDepBotAdoptRecords(path string) map[string]*depBotAdoptRecord {
	out := map[string]*depBotAdoptRecord{}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var m map[string]*depBotAdoptRecord
	if json.Unmarshal(data, &m) == nil && m != nil {
		out = m
	}
	return out
}

func (s *depBotConflictStore) saveAdoptLocked() {
	path := depBotAdoptPath(s.path)
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(s.adopt, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}
