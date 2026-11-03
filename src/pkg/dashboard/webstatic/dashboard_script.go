package webstatic

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
)

const (
	dashboardMainScriptMarker = "<script>\n    // Wrap fetch to inject auth token for mutation requests (PUT/POST/PATCH/DELETE)"
	dashboardScriptCache      = "private, max-age=31536000, immutable"
)

// This trusted inline listener is hashed with the served shell by the CSP
// layer. It must precede the external script and work without application code,
// including when a request reaches a replica with a different asset hash.
const dashboardScriptBootstrap = `<script>
(function () {
  var scriptID = 'hive-dashboard-main-script';
  function isMain(event) { return event.target && event.target.id === scriptID; }
  function stopListening() {
    window.removeEventListener('error', onError, true);
    window.removeEventListener('load', onLoad, true);
  }
  function onLoad(event) { if (isMain(event)) stopListening(); }
  function onError(event) {
    var script = document.getElementById(scriptID);
    if (!isMain(event) && !(script && event.filename === script.src)) return;
    stopListening();
    var root = document.getElementById('hive-dashboard-root');
    if (root) { root.hidden = true; root.setAttribute('inert', ''); }
    var dialog = document.createElement('dialog');
    dialog.id = 'hive-dashboard-load-error';
    dialog.setAttribute('role', 'alertdialog');
    dialog.setAttribute('aria-labelledby', 'hive-dashboard-load-error-title');
    dialog.setAttribute('aria-describedby', 'hive-dashboard-load-error-detail');
    var title = document.createElement('h2');
    title.id = 'hive-dashboard-load-error-title';
    title.textContent = 'Dashboard could not finish loading';
    var detail = document.createElement('p');
    detail.id = 'hive-dashboard-load-error-detail';
    detail.textContent = 'The application script could not be loaded. A dashboard update or connection problem may have interrupted loading. Reload to try again. If this keeps happening, contact your hive operator.';
    var reload = document.createElement('button');
    reload.type = 'button';
    reload.textContent = 'Reload dashboard';
    reload.addEventListener('click', function () { window.location.reload(); });
    dialog.addEventListener('cancel', function (event) { event.preventDefault(); });
    dialog.appendChild(title);
    dialog.appendChild(detail);
    dialog.appendChild(reload);
    document.body.appendChild(dialog);
    if (typeof dialog.showModal === 'function') dialog.showModal();
    else dialog.setAttribute('open', '');
    reload.focus();
  }
  window.addEventListener('error', onError, true);
  window.addEventListener('load', onLoad, true);
})();
</script>`

// DashboardScript is the exact main classic script from the final branded
// document. Its full SHA-256 URL changes with its bytes, including branding.
// Register it behind the same authentication middleware as the index document.
type DashboardScript struct {
	path      string
	integrity string
	document  *IndexDocument
	gzipOnce  sync.Once
	gzipETag  string
}

// ExternalizeDashboardScript replaces only the known main script with a
// same-origin deferred classic script. Keeping one script (not a module or
// wrapper) preserves its global lexical declarations and function bindings.
// The canonical generated index remains untouched for source-string consumers.
// Unexpected markup retains the original, working inline document and its CSP.
func ExternalizeDashboardScript(raw []byte) ([]byte, *DashboardScript) {
	marker := []byte(dashboardMainScriptMarker)
	if bytes.Count(raw, marker) != 1 {
		return raw, nil
	}
	start := bytes.Index(raw, marker)
	bodyStart := start + len("<script>")
	closeOffset := bytes.Index(raw[bodyStart:], []byte("</script>"))
	if closeOffset < 0 {
		return raw, nil
	}
	bodyEnd := bodyStart + closeOffset
	end := bodyEnd + len("</script>")
	// Deferring a script ahead of a later inline script would reverse their
	// execution order. Only split the final script element in the document.
	if bytes.Contains(bytes.ToLower(raw[end:]), []byte("<script")) {
		return raw, nil
	}
	body := raw[bodyStart:bodyEnd]
	sum := sha256.Sum256(body)
	asset := &DashboardScript{
		path:      "/assets/dashboard-main." + hex.EncodeToString(sum[:]) + ".js",
		integrity: "sha256-" + base64.StdEncoding.EncodeToString(sum[:]),
		document:  NewIndexDocument(body),
	}
	tag := []byte(dashboardScriptBootstrap + asset.scriptTag())
	shell := make([]byte, 0, len(raw)-(end-start)+len(tag))
	shell = append(shell, raw[:start]...)
	shell = append(shell, tag...)
	shell = append(shell, raw[end:]...)
	return shell, asset
}

// Path is the only URL this handler serves; it never opens filesystem paths.
func (s *DashboardScript) Path() string { return s.path }

func (s *DashboardScript) scriptTag() string {
	return `<script id="hive-dashboard-main-script" defer src="` + s.Path() + `" integrity="` + s.integrity + `"></script>`
}

// Precompress prepares both the compressed bytes and their strong validator.
func (s *DashboardScript) Precompress() {
	s.gzipOnce.Do(func() {
		if body := s.document.compressedBody(); body != nil {
			s.gzipETag = ETagFor(body)
		}
	})
}

func (s *DashboardScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path != s.path {
		h.Set("Cache-Control", "no-cache")
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Cache-Control", "no-cache")
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", dashboardScriptCache)
	h.Add("Vary", "Accept-Encoding")
	body, etag := s.document.raw, s.document.etag
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		s.Precompress()
		if s.document.gzipped != nil {
			body, etag = s.document.gzipped, s.gzipETag
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("ETag", etag)
	if ifNoneMatchHits(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
