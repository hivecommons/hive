package config

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"
)

// Nous gate protocol tunables (RFC #11585 part 3, #11696).
const (
	// DefaultNousGatePollInterval is how often the hive re-reads
	// <nous_dir>/gates/pending.json for a gate the agent is waiting on.
	// GET /api/nous/gate-pending also re-reads it on demand, so this only
	// bounds how late the owner notification can be.
	DefaultNousGatePollInterval = 15 * time.Second

	// NousGatePollIntervalEnv overrides nous.gate_poll_seconds at runtime.
	// Accepts a Go duration ("30s") or a bare number of seconds.
	NousGatePollIntervalEnv = "HIVE_NOUS_GATE_POLL_INTERVAL"
)

// Experimenter role and engine contract (RFC #11585 part 5, #11698).
//
// The experimenter is the agent role that runs one experiment campaign end
// to end: it takes a campaign definition, runs the rounds, pauses at gates
// for an owner, and writes results and a verdict. It never files issues,
// opens PRs or admits runs on its own — promotion is an owner's decision.
// The role is defined by the engine contract in pkg/nousengine, not by Nous;
// Nous is the default (and, in this part, the only) engine.
const (
	// NousDispatcherInline runs Nous inside the experimenter agent's own CLI
	// session. The agent's CLI does the LLM work, so Hive's scheduling,
	// token budget, scoped tokens, GitHub proxy and cost tracking all apply
	// unchanged. This is the default.
	NousDispatcherInline = "inline"

	// NousDispatcherSDK has Nous call the model API directly. That spend is
	// invisible to Hive's cost tracking; it remains selectable for operators
	// who ran Nous that way before this part.
	NousDispatcherSDK = cruxconfig.NousDispatcherSDK

	// DefaultNousDispatcher is the dispatcher used when nous.dispatcher is
	// absent and NousDispatcherEnv is unset.
	DefaultNousDispatcher = NousDispatcherInline

	// NousDispatcherEnv overrides nous.dispatcher at runtime (env > yaml >
	// default). It is also the name the hive exports the RESOLVED dispatcher
	// under to experimenter agents, so bin/nous-runner.sh reads one answer.
	NousDispatcherEnv = cruxconfig.NousDispatcherEnv

	// NousEngineEnv is the name the hive exports an experimenter agent's
	// resolved engine under.
	NousEngineEnv = cruxconfig.NousEngineEnv
)

// Pinned campaign dependencies (RFC #11585 part 7, #11700).
//
// A campaign may declare dependencies — a target repo pinned by commit, data
// files pinned by sha256 — that `hive nous-setup` (pkg/nousdeps) fetches
// before the engine starts. Fetches are limited to an exact-hostname
// allowlist, modelled on runs.spektacular.recheck.egress_allowlist, and to
// the setup step: once setup finishes the agent needs no new network access.
const (
	// NousFetchAllowlistEnv overrides nous.fetch_allowlist at runtime as a
	// comma-separated list of hostnames (env > yaml > default). It is also
	// the name the hive exports the RESOLVED list under to experimenter
	// agents, so `hive nous-setup` inside the agent pane enforces the same
	// list the operator configured.
	NousFetchAllowlistEnv = "HIVE_NOUS_FETCH_ALLOWLIST"

	// NousFetchAllowlistSeparator joins hosts in NousFetchAllowlistEnv.
	NousFetchAllowlistSeparator = ","
)

// DefaultNousFetchAllowlist is the conservative default: only the host the
// hive's own repos live on. It applies when nous.fetch_allowlist is absent
// (nil) and NousFetchAllowlistEnv is unset. An explicitly empty yaml list
// denies every fetch.
var DefaultNousFetchAllowlist = []string{"github.com"}

// ValidExperimentEngines is the closed set `agents.<name>.engine` may name.
var ValidExperimentEngines = map[string]bool{ExperimentEngineNous: true}

// ValidNousDispatchers is the closed set nous.dispatcher may name.
var ValidNousDispatchers = map[string]bool{NousDispatcherInline: true, NousDispatcherSDK: true}

// NousDispatcherValues lists the dispatchers nous.dispatcher may name, in
// the order the Settings dialog presents them (the default first).
func NousDispatcherValues() []string {
	out := []string{DefaultNousDispatcher}
	for _, v := range strings.Split(sortedKeys(ValidNousDispatchers), ", ") {
		if v != DefaultNousDispatcher {
			out = append(out, v)
		}
	}
	return out
}

// CanonicalNousDispatcher trims and lower-cases v and reports whether the
// result is a recognised nous.dispatcher value. The empty string is accepted
// and means "use the default".
func CanonicalNousDispatcher(v string) (string, bool) {
	v = normalizeNousDispatcher(v)
	if v == "" {
		return "", true
	}
	return v, ValidNousDispatchers[v]
}

// NousConfig is the `nous:` section of hive.yaml.
type NousConfig struct {
	// GatePollSeconds is the pending-gate poll cadence in seconds. Zero or
	// absent applies DefaultNousGatePollInterval.
	GatePollSeconds int `yaml:"gate_poll_seconds,omitempty" json:"gate_poll_seconds,omitempty"`
	// Dispatcher selects how Nous performs its LLM work: inline (default)
	// or sdk. See NousDispatcherInline / NousDispatcherSDK.
	Dispatcher string `yaml:"dispatcher,omitempty" json:"dispatcher,omitempty"`
	// FetchAllowlist is the exact hostnames `hive nous-setup` may fetch
	// campaign dependencies from. Absent (nil) applies
	// DefaultNousFetchAllowlist; an explicitly empty list denies all fetches.
	FetchAllowlist []string `yaml:"fetch_allowlist,omitempty" json:"fetch_allowlist,omitempty"`
	// ArtifactRepo is the optional GitHub repository campaign artifacts are
	// mirrored to (RFC #11585 part 8). The zero value keeps the mirror OFF.
	ArtifactRepo NousArtifactRepoConfig `yaml:"artifact_repo,omitempty" json:"artifact_repo,omitempty"`
	// Observations gates filing hive-state detector findings upstream
	// (#12654). The zero value keeps filing OFF.
	Observations NousObservationsConfig `yaml:"observations,omitempty" json:"observations,omitempty"`
}

