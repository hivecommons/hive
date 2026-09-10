package dashboard

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// --- Knowledge endpoints ---

func (s *Server) handleKnowledgeToggle(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.deps.Config.Knowledge.Enabled = body.Enabled

	if body.Enabled && s.deps.Knowledge == nil {
		layers := make([]knowledge.LayerConfig, len(s.deps.Config.Knowledge.Layers))
		for i, l := range s.deps.Config.Knowledge.Layers {
			layers[i] = knowledge.LayerConfig{Type: knowledge.LayerType(l.Type), Path: l.Path, URL: l.URL, Shared: l.Shared}
		}
		kcfg := knowledge.KnowledgeConfig{
			Enabled: true,
			Layers:  layers,
			Primer: knowledge.PrimerConfig{
				MaxFacts:      s.deps.Config.Knowledge.Primer.MaxFacts,
				MergeStrategy: s.deps.Config.Knowledge.Primer.MergeStrategy,
			},
		}
		api := knowledge.NewKnowledgeAPI(layers, kcfg, s.deps.Logger)
		s.deps.Knowledge = api
	} else if !body.Enabled {
		s.deps.Knowledge = nil
	}

	if s.deps.BeadSynthesizer != nil {
		if body.Enabled && s.deps.Config.Knowledge.BeadSynthesizer.IsEnabled() {
			s.deps.BeadSynthesizer.StartBackground(s.deps.Ctx)
			s.logger.Info("bead synthesizer started via knowledge toggle")
		} else if !body.Enabled {
			s.deps.BeadSynthesizer.Stop()
			s.logger.Info("bead synthesizer stopped via knowledge toggle")
		}
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after knowledge toggle", "error", err)
	}
	s.auditFromRequest(r, "knowledge_toggle", auditDetail("enabled", fmt.Sprintf("%v", body.Enabled)), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "enabled": fmt.Sprintf("%v", body.Enabled)})
}

func (s *Server) handleBeadSynthStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.Knowledge.BeadSynthesizer
	running := false
	if s.deps.BeadSynthesizer != nil {
		running = s.deps.BeadSynthesizer.IsRunning()
	}
	jsonResponse(w, map[string]interface{}{
		"enabled":             cfg.IsEnabled(),
		"running":             running,
		"schedule":            cfg.Schedule,
		"min_confidence":      cfg.MinConfidence,
		"target_layer":        cfg.TargetLayer,
		"max_facts_per_cycle": cfg.MaxFactsPerCycle,
		"vault_path":          cfg.VaultPath,
	})
}

func (s *Server) handleBeadSynthToggle(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	enabled := body.Enabled
	s.deps.Config.Knowledge.BeadSynthesizer.Enabled = &enabled

	if s.deps.BeadSynthesizer != nil {
		if enabled {
			s.deps.BeadSynthesizer.StartBackground(s.deps.Ctx)
			s.logger.Info("bead synthesizer enabled via dashboard")
		} else {
			s.deps.BeadSynthesizer.Stop()
			s.logger.Info("bead synthesizer disabled via dashboard")
		}
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after bead-synth toggle", "error", err)
	}
	s.auditFromRequest(r, "bead_synth_toggle", auditDetail("enabled", fmt.Sprintf("%v", enabled)), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "bead_synthesizer_enabled": fmt.Sprintf("%v", enabled)})
}

func (s *Server) handleKnowledgeList(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false, "facts": []interface{}{}})
		return
	}

	typeFilter := r.URL.Query().Get("type")
	facts := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", typeFilter, 0)
	jsonResponse(w, map[string]interface{}{
		"enabled": true,
		"count":   len(facts),
		"facts":   facts,
	})
}

