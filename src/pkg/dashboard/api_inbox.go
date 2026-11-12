package dashboard

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// inboxListTimeout bounds one Inbox listing (reads connector facts).
const inboxListTimeout = 20 * time.Second

// Inbox (#voice-sources): source-agnostic triage of voice-recorder items
// that knowledge connectors ingest, with routes to spek RFCs and dibs.
func (s *Server) registerInboxRoutes() {
	s.mux.HandleFunc("GET /api/inbox", s.handleInboxList)
	s.mux.HandleFunc("POST /api/inbox/route", s.handleInboxRoute)
	s.mux.HandleFunc("POST /api/inbox/dismiss", s.handleInboxDismiss)
	s.mux.HandleFunc("GET /api/inbox/settings", s.handleInboxSettingsGet)
	s.mux.HandleFunc("POST /api/inbox/settings", s.handleInboxSettingsPost)
	s.mux.HandleFunc("POST /api/knowledge/connectors/{name}/auth/complete", s.handleKnowledgeConnectorAuthComplete)
}

// InboxSourceStatus is one configured source as the Inbox header shows it.
type InboxSourceStatus struct {
	Type      string `json:"type"`
	Connector string `json:"connector,omitempty"`
	// Configured is true when a knowledge.connectors entry of this type exists.
	Configured bool `json:"configured"`
	// Connected mirrors the connector's AuthState when it signs in interactively.
	Connected bool   `json:"connected"`
	Account   string `json:"account,omitempty"`
}

// InboxListResponse is GET /api/inbox.
type InboxListResponse struct {
	Items       []inbox.Item        `json:"items"`
	Unprocessed int                 `json:"unprocessed"`
	Sources     []InboxSourceStatus `json:"sources"`
	// Repos are the hive's configured repos for the RFC repo picker.
	Repos    []string       `json:"repos"`
	Settings inbox.Settings `json:"settings"`
	Error    string         `json:"error,omitempty"`
}

func (s *Server) inboxService(w http.ResponseWriter) (*inbox.Service, bool) {
	if s == nil || s.deps == nil || s.deps.Inbox == nil {
		jsonError(w, "inbox is not available: configure a voice knowledge connector (plaud, wisprflow)", http.StatusNotFound)
		return nil, false
	}
	return s.deps.Inbox, true
}

func (s *Server) handleInboxList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), inboxListTimeout)
	defer cancel()
	if s == nil || s.deps == nil || s.deps.Inbox == nil {
		// No service (older boot path / tests): still a valid, empty inbox —
		// the panel must render its empty state, not spin on a 404.
		resp := InboxListResponse{Items: []inbox.Item{}, Sources: []InboxSourceStatus{}, Repos: s.inboxRepos(), Settings: inbox.Settings{Rules: []config.InboxRule{}}}
		if s != nil && s.deps != nil && s.deps.Config != nil {
			resp.Sources = s.inboxSources(ctx, s.deps.Config.Inbox.EffectiveSources())
		}
		jsonResponse(w, resp)
		return
	}
	svc := s.deps.Inbox
	items, err := svc.Items(ctx)
	resp := InboxListResponse{
		Items:       items,
		Unprocessed: inbox.Unprocessed(items),
		Sources:     s.inboxSources(ctx, svc.Config().EffectiveSources()),
		Repos:       s.inboxRepos(),
		Settings:    svc.Settings(),
	}
	if resp.Items == nil {
		resp.Items = []inbox.Item{}
	}
	if err != nil {
		resp.Error = err.Error()
	}
	jsonResponse(w, resp)
}

// inboxSources pairs each source type with its connector's cached auth state.
func (s *Server) inboxSources(ctx context.Context, types []string) []InboxSourceStatus {
	out := make([]InboxSourceStatus, 0, len(types))
	rt := s.knowledgeConnectors()
	var statuses []connector.Status
	if rt != nil {
		statuses = rt.Statuses()
	}
	lookup, _ := rt.(knowledgeConnectorLookup)
	for _, typ := range types {
		st := InboxSourceStatus{Type: typ}
		for _, cs := range statuses {
			if cs.Type != typ {
				continue
			}
			st.Configured, st.Connector = true, cs.Name
			if lookup != nil {
				if c, ok := lookup.Connector(cs.Name); ok {
					if auth, ok := c.(connector.Authenticator); ok {
						a := auth.AuthStatus(ctx, false)
						st.Connected, st.Account = a.Connected, a.Account
					}
				}
			}
			break
		}
		out = append(out, st)
	}
	return out
}

