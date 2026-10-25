package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/hivecap"
	"github.com/hivecommons/hive/crux/internal/testutil"
)

// policyTarget records what the policy applier hands the runtime.
type policyTarget struct {
	*fakeRuntime
	mu        sync.Mutex
	build     func() (string, string)
	gateway   func(string) bool
	explain   func() string
	autoMerge func(string) bool
	kickLog   func() string
	sandbox   []config.AgentSandboxConfig
	owner     []bool
}

func (p *policyTarget) SetBuildInfo(fn func() (string, string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.build = fn
}

func (p *policyTarget) SetGatewayBackendChecker(fn func(string) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gateway = fn
}

func (p *policyTarget) SetExplainModeDefaultResolver(fn func() string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.explain = fn
}

func (p *policyTarget) SetRepoAutoMergeEnabledResolver(fn func(string) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.autoMerge = fn
}

func (p *policyTarget) SetKickLogSourceResolver(fn func() string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kickLog = fn
}

func (p *policyTarget) SetSandboxConfig(cfg config.AgentSandboxConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sandbox = append(p.sandbox, cfg)
}

func (p *policyTarget) SetDeadSessionRecoveryOwner(owned bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.owner = append(p.owner, owned)
}

var _ hivePolicyTarget = (*policyTarget)(nil)

// policyHive is a hive callback that serves only the policy half.
type policyHive struct {
	callback.Capabilities
	mu     sync.Mutex
	policy callback.Policy
}

func (h *policyHive) Configured(c string) bool { return c == callback.CapabilityPolicy }

func (h *policyHive) Policy(context.Context) (callback.Policy, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.policy, nil
}

func (h *policyHive) RepoAutoMergeEnabled(_ context.Context, repo string) (bool, error) {
	return repo == "o/merge", nil
}

func (h *policyHive) set(p callback.Policy) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.policy = p
}

// Resolvers are installed once, when hive first reports the value; pushed
// values are set again only when they change; absent values leave the
// runtime's own defaults alone.
func TestPolicyApplier(t *testing.T) {
	explain := "on"
	owned := true
	gateways := []string{"gw"}
	hive := &policyHive{policy: callback.Policy{
		ExplainModeDefault:                &explain,
		GatewayBackends:                   &gateways,
		DeadSessionRecoveryOwnedElsewhere: &owned,
		Sandbox:                           json.RawMessage(`{"enabled":true,"image":"img"}`),
	}}
	pol := hivecap.NewPolicy(hive, time.Second)
	target := &policyTarget{fakeRuntime: &fakeRuntime{}}
	var errw strings.Builder
	a := newPolicyApplier(target, pol, &errw)

	first, err := pol.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.apply(first)
	if target.explain == nil || target.explain() != "on" || target.gateway == nil || !target.gateway("GW") {
		t.Fatal("resolvers not installed from the first snapshot")
	}
	if target.build != nil || target.kickLog != nil || target.autoMerge != nil {
		t.Fatal("absent values installed a resolver")
	}
	if len(target.owner) != 1 || !target.owner[0] || len(target.sandbox) != 1 || !target.sandbox[0].Enabled || target.sandbox[0].Image != "img" {
		t.Fatalf("pushed values = owner %v sandbox %+v", target.owner, target.sandbox)
	}

	a.apply(first)
	if len(target.owner) != 1 || len(target.sandbox) != 1 {
		t.Fatal("unchanged values set again")
	}

	explain2, owned2 := "off", false
	hive.set(callback.Policy{
		Build:                             &callback.BuildInfo{Version: "v5", Commit: "c"},
		ExplainModeDefault:                &explain2,
		RepoAutoMerge:                     true,
		DeadSessionRecoveryOwnedElsewhere: &owned2,
		Sandbox:                           json.RawMessage(`{"enabled":false}`),
	})
	second, err := pol.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	installedExplain := target.explain
	a.apply(second)
	if target.explain() != "off" || installedExplain() != "off" {
		t.Fatal("explain resolver does not read the live snapshot")
	}
	if v, c := target.build(); v != "v5" || c != "c" {
		t.Fatalf("build = %s %s", v, c)
	}
	if target.autoMerge == nil || !target.autoMerge("o/merge") || target.autoMerge("o/other") {
		t.Fatal("auto-merge resolver wrong")
	}
	if len(target.owner) != 2 || target.owner[1] || len(target.sandbox) != 2 || target.sandbox[1].Enabled {
		t.Fatalf("changed values = owner %v sandbox %+v", target.owner, target.sandbox)
	}

	a.apply(callback.Policy{Sandbox: json.RawMessage(`"not an object"`)})
	if len(target.sandbox) != 2 || !strings.Contains(errw.String(), "undecodable sandbox config") {
		t.Fatalf("bad sandbox applied or not reported: %d, %q", len(target.sandbox), errw.String())
	}
}

// watchPolicy feeds a policy-capable runtime from the hive callback, and
// leaves a runtime without the setters alone.
func TestWatchPolicyOverTheCallback(t *testing.T) {
	explain := "on"
	hive := &policyHive{policy: callback.Policy{ExplainModeDefault: &explain}}
	sock := filepath.Join(shortSocketDir(t), "hive.sock")
	ls, err := callback.Listen(callback.ListenConfig{Socket: sock})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = callback.NewServer(hive, "").Serve(ctx, ls) }()

	h, err := dialHiveCallback(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	var out, errw strings.Builder
	h.watchPolicy(ctx, &fakeRuntime{}, &out, &errw)
	if out.Len() != 0 {
		t.Fatalf("plain runtime announced host policy: %q", out.String())
	}

	target := &policyTarget{fakeRuntime: &fakeRuntime{}}
	h.watchPolicy(ctx, target, &out, &errw)
	if !strings.Contains(out.String(), ": host policy") {
		t.Fatalf("banner = %q", out.String())
	}
	explainFn := testutil.EventuallyValue(t, 5*time.Second, func() (func() string, bool) {
		target.mu.Lock()
		defer target.mu.Unlock()
		return target.explain, target.explain != nil
	}, "policy never applied over the callback")
	if got := explainFn(); got != "on" {
		t.Fatalf("explain = %q", got)
	}
}
