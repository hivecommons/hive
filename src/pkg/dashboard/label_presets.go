package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// FrontendLabelPreset is one built-in preset of the Projects cards' faceted
// label filter (hivecommons/hive#12172). Labels is in display priority
// order: the filter sorts matching rows by the index of the first label they
// carry, so for the Needs-human preset an operator decision
// (stage/045-needs-decision) lands above a plain needs-human park.
type FrontendLabelPreset struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Labels match case-insensitively against the item's GitHub labels (any
	// of them, unless the user switches the preset to match-all).
	Labels []string `json:"labels,omitempty"`
	// States are derived facets the page computes from the snapshot
	// ("covered-by-pr", "parked", "claimed", "unclaimed").
	States []string `json:"states,omitempty"`
}

const (
	labelPresetNeedsHumanKey  = "needs-human"
	labelPresetHelpWantedKey  = "help-wanted"
	labelPresetCoveredByPRKey = "covered-by-pr"
	labelFacetStateCoveredPR  = "covered-by-pr"
)

// needsHumanPresetLabels is the "waiting on a person" label set in priority
// order, built from the SAME sources the enumeration parks on so the preset
// cannot drift from them (ADR-0019; hivecommons/hive#12002):
//
//   - the four project.issue_filter.hard_suppress_labels buckets
//     (config.HardSuppressLabelsConfig), honouring operator overrides;
//   - github.NeedsSignalLabel, the escalation label that is not a hard
//     suppress but still asks a person for a better signal;
//   - github.HoldLabels plus this hive's canonical hold label;
//   - stage/071-needs-author, the PR stage where a human author must push
//     or answer the hive's diagnosis;
//   - the sentinel alert label (sentinel.label, default sentinel-alert): the
//     sweep refuses to merge a flagged PR until a maintainer reviews it and
//     removes the label (hivecommons/hive#13780);
//   - the reporter-trust awaiting label (default needs-triage): the issue
//     waits for a maintainer to triage it before the hive may pick it up.
//
// The Hive activity monitor's synthetic "Needs human" facet reads the same
// list (StageTrackerResponse.NeedsHumanLabels); a label matches when it
// equals an entry or sits under it as `entry/...` (needs-human/agent-ready),
// or matches the needs-human*, *needs-decision or *needs-author patterns
// (stageTrackerLabelNeedsHuman).
//
// "blocked" is deliberately absent: the codebase treats it as a contributor
// skip marker and a PR mergeability state, not as a parked-on-a-person label.
func needsHumanPresetLabels(cfg *config.Config, hiveID string) []string {
	var hs config.HardSuppressLabelsConfig
	if cfg != nil {
		hs = cfg.Project.IssueFilter.HardSuppressLabels
	}
	var out []string
	seen := map[string]bool{}
	add := func(labels ...string) {
		for _, label := range labels {
			label = strings.TrimSpace(label)
			if label == "" || seen[strings.ToLower(label)] {
				continue
			}
			seen[strings.ToLower(label)] = true
			out = append(out, label)
		}
	}
	add(hs.EffectiveNeedsDecision()...)
	add(github.PRNeedsDecisionStageLabel)
	add(config.LegacyNeedsDecisionLabels...)
	add(hs.EffectiveNeedsDirection()...)
	add(hs.EffectiveNeedsSpec()...)
	add(github.NeedsSignalLabel)
	add(hs.EffectiveNeedsHuman()...)
	add(github.HoldLabels...)
	add(github.CanonicalHiveHoldLabel(hiveID))
	add(stagelabels.NeedsAuthor.Label())
	if cfg != nil {
		add(cfg.Sentinel.LabelOrDefault())
		add(cfg.Project.IssueFilter.ReporterTrustAwaitingLabel())
	} else {
		add(config.DefaultSentinelLabel)
		add(config.DefaultReporterTrustAwaitingLabel)
	}
	return out
}

// labelPresets returns the built-in presets in the order the page lists them.
func labelPresets(cfg *config.Config, hiveID string) []FrontendLabelPreset {
	return []FrontendLabelPreset{
		{
			Key:         labelPresetNeedsHumanKey,
			Label:       "Needs human",
			Description: "Open issues and PRs parked on an escalation, hard-suppress, hold, needs-author or needs-triage label — waiting on a person, not on an agent",
			Labels:      needsHumanPresetLabels(cfg, hiveID),
		},
		{
			Key:         labelPresetHelpWantedKey,
			Label:       "Help wanted",
			Description: "Items the maintainers have opened up to contributors",
			Labels:      []string{"help wanted", "good first issue"},
		},
		{
			Key:         labelPresetCoveredByPRKey,
			Label:       "Covered by PR",
			Description: "Issues hive has verified an open or merged pull request covers",
			States:      []string{labelFacetStateCoveredPR},
		},
	}
}
