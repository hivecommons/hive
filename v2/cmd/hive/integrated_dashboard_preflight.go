package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/dashboard"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
)

const dashboardPreflightValidity = 15 * time.Minute

type dashboardPreflightReceipt struct {
	SchemaVersion          string    `json:"schema_version"`
	Repository             string    `json:"repository"`
	RepositoryID           string    `json:"repository_id"`
	BindingSHA256          string    `json:"binding_sha256"`
	StateRoot              string    `json:"state_root"`
	HiveCommit             string    `json:"hive_commit"`
	HiveExecutableSHA256   string    `json:"hive_executable_sha256"`
	ImageDigest            string    `json:"image_digest,omitempty"`
	VisualHiveRef          string    `json:"visual_hive_ref"`
	VisualCommand          string    `json:"visual_command"`
	VisualArgs             []string  `json:"visual_args"`
	VisualCommandSHA256    string    `json:"visual_command_sha256"`
	VisualEntrypointSHA256 string    `json:"visual_entrypoint_sha256,omitempty"`
	Provider               string    `json:"provider"`
	ProviderBinary         string    `json:"provider_binary"`
	ProviderArgs           []string  `json:"provider_args"`
	ProviderModel          string    `json:"provider_model"`
	ProviderBinarySHA256   string    `json:"provider_binary_sha256"`
	TestedAt               time.Time `json:"tested_at"`
	ExpiresAt              time.Time `json:"expires_at"`
}

type dashboardPreflightRuntimeIdentity struct {
	HiveCommit             string
	HiveExecutableSHA256   string
	ImageDigest            string
	ProviderBinarySHA256   string
	VisualCommandSHA256    string
	VisualEntrypointSHA256 string
}

var (
	dashboardPreflightNow                    = func() time.Time { return time.Now().UTC() }
	dashboardPreflightResolveProvider        = resolveIntegratedProvider
	dashboardPreflightResolveVisual          = resolveSetupVisualHive
	dashboardPreflightVerifyVisual           = integrated.VerifyVisualHiveCommit
	dashboardPreflightVerifyProvider         = verifyDashboardPreflightProvider
	dashboardPreflightResolveRuntimeIdentity = resolveDashboardPreflightRuntimeIdentity
	dashboardPreflightGitHubClient           = func(token string) *hivegithub.Client {
		return hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	}
)

