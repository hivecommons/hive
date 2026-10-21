package commands

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
)

// commonsTreeSession is the one staged-edit session a Web server process holds.
// Edits are staged in memory through hivectl.StagedEdits; nothing touches
// profiles.yml until /api/tree/apply. The browser never owns consent state.
type commonsTreeSession struct {
	edits     *hivectl.StagedEdits
	discovery commonsTreeDiscovery
	openedAt  time.Time
}

// commonsTreeDiscoveryState reports one discovery read without its raw error:
// endpoints and errors can carry query strings or tokens, so only a short
// reason reaches the browser.
type commonsTreeDiscoveryState struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type commonsTreeDiscovery struct {
	Hubs          map[string]commonsTreeDiscoveryState `json:"hubs"`
	Subscriptions map[string]commonsTreeDiscoveryState `json:"subscriptions"`
	// Available is false when the server has no discovery functions at all.
	Available bool `json:"available"`
}

type commonsTreeResponse struct {
	Tree      *hivectl.ContributionTree `json:"tree"`
	Diff      hivectl.ContributionDiff  `json:"diff"`
	Revision  string                    `json:"revision"`
	Staged    bool                      `json:"staged"`
	Discovery commonsTreeDiscovery      `json:"discovery"`
	OpenedAt  time.Time                 `json:"opened_at"`
	// Runtime carries the live relay and durable activity overlays; when a
	// source is missing it says so with a reason rather than showing idle or
	// zero.
	Runtime hivectl.TreeRuntime `json:"runtime"`
}

const (
	commonsDiscoveryUnavailable   = "unavailable"
	commonsDiscoveryComplete      = "complete"
	commonsDiscoveryReasonMissing = "no discovery source configured"
	commonsDiscoveryReasonHubURL  = "hub URL is not a usable origin"
)

var errCommonsTreePending = errors.New("staged edits are pending; apply or discard them before refreshing discovery")

