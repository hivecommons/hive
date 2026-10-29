package dashboard

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// StageTrackerNewSlug is the pre-stage slug a brand-new open item wears in
// the Open column before any stage pass has placed it: it has no stage/*
// label and no store record, nothing holds or blocks it, and the only
// thing between it and 010/050 is the next pass of whoever moves it
// (hivecommons/hive#15094). Stage stays 0 so the column keys are unchanged.
const StageTrackerNewSlug = "000-new"

// Next movers of a 000-new item.
const (
	// StageTrackerMoverMaintainer: a human must accept triage first (the
	// reporter-trust gate holds an un-admitted reporter's issue).
	StageTrackerMoverMaintainer = "maintainer"
	// StageTrackerMoverHive: the hive's own stage-label pass places it on
	// its next governor cycle.
	StageTrackerMoverHive = "hive"
)

// Gate states on a 000-new item.
const (
	StageTrackerGateDone  = "done"
	StageTrackerGateNext  = "next"
	StageTrackerGateLater = "later"
	// StageTrackerGateHumanMerge names the gate a breaking-titled or
	// needs-owner-merge PR ends on: the owner's merge click.
	StageTrackerGateHumanMerge = "human-merge"
)

// StageTrackerGate is one gate ahead of a 000-new item.
type StageTrackerGate struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Text  string `json:"text"`
}

// StageTrackerNew narrates a 000-new item: who moves it next, when that
// pass is due, and the gates between it and a claim.
type StageTrackerNew struct {
	// Since is when the item was opened (the row's since-time).
	Since *time.Time `json:"since,omitempty"`
	// NextMover is maintainer or hive; NextMoverText the plain words.
	NextMover     string `json:"next_mover"`
	NextMoverText string `json:"next_mover_text"`
	// NextPassAt / NextPassInSeconds: the governor's next cycle, when the
	// hive is the mover and the governor has published one.
	NextPassAt        *time.Time         `json:"next_pass_at,omitempty"`
	NextPassInSeconds int64              `json:"next_pass_in_s,omitempty"`
	Gates             []StageTrackerGate `json:"gates"`
	// Text is the row caption: "New · waiting for <mover> (next pass in Nm)".
	Text string `json:"text"`
}

// stageTrackerIsNew reports whether a finished row is a 000-new item: no
// stage, no record, nothing holding or blocking it, not a draft, and in a
// bucket that means "not placed yet" rather than "placed outside the
// actionable set" (filtered, hive_advisory, claimed, …).
func stageTrackerIsNew(it StageTrackerItem, hasRec bool) bool {
	if it.Stage != 0 || hasRec || it.Hold != nil || len(it.BlockedOn) > 0 || it.BlockedOnPR != nil || it.LinkedPR > 0 || len(it.LinkedPRs) > 0 {
		return false
	}
	for _, m := range it.Modifiers {
		if m != modNeedsTriage {
			return false
		}
	}
	switch it.Bucket {
	case "", stageengineReporterTriageBucket:
		return true
	case "other":
		return it.BucketReason == forgeUnclassifiedReason || it.BucketReason == webhookUnclassifiedReason
	}
	return false
}

// stageengineReporterTriageBucket mirrors github's workBucketReporterTriage.
const stageengineReporterTriageBucket = "reporter_triage"