func runDashboardIntegratedPreflight(ctx context.Context, request dashboard.IntegratedPreflightRequest, token string) (map[string]any, error) {
	if err := validateHostedIntegratedStateRoot(); err != nil {
		return nil, err
	}
	stateRoot, err := dashboardSetupStateDir(request.Repository)
	if err != nil {
		return nil, err
	}
	if err := probeIntegratedStateStorage(stateRoot); err != nil {
		return nil, err
	}
	client := dashboardPreflightGitHubClient(token)
	owner, repository, ok := strings.Cut(request.Repository, "/")
	if !ok || owner == "" || repository == "" {
		return nil, fmt.Errorf("integrated preflight repository identity is invalid")
	}
	metadata, _, err := client.GoGitHub().Repositories.Get(ctx, owner, repository)
	if err != nil {
		return nil, fmt.Errorf("verify owner repository read access: %w", err)
	}
	if metadata.GetID() <= 0 || !strings.EqualFold(metadata.GetFullName(), request.Repository) || strings.TrimSpace(metadata.GetDefaultBranch()) == "" {
		return nil, fmt.Errorf("GitHub repository identity does not match %s", request.Repository)
	}
	if err := ensureNoManagedRepositoryContractBeforeSetup(ctx, client, owner, repository, metadata.GetDefaultBranch()); err != nil {
		return nil, err
	}
	normalized, err := integrated.NormalizeNormalHiveProvider(integrated.SetupOptions{
		Provider: request.Provider, ExecutionMode: integrated.ExecutionLocal, VisualHive: true,
		Automation: integrated.Automation(request.Automation),
	})
	if err != nil {
		return nil, fmt.Errorf("resolve governed repair provider: %w", err)
	}
	providerArgs := append([]string(nil), normalized.ProviderArgs...)
	providerCtx, providerCancel := context.WithTimeout(ctx, 90*time.Second)
	providerCommand, err := dashboardPreflightResolveProvider(providerCtx, request.Provider, os.Getenv("HIVE_CODEX_COMMAND"), providerArgs)
	providerCancel()
	if err != nil {
		return nil, fmt.Errorf("verify unattended Codex runtime: %w", err)
	}
	modelCtx, modelCancel := context.WithTimeout(ctx, 2*time.Minute)
	providerModel, err := dashboardPreflightVerifyProvider(modelCtx, providerCommand, providerArgs)
	modelCancel()
	if err != nil {
		return nil, fmt.Errorf("verify configured Codex model: %w", err)
	}
	visualCommand, visualArgs, visualRef, err := dashboardPreflightResolveVisual(
		os.Getenv("VISUAL_HIVE_CLI"), nil, os.Getenv("HIVE_VISUAL_HIVE_HOME"), request.VisualHiveRef,
	)
	if err != nil {
		return nil, fmt.Errorf("verify immutable Visual Hive runtime: %w", err)
	}
	if err := dashboardPreflightVerifyVisual(ctx, client, defaultVisualHiveRepository, visualRef); err != nil {
		return nil, fmt.Errorf("verify Visual Hive source commit: %w", err)
	}
	if _, installed, loadErr := loadAuthoritativeVisualWorkContract(); loadErr != nil {
		return nil, fmt.Errorf("inspect existing Visual Hive controller contract: %w", loadErr)
	} else if installed {
		return nil, fmt.Errorf("Visual Hive is already installed; use status and doctor instead of preflight")
	}
	if err := ensureNoVisualRuntimeBeforeSetup(stateRoot); err != nil {
		return nil, err
	}
	runtimeIdentity, err := dashboardPreflightResolveRuntimeIdentity(providerCommand, visualCommand, visualArgs)
	if err != nil {
		return nil, fmt.Errorf("bind hosted runtime identity: %w", err)
	}
	now := dashboardPreflightNow()
	receipt := dashboardPreflightReceipt{
		SchemaVersion: "hive.dashboard-integrated-preflight-receipt.v1", Repository: request.Repository,
		RepositoryID: fmt.Sprintf("%d", metadata.GetID()), StateRoot: filepath.Clean(stateRoot), VisualHiveRef: visualRef,
		HiveCommit: runtimeIdentity.HiveCommit, HiveExecutableSHA256: runtimeIdentity.HiveExecutableSHA256, ImageDigest: runtimeIdentity.ImageDigest,
		VisualCommand: filepath.Clean(visualCommand), VisualArgs: append([]string(nil), visualArgs...),
		VisualCommandSHA256: runtimeIdentity.VisualCommandSHA256, VisualEntrypointSHA256: runtimeIdentity.VisualEntrypointSHA256,
		Provider: request.Provider, ProviderBinary: filepath.Clean(providerCommand), ProviderArgs: providerArgs, ProviderModel: providerModel,
		ProviderBinarySHA256: runtimeIdentity.ProviderBinarySHA256,
		TestedAt:             now, ExpiresAt: now.Add(dashboardPreflightValidity),
	}
	receipt.BindingSHA256, err = dashboardPreflightBinding(request, receipt)
	if err != nil {
		return nil, err
	}
	if err := saveDashboardPreflightReceipt(integratedStateRoot(), receipt); err != nil {
		return nil, err
	}
	return map[string]any{
		"schema_version": "hive.dashboard-integrated-preflight.v1", "ready": true,
		"repository": request.Repository, "repository_id": receipt.RepositoryID, "request_id": request.RequestID,
		"hosted":     config.IsKubernetesPod() && strings.TrimSpace(os.Getenv("HIVE_ID")) != "",
		"state_root": receipt.StateRoot, "storage": map[string]any{"persistent": hostedStatePathIsPersistent(receipt.StateRoot), "writable": true},
		"runtime": map[string]any{"hive_commit": receipt.HiveCommit, "hive_executable_sha256": receipt.HiveExecutableSHA256,
			"image_digest": receipt.ImageDigest, "image_digest_reported": receipt.ImageDigest != ""},
		"provider": map[string]any{"name": request.Provider, "command": filepath.Base(providerCommand), "binary_sha256": receipt.ProviderBinarySHA256,
			"model": providerModel, "health": "authenticated_model_verified", "model_calls": 1, "unattended": true},
		"visual_hive": map[string]any{"ref": visualRef, "command": filepath.Base(visualCommand), "args": append([]string(nil), visualArgs...),
			"command_sha256": receipt.VisualCommandSHA256, "entrypoint_sha256": receipt.VisualEntrypointSHA256},
		"controller_active": false, "repository_mutations": false, "binding_sha256": receipt.BindingSHA256,
		"tested_at": receipt.TestedAt, "expires_at": receipt.ExpiresAt,
	}, nil
}

