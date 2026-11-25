package automerge

// Tests for the decision gate (auto_merge.decision_gate). The invariant: an
// automerge lane never merges a PR that touches a credential, keystore, auth
// or secret path, or that wears the sentinel alert label, until a person has
// decided on that exact head. The author's role is not that decision. Every
// refusal asserts zero merge calls, not merely a skip reason.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// testCredentialFile is a file #19678 changed: the push broker's forge remote,
// which mints and hands a forge token to git.
const testCredentialFile = "src/crux/pushbroker/forge_remote.go"

// decisionGateOn is the policy with the shipped default paths and the
// sentinel switch on, for every repo.
func decisionGateOn() func(string) DecisionGatePolicy {
	return func(string) DecisionGatePolicy {
		return DecisionGatePolicy{Enabled: true, Paths: config.DecisionGateConfig{}.EffectivePaths(), Sentinel: true}
	}
}

func decisionApprovedReview(login, head string) map[string]any {
	return map[string]any{
		"id":    1,
		"user":  map[string]any{"login": login, "type": "User"},
		"state": "APPROVED",
		// No submitted_at: v6 binds a dated review to head by walking the
		// commits (automerge_review_head_binding.go), which this fixture
		// does not serve. A dateless review binds by commit_id alone.
		"commit_id": head,
	}
}

// decisionFront fronts the trusted-author fixture: it serves the PR's files
// and reviews and proxies everything else, so the fixture keeps driving the
// merge path.
type decisionFront struct {
	mu          sync.Mutex
	files       []string
	filesStatus int
	reviews     []map[string]any
}

func newDecisionFront(t *testing.T, backend string, f *decisionFront) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/files":
			if f.filesStatus != 0 {
				w.WriteHeader(f.filesStatus)
				return
			}
			files := []map[string]any{}
			for _, name := range f.files {
				files = append(files, map[string]any{"filename": name, "status": "modified", "additions": 1})
			}
			json.NewEncoder(w).Encode(files)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			reviews := f.reviews
			if reviews == nil {
				reviews = []map[string]any{}
			}
			json.NewEncoder(w).Encode(reviews)
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
}

// newDecisionTrustedEngine is the trusted-author engine with the gate, a
// trusted-merger authorizer (bob, carol) and its own stage store, so no
// admission recorded by another test can stand in for a decision here.
func newDecisionTrustedEngine(apiURL string, gate func(string) DecisionGatePolicy) (*Engine, TrustedAuthorPolicy) {
	policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}
	engine := newTrustedSweepEngine(apiURL, policy, Options{
		DecisionGate:  gate,
		SentinelLabel: func() string { return defaultSentinelLabel },
		StageStore:    stagelabels.NewStore("", nil),
		MergerAuthorizer: func(login string) bool {
			return testTrustedMergers[strings.ToLower(strings.TrimSpace(login))]
		},
	})
	return engine, policy
}

// The #19678 shape: a trusted (owner) author, CI green, no swarm or human
// objection GitHub can see, and a change to the push broker's credential
// code. The trusted-author lane must hold it for a person, on every tick.
func TestTrustedAuthorSweepCredentialPathNeedsDecision(t *testing.T) {
	fx := trustedSweepFixture{}
	api := newTrustedSweepAPI(t, &fx)
	defer api.Close()
	front := &decisionFront{files: []string{testCredentialFile, "src/a.go"}}
	srv := newDecisionFront(t, api.URL, front)
	defer srv.Close()

	engine, policy := newDecisionTrustedEngine(srv.URL, decisionGateOn())
	for tick := 0; tick < 2; tick++ {
		event, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
		if err != nil {
			t.Fatalf("tick %d: err = %v, want nil", tick, err)
		}
		if reason != autoMergeReasonSensitivePathDecision {
			t.Fatalf("tick %d: reason = %q, want %q", tick, reason, autoMergeReasonSensitivePathDecision)
		}
		if event.MergeSHA != "" || fx.merges != 0 {
			t.Fatalf("tick %d: event = %+v, merges = %d; a credential-path PR merged without a decision", tick, event, fx.merges)
		}
	}
}

