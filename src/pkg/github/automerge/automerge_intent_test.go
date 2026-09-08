package automerge

// Tests for the self-authored sweep's intent tier gate (#6258). The
// invariant under test is that the MERGE DOES NOT HAPPEN — every refusal
// case asserts zero PUT .../merge calls against the fixture, not merely a
// skip reason — because before this gate the App squashed every green PR it
// authored regardless of tier.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/intent"
)

const (
	testIntentSourceFile    = "src/pkg/thing/thing.go"
	testIntentDocsFile      = "docs/guide.md"
	testIntentGuardrailFile = ".github/workflows/ci.yml"
	testIntentLinkedBody    = "Fixes #12"
)

func newIntentGateSweepClient(apiURL string, enforce bool) *Engine {
	c := newAutoMergeSweepClient(apiURL)
	c.SetIntentGate(&IntentGate{Enforce: func() bool { return enforce }})
	return c
}

func TestSelfAuthoredSweepIntentGateRefusesUnauthorizedTier(t *testing.T) {
	tests := []struct {
		name string
		fx   selfSweepFixture
	}{
		// Tier1 (bugfix/chore default) requires a linked issue. No body, no
		// issue, no merge — the exact PR the human lane would refuse.
		{name: "tier1 without linked issue", fx: selfSweepFixture{files: []string{testIntentSourceFile}}},
		// A body that mentions an issue without a closing keyword is not
		// linked-issue evidence.
		{name: "tier1 with unlinked issue mention", fx: selfSweepFixture{body: "see issue 12", files: []string{testIntentSourceFile}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merges := 0
			tt.fx.mergeCalls = &merges
			api := newSelfSweepGuardAPI(t, tt.fx)
			defer api.Close()
			c := newIntentGateSweepClient(api.URL, true)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if reason != autoMergeReasonIntentUnauthorized {
				t.Fatalf("reason = %q, want %q", reason, autoMergeReasonIntentUnauthorized)
			}
			if event.MergeSHA != "" {
				t.Fatalf("event = %+v, want no merge recorded", event)
			}
			if merges != 0 {
				t.Fatalf("merge endpoint called %d times, want 0: the intent tier gate must withhold the merge, not just log it", merges)
			}
		})
	}
}

func TestSelfAuthoredSweepIntentGateFailsClosedWithoutEvidence(t *testing.T) {
	tests := []struct {
		name string
		fx   selfSweepFixture
	}{
		{name: "files API error", fx: selfSweepFixture{body: testIntentLinkedBody, files: []string{testIntentSourceFile}, filesHTTPCode: http.StatusInternalServerError}},
		// GitHub says 3 files changed but the API returned 1: a partial list
		// could hide a guardrail path, so the tier is unknown.
		{name: "incomplete file list", fx: selfSweepFixture{body: testIntentLinkedBody, files: []string{testIntentSourceFile}, changedFiles: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merges := 0
			tt.fx.mergeCalls = &merges
			api := newSelfSweepGuardAPI(t, tt.fx)
			defer api.Close()
			c := newIntentGateSweepClient(api.URL, true)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
			if err == nil {
				t.Fatalf("err = nil, want evidence error")
			}
			if reason != autoMergeReasonIntentEvidence {
				t.Fatalf("reason = %q, want %q", reason, autoMergeReasonIntentEvidence)
			}
			if event.MergeSHA != "" || merges != 0 {
				t.Fatalf("event = %+v, merges = %d; want no merge when the tier cannot be established", event, merges)
			}
		})
	}
}

func TestSelfAuthoredSweepIntentGateAuthorizedTiersMerge(t *testing.T) {
	tests := []struct {
		name string
		fx   selfSweepFixture
	}{
		// Tier0: additive docs-only change needs no evidence at all.
		{name: "tier0 additive docs", fx: selfSweepFixture{files: []string{testIntentDocsFile}}},
		// Tier1 with a linked issue is authorized.
		{name: "tier1 with linked issue", fx: selfSweepFixture{body: testIntentLinkedBody, files: []string{testIntentSourceFile}}},
		// Tier3 guardrail path: EvaluateForAppSelfMerge drops ONLY the
		// human-approval requirement (Prow forbids the App approving its
		// own PR), so this is authorized by that function's documented
		// contract — pinned here so a future tightening is deliberate.
		{name: "tier3 guardrail under app self-merge contract", fx: selfSweepFixture{files: []string{testIntentGuardrailFile}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merges := 0
			tt.fx.mergeCalls = &merges
			tt.fx.mergeApplied = true
			api := newSelfSweepGuardAPI(t, tt.fx)
			defer api.Close()
			c := newIntentGateSweepClient(api.URL, true)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
			if err != nil || reason != "" {
				t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want clean merge", reason, err)
			}
			if event.MergeSHA != "merge7" || merges != 1 {
				t.Fatalf("event = %+v, merges = %d; want exactly one merge at merge7", event, merges)
			}
		})
	}
}

