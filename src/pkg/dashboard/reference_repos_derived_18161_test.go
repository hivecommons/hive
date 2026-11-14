package dashboard

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#18161: a derived reference repo carries a link to the
// request issue it came from, built on the hive's GitHub host, so the
// dashboard can show "from owner/repo#N" without guessing the host.
func TestStatusReferenceReposCarryDerivedFromURL(t *testing.T) {
	srv := newFullServer(t)
	cfg := srv.deps.Config
	cfg.Project.ReferenceRepos = []config.ReferenceRepo{
		{Repo: "hivecommons/docs", Write: []string{config.ReferenceWriteIssues}, DerivedFrom: "cncf/techdocs#395", CredentialState: config.ReferenceCredentialWriteTier},
		{Repo: "hivecommons/hive"},
	}
	srv.UpdateStatus(&StatusPayload{ReferenceRepos: buildReferenceRepos(cfg)})

	rec := doGet(srv, "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/status = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ReferenceRepos []map[string]any `json:"referenceRepos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/status: %v", err)
	}
	got := map[string]map[string]any{}
	for _, rr := range body.ReferenceRepos {
		full, _ := rr["full"].(string)
		got[full] = rr
	}
	if u := got["hivecommons/docs"]["derived_from_url"]; u != "https://github.com/cncf/techdocs/issues/395" {
		t.Errorf("derived entry derived_from_url = %v, want the request issue URL", u)
	}
	if _, present := got["hivecommons/hive"]["derived_from_url"]; present {
		t.Errorf("hand-written entry carries derived_from_url: %v", got["hivecommons/hive"])
	}
}

func TestReferenceRepoSourceURL(t *testing.T) {
	cases := []struct {
		from, base, want string
	}{
		{"cncf/techdocs#395", "https://github.com", "https://github.com/cncf/techdocs/issues/395"},
		{"cncf/techdocs#395", "https://ghe.example.com/", "https://ghe.example.com/cncf/techdocs/issues/395"},
		{"", "https://github.com", ""},
		{"cncf/techdocs", "https://github.com", ""},
		{"techdocs#395", "https://github.com", ""},
		{"cncf/techdocs#0", "https://github.com", ""},
		{"cncf/techdocs#x", "https://github.com", ""},
		{"a/b/c#1", "https://github.com", ""},
	}
	for _, c := range cases {
		if got := referenceRepoSourceURL(c.from, c.base); got != c.want {
			t.Errorf("referenceRepoSourceURL(%q, %q) = %q, want %q", c.from, c.base, got, c.want)
		}
	}
}

// hivecommons/hive#18161: the reference repo card renders a derived entry
// read-only with a lock, a "from owner/repo#N" link and its credential chip,
// and renders nothing extra for a hand-written entry.
func TestReferenceRepoCardRendersDerivedEntryReadOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the derived reference repo card was NOT executed by this run")
	}
	html := indexHTML(t)
	script := extractJSFunction(t, html, "esc") + "\n    }\n" +
		extractJSFunction(t, html, "renderReferenceRepoDerived") + "\n    }\n" + `
const assert = require('node:assert/strict');
assert.equal(renderReferenceRepoDerived({full: 'hivecommons/hive', reference_tier: 'read-only'}), '');
const w = renderReferenceRepoDerived({full: 'hivecommons/docs', derived_from: 'cncf/techdocs#395', derived_from_url: 'https://github.com/cncf/techdocs/issues/395', credential_state: 'write-tier'});
assert.match(w, /🔒 from <a class="repo-reference-source" href="https:\/\/github.com\/cncf\/techdocs\/issues\/395"[^>]*>cncf\/techdocs#395<\/a>/);
assert.match(w, /data-credential-state="write-tier"[^>]*>🔑 App installation</);
const r = renderReferenceRepoDerived({full: 'acme/docs', derived_from: 'cncf/techdocs#395', credential_state: 'read-only', credential_reason: 'the hive\'s GitHub App is not installed on acme'});
assert.match(r, /<span class="repo-reference-source">cncf\/techdocs#395<\/span>/);
assert.match(r, /data-credential-state="read-only" title="the hive&#39;s GitHub App is not installed on acme">read-only</);
for (const out of [w, r]) {
  assert.doesNotMatch(out, /<button|<input|data-action=/, 'derived entries must offer no edit or delete control');
}
console.log('ok');
`
	out, err := runNodeStdin(node, script)
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