// Only a trusted person's decision on exactly the head about to merge lets
// the credential-path PR through. An approval on an older head, or by
// someone who is not a trusted merger, is no decision.
func TestTrustedAuthorSweepCredentialPathDecisionMustBeOnHeadByTrustedPerson(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reviews    []map[string]any
		wantReason string
		wantMerges int
	}{
		{name: "trusted approval on head", reviews: []map[string]any{decisionApprovedReview("carol", "sha7")}, wantMerges: 1},
		{name: "trusted approval on an older head", reviews: []map[string]any{decisionApprovedReview("carol", "sha6")}, wantReason: autoMergeReasonSensitivePathDecision},
		{name: "approval by an untrusted login", reviews: []map[string]any{decisionApprovedReview("mallory", "sha7")}, wantReason: autoMergeReasonSensitivePathDecision},
		{name: "no review at all", wantReason: autoMergeReasonSensitivePathDecision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := trustedSweepFixture{}
			api := newTrustedSweepAPI(t, &fx)
			defer api.Close()
			front := &decisionFront{files: []string{testCredentialFile}, reviews: tc.reviews}
			srv := newDecisionFront(t, api.URL, front)
			defer srv.Close()

			engine, policy := newDecisionTrustedEngine(srv.URL, decisionGateOn())
			_, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if reason != tc.wantReason || fx.merges != tc.wantMerges {
				t.Fatalf("reason = %q, merges = %d; want %q / %d", reason, fx.merges, tc.wantReason, tc.wantMerges)
			}
		})
	}
}

// A sentinel alert on a trusted author's PR is notice-only in the label
// predicate (labelBlockReason with trusted=true). With the gate's sentinel
// switch on, it still needs a person's decision; with the gate off, the
// pre-gate behaviour (merge) is unchanged.
func TestTrustedAuthorSweepSentinelLabelNeedsDecision(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gate       func(string) DecisionGatePolicy
		wantReason string
		wantMerges int
	}{
		{name: "gate on", gate: decisionGateOn(), wantReason: autoMergeReasonSentinelDecision},
		{name: "gate on, sentinel switch off", gate: func(string) DecisionGatePolicy {
			return DecisionGatePolicy{Enabled: true, Paths: config.DecisionGateConfig{}.EffectivePaths()}
		}, wantMerges: 1},
		{name: "gate not installed", gate: nil, wantMerges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := trustedSweepFixture{labels: []string{"sentinel-alert"}}
			api := newTrustedSweepAPI(t, &fx)
			defer api.Close()
			front := &decisionFront{files: []string{"src/a.go"}}
			srv := newDecisionFront(t, api.URL, front)
			defer srv.Close()

			engine, policy := newDecisionTrustedEngine(srv.URL, tc.gate)
			_, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if reason != tc.wantReason || fx.merges != tc.wantMerges {
				t.Fatalf("reason = %q, merges = %d; want %q / %d", reason, fx.merges, tc.wantReason, tc.wantMerges)
			}
		})
	}
}

// do-not-merge/* outranks everything, a person's approval on the head
// included: the label is the person saying no.
func TestTrustedAuthorSweepDoNotMergeLabelHoldsEvenWithDecision(t *testing.T) {
	fx := trustedSweepFixture{labels: []string{"do-not-merge/hold"}}
	api := newTrustedSweepAPI(t, &fx)
	defer api.Close()
	front := &decisionFront{files: []string{testCredentialFile}, reviews: []map[string]any{decisionApprovedReview("carol", "sha7")}}
	srv := newDecisionFront(t, api.URL, front)
	defer srv.Close()

	engine, policy := newDecisionTrustedEngine(srv.URL, decisionGateOn())
	_, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if reason != "label:do-not-merge/hold" || fx.merges != 0 {
		t.Fatalf("reason = %q, merges = %d; want label:do-not-merge/hold and no merge", reason, fx.merges)
	}
}

// When the changed-file list cannot be read the gate cannot tell whether a
// credential path is touched, so it fails closed.
func TestTrustedAuthorSweepDecisionGateFileListErrorFailsClosed(t *testing.T) {
	fx := trustedSweepFixture{}
	api := newTrustedSweepAPI(t, &fx)
	defer api.Close()
	front := &decisionFront{filesStatus: http.StatusInternalServerError}
	srv := newDecisionFront(t, api.URL, front)
	defer srv.Close()

	engine, policy := newDecisionTrustedEngine(srv.URL, decisionGateOn())
	_, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err == nil || reason != autoMergeReasonSensitivePathEvidence || fx.merges != 0 {
		t.Fatalf("reason = %q, err = %v, merges = %d; want %q with an error and no merge", reason, err, fx.merges, autoMergeReasonSensitivePathEvidence)
	}
}

