package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// "Let hive decide" settings: decisions.delegate, owner only.
//
// GET  /api/config/decisions/delegate returns the block with its defaults
// resolved (enabled nil = on, llm.enabled nil = off).
// PUT  /api/config/decisions/delegate changes it. Every field is optional and
// an absent one is left alone, except repos, which is the FULL per-repository
// override map when sent: {"owner/repo": {"enabled": false}}, an entry with
// enabled null or {} is dropped (the repository then follows the hive-wide
// switch), and {} clears every override. The admin MCP operation
// decisions.delegate_settings merges one repository's change into the current
// map and sends the whole map.
//
// The block holds no secret: the LLM step calls the governor reviewer endpoint
// (governor.trajectory / governor.litellm) with that endpoint's key, which is
// named there by environment variable or file, never here. llm.model is the
// only free-text field, so a value that looks like an API key is refused on
// PUT and masked on GET.
//
// Nothing here starts a decision, merges or changes what a delegated decision
// does: the change is saved, audited as config_decisions_delegate, and read
// per request by the Let hive decide endpoints, the reviewer-question sweep
// and the decider turn, so it applies without a restart.
const (
	auditConfigDecisionsDelegate = "config_decisions_delegate"

	// delegateLLMModelMaxLen bounds llm.model; real model names are far
	// shorter.
	delegateLLMModelMaxLen = 200
)

func (s *Server) registerDecisionsDelegateRoutes() {
	s.mux.HandleFunc("GET /api/config/decisions/delegate", s.handleDecisionsDelegateGet)
	s.mux.HandleFunc("PUT /api/config/decisions/delegate", s.handleDecisionsDelegatePut)
}

// delegateLLMModelPattern is the shape of a model name: provider/model,
// model:tag, model@version. No spaces, quotes or ${...} placeholders.
var delegateLLMModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)

// delegateKeyRunPattern is a run of 32 or more letters and digits with no
// separator, the shape of a hex or base64 secret rather than a model name.
var delegateKeyRunPattern = regexp.MustCompile(`[A-Za-z0-9]{32,}`)

// delegateKeyPrefixes are the prefixes of API keys and tokens people paste.
var delegateKeyPrefixes = []string{"sk-", "sk_", "ghp_", "gho_", "ghs_", "ghu_", "github_pat_", "xoxb-", "xoxp-", "hf_", "akia", "aiza", "bearer"}

// delegateModelLooksLikeKey reports whether a llm.model value looks like an
// API key or token instead of a model name.
func delegateModelLooksLikeKey(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, p := range delegateKeyPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return delegateKeyRunPattern.MatchString(model)
}

// validateDelegateLLMModel checks a llm.model value; "" clears the override.
func validateDelegateLLMModel(model string) error {
	if model == "" {
		return nil
	}
	if delegateModelLooksLikeKey(model) {
		return errors.New("llm.model looks like an API key or token; it takes a model name only. The model's key is set on the governor reviewer endpoint (governor.trajectory or governor.litellm), by environment variable or file")
	}
	if len(model) > delegateLLMModelMaxLen {
		return fmt.Errorf("llm.model must be at most %d characters", delegateLLMModelMaxLen)
	}
	if !delegateLLMModelPattern.MatchString(model) {
		return errors.New("llm.model must be a model name such as provider/model or model:tag (letters, digits and . _ : / @ + -)")
	}
	return nil
}

func (s *Server) handleDecisionsDelegateGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, decisionsDelegateSectionResponse(s.deps.Config))
}

// decisionsDelegateSectionResponse renders decisions.delegate for the
// dashboard and settings_read. A stored llm.model that looks like a key is
// never returned: model is "" and model_masked is true.
func decisionsDelegateSectionResponse(cfg *config.Config) map[string]interface{} {
	d := cfg.Decisions.Delegate
	repos := map[string]interface{}{}
	for key, rc := range d.Repos {
		if rc.Enabled != nil {
			repos[key] = map[string]interface{}{"enabled": *rc.Enabled}
		}
	}
	model := d.LLMModel()
	masked := model != "" && validateDelegateLLMModel(model) != nil
	if masked {
		model = ""
	}
	route := reviewerTurnRoute(cfg, model)
	effectiveModel := route.Model
	if masked {
		effectiveModel = ""
	}
	projectRepos := make([]string, 0, len(cfg.Project.Repos))
	for _, repo := range cfg.Project.Repos {
		if full := config.QualifyRepo(cfg.Project.Org, repo); full != "" {
			projectRepos = append(projectRepos, full)
		}
	}
	return map[string]interface{}{
		"enabled":     d.IsEnabled(),
		"enabled_set": d.Enabled != nil,
		"repos":       repos,
		"llm": map[string]interface{}{
			"enabled":      d.LLMEnabled(),
			"model":        model,
			"model_masked": masked,
		},
		// llm_reviewer_ready says whether the decider turn can run; with
		// llm on and this false, every decision keeps the rules' reversible
		// default. llm_reviewer_path is where it runs (http: the reviewer
		// endpoint; claude-cli: the Claude CLI on the hive's Claude login,
		// with no tools; none) and llm_reviewer_reason why it cannot, when
		// it cannot (governor.reviewer_backend picks the path).
		// llm_model_effective is the model the turn uses (the override,
		// else the reviewer model; empty on claude-cli means the CLI's own
		// default). The endpoint and its key are never returned.
		"llm_reviewer_ready":       route.Ready && !masked,
		"llm_reviewer_path":        route.Path,
		"llm_reviewer_reason":      delegateReviewerReason(route, masked),
		"llm_model_effective":      effectiveModel,
		"effect_timeout":           strings.TrimSpace(d.EffectTimeout),
		"effect_timeout_effective": d.EffectTimeoutDuration().String(),
		"project_repos":            projectRepos,
	}
}

