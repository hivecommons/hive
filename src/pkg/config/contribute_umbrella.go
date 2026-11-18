package config

import (
	"strings"

	"github.com/hivecommons/hive/pkg/gfi"
)

// ContributeUmbrella (hub.contribute_umbrella) decides which issues are
// umbrella / tracker / owner-only work that a contributor relay is never
// handed (hivecommons/hive#19078). Everything is optional: an omitted block
// is the default behaviour (enabled, gfi.DefaultUmbrellaLabels,
// gfi.DefaultUmbrellaTitlePrefixes, gfi.DefaultHumanOnlyLabels, a task list
// of two issue references, advertisement withdrawn). The excluded issues stay
// actionable for the owner and the hive and stay closable: this is NOT the
// `hive/tracking` vocabulary, which no hive path may close.
//
// PerRepo overrides the hive-wide value for one full `owner/name`
// repository; a field a repo leaves unset inherits the hive-wide value.
type ContributeUmbrella struct {
	ContributeUmbrellaRepo `yaml:",inline"`
	// PerRepo is keyed by full owner/name, matched case-insensitively.
	PerRepo map[string]ContributeUmbrellaRepo `yaml:"per_repo,omitempty" json:"per_repo,omitempty"`
}

// ContributeUmbrellaRepo is the set of knobs both scopes share.
type ContributeUmbrellaRepo struct {
	// Enabled is a pointer so an omitted key inherits (nil). False turns the
	// classification off for the scope.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Labels replaces the accepted umbrella label set (default umbrella,
	// tracker, epic, meta-tracker).
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// TitlePrefixes replaces the accepted title prefixes (default [Tracking],
	// [Epic], [Umbrella], [Tracker]), compared case-insensitively.
	TitlePrefixes []string `yaml:"title_prefixes,omitempty" json:"title_prefixes,omitempty"`
	// TaskListMin is how many issue- or PR-referencing checklist entries make
	// an umbrella. Zero inherits (default 2).
	TaskListMin int `yaml:"task_list_min,omitempty" json:"task_list_min,omitempty"`
	// HumanOnlyLabels replaces the accepted owner-only label set (default
	// needs-human, human-only; `<label>:*` reasons match too).
	HumanOnlyLabels []string `yaml:"human_only_labels,omitempty" json:"human_only_labels,omitempty"`
	// WithdrawAdvertisement, when false, keeps `help wanted` / `good first
	// issue` on an umbrella instead of withdrawing them on the next sweep.
	// Nil inherits (default true).
	WithdrawAdvertisement *bool `yaml:"withdraw_advertisement,omitempty" json:"withdraw_advertisement,omitempty"`
}

// policy converts one scope to a gfi.UmbrellaPolicy overlay.
func (r ContributeUmbrellaRepo) policy() gfi.UmbrellaPolicy {
	return gfi.UmbrellaPolicy{
		Disabled:        r.Enabled != nil && !*r.Enabled,
		Labels:          r.Labels,
		TitlePrefixes:   r.TitlePrefixes,
		TaskListMin:     r.TaskListMin,
		HumanOnlyLabels: r.HumanOnlyLabels,
		// KeepAdvertisement is resolved by UmbrellaPolicyFor: an explicit
		// false here is an override, not the zero value.
		KeepAdvertisement: r.WithdrawAdvertisement != nil && !*r.WithdrawAdvertisement,
	}
}

// UmbrellaPolicyFor is the umbrella / owner-only policy for repo (full
// owner/name or bare name): the hive-wide block with the repo's per_repo
// override laid over it.
func (h HubConfig) UmbrellaPolicyFor(repo string) gfi.UmbrellaPolicy {
	pol := h.ContributeUmbrella.ContributeUmbrellaRepo.policy()
	over, ok := h.ContributeUmbrella.repoOverride(repo)
	if !ok {
		return pol
	}
	merged := pol.Merge(over.policy())
	if over.Enabled != nil {
		merged.Disabled = !*over.Enabled
	}
	if over.WithdrawAdvertisement != nil {
		merged.KeepAdvertisement = !*over.WithdrawAdvertisement
	}
	return merged
}

// repoOverride finds repo's per_repo entry, case-insensitively.
func (u ContributeUmbrella) repoOverride(repo string) (ContributeUmbrellaRepo, bool) {
	key := strings.ToLower(strings.TrimSpace(repo))
	if key == "" || len(u.PerRepo) == 0 {
		return ContributeUmbrellaRepo{}, false
	}
	if r, ok := u.PerRepo[repo]; ok {
		return r, true
	}
	for configured, r := range u.PerRepo {
		if strings.ToLower(strings.TrimSpace(configured)) == key {
			return r, true
		}
	}
	return ContributeUmbrellaRepo{}, false
}
