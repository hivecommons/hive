package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/integrated"
)

func TestExistingSetupInheritsEveryOmittedPolicyAndRuntimePin(t *testing.T) {
	for _, name := range []string{"HIVE_CODEX_COMMAND", "VISUAL_HIVE_REPOSITORY", "VISUAL_HIVE_REF", "HIVE_VISUAL_HIVE_HOME", "VISUAL_HIVE_CLI"} {
		t.Setenv(name, "")
	}
	prior := integrated.Config{
		Repository: "owner/repo", Coverage: integrated.CoverageComprehensive, Automation: integrated.AutomationAutoMerge,
		Provider: "codex-custom", ProviderCommand: "old-codex", ProviderArgs: []string{"exec"}, VisualHive: false,
		VisualHiveRepo: "owner/visual", VisualHiveRef: strings.Repeat("a", 40), VisualHiveCommand: "old-node", VisualHiveArgs: []string{"old-cli"},
		MaxActiveIssues: 9, MaxRepairAttempts: 4,
	}
	repository, coverage, authority, provider, providerCommand := "", "", "", "codex", ""
	visualCommand, visualRepo, visualRef, visualHive := "", "default/visual", "", true
	maxIssues, maxAttempts := 5, 3
	providerArgs, visualArgs := stringListFlag{}, stringListFlag{}
	if err := inheritExistingSetup(prior, map[string]bool{}, &repository, &coverage, &authority, &provider, &providerCommand, &visualCommand, &visualRepo, &visualRef, &visualHive, &maxIssues, &maxAttempts, &providerArgs, &visualArgs); err != nil {
		t.Fatal(err)
	}
	if repository != prior.Repository || coverage != string(prior.Coverage) || authority != string(prior.Automation) || provider != prior.Provider || providerCommand != prior.ProviderCommand || visualHive != prior.VisualHive || visualRepo != prior.VisualHiveRepo || visualRef != prior.VisualHiveRef || visualCommand != prior.VisualHiveCommand || maxIssues != 9 || maxAttempts != 4 || !reflect.DeepEqual([]string(providerArgs), prior.ProviderArgs) || !reflect.DeepEqual([]string(visualArgs), prior.VisualHiveArgs) {
		t.Fatalf("existing setup defaults were reset: repo=%s coverage=%s authority=%s provider=%s visual=%t ref=%s limits=%d/%d", repository, coverage, authority, provider, visualHive, visualRef, maxIssues, maxAttempts)
	}
}

func TestExistingSetupAllowsLocalRuntimeRebindWithoutChangingImmutablePolicy(t *testing.T) {
	for _, name := range []string{"HIVE_CODEX_COMMAND", "VISUAL_HIVE_REPOSITORY", "VISUAL_HIVE_REF", "HIVE_VISUAL_HIVE_HOME", "VISUAL_HIVE_CLI"} {
		t.Setenv(name, "")
	}
	prior := integrated.Config{Repository: "owner/repo", Coverage: integrated.CoverageStandard, Automation: integrated.AutomationIssues, Provider: "codex", VisualHive: true, VisualHiveRepo: "owner/visual", VisualHiveRef: strings.Repeat("b", 40), VisualHiveCommand: "old-node", VisualHiveArgs: []string{"old-cli"}, MaxActiveIssues: 5, MaxRepairAttempts: 4}
	repository, coverage, authority, provider, providerCommand := "owner/repo", "", "", "codex", ""
	visualCommand, visualRepo, visualRef, visualHive := "new-node", "", "", true
	maxIssues, maxAttempts := 5, 4
	providerArgs, visualArgs := stringListFlag{}, stringListFlag{"new-cli"}
	explicit := map[string]bool{"repo": true, "visual-hive-home": true}
	if err := inheritExistingSetup(prior, explicit, &repository, &coverage, &authority, &provider, &providerCommand, &visualCommand, &visualRepo, &visualRef, &visualHive, &maxIssues, &maxAttempts, &providerArgs, &visualArgs); err != nil {
		t.Fatal(err)
	}
	if visualCommand != "new-node" || !reflect.DeepEqual([]string(visualArgs), []string{"new-cli"}) || visualRef != prior.VisualHiveRef || coverage != string(prior.Coverage) || authority != string(prior.Automation) {
		t.Fatalf("runtime-only rebind changed repository policy or immutable pin: command=%s args=%v ref=%s coverage=%s authority=%s", visualCommand, visualArgs, visualRef, coverage, authority)
	}
	other := "owner/other"
	if err := inheritExistingSetup(prior, map[string]bool{"repo": true}, &other, &coverage, &authority, &provider, &providerCommand, &visualCommand, &visualRepo, &visualRef, &visualHive, &maxIssues, &maxAttempts, &providerArgs, &visualArgs); err == nil {
		t.Fatal("existing state directory was rebound to another repository")
	}
}
