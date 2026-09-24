package dashboard

import (
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

type dashboardThemeRequest struct {
	Theme          string                         `json:"theme"`
	ThemeOverrides config.DashboardThemeOverrides `json:"theme_overrides"`
}

type dashboardThemeListItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      string   `json:"author,omitempty"`
	Dark        bool     `json:"dark"`
	Swatches    []string `json:"swatches"`
}

func (s *Server) currentDashboardTheme() (config.DashboardTheme, error) {
	if s.deps == nil || s.deps.Config == nil {
		return config.DashboardThemeEffective(config.DashboardConfig{})
	}
	cfg := s.deps.Config.Dashboard
	return config.DashboardThemeEffective(cfg)
}

func (s *Server) handleThemeCSS(w http.ResponseWriter, r *http.Request) {
	th, err := s.themeForCSSRequest(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	css, err := config.DashboardThemeCSS(th)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	etag, err := config.DashboardThemeETag(th)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=60, must-revalidate")
	w.Header().Set("ETag", etag)
	if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write([]byte(css))
}

func (s *Server) themeForCSSRequest(r *http.Request) (config.DashboardTheme, error) {
	id := strings.TrimSpace(r.URL.Query().Get("theme"))
	if id == "" {
		return s.currentDashboardTheme()
	}
	th, ok := config.DashboardThemeBuiltin(id)
	if !ok {
		return config.DashboardTheme{}, errUnknownDashboardTheme(id)
	}
	return th, nil
}

func errUnknownDashboardTheme(id string) error {
	return &unknownDashboardThemeError{id: id}
}

type unknownDashboardThemeError struct {
	id string
}

func (e *unknownDashboardThemeError) Error() string {
	return "unknown dashboard theme " + e.id
}

func (s *Server) handleThemesList(w http.ResponseWriter, r *http.Request) {
	items := make([]dashboardThemeListItem, 0, len(config.DashboardThemeCatalog()))
	for _, th := range config.DashboardThemeCatalog() {
		items = append(items, dashboardThemeListItem{
			ID:          th.ID,
			Name:        th.Name,
			Description: th.Description,
			Author:      th.Author,
			Dark:        th.Dark,
			Swatches:    themeSwatches(th),
		})
	}
	jsonResponse(w, map[string]any{"themes": items})
}

func (s *Server) handleDashboardThemeGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	cfg := config.DashboardConfig{}
	if s.deps != nil && s.deps.Config != nil {
		cfg = s.deps.Config.Dashboard
	}
	th, err := config.DashboardThemeEffective(cfg)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{
		"theme":           defaultDashboardThemeID(cfg.Theme),
		"theme_overrides": cfg.ThemeOverrides,
		"effective":       th,
		"catalog":         config.DashboardThemeCatalog(),
	})
}

func (s *Server) handleDashboardThemePut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body dashboardThemeRequest
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Theme = strings.TrimSpace(body.Theme)
	if err := validateDashboardThemeRequest(body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusInternalServerError)
		return
	}
	s.deps.Config.Dashboard.Theme = defaultDashboardThemeID(body.Theme)
	s.deps.Config.Dashboard.ThemeOverrides = body.ThemeOverrides
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after dashboard theme update", "error", err)
		jsonError(w, "failed to save config", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "config_dashboard_theme", auditDetail("section", "appearance", "theme", s.deps.Config.Dashboard.Theme), "")
	th, err := config.DashboardThemeEffective(s.deps.Config.Dashboard)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "theme": s.deps.Config.Dashboard.Theme, "theme_overrides": s.deps.Config.Dashboard.ThemeOverrides, "effective": th})
}

func validateDashboardThemeRequest(body dashboardThemeRequest) error {
	_, err := config.DashboardThemeEffective(config.DashboardConfig{Theme: body.Theme, ThemeOverrides: body.ThemeOverrides})
	return err
}

func defaultDashboardThemeID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return config.DefaultDashboardThemeID()
	}
	return config.CanonicalDashboardThemeID(id)
}

func themeSwatches(th config.DashboardTheme) []string {
	keys := []string{"--bg", "--panel", "--accent", "--text"}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if v := strings.TrimSpace(th.Tokens[key]); v != "" {
			out = append(out, v)
		}
	}
	return out
}
