package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

var (
	configExportNow = time.Now
	// Var only so tests can redirect side-file reads away from the host.
	configExportRestrictionsFile = "/data/restrictions.conf"
	configExportAgentsDataDir    = "/data/agents"
)

type configExportResponse struct {
	SchemaVersion int                   `json:"schema_version"`
	GeneratedAt   string                `json:"generated_at"`
	Metadata      configExportMetadata  `json:"metadata"`
	Effective     any                   `json:"effective"`
	Layers        configExportLayers    `json:"layers"`
	SideFiles     configExportSideFiles `json:"side_files"`
	Provenance    []config.FieldOrigin  `json:"provenance"`
	Warnings      []string              `json:"warnings,omitempty"`
}

type configExportMetadata struct {
	HiveID    string                    `json:"hive_id"`
	Name      string                    `json:"name"`
	Org       string                    `json:"org,omitempty"`
	Version   string                    `json:"version"`
	Commit    string                    `json:"commit"`
	LayerPath map[string]configFileInfo `json:"layer_paths"`
}

type configExportLayers struct {
	Seed             configLayerSnapshot            `json:"seed"`
	DashboardOverlay configLayerSnapshot            `json:"dashboard_overlay"`
	AgentOverlays    map[string]configLayerSnapshot `json:"agent_overlays"`
	ConfigEnv        configLayerSnapshot            `json:"config_env"`
}

type configExportSideFiles struct {
	PromptTemplates    map[string]configFileSnapshot `json:"prompt_templates"`
	GlobalRestrictions configFileSnapshot            `json:"global_restrictions"`
	AgentRestrictions  map[string]configFileSnapshot `json:"agent_restrictions"`
	Sidebar            configFileSnapshot            `json:"sidebar"`
}

type configFileInfo struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Reason  string `json:"reason,omitempty"`
}

type configLayerSnapshot struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Reason  string `json:"reason,omitempty"`
	Content any    `json:"content,omitempty"`
	Keys    any    `json:"keys,omitempty"`
}

type configFileSnapshot struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Reason  string `json:"reason,omitempty"`
	Content any    `json:"content,omitempty"`
}

type configRedactionMarker struct {
	Redacted bool   `json:"redacted"`
	SHA256   string `json:"sha256"`
}

var configExportSecretKeyRE = regexp.MustCompile(`(?i)(token|secret|password|authorization|api[_-]?key|apikey|backup.*key|webhook.*secret|oauth.*secret|bob.*key)`)
var configEnvSecretKeyRE = regexp.MustCompile(`(?i)(TOKEN|KEY|SECRET|PASSWORD)`)

