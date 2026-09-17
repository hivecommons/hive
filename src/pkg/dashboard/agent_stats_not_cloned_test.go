package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests pin #7411: a freshly created "reviewer" agent — ADVISORY,
// on-demand, owning no repository — rendered ci-maintainer's whole
// CI/coverage strip, headed by "COVERAGE 0% current, goal: 91%".
//
// The strip did not come from POST /api/agents or from defaultStatsConfig
// (which returns nothing for an unknown name). It came from the image:
// src/deploy/data/agents/reviewer/stats.json was a retired seed that was a
// byte-copy of ci-maintainer's set, and the entrypoint copies every seed into
// /data on boot (`cp -rn`). Any spoke that later created an agent called
// "reviewer" inherited the file, and the card rendered a coverage number that
// measured nothing.

// seededStats returns every seeded stats.json under deploy/data/agents by
// agent name, parsed to its ordered key list.
func seededStats(t *testing.T) map[string][]string {
	t.Helper()
	matches, err := filepath.Glob("../../deploy/data/agents/*/stats.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Skip("deploy/data/agents not reachable from this package")
	}
	out := map[string][]string{}
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		var stats []any
		var wrapper struct {
			Stats []any `json:"stats"`
		}
		if json.Unmarshal(raw, &wrapper) == nil && len(wrapper.Stats) > 0 {
			stats = wrapper.Stats
		} else if err := json.Unmarshal(raw, &stats); err != nil {
			t.Fatalf("%s is not a stats file: %v", m, err)
		}
		out[filepath.Base(filepath.Dir(m))] = statKeys(stats)
	}
	return out
}

// TestSeededStatsNeverCloneCIMaintainer is the fix for the bug's origin: no
// seed other than ci-maintainer's may carry ci-maintainer's strip, and the
// retired reviewer seed is gone.
func TestSeededStatsNeverCloneCIMaintainer(t *testing.T) {
	seeds := seededStats(t)
	if _, still := seeds["reviewer"]; still {
		t.Error("deploy/data/agents/reviewer/stats.json is still shipped; every spoke that creates a reviewer agent inherits it (#7411)")
	}
	ci := statKeys(defaultStatsConfig(ciMaintainerStatsAgent))
	for name, keys := range seeds {
		if name == ciMaintainerStatsAgent {
			continue
		}
		if strings.Join(keys, ",") == strings.Join(ci, ",") {
			t.Errorf("seed for %q is ci-maintainer's CI/coverage strip verbatim: %v", name, keys)
		}
		for _, k := range keys {
			if k == "coverage" {
				t.Errorf("seed for %q carries a coverage stat; only the CI owner has a coverage measurement", name)
			}
		}
	}
}

// writeStats writes a stats.json for name under the test data dir.
func writeStats(t *testing.T, name string, stats any) string {
	t.Helper()
	p := agentStatsPath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stats)
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func useTempStatsDir(t *testing.T) {
	t.Helper()
	prev := agentStatsDataDir
	agentStatsDataDir = t.TempDir()
	t.Cleanup(func() { agentStatsDataDir = prev })
}

// TestReaderPrunesClonedCIStrip covers spokes that already have the copy on
// disk (the issue's fix 4): the readers recognise ci-maintainer's strip on
// another agent, ignore it, and delete the file so the next status build is
// clean — while ci-maintainer's own file and any genuinely custom set are
// untouched.
func TestReaderPrunesClonedCIStrip(t *testing.T) {
	useTempStatsDir(t)
	clone := defaultStatsConfig(ciMaintainerStatsAgent)

	// The seed shape: a bare array.
	p := writeStats(t, "reviewer", clone)
	if got := loadStatsConfig("reviewer"); len(got) != 0 {
		t.Errorf("loadStatsConfig(reviewer) = %d stats, want none: the cloned strip was served", len(got))
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("cloned stats.json for reviewer still on disk after read (err=%v)", err)
	}

	// The API's shape: {"stats": [...]}. Same verdict through the other readers.
	p = writeStats(t, "reviewer", map[string]any{"stats": clone})
	s := NewServer(0, testLogger())
	if got := s.loadAgentStats("reviewer"); len(got) != 0 {
		t.Errorf("loadAgentStats(reviewer) = %d stats, want none", len(got))
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("cloned wrapper stats.json for reviewer still on disk (err=%v)", err)
	}
	cfg := &config.Config{Agents: map[string]config.AgentConfig{"reviewer": {}}}
	writeStats(t, "reviewer", clone)
	if got := LoadStatsConfigWithCfg("reviewer", cfg); len(got) != 0 {
		t.Errorf("LoadStatsConfigWithCfg(reviewer) = %d stats, want none", len(got))
	}

	// ci-maintainer keeps its own strip.
	p = writeStats(t, ciMaintainerStatsAgent, clone)
	if got := loadStatsConfig(ciMaintainerStatsAgent); len(got) != len(clone) {
		t.Errorf("loadStatsConfig(ci-maintainer) = %d stats, want %d", len(got), len(clone))
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("ci-maintainer's own stats.json was removed: %v", err)
	}

	// A reviewer set that merely OVERLAPS (or is a strict subset) is the
	// operator's choice and stays.
	custom := []any{
		map[string]any{"key": "prs", "label": "PRs", "source": "agentMetrics", "field": "prs", "style": "number"},
		map[string]any{"key": "ci", "label": "CI", "source": "health", "field": "ci", "style": "pct"},
	}
	p = writeStats(t, "reviewer", custom)
	if got := loadStatsConfig("reviewer"); len(got) != 2 {
		t.Errorf("custom reviewer stats = %d, want 2", len(got))
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("custom reviewer stats.json was removed: %v", err)
	}
}

