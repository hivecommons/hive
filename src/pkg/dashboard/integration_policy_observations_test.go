package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/github"
)

func TestIntegrationNativePrefixPromotionRetainsOriginalIdentity(t *testing.T) {
	base, a, b, tree, head1, head2 := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 40), strings.Repeat("f", 40)
	original := integration.Subject{Repo: "o/r", Base: "v5", BaseSHA: base, Authority: "group", TestedSHA: b, TestedTree: tree,
		Members: []integration.Member{{PR: 1, Head: head1, Entry: "A", Position: 1, BaseCommit: base, HeadCommit: a}, {PR: 2, Head: head2, Entry: "B", Position: 2, BaseCommit: a, HeadCommit: b}}}
	promoted := github.NativeIntegrationGroup{Repo: "o/r", Base: "v5", BaseSHA: a, HeadRef: "group", HeadSHA: b, Tree: tree,
		Members: []github.NativeIntegrationMember{{PR: 2, Head: head2, Entry: "B", Position: 1, BaseCommit: a, HeadCommit: b}}}
	if !integrationNativeMembershipCompatible(original, promoted) || len(original.Members) != 2 {
		t.Fatal("valid documented prefix discarded or rewritten")
	}
	for _, change := range []func(*github.NativeIntegrationGroup){
		func(g *github.NativeIntegrationGroup) { g.BaseSHA = base },
		func(g *github.NativeIntegrationGroup) { g.Tree = base },
		func(g *github.NativeIntegrationGroup) { g.Members[0].Head = head1 },
		func(g *github.NativeIntegrationGroup) { g.Members[0].Entry = "other" },
		func(g *github.NativeIntegrationGroup) { g.HeadSHA = a },
	} {
		bad := promoted
		bad.Members = append([]github.NativeIntegrationMember(nil), promoted.Members...)
		change(&bad)
		if integrationNativeMembershipCompatible(original, bad) {
			t.Fatalf("transferred evidence to changed candidate: %+v", bad)
		}
	}
}

func TestIntegrationRepairUsesExactLiveFrontWithoutBlameOrAuthority(t *testing.T) {
	for _, scenario := range []string{"valid", "direct-bound", "unbound-author-head", "advisory", "superseded", "unknown", "stale", "future", "new-head", "closed", "retargeted", "read-error"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().UTC()
			head, base, tested, tree := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
			server := orchestrationServer(t, config.IntegrationEnforce)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/o/r/pulls/7" {
					t.Errorf("unexpected request %s", r.URL)
				}
				if scenario == "read-error" {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				actualHead, state, target := head, "open", "v5"
				if scenario == "new-head" {
					actualHead = strings.Repeat("f", 40)
				}
				if scenario == "closed" {
					state = "closed"
				}
				if scenario == "retargeted" {
					target = "other"
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"number":7,"state":%q,"title":"front","html_url":"https://github.com/o/r/pull/7","user":{"login":"service-owner"},"head":{"sha":%q},"base":{"ref":%q}}`, state, actualHead, target)
			}))
			defer api.Close()
			server.deps.GHClient = github.NewClient("token", "o", []string{"r"}, nil, api.URL)
			if scenario == "advisory" {
				server.deps.Config.IntegrationOrchestration.Mode = config.IntegrationAdvisory
			}
			store, err := server.integrationStore()
			if err != nil {
				t.Fatal(err)
			}
			receipt := integration.Evidence{Subject: integration.Subject{Version: 1, Repo: "o/r", Base: "v5", Engine: "native", Authority: "group", BaseSHA: base, TestedSHA: tested, TestedTree: tree,
				Policy: "unverified-policy", Producer: "github-merge-group-webhook", Workflow: "merge_group", Attempt: 1, Generation: 1,
				Members: []integration.Member{{PR: 7, Head: head, Entry: "entry", Position: 1}}}, State: integration.Waiting, ObservedAt: now, Source: "github-merge-group-v1"}
			switch scenario {
			case "superseded":
				receipt.State = integration.Superseded
			case "unknown":
				receipt.State = integration.Unknown
			case "stale":
				receipt.ObservedAt = now.Add(-time.Hour)
			case "future":
				receipt.ObservedAt = now.Add(time.Hour)
			}
			if err := store.Append(integration.Event{Version: 1, ID: "receipt", At: now, Evidence: &receipt}); err != nil {
				t.Fatal(err)
			}
			failure := &github.WebhookCIFailure{HeadSHA: tested}
			if scenario == "direct-bound" || scenario == "unbound-author-head" {
				receipt.RunID, receipt.SuiteID, receipt.CheckIDs = 10, 20, []int64{30}
				receipt.Subject.Authority, receipt.Subject.Engine = "pull/7", "direct"
				receipt.ObservedAt = now.Add(time.Nanosecond)
				if err := store.Append(integration.Event{Version: 1, ID: "direct-receipt", At: now, Evidence: &receipt}); err != nil {
					t.Fatal(err)
				}
				failure.HeadSHA = head
				if scenario == "direct-bound" {
					failure.SuiteID, failure.CheckRunID = 20, 30
				}
			}
			owner, context, exact := server.integrationFailureOwner("o/r", failure)
			if scenario == "valid" || scenario == "direct-bound" {
				if owner == nil || owner.Number != 7 || context == nil || context.TestedTree != tree || exact != head || len(context.Members) != 1 {
					t.Fatalf("%+v %+v %s", owner, context, exact)
				}

			} else if owner != nil || context != nil || exact != "" {
				t.Fatalf("stale/unknown repair owner selected: %s %+v", scenario, owner)
			}
		})
	}
}

func TestIntegrationFailureIdentityOnlyAddsSignedPayloadIDsInEnforce(t *testing.T) {
	for _, mode := range []string{config.IntegrationOff, config.IntegrationAdvisory, config.IntegrationEnforce} {
		server := orchestrationServer(t, mode)
		failure := &github.WebhookCIFailure{}
		body := []byte(`{"check_run":{"id":30,"check_suite":{"id":20}},"check_suite":{"id":40}}`)
		if err := server.integrationFailureIdentity("check_run", body, failure); err != nil {
			t.Fatal(err)
		}
		if mode == config.IntegrationEnforce {
			if failure.SuiteID != 20 || failure.CheckRunID != 30 {
				t.Fatalf("lost signed check identity: %+v", failure)
			}
			if err := server.integrationFailureIdentity("check_suite", body, failure); err != nil || failure.SuiteID != 40 {
				t.Fatal("lost signed suite")
			}
			if err := server.integrationFailureIdentity("check_run", []byte("{"), failure); err == nil {
				t.Fatal("malformed identity accepted")
			}
		} else if failure.SuiteID != 0 || failure.CheckRunID != 0 {
			t.Fatal("disabled/advisory changed legacy failure")
		}
	}
}
