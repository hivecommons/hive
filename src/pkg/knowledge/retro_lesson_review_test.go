package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

var reviewLessonDate = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// remoteProjectAPI returns an API whose project layer is a remote server and
// a counter of the ingest requests that server received.
func remoteProjectAPI(t *testing.T) (*KnowledgeAPI, *atomic.Int32) {
	t.Helper()
	var ingests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []searchResult{}})
		case "/api/ingest":
			ingests.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	api := NewKnowledgeAPI([]LayerConfig{{Type: LayerProject, URL: srv.URL}}, KnowledgeConfig{Enabled: true}, discardLogger())
	return api, &ingests
}

func TestRetroLessonFromReviewFeedbackStoresLocalDraft(t *testing.T) {
	cases := []struct {
		name   string
		remote bool
	}{
		{"local-only hive", false},
		{"remote project layer", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := withKnowledgeBaseDir(t)
			api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
			var ingests *atomic.Int32
			if tc.remote {
				api, ingests = remoteProjectAPI(t)
			}
			ctx := context.Background()

			slug, created, err := api.IngestRetroLesson(ctx, RetroLesson{
				Lesson: repeatLessonText, SourceBead: "b-1", SourcePR: "org/repo#1",
				SourceDate: reviewLessonDate, FromReviewFeedback: true,
			})
			if err != nil || !created || slug != retroSlugFor(repeatLessonText) {
				t.Fatalf("ingest = (%q, %v, %v)", slug, created, err)
			}
			content := readRetroLessonFile(t, base, slug)
			if got := frontmatterLine(content, "state"); got != "state: draft" {
				t.Fatalf("state line = %q", got)
			}
			if got := frontmatterLine(content, "tags"); got != "tags: [retro, lesson, "+ReviewFeedbackTag+"]" {
				t.Fatalf("tags line = %q", got)
			}
			if ingests != nil && ingests.Load() != 0 {
				t.Fatalf("remote layer received %d writes, want 0", ingests.Load())
			}

			fact, err := api.VaultFact(slug)
			if err != nil || fact.EffectiveState() != StateDraft {
				t.Fatalf("VaultFact = %+v err=%v, want draft", fact, err)
			}
			assertReviewLessonVisible(t, api, slug, false)

			if _, err := api.SetEntryState(slug, StateApproved, ""); err != nil {
				t.Fatalf("approve: %v", err)
			}
			assertReviewLessonVisible(t, api, slug, true)
			if got := frontmatterLine(readRetroLessonFile(t, base, slug), "state"); got != "state: approved" {
				t.Fatalf("state after approval = %q", got)
			}
			if ingests != nil && ingests.Load() != 0 {
				t.Fatalf("approval moved the lesson to the remote layer (%d writes)", ingests.Load())
			}
		})
	}
}

// assertReviewLessonVisible checks every default read path that must hide a
// draft: primer, agent search listings, TOC scopes and non-owner (default
// lifecycle filter) reads.
func assertReviewLessonVisible(t *testing.T, api *KnowledgeAPI, slug string, want bool) {
	t.Helper()
	has := func(facts []Fact) bool {
		for _, f := range facts {
			if f.Slug == slug {
				return true
			}
		}
		return false
	}
	ctx := context.Background()
	all := api.SearchAllWithVaults(ctx, "validation", "", 10)
	if !has(all) {
		t.Fatalf("duplicate search must see the lesson regardless of state: %v", all)
	}
	if got := has(FilterByLifecycle(all, nil)); got != want {
		t.Errorf("default lifecycle filter (search/MCP/export/non-owner) visible = %v, want %v", got, want)
	}
	for _, f := range all {
		if f.Slug != slug {
			continue
		}
		if got := (TOCScope{}).InScope(f); got != want {
			t.Errorf("approved-only TOC scope visible = %v, want %v", got, want)
		}
		if got := AgentScope(nil, nil, nil, nil, []string{"deprecated"}).InScope(f); got != want {
			t.Errorf("agent scope without draft visible = %v, want %v", got, want)
		}
	}
	p := NewPrimer(nil, PrimerConfig{MaxFacts: 50}, discardLogger())
	p.AddFileStore("vault", api.vaults[0], LayerProject)
	if got := has(p.Prime(ctx, nil, []string{"validation"}).Facts); got != want {
		t.Errorf("primer visible = %v, want %v", got, want)
	}
}

func TestRetroLessonWithoutReviewFlagUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		remote bool
	}{
		{"local-only hive", false},
	}
	const golden = "---\n" +
		"title: Retro lesson: Run targeted validation before opening PRs when changes affect CI-sensitive code paths\n" +
		"type: pattern\n" +
		"layer: project\n" +
		"confidence: 0.70\n" +
		"tags: [retro, lesson]\n" +
		"source: retro bead:b-1 pr:org/repo#1\n" +
		"synthesized: 2026-01-02T03:04:05Z\n" +
		"---\n\n" + repeatLessonText
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := withKnowledgeBaseDir(t)
			api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
			slug, created, err := api.IngestRetroLesson(context.Background(), RetroLesson{
				Lesson: repeatLessonText, SourceBead: "b-1", SourcePR: "org/repo#1", SourceDate: reviewLessonDate,
			})
			if err != nil || !created {
				t.Fatalf("ingest = (%q, %v, %v)", slug, created, err)
			}
			if got := readRetroLessonFile(t, base, slug); got != golden {
				t.Fatalf("lesson file changed:\n%s", got)
			}
			fact, err := api.VaultFact(slug)
			if err != nil || fact.EffectiveState() != StateApproved {
				t.Fatalf("VaultFact = %+v err=%v, want approved", fact, err)
			}
		})
	}
}

func TestRetroLessonWithoutReviewFlagStillRoutesRemote(t *testing.T) {
	base := withKnowledgeBaseDir(t)
	api, ingests := remoteProjectAPI(t)
	slug, created, err := api.IngestRetroLesson(context.Background(), RetroLesson{Lesson: repeatLessonText, SourceBead: "b-1", SourceDate: reviewLessonDate})
	if err != nil || !created || ingests.Load() != 1 {
		t.Fatalf("ingest = (%q, %v, %v) remote writes=%d", slug, created, err, ingests.Load())
	}
	if _, err := os.Stat(retroLessonFilePath(base, slug)); !os.IsNotExist(err) {
		t.Fatalf("lesson without the flag was written locally: %v", err)
	}
}

func TestRetroLessonReviewRepeats(t *testing.T) {
	approvedFixture := "---\ntitle: Retro lesson: Run targeted validation\ntype: pattern\nlayer: project\nconfidence: 0.70\ntags: [retro, lesson]\nstate: approved\nsource: retro bead:b-0\nsynthesized: 2026-01-01T00:00:00Z\n---\n\n" + repeatLessonText
	cases := []struct {
		name      string
		remote    bool
		approved  bool
		wantState string
	}{
		{"same lesson from two PRs stays one draft", false, false, "state: draft"},
		{"same lesson from two PRs with remote layer", true, false, "state: draft"},
		{"matches approved lesson", false, true, "state: approved"},
		{"matches approved lesson with remote layer", true, true, "state: approved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := withKnowledgeBaseDir(t)
			api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
			var ingests *atomic.Int32
			if tc.remote {
				api, ingests = remoteProjectAPI(t)
			}
			ctx := context.Background()
			slug := retroSlugFor(repeatLessonText)
			wantObs, wantConf := "observations: 2", "confidence: 0.75"

			if tc.approved {
				writeLessonFixture(t, api, base, slug, approvedFixture)
			} else {
				first, created, err := api.IngestRetroLesson(ctx, RetroLesson{Lesson: repeatLessonText, SourceBead: "b-1", SourcePR: "org/repo#1", SourceDate: reviewLessonDate, FromReviewFeedback: true})
				if err != nil || !created || first != slug {
					t.Fatalf("first ingest = (%q, %v, %v)", first, created, err)
				}
			}
			got, created, err := api.IngestRetroLesson(ctx, RetroLesson{Lesson: repeatLessonText, SourceBead: "b-2", SourcePR: "org/repo#2", SourceDate: reviewLessonDate, FromReviewFeedback: true})
			if err != nil || created || got != slug {
				t.Fatalf("review repeat = (%q, %v, %v), want (%q, false, nil)", got, created, err, slug)
			}
			after := readRetroLessonFile(t, base, slug)
			for key, want := range map[string]string{"observations": wantObs, "confidence": wantConf, "state": tc.wantState} {
				if line := frontmatterLine(after, key); line != want {
					t.Errorf("%s line = %q, want %q", key, line, want)
				}
			}
			if ingests != nil && ingests.Load() != 0 {
				t.Errorf("remote layer received %d writes, want 0", ingests.Load())
			}
			if n := countRetroLessonFiles(t, base); n != 1 {
				t.Errorf("lesson files = %d, want 1", n)
			}
		})
	}
}

func countRetroLessonFiles(t *testing.T, base string) int {
	t.Helper()
	entries, err := os.ReadDir(base + "/project")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if len(e.Name()) > 3 && e.Name()[len(e.Name())-3:] == ".md" {
			n++
		}
	}
	return n
}
