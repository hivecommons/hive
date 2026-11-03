// Package nousengine defines the experimentation-engine contract behind the
// experimenter agent role (RFC #11585 part 5, #11698) and ships its two
// implementations: Nous, the default engine, and Fake, a test double.
//
// The contract is deliberately small and engine-neutral. The dashboard, the
// gate protocol (pkg/nousstore), durable storage and the future promotion
// code depend only on this interface, so another engine could be plugged in
// later without changing any of them. The six faces of the contract are the
// rows of the RFC's table:
//
//	Input      a campaign definition                    → Invocation
//	Gates      "waiting for an owner" + approve/reject  → Layout.GatesDir (pkg/nousstore)
//	Progress   a per-round history                      → Progress
//	Knowledge  learned principles                       → Principles
//	Verdict    deploy / caveats / fall back + score     → Verdict
//	Artifacts  everything else, saved as-is             → Layout.ArtifactsDir
//
// Nothing in this package promotes anything. A Verdict is data an owner
// reads; acting on it is a separate, owner-only, audited step.
package nousengine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// Campaign is the engine-neutral input: what an experimenter is asked to run.
type Campaign struct {
	// ID names the campaign run; it becomes the run directory under the
	// engine's root and the Nous --run-id.
	ID string
	// DefinitionPath is the engine's native campaign definition on disk
	// (Nous: a campaign.yaml). Empty lets the engine pick its default.
	DefinitionPath string
	// Question is the research question, for logs and the dashboard.
	Question string
	// TargetRepo is the owner/name the campaign experiments on. The agent
	// is scoped to it by the proxy exactly like every other agent.
	TargetRepo string
	// Mode is the campaign's trust mode (observe | suggest | evolve).
	Mode string
	// Scope is the campaign's subject (governor | repo | both).
	Scope string
}

// Invocation is the command an experimenter agent executes for a campaign
// and the environment it needs. Env entries are KEY=VALUE.
type Invocation struct {
	Argv []string
	Env  []string
}

// Layout maps the contract's artifact faces onto paths under an engine root.
// Every field is an absolute path.
type Layout struct {
	Root       string
	Ledger     string
	Principles string
	Verdict    string
	GatesDir   string
	// ArtifactsDir holds everything the contract does not name (Nous: runs/,
	// report.md, patches/). It is saved as-is.
	ArtifactsDir string
}

// Round is one entry of the per-round history.
type Round struct {
	Index     int            `json:"index"`
	Timestamp string         `json:"ts,omitempty"`
	Type      string         `json:"type,omitempty"`
	Status    string         `json:"status,omitempty"`
	Summary   string         `json:"summary,omitempty"`
	Raw       map[string]any `json:"-"`
}

// Principle is one learned principle with its confidence and evidence.
type Principle struct {
	ID         string   `json:"id,omitempty"`
	Text       string   `json:"text"`
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence,omitempty"`
	Source     string   `json:"source,omitempty"`
}

// Verdict recommendations. These are the contract's vocabulary; an engine
// maps its native labels onto them.
const (
	VerdictDeploy      = "deploy"
	VerdictWithCaveats = "deploy_with_caveats"
	VerdictFallBack    = "fall_back_to_baseline"
)

// Verdict is a campaign's final recommendation. It is never acted on
// automatically: an owner promotes it (or not) in a later, audited step.
type Verdict struct {
	Recommendation string  `json:"recommendation"`
	Score          float64 `json:"score"`
	Baseline       float64 `json:"baseline"`
	// MeasuredAgainstBaseline is false when the campaign had no objective
	// or no baseline measurements, so Score says nothing about whether the
	// change beats what is running today (RFC §4).
	MeasuredAgainstBaseline bool     `json:"measured_against_baseline"`
	Citations               []string `json:"citations,omitempty"`
	Caveats                 []string `json:"caveats,omitempty"`
	Summary                 string   `json:"summary,omitempty"`
}

// ErrNoVerdict is returned by Engine.Verdict while a campaign has not
// produced one yet (still running, aborted before analysis, or crashed).
var ErrNoVerdict = errors.New("nousengine: campaign has no verdict yet")

