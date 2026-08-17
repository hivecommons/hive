package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/dashboard"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
)

const dashboardPreflightValidity = 15 * time.Minute

const dashboardPreflightReceiptMaxBytes = 1 << 20

type dashboardPreflightReceipt struct {
	SchemaVersion          string                          `json:"schema_version"`
	Repository             string                          `json:"repository"`
	RepositoryID           string                          `json:"repository_id"`
	BindingSHA256          string                          `json:"binding_sha256"`
	StateRoot              string                          `json:"state_root"`
	HiveCommit             string                          `json:"hive_commit"`
	HiveExecutableSHA256   string                          `json:"hive_executable_sha256"`
	ImageDigest            string                          `json:"image_digest,omitempty"`
	VisualHiveRef          string                          `json:"visual_hive_ref"`
	VisualCommand          string                          `json:"visual_command"`
	VisualArgs             []string                        `json:"visual_args"`
	VisualCommandSHA256    string                          `json:"visual_command_sha256"`
	VisualEntrypointSHA256 string                          `json:"visual_entrypoint_sha256,omitempty"`
	Provider               string                          `json:"provider"`
	ProviderBinary         string                          `json:"provider_binary"`
	ProviderArgs           []string                        `json:"provider_args"`
	ProviderModel          string                          `json:"provider_model"`
	ProviderBinarySHA256   string                          `json:"provider_binary_sha256"`
	OperatorID             int64                           `json:"operator_id"`
	OperatorLogin          string                          `json:"operator_login"`
	WriterID               int64                           `json:"writer_id"`
	WriterLogin            string                          `json:"writer_login"`
	WriterType             string                          `json:"writer_type"`
	AppID                  int64                           `json:"app_id,omitempty"`
	InstallationID         int64                           `json:"installation_id,omitempty"`
	PermissionDigest       string                          `json:"permission_digest,omitempty"`
	Permissions            map[string]string               `json:"permissions,omitempty"`
	RuntimeBindingDigest   string                          `json:"runtime_binding_digest"`
	QualityProbe           agent.QualityRuntimeProbeResult `json:"quality_probe"`
	TestedAt               time.Time                       `json:"tested_at"`
	ExpiresAt              time.Time                       `json:"expires_at"`
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
	dashboardPreflightQualityRuntimeProbe = func(context.Context) (agent.QualityRuntimeProbeResult, error) {
		return agent.QualityRuntimeProbeResult{}, fmt.Errorf("Quality runtime probe is unavailable")
	}
)

