package proxy

import (
	"strings"
	"testing"
	"time"
)

// TestPolicyCallbackFailureDenies injects faults into every live policy
// predicate on the write-gate decision path and asserts the gate refuses.
// Before the guard, the panic rows crashed the test binary (no recover on the
// request path) and the hang rows blocked until the test timeout.
func TestPolicyCallbackFailureDenies(t *testing.T) {
	prev := policyCallbackTimeout
	policyCallbackTimeout = 50 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		policyCallbackTimeout = prev
	})

	type fault struct {
		name string
		hit  func()
	}
	faults := []fault{
		{"panic", func() { panic("injected policy fault") }},
		{"timeout", func() { <-release }},
	}

	const mergeGraphQL = `{"query":"mutation { mergePullRequest(input:{pullRequestId:\"PR_kw\"}) { clientMutationId } }","variables":{"repo":"acme/app"}}`

	gates := []struct {
		name  string
		gate  string // policy name in policyCallbackFailedReason; "" = bool-only check, no reason
		check func(p *GitHubProxy, hit func()) (string, bool)
	}{
		{"repo pause", "repo pause", func(p *GitHubProxy, hit func()) (string, bool) {
			p.SetRepoPausedFunc(func(string) bool { hit(); return false })
			return p.repoPauseRefusal("POST", "/repos/acme/app/issues")
		}},
		{"agent repo scope", "agent repo scope", func(p *GitHubProxy, hit func()) (string, bool) {
			p.SetAgentRepoScopeFunc(func(string, string) bool { hit(); return true })
			return p.agentRepoScopeRefusal("scanner", "POST", "/repos/acme/app/issues")
		}},
		{"repo auto-merge REST", "repo auto-merge", func(p *GitHubProxy, hit func()) (string, bool) {
			p.SetRepoAutoMergeEnabledFunc(func(string) bool { hit(); return true })
			return p.autoMergeRefusal("PUT", "/repos/acme/app/pulls/7/merge")
		}},
		{"repo auto-merge GraphQL", "repo auto-merge", func(p *GitHubProxy, hit func()) (string, bool) {
			p.SetRepoAutoMergeEnabledFunc(func(string) bool { hit(); return true })
			return p.graphQLAutoMergeRefusal([]byte(mergeGraphQL))
		}},
		{"write_surface.enforce", "write_surface.enforce", func(p *GitHubProxy, hit func()) (string, bool) {
			p.SetWriteSurfaceEnforceFunc(func(string) bool { hit(); return false })
			reason, _, refused := p.writeSurfaceEnforceRefusal("scanner", "POST", "/repos/acme/app/issues", "")
			return reason, refused
		}},
		{"write_surface.enforce GraphQL", "write_surface.enforce", func(p *GitHubProxy, hit func()) (string, bool) {
			p.SetWriteSurfaceEnforceFunc(func(string) bool { hit(); return false })
			return p.writeSurfaceGraphQLRefusal("scanner", []byte(mergeGraphQL))
		}},
		{"write_surface.enforce MITM", "", func(p *GitHubProxy, hit func()) (string, bool) {
			p.SetWriteSurfaceEnforceFunc(func(string) bool { hit(); return false })
			return "", p.laneWriteSurfaceEnforced("scanner")
		}},
	}

	for _, g := range gates {
		for _, f := range faults {
			t.Run(g.name+"/"+f.name, func(t *testing.T) {
				p := &GitHubProxy{}
				reason, denied := g.check(p, f.hit)
				if !denied {
					t.Fatalf("%s callback %s: gate allowed the request; want deny", g.name, f.name)
				}
				if g.gate != "" && reason != policyCallbackFailedReason(g.gate) {
					t.Fatalf("%s callback %s: reason = %q; want policyCallbackFailedReason(%q)", g.name, f.name, reason, g.gate)
				}
				if strings.Contains(reason, "PAUSED by the operator") || strings.Contains(reason, "is NOT scoped") {
					t.Fatalf("failure reason asserts an operator decision nobody made: %q", reason)
				}
			})
		}
	}

	// A healthy predicate still decides: the guard must not turn every
	// answer into a deny.
	p := &GitHubProxy{}
	p.SetRepoPausedFunc(func(string) bool { return false })
	if reason, denied := p.repoPauseRefusal("POST", "/repos/acme/app/issues"); denied {
		t.Fatalf("healthy pause predicate refused: %q", reason)
	}
	p.SetAgentRepoScopeFunc(func(string, string) bool { return true })
	if reason, denied := p.agentRepoScopeRefusal("scanner", "POST", "/repos/acme/app/issues"); denied {
		t.Fatalf("healthy scope predicate refused: %q", reason)
	}
}