func (s *Server) inboxRepos() []string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return []string{}
	}
	repos := append([]string(nil), s.deps.Config.Project.Repos...)
	if def := s.deps.Config.Inbox.Spek.Repo; def != "" {
		found := false
		for _, r := range repos {
			if r == def {
				found = true
			}
		}
		if !found {
			repos = append([]string{def}, repos...)
		}
	}
	if repos == nil {
		repos = []string{}
	}
	return repos
}

type inboxRouteRequest struct {
	Source      string `json:"source"`
	ItemID      string `json:"item_id"`
	Destination string `json:"destination"`
	Repo        string `json:"repo,omitempty"`
}

// InboxRouteResponse is POST /api/inbox/route.
type InboxRouteResponse struct {
	Source      string           `json:"source"`
	ItemID      string           `json:"item_id"`
	Destination string           `json:"destination"`
	State       inbox.RouteState `json:"state"`
}

// handleInboxRoute (owner) queues one item for one destination; progress is
// read back from GET /api/inbox.
func (s *Server) handleInboxRoute(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	var req inboxRouteRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Source, req.ItemID, req.Destination, req.Repo = strings.TrimSpace(req.Source), strings.TrimSpace(req.ItemID), strings.TrimSpace(req.Destination), strings.TrimSpace(req.Repo)
	if req.Source == "" || req.ItemID == "" || req.Destination == "" {
		jsonError(w, "source, item_id and destination are required", http.StatusBadRequest)
		return
	}
	if req.Repo != "" && !inboxRepoOK(req.Repo) {
		jsonError(w, "repo must be owner/repo", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), inboxListTimeout)
	defer cancel()
	it, found, err := svc.Find(ctx, req.Source, req.ItemID)
	if !found {
		msg := "unknown inbox item"
		if err != nil {
			msg += ": " + err.Error()
		}
		jsonError(w, msg, http.StatusNotFound)
		return
	}
	st, err := svc.Route(ctx, it, inbox.Request{Destination: req.Destination, Repo: req.Repo})
	switch {
	case errors.Is(err, inbox.ErrRouteBusy):
		jsonError(w, "that route is already queued or running", http.StatusConflict)
		return
	case errors.Is(err, inbox.ErrUnknownDestination):
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		jsonError(w, "route failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "inbox_route", auditDetail("source", req.Source, "item", req.ItemID, "destination", req.Destination, "repo", req.Repo), "")
	jsonStatusResponse(w, http.StatusAccepted, InboxRouteResponse{Source: req.Source, ItemID: req.ItemID, Destination: req.Destination, State: st})
}

func inboxRepoOK(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && owner != "" && name != "" && !strings.ContainsAny(repo, " \t\n")
}

type inboxDismissRequest struct {
	Items []struct {
		Source string `json:"source"`
		ItemID string `json:"item_id"`
	} `json:"items"`
	Undo bool `json:"undo,omitempty"`
}

// InboxDismissResponse is POST /api/inbox/dismiss.
type InboxDismissResponse struct {
	Dismissed int `json:"dismissed"`
}

// handleInboxDismiss (owner) marks items "knowledge only" (bulk).
func (s *Server) handleInboxDismiss(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	var req inboxDismissRequest
	if err := decodeBody(r, &req); err != nil || len(req.Items) == 0 {
		jsonError(w, "items[] is required", http.StatusBadRequest)
		return
	}
	n := 0
	for _, it := range req.Items {
		if it.Source == "" || it.ItemID == "" {
			continue
		}
		if err := svc.Dismiss(it.Source, it.ItemID, !req.Undo); err != nil {
			jsonError(w, "dismiss failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		n++
	}
	s.auditFromRequest(r, "inbox_dismiss", auditDetail("count", strconv.Itoa(n)), "")
	jsonResponse(w, InboxDismissResponse{Dismissed: n})
}

func (s *Server) handleInboxSettingsGet(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	jsonResponse(w, svc.Settings())
}

type inboxSettingsRequest struct {
	Rules    *[]config.InboxRule `json:"rules,omitempty"`
	SpekRepo *string             `json:"spek_repo,omitempty"`
	DibsURL  *string             `json:"dibs_url,omitempty"`
}

// handleInboxSettingsPost (owner) updates inbox.rules / inbox.spek.repo /
// inbox.dibs.url and persists hive.yaml.
func (s *Server) handleInboxSettingsPost(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	if s.deps.Config == nil {
		jsonError(w, "config is not available", http.StatusServiceUnavailable)
		return
	}
	var req inboxSettingsRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	next := s.deps.Config.Inbox
	if req.Rules != nil {
		next.Rules = *req.Rules
	}
	if req.SpekRepo != nil {
		next.Spek.Repo = strings.TrimSpace(*req.SpekRepo)
	}
	if req.DibsURL != nil {
		next.Dibs.URL = strings.TrimRight(strings.TrimSpace(*req.DibsURL), "/")
	}
	if err := config.ValidateInbox(next); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.deps.Config.Inbox = next
	svc.UpdateConfig(next)
	if err := s.saveConfig(); err != nil {
		jsonError(w, "saving config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "inbox_settings", auditDetail("rules", strconv.Itoa(len(next.Rules)), "spek_repo", next.Spek.Repo, "dibs_url", next.Dibs.URL), "")
	jsonResponse(w, svc.Settings())
}

// handleKnowledgeConnectorAuthComplete (owner) finishes a sign-in whose
// OAuth callback cannot reach a hosted hive: the operator pastes either the
// redirect URL the browser landed on or the vendor token file contents.
func (s *Server) handleKnowledgeConnectorAuthComplete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	name := r.PathValue("name")
	auth, ok := s.knowledgeAuthenticator(w, name)
	if !ok {
		return
	}
	completer, ok := auth.(connector.AuthCompleter)
	if !ok {
		jsonError(w, "this connector does not support pasted sign-in completion", http.StatusBadRequest)
		return
	}
	var comp connector.AuthCompletion
	if err := decodeBody(r, &comp); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	ctx, cancel := knowledgeAuthContext(r)
	defer cancel()
	st, err := completer.CompleteAuth(ctx, comp)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	mode := "redirect_url"
	if comp.Tokens != "" {
		mode = "tokens"
	}
	s.auditFromRequest(r, "knowledge_connector_auth_complete", auditDetail("connector", name, "mode", mode), "")
	jsonResponse(w, st)
}

// NewInboxDestinations builds the spek_rfc and dibs destinations from
// config; token minting and agent kicks come from the dashboard deps.
func NewInboxDestinations(cfg *config.Config, token func(ctx context.Context, repo string) (string, error), kick inbox.Kicker) []inbox.Destination {
	binary := cfg.Inbox.Spek.Binary
	if binary == "" {
		binary = cfg.Runs.Spektacular.BinaryOrDefault()
	}
	spek := &inbox.SpekDestination{
		Binary:      binary,
		DefaultRepo: cfg.Inbox.Spek.Repo,
		Token:       token,
		Agent:       cfg.InboxSpekAgent(),
		Kick:        kick,
	}
	dibs := &inbox.DibsDestination{URL: cfg.Inbox.Dibs.URL}
	if env := cfg.Inbox.Dibs.TokenEnv; env != "" {
		dibs.Token = os.Getenv(env)
	}
	return []inbox.Destination{spek, dibs}
}

// InboxReadiness returns the probes the settings view reports.
func InboxReadiness(cfg *config.Config) (spek func() bool, dibs func() bool) {
	binary := cfg.Inbox.Spek.Binary
	if binary == "" {
		binary = cfg.Runs.Spektacular.BinaryOrDefault()
	}
	spek = func() bool {
		if _, err := exec.LookPath(binary); err == nil {
			return true
		}
		return cfg.InboxSpekAgent() != ""
	}
	dibs = func() bool {
		return cfg.Inbox.Dibs.TokenEnv != "" && os.Getenv(cfg.Inbox.Dibs.TokenEnv) != ""
	}
	return spek, dibs
}
