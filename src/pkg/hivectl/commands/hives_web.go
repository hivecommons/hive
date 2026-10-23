package commands

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

const (
	defaultCommonsWebAddr = "127.0.0.1:0"
	commonsWebAuthHeader  = "X-Hive-Commons-Token"
	commonsWebTokenBytes  = 18
	commonsWebReadBytes   = 1 << 20
)

type hivesWebOptions struct {
	addr string
}

func newHivesWebCommand(env *commandEnv) *cobra.Command {
	opts := &hivesWebOptions{}
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Serve a loopback-only The Commons web UI",
		Long: "Serves a local, token-authenticated contribution tree with staged Apply/Discard, " +
			"plus subscription, rank order and The Commons routing strategy controls. " +
			"The server binds only to loopback and never returns registration tokens.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runHivesWeb(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.addr, "addr", defaultCommonsWebAddr, "loopback listen address")
	return cmd
}

func (e *commandEnv) runHivesWeb(cmd *cobra.Command, opts *hivesWebOptions) error {
	addr := strings.TrimSpace(opts.addr)
	if addr == "" {
		addr = defaultCommonsWebAddr
	}
	if err := validateLoopbackListenAddr(addr); err != nil {
		return err
	}
	deps, err := e.hivesDeps()
	if err != nil {
		return err
	}
	token, err := randomCommonsWebToken()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	srv := &http.Server{
		Handler: newCommonsWebServer(deps, token),
	}
	go func() {
		<-cmd.Context().Done()
		ctx, cancel := context.WithTimeout(context.Background(), e.options.timeout)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	u := url.URL{Scheme: "http", Host: listener.Addr().String(), Path: "/", RawQuery: "token=" + url.QueryEscape(token)}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "The Commons web UI: %s\n", u.String())
	err = srv.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func validateLoopbackListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return &usageError{message: fmt.Sprintf("--addr must be host:port on loopback (for example %s): %v", defaultCommonsWebAddr, err)}
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return &usageError{message: "--addr must bind to loopback only (127.0.0.1, ::1 or localhost)"}
	}
	return nil
}

