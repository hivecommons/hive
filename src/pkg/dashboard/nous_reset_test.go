package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeNousResetStore records calls and returns scripted counts/errors.
type fakeNousResetStore struct {
	calls                   []NousResetScope
	counts                  NousResetCounts
	err                     error
	knowledgeAfterReset     *NousKnowledge
	knowledgeStoreToRefresh *fakeNousKnowledgeStore
}

func (f *fakeNousResetStore) Reset(scope NousResetScope) (NousResetCounts, error) {
	f.calls = append(f.calls, scope)
	if f.err != nil {
		return NousResetCounts{}, f.err
	}
	if scope.Knowledge && f.knowledgeStoreToRefresh != nil && f.knowledgeAfterReset != nil {
		f.knowledgeStoreToRefresh.k = *f.knowledgeAfterReset
		f.knowledgeStoreToRefresh.changed = true
	}
	var c NousResetCounts
	if scope.Baseline {
		c.Snapshots = f.counts.Snapshots
	}
	if scope.Knowledge {
		c.LedgerEntries, c.Principles = f.counts.LedgerEntries, f.counts.Principles
	}
	return c, nil
}

func (f *fakeNousResetStore) scopes() string {
	var out []string
	for _, c := range f.calls {
		out = append(out, fmt.Sprintf("b=%t,k=%t,g=%t", c.Baseline, c.Knowledge, c.Gates))
	}
	return strings.Join(out, ";")
}

// fakeNousKnowledgeStore serves a fixed knowledge payload, reporting a
// change once per update.
type fakeNousKnowledgeStore struct {
	k       NousKnowledge
	changed bool
}

func (f *fakeNousKnowledgeStore) LoadKnowledge() (NousKnowledge, bool, error) {
	ch := f.changed
	f.changed = false
	return f.k, ch, nil
}

func resetServer(t *testing.T) (*Server, *Dependencies, *fakeNousResetStore) {
	t.Helper()
	s, deps := apiServerWithNous(t)
	store := &fakeNousResetStore{counts: NousResetCounts{Snapshots: 75110, LedgerEntries: 4, Principles: 2}}
	deps.Nous.ResetStore = store
	deps.Nous.Mode = "suggest"
	deps.Nous.Phase = nousPhaseObserving
	deps.Nous.Status = map[string]interface{}{
		"snapshots": 75110, "snapshotCount": 75110, "baseline_pct": 100.0,
		"iterations": 4, "principles": 2, "principleCount": 2, "mode": "suggest",
	}
	return s, deps, store
}

func decodeReset(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return m
}

func TestNousResetRequiresOwner(t *testing.T) {
	s, _, store := resetServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/nous/reset", strings.NewReader(`{"scope":"all","confirm":"RESET"}`))
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || len(store.calls) != 0 {
		t.Fatalf("anonymous reset = %d calls=%v", rec.Code, store.calls)
	}
}

