package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/issueclaim"
)

// humanClaimIssue describes the forge state behind IssueHumanClaimReason.
type humanClaimIssue struct {
	state     string
	assignees []string
	labels    []string
	// timeline events, as [event, subject, actor]: subject is the assignee
	// for (un)assigned and the label for (un)labeled.
	timeline [][3]string
	// comments, as [login, association, body].
	comments [][3]string
	// write maps a login to its repository permission.
	write map[string]string
}

func humanClaimServer(t *testing.T, in humanClaimIssue) *httptest.Server {
	t.Helper()
	if in.state == "" {
		in.state = "open"
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/issues/42":
			var as, ls []string
			for _, a := range in.assignees {
				as = append(as, fmt.Sprintf(`{"login":%q}`, a))
			}
			for _, l := range in.labels {
				ls = append(ls, fmt.Sprintf(`{"name":%q}`, l))
			}
			fmt.Fprintf(w, `{"number":42,"state":%q,"assignees":[%s],"labels":[%s]}`, in.state, strings.Join(as, ","), strings.Join(ls, ","))
		case r.URL.Path == "/repos/o/r/issues/42/timeline":
			var evs []string
			for _, e := range in.timeline {
				switch e[0] {
				case "assigned", "unassigned":
					evs = append(evs, fmt.Sprintf(`{"event":%q,"assignee":{"login":%q},"actor":{"login":%q}}`, e[0], e[1], e[2]))
				default:
					evs = append(evs, fmt.Sprintf(`{"event":%q,"label":{"name":%q},"actor":{"login":%q}}`, e[0], e[1], e[2]))
				}
			}
			fmt.Fprintf(w, `[%s]`, strings.Join(evs, ","))
		case r.URL.Path == "/repos/o/r/issues/42/comments":
			var cs []string
			for _, c := range in.comments {
				cs = append(cs, fmt.Sprintf(`{"user":{"login":%q},"author_association":%q,"body":%q}`, c[0], c[1], c[2]))
			}
			fmt.Fprintf(w, `[%s]`, strings.Join(cs, ","))
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/collaborators/") && strings.HasSuffix(r.URL.Path, "/permission"):
			login := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/o/r/collaborators/"), "/permission")
			perm := in.write[login]
			if perm == "" {
				perm = "read"
			}
			fmt.Fprintf(w, `{"permission":%q,"role_name":%q,"user":{"login":%q}}`, perm, perm, login)
		default:
			http.NotFound(w, r)
		}
	}))
}

func humanClaimReasonFor(t *testing.T, in humanClaimIssue, holder string) string {
	t.Helper()
	srv := humanClaimServer(t, in)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	got, err := c.IssueHumanClaimReason(context.Background(), "o/r", 42, holder)
	if err != nil {
		t.Fatalf("IssueHumanClaimReason: %v", err)
	}
	return got
}

// The operator's dispatcher claims with `gh issue edit --add-assignee` plus a
// plain "Claimed; fix in progress." comment and no ledger entry: an assignee
// set by an owner/maintainer on an open issue is a human claim.
func TestIssueHumanClaim_AssigneeSetByMaintainerPlusPlainComment(t *testing.T) {
	got := humanClaimReasonFor(t, humanClaimIssue{
		assignees: []string{"andy"},
		timeline:  [][3]string{{"assigned", "andy", "andy"}},
		comments:  [][3]string{{"andy", "MEMBER", "Claimed; fix in progress."}},
		write:     map[string]string{"andy": "admin"},
	}, "karibhou")
	want := claims.HumanClaimReason("assigned", "andy")
	if got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	if login, ok := claims.ParseHumanClaimReason(got); !ok || login != "andy" {
		t.Fatalf("reason %q does not parse back to the person", got)
	}
}

func TestIssueHumanClaim_AssigneeAlonePlusCommentAloneAndLabel(t *testing.T) {
	// The assignee alone (no comment) is enough.
	if got := humanClaimReasonFor(t, humanClaimIssue{
		assignees: []string{"andy"},
		timeline:  [][3]string{{"assigned", "andy", "andy"}},
		write:     map[string]string{"andy": "write"},
	}, "karibhou"); got != claims.HumanClaimReason("assigned", "andy") {
		t.Fatalf("assignee alone: %q", got)
	}
	// A `/claim` comment from a maintainer (no assignee).
	if got := humanClaimReasonFor(t, humanClaimIssue{
		comments: [][3]string{{"andy", "OWNER", "/claim\nworking on this"}},
	}, "karibhou"); got != claims.HumanClaimReason("/claim comment", "andy") {
		t.Fatalf("/claim comment: %q", got)
	}
	// hive/claimed-by-human applied by hand by a maintainer.
	if got := humanClaimReasonFor(t, humanClaimIssue{
		labels:   []string{"hive/claimed-by-human"},
		timeline: [][3]string{{"labeled", "hive/claimed-by-human", "andy"}},
		write:    map[string]string{"andy": "maintain"},
	}, "karibhou"); got != claims.HumanClaimReason("hive/claimed-by-human label", "andy") {
		t.Fatalf("hand-applied label: %q", got)
	}
}

