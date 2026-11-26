package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

const forgejoUnsetTokenEnv = "HIVE_TEST_FORGEJO_UNSET_TOKEN"

// TestForgejoRESTBuildersRefuseAnEmptyToken pins hivecommons/hive#19919:
// Forgejo answers a request with no token for a private repo with 404, so
// every Forgejo REST builder refuses an unset token with an error that names
// the env var, instead of building a client whose first call reports "The
// target couldn't be found".
func TestForgejoRESTBuildersRefuseAnEmptyToken(t *testing.T) {
	t.Setenv(forgejoUnsetTokenEnv, "")
	cfg := forgejoOnlyConfig("https://fj.example")
	cfg.Forgejo.TokenEnv = forgejoUnsetTokenEnv

	assertRefusal := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, errForgeTokenEmpty) {
			t.Errorf("%s: err = %v, want errForgeTokenEmpty", name, err)
			return
		}
		if !strings.Contains(err.Error(), forgejoUnsetTokenEnv) {
			t.Errorf("%s: err = %q does not name %s", name, err, forgejoUnsetTokenEnv)
		}
	}

	l, err := forgeListerForRepo(cfg, "widgets")
	if l != nil {
		t.Errorf("lister built with no token: %T", l)
	}
	assertRefusal("forgeListerForRepo", err)

	w, err := forgeRelayWriterForRepo(cfg, "widgets")
	if w != nil {
		t.Errorf("relay writer built with no token: %T", w)
	}
	assertRefusal("forgeRelayWriterForRepo", err)

	m, err := forgeMergerForRepo(cfg, "acme/widgets")
	if m != nil {
		t.Errorf("merger built with no token: %T", m)
	}
	assertRefusal("forgeMergerForRepo", err)

	// The writer builds nothing, and never falls back to the GitHub client: a
	// Forgejo repo's write must not go to GitHub.
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	gh := github.NewClient("t", "acme", nil, discardLogger(), "")
	if got := forgeForRepo(cfg, "acme/widgets", gh, logger); got != nil {
		t.Errorf("forgeForRepo with no token = %T, want nil", got)
	}
	if !strings.Contains(logs.String(), forgejoUnsetTokenEnv) {
		t.Errorf("forgeForRepo log does not name %s:\n%s", forgejoUnsetTokenEnv, logs.String())
	}
	if got := governorForge(cfg, gh, logger); got != nil {
		t.Errorf("governorForge with no token = %T, want nil", got)
	}

	// With the token set, every builder builds.
	t.Setenv(forgejoUnsetTokenEnv, "fj-token")
	if _, err := forgeListerForRepo(cfg, "widgets"); err != nil {
		t.Errorf("forgeListerForRepo with a token: %v", err)
	}
	if _, err := forgeMergerForRepo(cfg, "acme/widgets"); err != nil {
		t.Errorf("forgeMergerForRepo with a token: %v", err)
	}
	if got := forgeKindOf(t, forgeForRepo(cfg, "acme/widgets", gh, discardLogger())); got != forge.KindForgejo {
		t.Errorf("forgeForRepo with a token: kind %q, want forgejo", got)
	}
}

// requireForgeToken refuses only an empty Forgejo token. GitLab and Gitea keep
// passing an empty token through, as before.
func TestRequireForgeToken(t *testing.T) {
	cases := []struct {
		kind    forge.Kind
		token   string
		refused bool
	}{
		{forge.KindForgejo, "", true},
		{forge.KindForgejo, "  \n", true},
		{forge.KindForgejo, "tok", false},
		{forge.KindGitLab, "", false},
		{forge.KindGitea, "", false},
	}
	for _, tc := range cases {
		err := requireForgeToken(tc.kind, "SOME_ENV", tc.token)
		if got := err != nil; got != tc.refused {
			t.Errorf("requireForgeToken(%s, %q) = %v, want refused=%v", tc.kind, tc.token, err, tc.refused)
		}
	}

	t.Setenv(forgejoUnsetTokenEnv, "")
	cfg := &config.Config{}
	cfg.Project.Forge = config.ForgeGitLab
	cfg.GitLab.URL = "https://gl.example"
	cfg.GitLab.TokenEnv = forgejoUnsetTokenEnv
	if _, err := forgeListerForRepo(cfg, "widgets"); err != nil {
		t.Errorf("GitLab lister with an empty token: %v, want it built as before", err)
	}
}
