package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNextRetroLessonConfidence(t *testing.T) {
	cases := []struct {
		name              string
		conf, step, limit float64
		want              float64
	}{
		{"zero config uses defaults", 0.70, 0, 0, 0.75},
		{"negative config uses defaults", 0.70, -1, -1, 0.75},
		{"at cap unchanged", 0.90, 0.05, 0.9, 0.90},
		{"above cap unchanged", 0.95, 0.05, 0.9, 0.95},
		{"step clamped to cap", 0.88, 0.05, 0.9, 0.90},
		{"cap above one clamps to one", 0.98, 0.05, 5, 1.0},
		{"cap with extra precision floors", 0.80, 0.10, 0.857, 0.85},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextRetroLessonConfidence(tc.conf, tc.step, tc.limit); got != tc.want {
				t.Fatalf("nextRetroLessonConfidence(%v, %v, %v) = %v, want %v", tc.conf, tc.step, tc.limit, got, tc.want)
			}
		})
	}
}

func TestApplyRetroLessonRepeatRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"no front matter":      "just a body",
		"unterminated":         "---\ntitle: x\ntags: [retro, lesson]\n",
		"empty front matter":   "---\n---\nbody",
		"missing lesson tag":   "---\ntags: [retro]\n---\n\nbody",
		"missing retro tag":    "---\ntags: [lesson]\n---\n\nbody",
		"deprecated state":     "---\ntags: [retro, lesson]\nstate: deprecated\n---\n\nbody",
		"superseded lifecycle": "---\ntags: [retro, lesson]\nsuperseded_by: other\n---\n\nbody",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if out, n, ok := applyRetroLessonRepeat(content, "retro-lesson-x", "bead:b-1", "b-1", 0, 0); ok || out != "" || n != 0 {
				t.Fatalf("got (%q, %d, %v), want rejection", out, n, ok)
			}
		})
	}
}

