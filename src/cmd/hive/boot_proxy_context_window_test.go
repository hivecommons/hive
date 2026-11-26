package main

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/proxy"
)

// windowLookupHost is a crux.Host that takes the proxy's context-window
// lookup, like the in-process agent manager.
type windowLookupHost struct {
	crux.Host
	fn func(string) (int, string)
}

func (h *windowLookupHost) SetInferenceContextWindowLookup(fn func(agentName string) (window int, model string)) {
	h.fn = fn
}

// The boot wires the proxy's per-agent window into the manager so the CLI is
// told its window at launch (hivecommons/hive#19913).
func TestInstallInferenceContextWindowLookup(t *testing.T) {
	p, err := proxy.NewGitHubProxyEphemeral(slog.New(slog.DiscardHandler), "acme", []string{"widgets"})
	if err != nil {
		t.Fatal(err)
	}
	p.SetInferenceRoute("quality", &proxy.InferenceRoute{Backend: "vllm", Endpoint: "http://127.0.0.1:1", Model: "Qwen/Qwen3", MaxContextLen: 262144})

	h := &windowLookupHost{}
	installInferenceContextWindowLookup(h, p)
	if h.fn == nil {
		t.Fatal("lookup not installed on a host that takes one")
	}
	if w, model := h.fn("quality"); w != 262144 || model != "Qwen/Qwen3" {
		t.Fatalf("lookup = (%d, %q), want (262144, Qwen/Qwen3)", w, model)
	}

	// A nil proxy installs nothing.
	h2 := &windowLookupHost{}
	installInferenceContextWindowLookup(h2, nil)
	if h2.fn != nil {
		t.Fatal("lookup installed without a proxy")
	}

	// A host without the method (the sidecar client) is left alone.
	var plain crux.Host
	installInferenceContextWindowLookup(plain, p)
}

// bootProxyWith hands the proxy it built to the lookup installer.
func TestBootProxyWith_InstallsContextWindowLookup(t *testing.T) {
	f := newBootProxyFake()
	var gotProxy *proxy.GitHubProxy
	calls := 0
	f.deps.installContextWindowLookup = func(_ crux.Host, p *proxy.GitHubProxy) {
		calls++
		gotProxy = p
	}
	b := newBootProxyBoot(t, f, bootProxyConfig())

	b.bootProxyWith(f.deps)

	if calls != 1 || gotProxy == nil || gotProxy != f.proxy {
		t.Fatalf("installer called %d times with proxy %p, want once with %p", calls, gotProxy, f.proxy)
	}
}
