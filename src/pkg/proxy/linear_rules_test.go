package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// allModes is every ACMM tier, lowest first. Tests that assert "at every tier"
// range over this rather than naming tiers, so a future fifth mode is covered
// automatically instead of silently escaping the assertion.
var allModes = []agent.AgentMode{
	agent.ModeAdvisory,
	agent.ModeIssuesOnly,
	agent.ModeIssuesAndPRs,
	agent.ModeIssuesPRsMerge,
}

// linearBody builds a Linear GraphQL request body.
func linearBody(query string) []byte {
	b, err := json.Marshal(linearRequest{Query: query})
	if err != nil {
		panic(err)
	}
	return b
}

// decide is the common call shape: no capabilities, untruncated.
func decide(mode agent.AgentMode, query string) LinearDecision {
	return LinearGraphQLAllowed(mode, agent.AgentCapabilities{}, linearBody(query), false)
}

// newLinearTestRequest builds a POST /graphql request carrying body, shaped as
// enforceLinear sees it after TLS termination: a path, not an absolute URL.
func newLinearTestRequest(body []byte) *http.Request {
	req, err := http.NewRequest(http.MethodPost, "https://"+linearHost+linearGraphQLPath, bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	return req
}

// ─────────────────────────────────────────────────────────────────────────────
// agentActivityCreate must be reachable at EVERY tier.
//
// This is the invariant with the quietest failure mode. Linear drops an agent
// session that is not acknowledged within 10s, so a regression that lifts this
// above ADVISORY does not produce an error anyone reads — every Linear session
// for an advisory agent just silently dies.
// ─────────────────────────────────────────────────────────────────────────────

func TestLinearAgentActivityCreateReachableAtEveryTier(t *testing.T) {
	const q = `mutation { agentActivityCreate(input: {content: "thinking"}) { success } }`

	for _, mode := range allModes {
		d := decide(mode, q)
		if !d.Allowed {
			t.Errorf("agentActivityCreate DENIED at %s (reason %q) — Linear sessions require an ack within 10s at every tier", mode, d.Reason)
		}
		if !d.IsMutation {
			t.Errorf("agentActivityCreate at %s: expected IsMutation=true", mode)
		}
	}

	// Positive control for the assertion above: the LOWEST tier is genuinely
	// the lowest, and the gate is capable of denying at it. Without this, a
	// gate that allowed everything would pass the loop.
	if d := decide(agent.ModeAdvisory, `mutation { issueUpdate(id: "x", input: {}) { success } }`); d.Allowed {
		t.Fatal("positive control failed: issueUpdate allowed at ADVISORY — the gate is not denying anything, so the test above proves nothing")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// issueUpdate: denied below ISSUES_ONLY, permitted at and above it.
// ─────────────────────────────────────────────────────────────────────────────

func TestLinearIssueUpdateRequiresIssuesOnly(t *testing.T) {
	const q = `mutation { issueUpdate(id: "abc", input: {title: "t"}) { success } }`

	if d := decide(agent.ModeAdvisory, q); d.Allowed {
		t.Error("issueUpdate must be DENIED at ADVISORY")
	}

	for _, mode := range []agent.AgentMode{agent.ModeIssuesOnly, agent.ModeIssuesAndPRs, agent.ModeIssuesPRsMerge} {
		if d := decide(mode, q); !d.Allowed {
			t.Errorf("issueUpdate must be ALLOWED at %s, got reason %q", mode, d.Reason)
		}
	}
}

// issueUpdate is artifact production, not conversation, so `converse` must NOT
// unlock it. This is the distinction Part 1 (#4515) drew, and the one most
// likely to be eroded by someone "fixing" a Linear agent that cannot edit.
func TestLinearConverseDoesNotGrantIssueMutation(t *testing.T) {
	caps := agent.AgentCapabilities{Converse: true}
	const q = `mutation { issueUpdate(id: "abc", input: {title: "t"}) { success } }`

	if d := LinearGraphQLAllowed(agent.ModeAdvisory, caps, linearBody(q), false); d.Allowed {
		t.Error("converse must NOT grant issueUpdate — editing an issue is not conversation")
	}

	// Positive control: the same capability at the same mode DOES open the
	// conversational mutation, so the denial above is about the operation and
	// not about converse being inert.
	const comment = `mutation { commentCreate(input: {body: "hi"}) { success } }`
	if d := LinearGraphQLAllowed(agent.ModeAdvisory, caps, linearBody(comment), false); !d.Allowed {
		t.Fatalf("positive control failed: converse did not grant commentCreate at ADVISORY (reason %q) — the denial above may be vacuous", d.Reason)
	}
	// …and without the capability that same comment is refused at ADVISORY.
	if d := decide(agent.ModeAdvisory, comment); d.Allowed {
		t.Error("commentCreate at ADVISORY without converse must be denied")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fail-closed on anything unclassifiable, with a positive control so the suite
// cannot pass by denying everything.
// ─────────────────────────────────────────────────────────────────────────────

func TestLinearFailsClosedOnUnclassifiable(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"unknown mutation", linearBody(`mutation { issueDelete(id: "x") { success } }`)},
		{"destructive mutation", linearBody(`mutation { organizationDelete { success } }`)},
		{"admin mutation", linearBody(`mutation { apiKeyCreate(input: {}) { success } }`)},
		{"not JSON", []byte(`this is not json`)},
		{"empty body", []byte(``)},
		{"empty query", linearBody(``)},
		{"unbalanced braces", linearBody(`mutation { commentCreate(input: {body: "x"}) { success }`)},
		{"unterminated string", linearBody(`mutation { commentCreate(input: {body: "oops) { success } }`)},
		{"two operations", linearBody(`mutation A { commentCreate { success } } mutation B { issueUpdate { success } }`)},
		{"fragment spread", linearBody(`mutation { commentCreate(input: {}) { ...CommentFields } }`)},
		{"no selection", linearBody(`mutation { }`)},
	}

	// Every one of these is denied even at the HIGHEST tier: an unclassifiable
	// document is not a permissions question, so no amount of mode fixes it.
	for _, tc := range cases {
		d := LinearGraphQLAllowed(agent.ModeIssuesPRsMerge, agent.AgentCapabilities{Converse: true}, tc.body, false)
		if d.Allowed {
			t.Errorf("%s: must be DENIED (fail-closed), was allowed", tc.name)
		}
		if d.Reason == "" {
			t.Errorf("%s: denial must carry a reason", tc.name)
		}
	}

	// POSITIVE CONTROL. A known operation at a sufficient tier is ALLOWED, so
	// the denials above are the gate discriminating rather than refusing
	// unconditionally. Without this, deleting the whole allowlist would still
	// pass every assertion in this function.
	if d := decide(agent.ModeIssuesOnly, `mutation { issueUpdate(id: "x", input: {}) { success } }`); !d.Allowed {
		t.Fatalf("positive control failed: known operation at sufficient tier was denied (%q) — the fail-closed assertions above are vacuous", d.Reason)
	}
	if d := decide(agent.ModeAdvisory, `query { issues { nodes { id } } }`); !d.Allowed {
		t.Fatalf("positive control failed: a plain read was denied (%q)", d.Reason)
	}
}

// A body that hit the inspection cap cannot be classified — the forbidden
// mutation could be in the part that was cut off.
func TestLinearTruncatedBodyDenied(t *testing.T) {
	body := linearBody(`mutation { agentActivityCreate(input: {}) { success } }`)

	if d := LinearGraphQLAllowed(agent.ModeIssuesPRsMerge, agent.AgentCapabilities{}, body, true); d.Allowed {
		t.Error("a truncated body must be denied — its tail may hide a forbidden mutation")
	}
	// Positive control: the identical body untruncated is allowed, so the
	// denial is attributable to truncation alone.
	if d := LinearGraphQLAllowed(agent.ModeIssuesPRsMerge, agent.AgentCapabilities{}, body, false); !d.Allowed {
		t.Fatalf("positive control failed: untruncated body denied (%q)", d.Reason)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Batching: the request needs the UNION of its operations' requirements.
// ─────────────────────────────────────────────────────────────────────────────

func TestLinearBatchedDocumentDeniedAsAWhole(t *testing.T) {
	// agentActivityCreate is permitted at ADVISORY; issueUpdate is not.
	// Authorizing on the first operation found would let the second ride in.
	const mixed = `mutation {
		agentActivityCreate(input: {content: "ack"}) { success }
		issueUpdate(id: "x", input: {title: "t"}) { success }
	}`

	if d := LinearGraphQLAllowed(agent.ModeAdvisory, agent.AgentCapabilities{Converse: true}, linearBody(mixed), false); d.Allowed {
		t.Error("a batch mixing a permitted and a forbidden mutation must be DENIED as a whole")
	}

	// Positive controls, both halves:
	// (a) the permitted half alone at that tier IS allowed, so the denial is
	//     caused by the forbidden half and not by batching being rejected
	//     outright or by the tier being too low for anything.
	if d := LinearGraphQLAllowed(agent.ModeAdvisory, agent.AgentCapabilities{Converse: true},
		linearBody(`mutation { agentActivityCreate(input: {content: "ack"}) { success } }`), false); !d.Allowed {
		t.Fatalf("positive control failed: permitted half alone was denied (%q)", d.Reason)
	}
	// (b) the SAME batch at a tier sufficient for both IS allowed, proving the
	//     gate takes the union rather than rejecting every multi-field document.
	if d := LinearGraphQLAllowed(agent.ModeIssuesOnly, agent.AgentCapabilities{Converse: true}, linearBody(mixed), false); !d.Allowed {
		t.Fatalf("positive control failed: batch denied at a tier sufficient for BOTH operations (%q) — the gate is rejecting batching itself, not taking the union", d.Reason)
	}
}

// The union must hold regardless of which order the operations appear in — a
// forbidden operation must not become permitted by being listed second (or
// first).
func TestLinearBatchOrderDoesNotMatter(t *testing.T) {
	forward := `mutation { agentActivityCreate(input: {}) { success } issueUpdate(id: "x", input: {}) { success } }`
	reverse := `mutation { issueUpdate(id: "x", input: {}) { success } agentActivityCreate(input: {}) { success } }`

	for _, q := range []string{forward, reverse} {
		if d := LinearGraphQLAllowed(agent.ModeAdvisory, agent.AgentCapabilities{Converse: true}, linearBody(q), false); d.Allowed {
			t.Errorf("batch must be denied at ADVISORY regardless of operation order: %s", q)
		}
	}
}

// An oversized batch is refused rather than classified, so classification
// cannot be made expensive.
func TestLinearOversizedBatchDenied(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("mutation {")
	for i := 0; i < linearMaxOperations+5; i++ {
		sb.WriteString(` agentActivityCreate(input: {}) { success }`)
	}
	sb.WriteString(" }")

	if d := decide(agent.ModeIssuesPRsMerge, sb.String()); d.Allowed {
		t.Error("a batch exceeding linearMaxOperations must be denied even though every operation in it is individually permitted")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Aliases must not evade classification.
// ─────────────────────────────────────────────────────────────────────────────

func TestLinearAliasedMutationsDoNotEvadeClassification(t *testing.T) {
	// An alias renames the RESPONSE key only. Classifying on the alias would
	// let any mutation be disguised as a permitted one — here, issueUpdate
	// wearing agentActivityCreate's name.
	const disguised = `mutation { agentActivityCreate: issueUpdate(id: "x", input: {}) { success } }`
	dis := decide(agent.ModeAdvisory, disguised)
	if dis.Allowed {
		t.Error("an aliased issueUpdate must be classified as issueUpdate, not as its alias")
	}
	if len(dis.Operations) != 1 || dis.Operations[0] != "issueUpdate" {
		t.Errorf("expected the resolved field name issueUpdate, got %v", dis.Operations)
	}

	// The converse direction: a legitimately aliased permitted mutation still
	// resolves and is still allowed — aliasing is valid GraphQL and must not
	// itself be a denial reason.
	const legit = `mutation { ack: agentActivityCreate(input: {}) { success } }`
	got := decide(agent.ModeAdvisory, legit)
	if !got.Allowed {
		t.Fatalf("positive control failed: a legitimately aliased agentActivityCreate was denied (%q)", got.Reason)
	}
	if len(got.Operations) != 1 || got.Operations[0] != "agentActivityCreate" {
		t.Errorf("alias must resolve to the real field, got %v", got.Operations)
	}

	// An alias in a batch must resolve too, so it cannot smuggle a forbidden
	// operation past the union check.
	const smuggle = `mutation { ack: agentActivityCreate(input: {}) { success } thought: issueUpdate(id: "x", input: {}) { success } }`
	if d := LinearGraphQLAllowed(agent.ModeAdvisory, agent.AgentCapabilities{Converse: true}, linearBody(smuggle), false); d.Allowed {
		t.Error("an aliased forbidden mutation in a batch must still be caught by the union check")
	}
}

// A mutation name appearing inside a string argument is not a selected field
// and must not be classified as one.
func TestLinearOperationNameInStringArgumentIsNotAField(t *testing.T) {
	// The body text mentions issueUpdate; the only real mutation is a comment.
	const q = `mutation { commentCreate(input: {body: "I would issueUpdate this"}) { success } }`
	got := LinearGraphQLAllowed(agent.ModeAdvisory, agent.AgentCapabilities{Converse: true}, linearBody(q), false)
	if !got.Allowed {
		t.Errorf("a mutation name inside a string argument must not be treated as a selection (reason %q)", got.Reason)
	}
	for _, op := range got.Operations {
		if op == "issueUpdate" {
			t.Error("issueUpdate was extracted from a string literal — the argument skipper is not honouring quotes")
		}
	}
}

// Reads sit at ADVISORY, matching the pre-existing ungated enumeration path.
func TestLinearReadsAllowedAtAdvisory(t *testing.T) {
	for _, q := range []string{
		`query { issues { nodes { id title } } }`,
		`query Issues($n: Int) { issues(first: $n) { nodes { id } } }`,
		`{ viewer { id } }`, // anonymous shorthand is a query by definition
	} {
		got := decide(agent.ModeAdvisory, q)
		if !got.Allowed {
			t.Errorf("read must be allowed at ADVISORY: %s (reason %q)", q, got.Reason)
		}
		if got.IsMutation {
			t.Errorf("read misclassified as a mutation: %s", q)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// No request body content in logs.
// ─────────────────────────────────────────────────────────────────────────────

// TestLinearDenialLogsNoBodyContent drives a real denial through the proxy's
// logger and asserts that nothing from the request body reaches the output.
// Linear documents carry issue text and, in variables, potentially tokens.
func TestLinearDenialLogsNoBodyContent(t *testing.T) {
	var buf bytes.Buffer
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(&buf, nil))}

	// Distinctive markers in every place a body can carry content.
	const secretBody = "SUPERSECRET-COMMENT-TEXT"
	const secretVar = "SUPERSECRET-TOKEN-VALUE"
	const secretTitle = "SUPERSECRET-ISSUE-TITLE"

	raw, err := json.Marshal(map[string]any{
		"query":     `mutation { issueDelete(id: "x", title: "` + secretTitle + `", body: "` + secretBody + `") { success } }`,
		"variables": map[string]any{"token": secretVar},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := newLinearTestRequest(raw)
	blocked, reason := p.enforceLinear(req, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})
	if !blocked {
		t.Fatal("expected the request to be blocked")
	}

	// The 403 reason the agent sees, and the log line, must both be clean.
	for _, secret := range []string{secretBody, secretVar, secretTitle} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("request body content %q leaked into the log output:\n%s", secret, buf.String())
		}
		if strings.Contains(reason, secret) {
			t.Errorf("request body content %q leaked into the agent-facing block reason: %s", secret, reason)
		}
	}

	// Positive control: the log line was actually produced and is about this
	// request. Otherwise a logger that emitted NOTHING would pass the above.
	if !strings.Contains(buf.String(), "linear") {
		t.Fatalf("positive control failed: no linear log line was emitted, so the leak assertions are vacuous:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "scanner") {
		t.Error("the log line should identify the agent")
	}
}

// The operation NAME is schema vocabulary, not user content, and IS logged —
// it is what makes a denial debuggable.
func TestLinearDenialLogsOperationName(t *testing.T) {
	var buf bytes.Buffer
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(&buf, nil))}

	req := newLinearTestRequest(linearBody(`mutation { issueUpdate(id: "x", input: {}) { success } }`))
	if blocked, _ := p.enforceLinear(req, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{}); !blocked {
		t.Fatal("expected a block")
	}
	if !strings.Contains(buf.String(), "issueUpdate") {
		t.Errorf("the denied operation name should be logged for debuggability:\n%s", buf.String())
	}
}

// An allowed request must forward with its body intact — inspection must not
// consume it.
func TestLinearAllowedRequestBodyIsRestored(t *testing.T) {
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	body := linearBody(`mutation { agentActivityCreate(input: {content: "ack"}) { success } }`)

	req := newLinearTestRequest(body)
	blocked, _ := p.enforceLinear(req, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})
	if blocked {
		t.Fatal("expected the request to be allowed")
	}

	got := make([]byte, len(body))
	n, _ := req.Body.Read(got)
	if !bytes.Equal(got[:n], body) {
		t.Errorf("body was not restored for forwarding:\ngot  %s\nwant %s", got[:n], body)
	}
	if req.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", req.ContentLength, len(body))
	}
}

// Any path other than /graphql on the Linear host is refused: Linear exposes
// exactly one endpoint, so anything else is a shape this gate cannot reason
// about.
func TestLinearNonGraphQLPathDenied(t *testing.T) {
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}

	for _, tc := range []struct{ method, path string }{
		{"GET", "/graphql"},
		{"POST", "/oauth/token"},
		{"DELETE", "/graphql"},
	} {
		req := newLinearTestRequest(linearBody(`query { viewer { id } }`))
		req.Method = tc.method
		req.URL.Path = tc.path
		if blocked, _ := p.enforceLinear(req, "scanner", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{}); !blocked {
			t.Errorf("%s %s on the Linear host must be denied", tc.method, tc.path)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Host routing.
// ─────────────────────────────────────────────────────────────────────────────

func TestLinearHostIsInspected(t *testing.T) {
	if !NeedsMITM(linearHost) {
		t.Error("api.linear.app must need MITM — otherwise agent writes to Linear tunnel out ungated")
	}
	// The real regression risk: NeedsMITM alone is inert, because the CONNECT
	// seams used to gate on IsGitHubHost first. NeedsInspection is what the
	// call sites ask.
	if !NeedsInspection(linearHost) {
		t.Error("api.linear.app must be inspected — NeedsMITM alone is not enough, the CONNECT seams ask NeedsInspection")
	}
	if IsGitHubHost(linearHost) {
		t.Error("api.linear.app must not be classified as a GitHub host")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GitHub behaviour is UNCHANGED. This gate must not regress the GitHub path.
// ─────────────────────────────────────────────────────────────────────────────

func TestGitHubGatingUnchangedByLinearGate(t *testing.T) {
	// Host classification.
	if !NeedsMITM("api.github.com") {
		t.Error("api.github.com must still need MITM")
	}
	if !NeedsInspection("api.github.com") {
		t.Error("api.github.com must still be inspected")
	}
	if NeedsMITM("github.com") {
		t.Error("github.com must still NOT be MITM'd — OAuth device flow and git smart HTTP depend on an opaque tunnel")
	}
	if NeedsInspection("github.com") {
		t.Error("github.com must still NOT be inspected")
	}
	if NeedsInspection("example.com") {
		t.Error("an unrelated host must not be inspected")
	}

	// The REST tier table still answers exactly as before.
	restCases := []struct {
		mode   agent.AgentMode
		method string
		path   string
		want   bool
	}{
		{agent.ModeAdvisory, "GET", "/repos/o/r/issues", true},
		{agent.ModeAdvisory, "POST", "/repos/o/r/issues", false},
		{agent.ModeIssuesOnly, "POST", "/repos/o/r/issues", true},
		{agent.ModeIssuesOnly, "POST", "/repos/o/r/issues/1/comments", true},
		{agent.ModeAdvisory, "POST", "/repos/o/r/issues/1/comments", false},
		{agent.ModeIssuesOnly, "PATCH", "/repos/o/r/pulls/1", false},
		{agent.ModeIssuesAndPRs, "PATCH", "/repos/o/r/pulls/1", true},
		// Hard denies still win at the highest tier.
		{agent.ModeIssuesPRsMerge, "POST", "/repos/o/r/pulls", false},
		{agent.ModeIssuesPRsMerge, "PUT", "/repos/o/r/pulls/1/merge", false},
		// Deny-by-default still holds.
		{agent.ModeIssuesPRsMerge, "POST", "/some/unknown/route", false},
	}
	for _, tc := range restCases {
		if got := AllowedByMode(tc.mode, tc.method, tc.path); got != tc.want {
			t.Errorf("AllowedByMode(%s, %s, %s) = %v, want %v", tc.mode, tc.method, tc.path, got, tc.want)
		}
	}

	// GitHub's GraphQL classifier keeps its own posture — notably the
	// permissive default for an unrecognised mutation, which is correct there
	// because the REST table is the primary gate. The Linear gate's
	// fail-closed posture must NOT have been applied to it.
	unknownGitHub := []byte(`{"query":"mutation { someUnknownGitHubMutation { id } }"}`)
	if allowed, isMutation := GraphQLAllowed(agent.ModeIssuesOnly, unknownGitHub); !allowed || !isMutation {
		t.Errorf("GitHub GraphQL classifier changed: unknown mutation at ISSUES_ONLY = (allowed %v, mutation %v), want (true, true)", allowed, isMutation)
	}
	// And its merge escalation still applies.
	mergeQ := []byte(`{"query":"mutation { mergePullRequest(input: {}) { clientMutationId } }"}`)
	if allowed, _ := GraphQLAllowed(agent.ModeIssuesAndPRs, mergeQ); allowed {
		t.Error("GitHub GraphQL merge escalation regressed: mergePullRequest allowed below ISSUES_PRS_MERGE")
	}
	if allowed, _ := GraphQLAllowed(agent.ModeIssuesPRsMerge, mergeQ); !allowed {
		t.Error("GitHub GraphQL merge regressed: mergePullRequest denied at ISSUES_PRS_MERGE")
	}
	// A GitHub read is still a read.
	if allowed, isMutation := GraphQLAllowed(agent.ModeAdvisory, []byte(`{"query":"query { viewer { login } }"}`)); !allowed || isMutation {
		t.Error("GitHub GraphQL read classification regressed")
	}
}

// The Linear allowlist must never be consulted for GitHub traffic, and vice
// versa: the two gates are independent, and a Linear operation name arriving on
// the GitHub path (or the reverse) must not be granted by the wrong table.
func TestLinearAndGitHubGatesAreIndependent(t *testing.T) {
	// A Linear-only mutation name sent to GitHub's classifier falls into its
	// permissive default at ISSUES_ONLY — unchanged GitHub behaviour, and NOT
	// the Linear allowlist's ADVISORY grant.
	if allowed, _ := GraphQLAllowed(agent.ModeAdvisory, []byte(`{"query":"mutation { agentActivityCreate { success } }"}`)); allowed {
		t.Error("agentActivityCreate must NOT be granted at ADVISORY on the GitHub path — that grant is Linear-specific")
	}

	// A GitHub mutation name sent to the Linear gate is not in the Linear
	// allowlist, so it is denied even at the top tier.
	if got := decide(agent.ModeIssuesPRsMerge, `mutation { addComment(input: {}) { clientMutationId } }`); got.Allowed {
		t.Error("a GitHub mutation name must not be granted by the Linear allowlist")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// agentSessionUpdate carries the same 10-second invariant as
// agentActivityCreate: an agent that cannot set externalUrls/plan on its own
// session cannot maintain session presence, so it must be reachable at every
// tier (RFC #4492 Part 2, component C).
// ─────────────────────────────────────────────────────────────────────────────

func TestLinearAgentSessionUpdateReachableAtEveryTier(t *testing.T) {
	const q = `mutation { agentSessionUpdate(id: "s1", input: {externalUrls: []}) { success } }`
	for _, mode := range allModes {
		d := decide(mode, q)
		if !d.Allowed {
			t.Errorf("agentSessionUpdate DENIED at %s (reason %q)", mode, d.Reason)
		}
		if !d.IsMutation {
			t.Errorf("agentSessionUpdate at %s: expected IsMutation=true", mode)
		}
	}

	// Batching it with a forbidden mutation must still deny the whole
	// document — session presence is not a smuggling vehicle.
	batch := `mutation { agentSessionUpdate(id: "s1", input: {}) { success } issueUpdate(id: "x", input: {}) { success } }`
	if d := decide(agent.ModeAdvisory, batch); d.Allowed {
		t.Error("agentSessionUpdate + issueUpdate batch allowed at ADVISORY")
	}
}