// titleCaseWords capitalizes the first letter following each run of
// whitespace, replicating the exact word-boundary rule of the deprecated
// strings.Title (boundary = unicode.IsSpace, not "non-letter" — so
// underscore-joined identifiers like "test_scaffold" are left as
// "Test_scaffold", matching prior output byte-for-byte). It exists only as a
// dependency-free fallback for a fact type absent from typeLabels;
// golang.org/x/text/cases is not a module dependency here.
func titleCaseWords(s string) string {
	prevSpace := true
	return strings.Map(func(r rune) rune {
		if prevSpace && unicode.IsLetter(r) {
			r = unicode.ToTitle(r)
		}
		prevSpace = unicode.IsSpace(r)
		return r
	}, s)
}

func (s *Server) handleKnowledgeExport(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte("# Agent Knowledge\n\nKnowledge base not available.\n"))
		return
	}

	facts := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", "", 0)

	grouped := make(map[string][]knowledge.Fact)
	for _, f := range facts {
		t := string(f.Type)
		if t == "" {
			t = "general"
		}
		grouped[t] = append(grouped[t], f)
	}

	typeLabels := map[string]string{
		"pattern":       "Patterns",
		"gotcha":        "Gotchas",
		"decision":      "Decisions",
		"regression":    "Regressions",
		"test_scaffold": "Test Scaffolds",
		"integration":   "Integration",
		"coverage_rule": "Coverage Rules",
		"idea":          "Ideas",
		"vision":        "Vision",
		"constitution":  "Constitution",
		"requirement":   "Requirements",
		"constraint":    "Constraints",
		"stakeholder":   "Stakeholders",
		"general":       "General",
	}

	var sb strings.Builder
	sb.WriteString("# Agent Knowledge\n\n")
	sb.WriteString("This file is auto-generated from the hive knowledge base.\n")
	sb.WriteString("It refreshes periodically — do not edit manually.\n\n")

	order := []string{"constitution", "constraint", "requirement", "decision",
		"pattern", "gotcha", "regression", "coverage_rule", "test_scaffold",
		"integration", "idea", "vision", "stakeholder", "general"}

	for _, t := range order {
		ff, ok := grouped[t]
		if !ok || len(ff) == 0 {
			continue
		}
		// Facts within a type group arrive aggregated across layers, vaults, and
		// git sources, so their relative order is not stable between fetches
		// (see #3090). Sort by the natural identifier (Slug, then Title, then
		// Body) so identical knowledge always serializes byte-for-byte the same
		// and unchanged data does not rewrite downstream files on every refresh.
		sortFactsStable(ff)
		label := typeLabels[t]
		if label == "" {
			label = titleCaseWords(t)
		}
		sb.WriteString("## " + label + "\n\n")
		for _, f := range ff {
			sb.WriteString("### " + f.Title + "\n\n")
			if f.Body != "" {
				sb.WriteString(f.Body + "\n\n")
			}
			if len(f.Tags) > 0 {
				sb.WriteString("Tags: " + strings.Join(f.Tags, ", ") + "\n\n")
			}
		}
	}

	body := sb.String()
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	// ETag over the rendered body (not just the fact count) so consumers can
	// skip a rewrite whenever the content is unchanged, and always see a new
	// tag when it genuinely changes.
	sum := sha256.Sum256([]byte(body))
	w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sum))
	_, _ = w.Write([]byte(body))
}

// sortFactsStable orders facts by their natural identifier so an unchanged
// knowledge base serializes to byte-identical output regardless of the order
// facts were aggregated across sources. Slug is the primary key (unique per
// fact); Title and Body are tiebreakers for facts that lack a slug.
func sortFactsStable(facts []knowledge.Fact) {
	sort.SliceStable(facts, func(i, j int) bool {
		if facts[i].Slug != facts[j].Slug {
			return facts[i].Slug < facts[j].Slug
		}
		if facts[i].Title != facts[j].Title {
			return facts[i].Title < facts[j].Title
		}
		return facts[i].Body < facts[j].Body
	})
}

