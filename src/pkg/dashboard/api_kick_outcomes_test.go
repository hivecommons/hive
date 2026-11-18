package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/kickoutcome"
)

func kickOutcomeServer(t *testing.T) (*Server, *kickoutcome.Store) {
	t.Helper()
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	deps := testDeps(t)
	store, _ := kickoutcome.Open("", nil)
	deps.KickOutcomes = store
	s.RegisterAPI(deps)
	return s, store
}

func kickOutcomeGet(s *Server, path, role string, verified bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Hive-User", "u")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func seedStandDown(store *kickoutcome.Store) time.Time {
	t0 := time.Now().Add(-time.Hour)
	store.Delivered("scanner", t0)
	store.TurnEnded("scanner", "stand-down", "stand down: no push rights ghp_abcdefghijklmnopqrstuvwxyz0123456789", t0, t0.Add(time.Minute))
	return t0
}

// A read-only session sees the one-line summary and nothing else: no quoted
// reason, no trigger, no times of the turn. An owner and a read-write session
// see the detail, with tokens redacted.
func TestKickOutcomesRoleGatedDetail(t *testing.T) {
	s, store := kickOutcomeServer(t)
	seedStandDown(store)

	for _, c := range []struct {
		name, role string
		verified   bool
		detail     bool
	}{{"read-only", "read", false, false}, {"proofless owner", "owner", false, false}, {"read-write", "read-write", false, true}, {"owner", "owner", true, true}} {
		rec := kickOutcomeGet(s, "/api/kick-outcomes?agent=scanner", c.role, c.verified)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		hasQuote := strings.Contains(body, "no push rights")
		if hasQuote != c.detail {
			t.Fatalf("%s: quote visible=%v want %v: %s", c.name, hasQuote, c.detail, body)
		}
		if strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
			t.Fatalf("%s: token leaked: %s", c.name, body)
		}
		if !c.detail && (strings.Contains(body, "trigger") || strings.Contains(body, "started_at") || strings.Contains(body, "kicks")) {
			t.Fatalf("%s: detail fields leaked to a read-only session: %s", c.name, body)
		}
		if !strings.Contains(body, "stood down") {
			t.Fatalf("%s: summary missing: %s", c.name, body)
		}
	}
}

func TestKickOutcomesLatestListHoldsSummariesOnly(t *testing.T) {
	s, store := kickOutcomeServer(t)
	seedStandDown(store)
	rec := kickOutcomeGet(s, "/api/kick-outcomes", "owner", true)
	var resp struct {
		Agents map[string]map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if v := resp.Agents["scanner"]; v["kind"] != "stood_down" || v["quote"] != nil {
		t.Fatalf("%v", v)
	}
}

func TestKickOutcomesUnavailableWithoutStore(t *testing.T) {
	s, _ := kickOutcomeServer(t)
	s.deps.KickOutcomes = nil
	if rec := kickOutcomeGet(s, "/api/kick-outcomes", "owner", true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d", rec.Code)
	}
}

// A refusal is recorded only with a documented reason code; the record never
// invents one.
func TestKickRefusedOnErrorUsesDocumentedCodes(t *testing.T) {
	s, store := kickOutcomeServer(t)
	s.kickRefusedOnError("a", kickoutcome.TriggerDashboard, "alice", errors.New("agent a cannot be kicked: it is paused (by budget)"))
	s.kickRefusedOnError("a", kickoutcome.TriggerDashboard, "alice", errors.New("something nobody documented"))
	s.kickRefusedOnError("a", kickoutcome.TriggerDashboard, "alice", nil)
	rs := store.Records("a", 0)
	if len(rs) != 1 || rs[0].RefusalCode != kickRefusalPaused || rs[0].TriggerActor != "alice" {
		t.Fatalf("%+v", rs)
	}
}
