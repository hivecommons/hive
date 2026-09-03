package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/skillreg"
)

// useSkillsDir points the kick-time registry loader at a temp dir for the
// duration of a test and returns that dir.
func useSkillsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := skillsRegistryDir
	t.Cleanup(func() { skillsRegistryDir = orig })
	skillsRegistryDir = dir
	return dir
}

// writeSkill drops a skill file into dir.
func writeSkill(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// schedulerWithSkills builds a Scheduler whose named agent declares skills.
func schedulerWithSkills(agent string, skills []string) *Scheduler {
	return New(&config.Config{
		Agents: map[string]config.AgentConfig{
			agent: {Skills: skills},
		},
	}, slog.Default())
}

func TestPrimeSkills_NoDeclaredSkillsIsNoOp(t *testing.T) {
	dir := useSkillsDir(t)
	writeSkill(t, dir, "go-testing.md", "Prefer t.TempDir.")

	s := schedulerWithSkills("scanner", nil)
	if got := s.primeSkills("scanner", ""); got != "" {
		t.Errorf("primeSkills with no declared skills = %q, want empty", got)
	}
}

func TestPrimeSkills_UnknownAgentIsNoOp(t *testing.T) {
	useSkillsDir(t)
	s := schedulerWithSkills("scanner", []string{"go-testing"})
	if got := s.primeSkills("nobody", ""); got != "" {
		t.Errorf("primeSkills for an unconfigured agent = %q, want empty", got)
	}
}

func TestPrimeSkills_MissingRegistryDirIsNoOp(t *testing.T) {
	orig := skillsRegistryDir
	t.Cleanup(func() { skillsRegistryDir = orig })
	skillsRegistryDir = filepath.Join(t.TempDir(), "does-not-exist")

	s := schedulerWithSkills("scanner", []string{"go-testing"})
	if got := s.primeSkills("scanner", ""); got != "" {
		t.Errorf("primeSkills with no registry dir = %q, want empty", got)
	}
}

// The whole point of the wiring: a declared skill's body must actually reach
// the injected text. A no-op here means the feature is dead again.
func TestPrimeSkills_InjectsDeclaredSkillBody(t *testing.T) {
	dir := useSkillsDir(t)
	const body = "Never hit the network in unit tests."
	writeSkill(t, dir, "go-testing.md", "---\nname: go-testing\nversion: 1.2.0\n---\n"+body+"\n")

	s := schedulerWithSkills("scanner", []string{"go-testing"})
	got := s.primeSkills("scanner", "")
	if got == "" {
		t.Fatal("primeSkills returned empty for a declared, present skill")
	}
	if !strings.Contains(got, body) {
		t.Errorf("injection %q does not contain the skill body %q", got, body)
	}
	if !strings.Contains(got, "go-testing") {
		t.Errorf("injection %q does not name the skill", got)
	}
	if !strings.Contains(got, "1.2.0") {
		t.Errorf("injection %q does not carry the resolved version", got)
	}
}

// Skills the agent did NOT declare must stay out of its context.
func TestPrimeSkills_OnlyDeclaredSkillsAreInjected(t *testing.T) {
	dir := useSkillsDir(t)
	writeSkill(t, dir, "wanted.md", "WANTED BODY")
	writeSkill(t, dir, "unwanted.md", "UNWANTED BODY")

	s := schedulerWithSkills("scanner", []string{"wanted"})
	got := s.primeSkills("scanner", "")
	if !strings.Contains(got, "WANTED BODY") {
		t.Errorf("injection %q missing the declared skill", got)
	}
	if strings.Contains(got, "UNWANTED BODY") {
		t.Errorf("injection %q leaked an undeclared skill", got)
	}
}

func TestPrimeSkills_UnresolvableNameIsNoOp(t *testing.T) {
	dir := useSkillsDir(t)
	writeSkill(t, dir, "present.md", "PRESENT BODY")

	s := schedulerWithSkills("scanner", []string{"typo-not-a-skill"})
	if got := s.primeSkills("scanner", ""); got != "" {
		t.Errorf("primeSkills with only an unknown name = %q, want empty", got)
	}
}

// A typo alongside a good name must degrade, not blank the injection.
func TestPrimeSkills_UnknownNameDoesNotSuppressKnownOne(t *testing.T) {
	dir := useSkillsDir(t)
	writeSkill(t, dir, "present.md", "PRESENT BODY")

	s := schedulerWithSkills("scanner", []string{"typo", "present"})
	got := s.primeSkills("scanner", "")
	if !strings.Contains(got, "PRESENT BODY") {
		t.Errorf("injection %q dropped the resolvable skill because a sibling name was unknown", got)
	}
}

// A repo-local skill is the fallback half of ResolveRequested: it must work
// even when the hive-wide registry directory does not exist.
func TestPrimeSkills_RepoInlineSkillFallbackWithoutRegistry(t *testing.T) {
	orig := skillsRegistryDir
	t.Cleanup(func() { skillsRegistryDir = orig })
	skillsRegistryDir = filepath.Join(t.TempDir(), "does-not-exist")

	repoRoot := t.TempDir()
	const body = "Use table-driven tests for validation branches."
	if err := os.WriteFile(filepath.Join(repoRoot, "AGENTS.md"), []byte("## Skill: go-testing\n"+body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := schedulerWithSkills("scanner", []string{"go-testing"})
	got := s.primeSkills("scanner", repoRoot)
	if !strings.Contains(got, body) {
		t.Errorf("injection %q missing repo-local fallback body %q", got, body)
	}
}

// Registry definitions are curated and versioned, so they retain the package's
// documented precedence over an ad-hoc repo-local definition of the same name.
func TestPrimeSkills_RegistryOverridesRepoInlineSkill(t *testing.T) {
	dir := useSkillsDir(t)
	writeSkill(t, dir, "go-testing.md", "REGISTRY BODY")

	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "AGENTS.md"), []byte("## Skill: go-testing\nREPO BODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := schedulerWithSkills("scanner", []string{"go-testing"})
	got := s.primeSkills("scanner", repoRoot)
	if !strings.Contains(got, "REGISTRY BODY") {
		t.Errorf("injection %q missing registry definition", got)
	}
	if strings.Contains(got, "REPO BODY") {
		t.Errorf("injection %q used repo fallback despite registry match", got)
	}
}

// End-to-end through the kick assembly: the skill body must land in ${KNOWLEDGE}.
func TestSubstituteTemplate_KnowledgeCarriesSkills(t *testing.T) {
	dir := useSkillsDir(t)
	const body = "Always sign commits with -s."
	writeSkill(t, dir, "commits.md", body)

	s := schedulerWithSkills("scanner", []string{"commits"})
	out := s.substituteTemplate("BEGIN ${KNOWLEDGE} END", nil, "scanner", nil)
	if !strings.Contains(out, body) {
		t.Errorf("expanded kick %q does not contain the injected skill body %q", out, body)
	}
}

// End-to-end through checkout-root resolution and kick assembly: an agent's
// skills list can name a definition that exists only in the primary repo.
func TestSubstituteTemplate_KnowledgeCarriesRepoSkillFallback(t *testing.T) {
	orig := skillsRegistryDir
	t.Cleanup(func() { skillsRegistryDir = orig })
	skillsRegistryDir = filepath.Join(t.TempDir(), "does-not-exist")

	checkouts := t.TempDir()
	repoRoot := filepath.Join(checkouts, "hive")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "Run the repository's focused verifier before pushing."
	if err := os.WriteFile(filepath.Join(repoRoot, "AGENTS.md"), []byte("## Skill: verification\n"+body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Skills: []string{"verification"}},
		},
	}
	cfg.Project.PrimaryRepo = "hive"
	cfg.Project.CheckoutsDir = checkouts
	s := New(cfg, slog.Default())

	out := s.substituteTemplate("BEGIN ${KNOWLEDGE} END", nil, "scanner", nil)
	if !strings.Contains(out, body) {
		t.Errorf("expanded kick %q does not contain repo-local fallback body %q", out, body)
	}
}

// --- capSkills ---

func TestCapSkills_KeepsEverythingUnderCap(t *testing.T) {
	in := []skillreg.Skill{
		{Name: "a", Body: "aaa"},
		{Name: "b", Body: "bbb"},
	}
	kept, dropped := capSkills(in, 100)
	if len(kept) != 2 {
		t.Errorf("kept %d skills, want 2", len(kept))
	}
	if len(dropped) != 0 {
		t.Errorf("dropped %v, want none", dropped)
	}
}

func TestCapSkills_DropsOversizedSkillWhole(t *testing.T) {
	in := []skillreg.Skill{{Name: "huge", Body: strings.Repeat("x", 50)}}
	kept, dropped := capSkills(in, 10)
	if len(kept) != 0 {
		t.Errorf("kept %d skills, want 0 (oversized skill must be dropped, not truncated)", len(kept))
	}
	if len(dropped) != 1 || dropped[0] != "huge" {
		t.Errorf("dropped = %v, want [huge]", dropped)
	}
}

// A single oversized skill must not suppress the smaller ones after it.
func TestCapSkills_SmallSkillAfterOversizedStillKept(t *testing.T) {
	in := []skillreg.Skill{
		{Name: "huge", Body: strings.Repeat("x", 50)},
		{Name: "small", Body: "ok"},
	}
	kept, dropped := capSkills(in, 10)
	if len(kept) != 1 || kept[0].Name != "small" {
		t.Errorf("kept = %v, want just [small]", kept)
	}
	if len(dropped) != 1 || dropped[0] != "huge" {
		t.Errorf("dropped = %v, want [huge]", dropped)
	}
}

func TestPrimeSkills_OversizedSkillIsNotInjected(t *testing.T) {
	dir := useSkillsDir(t)
	writeSkill(t, dir, "huge.md", strings.Repeat("x", maxSkillsInjectionBytes+1))

	s := schedulerWithSkills("scanner", []string{"huge"})
	if got := s.primeSkills("scanner", ""); got != "" {
		t.Errorf("primeSkills injected %d chars for an over-cap skill, want empty", len(got))
	}
}
