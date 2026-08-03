package integrated

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
)

func TestExistingHostedSetupCannotReplaceActiveReleaseIdentity(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	active := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
		ExecutionMode: ExecutionHosted, HiveReleaseRepository: "owner/hive", HiveReleaseVersion: "v0.4.1-integrated.19",
		HiveCommit: strings.Repeat("a", 40), VisualHiveRef: strings.Repeat("b", 40),
		DistributionManifestSHA256: strings.Repeat("c", 64), HostedControllerProtocol: HostedControllerProtocol,
	}
	if err := store.Save(active); err != nil {
		t.Fatal(err)
	}
	_, err = RunSetup(context.Background(), SetupOptions{
		Repository: "owner/repo", Coverage: CoverageStandard, Automation: AutomationAdvisory,
		Provider: "codex", StateDir: stateDir, MaxActiveIssues: 5, MaxRepairAttempts: 4,
		ExecutionMode: ExecutionHosted, RunInterval: 15 * time.Minute, HostedSchedule: "*/15 * * * *",
		HiveReleaseRepository: "owner/hive", HiveReleaseVersion: "v0.4.1-integrated.20",
		HiveCommit: strings.Repeat("d", 40), DistributionManifestSHA256: strings.Repeat("e", 64), HostedControllerProtocol: HostedControllerProtocol,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("f", 40),
	})
	if err == nil || !strings.Contains(err.Error(), "use hive upgrade or hive rollback") {
		t.Fatalf("hosted setup release replacement was not rejected: %v", err)
	}
	persisted, loadErr := store.Load()
	if loadErr != nil || persisted.HiveReleaseVersion != active.HiveReleaseVersion || persisted.HiveCommit != active.HiveCommit || persisted.VisualHiveRef != active.VisualHiveRef {
		t.Fatalf("rejected setup changed active release: %+v, err=%v", persisted, loadErr)
	}
}

func TestExistingHostedSetupCannotBypassTransitionThroughLocalRuntime(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	active := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
		ExecutionMode: ExecutionHosted, HiveReleaseRepository: "owner/hive", HiveReleaseVersion: "v0.4.1-integrated.19",
		HiveCommit: strings.Repeat("a", 40), VisualHiveRef: strings.Repeat("b", 40),
		DistributionManifestSHA256: strings.Repeat("c", 64), HostedControllerProtocol: HostedControllerProtocol,
	}
	if err := store.Save(active); err != nil {
		t.Fatal(err)
	}
	_, err = RunSetup(context.Background(), SetupOptions{
		Repository: "owner/repo", Coverage: CoverageStandard, Automation: AutomationAdvisory,
		Provider: "codex", StateDir: stateDir, MaxActiveIssues: 5, MaxRepairAttempts: 4,
		ExecutionMode: ExecutionLocal, RunInterval: 15 * time.Minute,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: active.VisualHiveRef,
	})
	if err == nil || !strings.Contains(err.Error(), "explicit hosted-to-local migration") {
		t.Fatalf("hosted-to-local setup bypass was not rejected: %v", err)
	}
}

func TestSetupAuthorizationIdentityStaysStableWhileOperationBranchChanges(t *testing.T) {
	config := Config{SetupBranch: "hive/upgrade-123", SetupAuthorizationActorID: 456}
	if updateSetupAuthorizationBranchForManagedChange(&config, "hive/setup-123", "") || config.SetupBranch != "hive/upgrade-123" || config.SetupAuthorizationActorID != 456 {
		t.Fatalf("identical cross-user setup rerun rotated authorization: %+v", config)
	}
	if !updateSetupAuthorizationBranchForManagedChange(&config, "hive/setup-123", "visual-hive.config.yaml\n") || config.SetupBranch != "hive/setup-123" || config.SetupAuthorizationActorID != 456 {
		t.Fatalf("substantive setup after upgrade did not rerender the operation branch while preserving the base-workflow authorizer: %+v", config)
	}
}

