package repair

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
)

func TestCodexSpecialistExecutorRequiresOneExplicitModelAndSupportedHost(t *testing.T) {
	for _, provider := range []CodexProvider{
		{Command: "codex"},
		{Command: "codex", Prefix: []string{"--model"}},
		{Command: "codex", Prefix: []string{"--model", "one", "--model=two"}},
		{Command: "codex", Prefix: []string{"--model=one", "--bogus"}},
		{Command: "codex", Prefix: []string{"--model=one", "exec"}},
	} {
		if _, err := NewCodexSpecialistChildExecutor(provider); err == nil {
			t.Fatalf("unsafe executor model configuration was accepted: %+v", provider)
		}
	}
	if _, err := NewCodexSpecialistChildExecutor(CodexProvider{Command: "codex", Prefix: []string{"--model", "proof-model"}}); err != nil {
		t.Fatalf("one explicit model was rejected: %v", err)
	}
	for _, goos := range []string{"darwin", "freebsd", "plan9", "js"} {
		if codexSpecialistContainmentHostSupported(goos) {
			t.Fatalf("unsupported containment host %q was accepted", goos)
		}
	}
}

func TestOrdinaryCodexPathNeverSelectsSpecialistExactBoundary(t *testing.T) {
	// Empty is the public Provider Health/Run signal on every host, including
	// unsupported specialist hosts such as macOS and BSD. The later temporary
	// root must not be able to change this decision.
	for _, goos := range []string{"linux", "windows", "darwin", "freebsd"} {
		if codexProposalProcessTreeRequested("") {
			t.Fatalf("ordinary Codex invocation selected specialist process-tree containment on %s", goos)
		}
	}
	if !codexProposalProcessTreeRequested(t.TempDir()) {
		t.Fatal("explicit Manager-owned specialist root did not select exact process-tree containment")
	}
	if !codexSpecialistExactHealthRequired("linux", false) || codexSpecialistExactHealthRequired("darwin", false) ||
		codexSpecialistExactHealthRequired("freebsd", false) || codexSpecialistExactHealthRequired("linux", true) {
		t.Fatal("specialist-only nested health selection drifted into ordinary or unsupported-host behavior")
	}
}

func TestCodexSpecialistExecutorAuthorizationIsExactAndSingleConsumer(t *testing.T) {
	authorizationID := "codex-auth-" + strings.Repeat("c", 64)
	providerSHA := strings.Repeat("a", 64)
	configurationSHA := strings.Repeat("b", 64)
	executor := &CodexSpecialistChildExecutor{
		pending: map[string]codexSpecialistAuthorization{
			authorizationID: {
				attestation: &codexProviderIdentityAttestation{Command: codexProviderFileIdentity{SHA256: providerSHA}},
				model:       "proof-model", configurationSHA256: configurationSHA, expiresAt: time.Now().UTC().Add(time.Minute),
			},
		},
	}
	request := agent.SpecialistChildExecutionRequest{
		Root: "ignored", Prompt: "ignored", ContainmentProfile: agent.SpecialistContainmentProfileV1,
		AuthorizationID: authorizationID, ProviderSHA256: providerSHA, ConfigurationSHA256: configurationSHA,
		Model: "mismatched-model",
	}
	errorsSeen := make(chan string, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := executor.Start(context.Background(), request)
			if err == nil {
				errorsSeen <- ""
				return
			}
			errorsSeen <- err.Error()
		}()
	}
	wait.Wait()
	close(errorsSeen)
	changed, absent := 0, 0
	for message := range errorsSeen {
		switch {
		case strings.Contains(message, "identity changed"):
			changed++
		case strings.Contains(message, "absent or already consumed"):
			absent++
		default:
			t.Fatalf("unexpected authorization result: %q", message)
		}
	}
	if changed != 1 || absent != 1 {
		t.Fatalf("authorization consumption results: changed=%d absent=%d", changed, absent)
	}
}

func TestCodexSpecialistAuthorizationsAreIndependentSingleUseBoundedAndExpire(t *testing.T) {
	executor := &CodexSpecialistChildExecutor{pending: make(map[string]codexSpecialistAuthorization), authorizationTTL: 25 * time.Millisecond}
	authorization := codexSpecialistAuthorization{attestation: &codexProviderIdentityAttestation{}, model: "proof-model", configurationSHA256: strings.Repeat("a", 64)}
	if err := executor.storeAuthorization("first", authorization); err != nil {
		t.Fatal(err)
	}
	if err := executor.storeAuthorization("second", authorization); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.consumeAuthorization("first"); err != nil {
		t.Fatalf("first independent reservation was not consumable: %v", err)
	}
	if _, err := executor.consumeAuthorization("second"); err != nil {
		t.Fatalf("second independent reservation was revoked by the first: %v", err)
	}
	if _, err := executor.consumeAuthorization("first"); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("first reservation was reusable: %v", err)
	}
	if err := executor.storeAuthorization("expiring", authorization); err != nil {
		t.Fatal(err)
	}
	time.Sleep(75 * time.Millisecond)
	executor.mu.Lock()
	remaining := len(executor.pending)
	executor.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expired authorization remained live: %d", remaining)
	}
	executor.authorizationTTL = time.Minute
	for index := range maxCodexSpecialistAuthorizations {
		if err := executor.storeAuthorization(fmt.Sprintf("bounded-%02d", index), authorization); err != nil {
			t.Fatal(err)
		}
	}
	if err := executor.storeAuthorization("overflow", authorization); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("authorization reservation cap was not enforced: %v", err)
	}
}

