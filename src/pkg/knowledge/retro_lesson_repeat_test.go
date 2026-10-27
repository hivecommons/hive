package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const repeatLessonText = "Run targeted validation before opening PRs when changes affect CI-sensitive code paths."

func retroLessonFilePath(base, slug string) string {
	return filepath.Join(base, "project", slug+".md")
}

func readRetroLessonFile(t *testing.T, base, slug string) string {
	t.Helper()
	raw, err := os.ReadFile(retroLessonFilePath(base, slug))
	if err != nil {
		t.Fatalf("reading lesson %s: %v", slug, err)
	}
	return string(raw)
}

func frontmatterLine(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, key+":") {
			return line
		}
	}
	return ""
}

// withoutRepeatFields drops the only lines a repeat may change.
func withoutRepeatFields(content string) string {
	var kept []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "confidence:") || strings.HasPrefix(line, "observations:") || strings.HasPrefix(line, "sources:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func ingestLocalLesson(t *testing.T, api *KnowledgeAPI, text, bead string) string {
	t.Helper()
	slug, _, err := api.IngestRetroLesson(context.Background(), RetroLesson{Lesson: text, SourceBead: bead, SourcePR: "org/repo#" + bead})
	if err != nil {
		t.Fatalf("IngestRetroLesson(%s): %v", bead, err)
	}
	return slug
}

