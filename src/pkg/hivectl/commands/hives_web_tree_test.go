package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
)

const (
	treeRepoA    = "github/github.com/org/a"
	treeRepoB    = "github/github.com/org/b"
	treeHubOne   = "https://one.example"
	treeHubTwo   = "https://two.example"
	treeOrgAlpha = "https://alpha.example"
	treeOrgBeta  = "https://beta.example"
)

// treeHarness wires discovery stubs whose behaviour a test can flip between
// calls, plus counters so refresh semantics are observable.
type treeHarness struct {
	server      *httptest.Server
	store       *hivectl.ProfileStore
	signals     *atomic.Int64
	hubCalls    *atomic.Int64
	repoCalls   *atomic.Int64
	hubErr      atomic.Pointer[error]
	repoErr     atomic.Pointer[error]
	signalErr   atomic.Pointer[error]
	relayFound  atomic.Bool
	deps        *hivesDeps
	tokenHeader string
}

func treeFixtureSet() *hivectl.ProfileSet {
	return &hivectl.ProfileSet{Version: hivectl.ProfilesVersion, Active: "alpha", Profiles: []hivectl.Profile{
		{Name: "alpha", Hub: "wss://alpha.example/contribute?token=endpoint-secret", RegistrationToken: "registration-secret", Selection: &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected}},
		{Name: "beta", Hub: "wss://beta.example/contribute", RegistrationToken: "other-secret", Selection: &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected}},
	}, Hubs: []hivectl.HubEntry{
		{Name: "one", URL: treeHubOne, Follow: hivectl.HubFollowCurrent},
		{Name: "two", URL: treeHubTwo},
	}}
}

func newTreeHarness(t *testing.T, set *hivectl.ProfileSet) *treeHarness {
	t.Helper()
	h := &treeHarness{store: hivectl.NewProfileStore(t.TempDir()), signals: &atomic.Int64{}, hubCalls: &atomic.Int64{}, repoCalls: &atomic.Int64{}, tokenHeader: "secret"}
	h.relayFound.Store(true)
	if set != nil {
		if err := h.store.Save(set); err != nil {
			t.Fatalf("seed profiles: %v", err)
		}
	}
	h.deps = &hivesDeps{
		store: h.store,
		signalRelay: func(context.Context, *hivectl.ProfileStore) (hivectl.RelaySwitchResult, error) {
			h.signals.Add(1)
			if p := h.signalErr.Load(); p != nil {
				return hivectl.RelaySwitchResult{}, *p
			}
			return hivectl.RelaySwitchResult{Running: h.relayFound.Load(), Target: "pid 123"}, nil
		},
		now: func() time.Time { return time.Date(2026, 10, 21, 5, 0, 0, 0, time.UTC) },
		discoverHubHives: func(_ context.Context, hubURL string) ([]hivectl.HubHive, error) {
			h.hubCalls.Add(1)
			if p := h.hubErr.Load(); p != nil {
				return nil, *p
			}
			if hubURL == treeHubOne {
				return []hivectl.HubHive{{ID: treeOrgAlpha, HubURL: treeOrgAlpha}, {ID: treeOrgBeta, HubURL: treeOrgBeta}}, nil
			}
			return []hivectl.HubHive{{ID: treeOrgAlpha, HubURL: treeOrgAlpha}}, nil
		},
		discoverRepositories: func(_ context.Context, hub string) ([]string, error) {
			h.repoCalls.Add(1)
			if p := h.repoErr.Load(); p != nil && strings.Contains(hub, "beta") {
				return nil, *p
			}
			return []string{treeRepoA, treeRepoB}, nil
		},
	}
	h.server = httptest.NewServer(newCommonsWebServer(h.deps, "secret"))
	t.Cleanup(h.server.Close)
	return h
}

