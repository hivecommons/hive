package proxy

import (
	"encoding/json"
	"strings"

	"github.com/hivecommons/hive/pkg/agent"
)

// Linear ACMM enforcement (#4492, component F).
//
// This is the first non-GitHub host hive gates on an agent's behalf, and it
// does not fit the rule table in rules.go at all. That table keys on
// (method, path): every GitHub operation has a distinct route, so the tuple
// alone identifies what an agent is trying to do. Linear is a single
// `POST /graphql` — reading an issue, commenting on it, relabelling it and
// acknowledging an agent session are the SAME tuple. A (method, path) rule for
// Linear could only say "all of it" or "none of it", and both are wrong.
//
// So enforcement here is by GraphQL operation, and the posture is inverted
// relative to GitHub's classifier. GraphQLAllowedCaps escalates on substring
// match and ends in a permissive `default:` — an unrecognised GitHub mutation
// lands at ModeIssuesOnly rather than being refused, which is defensible
// because GitHub's REST table is the primary gate and GraphQL is the secondary
// one. Linear has no primary gate. There is no route table underneath to catch
// what the classifier misses, so an operation this file does not recognise is
// DENIED. See linearMutations and LinearGraphQLAllowed.

// linearHost is Linear's API host. Linear serves its entire API from a single
// GraphQL endpoint on this host.
const linearHost = "api.linear.app"

// linearGraphQLPath is the only path Linear's API exposes.
const linearGraphQLPath = "/graphql"

// linearBodyLimit caps how much of a Linear request body is read for
// inspection. Distinct from graphQLBodyLimit and deliberately smaller: a
// legitimate Linear mutation is a handful of fields plus comment markdown,
// nowhere near this. The cap is what stops a hostile or runaway agent from
// making the classifier itself the DoS — the proxy must not buffer an
// unbounded document per request just to decide whether to refuse it.
//
// A body that hits the cap is refused rather than classified on its prefix: a
// truncated document is exactly the "cannot classify" case, and a truncated
// prefix could hide a forbidden mutation past the cut.
const linearBodyLimit = 32 * 1024

// linearMaxOperations bounds how many top-level mutation fields one document
// may select. Batching is legitimate — Linear's own client batches — but an
// unbounded batch is a way to spend proxy CPU on classification. Well past any
// real client's batch size, so this only ever trips on abuse.
const linearMaxOperations = 32

// linearRequirement is what an agent must hold to run one Linear operation.
//
// The two fields are an OR, mirroring ProxyRule: an operation is permitted when
// the agent holds the capability OR reaches the tier. That is the same shape
// Part 1 (#4515) established for GitHub, and for the same reason — a capability
// widens, it never narrows.
type linearRequirement struct {
	// MinMode is the tier that grants this operation without any capability.
	MinMode agent.AgentMode

	// Capability, when non-nil, is an additional grant path.
	Capability func(agent.AgentCapabilities) bool
}

// linearReadRequirement is the requirement for a read-only Linear query.
// Reads sit at ADVISORY, matching the GitHub catch-all GET rule and the
// existing read path in pkg/worksource/linear.go, which #4178 shipped
// ungated — this file must not retroactively break backlog enumeration.
var linearReadRequirement = linearRequirement{MinMode: agent.ModeAdvisory}

