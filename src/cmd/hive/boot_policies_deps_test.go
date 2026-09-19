package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/snapshot"
)

type bootPoliciesFake struct {
	deps bootPoliciesDeps

	applied   []int
	appliedTo *dashboard.Server
	applyErr  error
	watcher   []string
	watchPoll time.Duration
	watchErr  error
}

func newBootPoliciesFake() *bootPoliciesFake {
	f := &bootPoliciesFake{}
	f.deps = bootPoliciesDeps{
		applyPack: func(srv *dashboard.Server, level int) (*dashboard.ApplyPackResult, error) {
			f.applied = append(f.applied, level)
			f.appliedTo = srv
			if f.applyErr != nil {
				return nil, f.applyErr
			}
			return &dashboard.ApplyPackResult{Name: "pack", Created: []string{"scanner"}}, nil
		},
		startPolicyWatcher: func(_ context.Context, repo, branch, subPath, localDir string, poll time.Duration, _ *slog.Logger) error {
			f.watcher = []string{repo, branch, subPath, localDir}
			f.watchPoll = poll
			return f.watchErr
		},
	}
	return f
}

func newBootPoliciesBoot(t *testing.T, cfg *config.Config, saved *snapshot.PersistedState) (*boot, *strings.Builder) {
	t.Helper()
	b, _ := newDepsTestBoot(t, cfg)
	var log strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&log, nil))
	b.dashSrv = dashboard.NewServer(0, b.logger)
	b.saved = saved
	return b, &log
}

func TestBootPoliciesWithNoLevelNoRepoDoesNothing(t *testing.T) {
	t.Setenv(hiveLevelEnv, "")
	b, _ := newBootPoliciesBoot(t, &config.Config{}, nil)
	f := newBootPoliciesFake()

	b.bootPoliciesWith(f.deps)

	if len(f.applied) != 0 || f.watcher != nil {
		t.Fatalf("applied=%v watcher=%v", f.applied, f.watcher)
	}
}

func TestBootPoliciesWithFirstStartAutoAppliesEnvLevel(t *testing.T) {
	t.Setenv(hiveLevelEnv, "3")
	b, log := newBootPoliciesBoot(t, &config.Config{}, nil)
	f := newBootPoliciesFake()

	b.bootPoliciesWith(f.deps)

	if len(f.applied) != 1 || f.applied[0] != 3 || f.appliedTo != b.dashSrv {
		t.Fatalf("applied = %v", f.applied)
	}
	for _, want := range []string{"first start detected, auto-applying ACMM pack", "ACMM pack auto-applied"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("log missing %q:\n%s", want, log.String())
		}
	}
}

func TestBootPoliciesWithInvalidEnvLevelWarns(t *testing.T) {
	t.Setenv(hiveLevelEnv, "banana")
	b, log := newBootPoliciesBoot(t, &config.Config{}, nil)
	f := newBootPoliciesFake()

	b.bootPoliciesWith(f.deps)

	if len(f.applied) != 0 || !strings.Contains(log.String(), "invalid HIVE_LEVEL") {
		t.Fatalf("applied=%v log:\n%s", f.applied, log.String())
	}
}

func TestBootPoliciesWithRestartReappliesConfigLevel(t *testing.T) {
	t.Setenv(hiveLevelEnv, "")
	cfg := &config.Config{}
	lvl := 4
	cfg.ACMMLevel = &lvl
	savedLvl := 2
	b, log := newBootPoliciesBoot(t, cfg, &snapshot.PersistedState{ACMMLevel: &savedLvl})
	f := newBootPoliciesFake()

	b.bootPoliciesWith(f.deps)

	if len(f.applied) != 1 || f.applied[0] != 4 {
		t.Fatalf("applied = %v", f.applied)
	}
	for _, want := range []string{"audit: ", "ACMM pack applied on startup"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("log missing %q:\n%s", want, log.String())
		}
	}
}

func TestBootPoliciesWithApplyErrorsAreLogged(t *testing.T) {
	t.Setenv(hiveLevelEnv, "2")
	cases := []struct {
		name  string
		saved *snapshot.PersistedState
		want  string
	}{
		{"first start", nil, "failed to auto-apply ACMM pack"},
		{"restart", &snapshot.PersistedState{}, "failed to apply ACMM pack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, log := newBootPoliciesBoot(t, &config.Config{}, tc.saved)
			f := newBootPoliciesFake()
			f.applyErr = errors.New("pack missing")
			b.bootPoliciesWith(f.deps)
			if !strings.Contains(log.String(), tc.want) {
				t.Fatalf("log missing %q:\n%s", tc.want, log.String())
			}
		})
	}
}

func TestBootPoliciesWithStartsPolicyWatcher(t *testing.T) {
	t.Setenv(hiveLevelEnv, "")
	cfg := &config.Config{}
	cfg.Policies.Repo = "https://example.invalid/acme/policies.git"
	cfg.Policies.Branch = "main"
	cfg.Policies.Path = "hive"
	cfg.Policies.LocalDir = t.TempDir()
	cfg.Policies.PollInterval = 7 * time.Minute
	b, log := newBootPoliciesBoot(t, cfg, nil)
	f := newBootPoliciesFake()
	f.watchErr = errors.New("clone failed")

	b.bootPoliciesWith(f.deps)

	if got := strings.Join(f.watcher, "|"); got != cfg.Policies.Repo+"|main|hive|"+cfg.Policies.LocalDir || f.watchPoll != 7*time.Minute {
		t.Fatalf("watcher = %q poll=%v", got, f.watchPoll)
	}
	if !strings.Contains(log.String(), "policy watcher failed to start") {
		t.Fatalf("log:\n%s", log.String())
	}
}

func TestBootPoliciesWithDefaultLocalDir(t *testing.T) {
	t.Setenv(hiveLevelEnv, "")
	cfg := &config.Config{}
	cfg.Policies.Repo = "https://example.invalid/acme/policies.git"
	b, _ := newBootPoliciesBoot(t, cfg, nil)
	f := newBootPoliciesFake()

	b.bootPoliciesWith(f.deps)

	if f.watcher == nil || f.watcher[3] != defaultPoliciesLocalDir {
		t.Fatalf("watcher = %v", f.watcher)
	}
}