func (h *treeHarness) do(t *testing.T, method, path string, body any, ifMatch string) (int, []byte) {
	t.Helper()
	req := commonsReq(t, h.server, method, path, body)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (h *treeHarness) tree(t *testing.T, method, path string, body any) commonsTreeResponse {
	t.Helper()
	status, raw := h.do(t, method, path, body, "")
	if status != http.StatusOK {
		t.Fatalf("%s %s status = %d: %s", method, path, status, raw)
	}
	var out commonsTreeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode tree: %v", err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("tree response leaked a secret: %s", raw)
	}
	return out
}

func (h *treeHarness) edit(t *testing.T, req commonsTreeEditRequest) commonsTreeResponse {
	t.Helper()
	return h.tree(t, http.MethodPost, "/api/tree/edit", req)
}

func (h *treeHarness) apply(t *testing.T, ifMatch string) (int, commonsTreeApplyResponse) {
	t.Helper()
	status, raw := h.do(t, http.MethodPost, "/api/tree/apply", map[string]any{}, ifMatch)
	var out commonsTreeApplyResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode apply (%d): %s", status, raw)
	}
	return status, out
}

func treeSub(t *testing.T, tree commonsTreeResponse, id string) hivectl.TreeSubscription {
	t.Helper()
	for _, s := range tree.Tree.Subscriptions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("subscription %q missing from tree", id)
	return hivectl.TreeSubscription{}
}

func TestCommonsTreeRendersSharedBranchesAndRuntimeUnavailable(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	tree := h.tree(t, http.MethodGet, "/api/tree", nil)
	if tree.Staged || tree.Revision == "" {
		t.Fatalf("fresh session: staged=%v revision=%q", tree.Staged, tree.Revision)
	}
	if len(tree.Tree.Hubs) != 2 {
		t.Fatalf("hubs = %+v", tree.Tree.Hubs)
	}
	alpha := treeSub(t, tree, "alpha")
	if alpha.SharedAcrossHubs != 2 || alpha.HiveID != treeOrgAlpha {
		t.Fatalf("alpha = %+v", alpha)
	}
	if alpha.Selection.State != "off" || alpha.Selection.Repositories != 2 || alpha.Selection.MembershipIncomplete {
		t.Fatalf("alpha selection = %+v", alpha.Selection)
	}
	if alpha.Readiness != "unknown" || alpha.Activity != "unavailable" {
		t.Fatalf("runtime columns must be unavailable, got %q/%q", alpha.Readiness, alpha.Activity)
	}
	if tree.Runtime.Live.Available || tree.Runtime.Live.Reason == "" || tree.Runtime.Durable.Available || tree.Runtime.Durable.Reason != hivectl.DurableActivityNotShipped {
		t.Fatalf("runtime = %+v", tree.Runtime)
	}
	if !strings.HasPrefix(tree.Runtime.Live.Summary, "unavailable — ") || !strings.HasPrefix(tree.Runtime.Durable.Summary, "unavailable — ") {
		t.Fatalf("runtime summaries = %q / %q", tree.Runtime.Live.Summary, tree.Runtime.Durable.Summary)
	}
	if alpha.History != "unavailable" {
		t.Fatalf("history = %q", alpha.History)
	}
	if !tree.Discovery.Available || tree.Discovery.Hubs[treeHubOne].Status != commonsDiscoveryComplete || tree.Discovery.Subscriptions["beta"].Status != commonsDiscoveryComplete {
		t.Fatalf("discovery = %+v", tree.Discovery)
	}
	if !tree.OpenedAt.Equal(time.Date(2026, 10, 21, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("opened_at = %v", tree.OpenedAt)
	}
	// A second GET without refresh reuses the session: no new discovery.
	calls := h.hubCalls.Load()
	h.tree(t, http.MethodGet, "/api/tree", nil)
	if h.hubCalls.Load() != calls {
		t.Fatalf("GET without refresh re-ran discovery")
	}
}

func TestCommonsTreeStagesWithoutWritingAndDiscards(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	before, _ := os.ReadFile(h.store.Path())
	tree := h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "alpha", Repository: treeRepoA, Value: true})
	if !tree.Staged || tree.Diff.SelectedCount != 1 || tree.Diff.Selected[0].Repository != treeRepoA {
		t.Fatalf("diff = %+v", tree.Diff)
	}
	if treeSub(t, tree, "alpha").Selection.State != "partial" {
		t.Fatalf("alpha should be partial after one of two selected")
	}
	after, _ := os.ReadFile(h.store.Path())
	if string(before) != string(after) || h.signals.Load() != 0 {
		t.Fatalf("staging must not write profiles.yml or signal the relay")
	}
	tree = h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpExclude, Subscription: "beta", Repository: treeRepoB, Value: true})
	if tree.Diff.ExcludedCount != 1 || len(tree.Diff.Exclusions) != 1 || tree.Diff.Exclusions[0].Subscription != "beta" {
		t.Fatalf("exclusion diff = %+v", tree.Diff.Exclusions)
	}
	tree = h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpFollow, Subscription: "alpha", Value: true})
	if len(tree.Diff.Follow) != 1 || !tree.Diff.Follow[0].After {
		t.Fatalf("follow diff = %+v", tree.Diff.Follow)
	}
	tree = h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpEnable, Subscription: "beta", Value: false})
	if len(tree.Diff.Enabled) != 1 || tree.Diff.Enabled[0].After {
		t.Fatalf("enabled diff = %+v", tree.Diff.Enabled)
	}
	tree = h.tree(t, http.MethodPost, "/api/tree/discard", map[string]any{})
	if tree.Staged || !tree.Diff.Empty() {
		t.Fatalf("discard left staged edits: %+v", tree.Diff)
	}
}

