package repair

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const orphanRepairRefGrace = 25 * time.Hour

type repairRefJournalEntry struct {
	Timestamp          time.Time `json:"timestamp"`
	Phase              string    `json:"phase"`
	Kind               string    `json:"kind"`
	Ref                string    `json:"ref"`
	Commit             string    `json:"commit"`
	Binding            string    `json:"binding"`
	Repository         string    `json:"repository"`
	GitCommonDir       string    `json:"git_common_dir"`
	RepositoryIdentity string    `json:"repository_identity"`
	TombstoneRef       string    `json:"tombstone_ref,omitempty"`
}

var repairRefTombstonePattern = regexp.MustCompile(`^refs/hive/repair-ref-tombstones/[a-f0-9]{24}/[a-f0-9]{32}$`)

func (s *Store) recordRepairRefIntent(ctx context.Context, worktree, repository, kind, ref, commit, binding string) error {
	return s.recordRepairRefJournalPhase(ctx, worktree, repository, "intent", kind, ref, "", commit, binding)
}

func (s *Store) recordRepairRefConsumed(ctx context.Context, worktree, repository, kind, ref, commit, binding string) error {
	return s.recordRepairRefJournalPhase(ctx, worktree, repository, "consumed", kind, ref, "", commit, binding)
}

func (s *Store) recordRepairRefTombstoneIntent(ctx context.Context, worktree, repository, kind, ref, tombstone, commit, binding string) error {
	return s.recordRepairRefJournalPhase(ctx, worktree, repository, "tombstone_intent", kind, ref, tombstone, commit, binding)
}

func (s *Store) recordRepairRefTombstoneConsumed(ctx context.Context, worktree, repository, kind, ref, tombstone, commit, binding string) error {
	return s.recordRepairRefJournalPhase(ctx, worktree, repository, "tombstone_consumed", kind, ref, tombstone, commit, binding)
}

