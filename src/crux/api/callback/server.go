package callback

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

const (
	// maxBodyBytes bounds a request body. Scan bodies are the largest; the
	// proxy already caps the GitHub write bodies it scans well under this.
	maxBodyBytes = 4 << 20
	// shutdownGrace is how long Serve waits for in-flight requests.
	shutdownGrace = 5 * time.Second
	// networkUnix tags requests that arrived on the unix socket.
	networkUnix  = "unix"
	bearerPrefix = "Bearer "
	// installationTokenTTL is the lifetime of a GitHub App installation
	// token; a minter that does not know its token's expiry reports this.
	installationTokenTTL = time.Hour
)

type ctxKey int

const networkKey ctxKey = iota

// Server mounts a Capabilities implementation on the callback routes. Hive
// builds one over pkg/cruxhost; tests build one over a fake. Zero values are
// usable after Caps is set.
type Server struct {
	// Caps answers the routes. A capability it reports as absent through
	// Configured (optional) answers 404 CodeNotConfigured.
	Caps Capabilities
	// Token, when set, is required on every request as a bearer token. When
	// empty only requests that arrived on the unix socket Serve opened are
	// answered; anything mounted elsewhere is refused.
	Token string
	// Logger logs method, path and status only; never a body or a token.
	// Nil discards.
	Logger *slog.Logger
}

// Configured is optionally implemented by a Capabilities to say which of
// its capabilities exist; the server turns an absent one into 404
// CodeNotConfigured before dispatching and lists the present ones on
// /v1/health. A Capabilities without it is taken to have all of them.
type Configured interface {
	Configured(capability string) bool
}

// NewServer is Server{Caps: caps, Token: token}.
func NewServer(caps Capabilities, token string) *Server {
	return &Server{Caps: caps, Token: token}
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// Handler is the routed, authenticated http.Handler. Mount it under Serve
// (which tags unix-socket requests) or, with a Token, anywhere.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathHealth, s.handleHealth)
	mux.HandleFunc("POST "+PathTokens, s.capability(CapabilityTokens, s.handleMintToken))
	mux.HandleFunc("POST "+PathGitHubREST, s.capability(CapabilityGitHubREST, s.handleRecordREST))
	mux.HandleFunc("GET "+PathGitHubRateLimit, s.capability(CapabilityGitHubREST, s.handleRateLimit))
	mux.HandleFunc("POST "+PathUsage, s.capability(CapabilityUsage, s.handleUsage))
	mux.HandleFunc("POST "+PathScanCanary, s.capability(CapabilityCanary, s.handleScanCanary))
	mux.HandleFunc("POST "+PathScanText, s.capability(CapabilityText, s.handleScanText))
	mux.HandleFunc("POST "+PathConditions, s.capability(CapabilityConditions, s.handleCondition))
	mux.HandleFunc("GET "+PathLinearCredential, s.capability(CapabilityLinear, s.handleLinear))
	mux.HandleFunc("POST "+PathPulls, s.capability(CapabilityPulls, s.handleCreatePR))
	return s.authenticate(mux)
}