// linearMutations is the ALLOWLIST of Linear mutations hive recognises, and the
// requirement for each. An operation absent from this map is denied at every
// tier — that is the fail-closed posture, and it is why this is a map lookup
// rather than a regex classifier with a fallthrough.
//
// Tiers are derived from the GitHub operation each Linear mutation is the
// analogue of, so an agent's autonomy does not silently depend on which tracker
// its work came from:
//
//   - agentActivityCreate is the ONLY mutation at ModeAdvisory. Linear requires
//     an AgentActivity of type `thought` within 10 seconds of session creation
//     or the session is dropped, so this is not a write in the ACMM sense — it
//     is how an agent acknowledges that it exists at all. Gating it above
//     ADVISORY would mean an advisory agent could be assigned Linear work and
//     then be unable to admit it received it. It carries `converse` too, but
//     the ADVISORY floor is what actually makes it reachable everywhere; the
//     capability is recorded for intent, not because it changes reachability.
//     The RFC is explicit: "must be reachable at every tier".
//
//   - commentCreate is conversation — the exact GitHub POST /issues/{n}/comments
//     that Part 1 moved onto the converse axis. Same floor (ModeIssuesOnly),
//     same capability, so a converse-holding ADVISORY agent can reply in Linear
//     precisely where it can reply on GitHub.
//
//   - issueCreate / issueUpdate / issueLabel-shaped mutations are artifact
//     production: POST /issues, PATCH /issues/{n}, POST /issues/{n}/labels, all
//     ModeIssuesOnly with NO capability. The RFC names issueUpdate → ISSUES_ONLY
//     directly. Deliberately no converse grant — editing an issue body is not
//     conversation, which is the entire distinction Part 1 drew.
//
//   - attachment* mutations link an external artifact (a PR, a build) onto an
//     issue. That is how a PR-capable agent reports what it produced, so they
//     sit at ModeIssuesAndPRs alongside PR creation.
//
// Mutations NOT listed are denied, including every destructive one
// (issueDelete, commentDelete, projectDelete, teamDelete, issueArchive) and
// every administrative one (organizationInviteCreate, apiKeyCreate,
// webhookCreate). They are absent rather than mapped-to-a-high-tier on purpose:
// there is no hive workflow that needs an agent to delete a Linear issue or
// mint a Linear API key, so the correct tier is "none", and absence expresses
// that without inviting someone to relax it later.
var linearMutations = map[string]linearRequirement{
	// Session acknowledgement — reachable at EVERY tier. See above.
	"agentActivityCreate": {MinMode: agent.ModeAdvisory, Capability: agent.AgentCapabilities.CanConverse},

	// Session presence — same requirement as agentActivityCreate, and for the
	// same reason. agentSessionUpdate carries the session's externalUrls and
	// plan: it is how an agent points the user at where the work is happening
	// and keeps the session from being marked unresponsive. Linear documents
	// "send an activity OR update your external URL within 10 seconds", so a
	// tier that can acknowledge must also be able to do this. It cannot touch
	// issues, comments, or any other entity.
	"agentSessionUpdate": {MinMode: agent.ModeAdvisory, Capability: agent.AgentCapabilities.CanConverse},

	// Conversation.
	"commentCreate": {MinMode: agent.ModeIssuesOnly, Capability: agent.AgentCapabilities.CanConverse},

	// Artifact production on issues — no converse grant.
	"issueCreate":      {MinMode: agent.ModeIssuesOnly},
	"issueUpdate":      {MinMode: agent.ModeIssuesOnly},
	"issueAddLabel":    {MinMode: agent.ModeIssuesOnly},
	"issueRemoveLabel": {MinMode: agent.ModeIssuesOnly},

	// Linking produced artifacts (PRs, builds) onto an issue.
	"attachmentCreate":          {MinMode: agent.ModeIssuesAndPRs},
	"attachmentLinkURL":         {MinMode: agent.ModeIssuesAndPRs},
	"attachmentLinkGitHubPR":    {MinMode: agent.ModeIssuesAndPRs},
	"attachmentLinkGitHubIssue": {MinMode: agent.ModeIssuesAndPRs},
}

// IsLinearHost reports whether host is Linear's API host.
func IsLinearHost(host string) bool { return host == linearHost }

// IsLinearGraphQLPath reports whether path is Linear's GraphQL endpoint.
func IsLinearGraphQLPath(path string) bool { return path == linearGraphQLPath }

// linearRequest is the subset of a GraphQL request body this gate reads.
//
// Note what is absent: Variables. Linear mutations carry their payload —
// issue bodies, comment markdown — in variables, and this gate never needs to
// look at it. Not decoding it is deliberate: it cannot then be logged by
// accident, and it keeps the decision a pure function of the operation names.
type linearRequest struct {
	Query         string `json:"query"`
	OperationName string `json:"operationName"`
}

