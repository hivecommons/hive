package nousstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Promotions (RFC #11585 part 6, #11699).
//
// A campaign's verdict (deploy / deploy with caveats / fall back) is data an
// owner reads; nothing acts on it by itself. When an owner chooses to
// promote it — into an issue on the target project, or into a Spektacular
// run — the hive records that choice here, append-only, so the audit trail
// survives a restart and a repeated Promote of the same target is answered
// with the existing result instead of a second issue or run:
//
//	<root>/promotions/history.jsonl   every promotion ever recorded
//
// Nothing here opens an issue or admits a run. pkg/dashboard performs the
// target's action and then records the outcome through RecordPromotion.
const (
	// PromotionsSubdir holds the promotion record.
	PromotionsSubdir = "promotions"

	// PromotionHistoryFile is the append-only record of every promotion.
	PromotionHistoryFile = "history.jsonl"
)

// Targets an owner may promote a verdict to. Anything else is rejected.
const (
	// PromoteTargetIssue opens an issue in the target project.
	PromoteTargetIssue = "issue"
	// PromoteTargetSpektacularRun opens the same issue and admits a
	// Spektacular spec run against it.
	PromoteTargetSpektacularRun = "spektacular-run"
)

// ErrPromotionMalformed is returned when a promotion cannot be recorded
// because it lacks the fields the audit trail needs.
var ErrPromotionMalformed = errors.New("nous promotion malformed")

// ValidPromoteTarget reports whether s is a known promotion target.
func ValidPromoteTarget(s string) bool {
	switch s {
	case PromoteTargetIssue, PromoteTargetSpektacularRun:
		return true
	}
	return false
}

// PromoteTargets lists the known targets in display order.
func PromoteTargets() []string {
	return []string{PromoteTargetIssue, PromoteTargetSpektacularRun}
}

// Promotion is one owner decision to promote a campaign verdict.
type Promotion struct {
	// Campaign names the campaign whose verdict was promoted.
	Campaign string `json:"campaign"`
	// Target is the PromoteTarget* the owner picked.
	Target string `json:"target"`
	// Recommendation is the verdict at the time of promotion.
	Recommendation string `json:"recommendation,omitempty"`
	// PromotedBy is the owner who promoted; PromotedAt when.
	PromotedBy string    `json:"promoted_by"`
	PromotedAt time.Time `json:"promoted_at"`
	// Repo and Number identify the issue the promotion opened (or reused);
	// URL is its web address.
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
	// RunKey is the Spektacular run admitted, when Target is a run.
	RunKey string `json:"run_key,omitempty"`
}

// PromotionsDir is <root>/promotions.
func (l Layout) PromotionsDir() string { return filepath.Join(l.Root, PromotionsSubdir) }

// PromotionHistoryPath is <root>/promotions/history.jsonl.
func (l Layout) PromotionHistoryPath() string {
	return filepath.Join(l.PromotionsDir(), PromotionHistoryFile)
}

// RecordPromotion appends p to the promotion history. The target must be
// valid and the campaign and promoter named; PromotedAt is stamped with now
// when zero.
func (l Layout) RecordPromotion(p Promotion, now time.Time) error {
	if !ValidPromoteTarget(p.Target) {
		return fmt.Errorf("%w: invalid target %q", ErrPromotionMalformed, p.Target)
	}
	if strings.TrimSpace(p.Campaign) == "" {
		return fmt.Errorf("%w: campaign is required", ErrPromotionMalformed)
	}
	if strings.TrimSpace(p.PromotedBy) == "" {
		return fmt.Errorf("%w: promoted_by is required", ErrPromotionMalformed)
	}
	if p.PromotedAt.IsZero() {
		p.PromotedAt = now
	}
	p.PromotedAt = p.PromotedAt.UTC()
	line, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal nous promotion: %w", err)
	}
	if err := os.MkdirAll(l.PromotionsDir(), dirMode); err != nil {
		return fmt.Errorf("create nous promotions dir %s: %w", l.PromotionsDir(), err)
	}
	return appendLine(l.PromotionHistoryPath(), "nous promotion history", line)
}

// Promotions returns every recorded promotion, oldest first. Unparseable
// lines are skipped so one bad write cannot hide the rest of the record.
func (l Layout) Promotions() ([]Promotion, error) {
	data, err := os.ReadFile(l.PromotionHistoryPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read nous promotion history: %w", err)
	}
	var out []Promotion
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var p Promotion
		if json.Unmarshal([]byte(line), &p) == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// FindPromotion returns the most recent promotion of campaign to target,
// if any.
func (l Layout) FindPromotion(campaign, target string) (Promotion, bool, error) {
	all, err := l.Promotions()
	if err != nil {
		return Promotion{}, false, err
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Campaign == campaign && all[i].Target == target {
			return all[i], true, nil
		}
	}
	return Promotion{}, false, nil
}
