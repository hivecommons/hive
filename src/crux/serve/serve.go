// Package serve is the server side of the Crux API (Crux Stage 3,
// hivecommons/hive#13536; src/docs/design/crux.md "Stage 3"): an
// http.Handler that exposes a crux.Runtime over the routes crux/api defines,
// and the listener setup `crux serve` uses. It is the process boundary a
// harness other than Hive drives agents through; Hive itself still holds the
// runtime in-process.
//
// Fail-closed rules, enforced here and not left to the caller: a TCP
// listener is refused without a bearer token; on a server with no token,
// Handler refuses every request except those Serve tagged as having arrived
// on the unix socket, so mounting Handler on some other http.Server over TCP
// gets 401s, not an open runtime; the unix socket is created 0600 in a 0700
// directory the server owns; a token, when set, is required on every route
// including the socket.
package serve

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

const (
	// DefaultPollInterval is how often followed logs and the state watcher
	// re-read the runtime.
	DefaultPollInterval = time.Second
	// followWindow is how many ring-buffer lines each follow poll asks for.
	followWindow = 200
	// maxRequestBody bounds a JSON request body; a definition is a few KB.
	maxRequestBody = 1 << 20
	// subscriberBuffer is how many events a slow subscriber can lag before
	// it is dropped rather than block the publisher.
	subscriberBuffer = 64
	// shutdownGrace is how long Serve waits for in-flight requests.
	shutdownGrace = 5 * time.Second
	// maxAgentNameLen matches the CLI's definition-name gate.
	maxAgentNameLen = 64
	// networkUnix is the networkKey value Serve tags unix-socket requests
	// with; anything else, including no tag, is treated as TCP.
	networkUnix = "unix"
	// bearerPrefix is the Authorization scheme.
	bearerPrefix = "Bearer "
)

// ctxKey is the context key type for per-listener facts.
type ctxKey int

// networkKey carries the listener's network ("unix" or "tcp") into each
// request's context; see Listen.
const networkKey ctxKey = iota

// Server exposes one Runtime. Zero values are usable after Runtime is set;
// construct with New for the defaults.
type Server struct {
	// Runtime is what every route acts on.
	Runtime crux.Runtime
	// Token, when non-empty, is required as a bearer token on every request.
	Token string
	// Logger receives request errors; nil discards.
	Logger *slog.Logger
	// PollInterval is the follow/state-watch cadence; zero means
	// DefaultPollInterval.
	PollInterval time.Duration
	// LaunchContext is the context agents are started under; it must outlive
	// the request that starts them, since the runtime ties the agent's
	// lifetime to it. Serve sets it; a caller using Handler directly sets it
	// or gets context.Background.
	LaunchContext context.Context
	// Background, when set, makes Serve run the crux.Background loops
	// (token refresh, credential watchdog, Copilot session refresh,
	// transcript activity) in this process; Health lists them. A sidecar
	// host's own Background methods are then no-ops.
	Background bool
	// AuditSink and ConditionReporter, when set, receive the runtime's
	// audit records and conditions in addition to the event stream, which
	// always carries them (EventAudit, EventCondition).
	AuditSink         crux.AuditSink
	ConditionReporter crux.ConditionReporter
	// HiveCallback, when set, is reported on Health: the hive callback
	// endpoint's reachability as `crux serve --hive-callback` sees it.
	HiveCallback func() api.HiveCallback

	now     func() time.Time
	started time.Time

	mu          sync.Mutex
	subscribers map[chan api.Event]struct{}
	lastStates  map[string]string
	background  []string
}

// New returns a Server for rt with the defaults filled in.
func New(rt crux.Runtime) *Server {
	s := &Server{Runtime: rt}
	s.defaults()
	return s
}

