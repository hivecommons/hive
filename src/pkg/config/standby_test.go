package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Standby configuration, step S2 of RFC #7629. These tests are the validation
// rules from src/docs/design/standby-contributors.md ("Validation rules")
// applied through the real load path, so what they assert is what an operator
// gets at boot — not what a helper returns in isolation.

// standbyYAML builds a loadable hive.yaml whose quality lane carries the given
// standby block and whose hub carries the given standby keys. Both halves are
// passed as raw YAML fragments so a test can express "absent" as "".
func standbyYAML(laneBlock, hubBlock string) string {
	yaml := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_test
agents:
  quality:
    backend: claude
    enabled: true
`
	if laneBlock != "" {
		yaml += laneBlock
	}
	if hubBlock != "" {
		yaml += "hub:\n" + hubBlock
	}
	return yaml
}

func loadStandby(t *testing.T, laneBlock, hubBlock string) (*Config, error) {
	t.Helper()
	return Load(writeTempConfig(t, standbyYAML(laneBlock, hubBlock)))
}

func mustLoadStandby(t *testing.T, laneBlock, hubBlock string) *Config {
	t.Helper()
	cfg, err := loadStandby(t, laneBlock, hubBlock)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Rule 1 — the floor defaults to T1, the strongest floor
// ---------------------------------------------------------------------------

func TestStandby_FloorDefaultsToT1(t *testing.T) {
	cfg := mustLoadStandby(t, `    standby:
      enabled: false
`, "")
	standby := cfg.Agents["quality"].Standby
	if standby == nil {
		t.Fatal("Agents[quality].Standby = nil, want the parsed block")
	}
	if standby.MinModelCapability != StandbyTierT1 {
		t.Errorf("MinModelCapability = %q, want %q — an omitted floor must default to the STRONGEST tier, not the weakest",
			standby.MinModelCapability, StandbyTierT1)
	}
	if got := standby.StandbyFloor(); got != StandbyTierT1 {
		t.Errorf("StandbyFloor() = %q, want %q", got, StandbyTierT1)
	}
}

// A block that never went through the defaults pass — a Config assembled in
// code, or a lane read straight off a partial overlay — still resolves to T1.
func TestStandby_FloorResolverDefaultsWithoutLoad(t *testing.T) {
	var nilBlock *StandbyConfig
	if got := nilBlock.StandbyFloor(); got != StandbyTierT1 {
		t.Errorf("(nil).StandbyFloor() = %q, want %q", got, StandbyTierT1)
	}
	blank := &StandbyConfig{}
	if got := blank.StandbyFloor(); got != StandbyTierT1 {
		t.Errorf("blank.StandbyFloor() = %q, want %q", got, StandbyTierT1)
	}
}

func TestStandby_FloorSpellingIsNormalized(t *testing.T) {
	cfg := mustLoadStandby(t, `    standby:
      min_model_capability: " t2 "
`, "")
	if got := cfg.Agents["quality"].Standby.MinModelCapability; got != StandbyTierT2 {
		t.Errorf("MinModelCapability = %q, want %q", got, StandbyTierT2)
	}
}

// ---------------------------------------------------------------------------
// Rule 2 — the floor is exactly T1/T2/T3, and "unknown" is rejected by name
// ---------------------------------------------------------------------------

// This is the guard the design calls non-negotiable: unknown is the ABSENCE of
// a tier, so a lane floored at unknown is a lane every configuration clears.
// The assertion is on the behaviour (the load fails, and the message names the
// field and the reason), so deleting validateStandbyFloor's unknown branch
// fails this test rather than leaving it green on a coincidence of encoding.
func TestStandby_UnknownFloorIsRejected(t *testing.T) {
	for _, spelling := range []string{"unknown", "UNKNOWN", " Unknown "} {
		t.Run(strings.TrimSpace(spelling), func(t *testing.T) {
			_, err := loadStandby(t, "    standby:\n      min_model_capability: \""+spelling+"\"\n", "")
			if err == nil {
				t.Fatal("Load() = nil error, want a load error: unknown is not a floor")
			}
			msg := err.Error()
			if !strings.Contains(msg, "min_model_capability") {
				t.Errorf("error %q does not name the field", msg)
			}
			if !strings.Contains(msg, "not a floor") {
				t.Errorf("error %q does not say WHY unknown is refused; an operator who typed it needs the reason, not a value list", msg)
			}
		})
	}
}

func TestStandby_UnrecognisedFloorIsRejected(t *testing.T) {
	_, err := loadStandby(t, `    standby:
      min_model_capability: T4
`, "")
	if err == nil {
		t.Fatal("Load() = nil error, want a load error for an unrecognised tier")
	}
	if !strings.Contains(err.Error(), "min_model_capability") {
		t.Errorf("error %q does not name the field", err.Error())
	}
}

func TestIsStandbyTier(t *testing.T) {
	for _, tier := range []string{"T1", "T2", "T3", "t1", " t3 "} {
		if !IsStandbyTier(tier) {
			t.Errorf("IsStandbyTier(%q) = false, want true", tier)
		}
	}
	for _, tier := range []string{"", "unknown", "UNKNOWN", "T0", "T4", "opus"} {
		if IsStandbyTier(tier) {
			t.Errorf("IsStandbyTier(%q) = true, want false", tier)
		}
	}
}

// ---------------------------------------------------------------------------
// Rule 3 — an enabled lane with nobody approved is a load error
// ---------------------------------------------------------------------------

func TestStandby_EnabledWithoutApprovedListIsLoadError(t *testing.T) {
	_, err := loadStandby(t, `    standby:
      enabled: true
`, "")
	if err == nil {
		t.Fatal("Load() = nil error, want a load error: a lane that is on with nobody approved can never offer work")
	}
	msg := err.Error()
	if !strings.Contains(msg, "standby_contributors") {
		t.Errorf("error %q does not name hub.standby_contributors — the operator needs to know which list to fill", msg)
	}
}

func TestStandby_EnabledWithApprovedListLoads(t *testing.T) {
	cfg := mustLoadStandby(t, `    standby:
      enabled: true
      daily_cap_per_contributor: 3
`, "  standby_contributors: [alice, Bob]\n")
	if !cfg.Agents["quality"].Standby.IsStandbyEnabled() {
		t.Error("IsStandbyEnabled() = false, want true")
	}
	if !cfg.Hub.IsStandbyContributorApproved("ALICE") {
		t.Error("IsStandbyContributorApproved(ALICE) = false; GitHub logins compare case-insensitively")
	}
	if !cfg.Hub.IsStandbyContributorApproved("@bob") {
		t.Error("IsStandbyContributorApproved(@bob) = false; a typed @ prefix must not change who is approved")
	}
	if cfg.Hub.IsStandbyContributorApproved("mallory") {
		t.Error("IsStandbyContributorApproved(mallory) = true, want false")
	}
}

// A lane that is OFF needs no approved list: the block is inert, so refusing
// the load would block an operator from writing the floor they intend to use
// before they have anyone to approve.
func TestStandby_DisabledLaneNeedsNoApprovedList(t *testing.T) {
	cfg := mustLoadStandby(t, `    standby:
      enabled: false
      min_model_capability: T2
`, "")
	if cfg.Agents["quality"].Standby.IsStandbyEnabled() {
		t.Error("IsStandbyEnabled() = true, want false")
	}
}

// ---------------------------------------------------------------------------
// Rule 4 — the daily cap defaults to 0, negatives fail, excess is clamped
// ---------------------------------------------------------------------------

func TestStandby_DailyCapDefaultsToZero(t *testing.T) {
	cfg := mustLoadStandby(t, `    standby:
      min_model_capability: T2
`, "")
	standby := cfg.Agents["quality"].Standby
	if standby.DailyCapPerContributor != 0 {
		t.Errorf("DailyCapPerContributor = %d, want 0", standby.DailyCapPerContributor)
	}
	if got := standby.StandbyDailyCap(); got != 0 {
		t.Errorf("StandbyDailyCap() = %d, want 0 — 0 is what makes this block safe to adopt before dispatch exists", got)
	}
}

func TestStandby_NegativeDailyCapIsLoadError(t *testing.T) {
	_, err := loadStandby(t, `    standby:
      daily_cap_per_contributor: -1
`, "")
	if err == nil {
		t.Fatal("Load() = nil error, want a load error for a negative cap")
	}
	if !strings.Contains(err.Error(), "daily_cap_per_contributor") {
		t.Errorf("error %q does not name the field", err.Error())
	}
}

func TestStandby_ExcessDailyCapIsClamped(t *testing.T) {
	cfg := mustLoadStandby(t, `    standby:
      daily_cap_per_contributor: 9999
`, "")
	if got := cfg.Agents["quality"].Standby.DailyCapPerContributor; got != standbyDailyCapMax {
		t.Errorf("DailyCapPerContributor = %d, want %d (clamped)", got, standbyDailyCapMax)
	}
}

// The resolver re-clamps for callers that build an AgentConfig without the
// defaults pass, the same way ContributeCooldownHoursOrDefault does.
func TestStandbyDailyCap_ResolverClampsDefensively(t *testing.T) {
	var nilBlock *StandbyConfig
	if got := nilBlock.StandbyDailyCap(); got != 0 {
		t.Errorf("(nil).StandbyDailyCap() = %d, want 0", got)
	}
	over := &StandbyConfig{DailyCapPerContributor: standbyDailyCapMax + 1}
	if got := over.StandbyDailyCap(); got != standbyDailyCapMax {
		t.Errorf("StandbyDailyCap() = %d, want %d", got, standbyDailyCapMax)
	}
	under := &StandbyConfig{DailyCapPerContributor: -5}
	if got := under.StandbyDailyCap(); got != 0 {
		t.Errorf("StandbyDailyCap() = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Rule 5 — the tier map ships empty, rejects bad tiers and duplicate tuples
// ---------------------------------------------------------------------------

func TestStandby_ModelTiersShipEmpty(t *testing.T) {
	cfg := mustLoadStandby(t, "", "")
	if len(cfg.Hub.StandbyModelTiers) != 0 {
		t.Fatalf("Hub.StandbyModelTiers = %v, want empty — Hive publishes no tier mapping, so an unmapped configuration stays unknown and nothing qualifies out of the box",
			cfg.Hub.StandbyModelTiers)
	}
}

func TestStandby_ModelTiersParseAndNormalize(t *testing.T) {
	cfg := mustLoadStandby(t, "", `  standby_model_tiers:
    - { backend: Claude, model: claude-opus-5, reasoning_effort: HIGH, tier: t1 }
    - { backend: codex, model: gpt-5.6-terra, reasoning_effort: high, tier: T2 }
`)
	if len(cfg.Hub.StandbyModelTiers) != 2 {
		t.Fatalf("len(StandbyModelTiers) = %d, want 2", len(cfg.Hub.StandbyModelTiers))
	}
	first := cfg.Hub.StandbyModelTiers[0]
	if first.Backend != "claude" || first.ReasoningEffort != "high" || first.Tier != StandbyTierT1 {
		t.Errorf("entry[0] = %+v, want backend/effort case-folded and tier %q", first, StandbyTierT1)
	}
}

func TestStandby_ModelTierRejectsUnknownTier(t *testing.T) {
	_, err := loadStandby(t, "", `  standby_model_tiers:
    - { backend: claude, model: claude-opus-5, tier: unknown }
`)
	if err == nil {
		t.Fatal("Load() = nil error, want a load error: mapping a configuration to unknown says nothing")
	}
	if !strings.Contains(err.Error(), "standby_model_tiers") {
		t.Errorf("error %q does not name the field", err.Error())
	}
}

func TestStandby_ModelTierRejectsInvalidTier(t *testing.T) {
	_, err := loadStandby(t, "", `  standby_model_tiers:
    - { backend: claude, model: claude-opus-5, tier: gold }
`)
	if err == nil {
		t.Fatal("Load() = nil error, want a load error for an unrecognised tier")
	}
}

func TestStandby_ModelTierRequiresBackendAndModel(t *testing.T) {
	if _, err := loadStandby(t, "", "  standby_model_tiers:\n    - { model: claude-opus-5, tier: T1 }\n"); err == nil {
		t.Error("Load() = nil error, want a load error when backend is missing")
	}
	// A model-less entry would be a wildcard over every model on a backend —
	// the opposite of matching the whole configuration.
	if _, err := loadStandby(t, "", "  standby_model_tiers:\n    - { backend: claude, tier: T1 }\n"); err == nil {
		t.Error("Load() = nil error, want a load error when model is missing")
	}
}

func TestStandby_DuplicateModelTierTupleIsLoadError(t *testing.T) {
	_, err := loadStandby(t, "", `  standby_model_tiers:
    - { backend: claude, model: claude-opus-5, reasoning_effort: high, tier: T1 }
    - { backend: Claude, model: claude-opus-5, reasoning_effort: High, tier: T3 }
`)
	if err == nil {
		t.Fatal("Load() = nil error, want a load error: one configuration maps to exactly one tier, and last-one-wins would silently pick which intention to honour")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error %q does not say the entries are duplicates", err.Error())
	}
}

// The tuple is the WHOLE configuration: the same model at a different
// reasoning effort is a different key, which is what stops a contributor
// offering one configuration and running another.
func TestStandby_ModelTierTupleIsTheWholeConfiguration(t *testing.T) {
	cfg := mustLoadStandby(t, "", `  standby_model_tiers:
    - { backend: claude, model: claude-opus-5, reasoning_effort: high, tier: T1 }
    - { backend: claude, model: claude-opus-5, reasoning_effort: low, tier: T2 }
`)
	if len(cfg.Hub.StandbyModelTiers) != 2 {
		t.Fatalf("len(StandbyModelTiers) = %d, want 2 — a different reasoning effort is a different configuration", len(cfg.Hub.StandbyModelTiers))
	}
	if a, b := cfg.Hub.StandbyModelTiers[0].TupleKey(), cfg.Hub.StandbyModelTiers[1].TupleKey(); a == b {
		t.Errorf("TupleKey collision: %q == %q", a, b)
	}
}

// ---------------------------------------------------------------------------
// Rule 6 — approved entries are GitHub logins, sanitised and bounded
// ---------------------------------------------------------------------------

func TestStandby_ContributorsAreNormalizedAndDeduped(t *testing.T) {
	cfg := mustLoadStandby(t, "", "  standby_contributors: [\"@Alice\", \" alice \", bob]\n")
	got := cfg.Hub.StandbyContributors
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("StandbyContributors = %v, want [alice bob]", got)
	}
}

func TestStandby_InvalidContributorLoginIsLoadError(t *testing.T) {
	for _, login := range []string{"not a login", "-leading", "trailing-", "dou--ble", "alice@example.com", strings.Repeat("a", githubLoginMaxLen+1)} {
		t.Run(login, func(t *testing.T) {
			_, err := loadStandby(t, "", "  standby_contributors: [\""+login+"\"]\n")
			if err == nil {
				t.Fatalf("Load() = nil error for %q, want a load error — a typo in an approval list is somebody silently not approved", login)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Private repositories default off
// ---------------------------------------------------------------------------

func TestStandby_PrivateReposDefaultOff(t *testing.T) {
	cfg := mustLoadStandby(t, "", "")
	if cfg.Hub.IsStandbyPrivateReposAllowed() {
		t.Error("IsStandbyPrivateReposAllowed() = true on a config that never mentioned it, want false — task context for a private repo is read access in substance")
	}
}

func TestStandby_PrivateReposOptIn(t *testing.T) {
	cfg := mustLoadStandby(t, "", "  standby_allow_private_repos: true\n")
	if !cfg.Hub.IsStandbyPrivateReposAllowed() {
		t.Error("IsStandbyPrivateReposAllowed() = false after an explicit opt-in, want true")
	}
}

// ---------------------------------------------------------------------------
// No behaviour change: a hive with no standby block is untouched
// ---------------------------------------------------------------------------

// The acceptance criterion for S2. A config that never mentions standby must
// survive a load/save round trip with no standby keys introduced — otherwise
// "no behaviour change" is only true until the first config write.
func TestStandby_AbsentBlockSerializesBackOutAbsent(t *testing.T) {
	cfg := mustLoadStandby(t, "", "")
	if cfg.Agents["quality"].Standby != nil {
		t.Fatal("Agents[quality].Standby is non-nil on a config that never wrote one")
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, key := range []string{"standby:", "standby_contributors", "standby_model_tiers", "standby_allow_private_repos", "min_model_capability"} {
		if strings.Contains(string(out), key) {
			t.Errorf("marshalled config contains %q; a hive with no standby block must round-trip unchanged\n%s", key, out)
		}
	}
}

// The defaults pass must not invent a block for a lane that has none: that is
// the difference between "absent" and "present and off", and the round trip
// above depends on it.
func TestStandby_DefaultsDoNotMaterializeABlock(t *testing.T) {
	cfg := &Config{Agents: map[string]AgentConfig{"quality": {}}}
	cfg.applyStandbyDefaults()
	if cfg.Agents["quality"].Standby != nil {
		t.Error("applyStandbyDefaults() created a standby block for a lane that never declared one")
	}
}

// ---------------------------------------------------------------------------
// Error determinism
// ---------------------------------------------------------------------------

// Several bad lanes must report the SAME error on every load. Map iteration
// order would otherwise make the reported error a coin flip, and an operator
// fixing one error at a time needs the next load to move forward.
func TestStandby_ValidationErrorIsDeterministic(t *testing.T) {
	yamlDoc := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_test
agents:
  alpha:
    backend: claude
    enabled: true
    standby:
      min_model_capability: T9
  omega:
    backend: claude
    enabled: true
    standby:
      min_model_capability: T8
`
	path := writeTempConfig(t, yamlDoc)
	first := ""
	for i := 0; i < 12; i++ {
		_, err := Load(path)
		if err == nil {
			t.Fatal("Load() = nil error, want a load error")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("Load() error varies between runs:\n  %s\n  %s", first, err.Error())
		}
	}
	if !strings.Contains(first, "alpha") {
		t.Errorf("error %q does not report the first agent in name order", first)
	}
}