// ResolvedFetchAllowlist resolves the dependency fetch allowlist with
// precedence env > yaml > default. Hosts are lower-cased, trimmed and
// de-duplicated; order is preserved.
func (n NousConfig) ResolvedFetchAllowlist() []string {
	return n.ResolveFetchAllowlist(os.Getenv)
}

// ResolveFetchAllowlist is ResolvedFetchAllowlist with an injectable
// environment reader (nil skips the env tier). A set but blank env value is
// treated as unset so `HIVE_NOUS_FETCH_ALLOWLIST=` cannot silently deny
// everything; use an empty yaml list for that.
func (n NousConfig) ResolveFetchAllowlist(getenv func(string) string) []string {
	if getenv != nil {
		if raw := strings.TrimSpace(getenv(NousFetchAllowlistEnv)); raw != "" {
			return normalizeFetchAllowlist(strings.Split(raw, NousFetchAllowlistSeparator))
		}
	}
	if n.FetchAllowlist != nil {
		return normalizeFetchAllowlist(n.FetchAllowlist)
	}
	return normalizeFetchAllowlist(DefaultNousFetchAllowlist)
}

func normalizeFetchAllowlist(hosts []string) []string {
	out := make([]string, 0, len(hosts))
	seen := map[string]bool{}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// ValidateFetchAllowlistHost rejects anything that is not an exact DNS
// hostname: wildcards, schemes, paths, ports, IP literals and loopback names.
// The rule matches runs.spektacular.recheck.egress_allowlist so operators
// learn one shape.
func ValidateFetchAllowlistHost(host string) error {
	h := strings.TrimSpace(host)
	if h == "" || h != strings.ToLower(h) || strings.ContainsAny(h, "/:*@?# ") || net.ParseIP(h) != nil || h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("fetch_allowlist entry %q must be an exact lowercase DNS hostname", host)
	}
	return nil
}

// GatePollInterval resolves the pending-gate poll cadence with precedence
// env > yaml > default. Non-positive or unparseable values fall through to
// the next source so a typo can never stop gate polling.
func (n NousConfig) GatePollInterval() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(NousGatePollIntervalEnv)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	if n.GatePollSeconds > 0 {
		return time.Duration(n.GatePollSeconds) * time.Second
	}
	return DefaultNousGatePollInterval
}

// ResolvedDispatcher resolves the Nous dispatcher with precedence env > yaml
// > default. An unknown value in either source falls through to the next so
// a typo degrades to the inline default rather than to an unrunnable engine;
// Validate reports the yaml typo at load time.
func (n NousConfig) ResolvedDispatcher() string {
	return n.ResolveDispatcher(os.Getenv)
}

// ResolveDispatcher is ResolvedDispatcher with an injectable environment
// reader (nil skips the env tier).
func (n NousConfig) ResolveDispatcher(getenv func(string) string) string {
	if getenv != nil {
		if v := normalizeNousDispatcher(getenv(NousDispatcherEnv)); ValidNousDispatchers[v] {
			return v
		}
	}
	if v := normalizeNousDispatcher(n.Dispatcher); ValidNousDispatchers[v] {
		return v
	}
	return DefaultNousDispatcher
}

func normalizeNousDispatcher(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// Validate rejects a yaml dispatcher outside ValidNousDispatchers and a
// fetch_allowlist entry that is not an exact hostname. Empty is valid and
// means "default".
func (n NousConfig) Validate() error {
	if v := normalizeNousDispatcher(n.Dispatcher); v != "" && !ValidNousDispatchers[v] {
		return fmt.Errorf("nous: invalid dispatcher %q (must be %s, or empty for %s)", n.Dispatcher, sortedKeys(ValidNousDispatchers), DefaultNousDispatcher)
	}
	for _, h := range n.FetchAllowlist {
		if err := ValidateFetchAllowlistHost(h); err != nil {
			return fmt.Errorf("nous: %w", err)
		}
	}
	if err := n.ArtifactRepo.Validate(); err != nil {
		return fmt.Errorf("nous: %w", err)
	}
	if err := n.Observations.Validate(); err != nil {
		return fmt.Errorf("nous: %w", err)
	}
	return nil
}

// ValidateExperimentEngine checks an agent's `engine:` field. Empty is valid
// (the role default applies). A non-empty engine on an agent whose role is
// not experimenter is an error: nothing else reads the field, so it would be
// a silent no-op the operator believes is in force.
func ValidateExperimentEngine(role, engine string) error {
	e := strings.ToLower(strings.TrimSpace(engine))
	if e == "" {
		return nil
	}
	if role != RoleExperimenter {
		return fmt.Errorf("engine %q is only meaningful with role %s (role is %q)", engine, RoleExperimenter, role)
	}
	if !ValidExperimentEngines[e] {
		return fmt.Errorf("invalid engine %q (must be %s)", engine, sortedKeys(ValidExperimentEngines))
	}
	return nil
}

// effectiveRole is the role an agent will carry once defaults are applied:
// the explicit Role, else the agent's name. Validation runs on overlay files
// before defaulting, so it must not mistake a bare `experimenter:` entry for
// a role-less agent.
func effectiveRole(name string, agent AgentConfig) string {
	if agent.Role != "" {
		return agent.Role
	}
	return name
}

func sortedKeys(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
