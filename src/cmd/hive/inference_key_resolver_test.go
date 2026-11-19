package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestInferenceKeyResolvers_ReadTheLiveConfig: the resolvers handed to the
// proxy (hive#18532) re-resolve from the live config each time, so a changed
// key file or env var reaches the next request without a restart.
func TestInferenceKeyResolvers_ReadTheLiveConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.SetSecretFileRootsForTest(root))
	keyFile := filepath.Join(root, "gw_key")
	if err := os.WriteFile(keyFile, []byte("gw-one"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := bootProxyConfig()
	cfg.Governor.Gateways = []config.GatewayConfig{{Name: "corp", Kind: config.GatewayKindLiteLLM, APIKeyFile: keyFile}}

	gw := gatewayInferenceKeyResolver(cfg, &cfg.Governor.Gateways[0], "corp")
	if gw == nil {
		t.Fatal("a litellm-kind gateway must get a resolver")
	}
	if key, src, _ := gw.Refresh(); key != "gw-one" || src != "file:"+keyFile {
		t.Fatalf("gateway resolver = (%q, %q)", key, src)
	}
	if err := os.WriteFile(keyFile, []byte("gw-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, _, changed := gw.Refresh(); key != "gw-two" || !changed {
		t.Fatalf("gateway resolver did not pick up the rewritten file: %q changed=%v", key, changed)
	}
	cfg.Governor.Gateways = nil
	if key, src, _ := gw.Refresh(); key != "" || src != "" {
		t.Fatalf("a gateway removed from the live config must resolve nothing, got (%q, %q)", key, src)
	}

	if gatewayInferenceKeyResolver(cfg, nil, "x") != nil {
		t.Fatal("a nil gateway gets no resolver")
	}
	if gatewayInferenceKeyResolver(cfg, &config.GatewayConfig{Name: "ibm", Kind: config.GatewayKindWatsonx}, "ibm") != nil {
		t.Fatal("watsonx carries a minted IAM token and must keep a static key")
	}

	t.Setenv(config.DefaultLiteLLMAPIKeyEnv, "legacy-one")
	ll := litellmInferenceKeyResolver(cfg, "litellm")
	if key, src, _ := ll.Refresh(); key != "legacy-one" || src != "env:"+config.DefaultLiteLLMAPIKeyEnv {
		t.Fatalf("litellm resolver = (%q, %q)", key, src)
	}
	t.Setenv(config.DefaultLiteLLMAPIKeyEnv, "legacy-two")
	if key, _, changed := ll.Refresh(); key != "legacy-two" || !changed {
		t.Fatalf("litellm resolver did not follow the env: %q changed=%v", key, changed)
	}
}

// TestBootProxyWith_RoutesCarryKeyResolvers: the named-gateway and litellm
// branches of the route callback attach a resolver; vllm does not.
func TestBootProxyWith_RoutesCarryKeyResolvers(t *testing.T) {
	cfg := bootProxyConfig()
	cfg.Governor.LiteLLM.Endpoint = "http://127.0.0.1:1"
	cfg.Governor.Gateways = []config.GatewayConfig{
		{Name: "openrouter", Kind: "openrouter", Endpoint: "https://openrouter.ai/api/v1", DefaultModel: "or/default"},
	}
	f := newBootProxyFake()
	b := newBootProxyBoot(t, f, cfg)
	b.bootProxyWith(f.deps)

	f.setRoute("scanner", "openrouter", "")
	if r := f.routes["scanner"]; r == nil || r.KeyResolver == nil {
		t.Fatalf("gateway route = %+v, want a key resolver", r)
	}
}

func TestBootProxyWith_LiteLLMRouteCarriesKeyResolver(t *testing.T) {
	cfg := bootProxyConfig()
	cfg.Governor.LiteLLM.Endpoint = "http://127.0.0.1:1"
	f := newBootProxyFake()
	b := newBootProxyBoot(t, f, cfg)
	b.bootProxyWith(f.deps)

	f.setRoute("triage", "litellm", "m")
	if r := f.routes["triage"]; r == nil || r.KeyResolver == nil {
		t.Fatalf("litellm route = %+v, want a key resolver", r)
	}
}