func TestCommonsTreeBranchActionsAndHubEnable(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	tree := h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpSelectCurrent, Hub: treeHubOne, Value: true})
	if tree.Diff.SelectedCount != 4 || len(tree.Diff.SharedBranches) != 1 || tree.Diff.SharedBranches[0].Subscription != "alpha" {
		t.Fatalf("hub select-current diff = %+v", tree.Diff)
	}
	for _, id := range []string{"alpha", "beta"} {
		if treeSub(t, tree, id).Selection.State != "all-current" {
			t.Fatalf("%s should be all-current", id)
		}
	}
	// Deselect current on one Hive retains its follow policy.
	h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpFollow, Subscription: "alpha", Value: true})
	tree = h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpSelectCurrent, Subscription: "alpha", Value: false})
	alpha := treeSub(t, tree, "alpha")
	if alpha.Selection.Selected != 0 || !alpha.Follow {
		t.Fatalf("deselect current must retain follow: %+v", alpha)
	}
	tree = h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpHubEnable, Hub: treeHubOne, Value: false})
	if len(tree.Diff.Enabled) != 2 || len(tree.Diff.HubFollow) != 1 || tree.Diff.HubFollow[0].After != hivectl.HubFollowOff {
		t.Fatalf("hub disable diff = %+v", tree.Diff)
	}
	if !treeSub(t, tree, "beta").Disabled || treeSub(t, tree, "beta").Selection.Selected != 2 {
		t.Fatalf("disable must retain saved selections: %+v", treeSub(t, tree, "beta"))
	}
}

