package dashboard

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// knowledgeTOCScope parses the scope query shared by the TOC and full-read
// endpoints: layers, repos, types and tags (comma-separated allow-lists) plus
// include_states for operators who need draft/deprecated/superseded entries.
// The default is approved knowledge only (#11102).
func knowledgeTOCScope(r *http.Request) (knowledge.TOCScope, error) {
	q := r.URL.Query()
	include, err := knowledge.ParseLifecycleStates(q.Get("include_states"))
	if err != nil {
		return knowledge.TOCScope{}, err
	}
	scope := knowledge.TOCScope{
		Repos:         splitCSV(q.Get("repos")),
		Types:         splitCSV(q.Get("types")),
		Tags:          splitCSV(q.Get("tags")),
		IncludeStates: include,
	}
	for _, l := range splitCSV(q.Get("layers")) {
		scope.Layers = append(scope.Layers, knowledge.LayerType(strings.ToLower(l)))
	}
	return scope, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// handleKnowledgeTOC serves GET /api/knowledge/toc: a capped, body-free table
// of contents of the knowledge visible under the requested scope. With
// format=prompt it also returns the markdown rendering kicks inject, bounded
// by max_chars.
func (s *Server) handleKnowledgeTOC(w http.ResponseWriter, r *http.Request) {
	scope, err := knowledgeTOCScope(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	maxChars, _ := strconv.Atoi(r.URL.Query().Get("max_chars"))

	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false, "entries": []interface{}{}, "total": 0, "returned": 0, "truncated": false})
		return
	}
	facts := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", "", 0)
	toc := knowledge.BuildTOC(facts, scope, limit)
	resp := map[string]interface{}{
		"enabled":   true,
		"entries":   toc.Entries,
		"total":     toc.Total,
		"returned":  toc.Returned,
		"truncated": toc.Truncated,
	}
	if r.URL.Query().Get("format") == "prompt" {
		resp["prompt"] = knowledge.FormatTOCForPrompt(toc, maxChars)
	}
	jsonResponse(w, resp)
}

// handleKnowledgeEntry serves GET /api/knowledge/entry/{id}: one entry's
// complete markdown and front-matter. The same scope and lifecycle filter as
// the TOC applies, so an entry the TOC would hide cannot be read by guessing
// its id.
func (s *Server) handleKnowledgeEntry(w http.ResponseWriter, r *http.Request) {
	scope, err := knowledgeTOCScope(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not enabled", http.StatusNotFound)
		return
	}
	fact, err := s.deps.Knowledge.ReadEntry(s.deps.Ctx, r.PathValue("id"))
	if err != nil || fact == nil || !scope.InScope(*fact) {
		jsonError(w, "knowledge entry not found", http.StatusNotFound)
		return
	}
	state := fact.EffectiveState()
	fact.State = state
	jsonResponse(w, map[string]interface{}{
		"id":       fact.Slug,
		"title":    fact.Title,
		"type":     fact.Type,
		"layer":    fact.Layer,
		"repo":     knowledge.FactRepo(*fact),
		"tags":     fact.Tags,
		"status":   state,
		"markdown": knowledge.RenderFactMarkdown(*fact),
		"fact":     fact,
	})
}