func TestApplyRetroLessonRepeatAddsMissingLines(t *testing.T) {
	content := "---\ntitle: Retro lesson: x\ntags: [retro, lesson]\n---\n\nbody"
	out, n, ok := applyRetroLessonRepeat(content, "retro-lesson-x", "pr:org/repo#1", "", 0, 0)
	if !ok || n != 2 {
		t.Fatalf("ok=%v observations=%d", ok, n)
	}
	for _, want := range []string{"confidence: 0.75", "observations: 2", "sources: [pr:org/repo#1]"} {
		if frontmatterLine(out, strings.SplitN(want, ":", 2)[0]) != want {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n---\n\nbody") {
		t.Fatalf("body not preserved:\n%s", out)
	}
}

func TestApplyRetroLessonRepeatWithoutBeadDedupesExactEntry(t *testing.T) {
	content := "---\ntags: [retro, lesson]\nconfidence: 0.70\nobservations: 2\nsources: [pr:org/repo#1]\n---\n\nbody"
	if _, _, ok := applyRetroLessonRepeat(content, "retro-lesson-x", "pr:org/repo#1", "", 0, 0); ok {
		t.Fatal("exact repeated source must be a no-op")
	}
	out, n, ok := applyRetroLessonRepeat(content, "retro-lesson-x", "pr:org/repo#2", "", 0, 0)
	if !ok || n != 3 || frontmatterLine(out, "sources") != "sources: [pr:org/repo#1, pr:org/repo#2]" {
		t.Fatalf("ok=%v n=%d\n%s", ok, n, out)
	}
}

func TestApplyRetroLessonRepeatAtCapKeepsConfidence(t *testing.T) {
	content := "---\ntags: [retro, lesson]\nconfidence: 0.95\nobservations: bogus\nsources: not-a-list\n---\n\nbody"
	out, n, ok := applyRetroLessonRepeat(content, "retro-lesson-x", "bead:b-1", "b-1", 0, 0)
	if !ok || n != 2 {
		t.Fatalf("ok=%v n=%d", ok, n)
	}
	if frontmatterLine(out, "confidence") != "confidence: 0.95" || frontmatterLine(out, "sources") != "sources: [bead:b-1]" {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestParseInlineFrontmatterList(t *testing.T) {
	if got := parseInlineFrontmatterList("plain text"); got != nil {
		t.Fatalf("non-list = %v", got)
	}
	got := parseInlineFrontmatterList(` ["a", 'b', , c ]`)
	if strings.Join(got, "|") != "a|b|c" {
		t.Fatalf("list = %v", got)
	}
}

func TestRetroSourceTokenStripsListMetacharacters(t *testing.T) {
	got := retroSourceToken(`a,b [c] 'd' "e"`)
	if got != "a_b_c_d_e" {
		t.Fatalf("token = %q", got)
	}
	if retroSourceToken(" , [] ") != "" {
		t.Fatal("metacharacters only must yield an empty token")
	}
}

func TestRetroLessonRepeatNoOpPaths(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
	slug := ingestLocalLesson(t, api, repeatLessonText, "b-1")
	before := readRetroLessonFile(t, base, slug)

	api.recordRetroLessonRepeat("not-a-retro-slug", RetroLesson{SourceBead: "b-2"})
	api.recordRetroLessonRepeat(slug, RetroLesson{Lesson: repeatLessonText})
	api.recordRetroLessonRepeat(retroLessonSlugPrefix+"missing", RetroLesson{SourceBead: "b-2"})
	if after := readRetroLessonFile(t, base, slug); after != before {
		t.Fatalf("no-op repeat changed the lesson:\n%s", after)
	}
}

func TestRetroLessonRepeatUnreadableLessonIsSkipped(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
	slug := retroLessonSlugPrefix + "dirlesson"
	// A directory at the lesson path passes Stat but cannot be read as a file.
	if err := os.MkdirAll(retroLessonFilePath(base, slug), 0o755); err != nil {
		t.Fatal(err)
	}
	api.recordRetroLessonRepeat(slug, RetroLesson{SourceBead: "b-2"})
}

func TestRetroLessonRepeatWriteFailureIsSkipped(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
	slug := ingestLocalLesson(t, api, repeatLessonText, "b-1")
	path := retroLessonFilePath(base, slug)
	content := readRetroLessonFile(t, base, slug)

	// Rename onto a non-empty directory fails, after the temp file is written.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep", "f"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stat passes and ReadFile fails on a directory, so exercise the write error via writeFileAtomic directly.
	api.recordRetroLessonRepeat(slug, RetroLesson{SourceBead: "b-2"})

	dir := filepath.Join(base, "project")
	if err := writeFileAtomic(dir, path, []byte(content)); err == nil {
		t.Fatal("rename onto a non-empty directory must fail")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".retro-lesson-*.tmp")); len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}

func TestWriteFileAtomicMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if err := writeFileAtomic(dir, filepath.Join(dir, "x.md"), []byte("x")); err == nil {
		t.Fatal("writing into a missing directory must fail")
	}
}

func TestWriteFileAtomicReplacesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.md")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(dir, path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "new" {
		t.Fatalf("content = %q err=%v", raw, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v err=%v", info, err)
	}
}

func TestLockRetroLessonDir(t *testing.T) {
	// Lock file cannot be opened: the in-process mutex alone still serializes.
	lockRetroLessonDir(filepath.Join(t.TempDir(), "missing"))()

	dir := t.TempDir()
	unlock := lockRetroLessonDir(dir)
	unlock()
	if _, err := os.Stat(filepath.Join(dir, retroLessonLockFile)); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
	lockRetroLessonDir(dir)()
}

func TestRetroLessonRepeatNearDuplicateOutsideProjectDir(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
	slug := retroLessonSlugPrefix + "elsewhere"
	content := "---\ntitle: Retro lesson: elsewhere\ntype: pattern\nlayer: team\nconfidence: 0.70\ntags: [retro, lesson]\nsource: retro bead:b-1\n---\n\n" + repeatLessonText
	dir := filepath.Join(base, "team")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	api.triggerVaultReindex(dir)
	api.recordRetroLessonRepeat(slug, RetroLesson{SourceBead: "b-2"})
	if raw, err := os.ReadFile(filepath.Join(dir, slug+".md")); err != nil || string(raw) != content {
		t.Fatalf("lesson outside <kb>/project changed: %v\n%s", err, raw)
	}
}