func TestCodexSpecialistTwoCheckedReservationsBothStartExactlyOnce(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	t.Setenv("HIVE_TEST_CODEX_MODEL_OUTPUT", "DENIED")
	executor, err := NewCodexSpecialistChildExecutor(CodexProvider{
		Command: os.Args[0], Prefix: []string{"--model", "proof-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	first, err := executor.Check(ctx)
	if err != nil {
		t.Fatalf("first Check failed: %v", err)
	}
	second, err := executor.Check(ctx)
	if err != nil {
		t.Fatalf("second Check revoked or blocked the first: %v", err)
	}
	if first.AuthorizationID == second.AuthorizationID {
		t.Fatal("independent Checks returned the same authorization")
	}
	request := func(identity agent.SpecialistChildExecutorIdentity, root string) agent.SpecialistChildExecutionRequest {
		return agent.SpecialistChildExecutionRequest{
			Root: root, Prompt: "Return DENIED", ContainmentProfile: agent.SpecialistContainmentProfileV1,
			AuthorizationID: identity.AuthorizationID, ProviderSHA256: identity.ProviderSHA256,
			ConfigurationSHA256: identity.ConfigurationSHA256, Model: identity.Model,
		}
	}
	firstRequest := request(first, t.TempDir())
	secondRequest := request(second, t.TempDir())
	firstProcess, err := executor.Start(ctx, firstRequest)
	if err != nil {
		t.Fatalf("first checked reservation did not start: %v", err)
	}
	secondProcess, err := executor.Start(ctx, secondRequest)
	if err != nil {
		t.Fatalf("second checked reservation was revoked by first Start: %v", err)
	}
	for index, value := range []struct {
		process agent.SpecialistChildProcess
		turn    string
	}{{firstProcess, first.AuthorizationID}, {secondProcess, second.AuthorizationID}} {
		result, err := value.process.Wait()
		if err != nil || string(result.Response) != "DENIED" || result.TurnID != value.turn {
			t.Fatalf("process %d result=%+v err=%v", index, result, err)
		}
	}
	if _, err := executor.Start(ctx, firstRequest); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("first authorization was reusable: %v", err)
	}
	crossRequest := firstRequest
	crossRequest.AuthorizationID = "codex-auth-" + strings.Repeat("d", 64)
	if _, err := executor.Start(ctx, crossRequest); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("cross-authorization request reached launch: %v", err)
	}
}

func TestCodexSpecialistConfigurationDigestBindsExecutableAttestationModelAndCapabilities(t *testing.T) {
	helper := codexProviderFileIdentity{Path: "/sealed/usr/bin/bwrap", SHA256: strings.Repeat("e", 64), Bytes: 1234}
	base, err := codexSpecialistConfigurationDigest(strings.Repeat("a", 64), strings.Repeat("b", 64), helper, "proof-model")
	if err != nil {
		t.Fatal(err)
	}
	values := []struct {
		provider, attestation, model string
		helper                       codexProviderFileIdentity
	}{
		{strings.Repeat("c", 64), strings.Repeat("b", 64), "proof-model", helper},
		{strings.Repeat("a", 64), strings.Repeat("d", 64), "proof-model", helper},
		{strings.Repeat("a", 64), strings.Repeat("b", 64), "other-model", helper},
		{strings.Repeat("a", 64), strings.Repeat("b", 64), "proof-model", codexProviderFileIdentity{Path: "/other/bwrap", SHA256: helper.SHA256, Bytes: helper.Bytes}},
		{strings.Repeat("a", 64), strings.Repeat("b", 64), "proof-model", codexProviderFileIdentity{Path: helper.Path, SHA256: strings.Repeat("f", 64), Bytes: helper.Bytes}},
	}
	for _, value := range values {
		digest, err := codexSpecialistConfigurationDigest(value.provider, value.attestation, value.helper, value.model)
		if err != nil || digest == base {
			t.Fatalf("executor identity component was not digest-bound: value=%v digest=%q err=%v", value, digest, err)
		}
	}
}

func TestCodexSpecialistForceReapIsIdempotentAfterExactWaitProof(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	running := &codexRunningProcess{
		pid: 42, providerSHA256: strings.Repeat("a", 64),
		wait: func() error {
			waits++
			return nil
		},
		cancel: cancel, commandContext: ctx,
		stdout: &codexHardLimitBuffer{limit: codexStdoutHardLimit},
		stderr: &codexHardLimitBuffer{limit: codexStderrCaptureLimit("")},
	}
	process := &codexSpecialistChildProcess{process: running, providerSHA256: strings.Repeat("a", 64)}
	if err := process.ForceReap(context.Background()); err != nil {
		t.Fatalf("first exact reap proof failed: %v", err)
	}
	if err := process.ForceReap(context.Background()); err != nil {
		t.Fatalf("idempotent exact reap proof failed: %v", err)
	}
	if waits != 1 {
		t.Fatalf("exact process wait ran %d times; want one", waits)
	}
}
