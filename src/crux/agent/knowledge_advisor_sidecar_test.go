package agent

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/hivecap"
	"github.com/hivecommons/hive/crux/internal/testutil"
)

// Golden test for the sidecar knowledge adviser (hivecommons/hive#17157): a
// manager whose adviser is the hivecap one, calling POST /v1/knowledge/advise
// on a hive that runs the same chain in-process, types exactly the lines an
// in-process manager types for the same pane fixture, and the hive sees the
// same scope, pane tail included, the in-process adviser sees.

const sidecarGoldenToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

// goldenHive is the hive side of the route over an in-process adviser. Only
// the knowledge capability is served; the embedded nil Capabilities is never
// called.
type goldenHive struct {
	callback.Capabilities
	adviser crux.KnowledgeAdviser

	mu       sync.Mutex
	requests []callback.KnowledgeAdviseRequest
	scopes   []crux.KnowledgeScope
}

func (h *goldenHive) Configured(capability string) bool {
	return capability == callback.CapabilityKnowledgeAdvise
}

func (h *goldenHive) AdviseKnowledge(_ context.Context, req callback.KnowledgeAdviseRequest) (callback.KnowledgeAdviseResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pol := h.adviser.Policy(req.Agent)
	resp := callback.KnowledgeAdviseResponse{
		Advice: []callback.KnowledgeAdvice{},
		Policy: callback.KnowledgeAdvisorPolicy{
			Enabled: pol.Enabled, MaxPerKick: pol.MaxPerKick, CooldownMS: int(pol.Cooldown / time.Millisecond),
			WantsPaneTail: pol.WantsPaneTail, PaneLines: pol.PaneLines,
		},
	}
	if req.PolicyOnly {
		return resp, nil
	}
	h.requests = append(h.requests, req)
	scope := crux.KnowledgeScope{
		Kind: req.Scope.Kind, Key: req.Scope.Key, Path: req.Scope.Path, Keywords: req.Scope.Keywords,
		Action: req.Scope.Action, PaneTail: req.Scope.PaneTail,
	}
	h.scopes = append(h.scopes, scope)
	for _, a := range h.adviser.Advise(req.Agent, scope) {
		resp.Advice = append(resp.Advice, callback.KnowledgeAdvice{Slug: a.Slug, Title: a.Title, Summary: a.Summary, Ref: a.Ref})
	}
	return resp, nil
}

func (h *goldenHive) seen() ([]callback.KnowledgeAdviseRequest, []crux.KnowledgeScope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]callback.KnowledgeAdviseRequest(nil), h.requests...), append([]crux.KnowledgeScope(nil), h.scopes...)
}

func goldenAdviser() *fakeAdviser {
	return &fakeAdviser{
		policy: crux.KnowledgeAdvisorPolicy{Enabled: true, MaxPerKick: 2, Cooldown: time.Minute, WantsPaneTail: true, PaneLines: 6},
		byKey: map[string][]crux.KnowledgeAdvice{
			"src/scripts": {
				{Slug: "scripts-tree", Title: "Re-pin TRUSTED_SCRIPTS_TREE", Summary: "Any src/scripts edit must re-pin the tree.", Ref: "knowledge entry scripts-tree"},
				{Slug: "scripts-shellcheck", Title: "Shellcheck gate", Summary: "Scripts run under shellcheck in CI."},
			},
		},
	}
}

// goldenPane is the fixture: scrollback with work from before the kick, then a
// new scope entered with an edit and a token in the tail.
func goldenScrollback() (baseline, entered string) {
	baseline = "⏺ Read(src/crux/agent/manager.go)"
	entered = baseline + "\n$ export GH_TOKEN=" + sidecarGoldenToken + "\n⏺ Edit(src/scripts/ci.sh)"
	return baseline, entered
}

