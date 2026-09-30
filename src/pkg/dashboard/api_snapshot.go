package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
)

func (s *Server) handleSnapshotAPI(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil || !s.deps.Config.Hub.AutoSnapshot {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"snapshots not enabled"}`))
		return
	}
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		http.Error(w, "no data yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	jsonResponse(w, status)
}

func (s *Server) handleSnapshotFrameAncestors(w http.ResponseWriter, r *http.Request) {
	var origins []string
	if s.deps != nil && s.deps.Config != nil {
		origins = s.deps.Config.Dashboard.SnapshotFrameAncestors
	}
	jsonResponse(w, map[string]any{"origins": origins})
}

func (s *Server) handleSnapshotPage(w http.ResponseWriter, r *http.Request) {
	hubURL := "https://hive.hivecommons.dev"
	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Hub.URL != "" {
		hubURL = s.deps.Config.Hub.URL
	}

	cfg := s.deps.Config
	if s.deps == nil || cfg == nil || !cfg.Hub.AutoSnapshot {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="3;url=%s"><title>Hive</title>
<style>body{font-family:system-ui,sans-serif;background:#0a0a0a;color:#e0e0e0;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0}
.card{text-align:center;max-width:480px;padding:40px}.bee{font-size:3rem;margin-bottom:16px}h1{color:#f59e0b;margin:0 0 8px}p{color:#8b949e;line-height:1.6}a{color:#58a6ff}</style>
</head><body><div class="card"><div class="bee">🐝</div><h1>Hive</h1><p>AI Agent Orchestration for Open Source</p><p>Snapshot is not currently published for this hive.</p><p>Redirecting to <a href="%s">%s</a>...</p></div></body></html>`,
			hubURL, hubURL, hubURL)
		return
	}

	mode := r.URL.Query().Get("mode")
	if mode == "classic" {
		mode = "dark"
	}
	if mode != "dark" {
		mode = "light"
	}
	snapDir := s.snapshotDirOrDefault()
	snapshotFile := filepath.Join(snapDir, fmt.Sprintf("snapshot-%s.html", mode))
	info, err := os.Stat(snapshotFile)
	intervalMin := cfg.Hub.SnapshotIntervalMin
	if intervalMin < 5 {
		intervalMin = 15
	}
	staleThreshold := time.Duration(intervalMin) * time.Minute
	needsRebuild := err != nil || time.Since(info.ModTime()) > staleThreshold

	if needsRebuild {
		s.buildSnapshot(filepath.Join(snapDir, "snapshot-dark.html"), "dark")
		s.buildSnapshot(filepath.Join(snapDir, "snapshot-light.html"), "light")
	}

	data, err := os.ReadFile(snapshotFile)
	if err != nil {
		http.Error(w, "snapshot not yet generated — try again in a moment", http.StatusServiceUnavailable)
		return
	}

	data = []byte(strings.ReplaceAll(string(data),
		`href="/live/hive/light"`,
		`href="/snapshot?mode=light"`))
	data = []byte(strings.ReplaceAll(string(data),
		`href="/live/hive/dark"`,
		`href="/snapshot?mode=dark"`))
	data = []byte(strings.ReplaceAll(string(data),
		`href="/live/hive"`,
		`href="/snapshot"`))

	html := string(data)
	dashURL := ""
	if s.deps != nil && s.deps.Config != nil {
		dashURL = s.deps.Config.Hub.DashboardURL
	}
	if dashURL != "" {
		html = strings.ReplaceAll(html, `href="`+dashURL, `href="/snapshot`)
		html = strings.ReplaceAll(html, `action="`+dashURL, `action="/snapshot`)
		html = strings.ReplaceAll(html, dashURL, "/snapshot")
	}
	html = regexp.MustCompile(`href="https?://[^"]*\.hive\.kubestellar\.io[^"]*"`).ReplaceAllString(html, `href="/snapshot"`)
	html = regexp.MustCompile(`href="http://localhost:\d+[^"]*"`).ReplaceAllString(html, `href="/snapshot"`)
	html = regexp.MustCompile(`href="http://192\.168\.[^"]*"`).ReplaceAllString(html, `href="/snapshot"`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	// The snapshot document is built at runtime by build-snapshot.mjs and then
	// rewritten above, so its inline <script> content is only known here. Stamp
	// the CSP script-src-elem hash allowlist from the exact bytes being served
	// (#3848 part 1 / #3907, see pkg/dashboard/webstatic).
	webstatic.ApplyDocumentScriptSrcElem(w, []byte(html))
	_, _ = w.Write([]byte(html))
}