// discoverCommonsTree reads hub registries and repository membership for the
// saved set. Every failure stays a per-branch unavailable state: an outage
// must never become an empty collection, a wildcard, or a blocked page.
func (s *commonsWebServer) discoverCommonsTree(ctx context.Context, set *hivectl.ProfileSet) ([]hivectl.TreeHubSnapshot, map[string]hivectl.TreeRepositorySnapshot, commonsTreeDiscovery) {
	disc := commonsTreeDiscovery{
		Hubs:          map[string]commonsTreeDiscoveryState{},
		Subscriptions: map[string]commonsTreeDiscoveryState{},
		Available:     s.deps.discoverHubHives != nil && s.deps.discoverRepositories != nil,
	}
	hubs := make([]hivectl.TreeHubSnapshot, 0, len(set.Hubs))
	repos := make(map[string]hivectl.TreeRepositorySnapshot, len(set.Profiles))
	var mu sync.Mutex
	var wg sync.WaitGroup
	// Snapshot every hub before any goroutine starts: the slice header and
	// the discovery maps are then only written concurrently under mu.
	hubIDs := make([]string, 0, len(set.Hubs))
	for _, h := range set.Hubs {
		id, err := hivectl.NormalizeHiveID(h.URL)
		if err != nil {
			// An unusable saved hub URL cannot be keyed by origin; report it by
			// name and leave it out of the tree rather than failing the page.
			disc.Hubs[h.Name] = commonsTreeDiscoveryState{Status: commonsDiscoveryUnavailable, Reason: commonsDiscoveryReasonHubURL}
			continue
		}
		hubs = append(hubs, hivectl.TreeHubSnapshot{Name: h.Name, URL: h.URL, Follow: h.EffectiveFollow(), Discovery: hivectl.TreeUnavailable})
		hubIDs = append(hubIDs, id)
	}
	if s.deps.discoverHubHives == nil {
		for _, id := range hubIDs {
			disc.Hubs[id] = commonsTreeDiscoveryState{Status: commonsDiscoveryUnavailable, Reason: commonsDiscoveryReasonMissing}
		}
	}
	if s.deps.discoverRepositories == nil {
		for _, p := range set.Profiles {
			repos[p.Name] = hivectl.TreeRepositorySnapshot{Discovery: hivectl.TreeUnavailable}
			disc.Subscriptions[p.Name] = commonsTreeDiscoveryState{Status: commonsDiscoveryUnavailable, Reason: commonsDiscoveryReasonMissing}
		}
	}
	for i := range hubs {
		if s.deps.discoverHubHives == nil {
			break
		}
		wg.Add(1)
		go func(i int, id, hubURL string) {
			defer wg.Done()
			hives, err := s.deps.discoverHubHives(ctx, hubURL)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				disc.Hubs[id] = commonsTreeDiscoveryState{Status: commonsDiscoveryUnavailable, Reason: commonsDiscoveryReason(err)}
				return
			}
			hubs[i].Discovery = hivectl.TreeComplete
			for _, hive := range hives {
				hubs[i].HiveOrigins = append(hubs[i].HiveOrigins, hive.HubURL)
			}
			disc.Hubs[id] = commonsTreeDiscoveryState{Status: commonsDiscoveryComplete}
		}(i, hubIDs[i], hubs[i].URL)
	}
	for _, p := range set.Profiles {
		if s.deps.discoverRepositories == nil {
			break
		}
		wg.Add(1)
		go func(name, hub string) {
			defer wg.Done()
			list, err := s.deps.discoverRepositories(ctx, hub)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				repos[name] = hivectl.TreeRepositorySnapshot{Discovery: hivectl.TreeUnavailable}
				disc.Subscriptions[name] = commonsTreeDiscoveryState{Status: commonsDiscoveryUnavailable, Reason: commonsDiscoveryReason(err)}
				return
			}
			repos[name] = hivectl.TreeRepositorySnapshot{Discovery: hivectl.TreeComplete, Repositories: list}
			disc.Subscriptions[name] = commonsTreeDiscoveryState{Status: commonsDiscoveryComplete}
		}(p.Name, p.Hub)
	}
	wg.Wait()
	return hubs, repos, disc
}

// commonsDiscoveryReason keeps only a coarse cause. Transport errors embed the
// URL (which may carry a query string), so they are summarized, not forwarded.
func commonsDiscoveryReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "discovery timed out"
	case errors.Is(err, context.Canceled):
		return "discovery cancelled"
	case errors.Is(err, hivectl.ErrHubUnavailable):
		return "hub registry unavailable"
	}
	msg := err.Error()
	if i := strings.Index(msg, ";"); i > 0 {
		msg = msg[:i]
	}
	if strings.Contains(msg, "://") || strings.Contains(msg, "?") {
		return "discovery unavailable"
	}
	return msg
}

// openCommonsTree builds a fresh session from the saved set. The caller holds
// s.mu. Discovery errors are recorded per branch and never fail the open.
func (s *commonsWebServer) openCommonsTree(ctx context.Context) (*commonsTreeSession, error) {
	set, err := s.loadSet(true)
	if err != nil {
		return nil, err
	}
	hubs, repos, disc := s.discoverCommonsTree(ctx, set)
	edits, err := hivectl.NewStagedEdits(set, hubs, repos)
	if err != nil {
		return nil, err
	}
	edits.SetRelaySignal(s.deps.signalRelay)
	now := time.Now
	if s.deps.now != nil {
		now = s.deps.now
	}
	session := &commonsTreeSession{edits: edits, discovery: disc, openedAt: now().UTC()}
	s.tree = session
	return session, nil
}

