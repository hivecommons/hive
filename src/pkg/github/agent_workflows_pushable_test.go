package github

import (
	"context"
	"errors"
	"testing"
)

// AgentWorkflowsPushable asks about the one lane that will push, not about
// any capable lane: the adoption sweep keeps a same-repo PR on its own branch
// across a base workflow change only when the fallback fixer can push it
// (#12865: a contributor-tier fixer's push was refused while another lane
// held the grant).
func TestAgentWorkflowsPushable(t *testing.T) {
	write := map[string]string{"workflows": "write"}
	cases := []struct {
		name         string
		appAuth      bool
		fixerTrusted bool
		perms        map[string]string
		err          error
		want         bool
	}{
		{name: "grant present, fixer at contributor tier", appAuth: true, perms: write},
		{name: "grant present, fixer at trusted tier", appAuth: true, fixerTrusted: true, perms: write, want: true},
		{name: "token-authenticated client", fixerTrusted: true, perms: write},
		{name: "installation grants read only", appAuth: true, fixerTrusted: true, perms: map[string]string{"workflows": "read"}},
		{name: "installation read fails", appAuth: true, fixerTrusted: true, err: errors.New("502")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubInstallation(t, tc.perms, tc.err)
			c := &Client{}
			if tc.appAuth {
				c.appAuth = &AppAuth{}
			}
			// Another lane (ci-maintainer) always holds a trusted-tier
			// token: the answer for the fixer must not borrow it.
			c.SetWorkflowsSelfPushFunc(func(agent string) bool {
				return agent == "ci-maintainer" || (agent == FallbackFixAgent && tc.fixerTrusted)
			})
			c.SetWorkflowsLaneFunc(func(string, string) string { return "ci-maintainer" })
			if got := c.AgentWorkflowsPushable(context.Background(), FallbackFixAgent); got != tc.want {
				t.Fatalf("AgentWorkflowsPushable(%s) = %v, want %v", FallbackFixAgent, got, tc.want)
			}
		})
	}
	var nilClient *Client
	if nilClient.AgentWorkflowsPushable(context.Background(), FallbackFixAgent) {
		t.Fatal("nil client reported a pushable fixer")
	}
}
