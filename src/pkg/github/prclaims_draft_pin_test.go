package github

import (
	"context"
	"net/http"
	"testing"
)

// #3970 interaction pin. FetchClaims lists open PRs itself
// (PullRequests.List, state=open) and never filters on GetDraft(), so a draft
// PR registers its claim exactly like a ready one — a draft is still "someone
// is on it" for offer suppression. That is independent of fetchPRs, the
// actionable-list path #3970 (b3c92682) changed to surface the App's own stale
// drafts: the two listings do not share code, so neither drops nor
// double-counts the other's items. These tests pin that a future draft filter
// in either path cannot silently blind the claim ledger.

// draftPR mirrors pr() with the draft flag set.
func draftPR(number int, author, title, body string) map[string]any {
	p := pr(number, author, title, body)
	p["draft"] = true
	return p
}

func TestFetchClaimsDraftPRsStillClaim(t *testing.T) {
	tests := []struct {
		name          string
		prs           []map[string]any
		wantIssue     int
		wantReference bool
	}{
		{
			// The #3980 shape as a draft: still one claim, still a weak
			// reference — the contribute queue must not re-offer the issue
			// just because the covering PR is a draft.
			name:          "draft with a non-closing reference claims once, as a reference",
			prs:           []map[string]any{draftPR(3898, "Danathar", "wip", "Refs #3498 — no `Fixes` keyword, deliberately.")},
			wantIssue:     3498,
			wantReference: true,
		},
		{
			name:          "draft with a closing keyword hard-claims once",
			prs:           []map[string]any{draftPR(500, "clubanderson", "wip", "Fixes #301")},
			wantIssue:     301,
			wantReference: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := prClaimServer(t, tt.prs, http.StatusOK)
			c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
			claims, err := c.FetchClaims(context.Background(), HiveIdentity{AIAuthor: "clubanderson"})
			if err != nil {
				t.Fatalf("FetchClaims: %v", err)
			}
			// Exactly one claim: drafts are neither dropped nor double-counted.
			if len(claims) != 1 {
				t.Fatalf("expected exactly one claim from a draft PR, got %+v", claims)
			}
			if claims[0].Issue != tt.wantIssue {
				t.Errorf("Issue = %d, want %d", claims[0].Issue, tt.wantIssue)
			}
			if claims[0].Reference != tt.wantReference {
				t.Errorf("Reference = %v, want %v", claims[0].Reference, tt.wantReference)
			}
		})
	}
}
