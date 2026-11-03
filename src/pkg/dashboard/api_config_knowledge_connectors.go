package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// KnowledgeConnectorRuntime is the running connector syncer as seen by the
// dashboard. It is nil when no syncer is wired, in which case the status
// endpoint reports configuration only and "Sync now" answers 503.
type KnowledgeConnectorRuntime interface {
	Statuses() []connector.Status
	SyncNow(ctx context.Context, name string) (connector.Status, error)
}

// knowledgeConnectorsMu serializes read-modify-write of knowledge.connectors.
var knowledgeConnectorsMu sync.Mutex

type knowledgeConnectorAuthJSON struct {
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

// knowledgeConnectorJSON is the dashboard form of one knowledge.connectors
// entry. Auth carries only an env var name or a secret file path.
type knowledgeConnectorJSON struct {
	Name     string                     `json:"name"`
	Type     string                     `json:"type"`
	Enabled  bool                       `json:"enabled"`
	Interval string                     `json:"interval,omitempty"`
	Layer    string                     `json:"layer"`
	Scope    map[string]string          `json:"scope,omitempty"`
	Auth     knowledgeConnectorAuthJSON `json:"auth"`
	// CredentialSet is response-only and reports whether the referenced
	// credential resolves (set/unset); the value is never returned.
	CredentialSet *bool `json:"credential_set,omitempty"`
}

// context7AutoImportJSON is one legacy knowledge.context7.auto_import entry.
type context7AutoImportJSON struct {
	ID    string `json:"id"`
	Query string `json:"query,omitempty"`
}

// context7ValidateSearch is the live Context7 search used by Validate; a var
// so tests can stub it.
var context7ValidateSearch = knowledge.SearchContext7

type knowledgeConnectorStatusRow struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Layer     string `json:"layer"`
	Status    string `json:"status"`
	Enabled   bool   `json:"enabled"`
	Interval  string `json:"interval,omitempty"`
	LastSync  string `json:"last_sync,omitempty"`
	Pages     int    `json:"pages"`
	Facts     int    `json:"facts"`
	LastError string `json:"last_error,omitempty"`
}

// context7APIKey resolves the Context7 key for c: the referenced auth source,
// else the CONTEXT7_API_KEY environment variable.
func context7APIKey(c config.KnowledgeConnector) (string, error) {
	if c.Auth.Env != "" || c.Auth.File != "" {
		return connector.Auth{Env: c.Auth.Env, File: c.Auth.File}.Secret()
	}
	return strings.TrimSpace(os.Getenv(connector.Context7APIKeyEnv)), nil
}

func knowledgeConnectorToJSON(c config.KnowledgeConnector) knowledgeConnectorJSON {
	var credSet *bool
	if c.Type == connector.TypeContext7 {
		key, err := context7APIKey(c)
		set := err == nil && key != ""
		credSet = &set
	}
	return knowledgeConnectorJSON{
		CredentialSet: credSet,
		Name:          c.Name,
		Type:          c.Type,
		Enabled:       c.IsEnabled(),
		Interval:      c.Interval,
		Layer:         c.Layer,
		Scope:         c.Scope,
		Auth:          knowledgeConnectorAuthJSON{Env: c.Auth.Env, File: c.Auth.File},
	}
}

func (c knowledgeConnectorJSON) toConfig() config.KnowledgeConnector {
	enabled := c.Enabled
	scope := map[string]string{}
	for k, v := range c.Scope {
		if k = strings.TrimSpace(k); k != "" {
			scope[k] = strings.TrimSpace(v)
		}
	}
	if len(scope) == 0 {
		scope = nil
	}
	return config.KnowledgeConnector{
		Name:     strings.TrimSpace(c.Name),
		Type:     strings.TrimSpace(c.Type),
		Enabled:  &enabled,
		Interval: strings.TrimSpace(c.Interval),
		Layer:    strings.TrimSpace(c.Layer),
		Scope:    scope,
		Auth:     config.KnowledgeConnectorAuth{Env: strings.TrimSpace(c.Auth.Env), File: strings.TrimSpace(c.Auth.File)},
	}
}

// knowledgeConnectorRuntimeConfig converts a config entry to the connector package form.
func knowledgeConnectorRuntimeConfig(c config.KnowledgeConnector) (connector.ConnectorConfig, error) {
	d, err := c.IntervalDuration()
	if err != nil {
		return connector.ConnectorConfig{}, err
	}
	return connector.ConnectorConfig{
		Name:     c.Name,
		Type:     c.Type,
		Enabled:  c.IsEnabled(),
		Interval: d,
		Layer:    c.Layer,
		Scope:    c.Scope,
		Auth:     connector.Auth{Env: c.Auth.Env, File: c.Auth.File},
	}, nil
}

