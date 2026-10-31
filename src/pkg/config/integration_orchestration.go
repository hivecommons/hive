package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	IntegrationOff                     = "off"
	IntegrationAdvisory                = "advisory"
	IntegrationEnforce                 = "enforce"
	DefaultIntegrationLease            = 5 * time.Minute
	DefaultIntegrationWindow           = time.Hour
	DefaultIntegrationMaxActive        = 2
	DefaultIntegrationRunnerMinutes    = 60
	DefaultIntegrationMaxJobs          = 20
	DefaultIntegrationMaxAttempts      = 2
	DefaultIntegrationMaxAge           = 30 * time.Minute
	DefaultIntegrationAdmissionMinutes = 10
	DefaultIntegrationAdmissionJobs    = 1
	MaxIntegrationScopes               = 8
)

// IntegrationOrchestrationConfig controls admission, never merge authority.
// Scopes must name exact repository/base pairs; an empty list enrolls nothing.
type IntegrationOrchestrationConfig struct {
	Mode                   string                     `yaml:"mode,omitempty" json:"mode"`
	Scopes                 []IntegrationScope         `yaml:"scopes,omitempty" json:"scopes"`
	LeaseTTL               time.Duration              `yaml:"lease_ttl,omitempty" json:"lease_ttl"`
	Window                 time.Duration              `yaml:"window,omitempty" json:"window"`
	MaxActive              int                        `yaml:"max_active,omitempty" json:"max_active"`
	MaxRunnerMinutes       float64                    `yaml:"max_runner_minutes,omitempty" json:"max_runner_minutes"`
	MaxJobs                int                        `yaml:"max_jobs,omitempty" json:"max_jobs"`
	MaxAttempts            int                        `yaml:"max_attempts,omitempty" json:"max_attempts"`
	MaxObservationAge      time.Duration              `yaml:"max_observation_age,omitempty" json:"max_observation_age"`
	AdmissionRunnerMinutes float64                    `yaml:"admission_runner_minutes,omitempty" json:"admission_runner_minutes"`
	AdmissionJobs          int                        `yaml:"admission_jobs,omitempty" json:"admission_jobs"`
	Policies               []IntegrationPolicy        `yaml:"policies,omitempty" json:"policies"`
	Execution              IntegrationExecutionConfig `yaml:"execution,omitempty" json:"execution"`
}

// IntegrationPolicy pins an already-reviewed workflow blob and its exact jobs.
// It is only an evidence-reader contract, never a replacement required context.
type IntegrationPolicy struct {
	Repo     string   `yaml:"repo" json:"repo"`
	Base     string   `yaml:"base" json:"base"`
	Engine   string   `yaml:"engine" json:"engine"`
	Workflow string   `yaml:"workflow" json:"workflow"`
	Blob     string   `yaml:"blob" json:"blob"`
	AppID    int64    `yaml:"app_id" json:"app_id"`
	Jobs     []string `yaml:"jobs" json:"jobs"`
}

func (p IntegrationPolicy) Fingerprint() string {
	p.Repo = strings.ToLower(p.Repo)
	data, _ := json.Marshal(p)
	sum := sha256.Sum256(data)
	return "workflow-policy-v1/" + hex.EncodeToString(sum[:])
}

type IntegrationScope struct {
	Repo string `yaml:"repo" json:"repo"`
	Base string `yaml:"base" json:"base"`
}

func (c IntegrationOrchestrationConfig) Effective() IntegrationOrchestrationConfig {
	if c.Mode == "" {
		c.Mode = IntegrationOff
	}
	if c.AdmissionRunnerMinutes == 0 {
		c.AdmissionRunnerMinutes = DefaultIntegrationAdmissionMinutes
	}
	if c.AdmissionJobs == 0 {
		c.AdmissionJobs = DefaultIntegrationAdmissionJobs
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = DefaultIntegrationLease
	}
	if c.Window == 0 {
		c.Window = DefaultIntegrationWindow
	}
	if c.MaxActive == 0 {
		c.MaxActive = DefaultIntegrationMaxActive
	}
	if c.MaxRunnerMinutes == 0 {
		c.MaxRunnerMinutes = DefaultIntegrationRunnerMinutes
	}
	if c.MaxJobs == 0 {
		c.MaxJobs = DefaultIntegrationMaxJobs
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = DefaultIntegrationMaxAttempts
	}
	if c.MaxObservationAge == 0 {
		c.MaxObservationAge = DefaultIntegrationMaxAge
	}
	return c
}

