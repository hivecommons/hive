package cline

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const (
	ProviderMountPath = "/run/hive-cline-provider/providers.json"
	ProviderStatePath = "/run/hive-cline-state"
)

// ProviderFile is an opaque, per-run selected-provider capability, not a host
// mount description. Only StageProviderFile can construct a usable instance.
type ProviderFile struct {
	mu      sync.Mutex
	dir     string
	model   string
	digest  [sha256.Size]byte
	secrets []string
	closed  bool
	removed bool
}

func (*ProviderFile) String() string { return "ClineProviderFile(redacted)" }

func StageProviderFile(source, model, workspace string) (*ProviderFile, error) {
	if !filepath.IsAbs(source) {
		return nil, errors.New("Cline Podman requires an explicit absolute HIVE_CLINE_CONFIG_DIR for dedicated operator credentials")
	}
	provider, _, err := Selection(model)
	if err != nil {
		return nil, err
	}
	sourceFile := filepath.Join(source, "data", "settings", "providers.json")
	if info, err := os.Stat(sourceFile); err != nil || !info.Mode().IsRegular() || info.Size() > maxProviderSecretBytes {
		return nil, errors.New("Cline provider config missing in the explicit HIVE_CLINE_CONFIG_DIR; configure its dedicated provider with cline auth")
	}
	if err := secretStateOutsideWorkspace(sourceFile, workspace); err != nil {
		return nil, errors.New("Cline credential source must resolve outside the Git workspace")
	}
	credentials, err := selectedCredentials(source, provider)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.New("Cline requires a private state home")
	}
	parent := filepath.Join(home, ".local", "state", "crux", "cline", "provider-mounts")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, errors.New("cannot create Cline private provider staging root")
	}
	if err := secretStateOutsideWorkspace(parent, workspace); err != nil {
		return nil, err
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("Cline provider staging root must be a private, non-symlink directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || os.Geteuid() != os.Getuid() {
		return nil, errors.New("Cline provider staging root must belong to the Podman keep-id user")
	}
	dir, err := os.MkdirTemp(parent, "provider-")
	if err != nil {
		return nil, errors.New("cannot create private Cline provider stage")
	}
	p := &ProviderFile{dir: dir, model: model, digest: sha256.Sum256(credentials), secrets: credentialValues(credentials)}
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), credentials, 0o600); err != nil {
		_ = p.Close()
		return nil, errors.New("cannot stage selected Cline provider")
	}
	if _, err := p.MountFile(workspace); err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

func (p *ProviderFile) MountFile(workspace string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.dir == "" || strings.ContainsAny(p.dir, ":\n\r") {
		return "", errors.New("invalid or closed Cline provider capability")
	}
	if err := secretStateOutsideWorkspace(p.dir, workspace); err != nil {
		return "", err
	}
	file := filepath.Join(p.dir, "providers.json")
	for path, mode := range map[string]os.FileMode{p.dir: 0o700, file: 0o600} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode {
			return "", errors.New("Cline provider stage must retain private, non-symlink permissions")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) || os.Geteuid() != os.Getuid() {
			return "", errors.New("Cline provider stage must be owned by the Podman keep-id user")
		}
	}
	raw, err := os.ReadFile(file)
	if err != nil || sha256.Sum256(raw) != p.digest {
		return "", errors.New("Cline selected-provider stage was modified")
	}
	return file, nil
}

func (p *ProviderFile) Model() string { return p.model }
func (p *ProviderFile) ID() string    { return filepath.Base(p.dir) }

func (*ProviderFile) SanitizeEnv(env []string) []string { return workerEnv(env) }

func (p *ProviderFile) Redact(text string) string {
	for _, secret := range p.secrets {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return text
}

func (p *ProviderFile) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.removed || p.dir == "" {
		return nil
	}
	p.closed = true
	// Revoke future reads through an existing bind mount before unlinking it.
	if info, err := os.Lstat(p.dir); err == nil && info.IsDir() {
		if fd, err := syscall.Open(filepath.Join(p.dir, "providers.json"), syscall.O_WRONLY|syscall.O_TRUNC|syscall.O_NOFOLLOW, 0); err == nil {
			_ = syscall.Close(fd)
		}
	}
	if err := os.RemoveAll(p.dir); err != nil {
		return errors.New("cannot remove private Cline provider stage")
	}
	p.removed = true
	return nil
}

func credentialValues(raw []byte) []string {
	var value any
	_ = json.Unmarshal(raw, &value)
	var secrets []string
	var visit func(any, bool)
	visit = func(value any, sensitive bool) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				k := strings.ToLower(key)
				secret := k != "tokensource" && (strings.Contains(k, "key") || strings.Contains(k, "token") ||
					strings.Contains(k, "secret") || strings.Contains(k, "password") || strings.Contains(k, "authorization"))
				visit(child, sensitive || secret)
			}

		case []any:
			for _, child := range v {
				visit(child, sensitive)
			}
		case string:
			if sensitive && v != "" {
				secrets = append(secrets, v)
			}
		}
	}
	visit(value, false)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return secrets
}

func containsRefreshToken(raw []byte) bool {
	var value any
	_ = json.Unmarshal(raw, &value)
	var visit func(any) bool
	visit = func(value any) bool {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
				if strings.Contains(normalized, "refreshtoken") && child != nil && child != "" {
					return true
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range v {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}
