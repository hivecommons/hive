package knowledge

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestNewInceptionEngineNoState(t *testing.T) {
	dir := t.TempDir()
	e := &InceptionEngine{
		dataDir: dir,
		logger:  slog.Default(),
	}
	e.loadState()
	e.connectExistingVault()

	if e.state != nil {
		t.Error("no state file should leave state nil")
	}
}

func TestGetStateNil(t *testing.T) {
	e := newTestEngine(t)
	if e.GetState() != nil {
		t.Error("initial state should be nil")
	}
}

func TestStartGreenfield(t *testing.T) {
	e := newTestEngine(t)
	state, err := e.Start("build a CLI tool for managing tasks")
	if err != nil {
		t.Fatalf("Start error: %v", err)
	}
	if state == nil {
		t.Fatal("state should not be nil")
	}
	if state.Phase != PhaseCapture {
		t.Errorf("phase = %q, want %q", state.Phase, PhaseCapture)
	}
	if state.Mode != InceptionGreenfield {
		t.Errorf("mode = %q, want %q", state.Mode, InceptionGreenfield)
	}
	if state.IdeaText != "build a CLI tool for managing tasks" {
		t.Errorf("idea text = %q", state.IdeaText)
	}
	if state.IdeaSlug == "" {
		t.Error("idea slug should be set")
	}
}

func TestStartDuplicate(t *testing.T) {
	e := newTestEngine(t)
	e.Start("first idea")
	_, err := e.Start("second idea")
	if err == nil {
		t.Error("starting while active should error")
	}
}

func TestStartBrownfield(t *testing.T) {
	e := newTestEngine(t)
	state, err := e.StartBrownfield("https://github.com/org/repo")
	if err != nil {
		t.Fatalf("StartBrownfield error: %v", err)
	}
	if state.Mode != InceptionBrownfield {
		t.Errorf("mode = %q, want %q", state.Mode, InceptionBrownfield)
	}
	if state.RepoURL != "https://github.com/org/repo" {
		t.Errorf("repo URL = %q", state.RepoURL)
	}
}

func TestStartBrownfieldDuplicate(t *testing.T) {
	e := newTestEngine(t)
	e.StartBrownfield("https://github.com/org/repo")
	_, err := e.StartBrownfield("https://github.com/org/repo2")
	if err == nil {
		t.Error("starting while active should error")
	}
}

func TestSetQuestions(t *testing.T) {
	e := newTestEngine(t)
	e.Start("my idea")

	questions := []Question{
		{ID: "language", Text: "What language?", Category: "language"},
		{ID: "users", Text: "Who uses it?", Category: "users"},
	}
	err := e.SetQuestions(questions)
	if err != nil {
		t.Fatalf("SetQuestions error: %v", err)
	}

	state := e.GetState()
	if len(state.Questions) != 2 {
		t.Errorf("expected 2 questions, got %d", len(state.Questions))
	}
}

func TestSetQuestionsNoState(t *testing.T) {
	e := newTestEngine(t)
	err := e.SetQuestions([]Question{{ID: "q1", Text: "test"}})
	if err == nil {
		t.Error("should error with no active inception")
	}
}

func TestSubmitAnswers(t *testing.T) {
	e := newTestEngine(t)
	e.Start("my idea")
	e.SetQuestions([]Question{
		{ID: "language", Text: "What language?"},
	})

	answers := map[string]string{"language": "Go"}
	_, err := e.SubmitAnswers(answers)
	if err != nil {
		t.Fatalf("SubmitAnswers error: %v", err)
	}

	state := e.GetState()
	if state.Answers["language"] != "Go" {
		t.Errorf("answer = %q", state.Answers["language"])
	}
}

func TestSubmitAnswersNoState(t *testing.T) {
	e := newTestEngine(t)
	_, err := e.SubmitAnswers(map[string]string{"q1": "a1"})
	if err == nil {
		t.Error("should error with no active inception")
	}
}

func TestAdvanceToCompleteNoState(t *testing.T) {
	e := newTestEngine(t)
	err := e.AdvanceToComplete()
	if err == nil {
		t.Error("should error with no active inception")
	}
}

func TestAdvanceToCompleteWrongPhase(t *testing.T) {
	e := newTestEngine(t)
	e.Start("my idea")
	err := e.AdvanceToComplete()
	if err == nil {
		t.Error("should error in questions phase")
	}
}

