package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
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
	s.mux.HandleFunc("GET /api/inbox/{source}/{id}/content", s.handleInboxContent)
	s.mux.HandleFunc("GET /api/inbox/{source}/{id}/audio", s.handleInboxAudio)
	s.mux.HandleFunc("GET /api/inbox/{source}/{id}/audio/stream", s.handleInboxAudioStream)
	s.mux.HandleFunc("POST /api/knowledge/connectors/{name}/auth/complete", s.handleKnowledgeConnectorAuthComplete)
	s.mux.HandleFunc("POST /api/inbox/{source}/{id}/transcribe", s.handleInboxTranscribe)
	s.mux.HandleFunc("GET /api/inbox/transcriptions", s.handleInboxTranscriptions)
	s.registerInboxUploadRoutes()
	s.registerInboxSpekRunner()
	s.registerInboxEmailRoutes()
	s.registerInboxDeleteRoutes()
}

// inboxSourceReadTimeout bounds one read that may spawn the source's MCP
// process (a direct Plaud content read or an audio link).
const inboxSourceReadTimeout = 90 * time.Second

// InboxContentResponse is GET /api/inbox/{source}/{id}/content.
type InboxContentResponse struct {
	Source string `json:"source"`
	ItemID string `json:"item_id"`
	inbox.Content
}

