package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func newDepsTestBoot(t *testing.T, cfg *config.Config) (*boot, *bytes.Buffer) {
	t.Helper()
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &boot{ctx: ctx, cfg: cfg, logger: logger}, &log
}

func fakeGitHubClient(t *testing.T) *github.Client {
	t.Helper()
	return github.NewClient("fake-token", "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), "http://127.0.0.1:1")
}

// --- bootGitHub -------------------------------------------------------------

func TestBootGitHubWithHandsOffResolvedAuth(t *testing.T) {
	cfg := &config.Config{}
	cfg.Governor.Labels.Exempt = []string{"wip"}
	cfg.Governor.Labels.AutoMerge = "ship-it"
	b, _ := newDepsTestBoot(t, cfg)
	client := fakeGitHubClient(t)

	var gotCfg *config.Config
	b.bootGitHubWith(bootGitHubDeps{
		initGitHubAuth: func(_ context.Context, c *config.Config, _ *slog.Logger) githubAuth {
			gotCfg = c
			return githubAuth{Client: client}
		},
	})

	if gotCfg != cfg {
		t.Fatal("initGitHubAuth did not receive the boot config")
	}
	if b.ghClient != client || b.ghAuth.Client != client {
		t.Fatal("resolved client not handed off to boot")
	}
	if b.appAuthFailure != "" || b.appAuthState != github.AppStateUnknown {
		t.Fatalf("healthy auth must leave failure empty: %q/%v", b.appAuthFailure, b.appAuthState)
	}
	if got := client.AutoMergeLabel(); got != "ship-it" {
		t.Fatalf("auto-merge label = %q, want ship-it", got)
	}
}

func TestBootGitHubWithNilClientIsNotFatal(t *testing.T) {
	cfg := &config.Config{}
	cfg.Governor.Labels.Exempt = []string{"wip"}
	b, _ := newDepsTestBoot(t, cfg)

	b.bootGitHubWith(bootGitHubDeps{
		initGitHubAuth: func(context.Context, *config.Config, *slog.Logger) githubAuth {
			return githubAuth{Failure: "no key delivered", State: github.AppStateKeyMissing}
		},
	})

	if b.ghClient != nil {
		t.Fatal("expected nil client to be preserved")
	}
	if b.appAuthFailure != "no key delivered" || b.appAuthState != github.AppStateKeyMissing {
		t.Fatalf("failure/state not handed off: %q/%v", b.appAuthFailure, b.appAuthState)
	}
}

// --- bootAdvisory -----------------------------------------------------------

type bootAdvisoryFake struct {
	deps bootAdvisoryDeps

	ensured    []string
	ensureNum  int
	ensureErr  error
	classified int
	raise      bool
	diag       string
	state      github.AppAuthState
}

func newBootAdvisoryFake() *bootAdvisoryFake {
	f := &bootAdvisoryFake{ensureNum: 42}
	f.deps = bootAdvisoryDeps{
		ensureAdvisoryIssue: func(_ context.Context, _ *github.Client, repo string) (int, error) {
			f.ensured = append(f.ensured, repo)
			return f.ensureNum, f.ensureErr
		},
		classifyAppFailure: func(context.Context, *github.AppAuth, string, *slog.Logger) (bool, string, github.AppAuthState) {
			f.classified++
			return f.raise, f.diag, f.state
		},
	}
	return f
}

func bootAdvisoryConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.HiveID = "boot-advisory-test"
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"acme/widgets", "acme/gadgets"}
	cfg.Policies.LocalDir = t.TempDir()
	lvl := 3
	cfg.ACMMLevel = &lvl
	return cfg
}

func TestBootAdvisoryWithEnsuresIssueAndWritesPolicy(t *testing.T) {
	t.Setenv("HIVE_ADVISORY_ISSUE", "")
	cfg := bootAdvisoryConfig(t)
	b, _ := newDepsTestBoot(t, cfg)
	b.ghClient = fakeGitHubClient(t)
	f := newBootAdvisoryFake()

	b.bootAdvisoryWith(f.deps)

	if len(f.ensured) != 1 || f.ensured[0] != "acme/widgets" {
		t.Fatalf("ensured = %v, want the first repo as primary", f.ensured)
	}
	if b.advisoryIssues["acme/widgets"] != 42 {
		t.Fatalf("advisoryIssues = %v", b.advisoryIssues)
	}
	if got := os.Getenv("HIVE_ADVISORY_ISSUE"); got != "42" {
		t.Fatalf("HIVE_ADVISORY_ISSUE = %q", got)
	}
	if b.githubAppRequired || b.githubAppDiag != "" {
		t.Fatalf("healthy boot must not raise the banner: %v %q", b.githubAppRequired, b.githubAppDiag)
	}
	if b.acmmLevel != 3 || b.notifier == nil || b.advisoryStore == nil {
		t.Fatal("notifier/acmm/store not handed off")
	}
	if b.policyDirPath != cfg.Policies.LocalDir {
		t.Fatalf("policyDirPath = %q, want %q", b.policyDirPath, cfg.Policies.LocalDir)
	}
	data, err := os.ReadFile(filepath.Join(cfg.Policies.LocalDir, "brainstorm-advisory.md"))
	if err != nil || len(data) == 0 {
		t.Fatalf("brainstorm policy not written: %v", err)
	}
	if f.classified != 0 {
		t.Fatal("classifier must not run when ensure succeeds")
	}
}

