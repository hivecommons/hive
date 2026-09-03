package dashboard

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// covFInceptionServerWithBeads builds a test server with a real bead store
// under the "brainstorm" key so that clearInceptionBeads gets exercised.
func covFInceptionServerWithBeads(t *testing.T) (*Server, *knowledge.InceptionEngine, *beads.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	kapi := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{}, logger)
	eng := knowledge.NewInceptionEngine(t.TempDir(), kapi, logger)

	beadDir := t.TempDir()
	store, err := beads.NewStore(beadDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.Inception = eng
	deps.Knowledge = kapi
	deps.BeadStores = map[string]*beads.Store{"brainstorm": store}
	s.RegisterAPI(deps)
	return s, eng, store
}

func TestCovF_ClearInceptionBeadsOnForceStart(t *testing.T) {
	s, _, store := covFInceptionServerWithBeads(t)

	// Seed the bead store with open brainstorm/inception beads that should be closed.
	b1, err := store.Create("brainstorm bead 1", beads.TypeTask, beads.PriorityMedium, "brainstorm", "")
	if err != nil {
		t.Fatalf("create bead1: %v", err)
	}
	b2, err := store.Create("inception bead", beads.TypeAdvisory, beads.PriorityHigh, "inception", "")
	if err != nil {
		t.Fatalf("create bead2: %v", err)
	}
	// A bead with a different actor should NOT be closed.
	b3, err := store.Create("scanner bead", beads.TypeTask, beads.PriorityLow, "scanner", "")
	if err != nil {
		t.Fatalf("create bead3: %v", err)
	}

	// First start to get into an active state.
	rec := doPost(s, "/api/inception/start", map[string]interface{}{"idea": "clear beads test idea"})
	if rec.Code != http.StatusOK {
		t.Fatalf("start1: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Force start — this should call clearInceptionBeads and close b1 + b2.
	rec = doPost(s, "/api/inception/start", map[string]interface{}{
		"idea":  "second idea after force reset",
		"force": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("start2 force: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	time.Sleep(80 * time.Millisecond) // let kickBrainstorm goroutine run

	// Verify brainstorm/inception beads were closed.
	allBeads := store.List(beads.ListFilter{})
	for _, b := range allBeads {
		switch b.ID {
		case b1.ID, b2.ID:
			if b.Status != beads.StatusClosed {
				t.Errorf("bead %s (actor=%s) should be closed, got status=%s", b.ID, b.Actor, b.Status)
			}
		case b3.ID:
			if b.Status != beads.StatusOpen {
				t.Errorf("scanner bead %s should remain open, got status=%s", b.ID, b.Status)
			}
		}
	}
}

func TestCovF_ClearInceptionBeadsOnForceScan(t *testing.T) {
	s, _, store := covFInceptionServerWithBeads(t)

	// Seed beads.
	b1, err := store.Create("brainstorm finding", beads.TypeAdvisory, beads.PriorityHigh, "brainstorm", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Do a normal start first.
	rec := doPost(s, "/api/inception/start", map[string]interface{}{"idea": "scan test idea"})
	if rec.Code != http.StatusOK {
		t.Fatalf("start: expected 200, got %d", rec.Code)
	}

	// Force scan — exercises the force branch in handleInceptionScan.
	rec = doPost(s, "/api/inception/scan", map[string]interface{}{
		"repo_url": "https://github.com/example/repo",
		"force":    true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("scan force: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	time.Sleep(80 * time.Millisecond)

	// Verify bead was closed.
	allBeads := store.List(beads.ListFilter{})
	for _, b := range allBeads {
		if b.ID == b1.ID && b.Status != beads.StatusClosed {
			t.Errorf("brainstorm bead should be closed after force scan, got %s", b.Status)
		}
	}
}

func TestCovF_ClearInceptionBeadsOnReset(t *testing.T) {
	s, _, store := covFInceptionServerWithBeads(t)

	// Seed beads.
	b1, _ := store.Create("inception bead for reset", beads.TypeTask, beads.PriorityMedium, "inception", "")
	b2, _ := store.Create("other agent bead", beads.TypeTask, beads.PriorityMedium, "quality", "")

	// Start, then reset.
	doPost(s, "/api/inception/start", map[string]interface{}{"idea": "reset beads test"})
	rec := doPost(s, "/api/inception/reset", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	allBeads := store.List(beads.ListFilter{})
	for _, b := range allBeads {
		switch b.ID {
		case b1.ID:
			if b.Status != beads.StatusClosed {
				t.Errorf("inception bead should be closed on reset, got %s", b.Status)
			}
		case b2.ID:
			if b.Status != beads.StatusOpen {
				t.Errorf("quality bead should remain open, got %s", b.Status)
			}
		}
	}
}

func TestCovF_ClearInceptionBeadsSkipsNonOpen(t *testing.T) {
	s, _, store := covFInceptionServerWithBeads(t)

	// Create a bead and close it manually before force-start.
	b1, _ := store.Create("already closed bead", beads.TypeTask, beads.PriorityMedium, "brainstorm", "")
	_ = store.Close(b1.ID)

	doPost(s, "/api/inception/start", map[string]interface{}{"idea": "skip closed beads test"})
	rec := doPost(s, "/api/inception/start", map[string]interface{}{
		"idea":  "second idea",
		"force": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("force start: expected 200, got %d", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	// No panic, no error — already-closed bead should be skipped gracefully.
}

func TestCovF_KickBrainstormWithBeadStore(t *testing.T) {
	s, eng, _ := covFInceptionServerWithBeads(t)

	// Start and advance to structure phase to exercise sendKickBrainstorm path.
	doPost(s, "/api/inception/start", map[string]interface{}{"idea": "kick with beads test"})
	doPost(s, "/api/inception/questions", map[string]interface{}{
		"questions": []knowledge.Question{
			{ID: "lang", Text: "Language?", Category: "language"},
		},
	})
	rec := doPost(s, "/api/inception/answer", map[string]interface{}{
		"answers": map[string]string{"lang": "Python"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("answer: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	// Let sendKickBrainstorm goroutine run.
	time.Sleep(150 * time.Millisecond)

	// Record facts to get to scaffold phase.
	_ = eng.RecordFacts(context.Background(), []knowledge.IdeationFact{
		{Title: "Vision", Body: "a tool", Type: knowledge.FactVision},
		{Title: "Const", Body: "keep it simple", Type: knowledge.FactConstitution},
		{Title: "Req", Body: "do stuff", Type: knowledge.FactRequirement},
	})

	// kickBrainstorm should now skip (past structure phase).
	s.kickBrainstorm()
	time.Sleep(50 * time.Millisecond)
}
