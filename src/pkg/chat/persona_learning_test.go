package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/persona"
)

var learningTestClock = time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)

type recordingAuditSink struct {
	mu      sync.Mutex
	entries []auditEntry
}

type auditEntry struct {
	actor, action, agent string
	fields               map[string]any
}

func (r *recordingAuditSink) Record(actor, action, agentName string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, auditEntry{actor: actor, action: action, agent: agentName, fields: fields})
}

func (r *recordingAuditSink) snapshot() []auditEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]auditEntry(nil), r.entries...)
}

// learningRunsServer serves one run at /api/runs/<key> and accepts plan
// approve/reject posts, so signals can be driven through the real commands.
func learningRunsServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case strings.HasPrefix(path, "/api/runs/"):
			key := strings.TrimPrefix(path, "/api/runs/")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key":   strings.ReplaceAll(strings.ReplaceAll(key, "%2F", "/"), "%23", "#"),
				"stage": "plan", "waiting_on": "human", "plan_epic_id": "epic-" + key, "title": "Ship it",
			})
		case strings.HasPrefix(path, "/api/plan/"):
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %q", path)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func newLearningService(t *testing.T, store *testPersonaStore, enabled bool, audit *recordingAuditSink) *Service {
	t.Helper()
	ts := learningRunsServer(t)
	cfg := Config{
		DashboardURL: ts.URL,
		AllowedUsers: []string{"alice:owner", "bob:owner"},
		PersonaStore: store,
		PersonaLearning: func() persona.LearningConfig {
			return persona.LearningConfig{Enabled: enabled, Threshold: 2}
		},
	}
	if audit != nil {
		cfg.AuditSink = audit
	}
	s := NewService(&recordingBackend{}, cfg, discardLogger())
	s.client = ts.Client()
	s.now = func() time.Time { return learningTestClock }
	return s
}

func ownerCtx(author string) context.Context {
	ctx := context.WithValue(context.Background(), commandAuthorContextKey{}, author)
	return context.WithValue(ctx, commandRoleContextKey{}, "owner")
}

func TestPersonaLearningSignalsAccumulatePerUserAndSuggestPastThreshold(t *testing.T) {
	store := &testPersonaStore{records: map[string]persona.Record{
		"alice": {Depth: persona.DepthOutcomes, SummaryLength: persona.SummaryStandard},
		"bob":   {Depth: persona.DepthOutcomes, SummaryLength: persona.SummaryStandard},
	}}
	s := newLearningService(t, store, true, nil)
	var sent []string

	if _, err := s.cmdRuns(ownerCtx("alice"), "acme/w#1 more"); err != nil {
		t.Fatalf("more #1: %v", err)
	}
	drainQueue(s, &sent)
	if got := store.records["alice"].Learning; got == nil || got.Signals.Expanded != 1 {
		t.Fatalf("alice expanded counter after one more = %#v", got)
	}
	if store.records["bob"].Learning != nil {
		t.Fatalf("bob accumulated alice's signal: %#v", store.records["bob"].Learning)
	}
	if len(sent) != 0 {
		t.Fatalf("suggestion announced below threshold: %#v", sent)
	}
	reply, err := s.cmdPersona(ownerCtx("alice"), "suggestions")
	if err != nil || !strings.Contains(reply, "No persona suggestions pending") {
		t.Fatalf("suggestions below threshold = %q, %v", reply, err)
	}

	if _, err := s.cmdRuns(ownerCtx("alice"), "acme/w#2 more"); err != nil {
		t.Fatalf("more #2: %v", err)
	}
	drainQueue(s, &sent)
	if len(sent) != 1 || !strings.Contains(sent[0], "For alice: Persona suggestion: set depth from outcomes to technical") || !strings.Contains(sent[0], "2 expansions in 7 days") {
		t.Fatalf("suggestion announcement = %#v", sent)
	}
	if store.records["alice"].Depth != persona.DepthOutcomes {
		t.Fatal("suggestion silently applied")
	}
	reply, err = s.cmdPersona(ownerCtx("alice"), "suggestions")
	if err != nil || !strings.Contains(reply, "1. set depth from outcomes to technical (evidence: 2 expansions in 7 days)") {
		t.Fatalf("suggestions = %q, %v", reply, err)
	}
	reply, err = s.cmdPersona(ownerCtx("alice"), "show")
	if err != nil || !strings.Contains(reply, "suggestions: 1 pending") {
		t.Fatalf("show = %q, %v", reply, err)
	}
}

