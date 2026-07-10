package automation

import "testing"

func TestACMMLowerLevelsCannotWriteWithCredentialsAvailable(t *testing.T) {
	actions := []Action{ActionCreateIssue, ActionRepairModel, ActionCreateBranch, ActionCommit, ActionPush, ActionCreatePR, ActionMergePR}
	for _, level := range []int{1, 2} {
		policy := Policy{ACMMLevel: level, Mode: ModeAutoMerge, AllowedRepositories: []string{"owner/repo"}}
		for _, action := range actions {
			decision := policy.Authorize(ActionRequest{Action: action, Agent: "quality", Repository: "owner/repo"})
			if decision.Allowed {
				t.Fatalf("ACMM L%d unexpectedly allowed %s", level, action)
			}
		}
	}
}

func TestAutomationModeCapsACMMCapability(t *testing.T) {
	policy := Policy{ACMMLevel: 6, Mode: ModeIssues, AllowedRepositories: []string{"owner/repo"}}
	if !policy.Authorize(ActionRequest{Action: ActionCreateIssue, Agent: "quality", Repository: "owner/repo"}).Allowed {
		t.Fatal("issues mode must allow issue creation at L6")
	}
	if policy.Authorize(ActionRequest{Action: ActionCreatePR, Agent: "quality", Repository: "owner/repo"}).Allowed {
		t.Fatal("issues mode must block PR creation even at L6")
	}
	if policy.Authorize(ActionRequest{Action: ActionMergePR, Agent: "quality", Repository: "owner/repo"}).Allowed {
		t.Fatal("issues mode must block merge even at L6")
	}
}

func TestACMMSelectiveAgentCapabilities(t *testing.T) {
	tests := []struct {
		name    string
		level   int
		agent   string
		action  Action
		allowed bool
	}{
		{"quality L3 repair PR", 3, "quality", ActionCreatePR, true},
		{"scanner L3 issue denied", 3, "scanner", ActionCreateIssue, false},
		{"scanner L4 issue", 4, "scanner", ActionCreateIssue, true},
		{"scanner L4 PR denied", 4, "scanner", ActionCreatePR, false},
		{"security L4 PR", 4, "sec-check", ActionCreatePR, true},
		{"quality L5 merge denied", 5, "quality", ActionMergePR, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := (Policy{ACMMLevel: test.level, Mode: ModeAutoMerge, AllowedRepositories: []string{"owner/repo"}}).Authorize(ActionRequest{
				Action: test.action, Agent: test.agent, Repository: "owner/repo",
			})
			if decision.Allowed != test.allowed {
				t.Fatalf("allowed=%t want %t reasons=%v", decision.Allowed, test.allowed, decision.Reasons)
			}
		})
	}
}

func TestAutoMergeRequiresEveryProductionGate(t *testing.T) {
	policy := Policy{
		ACMMLevel: 6, Mode: ModeAutoMerge, AllowedRepositories: []string{"owner/repo"},
		AllowedAutoMergeRisk: []RiskTier{RiskAutomatic}, MaxRepairAttempts: 3,
	}
	valid := ActionRequest{
		Action: ActionMergePR, Agent: "quality", Repository: "owner/repo", Risk: RiskAutomatic,
		ChangedFiles: []string{"src/__tests__/app.test.ts"}, RepairAttempts: 1,
		ExpectedHeadSHA: "abc", TestedHeadSHA: "abc", VisualHiveVerdictGreen: true,
		MergeableKnown: true, Mergeable: true,
		RequiredCheckStates: []string{"success", "success"}, BranchProtectionEnabled: true,
	}
	if decision := policy.Authorize(valid); !decision.Allowed {
		t.Fatalf("valid merge was denied: %v", decision.Reasons)
	}

	cases := map[string]func(*ActionRequest){
		"stale SHA":        func(r *ActionRequest) { r.TestedHeadSHA = "old" },
		"merge conflict":   func(r *ActionRequest) { r.Mergeable = false },
		"red visual":       func(r *ActionRequest) { r.VisualHiveVerdictGreen = false },
		"pending check":    func(r *ActionRequest) { r.RequiredCheckStates = []string{"success", "pending"} },
		"no protection":    func(r *ActionRequest) { r.BranchProtectionEnabled = false },
		"hold":             func(r *ActionRequest) { r.Hold = true },
		"baseline":         func(r *ActionRequest) { r.BaselineChanged = true },
		"workflow":         func(r *ActionRequest) { r.WorkflowChanged = true },
		"unsafe path":      func(r *ActionRequest) { r.ChangedFiles = []string{"src/auth/session.ts"} },
		"budget exhausted": func(r *ActionRequest) { r.RepairAttempts = 3 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := valid
			request.ChangedFiles = append([]string(nil), valid.ChangedFiles...)
			request.RequiredCheckStates = append([]string(nil), valid.RequiredCheckStates...)
			mutate(&request)
			if decision := policy.Authorize(request); decision.Allowed {
				t.Fatalf("unsafe merge was allowed: %+v", request)
			}
		})
	}
}

func TestPauseKillSwitchAndRepositoryAllowlist(t *testing.T) {
	request := ActionRequest{Action: ActionCreateIssue, Agent: "quality", Repository: "owner/repo"}
	for _, policy := range []Policy{
		{ACMMLevel: 6, Mode: ModeIssues, Paused: true, AllowedRepositories: []string{"owner/repo"}},
		{ACMMLevel: 6, Mode: ModeIssues, KillSwitch: true, AllowedRepositories: []string{"owner/repo"}},
		{ACMMLevel: 6, Mode: ModeIssues, AllowedRepositories: []string{"owner/other"}},
	} {
		if policy.Authorize(request).Allowed {
			t.Fatalf("safety policy unexpectedly allowed issue write: %+v", policy)
		}
	}
}

func TestSetupWritesRequireExplicitApprovalButNotAutonomousMode(t *testing.T) {
	policy := Policy{ACMMLevel: 1, Mode: ModeAdvisory, AllowedRepositories: []string{"owner/repo"}}
	denied := policy.Authorize(ActionRequest{Action: ActionSetupPR, Agent: "setup", Repository: "owner/repo"})
	if denied.Allowed {
		t.Fatal("setup PR must require explicit setup approval")
	}
	allowed := policy.Authorize(ActionRequest{Action: ActionSetupPR, Agent: "setup", Repository: "owner/repo", SetupApproved: true})
	if !allowed.Allowed {
		t.Fatalf("explicitly approved setup PR should not be conflated with autonomous authority: %+v", allowed)
	}
}
