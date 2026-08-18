package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================
// saas.go — hub banner handlers
// ============================================================

func newBannerHub() *HubServer {
	return &HubServer{
		logger:     slog.Default(),
		hubBanners: make(map[string]*HubBannerEntry),
	}
}

func postJSON(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestHandleSendHubBannerValidation(t *testing.T) {
	s := newBannerHub()

	cases := []struct {
		name, body string
		wantStatus int
	}{
		{"bad json", `{not json`, http.StatusBadRequest},
		{"empty message", `{"message":"  ","hive_ids":["h1"]}`, http.StatusBadRequest},
		{"too long", `{"message":"` + strings.Repeat("x", maxBannerMessageLen+1) + `","hive_ids":["h1"]}`, http.StatusBadRequest},
		{"bad color", `{"message":"hi","color":"purple","hive_ids":["h1"]}`, http.StatusBadRequest},
		{"no hives", `{"message":"hi","hive_ids":[]}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.handleSendHubBanner(rec, postJSON("/api/hub/banner", tc.body))
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestHandleSendHubBannerTooManyHives(t *testing.T) {
	s := newBannerHub()
	ids := make([]string, maxBannerTargetHives+1)
	for i := range ids {
		ids[i] = "h"
	}
	body, _ := json.Marshal(map[string]interface{}{"message": "hi", "hive_ids": ids})
	rec := httptest.NewRecorder()
	s.handleSendHubBanner(rec, postJSON("/api/hub/banner", string(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("too many hives status = %d, want 400", rec.Code)
	}
}

func TestHandleSendHubBannerSuccess(t *testing.T) {
	s := newBannerHub()
	// Default color path (empty color -> "green").
	rec := httptest.NewRecorder()
	s.handleSendHubBanner(rec, postJSON("/api/hub/banner", `{"message":"maintenance","hive_ids":["h1","h2"]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	s.hubBannersMu.RLock()
	defer s.hubBannersMu.RUnlock()
	if len(s.hubBanners) != 2 {
		t.Errorf("expected 2 banners stored, got %d", len(s.hubBanners))
	}
	if s.hubBanners["h1"].Color != "green" {
		t.Errorf("expected default green color, got %q", s.hubBanners["h1"].Color)
	}
}

func TestHandleGetHubBanner(t *testing.T) {
	s := newBannerHub()

	// Empty -> banners: [].
	rec := httptest.NewRecorder()
	s.handleGetHubBanner(rec, httptest.NewRequest(http.MethodGet, "/api/hub/banner", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var empty map[string][]map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &empty)
	if empty["banners"] == nil || len(empty["banners"]) != 0 {
		t.Errorf("expected empty banners array, got %+v", empty)
	}

	// With a banner.
	s.hubBanners["h1"] = &HubBannerEntry{ID: "b1", Message: "hi", Color: "blue", SentAt: "now"}
	rec = httptest.NewRecorder()
	s.handleGetHubBanner(rec, httptest.NewRequest(http.MethodGet, "/api/hub/banner", nil))
	var withOne map[string][]map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &withOne)
	if len(withOne["banners"]) != 1 {
		t.Errorf("expected 1 banner, got %+v", withOne)
	}
}

func TestHandleClearHubBanner(t *testing.T) {
	s := newBannerHub()
	s.hubBanners["h1"] = &HubBannerEntry{ID: "b1"}
	s.hubBanners["h2"] = &HubBannerEntry{ID: "b2"}

	rec := httptest.NewRecorder()
	s.handleClearHubBanner(rec, httptest.NewRequest(http.MethodPost, "/api/hub/banner/clear", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"cleared":2`) {
		t.Errorf("expected cleared:2, got %s", rec.Body.String())
	}
	if len(s.hubBanners) != 0 {
		t.Errorf("banners not cleared: %d remain", len(s.hubBanners))
	}
}