// LinearDecision is the outcome of inspecting one Linear GraphQL request.
type LinearDecision struct {
	// Allowed is whether the request may proceed.
	Allowed bool

	// IsMutation is whether the document mutates. Reads are not mutations.
	IsMutation bool

	// Operations are the operation names inspected, for logging. This is the
	// ONLY thing about the request that is safe to log — see Reason.
	Operations []string

	// Reason is a short, body-free explanation of a denial, suitable for both
	// the log line and the agent-facing 403. It never contains any part of the
	// request body: Linear documents carry issue content and, in variables,
	// potentially tokens. Operation NAMES are schema identifiers, not user
	// content, so those are safe and are what makes a denial debuggable.
	Reason string
}

// LinearGraphQLAllowed decides whether a Linear GraphQL request is permitted
// for the given mode and capabilities.
//
// Fail-closed at every exit: an unparseable body, an unreadable document, an
// unrecognised operation, or a batch exceeding the bound is DENIED. This is the
// opposite default from the IPv6 egress gate (#4327/#4350), and deliberately
// so. There, failing closed on an ABSENT capability broke boots on clusters
// that were fine — the gate could not observe what it needed. Here the request
// is present and fully in hand; being unable to classify it is not missing
// information about the environment, it is a document doing something this gate
// does not understand. "We could not tell, so we allowed it" would make the
// gate worthless — an agent could evade it by malforming its query.
//
// body must be the raw JSON request body, already bounded by linearBodyLimit.
// truncated reports whether the reader hit that cap.
func LinearGraphQLAllowed(mode agent.AgentMode, caps agent.AgentCapabilities, body []byte, truncated bool) LinearDecision {
	// A body that hit the read cap cannot be classified: the tail is missing,
	// and a forbidden mutation could be sitting in it.
	if truncated {
		return LinearDecision{Reason: "request body exceeds inspection limit"}
	}

	var req linearRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// Not "malformed JSON, let upstream reject it" — an agent that can get
		// an unparseable body past the gate has found the bypass.
		return LinearDecision{Reason: "unparseable GraphQL request"}
	}

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return LinearDecision{Reason: "empty GraphQL query"}
	}

	kind, fields, ok := linearOperationFields(query)
	if !ok {
		return LinearDecision{Reason: "unreadable GraphQL document"}
	}

	if kind == linearOpQuery {
		// Read-only. Linear's schema has no mutating fields on the query root,
		// so a document whose sole operation is a query cannot write.
		return LinearDecision{
			Allowed:    mode >= linearReadRequirement.MinMode,
			IsMutation: false,
			Reason:     "read requires ADVISORY",
		}
	}

	if len(fields) == 0 {
		return LinearDecision{IsMutation: true, Reason: "no operation selected"}
	}
	if len(fields) > linearMaxOperations {
		return LinearDecision{IsMutation: true, Reason: "too many operations in one document"}
	}

	// The request needs the UNION of every selected mutation's requirement, so
	// every field is evaluated and the first refusal denies the whole document.
	// Authorizing on the first field found would let a batch smuggle
	// `issueUpdate` in behind a permitted `agentActivityCreate` — the exact
	// shape the RFC warns about, and the reason this loop does not break early
	// on success.
	decision := LinearDecision{IsMutation: true, Operations: fields}
	for _, f := range fields {
		reqmt, known := linearMutations[f]
		if !known {
			// Fail closed. An unknown mutation is not assumed harmless just
			// because this gate has not heard of it; Linear ships schema
			// changes on its own cadence and a new destructive mutation must
			// not become reachable by default.
			decision.Reason = "unrecognised mutation"
			return decision
		}
		if reqmt.Capability != nil && reqmt.Capability(caps) {
			continue
		}
		if mode >= reqmt.MinMode {
			continue
		}
		decision.Reason = "insufficient mode for mutation"
		return decision
	}

	decision.Allowed = true
	return decision
}

// linearOpKind distinguishes the operation types this gate handles.
type linearOpKind int

const (
	linearOpQuery linearOpKind = iota
	linearOpMutation
)

