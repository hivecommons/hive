package skillreg

import (
	"strings"
	"testing"
)

// legacySpec is an AgentSpec written before repo scope existed: it implements
// the five original accessors and knows nothing about RepoScoped. It must keep
// compiling and keep meaning "hive-wide" — that is the entire reason scope is
// an optional interface rather than a sixth method on AgentSpec.
type legacySpec struct{}

func (legacySpec) AgentName() string       { return "legacy" }
func (legacySpec) Backend() string         { return "claude-code" }
func (legacySpec) Model() string           { return "sonnet" }
func (legacySpec) Mode() AgentMode         { return ModeSuggest }
func (legacySpec) DefaultSkills() []string { return nil }

var _ AgentSpec = legacySpec{}

func TestSpecRepos_LegacySpecIsUnscoped(t *testing.T) {
	if got := SpecRepos(legacySpec{}); got != nil {
		t.Errorf("SpecRepos(legacy) = %v, want nil — a spec that predates the scope is hive-wide", got)
	}
	for _, repo := range []string{"console", "acme/console", "anything"} {
		if !SpecServesRepo(legacySpec{}, repo) {
			t.Errorf("legacy spec refused %q; an unscoped spec serves every repo", repo)
		}
	}
}

func TestParseAgentSpec_ReposOptional(t *testing.T) {
	spec, err := ParseAgentSpec([]byte("name: reviewer\nbackend: claude-code\nmodel: sonnet\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spec.RepoScope != nil || SpecRepos(spec) != nil {
		t.Errorf("a spec with no repos key must be unscoped, got %v", spec.RepoScope)
	}
	if !SpecServesRepo(spec, "anything") {
		t.Error("unscoped spec did not serve an arbitrary repo")
	}
}

func TestParseAgentSpec_ReposParsedAndTrimmed(t *testing.T) {
	spec, err := ParseAgentSpec([]byte(`
name: schema-reviewer
backend: claude-code
model: opus
repos:
  - "  console  "
  - laredo/cuga-agent
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := SpecRepos(spec)
	if len(got) != 2 || got[0] != "console" || got[1] != "laredo/cuga-agent" {
		t.Fatalf("SpecRepos = %v, want [console laredo/cuga-agent]", got)
	}
	for _, in := range []string{"console", "Console", "acme/console", "laredo/cuga-agent", "cuga-agent"} {
		if !SpecServesRepo(spec, in) {
			t.Errorf("spec did not serve %q", in)
		}
	}
	if SpecServesRepo(spec, "dashboard") {
		t.Error("spec served a repo outside its scope")
	}
	if SpecServesRepo(spec, "") {
		t.Error("spec served an unnamed repo despite being scoped")
	}
}

// A `repos:` key that normalizes empty is a typo, not a scope. Accepting it
// would silently widen the agent to the whole hive — the opposite of what
// writing the key meant — so the contract rejects it with the rest of the spec.
func TestParseAgentSpec_RejectsReposThatNameNothing(t *testing.T) {
	_, err := ParseAgentSpec([]byte("name: r\nbackend: b\nmodel: m\nrepos: [\"\", \"   \"]\n"))
	if err == nil {
		t.Fatal("expected an error for a repos list that names nothing")
	}
	if !strings.Contains(err.Error(), "names none") {
		t.Errorf("error = %q, want it to say the scope names no repository", err)
	}
}

func TestNormalizeSpecRepos(t *testing.T) {
	if got := NormalizeSpecRepos(nil); got != nil {
		t.Errorf("NormalizeSpecRepos(nil) = %v, want nil", got)
	}
	if got := NormalizeSpecRepos([]string{" ", ""}); got != nil {
		t.Errorf("NormalizeSpecRepos(blanks) = %v, want nil", got)
	}
	got := NormalizeSpecRepos([]string{" a ", "", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("NormalizeSpecRepos = %v, want [a b]", got)
	}
}

// SpecData must satisfy BOTH halves of the contract, and the YAML round-trip
// must keep the scope: this is the disk format a third party ships.
func TestSpecDataImplementsBothContracts(t *testing.T) {
	var spec AgentSpec = &SpecData{Name: "n", BackendID: "b", ModelID: "m", RepoScope: []string{"console"}}
	scoped, ok := spec.(RepoScoped)
	if !ok {
		t.Fatal("SpecData does not implement RepoScoped")
	}
	if got := scoped.Repos(); len(got) != 1 || got[0] != "console" {
		t.Errorf("Repos() = %v, want [console]", got)
	}
}
