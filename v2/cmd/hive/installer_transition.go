package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

const installerTransitionSchema = "hive.installer-transition.v1"
const installerRuntimeReleaseTimeout = 30 * time.Second

type installerTransitionEntry struct {
	StateDir        string `json:"state_dir"`
	IntervalSeconds int64  `json:"interval_seconds"`
	PreviousPID     int    `json:"previous_pid,omitempty"`
	Phase           string `json:"phase"`
}

type installerTransitionJournal struct {
	SchemaVersion   string                     `json:"schema_version"`
	CandidateCommit string                     `json:"candidate_commit"`
	CandidateSHA256 string                     `json:"candidate_sha256"`
	OwnedInstallDir string                     `json:"owned_install_dir"`
	Phase           string                     `json:"phase"`
	Entries         []installerTransitionEntry `json:"entries"`
	UpdatedAt       time.Time                  `json:"updated_at"`
}

type installerDaemonDescriptor struct {
	StateDir        string `json:"state_dir"`
	Executable      string `json:"executable"`
	Name            string `json:"name"`
	IntervalSeconds int64  `json:"interval_seconds"`
}

type installerDaemonRuntime struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
}

var installerTransitionStateRoot = integratedStateRoot
var runInstallerSchedulerStart = func(ctx context.Context, executable string, arguments []string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = daemonEnvironment(os.Environ())
	return command.CombinedOutput()
}

func runInstallerTransition(args []string) int {
	flags := flag.NewFlagSet("hive installer-transition", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	phase := flags.String("phase", "", "prepare, commit, rollback, or finalize")
	journalPath := flags.String("journal", "", "absolute installer transaction journal")
	ownedInstallDir := flags.String("owned-install-dir", "", "exact recognized Hive installation directory")
	runtimeExecutable := flags.String("runtime-executable", "", "restored or activated Hive executable used to restart schedulers")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	journal, err := executeInstallerTransition(*phase, *journalPath, *ownedInstallDir, *runtimeExecutable)
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": installerTransitionSchema, "phase": *phase, "error": err.Error(), "journal": journal})
		} else {
			fmt.Fprintln(os.Stderr, "installer scheduler transition failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(map[string]any{"schema_version": installerTransitionSchema, "phase": *phase, "schedulers": len(journal.Entries), "completed": true})
	}
	return 0
}

func executeInstallerTransition(phase, journalPath, ownedInstallDir, runtimeExecutable string) (installerTransitionJournal, error) {
	journalPath, err := validateInstallerTransitionJournalPath(journalPath)
	if err != nil {
		return installerTransitionJournal{}, err
	}
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "prepare":
		return prepareInstallerTransition(journalPath, ownedInstallDir)
	case "commit", "rollback":
		return restartInstallerTransition(journalPath, runtimeExecutable, strings.ToLower(strings.TrimSpace(phase)))
	case "finalize":
		return finalizeInstallerTransition(journalPath)
	default:
		return installerTransitionJournal{}, errors.New("installer transition phase must be prepare, commit, rollback, or finalize")
	}
}

func validateInstallerTransitionJournalPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("installer transition journal must be an absolute path")
	}
	path = filepath.Clean(path)
	if filepath.Dir(path) == path {
		return "", errors.New("installer transition journal cannot be a filesystem root")
	}
	return path, nil
}

