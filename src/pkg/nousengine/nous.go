package nousengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// Nous install and file-format constants. The Dockerfile installs the runner
// scripts at DefaultBinDir and the example campaigns at DefaultCampaignDir.
const (
	// DefaultBinDir is where the Dockerfile copies bin/nous-runner.sh.
	DefaultBinDir = "/tmp/hive/bin"
	// DefaultCampaignDir is where the Dockerfile copies the example campaigns.
	DefaultCampaignDir = "/etc/hive"
	// RunnerScript is the Nous entry point the experimenter agent runs.
	RunnerScript = "nous-runner.sh"
	// DefaultCampaignFile is the campaign definition RunnerScript reads when
	// none is named.
	DefaultCampaignFile = "nous-campaign.yaml"

	// MetaFindingsFile is Nous's end-of-campaign analysis; its
	// deployment_recommendation is the Verdict.
	MetaFindingsFile = "meta_findings.json"
	// RunsSubdir holds Nous's per-run artifacts (report.md, patches/, …).
	RunsSubdir = "runs"

	// Environment the runner reads. NOUS_AGENT is Nous's own name for the
	// dispatcher (run_campaign.py --agent); the others are the hive's.
	runnerDispatcherEnv = "NOUS_AGENT"
	runnerDataDirEnv    = "NOUS_DATA_DIR"
	runnerCampaignEnv   = "NOUS_CAMPAIGN_PATH"
	runnerRunIDEnv      = "NOUS_RUN_ID"
	runnerModeEnv       = "NOUS_HIVE_MODE"
	runnerScopeEnv      = "NOUS_HIVE_SCOPE"
)

// Nous confidence labels as written by principles.json, mapped onto the
// contract's 0–1 scale. The numbers match cmd/hive's confidenceToFloat so
// the dashboard and the engine never disagree about the same file.
const (
	confidenceHigh    = 0.9
	confidenceMedium  = 0.7
	confidenceLow     = 0.4
	confidenceUnknown = 0.5
)

// nousVerdictLabels maps Nous's deployment_recommendation vocabulary onto the
// contract's. Nous already uses these spellings; the map exists so a future
// rename on the Nous side is a one-line change here.
var nousVerdictLabels = map[string]string{
	"deploy":                VerdictDeploy,
	"deploy_with_caveats":   VerdictWithCaveats,
	"fall_back_to_baseline": VerdictFallBack,
	"fallback":              VerdictFallBack,
}

func init() {
	Register(config.ExperimentEngineNous, func(s Settings) (Engine, error) { return NewNous(s) })
}

// Nous is the default engine: the Nous framework run through bin/nous-runner.sh
// with the inline dispatcher unless the hive resolved sdk.
type Nous struct {
	layout         nousstore.Layout
	dispatcher     string
	binDir         string
	campaignDir    string
	fetchAllowlist []string
}

// NewNous builds the Nous engine. An empty Dispatcher means the default; an
// unknown one is an error — Settings carry the RESOLVED value, so a typo
// here is a programming error rather than operator input.
func NewNous(s Settings) (*Nous, error) {
	d := strings.ToLower(strings.TrimSpace(s.Dispatcher))
	if d == "" {
		d = config.DefaultNousDispatcher
	}
	if !config.ValidNousDispatchers[d] {
		return nil, fmt.Errorf("nousengine: invalid dispatcher %q", s.Dispatcher)
	}
	root := strings.TrimSpace(s.Root)
	if root == "" {
		root = nousstore.DefaultRoot
	}
	bin := s.BinDir
	if bin == "" {
		bin = DefaultBinDir
	}
	cd := s.CampaignDir
	if cd == "" {
		cd = DefaultCampaignDir
	}
	return &Nous{layout: nousstore.New(root), dispatcher: d, binDir: bin, campaignDir: cd, fetchAllowlist: s.FetchAllowlist}, nil
}

// Name implements Engine.
func (n *Nous) Name() string { return config.ExperimentEngineNous }

// Dispatcher implements Engine.
func (n *Nous) Dispatcher() string { return n.dispatcher }

// Invocation implements Engine: `bash <bin>/nous-runner.sh` with the
// dispatcher, data root and campaign definition in the environment. The
// runner honours a NOUS_AGENT already in the agent's environment first, so
// an operator who exported it before this part keeps that behaviour.
func (n *Nous) Invocation(c Campaign) (Invocation, error) {
	def := c.DefinitionPath
	if def == "" {
		def = filepath.Join(n.campaignDir, DefaultCampaignFile)
	}
	if !filepath.IsAbs(def) {
		return Invocation{}, fmt.Errorf("nousengine: campaign definition must be an absolute path, got %q", def)
	}
	env := []string{
		runnerDispatcherEnv + "=" + n.dispatcher,
		config.NousDispatcherEnv + "=" + n.dispatcher,
		config.NousEngineEnv + "=" + n.Name(),
		runnerDataDirEnv + "=" + n.layout.Root,
		nousstore.EnvRoot + "=" + n.layout.Root,
		runnerCampaignEnv + "=" + def,
		config.NousFetchAllowlistEnv + "=" + fetchAllowlistEnvValue(n.fetchAllowlist),
	}
	if c.ID != "" {
		env = append(env, runnerRunIDEnv+"="+c.ID)
	}
	if c.Mode != "" {
		env = append(env, runnerModeEnv+"="+c.Mode)
	}
	if c.Scope != "" {
		env = append(env, runnerScopeEnv+"="+c.Scope)
	}
	return Invocation{
		Argv: []string{"bash", filepath.Join(n.binDir, RunnerScript)},
		Env:  env,
	}, nil
}