func TestEffectiveSetupAutoMergePolicyIsVisiblePreservedAndExplicit(t *testing.T) {
	prior := Config{AllowedAutoMergePaths: []string{"tests/**"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskLow}}
	preserved := effectiveSetupAutoMergePolicy(SetupOptions{}, prior, true)
	if strings.Join(preserved.AllowedAutoMergePaths, ",") != "tests/**" || len(preserved.AllowedAutoMergeRisk) != 1 || preserved.AllowedAutoMergeRisk[0] != automation.RiskLow {
		t.Fatalf("existing auto-merge policy was not preserved: %+v", preserved)
	}
	overridden := effectiveSetupAutoMergePolicy(SetupOptions{
		AllowedAutoMergePaths: []string{"**/*.test.*", "**/*.test.*"}, AutoMergePathsExplicit: true,
	}, prior, true)
	if len(overridden.AllowedAutoMergePaths) != 1 || overridden.AllowedAutoMergePaths[0] != "**/*.test.*" || overridden.AllowedAutoMergeRisk[0] != automation.RiskLow {
		t.Fatalf("explicit path policy did not replace only paths: %+v", overridden)
	}
	fresh := effectiveSetupAutoMergePolicy(SetupOptions{}, Config{}, false)
	if len(fresh.AllowedAutoMergePaths) == 0 || len(fresh.AllowedAutoMergeRisk) != 1 || fresh.AllowedAutoMergeRisk[0] != automation.RiskAutomatic {
		t.Fatalf("fresh setup did not expose conservative test-only/automatic defaults: %+v", fresh)
	}
}

func TestSetupRejectsInvalidExplicitAutoMergePolicy(t *testing.T) {
	base := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageStandard, Automation: AutomationAutoMerge,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 5, MaxRepairAttempts: 4,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40),
	}
	invalidPath := base
	invalidPath.AutoMergePathsExplicit = true
	invalidPath.AllowedAutoMergePaths = []string{"../workflow.yml"}
	if err := validateSetupOptions(invalidPath); err == nil || !strings.Contains(err.Error(), "repository-relative") {
		t.Fatalf("unsafe auto-merge path was accepted: %v", err)
	}
	invalidRisk := base
	invalidRisk.AutoMergeRiskExplicit = true
	invalidRisk.AllowedAutoMergeRisk = []automation.RiskTier{99}
	if err := validateSetupOptions(invalidRisk); err == nil || !strings.Contains(err.Error(), "risk tier") {
		t.Fatalf("unknown auto-merge risk was accepted: %v", err)
	}
}

func TestReadOnlySetupPlanDoesNotRequireProviderExecutable(t *testing.T) {
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageStandard, Automation: AutomationAdvisory,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 1, MaxRepairAttempts: 1,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40),
	}
	if err := validateSetupOptions(options); err != nil {
		t.Fatalf("read-only setup plan rejected without provider executable: %v", err)
	}
	options.Apply = true
	if err := validateSetupOptions(options); err == nil {
		t.Fatal("applied setup accepted without provider executable")
	}
}

func TestIntegratedSetupRejectsVisualHiveDisable(t *testing.T) {
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageStandard, Automation: AutomationAdvisory,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 1, MaxRepairAttempts: 1,
	}
	if err := validateSetupOptions(options); err == nil {
		t.Fatal("integrated setup accepted a configuration without Visual Hive")
	}
}

func TestUsesNormalHiveRuntimeMatchesExecutionAuthority(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		want   bool
	}{
		{name: "local repair pr", config: Config{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationRepairPR}, want: true},
		{name: "local auto merge", config: Config{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationAutoMerge}, want: true},
		{name: "local issues", config: Config{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationIssues}},
		{name: "local advisory", config: Config{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationAdvisory}},
		{name: "local without visual hive", config: Config{ExecutionMode: ExecutionLocal, Automation: AutomationRepairPR}},
		{name: "hosted repair pr", config: Config{ExecutionMode: ExecutionHosted, VisualHive: true, Automation: AutomationRepairPR}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := UsesNormalHiveRuntime(test.config); got != test.want {
				t.Fatalf("UsesNormalHiveRuntime(%+v) = %t, want %t", test.config, got, test.want)
			}
		})
	}
}

