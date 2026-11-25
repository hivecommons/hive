package main

import (
	"crypto/tls"
	"net"

	"github.com/hivecommons/hive/crux/proxy"
)

// wireSandboxInferenceRelay starts the sandbox inference listener when
// agent_sandbox.job.inference_proxy.enabled is true (#19619). Default off.
// Any failure here is logged and leaves Jobs on their existing model path; it
// never stops boot.
func (b *boot) wireSandboxInferenceRelay() {
	cfg := b.cfg.AgentSandbox.Job
	if b.githubProxy == nil || cfg == nil || cfg.InferenceProxy == nil || !cfg.InferenceProxy.Enabled {
		return
	}
	ip := cfg.InferenceProxy
	if err := ip.Validate(); err != nil {
		b.logger.Error("sandbox inference listener not started", "error", err.Error())
		return
	}
	if b.agentMgr == nil {
		b.logger.Error("sandbox inference listener not started: no agent manager")
		return
	}
	relay, err := proxy.NewSandboxInferenceRelay(b.githubProxy, proxy.SandboxInferenceRelayOptions{
		Endpoint: ip.Endpoint,
		RootsPEM: readSystemRootsPEM(),
	})
	if err != nil {
		b.logger.Error("sandbox inference listener not started", "error", err.Error())
		return
	}
	ln, err := net.Listen("tcp", ip.Listen())
	if err != nil {
		b.logger.Error("sandbox inference listener not started: cannot listen", "addr", ip.Listen(), "error", err.Error())
		return
	}
	// TLS only: the per-kick key travels in a request header.
	tlsLn := tls.NewListener(ln, relay.TLSConfig())
	go func() {
		// Serve only returns when the listener fails, always with an error.
		b.logger.Error("sandbox inference listener stopped", "error", relay.Serve(tlsLn).Error())
	}()
	b.agentMgr.SetSandboxInferenceRelay(relay)
	b.logger.Info("sandbox inference listener enabled (TLS): Job kicks on a model gateway backend reach the translator with a per-kick key; the gateway key stays in hive", "listen", ip.Listen(), "endpoint", ip.Endpoint)
}