// driveGolden runs the baseline tick and then ticks the entered pane until a
// line is typed (a networked adviser answers on a later tick when it is slower
// than the grace period).
func driveGolden(t *testing.T, adviser crux.KnowledgeAdviser) []string {
	t.Helper()
	m, a, typed := advisorManager(t, adviser)
	fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	// Let a policy that is fetched in the background arrive before the first
	// tick: the tick that baselines the pane must already see it enabled.
	testutil.Eventually(t, 5*time.Second, func() bool { return adviser.Policy(a.Name).Enabled }, "adviser policy never became enabled")
	baseline, entered := goldenScrollback()
	m.adviseKnowledge(a, baseline, workingPane)
	testutil.Eventually(t, 5*time.Second, func() bool {
		m.adviseKnowledge(a, entered, workingPane)
		return len(*typed) > 0
	}, "no advice line was typed")
	return *typed
}

func TestSidecarAdviserDeliversTheSameLinesAsInProcess(t *testing.T) {
	inProcess := goldenAdviser()
	wantLines := driveGolden(t, inProcess)

	hiveAdviser := goldenAdviser()
	hive := &goldenHive{adviser: hiveAdviser}
	srv := httptest.NewServer(callback.NewServer(hive, "golden-token").Handler())
	t.Cleanup(srv.Close)
	client, err := callback.NewClient(srv.URL, "golden-token")
	if err != nil {
		t.Fatal(err)
	}
	sidecar := hivecap.NewKnowledgeAdviser(client, func() bool { return true }, 0)
	gotLines := driveGolden(t, sidecar)

	if len(wantLines) != 1 || !strings.HasPrefix(wantLines[0], "📚 Knowledge: Re-pin TRUSTED_SCRIPTS_TREE — Any src/scripts edit") {
		t.Fatalf("in-process lines = %v", wantLines)
	}
	if !reflect.DeepEqual(gotLines, wantLines) {
		t.Fatalf("sidecar typed %v, in-process typed %v", gotLines, wantLines)
	}

	// The hive saw the scope the in-process adviser saw, pane tail and all,
	// with no raw token.
	requests, scopes := hive.seen()
	if len(scopes) != 1 || len(inProcess.scopes) != 1 {
		t.Fatalf("hive saw %d scopes, in-process adviser %d", len(scopes), len(inProcess.scopes))
	}
	if !reflect.DeepEqual(scopes[0], inProcess.scopes[0]) {
		t.Errorf("hive scope = %+v\nin-process scope = %+v", scopes[0], inProcess.scopes[0])
	}
	if scopes[0].Action != crux.KnowledgeActionEdit || scopes[0].Key != "src/scripts" || scopes[0].PaneTail == "" {
		t.Errorf("scope = %+v", scopes[0])
	}
	if strings.Contains(scopes[0].PaneTail, sidecarGoldenToken) || !strings.Contains(scopes[0].PaneTail, "ghp_abc***") {
		t.Errorf("pane tail not redacted: %q", scopes[0].PaneTail)
	}
	if requests[0].Agent != "developer" || requests[0].KickAt.IsZero() {
		t.Errorf("request identity = %+v", requests[0])
	}
}

// With the hive unreachable nothing is typed: the policy never arrives, so the
// advisor stays off and no pane is parsed (the per-kick warning for a call that
// fails mid-kick is covered in crux/hivecap).
func TestSidecarAdviserTypesNothingWhenTheHiveIsAway(t *testing.T) {
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close() // nothing listens at url any more
	client, err := callback.NewClient(url, "t")
	if err != nil {
		t.Fatal(err)
	}
	client.SetTimeout(200 * time.Millisecond)
	sidecar := hivecap.NewKnowledgeAdviser(client, func() bool { return true }, 200*time.Millisecond)

	m, a, typed := advisorManager(t, sidecar)
	fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	baseline, entered := goldenScrollback()
	// An unreachable hive never gives a policy, so the advisor stays off: no
	// pane is parsed and nothing is typed.
	m.adviseKnowledge(a, baseline, workingPane)
	for i := 0; i < 3; i++ {
		m.adviseKnowledge(a, entered, workingPane)
	}
	if len(*typed) != 0 {
		t.Fatalf("typed with the hive away: %v", *typed)
	}
	if p := sidecar.Policy(a.Name); p.Enabled {
		t.Fatalf("policy with the hive away = %+v", p)
	}
}