func TestNormalHiveProviderModelIsDefaultedAndValidatedBeforeSetup(t *testing.T) {
	for _, test := range []struct {
		name      string
		options   SetupOptions
		wantArgs  []string
		wantError bool
	}{
		{name: "local repair default", options: SetupOptions{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationRepairPR, Provider: "codex"}, wantArgs: []string{"--model=gpt-5.6-sol"}},
		{name: "local auto merge default", options: SetupOptions{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationAutoMerge, Provider: "CODEX"}, wantArgs: []string{"--model=gpt-5.6-sol"}},
		{name: "explicit model preserved", options: SetupOptions{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationRepairPR, Provider: "codex", ProviderArgs: []string{"--color", "never", "--model", "custom-model"}}, wantArgs: []string{"--color", "never", "--model", "custom-model"}},
		{name: "hosted unchanged", options: SetupOptions{ExecutionMode: ExecutionHosted, VisualHive: true, Automation: AutomationRepairPR, Provider: "codex"}},
		{name: "issues unchanged", options: SetupOptions{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationIssues, Provider: "codex"}},
		{name: "non Codex normal owner", options: SetupOptions{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationRepairPR, Provider: "other"}, wantError: true},
		{name: "missing model value", options: SetupOptions{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationRepairPR, Provider: "codex", ProviderArgs: []string{"--model"}}, wantError: true},
		{name: "duplicate model", options: SetupOptions{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationRepairPR, Provider: "codex", ProviderArgs: []string{"--model=one", "--model=two"}}, wantError: true},
		{name: "valid explicit options gain default", options: SetupOptions{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationRepairPR, Provider: "codex", ProviderArgs: []string{"--disable", "feature"}}, wantArgs: []string{"--disable", "feature", "--model=gpt-5.6-sol"}},
		{name: "unreviewed option", options: SetupOptions{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationRepairPR, Provider: "codex", ProviderArgs: []string{"--model=one", "--bogus"}}, wantError: true},
		{name: "positional payload", options: SetupOptions{ExecutionMode: ExecutionLocal, VisualHive: true, Automation: AutomationRepairPR, Provider: "codex", ProviderArgs: []string{"--model=one", "exec"}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeNormalHiveProvider(test.options)
			if (err != nil) != test.wantError {
				t.Fatalf("normalize error=%v wantError=%t", err, test.wantError)
			}
			if !test.wantError && strings.Join(got.ProviderArgs, "\x00") != strings.Join(test.wantArgs, "\x00") {
				t.Fatalf("provider args=%v, want %v", got.ProviderArgs, test.wantArgs)
			}
		})
	}
}

func TestManagedQuickstartAndRequiredActionsDistinguishRuntimeOwner(t *testing.T) {
	const stateDir = "/var/lib/hive/console-fork"
	tests := []struct {
		name          string
		mode          ExecutionMode
		automation    Automation
		normalOwner   bool
		actionSnippet string
	}{
		{name: "normal repair pr", mode: ExecutionLocal, automation: AutomationRepairPR, normalOwner: true, actionSnippet: "exact HIVE_STATE_DIR"},
		{name: "normal auto merge", mode: ExecutionLocal, automation: AutomationAutoMerge, normalOwner: true, actionSnippet: "exact HIVE_STATE_DIR"},
		{name: "legacy local issues", mode: ExecutionLocal, automation: AutomationIssues, actionSnippet: "legacy local scheduler"},
		{name: "hosted repair pr", mode: ExecutionHosted, automation: AutomationRepairPR, actionSnippet: "hosted controller"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := Config{
				Repository: "owner/console-fork", DefaultBranch: "main", StateDir: stateDir,
				ExecutionMode: test.mode, VisualHive: true, Automation: test.automation,
				Coverage: CoverageComprehensive, MaxActiveIssues: 1, MaxRepairAttempts: 2,
			}
			guide := quickstart(config, RepositoryInspection{Languages: []string{"TypeScript"}})
			plan := buildSetupPlan(SetupOptions{
				Repository: config.Repository, StateDir: stateDir, ExecutionMode: test.mode,
				VisualHive: true, Automation: test.automation, Coverage: config.Coverage,
				MaxActiveIssues: config.MaxActiveIssues, MaxRepairAttempts: config.MaxRepairAttempts,
			}, RepositoryInspection{DefaultBranch: "main"})
			actions := strings.Join(plan.RequiredActions, "\n")
			if !strings.Contains(actions, test.actionSnippet) {
				t.Fatalf("required actions did not describe %q owner: %s", test.name, actions)
			}
			if test.normalOwner {
				for _, required := range []string{"HIVE_STATE_DIR=" + stateDir, "normal project scope includes `owner/console-fork`", "dashboard listener is HTTP-ready", "automatically reconciles and activates the installed contract without a restart", "Use `hive stop` only when `hive status` or `hive doctor` directs cleanup"} {
					if !strings.Contains(guide, required) {
						t.Fatalf("normal-owner guide omitted %q: %s", required, guide)
					}
				}
				if !strings.Contains(actions, "automatically reconciles and activates the installed contract without a restart") {
					t.Fatalf("normal-owner required actions still advertise a restart: %s", actions)
				}
				for _, forbidden := range []string{"\nhive run --json\n", "\nhive start --json\n", "\nhive stop --json\n"} {
					if strings.Contains(guide, forbidden) {
						t.Fatalf("normal-owner guide advertised legacy command %q: %s", forbidden, guide)
					}
				}
				return
			}
			if !strings.Contains(guide, "\nhive run --json\n") || !strings.Contains(guide, "\nhive start --json\n") || !strings.Contains(guide, "\nhive stop --json\n") {
				t.Fatalf("%s guide lost its configured controller commands: %s", test.name, guide)
			}
			if strings.Contains(guide, "Use `hive stop` only when `hive status` or `hive doctor` directs cleanup") {
				t.Fatalf("%s guide incorrectly claimed ordinary-Hive ownership: %s", test.name, guide)
			}
		})
	}
}