// linearOperationFields extracts the operation kind and its top-level selected
// field names from a GraphQL document.
//
// It is a hand-written extractor rather than a real parse because no GraphQL
// parser is vendored (checked go.mod: none), and adding a parser to the tree to
// gate one host is a dependency this does not need — the extractor's contract
// is not "understand GraphQL" but "recognise a narrow shape, and refuse
// everything else", which is a much smaller thing to get right. It is modelled
// on topLevelMutationFields in rules.go, with the same string/comment/argument
// skipping and the same alias resolution, but it differs in two ways that
// matter:
//
//   - It reports the operation KIND, because Linear reads and writes share a
//     path and the read/write split cannot be inferred from anything else.
//   - Its failure mode is a refusal by the CALLER rather than a fallthrough to
//     a tier check. topLevelMutationFields returning nil means "no capability
//     grant, use the tier"; ok=false here means "deny".
//
// Returns ok=false on anything it cannot read with confidence: multiple
// operations, fragments, unbalanced delimiters, an unresolvable alias.
func linearOperationFields(query string) (linearOpKind, []string, bool) {
	// Fragments can move field selections out of the operation body, so a
	// document using them cannot be classified by reading the body alone.
	// Refuse rather than classify on an incomplete view.
	if strings.Contains(query, "fragment ") || strings.Contains(query, "...") {
		return 0, nil, false
	}

	kind, i, ok := linearOperationStart(query)
	if !ok {
		return 0, nil, false
	}

	// Skip the optional operation name and a balanced variable-definition
	// block, then find the opening brace of the selection set.
	depth := 0
	for ; i < len(query); i++ {
		switch query[i] {
		case '(':
			depth++
		case ')':
			depth--
		case '{':
			if depth == 0 {
				fields, ok := linearSelectionFields(query, i+1)
				return kind, fields, ok
			}
		}
	}
	return 0, nil, false
}

// linearOperationStart locates the document's single operation and returns its
// kind and the offset just past the keyword.
//
// Anonymous shorthand (`{ ... }`, no keyword) is treated as a query, which is
// what the GraphQL spec says it is — a mutation cannot be written in shorthand.
func linearOperationStart(query string) (linearOpKind, int, bool) {
	nMut := countGraphQLKeyword(query, "mutation")
	nQry := countGraphQLKeyword(query, "query")

	switch {
	case nMut == 1 && nQry == 0:
		return linearOpMutation, indexGraphQLKeyword(query, "mutation") + len("mutation"), true
	case nQry == 1 && nMut == 0:
		return linearOpQuery, indexGraphQLKeyword(query, "query") + len("query"), true
	case nMut == 0 && nQry == 0:
		// Anonymous shorthand — a query by definition.
		if strings.HasPrefix(query, "{") {
			return linearOpQuery, 0, true
		}
		return 0, 0, false
	default:
		// Multiple operations in one document: which one runs is decided by
		// operationName, which this extractor does not track. Refuse.
		return 0, 0, false
	}
}

// countGraphQLKeyword counts occurrences of an operation keyword appearing as a
// standalone token at brace depth zero, ignoring occurrences inside strings and
// comments. A substring count would miscount a field named `queryable` or the
// word "mutation" inside a comment body.
func countGraphQLKeyword(query, kw string) int {
	n := 0
	scanGraphQLTokens(query, func(name string, depth int) {
		if depth == 0 && name == kw {
			n++
		}
	})
	return n
}

// indexGraphQLKeyword returns the offset of the first depth-zero occurrence of
// an operation keyword, or -1.
func indexGraphQLKeyword(query, kw string) int {
	idx := -1
	scanGraphQLTokensAt(query, func(name string, depth, off int) bool {
		if depth == 0 && name == kw {
			idx = off
			return false
		}
		return true
	})
	return idx
}

func scanGraphQLTokens(query string, fn func(name string, depth int)) {
	scanGraphQLTokensAt(query, func(name string, depth, _ int) bool {
		fn(name, depth)
		return true
	})
}