// delegateReviewerReason is llm_reviewer_reason: why the decider turn cannot
// run, or "" when it can.
func delegateReviewerReason(route config.ReviewerTurnRoute, masked bool) string {
	if masked {
		return "the stored decisions.delegate.llm.model looks like an API key, so it is not used; enter a model name"
	}
	if route.Ready {
		return ""
	}
	return route.Reason
}

type decisionsDelegateRepoSetting struct {
	Enabled *bool `json:"enabled"`
}

type decisionsDelegateBody struct {
	Enabled *bool `json:"enabled"`
	// Repos is the FULL override map when present; absent leaves it alone.
	Repos map[string]*decisionsDelegateRepoSetting `json:"repos"`
	LLM   *struct {
		Enabled *bool   `json:"enabled"`
		Model   *string `json:"model"`
	} `json:"llm"`
	EffectTimeout *string `json:"effect_timeout"`
}

func (s *Server) handleDecisionsDelegatePut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body decisionsDelegateBody
	defer closeHTTPBody(r.Body)
	r.Body = http.MaxBytesReader(w, r.Body, maxDecodeBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		jsonError(w, "invalid body: expected {enabled, repos: {owner/repo: {enabled}}, llm: {enabled, model}, effect_timeout}; no other field is accepted: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		jsonError(w, "invalid body: expected one JSON object", http.StatusBadRequest)
		return
	}

	cfg := s.deps.Config
	before := cfg.Decisions.Delegate
	next, msg := s.applyDecisionsDelegateBody(before, body)
	if msg != "" {
		jsonError(w, msg, http.StatusBadRequest)
		return
	}
	changes := decisionsDelegateChanges(before, next)
	if len(changes) == 0 {
		jsonResponse(w, decisionsDelegateSectionResponse(cfg))
		return
	}

	cfg.Decisions.Delegate = next
	if err := s.saveConfig(); err != nil {
		cfg.Decisions.Delegate = before
		s.logger.Error("failed to persist config after decisions.delegate update", "error", err)
		jsonError(w, "failed to persist Let hive decide settings", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, auditConfigDecisionsDelegate, auditDetail("section", "decisions_delegate", "changes", strings.Join(changes, "; ")), "")
	s.refreshAndPersist()
	jsonResponse(w, decisionsDelegateSectionResponse(cfg))
}

// applyDecisionsDelegateBody returns the block the body asks for, built on a
// copy of current so a refused body changes nothing. A non-empty msg is the
// 400 to return.
func (s *Server) applyDecisionsDelegateBody(current config.DelegateDecisionConfig, body decisionsDelegateBody) (config.DelegateDecisionConfig, string) {
	next := current
	next.Repos = copyDelegateRepos(current.Repos)
	if current.LLM != nil {
		llm := *current.LLM
		next.LLM = &llm
	}

	if body.Enabled != nil {
		v := *body.Enabled
		next.Enabled = &v
	}
	if body.Repos != nil {
		repos, msg := s.decisionsDelegateRepos(current.Repos, body.Repos)
		if msg != "" {
			return current, msg
		}
		next.Repos = repos
	}
	if body.LLM != nil {
		if next.LLM == nil {
			next.LLM = &config.DelegateDecisionLLMConfig{}
		}
		if body.LLM.Enabled != nil {
			v := *body.LLM.Enabled
			next.LLM.Enabled = &v
		}
		if body.LLM.Model != nil {
			model := strings.TrimSpace(*body.LLM.Model)
			if err := validateDelegateLLMModel(model); err != nil {
				return current, err.Error()
			}
			next.LLM.Model = model
		}
		if next.LLM.Enabled == nil && next.LLM.Model == "" {
			next.LLM = nil
		}
	}
	if body.EffectTimeout != nil {
		raw := strings.TrimSpace(*body.EffectTimeout)
		if raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 {
				return current, "effect_timeout must be a duration above zero, such as 30m or 2h, or empty for the default (30m)"
			}
			raw = d.String()
		}
		next.EffectTimeout = raw
	}
	return next, ""
}

