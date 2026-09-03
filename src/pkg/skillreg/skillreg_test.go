package skillreg

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/agentsmd"
)

// writeSkill writes content to <dir>/<name> creating the dir as needed.
func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const goTestingSkill = `---
name: go-testing
version: 1.2.0
description: How this repo writes Go tests.
tags: [go, testing, quality]
---
Prefer t.TempDir over manual temp dirs.
Never hit the network in unit tests.`

const prEtiquetteSkill = `---
name: pr-etiquette
version: 2.0.0
description: PR conventions.
tags: [git, github]
---
Sign commits with DCO.
Keep PRs small.`

// noFrontMatter has no front-matter; name defaults to file base, version default.
const noFrontMatter = `Just a body of instructions with no metadata.`

func TestLoad_FrontMatterVersionsTags(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "go-testing.md", goTestingSkill)
	writeSkill(t, dir, "pr-etiquette.md", prEtiquetteSkill)
	writeSkill(t, dir, "plain.md", noFrontMatter)
	// Non-.md files and subdirs must be ignored.
	writeSkill(t, dir, "README.txt", "ignore me")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry()
	n, err := r.Load(dir, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n != 3 {
		t.Fatalf("loaded = %d, want 3", n)
	}

	got, ok := r.Get("go-testing")
	if !ok {
		t.Fatal("go-testing not found")
	}
	if got.Version != "1.2.0" {
		t.Errorf("version = %q, want 1.2.0", got.Version)
	}
	if got.Description != "How this repo writes Go tests." {
		t.Errorf("description = %q", got.Description)
	}
	if len(got.Tags) != 3 || got.Tags[0] != "go" {
		t.Errorf("tags = %v", got.Tags)
	}
	if got.Source != SourceURLLocalFile {
		t.Errorf("source = %q, want %q", got.Source, SourceURLLocalFile)
	}
	if want := "Prefer t.TempDir over manual temp dirs.\nNever hit the network in unit tests."; got.Body != want {
		t.Errorf("body = %q", got.Body)
	}

	// plain.md: name defaults to base, version to DefaultVersion.
	plain, ok := r.Get("plain")
	if !ok {
		t.Fatal("plain not found")
	}
	if plain.Version != DefaultVersion {
		t.Errorf("plain version = %q, want %q", plain.Version, DefaultVersion)
	}
	if plain.Body != noFrontMatter {
		t.Errorf("plain body = %q", plain.Body)
	}
}

func TestLoad_UnreadableDirTolerated(t *testing.T) {
	dir := t.TempDir()
	// Point Load at a path that is a file, not a directory: ReadDir errors with
	// a non-NotExist error, which Load must tolerate (0 loaded, nil error).
	writeSkill(t, dir, "afile.md", goTestingSkill)
	r := NewRegistry()
	n, err := r.Load(filepath.Join(dir, "afile.md"), nil)
	if err != nil {
		t.Fatalf("Load on a file path: %v", err)
	}
	if n != 0 {
		t.Fatalf("loaded = %d, want 0", n)
	}
}

func TestLoad_UnreadableSkillFileSkipped(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "good.md", goTestingSkill)
	writeSkill(t, dir, "locked.md", "body")
	// Make locked.md unreadable so os.ReadFile errors; it must be skipped.
	if err := os.Chmod(filepath.Join(dir, "locked.md"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "locked.md"), 0o644) })

	r := NewRegistry()
	n, err := r.Load(dir, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n != 1 {
		t.Skipf("expected 1 loaded (root may bypass perms); got %d", n)
	}
}

func TestVersionParts_NonNumericReadsZero(t *testing.T) {
	// A non-numeric segment reads as zero, so "abc" == "0.0.0".
	if compareVersions("abc", "0.0.0") != 0 {
		t.Errorf("non-numeric version should compare equal to 0.0.0")
	}
	if compareVersions("1.x.3", "1.0.3") != 0 {
		t.Errorf("non-numeric minor should read as zero")
	}
}

func TestMissingDirIsSafe(t *testing.T) {
	r := NewRegistry()
	n, err := r.Load(filepath.Join(t.TempDir(), "does-not-exist"), nil)
	if err != nil {
		t.Fatalf("Load missing dir: %v", err)
	}
	if n != 0 {
		t.Fatalf("loaded = %d, want 0", n)
	}
}

func TestLoad_MalformedFileTolerated(t *testing.T) {
	dir := t.TempDir()
	// Malformed front-matter (tab in YAML flow) — must fall back to defaults,
	// not crash the whole load.
	writeSkill(t, dir, "broken.md", "---\nname: [oops\n\tbad yaml\n---\nbody here")
	writeSkill(t, dir, "good.md", goTestingSkill)

	r := NewRegistry()
	n, err := r.Load(dir, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// good.md always loads; broken.md loads with defaults (name from filename).
	if n != 2 {
		t.Fatalf("loaded = %d, want 2", n)
	}
	if _, ok := r.Get("go-testing"); !ok {
		t.Error("good skill should still load past a malformed neighbor")
	}
	// broken.md keeps its filename as name and full content as body.
	broken, ok := r.Get("broken")
	if !ok {
		t.Fatal("broken skill should load with default name")
	}
	if broken.Version != DefaultVersion {
		t.Errorf("broken version = %q, want default", broken.Version)
	}
}

func TestAddGet(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(Skill{Name: "a", Version: "1.0.0", Body: "one"}); err != nil {
		t.Fatal(err)
	}
	// Add with no version defaults it; no source defaults to builtin.
	if err := r.Add(Skill{Name: "b", Body: "two"}); err != nil {
		t.Fatal(err)
	}
	b, ok := r.Get("b")
	if !ok {
		t.Fatal("b missing")
	}
	if b.Version != DefaultVersion {
		t.Errorf("b version = %q", b.Version)
	}
	if b.Source != SourceBuiltin {
		t.Errorf("b source = %q, want builtin", b.Source)
	}

	if _, ok := r.Get("missing"); ok {
		t.Error("missing should not be found")
	}
}

func TestAdd_EmptyNameRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(Skill{Name: "  ", Body: "x"}); err == nil {
		t.Fatal("empty name should be rejected")
	}
}