func (s *Server) handleKnowledgeSearch(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"results": []interface{}{}})
		return
	}

	query := r.URL.Query().Get("q")
	tagFilter := r.URL.Query().Get("tag")
	if query == "" && tagFilter == "" {
		jsonError(w, "q or tag parameter is required", http.StatusBadRequest)
		return
	}

	typeFilter := r.URL.Query().Get("type")
	limitStr := r.URL.Query().Get("limit")
	limit := 0
	if limitStr != "" {
		limit, _ = strconv.Atoi(limitStr)
	}

	var results []knowledge.Fact
	if tagFilter != "" {
		all := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", typeFilter, 0)
		for _, f := range all {
			for _, t := range f.Tags {
				if strings.EqualFold(t, tagFilter) {
					results = append(results, f)
					break
				}
			}
		}
		if limit > 0 && len(results) > limit {
			results = results[:limit]
		}
	} else {
		results = s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, query, typeFilter, limit)
	}

	jsonResponse(w, map[string]interface{}{
		"query":   query,
		"tag":     tagFilter,
		"count":   len(results),
		"results": results,
	})
}

func (s *Server) handleKnowledgeHealth(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, map[string]interface{}{"enabled": false})
		return
	}
	jsonResponse(w, s.deps.Knowledge.Health(s.deps.Ctx))
}

func (s *Server) handleKnowledgeStats(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false})
		return
	}
	stats := s.deps.Knowledge.Stats(s.deps.Ctx)
	stats["vaults"] = s.deps.Knowledge.Vaults()
	stats["git_sources"] = s.deps.Knowledge.GitSources()

	if layers, ok := stats["layers"].([]map[string]interface{}); ok {
		total := 0
		for _, m := range layers {
			if p, ok := m["total_pages"].(int); ok {
				total += p
			}
		}
		s.AppendFactHistory(total)
	}

	// Sample the estimated cost on the same cadence as the fact history so the
	// cost sparkline shares the fact sparkline's timer (both throttled to
	// ~5-min intervals). The figure is the same all-time cumulative estimated
	// total that GET /api/cost returns; the per-agent map feeds the agent
	// cards' spend-over-window display and the per-model map feeds the cost
	// table's mini sparklines (unpriced models included — tokens still trend).
	est := s.estimatedCost()
	perAgent := make(map[string]float64, len(est.ByAgent))
	for _, row := range est.ByAgent {
		if row.USD > 0 {
			perAgent[row.Name] = row.USD
		}
	}
	perModel := make(map[string]CostModelSnap, len(est.ByModel))
	for _, row := range est.ByModel {
		if row.Input > 0 || row.Output > 0 || row.USD > 0 {
			perModel[row.Name] = CostModelSnap{Input: row.Input, Output: row.Output, USD: row.USD}
		}
	}
	s.AppendCostHistoryFull(est.TotalUSD, perAgent, perModel)

	jsonResponse(w, stats)
}

func (s *Server) handleKnowledgeGraph(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, knowledge.GraphData{Nodes: []knowledge.GraphNode{}, Edges: []knowledge.GraphEdge{}})
		return
	}
	gs := s.deps.Knowledge.GraphStore()
	if gs == nil {
		jsonResponse(w, knowledge.GraphData{Nodes: []knowledge.GraphNode{}, Edges: []knowledge.GraphEdge{}})
		return
	}
	rootSlug := r.URL.Query().Get("root")
	const defaultGraphDepth = 2
	depth := defaultGraphDepth
	if d := r.URL.Query().Get("depth"); d != "" {
		if v, err := strconv.Atoi(d); err == nil && v > 0 {
			depth = v
		}
	}
	stores := s.deps.Knowledge.FileStores()
	data := gs.BuildGraphData(stores, rootSlug, depth)
	jsonResponse(w, data)
}