// scanGraphQLTokensAt walks the document emitting each bare name token with the
// brace depth it was found at, skipping strings and comments so their contents
// are never mistaken for tokens. fn returns false to stop the walk.
func scanGraphQLTokensAt(query string, fn func(name string, depth, off int) bool) {
	depth := 0
	for i := 0; i < len(query); {
		c := query[i]
		switch {
		case c == '#':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case c == '"':
			var ok bool
			if i, ok = skipGraphQLString(query, i); !ok {
				return
			}
		case c == '{':
			depth++
			i++
		case c == '}':
			depth--
			i++
		case isGraphQLNameStart(c):
			name := graphQLIdentRe.FindString(query[i:])
			if !fn(name, depth, i) {
				return
			}
			i += len(name)
		default:
			i++
		}
	}
}

// skipGraphQLString advances past a string or block-string literal beginning at
// i. Returns ok=false on an unterminated literal, which the callers treat as an
// unreadable document.
func skipGraphQLString(query string, i int) (int, bool) {
	if strings.HasPrefix(query[i:], `"""`) {
		end := strings.Index(query[i+3:], `"""`)
		if end < 0 {
			return 0, false
		}
		return i + 3 + end + 3, true
	}
	i++ // opening quote
	for i < len(query) {
		switch query[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i + 1, true
		case '\n':
			// A newline inside a single-quoted string is invalid GraphQL.
			return 0, false
		}
		i++
	}
	return 0, false
}

// linearSelectionFields collects the field names selected directly inside a
// selection set that opens at offset i (just past its '{').
//
// Aliases are resolved to the real field name. An alias renames only the
// RESPONSE key, so `ack: issueUpdate(...)` is an issueUpdate and must be
// classified as one — reading the alias would let any mutation be disguised as
// a permitted one, which is the whole point of resolving it here.
func linearSelectionFields(query string, i int) ([]string, bool) {
	var fields []string
	braces := 1
	for i < len(query) {
		c := query[i]
		switch {
		case c == '#':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case c == '"':
			var ok bool
			if i, ok = skipGraphQLString(query, i); !ok {
				return nil, false
			}
		case c == '{':
			braces++
			i++
		case c == '}':
			braces--
			if braces == 0 {
				return fields, true
			}
			i++
		case c == '@':
			// A directive is not a field selection; step past '@' and its name.
			i++
			i += len(graphQLIdentRe.FindString(query[i:]))
		case c == '(':
			var ok bool
			if i, ok = skipGraphQLArgs(query, i); !ok {
				return nil, false
			}
		case c == '$':
			// A variable reference, not a field.
			i++
			i += len(graphQLIdentRe.FindString(query[i:]))
		case braces == 1 && isGraphQLNameStart(c):
			name := graphQLIdentRe.FindString(query[i:])
			i += len(name)
			j := skipGraphQLSpace(query, i)
			if j < len(query) && query[j] == ':' {
				// Alias — the real field follows the colon.
				j = skipGraphQLSpace(query, j+1)
				real := graphQLIdentRe.FindString(query[j:])
				if real == "" {
					return nil, false
				}
				name = real
				i = j + len(real)
			}
			fields = append(fields, name)
			if len(fields) > linearMaxOperations {
				// Stop early — the caller refuses on the bound anyway, and an
				// adversarial document should not get unbounded scanning.
				return fields, true
			}
		default:
			i++
		}
	}
	// Ran off the end without closing the selection set.
	return nil, false
}

// skipGraphQLArgs advances past a balanced argument list beginning at i,
// honouring string literals so parens inside them do not unbalance the count.
func skipGraphQLArgs(query string, i int) (int, bool) {
	d := 0
	for i < len(query) {
		switch query[i] {
		case '"':
			var ok bool
			if i, ok = skipGraphQLString(query, i); !ok {
				return 0, false
			}
			continue
		case '(':
			d++
		case ')':
			d--
			if d == 0 {
				return i + 1, true
			}
		}
		i++
	}
	return 0, false
}

func skipGraphQLSpace(query string, i int) int {
	for i < len(query) && (query[i] == ' ' || query[i] == '\t' || query[i] == '\n' || query[i] == '\r' || query[i] == ',') {
		i++
	}
	return i
}
