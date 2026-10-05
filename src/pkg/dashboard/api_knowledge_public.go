package dashboard

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

type publicKnowledgeSettings struct {
	Enabled  bool     `json:"enabled"`
	Tags     []string `json:"tags"`
	Source   string   `json:"source"`
	Endpoint string   `json:"endpoint"`
	URL      string   `json:"url"`
}

func normalizePublicKnowledgeTags(tags []string) []string {
	result := make([]string, 0, len(tags))
	seen := make(map[string]bool)
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" && !seen[tag] {
			result = append(result, tag)
			seen[tag] = true
		}
	}
	return result
}

func (s *Server) publicKnowledgeSettings() publicKnowledgeSettings {
	s.publicKnowledgeMu.RLock()
	defer s.publicKnowledgeMu.RUnlock()
	result := publicKnowledgeSettings{Enabled: publicKnowledgeEnabled(), Tags: normalizePublicKnowledgeTags(publicKnowledgeTags()), Source: "env", Endpoint: publicKnowledgeMCPPath}
	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Knowledge.Public != nil {
		cfg := s.deps.Config.Knowledge.Public
		result.Enabled = cfg.Enabled
		result.Tags = normalizePublicKnowledgeTags(cfg.Tags)
		result.Source = "config"
	}
	return result
}

func (s *Server) handlePublicKnowledgeSettings(w http.ResponseWriter, r *http.Request) {
	settings := s.publicKnowledgeSettings()
	settings.URL = s.oauthPublicOrigin(r) + publicKnowledgeMCPPath
	jsonResponse(w, settings)
}

func (s *Server) handlePublicKnowledgeSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Enabled *bool    `json:"enabled"`
		Tags    []string `json:"tags"`
	}
	if err := decodeBody(r, &body); err != nil || body.Enabled == nil {
		jsonError(w, "enabled must be a boolean and tags must be an array of strings", http.StatusBadRequest)
		return
	}
	if s.deps == nil || s.deps.Config == nil || s.deps.Config.SourcePath == "" {
		jsonError(w, "runtime config persistence is unavailable", http.StatusServiceUnavailable)
		return
	}
	tags := normalizePublicKnowledgeTags(body.Tags)
	s.publicKnowledgeMu.Lock()
	previous := s.deps.Config.Knowledge.Public
	s.deps.Config.Knowledge.Public = &config.PublicKnowledgeConfig{Enabled: *body.Enabled, Tags: tags}
	if err := s.saveConfig(); err != nil {
		s.deps.Config.Knowledge.Public = previous
		s.publicKnowledgeMu.Unlock()
		s.logger.Error("failed to persist public knowledge settings", "error", err)
		jsonError(w, "could not persist public knowledge settings", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "knowledge_public_update", auditDetail("enabled", fmt.Sprint(*body.Enabled), "tags", strings.Join(tags, ",")), "")
	s.publicKnowledgeMu.Unlock()
	s.handlePublicKnowledgeSettings(w, r)
}