func TestCommonsTreeApplyReceiptsAndRelayOutcomes(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	tree := h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "alpha", Repository: treeRepoA, Value: true})
	status, out := h.apply(t, tree.Revision)
	if status != http.StatusOK || out.Outcome != commonsApplyOK {
		t.Fatalf("apply = %d %+v", status, out)
	}
	if out.Receipt.Saved != hivectl.StageSaved || out.Receipt.RelayReload != hivectl.StageReloadNotified || out.Receipt.Acknowledged != "pending" || out.Receipt.ScopeVersion == 0 {
		t.Fatalf("receipt = %+v", out.Receipt)
	}
	if out.Tree == nil || out.Tree.Staged || out.Tree.Revision == tree.Revision {
		t.Fatalf("post-apply tree = %+v", out.Tree)
	}
	saved, err := h.store.Load()
	if err != nil || !treeSavedSelected(saved, "alpha", treeRepoA) {
		t.Fatalf("apply did not persist selection: %v %+v", err, saved)
	}
	if h.signals.Load() != 1 {
		t.Fatalf("signals = %d", h.signals.Load())
	}

	// Nothing staged: Apply is a truthful no-op.
	status, out = h.apply(t, "")
	if status != http.StatusOK || out.Receipt.Saved != hivectl.StageUnchanged {
		t.Fatalf("empty apply = %d %+v", status, out)
	}

	// No managed relay: saved for next start.
	h.relayFound.Store(false)
	h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "alpha", Repository: treeRepoB, Value: true})
	_, out = h.apply(t, "")
	if out.Receipt.RelayReload != hivectl.StageReloadNoManaged {
		t.Fatalf("receipt without relay = %+v", out.Receipt)
	}

	// Signal failure: saved, reload pending/failed, still HTTP 200.
	sigErr := errors.New("kill: no such process")
	h.signalErr.Store(&sigErr)
	h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "beta", Repository: treeRepoA, Value: true})
	status, out = h.apply(t, "")
	if status != http.StatusOK || out.Outcome != commonsApplyReloadFailed || out.Receipt.Saved != hivectl.StageSaved || out.Receipt.RelayReload != hivectl.StageReloadFailed {
		t.Fatalf("reload failure = %d %+v", status, out)
	}
	if strings.Contains(out.Message, "kill") {
		t.Fatalf("raw signal error leaked: %q", out.Message)
	}
}

func treeSavedSelected(set *hivectl.ProfileSet, name, repo string) bool {
	p, _ := set.Find(name)
	if p == nil || p.Selection == nil {
		return false
	}
	effective, _ := p.Selection.EffectiveRepositories()
	for _, id := range effective {
		if id == repo {
			return true
		}
	}
	return false
}

func TestCommonsTreeRejectsStaleReviewAndConcurrentWriters(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	tree := h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "alpha", Repository: treeRepoA, Value: true})

	// A page that reviewed another revision cannot edit or apply.
	status, raw := h.do(t, http.MethodPost, "/api/tree/edit", commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "alpha", Repository: treeRepoB, Value: true}, "other-revision")
	if status != http.StatusConflict || !strings.Contains(string(raw), "renewed review") {
		t.Fatalf("stale If-Match edit = %d %s", status, raw)
	}
	if status, raw := h.do(t, http.MethodPost, "/api/tree/apply", map[string]any{}, "other-revision"); status != http.StatusConflict || !strings.Contains(string(raw), "renewed review") {
		t.Fatalf("stale If-Match apply = %d %s", status, raw)
	}

	// The CLI (another writer) commits meanwhile: the staged diff is rejected.
	set, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := set.SetDisabled("beta", true); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CommitExpecting(set, set.Revision()); err != nil {
		t.Fatal(err)
	}
	status, out := h.apply(t, tree.Revision)
	if status != http.StatusConflict || out.Outcome != commonsApplyStale || out.Receipt.Saved != hivectl.StageNotSaved {
		t.Fatalf("concurrent write apply = %d %+v", status, out)
	}
	if !treeSavedSelected(set, "alpha", treeRepoA) == false {
		t.Fatalf("stale diff must not be merged")
	}
	// Edits on the stale session are refused until it is refreshed.
	status, raw = h.do(t, http.MethodPost, "/api/tree/edit", commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "alpha", Repository: treeRepoB, Value: true}, "")
	if status != http.StatusConflict {
		t.Fatalf("edit after stale apply = %d %s", status, raw)
	}
	// Pending (now stale) edits block a plain refresh; discard clears the way.
	status, raw = h.do(t, http.MethodGet, "/api/tree?refresh=1", nil, "")
	if status != http.StatusConflict || !strings.Contains(string(raw), "pending") {
		t.Fatalf("refresh with pending edits = %d %s", status, raw)
	}
	h.tree(t, http.MethodPost, "/api/tree/discard", map[string]any{})
	calls := h.hubCalls.Load()
	fresh := h.tree(t, http.MethodGet, "/api/tree?refresh=1", nil)
	if h.hubCalls.Load() == calls || fresh.Revision == tree.Revision || !treeSub(t, fresh, "beta").Disabled {
		t.Fatalf("refresh did not reopen against the current file: %+v", fresh)
	}
}

