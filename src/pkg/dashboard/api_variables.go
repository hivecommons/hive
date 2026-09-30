package dashboard

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// substituteTemplateVars replaces ${VAR} placeholders in a prompt template
// with values from the running config, so the dashboard shows resolved content
// instead of raw variable names.
// handleVariablesList returns the operator-defined ${VAR} substitutions from
// the config `variables:` block, plus whether the exec/http resolver gates are
// enabled. It is READ-ONLY and never returns any secret value — only the
// variable name, type, scope, and (for env vars) the source env var NAME. This
// is the admin-facing view of custom variables; script/http variables and the
// security gates remain seed-only (GitOps/ConfigMap), so there is intentionally
// no companion write endpoint for them.
func (s *Server) handleVariablesList(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonResponse(w, map[string]any{"variables": []any{}, "exec_enabled": false, "http_enabled": false})
		return
	}
	v := s.deps.Config.Variables
	type varView struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Scope string `json:"scope"`
		// Source is a non-secret provenance hint: for env, the source env var
		// name; for static, "static"; for script, "script"; for http, the host.
		Source string `json:"source"`
	}
	out := make([]varView, 0, len(v.Defs))
	for name, def := range v.Defs {
		typ := def.Type
		if typ == "" {
			if def.Value != "" {
				typ = "static"
			} else {
				typ = "env"
			}
		}
		scope := def.Scope
		if scope == "" {
			scope = "template"
		}
		source := typ
		switch typ {
		case "env":
			if def.Env != "" {
				source = "env:" + def.Env
			} else {
				source = "env:" + name
			}
		case "http":
			if u, err := url.Parse(def.URL); err == nil && u.Host != "" {
				source = "http:" + u.Host
			} else {
				source = "http"
			}
		}
		out = append(out, varView{Name: name, Type: typ, Scope: scope, Source: source})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	jsonResponse(w, map[string]any{
		"variables":    out,
		"exec_enabled": v.Security.AllowExec,
		"http_enabled": v.Security.AllowHTTP,
	})
}

// variableNamePattern restricts dashboard-created variable names to the safe
// ${NAME} identifier shape (letters, digits, underscore; not starting with a
// digit). This is what a kick template references as ${NAME}.
var variableNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// looksLikeApiKeyValue heuristically flags a string that looks like a secret
// key value rather than a safe static config value — an sk-/ghp-/token-style
// prefix, or a long high-entropy run with no spaces. Mirrors the client-side
// guard that keeps operators from inlining a secret into hive.yaml (where
// Config.Save would persist it in plaintext).
func looksLikeApiKeyValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	lower := strings.ToLower(v)
	for _, p := range []string{"sk-", "sk_", "ghp_", "gho_", "github_pat_", "xoxb-", "bearer "} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	// A long token with no whitespace and no path/URL punctuation is likely a
	// secret, not a config value like "production" or "us-east-1".
	if len(v) >= 32 && !strings.ContainsAny(v, " /\\:.") {
		return true
	}
	return false
}