func (s *Server) defaults() {
	if s.Logger == nil {
		s.Logger = slog.New(slog.DiscardHandler)
	}
	if s.PollInterval <= 0 {
		s.PollInterval = DefaultPollInterval
	}
	if s.LaunchContext == nil {
		s.LaunchContext = context.Background()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.started.IsZero() {
		s.started = s.now()
	}
	if s.subscribers == nil {
		s.subscribers = map[chan api.Event]struct{}{}
	}
	if s.lastStates == nil {
		s.lastStates = map[string]string{}
	}
}

// Handler returns the API routes behind the auth check.
func (s *Server) Handler() http.Handler {
	s.defaults()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathHealth, s.handleHealth)
	mux.HandleFunc("GET "+api.PathAgents, s.handleList)
	mux.HandleFunc("POST "+api.PathAgents, s.handleRun)
	mux.HandleFunc("GET "+api.PathAgents+"/{name}", s.handleGet)
	mux.HandleFunc("GET "+api.PathAgents+"/{name}/logs", s.handleLogs)
	mux.HandleFunc("POST "+api.PathAgents+"/{name}/kick", s.handleKick)
	mux.HandleFunc("POST "+api.PathAgents+"/{name}/stop", s.handleStop)
	mux.HandleFunc("GET "+api.PathEvents, s.handleEvents)
	s.registerFamilies(mux)
	return s.authenticate(mux)
}

