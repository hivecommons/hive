package toolapprove

import (
	"context"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
)

// Decide evaluates a tool request against ACMM gates, agent identity, and tool policy,
// returning an initial approval verdict: auto-approve, security-scan, operator-approve, or deny.
func Decide(ctx context.Context, req ToolRequest, acmmLevel int, agentId AgentIdentity) Verdict {
	toolName := req.Tool
	agentName := agentId.Name

	// 1. Hard deny guardrails — apply to all agents and all ACMM levels
	if IsDirectPRCreation(toolName) {
		return Verdict{
			Decision:  DecisionDeny,
			Rationale: "direct PR creation is disabled for agents — use `hive-open-pr` so the hive opens the PR as the App bot (never the login user)",
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}
	}
	if IsDirectPRMerge(toolName) {
		return Verdict{
			Decision:  DecisionDeny,
			Rationale: "direct PR merge is disabled for agents — use `hive-merge` so the hive merges as the App bot with SHA-pin and merge-eligible binding",
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}
	}

	// 2. Configured agent tool rules (explicit deny patterns)
	if agentId.ToolsConfig != nil {
		for _, rule := range agentId.ToolsConfig.EffectiveRules() {
			if rule.Action == "deny" && (rule.Pattern == toolName || config.WildcardMatch(toolName, rule.Pattern)) {
				return Verdict{
					Decision:  DecisionDeny,
					Rationale: fmt.Sprintf("tool %q is explicitly denied by agent tool policy", toolName),
					Tool:      toolName,
					ACMMLevel: acmmLevel,
					Agent:     agentName,
				}
			}
		}
	}

	// 3. Repo allowlist filtering
	if len(agentId.AllowedRepos) > 0 {
		repo := req.GetRepo()
		if repo != "" && !agentId.AllowedRepos[repo] {
			return Verdict{
				Decision:  DecisionDeny,
				Rationale: fmt.Sprintf("target repository %q is not in the agent's allowed repos allowlist", repo),
				Tool:      toolName,
				ACMMLevel: acmmLevel,
				Agent:     agentName,
			}
		}
	}

	// 4. AgentMode capability restrictions
	if IsGitHubIssueOperation(toolName) && !agentId.Mode.CanCreateIssues() {
		return Verdict{
			Decision:  DecisionDeny,
			Rationale: fmt.Sprintf("agent mode %s does not allow GitHub issue operations", agentId.Mode),
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}
	}
	if IsGitHubPROperation(toolName) && !agentId.Mode.CanCreatePRs() {
		return Verdict{
			Decision:  DecisionDeny,
			Rationale: fmt.Sprintf("agent mode %s does not allow PR operations", agentId.Mode),
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}
	}

	// 5. Read-only / safe inspection tools — auto-approve at all ACMM levels
	if req.IsReadOnly() {
		return Verdict{
			Decision:  DecisionAutoApprove,
			Rationale: "read-only inspection tool auto-approved",
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}
	}

	// 6. ACMM maturity level gating for side-effectful tools
	switch {
	case acmmLevel <= 1:
		// L1 Assisted: all side-effectful actions require operator approval
		return Verdict{
			Decision:  DecisionOperatorApprove,
			Rationale: "ACMM L1 (assisted mode): side-effectful tool requires operator approval",
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}

	case acmmLevel == 2:
		// L2 Advisory: mutations denied or require operator approval
		if IsGitHubWrite(toolName) {
			return Verdict{
				Decision:  DecisionDeny,
				Rationale: "ACMM L2 (advisory mode): GitHub mutations not permitted",
				Tool:      toolName,
				ACMMLevel: acmmLevel,
				Agent:     agentName,
			}
		}
		return Verdict{
			Decision:  DecisionOperatorApprove,
			Rationale: "ACMM L2 (advisory mode): side-effectful tool requires operator approval",
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}

	case acmmLevel == 3:
		// L3 Measured: PR writes require operator approval; local side-effects route through security scan
		if IsGitHubPROperation(toolName) {
			return Verdict{
				Decision:  DecisionOperatorApprove,
				Rationale: "ACMM L3 (measured mode): PR operation requires operator approval",
				Tool:      toolName,
				ACMMLevel: acmmLevel,
				Agent:     agentName,
			}
		}
		return Verdict{
			Decision:  DecisionSecurityScan,
			Rationale: "ACMM L3 (measured mode): side-effectful tool routes through pre-execution security scan",
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}

	case acmmLevel == 4:
		// L4 Hold-Gated: side-effectful tools require operator approval
		return Verdict{
			Decision:  DecisionOperatorApprove,
			Rationale: "ACMM L4 (hold-gated mode): side-effectful tool requires operator approval",
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}

	case acmmLevel == 5:
		// L5 Hold-Gated Autonomous: side-effectful tools route through security scan
		return Verdict{
			Decision:  DecisionSecurityScan,
			Rationale: "ACMM L5 (hold-gated mode): side-effectful tool routes through pre-execution security scan",
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}

	default: // L6 Full Autonomy
		// L6 Full: side-effectful tools route through security scan, auto-approve on green
		return Verdict{
			Decision:  DecisionSecurityScan,
			Rationale: "ACMM L6 (full autonomy): side-effectful tool routes through pre-execution security scan; auto-approves on green",
			Tool:      toolName,
			ACMMLevel: acmmLevel,
			Agent:     agentName,
		}
	}
}

