package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
)

// fakeKnowledgeStore is an in-memory NousKnowledgeStore (#11697): the
// agent "writes a round" by calling publish, which flags the next
// LoadKnowledge as changed.
type fakeKnowledgeStore struct {
	mu      sync.Mutex
	k       NousKnowledge
	changed bool
	err     error
	loads   int
}

func (f *fakeKnowledgeStore) LoadKnowledge() (NousKnowledge, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	changed := f.changed
	f.changed = false
	return f.k, changed, f.err
}

func (f *fakeKnowledgeStore) publish(k NousKnowledge) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.k = k
	f.changed = true
}

func knowledgeTestServer(t *testing.T, store *fakeKnowledgeStore) (*Server, *Dependencies) {
	t.Helper()
	s, deps := apiServer(t)
	deps.Nous = &NousState{
		Mode:  "observe",
		Scope: "governor",
		Phase: nousPhaseCollecting,
		Status: map[string]interface{}{
			nousStatusKeyIterations:     0,
			nousStatusKeyPrinciples:     0,
			nousStatusKeyPrincipleCount: 0,
			nousStatusKeyPhase:          nousPhaseCollecting,
		},
		KnowledgeStore: store,
	}
	return s, deps
}

func decodeJSONList(t *testing.T, body string) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out
}

// A round the agent finishes after boot shows up on the very next
// /api/nous/ledger, /principles and /status without a restart, with the
// schema's type/status on every entry, and the status counts and phase
// follow the live content.
func TestNousKnowledgeServedLive(t *testing.T) {
	store := &fakeKnowledgeStore{}
	s, deps := knowledgeTestServer(t, store)

	rec := doGet(s, "/api/nous/ledger")
	if rec.Code != http.StatusOK || len(decodeJSONList(t, rec.Body.String())) != 0 {
		t.Fatalf("fresh ledger: %d %s", rec.Code, rec.Body.String())
	}
	status := decodeJSON(t, doGet(s, "/api/nous/status"))
	if status[nousStatusKeyPhase] != nousPhaseCollecting || status[nousStatusKeyIterations] != float64(0) {
		t.Fatalf("fresh status = %v", status)
	}

	store.publish(NousKnowledge{
		Ledger: []map[string]interface{}{
			{NousKeyType: NousLedgerTypeBaseline, NousKeyStatus: NousLedgerStatusCompleted, "family": "baseline"},
			{NousKeyType: NousLedgerTypeExperiment, NousKeyStatus: NousLedgerStatusCompleted, NousKeyOutcome: "confirmed", "iteration": 1},
		},
		Principles: []NousPrinciple{{ID: "p1", Text: "Prefer seams", Confidence: 0.9, Source: "meta", Type: NousPrincipleTypeMeta, Status: NousPrincipleStatusActive}},
	})

	ledger := decodeJSONList(t, doGet(s, "/api/nous/ledger").Body.String())
	if len(ledger) != 2 || ledger[0][NousKeyType] != NousLedgerTypeBaseline || ledger[1][NousKeyStatus] != NousLedgerStatusCompleted || ledger[1][NousKeyOutcome] != "confirmed" {
		t.Fatalf("live ledger = %v", ledger)
	}
	principles := decodeJSONList(t, doGet(s, "/api/nous/principles").Body.String())
	if len(principles) != 1 || principles[0]["type"] != NousPrincipleTypeMeta || principles[0]["status"] != NousPrincipleStatusActive || principles[0]["text"] != "Prefer seams" {
		t.Fatalf("live principles = %v", principles)
	}
	status = decodeJSON(t, doGet(s, "/api/nous/status"))
	if status[nousStatusKeyIterations] != float64(2) || status[nousStatusKeyPrinciples] != float64(1) || status[nousStatusKeyPrincipleCount] != float64(1) || status[nousStatusKeyPhase] != nousPhaseObserving {
		t.Fatalf("live status = %v", status)
	}
	if deps.Nous.Phase != nousPhaseObserving {
		t.Fatalf("phase = %q", deps.Nous.Phase)
	}

	// An unchanged store does not disturb the state; every read still asks.
	before := store.loads
	doGet(s, "/api/nous/ledger")
	doGet(s, "/api/nous/principles")
	if store.loads != before+2 {
		t.Fatalf("loads = %d, want %d (one per read)", store.loads, before+2)
	}
}

// A store error (half-written file) keeps serving the last good content
// and never turns a read into a 5xx.
func TestNousKnowledgeKeepsLastGoodOnError(t *testing.T) {
	store := &fakeKnowledgeStore{}
	s, _ := knowledgeTestServer(t, store)
	store.publish(NousKnowledge{Ledger: []map[string]interface{}{{NousKeyType: NousLedgerTypeDryRun, NousKeyStatus: NousLedgerStatusPending}}})
	if got := decodeJSONList(t, doGet(s, "/api/nous/ledger").Body.String()); len(got) != 1 {
		t.Fatalf("ledger = %v", got)
	}

	store.mu.Lock()
	store.err = errors.New("ledger.json: unexpected end of JSON input")
	store.mu.Unlock()
	rec := doGet(s, "/api/nous/ledger")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := decodeJSONList(t, rec.Body.String()); len(got) != 1 || got[0][NousKeyType] != NousLedgerTypeDryRun {
		t.Fatalf("after error ledger = %v, want last good copy", got)
	}
	if rec = doGet(s, "/api/nous/status"); rec.Code != http.StatusOK {
		t.Fatalf("status after error = %d", rec.Code)
	}
}

// Without a store (older wiring, or tests that seed Ledger directly) the
// handlers serve the boot snapshot exactly as before; a nil state is safe.
func TestNousKnowledgeWithoutStoreIsBootSnapshot(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{
		Status:     map[string]interface{}{},
		Ledger:     []map[string]interface{}{{"id": "boot"}},
		Principles: []NousPrinciple{{ID: "p0"}},
	}
	if err := deps.Nous.SyncKnowledge(); err != nil {
		t.Fatal(err)
	}
	if got := decodeJSONList(t, doGet(s, "/api/nous/ledger").Body.String()); len(got) != 1 || got[0]["id"] != "boot" {
		t.Fatalf("ledger = %v", got)
	}
	if got := decodeJSONList(t, doGet(s, "/api/nous/principles").Body.String()); len(got) != 1 || got[0]["id"] != "p0" {
		t.Fatalf("principles = %v", got)
	}
	var nilState *NousState
	if err := nilState.SyncKnowledge(); err != nil {
		t.Fatal(err)
	}
	// A state with a store but no Status map gets one rather than panicking.
	bare := &NousState{KnowledgeStore: &fakeKnowledgeStore{changed: true}}
	if err := bare.SyncKnowledge(); err != nil || bare.Status[nousStatusKeyIterations] != 0 {
		t.Fatalf("bare sync: %v %v", err, bare.Status)
	}
}

// The principle JSON shape the panel reads: id/text/confidence/source from
// before, plus type and status (#11697).
func TestNousPrincipleJSONCarriesTypeAndStatus(t *testing.T) {
	data, err := json.Marshal(NousPrinciple{ID: "p", Text: "t", Confidence: 0.4, Source: "domain", Type: NousPrincipleTypeDomain, Status: NousPrincipleStatusPruned})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"p","text":"t","confidence":0.4,"source":"domain","type":"domain","status":"pruned"}`
	if string(data) != want {
		t.Fatalf("json = %s, want %s", data, want)
	}
}
