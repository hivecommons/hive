package stagelabels

// Enforce is the ONE place a hive-owned label set is written (hivecommons/hive
// #12525; #12512 is the item that showed why). Before it, each writer — the issue reconciler, the PR
// lanes, the claim-ledger hooks — planned from whatever label snapshot it
// happened to hold and wrote the difference, and the claim hooks wrote only
// an add. An issue the reconciler had just staged 010 took a contributor
// claim seconds later; the hook added stage/020 and the issue carried two
// stages until the claim lapsed. Every writer now goes through Enforce,
// which re-reads the item when a write is due so a label another writer
// added since the snapshot is seen, and leaves exactly one `stage/NNN-*`.

import (
	"context"
	"errors"
	"strings"
)

// Labeler is the label I/O Enforce needs on one forge item.
type Labeler interface {
	// Labels returns the item's current labels (a fresh read, not a cache).
	Labels(ctx context.Context, owner, repo string, number int) ([]string, error)
	AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error
	// RemoveLabel treats an absent label (404) as success.
	RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error
}

// Outcome reports what Enforce wrote.
type Outcome struct {
	Added   []string
	Removed []string
	// Healed is set when the fresh read carried more than one stage label:
	// a duplicate another writer left behind that this pass removed.
	Healed bool
	// Reread is set when the item was re-read before writing.
	Reread bool
}

// Changed reports whether any label was written.
func (o Outcome) Changed() bool { return len(o.Added)+len(o.Removed) > 0 }

// Enforce makes the item carry exactly want. hint is the label set the
// caller's cycle already has (the list payload); when it implies no change
// nothing is read or written, so an unchanged item stays free. When it
// implies a write — including when it already carries two stage labels —
// the item is re-read and the plan recomputed from truth. Removes happen
// before adds so a transient state never shows two stages. A failed remove
// does not stop the others; the first error is returned after the adds.
func Enforce(ctx context.Context, l Labeler, owner, repo string, number int, hint []string, want Desired) (Outcome, error) {
	var out Outcome
	if l == nil {
		return out, errors.New("stagelabels: nil labeler")
	}
	add, remove := Plan(hint, want)
	if len(add)+len(remove) == 0 {
		return out, nil
	}
	fresh, err := l.Labels(ctx, owner, repo, number)
	if err != nil {
		return out, err
	}
	out.Reread = true
	out.Healed = len(StageLabels(fresh)) > 1
	add, remove = Plan(fresh, want)
	return applyPlan(ctx, l, owner, repo, number, add, remove, out)
}

// EnforceStage sets the pipeline stage on an item whose other hive-owned
// labels (author tier, health) the caller has no opinion about: the
// claim-ledger hooks, which know a claim began but not what the lanes think.
// It always re-reads, keeps the author and health labels it finds, and
// leaves exactly stage among `stage/NNN-*` (None clears them all). Retired
// labels (`claimed`, `hive/in-progress`, `🐝 covered by PR`) come off too.
func EnforceStage(ctx context.Context, l Labeler, owner, repo string, number int, stage Stage) (Outcome, error) {
	var out Outcome
	if l == nil {
		return out, errors.New("stagelabels: nil labeler")
	}
	fresh, err := l.Labels(ctx, owner, repo, number)
	if err != nil {
		return out, err
	}
	out.Reread = true
	out.Healed = len(StageLabels(fresh)) > 1
	want := Desired{Stage: stage, Author: AuthorOf(fresh)}
	for _, lab := range fresh {
		switch lab {
		case RegressedLabel:
			want.Regressed = true
		case StalledLabel:
			want.Stalled = true
		}
	}
	add, remove := Plan(fresh, want)
	return applyPlan(ctx, l, owner, repo, number, add, remove, out)
}

func applyPlan(ctx context.Context, l Labeler, owner, repo string, number int, add, remove []string, out Outcome) (Outcome, error) {
	var firstErr error
	for _, lab := range remove {
		if err := l.RemoveLabel(ctx, owner, repo, number, lab); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out.Removed = append(out.Removed, lab)
	}
	if len(add) > 0 {
		if err := l.AddLabels(ctx, owner, repo, number, add); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			out.Added = append(out.Added, add...)
		}
	}
	return out, firstErr
}

// IsStageLabel reports whether label is an ordered pipeline stage label
// (`stage/NNN-slug`, known or stale) as opposed to a health marker under the
// same prefix.
func IsStageLabel(label string) bool {
	rest, ok := strings.CutPrefix(label, Prefix)
	if !ok || len(rest) <= NumberWidth || rest[NumberWidth] != '-' {
		return false
	}
	for _, r := range rest[:NumberWidth] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// StageLabels returns every ordered stage label in labels, in input order.
// More than one is an invariant violation Enforce repairs.
func StageLabels(labels []string) []string {
	var out []string
	for _, l := range labels {
		if IsStageLabel(strings.TrimSpace(l)) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// AuthorOf returns the author tier a label set carries, or AuthorNone.
func AuthorOf(labels []string) Author {
	for _, l := range labels {
		for a, d := range authorDefs {
			if l == d.Name {
				return a
			}
		}
	}
	return AuthorNone
}
