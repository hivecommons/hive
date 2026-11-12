package config

import (
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