// snapshotDirOrDefault returns the directory handleSnapshotPage/buildSnapshot
// read and write snapshot-{mode}.html under: s.snapshotDir when a test has
// overridden it, otherwise the production default. See the field comment on
// Server.snapshotDir (#5235).
func (s *Server) snapshotDirOrDefault() string {
	if s.snapshotDir != "" {
		return s.snapshotDir
	}
	return "/data/snapshots"
}

func (s *Server) buildSnapshot(outputFile, mode string) {
	if s.buildSnapshotFn != nil {
		s.buildSnapshotFn(s, outputFile, mode)
		return
	}
	buildSnapshotProd(s, outputFile, mode)
}

// buildSnapshotProd is the real Node-builder invocation buildSnapshot runs in
// production. Split out from buildSnapshot so tests can override the whole
// invocation via Server.buildSnapshotFn (#5235) without ever spawning `node`
// — see the field comment on Server.buildSnapshotFn for the seam convention
// this follows (pkg/hub's afterGenerationsReadAttempt, #5080).
func buildSnapshotProd(s *Server, outputFile, mode string) {
	snapDir := s.snapshotDirOrDefault()
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		s.logger.Warn("snapshot directory creation failed", "error", err)
		return
	}
	dashURL := fmt.Sprintf("http://localhost:%d", s.port)
	htmlSource := "/opt/hive/proxy/public/index.html"
	builderScript := "/opt/hive/dashboard/build-snapshot.mjs"
	args := []string{
		builderScript,
		"--mode", mode,
		"--base-path", "/snapshot",
		"--html", htmlSource,
		dashURL, outputFile,
	}
	// The builder fetches /api/status (and siblings) from localhost. Those
	// endpoints require auth, so without a token the builder gets 401 and
	// bakes an empty snapshot (blank Governor/Tokens/Cost/Repos/Beads/Agents
	// panels, ACMM "--"). Pass s.authToken as DASHBOARD_AUTH_TOKEN so the
	// builder authenticates via the trusted X-Hive-Internal header path. The
	// token is used ONLY as a request header for the localhost fetch; the
	// builder never writes it into the snapshot HTML output.
	out, err := runSnapshotBuilder(args, snapshotBuilderEnv(os.Environ(), s.authToken))
	if err != nil {
		s.logger.Warn("snapshot build failed", "error", err, "output", string(out))
	} else {
		s.logger.Info("snapshot built", "file", outputFile)
	}
}

var runSnapshotBuilder = func(args []string, env []string) ([]byte, error) {
	cmd := exec.Command("node", args...)
	cmd.Env = env
	return cmd.CombinedOutput()
}

// snapshotBuilderEnv returns the environment for the Node snapshot builder.
// NODE_TLS_REJECT_UNAUTHORIZED=0 is always set for the localhost fetch. When
// authToken is non-empty it is exposed as DASHBOARD_AUTH_TOKEN so the builder
// can send the trusted X-Hive-Internal header and receive live data instead of
// a 401. Open/no-auth spokes (empty token) get the prior behavior unchanged.
func snapshotBuilderEnv(baseEnv []string, authToken string) []string {
	env := append(baseEnv, "NODE_TLS_REJECT_UNAUTHORIZED=0")
	if authToken != "" {
		env = append(env, "DASHBOARD_AUTH_TOKEN="+authToken)
	}
	return env
}
