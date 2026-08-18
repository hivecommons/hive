package repair

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
)

type codexSpecialistAuthorization struct {
	attestation         *codexProviderIdentityAttestation
	model               string
	configurationSHA256 string
	expiresAt           time.Time
}

const maxCodexSpecialistAuthorizations = 32

// CodexSpecialistChildExecutor adapts the exact integrated repair Codex core
// to the ordinary Manager's process interface. Each Check result owns one
// unshareable attestation ID consumed only by its matching Start.
type CodexSpecialistChildExecutor struct {
	provider         CodexProvider
	mu               sync.Mutex
	pending          map[string]codexSpecialistAuthorization
	authorizationTTL time.Duration
}

func NewCodexSpecialistChildExecutor(provider CodexProvider) (*CodexSpecialistChildExecutor, error) {
	if strings.TrimSpace(provider.Command) == "" {
		return nil, errors.New("Codex specialist executor requires a provider command")
	}
	model, err := CodexSpecialistProviderModel(provider.Prefix)
	if err != nil {
		return nil, err
	}
	if model == "" {
		return nil, errors.New("Codex specialist executor requires one explicit --model")
	}
	return &CodexSpecialistChildExecutor{provider: provider, pending: make(map[string]codexSpecialistAuthorization), authorizationTTL: 2 * time.Minute}, nil
}

func (executor *CodexSpecialistChildExecutor) Check(ctx context.Context) (agent.SpecialistChildExecutorIdentity, error) {
	if executor == nil {
		return agent.SpecialistChildExecutorIdentity{}, errors.New("Codex specialist executor is nil")
	}
	if !codexSpecialistContainmentHostSupported(runtime.GOOS) {
		return agent.SpecialistChildExecutorIdentity{}, fmt.Errorf("unsupported Codex containment host %q", runtime.GOOS)
	}
	model, err := CodexSpecialistProviderModel(executor.provider.Prefix)
	if err != nil {
		return agent.SpecialistChildExecutorIdentity{}, err
	}
	attestation, err := executor.provider.healthAttestation(ctx)
	if err != nil {
		return agent.SpecialistChildExecutorIdentity{}, err
	}
	if err := verifyCodexSpecialistExactProcessTree(ctx, executor.provider, attestation); err != nil {
		return agent.SpecialistChildExecutorIdentity{}, fmt.Errorf("Codex specialist exact process-tree health check failed: %w", err)
	}
	configurationSHA256, err := codexSpecialistConfigurationDigest(attestation.Command.SHA256, attestation.IdentitySHA256, attestation.ContainmentHelper, model)
	if err != nil {
		return agent.SpecialistChildExecutorIdentity{}, err
	}
	authorizationID, err := newCodexSpecialistAuthorizationID()
	if err != nil {
		return agent.SpecialistChildExecutorIdentity{}, err
	}
	if err := executor.storeAuthorization(authorizationID, codexSpecialistAuthorization{
		attestation: attestation, model: model, configurationSHA256: configurationSHA256,
	}); err != nil {
		return agent.SpecialistChildExecutorIdentity{}, err
	}
	return agent.SpecialistChildExecutorIdentity{
		Backend: "codex", ProviderSHA256: attestation.Command.SHA256, Model: model,
		ConfigurationSHA256: configurationSHA256, AuthorizationID: authorizationID,
	}, nil
}

func (executor *CodexSpecialistChildExecutor) storeAuthorization(authorizationID string, authorization codexSpecialistAuthorization) error {
	executor.mu.Lock()
	now := time.Now().UTC()
	for key, current := range executor.pending {
		if current.expiresAt.IsZero() || !now.Before(current.expiresAt) {
			delete(executor.pending, key)
		}
	}
	if len(executor.pending) >= maxCodexSpecialistAuthorizations {
		executor.mu.Unlock()
		return errors.New("Codex specialist authorization reservation limit reached")
	}
	if _, exists := executor.pending[authorizationID]; exists {
		executor.mu.Unlock()
		return errors.New("Codex specialist authorization identity already exists")
	}
	ttl := executor.authorizationTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	expiresAt := time.Now().UTC().Add(ttl)
	authorization.expiresAt = expiresAt
	executor.pending[authorizationID] = authorization
	executor.mu.Unlock()
	time.AfterFunc(ttl, func() {
		executor.mu.Lock()
		if current, ok := executor.pending[authorizationID]; ok && current.expiresAt.Equal(expiresAt) {
			delete(executor.pending, authorizationID)
		}
		executor.mu.Unlock()
	})
	return nil
}

func (executor *CodexSpecialistChildExecutor) consumeAuthorization(authorizationID string) (codexSpecialistAuthorization, error) {
	executor.mu.Lock()
	authorization, ok := executor.pending[authorizationID]
	if ok {
		delete(executor.pending, authorizationID)
	}
	executor.mu.Unlock()
	if !ok || authorization.attestation == nil {
		return codexSpecialistAuthorization{}, errors.New("Codex specialist authorization is absent or already consumed")
	}
	if authorization.expiresAt.IsZero() || !time.Now().UTC().Before(authorization.expiresAt) {
		return codexSpecialistAuthorization{}, errors.New("Codex specialist authorization expired before launch")
	}
	return authorization, nil
}

func codexSpecialistContainmentHostSupported(goos string) bool {
	return goos == "linux" || goos == "windows"
}

