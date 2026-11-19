package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func referenceRepoTestConfig(t *testing.T, refs string) *Config {
	t.Helper()
	src := `
project:
  org: cncf
  repos: [techdocs]
  reference_repos:
` + refs + `
agents:
  scanner:
    backend: claude
`
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &c
}

func TestReferenceReposParseScalarAndMapping(t *testing.T) {
	c := referenceRepoTestConfig(t, `
    - hivecommons/docs
    - repo: hivecommons/hivecommons.github.io
      write: [issues, prs]
    - repo: hivecommons/hive
      write: [issues]
      write_token_file: /data/ref-token
`)
	if err := c.validateReferenceRepos(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := len(c.Project.ReferenceRepos); got != 3 {
		t.Fatalf("want 3 entries, got %d", got)
	}
	ro, ok := c.ReferenceRepoFor("HiveCommons/Docs")
	if !ok || !ro.ReadOnly() || ro.WriteLabel() != "read-only" {
		t.Fatalf("docs: ok=%v entry=%+v", ok, ro)
	}
	rw, ok := c.ReferenceRepoFor("hivecommons/hivecommons.github.io")
	if !ok || !rw.WritesIssues() || !rw.WritesPRs() || rw.WriteLabel() != "issues+prs" {
		t.Fatalf("site: ok=%v entry=%+v", ok, rw)
	}
	tok, _ := c.ReferenceRepoFor("hivecommons/hive")
	if tok.WriteTokenFile != "/data/ref-token" || tok.WriteLabel() != "issues" || tok.Owner() != "hivecommons" || tok.Name() != "hive" {
		t.Fatalf("hive: %+v", tok)
	}
	if c.IsReferenceRepo("cncf/techdocs") {
		t.Fatal("authorized repo must not read as reference")
	}
	if got := len(c.ReferenceReposWithWrite()); got != 2 {
		t.Fatalf("want 2 write entries, got %d", got)
	}
	// Round-trip: the scalar entry stays a scalar.
	out, err := yaml.Marshal(c.Project.ReferenceRepos)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "- hivecommons/docs\n") {
		t.Fatalf("read-only entry should marshal as a scalar:\n%s", out)
	}
}

func TestReferenceReposValidationRejects(t *testing.T) {
	cases := []struct {
		name, refs, want string
	}{
		{"bare name", "    - docs\n", "explicit owner/name"},
		{"url", "    - https://github.com/hivecommons/docs\n", "explicit owner/name"},
		{"overlap with project.repos", "    - cncf/techdocs\n", "already in project.repos"},
		{"overlap case-insensitive", "    - CNCF/TechDocs\n", "already in project.repos"},
		{"duplicate", "    - hivecommons/docs\n    - HiveCommons/docs\n", "listed twice"},
		{"unknown write", "    - repo: hivecommons/docs\n      write: [merge]\n", "not one of"},
		{"token without write", "    - repo: hivecommons/docs\n      write_token_file: /x\n", "write is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := referenceRepoTestConfig(t, tc.refs)
			err := c.validateReferenceRepos()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestReferenceReposValidationRunsFromValidate(t *testing.T) {
	c := referenceRepoTestConfig(t, "    - docs\n")
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "reference_repos") {
		t.Fatalf("Validate should surface the reference_repos error, got %v", err)
	}
}

func TestReferenceRepoTierAccessor(t *testing.T) {
	c := referenceRepoTestConfig(t, `
    - hivecommons/docs
    - repo: hivecommons/hive
      write: [issues, prs]
    - repo: hivecommons/site
      write: [prs]
`)
	tier, ok := c.Project.ReferenceRepoTier("HiveCommons/Docs")
	if !ok || tier != ReferenceTierReadOnly || tier.String() != "read-only" {
		t.Fatalf("docs: tier=%v ok=%v", tier, ok)
	}
	tier, ok = c.Project.ReferenceRepoTier("hivecommons/hive")
	if !ok || !tier.Issues || !tier.PRs || tier.String() != "issues+prs" {
		t.Fatalf("hive: tier=%v ok=%v", tier, ok)
	}
	tier, _ = c.Project.ReferenceRepoTier("hivecommons/site")
	if tier.Issues || !tier.PRs || tier.String() != "prs" {
		t.Fatalf("site: tier=%v", tier)
	}
	if _, ok := c.Project.ReferenceRepoTier("hivecommons/nope"); ok {
		t.Fatal("unknown slug must report ok=false")
	}
	if _, ok := c.Project.ReferenceRepoTier("cncf/techdocs"); ok {
		t.Fatal("authorized repo must not report a reference tier")
	}
}

func TestReferenceRepoTierEmptyAndNilGuards(t *testing.T) {
	c := referenceRepoTestConfig(t, "    - hivecommons/docs\n")
	if tier, ok := c.Project.ReferenceRepoTier("  "); ok || tier != ReferenceTierReadOnly {
		t.Fatalf("blank slug: tier=%v ok=%v", tier, ok)
	}
	var nilCfg *Config
	if _, ok := nilCfg.ReferenceRepoFor("hivecommons/docs"); ok {
		t.Fatal("nil config must not match")
	}
	if _, ok := c.ReferenceRepoFor(""); ok {
		t.Fatal("blank slug must not match")
	}
	if got := nilCfg.ReferenceReposWithWrite(); got != nil {
		t.Fatalf("nil config: want nil, got %v", got)
	}
	if got := nilCfg.ReferenceRepoSlugs(); got != nil {
		t.Fatalf("nil config: want nil, got %v", got)
	}
	if got := c.ReferenceRepoSlugs(); len(got) != 1 || got[0] != "hivecommons/docs" {
		t.Fatalf("slugs: %v", got)
	}
}

func TestReferenceRepoDerivedFromNeverSerializesToYAML(t *testing.T) {
	entries := []ReferenceRepo{
		{Repo: "hivecommons/docs", Write: []string{ReferenceWriteIssues, ReferenceWritePRs}, DerivedFrom: "cncf/techdocs#395"},
		{Repo: "hivecommons/hive", DerivedFrom: "cncf/techdocs#395"},
	}
	if !entries[0].Derived() || (ReferenceRepo{Repo: "x/y"}).Derived() {
		t.Fatal("Derived must follow DerivedFrom")
	}
	out, err := yaml.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "395") || strings.Contains(string(out), "derived") {
		t.Fatalf("derived provenance leaked into YAML:\n%s", out)
	}
	var back []ReferenceRepo
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	for _, r := range back {
		if r.Derived() {
			t.Fatalf("round-tripped entry must not read as derived: %+v", r)
		}
	}
	js, err := json.Marshal(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), `"derived_from":"cncf/techdocs#395"`) {
		t.Fatalf("JSON must carry derived_from for the dashboard:\n%s", js)
	}
}

