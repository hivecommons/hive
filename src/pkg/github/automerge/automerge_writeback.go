package automerge

import (
	"context"

	"github.com/hivecommons/hive/pkg/writeback"
)

// writebackGateTransport is the optional transport hook that exposes the
// process's unsolicited write-back gate (hgithub.Client.WritebackGate). A
// transport without it, or with no gate installed, allows every write, as
// the client does before cmd/hive installs one.
type writebackGateTransport interface {
	WritebackGate() *writeback.Gate
}

// writebackGate returns the transport's write-back gate (nil when none).
func (c *Engine) writebackGate() *writeback.Gate {
	if c == nil {
		return nil
	}
	t, ok := c.transport.(writebackGateTransport)
	if !ok {
		return nil
	}
	return t.WritebackGate()
}

// writebackAllowed is the one call an unsolicited write in this package
// makes before touching GitHub (hgithub.Client.WritebackAllowed for the
// engine). A refusal is recorded by the gate for the dashboard drill-down;
// the caller skips the write.
func (c *Engine) writebackAllowed(repo string, item writeback.Item, kind writeback.Kind, preview string) writeback.Decision {
	if c != nil && c.observeOnly {
		return writeback.Decision{Reason: observeOnlyReasonRepoAutoMergeDisabled, Detail: "observe-only pass on a repo whose auto-merge is off"}
	}
	return c.writebackGate().AllowedPreview(repo, item, kind, preview)
}

// writebackItemForObservation is the gate's view of an observed PR the hive
// did not author (adoption only ever targets a contributor's PR).
func writebackItemForObservation(obs stageObservation) writeback.Item {
	return writeback.Item{Number: obs.Number, IsPR: true, Author: obs.Author, Labels: obs.Labels, CreatedAt: obs.OpenedAt}
}

// hiveAppliedStageTransport is the optional transport hook that says whether
// a PR's stage labels are the hive's own (hgithub.Client.HiveAppliedStageLabels).
type hiveAppliedStageTransport interface {
	HiveAppliedStageLabels(ctx context.Context, repo string, number int, labels []string) bool
}

// stageWriteItem is the gate's view of obs for a stage-label write, with
// Item.HiveAppliedStage stated as the stage labeler states it: only for a
// PR the backfill guard would hold, so a note that describes the label move
// answers as the labeler's own write will.
func (c *Engine) stageWriteItem(ctx context.Context, obs stageObservation) writeback.Item {
	item := writebackItemForObservation(obs)
	t, ok := c.transport.(hiveAppliedStageTransport)
	if !ok {
		return item
	}
	repo := obs.Owner + "/" + obs.Repo
	if !c.writebackGate().BackfillHolds(repo, item) {
		return item
	}
	item.HiveAppliedStage = t.HiveAppliedStageLabels(ctx, repo, obs.Number, obs.Labels)
	return item
}