// stageTrackerNewState builds the 000-new narration for it, or nil when
// the row is not new. filter is the project's issue filter (nil-safe);
// governor carries the next cycle when published.
func stageTrackerNewState(it StageTrackerItem, hasRec bool, filter *config.IssueFilterConfig, governor FrontendGovernor, now time.Time) *StageTrackerNew {
	if !stageTrackerIsNew(it, hasRec) {
		return nil
	}
	n := &StageTrackerNew{Since: it.CreatedAt}
	if n.Since == nil {
		n.Since = it.UpdatedAt
	}
	triageLabels := []string{config.DefaultUntrustedRequireLabels[0]}
	if filter != nil {
		if l := filter.ReporterTrustRequiredLabelsForNotice(); len(l) > 0 {
			triageLabels = l
		}
	}
	// The labels are the newer fact: once a maintainer's triage label is on
	// (the row menu's Accept triage, or applied by hand) the reporter-trust
	// gate admits the issue on its next poll, so the row waits on the hive
	// even while the bucket still says reporter_triage.
	triageAccepted := it.Kind == "issue" && stageTrackerHasAnyLabel(it.Labels, triageLabels)
	triagePending := it.Kind == "issue" && !triageAccepted && (it.Bucket == stageengineReporterTriageBucket || slices.Contains(it.Modifiers, modNeedsTriage))
	if t, ok := parseGovernorNextKick(governor.NextKickAt); ok {
		n.NextPassAt = &t
		if left := t.Sub(now); left > 0 {
			n.NextPassInSeconds = int64(left / time.Second)
		}
	}
	var pass string
	switch {
	case n.NextPassAt != nil && n.NextPassInSeconds > 0:
		pass = "next pass in " + formatETA(time.Duration(n.NextPassInSeconds)*time.Second)
	case n.NextPassAt != nil:
		pass = "next pass due now"
	}
	switch {
	case triagePending:
		n.NextMover = StageTrackerMoverMaintainer
		labels := triageLabels
		n.NextMoverText = "a maintainer's triage (add `" + strings.Join(labels, "` or `") + "`)"
		n.Gates = []StageTrackerGate{
			{Name: "triage", State: StageTrackerGateNext, Text: "a maintainer accepts triage (" + strings.Join(labels, " / ") + ")"},
			{Name: "actionable", State: StageTrackerGateLater, Text: "the stage-label pass marks it 010 actionable"},
			{Name: "claimed", State: StageTrackerGateLater, Text: "the scanner claims it (020)"},
		}
	case it.Kind == "pr":
		n.NextMover = StageTrackerMoverHive
		n.NextMoverText = "the stage-label pass (→ 050 review pending)"
		n.Gates = []StageTrackerGate{
			{Name: "review", State: StageTrackerGateNext, Text: "the stage-label pass places it at 050 review pending"},
			{Name: "merge-gate", State: StageTrackerGateLater, Text: "reviews and checks admit it to the merge queue"},
		}
		// A ⚠️-titled or needs-owner-merge PR ends at 090, not in the
		// merge queue: the owner merges it (hivecommons/hive#15442). The
		// row knows the default rule only; the lanes apply the configured one.
		if ok, _ := (stageengineprstage.HumanMergeRule{}).RequiresHumanMerge(it.Title, it.Labels); ok {
			n.Gates[1] = StageTrackerGate{Name: "merge-gate", State: StageTrackerGateLater, Text: "reviews and checks park it at 090 Ready for human merge"}
			n.Gates = append(n.Gates, StageTrackerGate{Name: StageTrackerGateHumanMerge, State: StageTrackerGateLater, Text: stageengineprstage.CauseHumanMergeRequired})
		}
	default:
		n.NextMover = StageTrackerMoverHive
		n.NextMoverText = "the stage-label pass (→ 010 actionable), then the scanner"
		triageGate := "reporter admitted; no triage label needed"
		if triageAccepted {
			triageGate = "triage accepted (" + strings.Join(stageTrackerLabelsOn(it.Labels, triageLabels), " / ") + ")"
		}
		n.Gates = []StageTrackerGate{
			{Name: "triage", State: StageTrackerGateDone, Text: triageGate},
			{Name: "actionable", State: StageTrackerGateNext, Text: "the stage-label pass marks it 010 actionable"},
			{Name: "claimed", State: StageTrackerGateLater, Text: "the scanner claims it (020)"},
		}
	}
	n.Text = "New · waiting for " + n.NextMoverText
	if triagePending {
		n.Text = stageTrackerTriageNeededCaption
	} else if pass != "" && n.NextMover == StageTrackerMoverHive {
		n.Text += " (" + pass + ")"
	} else if pass != "" {
		n.Text += fmt.Sprintf("; the hive's %s", pass)
	}
	return n
}

// stageTrackerHasAnyLabel reports whether labels carries any of want
// (case-insensitive, trimmed).
func stageTrackerHasAnyLabel(labels, want []string) bool {
	return len(stageTrackerLabelsOn(labels, want)) > 0
}

// stageTrackerLabelsOn returns the members of want that labels carries, in
// want's order.
func stageTrackerLabelsOn(labels, want []string) []string {
	var on []string
	for _, w := range want {
		for _, l := range labels {
			if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(w)) {
				on = append(on, strings.TrimSpace(w))
				break
			}
		}
	}
	return on
}

// parseGovernorNextKick reads FrontendGovernor.NextKickAt (RFC3339).
func parseGovernorNextKick(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// applyStageTrackerNew stamps the 000-new state on a finished row: slug,
// caption, actor and since-time all read from it.
func applyStageTrackerNew(it *StageTrackerItem, n *StageTrackerNew) {
	if n == nil {
		return
	}
	it.New = n
	it.StageSlug = StageTrackerNewSlug
	it.Caption = n.Text
	it.Actor = n.NextMover
	if n.Since != nil && it.StageSince == nil {
		it.StageSince = n.Since
	}
	// A reporter-trust hold is "Triage needed" whether or not the awaiting
	// label has landed yet: the gate decided it, the stage-label pass only
	// writes it down later. The same modifier the `needs-triage` label
	// yields drives the Open chevron word, the pill and Accept triage.
	if n.TriagePending() && !slices.Contains(it.Modifiers, modNeedsTriage) {
		it.Modifiers = append(it.Modifiers, modNeedsTriage)
		slices.SortFunc(it.Modifiers, func(a, b string) int {
			return slices.Index(stageTrackerModifierOrder, a) - slices.Index(stageTrackerModifierOrder, b)
		})
	}
}

// TriagePending reports whether the 000-new row waits on a maintainer's
// triage: the triage gate is the next one.
func (n *StageTrackerNew) TriagePending() bool {
	if n == nil || n.NextMover != StageTrackerMoverMaintainer {
		return false
	}
	for _, g := range n.Gates {
		if g.Name == "triage" && g.State == StageTrackerGateNext {
			return true
		}
	}
	return false
}

// issueFilterConfig is the project's issue filter, or nil without deps.
func (s *Server) issueFilterConfig() *config.IssueFilterConfig {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return nil
	}
	return &s.deps.Config.Project.IssueFilter
}
