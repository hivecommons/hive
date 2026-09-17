package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The #7238 god-file extraction left one-line wrappers in main.go so call
// sites kept their shape while the logic moved to pkg/apphealth, pkg/advisory
// and pkg/inference. The extracted packages carry their own tests; what is NOT
// covered anywhere is the wrappers themselves — that each one still delegates
// to the right function with the right argument plumbing. A wrapper that
// swaps two arguments, drops one, or hardcodes what should be read at call
// time compiles fine and only fails in production. These tests pin the
// delegation contracts through the deterministic, network-free paths each
// classifier documents.

// A nil *github.AppAuth is DiagnoseFull's documented "no App configured"
// shape: it short-circuits to AppStateOK without touching the network, which
// makes it the one input that exercises the wrapper hermetically.
func TestClassifyGitHubAppFailureNilAppAuthDoesNotRaise(t *testing.T) {
	raise, msg, state := classifyGitHubAppFailure(context.Background(), nil, "acme", discardLogger())
	if raise {
		t.Fatalf("raise = true for nil appAuth; a hive with no App configured must never raise the banner")
	}
	if msg != "" {
		t.Fatalf("msg = %q, want empty", msg)
	}
	if state != github.AppStateOK {
		t.Fatalf("state = %v, want AppStateOK", state)
	}
}

// With a nil appAuth the diagnosis reports OK, so ClassifyWriteForbidden must
// fall through to its repo-scope attribution: state WriteForbidden with copy
// that names the repo the 403 came from. That names-the-repo property is the
// whole point of the classifier (#2353) and proves the wrapper passes repo
// through in the right position rather than, say, swapping it with owner.
func TestClassifyGitHubAppWriteForbiddenAttributesRepoScope(t *testing.T) {
	msg, state := classifyGitHubAppWriteForbidden(context.Background(), nil, "acme", "acme/widgets")
	if state != github.AppStateWriteForbidden {
		t.Fatalf("state = %v, want AppStateWriteForbidden", state)
	}
	if !strings.Contains(msg, "acme/widgets") {
		t.Fatalf("message does not name the forbidden repo: %q", msg)
	}
	if !strings.Contains(msg, "'acme'") {
		t.Fatalf("message does not name the expected owner: %q", msg)
	}
}

// nil appAuth and an empty repo list are ClassifyRepoCoverage's documented
// "cannot conclude anything" inputs: raise=false, AppStateUnknown, no probe.
func TestClassifyGitHubAppRepoCoverageNilAppAuthIsUnknown(t *testing.T) {
	raise, msg, state := classifyGitHubAppRepoCoverage(context.Background(), nil, "acme", []string{"acme/widgets"}, discardLogger())
	if raise || msg != "" || state != github.AppStateUnknown {
		t.Fatalf("nil appAuth: got (raise=%v, msg=%q, state=%v), want (false, \"\", AppStateUnknown)", raise, msg, state)
	}
}

func TestPrimaryAdvisoryRepoResolutionOrder(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"nil config", nil, ""},
		{"primary repo wins", &config.Config{Project: config.ProjectConfig{
			PrimaryRepo: "acme/primary", Repos: []string{"acme/first", "acme/second"},
		}}, "acme/primary"},
		{"first repo fallback", &config.Config{Project: config.ProjectConfig{
			Repos: []string{"acme/first", "acme/second"},
		}}, "acme/first"},
		{"nothing configured", &config.Config{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := primaryAdvisoryRepo(tc.cfg); got != tc.want {
				t.Fatalf("primaryAdvisoryRepo() = %q, want %q", got, tc.want)
			}
		})
	}
}

// advisoryIssueUnresolved and advisoryIssueNumber answer the same question
// from opposite directions; asserting them against the same table pins that
// they can never disagree about whether the digest has somewhere to go.
func TestAdvisoryIssueUnresolvedAndNumberAgree(t *testing.T) {
	issues := map[string]int{
		"acme/pinned":   41,
		"acme/failed":   0,
		"acme/negative": -3,
	}
	cases := []struct {
		repo       string
		unresolved bool
		wantNum    int
		wantOK     bool
	}{
		{"acme/pinned", false, 41, true},
		{"acme/failed", true, 0, false}, // a recorded 0 is a failed ensure, not a target
		{"acme/negative", true, -3, false},
		{"acme/unknown", true, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.repo, func(t *testing.T) {
			if got := advisoryIssueUnresolved(issues, tc.repo); got != tc.unresolved {
				t.Fatalf("advisoryIssueUnresolved(%q) = %v, want %v", tc.repo, got, tc.unresolved)
			}
			num, ok := advisoryIssueNumber(issues, tc.repo)
			if num != tc.wantNum || ok != tc.wantOK {
				t.Fatalf("advisoryIssueNumber(%q) = (%d, %v), want (%d, %v)", tc.repo, num, ok, tc.wantNum, tc.wantOK)
			}
			if ok == advisoryIssueUnresolved(issues, tc.repo) {
				t.Fatalf("advisoryIssueNumber ok=%v agrees with unresolved=%v for %q — the two views diverged", ok, tc.unresolved, tc.repo)
			}
		})
	}
}

// resolveWatsonxGateway documents a two-pass preference: a gateway NAMED
// watsonx (of kind watsonx) wins over one merely of KIND watsonx, so
// `backend: watsonx` works however the operator named their gateway.
func TestResolveWatsonxGatewayPrefersNameThenKind(t *testing.T) {
	named := config.GatewayConfig{Name: "Watsonx", Kind: config.GatewayKindWatsonx, Endpoint: "https://named.example"}
	byKind := config.GatewayConfig{Name: "ibm-granite-prod", Kind: config.GatewayKindWatsonx, Endpoint: "https://kind.example"}
	other := config.GatewayConfig{Name: "openai", Kind: "openai", Endpoint: "https://other.example"}

	cfgWith := func(gws ...config.GatewayConfig) *config.Config {
		return &config.Config{Governor: config.GovernorConfig{Gateways: gws}}
	}

	if gw := resolveWatsonxGateway(cfgWith(other, byKind, named)); gw == nil || gw.Endpoint != "https://named.example" {
		t.Fatalf("named watsonx gateway not preferred: got %+v", gw)
	}
	if gw := resolveWatsonxGateway(cfgWith(other, byKind)); gw == nil || gw.Endpoint != "https://kind.example" {
		t.Fatalf("kind-only watsonx gateway not found: got %+v", gw)
	}
	if gw := resolveWatsonxGateway(cfgWith(other)); gw != nil {
		t.Fatalf("no watsonx gateway configured, got %+v", gw)
	}
}

// The translator forwards to the bundled litellm proxy on a fixed loopback
// port; a drift here would silently break the local_proxy fallback.
func TestLitellmLocalProxyURLPinsLoopbackPort(t *testing.T) {
	if got := litellmLocalProxyURL(); got != "http://127.0.0.1:18445" {
		t.Fatalf("litellmLocalProxyURL() = %q, want http://127.0.0.1:18445", got)
	}
}
