package dashboard

// GET /api/config/provenance — "which layer set this field, and can I edit it?"
//
// Before this endpoint, that question was answerable only by reading pod boot
// logs for a single entrypoint line. The cost was concrete: a GitHub
// Enterprise hive was repaired on the FOURTH attempt, after patching the hub's
// meta.json, then clusters.json, then the spoke's ConfigMap — three layers
// INERT for github.app_id — before patching the dashboard overlay, the layer
// that actually wins.
//
// The response answers, per field: which layer won, where to write to change
// it, and whether that layer is writable AT ALL. The ConfigMap seed's answer
// is "nobody" — nothing in the system can write it (the hub holds only a
// read-only get; the spoke's RBAC omits configmaps), which is why editing it
// and restarting appears to do nothing.

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/hivecommons/hive/pkg/config"
)

// provenanceResponse is the JSON body of GET /api/config/provenance.
type provenanceResponse struct {
	// Fields lists every tracked field with its winning layer and writability.
	Fields []config.FieldOrigin `json:"fields"`
	// Layers describes each layer once, so a client can render a legend
	// without hardcoding the precedence order.
	Layers []layerInfo `json:"layers"`
	// OverlayRejected is true when a dashboard overlay exists but failed the
	// plausibility guard and was DISCARDED, meaning this hive is running on
	// the bare ConfigMap seed. On the live fleet seeds are 4–11% of the
	// running config, so this can mean a hive booted with a fraction of its
	// agent roster. It is surfaced here so the condition is visible without
	// reading pod logs.
	OverlayRejected bool `json:"overlay_rejected"`
	// OverlayRejectReason names why the overlay was rejected.
	OverlayRejectReason string `json:"overlay_reject_reason,omitempty"`
	// LastGoodUsed is true when a rejected overlay was replaced by the rolling
	// last-good config rather than the bare seed.
	LastGoodUsed bool `json:"last_good_used"`
	// GitHubRatchetFired is true when the seed's github block was preserved
	// because the overlay's looked like a placeholder.
	GitHubRatchetFired bool `json:"github_ratchet_fired"`
	// IdentityIssues names inconsistencies in the GitHub identity set
	// (app_id/app_slug/api_url/base_url are one atomic identity). Empty is the
	// healthy case. A GHE app_id with an empty api_url produces
	// "404 Integration not found" on every token request.
	IdentityIssues []string `json:"identity_issues,omitempty"`
	// SeedPath and OverlayPath name the files this report was derived from.
	SeedPath    string `json:"seed_path"`
	OverlayPath string `json:"overlay_path"`
	// OverlayPresent is false when no overlay exists on disk — the ordinary
	// first-boot state, not an error.
	OverlayPresent bool `json:"overlay_present"`
}

// layerInfo describes one configuration layer for the response legend.
type layerInfo struct {
	// Rank is the precedence rank; higher wins.
	Rank int `json:"rank"`
	// Name is the stable layer identifier, e.g. "dashboard-overlay".
	Name string `json:"name"`
	// Path is where the layer lives.
	Path string `json:"path"`
	// Writer names who can write it at runtime.
	Writer string `json:"writer"`
	// Writable is false when no running component can write it.
	Writable bool `json:"writable"`
}

// handleConfigProvenance serves GET /api/config/provenance.
//
// Owner-gated, matching handleConfigDownload: the report names config paths
// and effective values, so it is operator information rather than public
// status. Secret values are redacted regardless.
func (s *Server) handleConfigProvenance(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	seedPath := "/etc/hive/hive.yaml"
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" {
		seedPath = envCfg
	}
	overlayPath := config.DashboardOverlayFile

	seedYAML, err := os.ReadFile(seedPath)
	if err != nil {
		http.Error(w, "config file not found", http.StatusNotFound)
		return
	}
	// A missing overlay is normal on first boot, so a read error here is not
	// an error condition — it means "seed only", which MergeLayersYAML
	// represents as an empty overlay.
	overlayYAML, overlayErr := os.ReadFile(overlayPath)
	if overlayErr != nil {
		overlayYAML = nil
	}

	merged, prov, err := config.MergeLayersYAML(seedYAML, overlayYAML)
	if err != nil {
		http.Error(w, "config is not parsable: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := provenanceResponse{
		Fields:              prov.Report(merged),
		Layers:              describeLayers(),
		OverlayRejected:     prov.OverlayRejected,
		OverlayRejectReason: prov.OverlayRejectReason,
		LastGoodUsed:        prov.LastGoodUsed,
		GitHubRatchetFired:  prov.GitHubRatchetFired,
		SeedPath:            seedPath,
		OverlayPath:         overlayPath,
		OverlayPresent:      len(overlayYAML) > 0,
	}
	if merged != nil {
		resp.IdentityIssues = config.IdentitySetIssues(merged.GitHub)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// describeLayers returns the layer legend, highest precedence last so the
// order in the response matches the documented precedence order.
func describeLayers() []layerInfo {
	layers := []config.Layer{
		config.LayerSeed,
		config.LayerDashboardOverlay,
		config.LayerAgentOverlay,
		config.LayerConfigEnv,
	}
	out := make([]layerInfo, 0, len(layers))
	for _, l := range layers {
		out = append(out, layerInfo{
			Rank:     int(l),
			Name:     l.String(),
			Path:     l.Path(),
			Writer:   l.Writer(),
			Writable: l.Writable(),
		})
	}
	return out
}