func writeLessonFixture(t *testing.T, api *KnowledgeAPI, base, slug, content string) {
	t.Helper()
	dir := filepath.Join(base, "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	api.triggerVaultReindex(dir)
}

func retroSlugFor(text string) string {
	return retroLessonSlugPrefix + shortHash(normalizedRetroLessonKey(text))
}

func TestRetroLessonRepeatSameTextTwoSources(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())

	slug := ingestLocalLesson(t, api, repeatLessonText, "b-1")
	before := readRetroLessonFile(t, base, slug)
	slug2, created, err := api.IngestRetroLesson(context.Background(), RetroLesson{Lesson: repeatLessonText, SourceBead: "b-2", SourcePR: "org/repo#b-2"})
	if err != nil || created || slug2 != slug {
		t.Fatalf("repeat ingest = (%q, %v, %v), want (%q, false, nil)", slug2, created, err, slug)
	}
	after := readRetroLessonFile(t, base, slug)
	if got := frontmatterLine(after, "observations"); got != "observations: 2" {
		t.Fatalf("observations line = %q", got)
	}
	if got := frontmatterLine(after, "confidence"); got != "confidence: 0.75" {
		t.Fatalf("confidence line = %q", got)
	}
	if got := frontmatterLine(after, "sources"); got != "sources: [bead:b-1 pr:org/repo#b-1, bead:b-2 pr:org/repo#b-2]" {
		t.Fatalf("sources line = %q", got)
	}
	// The legacy single source line is kept as is (A-AC6).
	if got := frontmatterLine(after, "source"); got != "source: retro bead:b-1 pr:org/repo#b-1" {
		t.Fatalf("source line = %q", got)
	}
	if withoutRepeatFields(before) != withoutRepeatFields(after) {
		t.Fatalf("repeat changed more than the repeat fields:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	fact, err := api.VaultFact(slug)
	if err != nil || fact.Confidence != 0.75 {
		t.Fatalf("VaultFact confidence = %v err=%v, want stored 0.75", fact, err)
	}
}

func TestRetroLessonRepeatContainedLesson(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())

	slug := ingestLocalLesson(t, api, "Reviewers reject PRs that fail lint.", "b-1")
	got, created, err := api.IngestRetroLesson(context.Background(), RetroLesson{
		Lesson:     "Run the linter before pushing: reviewers reject PRs that fail lint.",
		SourceBead: "b-2",
	})
	if err != nil || created || got != slug {
		t.Fatalf("contained repeat = (%q, %v, %v), want (%q, false, nil)", got, created, err, slug)
	}
	after := readRetroLessonFile(t, base, slug)
	if frontmatterLine(after, "observations") != "observations: 2" || frontmatterLine(after, "confidence") != "confidence: 0.75" {
		t.Fatalf("contained repeat not recorded:\n%s", after)
	}
	if !strings.Contains(frontmatterLine(after, "sources"), "bead:b-2") {
		t.Fatalf("sources missing new bead:\n%s", after)
	}
}

func TestRetroLessonRepeatSameSourceTwiceChangesNothing(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())

	slug := ingestLocalLesson(t, api, repeatLessonText, "b-1")
	ingestLocalLesson(t, api, repeatLessonText, "b-2")
	before := readRetroLessonFile(t, base, slug)
	ingestLocalLesson(t, api, repeatLessonText, "b-2")
	ingestLocalLesson(t, api, repeatLessonText, "b-1")
	if after := readRetroLessonFile(t, base, slug); after != before {
		t.Fatalf("repeat from a recorded bead changed the lesson:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestRetroLessonRepeatConfidenceCap(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())

	slug := ingestLocalLesson(t, api, repeatLessonText, "b-1")
	for i := 2; i <= 10; i++ {
		ingestLocalLesson(t, api, repeatLessonText, fmt.Sprintf("b-%d", i))
	}
	after := readRetroLessonFile(t, base, slug)
	if got := frontmatterLine(after, "confidence"); got != "confidence: 0.90" {
		t.Fatalf("confidence line = %q, want the cap", got)
	}

	const otherText = "Rotate long-lived deploy credentials before they expire in production."
	otherSlug := retroSlugFor(otherText)
	writeLessonFixture(t, api, base, otherSlug, "---\ntitle: Retro lesson: Rotate credentials\ntype: pattern\nlayer: project\nconfidence: 0.90\ntags: [retro, lesson]\nsource: retro bead:x-1\nsynthesized: 2026-01-01T00:00:00Z\n---\n\n"+otherText)
	repeated, err := api.VaultFact(slug)
	if err != nil {
		t.Fatal(err)
	}
	single, err := api.VaultFact(otherSlug)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Confidence != DefaultRetroLessonConfidenceCap || repeated.Confidence > single.Confidence {
		t.Fatalf("repeated lesson score %v exceeds single-source lesson at cap %v", repeated.Confidence, single.Confidence)
	}
}

func TestRetroLessonRepeatConfiguredStepAndCap(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())

	slug := ingestLocalLesson(t, api, repeatLessonText, "b-1")
	for _, bead := range []string{"b-2", "b-3"} {
		api.recordRetroLessonRepeat(slug, RetroLesson{Lesson: repeatLessonText, SourceBead: bead, ConfidenceStep: 0.1, ConfidenceCap: 0.85})
	}
	if got := frontmatterLine(readRetroLessonFile(t, base, slug), "confidence"); got != "confidence: 0.85" {
		t.Fatalf("confidence line = %q, want configured cap 0.85", got)
	}
}

func TestRetroLessonRepeatLeavesOtherFactsUntouched(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())

	content := "---\ntitle: Lint policy\ntype: pattern\nlayer: project\nconfidence: 0.70\ntags: [process]\nsource: manual\n---\n\nReviewers reject PRs that fail lint."
	writeLessonFixture(t, api, base, "lint-policy", content)
	if _, created, err := api.IngestRetroLesson(context.Background(), RetroLesson{
		Lesson:     "Run the linter before pushing: reviewers reject PRs that fail lint.",
		SourceBead: "b-2",
	}); err != nil || created {
		t.Fatalf("ingest created=%v err=%v, want duplicate skip", created, err)
	}
	if got := readRetroLessonFile(t, base, "lint-policy"); got != content {
		t.Fatalf("non-retro fact changed:\n%s", got)
	}

	// A retro-prefixed slug without the retro and lesson tags is not a retro lesson.
	untagged := strings.Replace(content, "title: Lint policy", "title: Untagged", 1)
	if _, _, ok := applyRetroLessonRepeat(untagged, "retro-lesson-x", "bead:b-3", "b-3", 0, 0); ok {
		t.Fatal("untagged fact must not record a repeat")
	}
}