// With intent.enforce off the gate is advisory, exactly like
// writeMergeEligible's `if enforceIntent`: parity with the human lane means
// the self-authored sweep must not be STRICTER than it either.
func TestSelfAuthoredSweepIntentGateAdvisoryWhenNotEnforced(t *testing.T) {
	merges := 0
	fx := selfSweepFixture{files: []string{testIntentSourceFile}, mergeApplied: true, mergeCalls: &merges}
	api := newSelfSweepGuardAPI(t, fx)
	defer api.Close()
	c := newIntentGateSweepClient(api.URL, false)
	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
	if err != nil || reason != "" {
		t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want advisory merge", reason, err)
	}
	if event.MergeSHA != "merge7" || merges != 1 {
		t.Fatalf("event = %+v, merges = %d; want one merge under advisory mode", event, merges)
	}
}

// The gate uses the SAME refusal predicate as the human merge lane
// (intent.Verdict.BlocksMerge) so the two lanes cannot drift apart.
func TestSelfAuthoredSweepIntentGateMatchesHumanLanePredicate(t *testing.T) {
	class := intent.Classify(intent.PR{Title: "fix widget", Files: []intent.ChangedFile{{Filename: testIntentSourceFile, Status: "modified", Additions: 1}}, AgentAuthor: true}, intent.Config{})
	if class.Tier != intent.Tier1 {
		t.Fatalf("classification = %+v, want Tier1", class)
	}
	verdict := intent.EvaluateForAppSelfMerge(class, intent.Evidence{}, true)
	if !verdict.BlocksMerge(true) {
		t.Fatalf("verdict = %+v, want BlocksMerge(true) for tier1 without a linked issue", verdict)
	}
	if verdict.BlocksMerge(false) {
		t.Fatalf("verdict = %+v, want advisory (no block) when enforcement is off", verdict)
	}
}

// Advisory-mode parity with writeMergeEligible (#6258, #6308): when
// intent.enforce is off, the gate must not withhold a merge even when the
// tier cannot be established. The human lane logs and proceeds when its
// evidence is missing; a self-authored sweep that failed closed here would
// be STRICTER than the human lane, which the gate's contract forbids. The
// invariant is the merge count: exactly one PUT .../merge, not zero.
func TestSelfAuthoredSweepIntentGateAdvisoryWhenEvidenceUnavailable(t *testing.T) {
	tests := []struct {
		name string
		fx   selfSweepFixture
	}{
		{name: "files API error", fx: selfSweepFixture{files: []string{testIntentSourceFile}, filesHTTPCode: http.StatusInternalServerError}},
		{name: "incomplete file list", fx: selfSweepFixture{files: []string{testIntentSourceFile}, changedFiles: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merges := 0
			tt.fx.mergeCalls = &merges
			tt.fx.mergeApplied = true
			api := newSelfSweepGuardAPI(t, tt.fx)
			defer api.Close()
			c := newIntentGateSweepClient(api.URL, false)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
			if err != nil || reason != "" {
				t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want advisory merge to proceed when evidence is unavailable", reason, err)
			}
			if event.MergeSHA != "merge7" || merges != 1 {
				t.Fatalf("event = %+v, merges = %d; want exactly one merge: advisory mode must warn and continue, not fail closed", event, merges)
			}
		})
	}
}

// An installed gate whose Enforce callback is nil is advisory by contract
// ("nil is treated as false"): a tier the enforced gate refuses still merges.
func TestSelfAuthoredSweepIntentGateNilEnforceIsAdvisory(t *testing.T) {
	merges := 0
	fx := selfSweepFixture{files: []string{testIntentSourceFile}, mergeApplied: true, mergeCalls: &merges}
	api := newSelfSweepGuardAPI(t, fx)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	c.SetIntentGate(&IntentGate{})
	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
	if err != nil || reason != "" {
		t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want advisory merge with a nil Enforce callback", reason, err)
	}
	if event.MergeSHA != "merge7" || merges != 1 {
		t.Fatalf("event = %+v, merges = %d; want one merge: a nil Enforce must read as enforce=off", event, merges)
	}
}