// validateKnowledgeConnectorEntry runs the config-level checks and then the
// connector's own Validate (scope keys, auth) through the registry. It does
// no network I/O. When the entry names a credential source, that source must
// also resolve locally (env var set / file readable and non-empty).
func validateKnowledgeConnectorEntry(c config.KnowledgeConnector) error {
	if err := config.ValidateKnowledgeConnectors([]config.KnowledgeConnector{c}); err != nil {
		return err
	}
	rc, err := knowledgeConnectorRuntimeConfig(c)
	if err != nil {
		return err
	}
	if _, err := connector.DefaultRegistry().New(rc, connector.Deps{}); err != nil {
		return err
	}
	if rc.Auth.Configured() {
		if _, err := rc.Auth.Secret(); err != nil {
			return err
		}
	}
	return nil
}

func knowledgeConnectorsResponse(cfg *config.Config) map[string]interface{} {
	rows := make([]knowledgeConnectorJSON, 0, len(cfg.Knowledge.Connectors))
	for _, c := range cfg.Knowledge.Connectors {
		rows = append(rows, knowledgeConnectorToJSON(c))
	}
	imports := make([]context7AutoImportJSON, 0, len(cfg.Knowledge.Context7.AutoImport))
	for _, it := range cfg.Knowledge.Context7.AutoImport {
		imports = append(imports, context7AutoImportJSON{ID: it.ID, Query: it.Query})
	}
	return map[string]interface{}{
		"connectors":           rows,
		"types":                connector.DefaultRegistry().Types(),
		"layers":               []string{"personal", "project", "org", "community"},
		"context7_auto_import": imports,
	}
}

// handleKnowledgeConnectorsGet returns the configured connectors. Credentials
// are never stored in the config, only the env var name or file path.
func (s *Server) handleKnowledgeConnectorsGet(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	knowledgeConnectorsMu.Lock()
	defer knowledgeConnectorsMu.Unlock()
	jsonResponse(w, knowledgeConnectorsResponse(s.deps.Config))
}

// handleKnowledgeConnectorsPut replaces knowledge.connectors with the posted
// list (owner-only). The whole list is validated before anything is applied.
func (s *Server) handleKnowledgeConnectorsPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Connectors []json.RawMessage `json:"connectors"`
		// Context7AutoImport, when present, replaces knowledge.context7.auto_import.
		Context7AutoImport *[]context7AutoImportJSON `json:"context7_auto_import"`
	}
	if err := decodeBody(r, &body); err != nil || body.Connectors == nil {
		jsonError(w, "invalid body: expected {\"connectors\": [...]}", http.StatusBadRequest)
		return
	}
	next := make([]config.KnowledgeConnector, 0, len(body.Connectors))
	for i, raw := range body.Connectors {
		var c knowledgeConnectorJSON
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			jsonError(w, "connectors["+strconv.Itoa(i)+"]: "+err.Error()+" (auth accepts only env or file)", http.StatusBadRequest)
			return
		}
		next = append(next, c.toConfig())
	}
	if err := config.ValidateKnowledgeConnectors(next); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	reg := connector.DefaultRegistry()
	for _, c := range next {
		rc, err := knowledgeConnectorRuntimeConfig(c)
		if err == nil {
			_, err = reg.New(rc, connector.Deps{})
		}
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	var imports []config.KnowledgeContext7Import
	if body.Context7AutoImport != nil {
		seen := map[string]bool{}
		for i, it := range *body.Context7AutoImport {
			id := strings.TrimSpace(it.ID)
			if !strings.HasPrefix(id, "/") || strings.ContainsAny(id, " \t?#") {
				jsonError(w, "context7_auto_import["+strconv.Itoa(i)+"].id must look like /org/project", http.StatusBadRequest)
				return
			}
			if seen[id] {
				jsonError(w, "context7_auto_import["+strconv.Itoa(i)+"].id "+id+" is listed twice", http.StatusBadRequest)
				return
			}
			seen[id] = true
			imports = append(imports, config.KnowledgeContext7Import{ID: id, Query: strings.TrimSpace(it.Query)})
		}
	}

	knowledgeConnectorsMu.Lock()
	defer knowledgeConnectorsMu.Unlock()
	s.deps.Config.Knowledge.Connectors = next
	if body.Context7AutoImport != nil {
		s.deps.Config.Knowledge.Context7.AutoImport = imports
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after knowledge connectors update", "error", err)
	}
	names := make([]string, 0, len(next))
	for _, c := range next {
		names = append(names, c.Name)
	}
	s.auditFromRequest(r, "config_knowledge_connectors", auditDetail("connectors", strings.Join(names, ",")), "")
	s.refreshAndPersist()
	jsonResponse(w, knowledgeConnectorsResponse(s.deps.Config))
}

// knowledgeConnectorPill maps a connector's runtime state to ok / syncing /
// error / disabled, or "pending" before its first sync attempt.
func knowledgeConnectorPill(enabled bool, st *connector.Status) string {
	switch {
	case !enabled:
		return "disabled"
	case st != nil && st.Running:
		return "syncing"
	case st != nil && st.LastError != "":
		return "error"
	case st != nil && !st.LastSync.IsZero():
		return "ok"
	default:
		return "pending"
	}
}

