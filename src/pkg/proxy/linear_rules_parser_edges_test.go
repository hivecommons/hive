package proxy

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// ─────────────────────────────────────────────────────────────────────────────
// Parser edge cases in the Linear GraphQL classifier.
//
// Every malformed document must fail CLOSED (denied even at the most
// privileged tier): the classifier refusing to parse is the security
// backstop, so each refusal branch deserves a pinned test. Well-formed
// documents that merely exercise rarely-hit scanner branches (comments,
// directives, variable references inside a selection set) must still classify
// correctly — a regression there would deny legitimate traffic.
// ─────────────────────────────────────────────────────────────────────────────

// TestLinearParserMalformedDocumentsFailClosed pins the refusal branches of
// the document scanner: each document is syntactically broken in a way that
// aborts a different part of the parse, and every one must be denied even at
// the merge tier.
func TestLinearParserMalformedDocumentsFailClosed(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantReason string
	}{
		{
			// linearFields runs off the end without ever finding the
			// operation's opening brace.
			name:       "keyword with no selection set",
			query:      `mutation `,
			wantReason: "unreadable GraphQL document",
		},
		{
			// No operation keyword and not anonymous shorthand: there is no
			// operation to classify.
			name:       "bare token document",
			query:      `broken`,
			wantReason: "unreadable GraphQL document",
		},
		{
			// An unterminated string at depth zero aborts keyword scanning.
			name:       "unterminated top-level string",
			query:      `"never closed`,
			wantReason: "unreadable GraphQL document",
		},
		{
			// An unterminated string inside the selection set aborts field
			// collection.
			name:       "unterminated string in selection set",
			query:      `mutation { "broken`,
			wantReason: "unreadable GraphQL document",
		},
		{
			// An alias colon with nothing after it: the "real" field name is
			// empty, which must refuse rather than record an empty field.
			name:       "alias with missing real field",
			query:      `mutation { ack: }`,
			wantReason: "unreadable GraphQL document",
		},
		{
			// Arguments that never close leave the selection set unbalanced.
			name:       "unterminated argument list",
			query:      `mutation { issueUpdate(input: {} `,
			wantReason: "unreadable GraphQL document",
		},
		{
			// Selection set never closes.
			name:       "unclosed selection set",
			query:      `mutation { issueUpdate(input: {}) { success }`,
			wantReason: "unreadable GraphQL document",
		},
		{
			// Fragments can move selections out of the operation body, so the
			// classifier refuses them by design rather than classify on an
			// incomplete view (see linearOperationFields).
			name:       "fragment in document",
			query:      `fragment F on Issue { id } mutation { issueUpdate(input: {}) { success } }`,
			wantReason: "unreadable GraphQL document",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(agent.ModeIssuesPRsMerge, tc.query)
			if d.Allowed {
				t.Fatalf("malformed document was ALLOWED at merge tier: %q", tc.query)
			}
			if d.Reason != tc.wantReason {
				t.Fatalf("malformed document reason = %q, want %q", d.Reason, tc.wantReason)
			}
			if len(d.Operations) != 0 {
				t.Fatalf("malformed document reported operations %v; unreadable input must not be partially trusted", d.Operations)
			}
		})
	}
}

func TestLinearSelectionFieldsRefusesMalformedSyntax(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "unterminated string",
			query: `mutation { "broken`,
		},
		{
			name:  "alias without real field",
			query: `mutation { ack: }`,
		},
		{
			name:  "unterminated arguments",
			query: `mutation { issueUpdate(input: {} `,
		},
		{
			name:  "unclosed selection set",
			query: `mutation { issueUpdate(input: {}) { success }`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := strings.IndexByte(tc.query, '{')
			if start < 0 {
				t.Fatal("test query must contain selection set")
			}
			fields, ok := linearSelectionFields(tc.query, start+1)
			if ok {
				t.Fatalf("linearSelectionFields accepted malformed syntax with fields %v", fields)
			}
			if fields != nil {
				t.Fatalf("linearSelectionFields returned partial fields %v; malformed selections must fail closed", fields)
			}
		})
	}
}

// TestLinearParserWellFormedEdgeSyntaxStillClassifies pins scanner branches
// that legitimate documents can hit: comments and directives inside a
// selection set, variable references, and a fragment preceding the operation
// keyword. All of these must still classify the mutation correctly and be
// allowed at a tier that permits issueUpdate.
func TestLinearParserWellFormedEdgeSyntaxStillClassifies(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{
			// The '#' branch of the selection-set scanner.
			name: "comment inside selection set",
			query: "mutation { # audit note\n" +
				`issueUpdate(input: {}) { success } }`,
		},
		{
			// The '@' branch: a directive is not a field selection.
			name:  "directive on selected field",
			query: `mutation { issueUpdate(input: {}) @include(if: true) { success } }`,
		},
		{
			// The '$' branch: a bare variable reference is not a field.
			name:  "variable reference in nested selection",
			query: `mutation { issueUpdate(input: {}) { success $junk } }`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(agent.ModeIssuesPRsMerge, tc.query)
			if !d.Allowed {
				t.Fatalf("well-formed issueUpdate document was DENIED at merge tier (reason %q): %q", d.Reason, tc.query)
			}
		})
	}
}

// TestLinearParserEdgeSyntaxDoesNotDisguiseMutations is the adversarial
// counterpart: edge syntax around a mutation must not stop the classifier
// from seeing it. An advisory-tier agent must stay blocked.
func TestLinearParserEdgeSyntaxDoesNotDisguiseMutations(t *testing.T) {
	queries := []string{
		"mutation { # comment\n issueUpdate(input: {}) { success } }",
		`mutation { issueUpdate(input: {}) @include(if: true) { success } }`,
		// A stray depth-zero token before the keyword forces
		// indexGraphQLKeyword to walk past non-keyword tokens; the mutation
		// behind it must still be seen and blocked.
		`stray mutation { issueUpdate(input: {}) { success } }`,
	}
	for _, q := range queries {
		d := decide(agent.ModeAdvisory, q)
		if d.Allowed {
			t.Fatalf("issueUpdate ALLOWED at advisory tier via edge syntax: %q", q)
		}
	}
}