// Engine is the experimentation-engine contract. Implementations must be
// safe for concurrent use; every method is a pure function of its inputs
// and the files under the root.
type Engine interface {
	// Name identifies the engine (config.ExperimentEngineNous).
	Name() string
	// Dispatcher reports how the engine performs its LLM work
	// (config.NousDispatcherInline or config.NousDispatcherSDK).
	Dispatcher() string
	// Invocation builds the command the experimenter agent runs for c.
	Invocation(c Campaign) (Invocation, error)
	// Layout resolves the contract's artifact paths for a campaign.
	Layout(c Campaign) Layout
	// Progress reads the per-round history. A campaign with no history yet
	// returns an empty slice and no error.
	Progress(c Campaign) ([]Round, error)
	// Principles reads the learned principles. None yet → empty, no error.
	Principles(c Campaign) ([]Principle, error)
	// Verdict reads the final recommendation; ErrNoVerdict until there is one.
	Verdict(c Campaign) (Verdict, error)
}

// Factory builds an engine from the hive's resolved settings.
type Factory func(Settings) (Engine, error)

// Settings is everything an engine needs from the hive to build itself.
type Settings struct {
	// Root is the durable Nous root (pkg/nousstore.Layout.Root).
	Root string
	// Dispatcher is the resolved dispatcher (config.NousConfig.ResolvedDispatcher).
	Dispatcher string
	// BinDir is where bin/nous-runner.sh is installed. Empty → DefaultBinDir.
	BinDir string
	// CampaignDir is where the campaign definitions live. Empty → DefaultCampaignDir.
	CampaignDir string
	// FetchAllowlist is the resolved dependency fetch allowlist
	// (config.NousConfig.ResolvedFetchAllowlist). Nil → the default list.
	FetchAllowlist []string
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds an engine factory under name. Registering a name twice is a
// programming error and panics, like http.Handle.
func Register(name string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("nousengine: engine %q registered twice", name))
	}
	registry[name] = f
}

// Names lists the registered engines, sorted.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ErrUnknownEngine is returned by New for a name nothing registered.
var ErrUnknownEngine = errors.New("nousengine: unknown engine")

// New builds the engine called name. An empty name means the default
// (config.DefaultExperimentEngine), so a hive with no engine configured
// runs Nous.
func New(name string, s Settings) (Engine, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = config.DefaultExperimentEngine
	}
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)", ErrUnknownEngine, name, strings.Join(Names(), ", "))
	}
	return f(s)
}

// HiveEnv is the hive-wide experimentation environment exported to every
// experimenter agent so its helper scripts (bin/nous-runner.sh) see the same
// dispatcher, data root and dependency fetch allowlist the hive resolved:
// config.NousDispatcherEnv, nousstore.EnvRoot and config.NousFetchAllowlistEnv.
// The per-agent engine name (config.NousEngineEnv) is added by the agent
// manager from that agent's own config.
func HiveEnv(s Settings) map[string]string {
	d := strings.ToLower(strings.TrimSpace(s.Dispatcher))
	if !config.ValidNousDispatchers[d] {
		d = config.DefaultNousDispatcher
	}
	root := strings.TrimSpace(s.Root)
	if root == "" {
		root = nousstore.DefaultRoot
	}
	return map[string]string{
		config.NousDispatcherEnv:     d,
		nousstore.EnvRoot:            root,
		config.NousFetchAllowlistEnv: fetchAllowlistEnvValue(s.FetchAllowlist),
	}
}

// fetchAllowlistEnvValue serialises the resolved allowlist for
// config.NousFetchAllowlistEnv. Nil means "not resolved" and gets the default
// list; an explicitly empty list — deny everything — is exported as the
// separator alone, because a blank value would read as unset downstream.
func fetchAllowlistEnvValue(hosts []string) string {
	if hosts == nil {
		hosts = config.DefaultNousFetchAllowlist
	}
	if len(hosts) == 0 {
		return config.NousFetchAllowlistSeparator
	}
	return strings.Join(hosts, config.NousFetchAllowlistSeparator)
}