func TestCommonsTreeReopensAfterExternalEditWhenNothingStaged(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	tree := h.tree(t, http.MethodGet, "/api/tree", nil)
	set, _ := h.store.Load()
	_ = set.SetDisabled("alpha", true)
	if err := h.store.CommitExpecting(set, set.Revision()); err != nil {
		t.Fatal(err)
	}
	next := h.tree(t, http.MethodGet, "/api/tree", nil)
	if next.Revision == tree.Revision || !treeSub(t, next, "alpha").Disabled {
		t.Fatalf("session should follow the file when nothing is staged: %+v", next)
	}
}

func TestCommonsTreeDiscoveryOutageIsNotEmptyOrWildcard(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	hubErr := &hivectl.HubUnavailableError{Hub: treeHubOne + "/?token=hub-secret", Reason: "GET /api/hives returned HTTP 503"}
	var asErr error = hubErr
	h.hubErr.Store(&asErr)
	repoErr := errors.New("repository discovery unavailable; selection unchanged: Get \"https://beta.example/api/contribute/repositories?x=secret\": dial tcp: refused")
	h.repoErr.Store(&repoErr)
	tree := h.tree(t, http.MethodGet, "/api/tree", nil)
	if got := tree.Discovery.Hubs[treeHubOne]; got.Status != commonsDiscoveryUnavailable || got.Reason != "hub registry unavailable" {
		t.Fatalf("hub discovery state = %+v", got)
	}
	if got := tree.Discovery.Subscriptions["beta"]; got.Status != commonsDiscoveryUnavailable || strings.Contains(got.Reason, "secret") || got.Reason == "" {
		t.Fatalf("repo discovery state = %+v", got)
	}
	beta := treeSub(t, tree, "beta")
	if !beta.Selection.MembershipIncomplete || beta.Discovery != hivectl.TreeUnavailable {
		t.Fatalf("beta must read membership incomplete: %+v", beta.Selection)
	}
	// Both hubs failed, so saved subscriptions fall under Direct Hives with the
	// hubs still listed as unavailable branches — never dropped.
	var direct bool
	for _, hub := range tree.Tree.Hubs {
		if hub.Direct {
			direct = true
		}
		if !hub.Direct && hub.Discovery != hivectl.TreeUnavailable {
			t.Fatalf("hub %s discovery = %q", hub.ID, hub.Discovery)
		}
	}
	if !direct {
		t.Fatalf("hubs = %+v", tree.Tree.Hubs)
	}
	// Bulk inclusion is refused on incomplete membership; deselection and
	// unrelated branches stay editable.
	status, raw := h.do(t, http.MethodPost, "/api/tree/edit", commonsTreeEditRequest{Op: commonsTreeOpSelectCurrent, Subscription: "beta", Value: true}, "")
	if status != http.StatusConflict || !strings.Contains(string(raw), "incomplete") {
		t.Fatalf("bulk include on outage = %d %s", status, raw)
	}
	h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpSelectCurrent, Subscription: "alpha", Value: true})
	h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "beta", Repository: treeRepoA, Value: true})
}