// TestHealthStatsScopedToRole: the "health" source describes the primary
// repo's workflows and is offered only to agents that can own them — never to
// an ADVISORY or on-demand agent.
func TestHealthStatsScopedToRole(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.AgentConfig
		want bool
	}{
		{"scheduled worker", config.AgentConfig{}, true},
		{"ci-maintainer", config.AgentConfig{Mode: "AUTONOMOUS"}, true},
		{"advisory", config.AgentConfig{Mode: "ADVISORY"}, false},
		{"advisory lowercase", config.AgentConfig{Mode: "advisory"}, false},
		{"on-demand", config.AgentConfig{OnDemand: true}, false},
		{"reviewer as created", config.AgentConfig{Role: "reviewer", Mode: "ADVISORY", OnDemand: true}, false},
	}
	for _, c := range cases {
		if got := healthStatsApply(c.cfg); got != c.want {
			t.Errorf("%s: healthStatsApply = %v, want %v", c.name, got, c.want)
		}
		_, offered := statSourcesFor(&c.cfg)["health"]
		if offered != c.want {
			t.Errorf("%s: statSourcesFor offers health=%v, want %v", c.name, offered, c.want)
		}
	}
	if _, ok := statSourcesFor(nil)["health"]; !ok {
		t.Error("the unscoped catalogue lost the health source")
	}
}

// TestStatSourcesEndpointAndAgentConfigAreScoped: the dashboard reads the
// catalogue from the agent config response (so the Stats tab renders the
// scoped set synchronously) and /api/config/stat-sources honours ?agent=.
func TestStatSourcesEndpointAndAgentConfigAreScoped(t *testing.T) {
	useTempStatsDir(t)
	s, deps := apiServer(t)
	deps.Config.Agents["reviewer"] = config.AgentConfig{Backend: "claude", Model: "sonnet", Enabled: true, Role: "reviewer", Mode: "ADVISORY", OnDemand: true}

	var catalogue struct {
		Sources map[string]any `json:"sources"`
	}
	rec := doGet(s, "/api/config/stat-sources?agent=reviewer")
	if rec.Code != http.StatusOK {
		t.Fatalf("stat-sources?agent=reviewer: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &catalogue)
	if _, ok := catalogue.Sources["health"]; ok {
		t.Error("stat-sources?agent=reviewer still offers the health source to an advisory on-demand agent")
	}
	rec = doGet(s, "/api/config/stat-sources?agent=scanner")
	_ = json.Unmarshal(rec.Body.Bytes(), &catalogue)
	if _, ok := catalogue.Sources["health"]; !ok {
		t.Error("stat-sources?agent=scanner lost the health source for a scheduled agent")
	}
	rec = doGet(s, "/api/config/stat-sources")
	_ = json.Unmarshal(rec.Body.Bytes(), &catalogue)
	if _, ok := catalogue.Sources["health"]; !ok {
		t.Error("unscoped stat-sources lost the health source")
	}

	var agentResp struct {
		Stats       []any `json:"stats"`
		StatSources struct {
			Sources map[string]any `json:"sources"`
			Styles  []string       `json:"styles"`
		} `json:"statSources"`
	}
	rec = doOwnerGet(s, "/api/config/agent/reviewer")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config/agent/reviewer: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &agentResp); err != nil {
		t.Fatal(err)
	}
	if len(agentResp.Stats) != 0 {
		t.Errorf("a fresh reviewer has %d stats, want none: %v", len(agentResp.Stats), statKeys(agentResp.Stats))
	}
	if agentResp.StatSources.Sources == nil {
		t.Fatal("agent config response carries no statSources catalogue for the Stats tab")
	}
	if _, ok := agentResp.StatSources.Sources["health"]; ok {
		t.Error("agent config response offers the health source to the advisory reviewer")
	}
	if len(agentResp.StatSources.Styles) == 0 {
		t.Error("agent config response carries no stat styles")
	}
}

// TestAbsentPercentRendersAsDash pins the client half (fix 3): a pct-bar or
// pct stat with no underlying metric renders "—" like LAST KICK / AVG/PASS on
// the same card, never a hard 0% against the goal.
func TestAbsentPercentRendersAsDash(t *testing.T) {
	html := indexHTML(t)
	start := strings.Index(html, "function renderStatHtml(stat, name)")
	if start < 0 {
		t.Fatal("renderStatHtml not found")
	}
	end := strings.Index(html[start:], "function renderStatsFromConfig")
	if end < 0 {
		t.Fatal("renderStatsFromConfig not found after renderStatHtml")
	}
	fn := html[start : start+end]
	for _, bad := range []string{"const pct = v || 0;", "${v || 0}%"} {
		if strings.Contains(fn, bad) {
			t.Errorf("renderStatHtml still turns an absent percentage into 0%% via %q (#7411)", bad)
		}
	}
	for _, want := range []string{
		"const hasValue = v !== undefined && v !== null && v !== '';",
		"${hasValue ? pct + '%' : '—'}",
		"${hasValue ? v + '%' : '—'}",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("renderStatHtml is missing %q", want)
		}
	}
	// The Stats tab draws its source list from the agent-scoped catalogue.
	for _, want := range []string{
		"function statSourcesForDialog()",
		"const scoped = _configState.data && _configState.data.statSources;",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
}
