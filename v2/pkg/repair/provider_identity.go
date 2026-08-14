package repair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const (
	maxCodexProviderExecutableBytes = 512 << 20
	codexBundledResourcesDirectory  = "codex-resources"
	codexBundledContainmentHelper   = "bwrap"
)

type codexProviderFileIdentity struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type codexProviderIdentityAttestation struct {
	ConfigKey               string
	IdentitySHA256          string
	Command                 codexProviderFileIdentity
	Prefix                  []string
	PrefixArtifacts         []codexProviderFileIdentity
	Auth                    codexProviderFileIdentity
	ContainmentHelper       codexProviderFileIdentity
	SealedCommand           codexProviderFileIdentity
	SealRoot                string
	SealedContainmentHelper codexProviderFileIdentity
	ContainmentHelperRoot   string
}

var codexProviderAttestations = struct {
	sync.RWMutex
	values map[string][]*codexProviderIdentityAttestation
}{values: make(map[string][]*codexProviderIdentityAttestation)}

func codexProviderConfigurationKey(provider CodexProvider) (string, error) {
	encoded, err := json.Marshal(struct {
		Command   string   `json:"command"`
		Prefix    []string `json:"prefix"`
		CodexHome string   `json:"codex_home"`
	}{Command: provider.Command, Prefix: append([]string(nil), provider.Prefix...), CodexHome: provider.CodexHome})
	if err != nil {
		return "", fmt.Errorf("encode Codex provider configuration: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func forgetCodexProviderAttestation(provider CodexProvider) {
	key, err := codexProviderConfigurationKey(provider)
	if err != nil {
		return
	}
	codexProviderAttestations.Lock()
	delete(codexProviderAttestations.values, key)
	codexProviderAttestations.Unlock()
}

func rememberCodexProviderAttestation(provider CodexProvider, attestation *codexProviderIdentityAttestation) {
	codexProviderAttestations.Lock()
	codexProviderAttestations.values[attestation.ConfigKey] = append(codexProviderAttestations.values[attestation.ConfigKey], attestation)
	codexProviderAttestations.Unlock()
}

func consumeCodexProviderAttestation(provider CodexProvider) (*codexProviderIdentityAttestation, error) {
	key, err := codexProviderConfigurationKey(provider)
	if err != nil {
		return nil, err
	}
	codexProviderAttestations.Lock()
	queue := codexProviderAttestations.values[key]
	var attestation *codexProviderIdentityAttestation
	if len(queue) > 0 {
		attestation = queue[0]
		queue = queue[1:]
		if len(queue) == 0 {
			delete(codexProviderAttestations.values, key)
		} else {
			codexProviderAttestations.values[key] = queue
		}
	}
	codexProviderAttestations.Unlock()
	if attestation == nil {
		return nil, fmt.Errorf("Codex provider has no unconsumed successful Health identity attestation; run Health immediately before each model invocation")
	}
	return attestation, nil
}

func prepareCodexProviderAttestation(provider CodexProvider) (*codexProviderIdentityAttestation, error) {
	key, err := codexProviderConfigurationKey(provider)
	if err != nil {
		return nil, err
	}
	command := strings.TrimSpace(provider.Command)
	if command == "" || strings.ContainsRune(command, '\x00') {
		return nil, fmt.Errorf("Codex provider command is required")
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("resolve Codex provider executable: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute Codex provider executable: %w", err)
	}
	commandIdentity, err := inspectCodexProviderFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("attest Codex provider executable: %w", err)
	}
	if !codexProviderExecutableCanBeSealed(commandIdentity.Path) {
		return nil, fmt.Errorf("Codex provider must be a reviewed native executable that Hive can seal; script and package-runner wrappers (.cmd, .ps1, shebang, npx, npm, and similar launchers) are unsupported; point --provider-command at the native Codex binary")
	}
	containmentHelper, err := attestCodexContainmentHelper(provider.Command, commandIdentity)
	if err != nil {
		return nil, err
	}
	if err := validateCodexProviderPrefix(provider.Prefix); err != nil {
		return nil, err
	}
	authIdentity, err := inspectCodexProviderAuth(provider)
	if err != nil {
		return nil, err
	}

	prefixArtifacts := make([]codexProviderFileIdentity, 0)
	seenArtifacts := make(map[string]bool)
	for _, argument := range provider.Prefix {
		if strings.ContainsRune(argument, '\x00') {
			return nil, fmt.Errorf("Codex provider prefix contains a NUL byte")
		}
		artifactPath := codexProviderPrefixArtifactPath(argument)
		if artifactPath == "" {
			continue
		}
		absolute := filepath.Clean(artifactPath)
		if seenArtifacts[absolute] {
			continue
		}
		info, statErr := os.Lstat(absolute)
		if os.IsNotExist(statErr) {
			// An absolute literal may be a model argument rather than an input
			// artifact. The literal remains bound by ConfigKey.
			continue
		}
		if statErr != nil || info == nil {
			return nil, fmt.Errorf("inspect Codex provider prefix artifact: %w", statErr)
		}
		identity, identityErr := inspectCodexProviderFile(absolute)
		if identityErr != nil {
			return nil, fmt.Errorf("attest Codex provider prefix artifact: %w", identityErr)
		}
		seenArtifacts[absolute] = true
		prefixArtifacts = append(prefixArtifacts, identity)
	}

	identityBytes, err := json.Marshal(struct {
		Command           codexProviderFileIdentity   `json:"command"`
		Prefix            []string                    `json:"prefix"`
		PrefixArtifacts   []codexProviderFileIdentity `json:"prefix_artifacts"`
		Auth              codexProviderFileIdentity   `json:"auth"`
		ContainmentHelper codexProviderFileIdentity   `json:"containment_helper"`
	}{Command: commandIdentity, Prefix: append([]string(nil), provider.Prefix...), PrefixArtifacts: prefixArtifacts, Auth: authIdentity, ContainmentHelper: containmentHelper})
	if err != nil {
		return nil, fmt.Errorf("encode Codex provider identity: %w", err)
	}
	identityDigest := sha256.Sum256(identityBytes)
	identitySHA := hex.EncodeToString(identityDigest[:])
	sealedCommand, sealedHelper, sealRoot, helperRoot, err := sealCodexProviderRuntime(commandIdentity, containmentHelper, identitySHA)
	if err != nil {
		return nil, err
	}
	return &codexProviderIdentityAttestation{
		ConfigKey: key, IdentitySHA256: identitySHA, Command: commandIdentity,
		Prefix: append([]string(nil), provider.Prefix...), PrefixArtifacts: prefixArtifacts,
		Auth: authIdentity, ContainmentHelper: containmentHelper,
		SealedCommand: sealedCommand, SealRoot: sealRoot,
		SealedContainmentHelper: sealedHelper, ContainmentHelperRoot: helperRoot,
	}, nil
}

func validateCodexProviderPrefix(prefix []string) error {
	valueOptions := map[string]bool{
		"--enable": true, "--disable": true, "--model": true, "--color": true,
	}
	flagOptions := map[string]bool{"--no-alt-screen": true}
	expectValue := false
	for _, argument := range prefix {
		if strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("Codex provider prefix contains a NUL byte")
		}
		trimmed := strings.TrimSpace(argument)
		if trimmed == "" {
			return fmt.Errorf("Codex provider prefix contains an empty argument")
		}
		if trimmed == "--%" || strings.Trim(trimmed, "-") == "" {
			return fmt.Errorf("Codex provider prefix contains parser terminator %q; operator arguments may not terminate Hive's mandatory security options", safeExcerpt(trimmed))
		}
		if expectValue {
			if codexProviderPrefixLooksLikePayload(trimmed) {
				return fmt.Errorf("Codex provider prefix contains a script, package, or path payload; point --provider-command at the reviewed native Codex binary and pass only native Codex options")
			}
			expectValue = false
			continue
		}
		option, value, hasValue := strings.Cut(trimmed, "=")
		if !strings.HasPrefix(option, "-") {
			return fmt.Errorf("Codex provider prefix contains positional payload %q; script and package-runner wrappers are unsupported; point --provider-command at the reviewed native Codex binary", safeExcerpt(option))
		}
		if !valueOptions[option] && !flagOptions[option] {
			return fmt.Errorf("Codex provider prefix option %q is not in Hive's reviewed native-option allowlist; script, interpreter, and package payload options are unsupported; point --provider-command at the reviewed native Codex binary", safeExcerpt(option))
		}
		if hasValue && codexProviderPrefixLooksLikePayload(value) {
			return fmt.Errorf("Codex provider prefix contains a script, package, or path payload; point --provider-command at the reviewed native Codex binary and pass only native Codex options")
		}
		if !hasValue && valueOptions[option] {
			expectValue = true
		}
	}
	if expectValue {
		return fmt.Errorf("Codex provider prefix option is missing its value")
	}
	return nil
}