func (c IntegrationOrchestrationConfig) Validate() error {
	c = c.Effective()
	if err := c.Execution.Validate(); err != nil {
		return err
	}
	if c.Execution.Effective().Mode == IntegrationEnforce {
		for _, execution := range c.Execution.Scopes {
			observed := false
			for _, authoring := range c.Scopes {
				if strings.EqualFold(authoring.Repo, execution.Repo) && authoring.Base == execution.Base {
					observed = true
					break
				}
			}
			if c.Mode == IntegrationOff || !observed {
				return fmt.Errorf("enforced execution requires its exact destination's existing authoring/CI observer in advisory or enforce mode")
			}
		}
	}
	if len(c.Scopes) > MaxIntegrationScopes || len(c.Policies) > MaxIntegrationScopes || c.AdmissionJobs < 1 || c.AdmissionJobs > c.MaxJobs ||
		math.IsNaN(c.AdmissionRunnerMinutes) || math.IsInf(c.AdmissionRunnerMinutes, 0) ||
		c.AdmissionRunnerMinutes <= 0 || c.AdmissionRunnerMinutes > c.MaxRunnerMinutes {
		return fmt.Errorf("integration admission allocations must fit scope budgets; at most %d scopes", MaxIntegrationScopes)
	}
	if c.Mode != IntegrationOff && c.Mode != IntegrationAdvisory && c.Mode != IntegrationEnforce {
		return fmt.Errorf("integration_orchestration.mode must be off, advisory or enforce")
	}
	if c.LeaseTTL < time.Minute || c.LeaseTTL > 15*time.Minute ||
		c.Window < time.Minute || c.Window > 24*time.Hour ||
		c.MaxObservationAge < time.Minute || c.MaxObservationAge > time.Hour ||
		c.MaxActive < 1 || c.MaxActive > 32 || c.MaxJobs < 1 || c.MaxJobs > 1000 ||
		c.MaxAttempts < 1 || c.MaxAttempts > 10 || math.IsNaN(c.MaxRunnerMinutes) || math.IsInf(c.MaxRunnerMinutes, 0) || c.MaxRunnerMinutes <= 0 || c.MaxRunnerMinutes > 10000 {
		return fmt.Errorf("integration_orchestration bounds invalid: lease 1-15m, window 1m-24h, observation age 1m-1h, active 1-32, jobs 1-1000, attempts 1-10, minutes (0,10000]")
	}
	seen := map[string]bool{}
	for _, s := range c.Scopes {
		parts := strings.Split(s.Repo, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(s.Repo+s.Base, "*? \t\n") || s.Base == "" {
			return fmt.Errorf("integration_orchestration scopes require exact owner/repo and base")
		}
		k := strings.ToLower(s.Repo) + "#" + s.Base
		if seen[k] {
			return fmt.Errorf("duplicate integration_orchestration scope %s", k)
		}
		seen[k] = true
	}
	policies := map[string]bool{}
	for _, p := range c.Policies {
		enrolled := false
		for _, scope := range c.Scopes {
			if strings.EqualFold(scope.Repo, p.Repo) && scope.Base == p.Base {
				enrolled = true
			}
		}
		if !enrolled || (p.Engine != "direct" && p.Engine != "native") || !strings.HasPrefix(p.Workflow, ".github/workflows/") ||
			strings.Contains(p.Workflow, "..") || len(p.Blob) != 40 || strings.Trim(p.Blob, "0123456789abcdef") != "" ||
			p.AppID <= 0 || len(p.Jobs) == 0 || len(p.Jobs) > c.MaxJobs {
			return fmt.Errorf("integration policy requires enrolled scope, engine, workflow blob, producer app and bounded exact jobs")
		}
		key := strings.ToLower(p.Repo) + "#" + p.Base + "/" + p.Engine + "/" + p.Workflow
		if policies[key] {
			return fmt.Errorf("duplicate integration policy")
		}
		policies[key] = true
		names := map[string]bool{}
		for _, name := range p.Jobs {
			if strings.TrimSpace(name) == "" || names[name] {
				return fmt.Errorf("integration policy jobs must be unique nonempty names")
			}
			names[name] = true
		}
	}
	if c.Mode != IntegrationOff && len(c.Scopes) == 0 {
		return fmt.Errorf("integration_orchestration requires explicit scopes when enabled")
	}
	return nil
}

func (c IntegrationOrchestrationConfig) Allows(repo, base string) bool {
	if c.Validate() != nil || c.Effective().Mode == IntegrationOff {
		return false
	}
	for _, s := range c.Scopes {
		if strings.EqualFold(s.Repo, repo) && s.Base == base {
			return true
		}
	}
	return false
}
