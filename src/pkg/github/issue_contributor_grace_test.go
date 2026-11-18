package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// graceIssueServer serves issue o/r#42 filed by author (with association)
// createdAgo ago, carrying labels, plus a timeline whose "labeled" events
// are the given (label, actor, actorType) triples. Every other path 404s,
// which the open-PR guard reads as "no link" (it fails open).
func graceIssueServer(t *testing.T, author, association string, createdAgo time.Duration, labels []string, timeline [][3]string) *httptest.Server {
	t.Helper()
	created := time.Now().Add(-createdAgo).UTC().Format(time.RFC3339)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/42":
			var ls []string
			for _, l := range labels {
				ls = append(ls, fmt.Sprintf(`{"name":%q}`, l))
			}
			fmt.Fprintf(w, `{"number":42,"state":"open","created_at":%q,"author_association":%q,"user":{"login":%q},"labels":[%s]}`,
				created, association, author, strings.Join(ls, ","))
		case "/repos/o/r/issues/42/timeline":
			var evs []string
			for _, e := range timeline {
				evs = append(evs, fmt.Sprintf(`{"event":"labeled","label":{"name":%q},"actor":{"login":%q,"type":%q}}`, e[0], e[1], e[2]))
			}
			fmt.Fprintf(w, `[%s]`, strings.Join(evs, ","))
		default:
			http.NotFound(w, r)
		}
	}))
}

func graceReason(t *testing.T, srv *httptest.Server, grace time.Duration) string {
	t.Helper()
	c := issueTestClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	c.SetContributorGrace(grace)
	got, err := c.IssueContributorDispatchBlockReason(context.Background(), "o/r", 42)
	if err != nil {
		t.Fatalf("IssueContributorDispatchBlockReason: %v", err)
	}
	return got
}

func TestContributorGrace_RefusesOwnerFiledIssueInsideWindow(t *testing.T) {
	for _, tc := range []struct{ name, author, association string }{
		{"owner", "andy", "OWNER"},
		{"member", "andy", "MEMBER"},
		{"collaborator", "andy", "COLLABORATOR"},
		{"hive app", "hive-app[bot]", "CONTRIBUTOR"},
		{"hive app without suffix", "hive-app", "CONTRIBUTOR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := graceIssueServer(t, tc.author, tc.association, 3*time.Minute, nil, nil)
			defer srv.Close()
			got := graceReason(t, srv, 15*time.Minute)
			if !strings.HasPrefix(got, ClaimBlockReasonOwnerGracePrefix) || !strings.Contains(got, "3m ago") || !strings.Contains(got, tc.author) {
				t.Fatalf("reason = %q, want an owner grace refusal naming %s and 3m", got, tc.author)
			}
			if strings.Contains(got, "@") {
				t.Fatalf("reason %q must not carry a raw @mention", got)
			}
		})
	}
}

func TestContributorGrace_AdmitsAfterWindowOutsideAuthorAndWhenOff(t *testing.T) {
	cases := []struct {
		name        string
		author      string
		association string
		age         time.Duration
		grace       time.Duration
	}{
		{"after the window", "andy", "OWNER", 20 * time.Minute, 15 * time.Minute},
		{"outside reporter", "stranger", "NONE", time.Minute, 15 * time.Minute},
		{"contributor association", "stranger", "CONTRIBUTOR", time.Minute, 15 * time.Minute},
		{"grace off", "andy", "OWNER", time.Minute, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := graceIssueServer(t, tc.author, tc.association, tc.age, nil, nil)
			defer srv.Close()
			if got := graceReason(t, srv, tc.grace); got != "" {
				t.Fatalf("reason = %q, want the claim admitted", got)
			}
		})
	}
}

func TestContributorGrace_HelpWantedFromAPersonWaives(t *testing.T) {
	labels := []string{"help wanted"}
	// A person added it after filing: the grace is waived.
	srv := graceIssueServer(t, "andy", "OWNER", time.Minute, labels, [][3]string{{"help wanted", "andy", "User"}})
	defer srv.Close()
	if got := graceReason(t, srv, 15*time.Minute); got != "" {
		t.Fatalf("reason = %q, want the person-added help wanted to waive the grace", got)
	}

	// A bot added it (the hive's own triage): the grace stands.
	srv2 := graceIssueServer(t, "andy", "OWNER", time.Minute, labels, [][3]string{{"help wanted", "hive-app[bot]", "Bot"}})
	defer srv2.Close()
	if got := graceReason(t, srv2, 15*time.Minute); !strings.HasPrefix(got, ClaimBlockReasonOwnerGracePrefix) {
		t.Fatalf("reason = %q, want a bot-applied help wanted to keep the grace", got)
	}

	// The label is on the issue but the timeline shows no person applying it.
	srv3 := graceIssueServer(t, "andy", "OWNER", time.Minute, labels, nil)
	defer srv3.Close()
	if got := graceReason(t, srv3, 15*time.Minute); !strings.HasPrefix(got, ClaimBlockReasonOwnerGracePrefix) {
		t.Fatalf("reason = %q, want no waiver without a person's labeled event", got)
	}
}

func TestContributorGraceGuard_SkipsTheForgeReadForAnOldIssue(t *testing.T) {
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetContributorGrace(15 * time.Minute)
	if got := c.ContributorGraceGuard(context.Background(), "o/r", 42, time.Now().Add(-time.Hour)); got != "" || reads != 0 {
		t.Fatalf("old issue: reason %q after %d forge reads, want none", got, reads)
	}
}

func TestContributorGraceGuard_RefusesAFreshOwnerIssue(t *testing.T) {
	srv := graceIssueServer(t, "andy", "OWNER", 2*time.Minute, nil, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetContributorGrace(15 * time.Minute)
	got := c.ContributorGraceGuard(context.Background(), "o/r", 42, time.Now().Add(-2*time.Minute))
	if !strings.HasPrefix(got, ClaimBlockReasonOwnerGracePrefix) {
		t.Fatalf("reason = %q, want an owner grace refusal", got)
	}
}
