package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/kubestellar/hive/pkg/automation"
)

// PrepareHostedRuntime rebinds portable durable policy to this ephemeral
// runner. A signed bundle never grants authority to absolute paths retained
// from the bootstrap computer or a previous runner.
func PrepareHostedRuntime(ctx context.Context, stateDir, checkoutDir, visualCommand string, visualArgs []string, providerCommand string, providerArgs []string, requireProvider bool, current HostedReleaseIdentity, previous *HostedReleaseIdentity) (Config, error) {
	_ = ctx
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve hosted runtime state: %w", err)
	}
	checkoutDir, err = filepath.Abs(checkoutDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve hosted runtime checkout: %w", err)
	}
	for label, candidate := range map[string]string{"hosted state": stateDir, "hosted checkout": checkoutDir} {
		info, statErr := os.Lstat(candidate)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return Config{}, fmt.Errorf("%s must be an existing ordinary directory", label)
		}
	}
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return Config{}, err
	}
	config, err := store.Load()
	if err != nil {
		return Config{}, fmt.Errorf("load restored hosted configuration: %w", err)
	}
	if config.ExecutionMode != ExecutionHosted {
		return Config{}, fmt.Errorf("portable state is not owned by hosted execution")
	}
	config, err = reconcileHostedManagedPolicy(checkoutDir, config, current, previous)
	if err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(visualCommand) == "" || len(visualArgs) == 0 {
		return Config{}, fmt.Errorf("hosted integrated Visual Hive runtime is incomplete")
	}
	if requireProvider && (config.Automation == AutomationRepairPR || config.Automation == AutomationAutoMerge) && strings.TrimSpace(providerCommand) == "" {
		return Config{}, fmt.Errorf("hosted repair authority requires the immutable Codex provider runtime")
	}
	config.StateDir, config.CheckoutDir = filepath.Clean(stateDir), filepath.Clean(checkoutDir)
	config.VisualHiveCommand, config.VisualHiveArgs = visualCommand, append([]string(nil), visualArgs...)
	if strings.TrimSpace(providerCommand) != "" {
		config.ProviderCommand, config.ProviderArgs = providerCommand, append([]string(nil), providerArgs...)
	} else if !requireProvider {
		// Control/status jobs never execute the repair provider. Do not retain an
		// absolute path from the bootstrap computer or a previous ephemeral runner.
		config.ProviderCommand, config.ProviderArgs = "", nil
	}
	if err := ensureStateOwnershipMarker(stateDir, config); err != nil {
		return Config{}, err
	}
	if err := store.Save(config); err != nil {
		return Config{}, fmt.Errorf("persist ephemeral hosted runtime bindings: %w", err)
	}
	return config, nil
}