func runDashboardIntegratedPreflight(ctx context.Context, request dashboard.IntegratedPreflightRequest, credential any) (map[string]any, error) {
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
	operator, runtimeSnapshot, legacyTestCredential, err := resolveDashboardPreflightCredential(request.Repository, credential)
	if err != nil {
		return nil, err
	}
	if runtimeSnapshot.Mode == "app" {
		runtimeSnapshot, err = refreshLiveGitHubAppRuntime(ctx, runtimeSnapshot)
		if err != nil {
			return nil, err
		}
	}
	if operator.ID <= 0 || strings.TrimSpace(operator.Login) == "" || !strings.EqualFold(operator.Type, "User") {
		return nil, fmt.Errorf("hosted preflight requires an exact authenticated human operator")
	}
	resolvedOperator := operator
	if !legacyTestCredential {
		resolvedOperator, err = runtimeSnapshot.Client.ResolveHumanNumericUser(ctx, operator.Login)
		if err != nil || resolvedOperator.ID != operator.ID {
			return nil, fmt.Errorf("hosted preflight operator identity does not match GitHub: %w", err)
		}
	}
	if runtimeSnapshot.Mode == "app" {
		if err := runtimeSnapshot.App.RequireVisualHivePermissions(); err != nil {
			return nil, fmt.Errorf("hosted preflight GitHub App permissions: %w", err)
		}
	}
	client := runtimeSnapshot.Client
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
	managedUpdate, err := ensureManagedRepositoryContractBeforeSetup(
		ctx, client, stateRoot, request.Repository, metadata.GetID(), owner, repository, metadata.GetDefaultBranch(),
	)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeDashboardPreflightProvider(request.Provider, integrated.Automation(request.Automation))
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
	var qualityProbe agent.QualityRuntimeProbeResult
	if legacyTestCredential {
		qualityProbe = agent.QualityRuntimeProbeResult{Agent: "quality", UID: 2006, Home: "/data/home", CodexHome: "/data/.codex-quality", Backend: request.Provider,
			Model: providerModel, CommandSHA256: runtimeIdentityOrPlaceholder(providerCommand), ApprovalPolicy: "never", ToolCall: "read-only-local-file", OutputSHA256: strings.Repeat("9", 64)}
	} else {
		qualityCtx, qualityCancel := context.WithTimeout(ctx, 3*time.Minute)
		qualityProbe, err = dashboardPreflightQualityRuntimeProbe(qualityCtx)
		qualityCancel()
		if err != nil {
			return nil, fmt.Errorf("verify actual Quality runtime: %w", err)
		}
	}
	if !strings.EqualFold(qualityProbe.Backend, request.Provider) || qualityProbe.Model != providerModel || qualityProbe.UID <= 0 ||
		strings.TrimSpace(qualityProbe.Home) == "" || strings.TrimSpace(qualityProbe.CodexHome) == "" || qualityProbe.ApprovalPolicy != "never" ||
		qualityProbe.ToolCall != "read-only-local-file" || !validDaemonHexIdentity(qualityProbe.CommandSHA256, 64) || !validDaemonHexIdentity(qualityProbe.OutputSHA256, 64) {
		return nil, fmt.Errorf("actual Quality runtime does not match the requested provider/model/UID/HOME/unattended tool policy")
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
	} else if installed && !managedUpdate {
		return nil, fmt.Errorf("Visual Hive is already installed; use status and doctor instead of preflight")
	}
	controllerActive, err := ensureNoVisualRuntimeBeforeSetup(stateRoot, request.Repository, managedUpdate)
	if err != nil {
		return nil, err
	}
	runtimeIdentity, err := dashboardPreflightResolveRuntimeIdentity(providerCommand, visualCommand, visualArgs)
	if err != nil {
		return nil, fmt.Errorf("bind hosted runtime identity: %w", err)
	}
	now := dashboardPreflightNow()
	receipt := dashboardPreflightReceipt{
		SchemaVersion: "hive.dashboard-integrated-preflight-receipt.v2", Repository: request.Repository,
		RepositoryID: fmt.Sprintf("%d", metadata.GetID()), StateRoot: filepath.Clean(stateRoot), VisualHiveRef: visualRef,
		HiveCommit: runtimeIdentity.HiveCommit, HiveExecutableSHA256: runtimeIdentity.HiveExecutableSHA256, ImageDigest: runtimeIdentity.ImageDigest,
		VisualCommand: filepath.Clean(visualCommand), VisualArgs: append([]string(nil), visualArgs...),
		VisualCommandSHA256: runtimeIdentity.VisualCommandSHA256, VisualEntrypointSHA256: runtimeIdentity.VisualEntrypointSHA256,
		Provider: request.Provider, ProviderBinary: filepath.Clean(providerCommand), ProviderArgs: providerArgs, ProviderModel: providerModel,
		ProviderBinarySHA256: runtimeIdentity.ProviderBinarySHA256,
		OperatorID:           resolvedOperator.ID, OperatorLogin: resolvedOperator.Login,
		WriterID: runtimeSnapshot.Writer.ID, WriterLogin: runtimeSnapshot.Writer.Login, WriterType: runtimeSnapshot.Writer.Type,
		AppID: runtimeSnapshot.App.AppID, InstallationID: runtimeSnapshot.App.InstallationID,
		PermissionDigest: runtimeSnapshot.App.PermissionDigest, Permissions: runtimeSnapshot.App.Permissions,
		RuntimeBindingDigest: runtimeSnapshot.BindingDigest, QualityProbe: qualityProbe,
		TestedAt: now, ExpiresAt: now.Add(dashboardPreflightValidity),
	}
	receipt.BindingSHA256, err = dashboardPreflightBinding(request, receipt)
	if err != nil {
		return nil, err
	}
	if err := saveDashboardPreflightReceipt(integratedStateRoot(), receipt); err != nil {
		return nil, err
	}
	return map[string]any{
		"schema_version": "hive.dashboard-integrated-preflight.v2", "ready": true,
		"repository": request.Repository, "repository_id": receipt.RepositoryID, "request_id": request.RequestID,
		"hosted":     config.IsKubernetesPod() && strings.TrimSpace(os.Getenv("HIVE_ID")) != "",
		"state_root": receipt.StateRoot, "storage": map[string]any{"persistent": hostedStatePathIsPersistent(receipt.StateRoot), "writable": true},
		"runtime": map[string]any{"hive_commit": receipt.HiveCommit, "hive_executable_sha256": receipt.HiveExecutableSHA256,
			"image_digest": receipt.ImageDigest, "image_digest_reported": receipt.ImageDigest != ""},
		"provider": map[string]any{"name": request.Provider, "command": filepath.Base(providerCommand), "binary_sha256": receipt.ProviderBinarySHA256,
			"model": providerModel, "health": "authenticated_model_verified", "model_calls": 1, "unattended": true},
		"operator": map[string]any{"id": receipt.OperatorID, "login": receipt.OperatorLogin},
		"app_writer": map[string]any{"id": receipt.WriterID, "login": receipt.WriterLogin, "type": receipt.WriterType,
			"app_id": receipt.AppID, "installation_id": receipt.InstallationID, "permission_digest": receipt.PermissionDigest,
			"permissions": receipt.Permissions, "runtime_binding_digest": receipt.RuntimeBindingDigest},
		"quality_runtime": receipt.QualityProbe,
		"visual_hive": map[string]any{"ref": visualRef, "command": filepath.Base(visualCommand), "args": append([]string(nil), visualArgs...),
			"command_sha256": receipt.VisualCommandSHA256, "entrypoint_sha256": receipt.VisualEntrypointSHA256},
		"controller_active": controllerActive, "repository_mutations": false, "binding_sha256": receipt.BindingSHA256,
		"tested_at": receipt.TestedAt, "expires_at": receipt.ExpiresAt,
	}, nil
}

