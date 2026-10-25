package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// Every repo card carries its forge, so a GitLab or Gitea/Forgejo hive's
// Projects cards stop claiming github.com (hivecommons/hive#14115).
func TestBuildReposForge14115(t *testing.T) {
	for _, tc := range []struct {
		name     string
		set      func(*config.Config)
		wantKind string
		wantURL  string
	}{
		{"unset defaults to github", func(*config.Config) {}, "github", ""},
		{"github reports no url", func(c *config.Config) { c.Project.Forge = "github" }, "github", ""},
		{"gitlab defaults to gitlab.com", func(c *config.Config) { c.Project.Forge = "gitlab" }, "gitlab", "https://gitlab.com"},
		{"gitlab self-managed", func(c *config.Config) {
			c.Project.Forge = "gitlab"
			c.GitLab.URL = "https://gitlab.example.com/"
		}, "gitlab", "https://gitlab.example.com"},
		{"gitea", func(c *config.Config) {
			c.Project.Forge = "gitea"
			c.Gitea.URL = "https://codeberg.org"
		}, "gitea", "https://codeberg.org"},
		{"gitea without url", func(c *config.Config) { c.Project.Forge = "gitea" }, "gitea", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Project.Org = "acme"
			cfg.Project.Repos = []string{"one", "other/two"}
			tc.set(cfg)
			repos := buildRepos(cfg, nil, governor.State{})
			if len(repos) != 2 {
				t.Fatalf("want 2 repo cards, got %d", len(repos))
			}
			for _, r := range repos {
				if r.Forge != tc.wantKind || r.ForgeURL != tc.wantURL {
					t.Errorf("%s: forge = %q %q, want %q %q", r.Full, r.Forge, r.ForgeURL, tc.wantKind, tc.wantURL)
				}
			}
		})
	}
}

func TestStatusRepoForge14115(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"one"}
	cfg.Project.Forge = "gitlab"
	cfg.GitLab.URL = "https://gitlab.example.com"

	s := newTestServer()
	s.status = &StatusPayload{Repos: buildRepos(cfg, nil, governor.State{})}
	for _, query := range []string{"", "?fields=repos"} {
		rr := httptest.NewRecorder()
		s.handleStatus(rr, httptest.NewRequest("GET", "/api/status"+query, nil))
		var response struct {
			Repos []map[string]any `json:"repos"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatalf("%s: %v\n%s", query, err, rr.Body.String())
		}
		if len(response.Repos) != 1 {
			t.Fatalf("%s: want 1 repo, got %d", query, len(response.Repos))
		}
		if got := response.Repos[0]; got["forge"] != "gitlab" || got["forgeUrl"] != "https://gitlab.example.com" {
			t.Errorf("%s: repo = forge %v forgeUrl %v, want gitlab https://gitlab.example.com", query, got["forge"], got["forgeUrl"])
		}
	}

	// A GitHub card always names its forge but omits the URL: the host comes
	// from /api/config as before.
	raw, err := json.Marshal(buildRepos(&config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"one"}}}, nil, governor.State{})[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"forge":"github"`) || strings.Contains(string(raw), "forgeUrl") {
		t.Errorf("github card JSON = %s, want forge github and no forgeUrl", raw)
	}
}

// The card reads the forge back: its links and label follow the forge, and a
// card with no forge (an older hive) keeps the GitHub host.
func TestRepoCardForgeWeb14115(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"const cardForge = repoForgeWeb(r, hostLabel);",
		"const repoUrl = cardForge.base + '/' + (r.full || r.name);",
		`<a href="${esc(repoUrl + cardForge.issuesPath)}"`,
		`<a href="${esc(repoUrl + cardForge.changesPath)}"`,
		`data-forge="${esc(cardForge.kind)}"`,
		"const hiveForge = repoForgeWeb(repos[0], hostLabel);",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := "const assert = require('node:assert/strict');\nconst window = {_githubBaseUrl: 'https://ghe.example.com'};\n" +
		jsFunc(t, html, "repoForgeWeb") + `
const pick = f => ({kind: f.kind, name: f.name, host: f.host, base: f.base, issues: f.issuesPath, changes: f.changesPath});
assert.deepEqual(pick(repoForgeWeb({forge: 'gitlab', forgeUrl: 'https://gitlab.example.com/'}, 'ghe.example.com')),
  {kind: 'gitlab', name: 'GitLab', host: 'gitlab.example.com', base: 'https://gitlab.example.com', issues: '/-/issues', changes: '/-/merge_requests'});
assert.deepEqual(pick(repoForgeWeb({forge: 'gitea', forgeUrl: 'https://codeberg.org'}, 'ghe.example.com')),
  {kind: 'gitea', name: 'Gitea/Forgejo', host: 'codeberg.org', base: 'https://codeberg.org', issues: '/issues', changes: '/pulls'});
for (const r of [undefined, {}, {forge: 'github'}, {forge: 'gitea'}]) {
  assert.deepEqual(pick(repoForgeWeb(r, 'ghe.example.com')),
    {kind: 'github', name: 'GitHub', host: 'ghe.example.com', base: 'https://ghe.example.com', issues: '/issues', changes: '/pulls'});
}
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("repoForgeWeb: %v\n%s", err, out)
	}
}