// agentNamePattern is the CLI's definition-name gate (cmd/crux
// validateDefinitionName): the name becomes a tmux socket and per-agent
// paths, so it must be a plain identifier.
var agentNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// authenticate is the fail-closed gate: when the server has no token, only
// a request Serve tagged as arriving on the unix socket is allowed, so a
// Handler mounted elsewhere (any TCP listener, tagged or not) is refused;
// when the server has a token, every request must present it.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		network, _ := r.Context().Value(networkKey).(string)
		if s.Token == "" {
			if network != networkUnix {
				s.writeError(w, http.StatusUnauthorized, api.CodeUnauthorized, "this server has no bearer token; only unix-socket requests are accepted")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), bearerPrefix)
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="crux"`)
			s.writeError(w, http.StatusUnauthorized, api.CodeUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ownedPrivateDir refuses a socket directory this user does not own
// outright: MkdirAll is silent about a directory that already existed, and
// on a shared temp dir another user could have created it first and could
// then swap the socket. The directory must be a real directory (not a
// symlink), owned by this uid, with no group or other permission bits.
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
		return fmt.Errorf("mode %04o is readable by others; want %04o", perm, config.ServeSocketDirMode)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("owned by uid %d, not this user (uid %d)", st.Uid, os.Getuid())
	}
	return nil
}

// Listeners is what Listen opened: the unix socket and, when configured, the
// TCP listener. Close releases both and removes the socket file.
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

// Listen validates cfg and binds: the unix socket (its directory created
// 0700 if missing, a stale socket file replaced, the socket chmod 0600) and
// the optional TCP address. A socket something is still listening on is an
// error so two servers never fight over one path.
func Listen(cfg config.ServeConfig) (*Listeners, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(cfg.Socket)
	if err := os.MkdirAll(dir, config.ServeSocketDirMode); err != nil {
		return nil, fmt.Errorf("socket directory: %w", err)
	}
	if err := ownedPrivateDir(dir); err != nil {
		return nil, fmt.Errorf("socket directory %s: %w", dir, err)
	}
	if conn, err := net.DialTimeout("unix", cfg.Socket, time.Second); err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socket %s: another crux serve is already listening", cfg.Socket)
	}
	// Bind under a temporary name, set the mode, then rename into place, so
	// the socket is never reachable at its advertised path with the default
	// (umask-derived) permissions.
	tmp := fmt.Sprintf("%s.%d.tmp", cfg.Socket, os.Getpid())
	_ = os.Remove(tmp)
	ul, err := net.Listen("unix", tmp)
	if err != nil {
		return nil, fmt.Errorf("socket %s: %w", cfg.Socket, err)
	}
	if err := os.Chmod(tmp, config.ServeSocketMode); err != nil {
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

// Serve runs the API on ls until ctx ends, then shuts down and closes ls.
// It starts the state watcher and installs the kick relay for the lifetime
// of the call. The Token is taken from s, not cfg; callers set both from
// the same source (cli.cmdServe does).
func (s *Server) Serve(ctx context.Context, ls *Listeners) error {
	s.defaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.LaunchContext = ctx
	s.Runtime.SetKickObserver(func(agentName, event, detail string) {
		s.publish(api.Event{Type: api.EventKick, Agent: agentName, State: event, Detail: detail})
	})
	defer s.Runtime.SetKickObserver(nil)
	defer s.installHostObservers()()
	s.startBackground(ctx)
	go s.watchStates(ctx)

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
		serveOn("unix", ls.Unix)
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

// ---- handlers ----

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	background := append([]string(nil), s.background...)
	s.mu.Unlock()
	now := s.now()
	h := api.Health{Version: api.Version, Agents: len(s.Runtime.AllStatuses()), Background: background, StartedAt: s.started, UptimeSeconds: now.Sub(s.started).Seconds()}
	if s.HiveCallback != nil {
		hc := s.HiveCallback()
		h.HiveCallback = &hc
	}
	s.writeJSON(w, http.StatusOK, h)
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	statuses := s.Runtime.AllStatuses()
	out := api.ProcessList{Processes: make([]api.Process, 0, len(statuses))}
	for _, p := range statuses {
		if p == nil {
			continue
		}
		out.Processes = append(out.Processes, toWire(p))
	}
	sort.Slice(out.Processes, func(i, j int) bool { return out.Processes[i].Name < out.Processes[j].Name })
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req api.RunRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "name is required")
		return
	}
	name := req.Name
	if len(req.Definition) > 0 {
		if len(name) > maxAgentNameLen || !agentNamePattern.MatchString(name) {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest,
				fmt.Sprintf("name must match %s and be at most %d characters", agentNamePattern, maxAgentNameLen))
			return
		}
		cfg, err := decodeDefinition(name, req.Definition)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "definition: "+err.Error())
			return
		}
		// The runtime's AddAgent is a no-op for a name it already holds, so a
		// re-post with a changed definition would silently start the old one.
		if _, err := s.Runtime.GetStatus(name); err == nil {
			s.writeError(w, http.StatusConflict, api.CodeConflict,
				fmt.Sprintf("agent %q is already defined on this server; post without a definition to start it", name))
			return
		}
		s.Runtime.AddAgent(name, cfg)
		if _, err := s.Runtime.GetStatus(name); err != nil {
			s.writeError(w, http.StatusUnprocessableEntity, api.CodeBadRequest,
				fmt.Sprintf("agent %q was not accepted by the runtime: %v", name, err))
			return
		}
	} else {
		name = s.Runtime.ResolveAgent(name)
		if _, err := s.Runtime.GetStatus(name); err != nil {
			s.writeError(w, http.StatusNotFound, api.CodeNotFound, fmt.Sprintf("agent %q is not defined on this server; send a definition to run one", req.Name))
			return
		}
	}
	if err := s.Runtime.Start(s.LaunchContext, name); err != nil {
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, fmt.Sprintf("start %s: %v", name, err))
		return
	}
	s.publish(api.Event{Type: api.EventStarted, Agent: name})
	s.writeJSON(w, http.StatusCreated, s.agentResponse(name))
}

// decodeDefinition parses a RunRequest definition and fills the per-agent
// defaults a start depends on, the way the CLI's loader does with
// Config.ApplyAgentDefaults (which crux/config does not have): ID and Role
// default to the name, Replicas to 1, and Enabled to true unless the
// document set it. Everything else is the caller's.
func decodeDefinition(name string, raw json.RawMessage) (config.AgentConfig, error) {
	var cfg config.AgentConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return cfg, err
	}
	if cfg.ID == "" {
		cfg.ID = name
	}
	if cfg.Role == "" {
		cfg.Role = name
	}
	if cfg.Replicas == 0 {
		cfg.Replicas = 1
	}
	if _, set := keys["enabled"]; !set {
		cfg.Enabled = true
	}
	return cfg, nil
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	name := s.Runtime.ResolveAgent(r.PathValue("name"))
	p, err := s.Runtime.GetStatus(name)
	if err != nil || p == nil {
		s.writeError(w, http.StatusNotFound, api.CodeNotFound, fmt.Sprintf("agent %q not found", name))
		return
	}
	s.writeJSON(w, http.StatusOK, toWire(p))
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := s.Runtime.ResolveAgent(r.PathValue("name"))
	q := r.URL.Query()
	lines := api.DefaultLogLines
	if v := q.Get(api.QueryLines); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, api.QueryLines+" must be a non-negative integer")
			return
		}
		lines = n
	}
	if isTrue(q.Get(api.QueryFollow)) {
		s.followLogs(w, r, name)
		return
	}
	source := q.Get(api.QuerySource)
	if source == "" {
		source = api.LogSourcePane
	}
	out := api.Logs{Agent: name, Source: source}
	var err error
	switch source {
	case api.LogSourcePane:
		out.Lines, err = s.Runtime.GetOutput(name, lines)
	case api.LogSourceBuffer:
		out.Lines, err = s.Runtime.GetBufferOutput(name, lines)
	case api.LogSourceFull:
		out.Header = s.Runtime.FullLogHeader(name)
		out.Content, err = s.Runtime.CaptureFullLog(name)
	default:
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, fmt.Sprintf("%s must be %s, %s or %s", api.QuerySource, api.LogSourcePane, api.LogSourceBuffer, api.LogSourceFull))
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, fmt.Sprintf("logs %s: %v", name, err))
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

// followLogs streams new ring-buffer lines as api.EventLog records until the
// client goes away or the runtime stops answering.
func (s *Server) followLogs(w http.ResponseWriter, r *http.Request, name string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, "streaming is not supported on this connection")
		return
	}
	prev, err := s.Runtime.GetBufferOutput(name, followWindow)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, fmt.Sprintf("logs %s: %v", name, err))
		return
	}
	s.startStream(w)
	flusher.Flush()
	if len(prev) > 0 {
		if api.WriteEvent(w, api.EventLog, api.LogChunk{Agent: name, Lines: prev}) != nil {
			return
		}
		flusher.Flush()
	}
	ticker := time.NewTicker(s.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			cur, err := s.Runtime.GetBufferOutput(name, followWindow)
			if err != nil {
				_ = api.WriteEvent(w, api.EventState, api.Event{Time: s.now(), Type: api.EventState, Agent: name, Detail: err.Error()})
				flusher.Flush()
				return
			}
			if fresh := newTail(prev, cur); len(fresh) > 0 {
				if api.WriteEvent(w, api.EventLog, api.LogChunk{Agent: name, Lines: fresh}) != nil {
					return
				}
				flusher.Flush()
			}
			prev = cur
		}
	}
}

func (s *Server) handleKick(w http.ResponseWriter, r *http.Request) {
	name := s.Runtime.ResolveAgent(r.PathValue("name"))
	var req api.KickRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "message is empty")
		return
	}
	if req.Async {
		started, err := s.Runtime.SendKickAsync(name, req.Message)
		if err != nil {
			s.writeRuntimeError(w, name, "kick", err)
			return
		}
		resp := s.agentResponse(name)
		resp.Started = &started
		s.writeJSON(w, http.StatusAccepted, resp)
		return
	}
	var err error
	if req.Source != "" {
		err = s.Runtime.SendKickWithSource(name, req.Message, req.Source)
	} else {
		err = s.Runtime.SendKick(name, req.Message)
	}
	if err != nil {
		s.writeRuntimeError(w, name, "kick", err)
		return
	}
	s.publish(api.Event{Type: api.EventKicked, Agent: name})
	s.writeJSON(w, http.StatusOK, s.agentResponse(name))
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	name := s.Runtime.ResolveAgent(r.PathValue("name"))
	if err := s.Runtime.Stop(name); err != nil {
		s.writeRuntimeError(w, name, "stop", err)
		return
	}
	s.publish(api.Event{Type: api.EventStopped, Agent: name})
	s.writeJSON(w, http.StatusOK, s.agentResponse(name))
}

// handleEvents streams runtime events until the client goes away.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, "streaming is not supported on this connection")
		return
	}
	ch := s.subscribe()
	defer s.unsubscribe(ch)
	s.startStream(w)
	flusher.Flush()
	keepalive := time.NewTicker(s.PollInterval * 15)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if api.WriteEvent(w, ev.Type, ev) != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---- events ----

func (s *Server) subscribe() chan api.Event {
	ch := make(chan api.Event, subscriberBuffer)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *Server) unsubscribe(ch chan api.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subscribers[ch]; ok {
		delete(s.subscribers, ch)
		close(ch)
	}
}

// publish stamps ev and fans it out. A subscriber whose buffer is full is
// dropped (its channel closed, which ends its stream) rather than allowed to
// stall the runtime's callbacks.
func (s *Server) publish(ev api.Event) {
	if ev.Time.IsZero() {
		ev.Time = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- ev:
		default:
			delete(s.subscribers, ch)
			close(ch)
		}
	}
}

// watchStates polls AllStatuses and publishes an EventState for every agent
// whose state differs from the last poll, which is how crashes, restarts
// and pauses the API did not cause reach subscribers.
func (s *Server) watchStates(ctx context.Context) {
	ticker := time.NewTicker(s.PollInterval)
	defer ticker.Stop()
	s.diffStates(false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.diffStates(true)
		}
	}
}

// diffStates records the current states and, when publishChanges is set,
// publishes the transitions since the previous call. The first call seeds
// the table silently so a fresh subscriber is not told about history.
func (s *Server) diffStates(publishChanges bool) {
	statuses := s.Runtime.AllStatuses()
	var changes []api.Event
	s.mu.Lock()
	seen := make(map[string]bool, len(statuses))
	for name, p := range statuses {
		if p == nil {
			continue
		}
		seen[name] = true
		state := string(p.State)
		if p.Paused {
			state = string(crux.StatePaused)
		}
		if prev, ok := s.lastStates[name]; publishChanges && (!ok || prev != state) {
			changes = append(changes, api.Event{Type: api.EventState, Agent: name, State: state, Detail: p.LastRestartReason})
		}
		s.lastStates[name] = state
	}
	for name := range s.lastStates {
		if !seen[name] {
			delete(s.lastStates, name)
		}
	}
	s.mu.Unlock()
	sort.Slice(changes, func(i, j int) bool { return changes[i].Agent < changes[j].Agent })
	for _, ev := range changes {
		s.publish(ev)
	}
}

// ---- helpers ----

func (s *Server) agentResponse(name string) api.AgentResponse {
	resp := api.AgentResponse{Agent: name}
	if p, err := s.Runtime.GetStatus(name); err == nil && p != nil {
		w := toWire(p)
		resp.Process = &w
	}
	return resp
}

// toWire projects a runtime snapshot onto the API's Process.
func toWire(p *crux.Process) api.Process {
	backend := p.Config.Backend
	if p.BackendOverride != "" {
		backend = p.BackendOverride
	}
	model := p.Config.Model
	if p.ModelOverride != "" {
		model = p.ModelOverride
	}
	return api.Process{
		Name:               p.Name,
		ID:                 p.ID,
		State:              string(p.State),
		Backend:            backend,
		Model:              model,
		PID:                p.PID,
		Paused:             p.Paused,
		PausedReason:       p.PausedReason,
		PausedTrigger:      p.PausedTrigger,
		PausedBy:           p.PausedBy,
		StartedAt:          p.StartedAt,
		LastKick:           p.LastKick,
		RestartCount:       p.RestartCount,
		LastRestartReason:  p.LastRestartReason,
		KickRefused:        p.KickRefused,
		KickRefusalReason:  p.KickRefusalReason,
		KicksUndeliverable: p.KicksUndeliverable,
		BusyCondition:      p.BusyCondition,
		StartFailureClass:  p.StartFailureClass,
	}
}

// writeRuntimeError maps a runtime error onto a status: "not found" is a
// 404, everything else a 500 with the runtime's message.
func (s *Server) writeRuntimeError(w http.ResponseWriter, name, verb string, err error) {
	status, code := http.StatusInternalServerError, api.CodeRuntime
	if strings.Contains(err.Error(), "not found") {
		status, code = http.StatusNotFound, api.CodeNotFound
	}
	s.writeError(w, status, code, fmt.Sprintf("%s %s: %v", verb, name, err))
}

func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "request body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", api.ContentTypeJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.Logger.Warn("crux serve: writing response", "err", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, msg string) {
	s.writeJSON(w, status, api.Error{Error: msg, Code: code})
}

func (s *Server) startStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", api.ContentTypeEvent)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