func (s *Store) recordRepairRefJournalPhase(ctx context.Context, worktree, repository, phase, kind, ref, tombstone, commit, binding string) error {
	commonDir, err := repairGitCommonDir(ctx, worktree)
	if err != nil {
		return err
	}
	identity, err := repairRepositoryIdentity(ctx, worktree, repository, commonDir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := repairRefJournalEntry{Timestamp: time.Now().UTC(), Phase: phase, Kind: kind, Ref: ref, TombstoneRef: tombstone, Commit: commit, Binding: binding, Repository: strings.TrimSpace(repository), GitCommonDir: commonDir, RepositoryIdentity: identity}
	if err := validateRepairRefJournalEntry(entry); err != nil {
		return err
	}
	return s.appendRepairRefJournalLocked(entry)
}

func (s *Store) appendRepairRefJournalLocked(entry repairRefJournalEntry) error {
	created := false
	if data, err := os.ReadFile(s.refJournalPath); err == nil {
		if _, err := s.recoverTornRepairRefJournalLocked(data); err != nil {
			return err
		}
	} else if os.IsNotExist(err) {
		created = true
	} else {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(s.refJournalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if created {
		return syncParentDirectory(s.refJournalPath)
	}
	return nil
}

func validateRepairRefJournalEntry(entry repairRefJournalEntry) error {
	if entry.Timestamp.IsZero() || !validGitCommitSHA(entry.Commit) || !recoveryDigestPattern.MatchString(entry.Binding) || strings.TrimSpace(entry.Repository) == "" || !filepath.IsAbs(entry.GitCommonDir) || !recoveryDigestPattern.MatchString(entry.RepositoryIdentity) {
		return fmt.Errorf("repair ref journal entry has invalid immutable fields")
	}
	switch entry.Phase {
	case "intent", "consumed":
		if entry.TombstoneRef != "" {
			return fmt.Errorf("ordinary repair ref journal phase names a tombstone")
		}
	case "tombstone_intent", "tombstone_consumed":
		if !repairRefTombstonePattern.MatchString(entry.TombstoneRef) || !strings.Contains(entry.TombstoneRef, "/"+entry.Binding[:24]+"/") {
			return fmt.Errorf("repair ref tombstone binding is invalid")
		}
	default:
		return fmt.Errorf("repair ref journal phase is invalid")
	}
	switch entry.Kind {
	case toolSnapshotPreparation, toolSnapshotValidation:
		if !toolSnapshotRefPattern.MatchString(entry.Ref) || !strings.Contains(entry.Ref, "/"+entry.Binding[:24]+"/") {
			return fmt.Errorf("repair tool snapshot journal binding is invalid")
		}
	case "preparation_recovery":
		if !preparationCleanupRefPattern.MatchString(entry.Ref) || !strings.Contains(entry.Ref, "/"+entry.Binding[:24]+"/") {
			return fmt.Errorf("preparation recovery journal binding is invalid")
		}
	case sealedTreeModelBase, sealedTreeCandidate:
		if !sealedTreeRefPattern.MatchString(entry.Ref) || !strings.Contains(entry.Ref, "/"+entry.Kind+"/"+entry.Binding[:24]+"/") {
			return fmt.Errorf("sealed repair tree journal binding is invalid")
		}
	default:
		return fmt.Errorf("repair ref journal kind is invalid")
	}
	return nil
}

func (s *Store) repairRefJournalEntries() ([]repairRefJournalEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.refJournalPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data, err = s.recoverTornRepairRefJournalLocked(data)
	if err != nil {
		return nil, err
	}
	entries := []repairRefJournalEntry{}
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry repairRefJournalEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil || validateRepairRefJournalEntry(entry) != nil {
			return nil, fmt.Errorf("repair ref journal line %d is invalid", lineNumber+1)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (s *Store) recoverTornRepairRefJournalLocked(data []byte) ([]byte, error) {
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return data, nil
	}
	lastNewline := strings.LastIndexByte(string(data), '\n')
	complete, fragment := []byte{}, data
	if lastNewline >= 0 {
		complete = append([]byte(nil), data[:lastNewline+1]...)
		fragment = data[lastNewline+1:]
	}
	digest := sha256.Sum256(fragment)
	quarantine := fmt.Sprintf("%s.torn-%x", s.refJournalPath, digest[:8])
	if err := writeNewSynced(quarantine, fragment); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("quarantine torn repair ref journal: %w", err)
		}
		existing, readErr := os.ReadFile(quarantine)
		if readErr != nil || !bytes.Equal(existing, fragment) {
			return nil, fmt.Errorf("torn repair ref journal quarantine content changed")
		}
	}
	if err := replaceFileSynced(s.refJournalPath, complete); err != nil {
		return nil, fmt.Errorf("remove torn repair ref journal record: %w", err)
	}
	return complete, nil
}

// retireRepairRef first moves the exact live guard into a random Hive-only
// tombstone with one Git ref transaction. Once moved, no code retries deletion
// of the original name: a later foreign recreation can never be consumed by
// stale cleanup authority. The internal tombstone remains journal-recoverable
// until its own exact CAS deletion is durable.
func retireRepairRef(ctx context.Context, worktree, repository string, store *Store, kind, ref, commit, binding string) error {
	if store == nil || !validGitCommitSHA(commit) || !recoveryDigestPattern.MatchString(binding) || strings.TrimSpace(repository) == "" {
		return fmt.Errorf("repair ref retirement inputs are invalid")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("create repair ref tombstone nonce: %w", err)
	}
	tombstone := fmt.Sprintf("refs/hive/repair-ref-tombstones/%s/%s", binding[:24], hex.EncodeToString(nonce))
	if err := store.recordRepairRefTombstoneIntent(ctx, worktree, repository, kind, ref, tombstone, commit, binding); err != nil {
		return fmt.Errorf("journal repair ref tombstone intent: %w", err)
	}
	entry := repairRefJournalEntry{Kind: kind, Ref: ref, TombstoneRef: tombstone, Commit: commit, Binding: binding, Repository: repository}
	return resumeRepairRefTombstone(ctx, worktree, repository, store, entry, false)
}

func resumeRepairRefTombstone(ctx context.Context, worktree, repository string, store *Store, entry repairRefJournalEntry, originalConsumed bool) error {
	original := ""
	var err error
	if !originalConsumed {
		original, err = readRepairRef(ctx, worktree, entry.Ref)
		if err != nil {
			return err
		}
	}
	tombstone, err := readRepairRef(ctx, worktree, entry.TombstoneRef)
	if err != nil {
		return err
	}
	switch {
	case originalConsumed && tombstone == entry.Commit:
		// Original authority is already permanently consumed. Never inspect,
		// move, or delete that name again; finish only the internal tombstone.
	case originalConsumed && tombstone == "":
		// Exact tombstone deletion committed before its final journal record.
	case original == entry.Commit && tombstone == "":
		if err := moveRepairRefToTombstone(ctx, worktree, entry.Ref, entry.TombstoneRef, entry.Commit); err != nil {
			return err
		}
	case original == "" && tombstone == entry.Commit:
		// A prior atomic move committed before its caller stopped.
	case original == "" && tombstone == "":
		// A prior exact tombstone deletion committed before its journal record.
	default:
		return fmt.Errorf("repair ref tombstone state is ambiguous: original=%q tombstone=%q", original, tombstone)
	}
	if !originalConsumed {
		if err := store.recordRepairRefConsumed(ctx, worktree, repository, entry.Kind, entry.Ref, entry.Commit, entry.Binding); err != nil {
			return fmt.Errorf("consume original repair ref authority: %w", err)
		}
	}
	tombstone, err = readRepairRef(ctx, worktree, entry.TombstoneRef)
	if err != nil {
		return err
	}
	if tombstone != "" {
		if tombstone != entry.Commit {
			return fmt.Errorf("repair ref tombstone changed before exact deletion")
		}
		if _, err := runGit(ctx, worktree, "update-ref", "--no-deref", "-d", entry.TombstoneRef, entry.Commit); err != nil {
			return fmt.Errorf("delete exact repair ref tombstone: %w", err)
		}
	}
	if err := store.recordRepairRefTombstoneConsumed(ctx, worktree, repository, entry.Kind, entry.Ref, entry.TombstoneRef, entry.Commit, entry.Binding); err != nil {
		return fmt.Errorf("consume repair ref tombstone authority: %w", err)
	}
	return nil
}

func moveRepairRefToTombstone(ctx context.Context, worktree, original, tombstone, commit string) error {
	input := fmt.Sprintf("start\ndelete %s %s\ncreate %s %s\nprepare\ncommit\n", original, commit, tombstone, commit)
	command := exec.CommandContext(ctx, "git", "update-ref", "--no-deref", "--stdin")
	command.Dir = worktree
	command.Env = gitCommandEnvironment(nil)
	command.Stdin = strings.NewReader(input)
	var output exactGitBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("atomically tombstone exact repair ref: %w: %s", err, safeExcerpt(output.String()))
	}
	if output.exceeded {
		return fmt.Errorf("repair ref tombstone transaction exceeded the exact-output safety limit")
	}
	return nil
}

func readRepairRef(ctx context.Context, worktree, ref string) (string, error) {
	symbolic := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", ref)
	symbolic.Dir = worktree
	symbolic.Env = gitCommandEnvironment(nil)
	var symbolicOutput exactGitBuffer
	symbolic.Stdout, symbolic.Stderr = &symbolicOutput, &symbolicOutput
	if err := symbolic.Run(); err == nil {
		return "", fmt.Errorf("repair ref %s is symbolic and cannot carry cleanup authority", ref)
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return "", fmt.Errorf("inspect repair ref %s for symbolic indirection: %w: %s", ref, err, safeExcerpt(symbolicOutput.String()))
		}
	}
	command := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", ref)
	command.Dir = worktree
	command.Env = gitCommandEnvironment(nil)
	var output exactGitBuffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && strings.TrimSpace(output.String()) == "" {
			return "", nil
		}
		return "", fmt.Errorf("read repair ref %s: %w: %s", ref, err, safeExcerpt(output.String()))
	}
	value := strings.TrimSpace(output.String())
	if !validGitCommitSHA(value) {
		return "", fmt.Errorf("repair ref %s resolved to invalid object ID", ref)
	}
	return value, nil
}

