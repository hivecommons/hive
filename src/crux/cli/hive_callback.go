package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/hivecap"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/serve"
	"github.com/hivecommons/hive/crux/toolapprove"
)

// hiveCapTarget is the subset of *agent.Manager setters the hive callback
// feeds in sidecar mode: the seams hive provides in-process through
// pkg/cruxhost (hivecommons/hive#14138 stage 2). A runtime that is not the
// manager (a test fake) is left alone.
type hiveCapTarget interface {
	SetConditionReporter(crux.ConditionReporter)
	SetSandboxPRClient(agent.PRCreator)
	SetLinearCredentialResolver(func() agent.LinearCredential)
	SetSandboxPushMinter(pushbroker.TokenMinter)
}

// hiveCallback is `crux serve --hive-callback`: the hive capability endpoint
// the runtime dials for the capabilities it does not own.
type hiveCallback struct {
	client *callback.Client
	set    *hivecap.Set
}

// dialHiveCallback builds the client and adapters for addr (a form
// api.ParseAddress accepts), presenting token when non-empty. Nothing is
// called yet: every adapter fails closed until hive answers, and
// hivecap.Watch reports the outage.
func dialHiveCallback(addr, token string) (*hiveCallback, error) {
	c, err := callback.NewClient(addr, token)
	if err != nil {
		return nil, fmt.Errorf("hive callback %s: %w", addr, err)
	}
	return &hiveCallback{client: c, set: hivecap.New(c, 0)}, nil
}

// install feeds the manager and process-wide seams from the adapters,
// reports the endpoint on the server's health route, and starts the
// condition watch, which ends with ctx. It reports to w which seams were
// wired so an operator can see the difference between a runtime that is a
// manager and one that is not.
func (h *hiveCallback) install(ctx context.Context, rt crux.Runtime, srv *serve.Server, w io.Writer) {
	srv.HiveCallback = h.health
	toolapprove.SetDefaultTextScanner(h.set.Text)
	seams := "text scanner"
	if m, ok := rt.(hiveCapTarget); ok {
		m.SetConditionReporter(h.set.Conditions)
		m.SetSandboxPRClient(h.set.PullRequest)
		linear := h.set.Linear
		m.SetLinearCredentialResolver(func() agent.LinearCredential {
			tok, key := linear.Resolve()
			return agent.LinearCredential{AccessToken: tok, APIKey: key}
		})
		m.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: h.set.TokenScoper("", 0)})
		seams += ", conditions, pull requests, Linear credential, push tokens"
	}
	fmt.Fprintf(w, "hive callback %s: %s\n", addressString(h.client.Address()), seams)
	go hivecap.Watch(ctx, h.client.Status, srv.Conditions(), 0)
}

// health is the status Health reports.
func (h *hiveCallback) health() api.HiveCallback {
	st := h.client.Status()
	out := api.HiveCallback{Address: addressString(h.client.Address()), Reachable: st.Reachable, LastError: st.LastError, FailedCalls: st.FailedCalls}
	if !st.Since.IsZero() {
		since := st.Since
		out.Since = &since
	}
	if !st.LastSuccess.IsZero() {
		last := st.LastSuccess
		out.LastSuccess = &last
	}
	return out
}

// addressString renders an address the way serve.Listeners.Addrs does.
func addressString(a api.Address) string {
	if a.Network == "unix" {
		return "unix://" + a.Path
	}
	return a.BaseURL
}
