package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

// forgeKindOf reports the forge kind of a writer, or "" when the writer is not
// a pkg/forge adapter at all (i.e. it is the raw *github.Client).
func forgeKindOf(t *testing.T, w forge.IssueWriter) forge.Kind {
	t.Helper()
	if f, ok := w.(forge.Forge); ok {
		return f.Kind()
	}
	return ""
}

// TestGovernorForgeKeepsGitHubOnTheConcreteClient pins the no-behavior-change
// half of the swap: on a GitHub hive (explicit or defaulted) the governor gets
// the very same *github.Client it always used, with no adapter interposed. If
// this ever starts returning an adapter, every GitHub hive silently changes its
// write path — which is exactly what this change is NOT allowed to do.
func TestGovernorForgeKeepsGitHubOnTheConcreteClient(t *testing.T) {
	gh := github.NewClient("t", "acme", nil, discardLogger(), "")

	for _, kind := range []string{"", config.ForgeGitHub} {
		cfg := &config.Config{}
		cfg.Project.Org = "acme"
		cfg.Project.Forge = kind

		got := governorForge(cfg, gh, discardLogger())
		if got != forge.IssueWriter(gh) {
			t.Fatalf("project.forge=%q: want the *github.Client itself, got %T", kind, got)
		}
	}
}

// TestGovernorForgeNilClientIsUntypedNil guards the typed-nil trap: a nil
// *github.Client stored in an interface is not == nil, so returning it would
// sail past the sweep's "no client, do nothing" guard and panic on the first
// write. The guard tests the interface, so the nil must be untyped.
func TestGovernorForgeNilClientIsUntypedNil(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Forge = config.ForgeGitHub

	if got := governorForge(cfg, nil, discardLogger()); got != nil {
		t.Fatalf("want untyped nil for a hive with no GitHub client, got %#v", got)
	}
	if got := governorForge(nil, nil, discardLogger()); got != nil {
		t.Fatalf("want untyped nil for a nil config with no client, got %#v", got)
	}
}

// TestGovernorForgeSelectsAdapters is the point of the whole change: a hive
// that says project.forge: gitlab (or gitea) gets the pkg/forge adapter for
// that forge on the governor path, not GitHub. Before this wiring the config
// key existed, the adapters existed and were tested, and nothing connected them.
func TestGovernorForgeSelectsAdapters(t *testing.T) {
	gh := github.NewClient("t", "acme", nil, discardLogger(), "")

	t.Run("gitlab uses the configured instance and token env", func(t *testing.T) {
		t.Setenv("HIVE_TEST_GITLAB_TOKEN", "glpat-xxx")
		cfg := &config.Config{}
		cfg.Project.Org = "acme"
		cfg.Project.Forge = config.ForgeGitLab
		cfg.GitLab.URL = "https://gitlab.example.com"
		cfg.GitLab.TokenEnv = "HIVE_TEST_GITLAB_TOKEN"

		if got := forgeKindOf(t, governorForge(cfg, gh, discardLogger())); got != forge.KindGitLab {
			t.Fatalf("want the GitLab adapter, got kind %q", got)
		}
	})

	t.Run("gitlab defaults to gitlab.com with no url configured", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Project.Forge = config.ForgeGitLab

		if got := forgeKindOf(t, governorForge(cfg, gh, discardLogger())); got != forge.KindGitLab {
			t.Fatalf("want the GitLab adapter, got kind %q", got)
		}
	})

	t.Run("gitea uses the configured instance", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Project.Org = "acme"
		cfg.Project.Forge = config.ForgeGitea
		cfg.Gitea.URL = "https://gitea.example.com"

		if got := forgeKindOf(t, governorForge(cfg, gh, discardLogger())); got != forge.KindGitea {
			t.Fatalf("want the Gitea adapter, got kind %q", got)
		}
	})
}

// TestGovernorForgeFallsBackOnUnusableConfig covers the two ways adapter
// construction can fail — Gitea named with no instance URL (it has no public
// default) and a forge kind this build does not know. Both fall back to the
// GitHub client rather than dropping the write, so a hive that typo'd its forge
// key still gets its escalation evidence onto the PR.
func TestGovernorForgeFallsBackOnUnusableConfig(t *testing.T) {
	gh := github.NewClient("t", "acme", nil, discardLogger(), "")

	cases := map[string]func(*config.Config){
		"gitea with no instance url": func(c *config.Config) { c.Project.Forge = config.ForgeGitea },
		"unknown forge kind":         func(c *config.Config) { c.Project.Forge = "bitbucket" },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{}
			setup(cfg)

			if got := governorForge(cfg, gh, discardLogger()); got != forge.IssueWriter(gh) {
				t.Fatalf("want the GitHub client as fallback, got %T", got)
			}
			// And with no GitHub client to fall back to, an untyped nil — never
			// a typed-nil that would panic on the first write.
			if got := governorForge(cfg, nil, discardLogger()); got != nil {
				t.Fatalf("want untyped nil when there is nothing to fall back to, got %#v", got)
			}
		})
	}
}
