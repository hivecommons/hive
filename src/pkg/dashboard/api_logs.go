package dashboard

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/logread"
)

// registerLogReadRoutes serves this hive's logs to operators with no shell on
// the host (hivecommons/hive#11942). The admin MCP wraps both routes as the
// log_read and log_sources tools. Neither route can rotate, truncate, delete
// or edit a log, or change a log setting.
func (s *Server) registerLogReadRoutes() {
	s.mux.HandleFunc("GET /api/logs/sources", s.handleLogSources)
	s.mux.HandleFunc("GET /api/logs/read", s.handleLogRead)
}

// handleLogSources lists the fixed log source list, each marked available or
// not with a reason. It names sources only, so the read role may call it.
func (s *Server) handleLogSources(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, logread.Sources(s.logReadOptions(nil)))
}

// handleLogRead serves GET /api/logs/read?source=&lines=&since=&until=
// &min_level=&include=&exclude=&cursor=. Log text can carry what a viewer
// with the read role must not see, so the handler itself requires read-write
// or owner whatever middleware admitted the request.
func (s *Server) handleLogRead(w http.ResponseWriter, r *http.Request) {
	if !config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleReadWrite) {
		jsonError(w, "reading the hive's logs requires the read-write or owner role", http.StatusForbidden)
		return
	}
	req, msg := parseLogReadRequest(r)
	if msg != "" {
		jsonError(w, msg, http.StatusBadRequest)
		return
	}
	secrets := logread.NewSecretScrubber(logread.CollectSecretValues(s.logSecretSources()))
	jsonResponse(w, logread.Read(req, s.logReadOptions(secrets)))
}

func parseLogReadRequest(r *http.Request) (logread.Request, string) {
	q := r.URL.Query()
	req := logread.Request{
		Source:  strings.TrimSpace(q.Get("source")),
		Include: strings.TrimSpace(q.Get("include")),
		Cursor:  strings.TrimSpace(q.Get("cursor")),
	}
	if raw := strings.TrimSpace(q.Get("lines")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return req, "lines must be a whole number"
		}
		req.Lines = n
	}
	var err error
	if req.Since, err = logread.ParseInstant("since", q.Get("since")); err != nil {
		return req, err.Error()
	}
	if req.Until, err = logread.ParseInstant("until", q.Get("until")); err != nil {
		return req, err.Error()
	}
	if !req.Since.IsZero() && !req.Until.IsZero() && req.Until.Before(req.Since) {
		return req, "until must not be before since"
	}
	if req.MinLevel, err = logread.ParseMinLevel(q.Get("min_level")); err != nil {
		return req, err.Error()
	}
	for _, ex := range q["exclude"] {
		if ex = strings.TrimSpace(ex); ex != "" {
			req.Exclude = append(req.Exclude, ex)
		}
	}
	return req, ""
}

func (s *Server) logReadOptions(secrets *logread.SecretScrubber) logread.Options {
	opts := logread.Options{Secrets: secrets}
	if s.deps != nil && s.deps.Config != nil {
		logging := s.deps.Config.Governor.Logging
		opts.LogDir = logging.Dir
		if logging.ReadScanMaxMB > 0 {
			opts.WorkBoundBytes = int64(logging.ReadScanMaxMB) << 20
		}
	}
	return opts
}

// logSecretSources lists this hive's own secret values for read-time
// redaction: the dashboard token, the terminal login in its user:token form,
// the terminal key, secret files, and secret-named environment values.
func (s *Server) logSecretSources() logread.SecretSources {
	values := []string{s.authToken, os.Getenv("HIVE_TTYD_CREDENTIAL"), spoke.TerminalSigningKey()}
	if s.authToken != "" {
		values = append(values, "hive:"+s.authToken)
	}
	if s.deps != nil && s.deps.Config != nil {
		values = append(values, s.deps.Config.Dashboard.AuthToken, s.deps.Config.GitHub.Token)
		if tok := s.deps.Config.Dashboard.AuthToken; tok != "" {
			values = append(values, "hive:"+tok)
		}
	}
	return logread.SecretSources{Values: values, Environ: os.Environ(), SecretDirs: config.SecretFileRoots()}
}
