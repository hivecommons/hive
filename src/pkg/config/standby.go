package config

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Standby contributors: the configuration surface, step S2 of RFC #7629.
//
// A lane paused for budget may offer its queue to approved standby
// contributors, behind a per-lane model floor. This file is the WHOLE of that
// feature that exists today: the schema, its defaults, and its validation.
// Nothing reads the block to make a decision yet — the pause signal is S1, the
// relay's standby mode is S3, and matching is S4. A hive with no `standby:`
// block and no `hub.standby_*` keys is unaffected, byte for byte.
//
// Design: src/docs/design/standby-contributors.md, "Configuration schema",
// "Validation rules", and "The rule that nothing suggests lowering the floor".

// The capability tiers standby borrows from RFC #6825. They are the same three
// names `governor.rotation.agents` already spells in hive.yaml — standby uses
// the vocabulary, not that mapping, because that one keys on agent names and
// this one keys on model configurations.
const (
	StandbyTierT1 = "T1"
	StandbyTierT2 = "T2"
	StandbyTierT3 = "T3"
	// StandbyTierUnknown is the ABSENCE of a tier, not a weak one. A
	// configuration Hive cannot map resolves to it, and it never qualifies
	// against any floor. It is deliberately NOT accepted as a floor value:
	// see validateStandbyFloor.
	StandbyTierUnknown = "unknown"
)

// StandbyDefaultFloor is what `min_model_capability` means when it is absent.
//
// The default is the STRONGEST floor on purpose. A hive that adopts the block
// without thinking about the floor gets the safe answer, and lowering it is a
// deliberate edit rather than a value that drifts down by omission.
const StandbyDefaultFloor = StandbyTierT1

const (
	// standbyDailyCapMax clamps `daily_cap_per_contributor` so a stray value
	// cannot turn a per-contributor cap into no cap at all. Follows the
	// ContributeCooldownHours precedent: clamp in the defaults pass, warn, and
	// keep loading. A NEGATIVE value is not clamped — it is a load error, so a
	// sign typo is reported rather than silently read as "cap of zero".
	standbyDailyCapMax = 50
	// standbyContributorsMax and standbyModelTiersMax bound the two hub-side
	// lists. They are approval and mapping data an operator writes by hand; a
	// list past these sizes is a generated-file accident, not a hive.
	standbyContributorsMax = 500
	standbyModelTiersMax   = 200
	standbyItemTiersMax    = 200
	// githubLoginMaxLen is GitHub's own limit on a login.
	githubLoginMaxLen = 39
)

// StandbyConfig is the per-lane `standby:` block on an agent.
//
// It is a POINTER on AgentConfig so that "no standby block" stays
// distinguishable from "a standby block that is off". The distinction is what
// keeps an existing hive byte-for-byte unchanged through a load/save cycle:
// absent stays absent, and no default is materialised into a config that never
// asked for one.
type StandbyConfig struct {
	// Enabled offers this lane's queue to approved standby contributors when
	// it is paused for budget. Off is the zero value.
	Enabled bool `yaml:"enabled" json:"enabled,omitempty"`
	// AutoDispatch lets Hive hand this lane's paused queue to qualified standby
	// contributors without an owner pressing the manual dispatch control.
	// Default off: S8 is opt-in per lane and preserves S5's hold-gated path.
	AutoDispatch bool `yaml:"auto_dispatch,omitempty" json:"auto_dispatch,omitempty"`
	// MinModelCapability is the lane's FLOOR: the weakest capability tier a
	// donated configuration may have and still be offered this lane's work.
	// Absent means StandbyDefaultFloor (T1).
	//
	// This field is READ-ONLY on every surface. There is no dashboard control,
	// no PUT field and no chat command that writes it, and
	// TestStandbyFloorHasNoWriterOutsideConfig asserts exactly that. The
	// reason is in the design's first section: a rule about what a UI *says*
	// can be undone by a well-meant copy change; an absent endpoint cannot.
	// Lowering a floor is an edit to hive.yaml, on purpose.
	MinModelCapability string `yaml:"min_model_capability,omitempty" json:"min_model_capability,omitempty"`
	// DailyCapPerContributor bounds how many donated tasks one approved
	// contributor may be dispatched on this lane per rolling day.
	//
	// Absent means 0, and 0 means NOTHING IS DISPATCHED. That is what makes
	// this block safe to adopt before the dispatch path exists (S5): a hive
	// can enable standby, see the counts S4 renders, and dispatch nothing.
	DailyCapPerContributor int `yaml:"daily_cap_per_contributor,omitempty" json:"daily_cap_per_contributor,omitempty"`
}

