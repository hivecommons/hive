package automation

import "testing"

func TestACMMLowerLevelsCannotWriteWithCredentialsAvailable(t *testing.T) {
	actions := []Action{ActionCreateIssue, ActionRepairModel, ActionApplyPatch, ActionCreateBranch, ActionCommit, ActionPush, ActionCreatePR, ActionRefreshRepairBranch, ActionCloseRepairPR, ActionDeleteRepairBranch, ActionDeleteBaselineBranch, ActionCreateBaselineReview, ActionApplyBaselineReview, ActionMergePR}
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

func TestRefreshExistingRepairBranchDoesNotSpendOrObeyModelAttemptBudget(t *testing.T) {
	policy := Policy{ACMMLevel: 6, Mode: ModeAutoMerge, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 2}
	decision := policy.Authorize(ActionRequest{
		Action: ActionRefreshRepairBranch, Agent: "quality", Repository: "owner/repo",
		RepairAttempts: 99, ChangedFiles: []string{"tests/example.test.ts"},
	})
	if !decision.Allowed {
		t.Fatalf("state-bound branch refresh was incorrectly treated as another model attempt: %+v", decision)
	}
}

func TestBaselineReviewActionsRequireRepairAuthority(t *testing.T) {
	for _, action := range []Action{ActionCreateBaselineReview, ActionApplyBaselineReview} {
		allowed := (Policy{ACMMLevel: 5, Mode: ModeRepairPR, AllowedRepositories: []string{"owner/repo"}}).Authorize(ActionRequest{
			Action: action, Agent: "quality", Repository: "owner/repo", RepairAttempts: 1,
		})
		if !allowed.Allowed {
			t.Fatalf("L5 repair policy denied %s: %v", action, allowed.Reasons)
		}
		denied := (Policy{ACMMLevel: 2, Mode: ModeAutoMerge, AllowedRepositories: []string{"owner/repo"}}).Authorize(ActionRequest{
			Action: action, Agent: "quality", Repository: "owner/repo", RepairAttempts: 1,
		})
		if denied.Allowed {
			t.Fatalf("L2 unexpectedly allowed %s", action)
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
		ExpectedBaseBranch: "main", TestedBaseBranch: "main", RequiredCheckNames: []string{"visual-hive", "unit"},
		RequiredCheckStates: []string{"success", "success"}, BranchProtectionEnabled: true,
	}
	if decision := policy.Authorize(valid); !decision.Allowed {
		t.Fatalf("valid merge was denied: %v", decision.Reasons)
	}
	finalAttempt := valid
	finalAttempt.RepairAttempts = policy.MaxRepairAttempts
	if decision := policy.Authorize(finalAttempt); !decision.Allowed {
		t.Fatalf("the final configured repair attempt must remain merge-eligible: %v", decision.Reasons)
	}
	exhaustedExistingPR := valid
	exhaustedExistingPR.RepairAttempts = policy.MaxRepairAttempts + 7
	if decision := policy.Authorize(exhaustedExistingPR); !decision.Allowed {
		t.Fatalf("repair budget stranded an already-created exact-head green PR: %v", decision.Reasons)
	}

	cases := map[string]func(*ActionRequest){
		"stale SHA":       func(r *ActionRequest) { r.TestedHeadSHA = "old" },
		"retargeted base": func(r *ActionRequest) { r.TestedBaseBranch = "release" },
		"visual optional": func(r *ActionRequest) { r.RequiredCheckNames = []string{"unit"} },
		"merge conflict":  func(r *ActionRequest) { r.Mergeable = false },
		"red visual":      func(r *ActionRequest) { r.VisualHiveVerdictGreen = false },
		"pending check":   func(r *ActionRequest) { r.RequiredCheckStates = []string{"success", "pending"} },
		"no protection":   func(r *ActionRequest) { r.BranchProtectionEnabled = false },
		"hold":            func(r *ActionRequest) { r.Hold = true },
		"baseline":        func(r *ActionRequest) { r.BaselineChanged = true },
		"workflow":        func(r *ActionRequest) { r.WorkflowChanged = true },
		"unsafe path":     func(r *ActionRequest) { r.ChangedFiles = []string{"src/auth/session.ts"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := valid
			request.ChangedFiles = append([]string(nil), valid.ChangedFiles...)
			request.RequiredCheckStates = append([]string(nil), valid.RequiredCheckStates...)
			request.RequiredCheckNames = append([]string(nil), valid.RequiredCheckNames...)
			mutate(&request)
			if decision := policy.Authorize(request); decision.Allowed {
				t.Fatalf("unsafe merge was allowed: %+v", request)
			}
		})
	}
}

func TestRepairAttemptBudgetStillBlocksNewRepairWrites(t *testing.T) {
	policy := Policy{ACMMLevel: 6, Mode: ModeAutoMerge, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3}
	for _, action := range []Action{ActionRepairModel, ActionApplyPatch, ActionCreateBranch, ActionCommit, ActionPush, ActionCreatePR, ActionCreateBaselineReview} {
		decision := policy.Authorize(ActionRequest{Action: action, Agent: "quality", Repository: "owner/repo", RepairAttempts: 4})
		if decision.Allowed {
			t.Fatalf("repair attempt budget did not block new %s work", action)
		}
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
