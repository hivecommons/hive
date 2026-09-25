package commands

import (
	"context"
	"crypto/rand"
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
		Long: "Serves a local, token-authenticated page for subscribing to hives, " +
			"unsubscribing, changing rank order and choosing The Commons routing strategy. " +
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
	deps  *hivesDeps
	token string
	mu    sync.Mutex
}

func newCommonsWebServer(deps *hivesDeps, token string) http.Handler {
	return &commonsWebServer{deps: deps, token: token}
}

func (s *commonsWebServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" && r.Method == http.MethodGet {
		s.servePage(w, r)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
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

func (s *commonsWebServer) authorized(r *http.Request) bool {
	token := strings.TrimSpace(r.Header.Get(commonsWebAuthHeader))
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("token"))
	}
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
}

func (s *commonsWebServer) handleList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.loadSet(true)
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
	resp.Strategy = set.EffectiveCommonsStrategy()
	active := set.ActiveProfile()
	for i, profile := range set.Ordered() {
		resp.Hives = append(resp.Hives, commonsWebHive{
			Name:          profile.Name,
			Hub:           profile.Hub,
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
	if err := set.SetCommonsStrategy(req.Strategy); err != nil {
		writeCommonsError(w, err)
		return
	}
	s.commitAndWrite(w, r, set)
}

func (s *commonsWebServer) commitAndWrite(w http.ResponseWriter, r *http.Request, set *hivectl.ProfileSet) {
	if err := s.deps.store.Commit(set); err != nil {
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
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
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
	if errors.Is(err, hivectl.ErrNoProfiles) || errors.Is(err, hivectl.ErrProfileNotFound) {
		code = http.StatusNotFound
	}
	http.Error(w, err.Error(), code)
}

var commonsWebTemplate = template.Must(template.New("commons").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>The Commons — Hive</title>
<style>
:root{color-scheme:light dark;font-family:system-ui,sans-serif;line-height:1.4}body{max-width:920px;margin:2rem auto;padding:0 1rem}button,input,select{font:inherit}table{width:100%;border-collapse:collapse;margin-top:1rem}th,td{border-bottom:1px solid #8884;padding:.45rem;text-align:left}.muted{color:#777}.row-actions{white-space:nowrap}.error{color:#b00020}.ok{color:#087f23}form{display:flex;gap:.5rem;flex-wrap:wrap;margin:1rem 0}input[type=text]{min-width:16rem;flex:1}.pill{border:1px solid #8886;border-radius:999px;padding:.1rem .45rem;font-size:.85em}</style>
<h1>The Commons</h1>
<p class="muted">This loopback-only page edits the same local profile store as <code>hivectl hives</code> and the TUI. Registration tokens are never displayed.</p>
<section>
  <label>Routing strategy
    <select id="strategy">
      <option value="ranked">ranked — strict priority/fall-through</option>
      <option value="spread">spread — weighted rotation</option>
      <option value="neediest">neediest — queued work + idle capacity</option>
    </select>
  </label>
  <button id="save-strategy">Save strategy</button>
</section>
<form id="subscribe">
  <input id="name" type="text" required placeholder="profile name, e.g. acme">
  <input id="hub" type="text" required placeholder="wss://hive.example/contribute">
  <button>Subscribe</button>
</form>
<div id="message" class="muted"></div>
<table>
  <thead><tr><th>Rank</th><th>Hive</th><th>Hub</th><th>Contributor</th><th>Session</th><th>Actions</th></tr></thead>
  <tbody id="hives"></tbody>
</table>
<p class="muted">Profile store: <span id="path"></span>; projection: <span id="envpath"></span></p>
<script>
const token = {{printf "%q" .Token}};
const headers = {'Content-Type':'application/json', 'X-Hive-Commons-Token': token};
const message = document.getElementById('message');
const nameInput = document.getElementById('name');
const hubInput = document.getElementById('hub');
const strategySelect = document.getElementById('strategy');
function say(text, cls='muted'){ message.textContent = text; message.className = cls; }
async function api(path, opts={}){
  const res = await fetch(path, {...opts, headers: {...headers, ...(opts.headers||{})}});
  if(!res.ok) throw new Error(await res.text());
  return res.json();
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
    tr.draggable = true;
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
    await api('/api/hives', {method:'POST', body: JSON.stringify({name: nameInput.value, hub: hubInput.value})});
    ev.target.reset(); say('Subscribed.', 'ok'); await load();
  } catch(e) { say(e.message, 'error'); }
});
document.getElementById('save-strategy').addEventListener('click', async () => {
  try { await api('/api/strategy', {method:'POST', body: JSON.stringify({strategy: strategySelect.value})}); say('Strategy saved.', 'ok'); await load(); }
  catch(e) { say(e.message, 'error'); }
});
document.getElementById('hives').addEventListener('click', async ev => {
  const button = ev.target.closest('button'); if(!button) return;
  const tr = ev.target.closest('tr'); const name = tr.dataset.name;
  try {
    if (button.dataset.act === 'delete') {
      if (!confirm('Unsubscribe from ' + name + '?')) return;
      await api('/api/hives/' + encodeURIComponent(name), {method:'DELETE'});
    } else {
      await api('/api/hives/' + encodeURIComponent(name) + '/move', {method:'POST', body: JSON.stringify({direction: button.dataset.act})});
    }
    say('Saved.', 'ok'); await load();
  } catch(e) { say(e.message, 'error'); }
});
let dragName = '';
document.getElementById('hives').addEventListener('dragstart', ev => { const tr=ev.target.closest('tr'); dragName = tr ? tr.dataset.name : ''; });
document.getElementById('hives').addEventListener('dragover', ev => ev.preventDefault());
document.getElementById('hives').addEventListener('drop', async ev => {
  ev.preventDefault();
  const target = ev.target.closest('tr'); if(!dragName || !target || dragName === target.dataset.name) return;
  try {
    const rows = Array.from(document.querySelectorAll('#hives tr')).map(tr => tr.dataset.name);
    let from = rows.indexOf(dragName), to = rows.indexOf(target.dataset.name);
    while (from < to) { await api('/api/hives/' + encodeURIComponent(dragName) + '/move', {method:'POST', body: JSON.stringify({direction:'down'})}); from++; }
    while (from > to) { await api('/api/hives/' + encodeURIComponent(dragName) + '/move', {method:'POST', body: JSON.stringify({direction:'up'})}); from--; }
    say('Rank order saved.', 'ok'); await load();
  } catch(e) { say(e.message, 'error'); }
});
load().catch(e => say(e.message, 'error'));
</script>
</html>`))