func reconcileHostedManagedPolicy(checkoutDir string, durable Config, current HostedReleaseIdentity, previous *HostedReleaseIdentity) (Config, error) {
	if !validHostedReleaseIdentity(current) || previous != nil && (!validHostedReleaseIdentity(*previous) || equalHostedReleaseIdentity(current, *previous)) {
		return Config{}, fmt.Errorf("hosted runtime release transition identity is invalid")
	}
	durableRelease := hostedReleaseIdentity(durable)
	if !equalHostedReleaseIdentity(durableRelease, current) && (previous == nil || !equalHostedReleaseIdentity(durableRelease, *previous)) {
		return Config{}, fmt.Errorf("signed hosted state is not bound to the current or exact predecessor release")
	}
	installedPath := filepath.Join(checkoutDir, ".hive", "integrated.json")
	data, err := readBoundedHostedCheckoutFile(installedPath, 1<<20)
	if err != nil {
		return Config{}, fmt.Errorf("read exact managed hosted policy: %w", err)
	}
	var installed installedRepositoryConfig
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&installed); err != nil {
		return Config{}, fmt.Errorf("parse exact managed hosted policy: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, fmt.Errorf("managed hosted policy has trailing content")
	}
	if installed.SchemaVersion != managedRepositoryConfigSchema || installed.Repository != durable.Repository || installed.RepositoryID != durable.RepositoryID ||
		installed.DefaultBranch != durable.DefaultBranch || installed.ExecutionMode != ExecutionHosted || installed.HostedControllerProtocol != HostedControllerProtocol ||
		!hostedDigestPattern.MatchString(installed.HostedWorkflowSHA256) {
		return Config{}, fmt.Errorf("managed hosted policy does not match the signed repository identity")
	}
	installedRelease := HostedReleaseIdentity{Version: installed.HiveReleaseVersion, HiveCommit: installed.HiveCommit, VisualHiveCommit: installed.VisualHiveRef, DistributionManifestSHA256: installed.DistributionManifestSHA256, HostedControllerProtocol: installed.HostedControllerProtocol}
	if !equalHostedReleaseIdentity(installedRelease, current) || !reflect.DeepEqual(installed.PreviousHostedRelease, previous) {
		return Config{}, fmt.Errorf("managed hosted policy does not match the current and predecessor release identities")
	}

	candidate := durable
	candidate.Coverage, candidate.Automation, candidate.Provider = installed.Coverage, installed.Automation, installed.Provider
	candidate.ExecutionMode, candidate.RunIntervalSeconds = installed.ExecutionMode, installed.RunIntervalSeconds
	candidate.HostedSchedule, candidate.HostedStateBranch = installed.HostedSchedule, installed.HostedStateBranch
	candidate.HiveReleaseRepository, candidate.HiveReleaseVersion = installed.HiveReleaseRepository, installed.HiveReleaseVersion
	candidate.HiveCommit, candidate.DistributionManifestSHA256 = installed.HiveCommit, installed.DistributionManifestSHA256
	candidate.HostedControllerProtocol, candidate.HostedWorkflowSHA256 = installed.HostedControllerProtocol, installed.HostedWorkflowSHA256
	candidate.PreviousHostedRelease = cloneHostedReleaseIdentity(installed.PreviousHostedRelease)
	candidate.ACMMLevel, candidate.MaxActiveIssues, candidate.MaxRepairAttempts = installed.ACMMLevel, installed.MaxActiveIssues, installed.MaxRepairAttempts
	candidate.VisualHive, candidate.VisualHiveRepo, candidate.VisualHiveRef = installed.VisualHive, installed.VisualHiveRepo, installed.VisualHiveRef
	candidate.VisualHiveConfigDigest = installed.VisualHiveConfigDigest
	candidate.TestCommands = cloneCommands(installed.TestCommands)
	candidate.AllowedRepairPaths = append([]string(nil), installed.AllowedRepairPaths...)
	candidate.AllowedAutoMergePaths = append([]string(nil), installed.AllowedAutoMergePaths...)
	candidate.AllowedAutoMergeRisk = append([]automation.RiskTier(nil), installed.AllowedAutoMergeRisk...)
	candidate.SetupAuthorizationActorID = installed.SetupAuthorizationActorID
	candidate.SetupAuthorizationActorLogin = installed.SetupAuthorizationActorLogin
	candidate.SetupAuthorizationWriterID = installed.SetupAuthorizationWriterID
	candidate.SetupAuthorizationWriterLogin = installed.SetupAuthorizationWriterLogin
	candidate.SetupAuthorizationWriterType = installed.SetupAuthorizationWriterType
	candidate.SetupAuthorizationAppID = installed.SetupAuthorizationAppID
	candidate.SetupAuthorizationInstallationID = installed.SetupAuthorizationInstallationID
	candidate.SetupAuthorizationPermissionDigest = installed.SetupAuthorizationPermissionDigest
	candidate.SetupAuthorizationAppBindingDigest = installed.SetupAuthorizationAppBindingDigest
	candidate.SetupAuthorizationPreviousActorID = installed.SetupAuthorizationPreviousActorID

	wantWorkflow, err := GenerateHostedControllerWorkflow(hostedWorkflowConfig(candidate))
	if err != nil {
		return Config{}, err
	}
	actualWorkflow, err := readBoundedHostedCheckoutFile(filepath.Join(checkoutDir, filepath.FromSlash(HostedControllerWorkflowPath)), 2<<20)
	if err != nil {
		return Config{}, fmt.Errorf("read exact managed hosted controller: %w", err)
	}
	if string(actualWorkflow) != wantWorkflow {
		return Config{}, fmt.Errorf("managed hosted controller does not exactly match the release-bound policy")
	}
	digest := sha256.Sum256(actualWorkflow)
	if hex.EncodeToString(digest[:]) != candidate.HostedWorkflowSHA256 {
		return Config{}, fmt.Errorf("managed hosted controller does not match its durable content digest")
	}
	return candidate, nil
}

func validHostedReleaseIdentity(identity HostedReleaseIdentity) bool {
	return hostedReleaseTagPattern.MatchString(identity.Version) && hostedCommitPattern.MatchString(strings.ToLower(identity.HiveCommit)) &&
		hostedCommitPattern.MatchString(strings.ToLower(identity.VisualHiveCommit)) && hostedDigestPattern.MatchString(strings.ToLower(identity.DistributionManifestSHA256)) &&
		identity.HostedControllerProtocol == HostedControllerProtocol
}

func readBoundedHostedCheckoutFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > limit {
		return nil, fmt.Errorf("%s is not a bounded ordinary file", filepath.Base(path))
	}
	return os.ReadFile(path)
}