// campaignDir is <root>/<id>; the "governor" campaign therefore shares the
// governor directory part 1 made durable, and the gates dir is the single
// protocol dir part 3 defined.
func (n *Nous) campaignRoot(c Campaign) string {
	id := strings.TrimSpace(c.ID)
	if id == "" {
		id = nousstore.GovernorSubdir
	}
	return filepath.Join(n.layout.Root, id)
}

// Layout implements Engine.
func (n *Nous) Layout(c Campaign) Layout {
	dir := n.campaignRoot(c)
	return Layout{
		Root:         dir,
		Ledger:       filepath.Join(dir, nousstore.LedgerFile),
		Principles:   filepath.Join(dir, nousstore.PrinciplesFile),
		Verdict:      filepath.Join(dir, MetaFindingsFile),
		GatesDir:     n.layout.GatesDir(),
		ArtifactsDir: filepath.Join(dir, RunsSubdir),
	}
}

// Progress implements Engine by reading Nous's ledger.json
// ({"iterations":[…]}). A missing file is an empty history.
func (n *Nous) Progress(c Campaign) ([]Round, error) {
	data, err := os.ReadFile(n.Layout(c).Ledger)
	if errors.Is(err, fs.ErrNotExist) {
		return []Round{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ledger struct {
		Iterations []map[string]any `json:"iterations"`
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("nousengine: parse %s: %w", n.Layout(c).Ledger, err)
	}
	rounds := make([]Round, 0, len(ledger.Iterations))
	for i, it := range ledger.Iterations {
		rounds = append(rounds, Round{
			Index:     i,
			Timestamp: str(it, "ts", "timestamp"),
			Type:      str(it, "type"),
			Status:    str(it, "status"),
			Summary:   str(it, "summary", "hypothesis"),
			Raw:       it,
		})
	}
	return rounds, nil
}

// Principles implements Engine by reading Nous's principles.json
// ({"principles":[{id,statement,confidence,category,evidence}]}).
func (n *Nous) Principles(c Campaign) ([]Principle, error) {
	data, err := os.ReadFile(n.Layout(c).Principles)
	if errors.Is(err, fs.ErrNotExist) {
		return []Principle{}, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Principles []map[string]any `json:"principles"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("nousengine: parse %s: %w", n.Layout(c).Principles, err)
	}
	out := make([]Principle, 0, len(file.Principles))
	for _, p := range file.Principles {
		out = append(out, Principle{
			ID:         str(p, "id"),
			Text:       str(p, "statement", "text"),
			Confidence: confidence(p["confidence"]),
			Evidence:   strs(p, "evidence", "citations"),
			Source:     str(p, "category", "source"),
		})
	}
	return out, nil
}

// Verdict implements Engine by reading meta_findings.json's
// deployment_recommendation. Nous writes it either as a bare label or as an
// object; both are accepted. ErrNoVerdict covers a missing file and a file
// with no recommendation yet.
func (n *Nous) Verdict(c Campaign) (Verdict, error) {
	path := n.Layout(c).Verdict
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Verdict{}, ErrNoVerdict
	}
	if err != nil {
		return Verdict{}, err
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return Verdict{}, fmt.Errorf("nousengine: parse %s: %w", path, err)
	}
	raw, ok := meta["deployment_recommendation"]
	if !ok || raw == nil {
		return Verdict{}, ErrNoVerdict
	}
	v := Verdict{}
	switch rec := raw.(type) {
	case string:
		v.Recommendation = mapVerdictLabel(rec)
	case map[string]any:
		v.Recommendation = mapVerdictLabel(str(rec, "recommendation", "decision", "verdict"))
		v.Score = num(rec["score"], rec["composite_score"], rec["best_score"])
		v.Baseline = num(rec["baseline"], rec["baseline_score"])
		v.Citations = strs(rec, "citations")
		v.Caveats = strs(rec, "caveats")
		v.Summary = str(rec, "summary", "rationale")
		if b, isBool := rec["measured_against_baseline"].(bool); isBool {
			v.MeasuredAgainstBaseline = b
		} else {
			// Nous scores against a baseline of 0 when the campaign had no
			// objective (RFC §4), so a non-zero baseline is the evidence.
			v.MeasuredAgainstBaseline = v.Baseline != 0
		}
	default:
		return Verdict{}, fmt.Errorf("nousengine: %s: deployment_recommendation has unexpected type %T", path, raw)
	}
	if v.Recommendation == "" {
		return Verdict{}, ErrNoVerdict
	}
	return v, nil
}

func mapVerdictLabel(label string) string {
	l := strings.ToLower(strings.TrimSpace(label))
	if mapped, ok := nousVerdictLabels[l]; ok {
		return mapped
	}
	return l
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}

func strs(m map[string]any, keys ...string) []string {
	for _, k := range keys {
		list, ok := m[k].([]any)
		if !ok {
			continue
		}
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func num(candidates ...any) float64 {
	for _, c := range candidates {
		switch v := c.(type) {
		case float64:
			return v
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

func confidence(v any) float64 {
	switch c := v.(type) {
	case float64:
		return c
	case string:
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "high":
			return confidenceHigh
		case "medium":
			return confidenceMedium
		case "low":
			return confidenceLow
		}
		if f, err := strconv.ParseFloat(c, 64); err == nil {
			return f
		}
	}
	return confidenceUnknown
}
