package knowledge

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordFactsSuccess(t *testing.T) {
	e := newTestEngine(t)
	e.Start("my project idea")

	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	e.SetQuestions([]Question{{ID: "lang", Text: "What language?"}})

	e.mu.Lock()
	e.state.Phase = PhaseClarify
	e.mu.Unlock()
	e.SubmitAnswers(map[string]string{"lang": "Go"})

	facts := []IdeationFact{
		{Title: "Project Vision", Body: "Build a CLI tool", Type: FactVision},
		{Title: "Main Requirement", Body: "Must be fast", Type: FactRequirement},
	}

	err := e.RecordFacts(context.Background(), facts)
	if err != nil {
		t.Fatalf("RecordFacts error: %v", err)
	}

	state := e.GetState()
	if state.Phase != PhaseScaffold {
		t.Errorf("phase = %q, want scaffold", state.Phase)
	}
	if len(state.FactSlugs) < 2 {
		t.Errorf("fact slugs = %d, want at least 2", len(state.FactSlugs))
	}
}

func TestRecordFactsNoState(t *testing.T) {
	e := newTestEngine(t)
	err := e.RecordFacts(context.Background(), []IdeationFact{
		{Title: "Test", Body: "Body", Type: FactVision},
	})
	if err == nil {
		t.Error("should error with no active inception")
	}
}

func TestRecordFactsEmpty(t *testing.T) {
	e := newTestEngine(t)
	e.Start("idea")
	err := e.RecordFacts(context.Background(), nil)
	if err == nil {
		t.Error("should error with empty facts")
	}
}

func TestRecordFactsInvalidType(t *testing.T) {
	e := newTestEngine(t)
	e.Start("idea")
	err := e.RecordFacts(context.Background(), []IdeationFact{
		{Title: "Test", Body: "Body", Type: FactPattern},
	})
	if err == nil {
		t.Error("non-ideation type should error")
	}
}

func TestRecordFactsMissingTitle(t *testing.T) {
	e := newTestEngine(t)
	e.Start("idea")
	err := e.RecordFacts(context.Background(), []IdeationFact{
		{Title: "", Body: "Body", Type: FactVision},
	})
	if err == nil {
		t.Error("empty title should error")
	}
}

func TestRecordFactsMissingBody(t *testing.T) {
	e := newTestEngine(t)
	e.Start("idea")
	err := e.RecordFacts(context.Background(), []IdeationFact{
		{Title: "Test", Body: "", Type: FactVision},
	})
	if err == nil {
		t.Error("empty body should error")
	}
}

func TestRecordFactsBrownfieldBoost(t *testing.T) {
	e := newTestEngine(t)
	e.StartBrownfield("https://github.com/org/repo")

	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	facts := []IdeationFact{
		{Title: "Vision", Body: "Existing project", Type: FactVision},
	}
	err := e.RecordFacts(context.Background(), facts)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
}

func TestGatherFactsPublicEmpty(t *testing.T) {
	e := newTestEngine(t)
	facts := e.GatherFactsPublic(context.Background())
	if len(facts) != 0 {
		t.Errorf("no wiki dir should return empty, got %d", len(facts))
	}
}

func TestGatherFactsPublicFromFiles(t *testing.T) {
	e := newTestEngine(t)
	wikiDir := filepath.Join(e.dataDir, inceptionWikiDir)
	os.MkdirAll(wikiDir, 0755)

	fact1 := `---
title: Project Vision
type: vision
confidence: 0.8
---
Build an awesome CLI tool.`

	fact2 := `---
title: Main Requirement
type: requirement
confidence: 0.7
---
Must be fast and reliable.`

	os.WriteFile(filepath.Join(wikiDir, "vision-project.md"), []byte(fact1), 0644)
	os.WriteFile(filepath.Join(wikiDir, "req-main.md"), []byte(fact2), 0644)
	os.WriteFile(filepath.Join(wikiDir, "not-a-fact.txt"), []byte("ignore"), 0644)
	os.Mkdir(filepath.Join(wikiDir, "subdir"), 0755)

	facts := e.GatherFactsPublic(context.Background())
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}

	visionFound := false
	for _, f := range facts {
		if f.Type == FactVision {
			visionFound = true
			if f.Title != "Project Vision" {
				t.Errorf("vision title = %q", f.Title)
			}
			if f.Confidence != 0.8 {
				t.Errorf("vision confidence = %f", f.Confidence)
			}
		}
	}
	if !visionFound {
		t.Error("vision fact should be found")
	}
}