func prepareInstallerTransition(journalPath, ownedInstallDir string) (installerTransitionJournal, error) {
	ownedInstallDir, err := filepath.Abs(strings.TrimSpace(ownedInstallDir))
	if err != nil || strings.TrimSpace(ownedInstallDir) == "" {
		return installerTransitionJournal{}, errors.New("exact owned installation directory is required")
	}
	executable, err := os.Executable()
	if err != nil {
		return installerTransitionJournal{}, err
	}
	commit, digest, err := currentDaemonExecutableIdentity(executable)
	if err != nil {
		return installerTransitionJournal{}, err
	}
	journal, exists, err := readInstallerTransitionJournal(journalPath)
	if err != nil {
		return journal, err
	}
	if exists {
		if journal.Phase == "commit-complete" {
			if !sameInstallerPath(journal.OwnedInstallDir, ownedInstallDir) {
				return journal, errors.New("prior committed installer scheduler journal belongs to a different Hive installation")
			}
			installedExecutable := filepath.Join(journal.OwnedInstallDir, "hive")
			if runtime.GOOS == "windows" {
				installedExecutable += ".exe"
			}
			installedDigest, digestErr := daemonExecutableSHA256(installedExecutable)
			if digestErr != nil || !strings.EqualFold(installedDigest, journal.CandidateSHA256) {
				return journal, errors.New("prior committed installer scheduler journal does not match the active Hive bytes")
			}
			if err := os.Remove(journalPath); err != nil {
				return journal, err
			}
			exists = false
		}
	}
	if exists {
		if !sameDaemonExecutableIdentity(executable, commit, digest, executable, journal.CandidateCommit, journal.CandidateSHA256) || !sameInstallerPath(journal.OwnedInstallDir, ownedInstallDir) {
			return journal, errors.New("an installer scheduler transaction for different immutable bytes is already pending; recover it with the original signed installer")
		}
	} else {
		entries, discoveryErr := discoverOwnedInstallerSchedulers(ownedInstallDir)
		if discoveryErr != nil {
			return journal, discoveryErr
		}
		journal = installerTransitionJournal{
			SchemaVersion: installerTransitionSchema, CandidateCommit: commit, CandidateSHA256: digest,
			OwnedInstallDir: filepath.Clean(ownedInstallDir), Phase: "preparing", Entries: entries,
		}
		if err := writeInstallerTransitionJournal(journalPath, &journal); err != nil {
			return journal, err
		}
	}
	for index := range journal.Entries {
		entry := &journal.Entries[index]
		if entry.Phase == "stopped" {
			continue
		}
		entry.Phase = "stopping"
		if err := writeInstallerTransitionJournal(journalPath, &journal); err != nil {
			return journal, err
		}
		if _, err := stopIntegratedDaemon(entry.StateDir); err != nil {
			return journal, fmt.Errorf("stop exact scheduler for %s: %w", entry.StateDir, err)
		}
		if entry.PreviousPID > 0 && processIsAlive(entry.PreviousPID) {
			expectedExecutable := filepath.Join(journal.OwnedInstallDir, "hive")
			if runtime.GOOS == "windows" {
				expectedExecutable += ".exe"
			}
			if !processMatchesExecutable(entry.PreviousPID, expectedExecutable) {
				return journal, fmt.Errorf("legacy scheduler PID %d for %s no longer belongs to exact installation %s", entry.PreviousPID, entry.StateDir, journal.OwnedInstallDir)
			}
			if err := terminateProcess(entry.PreviousPID); err != nil {
				return journal, fmt.Errorf("terminate exact legacy scheduler %d for %s: %w", entry.PreviousPID, entry.StateDir, err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) && processIsAlive(entry.PreviousPID) {
				time.Sleep(100 * time.Millisecond)
			}
			if processIsAlive(entry.PreviousPID) {
				return journal, fmt.Errorf("exact legacy scheduler %d for %s did not stop", entry.PreviousPID, entry.StateDir)
			}
		}
		entry.Phase = "stopped"
		if err := writeInstallerTransitionJournal(journalPath, &journal); err != nil {
			return journal, err
		}
	}
	if len(journal.Entries) > 0 {
		executableName := "hive"
		if runtime.GOOS == "windows" {
			executableName += ".exe"
		}
		if err := waitForInstallerExecutableRelease(filepath.Join(journal.OwnedInstallDir, executableName), installerRuntimeReleaseTimeout); err != nil {
			return journal, fmt.Errorf("wait for exact scheduler runtime release: %w", err)
		}
	}
	journal.Phase = "prepared"
	return journal, writeInstallerTransitionJournal(journalPath, &journal)
}

func restartInstallerTransition(journalPath, runtimeExecutable, phase string) (installerTransitionJournal, error) {
	journal, exists, err := readInstallerTransitionJournal(journalPath)
	if err != nil || !exists {
		if err == nil {
			err = errors.New("installer scheduler transition journal is missing")
		}
		return journal, err
	}
	runtimeExecutable, err = filepath.Abs(strings.TrimSpace(runtimeExecutable))
	if err != nil || strings.TrimSpace(runtimeExecutable) == "" {
		return journal, errors.New("exact runtime executable is required to restart schedulers")
	}
	expectedName := "hive"
	if runtime.GOOS == "windows" {
		expectedName = "hive.exe"
	}
	if !sameInstallerPath(filepath.Dir(runtimeExecutable), journal.OwnedInstallDir) || !strings.EqualFold(filepath.Base(runtimeExecutable), expectedName) {
		return journal, errors.New("scheduler restart executable is outside the exact owned Hive installation")
	}
	if info, statErr := os.Lstat(runtimeExecutable); statErr != nil || !info.Mode().IsRegular() {
		return journal, errors.New("scheduler restart executable is not a regular file")
	}
	journal.Phase = phase + "-restarting"
	if err := writeInstallerTransitionJournal(journalPath, &journal); err != nil {
		return journal, err
	}
	for index := range journal.Entries {
		entry := &journal.Entries[index]
		// A prior interrupted commit may already have started this state. Stop it
		// first so rollback cannot retain candidate bytes and commit cannot create
		// a second owner.
		if _, err := stopIntegratedDaemon(entry.StateDir); err != nil {
			return journal, fmt.Errorf("stop partially restarted scheduler for %s: %w", entry.StateDir, err)
		}
		entry.Phase = phase + "-restarting"
		if err := writeInstallerTransitionJournal(journalPath, &journal); err != nil {
			return journal, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		output, runErr := runInstallerSchedulerStart(ctx, runtimeExecutable, []string{"start", "--state-dir", entry.StateDir, "--interval", strconv.FormatInt(entry.IntervalSeconds, 10) + "s", "--json"})
		cancel()
		if runErr != nil {
			return journal, fmt.Errorf("restart scheduler for %s with exact %s runtime: %w: %s", entry.StateDir, phase, runErr, sanitizeDaemonError(errors.New(string(output))))
		}
		entry.Phase = phase + "-running"
		if err := writeInstallerTransitionJournal(journalPath, &journal); err != nil {
			return journal, err
		}
	}
	journal.Phase = phase + "-complete"
	if err := writeInstallerTransitionJournal(journalPath, &journal); err != nil {
		return journal, err
	}
	if phase == "rollback" {
		if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return journal, err
		}
	}
	return journal, nil
}

func finalizeInstallerTransition(journalPath string) (installerTransitionJournal, error) {
	journal, exists, err := readInstallerTransitionJournal(journalPath)
	if err != nil || !exists {
		if err == nil {
			err = errors.New("installer scheduler transition journal is missing")
		}
		return journal, err
	}
	if journal.Phase != "commit-complete" {
		return journal, fmt.Errorf("installer scheduler transition is not commit-complete: %s", journal.Phase)
	}
	installedExecutable := filepath.Join(journal.OwnedInstallDir, "hive")
	if runtime.GOOS == "windows" {
		installedExecutable += ".exe"
	}
	digest, err := daemonExecutableSHA256(installedExecutable)
	if err != nil || !strings.EqualFold(digest, journal.CandidateSHA256) {
		return journal, errors.New("active Hive bytes do not match the committed installer scheduler transition")
	}
	if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return journal, err
	}
	return journal, nil
}