func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if !s.requestRoleAllowsOwner(r) {
		http.Error(w, "owner access required", http.StatusForbidden)
		return
	}
	if r.URL.Query().Get("include_secrets") != "" {
		http.Error(w, "include_secrets is not supported for plaintext config exports", http.StatusBadRequest)
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		http.Error(w, "config not loaded", http.StatusServiceUnavailable)
		return
	}

	resp, err := s.buildConfigExport()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	hiveID := safeFilenamePart(resp.Metadata.HiveID)
	if hiveID == "" {
		hiveID = "hive"
	}
	stamp := configExportNow().UTC().Format("20060102-1504")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="hive-config-%s-%s.json"`, hiveID, stamp))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

func (s *Server) buildConfigExport() (configExportResponse, error) {
	cfg := s.deps.Config
	seedPath := "/etc/hive/hive.yaml"
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" {
		seedPath = envCfg
	}
	envPath := configExportConfigEnvPath(seedPath)
	agentsDir := cfg.Data.AgentsDir
	if agentsDir == "" {
		agentsDir = "/data/agent-configs"
	}

	effective, err := configStructToExportValue(cfg)
	if err != nil {
		return configExportResponse{}, fmt.Errorf("marshaling effective config: %w", err)
	}
	effective = redactConfigValue(effective, "")

	seed := yamlLayerSnapshot(seedPath)
	overlay := yamlLayerSnapshot(config.DashboardOverlayFile)
	agentOverlays := agentOverlaySnapshots(agentsDir)
	configEnv := envLayerSnapshot(envPath)
	sideFiles := s.configExportSideFiles()
	version, commit := fleetReportBuildInfo()

	seedYAML, seedErr := os.ReadFile(seedPath)
	overlayYAML, overlayErr := os.ReadFile(config.DashboardOverlayFile)
	if overlayErr != nil {
		overlayYAML = nil
	}
	var provenance []config.FieldOrigin
	var warnings []string
	if seedErr == nil {
		if merged, prov, err := config.MergeLayersYAML(seedYAML, overlayYAML); err == nil {
			provenance = prov.Report(merged)
			if prov.OverlayRejected {
				warnings = append(warnings, "overlay_rejected: "+prov.OverlayRejectReason)
			}
			if merged != nil {
				for _, issue := range config.IdentitySetIssues(merged.GitHub) {
					warnings = append(warnings, "identity_issues: "+issue)
				}
			}
		} else {
			warnings = append(warnings, "provenance_unavailable: "+err.Error())
		}
	} else {
		warnings = append(warnings, "seed_unavailable: "+seedErr.Error())
	}
	sort.Strings(warnings)

	layerPaths := map[string]configFileInfo{
		"seed":              {Path: seedPath, Present: seed.Present, Reason: seed.Reason},
		"dashboard_overlay": {Path: config.DashboardOverlayFile, Present: overlay.Present, Reason: overlay.Reason},
		"agent_overlays":    {Path: agentsDir, Present: len(agentOverlays) > 0, Reason: missingDirReason(agentsDir)},
		"config_env":        {Path: envPath, Present: configEnv.Present, Reason: configEnv.Reason},
	}

	return configExportResponse{
		SchemaVersion: 1,
		GeneratedAt:   configExportNow().UTC().Format(time.RFC3339),
		Metadata: configExportMetadata{
			HiveID:    cfg.HiveID,
			Name:      cfg.Project.Name,
			Org:       cfg.Project.Org,
			Version:   version,
			Commit:    commit,
			LayerPath: layerPaths,
		},
		Effective: effective,
		Layers: configExportLayers{
			Seed:             seed,
			DashboardOverlay: overlay,
			AgentOverlays:    agentOverlays,
			ConfigEnv:        configEnv,
		},
		SideFiles:  sideFiles,
		Provenance: provenance,
		Warnings:   warnings,
	}, nil
}

func configStructToExportValue(v any) (any, error) {
	data, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	return parseYAMLForExport(data)
}

func parseYAMLForExport(data []byte) (any, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return normalizeYAMLValue(raw), nil
}

func normalizeYAMLValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalizeYAMLValue(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[fmt.Sprint(k)] = normalizeYAMLValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalizeYAMLValue(val)
		}
		return out
	default:
		return x
	}
}

func redactConfigValue(v any, key string) any {
	if key != "" && configExportSecretKeyRE.MatchString(key) && hasConfiguredValue(v) {
		return redactMarker(fmt.Sprint(v))
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = redactConfigValue(val, k)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redactConfigValue(val, key)
		}
		return out
	default:
		return x
	}
}

func hasConfiguredValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	default:
		return true
	}
}

func redactMarker(value string) configRedactionMarker {
	sum := sha256.Sum256([]byte(value))
	return configRedactionMarker{Redacted: true, SHA256: hex.EncodeToString(sum[:])[:12]}
}

func yamlLayerSnapshot(path string) configLayerSnapshot {
	snap := configLayerSnapshot{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		snap.Reason = err.Error()
		return snap
	}
	content, err := parseYAMLForExport(data)
	if err != nil {
		snap.Present = true
		snap.Reason = "parse error: " + err.Error()
		return snap
	}
	snap.Present = true
	snap.Content = redactConfigValue(content, "")
	return snap
}

func agentOverlaySnapshots(dir string) map[string]configLayerSnapshot {
	out := map[string]configLayerSnapshot{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".yaml")
		out[name] = yamlLayerSnapshot(filepath.Join(dir, entry.Name()))
	}
	return out
}

func envLayerSnapshot(path string) configLayerSnapshot {
	snap := configLayerSnapshot{Path: path}
	if path == "" {
		snap.Reason = "config.env not found"
		return snap
	}
	env, err := config.ParseEnvFile(path)
	if err != nil {
		snap.Reason = err.Error()
		return snap
	}
	keys := make(map[string]any, len(env))
	for k, v := range env {
		if configEnvSecretKeyRE.MatchString(k) && v != "" {
			keys[k] = redactMarker(v)
		} else {
			keys[k] = v
		}
	}
	snap.Present = true
	snap.Keys = keys
	return snap
}

func configExportConfigEnvPath(seedPath string) string {
	candidates := []string{
		strings.TrimSuffix(seedPath, "hive.yaml") + "config.env",
		"/etc/hive/config.env",
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return candidates[1]
}

func (s *Server) configExportSideFiles() configExportSideFiles {
	var agentNames []string
	if s.deps != nil && s.deps.Config != nil {
		for name := range s.deps.Config.Agents {
			agentNames = append(agentNames, name)
		}
	}
	return configExportSideFiles{
		PromptTemplates:    promptTemplateSnapshots(promptTemplateSaveDir),
		GlobalRestrictions: textFileSnapshot(configExportRestrictionsFile),
		AgentRestrictions:  agentRestrictionSnapshots(configExportAgentsDataDir, agentNames),
		Sidebar:            jsonFileSnapshot(sidebarFile),
	}
}

func promptTemplateSnapshots(dir string) map[string]configFileSnapshot {
	out := map[string]configFileSnapshot{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		out["__directory__"] = configFileSnapshot{Path: dir, Reason: err.Error()}
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		out[entry.Name()] = textFileSnapshot(path)
	}
	return out
}

func agentRestrictionSnapshots(dir string, configuredAgents []string) map[string]configFileSnapshot {
	out := map[string]configFileSnapshot{}
	for _, name := range configuredAgents {
		out[name] = textFileSnapshot(filepath.Join(dir, name, "restrictions.conf"))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		out["__directory__"] = configFileSnapshot{Path: dir, Reason: err.Error()}
		return out
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name(), "restrictions.conf")
		out[entry.Name()] = textFileSnapshot(path)
	}
	return out
}

func textFileSnapshot(path string) configFileSnapshot {
	snap := configFileSnapshot{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		snap.Reason = err.Error()
		return snap
	}
	snap.Present = true
	snap.Content = string(data)
	return snap
}

func jsonFileSnapshot(path string) configFileSnapshot {
	snap := configFileSnapshot{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		snap.Reason = err.Error()
		return snap
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		snap.Present = true
		snap.Reason = "parse error: " + err.Error()
		return snap
	}
	snap.Present = true
	snap.Content = redactConfigValue(v, "")
	return snap
}

func missingDirReason(path string) string {
	if _, err := os.Stat(path); err != nil {
		return err.Error()
	}
	return ""
}

func safeFilenamePart(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