func (s *Server) knowledgeConnectorStatusRows() []knowledgeConnectorStatusRow {
	byName := map[string]connector.Status{}
	if s.deps.KnowledgeConnectors != nil {
		for _, st := range s.deps.KnowledgeConnectors.Statuses() {
			byName[st.Name] = st
		}
	}
	rows := make([]knowledgeConnectorStatusRow, 0, len(s.deps.Config.Knowledge.Connectors))
	for _, c := range s.deps.Config.Knowledge.Connectors {
		row := knowledgeConnectorStatusRow{Name: c.Name, Type: c.Type, Layer: c.Layer, Enabled: c.IsEnabled(), Interval: c.Interval}
		var stp *connector.Status
		if st, ok := byName[c.Name]; ok {
			stp = &st
			row.Pages, row.Facts, row.LastError = st.Pages, st.Facts, st.LastError
			if !st.LastSync.IsZero() {
				row.LastSync = st.LastSync.UTC().Format(time.RFC3339)
			}
		}
		row.Status = knowledgeConnectorPill(row.Enabled, stp)
		rows = append(rows, row)
	}
	return rows
}

// handleKnowledgeConnectorsStatus returns per-connector status for the
// Settings → Knowledge table (configuration only when no syncer is running).
func (s *Server) handleKnowledgeConnectorsStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	knowledgeConnectorsMu.Lock()
	rows := s.knowledgeConnectorStatusRows()
	knowledgeConnectorsMu.Unlock()
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	jsonResponse(w, map[string]interface{}{
		"connectors": rows,
		"runtime":    s.deps.KnowledgeConnectors != nil,
		"publish":    s.knowledgePublishView(),
	})
}

// handleKnowledgeConnectorsValidate dry-run validates a connector entry
// (owner-only): config rules, the connector's own scope/auth checks, and that
// the named credential source resolves locally. It performs no network I/O and
// never echoes a credential.
func (s *Server) handleKnowledgeConnectorsValidate(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var c knowledgeConnectorJSON
	defer closeHTTPBody(r.Body)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDecodeBodyBytes))
	if err == nil {
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		err = dec.Decode(&c)
	}
	if err != nil {
		jsonError(w, "invalid body: "+err.Error()+" (auth accepts only env or file)", http.StatusBadRequest)
		return
	}
	entry := c.toConfig()
	if err := validateKnowledgeConnectorEntry(entry); err != nil {
		jsonResponse(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	if entry.Type == connector.TypeContext7 {
		if err := validateContext7Live(r.Context(), entry); err != nil {
			jsonResponse(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
	}
	jsonResponse(w, map[string]interface{}{"ok": true})
}

// validateContext7Live proves the key reference works by running one Context7
// library search for the first configured library. The key is never echoed.
func validateContext7Live(ctx context.Context, c config.KnowledgeConnector) error {
	key, err := context7APIKey(c)
	if err != nil {
		return err
	}
	name := "react"
	if ids := strings.Split(c.Scope["library_ids"], ","); len(ids) > 0 {
		if base := path.Base(strings.TrimSpace(ids[0])); base != "." && base != "/" {
			name = base
		}
	}
	if _, err := context7ValidateSearch(ctx, name, key); err != nil {
		return err
	}
	return nil
}

// handleKnowledgeConnectorsSync starts an immediate sync of one connector
// (owner-only). The sync runs in the background; poll the status endpoint.
func (s *Server) handleKnowledgeConnectorsSync(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	known := false
	for _, c := range s.deps.Config.Knowledge.Connectors {
		if c.Name == name {
			known = true
			break
		}
	}
	if !known {
		jsonError(w, "unknown connector", http.StatusNotFound)
		return
	}
	rt := s.deps.KnowledgeConnectors
	if rt == nil {
		jsonError(w, "connector syncer is not running (restart the hive after enabling knowledge connectors)", http.StatusServiceUnavailable)
		return
	}
	for _, st := range rt.Statuses() {
		if st.Name == name && st.Running {
			jsonError(w, "sync already in progress", http.StatusConflict)
			return
		}
	}
	parent := s.deps.Ctx
	if parent == nil {
		parent = context.Background()
	}
	go func() {
		ctx, cancel := context.WithTimeout(parent, connector.DefaultSyncTimeout)
		defer cancel()
		if _, err := rt.SyncNow(ctx, name); err != nil {
			s.logger.Warn("knowledge connector manual sync failed", "name", name, "error", err)
		}
	}()
	s.auditFromRequest(r, "knowledge_connector_sync", auditDetail("name", name), "")
	jsonStatusResponse(w, http.StatusAccepted, map[string]interface{}{"ok": true, "name": name})
}
