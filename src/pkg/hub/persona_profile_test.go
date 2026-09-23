package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/persona"
)

func TestPersonaProfileEndpointFlagRoleAndSeededStore(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	record := persona.Record{
		Depth:         persona.DepthOutcomes,
		SummaryLength: persona.SummaryStandard,
		Notes:         "wants receipts",
		Learning: &persona.Learning{
			Signals: persona.Signals{WindowStart: now, Expanded: 2},
			Suggestions: []persona.Suggestion{{
				Key: persona.SuggestionKeyDepth, From: persona.DepthOutcomes, To: persona.DepthTechnical,
				Evidence: "2 expansions in 7 days", ProposedAt: now,
			}},
			History: []persona.HistoryEntry{{
				Outcome: "declined", Key: persona.SuggestionKeySummaryLength,
				From: persona.SummaryStandard, To: persona.SummaryDetailed,
				Evidence: "5 expansions in 7 days", At: now.Add(-time.Hour),
			}},
			LastAdjustment: &persona.Adjustment{
				Key: persona.SuggestionKeyDepth, From: persona.DepthTechnical, To: persona.DepthOutcomes,
				Evidence: "5 skips in 7 days", AppliedAt: now.Add(-2 * time.Hour),
			},
		},
	}
	if err := saveUserPersona("alice", record); err != nil {
		t.Fatalf("saveUserPersona: %v", err)
	}

	s := newHandlerHub()
	gated := s.requireAdmin(s.handlePersonaProfile)
	rec := httptest.NewRecorder()
	gated.ServeHTTP(rec, reqWithUser(http.MethodGet, "/api/persona/profile", "", hubAdminUsername))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("flag off status = %d, want 404", rec.Code)
	}

	s.personaLearningEnabled = true
	rec = httptest.NewRecorder()
	gated.ServeHTTP(rec, reqWithUser(http.MethodGet, "/api/persona/profile", "", "alice"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	gated.ServeHTTP(rec, reqWithUser(http.MethodGet, "/api/persona/profile", "", hubAdminUsername))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var payload personaProfileResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Profiles) != 1 {
		t.Fatalf("profiles = %#v", payload.Profiles)
	}
	got := payload.Profiles[0]
	if got.Identity != "github:alice" && got.Identity != "alice" {
		t.Fatalf("identity = %q", got.Identity)
	}
	if got.Traits["depth"] != persona.DepthOutcomes || got.Traits["summary_length"] != persona.SummaryStandard {
		t.Fatalf("traits = %#v", got.Traits)
	}
	if len(got.Suggestions) != 1 || got.Suggestions[0].Evidence != "2 expansions in 7 days" {
		t.Fatalf("suggestions = %#v", got.Suggestions)
	}
	if len(got.History) != 1 || got.History[0].Outcome != "declined" {
		t.Fatalf("history = %#v", got.History)
	}
	if got.LastAdjustment == nil || got.LastAdjustment.Evidence != "5 skips in 7 days" {
		t.Fatalf("last adjustment = %#v", got.LastAdjustment)
	}
	if got.LastUpdated == "" {
		t.Fatal("last_updated is empty")
	}
}

func TestPersonaProfileRenderingIsHiddenAndListsLearningState(t *testing.T) {
	raw, err := os.ReadFile("assets/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	src := string(raw)
	for _, want := range []string{
		`id="persona-profile-section" style="display:none`,
		`fetch('/api/persona/profile')`,
		`resp.status === 403 || resp.status === 404`,
		`Persona profile`,
		`Current traits / weights`,
		`Pending suggestions`,
		`Accepted / declined history`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
}