// currentCommonsTree returns the open session, opening one when needed. A
// session whose base revision no longer matches the file is replaced only when
// it has no staged edits; otherwise the stale diff is reported so the
// contributor can review again rather than lose or silently merge it.
func (s *commonsWebServer) currentCommonsTree(ctx context.Context, refresh bool) (*commonsTreeSession, error) {
	if s.tree != nil {
		pending := s.tree.edits.Pending()
		if refresh && pending {
			return s.tree, errCommonsTreePending
		}
		if refresh {
			return s.openCommonsTree(ctx)
		}
		set, err := s.loadSet(true)
		if err != nil {
			return nil, err
		}
		if set.Revision() == s.tree.edits.Revision() || pending {
			return s.tree, nil
		}
	}
	return s.openCommonsTree(ctx)
}

func (s *commonsWebServer) commonsTreeResponse(session *commonsTreeSession) (commonsTreeResponse, error) {
	tree, err := session.edits.Tree()
	if err != nil {
		return commonsTreeResponse{}, err
	}
	diff := session.edits.Diff()
	// Load, not LoadOrMigrate: the overlay compares against saved state and
	// must not touch legacy files. A read failure leaves the saved scope
	// version unknown, which the live label reports.
	saved, _ := s.deps.store.Load()
	runtime := s.deps.treeRuntime(saved)
	tree.ApplyRuntime(runtime)
	return commonsTreeResponse{
		Tree:      tree,
		Diff:      diff,
		Revision:  session.edits.Revision(),
		Staged:    !diff.Empty(),
		Discovery: session.discovery,
		OpenedAt:  session.openedAt,
		Runtime:   runtime,
	}, nil
}

func (s *commonsWebServer) writeCommonsTree(w http.ResponseWriter, session *commonsTreeSession) {
	resp, err := s.commonsTreeResponse(session)
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	writeCommonsJSON(w, resp)
}

func (s *commonsWebServer) handleTree(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	refresh := r.URL.Query().Get("refresh") != ""
	session, err := s.currentCommonsTree(r.Context(), refresh)
	if err != nil {
		writeCommonsTreeError(w, err)
		return
	}
	s.writeCommonsTree(w, session)
}

// commonsTreeEditRequest is one staged operation. Ops map one-to-one onto
// StagedEdits methods; the page cannot compose a mutation the model lacks.
type commonsTreeEditRequest struct {
	Op           string `json:"op"`
	Hub          string `json:"hub,omitempty"`
	Subscription string `json:"subscription,omitempty"`
	Repository   string `json:"repository,omitempty"`
	Value        bool   `json:"value"`
}

const (
	commonsTreeOpSelect         = "select"
	commonsTreeOpExclude        = "exclude"
	commonsTreeOpFollow         = "follow"
	commonsTreeOpEnable         = "enable"
	commonsTreeOpSelectCurrent  = "select-current"
	commonsTreeOpHubEnable      = "hub-enable"
	commonsTreeOpReviewRestrict = "review-restrict"
)

var errCommonsTreeBadOp = errors.New("unknown tree operation")

func applyCommonsTreeEdit(edits *hivectl.StagedEdits, req commonsTreeEditRequest) error {
	switch req.Op {
	case commonsTreeOpSelect:
		return edits.SelectRepository(req.Subscription, req.Repository, req.Value)
	case commonsTreeOpExclude:
		return edits.SetExcluded(req.Subscription, req.Repository, req.Value)
	case commonsTreeOpFollow:
		return edits.SetFollow(req.Subscription, req.Value)
	case commonsTreeOpEnable:
		return edits.SetHiveEnabled(req.Subscription, req.Value)
	case commonsTreeOpSelectCurrent:
		if req.Hub != "" {
			return edits.SelectCurrent(req.Hub, true, req.Value)
		}
		return edits.SelectCurrent(req.Subscription, false, req.Value)
	case commonsTreeOpHubEnable:
		return edits.SetHubEnabled(req.Hub, req.Value)
	case commonsTreeOpReviewRestrict:
		return edits.ReviewAndRestrict(req.Subscription)
	}
	return errCommonsTreeBadOp
}

