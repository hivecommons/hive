package dashboard

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kubestellar/hive/v2/pkg/config"
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

func (s *Server) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string              `json:"name"`
		Agent       config.AgentConfig  `json:"agent"`
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
	if err := config.SaveAgentFile(agentsDir, body.Name, body.Agent); err != nil {
		s.logger.Error("failed to save agent file", "agent", body.Name, "error", err)
		jsonError(w, "failed to save agent: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.deps.Config.Agents[body.Name] = body.Agent
	s.deps.Config.ApplyAgentDefaults(body.Name)

	finalCfg := s.deps.Config.Agents[body.Name]
	s.deps.AgentMgr.AddAgent(body.Name, finalCfg)

	s.reInitSubsystems()
	s.refreshAndPersist()

	s.logger.Info("agent created via API", "name", body.Name, "id", finalCfg.ID)
	okResponse(w, map[string]string{"status": "created", "agent": body.Name, "id": finalCfg.ID})
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
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

	s.deps.AgentMgr.RemoveAgent(name)
	delete(s.deps.Config.Agents, name)

	s.reInitSubsystems()
	s.refreshAndPersist()

	s.logger.Info("agent deleted via API", "name", name)
	okResponse(w, map[string]string{"status": "deleted", "agent": name})
}

// agentDefinition is the portable YAML format for importing/exporting agents.
type agentDefinition struct {
	APIVersion string                 `yaml:"apiVersion" json:"apiVersion"`
	Kind       string                 `yaml:"kind" json:"kind"`
	Metadata   agentDefinitionMeta    `yaml:"metadata" json:"metadata"`
	Spec       agentDefinitionSpec    `yaml:"spec" json:"spec"`
}

type agentDefinitionMeta struct {
	Name        string `yaml:"name" json:"name"`
	DisplayName string `yaml:"displayName,omitempty" json:"displayName,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Emoji       string `yaml:"emoji,omitempty" json:"emoji,omitempty"`
	Color       string `yaml:"color,omitempty" json:"color,omitempty"`
}

type agentDefinitionSpec struct {
	Backend         string            `yaml:"backend,omitempty" json:"backend,omitempty"`
	Model           string            `yaml:"model,omitempty" json:"model,omitempty"`
	Role            string            `yaml:"role,omitempty" json:"role,omitempty"`
	Mode            string            `yaml:"mode,omitempty" json:"mode,omitempty"`
	SortOrder       int               `yaml:"sortOrder,omitempty" json:"sortOrder,omitempty"`
	BeadRole        string            `yaml:"beadRole,omitempty" json:"beadRole,omitempty"`
	StaleTimeout    int               `yaml:"staleTimeout,omitempty" json:"staleTimeout,omitempty"`
	RestartStrategy string            `yaml:"restartStrategy,omitempty" json:"restartStrategy,omitempty"`
	ClearOnKick     bool              `yaml:"clearOnKick,omitempty" json:"clearOnKick,omitempty"`
	IncludeRepos    bool              `yaml:"includeRepos,omitempty" json:"includeRepos,omitempty"`
	LaneKeywords    []string          `yaml:"laneKeywords,omitempty" json:"laneKeywords,omitempty"`
	DetectKeywords  []string          `yaml:"detectKeywords,omitempty" json:"detectKeywords,omitempty"`
	Aliases         []string          `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Cadences        map[string]string `yaml:"cadences,omitempty" json:"cadences,omitempty"`
	PromptTemplate  string            `yaml:"promptTemplate,omitempty" json:"promptTemplate,omitempty"`
}

const (
	importMaxURLLen     = 2048
	importMaxContentLen = 512 * 1024 // 512 KiB
	importHTTPTimeoutS  = 10
)

func (s *Server) handleAgentImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source  string `json:"source"`
		URL     string `json:"url"`
		Content string `json:"content"`
		Preview bool   `json:"preview"`
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
		client := &http.Client{Timeout: importHTTPTimeoutS * 1e9} // nanoseconds
		resp, err := client.Get(body.URL)
		if err != nil {
			jsonError(w, "failed to fetch URL: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
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

	var def agentDefinition
	if err := yaml.Unmarshal([]byte(yamlContent), &def); err != nil {
		jsonError(w, "invalid YAML: "+err.Error(), http.StatusBadRequest)
		return
	}

	if def.Kind != exportKind {
		jsonError(w, fmt.Sprintf("expected kind %q, got %q", exportKind, def.Kind), http.StatusBadRequest)
		return
	}
	if def.Metadata.Name == "" {
		jsonError(w, "metadata.name is required", http.StatusBadRequest)
		return
	}

	if body.Preview {
		jsonResponse(w, map[string]any{
			"ok":     true,
			"parsed": def,
			"_importBody": map[string]any{
				"source":  body.Source,
				"url":     body.URL,
				"content": body.Content,
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

	includeRepos := def.Spec.IncludeRepos
	agentCfg := config.AgentConfig{
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

	s.deps.Config.Agents[name] = agentCfg
	s.deps.Config.ApplyAgentDefaults(name)

	finalCfg := s.deps.Config.Agents[name]
	s.deps.AgentMgr.AddAgent(name, finalCfg)

	s.reInitSubsystems()
	s.refreshAndPersist()

	s.logger.Info("agent imported via API", "name", name, "source", body.Source)
	okResponse(w, map[string]string{"status": "imported", "name": name})
}

func (s *Server) reInitSubsystems() {
	if s.deps != nil && s.deps.ReInitFunc != nil {
		s.deps.ReInitFunc()
	}
}
