package hub

import (
	"io/fs"
	"net/http"
	"slices"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
	"github.com/hivecommons/hive/pkg/webassets/trackerrow"
)

// Fleet activity page (#16063, part of #15965 / RFC #15861 by @hanthor).
//
// GET /activity serves a data-free shell to signed-in users; the board is
// filled in the browser by polling GET /api/hub/activity (activity.go),
// which scopes every hive to the viewer server-side. The page is read-only:
// rows link to GitHub and the hive chip to the hive's own tracker, with no
// action menus. Rows are drawn with the shared tracker row asset
// (pkg/webassets/trackerrow, #16062), served from activityAssetPrefix.

const (
	activityPagePath    = "/activity"
	activityAssetPrefix = "/activity/assets/"
	activityPageFile    = "static/activity.html"

	// activityPageCSP follows the script-src split of
	// pkg/dashboard/webstatic/csp_script_src.go: scripts are files under
	// 'self' (the document has no inline <script>, so script-src-elem is
	// recomputed from the served bytes and stays 'self'), inline event
	// handlers and style attributes are refused, and nothing else loads.
	activityPageCSP = "default-src 'none'; " +
		"script-src 'self'; script-src-elem 'self'; script-src-attr 'none'; " +
		"style-src 'self'; style-src-elem 'self'; style-src-attr 'none'; " +
		"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

// handleActivityPage serves the Fleet activity shell. A browser without a
// valid session (no cookie, or an expired or forged one) is sent to /login
// like /dashboard; everyone else goes through requireAuth, so a blocked
// account is refused before the shell is written.
func (s *HubServer) handleActivityPage(w http.ResponseWriter, r *http.Request) {
	if s.getAuthUser(r) == "" {
		http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
		return
	}
	s.requireAuth(s.serveActivityShell)(w, r)
}

func (s *HubServer) serveActivityShell(w http.ResponseWriter, r *http.Request) {
	page, err := staticFS.ReadFile(activityPageFile)
	if err != nil {
		s.logger.Error("hub activity page: read embedded shell", "error", err)
		s.writeHubErrorPage(w, http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", activityPageCSP)
	webstatic.ApplyDocumentScriptSrcElem(w, page)
	if webstatic.WriteCacheHeaders(w, r, webstatic.ETagFor(page)) {
		return
	}
	_, _ = w.Write(page)
}

// handleActivityAsset serves one file of the shared tracker row asset,
// byte-identical to pkg/webassets/trackerrow, with the static no-cache +
// content ETag headers. Only names listed in trackerrow.Files are served.
// The files are pure presentation code the spoke dashboard also ships, so
// they need no session.
func (s *HubServer) handleActivityAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !slices.Contains(trackerrow.Files, name) {
		s.writeHubErrorPage(w, http.StatusNotFound)
		return
	}
	body, err := fs.ReadFile(trackerrow.FS, name)
	if err != nil {
		s.writeHubErrorPage(w, http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	if webstatic.WriteCacheHeaders(w, r, webstatic.ETagFor(body)) {
		return
	}
	_, _ = w.Write(body)
}

// registerActivityPageRoutes mounts the page and its row asset.
func (s *HubServer) registerActivityPageRoutes() {
	s.mux.HandleFunc("GET "+activityPagePath, s.handleActivityPage)
	s.mux.HandleFunc("GET "+activityAssetPrefix+"{name}", s.handleActivityAsset)
}
