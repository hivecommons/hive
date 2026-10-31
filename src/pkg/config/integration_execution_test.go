package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

func executionConfigFixture() IntegrationExecutionConfig {
	return IntegrationExecutionConfig{
		Mode: IntegrationEnforce, MaxActive: 2, MaxJobs: 10, MaxAttempts: 1,
		MaxRunnerMinutes: 100, ObservationAge: time.Minute, Window: time.Hour, Cooldown: time.Minute,
		Scopes: []IntegrationExecutionScope{{
			Repo: "o/r", Base: "v5", Authority: "hive", RepositoryID: 1, RequestActorID: 2, CandidateActorID: 3,
			ProducerAppID: 4, MaterializerID: 5, CandidateWorkflowID: 6, DefaultBranch: "v5",
			PromotionAppID: 7, PromotionInstallationID: 8,
			Policy: strings.Repeat("a", 64), WorkflowTree: strings.Repeat("b", 40), RequiredJobs: []string{"tests"},
			ExecutorID: strings.Repeat("c", 64), AuthorityRef: "refs/heads/hive-integration-authority/v5",
			AuthorityCommit: strings.Repeat("d", 40), QualificationID: "qualification",
			MaxMembers: 2, ExpectedJobs: 2, ExpectedMinutes: 10,
		}},
	}
}

func TestIntegrationExecutionDefaultsAndExplicitScopes(t *testing.T) {
	var zero IntegrationExecutionConfig
	if zero.Effective().Mode != IntegrationOff || zero.Validate() != nil {
		t.Fatal("zero execution must remain OFF")
	}
	if _, ok := zero.Scope("o/r", "v5"); ok {
		t.Fatal("OFF enrolled a writer")
	}
	c := executionConfigFixture()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Scope("O/R", "v5"); !ok {
		t.Fatal("repository case should not change enrollment")
	}
	if _, ok := c.Scope("o/r", "V5"); ok {
		t.Fatal("base enrollment must preserve case")
	}
	if _, ok := c.Scope("o/other", "v5"); ok {
		t.Fatal("implicit repository enrollment")
	}
	c.Mode = IntegrationOff
	if err := c.Validate(); err != nil {
		t.Fatal("rollback should retain valid configured scopes:", err)
	}
	if _, ok := c.Scope("o/r", "v5"); ok {
		t.Fatal("rollback retained a writer")
	}
	c.Mode, c.Scopes[0].Authority = IntegrationAdvisory, "native"
	if err := c.Validate(); err != nil {
		t.Fatal("native admission must retain designated executor enrollment:", err)
	}
}

func TestIntegrationExecutionRejectsUnboundedOrUnqualifiedConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*IntegrationExecutionConfig)
	}{
		{"mode", func(c *IntegrationExecutionConfig) { c.Mode = "on" }},
		{"scopes", func(c *IntegrationExecutionConfig) { c.Scopes = nil }},
		{"active", func(c *IntegrationExecutionConfig) { c.MaxActive = MaxIntegrationExecutionCandidates + 1 }},
		{"jobs", func(c *IntegrationExecutionConfig) { c.MaxJobs = 0 }},
		{"attempts", func(c *IntegrationExecutionConfig) { c.MaxAttempts = DefaultIntegrationMaxAttempts + 1 }},
		{"nan", func(c *IntegrationExecutionConfig) { c.MaxRunnerMinutes = math.NaN() }},
		{"infinity", func(c *IntegrationExecutionConfig) { c.MaxRunnerMinutes = math.Inf(1) }},
		{"minutes", func(c *IntegrationExecutionConfig) { c.MaxRunnerMinutes = MaxIntegrationExecutionMinutes + 1 }},
		{"age", func(c *IntegrationExecutionConfig) { c.ObservationAge = time.Second }},
		{"window", func(c *IntegrationExecutionConfig) { c.Window = MaxIntegrationExecutionWindow + time.Second }},
		{"cooldown", func(c *IntegrationExecutionConfig) { c.Cooldown = 0 }},
		{"repository-path", func(c *IntegrationExecutionConfig) { c.Scopes[0].Repo = "../r" }},
		{"repository-query", func(c *IntegrationExecutionConfig) { c.Scopes[0].Repo = "o/r?query" }},
		{"branch", func(c *IntegrationExecutionConfig) { c.Scopes[0].Base = "bad/\x01branch" }},
		{"branch-lock", func(c *IntegrationExecutionConfig) { c.Scopes[0].Base = "v5.lock" }},
		{"branch-dot", func(c *IntegrationExecutionConfig) { c.Scopes[0].Base = "." }},
		{"duplicate", func(c *IntegrationExecutionConfig) { c.Scopes = append(c.Scopes, c.Scopes[0]) }},
		{"authority", func(c *IntegrationExecutionConfig) { c.Scopes[0].Authority = "direct-fallback" }},
		{"actor", func(c *IntegrationExecutionConfig) { c.Scopes[0].CandidateActorID = 0 }},
		{"workflow", func(c *IntegrationExecutionConfig) { c.Scopes[0].MaterializerID = 0 }},
		{"members", func(c *IntegrationExecutionConfig) { c.Scopes[0].MaxMembers = 0 }},
		{"expected-jobs", func(c *IntegrationExecutionConfig) { c.Scopes[0].ExpectedJobs = 0 }},
		{"expected-minutes", func(c *IntegrationExecutionConfig) { c.Scopes[0].ExpectedMinutes = math.NaN() }},
		{"policy", func(c *IntegrationExecutionConfig) { c.Scopes[0].Policy = strings.Repeat("0", 64) }},
		{"closure", func(c *IntegrationExecutionConfig) { c.Scopes[0].WorkflowTree = "short" }},
		{"executor", func(c *IntegrationExecutionConfig) { c.Scopes[0].ExecutorID = "" }},
		{"authority-ref", func(c *IntegrationExecutionConfig) { c.Scopes[0].AuthorityRef = "refs/heads/v5" }},
		{"authority-oid", func(c *IntegrationExecutionConfig) { c.Scopes[0].AuthorityCommit = "" }},
		{"qualification", func(c *IntegrationExecutionConfig) { c.Scopes[0].QualificationID = "" }},
		{"job-names", func(c *IntegrationExecutionConfig) { c.Scopes[0].RequiredJobs = []string{"tests", "tests"} }},
		{"blank-job", func(c *IntegrationExecutionConfig) { c.Scopes[0].RequiredJobs = []string{" "} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := executionConfigFixture()
			test.change(&c)
			if c.Validate() == nil {
				t.Fatalf("accepted unsafe execution config: %+v", c)
			}
			if _, ok := c.Scope("o/r", "v5"); ok {
				t.Fatal("invalid configuration enrolled a scope")
			}
		})
	}
	c := IntegrationExecutionConfig{MaxActive: -1}
	if c.Validate() == nil {
		t.Fatal("OFF silently accepted nonsensical explicit unscoped bounds")
	}
}
