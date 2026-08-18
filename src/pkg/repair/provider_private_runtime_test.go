package repair

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCodexProviderPrivateRuntimeDropsDynamicAmbientAuthority(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	t.Setenv("HIVE_TEST_EXPECT_CLEAN_CODEX_ENV", "1")
	for name, value := range map[string]string{
		"GH_TOKEN": "gh-canary", "GITHUB_TOKEN": "github-canary", "GITHUB_APP_PRIVATE_KEY": "app-key-canary",
		"SSH_AUTH_SOCK": "ssh-canary", "DYNAMIC_PROVIDER_TOKEN": "dynamic-canary", "HTTPS_PROXY": "proxy-canary",
		"HTTP_PROXY": "proxy-canary", "ALL_PROXY": "proxy-canary", "AWS_SHARED_CREDENTIALS_FILE": "credential-path-canary",
		"GIT_CONFIG_GLOBAL": "git-config-canary", "HOME": "ambient-home-canary", "USERPROFILE": "ambient-home-canary",
		"CODEX_HOME": "ambient-codex-home-canary", "APPDATA": "ambient-appdata-canary", "LOCALAPPDATA": "ambient-localappdata-canary",
		"SSL_CERT_FILE": "ambient-cert-file-canary", "SSL_CERT_DIR": "ambient-cert-dir-canary", "COMSPEC": "ambient-comspec-canary",
		"PATH": "ambient-path-canary",
	} {
		t.Setenv(name, value)
	}
	provider := CodexProvider{Command: os.Args[0], CodexHome: t.TempDir()}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatalf("clean private Health: %v", err)
	}
	result, err := provider.Run(context.Background(), "ignored-worktree", "Return the denial sentinel.")
	if err != nil || result.Output != "DENIED" {
		t.Fatalf("clean private Run: result=%+v err=%v", result, err)
	}
}

func TestCodexProviderPrivateRuntimeUsesPrivateWindowsProfileDirectories(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows private profile environment assertion")
	}
	t.Setenv("APPDATA", "ambient-appdata-canary")
	t.Setenv("LOCALAPPDATA", "ambient-localappdata-canary")
	state, err := prepareCodexPrivateRuntime(&codexProviderIdentityAttestation{}, os.Args[0], "")
	if err != nil {
		t.Fatal(err)
	}
	defer state.cleanup()
	environment := make(map[string]string)
	for _, pair := range state.environment {
		name, value, _ := strings.Cut(pair, "=")
		environment[strings.ToUpper(name)] = value
	}
	for _, name := range []string{"APPDATA", "LOCALAPPDATA"} {
		value := filepath.Clean(environment[name])
		if value == "" || value == filepath.Clean("ambient-"+strings.ToLower(name)+"-canary") || filepath.Dir(value) != filepath.Clean(state.root) {
			t.Fatalf("%s was not replaced with a private invocation directory: %q", name, value)
		}
		info, statErr := os.Lstat(value)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s private directory is unsafe: info=%v err=%v", name, info, statErr)
		}
		if reparse, reparseErr := codexProviderPathIsReparsePoint(value); reparseErr != nil || reparse {
			t.Fatalf("%s private directory is linked: reparse=%t err=%v", name, reparse, reparseErr)
		}
	}
}