func TestCommonsTreeLegacyProfileNeedsReviewAndRestrict(t *testing.T) {
	set := treeFixtureSet()
	set.Profiles[1].Selection = nil
	h := newTreeHarness(t, set)
	tree := h.tree(t, http.MethodGet, "/api/tree", nil)
	beta := treeSub(t, tree, "beta")
	if !beta.Selection.Legacy || beta.Selection.State != "off" {
		t.Fatalf("legacy profile = %+v", beta.Selection)
	}
	status, raw := h.do(t, http.MethodPost, "/api/tree/edit", commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "beta", Repository: treeRepoA, Value: true}, "")
	if status != http.StatusConflict || !strings.Contains(string(raw), "review and restrict") {
		t.Fatalf("legacy select = %d %s", status, raw)
	}
	// Parent bulk action may not convert it either.
	if status, _ := h.do(t, http.MethodPost, "/api/tree/edit", commonsTreeEditRequest{Op: commonsTreeOpSelectCurrent, Hub: treeHubOne, Value: true}, ""); status != http.StatusConflict {
		t.Fatalf("hub bulk include over legacy = %d", status)
	}
	// Disabling stays possible without migration.
	tree = h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpEnable, Subscription: "beta", Value: false})
	if len(tree.Diff.LegacyConversions) != 0 || !treeSub(t, tree, "beta").Selection.Legacy {
		t.Fatalf("disable migrated legacy profile: %+v", tree.Diff)
	}
	h.tree(t, http.MethodPost, "/api/tree/discard", map[string]any{})

	// Failed discovery leaves it unchanged.
	repoErr := errors.New("discovery timed out")
	h.repoErr.Store(&repoErr)
	h.tree(t, http.MethodGet, "/api/tree?refresh=1", nil)
	if status, _ := h.do(t, http.MethodPost, "/api/tree/edit", commonsTreeEditRequest{Op: commonsTreeOpReviewRestrict, Subscription: "beta"}, ""); status != http.StatusConflict {
		t.Fatalf("review-restrict without discovery = %d", status)
	}
	h.repoErr.Store(nil)
	h.tree(t, http.MethodGet, "/api/tree?refresh=1", nil)
	tree = h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpReviewRestrict, Subscription: "beta"})
	if len(tree.Diff.LegacyConversions) != 1 || tree.Diff.SelectedCount != 2 || treeSub(t, tree, "beta").Selection.State != "all-current" || treeSub(t, tree, "beta").Follow {
		t.Fatalf("review-restrict diff = %+v", tree.Diff)
	}
	if status, out := h.apply(t, tree.Revision); status != http.StatusOK || out.Outcome != commonsApplyOK {
		t.Fatalf("apply conversion = %d %+v", status, out)
	}
	saved, _ := h.store.Load()
	if p, _ := saved.Find("beta"); p.Selection == nil || p.Selection.Mode != hivectl.SelectionAllCurrent {
		t.Fatalf("conversion not persisted: %+v", p)
	}
}

func TestCommonsTreeWithoutDiscoverySourcesAndBadRequests(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.deps.discoverHubHives = nil
	h.deps.discoverRepositories = nil
	tree := h.tree(t, http.MethodGet, "/api/tree", nil)
	if tree.Discovery.Available || tree.Discovery.Hubs[treeHubOne].Reason != commonsDiscoveryReasonMissing || tree.Discovery.Subscriptions["alpha"].Reason != commonsDiscoveryReasonMissing {
		t.Fatalf("discovery = %+v", tree.Discovery)
	}
	if !treeSub(t, tree, "alpha").Selection.MembershipIncomplete {
		t.Fatalf("missing discovery must read incomplete")
	}
	if status, _ := h.do(t, http.MethodPost, "/api/tree/edit", commonsTreeEditRequest{Op: "bogus"}, ""); status != http.StatusBadRequest {
		t.Fatalf("unknown op = %d", status)
	}
	if status, _ := h.do(t, http.MethodPost, "/api/tree/edit", map[string]any{"op": "select", "nope": 1}, ""); status != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", status)
	}
	if status, _ := h.do(t, http.MethodPost, "/api/tree/edit", commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "ghost", Repository: treeRepoA}, ""); status != http.StatusNotFound {
		t.Fatalf("unknown subscription = %d", status)
	}
	if status, _ := h.do(t, http.MethodPost, "/api/tree/edit", commonsTreeEditRequest{Op: commonsTreeOpHubEnable, Hub: "https://nope.example"}, ""); status != http.StatusNotFound {
		t.Fatalf("unknown hub = %d", status)
	}
	// Unauthenticated and cross-origin callers never reach the tree.
	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/api/tree", nil)
	if resp, err := h.server.Client().Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %v %v", err, resp)
	}
}