func (s *Server) handleKnowledgeLayer(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false, "facts": []interface{}{}})
		return
	}

	layer := r.PathValue("layer")
	typeFilter := r.URL.Query().Get("type")

	const gitSourcePrefix = "git_source:"
	if strings.HasPrefix(layer, gitSourcePrefix) {
		name := strings.TrimPrefix(layer, gitSourcePrefix)
		store := s.deps.Knowledge.GetGitSourceStore(name)
		if store == nil {
			jsonResponse(w, map[string]interface{}{"layer": layer, "count": 0, "facts": []interface{}{}})
			return
		}
		facts := store.ListPages(typeFilter)
		for i := range facts {
			facts[i].Layer = knowledge.LayerType(layer)
		}
		jsonResponse(w, map[string]interface{}{
			"layer": layer,
			"count": len(facts),
			"facts": facts,
		})
		return
	}

	knowledgeLayer := knowledge.LayerType(layer)
	facts := s.deps.Knowledge.LayerFacts(s.deps.Ctx, knowledgeLayer, typeFilter)
	if facts == nil {
		facts = []knowledge.Fact{}
	}
	jsonResponse(w, map[string]interface{}{
		"layer": layer,
		"count": len(facts),
		"facts": facts,
	})
}

func (s *Server) handleKnowledgeFact(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not enabled", http.StatusNotFound)
		return
	}

	slug := r.PathValue("slug")
	fact, err := s.deps.Knowledge.ReadFact(s.deps.Ctx, slug)
	if err != nil || fact == nil {
		jsonError(w, "fact not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, fact)
}

func (s *Server) handleKnowledgeCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.CreateFactRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Title == "" || req.Body == "" {
		jsonError(w, "title and body are required", http.StatusBadRequest)
		return
	}
	if req.Layer == "" {
		req.Layer = "project"
	}
	if req.Type == "" {
		req.Type = "pattern"
	}
	const defaultConfidence = 0.7
	if req.Confidence <= 0 {
		req.Confidence = defaultConfidence
	}

	if err := s.deps.Knowledge.CreateFact(s.deps.Ctx, req); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "knowledge_create_fact", auditDetail("title", req.Title, "layer", req.Layer), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "title": req.Title, "layer": req.Layer})
}

func (s *Server) handleKnowledgeUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	layer := r.PathValue("layer")
	slug := r.PathValue("slug")

	var req knowledge.UpdateFactRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.UpdateFact(s.deps.Ctx, knowledge.LayerType(layer), slug, req); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "knowledge_update_fact", auditDetail("slug", slug, "layer", layer), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "slug": slug, "layer": layer})
}

func (s *Server) handleKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	layer := r.PathValue("layer")
	slug := r.PathValue("slug")

	if err := s.deps.Knowledge.DeleteFact(s.deps.Ctx, knowledge.LayerType(layer), slug); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "knowledge_delete_fact", auditDetail("slug", slug, "layer", layer), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "deleted": slug})
}

func (s *Server) handleKnowledgePromote(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.PromoteRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Slug == "" || req.FromLayer == "" || req.ToLayer == "" {
		jsonError(w, "slug, from_layer, and to_layer are required", http.StatusBadRequest)
		return
	}
	if req.Promoter == "" {
		req.Promoter = "dashboard"
	}

	result := s.deps.Knowledge.PromoteFact(s.deps.Ctx, req)
	if !result.Success {
		fact, err := s.deps.Knowledge.VaultFact(req.Slug)
		if err != nil || fact == nil {
			jsonError(w, result.Error, http.StatusBadRequest)
			return
		}
		syncReq := knowledge.ObsidianSyncRequest{
			Filename: req.Slug + ".md",
			Content:  fact.Body,
		}
		syncReq.Frontmatter = map[string]interface{}{
			"title":      fact.Title,
			"type":       string(fact.Type),
			"layer":      string(req.ToLayer),
			"confidence": fact.Confidence,
			"tags":       fact.Tags,
		}
		syncResult, syncErr := s.deps.Knowledge.ObsidianSync(s.deps.Ctx, syncReq)
		if syncErr != nil {
			jsonError(w, syncErr.Error(), http.StatusInternalServerError)
			return
		}
		s.auditFromRequest(r, "knowledge_promote_fact", auditDetail("slug", req.Slug, "from", string(req.FromLayer), "to", string(req.ToLayer)), "")
		jsonResponse(w, knowledge.PromoteResult{
			Slug:      syncResult.Slug,
			FromLayer: req.FromLayer,
			ToLayer:   req.ToLayer,
			Success:   true,
		})
		return
	}
	s.auditFromRequest(r, "knowledge_promote_fact", auditDetail("slug", req.Slug, "from", string(req.FromLayer), "to", string(req.ToLayer)), "")
	jsonResponse(w, result)
}