func TestRetroLessonRepeatLegacyLessonCountsAsOne(t *testing.T) {
	legacy := "---\ntitle: Retro lesson: Run targeted validation\ntype: pattern\nlayer: project\nconfidence: 0.70\ntags: [retro, lesson]\nsource: retro bead:old-1 pr:org/repo#1\nsynthesized: 2026-01-01T00:00:00Z\n---\n\n" + repeatLessonText
	updated, observations, ok := applyRetroLessonRepeat(legacy, "retro-lesson-x", "bead:new-1", "new-1", 0, 0)
	if !ok || observations != 2 {
		t.Fatalf("legacy repeat ok=%v observations=%d, want true, 2", ok, observations)
	}
	if got := frontmatterLine(updated, "sources"); got != "sources: [bead:old-1 pr:org/repo#1, bead:new-1]" {
		t.Fatalf("sources line = %q", got)
	}
	if !strings.Contains(updated, "confidence: 0.75\nobservations: 2\n") {
		t.Fatalf("observations not placed beside confidence:\n%s", updated)
	}
}

func TestRetroLessonRepeatRanksAboveOneOff(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())

	const oneOffText = "Rotate long-lived deploy credentials before they expire in production."
	oneOff := ingestLocalLesson(t, api, oneOffText, "o-1")
	seen := ingestLocalLesson(t, api, repeatLessonText, "b-1")
	ingestLocalLesson(t, api, repeatLessonText, "b-2")
	ingestLocalLesson(t, api, repeatLessonText, "b-3")
	if got := frontmatterLine(readRetroLessonFile(t, base, seen), "observations"); got != "observations: 3" {
		t.Fatalf("observations line = %q", got)
	}
	a, errA := api.VaultFact(seen)
	b, errB := api.VaultFact(oneOff)
	if errA != nil || errB != nil {
		t.Fatalf("VaultFact errors: %v %v", errA, errB)
	}
	if a.Confidence <= b.Confidence {
		t.Fatalf("lesson seen 3 times (%v) does not rank above one-off (%v)", a.Confidence, b.Confidence)
	}
}

func TestRetroLessonRepeatDraftStaysDraft(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())

	slug := retroSlugFor(repeatLessonText)
	draft := "---\ntitle: Retro lesson: Run targeted validation\ntype: pattern\nlayer: project\nconfidence: 0.70\ntags: [retro, lesson]\nstate: draft\nsource: retro bead:b-1\nsynthesized: 2026-01-01T00:00:00Z\n---\n\n" + repeatLessonText
	writeLessonFixture(t, api, base, slug, draft)
	if _, created, err := api.IngestRetroLesson(context.Background(), RetroLesson{Lesson: repeatLessonText, SourceBead: "b-2"}); err != nil || created {
		t.Fatalf("draft repeat created=%v err=%v", created, err)
	}
	after := readRetroLessonFile(t, base, slug)
	if frontmatterLine(after, "observations") != "observations: 2" || frontmatterLine(after, "state") != "state: draft" {
		t.Fatalf("draft repeat not recorded as draft:\n%s", after)
	}
	if withoutRepeatFields(draft) != withoutRepeatFields(after) {
		t.Fatalf("draft repeat changed more than the repeat fields:\n%s", after)
	}
	fact, err := api.VaultFact(slug)
	if err != nil {
		t.Fatal(err)
	}
	if fact.EffectiveState() != StateDraft || len(FilterByLifecycle([]Fact{*fact}, nil)) != 0 {
		t.Fatalf("draft lesson state=%q would reach a primer", fact.EffectiveState())
	}
}

func TestRetroLessonRepeatOtherStates(t *testing.T) {
	cases := []struct {
		name, line string
		recorded   bool
	}{
		{"approved", "state: approved", true},
		{"superseded", "superseded_by: retro-lesson-newer", false},
		{"deprecated", "state: deprecated", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := withKnowledgeBaseDir(t)
			api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
			slug := retroSlugFor(repeatLessonText)
			content := "---\ntitle: Retro lesson: Run targeted validation\ntype: pattern\nlayer: project\nconfidence: 0.70\ntags: [retro, lesson]\n" + tc.line + "\nsource: retro bead:b-1\n---\n\n" + repeatLessonText
			writeLessonFixture(t, api, base, slug, content)
			if _, created, err := api.IngestRetroLesson(context.Background(), RetroLesson{Lesson: repeatLessonText, SourceBead: "b-2"}); err != nil || created {
				t.Fatalf("created=%v err=%v", created, err)
			}
			after := readRetroLessonFile(t, base, slug)
			if !tc.recorded {
				if after != content {
					t.Fatalf("%s lesson changed:\n%s", tc.name, after)
				}
				return
			}
			if frontmatterLine(after, "observations") != "observations: 2" || frontmatterLine(after, "state") != tc.line {
				t.Fatalf("approved repeat lost its state or count:\n%s", after)
			}
		})
	}
}