func resolveDashboardPreflightCredential(repository string, credential any) (hivegithub.AuthenticatedUserIdentity, liveGitHubRuntimeSnapshot, bool, error) {
	switch value := credential.(type) {
	case hivegithub.AuthenticatedUserIdentity:
		runtimeSnapshot, err := currentDashboardGitHubRuntime()
		if err != nil {
			return value, liveGitHubRuntimeSnapshot{}, false, err
		}
		if !strings.EqualFold(runtimeSnapshot.Repository, repository) {
			return value, liveGitHubRuntimeSnapshot{}, false, fmt.Errorf("live GitHub runtime belongs to %s, not %s", runtimeSnapshot.Repository, repository)
		}
		return value, runtimeSnapshot, false, nil
	case string: // Narrow legacy test seam; dashboard handlers never pass tokens.
		token := strings.TrimSpace(value)
		operator := hivegithub.AuthenticatedUserIdentity{ID: 1, Login: "test-owner", Type: "User"}
		return operator, liveGitHubRuntimeSnapshot{Client: dashboardPreflightGitHubClient(token), Mode: "pat", Repository: repository,
			RepositoryID: 1, Writer: operator, BindingDigest: strings.Repeat("0", 64)}, true, nil
	default:
		return hivegithub.AuthenticatedUserIdentity{}, liveGitHubRuntimeSnapshot{}, false, errors.New("dashboard preflight credential is invalid")
	}
}

func runtimeIdentityOrPlaceholder(path string) string {
	if digest, err := daemonExecutableSHA256(path); err == nil {
		return digest
	}
	return strings.Repeat("8", 64)
}