func TestCodexLinuxContainmentHelperPathIsSealedAndSourceDriftFailsClosed(t *testing.T) {
	source := filepath.Join(t.TempDir(), "bwrap-source")
	if err := os.WriteFile(source, []byte("ordinary-bwrap-test-bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := inspectCodexProviderFile(source)
	if err != nil {
		t.Fatal(err)
	}
	sealed, root, err := sealCodexContainmentHelper(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupCodexContainmentHelperSeal(root, sealed.Path)
	commandIdentity, err := inspectCodexProviderFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	attestation := &codexProviderIdentityAttestation{
		Command: commandIdentity, ContainmentHelper: identity, SealedContainmentHelper: sealed, ContainmentHelperRoot: root,
	}
	environment, err := bindCodexRuntimePath("linux", []string{"PATH=ambient-untrusted", "LANG=C"}, attestation, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(environment, "\n") != "LANG=C\nPATH="+root {
		t.Fatalf("Linux helper PATH retained ambient entries: %q", environment)
	}
	if err := os.WriteFile(source, []byte("changed-bwrap-test-bytes!"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := attestation.revalidateSource(context.Background()); err == nil || !strings.Contains(err.Error(), "containment helper changed") {
		t.Fatalf("helper drift was not rejected before launch: %v", err)
	}
}

func TestCodexLinuxProviderRuntimeBundlesAttestedContainmentHelperBesideSealedCommand(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the packaged Codex bundled helper layout is Linux-only")
	}
	command, err := inspectCodexProviderFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	helperSource := filepath.Join(t.TempDir(), "bwrap-source")
	if err := os.WriteFile(helperSource, []byte("ordinary-bwrap-test-bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	helper, err := inspectCodexProviderFile(helperSource)
	if err != nil {
		t.Fatal(err)
	}
	sealedCommand, sealedHelper, sealRoot, helperRoot, err := sealCodexProviderRuntime(command, helper, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			_ = cleanupCodexProviderRuntimeSeal(sealRoot, sealedCommand.Path, sealedHelper.Path)
		}
	})
	wantHelperRoot := filepath.Join(sealRoot, codexBundledResourcesDirectory)
	wantHelperPath := filepath.Join(wantHelperRoot, codexBundledContainmentHelper)
	if filepath.Dir(sealedCommand.Path) != sealRoot || helperRoot != wantHelperRoot || sealedHelper.Path != wantHelperPath {
		t.Fatalf("sealed Codex runtime layout is not bundled: command=%q root=%q helper=%q helper_root=%q", sealedCommand.Path, sealRoot, sealedHelper.Path, helperRoot)
	}
	if sealedCommand.SHA256 != command.SHA256 || sealedCommand.Bytes != command.Bytes || sealedHelper.SHA256 != helper.SHA256 || sealedHelper.Bytes != helper.Bytes {
		t.Fatalf("sealed Codex runtime identities drifted: command=%+v helper=%+v", sealedCommand, sealedHelper)
	}
	attestation := &codexProviderIdentityAttestation{
		Command: command, ContainmentHelper: helper,
		SealedCommand: sealedCommand, SealRoot: sealRoot,
		SealedContainmentHelper: sealedHelper, ContainmentHelperRoot: helperRoot,
	}
	if err := attestation.revalidate(context.Background()); err != nil {
		t.Fatalf("fresh bundled runtime did not revalidate: %v", err)
	}
	environment, err := bindCodexRuntimePath("linux", []string{"PATH=ambient-untrusted", "LANG=C"}, attestation, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(environment, "\n") != "LANG=C\nPATH="+wantHelperRoot {
		t.Fatalf("bundled helper did not become the only private PATH entry: %q", environment)
	}
	if err := os.Chmod(sealedHelper.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sealedHelper.Path, []byte("changed-bwrap-test-bytes!"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := attestation.revalidate(context.Background()); err == nil || !strings.Contains(err.Error(), "sealed Codex containment helper changed") {
		t.Fatalf("bundled helper drift was not rejected: %v", err)
	}
	if err := cleanupCodexProviderRuntimeSeal(sealRoot, sealedCommand.Path, sealedHelper.Path); err != nil {
		t.Fatal(err)
	}
	cleaned = true
	if _, err := os.Lstat(sealRoot); !os.IsNotExist(err) {
		t.Fatalf("sealed runtime root survived cleanup: %v", err)
	}
}

func TestCodexLinuxContainmentAttestationPrefersPackagedHelperOverSystemPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the packaged Codex helper preference is Linux-only")
	}
	packageRoot := t.TempDir()
	commandRoot := filepath.Join(packageRoot, "bin")
	if err := os.Mkdir(commandRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	commandPath := filepath.Join(commandRoot, "codex")
	if err := os.WriteFile(commandPath, []byte("ordinary-codex-test-bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	resourcesRoot := filepath.Join(packageRoot, codexBundledResourcesDirectory)
	if err := os.Mkdir(resourcesRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	bundledPath := filepath.Join(resourcesRoot, codexBundledContainmentHelper)
	if err := os.WriteFile(bundledPath, []byte("packaged-bwrap-test-bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	systemRoot := t.TempDir()
	systemPath := filepath.Join(systemRoot, "bwrap")
	if err := os.WriteFile(systemPath, []byte("different-system-bwrap-bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", systemRoot)
	command, err := inspectCodexProviderFile(commandPath)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := attestCodexLinuxContainmentHelper(command)
	if err != nil {
		t.Fatal(err)
	}
	bundled, err := inspectCodexProviderFile(bundledPath)
	if err != nil {
		t.Fatal(err)
	}
	if helper != bundled || helper.Path == systemPath {
		t.Fatalf("Codex containment attested the wrong helper: got=%+v bundled=%+v system=%q", helper, bundled, systemPath)
	}
}

func TestCodexLinuxCapabilityDropHelperIsSealedRevalidatedAndCleaned(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the capability-drop helper is Linux-only")
	}
	source := filepath.Join(t.TempDir(), codexCapabilityDropHelper)
	if err := os.WriteFile(source, []byte("ordinary-setpriv-test-bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := inspectCodexProviderFile(source)
	if err != nil {
		t.Fatal(err)
	}
	sealed, root, err := sealCodexCapabilityDropHelper(identity)
	if err != nil {
		t.Fatal(err)
	}
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			_ = cleanupCodexCapabilityDropHelperSeal(root, sealed.Path)
		}
	})
	if filepath.Base(sealed.Path) != codexCapabilityDropHelper || sealed.SHA256 != identity.SHA256 || sealed.Bytes != identity.Bytes {
		t.Fatalf("sealed capability-drop identity drifted: source=%+v sealed=%+v", identity, sealed)
	}
	command, err := inspectCodexProviderFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	attestation := &codexProviderIdentityAttestation{
		Command:              command,
		CapabilityDropHelper: identity, SealedCapabilityDropHelper: sealed, CapabilityDropHelperRoot: root,
	}
	if err := attestation.revalidateSource(context.Background()); err != nil {
		t.Fatalf("fresh capability-drop helper did not revalidate: %v", err)
	}
	if err := os.WriteFile(source, []byte("changed-setpriv-test-bytes!"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := attestation.revalidateSource(context.Background()); err == nil || !strings.Contains(err.Error(), "capability-drop helper changed") {
		t.Fatalf("capability-drop source drift was not rejected: %v", err)
	}
	if err := cleanupCodexCapabilityDropHelperSeal(root, sealed.Path); err != nil {
		t.Fatal(err)
	}
	cleaned = true
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("capability-drop seal root survived cleanup: %v", err)
	}
}

func TestCodexProviderOutputOverflowCancelsContainedProcess(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TEST_CODEX_OUTPUT_OVERFLOW", "1")
	result, err := provider.Run(context.Background(), "", "Overflow the bounded transport.")
	if err == nil || !providerRunWasLaunched(err) || result.Output != "" || !strings.Contains(err.Error(), "hard cap") {
		t.Fatalf("overflow did not cancel fail-closed: result=%+v err=%v", result, err)
	}
}

func TestCodexProviderAllowsPromptMirroredOnStderrWithinBoundedTransport(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	t.Setenv("HIVE_TEST_CODEX_ECHO_PROMPT_STDERR", "1")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompt := "recurrence_key = \"rk-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"\n" +
		strings.Repeat("bounded governed prompt\n", 4096)
	if rule, confidence := classifyRepairSourceSecret([]byte(prompt)); confidence == repairSourceSecretNone || rule != "credential-assignment-entropy-v2" {
		t.Fatalf("regression prompt no longer exercises the controller-metadata false positive: rule=%q confidence=%d", rule, confidence)
	}
	result, err := provider.Run(context.Background(), "", prompt)
	if err != nil || result.Output != "DENIED" {
		t.Fatalf("prompt-sized stderr transport failed: result=%+v err=%v", result, err)
	}
}

func TestCodexPromptEchoExemptionRetainsFailClosedOutputScanning(t *testing.T) {
	prompt := "recurrence_key = \"rk-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"\n"
	if _, confidence := classifyRepairSourceSecret([]byte(prompt)); confidence == repairSourceSecretNone {
		t.Fatal("regression prompt must exercise output-secret classification")
	}
	nearPrompt := strings.Replace(prompt, "0123456789abcdef", "1123456789abcdef", 1)
	unsafeResidual := "const access = \"AK" + "IA" + "ABCDEFGHIJKLMNOP\""
	humanFrame := func(value string) string {
		return codexHumanBannerPrefix + "workdir: /tmp/hive-provider-test\nmodel: reviewed-test-model\n" + codexHumanPromptMarker + value + "\n"
	}
	cases := []struct {
		name       string
		stderr     string
		structured bool
		wantUnsafe bool
	}{
		{name: "one exact initial human echo", stderr: humanFrame(prompt) + "DENIED", wantUnsafe: false},
		{name: "bare exact prompt remains", stderr: "progress\n" + prompt + "DENIED", wantUnsafe: true},
		{name: "model framed exact prompt remains", stderr: "codex\n" + prompt + "DENIED", wantUnsafe: true},
		{name: "second exact echo remains", stderr: humanFrame(prompt) + prompt + "DENIED", wantUnsafe: true},
		{name: "near match remains", stderr: humanFrame(nearPrompt) + "DENIED", wantUnsafe: true},
		{name: "missing renderer newline remains", stderr: strings.TrimSuffix(humanFrame(prompt), "\n"), wantUnsafe: true},
		{name: "unsafe residual stderr remains", stderr: humanFrame(prompt) + unsafeResidual, wantUnsafe: true},
		{name: "structured output has no exemption", stderr: prompt, structured: true, wantUnsafe: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			output := codexOutputForSecretClassification([]byte("DENIED"), []byte(test.stderr), prompt, test.structured)
			_, confidence := classifyRepairSourceSecret(output)
			if gotUnsafe := confidence != repairSourceSecretNone; gotUnsafe != test.wantUnsafe {
				t.Fatalf("unexpected classification: unsafe=%t confidence=%d", gotUnsafe, confidence)
			}
		})
	}
}

func TestCodexProviderRejectsUnsafeStdoutAfterPromptMirror(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	t.Setenv("HIVE_TEST_CODEX_ECHO_PROMPT_STDERR", "1")
	secret := "AK" + "IA" + "ABCDEFGHIJKLMNOP"
	t.Setenv("HIVE_TEST_CODEX_MODEL_OUTPUT", "const access = \""+secret+"\"")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompt := "recurrence_key = \"rk-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"\n"
	result, err := provider.Run(context.Background(), "", prompt)
	if err == nil || !providerRunWasLaunched(err) || result.Output != "" || !strings.Contains(err.Error(), "aws-access-key-id-v1") || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe stdout escaped prompt-echo scanning without redaction: result=%+v err=%v", result, err)
	}
}

func TestCodexProviderStderrBeyondPromptAwareLimitCancelsContainedProcess(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	t.Setenv("HIVE_TEST_CODEX_STDERR_OVERFLOW", "1")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompt := strings.Repeat("bounded governed prompt\n", 4096)
	result, err := provider.Run(context.Background(), "", prompt)
	if err == nil || !providerRunWasLaunched(err) || result.Output != "" || !strings.Contains(err.Error(), "hard cap") {
		t.Fatalf("stderr overflow did not cancel fail-closed: result=%+v err=%v", result, err)
	}
}

func TestCodexProviderRejectsAndCleansUnexpectedNeutralCWDFile(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TEST_CODEX_WRITE_EXTRA_FILE", "1")
	result, err := provider.Run(context.Background(), "", "Attempt an unexpected file.")
	if err == nil || !providerRunWasLaunched(err) || result.Output != "" || !strings.Contains(err.Error(), "unexpected neutral-cwd files") {
		t.Fatalf("unexpected neutral cwd file was not rejected: result=%+v err=%v", result, err)
	}
}

func TestCodexProviderTimeoutReapsContainedDescendant(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "descendant-alive.log")
	t.Setenv("HIVE_TEST_CODEX_DESCENDANT_MARKER", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, err := provider.Run(ctx, "", "Wait until the controller deadline.")
	if err == nil || !providerRunWasLaunched(err) || result.Output != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout did not remain a launched contained failure: result=%+v err=%v", result, err)
	}
	first, statErr := os.Stat(marker)
	if statErr != nil || first.Size() == 0 {
		t.Fatalf("descendant probe never ran: info=%v err=%v", first, statErr)
	}
	time.Sleep(250 * time.Millisecond)
	second, statErr := os.Stat(marker)
	if statErr != nil || second.Size() != first.Size() {
		t.Fatalf("descendant survived process-tree cleanup: before=%v after=%v err=%v", first.Size(), second, statErr)
	}
}

func TestCodexProviderReapsDescendantRetainingInheritedOutputHandles(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "inherited-output-descendant.log")
	t.Setenv("HIVE_TEST_CODEX_DESCENDANT_MARKER", marker)
	t.Setenv("HIVE_TEST_CODEX_INHERITED_OUTPUT_DESCENDANT", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	started := time.Now()
	result, err := provider.Run(ctx, "", "Return after spawning the inherited-handle adversary.")
	if err != nil || result.Output != "DENIED" {
		t.Fatalf("inherited-handle descendant blocked or corrupted output: result=%+v err=%v", result, err)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("inherited output handles delayed bounded cleanup for %s", elapsed)
	}
	first, statErr := os.Stat(marker)
	if statErr != nil || first.Size() == 0 {
		t.Fatalf("inherited-handle descendant never ran: info=%v err=%v", first, statErr)
	}
	time.Sleep(250 * time.Millisecond)
	second, statErr := os.Stat(marker)
	if statErr != nil || second.Size() != first.Size() {
		t.Fatalf("inherited-handle descendant survived job/group cleanup: before=%v after=%v err=%v", first.Size(), second, statErr)
	}
}

func TestCodexProviderRejectsLinkedAuthenticationSourceBeforeInvocation(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	target := filepath.Join(t.TempDir(), "real-auth.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.Symlink(target, filepath.Join(home, "auth.json")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	provider := CodexProvider{Command: os.Args[0], CodexHome: home}
	if err := provider.Health(context.Background()); err == nil || !strings.Contains(strings.ToLower(err.Error()), "link") {
		t.Fatalf("linked authentication source was accepted: %v", err)
	}
	result, err := provider.Run(context.Background(), "", "must not launch")
	if err == nil || providerRunWasLaunched(err) || result.Output != "" {
		t.Fatalf("failed linked-auth Health left runnable authorization: result=%+v err=%v", result, err)
	}
}