func verifyCodexSpecialistExactProcessTree(ctx context.Context, provider CodexProvider, attestation *codexProviderIdentityAttestation) error {
	if !codexSpecialistExactHealthRequired(runtime.GOOS, codexProviderIsTestExecutable(provider.Command)) {
		return nil
	}
	sealed, err := attestation.sealForRun()
	if err != nil {
		return fmt.Errorf("seal exact specialist health executables: %w", err)
	}
	defer sealed.cleanupSeal()
	privateRuntime, err := prepareCodexPrivateRuntime(sealed, provider.Command, "")
	if err != nil {
		return fmt.Errorf("prepare exact specialist health runtime: %w", err)
	}
	defer privateRuntime.cleanup()
	securedProvider := sealed.provider()
	securedProvider.runtimeEnvironment = privateRuntime.environment
	return verifyCodexPlatformContainmentWithExactProcessTree(
		ctx, securedProvider.Command, securedProvider.commandEnvironment(), sealed.SealedContainmentHelper, sealed.SealedCapabilityDropHelper,
	)
}

func codexSpecialistExactHealthRequired(goos string, testExecutable bool) bool {
	return goos == "linux" && !testExecutable
}

func (executor *CodexSpecialistChildExecutor) Start(ctx context.Context, request agent.SpecialistChildExecutionRequest) (agent.SpecialistChildProcess, error) {
	if executor == nil {
		return nil, errors.New("Codex specialist executor is nil")
	}
	if request.ContainmentProfile != agent.SpecialistContainmentProfileV1 || request.Root == "" || request.Prompt == "" || request.AuthorizationID == "" {
		return nil, errors.New("Codex specialist execution request is incomplete")
	}
	authorization, err := executor.consumeAuthorization(request.AuthorizationID)
	if err != nil {
		return nil, err
	}
	if request.Model != authorization.model || request.ConfigurationSHA256 != authorization.configurationSHA256 || request.ProviderSHA256 != authorization.attestation.Command.SHA256 {
		return nil, errors.New("Codex specialist logical executor identity changed after Health")
	}
	process, err := startCodexAttestedProcess(ctx, executor.provider, authorization.attestation, request.Prompt, false, request.Root)
	if err != nil {
		return nil, err
	}
	return &codexSpecialistChildProcess{process: process, turnID: request.AuthorizationID, providerSHA256: request.ProviderSHA256}, nil
}

type codexSpecialistChildProcess struct {
	process        *codexRunningProcess
	turnID         string
	providerSHA256 string
}

func (process *codexSpecialistChildProcess) PID() int { return process.process.PID() }

func (process *codexSpecialistChildProcess) ProviderSHA256() string {
	return process.providerSHA256
}

func (process *codexSpecialistChildProcess) Wait() (agent.SpecialistChildExecutionResult, error) {
	result, err := process.process.WaitProvider()
	if err != nil {
		return agent.SpecialistChildExecutionResult{}, err
	}
	return agent.SpecialistChildExecutionResult{
		Response: []byte(result.Output), TurnID: process.turnID, CompletedAt: time.Now().UTC(),
	}, nil
}

func (process *codexSpecialistChildProcess) ForceReap(ctx context.Context) error {
	if process == nil || process.process == nil {
		return errors.New("Codex specialist child process is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	process.process.cancel()
	done := make(chan error, 1)
	go func() {
		_, _ = process.process.WaitProvider()
		done <- process.process.ReapError()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CodexSpecialistProviderModel returns the single explicitly configured Codex
// model. An empty result means no model was supplied; malformed or duplicate
// model options fail closed.
func CodexSpecialistProviderModel(arguments []string) (string, error) {
	if err := validateCodexProviderPrefix(arguments); err != nil {
		return "", err
	}
	model := ""
	for index := 0; index < len(arguments); index++ {
		argument := strings.TrimSpace(arguments[index])
		var candidate string
		switch {
		case argument == "--model":
			index++
			if index >= len(arguments) {
				return "", errors.New("Codex --model is missing its value")
			}
			candidate = strings.TrimSpace(arguments[index])
		case strings.HasPrefix(argument, "--model="):
			candidate = strings.TrimSpace(strings.TrimPrefix(argument, "--model="))
		default:
			continue
		}
		if candidate == "" || strings.ContainsAny(candidate, "\x00\r\n") || model != "" {
			return "", errors.New("Codex specialist executor model must be one explicit bounded value")
		}
		model = candidate
	}
	return model, nil
}

func codexSpecialistConfigurationDigest(providerSHA256, attestationSHA256 string, containmentHelper codexProviderFileIdentity, model string) (string, error) {
	encoded, err := json.Marshal(struct {
		ProviderSHA256    string                    `json:"provider_sha256"`
		AttestationSHA256 string                    `json:"attestation_sha256"`
		ContainmentHelper codexProviderFileIdentity `json:"containment_helper"`
		Model             string                    `json:"model"`
		Profile           string                    `json:"profile"`
		DisabledFeatures  []string                  `json:"disabled_features"`
		PermissionArgs    []string                  `json:"permission_args"`
	}{
		ProviderSHA256: providerSHA256, AttestationSHA256: attestationSHA256, Model: model, Profile: agent.SpecialistContainmentProfileV1,
		ContainmentHelper: containmentHelper, DisabledFeatures: append([]string(nil), codexDisabledToolFeatures...), PermissionArgs: codexNoFilesPermissionArgs(),
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func newCodexSpecialistAuthorizationID() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "codex-auth-" + hex.EncodeToString(value), nil
}

var _ agent.SpecialistChildExecutor = (*CodexSpecialistChildExecutor)(nil)