// ---------------------------------------------------------------------------
// S7 — the item-tier list: empty by default, validated tuples, and the
// verifiability rule that defines T3
// ---------------------------------------------------------------------------

func TestStandby_ItemTiersShipEmpty(t *testing.T) {
	cfg := mustLoadStandby(t, "", "")
	if len(cfg.Hub.StandbyItemTiers) != 0 {
		t.Fatalf("Hub.StandbyItemTiers = %v, want empty — the T3 list ships empty, so item-tier matching is not in force and S7 changes nothing until an owner opts in",
			cfg.Hub.StandbyItemTiers)
	}
}

func TestStandby_ItemTiersParseAndNormalize(t *testing.T) {
	cfg := mustLoadStandby(t, "", `  standby_item_tiers:
    - { repo: My-Org/Repo-A, label: Dependencies, tier: t3, signal: "the lockfile diff plus green CI" }
    - { label: kind/security, tier: T1 }
`)
	if len(cfg.Hub.StandbyItemTiers) != 2 {
		t.Fatalf("len(StandbyItemTiers) = %d, want 2", len(cfg.Hub.StandbyItemTiers))
	}
	first := cfg.Hub.StandbyItemTiers[0]
	if first.Repo != "my-org/repo-a" || first.Label != "dependencies" || first.Tier != StandbyTierT3 {
		t.Errorf("entry[0] = %+v, want repo/label case-folded and tier %q", first, StandbyTierT3)
	}
	if first.Signal == "" {
		t.Error("entry[0] lost its signal; a T3 entry is defined by the signal it names")
	}
}