func ensureNoManagedRepositoryContractBeforeSetup(ctx context.Context, client *hivegithub.Client, owner, repository, defaultBranch string) error {
	file, directory, response, err := client.GoGitHub().Repositories.GetContents(
		ctx, owner, repository, ".hive/integrated.json", &gh.RepositoryContentGetOptions{Ref: defaultBranch},
	)
	if err != nil {
		if response != nil && response.Response != nil && response.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("inspect repository contract before hosted setup: %w", err)
	}
	if file == nil || len(directory) != 0 || file.GetType() != "file" {
		return fmt.Errorf("repository managed contract is not one ordinary file")
	}
	return fmt.Errorf("repository already contains a managed Visual Hive contract without authoritative local state; inspect status and doctor, then use setup-reset recovery")
}

func ensureNoVisualRuntimeBeforeSetup(stateRoot string) error {
	if manager := dashboardNormalVisualRuntime.Load(); manager != nil {
		if binding, active := manager.ActiveBinding(); active {
			return fmt.Errorf("Visual Hive controller is already active for %s", binding.Repository)
		}
	}
	leasePath := filepath.Join(stateRoot, "integrated", "daemon.lease")
	if _, leaseErr := os.Lstat(leasePath); leaseErr == nil {
		return fmt.Errorf("Visual Hive ownership lease already exists at the selected state root")
	} else if !os.IsNotExist(leaseErr) {
		return fmt.Errorf("inspect Visual Hive ownership lease: %w", leaseErr)
	}
	return nil
}

func resolveDashboardPreflightRuntimeIdentity(providerCommand, visualCommand string, visualArgs []string) (dashboardPreflightRuntimeIdentity, error) {
	identity := dashboardPreflightRuntimeIdentity{HiveCommit: strings.ToLower(strings.TrimSpace(gitHash))}
	if !validDaemonHexIdentity(identity.HiveCommit, 40) {
		return identity, fmt.Errorf("Hive image does not expose an immutable 40-character commit")
	}
	executable, err := os.Executable()
	if err != nil {
		return identity, err
	}
	identity.HiveExecutableSHA256, err = daemonExecutableSHA256(executable)
	if err != nil {
		return identity, err
	}
	identity.ProviderBinarySHA256, err = daemonExecutableSHA256(providerCommand)
	if err != nil {
		return identity, fmt.Errorf("hash Codex executable: %w", err)
	}
	identity.VisualCommandSHA256, err = daemonExecutableSHA256(visualCommand)
	if err != nil {
		return identity, fmt.Errorf("hash Visual Hive command: %w", err)
	}
	if len(visualArgs) > 0 {
		entrypoint := filepath.Clean(visualArgs[0])
		if info, statErr := os.Lstat(entrypoint); statErr == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			identity.VisualEntrypointSHA256, err = daemonExecutableSHA256(entrypoint)
			if err != nil {
				return identity, fmt.Errorf("hash Visual Hive entrypoint: %w", err)
			}
		}
	}
	identity.ImageDigest = strings.ToLower(strings.TrimSpace(os.Getenv("HIVE_IMAGE_DIGEST")))
	if identity.ImageDigest != "" {
		parts := strings.Split(identity.ImageDigest, ":")
		if len(parts) != 2 || parts[0] != "sha256" || !validDaemonHexIdentity(parts[1], 64) {
			return identity, fmt.Errorf("HIVE_IMAGE_DIGEST must be an exact sha256 OCI manifest digest")
		}
	}
	return identity, nil
}

func verifyDashboardPreflightProvider(ctx context.Context, command string, args []string) (string, error) {
	model, err := repair.CodexSpecialistProviderModel(args)
	if err != nil || strings.TrimSpace(model) == "" {
		return "", fmt.Errorf("resolve configured Codex model: %w", err)
	}
	result, err := integratedSetupCodexProvider(command, args).Run(ctx, "", "Return exactly HIVE_HOSTED_PREFLIGHT_OK and do not use tools.")
	if err != nil {
		return "", err
	}
	if !strings.Contains(result.Summary+"\n"+result.Output, "HIVE_HOSTED_PREFLIGHT_OK") {
		return "", fmt.Errorf("configured Codex model did not return the bounded readiness token")
	}
	return model, nil
}

func probeIntegratedStateStorage(root string) error {
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create integrated state root %s: %w", root, err)
	}
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("integrated state root %s must be a real directory", root)
	}
	probe, err := os.CreateTemp(root, ".hive-storage-preflight-*")
	if err != nil {
		return fmt.Errorf("integrated state root %s is not writable: %w", root, err)
	}
	name := probe.Name()
	if _, err := probe.Write([]byte("hive-storage-preflight-v1\n")); err != nil {
		_ = probe.Close()
		_ = os.Remove(name)
		return fmt.Errorf("write integrated state root probe: %w", err)
	}
	if err := probe.Sync(); err != nil {
		_ = probe.Close()
		_ = os.Remove(name)
		return fmt.Errorf("sync integrated state root probe: %w", err)
	}
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close integrated state root probe: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove integrated state root probe: %w", err)
	}
	return nil
}