func (s *Server) handleKnowledgeImport(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Content string `json:"content"`
		Format  string `json:"format"`
		Layer   string `json:"layer"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Content == "" {
		jsonError(w, "content is required", http.StatusBadRequest)
		return
	}
	if req.Layer == "" {
		req.Layer = "project"
	}
	if req.Format == "" {
		req.Format = "markdown"
	}

	count, err := s.deps.Knowledge.ImportFacts(s.deps.Ctx, knowledge.LayerType(req.Layer), req.Content, req.Format)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "knowledge_import", auditDetail("layer", req.Layer, "format", req.Format), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "imported": count, "layer": req.Layer, "format": req.Format})
}

// handleKnowledgeChannelsList returns the user-writable channels (vaults) that
// facts can be imported into. The reserved automation vault is excluded (#3581).
func (s *Server) handleKnowledgeChannelsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.WritableVaults())
}

// handleKnowledgeChannelCreate creates a new local channel (vault) to import into.
func (s *Server) handleKnowledgeChannelCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	info, err := s.deps.Knowledge.CreateVault(req.Name)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, "knowledge_channel_create", auditDetail("channel", info.Name), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "channel": info})
}

func (s *Server) handleKnowledgeSubsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.Subscriptions())
}

func (s *Server) handleKnowledgeSubsAdd(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var sub knowledge.Subscription
	if err := decodeBody(r, &sub); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if sub.URL == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(sub.URL, "https://") && !strings.HasPrefix(sub.URL, "http://") {
		jsonError(w, "url must use http or https scheme", http.StatusBadRequest)
		return
	}
	if isPrivateURL(r.Context(), sub.URL) {
		jsonError(w, "subscription url must not point to private/internal addresses", http.StatusBadRequest)
		return
	}
	if sub.Layer == "" {
		sub.Layer = knowledge.LayerOrg
	}

	if err := s.deps.Knowledge.AddSubscription(sub); err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	s.auditFromRequest(r, "knowledge_add_subscription", auditDetail("url", sub.URL), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "subscription": sub})
}

func (s *Server) handleKnowledgeSubsRemove(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		URL string `json:"url"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.URL == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.RemoveSubscription(req.URL); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "knowledge_remove_subscription", auditDetail("url", req.URL), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "removed": req.URL})
}

// --- Vault endpoints ---

func (s *Server) handleVaultsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.Vaults())
}

// vaultPathDeniedPrefixes lists filesystem subtrees that must never be indexed
// as knowledge vaults regardless of caller role. These prefixes contain secrets,
// credentials, and sensitive OS/runtime state that an attacker could exfiltrate
// via the knowledge search API if connected as a vault.
var vaultPathDeniedPrefixes = []string{
	"/etc",
	"/proc",
	"/sys",
	"/run",
	"/var/run",
	"/dev",
	"/root",
	"/boot",
}