func TestCommonsTreeApplyWithoutSessionAndEmptyStore(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	if status, raw := h.do(t, http.MethodPost, "/api/tree/apply", map[string]any{}, ""); status != http.StatusBadRequest || !strings.Contains(string(raw), "no staged") {
		t.Fatalf("apply without session = %d %s", status, raw)
	}
	empty := newTreeHarness(t, nil)
	tree := empty.tree(t, http.MethodGet, "/api/tree", nil)
	if len(tree.Tree.Hubs) != 0 || len(tree.Tree.Subscriptions) != 0 || tree.Tree.Selection.State != "off" {
		t.Fatalf("empty store tree = %+v", tree.Tree)
	}
}

func TestCommonsTreePageMarkupIsAccessibleAndStyleFree(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	resp, err := http.Get(h.server.URL + "/?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	page := string(raw)
	for _, want := range []string{`role="tree"`, `aria-live="polite"`, `id="apply"`, `id="discard"`, `name="tree-filter"`, `id="tree-search"`, `id="tree-live"`, `id="tree-durable"`, "Review and restrict", "Deselect current", "relay adoption pending", "'History: '"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, " style=") {
		t.Errorf("page uses inline style attributes")
	}
	if strings.Contains(page, "http://") && strings.Contains(page, "<link") || strings.Contains(page, "<script src") {
		t.Errorf("page loads third-party assets")
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers = %v", resp.Header)
	}
}

func TestCommonsTreeRuntimeOverlayFollowsSavedState(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	var sawSaved atomic.Int64
	h.deps.liveRuntime = func(saved *hivectl.ProfileSet) hivectl.TreeLive {
		if saved != nil {
			sawSaved.Store(int64(saved.RepoScopeVersion))
		}
		return hivectl.TreeLive{Available: true, PID: 9, AdoptedScopeVersion: 1, Acknowledged: "pending", Summary: "live summary",
			CurrentTask:   &hivectl.RelayCurrentTask{Hive: treeOrgAlpha, Repository: treeRepoA},
			Subscriptions: map[string]hivectl.TreeLiveSubscription{treeOrgAlpha: {Readiness: "scope-pending", Acknowledged: "pending"}}}
	}
	h.deps.durableActivity = func(*hivectl.ProfileStore) (hivectl.TreeDurable, error) {
		return hivectl.TreeDurable{Available: true, Attempts: 2, Completed: 1, Repositories: map[string]hivectl.TreeDurableRepository{treeRepoA: {Attempts: 2, Completed: 1}}}, nil
	}
	tree := h.tree(t, http.MethodGet, "/api/tree", nil)
	if !tree.Runtime.Live.Available || tree.Runtime.Live.Summary != "live summary" || tree.Runtime.Durable.Summary != "2 attempt(s) · 1 completed" {
		t.Fatalf("runtime = %+v", tree.Runtime)
	}
	alpha := treeSub(t, tree, "alpha")
	if alpha.Readiness != "scope-pending" || alpha.Activity != "working on "+treeRepoA || alpha.History != "2 attempt(s) · 1 completed" {
		t.Fatalf("alpha overlay = %+v", alpha)
	}
	beta := treeSub(t, tree, "beta")
	if beta.Readiness != "not adopted by relay" || beta.Activity != "idle" || beta.History != "2 attempt(s) · 1 completed" {
		t.Fatalf("beta overlay = %+v", beta)
	}
	for _, r := range alpha.Repositories {
		switch r.ID {
		case treeRepoA:
			if r.Activity != "in progress" || r.History != "2 attempt(s) · 1 completed" {
				t.Fatalf("repo A = %+v", r)
			}
		case treeRepoB:
			if r.Activity != "idle" || r.History != "no recorded activity" {
				t.Fatalf("repo B = %+v", r)
			}
		}
	}
	// Staging does not change what the overlay compares against: the saved
	// scope version is read from disk, not from the staged session.
	before := sawSaved.Load()
	h.tree(t, http.MethodPost, "/api/tree/edit", map[string]any{"op": "select", "subscription": "alpha", "repository": treeRepoA, "value": true})
	if sawSaved.Load() != before {
		t.Fatalf("saved scope version moved while staging: %d → %d", before, sawSaved.Load())
	}
}

func TestCommonsDiscoveryReasonRedactsEndpoints(t *testing.T) {
	cases := map[string]string{
		"discovery timed out":                    commonsDiscoveryReason(context.DeadlineExceeded),
		"discovery cancelled":                    commonsDiscoveryReason(context.Canceled),
		"hub registry unavailable":               commonsDiscoveryReason(&hivectl.HubUnavailableError{Hub: "https://h?x=1", Reason: "boom"}),
		"discovery unavailable":                  commonsDiscoveryReason(errors.New("Get \"https://x.example/api?token=s\": refused")),
		"repository discovery returned HTTP 503": commonsDiscoveryReason(errors.New("repository discovery returned HTTP 503; selection unchanged")),
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("reason = %q, want %q", got, want)
		}
	}
}

