package commands

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
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
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) == 1
}

func (s *commonsWebServer) servePage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
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
:root{--card:Canvas;--border:GrayText;color-scheme:light dark;font-family:system-ui,sans-serif;line-height:1.4}body{max-width:920px;margin:2rem auto;padding:0 1rem}button,input,select{font:inherit}table{width:100%;border-collapse:collapse;margin-top:1rem}th,td{border-bottom:1px solid #8884;padding:.45rem;text-align:left}.muted{color:#777}.row-actions{white-space:nowrap}.error{color:#b00020}.ok{color:#087f23}form{display:flex;gap:.5rem;flex-wrap:wrap;margin:1rem 0}input[type=text]{min-width:16rem;flex:1}.pill{border:1px solid #8886;border-radius:999px;padding:.1rem .45rem;font-size:.85em}

:focus-visible{outline:3px solid #327bd6;outline-offset:3px}button{cursor:pointer}button:disabled,button[aria-disabled=true]{cursor:default;opacity:.65}#contribution-tree,[role=group]{list-style:none;padding-left:1rem}.tree-row{display:flex;align-items:center;gap:.5rem;flex-wrap:wrap;padding:.4rem;border-bottom:1px solid #8884}.identity{overflow-wrap:anywhere;flex:1;min-width:10rem}.tree-meta{font-size:.85em;overflow-wrap:anywhere}.staging{position:sticky;bottom:0;background:Canvas;border:1px solid GrayText;padding:.7rem;z-index:2}.staging pre{white-space:pre-wrap;overflow-wrap:anywhere;max-height:15rem;overflow:auto}.tree-tools{display:flex;gap:.5rem;flex-wrap:wrap;margin:1rem 0}table{display:block;overflow:auto}#tree-receipt{white-space:pre-wrap}dialog{background:Canvas;color:CanvasText;border:1px solid GrayText;border-radius:12px;max-width:28rem}dialog::backdrop{background:#0008}
</style>
<h1>The Commons</h1>
<p class="muted">This loopback-only page edits the same local profile store as <code>hivectl hives</code> and the TUI. Registration tokens are never displayed.</p>
<section aria-labelledby="tree-title">
<h2 id="tree-title">Contribution tree</h2>
<p>Review current membership, then Apply. Following is separate from selection; deselecting current membership retains follow rules, which can repopulate it. Readiness and activity are unavailable until relay evidence is connected.</p>
<div class="tree-tools"><label>Search <input id="tree-search" type="search"></label><button id="tree-refresh">Refresh membership</button><button id="tree-leave">Leave page</button></div>
<ul id="contribution-tree" role="tree" aria-label="Hub, Hive and repository contributions"></ul>
<div class="staging" aria-labelledby="diff-title"><h3 id="diff-title">Staged changes</h3><p id="diff-summary">No staged changes.</p><details><summary>Named targets and shared effects</summary><pre id="tree-diff"></pre></details><button id="tree-apply" disabled>Apply</button> <button id="tree-discard" disabled>Discard</button><p id="tree-receipt"></p></div>
</section>
<h2>Profile management</h2>
<p>These existing controls save separately. Apply or discard tree edits first.</p>
<section>
  <label>Routing strategy
    <select id="strategy">
      <option value="ranked">ranked — strict priority/fall-through</option>
      <option value="spread">spread — weighted rotation</option>
      <option value="neediest">neediest — queued work + idle capacity</option>
    </select>
  </label>
  <button id="save-strategy">Apply strategy</button>
</section>
<form id="subscribe">
  <input id="name" aria-label="Profile name" type="text" required placeholder="profile name, e.g. acme">
  <input id="hub" aria-label="Hive URL" type="text" required placeholder="wss://hive.example/contribute">
  <button>Apply subscription</button>
</form>
<div id="message" class="muted" role="status" aria-live="polite" aria-atomic="true"></div>
<table>
  <thead><tr><th>Rank</th><th>Hive</th><th>Hub</th><th>Contributor</th><th>Session</th><th>Actions</th></tr></thead>
  <tbody id="hives"></tbody>
</table>
<p class="muted">Profile store: <span id="path"></span>; projection: <span id="envpath"></span></p>
<script>
const token = {{printf "%q" .Token}};
history.replaceState(null, '', location.pathname);
const headers = {'Content-Type':'application/json', 'X-Hive-Commons-Token': token};
const message = document.getElementById('message');
const nameInput = document.getElementById('name');
const hubInput = document.getElementById('hub');
const strategySelect = document.getElementById('strategy');
let revision = '';
function say(text, cls='muted'){ message.textContent = text; message.className = cls; }
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
Promise.all([load(), loadTree()]).catch(e => say(e.message, 'error'));
</script>
</html>`))