func TestStandby_ItemTierRejectsUnknownTier(t *testing.T) {
	_, err := loadStandby(t, "", `  standby_item_tiers:
    - { label: dependencies, tier: unknown }
`)
	if err == nil {
		t.Fatal("Load() = nil error, want a load error: an item that is not on the list is already unknown")
	}
	if !strings.Contains(err.Error(), "standby_item_tiers") {
		t.Errorf("error %q does not name the field", err.Error())
	}
}

func TestStandby_ItemTierRejectsInvalidTier(t *testing.T) {
	if _, err := loadStandby(t, "", "  standby_item_tiers:\n    - { label: dependencies, tier: cheap }\n"); err == nil {
		t.Error("Load() = nil error, want a load error for an unrecognised tier")
	}
}

func TestStandby_ItemTierRequiresALabel(t *testing.T) {
	// A label-less entry would be a wildcard over every item in scope — the
	// opposite of naming a class of work whose correctness a reviewer can
	// check from a signal.
	if _, err := loadStandby(t, "", "  standby_item_tiers:\n    - { tier: T1 }\n"); err == nil {
		t.Error("Load() = nil error, want a load error when label is missing")
	}
}

func TestStandby_ItemTierRejectsAMalformedRepo(t *testing.T) {
	if _, err := loadStandby(t, "", "  standby_item_tiers:\n    - { repo: \"my org/repo a\", label: dependencies, tier: T1 }\n"); err == nil {
		t.Error("Load() = nil error, want a load error for a repo that is not a repository reference")
	}
}