// ensureManagedRepositoryContractBeforeSetup distinguishes a fresh install
// from an exact, durable managed update. A repository contract without local
// authoritative state remains an orphan and fails closed. An existing contract
// is accepted only when the selected state root belongs to the same immutable
// repository identity and its managed checkout contains byte-identical
// contract content. This permits supported policy transitions (for example
// L4/issues to L5/repair-pr) without weakening orphan detection.
func ensureManagedRepositoryContractBeforeSetup(
	ctx context.Context,
	client *hivegithub.Client,
	stateRoot, fullRepository string,
	repositoryID int64,
	owner, repository, defaultBranch string,
) (bool, error) {
	file, directory, response, err := client.GoGitHub().Repositories.GetContents(
		ctx, owner, repository, ".hive/integrated.json", &gh.RepositoryContentGetOptions{Ref: defaultBranch},
	)
	if err != nil {
		if response != nil && response.Response != nil && response.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("inspect repository contract before hosted setup: %w", err)
	}
	if file == nil || len(directory) != 0 || file.GetType() != "file" {
		return false, fmt.Errorf("repository managed contract is not one ordinary file")
	}
	orphaned := func(detail string) (bool, error) {
		if strings.TrimSpace(detail) != "" {
			detail = ": " + detail
		}
		return false, fmt.Errorf("repository already contains a managed Visual Hive contract without matching authoritative local state%s; inspect status and doctor, then use setup-reset recovery", detail)
	}
	store, err := integrated.NewStore(filepath.Join(filepath.Clean(stateRoot), "integrated"))
	if err != nil {
		return orphaned("state store is unavailable")
	}
	installed, err := store.Load()
	if err != nil {
		return orphaned("durable config is unavailable")
	}
	if !strings.EqualFold(strings.TrimSpace(installed.Repository), strings.TrimSpace(fullRepository)) ||
		strings.TrimSpace(installed.RepositoryID) != fmt.Sprintf("%d", repositoryID) ||
		filepath.Clean(installed.StateDir) != filepath.Clean(stateRoot) {
		return orphaned("repository or state binding differs")
	}
	expectedCheckout := filepath.Join(filepath.Clean(stateRoot), "integrated", "checkout")
	if filepath.Clean(installed.CheckoutDir) != expectedCheckout || installed.SetupPRNumber <= 0 || strings.TrimSpace(installed.SetupHeadSHA) == "" {
		return orphaned("setup transaction binding is incomplete")
	}
	contractPath := filepath.Join(expectedCheckout, ".hive", "integrated.json")
	info, err := os.Lstat(contractPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return orphaned("managed checkout contract is unavailable")
	}
	localContent, err := os.ReadFile(contractPath)
	if err != nil {
		return orphaned("managed checkout contract cannot be read")
	}
	remoteContent, err := file.GetContent()
	if err != nil || string(localContent) != remoteContent {
		return orphaned("repository contract differs from the durable managed checkout")
	}
	return true, nil
}

func ensureNoVisualRuntimeBeforeSetup(stateRoot, repository string, allowManagedUpdate bool) (bool, error) {
	exactActive := false
	if manager := dashboardNormalVisualRuntime.Load(); manager != nil {
		if binding, active := manager.ActiveBinding(); active {
			if !allowManagedUpdate || !strings.EqualFold(strings.TrimSpace(binding.Repository), strings.TrimSpace(repository)) ||
				filepath.Clean(binding.StateDir) != filepath.Clean(stateRoot) {
				return false, fmt.Errorf("Visual Hive controller is already active for %s", binding.Repository)
			}
			exactActive = true
		}
	}
	leasePath := filepath.Join(stateRoot, "integrated", "daemon.lease")
	if _, leaseErr := os.Lstat(leasePath); leaseErr == nil {
		if exactActive {
			owner, held := readNormalVisualDaemonLease(stateRoot)
			if !held || owner.PID != os.Getpid() {
				return false, fmt.Errorf("Visual Hive ownership lease at the selected state root is not held by this exact dashboard process")
			}
			return true, nil
		}
		return false, fmt.Errorf("Visual Hive ownership lease already exists at the selected state root")
	} else if !os.IsNotExist(leaseErr) {
		return false, fmt.Errorf("inspect Visual Hive ownership lease: %w", leaseErr)
	}
	if exactActive {
		return false, fmt.Errorf("active Visual Hive controller is missing its authoritative ownership lease")
	}
	return false, nil
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
	// The exact Quality runtime probe below is the single provider/model call
	// performed by hosted preflight.  Resolve the immutable model here, but do
	// not make a second prose-only call with a different HOME, UID, or approval
	// context: that would both spend provider capacity and overstate what was
	// actually tested.  Keep the parameters for the narrow test seam and to
	// preserve the existing resolver contract.
	_ = ctx
	_ = command
	model, err := repair.CodexSpecialistProviderModel(args)
	if err != nil {
		return "", fmt.Errorf("resolve configured Codex model: %w", err)
	}
	if strings.TrimSpace(model) == "" {
		return "", fmt.Errorf("configured Codex model is missing")
	}
	return model, nil
}