func TestNousResetNotConfigured(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doPostOwner(s, "/api/nous/reset", map[string]string{"scope": "all", "confirm": "RESET"}); rec.Code != http.StatusNotFound {
		t.Fatalf("nil nous = %d", rec.Code)
	}
	s2, _ := apiServerWithNous(t)
	if rec := doPostOwner(s2, "/api/nous/reset", map[string]string{"scope": "all", "confirm": "RESET"}); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "reset store") {
		t.Fatalf("nil store = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNousResetBadInput(t *testing.T) {
	s, _, store := resetServer(t)
	for name, body := range map[string]interface{}{
		"bad scope":       map[string]string{"scope": "everything", "confirm": "RESET"},
		"missing scope":   map[string]string{"confirm": "RESET"},
		"missing confirm": map[string]string{"scope": "all"},
		"wrong confirm":   map[string]string{"scope": "all", "confirm": "reset"},
		"not an object":   "nope",
	} {
		rec := doPostOwner(s, "/api/nous/reset", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d body=%s", name, rec.Code, rec.Body.String())
		}
	}
	if len(store.calls) != 0 {
		t.Fatalf("store touched on bad input: %v", store.calls)
	}
}

func TestNousResetRefusesPartialWhileActive(t *testing.T) {
	s, deps, store := resetServer(t)
	deps.Nous.Ledger = []map[string]interface{}{
		{"id": "r1", NousKeyStatus: NousLedgerStatusCompleted},
		{"id": "r2", NousKeyStatus: NousLedgerStatusActive},
	}
	for _, scope := range []string{NousResetScopeBaseline, NousResetScopeKnowledge} {
		rec := doPostOwner(s, "/api/nous/reset", map[string]string{"scope": scope, "confirm": "RESET"})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "abort") {
			t.Fatalf("%s while active = %d body=%s", scope, rec.Code, rec.Body.String())
		}
	}
	if len(store.calls) != 0 {
		t.Fatalf("store touched while active: %v", store.calls)
	}
	if e := lastAudit(t, s, nousAuditResetFailed); !strings.Contains(e.Detail, "scope="+NousResetScopeKnowledge) || !strings.Contains(e.Detail, "active") {
		t.Fatalf("failed audit = %q", e.Detail)
	}
	// "all" is allowed even while active: the experiment goes with it.
	rec := doPostOwner(s, "/api/nous/reset", map[string]string{"scope": "all", "confirm": "RESET"})
	if rec.Code != http.StatusOK {
		t.Fatalf("all while active = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNousResetBaseline(t *testing.T) {
	s, deps, store := resetServer(t)
	persisted := 0
	deps.Nous.PersistPanel = func(NousPanelSnapshot) error { persisted++; return nil }
	rec := doPostOwner(s, "/api/nous/reset", map[string]string{"scope": "baseline", "confirm": "RESET"})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeReset(t, rec.Body.String())
	if got["ok"] != true || got["scope"] != "baseline" || got["removed_snapshots"] != 75110.0 || got["removed_ledger_entries"] != 0.0 || got["removed_principles"] != 0.0 || got["mode"] != nousModeObserve {
		t.Fatalf("body = %v", got)
	}
	if store.scopes() != "b=true,k=false,g=false" {
		t.Fatalf("calls = %s", store.scopes())
	}
	ns := deps.Nous
	if ns.Mode != nousModeObserve || ns.Phase != nousPhaseCollecting {
		t.Fatalf("mode/phase = %s/%s", ns.Mode, ns.Phase)
	}
	if ns.Status["snapshots"] != 0 || ns.Status["snapshotCount"] != 0 || ns.Status["baseline_pct"] != 0.0 || ns.Status["mode"] != nousModeObserve || ns.Status["phase"] != nousPhaseCollecting {
		t.Fatalf("status = %v", ns.Status)
	}
	// Knowledge is untouched by a baseline reset.
	if ns.Status["iterations"] != 4 || len(ns.Principles) != 2 {
		t.Fatalf("knowledge touched: %v / %d", ns.Status["iterations"], len(ns.Principles))
	}
	if persisted != 1 {
		t.Fatalf("panel persisted %d times, want 1 (mode changed)", persisted)
	}
	e := lastAudit(t, s, nousAuditReset)
	for _, want := range []string{"scope=baseline", "removed_snapshots=75110", "mode=observe"} {
		if !strings.Contains(e.Detail, want) {
			t.Fatalf("audit %q missing %q", e.Detail, want)
		}
	}
}

func TestNousResetKnowledge(t *testing.T) {
	s, deps, store := resetServer(t)
	ks := &fakeNousKnowledgeStore{k: NousKnowledge{
		Ledger:     []map[string]interface{}{{"id": "r1"}, {"id": "r2"}, {"id": "r3"}, {"id": "r4"}},
		Principles: []NousPrinciple{{ID: "p1"}, {ID: "p2"}},
	}, changed: true}
	deps.Nous.KnowledgeStore = ks
	store.knowledgeStoreToRefresh = ks
	store.knowledgeAfterReset = &NousKnowledge{}
	persisted := 0
	deps.Nous.PersistPanel = func(NousPanelSnapshot) error { persisted++; return nil }

	rec := doPostOwner(s, "/api/nous/reset", map[string]string{"scope": "knowledge", "confirm": "RESET"})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeReset(t, rec.Body.String())
	if got["removed_snapshots"] != 0.0 || got["removed_ledger_entries"] != 4.0 || got["removed_principles"] != 2.0 || got["mode"] != "suggest" {
		t.Fatalf("body = %v", got)
	}
	if store.scopes() != "b=false,k=true,g=false" {
		t.Fatalf("calls = %s", store.scopes())
	}
	ns := deps.Nous
	if len(ns.Ledger) != 0 || len(ns.Principles) != 0 || ns.Status["iterations"] != 0 || ns.Status["principles"] != 0 || ns.Status["principleCount"] != 0 {
		t.Fatalf("knowledge not emptied: %d/%d %v", len(ns.Ledger), len(ns.Principles), ns.Status)
	}
	// Baseline and mode are untouched by a knowledge reset.
	if ns.Status["snapshots"] != 75110 || ns.Mode != "suggest" || persisted != 0 {
		t.Fatalf("baseline touched: snapshots=%v mode=%s persisted=%d", ns.Status["snapshots"], ns.Mode, persisted)
	}
	if e := lastAudit(t, s, nousAuditReset); !strings.Contains(e.Detail, "removed_ledger_entries=4") || !strings.Contains(e.Detail, "removed_principles=2") {
		t.Fatalf("audit = %q", e.Detail)
	}
}

func TestNousResetAll(t *testing.T) {
	s, deps, store := resetServer(t)
	rec := doPostOwner(s, "/api/nous/reset", map[string]string{"scope": "all", "confirm": "RESET"})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeReset(t, rec.Body.String())
	if got["removed_snapshots"] != 75110.0 || got["removed_ledger_entries"] != 4.0 || got["removed_principles"] != 2.0 || got["mode"] != nousModeObserve {
		t.Fatalf("body = %v", got)
	}
	if store.scopes() != "b=true,k=true,g=true" {
		t.Fatalf("calls = %s", store.scopes())
	}
	ns := deps.Nous
	if ns.GatePending != nil || ns.GateResponse != nil {
		t.Fatalf("gates not cleared: %v / %v", ns.GatePending, ns.GateResponse)
	}
	// Panel configuration is settings, not data: never reset.
	if ns.Config["key"] != "value" || ns.Scope != "full" {
		t.Fatalf("config/scope touched: %v / %s", ns.Config, ns.Scope)
	}
}

func TestNousResetStoreErrors(t *testing.T) {
	s, deps, store := resetServer(t)
	store.err = errors.New("disk full")
	rec := doPostOwner(s, "/api/nous/reset", map[string]string{"scope": "baseline", "confirm": "RESET"})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "reset baseline: disk full") {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if e := lastAudit(t, s, nousAuditResetFailed); !strings.Contains(e.Detail, "disk full") {
		t.Fatalf("audit = %q", e.Detail)
	}
	// Nothing in memory moved.
	if deps.Nous.Mode != "suggest" || deps.Nous.Status["snapshots"] != 75110 {
		t.Fatalf("state changed after a failed reset: %s %v", deps.Nous.Mode, deps.Nous.Status)
	}
}

func TestNousResetNilStatusMaps(t *testing.T) {
	s, deps, _ := resetServer(t)
	deps.Nous.Status = nil
	rec := doPostOwner(s, "/api/nous/reset", map[string]string{"scope": "all", "confirm": "RESET"})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if deps.Nous.Status["snapshots"] != 0 || deps.Nous.Status["iterations"] != 0 {
		t.Fatalf("status = %v", deps.Nous.Status)
	}
}

func TestValidNousResetScope(t *testing.T) {
	for _, ok := range []string{NousResetScopeBaseline, NousResetScopeKnowledge, NousResetScopeAll} {
		if !ValidNousResetScope(ok) {
			t.Fatalf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", "ALL", "everything", "gates"} {
		if ValidNousResetScope(bad) {
			t.Fatalf("%q must be invalid", bad)
		}
	}
}