func discoverOwnedInstallerSchedulers(ownedInstallDir string) ([]installerTransitionEntry, error) {
	stateDirs := map[string]bool{}
	registryRoot := daemonSchedulerRegistryRoot()
	if entries, err := os.ReadDir(registryRoot); err == nil {
		if len(entries) > 1000 {
			return nil, errors.New("Hive scheduler registry is excessive")
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(registryRoot, entry.Name())
			descriptor, readErr := readInstallerDaemonDescriptor(path)
			if readErr != nil {
				return nil, readErr
			}
			stateDirs[filepath.Clean(descriptor.StateDir)] = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	repositoriesRoot := filepath.Join(installerTransitionStateRoot(), "repositories")
	if entries, err := os.ReadDir(repositoriesRoot); err == nil {
		if len(entries) > 1000 {
			return nil, errors.New("Hive repository state registry is excessive")
		}
		for _, entry := range entries {
			if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				stateDirs[filepath.Join(repositoriesRoot, entry.Name())] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if current, exists, err := integratedCurrentStateForInstaller(); err != nil {
		return nil, err
	} else if exists {
		stateDirs[current] = true
	}
	ordered := make([]string, 0, len(stateDirs))
	for stateDir := range stateDirs {
		ordered = append(ordered, stateDir)
	}
	sort.Strings(ordered)
	result := []installerTransitionEntry{}
	for _, stateDir := range ordered {
		descriptorPath := daemonServiceDescriptorPath(stateDir)
		if _, err := os.Lstat(descriptorPath); errors.Is(err, os.ErrNotExist) {
			continue
		}
		descriptor, err := readInstallerDaemonDescriptor(descriptorPath)
		if err != nil {
			return nil, err
		}
		if !sameInstallerPath(descriptor.StateDir, stateDir) || descriptor.Name != platformDaemonServiceName(stateDir) || descriptor.IntervalSeconds < 60 || descriptor.IntervalSeconds > int64((24*time.Hour).Seconds()) {
			return nil, fmt.Errorf("scheduler descriptor for %s is not exactly state-bound", stateDir)
		}
		expectedExecutable := filepath.Join(ownedInstallDir, "hive")
		if runtime.GOOS == "windows" {
			expectedExecutable += ".exe"
		}
		if !sameInstallerPath(descriptor.Executable, expectedExecutable) {
			// Multiple dedicated Hive installations may share the user's state
			// registry. This transaction owns only the exact executable path passed
			// by the already-validated installer; leave every other installation
			// untouched.
			continue
		}
		runtimeRecord, runtimeErr := readInstallerDaemonRuntime(filepath.Join(stateDir, "integrated", "daemon.json"))
		if runtimeErr != nil && !errors.Is(runtimeErr, os.ErrNotExist) {
			return nil, fmt.Errorf("read scheduler runtime for %s: %w", stateDir, runtimeErr)
		}
		if runtimeRecord.PID > 0 && runtimeRecord.Executable != "" && !sameInstallerPath(runtimeRecord.Executable, expectedExecutable) {
			return nil, fmt.Errorf("scheduler runtime for %s is not owned by exact installation %s", stateDir, ownedInstallDir)
		}
		result = append(result, installerTransitionEntry{StateDir: filepath.Clean(stateDir), IntervalSeconds: descriptor.IntervalSeconds, PreviousPID: runtimeRecord.PID, Phase: "pending"})
	}
	return result, nil
}

func integratedCurrentStateForInstaller() (string, bool, error) {
	return integrated.CurrentState(installerTransitionStateRoot())
}

func readInstallerDaemonDescriptor(path string) (installerDaemonDescriptor, error) {
	data, err := readBoundedInstallerJSON(path)
	if err != nil {
		return installerDaemonDescriptor{}, err
	}
	var descriptor installerDaemonDescriptor
	if err := json.Unmarshal(data, &descriptor); err != nil || descriptor.StateDir == "" || descriptor.Executable == "" || descriptor.Name == "" {
		return installerDaemonDescriptor{}, fmt.Errorf("invalid Hive scheduler descriptor: %s", path)
	}
	return descriptor, nil
}

func readInstallerDaemonRuntime(path string) (installerDaemonRuntime, error) {
	data, err := readBoundedInstallerJSON(path)
	if err != nil {
		return installerDaemonRuntime{}, err
	}
	var record installerDaemonRuntime
	if err := json.Unmarshal(data, &record); err != nil {
		return installerDaemonRuntime{}, err
	}
	return record, nil
}

func readBoundedInstallerJSON(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<20 {
		return nil, fmt.Errorf("installer transaction input is not a bounded regular file: %s", path)
	}
	return os.ReadFile(path)
}

func readInstallerTransitionJournal(path string) (installerTransitionJournal, bool, error) {
	data, err := readBoundedInstallerJSON(path)
	if errors.Is(err, os.ErrNotExist) {
		return installerTransitionJournal{}, false, nil
	}
	if err != nil {
		return installerTransitionJournal{}, false, err
	}
	var journal installerTransitionJournal
	if err := json.Unmarshal(data, &journal); err != nil || journal.SchemaVersion != installerTransitionSchema || !validDaemonHexIdentity(journal.CandidateCommit, 40) || !validDaemonHexIdentity(journal.CandidateSHA256, 64) || !filepath.IsAbs(journal.OwnedInstallDir) || len(journal.Entries) > 1000 {
		return journal, false, errors.New("installer scheduler transition journal is invalid")
	}
	for _, entry := range journal.Entries {
		if !filepath.IsAbs(entry.StateDir) || entry.IntervalSeconds < 60 || entry.IntervalSeconds > int64((24*time.Hour).Seconds()) {
			return journal, false, errors.New("installer scheduler transition entry is invalid")
		}
	}
	return journal, true, nil
}

func writeInstallerTransitionJournal(path string, journal *installerTransitionJournal) error {
	journal.SchemaVersion = installerTransitionSchema
	journal.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("installer scheduler transition journal is excessive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func sameInstallerPath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
