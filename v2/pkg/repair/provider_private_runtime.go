package repair

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type codexPrivateRuntime struct {
	root        string
	environment []string
	ownedRoot   bool
}

func prepareCodexPrivateRuntime(attestation *codexProviderIdentityAttestation, command, parent string) (*codexPrivateRuntime, error) {
	if attestation == nil {
		return nil, errors.New("private Codex runtime requires an attestation")
	}
	root := ""
	owned := false
	var err error
	if parent == "" {
		root, err = os.MkdirTemp("", "hive-codex-private-")
		owned = true
	} else {
		parent = filepath.Clean(parent)
		if !filepath.IsAbs(parent) {
			return nil, errors.New("private Codex runtime parent must be absolute")
		}
		root = filepath.Join(parent, "runtime")
		err = os.Mkdir(root, 0o700)
	}
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(root)
		}
	}()
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(root, "codex")
	temporary := filepath.Join(root, "tmp")
	xdgConfig := filepath.Join(root, "xdg-config")
	xdgCache := filepath.Join(root, "xdg-cache")
	xdgData := filepath.Join(root, "xdg-data")
	appData := filepath.Join(root, "appdata")
	localAppData := filepath.Join(root, "local-appdata")
	privateBin := filepath.Join(root, "bin")
	for _, directory := range []string{home, codexHome, temporary, xdgConfig, xdgCache, xdgData, appData, localAppData, privateBin} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return nil, err
		}
	}
	if attestation.Auth.Path != "" {
		if err := copyAttestedCodexAuth(attestation.Auth, filepath.Join(codexHome, "auth.json")); err != nil {
			return nil, err
		}
	} else if codexProviderIsTestExecutable(command) {
		if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte("{}\n"), 0o600); err != nil {
			return nil, err
		}
	}
	environment := cleanCodexEnvironment(command, home, codexHome, temporary, xdgConfig, xdgCache, xdgData, appData, localAppData)
	environment, err = bindCodexRuntimePath(runtime.GOOS, environment, attestation, privateBin)
	if err != nil {
		return nil, err
	}
	cleanup = false
	return &codexPrivateRuntime{root: root, environment: environment, ownedRoot: owned}, nil
}

func copyAttestedCodexAuth(expected codexProviderFileIdentity, destination string) error {
	actual, err := inspectCodexProviderBoundedFile(expected.Path, 2<<20, true)
	if err != nil || actual != expected {
		return errors.New("Codex authentication material changed before private copy")
	}
	source, err := os.Open(expected.Path)
	if err != nil {
		return err
	}
	defer source.Close()
	temporary, err := os.OpenFile(destination+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(temporary, io.LimitReader(source, expected.Bytes+1))
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || written != expected.Bytes {
		_ = os.Remove(destination + ".tmp")
		return errors.New("copy stable Codex authentication material")
	}
	if err := os.Rename(destination+".tmp", destination); err != nil {
		_ = os.Remove(destination + ".tmp")
		return err
	}
	copied, err := inspectCodexProviderBoundedFile(destination, 2<<20, true)
	if err != nil || copied.SHA256 != expected.SHA256 || copied.Bytes != expected.Bytes {
		return errors.New("private Codex authentication copy does not match attested material")
	}
	return nil
}

func cleanCodexEnvironment(command, home, codexHome, temporary, xdgConfig, xdgCache, xdgData, appData, localAppData string) []string {
	// This is an explicit allowlist. Proxy, GitHub/App, SSH, provider, user
	// config, token, secret, and credential-path variables are absent even when
	// their names are not known in advance.
	allowed := []string{
		"SYSTEMROOT", "WINDIR",
		"LANG", "LC_ALL", "LC_CTYPE", "TZ", "TERM",
	}
	result := make([]string, 0, len(allowed)+16)
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
			result = append(result, name+"="+value)
		}
	}
	result = append(result,
		"HOME="+home,
		"USERPROFILE="+home,
		"CODEX_HOME="+codexHome,
		"XDG_CONFIG_HOME="+xdgConfig,
		"XDG_CACHE_HOME="+xdgCache,
		"XDG_DATA_HOME="+xdgData,
		"APPDATA="+appData,
		"LOCALAPPDATA="+localAppData,
		"TMPDIR="+temporary,
		"TMP="+temporary,
		"TEMP="+temporary,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if codexProviderIsTestExecutable(command) {
		for _, pair := range os.Environ() {
			name, _, _ := strings.Cut(pair, "=")
			if name == "GO_WANT_CODEX_PROVIDER_HELPER" || strings.HasPrefix(name, "HIVE_TEST_") {
				result = append(result, pair)
			}
		}
	}
	return result
}

