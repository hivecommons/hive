package hivecap

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
)

// The sidecar knowledge adviser (hivecommons/hive#17157): off until the hive
// advertises the route, fail closed everywhere, one warning per kick.

const testToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

type fakeAdviseCaps struct {
	mu   sync.Mutex
	reqs []callback.KnowledgeAdviseRequest
	resp callback.KnowledgeAdviseResponse
	err  error
}

func (f *fakeAdviseCaps) AdviseKnowledge(_ context.Context, req callback.KnowledgeAdviseRequest) (callback.KnowledgeAdviseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return f.resp, f.err
}

func (f *fakeAdviseCaps) calls() []callback.KnowledgeAdviseRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]callback.KnowledgeAdviseRequest(nil), f.reqs...)
}

type adviserHarness struct {
	k    *KnowledgeAdviser
	caps *fakeAdviseCaps
	on   *atomic.Bool
	logs *bytes.Buffer
	now  *time.Time
}

func newAdviserHarness(t *testing.T) *adviserHarness {
	t.Helper()
	caps := &fakeAdviseCaps{resp: callback.KnowledgeAdviseResponse{
		Policy: callback.KnowledgeAdvisorPolicy{Enabled: true, MaxPerKick: 2, CooldownMS: 45000, WantsPaneTail: true, PaneLines: 30},
	}}
	on := &atomic.Bool{}
	on.Store(true)
	h := &adviserHarness{caps: caps, on: on, logs: &bytes.Buffer{}}
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	h.now = &now
	h.k = NewKnowledgeAdviser(caps, on.Load, time.Second)
	h.k.now = func() time.Time { return *h.now }
	h.k.spawn = func(f func()) { f() }
	h.k.Logger = slog.New(slog.NewTextHandler(h.logs, nil))
	return h
}

func (h *adviserHarness) warnings() int { return strings.Count(h.logs.String(), "level=WARN") }

var kickA = time.Date(2026, 11, 18, 11, 0, 0, 5, time.UTC)

func pathScope() crux.KnowledgeScope {
	return crux.KnowledgeScope{
		Kind: "path", Key: "src/crux/agent", Path: "src/crux/agent/manager.go",
		Keywords: []string{"crux", "agent"}, Action: crux.KnowledgeActionEdit, PaneTail: "line one\nline two",
	}
}

func TestKnowledgeAdviserIsOffUntilTheHiveAdvertisesTheRoute(t *testing.T) {
	h := newAdviserHarness(t)
	h.on.Store(false)
	if p := h.k.Policy("developer"); p != (crux.KnowledgeAdvisorPolicy{}) {
		t.Fatalf("policy with the route not advertised = %+v", p)
	}
	if got := h.k.AdviseForKick("developer", kickA, pathScope()); got != nil {
		t.Fatalf("advice with the route not advertised: %+v", got)
	}
	if n := len(h.caps.calls()); n != 0 {
		t.Fatalf("an unadvertised route was called %d times", n)
	}

	var nilAdviser *KnowledgeAdviser
	if p := nilAdviser.Policy("developer"); p.Enabled {
		t.Error("nil adviser enabled")
	}
	if got := nilAdviser.AdviseForKick("developer", kickA, pathScope()); got != nil {
		t.Error("nil adviser advised")
	}
	if got := NewKnowledgeAdviser(nil, func() bool { return true }, 0).Policy("developer"); got.Enabled {
		t.Error("an adviser with no capabilities enabled")
	}
	if got := NewKnowledgeAdviser(struct{}{}, func() bool { return true }, 0).AdviseForKick("developer", kickA, pathScope()); got != nil {
		t.Error("an adviser over capabilities with no route advised")
	}
	if got := NewKnowledgeAdviser(h.caps, nil, 0).Policy("developer"); got.Enabled {
		t.Error("an adviser with no switch enabled")
	}
	if got := h.k.Advise("developer", pathScope()); got != nil {
		t.Error("the kick-less seam must give no advice: the hive requires a kick identity")
	}
}

