package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

// forgeTokenCases are the ways a forge block can name its token env var. The
// default var is read when token_env is unset, and a named var replaces it.
var forgeTokenCases = []struct {
	name       string
	defaultEnv string
	setEnv     func(cfg *config.Config, name string)
	setURL     func(cfg *config.Config, url string)
	kind       string
}{
	{"gitlab", "GITLAB_TOKEN", func(c *config.Config, n string) { c.GitLab.TokenEnv = n }, func(c *config.Config, u string) { c.GitLab.URL = u }, config.ForgeGitLab},
	{"gitea", "GITEA_TOKEN", func(c *config.Config, n string) { c.Gitea.TokenEnv = n }, func(c *config.Config, u string) { c.Gitea.URL = u }, config.ForgeGitea},
	{"forgejo", "FORGEJO_TOKEN", func(c *config.Config, n string) { c.Forgejo.TokenEnv = n }, func(c *config.Config, u string) { c.Forgejo.URL = u }, config.ForgeForgejo},
}

// TestForgeTokenResolutionPin pins which token the governor writer and the
// merger send for each forge block: the default env var, a token_env override
// that ignores the default, and an unset var, which sends an empty token and
// still builds the writer. A token_env that is only whitespace, or that has
// surrounding spaces, is used verbatim, so it names no set var and sends an
// empty token.
func TestForgeTokenResolutionPin(t *testing.T) {
	for _, fc := range forgeTokenCases {
		for _, scenario := range forgeTokenScenarios {
			t.Run(fc.name+"/"+scenario.name, func(t *testing.T) {
				var mu sync.Mutex
				var auths []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					auths = append(auths, r.Header.Get("Authorization")+"|"+r.Header.Get("PRIVATE-TOKEN"))
					mu.Unlock()
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{}`))
				}))
				defer srv.Close()

				t.Setenv(fc.defaultEnv, "default-secret")
				t.Setenv("HIVE_PIN_OVERRIDE", "override-secret")
				cfg := &config.Config{}
				cfg.Project.Org = "acme"
				cfg.Project.Forge = fc.kind
				fc.setURL(cfg, srv.URL)
				if scenario.override != "" {
					fc.setEnv(cfg, scenario.override)
				}
				// Forgejo answers a private repo with no token with a 404, so an
				// empty Forgejo token builds no merger at all (#19919).
				if fc.kind == config.ForgeForgejo && scenario.wantToken == "" {
					if _, err := forgeMergerForRepo(cfg, "acme/app"); !errors.Is(err, errForgeTokenEmpty) {
						t.Fatalf("merger err = %v, want errForgeTokenEmpty", err)
					}
					return
				}

				w := governorForge(cfg, nil, discardLogger())
				if w == nil {
					t.Fatal("no writer built")
				}
				if err := w.CreateIssueComment(context.Background(), "acme/app", 1, "hi"); err != nil {
					t.Fatalf("comment: %v", err)
				}
				if _, err := forgeMergerForRepo(cfg, "acme/app"); err != nil {
					t.Fatalf("merger: %v", err)
				}

				mu.Lock()
				defer mu.Unlock()
				if len(auths) != 1 {
					t.Fatalf("got %d requests, want 1", len(auths))
				}
				got := strings.Contains(auths[0], scenario.wantToken)
				if scenario.wantToken == "" {
					got = !strings.Contains(auths[0], "default-secret") && !strings.Contains(auths[0], "override-secret")
				} else if scenario.wantToken == "default-secret" {
					got = got && !strings.Contains(auths[0], "override-secret")
				} else {
					got = got && !strings.Contains(auths[0], "default-secret")
				}
				if !got {
					t.Fatalf("request credentials do not match want %q", scenario.wantToken)
				}
			})
		}
	}
}

// TestForgeTokenEnvRegistryPin pins which env var names the agent-session strip
// registry learns from the forge blocks: the defaults when token_env is unset,
// and the configured name in addition when it is set.
func TestForgeTokenEnvRegistryPin(t *testing.T) {
	syncForgeCredentialEnv(&config.Config{})
	got := cruxconfig.ExtraCredentialEnvVars()
	for _, want := range []string{"GITLAB_TOKEN", "GITEA_TOKEN", "FORGEJO_TOKEN"} {
		if !slices.Contains(got, want) {
			t.Errorf("registry %v is missing default %s", got, want)
		}
	}

	cfg := &config.Config{}
	cfg.GitLab.TokenEnv, cfg.Gitea.TokenEnv, cfg.Forgejo.TokenEnv = "PIN_GL", "PIN_GT", "PIN_FJ"
	syncForgeCredentialEnv(cfg)
	got = cruxconfig.ExtraCredentialEnvVars()
	for _, want := range []string{"PIN_GL", "PIN_GT", "PIN_FJ"} {
		if !slices.Contains(got, want) {
			t.Errorf("registry %v is missing configured %s", got, want)
		}
	}
}

// TestCustomersProfileFillTokenPin pins what profile fill hands the adapter
// builder: the token from the default or named env var, an empty token when the
// var is unset, and a built source either way.
func TestCustomersProfileFillTokenPin(t *testing.T) {
	gh := github.NewClient("t", "acme", nil, discardLogger(), "")
	cases := []struct {
		name     string
		forge    string
		defEnv   string
		setEnv   func(*config.Config, string)
		override string
		want     string
	}{
		{"gitlab default", config.ForgeGitLab, "GITLAB_TOKEN", func(c *config.Config, n string) { c.GitLab.TokenEnv = n }, "", "default-secret"},
		{"gitlab override", config.ForgeGitLab, "GITLAB_TOKEN", func(c *config.Config, n string) { c.GitLab.TokenEnv = n }, "HIVE_PIN_OVERRIDE", "override-secret"},
		{"gitlab unset", config.ForgeGitLab, "GITLAB_TOKEN", func(c *config.Config, n string) { c.GitLab.TokenEnv = n }, "HIVE_PIN_UNSET", ""},
		{"gitea default", config.ForgeGitea, "GITEA_TOKEN", func(c *config.Config, n string) { c.Gitea.TokenEnv = n }, "", "default-secret"},
		{"gitea override", config.ForgeGitea, "GITEA_TOKEN", func(c *config.Config, n string) { c.Gitea.TokenEnv = n }, "HIVE_PIN_OVERRIDE", "override-secret"},
		{"gitea unset", config.ForgeGitea, "GITEA_TOKEN", func(c *config.Config, n string) { c.Gitea.TokenEnv = n }, "HIVE_PIN_UNSET", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.defEnv, "default-secret")
			t.Setenv("HIVE_PIN_OVERRIDE", "override-secret")
			cfg := &config.Config{}
			cfg.Project.Forge = tc.forge
			cfg.GitLab.URL = "https://gitlab.example.com"
			cfg.Gitea.URL = "https://gitea.example.com"
			if tc.override != "" {
				tc.setEnv(cfg, tc.override)
			}
			var gotToken string
			var built int
			deps := customersProfileFillDeps{
				cfg:    func() *config.Config { return cfg },
				client: func() *github.Client { return gh },
				orgs:   newCustomersOrgNames(),
				newForge: func(kind forge.Kind, token string, opts forge.Options) (forge.Forge, error) {
					gotToken = token
					built++
					return forge.NewForge(kind, token, opts)
				},
			}
			if got := customersProfileSources(deps); len(got) != 1 {
				t.Fatalf("sources = %d, want 1", len(got))
			}
			if built != 1 || gotToken != tc.want {
				t.Fatalf("adapter built %d times with token %q, want once with %q", built, gotToken, tc.want)
			}
		})
	}
}

// forgeTokenScenarios are the ways a block's token_env can resolve. Padded and
// whitespace-only names are used verbatim, so they name no set var.
var forgeTokenScenarios = []struct {
	name      string
	override  string
	wantToken string
}{
	{"default env", "", "default-secret"},
	{"token_env override", "HIVE_PIN_OVERRIDE", "override-secret"},
	{"named env unset", "HIVE_PIN_UNSET", ""},
	{"whitespace-only name", "   ", ""},
	{"name with surrounding spaces", " HIVE_PIN_OVERRIDE ", ""},
}

// TestForgeListerTokenPin pins the token the work-discovery reader sends for
// each forge block: the default env var, a token_env override, and an unset or
// padded name, which sends no token.
func TestForgeListerTokenPin(t *testing.T) {
	for _, fc := range forgeTokenCases {
		for _, scenario := range forgeTokenScenarios {
			t.Run(fc.name+"/"+scenario.name, func(t *testing.T) {
				var mu sync.Mutex
				var auths []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					auths = append(auths, r.Header.Get("Authorization")+"|"+r.Header.Get("PRIVATE-TOKEN"))
					mu.Unlock()
					_, _ = w.Write([]byte(`[]`))
				}))
				defer srv.Close()

				t.Setenv(fc.defaultEnv, "default-secret")
				t.Setenv("HIVE_PIN_OVERRIDE", "override-secret")
				cfg := &config.Config{}
				cfg.Project.Org = "acme"
				cfg.Project.Forge = fc.kind
				fc.setURL(cfg, srv.URL)
				if scenario.override != "" {
					fc.setEnv(cfg, scenario.override)
				}
				// An empty Forgejo token builds no reader (#19919).
				if fc.kind == config.ForgeForgejo && scenario.wantToken == "" {
					if _, err := forgeListerForRepo(cfg, "acme/app"); !errors.Is(err, errForgeTokenEmpty) {
						t.Fatalf("lister err = %v, want errForgeTokenEmpty", err)
					}
					return
				}

				l, err := forgeListerForRepo(cfg, "acme/app")
				if err != nil {
					t.Fatalf("lister: %v", err)
				}
				_, _ = l.ListOpenIssues(context.Background(), "acme/app")

				mu.Lock()
				defer mu.Unlock()
				if len(auths) == 0 {
					t.Fatal("no request reached the forge")
				}
				for _, a := range auths {
					sent := strings.Contains(a, "default-secret") || strings.Contains(a, "override-secret")
					switch scenario.wantToken {
					case "":
						if sent {
							t.Fatalf("a token was sent, want none")
						}
					default:
						if !strings.Contains(a, scenario.wantToken) {
							t.Fatalf("request credentials do not carry %q", scenario.wantToken)
						}
						other := "override-secret"
						if scenario.wantToken == other {
							other = "default-secret"
						}
						if strings.Contains(a, other) {
							t.Fatalf("request credentials carry the wrong token")
						}
					}
				}
			})
		}
	}
}

// TestForgejoSweepTokenPin pins the token the Forgejo sweeps send and when they
// refuse to build a client: the default env var, a token_env override, and an
// unset, blank, whitespace-only-valued or padded-name var, each refused with an
// error that names the env var.
func TestForgejoSweepTokenPin(t *testing.T) {
	scenarios := []struct {
		name     string
		override string
		value    string
		want     string
		wantErr  string
	}{
		{"default env", "", "", "default-secret", ""},
		{"token_env override", "HIVE_PIN_OVERRIDE", "", "override-secret", ""},
		{"named env unset", "HIVE_PIN_UNSET", "", "", "HIVE_PIN_UNSET"},
		{"default env blank", "", "   ", "", "FORGEJO_TOKEN"},
		{"whitespace-only name", "   ", "", "", "   "},
		{"name with surrounding spaces", " HIVE_PIN_OVERRIDE ", "", "", " HIVE_PIN_OVERRIDE "},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Setenv("FORGEJO_TOKEN", "default-secret")
			if sc.value != "" {
				t.Setenv("FORGEJO_TOKEN", sc.value)
			}
			t.Setenv("HIVE_PIN_OVERRIDE", "override-secret")
			var tokens []string
			prev := newForgejoSweepAPI
			newForgejoSweepAPI = func(token string, _ forge.Options) (forge.SweepAPI, error) {
				tokens = append(tokens, token)
				return nil, nil
			}
			t.Cleanup(func() { newForgejoSweepAPI = prev })

			cfg := &config.Config{}
			cfg.Project.Org = "acme"
			cfg.Project.Forge = config.ForgeForgejo
			cfg.Forgejo.URL = "https://forgejo.example.com"
			cfg.Forgejo.TokenEnv = sc.override

			_, err := forgejoSweepAPIForRepo(cfg, "acme/app")
			if sc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), sc.wantErr+" is empty") {
					t.Fatalf("err = %v, want %q is empty", err, sc.wantErr)
				}
				if len(tokens) != 0 {
					t.Fatalf("a client was built with %q", tokens)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(tokens) != 1 || tokens[0] != sc.want {
				t.Fatalf("client built with %q, want once with %q", tokens, sc.want)
			}
		})
	}
}

// TestForgejoRemoteMinterTokenPin pins the token git clone and push use on a
// Forgejo repo when forgejo.integration is unset: the default env var or a
// token_env override, and a mint error, never an empty token, for an unset,
// whitespace-only-valued or padded name.
func TestForgejoRemoteMinterTokenPin(t *testing.T) {
	scenarios := []struct {
		name     string
		override string
		value    string
		want     string
		wantErr  bool
	}{
		{"default env", "", "", "default-secret", false},
		{"token_env override", "HIVE_PIN_OVERRIDE", "", "override-secret", false},
		{"padded token value", "", "  default-secret \n", "default-secret", false},
		{"named env unset", "HIVE_PIN_UNSET", "", "", true},
		{"default env blank", "", "   ", "", true},
		{"whitespace-only name", "   ", "", "", true},
		{"name with surrounding spaces", " HIVE_PIN_OVERRIDE ", "", "", true},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Setenv("FORGEJO_TOKEN", "default-secret")
			if sc.value != "" {
				t.Setenv("FORGEJO_TOKEN", sc.value)
			}
			t.Setenv("HIVE_PIN_OVERRIDE", "override-secret")
			cfg := &config.Config{}
			cfg.Project.Org = "acme"
			cfg.Project.Forge = config.ForgeForgejo
			cfg.Forgejo.URL = "https://forgejo.example.com"
			cfg.Forgejo.TokenEnv = sc.override

			remote, ok, err := forgeRemoteResolver(func() *config.Config { return cfg })("acme/app")
			if err != nil || !ok {
				t.Fatalf("resolver: ok=%v err=%v", ok, err)
			}
			got, err := remote.Minter.MintPushToken(context.Background(), "acme/app")
			if sc.wantErr {
				if err == nil || got != "" {
					t.Fatalf("token=%q err=%v, want a mint error and no token", got, err)
				}
				return
			}
			if err != nil || got != sc.want {
				t.Fatalf("token=%q err=%v, want %q", got, err, sc.want)
			}
		})
	}
}