// authenticate is the fail-closed gate, the same rule as crux/serve: no
// token → only tagged unix-socket requests; a token → every request must
// present it.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		network, _ := r.Context().Value(networkKey).(string)
		if s.Token == "" {
			if network != networkUnix {
				s.writeError(w, r, http.StatusUnauthorized, CodeUnauthorized, "this endpoint has no bearer token; only unix-socket requests are accepted")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), bearerPrefix)
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="crux-hive"`)
			s.writeError(w, r, http.StatusUnauthorized, CodeUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) configured(capability string) bool {
	if s.Caps == nil {
		return false
	}
	if c, ok := s.Caps.(Configured); ok {
		return c.Configured(capability)
	}
	return true
}

func (s *Server) capability(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.configured(name) {
			s.writeError(w, r, http.StatusNotFound, CodeNotConfigured, fmt.Sprintf("this hive has no %s capability", name))
			return
		}
		h(w, r)
	}
}

// allCapabilities is the Health order.
var allCapabilities = []string{
	CapabilityTokens, CapabilityGitHubREST, CapabilityUsage, CapabilityCanary,
	CapabilityText, CapabilityConditions, CapabilityLinear, CapabilityPulls,
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := Health{Version: Version, Capabilities: []string{}}
	for _, c := range allCapabilities {
		if s.configured(c) {
			h.Capabilities = append(h.Capabilities, c)
		}
	}
	s.writeJSON(w, r, http.StatusOK, h)
}

func (s *Server) handleMintToken(w http.ResponseWriter, r *http.Request) {
	var req MintTokenRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.TTLSeconds < 0 {
		s.writeError(w, r, http.StatusBadRequest, CodeBadRequest, "ttl_seconds must not be negative")
		return
	}
	resp, err := s.Caps.MintToken(r.Context(), req)
	if err != nil {
		s.writeCapabilityError(w, r, "minting token", err)
		return
	}
	if resp.Token == "" {
		s.writeError(w, r, http.StatusBadGateway, CodeUpstream, "minting token: empty token")
		return
	}
	if resp.ExpiresAt.IsZero() {
		resp.ExpiresAt = time.Now().Add(installationTokenTTL)
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

func (s *Server) handleRecordREST(w http.ResponseWriter, r *http.Request) {
	var rec RESTRecord
	if !s.readJSON(w, r, &rec) {
		return
	}
	if rec.Method == "" || rec.Path == "" {
		s.writeError(w, r, http.StatusBadRequest, CodeBadRequest, "method and path are required")
		return
	}
	if err := s.Caps.RecordREST(r.Context(), rec); err != nil {
		s.writeCapabilityError(w, r, "recording REST request", err)
		return
	}
	s.writeNoContent(w, r)
}

func (s *Server) handleRateLimit(w http.ResponseWriter, r *http.Request) {
	snap, err := s.Caps.RateLimit(r.Context())
	if err != nil {
		s.writeCapabilityError(w, r, "reading rate limit", err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, snap)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	var rec UsageRecord
	if !s.readJSON(w, r, &rec) {
		return
	}
	switch rec.Kind {
	case UsageKindGateway, UsageKindCopilot, UsageKindCopilotCache:
	default:
		s.writeError(w, r, http.StatusBadRequest, CodeBadRequest, "kind must be gateway, copilot or copilot_cache")
		return
	}
	if err := s.Caps.RecordUsage(r.Context(), rec); err != nil {
		s.writeCapabilityError(w, r, "recording usage", err)
		return
	}
	s.writeNoContent(w, r)
}

func (s *Server) handleScanCanary(w http.ResponseWriter, r *http.Request) {
	var req CanaryScanRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	resp, err := s.Caps.ScanCanary(r.Context(), req)
	if err != nil {
		s.writeCapabilityError(w, r, "scanning for canaries", err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

func (s *Server) handleScanText(w http.ResponseWriter, r *http.Request) {
	var req TextScanRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.Direction != ScanInput && req.Direction != ScanOutput {
		s.writeError(w, r, http.StatusBadRequest, CodeBadRequest, "direction must be input or output")
		return
	}
	resp, err := s.Caps.ScanText(r.Context(), req)
	if err != nil {
		s.writeCapabilityError(w, r, "scanning text", err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

func (s *Server) handleCondition(w http.ResponseWriter, r *http.Request) {
	var rep ConditionReport
	if !s.readJSON(w, r, &rep) {
		return
	}
	if rep.Action != ConditionObserve && rep.Action != ConditionClear {
		s.writeError(w, r, http.StatusBadRequest, CodeBadRequest, "action must be observe or clear")
		return
	}
	if rep.Class == "" {
		s.writeError(w, r, http.StatusBadRequest, CodeBadRequest, "class is required")
		return
	}
	if err := s.Caps.ReportCondition(r.Context(), rep); err != nil {
		s.writeCapabilityError(w, r, "reporting condition", err)
		return
	}
	s.writeNoContent(w, r)
}

func (s *Server) handleLinear(w http.ResponseWriter, r *http.Request) {
	cred, err := s.Caps.LinearCredential(r.Context())
	if err != nil {
		s.writeCapabilityError(w, r, "resolving Linear credential", err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, cred)
}

func (s *Server) handleCreatePR(w http.ResponseWriter, r *http.Request) {
	var req CreatePRRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.Repo == "" || req.Head == "" || req.Base == "" || req.Title == "" {
		s.writeError(w, r, http.StatusBadRequest, CodeBadRequest, "repo, head, base and title are required")
		return
	}
	resp, err := s.Caps.CreatePR(r.Context(), req)
	if err != nil {
		s.writeCapabilityError(w, r, "creating pull request", err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// writeCapabilityError maps a Capabilities error: ErrNotConfigured → 404,
// anything else → 502 CodeUpstream with the message. The message is the
// implementation's; hive-side code is responsible for keeping secrets out
// of the errors it returns, as it already is for its logs.
func (s *Server) writeCapabilityError(w http.ResponseWriter, r *http.Request, verb string, err error) {
	if errors.Is(err, ErrNotConfigured) {
		s.writeError(w, r, http.StatusNotFound, CodeNotConfigured, verb+": "+err.Error())
		return
	}
	s.writeError(w, r, http.StatusBadGateway, CodeUpstream, verb+": "+err.Error())
}

func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	if err := dec.Decode(v); err != nil {
		s.writeError(w, r, http.StatusBadRequest, CodeBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
	s.logger().Debug("hive callback", "method", r.Method, "path", r.URL.Path, "status", status)
}

func (s *Server) writeNoContent(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
	s.logger().Debug("hive callback", "method", r.Method, "path", r.URL.Path, "status", http.StatusNoContent)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Error{Error: msg, Code: code})
	s.logger().Warn("hive callback refused", "method", r.Method, "path", r.URL.Path, "status", status, "code", code)
}

// ListenConfig is what Serve binds. Zero values mean "unix socket at
// DefaultSocket, no TCP".
type ListenConfig struct {
	// Socket is the unix socket path; empty selects DefaultSocket().
	Socket string
	// Listen is an optional TCP host:port; it requires Token.
	Listen string
	// Token is the bearer token Serve will require. It is here only so
	// Validate can refuse TCP without one; the Server carries it too.
	Token string
}

// Validate fills defaults and refuses TCP without a token.
func (c *ListenConfig) Validate() error {
	if c.Socket == "" {
		c.Socket = DefaultSocket()
	}
	if c.Listen != "" && c.Token == "" {
		return fmt.Errorf("a TCP callback listener on %s needs a bearer token (%s); refusing to serve hive capabilities unauthenticated", c.Listen, TokenEnvVar)
	}
	return nil
}

// Listeners is what Listen opened.
type Listeners struct {
	Unix net.Listener
	TCP  net.Listener
	path string
}

// Addrs lists the bound addresses for logging.
func (l *Listeners) Addrs() []string {
	var out []string
	if l.Unix != nil {
		out = append(out, "unix://"+l.path)
	}
	if l.TCP != nil {
		out = append(out, "http://"+l.TCP.Addr().String())
	}
	return out
}

// Close closes every listener and removes the socket file.
func (l *Listeners) Close() {
	if l.Unix != nil {
		_ = l.Unix.Close()
		_ = os.Remove(l.path)
	}
	if l.TCP != nil {
		_ = l.TCP.Close()
	}
}

// Listen validates cfg and binds the unix socket (directory created 0700 if
// missing and checked to be this uid's private directory, a stale socket
// replaced, the socket 0600 before it is reachable at its path) and the
// optional TCP address. The rule is crux/serve.Listen's.
func Listen(cfg ListenConfig) (*Listeners, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(cfg.Socket)
	if err := os.MkdirAll(dir, SocketDirMode); err != nil {
		return nil, fmt.Errorf("socket directory: %w", err)
	}
	if err := ownedPrivateDir(dir); err != nil {
		return nil, fmt.Errorf("socket directory %s: %w", dir, err)
	}
	if conn, err := net.DialTimeout("unix", cfg.Socket, time.Second); err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socket %s: another callback endpoint is already listening", cfg.Socket)
	}
	tmp := fmt.Sprintf("%s.%d.tmp", cfg.Socket, os.Getpid())
	_ = os.Remove(tmp)
	ul, err := net.Listen("unix", tmp)
	if err != nil {
		return nil, fmt.Errorf("socket %s: %w", cfg.Socket, err)
	}
	if err := os.Chmod(tmp, SocketMode); err != nil {
		_ = ul.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("socket %s: chmod: %w", cfg.Socket, err)
	}
	if err := os.Rename(tmp, cfg.Socket); err != nil {
		_ = ul.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("socket %s: %w", cfg.Socket, err)
	}
	ls := &Listeners{Unix: ul, path: cfg.Socket}
	if cfg.Listen != "" {
		tl, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			ls.Close()
			return nil, fmt.Errorf("listen %s: %w", cfg.Listen, err)
		}
		ls.TCP = tl
	}
	return ls, nil
}

// ownedPrivateDir refuses a socket directory this user does not own
// outright: a real directory (not a symlink), owned by this uid, with no
// group or other permission bits.
func ownedPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if !fi.IsDir() {
		return errors.New("is not a directory")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("mode %04o is readable by others; want %04o", perm, SocketDirMode)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("owned by uid %d, not this user (uid %d)", st.Uid, os.Getuid())
	}
	return nil
}

// Serve runs the endpoint on ls until ctx ends, then shuts down and closes
// ls. Requests on the unix listener are tagged so a token-less server
// accepts them.
func (s *Server) Serve(ctx context.Context, ls *Listeners) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	handler := s.Handler()
	errc := make(chan error, 2)
	var servers []*http.Server
	serveOn := func(network string, l net.Listener) {
		srv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			BaseContext: func(net.Listener) context.Context {
				return context.WithValue(ctx, networkKey, network)
			},
		}
		servers = append(servers, srv)
		go func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", network, err)
				return
			}
			errc <- nil
		}()
	}
	if ls.Unix != nil {
		serveOn(networkUnix, ls.Unix)
	}
	if ls.TCP != nil {
		serveOn("tcp", ls.TCP)
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
		cancel()
	}
	for _, srv := range servers {
		sctx, scancel := context.WithTimeout(context.Background(), shutdownGrace)
		_ = srv.Shutdown(sctx)
		scancel()
	}
	ls.Close()
	return err
}

// WithUnixNetwork tags ctx as a unix-socket request context, for a caller
// that mounts Handler on its own listener (an httptest server over a unix
// socket, or a hive that already owns an http.Server) and wants the
// token-less rule to admit it. Use it as the http.Server's BaseContext.
func WithUnixNetwork(ctx context.Context) context.Context {
	return context.WithValue(ctx, networkKey, networkUnix)
}
