package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"time"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/crux/proxy"
)

// sandboxReadTokenTimeout bounds one read-token mint for the sandbox read
// relay.
const sandboxReadTokenTimeout = 20 * time.Second

// systemRootBundles are the PEM trust bundles hive looks for, first match
// wins. The relay hands one to each Job with the hive CA appended, so a Job
// that sets SSL_CERT_FILE still trusts the real internet.
var systemRootBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/cert.pem",
}

func readSystemRootsPEM() []byte {
	for _, p := range systemRootBundles {
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
			return data
		}
	}
	return nil
}

// wireSandboxGitHubReadRelay starts the sandbox GitHub read relay when
// agent_sandbox.job.github_read_proxy.enabled is true (#18865). Default off.
// Any failure here is logged and leaves Jobs on their existing network path;
// it never stops boot.
func (b *boot) wireSandboxGitHubReadRelay() {
	cfg := b.cfg.AgentSandbox.Job
	if b.githubProxy == nil || cfg == nil || cfg.GitHubReadProxy == nil || !cfg.GitHubReadProxy.Enabled {
		return
	}
	rp := cfg.GitHubReadProxy
	if err := rp.Validate(); err != nil {
		b.logger.Error("sandbox GitHub read relay not started", "error", err.Error())
		return
	}
	if b.agentMgr == nil {
		b.logger.Error("sandbox GitHub read relay not started: no agent manager")
		return
	}
	relay, err := proxy.NewSandboxReadRelay(b.githubProxy, proxy.SandboxReadRelayOptions{
		Endpoint: rp.Endpoint,
		Token:    b.sandboxReadToken,
		RootsPEM: readSystemRootsPEM(),
	})
	if err != nil {
		b.logger.Error("sandbox GitHub read relay not started", "error", err.Error())
		return
	}
	ln, err := net.Listen("tcp", rp.Listen())
	if err != nil {
		b.logger.Error("sandbox GitHub read relay not started: cannot listen", "addr", rp.Listen(), "error", err.Error())
		return
	}
	// TLS only: the per-kick key is carried in Proxy-Authorization, so a
	// plain-HTTP connection is never served. The certificate comes from the
	// hive proxy CA for the endpoint's host.
	tlsLn := tls.NewListener(ln, relay.TLSConfig())
	go func() {
		// Serve only returns when the listener fails, always with an error.
		b.logger.Error("sandbox GitHub read relay stopped", "error", relay.Serve(tlsLn).Error())
	}()
	b.agentMgr.SetSandboxGitHubReadRelay(relay, rp.NoProxy)
	b.logger.Info("sandbox GitHub read relay enabled (TLS): Job GitHub reads use a read-only, repo-scoped token held by hive; writes are refused", "listen", rp.Listen(), "endpoint", rp.Endpoint)
}

// sandboxReadToken mints the READ identity the relay attaches: the
// shared-uid-read tier (metadata, contents, actions, checks, pull requests and
// issues, all read), narrowed to the project's repositories. It is never the
// write-capable credential.
func (b *boot) sandboxReadToken() (string, error) {
	auth := b.appAuth
	if auth == nil {
		return "", errors.New("no GitHub App auth to mint a read token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxReadTokenTimeout)
	defer cancel()
	return auth.ScopedTokenForRepos(ctx, agentmode.TokenTierSharedUIDRead, b.cfg.Project.Repos)
}