func bindCodexRuntimePath(platform string, environment []string, attestation *codexProviderIdentityAttestation, privateBin string) ([]string, error) {
	path := ""
	if platform == "windows" {
		if privateBin == "" || !filepath.IsAbs(privateBin) {
			return nil, errors.New("Codex Windows runtime requires a private executable path")
		}
		path = filepath.Clean(privateBin)
	} else if platform != "linux" {
		return environment, nil
	}
	if platform == "linux" {
		if attestation == nil || attestation.ContainmentHelper.Path == "" || attestation.SealedContainmentHelper.Path == "" || attestation.ContainmentHelperRoot == "" {
			return nil, errors.New("Codex Linux containment helper is not attested and sealed")
		}
		if filepath.Dir(attestation.SealedContainmentHelper.Path) != filepath.Clean(attestation.ContainmentHelperRoot) || filepath.Base(attestation.SealedContainmentHelper.Path) != "bwrap" {
			return nil, errors.New("Codex Linux containment helper seal path is invalid")
		}
		sealed, err := inspectCodexProviderFile(attestation.SealedContainmentHelper.Path)
		if err != nil || sealed != attestation.SealedContainmentHelper || sealed.SHA256 != attestation.ContainmentHelper.SHA256 || sealed.Bytes != attestation.ContainmentHelper.Bytes {
			return nil, errors.New("Codex Linux containment helper seal does not match its attested source")
		}
		path = attestation.ContainmentHelperRoot
	}
	result := make([]string, 0, len(environment)+1)
	for _, pair := range environment {
		name, _, _ := strings.Cut(pair, "=")
		if strings.EqualFold(name, "PATH") {
			continue
		}
		result = append(result, pair)
	}
	return append(result, "PATH="+path), nil
}

func codexProviderIsTestExecutable(command string) bool {
	current, err := os.Executable()
	if err != nil {
		return false
	}
	current, err = filepath.Abs(current)
	if err != nil {
		return false
	}
	base := strings.ToLower(filepath.Base(current))
	if !strings.Contains(base, ".test") && !strings.HasSuffix(base, "test.exe") {
		return false
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return false
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return false
	}
	same := resolved == current
	if runtime.GOOS == "windows" {
		same = strings.EqualFold(resolved, current)
	}
	if same {
		return true
	}
	// Several containment tests intentionally copy the current native test
	// executable to exercise sealing and sandbox-bin paths. Only accept such a
	// copy when its complete attested bytes match the running Go test binary.
	// This hook is unreachable in production binaries because of the suffix
	// check above.
	resolvedIdentity, resolvedErr := inspectCodexProviderFile(resolved)
	currentIdentity, currentErr := inspectCodexProviderFile(current)
	return resolvedErr == nil && currentErr == nil &&
		resolvedIdentity.Bytes == currentIdentity.Bytes &&
		resolvedIdentity.SHA256 == currentIdentity.SHA256
}

func (runtimeState *codexPrivateRuntime) cleanup() error {
	if runtimeState == nil || runtimeState.root == "" {
		return nil
	}
	root := filepath.Clean(runtimeState.root)
	base := filepath.Base(root)
	if runtimeState.ownedRoot {
		if !strings.HasPrefix(base, "hive-codex-private-") {
			return errors.New("refuse unsafe private Codex runtime cleanup")
		}
	} else if base != "runtime" {
		return errors.New("refuse unsafe child Codex runtime cleanup")
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refuse replaced private Codex runtime cleanup")
	}
	if reparse, reparseErr := codexProviderPathIsReparsePoint(root); reparseErr != nil || reparse {
		return fmt.Errorf("refuse linked private Codex runtime cleanup")
	}
	return os.RemoveAll(root)
}

func (p CodexProvider) commandEnvironment() []string {
	if p.runtimeEnvironment != nil {
		return append([]string(nil), p.runtimeEnvironment...)
	}
	// Metadata paths should still be clean if a caller invokes them outside the
	// normal private-runtime flow.
	return cleanCodexEnvironment(p.Command, os.DevNull, os.DevNull, os.TempDir(), os.DevNull, os.DevNull, os.DevNull, os.DevNull, os.DevNull)
}
