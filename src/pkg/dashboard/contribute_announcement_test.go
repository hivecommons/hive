package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func putAnnouncementWithRole(s *Server, role string, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/contribute/announcement", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestContributeAnnouncementAuthMatrix(t *testing.T) {
	s, _ := apiServer(t)
	for _, tc := range []struct {
		name string
		role string
		want int
	}{
		{"anonymous", "", http.StatusUnauthorized},
		{"read", "read", http.StatusForbidden},
		{"read-write", "read-write", http.StatusOK},
		{"owner", "owner", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := putAnnouncementWithRole(s, tc.role, `{"text":"hello","level":"info"}`)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestContributeAnnouncementSanitizeCapAndExpiry(t *testing.T) {
	long := strings.Repeat("x", contributeAnnouncementMaxChars+25)
	ann, err := sanitizeContributeAnnouncement(config.ContributeAnnouncement{Text: `<script>alert(1)</script>` + long, Level: "bad"}, config.ContributeAnnouncement{}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ann.Text, "<script>alert(1)</script>") {
		t.Fatalf("plain text markup should be preserved as text for safe textContent rendering: %q", ann.Text[:40])
	}
	if got := len([]rune(ann.Text)); got != contributeAnnouncementMaxChars {
		t.Fatalf("announcement text cap = %d, want %d", got, contributeAnnouncementMaxChars)
	}
	if ann.Level != "info" {
		t.Fatalf("level = %q, want info", ann.Level)
	}

	now := time.Now()
	cfg := &config.Config{}
	cfg.Hub.ContributeAnnouncement = config.ContributeAnnouncement{ID: "a1", Text: "expired", Level: "warning", ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339)}
	if got := activeContributeAnnouncementFromConfig(cfg, now); got != nil {
		t.Fatalf("expired announcement active: %+v", got)
	}
	cfg.Hub.ContributeAnnouncement.ExpiresAt = now.Add(time.Hour).Format(time.RFC3339)
	if got := activeContributeAnnouncementFromConfig(cfg, now); got == nil || got.Text != "expired" || got.Level != "warning" {
		t.Fatalf("future announcement missing: %+v", got)
	}
}

func TestContributeAnnouncementIDRotationAndClear(t *testing.T) {
	now := time.Unix(100, 0)
	first, err := sanitizeContributeAnnouncement(config.ContributeAnnouncement{Text: "hello"}, config.ContributeAnnouncement{}, now)
	if err != nil {
		t.Fatal(err)
	}
	same, err := sanitizeContributeAnnouncement(config.ContributeAnnouncement{Text: "hello", Level: "warning"}, first, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if same.ID != first.ID {
		t.Fatalf("id rotated without text change: %q -> %q", first.ID, same.ID)
	}
	changed, err := sanitizeContributeAnnouncement(config.ContributeAnnouncement{Text: "new text"}, same, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if changed.ID == same.ID {
		t.Fatalf("id did not rotate on text change: %q", changed.ID)
	}
	cleared, err := sanitizeContributeAnnouncement(config.ContributeAnnouncement{Text: "   "}, changed, now)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != (config.ContributeAnnouncement{}) {
		t.Fatalf("empty text did not clear: %+v", cleared)
	}
}

func TestContributeAnnouncementStatusAndSSEPayload(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.ContributeAnnouncement = config.ContributeAnnouncement{ID: "ann-1", Text: "maint <b>window</b> & safe", Level: "warning"}

	rec := doGet(s, "/api/contribute/status")
	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	ann, ok := status["announcement"].(map[string]any)
	if !ok || ann["id"] != "ann-1" || ann["level"] != "warning" {
		t.Fatalf("status announcement = %#v", status["announcement"])
	}
	if ann["text"] != "maint <b>window</b> & safe" {
		t.Fatalf("status announcement text double-escaped or raw: %#v", ann["text"])
	}

	w := httptest.NewRecorder()
	if !writeSSE(w, sseEvent{Type: "hello", Announcement: s.activeContributeAnnouncement()}) {
		t.Fatal("writeSSE returned false")
	}
	body := w.Body.String()
	if !strings.Contains(body, `"type":"hello"`) || !strings.Contains(body, `"announcement"`) || !strings.Contains(body, `ann-1`) {
		t.Fatalf("SSE hello missing announcement: %s", body)
	}
}