// Policy never waits on the hive: the first call starts a fetch and answers
// off; the next ones answer from the cache until it ages out.
func TestKnowledgeAdviserPolicyIsFetchedInTheBackgroundAndCached(t *testing.T) {
	h := newAdviserHarness(t)
	var pending []func()
	h.k.spawn = func(f func()) { pending = append(pending, f) }

	if p := h.k.Policy("developer"); p.Enabled {
		t.Fatalf("policy before the first answer = %+v, want off", p)
	}
	if p := h.k.Policy("developer"); p.Enabled || len(pending) != 1 {
		t.Fatalf("a second call while a fetch is in flight started another: %d pending", len(pending))
	}
	pending[0]()
	want := crux.KnowledgeAdvisorPolicy{Enabled: true, MaxPerKick: 2, Cooldown: 45 * time.Second, WantsPaneTail: true, PaneLines: 30}
	if p := h.k.Policy("developer"); p != want {
		t.Fatalf("policy = %+v, want %+v", p, want)
	}
	calls := h.caps.calls()
	if len(calls) != 1 || !calls[0].PolicyOnly || calls[0].Agent != "developer" || !calls[0].KickAt.IsZero() || calls[0].Scope.Key != "" {
		t.Fatalf("policy request = %+v", calls)
	}

	*h.now = h.now.Add(DefaultKnowledgePolicyTTL - time.Second)
	h.k.Policy("developer")
	if len(pending) != 1 {
		t.Fatal("refetched inside the TTL")
	}
	*h.now = h.now.Add(2 * time.Second)
	if p := h.k.Policy("developer"); p != want {
		t.Fatalf("a stale policy is still served while the refresh runs: %+v", p)
	}
	if len(pending) != 2 {
		t.Fatalf("no refetch after the TTL: %d", len(pending))
	}

	// The hive turns the advisor off for the agent: the next refresh says so.
	h.caps.mu.Lock()
	h.caps.resp.Policy = callback.KnowledgeAdvisorPolicy{}
	h.caps.mu.Unlock()
	pending[1]()
	if p := h.k.Policy("developer"); p.Enabled {
		t.Fatalf("policy after the hive disabled the agent = %+v", p)
	}
	// Policies are per agent.
	if p := h.k.Policy("scanner"); p.Enabled {
		t.Fatalf("another agent inherited a policy: %+v", p)
	}
	if got := h.k.Policy(""); got.Enabled {
		t.Fatal("empty agent enabled")
	}
}

func TestKnowledgeAdviserPolicyFetchFailureKeepsTheAdvisorOff(t *testing.T) {
	h := newAdviserHarness(t)
	h.k.Policy("developer") // spawn is inline: the fetch succeeded
	if p := h.k.Policy("developer"); !p.Enabled {
		t.Fatal("setup: policy not fetched")
	}
	h.caps.mu.Lock()
	h.caps.err = fmt.Errorf("%w: hive away", callback.ErrUnavailable)
	h.caps.mu.Unlock()
	for i := 0; i < 3; i++ {
		*h.now = h.now.Add(DefaultKnowledgePolicyTTL + time.Second)
		h.k.Policy("developer")
	}
	if p := h.k.Policy("developer"); p.Enabled {
		t.Fatalf("policy after failed fetches = %+v, want off", p)
	}
	if n := h.warnings(); n != 1 {
		t.Fatalf("policy failures logged %d warnings, want one per outage:\n%s", n, h.logs)
	}
	// Recovery turns it back on.
	h.caps.mu.Lock()
	h.caps.err = nil
	h.caps.mu.Unlock()
	*h.now = h.now.Add(DefaultKnowledgePolicyTTL + time.Second)
	h.k.Policy("developer")
	if p := h.k.Policy("developer"); !p.Enabled {
		t.Fatal("policy did not recover")
	}
}