func (s *Server) handleVaultsConnect(w http.ResponseWriter, r *http.Request) {
	// Vault connection is an owner-only operation: it indexes an entire directory
	// tree and makes its contents queryable. Restricting to owner prevents a
	// write-role contributor from exfiltrating secrets via the knowledge API.
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		jsonError(w, "path is required", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Path, "..") {
		jsonError(w, "vault path must not contain '..'", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(req.Path) {
		jsonError(w, "vault path must be absolute", http.StatusBadRequest)
		return
	}
	// Reject well-known sensitive filesystem prefixes regardless of role to
	// prevent accidental or deliberate secret exposure via the knowledge search.
	cleanPath := filepath.Clean(req.Path)
	for _, denied := range vaultPathDeniedPrefixes {
		if cleanPath == denied || strings.HasPrefix(cleanPath, denied+"/") {
			jsonError(w, "vault path is in a restricted filesystem location", http.StatusBadRequest)
			return
		}
	}
	if req.Name == "" {
		req.Name = filepath.Base(req.Path)
	}

	if err := s.deps.Knowledge.ConnectVault(req.Path, req.Name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditFromRequest(r, "vault_connect", auditDetail("name", req.Name, "path", req.Path), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "name": req.Name, "path": req.Path})
}

func (s *Server) handleVaultsDisconnect(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		jsonError(w, "path is required", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Path, "..") {
		jsonError(w, "vault path must not contain '..'", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(req.Path) {
		jsonError(w, "vault path must be absolute", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.DisconnectVault(req.Path); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "vault_disconnect", auditDetail("path", req.Path), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "removed": req.Path})
}

func (s *Server) handleVaultsReindex(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		jsonError(w, "path is required", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Path, "..") {
		jsonError(w, "path must not contain '..'", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(req.Path) {
		jsonError(w, "path must be absolute", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.ReindexVault(req.Path); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "vault_reindex", auditDetail("path", req.Path), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "reindexed": req.Path})
}

func (s *Server) handleVaultFacts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}

	name := r.PathValue("name")
	facts := s.deps.Knowledge.VaultFacts(name)
	if facts == nil {
		facts = []knowledge.Fact{}
	}
	jsonResponse(w, facts)
}

// --- Git source endpoints ---

func (s *Server) handleGitSourcesList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.GitSources())
}

func (s *Server) handleGitSourcesConnect(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Branch  string `json:"branch"`
		Subpath string `json:"subpath"`
		Layer   string `json:"layer"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.URL == "" || req.Name == "" {
		jsonError(w, "name and url are required", http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	// Context-aware so the SSRF DNS lookup inherits this request's deadline and
	// cancellation rather than running on a background context.
	if err := knowledge.ValidateGitSourceURLContext(r.Context(), req.URL); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Name, "..") || strings.ContainsAny(req.Name, "/\\") {
		jsonError(w, "invalid name", http.StatusBadRequest)
		return
	}
	if req.Branch != "" && strings.HasPrefix(req.Branch, "-") {
		jsonError(w, "branch must not start with '-'", http.StatusBadRequest)
		return
	}
	if req.Subpath != "" && (strings.HasPrefix(req.Subpath, "-") || strings.Contains(req.Subpath, "..")) {
		jsonError(w, "subpath must not start with '-' or contain '..'", http.StatusBadRequest)
		return
	}
	if req.Layer == "" {
		req.Layer = "project"
	}

	gsConfig := knowledge.GitSourceConfig{
		Name:    req.Name,
		URL:     req.URL,
		Branch:  req.Branch,
		Subpath: req.Subpath,
		Layer:   knowledge.LayerType(req.Layer),
	}
	if err := s.deps.Knowledge.ConnectGitSource(s.deps.Ctx, gsConfig); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	alreadyInConfig := false
	for _, gs := range s.deps.Config.Knowledge.GitSources {
		if gs.URL == req.URL && gs.Subpath == req.Subpath {
			alreadyInConfig = true
			break
		}
	}
	if !alreadyInConfig {
		s.deps.Config.Knowledge.GitSources = append(s.deps.Config.Knowledge.GitSources, config.GitSourceConfigYAML{
			Name:    req.Name,
			URL:     req.URL,
			Branch:  req.Branch,
			Subpath: req.Subpath,
			Layer:   req.Layer,
		})
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after git source connect", "error", err)
	}

	s.auditFromRequest(r, "git_source_connect", auditDetail("name", req.Name, "url", req.URL), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "name": req.Name, "url": req.URL})
}

func (s *Server) handleGitSourcesDisconnect(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		URL     string `json:"url"`
		Subpath string `json:"subpath"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.DisconnectGitSource(req.URL, req.Subpath); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	filtered := make([]config.GitSourceConfigYAML, 0, len(s.deps.Config.Knowledge.GitSources))
	for _, gs := range s.deps.Config.Knowledge.GitSources {
		if gs.URL == req.URL && gs.Subpath == req.Subpath {
			continue
		}
		filtered = append(filtered, gs)
	}
	s.deps.Config.Knowledge.GitSources = filtered
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after git source disconnect", "error", err)
	}

	s.auditFromRequest(r, "git_source_disconnect", auditDetail("url", req.URL), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "removed": req.URL})
}

