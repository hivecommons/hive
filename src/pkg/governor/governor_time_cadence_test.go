package governor

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

func timeCadenceGovernor(c config.Cadence, now time.Time) *Governor {
	cfg := config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": c}},
	}}
	g := New(cfg, map[string]config.AgentConfig{"scanner": {Backend: "bob", Enabled: true}}, slog.Default())
	g.now = func() time.Time { return now }
	return g
}

func TestTimeCadenceDueOncePerOccurrence(t *testing.T) {
	c, err := configFromYAML(`{times: ["09:00"], tz: UTC}`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 9, 0, 5, 0, time.UTC)
	g := timeCadenceGovernor(c, now)
	if due := g.Evaluate(0, 0, 0, 0); len(due) != 1 || due[0] != "scanner" {
		t.Fatalf("due at occurrence = %v", due)
	}
	g.RecordKick("scanner")
	if due := g.Evaluate(0, 0, 0, 0); len(due) != 0 {
		t.Fatalf("same occurrence double-fired: %v", due)
	}
}

func TestTimeCadencePausedOffAndCatchUp(t *testing.T) {
	c, err := configFromYAML(`{times: ["09:00"], tz: UTC}`)
	if err != nil {
		t.Fatal(err)
	}
	g := timeCadenceGovernor(c, time.Date(2026, 8, 7, 9, 8, 0, 0, time.UTC))
	g.SeedLastKicks(map[string]time.Time{"scanner": time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)})
	if due := g.Evaluate(0, 0, 0, 0); len(due) != 1 {
		t.Fatalf("expected catch-up inside window, got %v", due)
	}
	g.now = func() time.Time { return time.Date(2026, 8, 7, 9, 11, 0, 0, time.UTC) }
	if due := g.Evaluate(0, 0, 0, 0); len(due) != 0 {
		t.Fatalf("expected missed occurrence outside catch-up window to skip, got %v", due)
	}

	g.cfg.Modes["idle"] = config.ModeConfig{Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "off"}}
	if due := g.Evaluate(0, 0, 0, 0); len(due) != 0 {
		t.Fatalf("off agent fired: %v", due)
	}
}

func configFromYAML(src string) (config.Cadence, error) {
	var c config.Cadence
	err := c.UnmarshalYAML(yamlNode(src))
	return c, err
}

func yamlNode(src string) *yaml.Node {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		panic(err)
	}
	return n.Content[0]
}