// No policy installed means no intent tier gate at all: the sweep behaves
// exactly as it did before #6258. Removing the gate with SetIntentGate(nil)
// restores that behaviour on a live Engine. The fixture's files API is set
// to error so the test also proves the files list is never consulted - an
// enforced gate on this fixture fails closed (see
// TestSelfAuthoredSweepIntentGateFailsClosedWithoutEvidence), so a merge
// here can only mean the gate was not in the path.
func TestSelfAuthoredSweepWithoutIntentGateMergesWithoutConsultingFiles(t *testing.T) {
	merges := 0
	fx := selfSweepFixture{files: []string{testIntentSourceFile}, filesHTTPCode: http.StatusInternalServerError, mergeApplied: true, mergeCalls: &merges}
	api := newSelfSweepGuardAPI(t, fx)
	defer api.Close()
	c := newIntentGateSweepClient(api.URL, true)
	c.SetIntentGate(nil)
	if got := c.currentIntentGate(); got != nil {
		t.Fatalf("currentIntentGate() after SetIntentGate(nil) = %+v, want nil", got)
	}
	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
	if err != nil || reason != "" {
		t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want a clean merge with no gate installed", reason, err)
	}
	if event.MergeSHA != "merge7" || merges != 1 {
		t.Fatalf("event = %+v, merges = %d; want exactly one merge: no policy means the pre-#6258 sweep", event, merges)
	}
}

// Options.IntentGate installs the gate at construction; it is the same gate
// currentIntentGate later hands to the sweep.
func TestNewEngineInstallsOptionsIntentGate(t *testing.T) {
	gate := &IntentGate{Enforce: func() bool { return true }}
	c := New(nil, Options{IntentGate: gate})
	if got := c.currentIntentGate(); got != gate {
		t.Fatalf("currentIntentGate() = %p, want the Options.IntentGate %p", got, gate)
	}
	if !gate.enforced() {
		t.Fatal("enforced() = false for a gate whose Enforce returns true")
	}
}

// A nil *Engine is nil-safe for the gate accessors, matching the Engine's
// warn/info helpers: installing on nil is a no-op and reading yields no
// policy, so callers holding an unconfigured engine never panic.
func TestIntentGateAccessorsNilEngineSafe(t *testing.T) {
	var c *Engine
	c.SetIntentGate(&IntentGate{Enforce: func() bool { return true }})
	if got := c.currentIntentGate(); got != nil {
		t.Fatalf("(*Engine)(nil).currentIntentGate() = %+v, want nil", got)
	}
	var g *IntentGate
	if g.enforced() {
		t.Fatal("(*IntentGate)(nil).enforced() = true, want false: no gate means no enforcement")
	}
}

// ListChangedFiles refuses to classify without a client or PR: a nil input
// must surface as an error the gate fails closed on, never as an empty
// (and therefore Tier0-looking) file list.
func TestListChangedFilesRefusesNilClientOrPR(t *testing.T) {
	api := newSelfSweepGuardAPI(t, selfSweepFixture{files: []string{testIntentSourceFile}})
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	tests := []struct {
		name   string
		client *gh.Client
		pr     *gh.PullRequest
	}{
		{name: "nil client", client: nil, pr: &gh.PullRequest{Number: gh.Ptr(7)}},
		{name: "nil PR", client: c.gh, pr: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, err := ListChangedFiles(context.Background(), tt.client, "acme", "widget", tt.pr)
			if err == nil {
				t.Fatal("err = nil, want an error so the tier is treated as unknown")
			}
			if files != nil {
				t.Fatalf("files = %v, want nil: a nil input must not look like an empty (Tier0) change", files)
			}
		})
	}
}

// ListChangedFiles pages through the files API so a PR with more changed
// files than one page returns the COMPLETE list - a single page would trip
// the incomplete-list refusal for every large PR and fail every self-merge
// closed.
func TestListChangedFilesPagesThroughFilesAPI(t *testing.T) {
	var base string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/pulls/7/files" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", `<`+base+`/repos/acme/widget/pulls/7/files?page=2>; rel="next"`)
			json.NewEncoder(w).Encode([]map[string]any{{"filename": testIntentSourceFile, "status": "modified", "additions": 1, "deletions": 2}})
		case "2":
			json.NewEncoder(w).Encode([]map[string]any{{"filename": testIntentDocsFile, "status": "added", "additions": 3, "deletions": 0}})
		default:
			t.Fatalf("unexpected page: %s", r.URL.RawQuery)
		}
	}))
	defer api.Close()
	base = api.URL

	c := newAutoMergeSweepClient(api.URL)
	pr := &gh.PullRequest{Number: gh.Ptr(7), ChangedFiles: gh.Ptr(2)}
	files, err := ListChangedFiles(context.Background(), c.gh, "acme", "widget", pr)
	if err != nil {
		t.Fatalf("ListChangedFiles() error = %v, want nil for a complete two-page list", err)
	}
	want := []intent.ChangedFile{
		{Filename: testIntentSourceFile, Status: "modified", Additions: 1, Deletions: 2},
		{Filename: testIntentDocsFile, Status: "added", Additions: 3, Deletions: 0},
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("ListChangedFiles() = %+v, want both pages in order %+v", files, want)
	}
}