func TestKnowledgeAdviserSendsTheScopeAndRedactsEverything(t *testing.T) {
	h := newAdviserHarness(t)
	h.caps.resp.Advice = []callback.KnowledgeAdvice{
		{Slug: "a", Title: "A", Summary: "sum", Ref: "ref"},
		{Slug: "", Title: "no slug"},
		{Slug: "b", Title: "B"},
	}
	scope := pathScope()
	scope.PaneTail = "run with token " + testToken + "\nnext line"
	scope.Key = "src/" + testToken
	scope.Path = "src/" + testToken + "/a.go"
	scope.Keywords = []string{testToken, "crux"}

	got := h.k.AdviseForKick("developer", kickA, scope)
	if len(got) != 2 || got[0] != (crux.KnowledgeAdvice{Slug: "a", Title: "A", Summary: "sum", Ref: "ref"}) || got[1].Slug != "b" {
		t.Fatalf("advice = %+v (an entry with no slug is dropped)", got)
	}
	calls := h.caps.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	req := calls[0]
	if req.Agent != "developer" || !req.KickAt.Equal(kickA) || req.PolicyOnly {
		t.Errorf("request identity = %+v", req)
	}
	if req.Scope.Kind != "path" || req.Scope.Action != crux.KnowledgeActionEdit || req.Scope.Key == "" {
		t.Errorf("scope = %+v", req.Scope)
	}
	wire := fmt.Sprintf("%+v", req)
	if strings.Contains(wire, "ghp_abcdefghijklmnopqrstuvwxyz") || !strings.Contains(req.Scope.PaneTail, "ghp_abc***") {
		t.Errorf("a token crossed to the hive unredacted: %s", wire)
	}
	// The response's policy is now the agent's current one.
	if p := h.k.Policy("developer"); p.PaneLines != 30 || !p.Enabled {
		t.Errorf("policy after an advise = %+v", p)
	}
}

func TestKnowledgeAdviserCapsAdviceLines(t *testing.T) {
	h := newAdviserHarness(t)
	for i := 0; i < maxKnowledgeAdviceLines+4; i++ {
		h.caps.resp.Advice = append(h.caps.resp.Advice, callback.KnowledgeAdvice{Slug: fmt.Sprintf("s%d", i), Title: "t"})
	}
	if got := h.k.AdviseForKick("developer", kickA, pathScope()); len(got) != maxKnowledgeAdviceLines {
		t.Fatalf("advice lines = %d, want %d", len(got), maxKnowledgeAdviceLines)
	}
}

// Any failure is no advice, and a kick logs one warning however many scopes
// and ticks fail; the next kick logs its own.
func TestKnowledgeAdviserFailsClosedWithOneWarningPerKick(t *testing.T) {
	h := newAdviserHarness(t)
	for name, err := range map[string]error{
		"unavailable":  fmt.Errorf("%w: down", callback.ErrUnavailable),
		"forbidden":    fmt.Errorf("%w: stale kick", callback.ErrForbidden),
		"rate limited": fmt.Errorf("%w: slow", callback.ErrRateLimited),
		"unauthorized": callback.ErrUnauthorized,
		"not enabled":  callback.ErrNotConfigured,
	} {
		h.caps.mu.Lock()
		h.caps.err = err
		h.caps.mu.Unlock()
		if got := h.k.AdviseForKick("developer", kickA.Add(time.Duration(len(name))*time.Hour), pathScope()); got != nil {
			t.Errorf("%s: advice %+v", name, got)
		}
	}

	h2 := newAdviserHarness(t)
	h2.caps.err = fmt.Errorf("%w: down", callback.ErrUnavailable)
	h2.caps.resp.Advice = []callback.KnowledgeAdvice{{Slug: "x", Title: "t"}}
	for i := 0; i < 5; i++ {
		if got := h2.k.AdviseForKick("developer", kickA, pathScope()); got != nil {
			t.Fatalf("advice on a failed call: %+v", got)
		}
	}
	if n := h2.warnings(); n != 1 {
		t.Fatalf("one kick logged %d warnings, want 1:\n%s", n, h2.logs)
	}
	if got := h2.k.AdviseForKick("developer", kickA.Add(time.Minute), pathScope()); got != nil {
		t.Fatal("advice on a failed call in the next kick")
	}
	if n := h2.warnings(); n != 2 {
		t.Fatalf("two kicks logged %d warnings, want 2:\n%s", n, h2.logs)
	}
	if strings.Contains(h2.logs.String(), testToken) {
		t.Fatalf("a token reached the log:\n%s", h2.logs)
	}
	// Another agent's failure is its own warning.
	h2.k.AdviseForKick("scanner", kickA, pathScope())
	if n := h2.warnings(); n != 3 {
		t.Fatalf("a second agent logged %d warnings in total, want 3", n)
	}
}