// --- Obsidian sync endpoint ---

func (s *Server) ensureKnowledge() bool {
	if s.deps == nil {
		return false
	}
	s.knowledgeMu.Lock()
	defer s.knowledgeMu.Unlock()
	if s.deps.Knowledge == nil {
		s.deps.Knowledge = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, s.logger)
		s.logger.Info("created file-based knowledge API for vault/obsidian access")
		entries, err := os.ReadDir("/data/knowledge")
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					dir := filepath.Join("/data/knowledge", e.Name())
					if connErr := s.deps.Knowledge.ConnectVault(dir, e.Name()); connErr == nil {
						s.logger.Info("auto-connected knowledge vault", "name", e.Name(), "dir", dir)
					}
				}
			}
		}
	}
	return true
}

func (s *Server) handleObsidianSync(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "server not initialized", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.ObsidianSyncRequest
	if err := decodeBody(r, &req); err != nil {
		s.logger.Warn("obsidian sync: json decode failed", "error", err)
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Filename == "" {
		jsonError(w, "filename is required", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		if title, ok := req.Frontmatter["title"]; ok {
			if s, ok := title.(string); ok && s != "" {
				req.Content = s
			}
		}
		if req.Content == "" {
			req.Content = "(no body)"
		}
	}

	result, err := s.deps.Knowledge.ObsidianSync(s.deps.Ctx, req)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"ok":     true,
		"slug":   result.Slug,
		"action": result.Action,
		"fact":   result.Fact,
	})
}

// --- Document source endpoints ---

func (s *Server) handleDocumentsList(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, s.deps.Knowledge.ListDocuments())
}

func (s *Server) handleDocumentsImport(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.DocSourceConfig
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.URL == "" && req.FilePath == "" && req.Context7ID == "" {
		jsonError(w, "url, file_path, or context7_id is required", http.StatusBadRequest)
		return
	}
	if req.URL != "" {
		if !strings.HasPrefix(req.URL, "https://") && !strings.HasPrefix(req.URL, "http://") {
			jsonError(w, "document url must use http or https scheme", http.StatusBadRequest)
			return
		}
		if isPrivateURL(r.Context(), req.URL) {
			jsonError(w, "document url must not point to private/internal addresses", http.StatusBadRequest)
			return
		}
	}
	if req.Layer == "" {
		req.Layer = knowledge.LayerProject
	}

	meta, err := s.deps.Knowledge.ImportDocument(s.deps.Ctx, req)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "document_import", auditDetail("name", req.Name), "")
	jsonResponse(w, meta)
}

func (s *Server) handleDocumentGet(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	slug := r.PathValue("slug")
	meta, err := s.deps.Knowledge.GetDocument(slug)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResponse(w, meta)
}

func (s *Server) handleDocumentDelete(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	slug := r.PathValue("slug")
	if err := s.deps.Knowledge.DeleteDocument(slug); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "document_delete", auditDetail("slug", slug), "")
	okResponse(w, map[string]string{"status": "deleted", "slug": slug})
}

func (s *Server) handleDocumentReimport(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	slug := r.PathValue("slug")
	meta, err := s.deps.Knowledge.ReimportDocument(s.deps.Ctx, slug)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "document_reimport", auditDetail("slug", slug), "")
	jsonResponse(w, meta)
}

func (s *Server) handleCleanupOrphans(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	removed, err := s.deps.Knowledge.CleanupOrphanedDocFacts()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "removed": removed})
}

func (s *Server) handleContext7Search(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		jsonError(w, "missing q parameter", http.StatusBadRequest)
		return
	}

	results, err := s.deps.Knowledge.SearchContext7Libraries(r.Context(), query)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, results)
}