func randomCommonsWebToken() (string, error) {
	var b [commonsWebTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate local web token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

type commonsWebServer struct {
	deps     *hivesDeps
	token    string
	mu       sync.Mutex
	stages   map[string]*commonsWebStage
	discover func(context.Context, *hivectl.ProfileSet) ([]hivectl.TreeHubSnapshot, map[string]hivectl.TreeRepositorySnapshot)
}

func newCommonsWebServer(deps *hivesDeps, token string) http.Handler {
	return &commonsWebServer{deps: deps, token: token, stages: make(map[string]*commonsWebStage), discover: discoverCommonsTree}
}

func (s *commonsWebServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	if !validCommonsHost(r) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if !validCommonsOrigin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/" && r.Method == http.MethodGet {
		s.servePage(w, r)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/api/tree" && r.Method == http.MethodGet:
		s.handleTree(w, r)
	case (r.URL.Path == "/api/tree/diff" || r.URL.Path == "/api/tree/apply" || r.URL.Path == "/api/tree/discard") && r.Method == http.MethodPost:
		s.handleTreeEdit(w, r)
	case r.URL.Path == "/api/hives" && r.Method == http.MethodGet:
		s.handleList(w, r)
	case r.URL.Path == "/api/hives" && r.Method == http.MethodPost:
		s.handleSubscribe(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/hives/") && r.Method == http.MethodDelete:
		s.handleUnsubscribe(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/hives/") && strings.HasSuffix(r.URL.Path, "/move") && r.Method == http.MethodPost:
		s.handleMove(w, r)
	case r.URL.Path == "/api/strategy" && r.Method == http.MethodPost:
		s.handleStrategy(w, r)
	default:
		http.NotFound(w, r)
	}
}

// validCommonsHost rejects Host headers that are not a loopback host on the
// port the server is actually listening on (DNS-rebinding protection).
func validCommonsHost(r *http.Request) bool {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		return false
	}
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, listenPort, err := net.SplitHostPort(addr.String()); err == nil && listenPort != port {
			return false
		}
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validCommonsOrigin rejects browser requests that are not same-origin with
// this server (CSRF protection). Requests without Origin/Sec-Fetch-Site
// (non-browser clients) are left to token auth.
func validCommonsOrigin(r *http.Request) bool {
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.Path != "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *commonsWebServer) authorized(r *http.Request) bool {
	token := strings.TrimSpace(r.Header.Get(commonsWebAuthHeader))
	return token != "" && token == s.token
}

func (s *commonsWebServer) servePage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token != s.token {
		http.Error(w, "open the tokenized URL printed by hivectl hives web", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = commonsWebTemplate.Execute(w, map[string]string{"Token": token})
}

type commonsWebHive struct {
	Name          string `json:"name"`
	Hub           string `json:"hub"`
	ContributorID string `json:"contributor_id,omitempty"`
	Session       string `json:"session,omitempty"`
	Active        bool   `json:"active"`
	Rank          int    `json:"rank"`
}

type commonsWebListResponse struct {
	Strategy string           `json:"strategy"`
	Path     string           `json:"path"`
	EnvPath  string           `json:"env_path"`
	Hives    []commonsWebHive `json:"hives"`
	// Revision is the profiles.yml revision this listing shows. The page sends
	// it back as If-Match with every edit so a change reviewed against an older
	// file is refused (409) rather than merged.
	Revision string `json:"revision"`
}

func (s *commonsWebServer) handleList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.reviewSet()
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	writeCommonsJSON(w, commonsWebResponse(s.deps.store, set))
}

func (s *commonsWebServer) loadSet(allowEmpty bool) (*hivectl.ProfileSet, error) {
	set, _, err := s.deps.store.LoadOrMigrate()
	if errors.Is(err, hivectl.ErrNoProfiles) && allowEmpty {
		return &hivectl.ProfileSet{Version: hivectl.ProfilesVersion}, nil
	}
	return set, err
}

func commonsWebResponse(store *hivectl.ProfileStore, set *hivectl.ProfileSet) commonsWebListResponse {
	resp := commonsWebListResponse{
		Strategy: hivectl.CommonsStrategyRanked,
		Path:     store.Path(),
		EnvPath:  store.EnvPath(),
	}
	if set == nil {
		return resp
	}
	resp.Revision = set.Revision()
	resp.Strategy = set.EffectiveCommonsStrategy()
	active := set.ActiveProfile()
	for i, profile := range set.Ordered() {
		resp.Hives = append(resp.Hives, commonsWebHive{
			Name:          profile.Name,
			Hub:           commonsWebOrigin(profile.Hub),
			ContributorID: profile.ContributorID,
			Session:       profile.Session,
			Active:        active != nil && strings.EqualFold(active.Name, profile.Name),
			Rank:          i + 1,
		})
	}
	return resp
}

type commonsSubscribeRequest struct {
	Name string `json:"name"`
	Hub  string `json:"hub"`
}

func (s *commonsWebServer) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var req commonsSubscribeRequest
	if !decodeCommonsJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name := strings.TrimSpace(req.Name)
	hub := strings.TrimSpace(req.Hub)
	if err := hivectl.ValidateProfileName(name); err != nil {
		writeCommonsError(w, err)
		return
	}
	if err := hivectl.ValidateHubURL(hub); err != nil {
		writeCommonsError(w, err)
		return
	}
	set, err := s.loadSet(true)
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	if !checkCommonsReviewed(w, r, set) {
		return
	}
	if existing, _ := set.Find(name); existing != nil {
		writeCommonsError(w, &usageError{message: fmt.Sprintf("a hive profile named %q already exists (%s)", existing.Name, existing.Hub)})
		return
	}
	base, err := hivectl.HubHTTPBase(hub)
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	user, err := s.deps.githubUser(r.Context())
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	reg, err := s.deps.registrar.Register(r.Context(), base, user)
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	if strings.TrimSpace(reg.RegistrationToken) == "" {
		writeCommonsError(w, alreadyRegisteredError(user, base, reg.Message, name, hub))
		return
	}
	profile := hivectl.Profile{
		Name:              name,
		Hub:               hub,
		RegistrationToken: strings.TrimSpace(reg.RegistrationToken),
		ContributorID:     strings.TrimSpace(reg.ContributorID),
		AddedAt:           s.deps.now().UTC().Truncate(time.Second),
	}
	if err := set.Add(profile, false); err != nil {
		writeCommonsError(w, err)
		return
	}
	s.commitAndWrite(w, r, set)
}

func (s *commonsWebServer) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	name, ok := commonsPathName(w, r, "/api/hives/")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.loadSet(false)
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	if !checkCommonsReviewed(w, r, set) {
		return
	}
	if _, _, err := set.Remove(name); err != nil {
		writeCommonsError(w, err)
		return
	}
	s.commitAndWrite(w, r, set)
}

type commonsMoveRequest struct {
	Direction string `json:"direction"`
}

func (s *commonsWebServer) handleMove(w http.ResponseWriter, r *http.Request) {
	name, ok := commonsPathName(w, r, "/api/hives/")
	if !ok {
		return
	}
	name = strings.TrimSuffix(name, "/move")
	var req commonsMoveRequest
	if !decodeCommonsJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delta := 0
	switch strings.ToLower(strings.TrimSpace(req.Direction)) {
	case "up":
		delta = -1
	case "down":
		delta = 1
	default:
		writeCommonsError(w, &usageError{message: "direction must be up or down"})
		return
	}
	set, err := s.loadSet(false)
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	if !checkCommonsReviewed(w, r, set) {
		return
	}
	set.Profiles = set.Ordered()
	if len(set.Profiles) > 0 {
		set.Active = set.Profiles[0].Name
	}
	if _, _, err := set.Move(name, delta); err != nil {
		writeCommonsError(w, err)
		return
	}
	s.commitAndWrite(w, r, set)
}

type commonsStrategyRequest struct {
	Strategy string `json:"strategy"`
}

func (s *commonsWebServer) handleStrategy(w http.ResponseWriter, r *http.Request) {
	var req commonsStrategyRequest
	if !decodeCommonsJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.loadSet(false)
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	if !checkCommonsReviewed(w, r, set) {
		return
	}
	if err := set.SetCommonsStrategy(req.Strategy); err != nil {
		writeCommonsError(w, err)
		return
	}
	s.commitAndWrite(w, r, set)
}

// checkCommonsReviewed refuses an edit whose If-Match names a profiles.yml
// revision other than the one just loaded: the operator reviewed a page the
// CLI, the TUI or another tab has since changed. A request without If-Match
// (scripts, older pages) is still guarded by CommitExpecting below.
func checkCommonsReviewed(w http.ResponseWriter, r *http.Request, set *hivectl.ProfileSet) bool {
	reviewed := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if reviewed == "" || reviewed == set.Revision() {
		return true
	}
	writeCommonsError(w, hivectl.ErrStaleRevision)
	return false
}

func (s *commonsWebServer) commitAndWrite(w http.ResponseWriter, r *http.Request, set *hivectl.ProfileSet) {
	if err := s.deps.store.CommitExpecting(set, set.Revision()); err != nil {
		writeCommonsError(w, err)
		return
	}
	if s.deps.signalRelay != nil {
		if _, err := s.deps.signalRelay(r.Context(), s.deps.store); err != nil {
			writeCommonsError(w, err)
			return
		}
	}
	writeCommonsJSON(w, commonsWebResponse(s.deps.store, set))
}

func commonsPathName(w http.ResponseWriter, r *http.Request, prefix string) (string, bool) {
	raw := strings.TrimPrefix(r.URL.Path, prefix)
	name, err := url.PathUnescape(raw)
	if err != nil || strings.TrimSpace(name) == "" {
		http.Error(w, "bad hive name", http.StatusBadRequest)
		return "", false
	}
	return name, true
}

func decodeCommonsJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, commonsWebReadBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func writeCommonsJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeCommonsError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	message := "hive operation failed; check the local configuration and retry"
	switch {
	case errors.Is(err, hivectl.ErrNoProfiles) || errors.Is(err, hivectl.ErrProfileNotFound):
		code = http.StatusNotFound
	case errors.Is(err, hivectl.ErrStaleRevision):
		code = http.StatusConflict
		message = "the hive profiles were changed by hivectl, the TUI or another tab after this page loaded; nothing was saved — reload and review the current hives before retrying (requires renewed review)"
	}
	http.Error(w, message, code)
}