func TestCommonsTreeProjectionFailureIsNeverShownAsApplied(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	tree := h.edit(t, commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "alpha", Repository: treeRepoA, Value: true})
	// A directory at contributor.env makes the projection write fail after
	// profiles.yml has already been saved.
	if err := os.Remove(h.store.EnvPath()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.store.EnvPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	status, out := h.apply(t, tree.Revision)
	if status != http.StatusInternalServerError || out.Outcome != commonsApplyProjectionFailed || out.Receipt.Saved != hivectl.StageProfilesSaved || out.Receipt.RelayReload != hivectl.StageReloadNotRequested {
		t.Fatalf("projection failure = %d %+v", status, out)
	}
	if h.signals.Load() != 0 {
		t.Fatalf("relay must not be signaled while the projection is stale")
	}
	// Further edits and discard are refused until the projection is repaired.
	if status, _ := h.do(t, http.MethodPost, "/api/tree/edit", commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "alpha", Repository: treeRepoB, Value: true}, ""); status != http.StatusConflict {
		t.Fatalf("edit during stale projection = %d", status)
	}
	if status, _ := h.do(t, http.MethodPost, "/api/tree/discard", map[string]any{}, ""); status != http.StatusConflict {
		t.Fatalf("discard during stale projection = %d", status)
	}
	if err := os.Remove(h.store.EnvPath()); err != nil {
		t.Fatal(err)
	}
	status, out = h.apply(t, "")
	if status != http.StatusOK || out.Receipt.Saved != hivectl.StageSaved || out.Receipt.RelayReload != hivectl.StageReloadNotified {
		t.Fatalf("retry = %d %+v", status, out)
	}
}

func TestCommonsTreeUnreadableProfilesSurfaceAsErrors(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.tree(t, http.MethodGet, "/api/tree", nil)
	if err := os.WriteFile(h.store.Path(), []byte("version: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path string }{{http.MethodGet, "/api/tree"}, {http.MethodGet, "/api/tree?refresh=1"}, {http.MethodPost, "/api/tree/edit"}, {http.MethodPost, "/api/tree/discard"}} {
		var body any
		if c.method == http.MethodPost {
			body = commonsTreeEditRequest{Op: commonsTreeOpSelect, Subscription: "alpha", Repository: treeRepoA}
		}
		if status, _ := h.do(t, c.method, c.path, body, ""); status != http.StatusBadRequest {
			t.Errorf("%s %s with unreadable profiles = %d", c.method, c.path, status)
		}
	}
}