func codexProviderPrefixLooksLikePayload(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "@") || filepath.IsAbs(value) {
		return true
	}
	lower := strings.ToLower(value)
	for _, suffix := range []string{".cmd", ".bat", ".ps1", ".sh", ".js", ".mjs", ".cjs", ".ts", ".py"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func codexProviderPrefixArtifactPath(argument string) string {
	candidates := []string{argument}
	if _, value, found := strings.Cut(argument, "="); found {
		candidates = append(candidates, value)
	}
	if strings.HasPrefix(argument, "@") {
		candidates = append(candidates, strings.TrimPrefix(argument, "@"))
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if filepath.IsAbs(candidate) {
			return candidate
		}
	}
	return ""
}

func attestCodexContainmentHelper(command string, commandIdentity codexProviderFileIdentity) (codexProviderFileIdentity, error) {
	if runtime.GOOS != "linux" {
		return codexProviderFileIdentity{}, nil
	}
	// The native Go test provider models Codex's sandbox command itself. Bind
	// its exact executable as the test-only helper so production remains strict
	// without making unit tests depend on a host-installed bubblewrap package.
	if codexProviderIsTestExecutable(command) {
		return commandIdentity, nil
	}
	return attestCodexLinuxContainmentHelper(commandIdentity)
}

func attestCodexLinuxContainmentHelper(commandIdentity codexProviderFileIdentity) (codexProviderFileIdentity, error) {
	commandRoot := filepath.Dir(commandIdentity.Path)
	bundledCandidates := []string{filepath.Join(commandRoot, codexBundledResourcesDirectory, codexBundledContainmentHelper)}
	if filepath.Base(commandRoot) == "bin" {
		bundledCandidates = append(bundledCandidates, filepath.Join(filepath.Dir(commandRoot), codexBundledResourcesDirectory, codexBundledContainmentHelper))
	}
	for _, bundledPath := range bundledCandidates {
		if _, err := os.Lstat(bundledPath); err == nil {
			identity, inspectErr := inspectCodexProviderFile(bundledPath)
			if inspectErr != nil {
				return codexProviderFileIdentity{}, fmt.Errorf("attest packaged Codex Linux bwrap executable: %w", inspectErr)
			}
			if !codexProviderExecutableCanBeSealed(identity.Path) {
				return codexProviderFileIdentity{}, errors.New("packaged Codex Linux bwrap helper must be a reviewed native executable")
			}
			return identity, nil
		} else if !os.IsNotExist(err) {
			return codexProviderFileIdentity{}, fmt.Errorf("inspect packaged Codex Linux bwrap executable: %w", err)
		}
	}
	resolved, err := exec.LookPath("bwrap")
	if err != nil {
		return codexProviderFileIdentity{}, fmt.Errorf("Codex Linux containment requires a bounded ordinary bwrap executable: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return codexProviderFileIdentity{}, fmt.Errorf("resolve absolute bwrap executable: %w", err)
	}
	identity, err := inspectCodexProviderFile(resolved)
	if err != nil {
		return codexProviderFileIdentity{}, fmt.Errorf("attest Codex Linux bwrap executable: %w", err)
	}
	if !codexProviderExecutableCanBeSealed(identity.Path) {
		return codexProviderFileIdentity{}, errors.New("Codex Linux bwrap helper must be a reviewed native executable")
	}
	return identity, nil
}

func (attestation *codexProviderIdentityAttestation) revalidate(ctx context.Context) error {
	if err := attestation.revalidateSource(ctx); err != nil {
		return err
	}
	sealed, err := inspectCodexProviderFile(attestation.SealedCommand.Path)
	if err != nil || sealed != attestation.SealedCommand || sealed.SHA256 != attestation.Command.SHA256 || sealed.Bytes != attestation.Command.Bytes {
		return fmt.Errorf("Hive-sealed Codex provider executable changed after Health")
	}
	if attestation.ContainmentHelper.Path != "" {
		sealedHelper, inspectErr := inspectCodexProviderFile(attestation.SealedContainmentHelper.Path)
		if inspectErr != nil || sealedHelper != attestation.SealedContainmentHelper || sealedHelper.SHA256 != attestation.ContainmentHelper.SHA256 || sealedHelper.Bytes != attestation.ContainmentHelper.Bytes {
			return fmt.Errorf("Hive-sealed Codex containment helper changed after Health")
		}
	}
	return nil
}

func (attestation *codexProviderIdentityAttestation) revalidateSource(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	actualCommand, err := inspectCodexProviderFile(attestation.Command.Path)
	if err != nil || actualCommand != attestation.Command {
		return fmt.Errorf("Codex provider executable changed after Health")
	}
	for _, expected := range attestation.PrefixArtifacts {
		actual, inspectErr := inspectCodexProviderFile(expected.Path)
		if inspectErr != nil || actual != expected {
			return fmt.Errorf("Codex provider prefix artifact changed after Health")
		}
	}
	if attestation.ContainmentHelper.Path != "" {
		actual, inspectErr := inspectCodexProviderFile(attestation.ContainmentHelper.Path)
		if inspectErr != nil || actual != attestation.ContainmentHelper {
			return fmt.Errorf("Codex containment helper changed after Health")
		}
	}
	if attestation.Auth.Path != "" {
		actual, inspectErr := inspectCodexProviderBoundedFile(attestation.Auth.Path, 2<<20, true)
		if inspectErr != nil || actual != attestation.Auth {
			return fmt.Errorf("Codex authentication material changed after Health")
		}
	}
	return nil
}

func (attestation *codexProviderIdentityAttestation) authorizationCopy() *codexProviderIdentityAttestation {
	copy := *attestation
	copy.Prefix = append([]string(nil), attestation.Prefix...)
	copy.PrefixArtifacts = append([]codexProviderFileIdentity(nil), attestation.PrefixArtifacts...)
	copy.SealedCommand = codexProviderFileIdentity{}
	copy.SealRoot = ""
	copy.SealedContainmentHelper = codexProviderFileIdentity{}
	copy.ContainmentHelperRoot = ""
	return &copy
}

func (attestation *codexProviderIdentityAttestation) sealForRun() (*codexProviderIdentityAttestation, error) {
	sealedCommand, sealedHelper, sealRoot, helperRoot, err := sealCodexProviderRuntime(attestation.Command, attestation.ContainmentHelper, attestation.IdentitySHA256)
	if err != nil {
		return nil, err
	}
	copy := attestation.authorizationCopy()
	copy.SealedCommand = sealedCommand
	copy.SealRoot = sealRoot
	copy.SealedContainmentHelper = sealedHelper
	copy.ContainmentHelperRoot = helperRoot
	return copy, nil
}

func (attestation *codexProviderIdentityAttestation) cleanupSeal() error {
	if attestation == nil {
		return nil
	}
	var result error
	if attestation.SealRoot != "" || attestation.SealedCommand.Path != "" {
		result = errors.Join(result, cleanupCodexProviderRuntimeSeal(attestation.SealRoot, attestation.SealedCommand.Path, attestation.SealedContainmentHelper.Path))
	} else if attestation.ContainmentHelperRoot != "" || attestation.SealedContainmentHelper.Path != "" {
		result = errors.Join(result, cleanupCodexContainmentHelperSeal(attestation.ContainmentHelperRoot, attestation.SealedContainmentHelper.Path))
	}
	return result
}

func (attestation *codexProviderIdentityAttestation) provider() CodexProvider {
	codexHome := ""
	if attestation.Auth.Path != "" {
		codexHome = filepath.Dir(attestation.Auth.Path)
	}
	return CodexProvider{Command: attestation.SealedCommand.Path, Prefix: append([]string(nil), attestation.Prefix...), CodexHome: codexHome}
}

func inspectCodexProviderAuth(provider CodexProvider) (codexProviderFileIdentity, error) {
	home := strings.TrimSpace(provider.CodexHome)
	if home == "" {
		// Never let an ambient CODEX_HOME redirect the credential source. An
		// operator may inject an explicit bounded home; otherwise use the OS
		// account's conventional Codex home and copy only its attested auth.json.
		userHome, err := os.UserHomeDir()
		if err != nil {
			return codexProviderFileIdentity{}, fmt.Errorf("resolve Codex authentication home: %w", err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	if !filepath.IsAbs(home) || home != filepath.Clean(home) {
		return codexProviderFileIdentity{}, fmt.Errorf("Codex home must be one absolute clean directory")
	}
	authPath := filepath.Join(home, "auth.json")
	identity, err := inspectCodexProviderBoundedFile(authPath, 2<<20, true)
	if errors.Is(err, os.ErrNotExist) {
		return codexProviderFileIdentity{}, nil
	}
	if err != nil {
		return codexProviderFileIdentity{}, fmt.Errorf("attest Codex auth.json: %w", err)
	}
	return identity, nil
}

func inspectCodexProviderBoundedFile(candidate string, limit int64, requireJSON bool) (codexProviderFileIdentity, error) {
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return codexProviderFileIdentity{}, err
	}
	absolute = filepath.Clean(absolute)
	if err := rejectCodexProviderLinkedPath(absolute); err != nil {
		return codexProviderFileIdentity{}, err
	}
	before, err := os.Lstat(absolute)
	if err != nil {
		return codexProviderFileIdentity{}, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() <= 1 || before.Size() > limit {
		return codexProviderFileIdentity{}, fmt.Errorf("path is not a bounded ordinary file")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return codexProviderFileIdentity{}, err
	}
	hasher := sha256.New()
	var captured bytes.Buffer
	writer := io.Writer(hasher)
	if requireJSON {
		writer = io.MultiWriter(hasher, &captured)
	}
	written, copyErr := io.Copy(writer, io.LimitReader(file, limit+1))
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := os.Lstat(absolute)
	if copyErr != nil || statErr != nil || closeErr != nil || lstatErr != nil || written != before.Size() || written > limit || !os.SameFile(before, opened) || !os.SameFile(before, after) || after.Mode()&os.ModeSymlink != 0 {
		return codexProviderFileIdentity{}, fmt.Errorf("bounded ordinary file changed while hashing")
	}
	if requireJSON && !json.Valid(captured.Bytes()) {
		return codexProviderFileIdentity{}, fmt.Errorf("Codex auth.json is not valid JSON")
	}
	return codexProviderFileIdentity{Path: absolute, SHA256: hex.EncodeToString(hasher.Sum(nil)), Bytes: written}, nil
}

func inspectCodexProviderFile(candidate string) (codexProviderFileIdentity, error) {
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return codexProviderFileIdentity{}, err
	}
	absolute = filepath.Clean(absolute)
	if err := rejectCodexProviderLinkedPath(absolute); err != nil {
		return codexProviderFileIdentity{}, err
	}
	before, err := os.Lstat(absolute)
	if err != nil || before == nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() <= 0 || before.Size() > maxCodexProviderExecutableBytes {
		return codexProviderFileIdentity{}, fmt.Errorf("path is not a bounded ordinary file")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return codexProviderFileIdentity{}, err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(hasher, io.LimitReader(file, maxCodexProviderExecutableBytes+1))
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := os.Lstat(absolute)
	if copyErr != nil || statErr != nil || closeErr != nil || lstatErr != nil {
		return codexProviderFileIdentity{}, fmt.Errorf("read stable ordinary file")
	}
	if written != before.Size() || written > maxCodexProviderExecutableBytes || !os.SameFile(before, opened) || !os.SameFile(before, after) || after.Mode()&os.ModeSymlink != 0 {
		return codexProviderFileIdentity{}, fmt.Errorf("ordinary file identity changed while hashing")
	}
	return codexProviderFileIdentity{Path: absolute, SHA256: hex.EncodeToString(hasher.Sum(nil)), Bytes: written}, nil
}

func rejectCodexProviderLinkedPath(absolute string) error {
	volume := filepath.VolumeName(absolute)
	remainder := strings.TrimPrefix(absolute, volume)
	remainder = strings.TrimLeft(remainder, string(filepath.Separator))
	cursor := volume + string(filepath.Separator)
	if volume == "" {
		cursor = string(filepath.Separator)
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		cursor = filepath.Join(cursor, component)
		info, err := os.Lstat(cursor)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path resolves through a symbolic link")
		}
		reparse, err := codexProviderPathIsReparsePoint(cursor)
		if err != nil {
			return err
		}
		if reparse {
			return fmt.Errorf("path resolves through a reparse point")
		}
	}
	return nil
}

func codexProviderExecutableCanBeSealed(path string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Ext(path), ".exe")
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, 2)
	_, _ = io.ReadFull(file, header)
	return string(header) != "#!"
}

func sealCodexProviderExecutable(identity codexProviderFileIdentity, identitySHA string) (string, string, error) {
	return sealCodexBoundedExecutable(identity, "hive-codex-provider-seals-", identitySHA+filepath.Ext(identity.Path))
}

// sealCodexProviderRuntime places the attested containment helper in the
// resource layout expected by the packaged Codex binary. The private runtime
// intentionally does not expose the ambient system PATH, so Codex must find
// bwrap beside its own Hive-sealed executable as codex-resources/bwrap.
func sealCodexProviderRuntime(command, helper codexProviderFileIdentity, identitySHA string) (codexProviderFileIdentity, codexProviderFileIdentity, string, string, error) {
	commandPath, sealRoot, err := sealCodexProviderExecutable(command, identitySHA)
	if err != nil {
		return codexProviderFileIdentity{}, codexProviderFileIdentity{}, "", "", err
	}
	sealedCommand, err := inspectCodexProviderFile(commandPath)
	if err != nil || sealedCommand.SHA256 != command.SHA256 || sealedCommand.Bytes != command.Bytes {
		_ = cleanupCodexProviderSeal(sealRoot, commandPath)
		return codexProviderFileIdentity{}, codexProviderFileIdentity{}, "", "", errors.New("verify sealed Codex provider executable")
	}
	if helper.Path == "" {
		return sealedCommand, codexProviderFileIdentity{}, sealRoot, "", nil
	}
	resourcesRoot := filepath.Join(sealRoot, codexBundledResourcesDirectory)
	if err := os.Mkdir(resourcesRoot, 0o700); err != nil {
		_ = cleanupCodexProviderSeal(sealRoot, commandPath)
		return codexProviderFileIdentity{}, codexProviderFileIdentity{}, "", "", fmt.Errorf("create sealed Codex resource directory: %w", err)
	}
	if err := os.Chmod(resourcesRoot, 0o700); err != nil {
		_ = os.Remove(resourcesRoot)
		_ = cleanupCodexProviderSeal(sealRoot, commandPath)
		return codexProviderFileIdentity{}, codexProviderFileIdentity{}, "", "", fmt.Errorf("protect sealed Codex resource directory: %w", err)
	}
	helperPath, err := sealCodexBoundedExecutableInto(helper, resourcesRoot, codexBundledContainmentHelper)
	if err != nil {
		_ = os.Remove(resourcesRoot)
		_ = cleanupCodexProviderSeal(sealRoot, commandPath)
		return codexProviderFileIdentity{}, codexProviderFileIdentity{}, "", "", fmt.Errorf("seal Codex bundled containment helper: %w", err)
	}
	sealedHelper, err := inspectCodexProviderFile(helperPath)
	if err != nil || sealedHelper.SHA256 != helper.SHA256 || sealedHelper.Bytes != helper.Bytes {
		_ = cleanupCodexProviderRuntimeSeal(sealRoot, commandPath, helperPath)
		return codexProviderFileIdentity{}, codexProviderFileIdentity{}, "", "", errors.New("verify sealed Codex bundled containment helper")
	}
	return sealedCommand, sealedHelper, sealRoot, resourcesRoot, nil
}

func sealCodexContainmentHelper(identity codexProviderFileIdentity) (codexProviderFileIdentity, string, error) {
	if identity.Path == "" {
		return codexProviderFileIdentity{}, "", nil
	}
	path, root, err := sealCodexBoundedExecutable(identity, "hive-codex-helper-seals-", "bwrap")
	if err != nil {
		return codexProviderFileIdentity{}, "", err
	}
	sealed, err := inspectCodexProviderFile(path)
	if err != nil || sealed.SHA256 != identity.SHA256 || sealed.Bytes != identity.Bytes {
		_ = cleanupCodexContainmentHelperSeal(root, path)
		return codexProviderFileIdentity{}, "", errors.New("verify sealed Codex containment helper")
	}
	return sealed, root, nil
}

func sealCodexBoundedExecutable(identity codexProviderFileIdentity, rootPrefix, destinationName string) (string, string, error) {
	if identity.Path == "" || filepath.Base(destinationName) != destinationName || destinationName == "." || destinationName == "" {
		return "", "", errors.New("invalid Codex executable seal request")
	}
	actual, err := inspectCodexProviderFile(identity.Path)
	if err != nil || actual != identity {
		return "", "", errors.New("attested Codex executable changed before sealing")
	}
	sealRoot, err := os.MkdirTemp("", rootPrefix)
	if err != nil {
		return "", "", fmt.Errorf("create Hive-owned Codex executable seal directory: %w", err)
	}
	if err := os.Chmod(sealRoot, 0o700); err != nil {
		_ = os.Remove(sealRoot)
		return "", "", fmt.Errorf("protect Hive-owned Codex executable seal directory: %w", err)
	}
	destination, err := sealCodexBoundedExecutableInto(identity, sealRoot, destinationName)
	if err != nil {
		_ = os.Remove(sealRoot)
		return "", "", err
	}
	return destination, sealRoot, nil
}

func sealCodexBoundedExecutableInto(identity codexProviderFileIdentity, destinationRoot, destinationName string) (string, error) {
	if identity.Path == "" || !filepath.IsAbs(destinationRoot) || filepath.Base(destinationName) != destinationName || destinationName == "." || destinationName == "" {
		return "", errors.New("invalid Codex executable seal destination")
	}
	rootInfo, err := os.Lstat(destinationRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Codex executable seal destination is not an ordinary directory")
	}
	rootReparse, reparseErr := codexProviderPathIsReparsePoint(destinationRoot)
	if reparseErr != nil || rootReparse {
		return "", errors.New("Codex executable seal destination is linked")
	}
	actual, err := inspectCodexProviderFile(identity.Path)
	if err != nil || actual != identity {
		return "", errors.New("attested Codex executable changed before sealing")
	}
	destination := filepath.Join(destinationRoot, destinationName)
	source, err := os.Open(identity.Path)
	if err != nil {
		return "", fmt.Errorf("open attested Codex executable: %w", err)
	}
	defer source.Close()
	temporary, err := os.CreateTemp(destinationRoot, "seal-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create Codex executable seal: %w", err)
	}
	if err := temporary.Chmod(0o500); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporary.Name())
		return "", fmt.Errorf("protect temporary Codex executable seal: %w", err)
	}
	written, copyErr := io.Copy(temporary, io.LimitReader(source, maxCodexProviderExecutableBytes+1))
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || written != identity.Bytes {
		_ = os.Remove(temporary.Name())
		return "", errors.New("write Codex executable seal")
	}
	if err := os.Rename(temporary.Name(), destination); err != nil {
		_ = os.Remove(temporary.Name())
		return "", fmt.Errorf("activate Codex executable seal: %w", err)
	}
	if err := os.Chmod(destination, 0o500); err != nil {
		_ = os.Remove(destination)
		return "", fmt.Errorf("protect Codex executable seal: %w", err)
	}
	return destination, nil
}