func TestProduceScaffoldNoState(t *testing.T) {
	e := newTestEngine(t)
	_, err := e.ProduceScaffold(context.Background())
	if err == nil {
		t.Error("should error with no active inception")
	}
}

func TestProduceScaffoldGreenfield(t *testing.T) {
	e := newTestEngine(t)
	e.Start("build a Go CLI tool for task management")

	wikiDir := filepath.Join(e.dataDir, inceptionWikiDir)
	os.MkdirAll(wikiDir, 0755)

	visionFact := `---
title: Task Manager
type: vision
confidence: 0.8
---
A CLI tool for managing tasks efficiently.`

	constFact := `---
title: Go Architecture
type: constitution
confidence: 0.7
---
Go microservice with cobra CLI framework.`

	os.WriteFile(filepath.Join(wikiDir, "vision-task.md"), []byte(visionFact), 0644)
	os.WriteFile(filepath.Join(wikiDir, "const-arch.md"), []byte(constFact), 0644)

	e.mu.Lock()
	e.state.Phase = PhaseScaffold
	e.mu.Unlock()

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result == nil {
		t.Fatal("result should not be nil")
	}
	if len(result.Files) == 0 {
		t.Error("should produce at least one file")
	}

	hasReadme := false
	for _, f := range result.Files {
		if f.Path == "README.md" {
			hasReadme = true
		}
	}
	if !hasReadme {
		t.Error("scaffold should include README.md")
	}
}

func TestClearWikiVault(t *testing.T) {
	e := newTestEngine(t)
	wikiDir := filepath.Join(e.dataDir, inceptionWikiDir)
	os.MkdirAll(wikiDir, 0755)
	os.WriteFile(filepath.Join(wikiDir, "fact.md"), []byte("test"), 0644)
	os.WriteFile(filepath.Join(wikiDir, "other.txt"), []byte("keep"), 0644)

	e.clearWikiVault()

	entries, _ := os.ReadDir(wikiDir)
	for _, entry := range entries {
		if entry.Name() == "fact.md" {
			t.Error("clearWikiVault should remove .md files")
		}
	}
}

func TestWriteFactsToVault(t *testing.T) {
	e := newTestEngine(t)
	facts := []IdeationFact{
		{Title: "Vision", Body: "Build something", Type: FactVision},
		{Title: "Requirement", Body: "Must be fast", Type: FactRequirement},
	}

	e.writeFactsToVault(facts)

	wikiDir := filepath.Join(e.dataDir, inceptionWikiDir)
	entries, err := os.ReadDir(wikiDir)
	if err != nil {
		t.Fatalf("wiki dir should exist: %v", err)
	}

	mdCount := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".md" {
			mdCount++
		}
	}
	if mdCount != 2 {
		t.Errorf("expected 2 .md files, got %d", mdCount)
	}
}

func TestRecordFactsRequiresConfirmationForProposedTranscriptFacts(t *testing.T) {
	e := newTestEngine(t)
	if _, err := e.Start("transcript idea"); err != nil {
		t.Fatalf("start: %v", err)
	}
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	err := e.RecordFacts(context.Background(), []IdeationFact{
		{
			Title:     "Agreed option",
			Body:      "The team agreed to ship email alerts.",
			Type:      FactRequirement,
			Proposed:  true,
			SourceRef: "transcripts/meeting.txt:3-5",
		},
	})
	if err == nil {
		t.Fatal("unconfirmed proposed fact should not be recorded")
	}
	state := e.GetState()
	if state.Phase != PhaseStructure {
		t.Fatalf("phase = %s, want structure", state.Phase)
	}
	if len(state.ProposedFacts) != 1 {
		t.Fatalf("proposed facts = %d, want 1", len(state.ProposedFacts))
	}
	if len(state.FactSlugs) != 1 {
		t.Fatalf("fact slugs = %d, want only seed fact", len(state.FactSlugs))
	}
}