// decisionsDelegateRepos validates the full override map a PUT sends. Each
// key is owner/repo or a bare name, stored as owner/repo. A key must name a
// repository in project.repos, unless the current map already holds it, so a
// stale override for a repository no longer watched can still be kept or
// removed. An entry whose enabled is null is dropped.
func (s *Server) decisionsDelegateRepos(current map[string]config.DelegateDecisionRepoConfig, sent map[string]*decisionsDelegateRepoSetting) (map[string]config.DelegateDecisionRepoConfig, string) {
	org := s.deps.Config.Project.Org
	held := map[string]string{}
	for key := range current {
		held[strings.ToLower(config.QualifyRepo(org, strings.TrimSpace(key)))] = key
	}
	keys := make([]string, 0, len(sent))
	for key := range sent {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := map[string]config.DelegateDecisionRepoConfig{}
	seen := map[string]bool{}
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		full := config.QualifyRepo(org, trimmed)
		owner, name, ok := strings.Cut(full, "/")
		if trimmed == "" || !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(full, " \t#") {
			return nil, "repos: " + key + " is not an owner/repo name"
		}
		lower := strings.ToLower(full)
		if seen[lower] {
			return nil, "repos names " + full + " twice"
		}
		seen[lower] = true
		if _, wasHeld := held[lower]; !wasHeld && !s.watchesRepo(full) {
			return nil, "repos: " + key + " is not in project.repos"
		}
		entry := sent[key]
		if entry == nil || entry.Enabled == nil {
			continue
		}
		v := *entry.Enabled
		out[full] = config.DelegateDecisionRepoConfig{Enabled: &v}
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

func copyDelegateRepos(in map[string]config.DelegateDecisionRepoConfig) map[string]config.DelegateDecisionRepoConfig {
	if in == nil {
		return nil
	}
	out := make(map[string]config.DelegateDecisionRepoConfig, len(in))
	for k, v := range in {
		if v.Enabled != nil {
			b := *v.Enabled
			v.Enabled = &b
		}
		out[k] = v
	}
	return out
}

// decisionsDelegateChanges lists what differs between two blocks, for the
// audit row: "llm.enabled: false → true".
func decisionsDelegateChanges(before, after config.DelegateDecisionConfig) []string {
	var out []string
	add := func(field, from, to string) {
		if from != to {
			out = append(out, field+": "+from+" → "+to)
		}
	}
	add("enabled", delegateTriState(before.Enabled, "default on"), delegateTriState(after.Enabled, "default on"))
	add("repos", delegateReposSummary(before.Repos), delegateReposSummary(after.Repos))
	var bLLM, aLLM config.DelegateDecisionLLMConfig
	if before.LLM != nil {
		bLLM = *before.LLM
	}
	if after.LLM != nil {
		aLLM = *after.LLM
	}
	add("llm.enabled", delegateTriState(bLLM.Enabled, "default off"), delegateTriState(aLLM.Enabled, "default off"))
	add("llm.model", delegateModelForAudit(bLLM.Model), delegateModelForAudit(aLLM.Model))
	add("effect_timeout", delegateOrDefault(before.EffectTimeout), delegateOrDefault(after.EffectTimeout))
	return out
}

func delegateTriState(v *bool, unset string) string {
	if v == nil {
		return unset
	}
	return fmt.Sprint(*v)
}

func delegateReposSummary(repos map[string]config.DelegateDecisionRepoConfig) string {
	keys := make([]string, 0, len(repos))
	for k, rc := range repos {
		if rc.Enabled != nil {
			keys = append(keys, k+"="+fmt.Sprint(*rc.Enabled))
		}
	}
	if len(keys) == 0 {
		return "none"
	}
	sort.Strings(keys)
	return strings.Join(keys, " ")
}

func delegateModelForAudit(model string) string {
	model = strings.TrimSpace(model)
	switch {
	case model == "":
		return "reviewer model"
	case validateDelegateLLMModel(model) != nil:
		return "(masked)"
	}
	return model
}

func delegateOrDefault(v string) string {
	if strings.TrimSpace(v) == "" {
		return "default"
	}
	return strings.TrimSpace(v)
}
