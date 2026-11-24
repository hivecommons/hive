package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	t.Run("forgejo uses the configured instance", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Project.Org = "acme"
		cfg.Project.Forge = config.ForgeForgejo
		cfg.Forgejo.URL = "https://forgejo.example.com"

		if got := forgeKindOf(t, governorForge(cfg, gh, discardLogger())); got != forge.KindForgejo {
			t.Fatalf("want the Forgejo adapter, got kind %q", got)
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

// TestConfigForgeKindsMatchForgeKinds pins the by-value contract between
// config's forge names and pkg/forge.Kind: config cannot import pkg/forge, so
// a rename on either side must fail here rather than silently fall back.
func TestConfigForgeKindsMatchForgeKinds(t *testing.T) {
	want := []forge.Kind{forge.KindGitHub, forge.KindGitLab, forge.KindGitea, forge.KindForgejo}
	if len(config.KnownForgeKinds) != len(want) {
		t.Fatalf("config.KnownForgeKinds = %v, want %v", config.KnownForgeKinds, want)
	}
	for i, k := range want {
		if config.KnownForgeKinds[i] != string(k) {
			t.Fatalf("config.KnownForgeKinds[%d] = %q, want %q", i, config.KnownForgeKinds[i], k)
		}
	}
}

// TestGovernorForgeRoutesPerRepo covers project.repo_policies[].forge
// (hivecommons/hive#14112): one hive watching a GitHub, a GitLab and a Gitea
// repo hands each write to the forge its repo lives on.
func TestGovernorForgeRoutesPerRepo(t *testing.T) {
	gh := github.NewClient("t", "acme", nil, discardLogger(), "")
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"app", "lab", "tea"}
	cfg.GitLab.URL = "https://gitlab.example.com"
	cfg.Project.RepoPolicies = []config.RepoPolicy{
		{Repo: "lab", Forge: config.ForgeGitLab, ForgeURL: "https://gitlab.example.com"},
		{Repo: "tea", Forge: config.ForgeGitea, ForgeURL: "https://codeberg.org"},
	}

	w := governorForge(cfg, gh, discardLogger())
	router, ok := w.(*repoForgeRouter)
	if !ok {
		t.Fatalf("want a per-repo router when repo_policies set a forge, got %T", w)
	}
	cases := map[string]forge.Kind{"acme/app": "", "acme/lab": forge.KindGitLab, "acme/tea": forge.KindGitea}
	for repo, want := range cases {
		got, err := router.writerFor(repo)
		if err != nil {
			t.Fatalf("writerFor(%q): %v", repo, err)
		}
		if want == "" {
			if got != forge.IssueWriter(gh) {
				t.Fatalf("writerFor(%q) = %T, want the *github.Client itself", repo, got)
			}
			continue
		}
		if k := forgeKindOf(t, got); k != want {
			t.Fatalf("writerFor(%q) kind = %q, want %q", repo, k, want)
		}
	}

	// No GitHub client and a GitHub repo: a clean error, never a nil deref.
	noGH := &repoForgeRouter{cfg: cfg, logger: discardLogger()}
	if err := noGH.AddLabels(context.Background(), "acme/app", 1, []string{"x"}); err == nil {
		t.Fatal("want an error for a GitHub repo with no GitHub client")
	}
}

// TestForgeTokenGoesOnlyToItsBlocksHost: a repo_policies forge_url on another
// host than the forge block's own URL must not receive the block's token. The
// fake server stands for that other host; it must see no request.
func TestForgeTokenGoesOnlyToItsBlocksHost(t *testing.T) {
	for _, kind := range []string{config.ForgeGitea, config.ForgeForgejo} {
		for _, matching := range []bool{false, true} {
			name := kind + "/other host"
			if matching {
				name = kind + "/matching host"
			}
			t.Run(name, func(t *testing.T) {
				var mu sync.Mutex
				var auths []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					auths = append(auths, r.Header.Get("Authorization"))
					mu.Unlock()
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{}`))
				}))
				defer srv.Close()

				t.Setenv("HIVE_TEST_HOST_TOKEN", "secret-token")
				blockURL := "https://git.example.com"
				if matching {
					blockURL = srv.URL
				}
				cfg := &config.Config{}
				cfg.Project.Org = "acme"
				cfg.Project.Repos = []string{"app"}
				cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "app", Forge: kind, ForgeURL: srv.URL}}
				if kind == config.ForgeGitea {
					cfg.Gitea.URL, cfg.Gitea.TokenEnv = blockURL, "HIVE_TEST_HOST_TOKEN"
				} else {
					cfg.Forgejo.URL, cfg.Forgejo.TokenEnv = blockURL, "HIVE_TEST_HOST_TOKEN"
				}

				w := governorForge(cfg, nil, discardLogger())
				var err error
				if w == nil {
					err = errors.New("no writer")
				} else {
					err = w.CreateIssueComment(context.Background(), "acme/app", 1, "hi")
				}
				_, mergeErr := forgeMergerForRepo(cfg, "acme/app")

				mu.Lock()
				defer mu.Unlock()
				if matching {
					if err != nil || mergeErr != nil {
						t.Fatalf("matching host: comment err %v, merger err %v", err, mergeErr)
					}
					if len(auths) != 1 || !strings.Contains(auths[0], "secret-token") {
						t.Fatalf("matching host: requests = %q, want one carrying the token", auths)
					}
					return
				}
				if err == nil || mergeErr == nil {
					t.Fatalf("other host: comment err %v, merger err %v, want both errors", err, mergeErr)
				}
				if len(auths) != 0 {
					t.Fatalf("other host received %d requests, want 0", len(auths))
				}
			})
		}
	}
}