// InboxAudioResponse is GET /api/inbox/{source}/{id}/audio: a presigned link
// minted for this request. It is never stored by the hive; clients must not
// cache it beyond expires_at.
type InboxAudioResponse struct {
	Source    string    `json:"source"`
	ItemID    string    `json:"item_id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// inboxFindItem resolves the {source}/{id} path of an item request.
func (s *Server) inboxFindItem(w http.ResponseWriter, r *http.Request, svc *inbox.Service) (inbox.Item, bool) {
	source, id := strings.TrimSpace(r.PathValue("source")), strings.TrimSpace(r.PathValue("id"))
	if source == "" || id == "" {
		jsonError(w, "source and id are required", http.StatusBadRequest)
		return inbox.Item{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), inboxListTimeout)
	defer cancel()
	it, found, err := svc.Find(ctx, source, id)
	if !found {
		msg := "unknown inbox item"
		if err != nil {
			msg += ": " + err.Error()
		}
		jsonError(w, msg, http.StatusNotFound)
		return inbox.Item{}, false
	}
	return it, true
}

// handleInboxContent returns the full summary/transcript/highlights of one
// item (dashboard auth, like the listing).
func (s *Server) handleInboxContent(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	it, ok := s.inboxFindItem(w, r, svc)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), inboxSourceReadTimeout)
	defer cancel()
	c, err := svc.Content(ctx, it)
	if err != nil {
		jsonError(w, "reading item: "+err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, InboxContentResponse{Source: it.Source, ItemID: it.ID, Content: c})
}

// requireInboxPlaybackRole admits owners (verified) and read-write sessions:
// playing a recording is a read of private audio, not an operator mutation.
//
// Sharing an item (route, Knowledge-only) takes the same owner or read-write
// admission (requireInboxShareRole); settings stay owner-only.
func requireInboxPlaybackRole(w http.ResponseWriter, r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if isOwnerRole(role) && r.Header.Get(ownerRoleVerifiedHeader) != "true" {
		jsonError(w, "owner or read-write access required", http.StatusForbidden)
		return false
	}
	if !config.RoleAtLeast(role, config.RoleReadWrite) {
		jsonError(w, "owner or read-write access required", http.StatusForbidden)
		return false
	}
	return true
}

// handleInboxAudio (owner or read-write) mints a fresh presigned audio link
// through the item's connector. The link is returned once and never written
// to facts or state; the connector's include_audio option only decides
// whether a sync also stores a link in the fact, playback never depends on it.
func (s *Server) handleInboxAudio(w http.ResponseWriter, r *http.Request) {
	if !requireInboxPlaybackRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	it, ok := s.inboxFindItem(w, r, svc)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), inboxSourceReadTimeout)
	defer cancel()
	link, err := svc.AudioLink(ctx, it)
	switch {
	case errors.Is(err, inbox.ErrNoAudio), errors.Is(err, connector.ErrPlaudNoAudio):
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		jsonError(w, "audio link: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.auditFromRequest(r, "inbox_audio", auditDetail("source", it.Source, "item", it.ID), "")
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, InboxAudioResponse{Source: it.Source, ItemID: it.ID, URL: link.URL, ExpiresAt: link.ExpiresAt})
}

// handleInboxAudioStream (owner or read-write) plays an item's audio on the
// dashboard's own origin: the hive fetches the presigned link and streams the
// bytes back, passing a single Range through so the player can seek. The
// browser never sees the link (the CSP has no media-src for cloud storage and
// must not), nothing is written to disk, and errors name the storage host
// only. A fresh link is audited as inbox_audio_stream; the Range requests of
// one playback reuse it from memory.
func (s *Server) handleInboxAudioStream(w http.ResponseWriter, r *http.Request) {
	if !requireInboxPlaybackRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	it, ok := s.inboxFindItem(w, r, svc)
	if !ok {
		return
	}
	if a, local, err := svc.OpenLocalAudio(r.Context(), it); local {
		// Audio kept on this hive (dashboard recordings) is served from disk.
		if err != nil {
			writeInboxLocalAudioError(w, svc, err)
			return
		}
		s.serveLocalAudio(w, r, a, "inbox_audio_stream", auditDetail("source", it.Source, "item", it.ID))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	st, err := svc.OpenAudioStream(r.Context(), it, r.Header.Get("Range"))
	switch {
	case errors.Is(err, inbox.ErrNoAudio), errors.Is(err, connector.ErrPlaudNoAudio):
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	case errors.Is(err, inbox.ErrAudioLinkRefused):
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	case errors.Is(err, inbox.ErrAudioTooLarge):
		jsonError(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		jsonError(w, "audio stream: "+err.Error(), http.StatusBadGateway)
		return
	}
	if st.Minted {
		s.auditFromRequest(r, "inbox_audio_stream", auditDetail("source", it.Source, "item", it.ID), "")
	}
	h := w.Header()
	if st.Status == http.StatusRequestedRangeNotSatisfiable {
		if st.ContentRange != "" {
			h.Set("Content-Range", st.ContentRange)
		}
		w.WriteHeader(st.Status)
		return
	}
	defer st.Body.Close()
	h.Set("Content-Type", st.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", "inline")
	if st.AcceptRanges {
		h.Set("Accept-Ranges", "bytes")
	}
	if st.ContentRange != "" {
		h.Set("Content-Range", st.ContentRange)
	}
	if st.ContentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(st.ContentLength, 10))
	}
	if st.ETag != "" {
		h.Set("ETag", st.ETag)
	}
	if st.LastModified != "" {
		h.Set("Last-Modified", st.LastModified)
	}
	w.WriteHeader(st.Status)
	// A client that stops listening (seek, pause, tab closed) ends the copy
	// through the request context; the error is expected and not logged.
	_, _ = io.Copy(w, st.Body)
}

// InboxTranscribeResponse is POST /api/inbox/{source}/{id}/transcribe.
type InboxTranscribeResponse struct {
	Source string                   `json:"source"`
	ItemID string                   `json:"item_id"`
	State  inbox.TranscriptionState `json:"state"`
}

// handleInboxTranscribe (owner or read-write) queues a machine transcription
// of one pending item on this hive, with the owner's own provider key. The
// hive downloads the audio itself and streams it to the provider; the
// presigned link is never forwarded or logged. Each run is audited as
// inbox_transcribe with its outcome. 202 queued, 200 skipped up front
// (no audio, too long, budget), 409 busy/already transcribed/has text,
// 412 transcription disabled or not consented.
func (s *Server) handleInboxTranscribe(w http.ResponseWriter, r *http.Request) {
	if !requireInboxPlaybackRole(w, r) {
		return
	}
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	it, ok := s.inboxFindItem(w, r, svc)
	if !ok {
		return
	}
	opts := inbox.TranscribeOptions{Actor: requestUser(r), Audit: func(a inbox.TranscribeAudit) {
		if s.audit != nil {
			s.audit.Log(a.Actor, "inbox_transcribe", auditDetail(a.Fields()...), "")
		}
	}}
	st, err := svc.Transcribe(r.Context(), it, opts)
	switch {
	case errors.Is(err, inbox.ErrTranscriptionDisabled), errors.Is(err, inbox.ErrTranscriptionConsent):
		jsonError(w, err.Error(), http.StatusPreconditionFailed)
		return
	case errors.Is(err, inbox.ErrTranscriptionBusy), errors.Is(err, inbox.ErrAlreadyTranscribed), errors.Is(err, inbox.ErrNotPending):
		jsonError(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		jsonError(w, "transcription failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	code := http.StatusAccepted
	if st.Status == inbox.TranscriptionSkipped {
		code = http.StatusOK
	}
	jsonStatusResponse(w, code, InboxTranscribeResponse{Source: it.Source, ItemID: it.ID, State: st})
}

// InboxTranscriptionsResponse is GET /api/inbox/transcriptions.
type InboxTranscriptionsResponse struct {
	// Active lists queued and running transcriptions in queue order, with
	// phase, progress, ETA and queue position.
	Active []inbox.ActiveTranscription `json:"active"`
}

// handleInboxTranscriptions (dashboard auth, like the listing) returns the
// live state of queued and running transcriptions only. It reads no source
// and no connector fact, so the Inbox can poll it every few seconds while a
// job runs; an item that leaves the list has finished.
func (s *Server) handleInboxTranscriptions(w http.ResponseWriter, _ *http.Request) {
	svc, ok := s.inboxService(w)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, InboxTranscriptionsResponse{Active: svc.ActiveTranscriptions()})
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
	// EmailIn is this hive's email-in address, for owner and read-write
	// callers (api_email_in.go).
	EmailIn *EmailInView `json:"email_in,omitempty"`
	// InputMethods is the static catalogue of input methods the Inbox
	// lists but does not connect to (inbox.InputMethods): tools that
	// already type into the Quick note box, and neural interfaces with no
	// public API yet. They are not sources and have no settings.
	InputMethods []inbox.InputMethod `json:"input_methods"`
}

func (s *Server) inboxService(w http.ResponseWriter) (*inbox.Service, bool) {
	if s == nil || s.deps == nil || s.deps.Inbox == nil {
		jsonError(w, "inbox is not available: configure a voice or meeting knowledge connector (plaud, wisprflow, zoom, teams, discord, google-meet, webex, slack-huddles, fireflies, fathom, limitless, granola, otter)", http.StatusNotFound)
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
		resp := InboxListResponse{Items: []inbox.Item{}, Sources: []InboxSourceStatus{}, Repos: s.inboxRepos(), Settings: inbox.Settings{Rules: []config.InboxRule{}}, InputMethods: inbox.InputMethods()}
		if s != nil && s.deps != nil && s.deps.Config != nil {
			resp.Sources = s.inboxSources(ctx, s.deps.Config.Inbox.EffectiveSources())
		}
		if s != nil {
			resp.EmailIn = s.emailInView(r)
		}
		jsonResponse(w, resp)
		return
	}
	svc := s.deps.Inbox
	items, err := svc.Items(ctx)
	if api := s.deps.Knowledge; api != nil {
		for i := range items {
			items[i].FactSourceLayer = string(api.VaultSourceLayerForFile(items[i].FactPath))
		}
	}
	resp := InboxListResponse{
		Items:       items,
		Unprocessed: inbox.Unprocessed(items),
		Sources:     s.inboxSources(ctx, svc.Config().EffectiveSources()),
		Repos:       s.inboxRepos(),
		Settings:    svc.Settings(),
		EmailIn:     s.emailInView(r),

		InputMethods: inbox.InputMethods(),
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
	// Params are short dialog choices kept with a transcribe-first share.
	Params map[string]string `json:"params,omitempty"`
}

// requireInboxShareRole admits owners (verified) and read-write sessions to
// share an inbox item; read is denied. Every share is audited with the user.
func requireInboxShareRole(w http.ResponseWriter, r *http.Request) bool {
	return requireInboxPlaybackRole(w, r)
}

// InboxRouteResponse is POST /api/inbox/route.
type InboxRouteResponse struct {
	Source      string           `json:"source"`
	ItemID      string           `json:"item_id"`
	Destination string           `json:"destination"`
	State       inbox.RouteState `json:"state"`
	// WaitingTranscript is true when the item has audio but no text: the hive
	// transcribes it first and shares when the transcript completes.
	WaitingTranscript bool `json:"waiting_transcript,omitempty"`
}

// handleInboxRoute (owner or read-write) queues one item for one destination; progress is
// read back from GET /api/inbox.
func (s *Server) handleInboxRoute(w http.ResponseWriter, r *http.Request) {
	if !requireInboxShareRole(w, r) {
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
	if req.Destination == inboxSpekIntentDest {
		// The session target of a spek share is checked by the seed
		// endpoints (role, session visibility and use); the route API never
		// takes one from the client.
		jsonError(w, "send items into an Inception (spek) session with POST /api/sessions/{id}/seed (Share → Hive → spek…)", http.StatusBadRequest)
		return
	}
	actor := requestUser(r)
	var st inbox.RouteState
	waiting := false
	if req.Destination == inbox.DestDibs {
		// DIBS sends straight away, audio-only items included.
		st, err = svc.Route(ctx, it, inbox.Request{Destination: req.Destination, Repo: req.Repo, Actor: actor})
	} else {
		var out inbox.ShareOutcome
		out, err = svc.ShareWhenReady(ctx, it, inbox.ShareIntent{Destination: req.Destination, Actor: actor, Repo: req.Repo, Params: req.Params}, s.inboxTranscribeOptions(actor))
		st, waiting = out.State, out.Waiting
	}
	switch {
	case errors.Is(err, inbox.ErrRouteBusy):
		jsonError(w, "that route is already queued or running", http.StatusConflict)
		return
	case errors.Is(err, inbox.ErrUnknownDestination):
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, inbox.ErrTranscribeUnavailable):
		jsonError(w, "this item has audio but no text yet, and "+err.Error(), http.StatusPreconditionFailed)
		return
	case err != nil:
		jsonError(w, "route failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "inbox_route", auditDetail("source", req.Source, "item", req.ItemID, "destination", req.Destination, "repo", req.Repo, "actor", actor, "transcribe_first", strconv.FormatBool(waiting)), "")
	jsonStatusResponse(w, http.StatusAccepted, InboxRouteResponse{Source: req.Source, ItemID: req.ItemID, Destination: req.Destination, State: st, WaitingTranscript: waiting})
}

// inboxTranscribeOptions builds the audited options for a transcription the
// request caused.
func (s *Server) inboxTranscribeOptions(actor string) inbox.TranscribeOptions {
	return inbox.TranscribeOptions{Actor: actor, Audit: func(a inbox.TranscribeAudit) {
		if s.audit != nil {
			s.audit.Log(a.Actor, "inbox_transcribe", auditDetail(a.Fields()...), "")
		}
	}}
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
	// WaitingTranscript counts items that will be kept once transcribed.
	WaitingTranscript int `json:"waiting_transcript,omitempty"`
}

// handleInboxDismiss (owner or read-write) marks items "knowledge only"
// (bulk). Audio-only items are transcribed first when transcription is
// available (the dismissal runs when the transcript completes).
func (s *Server) handleInboxDismiss(w http.ResponseWriter, r *http.Request) {
	if !requireInboxShareRole(w, r) {
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
	n, waiting := 0, 0
	actor := requestUser(r)
	for _, it := range req.Items {
		if it.Source == "" || it.ItemID == "" {
			continue
		}
		if !req.Undo {
			if item, found, _ := svc.Find(r.Context(), it.Source, it.ItemID); found {
				if out, err := svc.ShareWhenReady(r.Context(), item, inbox.ShareIntent{Destination: inbox.DestDismiss, Actor: actor}, s.inboxTranscribeOptions(actor)); err == nil {
					if out.Waiting {
						waiting++
					}
					n++
					continue
				} else if !errors.Is(err, inbox.ErrTranscribeUnavailable) {
					jsonError(w, "dismiss failed: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
		if err := svc.DismissBy(actor, it.Source, it.ItemID, !req.Undo); err != nil {
			jsonError(w, "dismiss failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		n++
	}
	s.auditFromRequest(r, "inbox_dismiss", auditDetail("count", strconv.Itoa(n), "actor", actor, "waiting_transcript", strconv.Itoa(waiting)), "")
	jsonResponse(w, InboxDismissResponse{Dismissed: n, WaitingTranscript: waiting})
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
	// Transcription replaces inbox.transcription whole when present.
	Transcription *config.InboxTranscription `json:"transcription,omitempty"`
}

// handleInboxSettingsPost (owner) updates inbox.rules / inbox.spek.repo /
// inbox.dibs.url / inbox.transcription and persists hive.yaml.
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
	if req.Transcription != nil {
		next.Transcription = *req.Transcription
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
	s.auditFromRequest(r, "inbox_settings", auditDetail("rules", strconv.Itoa(len(next.Rules)), "spek_repo", next.Spek.Repo, "dibs_url", next.Dibs.URL, "transcription", strconv.FormatBool(next.Transcription.Active()), "transcription_mode", next.Transcription.EffectiveMode()), "")
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

// inboxSectionFragment is the dashboard hash that opens the Inbox section.
const inboxSectionFragment = "/#section-inbox-section"

// NewInboxDestinations builds the spek_rfc and dibs destinations from
// config. File RFC always goes through an agent kick (spektacular has no
// command that drafts a spec on its own); kick comes from the dashboard deps.
func NewInboxDestinations(cfg *config.Config, kick inbox.Kicker) []inbox.Destination {
	binary := cfg.Inbox.Spek.Binary
	if binary == "" {
		binary = cfg.Runs.Spektacular.BinaryOrDefault()
	}
	spek := &inbox.SpekDestination{
		Binary:      binary,
		DefaultRepo: cfg.Inbox.Spek.Repo,
		Agent:       cfg.InboxSpekAgent(),
		Kick:        kick,
	}
	if base := strings.TrimRight(cfg.Dashboard.PublicURL, "/"); base != "" {
		spek.InboxURL = base + inboxSectionFragment
	}
	dibs := &inbox.DibsDestination{URL: cfg.Inbox.Dibs.URL}
	if env := cfg.Inbox.Dibs.TokenEnv; env != "" {
		dibs.Token = os.Getenv(env)
	}
	return []inbox.Destination{spek, dibs}
}

// InboxReadiness returns the probes the settings view reports.
// File RFC is ready when an agent can take the hand-off.
func InboxReadiness(cfg *config.Config) (spek func() bool, dibs func() bool) {
	spek = func() bool { return cfg.InboxSpekAgent() != "" }
	dibs = func() bool {
		return cfg.Inbox.Dibs.TokenEnv != "" && os.Getenv(cfg.Inbox.Dibs.TokenEnv) != ""
	}
	return spek, dibs
}