func (s *commonsWebServer) handleTreeEdit(w http.ResponseWriter, r *http.Request) {
	var req commonsTreeEditRequest
	if !decodeCommonsJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.currentCommonsTree(r.Context(), false)
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	if !checkCommonsTreeReviewed(w, r, session) {
		return
	}
	if err := applyCommonsTreeEdit(session.edits, req); err != nil {
		writeCommonsTreeError(w, err)
		return
	}
	s.writeCommonsTree(w, session)
}

// checkCommonsTreeReviewed refuses an edit or Apply from a page whose session
// revision differs from the server's: another tab or writer reopened the
// session, so the staged diff on that page is stale and needs renewed review.
func checkCommonsTreeReviewed(w http.ResponseWriter, r *http.Request, session *commonsTreeSession) bool {
	reviewed := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if reviewed == "" || reviewed == session.edits.Revision() {
		return true
	}
	writeCommonsError(w, hivectl.ErrStaleRevision)
	return false
}

// commonsTreeApplyResponse pairs the truthful receipt with the refreshed tree.
// Outcome is one of ok, stale, save-failed, projection-failed, reload-failed.
type commonsTreeApplyResponse struct {
	Receipt hivectl.ApplyReceipt `json:"receipt"`
	Outcome string               `json:"outcome"`
	Message string               `json:"message,omitempty"`
	Tree    *commonsTreeResponse `json:"tree,omitempty"`
}

const (
	commonsApplyOK               = "ok"
	commonsApplyStale            = "stale"
	commonsApplySaveFailed       = "save-failed"
	commonsApplyProjectionFailed = "projection-failed"
	commonsApplyReloadFailed     = "reload-failed"
)

func (s *commonsWebServer) handleTreeApply(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tree == nil {
		writeCommonsError(w, errors.New("no staged contribution edits to apply"))
		return
	}
	session := s.tree
	if !checkCommonsTreeReviewed(w, r, session) {
		return
	}
	receipt, err := session.edits.Apply(r.Context(), s.deps.store)
	resp := commonsTreeApplyResponse{Receipt: receipt, Outcome: commonsApplyOK}
	status := http.StatusOK
	switch {
	case err == nil:
	case errors.Is(err, hivectl.ErrStaleRevision):
		resp.Outcome, status = commonsApplyStale, http.StatusConflict
		resp.Message = "profiles changed since this review; nothing was saved — refresh and review again"
	case errors.Is(err, hivectl.ErrStageProjection):
		resp.Outcome, status = commonsApplyProjectionFailed, http.StatusInternalServerError
		resp.Message = "profiles.yml was saved but contributor.env was not regenerated; retry Apply before further edits"
	case errors.Is(err, hivectl.ErrStageReload):
		resp.Outcome = commonsApplyReloadFailed
		resp.Message = "saved; the running relay could not be signaled, so the change governs its next start"
	default:
		resp.Outcome, status = commonsApplySaveFailed, http.StatusInternalServerError
		resp.Message = "profiles were not saved; retry Apply"
	}
	if tree, terr := s.commonsTreeResponse(session); terr == nil {
		resp.Tree = &tree
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	writeCommonsJSON(w, resp)
}

func (s *commonsWebServer) handleTreeDiscard(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.currentCommonsTree(r.Context(), false)
	if err != nil {
		writeCommonsError(w, err)
		return
	}
	if err := session.edits.Discard(); err != nil {
		writeCommonsTreeError(w, err)
		return
	}
	s.writeCommonsTree(w, session)
}

// writeCommonsTreeError maps staged-edit refusals to statuses the page can
// explain: 409 for anything that needs renewed review or a prior step.
func writeCommonsTreeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hivectl.ErrStageIncomplete), errors.Is(err, hivectl.ErrStageLegacy), errors.Is(err, hivectl.ErrStageProjection), errors.Is(err, errCommonsTreePending):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errCommonsTreeBadOp):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, hivectl.ErrHubNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		writeCommonsError(w, err)
	}
}
