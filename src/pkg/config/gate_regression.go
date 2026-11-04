package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/writeback"
	"gopkg.in/yaml.v3"
)

// ConfigGuardConfig is the `config:` block of hive.yaml: knobs about how the
// config itself is loaded, as opposed to what it says.
type ConfigGuardConfig struct {
	// RefuseGateRegression makes a safety-gate regression at boot fatal
	// (hivecommons/hive#16438). Off (the default) the hive logs one WARN per
	// dropped gate and reports them in /api/status config.gate_regressions;
	// on, it exits instead of starting with weaker gates than the previous
	// runtime overlay carried.
	RefuseGateRegression bool `yaml:"refuse_gate_regression,omitempty" json:"refuse_gate_regression,omitempty"`
}

// Safety gate names as reported in logs, /api/status and the dashboard.
const (
	GateReviewRequireLabels = "review.require_labels"
	GateIssueRequireLabels  = "project.issue_filter.require_labels"
	GatePausedRepos         = "project.paused_repos"
	GateWritebackPerRepo    = "writeback.per_repo"
)

// GateRegression is one safety gate that a previous runtime overlay carried
// and the effective config lacks or weakens.
type GateRegression struct {
	// Gate is the config key, with the repo appended for per-repo gates
	// (project.paused_repos[org/repo], writeback.per_repo[org/repo].mode).
	Gate string `json:"gate"`
	// Previous is the value the prior overlay had, rendered for a log line.
	Previous string `json:"previous"`
	// Effective is the value the running config has.
	Effective string `json:"effective"`
	// Source is the prior overlay file the previous value was read from.
	Source string `json:"source,omitempty"`
}

// String renders the regression the way the boot WARN line reports it.
func (g GateRegression) String() string {
	s := fmt.Sprintf("%s (was %s) — effective config has %s", g.Gate, g.Previous, g.Effective)
	if g.Source != "" {
		s += " [from " + g.Source + "]"
	}
	return s
}

// GateRegressionError is returned by RefuseGateRegressions when
// config.refuse_gate_regression is on and at least one gate regressed.
type GateRegressionError struct {
	Regressions []GateRegression
}

func (e *GateRegressionError) Error() string {
	parts := make([]string, 0, len(e.Regressions))
	for _, g := range e.Regressions {
		parts = append(parts, g.String())
	}
	return fmt.Sprintf("refusing to start: config.refuse_gate_regression is on and %d safety gate(s) present in a previous runtime overlay are missing or weaker in the effective config: %s",
		len(e.Regressions), strings.Join(parts, "; "))
}

// PriorOverlayPaths lists the files a previous boot may have left behind
// that carry the gates this hive used to run with, in the order they are
// consulted: the runtime config itself, an operator's `.bak` of it, and the
// dashboard overlay. The legacy /data/hive.yaml.bak is deliberately NOT in
// the list: it is frozen at the rename and goes stale on every later save,
// so comparing against it would warn forever about gates an operator
// removed on purpose.
func PriorOverlayPaths() []string {
	return []string{RuntimeConfigFile, RuntimeConfigFile + ".bak", DashboardOverlayFile}
}

// DetectPriorOverlayGateRegressions reads each prior overlay in paths that
// exists, is non-empty, parses, and is not the file effective was loaded
// from, and reports every safety gate it carries that effective lacks or
// weakens. The first overlay to report a given gate wins; later ones add
// only gates not yet seen. A missing or unparsable overlay contributes
// nothing — this is a boot-time warning, never a reason to fail the load.
func DetectPriorOverlayGateRegressions(effective *Config, paths []string) []GateRegression {
	if effective == nil {
		return nil
	}
	var out []GateRegression
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" || sameConfigPath(p, effective.SourcePath) {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		var prior Config
		if err := yaml.Unmarshal([]byte(expandEnvVars(string(data))), &prior); err != nil {
			continue
		}
		for _, g := range DetectGateRegressions(effective, &prior) {
			if seen[g.Gate] {
				continue
			}
			seen[g.Gate] = true
			g.Source = p
			out = append(out, g)
		}
	}
	return out
}

func sameConfigPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ai, errA := os.Stat(a)
	bi, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ai, bi)
}

// DetectGateRegressions compares the safety gates of prior against
// effective and returns one entry per gate that effective has dropped or
// loosened. Tightenings and gates neither config sets are not reported.
func DetectGateRegressions(effective, prior *Config) []GateRegression {
	if effective == nil || prior == nil {
		return nil
	}
	var out []GateRegression
	if g, ok := labelListRegression(GateReviewRequireLabels, prior.Review.RequireLabels, effective.Review.RequireLabels); ok {
		out = append(out, g)
	}
	if g, ok := labelListRegression(GateIssueRequireLabels, prior.Project.IssueFilter.RequireLabels, effective.Project.IssueFilter.RequireLabels); ok {
		out = append(out, g)
	}
	out = append(out, pausedRepoRegressions(effective, prior)...)
	out = append(out, writebackPerRepoRegressions(effective, prior)...)
	return out
}

// labelListRegression reports a non-empty allow-list that became empty.
func labelListRegression(gate string, prior, effective []string) (GateRegression, bool) {
	if len(nonBlank(prior)) == 0 || len(nonBlank(effective)) != 0 {
		return GateRegression{}, false
	}
	return GateRegression{Gate: gate, Previous: formatList(prior), Effective: "none"}, true
}

// pausedRepoRegressions reports each repo paused in prior that effective no
// longer pauses. Keys are org-qualified and case-folded the same way
// RepoPauseFor matches them, each config qualified with its own org.
func pausedRepoRegressions(effective, prior *Config) []GateRegression {
	if len(prior.Project.PausedRepos) == 0 {
		return nil
	}
	paused := effective.PausedRepoSet()
	var out []GateRegression
	for _, rp := range prior.Project.PausedRepos {
		key := repoPauseKey(prior.Project.Org, rp.Repo)
		if key == "" || paused[key] {
			continue
		}
		out = append(out, GateRegression{
			Gate:      fmt.Sprintf("%s[%s]", GatePausedRepos, key),
			Previous:  "paused",
			Effective: "not paused",
		})
	}
	return out
}

// repoModeStrictness orders writeback.per_repo modes from most to least
// restrictive. RepoModeDefault (no override, the kind matrix applies) sits
// between labels-only and full: dropping an off/issues-only/labels-only
// entry is a loosening, dropping a full entry is a tightening.
func repoModeStrictness(m writeback.RepoMode) int {
	switch m {
	case writeback.RepoModeOff:
		return 0
	case writeback.RepoModeIssuesOnly:
		return 1
	case writeback.RepoModeLabelsOnly:
		return 2
	case writeback.RepoModeFull:
		return 4
	default:
		return 3
	}
}

// writebackPerRepoRegressions reports each restrictive per-repo write-back
// mode in prior that effective resolves to a looser mode (or no override).
func writebackPerRepoRegressions(effective, prior *Config) []GateRegression {
	if len(prior.Writeback.PerRepo) == 0 {
		return nil
	}
	repos := make([]string, 0, len(prior.Writeback.PerRepo))
	for r := range prior.Writeback.PerRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	var out []GateRegression
	for _, repo := range repos {
		was := prior.Writeback.PerRepo[repo].Mode
		if !was.Valid() || repoModeStrictness(was) >= repoModeStrictness(writeback.RepoModeDefault) {
			continue
		}
		now := effective.Writeback.RepoModeFor(repo)
		if repoModeStrictness(now) <= repoModeStrictness(was) {
			continue
		}
		out = append(out, GateRegression{
			Gate:      fmt.Sprintf("%s[%s].mode", GateWritebackPerRepo, strings.TrimSpace(repo)),
			Previous:  string(was),
			Effective: formatRepoMode(now),
		})
	}
	return out
}

func formatRepoMode(m writeback.RepoMode) string {
	if m == writeback.RepoModeDefault {
		return "no override (default matrix)"
	}
	return string(m)
}

func nonBlank(in []string) []string {
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func formatList(in []string) string {
	return "[" + strings.Join(nonBlank(in), ", ") + "]"
}
