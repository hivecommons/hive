package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
)

// brandingCSSPath returns where an operator's override stylesheet lives.
//
// Derived from AgentsDir's parent rather than a config field of its own:
// DataConfig has no single "root", but every deployment already places
// agents_dir on the persistent data volume, so its parent is that volume.
// HIVE_BRANDING_CSS overrides it outright for anything unusual. Living on the
// data volume means the override survives image upgrades and pod restarts.
func (s *Server) brandingCSSPath() string {
	if p := os.Getenv("HIVE_BRANDING_CSS"); p != "" {
		return p
	}
	dir := "/data"
	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Data.AgentsDir != "" {
		dir = filepath.Dir(s.deps.Config.Data.AgentsDir)
	}
	return filepath.Join(dir, "branding", "custom.css")
}

// handleBrandingCSS serves the operator's override stylesheet, or 404s when
// there is none. The index document links to this path unconditionally, so a
// missing file is the normal case and must be cheap and quiet.
func (s *Server) handleBrandingCSS(w http.ResponseWriter, r *http.Request) {
	path := s.brandingCSSPath()
	b, err := os.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Never let a stale override outlive an edit: this is an operator-facing
	// customisation knob, not a hot asset, so correctness beats caching.
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}