func TestAutoMergeSetupPlanExplainsPostMergeActivation(t *testing.T) {
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageComprehensive, Automation: AutomationAutoMerge,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true, Start: true,
	}
	plan := buildSetupPlan(options, RepositoryInspection{DefaultBranch: "main"})
	joined := strings.Join(append(append([]string{}, plan.RequiredActions...), plan.Warnings...), "\n")
	for _, expected := range []string{"merge the exact setup PR", "trusted", "activate exact-App-bound protection"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("two-phase setup plan does not explain %q: %s", expected, joined)
		}
	}
	message := setupActivationMessage(AutomationAutoMerge, true, true)
	if !strings.Contains(message, "durably recorded") || !strings.Contains(message, "non-scheduler doctor checks are green") || !strings.Contains(message, "before any lifecycle write") {
		t.Fatalf("started setup result does not explain automatic activation: %q", message)
	}
}

func TestSetupPlanDoesNotWarnWhenReviewedBaselinesExist(t *testing.T) {
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageComprehensive, Automation: AutomationAdvisory,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
	}
	plan := buildSetupPlan(options, RepositoryInspection{
		DefaultBranch: "main",
		BaselineFiles: []string{".visual-hive/snapshots/dashboard.png"},
	})
	for _, warning := range plan.Warnings {
		if strings.Contains(warning, "No reviewed visual baselines") {
			t.Fatalf("setup plan reported a missing baseline despite reviewed snapshots: %v", plan.Warnings)
		}
	}
}

func TestSetupPlanDisclosesExactVisualHiveDependencyUsedByApply(t *testing.T) {
	ref := strings.Repeat("b", 40)
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageComprehensive, Automation: AutomationRepairPR,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 5, MaxRepairAttempts: 4,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: ref,
	}
	if err := validateSetupOptions(options); err != nil {
		t.Fatalf("resolved read-only setup dependency was rejected: %v", err)
	}
	plan := buildSetupPlan(options, RepositoryInspection{DefaultBranch: "main"})
	if plan.StateDir != options.StateDir || plan.VisualHiveRepository != options.VisualHiveRepo || plan.VisualHiveRef != ref {
		t.Fatalf("plan dependency = %s@%s, want %s@%s", plan.VisualHiveRepository, plan.VisualHiveRef, options.VisualHiveRepo, ref)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, disclosed := range []string{`"state_dir":"` + strings.ReplaceAll(options.StateDir, `\`, `\\`) + `"`, `"visual_hive_repository":"owner/visual-hive"`, `"visual_hive_ref":"` + ref + `"`} {
		if !strings.Contains(string(data), disclosed) {
			t.Fatalf("setup plan JSON omitted %s: %s", disclosed, data)
		}
	}
	body := setupPRBody("<!-- hive-setup: owner/repo -->", plan)
	if !strings.Contains(body, "`"+options.VisualHiveRepo+"@"+ref+"`") {
		t.Fatalf("setup review body did not disclose the exact dependency: %s", body)
	}
}