func TestRetroLessonRepeatSourcesBounded(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())

	slug := ingestLocalLesson(t, api, repeatLessonText, "b-1")
	for i := 2; i <= 26; i++ {
		api.recordRetroLessonRepeat(slug, RetroLesson{Lesson: repeatLessonText, SourceBead: fmt.Sprintf("b-%d", i)})
	}
	after := readRetroLessonFile(t, base, slug)
	if got := frontmatterLine(after, "observations"); got != "observations: 26" {
		t.Fatalf("observations line = %q", got)
	}
	sources := parseInlineFrontmatterList(strings.TrimPrefix(frontmatterLine(after, "sources"), "sources:"))
	if len(sources) != retroLessonMaxSources || sources[0] != "bead:b-7" || sources[19] != "bead:b-26" {
		t.Fatalf("sources = %v, want the newest 20 (b-7..b-26)", sources)
	}
}

func TestRetroLessonRepeatRemoteLayerSkipsAndLogs(t *testing.T) {
	var updates int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/search":
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []searchResult{{
				Slug:       "retro-lesson-existing",
				Title:      "Retro lesson: Run targeted validation",
				Snippet:    repeatLessonText,
				Type:       string(FactPattern),
				Confidence: retroLessonConfidence,
			}}})
		case strings.HasPrefix(r.URL.Path, "/api/pages/"), r.URL.Path == "/api/ingest":
			updates++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var logs bytes.Buffer
	api := NewKnowledgeAPI([]LayerConfig{{Type: LayerProject, URL: srv.URL}}, KnowledgeConfig{Enabled: true}, slog.New(slog.NewTextHandler(&logs, nil)))
	slug, created, err := api.IngestRetroLesson(context.Background(), RetroLesson{Lesson: repeatLessonText, SourceBead: "b-2"})
	if err != nil || created || slug != "retro-lesson-existing" {
		t.Fatalf("remote repeat = (%q, %v, %v)", slug, created, err)
	}
	if updates != 0 {
		t.Fatalf("remote layer received %d writes, want 0", updates)
	}
	if !strings.Contains(logs.String(), "lesson repeat skipped") {
		t.Fatalf("remote skip not logged:\n%s", logs.String())
	}
}

func TestRetroLessonRepeatConcurrentLocal(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())

	slug := ingestLocalLesson(t, api, repeatLessonText, "b-1")
	var wg sync.WaitGroup
	for _, bead := range []string{"b-2", "b-3"} {
		wg.Add(1)
		go func(bead string) {
			defer wg.Done()
			api.recordRetroLessonRepeat(slug, RetroLesson{Lesson: repeatLessonText, SourceBead: bead})
		}(bead)
	}
	wg.Wait()
	after := readRetroLessonFile(t, base, slug)
	src := frontmatterLine(after, "sources")
	if frontmatterLine(after, "observations") != "observations: 3" || !strings.Contains(src, "bead:b-2") || !strings.Contains(src, "bead:b-3") {
		t.Fatalf("concurrent repeats lost an update:\n%s", after)
	}
}

func TestRetroRepeatSourceSanitizesFrontmatter(t *testing.T) {
	got := retroRepeatSource(RetroLesson{SourceBead: "b-1\nconfidence: 0.99", SourcePR: "org/repo#1, [x]"})
	if strings.ContainsAny(got, "\n,[]") {
		t.Fatalf("repeat source %q carries front-matter metacharacters", got)
	}
	if retroRepeatSource(RetroLesson{}) != "" {
		t.Fatal("lesson without sources must yield no entry")
	}
}
