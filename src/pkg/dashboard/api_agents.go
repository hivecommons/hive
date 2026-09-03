package dashboard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/promptsrc"
)

type agentListEntry struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	Enabled     bool   `json:"enabled"`
	Managed     bool   `json:"managed"`
	Backend     string `json:"backend"`
	Model       string `json:"model"`
}

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	agents := make([]agentListEntry, 0, len(s.deps.Config.Agents))
	for name, cfg := range s.deps.Config.Agents {
		displayName := cfg.DisplayName
		if displayName == "" {
			displayName = name
		}
		agents = append(agents, agentListEntry{
			Name:        name,
			ID:          cfg.ID,
			DisplayName: displayName,
			Enabled:     cfg.Enabled,
			Managed:     cfg.Managed,
			Backend:     cfg.Backend,
			Model:       cfg.Model,
		})
	}
	jsonResponse(w, agents)
}

// handleAgentCreate serves POST /api/agents. OWNER-ONLY, matching
// handleAgentDelete below: creating an agent writes an agent file, lifts any
// deletion tombstone, and starts a live process. Audit F16 (2026-08-13).
func (s *Server) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Name  string             `json:"name"`
		Agent config.AgentConfig `json:"agent"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if strings.ContainsAny(body.Name, " ./\\") || !kickTemplatePattern.MatchString(body.Name+".md") {
		jsonError(w, "name must contain only alphanumeric characters, hyphens, and underscores (no spaces)", http.StatusBadRequest)
		return
	}
	if len(body.Name) > 64 {
		jsonError(w, "name must be at most 64 characters", http.StatusBadRequest)
		return
	}

	// Reject an explain_mode the config loader would reject. Without this, a bad
	// value is written to the agent file here and only surfaces at the NEXT
	// config load, as a load failure far from the request that caused it.
	if !config.ValidateExplainMode(body.Agent.ExplainMode) {
		jsonError(w, "explain_mode must be one of: off, brief, full (or empty to inherit the hive default)", http.StatusBadRequest)
		return
	}

	// Same reasoning for caveman_mode: reject at the request rather than
	// persisting a value the config loader would fail on later (#4531,
	// flagged as a follow-up in #3897).
	if !config.ValidateCavemanMode(body.Agent.CavemanMode) {
		jsonError(w, "caveman_mode must be one of: lite, full, ultra, wenyan (or empty to disable)", http.StatusBadRequest)
		return
	}

	if _, exists := s.deps.Config.Agents[body.Name]; exists {
		jsonError(w, "agent already exists", http.StatusConflict)
		return
	}

	agentsDir := s.deps.Config.Data.AgentsDir
	if agentsDir == "" {
		jsonError(w, "agents_dir not configured", http.StatusInternalServerError)
		return
	}

	body.Agent.Managed = true
	// An agent the operator creates by hand is never a member of any pack's
	// roster, so the operator owns its pause/run state from birth: without
	// this claim, the ACMM pack visibility sweep — which runs on every
	// restart — paused it as "agent not in pack level N" on every pod roll
	// (#5706, same ownership contract as ModelOwner/#5632).
	body.Agent.PauseOwner = config.FieldOwnerOperator

	// If a GitHub prompt source is declared, validate it against the seed-only
	// allowlist and bake an initial copy so the very first kick has content even
	// if GitHub is momentarily unreachable. A bad/denied source is a hard error
	// here so the operator sees it at create time rather than silently at kick.
	if body.Agent.PromptSource.IsSet() {
		if err := s.bakePromptSource(r.Context(), body.Name, &body.Agent); err != nil {
			jsonError(w, "prompt source: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	if err := config.SaveAgentFile(agentsDir, body.Name, body.Agent); err != nil {
		s.logger.Error("failed to save agent file", "agent", body.Name, "error", err)
		jsonError(w, "failed to save agent: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Creating an agent under a previously-deleted name is an explicit
	// re-add — lift the tombstone, or the next config reload prunes it again.
	if s.deps.Config.ClearAgentRemoved(body.Name) {
		s.logger.Info("agent deletion tombstone lifted by explicit create", "agent", body.Name)
	}
	s.deps.Config.Agents[body.Name] = body.Agent
	s.deps.Config.ApplyAgentDefaults(body.Name)

	if err := s.deps.Config.ExpandAgentReplicas(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	finalCfg := s.deps.Config.Agents[body.Name]
	addedAgents := s.deps.AgentMgr.ReconcileAgents(s.deps.Config.EnabledAgents())
	for _, added := range addedAgents {
		if ac, ok := s.deps.Config.Agents[added]; ok && !ac.OnDemand {
			if err := s.deps.AgentMgr.Start(s.deps.Ctx, added); err != nil {
				s.logger.Warn("failed to start reconciled agent", "agent", added, "error", err)
			}
		}
	}
	if s.deps.Governor != nil {
		s.deps.Governor.UpdateAgents(s.deps.Config.EnabledAgents())
	}

	s.reInitSubsystems()
	s.refreshAndPersist()

	s.logger.Info("agent created via API", "name", body.Name, "id", finalCfg.ID)
	okResponse(w, map[string]string{"status": "created", "agent": body.Name, "id": finalCfg.ID})
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}
	if agentCfg.ReplicaOf != "" {
		name = agentCfg.ReplicaOf
		agentCfg = s.deps.Config.Agents[name]
	}

	if !agentCfg.Managed {
		jsonError(w, "cannot delete base config agent — only managed (CRUD-created) agents can be deleted", http.StatusForbidden)
		return
	}

	agentsDir := s.deps.Config.Data.AgentsDir
	if agentsDir != "" {
		if err := config.RemoveAgentFile(agentsDir, name); err != nil {
			s.logger.Error("failed to remove agent file", "agent", name, "error", err)
			jsonError(w, "failed to remove agent file: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	delete(s.deps.Config.Agents, name)
	if err := s.deps.Config.ExpandAgentReplicas(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.deps.AgentMgr.ReconcileAgents(s.deps.Config.EnabledAgents())
	if s.deps.Governor != nil {
		s.deps.Governor.UpdateAgents(s.deps.Config.EnabledAgents())
	}

	// Record the deletion durably. Removing the overlay file above is not
	// enough on its own: ApplyPack runs on every restart and re-creates any
	// pack agent missing from the roster, so a pack agent deleted here came
	// straight back. See handleGovernorRemoveAgent for the full chain.
	s.deps.Config.MarkAgentRemoved(name)
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist agent tombstone", "agent", name, "error", err)
	}

	s.reInitSubsystems()
	s.refreshAndPersist()

	s.logger.Info("agent deleted via API and tombstoned", "name", name,
		"note", "will not be re-created by an ACMM pack apply until explicitly re-added")
	jsonResponse(w, agentDeletionResponse("deleted", name, packLevelsDefining(name)))
}

// agentDefinition is the portable YAML format for importing/exporting agents.
// It is defined in pkg/defsrc (shared with the live definition_source resolver)
// so the whole-agent import and the "keep linked" re-apply path parse and map
// identically.
type agentDefinition = defsrc.AgentDefinition
type agentDefinitionMeta = defsrc.AgentDefinitionMeta
type agentDefinitionSpec = defsrc.AgentDefinitionSpec

const (
	importMaxURLLen     = 2048
	importMaxContentLen = 512 * 1024 // 512 KiB
	importHTTPTimeoutS  = 10
)

var importAllowedHosts = map[string]struct{}{
	"github.com":                 {},
	"raw.githubusercontent.com":  {},
	"gist.github.com":            {},
	"gist.githubusercontent.com": {},
}

const (
	importURLFormsOneShot = "https://github.com/<owner>/<repo>/blob/<ref>/<path>, https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<path>, https://gist.github.com/<user>/<gist-id>[/raw[/<file>]], or https://gist.githubusercontent.com/<user>/<gist-id>/raw/<path>"
	importURLFormsLinked  = "https://github.com/<owner>/<repo>/blob/<ref>/<path> or https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<path>"
)

// agentConfigFromDefinition maps a parsed AgentDefinition to an AgentConfig with
// the import defaults applied (backend copilot, immediate restart, worker bead
// role, enabled). It is the single mapping used by both the whole-agent import
// handler and any live re-apply path, so the two never drift.
func agentConfigFromDefinition(def *agentDefinition) config.AgentConfig {
	includeRepos := def.Spec.IncludeRepos
	return config.AgentConfig{
		Backend:         valueOrDefault(def.Spec.Backend, "copilot"),
		Model:           def.Spec.Model,
		Enabled:         true,
		ClearOnKick:     def.Spec.ClearOnKick,
		StaleTimeout:    def.Spec.StaleTimeout,
		RestartStrategy: valueOrDefault(def.Spec.RestartStrategy, "immediate"),
		DisplayName:     def.Metadata.DisplayName,
		Description:     def.Metadata.Description,
		Role:            def.Spec.Role,
		SortOrder:       def.Spec.SortOrder,
		Emoji:           def.Metadata.Emoji,
		Color:           def.Metadata.Color,
		LaneKeywords:    def.Spec.LaneKeywords,
		DetectKeywords:  def.Spec.DetectKeywords,
		Aliases:         def.Spec.Aliases,
		Mode:            def.Spec.Mode,
		BeadRole:        valueOrDefault(def.Spec.BeadRole, "worker"),
		IncludeRepos:    &includeRepos,
		Managed:         true,
		Channels:        def.Spec.Channels,
		Tools:           def.Spec.Tools,
		Connections:     def.Spec.Connections,
	}
}

// handleAgentImport serves POST /api/agents/import. OWNER-ONLY for the same
// reason as handleAgentCreate — it is a create path that additionally fetches a
// caller-supplied remote definition. Audit F16 (2026-08-13).
func (s *Server) handleAgentImport(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Source  string `json:"source"`
		URL     string `json:"url"`
		Content string `json:"content"`
		Preview bool   `json:"preview"`
		// KeepLinked (source=="url" only) persists a definition_source on the
		// created agent so the hive re-fetches the definition from the repo on
		// reload/kick and re-applies its operator-safe fields. Ignored for paste
		// (there is no repo to link to).
		KeepLinked bool `json:"keepLinked"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var yamlContent string
	switch body.Source {
	case "url":
		if body.URL == "" {
			jsonError(w, "url is required when source is url", http.StatusBadRequest)
			return
		}
		if len(body.URL) > importMaxURLLen {
			jsonError(w, fmt.Sprintf("url must be at most %d characters", importMaxURLLen), http.StatusBadRequest)
			return
		}
		// Reject a gist URL for a KEEP-LINKED import before anything else: a gist
		// has no stable raw URL across revisions, so a linked import would silently
		// pin to a snapshot that never updates. One-shot gist imports stay allowed.
		if err := s.validateImportURL(body.URL, body.KeepLinked); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Validate before fetching, then re-validate every redirect hop.
		// Without both, this handler fetched an arbitrary caller-supplied URL
		// and returned its body — a direct SSRF into cluster-internal services
		// and the cloud metadata endpoint.
		if err := validateImportFetchURL(r.Context(), body.URL); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		client := &http.Client{
			Timeout:       importHTTPTimeoutS * 1e9, // nanoseconds
			CheckRedirect: noRedirectToPrivate,
		}
		resp, err := client.Get(body.URL)
		if err != nil {
			jsonError(w, "failed to fetch URL: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer closeHTTPBody(resp.Body)
		if resp.StatusCode != http.StatusOK {
			jsonError(w, fmt.Sprintf("URL returned HTTP %d", resp.StatusCode), http.StatusBadGateway)
			return
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, importMaxContentLen))
		if err != nil {
			jsonError(w, "failed to read URL response: "+err.Error(), http.StatusBadGateway)
			return
		}
		yamlContent = string(data)

	case "paste":
		if body.Content == "" {
			jsonError(w, "content is required when source is paste", http.StatusBadRequest)
			return
		}
		yamlContent = body.Content

	default:
		jsonError(w, "source must be 'url' or 'paste'", http.StatusBadRequest)
		return
	}

	def, err := defsrc.ParseDefinition(yamlContent)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.Preview {
		jsonResponse(w, map[string]any{
			"ok":     true,
			"parsed": def,
			"_importBody": map[string]any{
				"source":     body.Source,
				"url":        body.URL,
				"content":    body.Content,
				"keepLinked": body.KeepLinked,
			},
		})
		return
	}

	name := strings.ToLower(strings.ReplaceAll(def.Metadata.Name, " ", "-"))
	if strings.ContainsAny(name, " ./\\") || !kickTemplatePattern.MatchString(name+".md") {
		jsonError(w, "agent name contains invalid characters", http.StatusBadRequest)
		return
	}
	const maxAgentNameLen = 64
	if len(name) > maxAgentNameLen {
		jsonError(w, fmt.Sprintf("agent name must be at most %d characters", maxAgentNameLen), http.StatusBadRequest)
		return
	}

	if _, exists := s.deps.Config.Agents[name]; exists {
		jsonError(w, "agent already exists: "+name, http.StatusConflict)
		return
	}

	agentsDir := s.deps.Config.Data.AgentsDir
	if agentsDir == "" {
		jsonError(w, "agents_dir not configured", http.StatusInternalServerError)
		return
	}

	agentCfg := agentConfigFromDefinition(def)
	// An import is an operator create (#5706): claim pause-state ownership so
	// the ACMM pack sweep never pauses the imported agent as "not in pack".
	// Stamped here rather than in agentConfigFromDefinition because that
	// mapping is shared with the live definition_source re-apply path, which
	// must not mint ownership claims on its own.
	agentCfg.PauseOwner = config.FieldOwnerOperator

	// Keep-linked (source=="url" only): persist a definition_source so the hive
	// re-fetches and re-applies this definition's operator-safe fields on reload.
	// Validate the parsed source repo against the seed-only allowlist here so a
	// non-allowlisted repo is rejected at import time rather than silently ignored
	// at reload. Paste imports carry no repo, so keep-linked is meaningless there.
	if body.KeepLinked && body.Source == "url" {
		defSrc, derr := s.definitionSourceFromURL(body.URL)
		if derr != nil {
			jsonError(w, "keep linked: "+derr.Error(), http.StatusBadRequest)
			return
		}
		if !s.deps.Config.GitHubDefinitionAllowed(defSrc.Slug()) {
			jsonError(w, fmt.Sprintf("keep linked: repo %q is not on the GitHub definition allowlist (an operator must add it to the seed config's variables.security.github_prompt_allowlist and set allow_github_prompt)", defSrc.Slug()), http.StatusBadRequest)
			return
		}
		agentCfg.DefinitionSource = defSrc
	}

	if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
		s.logger.Error("failed to save imported agent file", "agent", name, "error", err)
		jsonError(w, "failed to save agent: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Save prompt template if provided
	if def.Spec.PromptTemplate != "" {
		templateFileName := name + ".md"
		agentCfg.KickTemplate = templateFileName

		if err := os.MkdirAll(promptTemplateSaveDir, 0o755); err != nil {
			s.logger.Error("failed to create policies dir for import", "error", err)
		} else {
			savePath := filepath.Join(promptTemplateSaveDir, templateFileName)
			if err := os.WriteFile(savePath, []byte(def.Spec.PromptTemplate), 0o644); err != nil {
				s.logger.Error("failed to save imported prompt template", "agent", name, "error", err)
			}
		}

		// Re-save agent with updated KickTemplate
		if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
			s.logger.Error("failed to re-save agent with template", "agent", name, "error", err)
		}
	}

	// Importing an agent under a previously-deleted name is an explicit
	// re-add — lift the tombstone so the import is not pruned on reload.
	if s.deps.Config.ClearAgentRemoved(name) {
		s.logger.Info("agent deletion tombstone lifted by import", "agent", name)
	}
	s.deps.Config.Agents[name] = agentCfg
	s.deps.Config.ApplyAgentDefaults(name)

	if err := s.deps.Config.ExpandAgentReplicas(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	addedAgents := s.deps.AgentMgr.ReconcileAgents(s.deps.Config.EnabledAgents())
	for _, added := range addedAgents {
		if ac, ok := s.deps.Config.Agents[added]; ok && !ac.OnDemand {
			if err := s.deps.AgentMgr.Start(s.deps.Ctx, added); err != nil {
				s.logger.Warn("failed to start reconciled agent", "agent", added, "error", err)
			}
		}
	}
	if s.deps.Governor != nil {
		s.deps.Governor.UpdateAgents(s.deps.Config.EnabledAgents())
	}

	s.reInitSubsystems()
	s.refreshAndPersist()

	s.logger.Info("agent imported via API", "name", name, "source", body.Source)
	okResponse(w, map[string]string{"status": "imported", "name": name})
}

// promptSourceFetcher returns the GitHub client as a promptsrc.Fetcher, or nil
// when no client is wired (typed-nil interface avoidance, same reason as main.go).
func (s *Server) promptSourceFetcher() promptsrc.Fetcher {
	if s.deps != nil && s.deps.GHClient != nil {
		return s.deps.GHClient
	}
	return nil
}

// bakePromptSource validates an agent's GitHub prompt source against the
// seed-only allowlist, fetches it once, and writes the result to the policies
// directory as the agent's kick template. This gives the agent a last-known-good
// prompt on disk so the first kick (and any kick where GitHub is unreachable and
// the in-memory cache is cold) still has content. On success it repoints the
// agent's KickTemplate at the baked file. Returns an error (surfaced to the UI)
// when the source is denied, incomplete, or unreachable.
func (s *Server) bakePromptSource(ctx context.Context, name string, agent *config.AgentConfig) error {
	if !agent.PromptSource.IsSet() {
		return fmt.Errorf("owner, repo, and path are all required")
	}
	slug := agent.PromptSource.Slug()
	allow := func(sl string) bool { return s.deps.Config.GitHubPromptAllowed(sl) }
	if !allow(slug) {
		return fmt.Errorf("repo %q is not on the GitHub prompt allowlist (an operator must add it to the seed config's variables.security.github_prompt_allowlist and set allow_github_prompt)", slug)
	}
	src := promptsrc.Source{
		Owner: agent.PromptSource.Owner,
		Repo:  agent.PromptSource.Repo,
		Path:  agent.PromptSource.Path,
		Ref:   agent.PromptSource.Ref,
	}
	body, err := promptsrc.FetchOnce(ctx, s.promptSourceFetcher(), allow, src)
	if err != nil {
		return fmt.Errorf("failed to fetch from %s: %w", slug, err)
	}

	templateFileName := name + ".md"
	if err := os.MkdirAll(promptTemplateSaveDir, 0o755); err != nil {
		return fmt.Errorf("failed to create policies directory: %w", err)
	}
	savePath := filepath.Join(promptTemplateSaveDir, templateFileName)
	if err := os.WriteFile(savePath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("failed to write baked prompt: %w", err)
	}
	agent.KickTemplate = templateFileName
	s.logger.Info("baked GitHub prompt source", "agent", name, "repo", slug, "path", src.Path, "bytes", len(body))
	return nil
}

// definitionSourceFetcher returns the GitHub client as a defsrc.Fetcher, or nil
// when no client is wired (typed-nil interface avoidance, same reason as main.go).
func (s *Server) definitionSourceFetcher() defsrc.Fetcher {
	if s.deps != nil && s.deps.GHClient != nil {
		return s.deps.GHClient
	}
	return nil
}

// githubBlobURLPattern matches a GitHub "blob" web URL:
//
//	https://<host>/<owner>/<repo>/blob/<ref>/<path...>
//
// Captures owner, repo, ref, and path. It intentionally does not anchor to
// github.com so GitHub Enterprise hosts work too.
var githubBlobURLPattern = regexp.MustCompile(`^/([^/]+)/([^/]+)/blob/([^/]+)/(.+)$`)

// githubRawURLPattern matches a raw.githubusercontent.com URL:
//
//	https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<path...>
var githubRawURLPattern = regexp.MustCompile(`^/([^/]+)/([^/]+)/([^/]+)/(.+)$`)

func (s *Server) validateImportURL(rawURL string, keepLinked bool) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("import URL must be an https URL; supported forms: %s", importURLFormsOneShot)
	}

	host := strings.ToLower(u.Hostname())
	if _, ok := importAllowedHosts[host]; !ok {
		return fmt.Errorf("import URL host %q is not supported; supported forms: %s", host, importURLFormsOneShot)
	}

	if isGistImportHost(host) {
		if keepLinked {
			return fmt.Errorf("keep linked imports do not support gist URLs; use a GitHub repository file URL (%s), or uncheck Keep linked for a one-shot gist import", importURLFormsLinked)
		}
		if strings.Trim(u.Path, "/") == "" {
			return fmt.Errorf("gist import URL must include a gist path; supported forms: %s", importURLFormsOneShot)
		}
		return nil
	}

	if host == "raw.githubusercontent.com" {
		if githubRawURLPattern.FindStringSubmatch(u.Path) == nil {
			return fmt.Errorf("import URL %q is not a supported raw GitHub file URL; supported forms: %s", rawURL, importURLFormsOneShot)
		}
		return nil
	}

	if host == "github.com" && githubBlobURLPattern.FindStringSubmatch(u.Path) != nil {
		return nil
	}

	forms := importURLFormsOneShot
	if keepLinked {
		forms = importURLFormsLinked
	}
	return fmt.Errorf("import URL %q is not a supported GitHub file URL; supported forms: %s", rawURL, forms)
}