// StandbyModelTier maps ONE contributor model configuration to a capability
// tier. It is the whole configuration that is mapped, never the model alone:
// the same model at a different reasoning effort is a different key, which is
// what makes "nobody can offer Opus and run something else" structural rather
// than a promise.
//
// The list ships EMPTY and Hive publishes no defaults for it. An unmapped
// configuration is unknown and unknown never qualifies, so the out-of-the-box
// outcome on every hive is "0 qualify" until an owner writes the mapping
// deliberately. That is also how this design avoids pre-empting RFC #6825's
// unresolved question of where tier evidence comes from.
type StandbyModelTier struct {
	Backend                string `yaml:"backend" json:"backend"`
	Model                  string `yaml:"model" json:"model"`
	ReasoningEffort        string `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
	AdvisorModel           string `yaml:"advisor_model,omitempty" json:"advisor_model,omitempty"`
	AdvisorReasoningEffort string `yaml:"advisor_reasoning_effort,omitempty" json:"advisor_reasoning_effort,omitempty"`
	Tier                   string `yaml:"tier" json:"tier"`
}

// TupleKey renders the configuration half of a mapping entry as a stable,
// case-folded key. Two entries with the same key are duplicates regardless of
// how their tier is spelled — which is why duplicate detection uses this and
// not the whole struct.
func (t StandbyModelTier) TupleKey() string {
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(t.Backend)),
		strings.ToLower(strings.TrimSpace(t.Model)),
		strings.ToLower(strings.TrimSpace(t.ReasoningEffort)),
		strings.ToLower(strings.TrimSpace(t.AdvisorModel)),
		strings.ToLower(strings.TrimSpace(t.AdvisorReasoningEffort)),
	}, "|")
}

// StandbyItemTier maps ONE class of work item — a label, optionally scoped to
// one repository — to the capability tier a donated configuration must have to
// be offered an item of that class. It is step S7 of RFC #7629, and it is the
// authoritative list: Hive's classifier can propose a tier for an item, and no
// proposal adds an entry here or widens one that is here.
//
// The list ships EMPTY, and an empty list means item-tier matching is NOT IN
// FORCE: the lane floor decides alone and S7 changes nothing until an owner
// opts in. Once the list has an entry, an item that matches none of them is
// `unknown`, and an unknown item is not standby-eligible at all.
//
// A T3 entry must NAME ITS SIGNAL. The design's answer to the RFC's fourth
// open question makes verifiability the whole test for T3 eligibility:
//
//	An item is T3-eligible only if its correctness is established by an
//	automated signal a reviewer can read without reconstructing the change.
//
// Requiring `signal` is that rule with teeth. An owner who cannot name the
// signal has not met the test, and the load fails rather than accepting a T3
// class whose review cost nobody checked.
type StandbyItemTier struct {
	// Repo scopes the entry to one repository ("org/name", or the bare name).
	// Empty applies it to every repository the lane serves. A repository-
	// scoped entry and an unscoped one may both match an item; the STRONGER
	// tier wins, so scoping can only ever narrow eligibility.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
	// Label is the item label this entry is about — matched the way lane
	// routing matches labels: the whole label, or a whole "/"-delimited
	// segment of it, never a substring.
	Label string `yaml:"label" json:"label"`
	// Tier is the capability tier a configuration must have to be offered an
	// item of this class. T1, T2 or T3; "unknown" is rejected.
	Tier string `yaml:"tier" json:"tier"`
	// Signal names the automated evidence that makes an item of this class
	// cheap to review — "the lockfile diff plus green CI", "the link checker".
	// REQUIRED on a T3 entry, and free text: Hive does not evaluate it, the
	// reviewer does.
	Signal string `yaml:"signal,omitempty" json:"signal,omitempty"`
}

// TupleKey renders the (repo, label) half of an item-tier entry as a stable,
// case-folded key. Two entries with the same key are duplicates however their
// tier or signal is spelled, which is why duplicate detection uses this rather
// than the whole struct.
func (t StandbyItemTier) TupleKey() string {
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(t.Repo)),
		strings.ToLower(strings.TrimSpace(t.Label)),
	}, "|")
}

// IsStandbyEnabled resolves the lane's standby switch. A nil block is off.
func (s *StandbyConfig) IsStandbyEnabled() bool {
	return s != nil && s.Enabled
}

// StandbyFloor resolves the lane's effective floor, defaulting an absent or
// blank value to StandbyDefaultFloor.
//
// Always resolve through this rather than reading MinModelCapability directly:
// a nil block, a config built in a test, and a config that never ran through
// the defaults pass all legitimately carry an empty string, and every one of
// them means T1.
func (s *StandbyConfig) StandbyFloor() string {
	if s == nil {
		return StandbyDefaultFloor
	}
	if tier := NormalizeStandbyTier(s.MinModelCapability); tier != "" {
		return tier
	}
	return StandbyDefaultFloor
}

// StandbyDailyCap resolves the lane's per-contributor daily cap, re-clamping
// defensively for callers that build an AgentConfig without running the
// defaults pass. A nil block, and any value at or below zero, is 0 — nothing
// is dispatched.
func (s *StandbyConfig) StandbyDailyCap() int {
	if s == nil || s.DailyCapPerContributor <= 0 {
		return 0
	}
	if s.DailyCapPerContributor > standbyDailyCapMax {
		return standbyDailyCapMax
	}
	return s.DailyCapPerContributor
}

// NormalizeStandbyTier trims and upper-cases a tier name so `t2` in hive.yaml
// resolves to T2. It normalizes spelling ONLY: an unrecognised name (including
// "unknown") is returned upper-cased so the caller can name it in an error.
func NormalizeStandbyTier(tier string) string {
	return strings.ToUpper(strings.TrimSpace(tier))
}

// IsStandbyTier reports whether tier is one of the three legal capability
// tiers. "unknown" is NOT one of them: it is the absence of a tier.
func IsStandbyTier(tier string) bool {
	switch NormalizeStandbyTier(tier) {
	case StandbyTierT1, StandbyTierT2, StandbyTierT3:
		return true
	}
	return false
}

// StandbyContributorSet resolves the approved standby logins as a lookup set,
// case-folded the way GitHub compares logins.
//
// Approval is DURABLE and lives here; volunteering (a relay declaring standby)
// grants nothing. A login in this list grants nothing else either: not a trust
// tier, not a role, not a credential.
func (h HubConfig) StandbyContributorSet() map[string]bool {
	out := make(map[string]bool, len(h.StandbyContributors))
	for _, login := range h.StandbyContributors {
		if login = normalizeGitHubLogin(login); login != "" {
			out[login] = true
		}
	}
	return out
}

// IsStandbyContributorApproved reports whether login is on the hive's approved
// standby list. It makes no tier, cap, suspension or repository decision.
func (h HubConfig) IsStandbyContributorApproved(login string) bool {
	login = normalizeGitHubLogin(login)
	return login != "" && h.StandbyContributorSet()[login]
}

// IsStandbyPrivateReposAllowed resolves the private-repository opt-in.
//
// Default OFF, per the design's answer to the RFC's third open question: a
// standby contributor receives the full task context, which for a private
// repository is read access in substance. Turning it on is the owner saying
// they would give these people read access.
func (h HubConfig) IsStandbyPrivateReposAllowed() bool {
	return h.StandbyAllowPrivateRepos
}

// normalizeGitHubLogin trims a login, drops a leading "@" an operator is
// likely to type, and case-folds it. It does not validate — see
// validGitHubLogin.
func normalizeGitHubLogin(login string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(login), "@"))
}

// validGitHubLogin reports whether login is shaped like a GitHub account name:
// 1..39 characters of ASCII alphanumerics and hyphens, with no leading,
// trailing or doubled hyphen.
func validGitHubLogin(login string) bool {
	if login == "" || len(login) > githubLoginMaxLen {
		return false
	}
	if strings.HasPrefix(login, "-") || strings.HasSuffix(login, "-") || strings.Contains(login, "--") {
		return false
	}
	for _, r := range login {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// applyStandbyDefaults is the standby half of the config defaults pass. It
// normalizes spelling and clamps the daily cap; everything it cannot fix
// safely is left alone for validateStandby to reject, so a typo is reported
// rather than quietly reinterpreted.
//
// It touches a lane ONLY when that lane wrote a `standby:` block. An agent
// without one keeps a nil pointer and serializes back out with no standby key
// at all.
func (c *Config) applyStandbyDefaults() {
	for name, agent := range c.Agents {
		if agent.Standby == nil {
			continue
		}
		standby := *agent.Standby
		if tier := NormalizeStandbyTier(standby.MinModelCapability); tier == "" {
			standby.MinModelCapability = StandbyDefaultFloor
		} else {
			standby.MinModelCapability = tier
		}
		// Clamp DOWN only. A negative cap stays negative so validateStandby
		// can report it: silently reading -1 as "no dispatches" would hide a
		// sign typo behind behaviour that looks deliberate.
		if standby.DailyCapPerContributor > standbyDailyCapMax {
			slog.Warn("standby daily_cap_per_contributor clamped",
				"agent", name,
				"requested", standby.DailyCapPerContributor,
				"clamped_to", standbyDailyCapMax)
			standby.DailyCapPerContributor = standbyDailyCapMax
		}
		agent.Standby = &standby
		c.Agents[name] = agent
	}

	if len(c.Hub.StandbyContributors) > 0 {
		seen := make(map[string]bool, len(c.Hub.StandbyContributors))
		normalized := make([]string, 0, len(c.Hub.StandbyContributors))
		for _, login := range c.Hub.StandbyContributors {
			login = normalizeGitHubLogin(login)
			if login == "" || seen[login] {
				continue
			}
			seen[login] = true
			normalized = append(normalized, login)
		}
		c.Hub.StandbyContributors = normalized
	}

	for i, entry := range c.Hub.StandbyModelTiers {
		entry.Backend = strings.ToLower(strings.TrimSpace(entry.Backend))
		entry.Model = strings.TrimSpace(entry.Model)
		entry.ReasoningEffort = strings.ToLower(strings.TrimSpace(entry.ReasoningEffort))
		entry.AdvisorModel = strings.TrimSpace(entry.AdvisorModel)
		entry.AdvisorReasoningEffort = strings.ToLower(strings.TrimSpace(entry.AdvisorReasoningEffort))
		if tier := NormalizeStandbyTier(entry.Tier); tier != "" {
			entry.Tier = tier
		}
		c.Hub.StandbyModelTiers[i] = entry
	}

	for i, entry := range c.Hub.StandbyItemTiers {
		entry.Repo = strings.ToLower(strings.TrimSpace(entry.Repo))
		entry.Label = strings.ToLower(strings.TrimSpace(entry.Label))
		entry.Signal = strings.TrimSpace(entry.Signal)
		if tier := NormalizeStandbyTier(entry.Tier); tier != "" {
			entry.Tier = tier
		}
		c.Hub.StandbyItemTiers[i] = entry
	}
}

// validateStandby is the standby half of Config.Validate. Every rule here is a
// LOAD ERROR rather than a warning, and the design says why for each: a lane
// that is on with nobody approved, or a floor spelled in a way Hive cannot
// honour, is a misconfiguration the operator should see at boot instead of
// discovering as silence when a lane pauses.
func (c *Config) validateStandby() error {
	if err := c.validateStandbyContributors(); err != nil {
		return err
	}
	if err := c.validateStandbyModelTiers(); err != nil {
		return err
	}
	if err := c.validateStandbyItemTiers(); err != nil {
		return err
	}

	approved := len(c.Hub.StandbyContributors)
	for _, name := range sortedAgentNames(c.Agents) {
		agent := c.Agents[name]
		if agent.Standby == nil {
			continue
		}
		label := agentSourceLabel(name, agent.sourceFile)
		if err := validateStandbyFloor(label, agent.Standby.MinModelCapability); err != nil {
			return err
		}
		if agent.Standby.DailyCapPerContributor < 0 {
			return fmt.Errorf("agent %s: standby.daily_cap_per_contributor must not be negative (got %d; 0 means nothing is dispatched)",
				label, agent.Standby.DailyCapPerContributor)
		}
		// A lane that is on with nobody approved can never offer work to
		// anyone. Failing the load is the point: the alternative is a setting
		// that looks active on the tile and is inert forever.
		if agent.Standby.Enabled && approved == 0 {
			return fmt.Errorf("agent %s: standby.enabled is true but hub.standby_contributors is empty — approve at least one contributor (volunteering is not approval)", label)
		}
	}
	return nil
}

// validateStandbyFloor rejects anything that is not one of the three tiers,
// and rejects "unknown" with its own message.
//
// The separate message is deliberate and is what the test for this guard
// asserts. `unknown` is not a weak floor, it is the absence of one; accepting
// it would create a lane that every configuration clears, which is the exact
// failure the whole floor exists to prevent. An operator who typed it needs to
// be told that, not handed a list of valid values.
func validateStandbyFloor(label, floor string) error {
	tier := NormalizeStandbyTier(floor)
	// "" is legal here: the defaults pass resolves it to T1, and a config
	// assembled in code may never have run that pass.
	if tier == "" || IsStandbyTier(tier) {
		return nil
	}
	if strings.EqualFold(tier, StandbyTierUnknown) {
		return fmt.Errorf("agent %s: standby.min_model_capability must not be %q — unknown is the absence of a capability tier, not a floor, and a lane floored at unknown is a lane anything clears (use %s, %s or %s)",
			label, floor, StandbyTierT1, StandbyTierT2, StandbyTierT3)
	}
	return fmt.Errorf("agent %s: invalid standby.min_model_capability %q (must be %s, %s or %s; omit for %s)",
		label, floor, StandbyTierT1, StandbyTierT2, StandbyTierT3, StandbyDefaultFloor)
}

func (c *Config) validateStandbyContributors() error {
	if len(c.Hub.StandbyContributors) > standbyContributorsMax {
		return fmt.Errorf("hub.standby_contributors has %d entries (maximum %d)", len(c.Hub.StandbyContributors), standbyContributorsMax)
	}
	for _, login := range c.Hub.StandbyContributors {
		if !validGitHubLogin(normalizeGitHubLogin(login)) {
			return fmt.Errorf("hub.standby_contributors: %q is not a GitHub login (letters, digits and single inner hyphens, up to %d characters)", login, githubLoginMaxLen)
		}
	}
	return nil
}

func (c *Config) validateStandbyModelTiers() error {
	if len(c.Hub.StandbyModelTiers) > standbyModelTiersMax {
		return fmt.Errorf("hub.standby_model_tiers has %d entries (maximum %d)", len(c.Hub.StandbyModelTiers), standbyModelTiersMax)
	}
	seen := make(map[string]int, len(c.Hub.StandbyModelTiers))
	for i, entry := range c.Hub.StandbyModelTiers {
		if strings.TrimSpace(entry.Backend) == "" {
			return fmt.Errorf("hub.standby_model_tiers[%d]: backend is required", i)
		}
		// An entry with no model would be a wildcard over every model that
		// backend can run — the opposite of "the whole configuration is
		// matched", and a quiet way to tier a model nobody assessed.
		if strings.TrimSpace(entry.Model) == "" {
			return fmt.Errorf("hub.standby_model_tiers[%d] (backend %s): model is required — an entry maps ONE configuration, never every model on a backend", i, entry.Backend)
		}
		if !IsStandbyTier(entry.Tier) {
			if strings.EqualFold(strings.TrimSpace(entry.Tier), StandbyTierUnknown) {
				return fmt.Errorf("hub.standby_model_tiers[%d] (%s/%s): tier must not be %q — an unmapped configuration is already unknown, so mapping one to unknown says nothing",
					i, entry.Backend, entry.Model, entry.Tier)
			}
			return fmt.Errorf("hub.standby_model_tiers[%d] (%s/%s): invalid tier %q (must be %s, %s or %s)",
				i, entry.Backend, entry.Model, entry.Tier, StandbyTierT1, StandbyTierT2, StandbyTierT3)
		}
		// Duplicates are an error rather than last-one-wins: two rows
		// disagreeing about a configuration's tier is an operator who lost
		// track of their own mapping, and picking one silently picks which of
		// two intentions to honour.
		key := entry.TupleKey()
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("hub.standby_model_tiers[%d] duplicates entry [%d] (%s/%s) — one configuration maps to exactly one tier",
				i, prev, entry.Backend, entry.Model)
		}
		seen[key] = i
	}
	return nil
}

// validateStandbyItemTiers is the item half of the standby validation, and it
// is where the design's answer to the RFC's fourth open question is enforced
// rather than merely documented.
func (c *Config) validateStandbyItemTiers() error {
	if len(c.Hub.StandbyItemTiers) > standbyItemTiersMax {
		return fmt.Errorf("hub.standby_item_tiers has %d entries (maximum %d)", len(c.Hub.StandbyItemTiers), standbyItemTiersMax)
	}
	seen := make(map[string]int, len(c.Hub.StandbyItemTiers))
	for i, entry := range c.Hub.StandbyItemTiers {
		// An entry with no label would be a wildcard over every item in
		// scope — the opposite of naming a class of work whose correctness a
		// reviewer can check from a signal.
		if strings.TrimSpace(entry.Label) == "" {
			return fmt.Errorf("hub.standby_item_tiers[%d]: label is required — an entry names ONE class of item, never every item in the repository", i)
		}
		if !validStandbyItemRepo(entry.Repo) {
			return fmt.Errorf("hub.standby_item_tiers[%d] (label %s): invalid repo %q (use \"org/name\" or a bare repository name; omit for every repository the lane serves)",
				i, entry.Label, entry.Repo)
		}
		if !IsStandbyTier(entry.Tier) {
			if strings.EqualFold(strings.TrimSpace(entry.Tier), StandbyTierUnknown) {
				return fmt.Errorf("hub.standby_item_tiers[%d] (%s): tier must not be %q — an item that is not on this list is already unknown, and an unknown item is not standby-eligible at all",
					i, entry.Label, entry.Tier)
			}
			return fmt.Errorf("hub.standby_item_tiers[%d] (%s): invalid tier %q (must be %s, %s or %s)",
				i, entry.Label, entry.Tier, StandbyTierT1, StandbyTierT2, StandbyTierT3)
		}
		// The verifiability rule, as a load error. T3 is the tier that admits
		// the weakest donated configurations, so it is the one whose review
		// cost has to be paid for by an automated signal.
		if NormalizeStandbyTier(entry.Tier) == StandbyTierT3 && strings.TrimSpace(entry.Signal) == "" {
			return fmt.Errorf("hub.standby_item_tiers[%d] (%s): a %s entry must name its signal — an item is T3-eligible only if its correctness is established by an automated signal a reviewer can read without reconstructing the change (for example signal: \"the lockfile diff plus green CI\")",
				i, entry.Label, StandbyTierT3)
		}
		// Duplicates are an error rather than last-one-wins, for the same
		// reason as the model tiers: two rows disagreeing about one class of
		// item is an owner who lost track of their own list.
		key := entry.TupleKey()
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("hub.standby_item_tiers[%d] duplicates entry [%d] (repo %q, label %s) — one class of item maps to exactly one tier",
				i, prev, entry.Repo, entry.Label)
		}
		seen[key] = i
	}
	return nil
}

// validStandbyItemRepo reports whether repo is shaped like a repository
// reference. Empty is legal and means every repository the lane serves.
func validStandbyItemRepo(repo string) bool {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return true
	}
	parts := strings.Split(repo, "/")
	if len(parts) > 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			default:
				return false
			}
		}
	}
	return true
}

// sortedAgentNames orders the agent map so a config with several bad standby
// blocks reports the same one on every load. Map iteration order would make
// the reported error a coin flip, and an operator fixing one error at a time
// needs the next load to move forward rather than sideways.
func sortedAgentNames(agents map[string]AgentConfig) []string {
	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