func TestBootAdvisoryWithPrefersPrimaryRepo(t *testing.T) {
	cfg := bootAdvisoryConfig(t)
	cfg.Project.PrimaryRepo = "acme/gadgets"
	b, _ := newDepsTestBoot(t, cfg)
	b.ghClient = fakeGitHubClient(t)
	f := newBootAdvisoryFake()

	b.bootAdvisoryWith(f.deps)

	if len(f.ensured) != 1 || f.ensured[0] != "acme/gadgets" {
		t.Fatalf("ensured = %v", f.ensured)
	}
}

func TestBootAdvisoryWithSkipsEnsureWithoutClientOrLevel(t *testing.T) {
	for name, mutate := range map[string]func(*boot){
		"nil client": func(b *boot) { b.ghClient = nil },
		"level zero": func(b *boot) { b.ghClient = fakeGitHubClient(t); lvl := 0; b.cfg.ACMMLevel = &lvl },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := bootAdvisoryConfig(t)
			b, _ := newDepsTestBoot(t, cfg)
			mutate(b)
			f := newBootAdvisoryFake()

			b.bootAdvisoryWith(f.deps)

			if len(f.ensured) != 0 {
				t.Fatalf("ensure must be skipped, got %v", f.ensured)
			}
			if len(b.advisoryIssues) != 0 {
				t.Fatalf("advisoryIssues = %v", b.advisoryIssues)
			}
		})
	}
}

func TestBootAdvisoryWithSeedsBannerFromStartupFailure(t *testing.T) {
	cfg := bootAdvisoryConfig(t)
	b, _ := newDepsTestBoot(t, cfg)
	b.appAuthFailure, b.appAuthState = "key never delivered", github.AppStateKeyMissing

	b.bootAdvisoryWith(newBootAdvisoryFake().deps)

	if !b.githubAppRequired || b.githubAppDiag != "key never delivered" || b.githubAppState != github.AppStateKeyMissing {
		t.Fatalf("banner not seeded: %v %q %v", b.githubAppRequired, b.githubAppDiag, b.githubAppState)
	}
}

func TestBootAdvisoryWithConfigTruthOutranksProbes(t *testing.T) {
	cfg := bootAdvisoryConfig(t)
	cfg.GitHub.AppID = 12345
	cfg.GitHub.InstallationID = 0
	b, _ := newDepsTestBoot(t, cfg)

	b.bootAdvisoryWith(newBootAdvisoryFake().deps)

	if !b.githubAppRequired || b.githubAppState != github.AppStateNotInstalled {
		t.Fatalf("uninstalled App must raise: %v %v", b.githubAppRequired, b.githubAppState)
	}
	if !strings.Contains(b.githubAppDiag, "12345") || !strings.Contains(b.githubAppDiag, "no installation") {
		t.Fatalf("diag = %q", b.githubAppDiag)
	}
}

func TestBootAdvisoryWithEnsureFailureRaisesOnlyWhenClassifierSays(t *testing.T) {
	cases := []struct {
		name      string
		raise     bool
		wantRaise bool
		wantLog   string
	}{
		{"classifier raises", true, true, "GitHub App authentication failed at startup"},
		{"classifier declines", false, false, "not raising the App banner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bootAdvisoryConfig(t)
			b, log := newDepsTestBoot(t, cfg)
			b.ghClient = fakeGitHubClient(t)
			f := newBootAdvisoryFake()
			f.ensureErr = errors.New("403 Resource not accessible by integration")
			f.raise, f.diag, f.state = tc.raise, "App not installed on acme", github.AppStateNotInstalled

			b.bootAdvisoryWith(f.deps)

			if f.classified != 1 {
				t.Fatalf("classified %d times, want 1", f.classified)
			}
			if b.githubAppRequired != tc.wantRaise {
				t.Fatalf("githubAppRequired = %v, want %v", b.githubAppRequired, tc.wantRaise)
			}
			if tc.wantRaise && (b.githubAppDiag != f.diag || b.githubAppState != f.state) {
				t.Fatalf("diag/state = %q/%v", b.githubAppDiag, b.githubAppState)
			}
			if !strings.Contains(log.String(), tc.wantLog) {
				t.Fatalf("log missing %q:\n%s", tc.wantLog, log.String())
			}
			if len(b.advisoryIssues) != 0 {
				t.Fatalf("advisoryIssues = %v after failed ensure", b.advisoryIssues)
			}
		})
	}
}

func TestBootAdvisoryWithRateLimitDoesNotClassify(t *testing.T) {
	cfg := bootAdvisoryConfig(t)
	b, log := newDepsTestBoot(t, cfg)
	b.ghClient = fakeGitHubClient(t)
	f := newBootAdvisoryFake()
	f.ensureErr = errors.New("403 API rate limit exceeded")

	b.bootAdvisoryWith(f.deps)

	if f.classified != 0 {
		t.Fatal("rate limit must not trigger App classification")
	}
	if b.githubAppRequired {
		t.Fatal("rate limit must not raise the banner")
	}
	if !strings.Contains(log.String(), "rate limit hit during advisory issue ensure") {
		t.Fatalf("log:\n%s", log.String())
	}
}
