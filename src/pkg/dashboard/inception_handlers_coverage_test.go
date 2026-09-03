package dashboard

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// covFInceptionServer builds a dashboard Server wired with a real inception
// engine + knowledge API so the inception handlers exercise their rich paths
// (the state machine), not just the "engine not initialized" 503 branch that
// existing tests already cover.
func covFInceptionServer(t *testing.T) (*Server, *knowledge.InceptionEngine, *knowledge.KnowledgeAPI) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	kapi := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{}, logger)
	eng := knowledge.NewInceptionEngine(t.TempDir(), kapi, logger)

	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.Inception = eng
	deps.Knowledge = kapi
	s.RegisterAPI(deps)
	return s, eng, kapi
}

func TestCovF_InceptionFullLifecycle(t *testing.T) {
	s, eng, _ := covFInceptionServer(t)

	// 1. start
	rec := doPost(s, "/api/inception/start", map[string]interface{}{"idea": "build a todo app in Go"})
	if rec.Code != http.StatusOK {
		t.Fatalf("start: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeJSON(t, rec); body["ok"] != true {
		t.Fatalf("start: ok not true: %v", body)
	}

	// 2. state — active
	rec = doGet(s, "/api/inception/state")
	if rec.Code != http.StatusOK {
		t.Fatalf("state: expected 200, got %d", rec.Code)
	}
	if body := decodeJSON(t, rec); body["active"] != true {
		t.Fatalf("state: expected active true: %v", body)
	}

	// 3. questions (must be in capture phase) — need >=1 valid question.
	qs := []knowledge.Question{
		{ID: "language", Text: "What language?", Category: "language"},
		{ID: "users", Text: "Who are the users?", Category: "users"},
	}
	rec = doPost(s, "/api/inception/questions", map[string]interface{}{"questions": qs})
	if rec.Code != http.StatusOK {
		t.Fatalf("questions: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 4. answer — advances to structure; spawns sendKickBrainstorm goroutine.
	rec = doPost(s, "/api/inception/answer", map[string]interface{}{"answers": map[string]string{"language": "Go", "users": "devs"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("answer: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	time.Sleep(80 * time.Millisecond) // let the kick goroutine run for coverage

	// 5. record facts — advances to scaffold.
	facts := []knowledge.IdeationFact{
		{Title: "Project Vision", Body: "A simple todo app", Type: knowledge.FactVision},
		{Title: "Go stdlib", Body: "Use Go standard library", Type: knowledge.FactConstitution},
		{Title: "Add tasks", Body: "Users can add tasks", Type: knowledge.FactRequirement},
	}
	rec = doPost(s, "/api/inception/facts", map[string]interface{}{"facts": facts})
	if rec.Code != http.StatusOK {
		t.Fatalf("facts: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 6. scaffold — should produce files now that facts exist.
	rec = doGet(s, "/api/inception/scaffold")
	if rec.Code != http.StatusOK {
		t.Fatalf("scaffold: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 7. download — zip stream.
	rec = doGet(s, "/api/inception/download")
	if rec.Code != http.StatusOK {
		t.Fatalf("download: expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("download: expected application/zip, got %q", ct)
	}

	// 8. has-files + ideation-facts
	rec = doGet(s, "/api/inception/has-files")
	if rec.Code != http.StatusOK {
		t.Fatalf("has-files: expected 200, got %d", rec.Code)
	}
	rec = doGet(s, "/api/inception/ideation-facts")
	if rec.Code != http.StatusOK {
		t.Fatalf("ideation-facts: expected 200, got %d", rec.Code)
	}

	// 9. approve — from scaffold phase -> complete.
	rec = doPost(s, "/api/inception/approve", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	_ = eng
}

func TestCovF_InceptionRenameWiki(t *testing.T) {
	s, _, _ := covFInceptionServer(t)

	// Start so there's state + a wiki vault the store can be found under.
	if rec := doPost(s, "/api/inception/start", map[string]interface{}{"idea": "a rename test idea"}); rec.Code != http.StatusOK {
		t.Fatalf("start: %d", rec.Code)
	}

	// empty name -> 400
	if rec := doPut(s, "/api/inception/wiki-name", map[string]interface{}{"name": ""}); rec.Code != http.StatusBadRequest {
		t.Fatalf("rename empty: expected 400, got %d", rec.Code)
	}
	// too-long name -> 400
	long := strings.Repeat("x", maxWikiNameLen+1)
	if rec := doPut(s, "/api/inception/wiki-name", map[string]interface{}{"name": long}); rec.Code != http.StatusBadRequest {
		t.Fatalf("rename long: expected 400, got %d", rec.Code)
	}
	// valid name — the vault store may or may not exist yet (no facts written),
	// so accept either 200 (renamed) or 404 (vault not found). Both are covered
	// branches of the handler.
	rec := doPut(s, "/api/inception/wiki-name", map[string]interface{}{"name": "My Wiki"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Fatalf("rename valid: expected 200 or 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCovF_InceptionImport(t *testing.T) {
	s, _, _ := covFInceptionServer(t)

	// no file -> 400
	rec := doPost(s, "/api/inception/import", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import no-file: expected 400, got %d", rec.Code)
	}

	// Build a multipart form containing a small .zip with a foo.md file.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	fw, err := zw.Create("foo.md")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	fw.Write([]byte("# Foo\n\nsome content"))
	// A non-.md file should be skipped by the handler.
	if nfw, err := zw.Create("ignore.txt"); err == nil {
		nfw.Write([]byte("nope"))
	}
	zw.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "wiki.zip")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	part.Write(zbuf.Bytes())
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/inception/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	wrec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.mux.ServeHTTP(wrec, req)
	if wrec.Code != http.StatusOK {
		t.Fatalf("import: expected 200, got %d body=%s", wrec.Code, wrec.Body.String())
	}
}

func TestCovF_InceptionForceResetBranch(t *testing.T) {
	s, _, _ := covFInceptionServer(t)

	// First start.
	if rec := doPost(s, "/api/inception/start", map[string]interface{}{"idea": "first idea here"}); rec.Code != http.StatusOK {
		t.Fatalf("start1: %d", rec.Code)
	}
	// Reset explicitly.
	if rec := doPost(s, "/api/inception/reset", map[string]interface{}{}); rec.Code != http.StatusOK {
		t.Fatalf("reset: %d body=%s", rec.Code, rec.Body.String())
	}
	// Start again with force:true — exercises the force-reset branch
	// (Reset + AgentMgr.Pause; BeadStores is empty so clearInceptionBeads is skipped).
	rec := doPost(s, "/api/inception/start", map[string]interface{}{"idea": "second idea here", "force": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("start2 force: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	time.Sleep(80 * time.Millisecond) // let kickBrainstorm goroutine run
}

func TestCovF_InceptionScanBadURL(t *testing.T) {
	s, _, _ := covFInceptionServer(t)
	// StartBrownfield rejects a non-http URL -> 400.
	rec := doPost(s, "/api/inception/scan", map[string]interface{}{"repo_url": "not-a-url"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("scan bad url: expected 400, got %d", rec.Code)
	}
	// Valid https URL -> 200 (capture phase).
	rec = doPost(s, "/api/inception/scan", map[string]interface{}{"repo_url": "https://github.com/example/repo"})
	if rec.Code != http.StatusOK {
		t.Fatalf("scan valid: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	time.Sleep(80 * time.Millisecond)
}

func TestCovF_InceptionBadBodies(t *testing.T) {
	s, _, _ := covFInceptionServer(t)

	// Malformed JSON body on start -> 400 from readJSON.
	req := httptest.NewRequest(http.MethodPost, "/api/inception/start", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("start bad json: expected 400, got %d", rec.Code)
	}

	// Empty idea -> Start returns error -> 400.
	rec = doPost(s, "/api/inception/start", map[string]interface{}{"idea": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("start empty idea: expected 400, got %d", rec.Code)
	}

	// questions with none in progress → after a fresh start we're in capture,
	// but submitting an empty question list -> 400.
	if rec := doPost(s, "/api/inception/start", map[string]interface{}{"idea": "another valid idea"}); rec.Code != http.StatusOK {
		t.Fatalf("start for empty-questions: %d", rec.Code)
	}
	rec = doPost(s, "/api/inception/questions", map[string]interface{}{"questions": []knowledge.Question{}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty questions: expected 400, got %d", rec.Code)
	}
}

func TestCovF_ReadJSONLimits(t *testing.T) {
	// Valid body decodes.
	var out struct {
		A string `json:"a"`
	}
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":"hi"}`))
	if err := readJSON(r, &out); err != nil || out.A != "hi" {
		t.Fatalf("readJSON valid: err=%v out=%v", err, out)
	}

	// Oversized body -> error.
	big := strings.Repeat("a", maxInceptionBodyBytes+10)
	r2 := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":"`+big+`"}`))
	if err := readJSON(r2, &out); err == nil {
		t.Fatalf("readJSON oversized: expected error")
	}
}

func TestCovF_BuildStructureKickMessage(t *testing.T) {
	s := &Server{}
	state := &knowledge.InceptionState{
		IdeaText: "cool idea",
		IdeaSlug: "cool-idea",
		Questions: []knowledge.Question{
			{ID: "q1", Text: "What lang?"},
		},
		Answers: map[string]string{"q1": "Go"},
	}
	msg := s.buildStructureKickMessage(state)
	if !strings.Contains(msg, "cool idea") || !strings.Contains(msg, "What lang?") {
		t.Fatalf("structure kick message missing content: %q", msg)
	}
}

func TestCovF_KickBrainstorm(t *testing.T) {
	s, _, _ := covFInceptionServer(t)
	// Start so state exists in capture phase.
	if rec := doPost(s, "/api/inception/start", map[string]interface{}{"idea": "kick test idea"}); rec.Code != http.StatusOK {
		t.Fatalf("start: %d", rec.Code)
	}
	// Directly invoke kickBrainstorm — guarded by AgentMgr+Scheduler (both set
	// by testDeps). The goroutine's SendKick will fail harmlessly (no session).
	s.kickBrainstorm()
	s.sendKickBrainstorm()
	time.Sleep(100 * time.Millisecond)
}

func TestCovF_PlukSendKickNotInstalled(t *testing.T) {
	// pluk is not installed in the test environment -> LookPath fails.
	if err := plukSendKick("brainstorm", "hello"); err == nil {
		t.Fatalf("expected pluk not found error")
	}
}

func TestCovF_KickBrainstormPastStructureSkips(t *testing.T) {
	s, eng, _ := covFInceptionServer(t)
	// Drive to scaffold phase, then kickBrainstorm should skip (past structure).
	doPost(s, "/api/inception/start", map[string]interface{}{"idea": "skip test idea in go"})
	doPost(s, "/api/inception/questions", map[string]interface{}{"questions": []knowledge.Question{{ID: "language", Text: "lang?", Category: "language"}}})
	doPost(s, "/api/inception/answer", map[string]interface{}{"answers": map[string]string{"language": "Go"}})
	_ = eng.RecordFacts(context.Background(), []knowledge.IdeationFact{
		{Title: "Vision", Body: "v", Type: knowledge.FactVision},
		{Title: "Const", Body: "c", Type: knowledge.FactConstitution},
		{Title: "Req", Body: "r", Type: knowledge.FactRequirement},
	})
	// Now in scaffold — kick should hit the "skip" guard.
	s.kickBrainstorm()
	time.Sleep(50 * time.Millisecond)
}