func TestRecordFactsWritesSourceRefForConfirmedTranscriptFact(t *testing.T) {
	e := newTestEngine(t)
	if _, err := e.Start("transcript source refs"); err != nil {
		t.Fatalf("start: %v", err)
	}
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	err := e.RecordFacts(context.Background(), []IdeationFact{
		{
			Title:     "Confirmed option",
			Body:      "Use SSO for administrators.",
			Type:      FactRequirement,
			Proposed:  true,
			Confirmed: true,
			SourceRef: "transcripts/meeting.md:8-10",
		},
	})
	if err != nil {
		t.Fatalf("RecordFacts: %v", err)
	}
	wikiDir := filepath.Join(e.dataDir, inceptionWikiDir)
	files, err := os.ReadDir(wikiDir)
	if err != nil {
		t.Fatalf("read wiki dir: %v", err)
	}
	var content []byte
	for _, entry := range files {
		if strings.Contains(entry.Name(), "confirmed-option") {
			content, err = os.ReadFile(filepath.Join(wikiDir, entry.Name()))
			if err != nil {
				t.Fatalf("read fact: %v", err)
			}
			break
		}
	}
	if !strings.Contains(string(content), "source_ref: transcripts/meeting.md:8-10") {
		t.Fatalf("fact file missing source_ref citation:\n%s", string(content))
	}
	if !strings.Contains(string(content), "> Source: transcripts/meeting.md:8-10") {
		t.Fatalf("fact file missing source quote citation:\n%s", string(content))
	}
}

func TestImportTranscriptScrubsAndStoresRawDocument(t *testing.T) {
	e := newTestEngine(t)
	if _, err := e.Start("transcript import"); err != nil {
		t.Fatalf("start: %v", err)
	}
	doc, err := e.ImportTranscript("../meeting.txt", []byte("Authorization: Bearer abcdefghijklmnopqrst\nAgreed option\nDiscarded option"))
	if err != nil {
		t.Fatalf("ImportTranscript: %v", err)
	}
	if doc.Path != "transcripts/meeting.txt" || doc.Lines != 3 {
		t.Fatalf("doc = %+v, want sanitized path with 3 lines", doc)
	}
	content, err := os.ReadFile(filepath.Join(e.dataDir, inceptionWikiDir, doc.Path))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if strings.Contains(string(content), "abcdefghijklmnopqrst") {
		t.Fatalf("transcript was not scrubbed: %q", string(content))
	}
}

func TestBuildK8sConfigMap(t *testing.T) {
	got := buildK8sConfigMap("myproject")
	if got == "" {
		t.Error("should produce output")
	}
}

func TestBuildK8sSecret(t *testing.T) {
	got := buildK8sSecret("myproject")
	if got == "" {
		t.Error("should produce output")
	}
}

func TestBuildK8sBaseKustomization(t *testing.T) {
	got := buildK8sBaseKustomization()
	if got == "" {
		t.Error("should produce output")
	}
}

func TestBuildK8sOverlayKustomization(t *testing.T) {
	got := buildK8sOverlayKustomization("myproject", "dev")
	if got == "" {
		t.Error("should produce output")
	}
}

func TestBuildNightlyCI(t *testing.T) {
	got := buildNightlyCI("go")
	if got == "" {
		t.Error("should produce output")
	}
}

func TestBuildMakefile(t *testing.T) {
	got := buildMakefile("go", "myproject", "cli")
	if got == "" {
		t.Error("should produce output")
	}
}

func TestBuildI18nFile(t *testing.T) {
	got := buildI18nFile("en", "myproject")
	if got == "" {
		t.Error("should produce output")
	}
}

func TestBuildI18nIndex(t *testing.T) {
	got := buildI18nIndex()
	if got == "" {
		t.Error("should produce output")
	}
}

func TestNewInceptionEngineWithDir(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, slog.Default())
	if e == nil {
		t.Fatal("engine should not be nil")
	}
	if e.GetState() != nil {
		t.Error("fresh engine should have nil state")
	}
}
