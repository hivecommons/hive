package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/persona"
)

type testPersonaStore struct {
	records map[string]persona.Record
}

func (s *testPersonaStore) GetPersona(_ context.Context, author string) (persona.Record, bool, error) {
	r, ok := s.records[author]
	return r, ok, nil
}

func (s *testPersonaStore) PutPersona(_ context.Context, author string, record persona.Record) error {
	if s.records == nil {
		s.records = map[string]persona.Record{}
	}
	s.records[author] = record.Normalize()
	return nil
}

func TestPersonaSetupShowAndSet(t *testing.T) {
	store := &testPersonaStore{records: map[string]persona.Record{}}
	s := NewService(&recordingBackend{}, Config{AllowedUsers: []string{"uid:owner"}, PersonaStore: store}, discardLogger())

	ctx := context.WithValue(context.Background(), commandAuthorContextKey{}, "uid")
	got, err := s.cmdPersona(ctx, "setup")
	if err != nil || !strings.Contains(got, "1/3") {
		t.Fatalf("setup = %q, %v", got, err)
	}
	for _, answer := range []string{"technical", "detailed", "include receipts"} {
		if !s.handlePendingPersonaReply(context.Background(), makeMsg("1", answer, false), answer) {
			t.Fatalf("persona answer %q was not consumed", answer)
		}
	}
	if got := store.records["uid"]; got.Depth != persona.DepthTechnical || got.SummaryLength != persona.SummaryDetailed || got.Notes != "include receipts" {
		t.Fatalf("stored persona = %#v", got)
	}
	got, err = s.cmdPersona(ctx, "set depth outcomes")
	if err != nil || !strings.Contains(got, "depth: outcomes") {
		t.Fatalf("set = %q, %v", got, err)
	}
	got, err = s.cmdPersona(ctx, "show")
	if err != nil || !strings.Contains(got, "summary_length: detailed") {
		t.Fatalf("show = %q, %v", got, err)
	}
}

func TestRunsRenderingUsesAuthorPersonaAndMoreExpands(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/runs/acme%2Fwidgets%237" {
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"key":          "acme/widgets#7",
			"title":        "Ship widgets",
			"repo":         "acme/widgets",
			"stage":        "plan",
			"gen":          2,
			"waiting_on":   "human",
			"plan_epic_id": "epic-7",
			"last_receipt": "https://example.invalid/receipt",
		})
	}))
	defer ts.Close()

	store := &testPersonaStore{records: map[string]persona.Record{
		"outcome-user": {Depth: persona.DepthOutcomes, SummaryLength: persona.SummaryShort},
		"tech-user":    {Depth: persona.DepthTechnical, SummaryLength: persona.SummaryDetailed},
	}}
	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, PersonaStore: store}, discardLogger())
	s.client = ts.Client()

	outcomeCtx := context.WithValue(context.Background(), commandAuthorContextKey{}, "outcome-user")
	techCtx := context.WithValue(context.Background(), commandAuthorContextKey{}, "tech-user")
	outcome, err := s.cmdRuns(outcomeCtx, "acme/widgets#7")
	if err != nil {
		t.Fatalf("outcome run: %v", err)
	}
	technical, err := s.cmdRuns(techCtx, "acme/widgets#7")
	if err != nil {
		t.Fatalf("technical run: %v", err)
	}
	if strings.Contains(outcome, "Plan: epic-7") || !strings.Contains(technical, "Plan: epic-7") {
		t.Fatalf("persona depth did not affect rendering:\noutcome=%s\ntechnical=%s", outcome, technical)
	}
	more, err := s.cmdRuns(outcomeCtx, "acme/widgets#7 more")
	if err != nil || !strings.Contains(more, "Plan: epic-7") {
		t.Fatalf("more = %q, %v", more, err)
	}
}

func TestCheckpointRenderingUsesTwoPersonas(t *testing.T) {
	store := &testPersonaStore{records: map[string]persona.Record{
		"owner-a": {Depth: persona.DepthOutcomes, SummaryLength: persona.SummaryShort},
		"owner-b": {Depth: persona.DepthTechnical, SummaryLength: persona.SummaryDetailed},
	}}
	s := NewService(&recordingBackend{}, Config{
		AllowedUsers: []string{"owner-a:owner", "owner-b:owner"},
		PersonaStore: store,
	}, discardLogger())
	var sent []string

	s.enqueueRunCheckpoint(runSnapshot{
		Key:         "acme/widgets#7",
		Title:       "Ship widgets",
		Repo:        "acme/widgets",
		Stage:       "plan",
		Gen:         2,
		WaitingOn:   "human",
		PlanEpicID:  "epic-7",
		LastReceipt: "https://example.invalid/receipt",
	})
	drainQueue(s, &sent)

	if len(sent) != 2 {
		t.Fatalf("checkpoint messages = %#v", sent)
	}
	if !strings.Contains(sent[0], "For owner-a:") || strings.Contains(sent[0], "Plan: epic-7") {
		t.Fatalf("owner-a checkpoint = %q", sent[0])
	}
	if !strings.Contains(sent[1], "For owner-b:") || !strings.Contains(sent[1], "Plan: epic-7") {
		t.Fatalf("owner-b checkpoint = %q", sent[1])
	}
}