// Resolve executes the full tool-approval decision flow: evaluates initial ACMM gates,
// and if a security scan is requested, routes through the provided SecurityScanner before
// returning the final Verdict (auto-approve, operator-approve, or deny).
func Resolve(ctx context.Context, req ToolRequest, acmmLevel int, agentId AgentIdentity, scanner SecurityScanner) Verdict {
	v := Decide(ctx, req, acmmLevel, agentId)
	if v.Decision != DecisionSecurityScan {
		return v
	}

	if scanner == nil {
		scanner = NewDefaultSecurityScanner()
	}

	scanRes, err := scanner.Scan(ctx, req)
	if err != nil {
		return Verdict{
			Decision:  DecisionDeny,
			Rationale: fmt.Sprintf("security scan error: %v", err),
			Tool:      req.Tool,
			ACMMLevel: acmmLevel,
			Agent:     agentId.Name,
		}
	}

	v.ScanResult = &scanRes

	if !scanRes.Passed {
		v.Decision = DecisionDeny
		v.Rationale = fmt.Sprintf("security scan failed: %s", strings.Join(scanRes.Violations, "; "))
		return v
	}

	// Security scan passed (green)
	switch {
	case acmmLevel >= 6:
		v.Decision = DecisionAutoApprove
		v.Rationale = "ACMM L6 full autonomy: auto-approved on green security scan"
	case acmmLevel == 5:
		v.Decision = DecisionAutoApprove
		v.Rationale = "ACMM L5 hold-gated: auto-approved on green security scan"
	case acmmLevel == 4:
		v.Decision = DecisionOperatorApprove
		v.Rationale = "security scan green; ACMM L4 requires operator approval for side-effectful tool"
	case acmmLevel == 3:
		v.Decision = DecisionAutoApprove
		v.Rationale = "ACMM L3 measured: auto-approved on green security scan"
	default:
		v.Decision = DecisionOperatorApprove
		v.Rationale = fmt.Sprintf("security scan green; ACMM L%d requires operator approval", acmmLevel)
	}

	return v
}

// EvaluateAndAudit runs Resolve and emits the verdict to the provided AuditSink if configured.
func EvaluateAndAudit(ctx context.Context, req ToolRequest, acmmLevel int, agentId AgentIdentity, scanner SecurityScanner, sink agent.AuditSink, actor string) Verdict {
	v := Resolve(ctx, req, acmmLevel, agentId, scanner)
	if sink != nil {
		if actor == "" {
			actor = "system"
		}
		sink.Record(actor, agent.AuditToolApproval, agentId.Name, v.AuditFields())
	}
	return v
}