func TestSetWikiName(t *testing.T) {
	e := newTestEngine(t)
	e.Start("my idea")
	e.SetWikiName("my-cool-wiki")

	state := e.GetState()
	if state.WikiName != "my-cool-wiki" {
		t.Errorf("wiki name = %q", state.WikiName)
	}
}

func TestSetWikiNameNoState(t *testing.T) {
	e := newTestEngine(t)
	e.SetWikiName("test")
}

func TestIncrementAutoFactCount(t *testing.T) {
	e := newTestEngine(t)
	e.Start("my idea")
	e.IncrementAutoFactCount(3)
	e.IncrementAutoFactCount(2)

	state := e.GetState()
	if state.AutoFactCount != 5 {
		t.Errorf("auto fact count = %d, want 5", state.AutoFactCount)
	}
}

func TestIncrementAutoFactCountNoState(t *testing.T) {
	e := newTestEngine(t)
	e.IncrementAutoFactCount(1)
}

func TestIncrementAutoQuestionCount(t *testing.T) {
	e := newTestEngine(t)
	e.Start("my idea")
	e.IncrementAutoQuestionCount(4)

	state := e.GetState()
	if state.AutoQuestionCount != 4 {
		t.Errorf("auto question count = %d, want 4", state.AutoQuestionCount)
	}
}

func TestHasWikiFilesEmpty(t *testing.T) {
	e := newTestEngine(t)
	if e.HasWikiFiles() {
		t.Error("empty dir should have no wiki files")
	}
}

func TestHasWikiFilesPresent(t *testing.T) {
	e := newTestEngine(t)
	wikiDir := filepath.Join(e.dataDir, inceptionWikiDir)
	os.MkdirAll(wikiDir, 0755)
	os.WriteFile(filepath.Join(wikiDir, "fact.md"), []byte("# Fact"), 0644)

	if !e.HasWikiFiles() {
		t.Error("should detect .md files in wiki dir")
	}
}

func TestHasWikiFilesNonMD(t *testing.T) {
	e := newTestEngine(t)
	wikiDir := filepath.Join(e.dataDir, inceptionWikiDir)
	os.MkdirAll(wikiDir, 0755)
	os.WriteFile(filepath.Join(wikiDir, "readme.txt"), []byte("text"), 0644)

	if e.HasWikiFiles() {
		t.Error("non-.md files should not count")
	}
}

func TestReset(t *testing.T) {
	e := newTestEngine(t)
	e.Start("my idea")

	if e.GetState() == nil {
		t.Fatal("state should be set after Start")
	}

	err := e.Reset()
	if err != nil {
		t.Fatalf("Reset error: %v", err)
	}
	if e.GetState() != nil {
		t.Error("state should be nil after Reset")
	}
}

func TestResetNoState(t *testing.T) {
	e := newTestEngine(t)
	err := e.Reset()
	if err != nil {
		t.Errorf("Reset with no state should not error, got %v", err)
	}
}

func TestSaveAndLoadState(t *testing.T) {
	e := newTestEngine(t)
	e.Start("test idea")
	e.SetWikiName("test-wiki")

	e2 := &InceptionEngine{
		dataDir: e.dataDir,
		logger:  slog.Default(),
	}
	e2.loadState()

	if e2.state == nil {
		t.Fatal("loaded state should not be nil")
	}
	if e2.state.IdeaText != "test idea" {
		t.Errorf("idea text = %q", e2.state.IdeaText)
	}
	if e2.state.WikiName != "test-wiki" {
		t.Errorf("wiki name = %q", e2.state.WikiName)
	}
}

func TestLoadStateInvalidJSON(t *testing.T) {
	e := newTestEngine(t)
	statePath := filepath.Join(e.dataDir, inceptionStateFile)
	os.MkdirAll(filepath.Dir(statePath), 0755)
	os.WriteFile(statePath, []byte("{bad json"), 0644)

	e.loadState()
	if e.state != nil {
		t.Error("invalid JSON should leave state nil")
	}
}

func TestReadIdeaFactNoState(t *testing.T) {
	e := newTestEngine(t)
	_, err := e.ReadIdeaFact(nil)
	if err == nil {
		t.Error("should error with no active inception")
	}
}

func TestReadIdeaFactNoAPI(t *testing.T) {
	e := newTestEngine(t)
	e.Start("my idea")
	fact, err := e.ReadIdeaFact(nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if fact != nil {
		t.Error("nil API should return nil fact")
	}
}
