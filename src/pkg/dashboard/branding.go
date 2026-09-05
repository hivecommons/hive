package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	// brandingMaxBytes bounds an operator override file. Deliberately the same
	// 128 KiB the unrelated ?style= custom-stylesheets feature already caps its
	// fetched bodies at (customStyleMaxBytes) — a branding override is the same
	// kind of artefact served into the same document, so it inherits the same
	// ceiling rather than inventing a second number.
	brandingMaxBytes = customStyleMaxBytes

	// brandingUnsafeModeBits are the permission bits that make a branding file
	// writable by somebody other than its owner. The branding path is a trust
	// boundary: whoever can write it injects CSS into the operator's dashboard,
	// and the dashboard CSP allows `img-src https:`, so that CSS can beacon.
	// Group/world write means "more than the owner can do that" — refuse.
	brandingUnsafeModeBits fs.FileMode = 0o022

	// brandingAllowUnsafeEnv is the escape hatch for deployments whose branding
	// files legitimately cannot satisfy the ownership rule and whose operator
	// has decided the path is trustworthy anyway. It does NOT relax the
	// group/world-writable rule — that one has no legitimate deployment shape.
	brandingAllowUnsafeEnv = "HIVE_BRANDING_ALLOW_UNSAFE_OWNER"
)

// brandingWarnOnce keeps a refusal from filling the log: custom.css is read on
// EVERY request for /branding/custom.css, so an unguarded Warn would emit once
// per page load for as long as the file stays bad. Keyed by path+reason so a
// second, different problem still gets reported.
var brandingWarnOnce sync.Map

func (s *Server) brandingWarn(key string, msg string, args ...any) {
	if s.logger == nil {
		return
	}
	if _, loaded := brandingWarnOnce.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	s.logger.Warn(msg, args...)
}

// brandingAllowUnsafeOwner reports whether the operator has explicitly accepted
// a branding file owned by another uid.
func brandingAllowUnsafeOwner() bool {
	v, err := strconv.ParseBool(os.Getenv(brandingAllowUnsafeEnv))
	return err == nil && v
}

// readBrandingFile reads an operator branding file under the guards the path
// warrants, returning (nil, nil) when the file simply is not there — the normal
// case, which must stay cheap and quiet.
//
// Guards, in the order a bad file trips them:
//
//   - SYMLINK ESCAPE. The link is resolved and must land inside the directory
//     the configured path names, so a symlink dropped on the data volume cannot
//     turn the branding path into a reader of /etc/shadow or of any other file
//     on the volume.
//   - OWNERSHIP. The file must be owned by the hive process uid, or by root
//     (uid 0) — the documented read-only Secret mount is root-owned 0444 and
//     must keep working, and a file the process itself cannot rewrite is not a
//     privilege escalation. Anything else is refused unless the operator sets
//     HIVE_BRANDING_ALLOW_UNSAFE_OWNER.
//   - MODE. Group- or world-writable is refused unconditionally: that means
//     somebody other than the owner can replace the served bytes, which is
//     exactly the agent-on-the-shared-data-volume threat this closes.
//   - SIZE. Bounded at brandingMaxBytes. On exceed the file is REFUSED, not
//     truncated: half a stylesheet is a differently-broken page, and a
//     truncated branding.json is invalid JSON.
func (s *Server) readBrandingFile(path, kind string) ([]byte, error) {
	// Lstat first: Stat would follow a symlink and report the target's
	// ownership, which is precisely what the escape check needs to see through.
	li, err := os.Lstat(path)
	if err != nil {
		return nil, nil // absent (or unreachable) — the normal case
	}

	if li.Mode()&fs.ModeSymlink != 0 {
		resolved, rerr := filepath.EvalSymlinks(path)
		if rerr != nil {
			s.brandingWarn(path+"|symlink", kind+" is a symlink that does not resolve; ignoring",
				"path", path, "error", rerr)
			return nil, fmt.Errorf("branding: unresolvable symlink")
		}
		dir, derr := filepath.EvalSymlinks(filepath.Dir(path))
		if derr != nil || !isWithinDir(resolved, dir) {
			s.brandingWarn(path+"|escape", kind+" is a symlink pointing outside its own directory; refusing to serve it — replace the symlink with a regular file, or point "+kind+"'s path variable directly at the target",
				"path", path, "resolves_to", resolved, "branding_dir", dir)
			return nil, fmt.Errorf("branding: symlink escapes branding directory")
		}
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil, nil
	}
	if !fi.Mode().IsRegular() {
		s.brandingWarn(path+"|irregular", kind+" is not a regular file; ignoring",
			"path", path, "mode", fi.Mode().String())
		return nil, fmt.Errorf("branding: not a regular file")
	}

	if perm := fi.Mode().Perm(); perm&brandingUnsafeModeBits != 0 {
		s.brandingWarn(path+"|mode", kind+" is group- or world-writable, so anything on that volume can inject CSS into your dashboard; refusing to serve it — run: chmod go-w "+path,
			"path", path, "mode", fmt.Sprintf("%04o", perm))
		return nil, fmt.Errorf("branding: file is group- or world-writable")
	}

	if uid := brandingFileOwnerUID(fi); uid >= 0 {
		self := os.Getuid()
		// root-owned is accepted on purpose: the documented read-only Secret
		// mount (defaultMode 0444, uid 0) is the recommended hardened shape and
		// must not be broken by the guard meant to protect it.
		if uid != self && uid != 0 && !brandingAllowUnsafeOwner() {
			s.brandingWarn(path+"|owner", kind+" is owned by another user, so hive cannot vouch for its contents; refusing to serve it — run: chown "+strconv.Itoa(self)+" "+path+" (or set "+brandingAllowUnsafeEnv+"=true if that owner is trusted)",
				"path", path, "owner_uid", uid, "hive_uid", self)
			return nil, fmt.Errorf("branding: file owned by another user")
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = f.Close() }()

	// max+1 so an exactly-at-the-limit file is served and an over-limit one is
	// detectable rather than silently truncated at the cap.
	b, err := io.ReadAll(io.LimitReader(f, brandingMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > brandingMaxBytes {
		s.brandingWarn(path+"|size", kind+" exceeds the "+strconv.Itoa(brandingMaxBytes)+"-byte limit; refusing to serve it rather than truncating — shrink the file",
			"path", path, "limit_bytes", brandingMaxBytes)
		return nil, fmt.Errorf("branding: file exceeds %d byte limit", brandingMaxBytes)
	}
	return b, nil
}

// isWithinDir reports whether path is dir itself or lies beneath it. Both are
// expected to be already symlink-resolved.
func isWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

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
	b, err := s.readBrandingFile(path, "branding custom.css")
	if err != nil || b == nil {
		// A refused file 404s exactly like an absent one: the index links to
		// this path unconditionally, and telling the browser *why* the override
		// was rejected would only leak the deployment's file layout. The reason
		// goes to the operator's log, where it is actionable.
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
	// Same guards as custom.css: branding.json lives beside it on the same
	// volume, is written by the same actor, and its strings are substituted
	// into the served document — including into an inline script's string
	// literal (see applyBranding), so it is if anything the more sensitive read.
	raw, err := s.readBrandingFile(s.brandingJSONPath(), "branding.json")
	if err != nil || raw == nil {
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
