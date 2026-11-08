package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// fakeSeedProbe answers the seed's two GitHub questions from fixed data and
// records the paths it was asked about.
type fakeSeedProbe struct {
	branch    string
	branchErr error
	exists    map[string]bool
	probeErr  error
	asked     []string
}

func (f *fakeSeedProbe) DefaultBranch(_ context.Context, _, _ string) (string, error) {
	return f.branch, f.branchErr
}

func (f *fakeSeedProbe) PathExistsAtRef(_ context.Context, _, _, path, _ string) (bool, error) {
	f.asked = append(f.asked, path)
	if f.probeErr != nil {
		return false, f.probeErr
	}
	return f.exists[path], nil
}

func TestSeedKnowledgeGitSource(t *testing.T) {
	operatorSet := []config.GitSourceConfigYAML{{Name: "mine", URL: "https://github.com/acme/wiki", Layer: "org"}}
	tests := []struct {
		name      string
		cfg       func() *config.Config
		probe     *fakeSeedProbe
		nilProbe  bool
		want      *config.GitSourceConfigYAML
		wantAsked []string
		wantLen   int
	}{
		{
			name:      "primary repo with docs/knowledge",
			cfg:       func() *config.Config { return seedCfg("acme/widgets") },
			probe:     &fakeSeedProbe{branch: "main", exists: map[string]bool{"docs/knowledge": true}},
			want:      &config.GitSourceConfigYAML{Name: "widgets-knowledge", URL: "https://github.com/acme/widgets", Branch: "main", Subpath: "docs/knowledge", Layer: "project"},
			wantAsked: []string{"docs/knowledge"},
			wantLen:   1,
		},
		{
			name:      "falls through to src/docs/knowledge on the default branch",
			cfg:       func() *config.Config { return seedCfg("hivecommons/hive") },
			probe:     &fakeSeedProbe{branch: "v5", exists: map[string]bool{"src/docs/knowledge": true}},
			want:      &config.GitSourceConfigYAML{Name: "hive-knowledge", URL: "https://github.com/hivecommons/hive", Branch: "v5", Subpath: "src/docs/knowledge", Layer: "project"},
			wantAsked: []string{"docs/knowledge", "src/docs/knowledge"},
			wantLen:   1,
		},
		{
			name: "bead-synth publish path is probed first and wins",
			cfg: func() *config.Config {
				c := seedCfg("acme/widgets")
				c.Knowledge.BeadSynthesizer.Publish.Path = "knowledge/facts/"
				return c
			},
			probe:     &fakeSeedProbe{branch: "main", exists: map[string]bool{"knowledge/facts": true, "docs/knowledge": true}},
			want:      &config.GitSourceConfigYAML{Name: "widgets-knowledge", URL: "https://github.com/acme/widgets", Branch: "main", Subpath: "knowledge/facts", Layer: "project"},
			wantAsked: []string{"knowledge/facts"},
			wantLen:   1,
		},
		{
			name: "publish path equal to a default is not probed twice",
			cfg: func() *config.Config {
				c := seedCfg("acme/widgets")
				c.Knowledge.BeadSynthesizer.Publish.Path = "docs/knowledge/"
				return c
			},
			probe:     &fakeSeedProbe{branch: "main"},
			wantAsked: []string{"docs/knowledge", "src/docs/knowledge"},
		},
		{
			name: "GHES base URL is used for the clone URL",
			cfg: func() *config.Config {
				c := seedCfg("acme/widgets")
				c.GitHub.BaseURL = "https://github.example.com/"
				return c
			},
			probe:     &fakeSeedProbe{branch: "main", exists: map[string]bool{"docs/knowledge": true}},
			want:      &config.GitSourceConfigYAML{Name: "widgets-knowledge", URL: "https://github.example.com/acme/widgets", Branch: "main", Subpath: "docs/knowledge", Layer: "project"},
			wantAsked: []string{"docs/knowledge"},
			wantLen:   1,
		},
		{
			name:      "no knowledge dir anywhere skips",
			cfg:       func() *config.Config { return seedCfg("acme/widgets") },
			probe:     &fakeSeedProbe{branch: "main"},
			wantAsked: []string{"docs/knowledge", "src/docs/knowledge"},
		},
		{
			name: "operator list is never touched",
			cfg: func() *config.Config {
				c := seedCfg("acme/widgets")
				c.Knowledge.GitSources = append([]config.GitSourceConfigYAML(nil), operatorSet...)
				return c
			},
			probe:   &fakeSeedProbe{branch: "main", exists: map[string]bool{"docs/knowledge": true}},
			wantLen: 1,
		},
		{
			name: "explicit empty list opts out",
			cfg: func() *config.Config {
				c := seedCfg("acme/widgets")
				c.Knowledge.GitSources = []config.GitSourceConfigYAML{}
				return c
			},
			probe: &fakeSeedProbe{branch: "main", exists: map[string]bool{"docs/knowledge": true}},
		},
		{
			name: "falls back to the first project repo",
			cfg: func() *config.Config {
				c := seedCfg("")
				c.Project.Repos = []string{"acme/first", "acme/second"}
				return c
			},
			probe:     &fakeSeedProbe{branch: "main", exists: map[string]bool{"docs/knowledge": true}},
			want:      &config.GitSourceConfigYAML{Name: "first-knowledge", URL: "https://github.com/acme/first", Branch: "main", Subpath: "docs/knowledge", Layer: "project"},
			wantAsked: []string{"docs/knowledge"},
			wantLen:   1,
		},
		{
			name:  "no primary repo skips",
			cfg:   func() *config.Config { return seedCfg("") },
			probe: &fakeSeedProbe{branch: "main", exists: map[string]bool{"docs/knowledge": true}},
		},
		{
			name: "non-github forge skips",
			cfg: func() *config.Config {
				c := seedCfg("acme/widgets")
				c.Project.Forge = config.ForgeGitLab
				return c
			},
			probe: &fakeSeedProbe{branch: "main", exists: map[string]bool{"docs/knowledge": true}},
		},
		{
			name:     "no github client skips",
			cfg:      func() *config.Config { return seedCfg("acme/widgets") },
			nilProbe: true,
		},
		{
			name:  "default branch lookup failure skips",
			cfg:   func() *config.Config { return seedCfg("acme/widgets") },
			probe: &fakeSeedProbe{branchErr: errors.New("boom"), exists: map[string]bool{"docs/knowledge": true}},
		},
		{
			name:      "non-404 probe error skips rather than guessing",
			cfg:       func() *config.Config { return seedCfg("acme/widgets") },
			probe:     &fakeSeedProbe{branch: "main", probeErr: errors.New("503")},
			wantAsked: []string{"docs/knowledge"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg()
			var probe knowledgeSeedProbe
			if !tc.nilProbe {
				probe = tc.probe
			}
			seeded := seedKnowledgeGitSource(context.Background(), cfg, probe, slog.Default())
			if seeded != (tc.want != nil) {
				t.Fatalf("seeded = %v, want %v", seeded, tc.want != nil)
			}
			if len(cfg.Knowledge.GitSources) != tc.wantLen {
				t.Fatalf("git_sources = %+v, want %d entries", cfg.Knowledge.GitSources, tc.wantLen)
			}
			if tc.want != nil && cfg.Knowledge.GitSources[0] != *tc.want {
				t.Errorf("seeded = %+v, want %+v", cfg.Knowledge.GitSources[0], *tc.want)
			}
			if tc.probe != nil && !equalStrings(tc.probe.asked, tc.wantAsked) {
				t.Errorf("probed paths = %v, want %v", tc.probe.asked, tc.wantAsked)
			}
			// Idempotent: a second pass sees the seeded (or operator) list and
			// never appends again.
			before := len(cfg.Knowledge.GitSources)
			if seedKnowledgeGitSource(context.Background(), cfg, probe, slog.Default()) || len(cfg.Knowledge.GitSources) != before {
				t.Errorf("second pass changed git_sources: %+v", cfg.Knowledge.GitSources)
			}
		})
	}
}

func seedCfg(primary string) *config.Config {
	c := &config.Config{}
	c.Project.PrimaryRepo = primary
	return c
}