func cleanupCodexProviderSeal(root, executable string) error {
	return cleanupCodexSealedExecutable(root, executable, "hive-codex-provider-seals-")
}

func cleanupCodexProviderRuntimeSeal(root, executable, helper string) error {
	if helper == "" {
		return cleanupCodexProviderSeal(root, executable)
	}
	root, executable, helper = filepath.Clean(root), filepath.Clean(executable), filepath.Clean(helper)
	resourcesRoot := filepath.Join(root, codexBundledResourcesDirectory)
	if root == "" || executable == "" || !filepath.IsAbs(root) || filepath.Dir(executable) != root ||
		helper != filepath.Join(resourcesRoot, codexBundledContainmentHelper) || !strings.HasPrefix(filepath.Base(root), "hive-codex-provider-seals-") {
		return errors.New("refuse unsafe Codex provider runtime seal cleanup")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	rootReparse, rootReparseErr := codexProviderPathIsReparsePoint(root)
	resourcesInfo, resourcesErr := os.Lstat(resourcesRoot)
	resourcesReparse, resourcesReparseErr := codexProviderPathIsReparsePoint(resourcesRoot)
	if rootReparseErr != nil || resourcesErr != nil || resourcesReparseErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootReparse ||
		!resourcesInfo.IsDir() || resourcesInfo.Mode()&os.ModeSymlink != 0 || resourcesReparse {
		return errors.New("refuse linked or replaced Codex provider runtime seal cleanup")
	}
	rootEntries, rootReadErr := os.ReadDir(root)
	resourceEntries, resourceReadErr := os.ReadDir(resourcesRoot)
	if rootReadErr != nil || resourceReadErr != nil || len(rootEntries) != 2 || len(resourceEntries) != 1 {
		return errors.New("refuse Codex provider runtime seal cleanup with unexpected inventory")
	}
	wantCommand, wantResources := false, false
	for _, entry := range rootEntries {
		switch entry.Name() {
		case filepath.Base(executable):
			wantCommand = !entry.IsDir() && entry.Type()&os.ModeSymlink == 0
		case codexBundledResourcesDirectory:
			wantResources = entry.IsDir() && entry.Type()&os.ModeSymlink == 0
		}
	}
	if !wantCommand || !wantResources || resourceEntries[0].Name() != codexBundledContainmentHelper || resourceEntries[0].IsDir() || resourceEntries[0].Type()&os.ModeSymlink != 0 {
		return errors.New("refuse Codex provider runtime seal cleanup with unexpected inventory")
	}
	if _, err := inspectCodexProviderFile(executable); err != nil {
		return errors.New("refuse Codex provider runtime seal cleanup for non-ordinary executable")
	}
	if _, err := inspectCodexProviderFile(helper); err != nil {
		return errors.New("refuse Codex provider runtime seal cleanup for non-ordinary containment helper")
	}
	if err := os.Remove(helper); err != nil {
		return err
	}
	if err := os.Remove(resourcesRoot); err != nil {
		return err
	}
	if err := os.Remove(executable); err != nil {
		return err
	}
	return os.Remove(root)
}

func cleanupCodexContainmentHelperSeal(root, executable string) error {
	if root == "" && executable == "" {
		return nil
	}
	return cleanupCodexSealedExecutable(root, executable, "hive-codex-helper-seals-")
}

func cleanupCodexSealedExecutable(root, executable, rootPrefix string) error {
	root, executable = filepath.Clean(root), filepath.Clean(executable)
	if root == "" || executable == "" || filepath.Dir(executable) != root || !strings.HasPrefix(filepath.Base(root), rootPrefix) {
		return fmt.Errorf("refuse unsafe Codex executable seal cleanup")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	rootReparse, reparseErr := codexProviderPathIsReparsePoint(root)
	if reparseErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootReparse {
		return fmt.Errorf("refuse linked or replaced Codex executable seal cleanup")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(executable) || entries[0].Type()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse Codex executable seal cleanup with unexpected inventory")
	}
	if _, err := inspectCodexProviderFile(executable); err != nil {
		return fmt.Errorf("refuse Codex executable seal cleanup for non-ordinary executable")
	}
	if err := os.Remove(executable); err != nil {
		return err
	}
	return os.Remove(root)
}
