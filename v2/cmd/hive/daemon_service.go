package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const daemonServiceSchema = "hive.integrated-daemon-service.v2"

type daemonServiceSpec struct {
	SchemaVersion    string    `json:"schema_version"`
	Platform         string    `json:"platform"`
	Name             string    `json:"name"`
	StateDir         string    `json:"state_dir"`
	Executable       string    `json:"executable"`
	HiveCommit       string    `json:"hive_commit"`
	ExecutableSHA256 string    `json:"executable_sha256"`
	IntervalSeconds  int64     `json:"interval_seconds"`
	InstalledAt      time.Time `json:"installed_at"`
}

type daemonServiceStatus struct {
	SchemaVersion    string `json:"schema_version"`
	Managed          bool   `json:"managed"`
	Platform         string `json:"platform"`
	Name             string `json:"name,omitempty"`
	Installed        bool   `json:"installed"`
	Enabled          bool   `json:"enabled"`
	Active           bool   `json:"active"`
	RebootPersistent bool   `json:"reboot_persistent"`
	LoginPersistent  bool   `json:"login_persistent"`
	RestartOnFailure bool   `json:"restart_on_failure"`
	StartupMode      string `json:"startup_mode,omitempty"`
	DefinitionPath   string `json:"definition_path,omitempty"`
	HiveCommit       string `json:"hive_commit,omitempty"`
	ExecutableSHA256 string `json:"executable_sha256,omitempty"`
	InspectionError  string `json:"inspection_error,omitempty"`
}

type daemonServiceManager interface {
	Install(context.Context, daemonServiceSpec) error
	Start(context.Context, daemonServiceSpec) error
	Inspect(context.Context, daemonServiceSpec) (daemonServiceStatus, error)
	Uninstall(context.Context, daemonServiceSpec) error
}

var daemonServices daemonServiceManager = newPlatformDaemonServiceManager()
var daemonSchedulerRegistryRoot = func() string { return filepath.Join(integratedStateRoot(), "schedulers") }

func daemonServiceDescriptorPath(stateDir string) string {
	return filepath.Join(stateDir, "integrated", "daemon-service.json")
}

func daemonServiceRegistryPath(spec daemonServiceSpec) string {
	return filepath.Join(daemonSchedulerRegistryRoot(), spec.Name+".json")
}

func newDaemonServiceSpec(stateDir, executable string, interval time.Duration) (daemonServiceSpec, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return daemonServiceSpec{}, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return daemonServiceSpec{}, err
	}
	if interval < time.Minute || interval > 24*time.Hour {
		return daemonServiceSpec{}, fmt.Errorf("scan interval must be between 1 minute and 24 hours")
	}
	commit, digest, err := currentDaemonExecutableIdentity(executable)
	if err != nil {
		return daemonServiceSpec{}, err
	}
	return newDaemonServiceSpecWithIdentity(stateDir, executable, interval, commit, digest)
}

func newDaemonServiceSpecWithIdentity(stateDir, executable string, interval time.Duration, commit, digest string) (daemonServiceSpec, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return daemonServiceSpec{}, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return daemonServiceSpec{}, err
	}
	if interval < time.Minute || interval > 24*time.Hour {
		return daemonServiceSpec{}, fmt.Errorf("scan interval must be between 1 minute and 24 hours")
	}
	commit = strings.ToLower(strings.TrimSpace(commit))
	digest = strings.ToLower(strings.TrimSpace(digest))
	if !validDaemonHexIdentity(commit, 40) || !validDaemonHexIdentity(digest, 64) {
		return daemonServiceSpec{}, errors.New("Hive scheduler executable identity requires an immutable 40-character commit and SHA-256 digest")
	}
	return daemonServiceSpec{
		SchemaVersion:    daemonServiceSchema,
		Platform:         runtime.GOOS,
		Name:             platformDaemonServiceName(stateDir),
		StateDir:         stateDir,
		Executable:       executable,
		HiveCommit:       commit,
		ExecutableSHA256: digest,
		IntervalSeconds:  int64(interval.Seconds()),
		InstalledAt:      time.Now().UTC(),
	}, nil
}

func currentDaemonExecutableIdentity(executable string) (string, string, error) {
	commit := strings.ToLower(strings.TrimSpace(gitHash))
	if !validDaemonHexIdentity(commit, 40) {
		return "", "", fmt.Errorf("persistent Hive scheduler requires a release binary with an immutable 40-character Hive commit; got %q", gitHash)
	}
	digest, err := daemonExecutableSHA256(executable)
	if err != nil {
		return "", "", fmt.Errorf("hash persistent Hive scheduler executable: %w", err)
	}
	return commit, digest, nil
}

func daemonExecutableSHA256(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("scheduler executable is not a regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validDaemonHexIdentity(value string, length int) bool {
	if len(value) != length {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == length
}

func stableDaemonServiceSuffix(stateDir string) string {
	identity := filepath.Clean(stateDir)
	if runtime.GOOS == "windows" {
		identity = strings.ToLower(identity)
	}
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:8])
}

func validateDaemonServiceSpec(spec daemonServiceSpec) error {
	if spec.SchemaVersion != daemonServiceSchema || spec.Platform != runtime.GOOS || spec.Name == "" || spec.StateDir == "" || spec.Executable == "" || !validDaemonHexIdentity(spec.HiveCommit, 40) || !validDaemonHexIdentity(spec.ExecutableSHA256, 64) || spec.IntervalSeconds < 60 || spec.IntervalSeconds > int64((24*time.Hour).Seconds()) {
		return errors.New("Hive scheduler service descriptor is invalid")
	}
	stateDir, err := filepath.Abs(spec.StateDir)
	if err != nil || filepath.Clean(stateDir) != filepath.Clean(spec.StateDir) || spec.Name != platformDaemonServiceName(stateDir) {
		return errors.New("Hive scheduler service descriptor is not bound to its state directory")
	}
	if executable, err := filepath.Abs(spec.Executable); err != nil || filepath.Clean(executable) != filepath.Clean(spec.Executable) {
		return errors.New("Hive scheduler service descriptor executable is not absolute")
	}
	return nil
}

