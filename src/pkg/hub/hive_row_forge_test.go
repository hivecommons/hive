package hub

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// hivecommons/hive#20181: a Forgejo-only spoke showed on the hub as a GitHub
// hive — a github.com pill, a GitHub icon linking to
// https://github.com/<org>/<repo>, and "as: —". The hive row now builds those
// from the spoke-reported forge (forgeKind/forgeBaseUrl/forgeLogin). These
// cases run the real dashboard functions under node.

type hiveRowForgeResult struct {
	Host     string `json:"host"`
	RepoURL  string `json:"repoUrl"`
	Pill     string `json:"pill"`
	Identity string `json:"identity"`
	Icon     string `json:"icon"`
}

// jsVarLine returns the single-line `var NAME = ...;` declaration from the page.
func jsVarLine(t *testing.T, name string) string {
	t.Helper()
	line := regexp.MustCompile(`(?m)^    var ` + regexp.QuoteMeta(name) + ` = .*;$`).FindString(dashboardHTML)
	if line == "" {
		t.Fatalf("var %s not found in the page source", name)
	}
	return line
}

func runHiveRowForgeJS(t *testing.T, hives map[string]string) map[string]hiveRowForgeResult {
	t.Helper()
	var cases []string
	for name, hive := range hives {
		cases = append(cases, "  "+jsString(name)+": "+hive+",")
	}
	src := strings.Join([]string{
		"function esc(s) { return String(s || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }",
		jsFunc(t, "escAttr"),
		jsVarLine(t, "PAST_REQUESTS_DEFAULT_GITHUB_HOST"),
		jsVarLine(t, "GITHUB_REPO_ICON_SVG"),
		jsVarLine(t, "FORGE_KIND_LABELS"),
		jsFunc(t, "ghRepoURL"),
		jsFunc(t, "forgeHostPillHTML"),
		jsFunc(t, "githubHostPill"),
		jsFunc(t, "hiveForgeHost"),
		jsFunc(t, "hiveForgeKind"),
		jsFunc(t, "hiveForgeBaseURL"),
		jsFunc(t, "hiveRepoURL"),
		jsFunc(t, "hiveForgePill"),
		jsFunc(t, "hiveForgeLabel"),
		jsFunc(t, "hiveAuthorIdentity"),
		jsFunc(t, "hiveRepoIconSVG"),
		"var cases = {",
		strings.Join(cases, "\n"),
		"};",
		"var result = {};",
		"Object.keys(cases).forEach(function(k) { var h = cases[k]; result[k] = {host: hiveForgeHost(h), repoUrl: hiveRepoURL(h, h.org, h.primaryRepo), pill: hiveForgePill(h), identity: hiveAuthorIdentity(h), icon: hiveRepoIconSVG(h)}; });",
		"process.stdout.write(JSON.stringify(result));",
	}, "\n")
	stdout, err := exec.Command("node", "-e", src).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node failed: %v\nstderr:\n%s", err, ee.Stderr)
		}
		t.Skipf("node unavailable: %v", err)
	}
	got := map[string]hiveRowForgeResult{}
	if err := json.Unmarshal(stdout, &got); err != nil {
		t.Fatalf("bad JSON from node: %v\ngot: %s", err, stdout)
	}
	return got
}

func TestHiveRowRendersNonGitHubForge(t *testing.T) {
	got := runHiveRowForgeJS(t, map[string]string{
		// The reported hive: Forgejo-only, github_host left at its default.
		"forgejo": `{org: 'marinatedconcrete', primaryRepo: 'sunshine-buffalo', githubHost: 'github.com', aiAuthorEffective: '', forgeKind: 'forgejo', forgeBaseUrl: 'https://code.tswn.us', forgeLogin: 'hive-bot'}`,
		// Forgejo with no login configured keeps "—", never a GitHub identity.
		"forgejoNoLogin": `{org: 'acme', primaryRepo: 'app', githubHost: 'github.com', aiAuthorEffective: 'hive[bot]', forgeKind: 'forgejo', forgeBaseUrl: 'https://code.example.org'}`,
		// Forgejo with no usable base URL: no link at all, not a github.com one.
		"forgejoNoURL": `{org: 'acme', primaryRepo: 'app', githubHost: 'github.com', forgeKind: 'forgejo', forgeBaseUrl: 'javascript:alert(1)'}`,
		"giteaSubpath": `{org: 'acme', primaryRepo: 'app', forgeKind: 'gitea', forgeBaseUrl: 'http://git.example.org:3000/gitea/'}`,
		// GitHub and GHE hives render exactly as before.
		"github":      `{org: 'acme', primaryRepo: 'app', githubHost: 'github.com', aiAuthorEffective: 'hive[bot]', forgeKind: 'github'}`,
		"ghe":         `{org: 'acme', primaryRepo: 'app', githubHost: 'github.ibm.com', aiAuthorEffective: 'hive[bot]'}`,
		"unknownKind": `{org: 'acme', primaryRepo: 'app', githubHost: 'github.com', aiAuthorEffective: 'hive[bot]', forgeKind: 'sourcehut', forgeBaseUrl: 'https://git.sr.ht'}`,
	})

	fj := got["forgejo"]
	if fj.RepoURL != "https://code.tswn.us/marinatedconcrete/sunshine-buffalo" {
		t.Errorf("forgejo repo url = %q", fj.RepoURL)
	}
	if fj.Host != "code.tswn.us" || !strings.Contains(fj.Pill, ">code.tswn.us<") || !strings.Contains(fj.Pill, `title="Forgejo"`) {
		t.Errorf("forgejo host/pill = %q / %q", fj.Host, fj.Pill)
	}
	if strings.Contains(fj.Pill, "github.com") {
		t.Errorf("forgejo pill still names github.com: %q", fj.Pill)
	}
	if fj.Identity != "hive-bot" {
		t.Errorf("forgejo identity = %q, want hive-bot", fj.Identity)
	}
	if strings.Contains(fj.Icon, "M8 0C3.58") {
		t.Error("forgejo row still shows the GitHub mark")
	}

	if id := got["forgejoNoLogin"].Identity; id != "" {
		t.Errorf("forgejo with no login shows identity %q, want empty (rendered —)", id)
	}
	if u := got["forgejoNoURL"].RepoURL; u != "" {
		t.Errorf("forgejo with no base url links to %q, want no link", u)
	}
	if p := got["forgejoNoURL"].Pill; !strings.Contains(p, ">Forgejo<") {
		t.Errorf("forgejo with no base url pill = %q, want the kind", p)
	}
	if u := got["giteaSubpath"].RepoURL; u != "http://git.example.org:3000/gitea/acme/app" {
		t.Errorf("gitea repo url = %q", u)
	}

	for name, wantURL := range map[string]string{
		"github":      "https://github.com/acme/app",
		"ghe":         "https://github.ibm.com/acme/app",
		"unknownKind": "https://github.com/acme/app",
	} {
		r := got[name]
		if r.RepoURL != wantURL {
			t.Errorf("%s repo url = %q, want %q", name, r.RepoURL, wantURL)
		}
		if r.Identity != "hive[bot]" {
			t.Errorf("%s identity = %q, want hive[bot]", name, r.Identity)
		}
		if !strings.Contains(r.Icon, "M8 0C3.58") {
			t.Errorf("%s row lost the GitHub mark", name)
		}
	}
}
