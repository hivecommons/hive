package dashboard

import (
	"bytes"
	"encoding/json"
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

// Branding holds the operator-overridable strings baked into the served index
// document. Every field is optional; an empty field leaves the shipped default
// alone, so a partial file is valid and a missing file changes nothing.
type Branding struct {
	// ProductName replaces the "HIVE" wordmark in the sidebar.
	ProductName string `json:"product_name"`
	// Tagline replaces "GATEWAY DASHBOARD" beneath it.
	Tagline string `json:"tagline"`
	// Mark replaces the 🐝 glyph — in both sidebars, the <h1>, and the
	// favicon, which is an inline SVG data: URI wrapping the same character.
	Mark string `json:"mark"`
	// Title replaces the <title> and og:title text.
	Title string `json:"title"`
}

// brandingJSONPath is the strings file, beside custom.css on the data volume.
func (s *Server) brandingJSONPath() string {
	if p := os.Getenv("HIVE_BRANDING_JSON"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(s.brandingCSSPath()), "branding.json")
}

// loadBranding reads the strings file. A missing or malformed file yields the
// zero value, which is a no-op — branding must never be able to break startup.
func (s *Server) loadBranding() Branding {
	var b Branding
	raw, err := os.ReadFile(s.brandingJSONPath())
	if err != nil {
		return b
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		if s.logger != nil {
			s.logger.Warn("branding.json is not valid JSON; ignoring", "path", s.brandingJSONPath(), "error", err)
		}
		return Branding{}
	}
	return b
}

// applyBranding rewrites the shipped defaults in the index document.
//
// Substitution is ANCHORED TO EXACT MARKUP, never a bare string replace: the
// document is ~1.3 MB of inline HTML/CSS/JS and a loose replace of "HIVE" or
// "🐝" would corrupt unrelated script and prose. Each anchor below is the
// literal element that renders the thing being replaced, so a miss is a no-op
// rather than a mangled page — and if upstream markup changes, branding
// silently stops applying instead of breaking the dashboard.
func applyBranding(raw []byte, b Branding) []byte {
	repl := func(in []byte, old, new string) []byte {
		if new == "" || old == "" || !bytes.Contains(in, []byte(old)) {
			return in
		}
		return bytes.ReplaceAll(in, []byte(old), []byte(new))
	}

	if b.Mark != "" {
		raw = repl(raw, `<span class="oc-logo-icon">🐝</span>`,
			`<span class="oc-logo-icon">`+b.Mark+`</span>`)
		raw = repl(raw, `<span class="bee">🐝</span>`,
			`<span class="bee">`+b.Mark+`</span>`)
		// The Getting Started flyer writes its bee as an HTML ENTITY, not the
		// literal glyph — anchoring only on the emoji misses it and leaves a
		// bee animating on an otherwise rebranded page.
		raw = repl(raw, `<span class="wb-bee">&#x1F41D;</span>`,
			`<span class="wb-bee">`+b.Mark+`</span>`)
		// The favicon is an SVG data: URI with the same glyph inside a <text>.
		raw = repl(raw, `<text y='.9em' font-size='90'>🐝</text>`,
			`<text y='.9em' font-size='90'>`+b.Mark+`</text>`)
	}
	if b.ProductName != "" {
		raw = repl(raw, `<div class="oc-logo-title">HIVE</div>`,
			`<div class="oc-logo-title">`+b.ProductName+`</div>`)
	}
	if b.Tagline != "" {
		raw = repl(raw, `<div class="oc-logo-sub">GATEWAY DASHBOARD</div>`,
			`<div class="oc-logo-sub">`+b.Tagline+`</div>`)
	}
	if b.Title != "" {
		raw = repl(raw, `<title>Hive Dashboard</title>`, `<title>`+b.Title+`</title>`)
	}
	return raw
}