// normalizeDashboardPreflightProvider binds the concrete Codex model that the
// hosted readiness probe executes. Advisory and issues modes do not otherwise
// need a repair provider, but their preflight still promises to verify the
// configured provider/backend/model before setup. Use the same audited default
// that repair-pr will use later instead of accepting a model-less probe.
func normalizeDashboardPreflightProvider(provider string, automation integrated.Automation) (integrated.SetupOptions, error) {
	options := integrated.SetupOptions{
		Provider: provider, ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: automation,
	}
	normalized, err := integrated.NormalizeNormalHiveProvider(options)
	if err != nil {
		return integrated.SetupOptions{}, err
	}
	model, err := repair.CodexSpecialistProviderModel(normalized.ProviderArgs)
	if err != nil {
		return integrated.SetupOptions{}, err
	}
	if strings.TrimSpace(model) != "" {
		return normalized, nil
	}
	options.Automation = integrated.AutomationRepairPR
	return integrated.NormalizeNormalHiveProvider(options)
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
		Repository           string   `json:"repository"`
		RepositoryID         string   `json:"repository_id"`
		StateRoot            string   `json:"state_root"`
		Provider             string   `json:"provider"`
		ProviderBinary       string   `json:"provider_binary"`
		ProviderSHA256       string   `json:"provider_sha256"`
		ProviderArgs         []string `json:"provider_args"`
		ProviderModel        string   `json:"provider_model"`
		VisualHiveRef        string   `json:"visual_hive_ref"`
		VisualCommand        string   `json:"visual_command"`
		VisualArgs           []string `json:"visual_args"`
		VisualSHA256         string   `json:"visual_sha256"`
		EntrypointSHA        string   `json:"visual_entrypoint_sha256"`
		HiveCommit           string   `json:"hive_commit"`
		HiveSHA256           string   `json:"hive_executable_sha256"`
		ImageDigest          string   `json:"image_digest"`
		Coverage             string   `json:"coverage"`
		Automation           string   `json:"automation"`
		MaxActiveIssues      int      `json:"max_active_issues"`
		OperatorID           int64    `json:"operator_id"`
		OperatorLogin        string   `json:"operator_login"`
		WriterID             int64    `json:"writer_id"`
		WriterLogin          string   `json:"writer_login"`
		WriterType           string   `json:"writer_type"`
		AppID                int64    `json:"app_id,omitempty"`
		InstallationID       int64    `json:"installation_id,omitempty"`
		PermissionDigest     string   `json:"permission_digest,omitempty"`
		RuntimeBindingDigest string   `json:"runtime_binding_digest"`
		QualityUID           int      `json:"quality_uid"`
		QualityHome          string   `json:"quality_home"`
		QualityCodexHome     string   `json:"quality_codex_home"`
		QualityBackend       string   `json:"quality_backend"`
		QualityModel         string   `json:"quality_model"`
		QualityCommandSHA256 string   `json:"quality_command_sha256"`
		QualityOutputSHA256  string   `json:"quality_output_sha256"`
		QualityApproval      string   `json:"quality_approval_policy"`
		QualityToolCall      string   `json:"quality_tool_call"`
	}{
		Repository: request.Repository, RepositoryID: receipt.RepositoryID, StateRoot: receipt.StateRoot,
		Provider: request.Provider, ProviderBinary: receipt.ProviderBinary, ProviderSHA256: receipt.ProviderBinarySHA256,
		ProviderArgs: append([]string(nil), receipt.ProviderArgs...), ProviderModel: receipt.ProviderModel,
		VisualHiveRef: receipt.VisualHiveRef, VisualCommand: receipt.VisualCommand, VisualArgs: append([]string(nil), receipt.VisualArgs...),
		VisualSHA256: receipt.VisualCommandSHA256, EntrypointSHA: receipt.VisualEntrypointSHA256,
		HiveCommit: receipt.HiveCommit, HiveSHA256: receipt.HiveExecutableSHA256, ImageDigest: receipt.ImageDigest,
		Coverage: request.Coverage, Automation: request.Automation, MaxActiveIssues: preflightMaxActiveIssues(request),
		OperatorID: receipt.OperatorID, OperatorLogin: strings.ToLower(strings.TrimSpace(receipt.OperatorLogin)),
		WriterID: receipt.WriterID, WriterLogin: strings.ToLower(strings.TrimSpace(receipt.WriterLogin)), WriterType: strings.ToLower(strings.TrimSpace(receipt.WriterType)),
		AppID: receipt.AppID, InstallationID: receipt.InstallationID, PermissionDigest: receipt.PermissionDigest, RuntimeBindingDigest: receipt.RuntimeBindingDigest,
		QualityUID: receipt.QualityProbe.UID, QualityHome: receipt.QualityProbe.Home, QualityCodexHome: receipt.QualityProbe.CodexHome,
		QualityBackend: receipt.QualityProbe.Backend, QualityModel: receipt.QualityProbe.Model,
		QualityCommandSHA256: receipt.QualityProbe.CommandSHA256, QualityOutputSHA256: receipt.QualityProbe.OutputSHA256,
		QualityApproval: receipt.QualityProbe.ApprovalPolicy, QualityToolCall: receipt.QualityProbe.ToolCall,
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

func dashboardPreflightReceiptPath(root, repository string) (string, error) {
	ledgerPath, err := dashboardLifecycleLedgerPath(root)
	if err != nil {
		return "", fmt.Errorf("resolve hosted readiness preflight receipt: %w", err)
	}
	stateKey := strings.TrimSuffix(filepath.Base(ledgerPath), filepath.Ext(ledgerPath))
	repositoryDigest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(repository))))
	return filepath.Join(
		filepath.Dir(ledgerPath),
		"preflight",
		stateKey+"-"+hex.EncodeToString(repositoryDigest[:8])+".json",
	), nil
}

