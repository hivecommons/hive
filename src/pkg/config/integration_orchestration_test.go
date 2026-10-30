package config

import (
	"math"
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