func writeDaemonServiceSpec(spec daemonServiceSpec) error {
	if err := validateDaemonServiceSpec(spec); err != nil {
		return err
	}
	dir := filepath.Join(spec.StateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	temporary := filepath.Join(dir, fmt.Sprintf(".daemon-service-%d.tmp", os.Getpid()))
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, daemonServiceDescriptorPath(spec.StateDir)); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func writeDaemonServiceRegistry(spec daemonServiceSpec) error {
	if err := validateDaemonServiceSpec(spec); err != nil {
		return err
	}
	path := daemonServiceRegistryPath(spec)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + fmt.Sprintf(".%d.tmp", os.Getpid())
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func removeDaemonServiceRegistry(spec daemonServiceSpec) error {
	path := daemonServiceRegistryPath(spec)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readDaemonServiceSpec(stateDir string) (daemonServiceSpec, bool, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return daemonServiceSpec{}, false, err
	}
	data, err := os.ReadFile(daemonServiceDescriptorPath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return daemonServiceSpec{}, false, nil
	}
	if err != nil {
		return daemonServiceSpec{}, false, err
	}
	var spec daemonServiceSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return daemonServiceSpec{}, false, fmt.Errorf("decode Hive scheduler service descriptor: %w", err)
	}
	if err := validateDaemonServiceSpec(spec); err != nil {
		return daemonServiceSpec{}, false, err
	}
	if filepath.Clean(spec.StateDir) != filepath.Clean(stateDir) {
		return daemonServiceSpec{}, false, errors.New("Hive scheduler service descriptor points at a different state directory")
	}
	return spec, true, nil
}

func activateDaemonService(stateDir, executable string, interval time.Duration) (daemonServiceSpec, error) {
	spec, err := newDaemonServiceSpec(stateDir, executable, interval)
	if err != nil {
		return daemonServiceSpec{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if old, exists, readErr := readDaemonServiceSpec(stateDir); readErr != nil {
		return daemonServiceSpec{}, readErr
	} else if exists && (old.Name != spec.Name || old.Platform != spec.Platform) {
		if err := daemonServices.Uninstall(ctx, old); err != nil {
			return daemonServiceSpec{}, fmt.Errorf("remove replaced Hive scheduler service: %w", err)
		}
	}
	if err := daemonServices.Install(ctx, spec); err != nil {
		return daemonServiceSpec{}, fmt.Errorf("install persistent Hive scheduler service: %w", err)
	}
	if err := writeDaemonServiceSpec(spec); err != nil {
		_ = daemonServices.Uninstall(ctx, spec)
		return daemonServiceSpec{}, fmt.Errorf("persist Hive scheduler service descriptor: %w", err)
	}
	if err := writeDaemonServiceRegistry(spec); err != nil {
		_ = daemonServices.Uninstall(ctx, spec)
		_ = os.Remove(daemonServiceDescriptorPath(spec.StateDir))
		return daemonServiceSpec{}, fmt.Errorf("persist Hive scheduler registry: %w", err)
	}
	if err := daemonServices.Start(ctx, spec); err != nil {
		_ = daemonServices.Uninstall(ctx, spec)
		_ = os.Remove(daemonServiceDescriptorPath(spec.StateDir))
		_ = removeDaemonServiceRegistry(spec)
		return daemonServiceSpec{}, fmt.Errorf("start persistent Hive scheduler service: %w", err)
	}
	return spec, nil
}

func uninstallDaemonService(stateDir string) error {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	spec, exists, err := readDaemonServiceSpec(stateDir)
	if err != nil || !exists {
		executable, executableErr := os.Executable()
		if executableErr != nil {
			return executableErr
		}
		spec, err = newDaemonServiceSpec(stateDir, executable, 15*time.Minute)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := daemonServices.Uninstall(ctx, spec); err != nil {
		return fmt.Errorf("remove persistent Hive scheduler service: %w", err)
	}
	if err := os.Remove(daemonServiceDescriptorPath(stateDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := removeDaemonServiceRegistry(spec); err != nil {
		return err
	}
	return nil
}

func inspectDaemonService(stateDir string, runtimeActive bool) *daemonServiceStatus {
	result := &daemonServiceStatus{SchemaVersion: daemonServiceSchema, Platform: runtime.GOOS, Active: runtimeActive}
	spec, exists, err := readDaemonServiceSpec(stateDir)
	if err != nil {
		if _, statErr := os.Stat(daemonServiceDescriptorPath(stateDir)); statErr == nil {
			result.Managed = true
		}
		result.InspectionError = sanitizeDaemonError(err)
		return result
	}
	if !exists {
		return result
	}
	result.Managed, result.Name = true, spec.Name
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := daemonServices.Inspect(ctx, spec)
	if err != nil {
		result.InspectionError = sanitizeDaemonError(err)
		return result
	}
	status.SchemaVersion = daemonServiceSchema
	status.Managed = true
	status.Platform = spec.Platform
	status.Name = spec.Name
	status.Active = status.Active || runtimeActive
	return &status
}

func daemonServiceReady(status *daemonServiceStatus) bool {
	if status == nil || !status.Managed || !status.Installed || !status.Enabled || !status.RestartOnFailure || status.InspectionError != "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return status.LoginPersistent
	}
	return status.RebootPersistent
}

func serviceCommandError(operation string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %w: %s", operation, err, sanitizeDaemonError(errors.New(detail)))
}