func saveDashboardPreflightReceipt(root string, receipt dashboardPreflightReceipt) error {
	path, err := dashboardPreflightReceiptPath(root, receipt.Repository)
	if err != nil {
		return err
	}
	if err := ensureOrdinaryPreflightDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("create integrated preflight receipt directory: %w", err)
	}
	if _, err := validateDashboardPreflightReceiptFile(path); err != nil {
		return err
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
	if _, err := validateDashboardPreflightReceiptFile(path); err != nil {
		return err
	}
	return nil
}

func removeDashboardPreflightReceipt(repository string) error {
	path, pathErr := dashboardPreflightReceiptPath(integratedStateRoot(), repository)
	if pathErr != nil {
		return pathErr
	}
	exists, validateErr := validateDashboardPreflightReceiptFile(path)
	if validateErr != nil {
		return validateErr
	}
	if !exists {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("retire hosted readiness preflight receipt: %w", err)
	}
	return nil
}

func loadDashboardPreflightReceipt(repository string) (dashboardPreflightReceipt, bool, error) {
	path, pathErr := dashboardPreflightReceiptPath(integratedStateRoot(), repository)
	if pathErr != nil {
		return dashboardPreflightReceipt{}, false, pathErr
	}
	data, exists, err := readDashboardPreflightReceiptFile(path)
	if err != nil {
		return dashboardPreflightReceipt{}, false, fmt.Errorf("read hosted readiness preflight receipt: %w", err)
	}
	if !exists {
		return dashboardPreflightReceipt{}, false, nil
	}
	var receipt dashboardPreflightReceipt
	stateDir, stateErr := dashboardSetupStateDir(repository)
	if stateErr != nil {
		return dashboardPreflightReceipt{}, false, stateErr
	}
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.SchemaVersion != "hive.dashboard-integrated-preflight-receipt.v2" ||
		receipt.Repository != repository || receipt.StateRoot != filepath.Clean(stateDir) || receipt.TestedAt.IsZero() || receipt.ExpiresAt.IsZero() ||
		!validDaemonHexIdentity(receipt.HiveCommit, 40) || !validDaemonHexIdentity(receipt.HiveExecutableSHA256, 64) ||
		!validDaemonHexIdentity(receipt.ProviderBinarySHA256, 64) || !validDaemonHexIdentity(receipt.VisualCommandSHA256, 64) ||
		receipt.OperatorID <= 0 || strings.TrimSpace(receipt.OperatorLogin) == "" || receipt.WriterID <= 0 || strings.TrimSpace(receipt.WriterLogin) == "" ||
		strings.TrimSpace(receipt.WriterType) == "" || !validDaemonHexIdentity(receipt.RuntimeBindingDigest, 64) || receipt.QualityProbe.UID <= 0 ||
		receipt.QualityProbe.ApprovalPolicy != "never" || receipt.QualityProbe.ToolCall != "read-only-local-file" ||
		!validDaemonHexIdentity(receipt.QualityProbe.CommandSHA256, 64) || !validDaemonHexIdentity(receipt.QualityProbe.OutputSHA256, 64) {
		return dashboardPreflightReceipt{}, false, fmt.Errorf("hosted readiness preflight receipt is invalid")
	}
	return receipt, true, nil
}