func TestKnowledgeAdviserNeedsAgentAndKick(t *testing.T) {
	h := newAdviserHarness(t)
	if got := h.k.AdviseForKick("", kickA, pathScope()); got != nil {
		t.Error("advised an empty agent")
	}
	if got := h.k.AdviseForKick("developer", time.Time{}, pathScope()); got != nil {
		t.Error("advised without a kick identity")
	}
	if n := len(h.caps.calls()); n != 0 {
		t.Fatalf("an unidentified request reached the hive %d times", n)
	}
}

// A hive that cannot be reached is no advice and exactly one warning for the
// kick, over the real client, with no panic and no wait past the timeout.
func TestKnowledgeAdviserUnreachableOverTheClient(t *testing.T) {
	dir, err := os.MkdirTemp("", "hcapk")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	c, err := callback.NewClient("unix://"+filepath.Join(dir, "nobody.sock"), "")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	k := NewKnowledgeAdviser(c, func() bool { return true }, 200*time.Millisecond)
	k.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	start := time.Now()
	for i := 0; i < 3; i++ {
		if got := k.AdviseForKick("developer", kickA, pathScope()); got != nil {
			t.Fatalf("advice from an unreachable hive: %+v", got)
		}
	}
	if time.Since(start) > 5*time.Second {
		t.Error("calls blocked past the timeout")
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("unreachable hive logged %d warnings for one kick, want 1:\n%s", n, logs.String())
	}
	if c.Status().Reachable {
		t.Errorf("client status = %+v, want unreachable", c.Status())
	}
}

// policyAdvise is a hive that serves a policy snapshot and the knowledge
// route.
type policyAdvise struct {
	*policyDirect
	advise *fakeAdviseCaps
}

func (p policyAdvise) AdviseKnowledge(ctx context.Context, req callback.KnowledgeAdviseRequest) (callback.KnowledgeAdviseResponse, error) {
	return p.advise.AdviseKnowledge(ctx, req)
}

// The adviser a Set carries is gated by the policy snapshot's flag, so a hive
// that never sets it keeps the runtime exactly as it was: no call is made.
func TestSetKnowledgeAdviserFollowsTheSnapshotFlag(t *testing.T) {
	hive := policyAdvise{policyDirect: &policyDirect{direct: &direct{}}, advise: &fakeAdviseCaps{}}
	hive.advise.resp.Policy = callback.KnowledgeAdvisorPolicy{Enabled: true, MaxPerKick: 1}
	s := New(hive, 0)
	s.Knowledge.spawn = func(f func()) { f() }
	if s.Knowledge == nil || s.Policy.KnowledgeAdvise() {
		t.Fatal("a Set must carry an adviser that is off before any snapshot")
	}
	if got := s.Knowledge.Policy("developer"); got.Enabled {
		t.Fatalf("policy before any snapshot = %+v", got)
	}
	if _, err := s.Policy.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Policy.KnowledgeAdvise() || s.Knowledge.Policy("developer").Enabled {
		t.Fatal("a snapshot without the flag turned the adviser on")
	}
	if got := s.Knowledge.AdviseForKick("developer", kickA, pathScope()); got != nil {
		t.Fatal("advised while the hive does not advertise the route")
	}
	if n := len(hive.advise.calls()); n != 0 {
		t.Fatalf("the route was called %d times while not advertised", n)
	}

	hive.set(callback.Policy{KnowledgeAdvise: true}, nil)
	if _, err := s.Policy.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.Policy.KnowledgeAdvise() {
		t.Fatal("the snapshot flag was not read")
	}
	s.Knowledge.Policy("developer")
	if got := s.Knowledge.Policy("developer"); !got.Enabled || got.MaxPerKick != 1 {
		t.Fatalf("policy after the hive advertised the route = %+v", got)
	}

	// The hive switches the route off again: the adviser goes quiet at once.
	hive.set(callback.Policy{}, nil)
	if _, err := s.Policy.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.Knowledge.Policy("developer"); got.Enabled {
		t.Fatalf("policy after the hive switched the route off = %+v", got)
	}
	before := len(hive.advise.calls())
	if got := s.Knowledge.AdviseForKick("developer", kickA, pathScope()); got != nil || len(hive.advise.calls()) != before {
		t.Fatal("advised after the hive stopped advertising the route")
	}
	var nilPolicy *Policy
	if nilPolicy.KnowledgeAdvise() {
		t.Fatal("nil policy advertised the route")
	}
}