func TestAdd_SameVersionReplaces(t *testing.T) {
	r := NewRegistry()
	_ = r.Add(Skill{Name: "a", Version: "1.0.0", Body: "old"})
	_ = r.Add(Skill{Name: "a", Version: "1.0.0", Body: "new"})
	got, _ := r.Get("a")
	if got.Body != "new" {
		t.Errorf("body = %q, want new (same version replaces)", got.Body)
	}
}

func TestGet_HighestVersion(t *testing.T) {
	r := NewRegistry()
	_ = r.Add(Skill{Name: "a", Version: "1.0.0", Body: "v1"})
	_ = r.Add(Skill{Name: "a", Version: "2.3.0", Body: "v2"})
	_ = r.Add(Skill{Name: "a", Version: "1.9.9", Body: "v1.9"})
	got, ok := r.Get("a")
	if !ok || got.Version != "2.3.0" {
		t.Fatalf("Get highest = %q, want 2.3.0", got.Version)
	}
}

func TestResolveRequested_Precedence(t *testing.T) {
	r := NewRegistry()
	// Registry has a curated go-testing; config has an inline one too.
	_ = r.Add(Skill{Name: "go-testing", Version: "3.0.0", Body: "REGISTRY body", Source: SourceBuiltin})

	cfg := &agentsmd.AgentsConfig{
		Skills: map[string]string{
			"go-testing":   "INLINE body", // registry wins
			"repo-only":    "repo local",  // only in config -> fallback
			"empty-inline": "   ",         // empty inline -> skipped
		},
		RequestedSkills: []string{"go-testing", "repo-only", "unknown", "empty-inline"},
	}

	got := r.ResolveRequested(cfg, nil) // nil -> use cfg.RequestedSkills
	if len(got) != 2 {
		t.Fatalf("resolved %d skills, want 2: %+v", len(got), got)
	}
	// Order preserved: go-testing (registry), then repo-only (fallback).
	if got[0].Name != "go-testing" || got[0].Body != "REGISTRY body" {
		t.Errorf("first = %+v, want registry go-testing", got[0])
	}
	if got[0].Source != SourceBuiltin {
		t.Errorf("registry skill source = %q", got[0].Source)
	}
	if got[1].Name != "repo-only" || got[1].Body != "repo local" {
		t.Errorf("second = %+v, want repo-only fallback", got[1])
	}
	if got[1].Source != SourceRepo {
		t.Errorf("fallback source = %q, want repo", got[1].Source)
	}
}

func TestResolveRequested_ExplicitList(t *testing.T) {
	r := NewRegistry()
	_ = r.Add(Skill{Name: "a", Body: "A"})
	cfg := &agentsmd.AgentsConfig{Skills: map[string]string{}, RequestedSkills: []string{"ignored"}}
	got := r.ResolveRequested(cfg, []string{"a", " ", ""})
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("got %+v, want [a]", got)
	}
}

func TestResolveRequested_NilRegistryAndNilConfig(t *testing.T) {
	var r *Registry
	if got := r.ResolveRequested(nil, []string{"a"}); got != nil {
		t.Errorf("nil registry should return nil, got %v", got)
	}
	r = NewRegistry()
	if got := r.ResolveRequested(nil, []string{"a"}); len(got) != 0 {
		t.Errorf("nil config with unknown names should resolve nothing, got %v", got)
	}
}

func TestInjectionText(t *testing.T) {
	if got := InjectionText(nil); got != "" {
		t.Errorf("empty injection = %q, want ''", got)
	}
	skills := []Skill{
		{Name: "go-testing", Version: "1.2.0", Body: "test rules"},
		{Name: "plain", Version: DefaultVersion, Body: "no version tag"},
	}
	got := InjectionText(skills)
	if !contains(got, injectionHeader) {
		t.Errorf("missing header: %q", got)
	}
	if !contains(got, "### go-testing (v1.2.0)") {
		t.Errorf("missing versioned heading: %q", got)
	}
	// DefaultVersion is not rendered.
	if contains(got, "(v0.0.0)") {
		t.Errorf("default version should be hidden: %q", got)
	}
	if !contains(got, "### plain\n") {
		t.Errorf("missing plain heading: %q", got)
	}
	if !contains(got, "test rules") || !contains(got, "no version tag") {
		t.Errorf("missing bodies: %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestConcurrency(t *testing.T) {
	r := NewRegistry()
	const workers = 16
	const iters = 200

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = r.Add(Skill{Name: "shared", Version: "1.0.0", Body: "x"})
				_ = r.Add(Skill{Name: nameFor(w), Version: verFor(i), Body: "y"})
				_, _ = r.Get("shared")
			}
		}(w)
	}
	wg.Wait()

	if _, ok := r.Get("shared"); !ok {
		t.Error("shared skill missing after concurrent access")
	}
}

func nameFor(w int) string {
	return "w" + string(rune('a'+w%26))
}

func verFor(i int) string {
	return "1." + string(rune('0'+i%10)) + ".0"
}
