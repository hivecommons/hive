package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/celtrigger"
	"github.com/hivecommons/hive/pkg/config"
)

type eventTriggerPreset struct {
	Index *int   `json:"index"`
	Kind  string `json:"kind"`
	Repo  string `json:"repo"`
}

func (s *Server) eventTriggersResponse(w http.ResponseWriter) {
	cfg := s.deps.Config
	rules := cfg.TriggerRulesSnapshot()
	version := config.TriggerRulesVersion(rules)
	agents := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		agents = append(agents, name)
	}
	sort.Strings(agents)
	repos := make([]map[string]string, 0, len(cfg.Project.Repos))
	for _, repo := range cfg.Project.Repos {
		label := repo
		if !strings.Contains(repo, "/") {
			label = cfg.Project.Org + "/" + repo
		}
		// Normalized eval events retain the configured repository spelling.
		repos = append(repos, map[string]string{"value": repo, "label": label})
	}
	w.Header().Set("ETag", version)
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]any{
		"rules": rules, "version": version, "agents": agents, "repositories": repos,
		"apply_mode": "next_evaluation", "requires_restart": false,
	})
}

func (s *Server) handleEventTriggersGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	s.eventTriggersResponse(w)
}

func (s *Server) handleEventTriggersPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	version := r.Header.Get("If-Match")
	if version == "" {
		jsonError(w, "reload Event triggers before saving (missing If-Match revision)", http.StatusPreconditionRequired)
		return
	}
	var body struct {
		Rules  *[]config.TriggerRule `json:"rules"`
		Preset *eventTriggerPreset   `json:"preset,omitempty"`
	}
	defer closeHTTPBody(r.Body)
	r.Body = http.MaxBytesReader(w, r.Body, maxDecodeBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || body.Rules == nil {
		jsonError(w, "invalid body: expected rules with name, agent, expr and integer priority; no enabled field is supported", http.StatusBadRequest)
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		jsonError(w, "invalid body: expected one JSON object", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	rules := *body.Rules
	if body.Preset != nil {
		p := body.Preset
		if p.Index == nil || *p.Index < 0 || *p.Index >= len(rules) || (p.Kind != celtrigger.KindIssueOpened && p.Kind != celtrigger.KindPROpened) {
			jsonError(w, "invalid first-seen event preset", http.StatusBadRequest)
			return
		}
		known := false
		for _, repo := range cfg.Project.Repos {
			if p.Repo == repo {
				known = true
				break
			}
		}
		if !known {
			jsonError(w, "select one repository managed by this hive; a preset cannot use all repositories or a wildcard", http.StatusBadRequest)
			return
		}
		rules[*p.Index].Expr = fmt.Sprintf("event.kind == %q && event.repo == %q", p.Kind, p.Repo)
	}
	current := cfg.TriggerRulesSnapshot()
	if version != config.TriggerRulesVersion(current) {
		jsonError(w, config.ErrTriggerRulesChanged.Error(), http.StatusPreconditionFailed)
		return
	}
	remaining := make(map[config.TriggerRule]int, len(current))
	for _, rule := range current {
		remaining[rule]++
	}
	for i, rule := range rules {
		unchanged := remaining[rule] > 0
		if unchanged {
			remaining[rule]--
		}
		// Preserve legacy rules verbatim; new/edited rules must target an
		// existing agent, even though the runtime skips unknown targets.
		if !unchanged {
			if strings.TrimSpace(rule.Name) == "" {
				jsonError(w, fmt.Sprintf("rule[%d]: name is required", i), http.StatusBadRequest)
				return
			}
			if _, ok := cfg.Agents[rule.Agent]; !ok {
				jsonError(w, fmt.Sprintf("rule[%d]: select an existing target agent", i), http.StatusBadRequest)
				return
			}
		}
	}
	if _, err := celtrigger.CompileFromConfig(&config.Config{Triggers: rules}); err != nil {
		jsonError(w, "invalid CEL trigger: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.deps.SkipReloadFunc != nil {
		s.deps.SkipReloadFunc()
	}
	if err := cfg.SaveTriggerRules(version, rules); err != nil {
		if errors.Is(err, config.ErrTriggerRulesChanged) {
			jsonError(w, err.Error(), http.StatusPreconditionFailed)
			return
		}
		s.logger.Error("event triggers persistence failed", "error", err)
		jsonError(w, "could not persist Event triggers; runtime rules unchanged. Check config storage permissions before retrying", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "config_event_triggers", auditDetail("rules", strconv.Itoa(len(rules))), "")
	s.refreshAfterMutation()
	s.eventTriggersResponse(w)
}