func requireDashboardPreflightReceipt(ctx context.Context, request dashboard.IntegratedSetupRequest, bindings ...any) error {
	if !(config.IsKubernetesPod() && strings.TrimSpace(os.Getenv("HIVE_ID")) != "") {
		return nil
	}
	path, pathErr := dashboardPreflightReceiptPath(integratedStateRoot(), request.Repository)
	if pathErr != nil {
		return pathErr
	}
	data, exists, err := readDashboardPreflightReceiptFile(path)
	if err != nil {
		return fmt.Errorf("hosted setup requires a successful readiness preflight: %w", err)
	}
	if !exists {
		return fmt.Errorf("hosted setup requires a successful readiness preflight: receipt is absent")
	}
	var receipt dashboardPreflightReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return fmt.Errorf("decode hosted readiness preflight receipt: %w", err)
	}
	operator, runtimeSnapshot, err := dashboardPreflightSetupBindings(receipt, bindings)
	if err != nil {
		return err
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
	normalized, normalizeErr := normalizeDashboardPreflightProvider(request.Provider, integrated.Automation(request.Automation))
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
	if receipt.SchemaVersion != "hive.dashboard-integrated-preflight-receipt.v2" || receipt.Repository != request.Repository ||
		receipt.StateRoot != filepath.Clean(stateDir) || receipt.VisualHiveRef != request.VisualHiveRef ||
		receipt.Provider != request.Provider || receipt.BindingSHA256 != expected || receipt.TestedAt.IsZero() || !receipt.ExpiresAt.After(now) ||
		receipt.OperatorID != operator.ID || !strings.EqualFold(receipt.OperatorLogin, operator.Login) ||
		receipt.WriterID != runtimeSnapshot.Writer.ID || !strings.EqualFold(receipt.WriterLogin, runtimeSnapshot.Writer.Login) ||
		!strings.EqualFold(receipt.WriterType, runtimeSnapshot.Writer.Type) || receipt.AppID != runtimeSnapshot.App.AppID ||
		receipt.InstallationID != runtimeSnapshot.App.InstallationID || receipt.PermissionDigest != runtimeSnapshot.App.PermissionDigest ||
		receipt.RuntimeBindingDigest != runtimeSnapshot.BindingDigest || receipt.QualityProbe.ApprovalPolicy != "never" ||
		receipt.QualityProbe.ToolCall != "read-only-local-file" {
		return fmt.Errorf("hosted readiness preflight is missing, expired, or bound to different setup inputs")
	}
	if runtimeSnapshot.Mode == "app" {
		if err := runtimeSnapshot.App.RequireVisualHivePermissions(); err != nil {
			return fmt.Errorf("hosted readiness preflight App permissions changed: %w", err)
		}
	}
	return nil
}

