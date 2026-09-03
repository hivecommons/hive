package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestCheckModelAllowedNoConfig(t *testing.T) {
	hub := NewContributeWSHub(slog.Default(), nil)
	allowed, models := hub.checkModelAllowed("claude-sonnet")
	if !allowed {
		t.Error("should allow when no server config")
	}
	if models != nil {
		t.Error("should return nil models")
	}
}

func TestCheckModelAllowedEmptyList(t *testing.T) {
	srv := newMinimalServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{}

	hub := NewContributeWSHub(slog.Default(), nil)
	hub.server = srv

	allowed, _ := hub.checkModelAllowed("any-model")
	if !allowed {
		t.Error("should allow when allow list is empty")
	}
}

func TestCheckModelAllowedMatchesModel(t *testing.T) {
	srv := newMinimalServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{"claude-*", "gpt-4o"}

	hub := NewContributeWSHub(slog.Default(), nil)
	hub.server = srv

	allowed, _ := hub.checkModelAllowed("claude-sonnet")
	if !allowed {
		t.Error("should allow matching wildcard model")
	}

	allowed, _ = hub.checkModelAllowed("gpt-4o")
	if !allowed {
		t.Error("should allow exact match model")
	}
}

func TestCheckModelAllowedRejectsUnknown(t *testing.T) {
	srv := newMinimalServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{"claude-*"}
	srv.deps.Config.Hub.ContributeRejectUnknownModels = true

	hub := NewContributeWSHub(slog.Default(), nil)
	hub.server = srv

	allowed, acceptedModels := hub.checkModelAllowed("gpt-4o")
	if allowed {
		t.Error("should reject non-matching model when reject_unknown=true")
	}
	if len(acceptedModels) != 1 || acceptedModels[0] != "claude-*" {
		t.Errorf("should return accepted models list, got %v", acceptedModels)
	}
}

func TestCheckModelAllowedEmptyModelNoReject(t *testing.T) {
	srv := newMinimalServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{"claude-*"}
	srv.deps.Config.Hub.ContributeRejectUnknownModels = false

	hub := NewContributeWSHub(slog.Default(), nil)
	hub.server = srv

	allowed, _ := hub.checkModelAllowed("")
	if !allowed {
		t.Error("should allow empty model when reject_unknown=false")
	}
}

func TestCheckModelAllowedEmptyModelReject(t *testing.T) {
	srv := newMinimalServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{"claude-*"}
	srv.deps.Config.Hub.ContributeRejectUnknownModels = true

	hub := NewContributeWSHub(slog.Default(), nil)
	hub.server = srv

	allowed, acceptedModels := hub.checkModelAllowed("")
	if allowed {
		t.Error("should reject empty model when reject_unknown=true")
	}
	if len(acceptedModels) == 0 {
		t.Error("should return accepted models list")
	}
}

func TestCheckModelAllowedNoRejectUnknown(t *testing.T) {
	srv := newMinimalServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{"claude-*"}
	srv.deps.Config.Hub.ContributeRejectUnknownModels = false

	hub := NewContributeWSHub(slog.Default(), nil)
	hub.server = srv

	allowed, _ := hub.checkModelAllowed("unknown-model")
	if !allowed {
		t.Error("should allow unknown model when reject_unknown=false")
	}
}

func TestHandleGovernorHubNewFields(t *testing.T) {
	srv := newFullServer(t)
	body := `{
		"contribute_suspended": true,
		"contribute_allow_models": ["claude-*", "gpt-4o"],
		"contribute_reject_unknown_models": true,
		"contribute_deny_titles": ["WIP", "draft"],
		"contribute_deny_authors": ["bot-user"],
		"tier_limits": {"newcomer": {"max_concurrent": 1, "max_per_hour": 5}}
	}`

	req := httptest.NewRequest("PUT", "/api/governor/hub", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorHub(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	cfg := srv.deps.Config.Hub
	if !cfg.ContributeRejectUnknownModels {
		t.Error("ContributeRejectUnknownModels should be true")
	}
	if len(cfg.ContributeAllowModels) != 2 {
		t.Errorf("expected 2 allow models, got %d", len(cfg.ContributeAllowModels))
	}
}

func TestConfigMatchesAny(t *testing.T) {
	tests := []struct {
		text     string
		patterns []string
		want     bool
	}{
		{"claude-sonnet", []string{"claude-*"}, true},
		{"gpt-4o", []string{"claude-*", "gpt-4o"}, true},
		{"unknown", []string{"claude-*", "gpt-4o"}, false},
		{"claude-opus", []string{"claude-opus"}, true},
		{"", []string{"claude-*"}, false},
	}
	for _, tt := range tests {
		got := config.MatchesAny(tt.text, tt.patterns)
		if got != tt.want {
			t.Errorf("MatchesAny(%q, %v) = %v, want %v", tt.text, tt.patterns, got, tt.want)
		}
	}
}