//go:embed hives_web_tree.js
var commonsWebTreeJS string

var commonsWebTemplate = template.Must(template.New("commons").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>The Commons — Hive</title>
<style>
:root{color-scheme:dark light;--surface-0:#080b0f;--surface-1:#0d1218;--surface-2:#121922;--surface-3:#17212d;--line:#263545;--line-strong:#4a5d73;--text:#f6f8fb;--text-muted:#a8b3c2;--brand:#f4c75f;--brand-ink:#17120a;--ok:#74df9a;--err:#ff8a9b;--ring:#80bfff;--hover:#17212d;--r:6px;--r-lg:12px;--font-ui:Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;--font-mono:'SF Mono','Cascadia Code','Fira Code',ui-monospace,monospace}
@media (prefers-color-scheme:light){:root{--surface-0:#f7f8fa;--surface-1:#eef1f5;--surface-2:#fff;--surface-3:#f8fafc;--line:#d9e0e8;--line-strong:#7d8a9b;--text:#111827;--text-muted:#4b5563;--brand:#9a5f0b;--brand-ink:#fff;--ok:#166534;--err:#b91c1c;--ring:#1d4ed8;--hover:#e8edf3}}
*{box-sizing:border-box}
body{font-family:var(--font-ui);background:var(--surface-0);color:var(--text);line-height:1.45;max-width:980px;margin:0 auto;padding:0 1rem 2rem}
.brandbar{display:flex;align-items:center;gap:.8rem;margin:0 -1rem 1.2rem;padding:.9rem 1rem;background:var(--surface-1);border-bottom:1px solid var(--line)}
.bee{font-size:2rem;line-height:1}.eyebrow{font-size:.68rem;font-weight:800;letter-spacing:.12em;text-transform:uppercase;color:var(--brand)}h1{font-size:1.25rem;margin:0}h2{font-size:1.05rem}.brandbar .pill{margin-left:auto}
main>section[aria-labelledby]{background:var(--surface-2);border:1px solid var(--line);border-radius:var(--r-lg);padding:1rem}
button,input,select{font:inherit;color:inherit}button{background:var(--surface-3);border:1px solid var(--line-strong);border-radius:var(--r);padding:.25rem .65rem;cursor:pointer}button:hover:not(:disabled):not([aria-disabled=true]){border-color:var(--brand)}button:disabled,button[aria-disabled=true]{cursor:default;opacity:.6}
#tree-apply{background:var(--brand);color:var(--brand-ink);border-color:var(--brand);font-weight:700}
input[type=text],input[type=search],select{background:var(--surface-1);border:1px solid var(--line-strong);border-radius:var(--r);padding:.3rem .5rem}input[type=text]{min-width:min(16rem,100%);flex:1}select{max-width:100%}
:focus-visible{outline:3px solid var(--ring);outline-offset:2px}[role=treeitem]:focus{outline:none}
table{display:block;overflow:auto;width:100%;border-collapse:collapse;margin-top:1rem}th,td{border-bottom:1px solid var(--line);padding:.45rem;text-align:left}.row-actions{white-space:nowrap}
.muted{color:var(--text-muted)}.error{color:var(--err)}.ok{color:var(--ok)}form{display:flex;gap:.5rem;flex-wrap:wrap;margin:1rem 0}#path,#envpath{overflow-wrap:anywhere}
.pill{border:1px solid var(--line-strong);border-radius:999px;padding:.1rem .5rem;font-size:.78em;color:var(--text-muted)}
kbd{font-family:var(--font-mono);font-size:.78em;border:1px solid var(--line-strong);border-bottom-width:2px;border-radius:4px;padding:0 .3em;background:var(--surface-3)}.keys{font-size:.82rem}
#contribution-tree,[role=group]{list-style:none;margin:0;padding-left:1.25rem}#contribution-tree{padding-left:0}
.tree-row{display:flex;align-items:center;gap:.5rem;flex-wrap:wrap;padding:.35rem .5rem;border-radius:var(--r);border-bottom:1px solid var(--line)}.tree-row:hover{background:var(--hover)}
[role=treeitem]:focus>.tree-row{background:var(--hover);outline:2px solid var(--ring);outline-offset:-2px;box-shadow:inset 4px 0 0 var(--brand)}
.identity{overflow-wrap:anywhere;flex:1;min-width:10rem;cursor:default}.tree-meta{font-size:.85em;overflow-wrap:anywhere}.accept{display:inline-flex;align-items:center;gap:.35rem;font-size:.85em}
.tree-tools{display:flex;gap:.5rem;flex-wrap:wrap;margin:1rem 0}
.staging{position:sticky;bottom:0;background:var(--surface-2);border:1px solid var(--line-strong);border-radius:var(--r-lg);padding:.7rem;z-index:2;display:grid;grid-template-columns:minmax(0,1fr);gap:.7rem;box-shadow:0 -4px 16px rgba(0,0,0,.25);max-height:42vh;overflow:auto}.staging h3{margin:.2rem 0}.staging p{margin:.3rem 0}.staging pre{white-space:pre-wrap;overflow-wrap:anywhere;max-height:8rem;overflow:auto}
@media (min-width:860px){.staging{grid-template-columns:minmax(0,5fr) minmax(0,6fr)}#tree-info{border-right:1px solid var(--line);padding-right:.7rem}}
#tree-info{font-size:.85rem;min-height:5.5rem;overflow:visible}.info-kind{margin:0;font-size:.68rem;font-weight:800;letter-spacing:.12em;text-transform:uppercase;color:var(--brand)}.info-title{margin:.1rem 0 .3rem;font-weight:700;overflow-wrap:anywhere}.info-line{margin:.15rem 0}
#tree-receipt{white-space:pre-wrap}dialog{background:var(--surface-2);color:var(--text);border:1px solid var(--line-strong);border-radius:var(--r-lg);max-width:28rem}dialog::backdrop{background:#0008}dialog button{margin:.25rem .25rem 0 0}
.skip{position:absolute;left:-9999px;top:0}.skip:focus{left:1rem;top:1rem;z-index:10;background:var(--surface-2);color:var(--text);padding:.5rem;border:2px solid var(--ring)}
dl dt{font-weight:bold;margin-top:.5rem}dl dd{margin:0 0 0 1rem}summary{cursor:pointer}
@media (max-width:480px),(max-height:600px){.staging{position:static}}@media (max-width:480px){#contribution-tree,[role=group]{padding-left:.25rem}[role=group]{padding-left:.5rem}.identity{min-width:0}}
.sr-only{position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap}
.more{display:none;order:9;flex-basis:100%;gap:.5rem;flex-wrap:wrap;align-items:center;padding:.15rem 0 .1rem 1.9rem}.tree-row.active>.more{display:flex}.more form{margin:0;display:flex;gap:.4rem}
.toggle{padding:.05rem .45rem;min-width:1.9rem}details.help{margin:.5rem 0}details.help dl{margin:.4rem 0}.brandbar button{margin-left:.5rem}
#manage{margin-top:1.2rem}#manage>summary{font-size:1.05rem;font-weight:700;padding:.6rem 0}
.brandbar{flex-wrap:wrap}
</style>
<button type="button" class="skip" id="skip-apply">Skip to staged changes and Apply</button>
<button type="button" class="skip" id="skip-profiles">Skip to profile management</button>
<main>
<header class="brandbar"><span class="bee" aria-hidden="true">🐝</span><div><div class="eyebrow">Hive · local console</div><h1>The Commons</h1></div><span class="pill">loopback only</span><button id="tree-leave" type="button">Close</button></header>
<section aria-labelledby="tree-title">
<h2 id="tree-title">What you contribute to</h2>
<p>Turn on what this machine may work on. Nothing is saved until you press <strong>Apply</strong>.</p>
<details class="help"><summary>Help &amp; keyboard</summary><dl>
<dt>Hive</dt><dd>A project you can contribute to. A hub is a group of Hives.</dd>
<dt>New</dt><dd>Offered to you but not chosen yet. It gets no work until you turn it on.</dd>
<dt>Excluded</dt><dd>Never, even if you turn on the whole group.</dd>
<dt>Unrestricted</dt><dd>An older setup that gets work from everything the Hive offers. “Choose what to allow” limits it.</dd>
<dt>Accept all new</dt><dd>Also turn on repositories added later. “Accept N new” turns on the ones already waiting.</dd>
<dt>Turning on a group</dt><dd>Turns on everything inside it; a dash means only some are on.</dd>
</dl>
<p class="keys">Keys: <kbd>↑</kbd> <kbd>↓</kbd> move · <kbd>←</kbd> <kbd>→</kbd> collapse or expand · <kbd>Space</kbd> turn on or off · <kbd>/</kbd> search · type a name to jump · <kbd>Tab</kbd> for the row’s buttons.</p>
<p class="muted">This page edits the same local profiles as <code>hivectl hives</code> and the TUI. Registration tokens are never shown.</p>
</details>
<div class="tree-tools"><label>Search <input id="tree-search" type="search" placeholder="Press /"></label><button id="tree-select-matching" hidden>Turn on matching</button><button id="tree-collapse-all">Collapse all</button><button id="tree-expand-all">Expand all</button><button id="tree-refresh">Check for new</button></div>
<ul id="contribution-tree" role="tree" aria-label="Things you contribute to or could"></ul>
<div class="staging"><div id="tree-info" role="region" aria-label="Details of the focused or hovered row"></div><div class="staging-main" aria-labelledby="diff-title"><h3 id="diff-title">Your changes</h3><p id="tree-status" aria-hidden="true"></p><p id="diff-summary">No changes yet.</p><details><summary>Exact changes</summary><pre id="tree-diff"></pre><details><summary>Technical detail</summary><pre id="tree-diff-raw"></pre></details></details><button id="tree-apply" disabled>Apply</button> <button id="tree-discard" disabled>Discard</button><p id="tree-receipt"></p></div></div>
</section>
<div id="message" class="sr-only" role="status" aria-live="polite" aria-atomic="true"></div>
<details id="manage"><summary id="profiles-title">Manage Hives</summary>
<p class="muted">Add a Hive, change the order work is taken in, or remove one. These save straight away, separately from the list above.</p>
<section>
  <label>How to pick the next Hive
    <select id="strategy">
      <option value="ranked">ranked — in the order below</option>
      <option value="spread">spread — take turns</option>
      <option value="neediest">neediest — most waiting work first</option>
    </select>
  </label>
  <button id="save-strategy">Save</button>
</section>
<form id="subscribe">
  <input id="name" aria-label="Name" type="text" required placeholder="name, e.g. acme">
  <input id="hub" aria-label="Hive address" type="text" required placeholder="wss://hive.example/contribute">
  <button>Add Hive</button>
</form>
<table>
  <thead><tr><th>Order</th><th>Hive</th><th>Address</th><th>Contributor</th><th>Session</th><th></th></tr></thead>
  <tbody id="hives"></tbody>
</table>
<p class="muted">Profiles: <span id="path"></span>; projection: <span id="envpath"></span></p>
</details>
</main>
<script>
const token = {{.Token}};
history.replaceState(null, '', location.pathname);
{ const icon = document.createElement('link'); icon.rel = 'icon'; icon.href = 'data:image/svg+xml,' + encodeURIComponent("<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>🐝</text></svg>"); document.head.appendChild(icon); }
const headers = {'Content-Type':'application/json', 'X-Hive-Commons-Token': token};
const message = document.getElementById('message');
const nameInput = document.getElementById('name');
const hubInput = document.getElementById('hub');
const strategySelect = document.getElementById('strategy');
let revision = '';
function say(text, cls='muted'){ message.textContent = text; message.className = cls; const t = document.getElementById('tree-status'); if(t){ t.textContent = text; t.className = cls; } }
async function api(path, opts={}){
  const extra = (opts.method && opts.method !== 'GET' && revision) ? {'If-Match': revision} : {};
  const res = await fetch(path, {...opts, headers: {...headers, ...extra, ...(opts.headers||{})}});
  if(!res.ok) throw new Error(await res.text());
  const data = await res.json();
  if (data && typeof data.revision === 'string') revision = data.revision;
  return data;
}
async function load(){
  const data = await api('/api/hives');
  strategySelect.value = data.strategy || 'ranked';
  document.getElementById('path').textContent = data.path || '';
  document.getElementById('envpath').textContent = data.env_path || '';
  const body = document.getElementById('hives');
  body.textContent = '';
  for (const h of data.hives || []) {
    const tr = document.createElement('tr');
    tr.dataset.name = h.name;
    tr.innerHTML = '<td>' + h.rank + '</td><td><strong></strong> <span class="pill" hidden>active</span></td><td></td><td></td><td></td><td class="row-actions"><button data-act="up">↑</button> <button data-act="down">↓</button> <button data-act="delete">Unsubscribe</button></td>';
    tr.children[1].querySelector('strong').textContent = h.name;
    tr.children[1].querySelector('span').hidden = !h.active;
    tr.children[2].textContent = h.hub;
    tr.children[3].textContent = h.contributor_id || '—';
    tr.children[4].textContent = h.session || '—';
    body.appendChild(tr);
  }
}
document.getElementById('subscribe').addEventListener('submit', async ev => {
  ev.preventDefault();
  try {
    if (!await leaveTreeEdits()) return;
    await api('/api/hives', {method:'POST', body: JSON.stringify({name: nameInput.value, hub: hubInput.value})});
    ev.target.reset(); say('Subscribed.', 'ok'); await load(); await loadTree();
  } catch(e) { say(e.message, 'error'); }
  finally { if(!review) await loadTree().catch(e => say(e.message, 'error')); }
});
function styledConfirm(text, accept='Confirm') {
  return new Promise(resolve => {
    const previous = document.activeElement;
    const dialog = document.createElement('dialog');
    dialog.setAttribute('aria-labelledby', 'confirm-message');
    const p = document.createElement('p'); p.id = 'confirm-message'; p.textContent = text;
    const cancel = document.createElement('button'); cancel.textContent = 'Cancel';
    const yes = document.createElement('button'); yes.textContent = accept;
    dialog.append(p, cancel, yes); document.body.appendChild(dialog);
    let finished = false;
    function done(value) { if(finished) return; finished = true; dialog.close(); dialog.remove(); if(previous && previous.isConnected) previous.focus(); resolve(value); }
    cancel.onclick = () => done(false); yes.onclick = () => done(true);
    dialog.addEventListener('cancel', e => { e.preventDefault(); done(false); });
    dialog.addEventListener('keydown', e => {
      if(e.key !== 'Tab') return;
      if(e.shiftKey && document.activeElement === cancel) { e.preventDefault(); yes.focus(); }
      else if(!e.shiftKey && document.activeElement === yes) { e.preventDefault(); cancel.focus(); }
    });
    dialog.showModal(); cancel.focus();
  });
}
document.getElementById('save-strategy').addEventListener('click', async () => {
  try { if (!await leaveTreeEdits()) return; await api('/api/strategy', {method:'POST', body: JSON.stringify({strategy: strategySelect.value})}); say('Strategy saved.', 'ok'); await load(); await loadTree(); }
  catch(e) { say(e.message, 'error'); }
  finally { if(!review) await loadTree().catch(e => say(e.message, 'error')); }
});
document.getElementById('hives').addEventListener('click', async ev => {
  const button = ev.target.closest('button'); if(!button) return;
  const tr = ev.target.closest('tr'); const name = tr.dataset.name;
  try {
    const deleting = button.dataset.act === 'delete';
    const question = deleting ? 'Unsubscribe from ' + name + '?' : 'Move ' + name + ' ' + button.dataset.act + ' in routing priority?';
    if (!await styledConfirm(question, 'Apply') || !await leaveTreeEdits()) return;
    if (deleting) {
      await api('/api/hives/' + encodeURIComponent(name), {method:'DELETE'});
    } else {
      await api('/api/hives/' + encodeURIComponent(name) + '/move', {method:'POST', body: JSON.stringify({direction: button.dataset.act})});
    }
    say('Saved.', 'ok'); await load(); await loadTree();
  } catch(e) { say(e.message, 'error'); }
  finally { if(!review) await loadTree().catch(e => say(e.message, 'error')); }
});
` + commonsWebTreeJS + `
Promise.all([load(), loadTree()]).then(() => document.getElementById('tree-refresh').click()).catch(e => say(e.message, 'error'));
</script>
</html>`))