func TestPersonaLearningSkippedAndReAskedSignals(t *testing.T) {
	store := &testPersonaStore{records: map[string]persona.Record{
		"alice": {Depth: persona.DepthTechnical, SummaryLength: persona.SummaryStandard},
		"bob":   {Depth: persona.DepthOutcomes, SummaryLength: persona.SummaryStandard},
	}}
	s := newLearningService(t, store, true, nil)

	// A technical-depth author approving without expanding is a skip.
	if _, err := s.cmdRuns(ownerCtx("alice"), "approve acme/w#1"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := store.records["alice"].Learning; got == nil || got.Signals.Skipped != 1 {
		t.Fatalf("alice skipped counter = %#v", got)
	}
	// Expanding first means the decision was informed: no skip.
	if _, err := s.cmdRuns(ownerCtx("alice"), "acme/w#2 more"); err != nil {
		t.Fatalf("more: %v", err)
	}
	if _, err := s.cmdRuns(ownerCtx("alice"), "reject acme/w#2 nope"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got := store.records["alice"].Learning; got.Signals.Skipped != 1 || got.Signals.Expanded != 0 {
		t.Fatalf("technical author counted an expansion or a skip after expanding: %#v", got.Signals)
	}

	// Asking for the same outcomes summary twice without expanding is a re-ask.
	for i := 0; i < 2; i++ {
		if _, err := s.cmdRuns(ownerCtx("bob"), "acme/w#3"); err != nil {
			t.Fatalf("show #%d: %v", i+1, err)
		}
	}
	if got := store.records["bob"].Learning; got == nil || got.Signals.ReAsked != 1 {
		t.Fatalf("bob re-asked counter = %#v", got)
	}
}

func TestPersonaLearningAcceptAppliesAndAuditsRejectClears(t *testing.T) {
	audit := &recordingAuditSink{}
	store := &testPersonaStore{records: map[string]persona.Record{
		"alice": {Depth: persona.DepthOutcomes, SummaryLength: persona.SummaryStandard},
		"bob":   {Depth: persona.DepthOutcomes, SummaryLength: persona.SummaryStandard},
	}}
	s := newLearningService(t, store, true, audit)
	for _, author := range []string{"alice", "bob"} {
		for _, key := range []string{"acme/w#1 more", "acme/w#2 more"} {
			if _, err := s.cmdRuns(ownerCtx(author), key); err != nil {
				t.Fatalf("%s %s: %v", author, key, err)
			}
		}
	}

	reply, err := s.cmdPersona(ownerCtx("alice"), "accept 2")
	if err != nil || !strings.Contains(reply, "no persona suggestion #2") {
		t.Fatalf("accept out of range = %q, %v", reply, err)
	}
	reply, err = s.cmdPersona(ownerCtx("alice"), "accept x")
	if err != nil || !strings.Contains(reply, "Usage") {
		t.Fatalf("accept non-number = %q, %v", reply, err)
	}
	reply, err = s.cmdPersona(ownerCtx("alice"), "accept 1")
	if err != nil || !strings.Contains(reply, "Persona adjusted: depth outcomes to technical, evidence: 2 expansions in 7 days") {
		t.Fatalf("accept = %q, %v", reply, err)
	}
	got := store.records["alice"]
	if got.Depth != persona.DepthTechnical || len(got.Suggestions()) != 0 || got.Learning.LastAdjustment == nil {
		t.Fatalf("accepted record = %#v", got)
	}
	entries := audit.snapshot()
	if len(entries) != 1 || entries[0].actor != "alice" || entries[0].action != personaAuditAction || entries[0].agent != "" {
		t.Fatalf("audit entries = %#v", entries)
	}
	if entries[0].fields["outcome"] != personaAuditOutcomeAccepted || entries[0].fields["evidence"] != "2 expansions in 7 days" || entries[0].fields["to"] != persona.DepthTechnical {
		t.Fatalf("audit fields = %#v", entries[0].fields)
	}
	reply, err = s.cmdPersona(ownerCtx("alice"), "show")
	if err != nil || !strings.Contains(reply, "last adjustment: depth outcomes to technical, evidence: 2 expansions in 7 days") {
		t.Fatalf("show after accept = %q, %v", reply, err)
	}

	reply, err = s.cmdPersona(ownerCtx("bob"), "reject")
	if err != nil || !strings.Contains(reply, "Dismissed 1 persona suggestion") {
		t.Fatalf("reject = %q, %v", reply, err)
	}
	if bob := store.records["bob"]; bob.Depth != persona.DepthOutcomes || len(bob.Suggestions()) != 0 || bob.Learning.Signals.Total() != 0 {
		t.Fatalf("rejected record = %#v", bob)
	}
	reply, err = s.cmdPersona(ownerCtx("bob"), "reject")
	if err != nil || !strings.Contains(reply, "No persona suggestions pending") {
		t.Fatalf("second reject = %q, %v", reply, err)
	}
	if entries := audit.snapshot(); len(entries) != 2 || entries[1].fields["outcome"] != personaAuditOutcomeRejected {
		t.Fatalf("reject audit = %#v", entries)
	}
}

func TestPersonaLearningUndoPinsAndUnpinResumes(t *testing.T) {
	audit := &recordingAuditSink{}
	store := &testPersonaStore{records: map[string]persona.Record{
		"alice": {Depth: persona.DepthOutcomes, SummaryLength: persona.SummaryStandard},
	}}
	s := newLearningService(t, store, true, audit)
	ctx := ownerCtx("alice")

	reply, err := s.cmdPersona(ctx, "undo")
	if err != nil || !strings.Contains(reply, "no persona adjustment to undo") {
		t.Fatalf("undo without adjustment = %q, %v", reply, err)
	}
	for _, key := range []string{"acme/w#1 more", "acme/w#2 more"} {
		if _, err := s.cmdRuns(ctx, key); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	if _, err := s.cmdPersona(ctx, "accept 1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	reply, err = s.cmdPersona(ctx, "undo")
	if err != nil || !strings.Contains(reply, "Reverted depth to outcomes and pinned") {
		t.Fatalf("undo = %q, %v", reply, err)
	}
	got := store.records["alice"]
	if got.Depth != persona.DepthOutcomes || !got.Pinned {
		t.Fatalf("undone record = %#v", got)
	}
	if entries := audit.snapshot(); len(entries) != 2 || entries[1].fields["outcome"] != personaAuditOutcomeUndone {
		t.Fatalf("undo audit = %#v", entries)
	}

	// Pinned: expansions accumulate nothing and propose nothing.
	for _, key := range []string{"acme/w#3 more", "acme/w#4 more"} {
		if _, err := s.cmdRuns(ctx, key); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	if got := store.records["alice"]; got.Learning.Signals.Total() != 0 || len(got.Suggestions()) != 0 {
		t.Fatalf("pinned record kept learning: %#v", got.Learning)
	}
	reply, err = s.cmdPersona(ctx, "show")
	if err != nil || !strings.Contains(reply, "pinned: yes") {
		t.Fatalf("show pinned = %q, %v", reply, err)
	}

	reply, err = s.cmdPersona(ctx, "unpin")
	if err != nil || !strings.Contains(reply, "unpinned") {
		t.Fatalf("unpin = %q, %v", reply, err)
	}
	for _, key := range []string{"acme/w#5 more", "acme/w#6 more"} {
		if _, err := s.cmdRuns(ctx, key); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	if got := store.records["alice"]; len(got.Suggestions()) != 1 {
		t.Fatalf("unpinned record did not resume learning: %#v", got.Learning)
	}
	reply, err = s.cmdPersona(ctx, "pin")
	if err != nil || !strings.Contains(reply, "Persona pinned") || !store.records["alice"].Pinned || len(store.records["alice"].Suggestions()) != 0 {
		t.Fatalf("pin = %q, %v, record = %#v", reply, err, store.records["alice"])
	}
}

func TestPersonaLearningOffCollectsNothingAndOffersNothing(t *testing.T) {
	store := &testPersonaStore{records: map[string]persona.Record{
		"alice": {Depth: persona.DepthOutcomes, SummaryLength: persona.SummaryStandard},
	}}
	s := newLearningService(t, store, false, nil)
	ctx := ownerCtx("alice")
	var sent []string
	for _, args := range []string{"acme/w#1 more", "acme/w#2 more", "acme/w#3", "acme/w#3", "approve acme/w#4"} {
		if _, err := s.cmdRuns(ctx, args); err != nil {
			t.Fatalf("%s: %v", args, err)
		}
	}
	drainQueue(s, &sent)
	if got := store.records["alice"]; got.Learning != nil || got.Depth != persona.DepthOutcomes {
		t.Fatalf("learning off still touched the record: %#v", got)
	}
	if len(sent) != 0 {
		t.Fatalf("learning off announced: %#v", sent)
	}
	reply, err := s.cmdPersona(ctx, "suggestions")
	if err != nil || !strings.Contains(reply, "Persona learning is off") {
		t.Fatalf("suggestions with learning off = %q, %v", reply, err)
	}

	// No PersonaLearning func at all is the same as off.
	bare := NewService(&recordingBackend{}, Config{AllowedUsers: []string{"alice:owner"}, PersonaStore: store}, discardLogger())
	if bare.learningConfig().Enabled {
		t.Fatal("nil PersonaLearning must read as disabled")
	}
}

func TestPersonaLearningIgnoresAuthorsWithoutPersona(t *testing.T) {
	store := &testPersonaStore{records: map[string]persona.Record{}}
	s := newLearningService(t, store, true, nil)
	ctx := ownerCtx("alice")
	for _, args := range []string{"acme/w#1 more", "acme/w#2 more", "approve acme/w#3"} {
		if _, err := s.cmdRuns(ctx, args); err != nil {
			t.Fatalf("%s: %v", args, err)
		}
	}
	if _, ok := store.records["alice"]; ok {
		t.Fatalf("learning created a persona record from signals alone: %#v", store.records["alice"])
	}
	for _, sub := range []string{"suggestions", "accept 1", "reject", "undo", "pin"} {
		reply, err := s.cmdPersona(ctx, sub)
		if err != nil || !strings.Contains(reply, "No persona recorded") {
			t.Fatalf("%s without persona = %q, %v", sub, reply, err)
		}
	}
	reply, err := s.cmdPersona(ctx, "")
	if err != nil || !strings.Contains(reply, "suggestions") {
		t.Fatalf("usage should list learning subcommands: %q, %v", reply, err)
	}
}
