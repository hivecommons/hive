package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestIntegrationConfigDefaultOffAndBounds(t *testing.T) {
	if (IntegrationOrchestrationConfig{}).Effective().Mode != IntegrationOff {
		t.Fatal("default enabled")
	}

	if err := (IntegrationOrchestrationConfig{}).Validate(); err != nil {
		t.Fatal(err)
	}
	good := IntegrationOrchestrationConfig{Mode: IntegrationEnforce, Scopes: []IntegrationScope{{Repo: "o/r", Base: "v5"}}}
	if !good.Allows("O/R", "v5") || good.Allows("o/r", "V5") || good.Allows("other/r", "v5") {
		t.Fatal("scope matching")
	}
	for _, change := range []func(*IntegrationOrchestrationConfig){
		func(c *IntegrationOrchestrationConfig) { c.Mode = "shadow" }, func(c *IntegrationOrchestrationConfig) { c.Scopes = nil },
		func(c *IntegrationOrchestrationConfig) { c.MaxActive = -1 }, func(c *IntegrationOrchestrationConfig) { c.MaxActive = 33 },
		func(c *IntegrationOrchestrationConfig) { c.MaxJobs = 1001 }, func(c *IntegrationOrchestrationConfig) { c.MaxRunnerMinutes = math.NaN() },
		func(c *IntegrationOrchestrationConfig) { c.MaxRunnerMinutes = math.Inf(1) }, func(c *IntegrationOrchestrationConfig) { c.LeaseTTL = time.Hour },
		func(c *IntegrationOrchestrationConfig) { c.Window = 25 * time.Hour }, func(c *IntegrationOrchestrationConfig) { c.MaxObservationAge = time.Second },
		func(c *IntegrationOrchestrationConfig) { c.MaxAttempts = 11 },
		func(c *IntegrationOrchestrationConfig) { c.Scopes = []IntegrationScope{{Repo: "o/*", Base: "v5"}} },
		func(c *IntegrationOrchestrationConfig) { c.Scopes = []IntegrationScope{{Repo: "o/r", Base: ""}} },
		func(c *IntegrationOrchestrationConfig) { c.Scopes = append(c.Scopes, c.Scopes[0]) },
	} {
		bad := good
		bad.Scopes = append([]IntegrationScope(nil), good.Scopes...)
		change(&bad)
		if bad.Validate() == nil || bad.Allows("o/r", "v5") {
			t.Fatalf("invalid config admitted: %+v", bad)
		}
	}
}

func TestIntegrationPinnedPolicyValidationAndIdentity(t *testing.T) {
	policy := IntegrationPolicy{Repo: "o/r", Base: "v5", Engine: "native", Workflow: ".github/workflows/group.yml", Blob: strings.Repeat("a", 40), AppID: 42, Jobs: []string{"policy"}}
	good := IntegrationOrchestrationConfig{Mode: IntegrationAdvisory, Scopes: []IntegrationScope{{Repo: "o/r", Base: "v5"}}, Policies: []IntegrationPolicy{policy}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*IntegrationPolicy){
		func(p *IntegrationPolicy) { p.AppID = 0 }, func(p *IntegrationPolicy) { p.Repo = "other/r" },
		func(p *IntegrationPolicy) { p.Engine = "invented" }, func(p *IntegrationPolicy) { p.Workflow = "../../escape.yml" },
		func(p *IntegrationPolicy) { p.Blob = "not-a-full-blob" }, func(p *IntegrationPolicy) { p.Jobs = nil },
		func(p *IntegrationPolicy) { p.Jobs = []string{"policy", "policy"} }, func(p *IntegrationPolicy) { p.Jobs = []string{""} },
	} {
		bad := good
		bad.Policies = []IntegrationPolicy{policy}
		change(&bad.Policies[0])
		if bad.Validate() == nil {
			t.Fatalf("unbound policy accepted: %+v", bad)
		}
	}
	changed := policy
	changed.Jobs = []string{"policy", "required-extra"}
	if changed.Fingerprint() == policy.Fingerprint() {
		t.Fatal("policy requirements did not change evidence identity")
	}
	changed = policy
	changed.Repo = "O/R"
	if changed.Fingerprint() != policy.Fingerprint() {
		t.Fatal("repository case changed policy identity")
	}
	good.Policies = append(good.Policies, policy)
	if good.Validate() == nil {
		t.Fatal("duplicate policies accepted")
	}
}
