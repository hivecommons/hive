package dashboard

import (
	"encoding/json"
	"net/http"
	"reflect"

	"github.com/hivecommons/hive/pkg/config"
)

func (s *Server) contributeHelpLinks() []config.ContributeHelpLink {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return config.ContributeHelpLinksOrDefault(nil)
	}
	return config.ContributeHelpLinksOrDefault(s.deps.Config.Contribute.HelpLinks)
}

func (s *Server) publishContributeHelpLinksChange(links []config.ContributeHelpLink) {
	if s == nil || s.contributeHub == nil || s.contributeHub.sse == nil {
		return
	}
	s.contributeHub.sse.broadcast(sseEvent{Type: "help_links", HelpLinks: links})
}

func sameContributeHelpLinks(a, b []config.ContributeHelpLink) bool {
	return reflect.DeepEqual(a, b)
}

func (s *Server) handleContributeHelpLinks(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		HelpLinks []config.ContributeHelpLink `json:"help_links"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	links, err := config.NormalizeContributeHelpLinks(body.HelpLinks)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	oldLinks := s.contributeHelpLinks()
	s.deps.Config.Contribute.HelpLinks = links
	s.auditFromRequest(r, "config_governor_hub", auditDetail("section", "contribute.help_links"), "")
	s.refreshAndPersist()
	newLinks := s.contributeHelpLinks()
	if !sameContributeHelpLinks(oldLinks, newLinks) {
		s.publishContributeHelpLinksChange(newLinks)
	}
	jsonResponse(w, map[string]any{"ok": true, "help_links": newLinks})
}