// sweepOrphanRepairRefs deletes only an exact ref/commit/binding named by a
// durable journal intent, old enough to exceed every supported command guard,
// and absent from current durable state. Foreign refs are never candidates.
func sweepOrphanRepairRefs(ctx context.Context, repositoryDir, repository string, store *Store, cutoff time.Time) error {
	commonDir, err := repairGitCommonDir(ctx, repositoryDir)
	if err != nil {
		return err
	}
	identity, err := repairRepositoryIdentity(ctx, repositoryDir, repository, commonDir)
	if err != nil {
		return err
	}
	state := store.Snapshot()
	referenced := make(map[string]bool)
	for _, attempt := range state.Attempts {
		if attempt == nil {
			continue
		}
		referenced[attempt.ToolSnapshotRef] = attempt.ToolSnapshotRef != ""
		referenced[attempt.PreparationCleanupRef] = attempt.PreparationCleanupRef != ""
		referenced[attempt.ModelBaseGuardRef] = attempt.ModelBaseGuardRef != ""
		referenced[attempt.CandidateGuardRef] = attempt.CandidateGuardRef != ""
	}
	entries, err := store.repairRefJournalEntries()
	if err != nil {
		return fmt.Errorf("read repair ref journal: %w", err)
	}
	consumed := make(map[string]bool)
	tombstoneConsumed := make(map[string]bool)
	tombstoneIntents := make(map[string]repairRefJournalEntry)
	tombstoneInitiated := make(map[string]bool)
	for _, entry := range entries {
		// Revocation is monotonic across repository-identity changes. A consumed
		// exact cryptographic authority may conservatively suppress cleanup in a
		// different/reconfigured repository (a harmless leak), but must never be
		// forgotten if origin A -> B -> A around a tombstone crash window.
		if entry.Phase == "consumed" {
			consumed[entry.Ref+"\x00"+entry.Commit+"\x00"+entry.Binding] = true
		} else if entry.Phase == "tombstone_consumed" {
			tombstoneConsumed[entry.Ref+"\x00"+entry.TombstoneRef+"\x00"+entry.Commit+"\x00"+entry.Binding] = true
		} else if entry.Phase == "tombstone_intent" {
			key := entry.Ref + "\x00" + entry.Commit + "\x00" + entry.Binding
			tombstoneInitiated[key] = true
			if entry.Repository != strings.TrimSpace(repository) || !sameRepairCommonDir(entry.GitCommonDir, commonDir) || entry.RepositoryIdentity != identity {
				continue
			}
			if prior, exists := tombstoneIntents[key]; exists && prior.TombstoneRef != entry.TombstoneRef {
				return fmt.Errorf("repair ref has multiple uncoordinated tombstone intents: %s", entry.Ref)
			}
			tombstoneIntents[key] = entry
		}
	}
	for key, entry := range tombstoneIntents {
		tombstoneKey := entry.Ref + "\x00" + entry.TombstoneRef + "\x00" + entry.Commit + "\x00" + entry.Binding
		if tombstoneConsumed[tombstoneKey] || referenced[entry.Ref] || !entry.Timestamp.Before(cutoff) {
			continue
		}
		if err := resumeRepairRefTombstone(ctx, repositoryDir, repository, store, entry, consumed[key]); err != nil {
			return fmt.Errorf("resume repair ref tombstone %s: %w", entry.TombstoneRef, err)
		}
		consumed[key] = true
		tombstoneConsumed[tombstoneKey] = true
	}
	for _, entry := range entries {
		if entry.Repository != strings.TrimSpace(repository) || !sameRepairCommonDir(entry.GitCommonDir, commonDir) || entry.RepositoryIdentity != identity {
			continue
		}
		if entry.Phase != "intent" {
			continue
		}
		key := entry.Ref + "\x00" + entry.Commit + "\x00" + entry.Binding
		if consumed[key] || referenced[entry.Ref] || !entry.Timestamp.Before(cutoff) {
			continue
		}
		if tombstoneInitiated[key] {
			continue
		}
		anchored, err := readRepairRef(ctx, repositoryDir, entry.Ref)
		if err != nil || anchored != entry.Commit {
			continue
		}
		subject, err := runGit(ctx, repositoryDir, "show", "-s", "--format=%s", entry.Commit)
		if err != nil {
			continue
		}
		expectedSubject := "Hive preparation recovery " + entry.Binding
		if entry.Kind == toolSnapshotPreparation || entry.Kind == toolSnapshotValidation {
			expectedSubject = "Hive repair tool snapshot " + entry.Binding
		} else if entry.Kind == sealedTreeModelBase || entry.Kind == sealedTreeCandidate {
			expectedSubject = "Hive repair sealed tree " + entry.Kind + " " + entry.Binding
		}
		if strings.TrimSpace(subject) != expectedSubject {
			continue
		}
		if err := retireRepairRef(ctx, repositoryDir, repository, store, entry.Kind, entry.Ref, entry.Commit, entry.Binding); err != nil {
			return fmt.Errorf("retire journaled orphan repair ref %s: %w", entry.Ref, err)
		}
		consumed[key] = true
	}
	return nil
}

func sameRepairCommonDir(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func repairGitCommonDir(ctx context.Context, worktree string) (string, error) {
	value, err := runGit(ctx, worktree, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolve repair Git common directory: %w", err)
	}
	value = strings.TrimSpace(value)
	if !filepath.IsAbs(value) {
		value = filepath.Join(worktree, value)
	}
	value, err = filepath.Abs(value)
	if err != nil {
		return "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(value); resolveErr == nil {
		value = resolved
	}
	return filepath.Clean(value), nil
}

func repairRepositoryIdentity(ctx context.Context, worktree, repository, commonDir string) (string, error) {
	remote, err := runGit(ctx, worktree, "config", "--get", "remote.origin.url")
	if err != nil {
		return "", fmt.Errorf("read repair repository origin identity: %w", err)
	}
	value := strings.Join([]string{filepath.Clean(commonDir), strings.TrimSpace(remote), strings.TrimSpace(repository)}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:]), nil
}