// hivecommons/hive#18158: the resolved credential state is in-memory only,
// like the provenance it belongs to; JSON carries it for the dashboard.
func TestReferenceRepoCredentialStateNeverSerializesToYAML(t *testing.T) {
	rr := ReferenceRepo{Repo: "acme/docs", DerivedFrom: "cncf/techdocs#395", CredentialState: ReferenceCredentialReadOnly, CredentialReason: "the hive's GitHub App is not installed on acme"}
	if !rr.DerivedReadOnly() {
		t.Fatal("read-only derived entry must read as DerivedReadOnly")
	}
	if (ReferenceRepo{Repo: "acme/docs", CredentialState: ReferenceCredentialReadOnly}).DerivedReadOnly() {
		t.Fatal("a hand-written entry is never DerivedReadOnly")
	}
	if (ReferenceRepo{Repo: "acme/docs", DerivedFrom: "x/y#1", CredentialState: ReferenceCredentialWriteTier}).DerivedReadOnly() {
		t.Fatal("a write-tier entry is not DerivedReadOnly")
	}
	for _, entry := range []ReferenceRepo{rr, {Repo: "acme/docs", Write: []string{ReferenceWriteIssues}, DerivedFrom: "cncf/techdocs#395", CredentialState: ReferenceCredentialWriteTier}} {
		out, err := yaml.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "credential") || strings.Contains(string(out), "installed") {
			t.Fatalf("credential state leaked into YAML:\n%s", out)
		}
	}
	js, err := json.Marshal(rr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), `"credential_state":"read-only"`) || !strings.Contains(string(js), `"credential_reason":`) {
		t.Fatalf("JSON must carry the credential state:\n%s", js)
	}
}

func TestReferenceRepoDerivedFromIssue(t *testing.T) {
	cases := []struct {
		from   string
		repo   string
		number int
		ok     bool
	}{
		{"cncf/techdocs#395", "cncf/techdocs", 395, true},
		{" cncf/techdocs#7 ", "cncf/techdocs", 7, true},
		{"", "", 0, false},
		{"cncf/techdocs", "", 0, false},
		{"techdocs#395", "", 0, false},
		{"cncf/techdocs#0", "", 0, false},
		{"cncf/techdocs#abc", "", 0, false},
	}
	for _, tc := range cases {
		repo, number, ok := (ReferenceRepo{DerivedFrom: tc.from}).DerivedFromIssue()
		if repo != tc.repo || number != tc.number || ok != tc.ok {
			t.Errorf("DerivedFromIssue(%q) = %q, %d, %v; want %q, %d, %v", tc.from, repo, number, ok, tc.repo, tc.number, tc.ok)
		}
	}
}

// hivecommons/hive#18159: every save path (runtime config and dashboard
// overlay) marshals redactedForPersist, which must drop derived entries
// outright: yaml:"-" on the provenance alone would still write the repo.
func TestRedactedForPersistDropsDerivedReferenceRepos(t *testing.T) {
	cfg := referenceRepoTestConfig(t, `    - hand/written`)
	cfg.Project.ReferenceRepos = append(cfg.Project.ReferenceRepos,
		ReferenceRepo{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395", CredentialState: ReferenceCredentialWriteTier},
		ReferenceRepo{Repo: "zeta/site", DerivedFrom: "cncf/techdocs#395", CredentialState: ReferenceCredentialReadOnly, CredentialReason: "not installed"},
	)
	out, err := yaml.Marshal(cfg.redactedForPersist())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "hand/written") {
		t.Errorf("hand-written entry must persist:\n%s", s)
	}
	for _, leak := range []string{"acme/docs", "zeta/site", "techdocs#395", "derived_from", "credential"} {
		if strings.Contains(s, leak) {
			t.Errorf("persisted config leaks derived state %q:\n%s", leak, s)
		}
	}
	if len(cfg.Project.ReferenceRepos) != 3 {
		t.Errorf("redaction must copy, not mutate the live list: %+v", cfg.Project.ReferenceRepos)
	}
	if got := HandWrittenReferenceRepos(nil); got != nil {
		t.Errorf("nil in must be nil out, got %v", got)
	}
	if got := HandWrittenReferenceRepos([]ReferenceRepo{{Repo: "a/b", DerivedFrom: "o/r#1"}}); got != nil {
		t.Errorf("all-derived must be nil, got %v", got)
	}
}
