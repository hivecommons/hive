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
	// Clamped is set when the stage asked for does not apply to the item's
	// kind (a PR stage on an issue, an issue stage on a PR) and was replaced
	// by Stage.For before planning (hivecommons/hive#12451). Wanted is the
	// stage the caller asked for; Stage is what was enforced.
	Clamped bool
	Wanted  Stage
	Stage   Stage
	// Suppressed is set when the write-back gate kept the planned labels
	// off the item (a contributor PR nobody opted in, below the level
	// stage labels need): the stage is tracked internally only.
	Suppressed bool
}

// Changed reports whether any label was written.
func (o Outcome) Changed() bool { return len(o.Added)+len(o.Removed) > 0 }

// Enforce makes the item of kind k carry exactly want. hint is the label
// set the caller's cycle already has (the list payload); when it implies no
// change nothing is read or written, so an unchanged item stays free. When
// it implies a write — including when it already carries two stage labels —
// the item is re-read and the plan recomputed from truth. Removes happen
// before adds so a transient state never shows two stages. A failed remove
// does not stop the others; the first error is returned after the adds.
// want.Stage is clamped to the stages the Table allows on k (Stage.For), so
// a PR-only stage never lands on an issue and vice versa; Outcome.Clamped
// reports when that happened.
func Enforce(ctx context.Context, l Labeler, k Item, owner, repo string, number int, hint []string, want Desired) (Outcome, error) {
	var out Outcome
	if l == nil {
		return out, errors.New("stagelabels: nil labeler")
	}
	want.Stage, out = clampFor(k, want.Stage, out)
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

// EnforceStage sets the pipeline stage on an item of kind k whose other
// hive-owned labels (role, author kind, health) the caller has no opinion
// about: the claim-ledger hooks, which know a claim began but not what the
// lanes think. It always re-reads, keeps the role, author-kind and health
// labels it finds, and leaves exactly stage among `stage/NNN-*` (None clears
// them all). Retired labels (`claimed`, `hive/in-progress`, `🐝 covered by
// PR`, the `author/<tier>` role spellings — re-written as `role/<tier>`)
// come off too.
func EnforceStage(ctx context.Context, l Labeler, k Item, owner, repo string, number int, stage Stage) (Outcome, error) {
	return EnforceStageAdding(ctx, l, k, owner, repo, number, stage)
}

// EnforceStageAdding is EnforceStage that also puts extra (non-stage labels
// the caller owns, such as a claim's `hive/claimed-by-<agent>`) on the item
// in the same add call when they are missing, so a claim is one read and at
// most one add. Stage labels in extra are ignored; stage decides those.
func EnforceStageAdding(ctx context.Context, l Labeler, k Item, owner, repo string, number int, stage Stage, extra ...string) (Outcome, error) {
	var out Outcome
	if l == nil {
		return out, errors.New("stagelabels: nil labeler")
	}
	stage, out = clampFor(k, stage, out)
	fresh, err := l.Labels(ctx, owner, repo, number)
	if err != nil {
		return out, err
	}
	out.Reread = true
	out.Healed = len(StageLabels(fresh)) > 1
	want := Desired{Stage: stage, Role: RoleOf(fresh)}
	for _, lab := range fresh {
		if lab == StalledLabel {
			want.Stalled = true
		}
	}
	add, remove := Plan(fresh, want)
	for _, lab := range extra {
		lab = strings.TrimSpace(lab)
		if lab == "" || IsStageLabel(lab) || Owned(lab) || contains(fresh, lab) || contains(add, lab) {
			continue
		}
		add = append(add, lab)
	}
	return applyPlan(ctx, l, owner, repo, number, add, remove, out)
}

// clampFor records the asked-for stage in out and returns the stage that
// may be written to an item of kind k.
func clampFor(k Item, want Stage, out Outcome) (Stage, Outcome) {
	out.Wanted = want
	out.Stage = want.For(k)
	out.Clamped = out.Stage != want
	return out.Stage, out
}

func contains(labels []string, label string) bool {
	for _, l := range labels {
		if strings.TrimSpace(l) == label {
			return true
		}
	}
	return false
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

// RoleOf returns the admission tier a label set carries, or RoleNone. The
// current `role/<tier>` spelling wins; the retired `author/<tier>` spelling
// is honoured when no current one is present, so a hook that re-writes an
// item mid-migration carries the tier over instead of dropping it.
func RoleOf(labels []string) Role {
	legacy := RoleNone
	for _, l := range labels {
		l = strings.TrimSpace(l)
		for r, d := range roleDefs {
			if l == d.Name {
				return r
			}
			if legacy == RoleNone && l == r.LegacyLabel() {
				legacy = r
			}
		}
	}
	return legacy
}