// The App's own PR is held the same way: the App's authorship is not a
// decision. A trusted person's approval on the head releases it.
func TestSelfAuthoredSweepCredentialPathNeedsDecision(t *testing.T) {
	const keystoreFile = "src/crux/keystore/keystore.go"
	for _, tc := range []struct {
		name       string
		reviews    []map[string]any
		wantReason string
		wantMerges int
	}{
		{name: "no decision", wantReason: autoMergeReasonSensitivePathDecision},
		{name: "trusted approval on head", reviews: []map[string]any{decisionApprovedReview("bob", "sha7")}, wantMerges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{files: []string{keystoreFile}, reviews: tc.reviews, mergeApplied: true, mergeCalls: &merges})
			defer api.Close()

			c := newAutoMergeSweepClient(api.URL)
			c.stageStore = stagelabels.NewStore("", nil)
			c.SetDecisionGate(decisionGateOn())
			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if reason != tc.wantReason || merges != tc.wantMerges {
				t.Fatalf("reason = %q, merges = %d; want %q / %d", reason, merges, tc.wantReason, tc.wantMerges)
			}
		})
	}
}

// A PR that touches no decision path, or a gate that is off, merges as
// before.
func TestSelfAuthoredSweepDecisionGateOffOrNoMatchMerges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		gate  func(string) DecisionGatePolicy
	}{
		{name: "no credential path", files: []string{"src/pkg/tokens/collector.go", "docs/README.md"}, gate: decisionGateOn()},
		{name: "gate disabled", files: []string{testCredentialFile}, gate: func(string) DecisionGatePolicy {
			return DecisionGatePolicy{Enabled: false, Paths: config.DecisionGateConfig{}.EffectivePaths(), Sentinel: true}
		}},
		{name: "explicit empty path list", files: []string{testCredentialFile}, gate: func(string) DecisionGatePolicy {
			return DecisionGatePolicy{Enabled: true, Paths: config.DecisionGateConfig{Paths: []string{}}.EffectivePaths(), Sentinel: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{files: tc.files, mergeApplied: true, mergeCalls: &merges})
			defer api.Close()

			c := newAutoMergeSweepClient(api.URL)
			c.stageStore = stagelabels.NewStore("", nil)
			c.SetDecisionGate(tc.gate)
			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil || reason != "" || merges != 1 {
				t.Fatalf("reason = %q, err = %v, merges = %d; want one clean merge", reason, err, merges)
			}
		})
	}
}

// The shipped default covers the credential files #19678 and #19684 changed
// and the hive's keystore, auth and secret code, and leaves ordinary code
// (including the LLM token-usage package) alone.
func TestDefaultDecisionGatePathsCoverCredentialCode(t *testing.T) {
	paths := config.DecisionGateConfig{}.EffectivePaths()
	for _, f := range []string{
		testCredentialFile,
		"src/crux/pushbroker/pushbroker.go",
		"src/crux/proxy/rules.go",
		"src/crux/keystore/keystore.go",
		"src/pkg/keystore/openbao/client.go",
		"src/crux/authz/authz.go",
		"src/pkg/auth/session.go",
		"src/pkg/github/proxy_token_source.go",
		"src/crux/agent/copilot_token_store.go",
		"bin/git-credential-hive.sh",
		"bin/gh-app-token.sh",
		"deploy/tls/server.key",
		"src/pkg/dashboard/oauth_callback.go",
		"src/pkg/hub/jwt_verify.go",
		"src/pkg/config/secrets.go",
	} {
		if !intent.PathMatchesAny(f, paths) {
			t.Errorf("default decision paths do not cover %s", f)
		}
	}
	for _, f := range []string{
		"src/pkg/tokens/collector.go",
		"src/pkg/dashboard/static/tokens.css",
		"src/pkg/github/automerge/automerge_sweep.go",
		"src/crux/agent/manager_sandbox.go",
		"docs/README.md",
		"src/pkg/authorship/author.go",
	} {
		if intent.PathMatchesAny(f, paths) {
			t.Errorf("default decision paths unexpectedly cover %s", f)
		}
	}
}

func TestSetDecisionGateNilEngineAndRemoval(t *testing.T) {
	var nilEngine *Engine
	nilEngine.SetDecisionGate(decisionGateOn())
	if got := nilEngine.currentDecisionGate("acme/widget"); got.Enabled {
		t.Fatalf("nil engine gate = %+v, want off", got)
	}
	c := New(nil, Options{DecisionGate: decisionGateOn()})
	if !c.currentDecisionGate("acme/widget").Enabled {
		t.Fatal("gate from Options not installed")
	}
	c.SetDecisionGate(nil)
	if c.currentDecisionGate("acme/widget").Enabled {
		t.Fatal("SetDecisionGate(nil) did not remove the gate")
	}
}