func ensureOrdinaryPreflightDirectory(path string) error {
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil || strings.TrimSpace(path) == "" {
		return errors.New("preflight receipt directory is invalid")
	}
	absolute = filepath.Clean(absolute)
	parent := filepath.Dir(absolute)
	if parent != absolute {
		if err := ensureOrdinaryPreflightDirectory(parent); err != nil {
			return err
		}
	}
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		if err := os.Mkdir(absolute, 0o700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create preflight receipt directory %s: %w", absolute, err)
		}
		info, err = os.Lstat(absolute)
	}
	if err != nil {
		return fmt.Errorf("inspect preflight receipt directory %s: %w", absolute, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("preflight receipt directory %s must be a real directory", absolute)
	}
	return nil
}

func validateDashboardPreflightReceiptFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect hosted readiness preflight receipt: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, errors.New("hosted readiness preflight receipt must be a regular non-link file")
	}
	if info.Size() > dashboardPreflightReceiptMaxBytes {
		return false, errors.New("hosted readiness preflight receipt exceeds its size limit")
	}
	return true, nil
}

func readDashboardPreflightReceiptFile(path string) ([]byte, bool, error) {
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect hosted readiness preflight receipt: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, false, errors.New("hosted readiness preflight receipt must be a regular non-link file")
	}
	if before.Size() > dashboardPreflightReceiptMaxBytes {
		return nil, false, errors.New("hosted readiness preflight receipt exceeds its size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open hosted readiness preflight receipt: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return nil, false, errors.New("hosted readiness preflight receipt changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, dashboardPreflightReceiptMaxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read hosted readiness preflight receipt: %w", err)
	}
	if len(data) > dashboardPreflightReceiptMaxBytes {
		return nil, false, errors.New("hosted readiness preflight receipt exceeds its size limit")
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return nil, false, errors.New("hosted readiness preflight receipt changed while it was read")
	}
	return data, true, nil
}

func dashboardPreflightSetupBindings(receipt dashboardPreflightReceipt, bindings []any) (hivegithub.AuthenticatedUserIdentity, liveGitHubRuntimeSnapshot, error) {
	if len(bindings) == 0 {
		operator := hivegithub.AuthenticatedUserIdentity{ID: receipt.OperatorID, Login: receipt.OperatorLogin, Type: "User"}
		runtime := liveGitHubRuntimeSnapshot{
			Mode: "pat", Repository: receipt.Repository, Writer: hivegithub.AuthenticatedUserIdentity{
				ID: receipt.WriterID, Login: receipt.WriterLogin, Type: receipt.WriterType,
			}, BindingDigest: receipt.RuntimeBindingDigest,
		}
		if receipt.AppID > 0 || receipt.InstallationID > 0 {
			runtime.Mode = "app"
			runtime.App = hivegithub.AppRuntimeIdentity{
				AppID: receipt.AppID, InstallationID: receipt.InstallationID, BotID: receipt.WriterID,
				BotLogin: receipt.WriterLogin, BotType: receipt.WriterType, PermissionDigest: receipt.PermissionDigest,
				Permissions: receipt.Permissions,
			}
		}
		return operator, runtime, nil
	}
	if len(bindings) != 2 {
		return hivegithub.AuthenticatedUserIdentity{}, liveGitHubRuntimeSnapshot{}, errors.New("hosted readiness preflight setup binding is incomplete")
	}
	operator, operatorOK := bindings[0].(hivegithub.AuthenticatedUserIdentity)
	runtime, runtimeOK := bindings[1].(liveGitHubRuntimeSnapshot)
	if !operatorOK || !runtimeOK {
		return hivegithub.AuthenticatedUserIdentity{}, liveGitHubRuntimeSnapshot{}, errors.New("hosted readiness preflight setup binding is invalid")
	}
	return operator, runtime, nil
}

func hostedStatePathIsPersistent(stateDir string) bool {
	parent := filepath.Dir(filepath.Clean(hostedIntegratedStateRoot))
	relative, err := filepath.Rel(parent, filepath.Clean(stateDir))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
