package github

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// A PR older than the write-back gate's backfill cutoff keeps the stage
// labels it has: the guard refuses every stage write on it, the move to a
// new stage and the removal of the old one alike. When those labels are the
// hive's own, that freezes a stage the hive no longer believes on GitHub
// (projectbluefin/utah-packages#1148 at stage/100-merge-queue while not
// queued, projectbluefin/utah#1274 at 074 while the hive tracks it at 085).
// The guard is there so the hive does not start labelling items nobody asked
// about; an item already carrying the hive's stage label is not one, so the
// stage labeler states that fact (writeback.Item.HiveAppliedStage) and the
// gate lets the hive keep its own label current. The fact is read from the
// timeline, and only for a PR the guard would hold.

// hiveStageFactTTL bounds how long a timeline answer is reused for the same
// stage label set. A label change re-reads at once; the TTL catches a label
// a person removed and re-added under the same name.
const hiveStageFactTTL = 30 * time.Minute

// hiveStageNow is the clock the memo ages entries by (tests replace it).
var hiveStageNow = time.Now

// hiveStageFact is one memoised answer: the stage labels it was read for,
// whether the hive applied all of them, and when it was read.
type hiveStageFact struct {
	labels  string
	applied bool
	at      time.Time
}

// stageLabelSet is the sorted, comma-joined stage/* labels of labels, ""
// when it carries none.
func stageLabelSet(labels []string) string {
	var stage []string
	for _, l := range labels {
		if l = strings.TrimSpace(l); stagelabels.IsStageLabel(l) {
			stage = append(stage, strings.ToLower(l))
		}
	}
	sort.Strings(stage)
	return strings.Join(stage, ",")
}

// HiveAppliedStageLabels reports whether the item carries at least one
// stage/* label and the newest labeled event of every stage/* label in
// labels was made by one of the hive's own accounts (its App bot, an
// additional hive bot login, or the configured ai_author). A stage label a
// person put on the item, or one with no labeled event in the timeline,
// makes it false: that label is the person's, and the backfill guard keeps
// the hive off it as before. A timeline read that fails reports false and is
// not remembered.
func (c *Client) HiveAppliedStageLabels(ctx context.Context, repo string, number int, labels []string) bool {
	set := stageLabelSet(labels)
	if c == nil || c.client == nil || set == "" || number <= 0 {
		return false
	}
	key := strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
	now := hiveStageNow()
	c.hiveStageMu.Lock()
	if f, ok := c.hiveStage[key]; ok && f.labels == set && now.Sub(f.at) < hiveStageFactTTL {
		c.hiveStageMu.Unlock()
		return f.applied
	}
	c.hiveStageMu.Unlock()

	events, err := c.IssueTimelineSince(ctx, repo, number, time.Time{})
	if err != nil {
		return false
	}
	applied := c.stageLabelsAppliedByHive(strings.Split(set, ","), events)
	c.hiveStageMu.Lock()
	if c.hiveStage == nil {
		c.hiveStage = map[string]hiveStageFact{}
	}
	c.hiveStage[key] = hiveStageFact{labels: set, applied: applied, at: now}
	c.hiveStageMu.Unlock()
	return applied
}

// stageLabelsAppliedByHive reports whether the newest labeled event of each
// label in stage (lower-cased stage/* names) names a hive account. events
// are oldest first, as IssueTimelineSince returns them.
func (c *Client) stageLabelsAppliedByHive(stage []string, events []IssueEvent) bool {
	actor := map[string]string{}
	for _, ev := range events {
		if ev.Event == "labeled" {
			actor[strings.ToLower(strings.TrimSpace(ev.Label))] = ev.Actor
		}
	}
	for _, label := range stage {
		who, ok := actor[label]
		if !ok || !(c.isOwnBotLogin(who) || c.IsHiveAuthorLogin(who)) {
			return false
		}
	}
	return len(stage) > 0
}

// withHiveAppliedStage returns item with HiveAppliedStage stated when the
// write-back gate's backfill guard would hold a stage write on it and its
// stage labels are the hive's. Any other item is returned as is, without a
// timeline read: a hive-authored PR, one with no stage label, and one the
// guard does not cover are decided as before.
func (c *Client) withHiveAppliedStage(ctx context.Context, repo string, item writeback.Item) writeback.Item {
	if item.HiveAuthored || item.HiveAppliedStage || stageLabelSet(item.Labels) == "" {
		return item
	}
	if !c.WritebackGate().BackfillHolds(repo, item) {
		return item
	}
	item.HiveAppliedStage = c.HiveAppliedStageLabels(ctx, repo, item.Number, item.Labels)
	return item
}