func dashboardPreflightBinding(request dashboard.IntegratedPreflightRequest, receipt dashboardPreflightReceipt) (string, error) {
	value := struct {
		Repository      string   `json:"repository"`
		RepositoryID    string   `json:"repository_id"`
		StateRoot       string   `json:"state_root"`
		Provider        string   `json:"provider"`
		ProviderBinary  string   `json:"provider_binary"`
		ProviderSHA256  string   `json:"provider_sha256"`
		ProviderArgs    []string `json:"provider_args"`
		ProviderModel   string   `json:"provider_model"`
		VisualHiveRef   string   `json:"visual_hive_ref"`
		VisualCommand   string   `json:"visual_command"`
		VisualArgs      []string `json:"visual_args"`
		VisualSHA256    string   `json:"visual_sha256"`
		EntrypointSHA   string   `json:"visual_entrypoint_sha256"`
		HiveCommit      string   `json:"hive_commit"`
		HiveSHA256      string   `json:"hive_executable_sha256"`
		ImageDigest     string   `json:"image_digest"`
		Coverage        string   `json:"coverage"`
		Automation      string   `json:"automation"`
		MaxActiveIssues int      `json:"max_active_issues"`
	}{
		Repository: request.Repository, RepositoryID: receipt.RepositoryID, StateRoot: receipt.StateRoot,
		Provider: request.Provider, ProviderBinary: receipt.ProviderBinary, ProviderSHA256: receipt.ProviderBinarySHA256,
		ProviderArgs: append([]string(nil), receipt.ProviderArgs...), ProviderModel: receipt.ProviderModel,
		VisualHiveRef: receipt.VisualHiveRef, VisualCommand: receipt.VisualCommand, VisualArgs: append([]string(nil), receipt.VisualArgs...),
		VisualSHA256: receipt.VisualCommandSHA256, EntrypointSHA: receipt.VisualEntrypointSHA256,
		HiveCommit: receipt.HiveCommit, HiveSHA256: receipt.HiveExecutableSHA256, ImageDigest: receipt.ImageDigest,
		Coverage: request.Coverage, Automation: request.Automation, MaxActiveIssues: preflightMaxActiveIssues(request),
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode integrated preflight binding: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func preflightMaxActiveIssues(request dashboard.IntegratedPreflightRequest) int {
	if request.MaxActiveIssues == nil {
		return 5
	}
	return *request.MaxActiveIssues
}

func dashboardPreflightReceiptPath(root, repository string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(repository))))
	return filepath.Join(root, "preflight", hex.EncodeToString(digest[:8])+".json")
}

func saveDashboardPreflightReceipt(root string, receipt dashboardPreflightReceipt) error {
	path := dashboardPreflightReceiptPath(root, receipt.Repository)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create integrated preflight receipt directory: %w", err)
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return fmt.Errorf("encode integrated preflight receipt: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".preflight-*.json")
	if err != nil {
		return fmt.Errorf("create integrated preflight receipt: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("publish integrated preflight receipt: %w", err)
	}
	return nil
}

func removeDashboardPreflightReceipt(repository string) error {
	path := dashboardPreflightReceiptPath(integratedStateRoot(), repository)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("retire hosted readiness preflight receipt: %w", err)
	}
	return nil
}

func loadDashboardPreflightReceipt(repository string) (dashboardPreflightReceipt, bool, error) {
	path := dashboardPreflightReceiptPath(integratedStateRoot(), repository)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return dashboardPreflightReceipt{}, false, nil
	}
	if err != nil {
		return dashboardPreflightReceipt{}, false, fmt.Errorf("read hosted readiness preflight receipt: %w", err)
	}
	var receipt dashboardPreflightReceipt
	stateDir, stateErr := dashboardSetupStateDir(repository)
	if stateErr != nil {
		return dashboardPreflightReceipt{}, false, stateErr
	}
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.SchemaVersion != "hive.dashboard-integrated-preflight-receipt.v1" ||
		receipt.Repository != repository || receipt.StateRoot != filepath.Clean(stateDir) || receipt.TestedAt.IsZero() || receipt.ExpiresAt.IsZero() ||
		!validDaemonHexIdentity(receipt.HiveCommit, 40) || !validDaemonHexIdentity(receipt.HiveExecutableSHA256, 64) ||
		!validDaemonHexIdentity(receipt.ProviderBinarySHA256, 64) || !validDaemonHexIdentity(receipt.VisualCommandSHA256, 64) {
		return dashboardPreflightReceipt{}, false, fmt.Errorf("hosted readiness preflight receipt is invalid")
	}
	return receipt, true, nil
}