func isGistImportHost(host string) bool {
	return host == "gist.github.com" || host == "gist.githubusercontent.com"
}

// definitionSourceFromURL parses a GitHub blob or raw file URL into a
// DefinitionSourceConfig (owner/repo/path/ref). It is used when an operator
// imports an agent with "keep linked" checked, so the pasted human-facing URL
// becomes a durable, fetchable source pointer. The raw URL host is required for
// the raw pattern to avoid mis-parsing arbitrary 4-segment paths.
func (s *Server) definitionSourceFromURL(rawURL string) (*config.DefinitionSourceConfig, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("could not parse URL %q", rawURL)
	}
	var m []string
	if strings.EqualFold(u.Host, "raw.githubusercontent.com") {
		m = githubRawURLPattern.FindStringSubmatch(u.Path)
	} else {
		m = githubBlobURLPattern.FindStringSubmatch(u.Path)
	}
	if m == nil {
		return nil, fmt.Errorf("URL %q is not a recognized GitHub file URL (expected a GitHub blob URL .../<owner>/<repo>/blob/<ref>/<path> or https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<path>)", rawURL)
	}
	return &config.DefinitionSourceConfig{
		Type:  "github",
		Owner: m[1],
		Repo:  m[2],
		Ref:   m[3],
		Path:  m[4],
		URL:   rawURL,
	}, nil
}

func (s *Server) reInitSubsystems() {
	if s.deps != nil && s.deps.ReInitFunc != nil {
		s.deps.ReInitFunc()
	}
}

// validateImportFetchURL is the pre-fetch SSRF guard for agent-definition
// imports. It deliberately restricts only the TRANSPORT and the DESTINATION,
// not which sites may be imported from, so it closes the vulnerability without
// changing what users are allowed to import.
//
// Pairs with noRedirectToPrivate on the client: this checks the URL the caller
// supplied, and that re-checks every hop the remote server redirects to, so a
// public URL cannot 302 its way to an internal address.
func validateImportFetchURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	// file://, gopher:// and friends are never a legitimate import transport
	// and are the classic pivot for reading local files.
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("import URL must use http:// or https:// scheme (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("import URL must include a host")
	}
	// isPrivateURL resolves DNS and fails closed, so a hostname that resolves
	// to 169.254.169.254 or an RFC1918 address is rejected too — not just a
	// literal IP.
	if isPrivateURL(ctx, rawURL) {
		return fmt.Errorf("import URL must not point to private/internal addresses")
	}
	return nil
}