// handleAuthorizedUsersList returns the spoke's device-flow login allowlist —
// the users permitted to sign in to this hive and their roles — READ-ONLY. The
// list is authoritative on the hub (Manage Access) and propagated to the spoke
// via heartbeat; it is shown here so an operator can see who has access without
// editing it on the spoke. The first entry defaults to owner, later entries to
// read, matching AuthorizedRole's resolution.
func (s *Server) handleAuthorizedUsersList(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonResponse(w, map[string]any{"users": []any{}, "enforced": false})
		return
	}
	entries := s.deps.Config.Dashboard.AuthorizedUsers
	// names is the purely cosmetic key→display-name companion delivered by the
	// hub's heartbeat (AuthorizedUserNames). It is never consulted for
	// authorization — only AuthorizedUsers (the raw key) is — so a nil/empty
	// map here just means every row falls back to its raw key, exactly as
	// before this field existed.
	names := s.deps.Config.Dashboard.AuthorizedUserNames
	type userView struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		// DisplayName is the human-readable name for Username, when the hub
		// knows one (see AuthorizedUserNames). omitempty: absent means "no
		// known name", and the UI must render Username (the raw identity key)
		// instead — never blank, never "undefined".
		DisplayName string `json:"display_name,omitempty"`
	}
	out := make([]userView, 0, len(entries))
	for i, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		name, role := e, ""
		if idx := strings.LastIndex(e, ":"); idx >= 0 {
			name, role = strings.TrimSpace(e[:idx]), strings.TrimSpace(e[idx+1:])
		}
		if name == "" {
			continue
		}
		if role == "" {
			if i == 0 {
				role = "owner"
			} else {
				role = "read"
			}
		}
		out = append(out, userView{Username: name, Role: role, DisplayName: strings.TrimSpace(names[name])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	jsonResponse(w, map[string]any{
		// enforced == this is a direct-route spoke that gates logins by this list
		// (the heartbeat-only cluster); false means hub-proxied (the hub-reachable cluster), where nginx gates instead.
		"users":    out,
		"enforced": len(entries) > 0,
	})
}

// handleVariableUpsert creates or updates a single operator variable via the
// dashboard. It is deliberately restricted to the two SAFE resolver types —
// static and env — which cannot execute code or reach the network. script and
// http variables, and the exec/http security policy, are seed-only (GitOps): a
// user-writable overlay must never be able to introduce them, so this endpoint
// rejects them. The change is persisted to the dashboard overlay like any other
// dashboard config edit.
func (s *Server) handleVariableUpsert(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not loaded", http.StatusInternalServerError)
		return
	}
	name := r.PathValue("name")
	if !variableNamePattern.MatchString(name) {
		jsonError(w, "invalid variable name (use letters, digits, underscore; not starting with a digit)", http.StatusBadRequest)
		return
	}
	var body struct {
		Type    string  `json:"type"`
		Scope   string  `json:"scope"`
		Value   string  `json:"value"`
		Env     string  `json:"env"`
		Default *string `json:"default"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	switch body.Type {
	case "static", "env":
		// allowed
	case "script", "http":
		jsonError(w, "script and http variables can only be defined in the seed config (GitOps), not from the dashboard", http.StatusForbidden)
		return
	default:
		jsonError(w, "type must be 'static' or 'env'", http.StatusBadRequest)
		return
	}
	switch body.Scope {
	case "", "template", "config", "both":
		// allowed
	default:
		jsonError(w, "scope must be 'template', 'config', or 'both'", http.StatusBadRequest)
		return
	}
	// Guard against a secret value being pasted into a static var (it would be
	// persisted to hive.yaml in plaintext). Reuse the same heuristic the LiteLLM
	// key fields use.
	if body.Type == "static" && looksLikeApiKeyValue(body.Value) {
		jsonError(w, "that value looks like an API key or secret; use type 'env' pointing at an environment variable instead of inlining a secret", http.StatusBadRequest)
		return
	}

	def := config.VarDef{Type: body.Type, Scope: body.Scope, Value: body.Value, Env: body.Env, Default: body.Default}

	if s.deps.Config.Variables.Defs == nil {
		s.deps.Config.Variables.Defs = map[string]config.VarDef{}
	}
	s.deps.Config.Variables.Defs[name] = def
	if err := s.saveConfig(); err != nil {
		jsonError(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "variable_upsert", auditDetail("name", name, "type", body.Type), "")
	jsonResponse(w, map[string]any{"status": "saved", "name": name})
}

// handleVariableDelete removes an operator variable. Only static/env variables
// are dashboard-managed, so this refuses to delete a script/http variable
// (those are seed-only and must be removed via the seed config).
func (s *Server) handleVariableDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not loaded", http.StatusInternalServerError)
		return
	}
	name := r.PathValue("name")
	def, ok := s.deps.Config.Variables.Defs[name]
	if !ok {
		jsonError(w, "variable not found", http.StatusNotFound)
		return
	}
	if def.Type == "script" || def.Type == "http" {
		jsonError(w, "script and http variables are seed-managed and cannot be deleted from the dashboard", http.StatusForbidden)
		return
	}
	delete(s.deps.Config.Variables.Defs, name)
	if err := s.saveConfig(); err != nil {
		jsonError(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "variable_delete", auditDetail("name", name), "")
	jsonResponse(w, map[string]any{"status": "deleted", "name": name})
}