func requireDashboardPreflightReceipt(ctx context.Context, request dashboard.IntegratedSetupRequest) error {
	if !(config.IsKubernetesPod() && strings.TrimSpace(os.Getenv("HIVE_ID")) != "") {
		return nil
	}
	path := dashboardPreflightReceiptPath(integratedStateRoot(), request.Repository)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("hosted setup requires a successful readiness preflight: %w", err)
	}
	var receipt dashboardPreflightReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return fmt.Errorf("decode hosted readiness preflight receipt: %w", err)
	}
	preflightRequest := dashboard.IntegratedPreflightRequest{
		Repository: request.Repository, RequestID: request.RequestID, Provider: request.Provider, VisualHiveRef: request.VisualHiveRef,
		Coverage: request.Coverage, Automation: request.Automation, MaxActiveIssues: request.MaxActiveIssues,
	}
	expected, err := dashboardPreflightBinding(preflightRequest, receipt)
	if err != nil {
		return err
	}
	now := dashboardPreflightNow()
	stateDir, stateErr := dashboardSetupStateDir(request.Repository)
	if stateErr != nil {
		return stateErr
	}
	normalized, normalizeErr := integrated.NormalizeNormalHiveProvider(integrated.SetupOptions{
		Provider: request.Provider, ExecutionMode: integrated.ExecutionLocal, VisualHive: true,
		Automation: integrated.Automation(request.Automation),
	})
	if normalizeErr != nil || !slices.Equal(normalized.ProviderArgs, receipt.ProviderArgs) {
		return fmt.Errorf("hosted readiness preflight runtime identity changed after the readiness check")
	}
	providerCtx, providerCancel := context.WithTimeout(ctx, 30*time.Second)
	providerCommand, providerErr := dashboardPreflightResolveProvider(providerCtx, request.Provider, os.Getenv("HIVE_CODEX_COMMAND"), normalized.ProviderArgs)
	providerCancel()
	if providerErr != nil || filepath.Clean(providerCommand) != receipt.ProviderBinary {
		return fmt.Errorf("hosted readiness preflight runtime identity changed after the readiness check")
	}
	visualCommand, visualArgs, visualRef, visualErr := dashboardPreflightResolveVisual(
		os.Getenv("VISUAL_HIVE_CLI"), nil, os.Getenv("HIVE_VISUAL_HIVE_HOME"), request.VisualHiveRef,
	)
	if visualErr != nil || visualRef != receipt.VisualHiveRef || filepath.Clean(visualCommand) != receipt.VisualCommand || !slices.Equal(visualArgs, receipt.VisualArgs) {
		return fmt.Errorf("hosted readiness preflight runtime identity changed after the readiness check")
	}
	currentIdentity, identityErr := dashboardPreflightResolveRuntimeIdentity(receipt.ProviderBinary, visualCommand, visualArgs)
	if identityErr != nil || currentIdentity.HiveCommit != receipt.HiveCommit || currentIdentity.HiveExecutableSHA256 != receipt.HiveExecutableSHA256 ||
		currentIdentity.ImageDigest != receipt.ImageDigest || currentIdentity.ProviderBinarySHA256 != receipt.ProviderBinarySHA256 ||
		currentIdentity.VisualCommandSHA256 != receipt.VisualCommandSHA256 || currentIdentity.VisualEntrypointSHA256 != receipt.VisualEntrypointSHA256 {
		return fmt.Errorf("hosted readiness preflight runtime identity changed after the readiness check")
	}
	if receipt.SchemaVersion != "hive.dashboard-integrated-preflight-receipt.v1" || receipt.Repository != request.Repository ||
		receipt.StateRoot != filepath.Clean(stateDir) || receipt.VisualHiveRef != request.VisualHiveRef ||
		receipt.Provider != request.Provider || receipt.BindingSHA256 != expected || receipt.TestedAt.IsZero() || !receipt.ExpiresAt.After(now) {
		return fmt.Errorf("hosted readiness preflight is missing, expired, or bound to different setup inputs")
	}
	return nil
}

func hostedStatePathIsPersistent(stateDir string) bool {
	parent := filepath.Dir(filepath.Clean(hostedIntegratedStateRoot))
	relative, err := filepath.Rel(parent, filepath.Clean(stateDir))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
