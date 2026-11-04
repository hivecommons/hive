package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/writeback"
)

func regCfg(org string, mutate func(c *Config)) *Config {
	c := &Config{}
	c.Project.Org = org
	if mutate != nil {
		mutate(c)
	}
	return c
}

func gateRegNames(regs []GateRegression) []string {
	out := make([]string, 0, len(regs))
	for _, g := range regs {
		out = append(out, g.Gate)
	}
	return out
}

func TestDetectGateRegressions(t *testing.T) {
	full := func(c *Config) {
		c.Review.RequireLabels = []string{"hive-review"}
		c.Project.IssueFilter.RequireLabels = []string{"hive-ok"}
		c.Project.PausedRepos = []RepoPause{{Repo: "Console"}}
		c.Writeback.PerRepo = map[string]writeback.RepoOverride{
			"torch-spyre/spyre-inference": {Mode: writeback.RepoModeIssuesOnly},
		}
	}
	cases := []struct {
		name      string
		prior     *Config
		effective *Config
		want      []string
		wantPrev  map[string]string
	}{
		{
			name:      "unchanged",
			prior:     regCfg("acme", full),
			effective: regCfg("acme", full),
		},
		{
			name:      "no prior gates",
			prior:     regCfg("acme", nil),
			effective: regCfg("acme", nil),
		},
		{
			name:  "all dropped",
			prior: regCfg("acme", full),
			effective: regCfg("acme", func(c *Config) {
				c.Review.RequireLabels = []string{" "}
			}),
			want: []string{
				GateReviewRequireLabels,
				GateIssueRequireLabels,
				GatePausedRepos + "[acme/console]",
				GateWritebackPerRepo + "[torch-spyre/spyre-inference].mode",
			},
			wantPrev: map[string]string{
				GateReviewRequireLabels:                                     "[hive-review]",
				GatePausedRepos + "[acme/console]":                          "paused",
				GateWritebackPerRepo + "[torch-spyre/spyre-inference].mode": "issues-only",
			},
		},
		{
			name:  "weakened per-repo writeback mode",
			prior: regCfg("acme", full),
			effective: regCfg("acme", func(c *Config) {
				full(c)
				c.Writeback.PerRepo["torch-spyre/spyre-inference"] = writeback.RepoOverride{Mode: writeback.RepoModeFull}
			}),
			want: []string{GateWritebackPerRepo + "[torch-spyre/spyre-inference].mode"},
		},
		{
			name:  "tightened per-repo writeback mode is not a regression",
			prior: regCfg("acme", full),
			effective: regCfg("acme", func(c *Config) {
				full(c)
				c.Writeback.PerRepo["torch-spyre/spyre-inference"] = writeback.RepoOverride{Mode: writeback.RepoModeOff}
			}),
		},
		{
			name: "dropped full override is a tightening",
			prior: regCfg("acme", func(c *Config) {
				c.Writeback.PerRepo = map[string]writeback.RepoOverride{"acme/x": {Mode: writeback.RepoModeFull}}
			}),
			effective: regCfg("acme", nil),
		},
		{
			name:  "label list changed but still non-empty",
			prior: regCfg("acme", full),
			effective: regCfg("acme", func(c *Config) {
				full(c)
				c.Review.RequireLabels = []string{"other-label"}
			}),
		},
		{
			name:  "pause matched across spellings",
			prior: regCfg("acme", func(c *Config) { c.Project.PausedRepos = []RepoPause{{Repo: "acme/Console"}} }),
			effective: regCfg("acme", func(c *Config) {
				c.Project.PausedRepos = []RepoPause{{Repo: "console"}}
			}),
		},
		{
			name:      "nil configs",
			prior:     nil,
			effective: regCfg("acme", nil),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectGateRegressions(tc.effective, tc.prior)
			if strings.Join(gateRegNames(got), ",") != strings.Join(tc.want, ",") {
				t.Fatalf("gates = %v, want %v", gateRegNames(got), tc.want)
			}
			for _, g := range got {
				if want, ok := tc.wantPrev[g.Gate]; ok && g.Previous != want {
					t.Errorf("%s previous = %q, want %q", g.Gate, g.Previous, want)
				}
				if g.Effective == "" {
					t.Errorf("%s has empty effective value", g.Gate)
				}
			}
		})
	}
}

func TestDetectPriorOverlayGateRegressions(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	effective := regCfg("acme", nil)
	effective.SourcePath = write("hive.yaml", "project:\n  org: acme\n")

	t.Run("no prior overlay", func(t *testing.T) {
		got := DetectPriorOverlayGateRegressions(effective, []string{filepath.Join(dir, "missing.yaml")})
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("empty and malformed overlays are ignored", func(t *testing.T) {
		empty := write("hive.yaml.runtime", "")
		bad := write("hive.yaml.runtime.bak", "review: [not: a: map\n")
		if got := DetectPriorOverlayGateRegressions(effective, []string{empty, bad}); len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("effective source path is skipped", func(t *testing.T) {
		eff := regCfg("acme", nil)
		eff.SourcePath = write("gated.yaml", "project:\n  org: acme\nreview:\n  require_labels: [hive-review]\n")
		if got := DetectPriorOverlayGateRegressions(eff, []string{eff.SourcePath}); len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("first overlay wins per gate, later overlays add new gates", func(t *testing.T) {
		runtime := write("prior.runtime", "project:\n  org: acme\nreview:\n  require_labels: [hive-review]\n")
		dash := write("prior.dashboard", "project:\n  org: acme\n  paused_repos:\n    - repo: console\nreview:\n  require_labels: [other]\n")
		got := DetectPriorOverlayGateRegressions(effective, []string{runtime, dash})
		want := []string{GateReviewRequireLabels, GatePausedRepos + "[acme/console]"}
		if strings.Join(gateRegNames(got), ",") != strings.Join(want, ",") {
			t.Fatalf("gates = %v, want %v", gateRegNames(got), want)
		}
		if got[0].Previous != "[hive-review]" || got[0].Source != runtime {
			t.Errorf("review gate = %+v, want previous from %s", got[0], runtime)
		}
		if got[1].Source != dash {
			t.Errorf("pause gate source = %q, want %s", got[1].Source, dash)
		}
	})
}

func TestGateRegressionError_ListsEveryGate(t *testing.T) {
	err := &GateRegressionError{Regressions: []GateRegression{
		{Gate: GateReviewRequireLabels, Previous: "[hive-review]", Effective: "none", Source: "/data/hive.yaml.runtime.bak"},
		{Gate: GatePausedRepos + "[acme/console]", Previous: "paused", Effective: "not paused"},
	}}
	msg := err.Error()
	for _, want := range []string{"refuse_gate_regression", "2 safety gate(s)", "review.require_labels (was [hive-review])", "acme/console", "/data/hive.yaml.runtime.bak"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
}