func TestIssueHumanClaim_NotAClaim(t *testing.T) {
	cases := map[string]humanClaimIssue{
		"the hive's own App assigned it": {
			assignees: []string{"someone"},
			timeline:  [][3]string{{"assigned", "someone", "hive-app[bot]"}},
		},
		"the contributor's own assignment": {
			assignees: []string{"karibhou"},
			timeline:  [][3]string{{"assigned", "karibhou", "andy"}},
			write:     map[string]string{"andy": "admin"},
		},
		"assigned by a user without write access": {
			assignees: []string{"stranger"},
			timeline:  [][3]string{{"assigned", "stranger", "stranger"}},
		},
		"label applied by the hive": {
			labels:   []string{"hive/claimed-by-human"},
			timeline: [][3]string{{"labeled", "hive/claimed-by-human", "hive-app[bot]"}},
		},
		"claim comment from an outside commenter": {
			comments: [][3]string{{"stranger", "NONE", "/claim"}},
		},
		"claim comment withdrawn": {
			comments: [][3]string{{"andy", "OWNER", "/claim"}, {"andy", "OWNER", "/unclaim"}},
		},
		"unassigned since": {
			assignees: nil,
			timeline:  [][3]string{{"assigned", "andy", "andy"}, {"unassigned", "andy", "andy"}},
			write:     map[string]string{"andy": "admin"},
		},
		"closed issue": {
			state:     "closed",
			assignees: []string{"andy"},
			timeline:  [][3]string{{"assigned", "andy", "andy"}},
			write:     map[string]string{"andy": "admin"},
		},
		"the contributor's own claim comment": {
			comments: [][3]string{{"karibhou", "COLLABORATOR", "/claim"}},
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got := humanClaimReasonFor(t, in, "karibhou"); got != "" {
				t.Fatalf("reason = %q, want none", got)
			}
		})
	}
}

func TestIssueHumanClaim_VerdictIsCached(t *testing.T) {
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			fmt.Fprint(w, `[]`)
			return
		}
		reads++
		fmt.Fprint(w, `{"number":42,"state":"open"}`)
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := c.IssueHumanClaimReason(context.Background(), "o/r", 42, "karibhou"); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 {
		t.Fatalf("forge reads = %d, want 1 (cached for %s)", reads, humanClaimCacheTTL)
	}
}

// A live ledger claim by a person or by one of the hive's own agents is read
// without a forge round trip (hivecommons/hive#18900): adoption does not open
// a copy beside work either already holds. A hive agent's claim is not a
// human-claim reason, so it never triggers the contributor yank.
func TestIssueHumanClaim_LedgerClaimsByPersonAndHiveAgent(t *testing.T) {
	srv := humanClaimServer(t, humanClaimIssue{})
	defer srv.Close()
	for kind, want := range map[string]string{
		"human": claims.HumanClaimReason("claimed in the ledger", "andy"),
		"agent": claims.ReasonHiveClaim + ": scanner",
	} {
		t.Run(kind, func(t *testing.T) {
			c := issueTestClient(t, srv.URL)
			identity := "andy"
			if kind == "agent" {
				identity = "scanner"
			}
			c.SetIssueClaimLedger(func(repo string, number int) (issueclaim.Claim, bool) {
				return issueclaim.Claim{Identity: identity, Kind: kind, ExpiresAt: time.Now().Add(time.Hour), Source: issueclaim.SourceLease}, true
			})
			got, err := c.IssueHumanClaimReason(context.Background(), "o/r", 42, "karibhou")
			if err != nil || got != want {
				t.Fatalf("reason = %q, %v; want %q", got, err, want)
			}
			if kind == "agent" {
				if _, human := claims.ParseHumanClaimReason(got); human {
					t.Fatalf("a hive agent's claim %q must not parse as a human claim", got)
				}
			}
		})
	}
}