// The design's answer to the RFC's fourth open question, as a load error: an
// item is T3-eligible only if its correctness is established by an automated
// signal a reviewer can read without reconstructing the change. An owner who
// cannot name that signal has not met the test.
func TestStandby_T3ItemEntryMustNameItsSignal(t *testing.T) {
	_, err := loadStandby(t, "", "  standby_item_tiers:\n    - { label: dependencies, tier: T3 }\n")
	if err == nil {
		t.Fatal("Load() = nil error, want a load error: a T3 entry with no signal is a T3 class whose review cost nobody checked")
	}
	if !strings.Contains(err.Error(), "signal") {
		t.Errorf("error %q does not name the missing signal", err.Error())
	}
	// T1 and T2 entries carry no such requirement: the tier that admits the
	// weakest configurations is the one that has to pay for itself.
	if _, err := loadStandby(t, "", "  standby_item_tiers:\n    - { label: kind/security, tier: T1 }\n"); err != nil {
		t.Errorf("Load() error = %v, want a T1 entry to load without a signal", err)
	}
}

func TestStandby_DuplicateItemTierTupleIsLoadError(t *testing.T) {
	_, err := loadStandby(t, "", `  standby_item_tiers:
    - { repo: my-org/repo-a, label: dependencies, tier: T1 }
    - { repo: My-Org/Repo-A, label: Dependencies, tier: T3, signal: green CI }
`)
	if err == nil {
		t.Fatal("Load() = nil error, want a load error: one class of item maps to exactly one tier, and last-one-wins would silently pick which intention to honour")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error %q does not say the entries are duplicates", err.Error())
	}
}

// The tuple is (repo, label): the same label in two repositories is two
// entries, because the signal that makes a class cheap to review — a link
// checker, a lockfile job — is a property of the repository.
func TestStandby_ItemTierTupleIsRepoAndLabel(t *testing.T) {
	cfg := mustLoadStandby(t, "", `  standby_item_tiers:
    - { repo: my-org/repo-a, label: dependencies, tier: T3, signal: "the lockfile diff plus green CI" }
    - { repo: my-org/repo-b, label: dependencies, tier: T1 }
    - { label: dependencies, tier: T2 }
`)
	if len(cfg.Hub.StandbyItemTiers) != 3 {
		t.Fatalf("len(StandbyItemTiers) = %d, want 3", len(cfg.Hub.StandbyItemTiers))
	}
	keys := map[string]bool{}
	for _, entry := range cfg.Hub.StandbyItemTiers {
		if keys[entry.TupleKey()] {
			t.Errorf("TupleKey collision on %q", entry.TupleKey())
		}
		keys[entry.TupleKey()] = true
	}
}
