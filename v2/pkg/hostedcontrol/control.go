// Package hostedcontrol persists Hive's hosted controller state in one
// authenticated, content-bounded file on a dedicated Git state branch. It is
// deliberately independent of GitHub's API: the controller checks out the
// branch with its normal GitHub Actions credential and this package performs
// exact-parent compare-and-swap pushes with git.
package hostedcontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/checkpoint"
	"github.com/kubestellar/hive/v2/pkg/hostedstate"
)

const (
	bundleFile            = "state.json"
	defaultRemote         = "origin"
	defaultCommandTimeout = 2 * time.Minute
	maxGitOutput          = 1 << 20
)

var safeRemoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Config is the caller-owned trust context for one hosted controller run.
// CheckoutDir must be a checkout whose HEAD is the exact remote state-branch
// head (or an initialized repository when that branch does not yet exist).
// RuntimeRoot is a separate, empty location populated only after bundle
// authentication succeeds.
type Config struct {
	CheckoutDir string
	RuntimeRoot string
	Remote      string
	StateBranch string
	Secret      []byte
	Repository  hostedstate.RepositoryIdentity
	Release     hostedstate.ReleaseIdentity
	// RestoreRelease explicitly authorizes one release transition. When set,
	// Open requires the stored bundle to have this identity; the next
	// Checkpoint writes Release. Omitting it requires an exact same-release
	// restore. This keeps upgrade and rollback transitions explicit.
	RestoreRelease      *hostedstate.ReleaseIdentity
	Controller          hostedstate.ControllerIdentity
	TrustedWorkflowPath string
	Limits              hostedstate.Limits
	CommandTimeout      time.Duration
	GitExecutable       string
	Now                 func() time.Time
}

// Manager serializes restore/checkpoint operations for one controller. A
// Manager is not safe to copy.
type Manager struct {
	mu       sync.Mutex
	config   Config
	head     string
	sequence uint64
}

// Open authenticates and restores the current state-branch bundle. For a new
// branch it creates an empty private runtime root; the first Checkpoint creates
// sequence 1 as a root commit. No target checkout, worktree, evidence artifact,
// runtime cache, lock, or local daemon state is restored.
func Open(ctx context.Context, config Config) (*Manager, error) {
	normalized, err := normalizeConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	manager := &Manager{config: normalized}

	head, exists, err := manager.remoteHead(ctx)
	if err != nil {
		return nil, err
	}
	localHead, localExists, err := manager.localHead(ctx)
	if err != nil {
		return nil, err
	}
	if exists != localExists || (exists && head != localHead) {
		return nil, fmt.Errorf("state checkout HEAD does not exactly match remote refs/heads/%s", normalized.StateBranch)
	}
	if !exists {
		if err := prepareRuntimeRootForNewBranch(normalized.RuntimeRoot); err != nil {
			return nil, err
		}
		return manager, nil
	}

	if err := manager.validateCommitShape(ctx, head); err != nil {
		return nil, err
	}
	encoded, err := manager.git(ctx, nil, "show", head+":"+bundleFile)
	if err != nil {
		return nil, fmt.Errorf("read hosted state bundle from %s: %w", head, err)
	}
	bundle, err := hostedstate.Decode(encoded)
	if err != nil {
		return nil, err
	}
	parents, err := manager.commitParents(ctx, head)
	if err != nil {
		return nil, err
	}
	if bundle.Metadata.Sequence == 1 {
		if len(parents) != 0 || bundle.Metadata.PriorStateCommit != "" {
			return nil, errors.New("hosted state sequence 1 must be a root commit")
		}
	} else if len(parents) != 1 || parents[0] != bundle.Metadata.PriorStateCommit {
		return nil, errors.New("hosted state commit parent does not match the signed prior state commit")
	}
	if bundle.Metadata.Controller.WorkflowPath != normalized.TrustedWorkflowPath {
		return nil, fmt.Errorf("hosted state was written by untrusted workflow path %q", bundle.Metadata.Controller.WorkflowPath)
	}
	if bundle.Metadata.Sequence > 1 {
		parent, err := manager.authenticatedParentMetadata(ctx, bundle.Metadata.PriorStateCommit)
		if err != nil {
			return nil, err
		}
		if parent.Sequence+1 != bundle.Metadata.Sequence {
			return nil, errors.New("hosted state sequence is not exactly one greater than its authenticated parent")
		}
	}
	restoreRelease := normalized.Release
	if bundle.Metadata.Release == normalized.Release {
		// Normal steady-state restore.
	} else if normalized.RestoreRelease != nil && bundle.Metadata.Release == *normalized.RestoreRelease {
		restoreRelease = *normalized.RestoreRelease
	} else {
		return nil, errors.New("hosted state release is neither the current immutable release nor its explicitly authorized transition predecessor")
	}
	expected := hostedstate.ExpectedState{
		Repository:       normalized.Repository,
		StateBranch:      normalized.StateBranch,
		Sequence:         bundle.Metadata.Sequence,
		PriorStateCommit: bundle.Metadata.PriorStateCommit,
		Release:          restoreRelease,
	}
	if err := hostedstate.Restore(hostedstate.RestoreOptions{
		Bundle: encoded, Destination: normalized.RuntimeRoot, Secret: normalized.Secret,
		Expected: expected, Limits: normalized.Limits,
	}); err != nil {
		return nil, err
	}
	manager.head = head
	manager.sequence = bundle.Metadata.Sequence
	return manager, nil
}

// Callback has the exact function shape accepted by checkpoint.WithCallback.
// The compile-time assignment prevents the two packages from silently drifting.
func (manager *Manager) Callback(ctx context.Context, detail string) error {
	return manager.Checkpoint(ctx, detail)
}

var _ checkpoint.Callback = (*Manager)(nil).Callback

// Checkpoint snapshots the portable durable inventory, signs it, creates an
// exact-parent commit containing only state.json, and pushes with an exact
// force-with-lease. Any remote race fails closed and leaves manager state at the
// last confirmed remote commit.
func (manager *Manager) Checkpoint(ctx context.Context, detail string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	remoteHead, exists, err := manager.remoteHead(ctx)
	if err != nil {
		return err
	}
	if (manager.head == "") != !exists || (exists && remoteHead != manager.head) {
		return fmt.Errorf("hosted state compare-and-swap precondition failed: remote head changed")
	}

	paths, err := PortableInventory(manager.config.RuntimeRoot)
	if err != nil {
		return err
	}
	stage, err := stagePortableFiles(manager.config.RuntimeRoot, paths)
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	nextSequence := manager.sequence + 1
	metadata := hostedstate.Metadata{
		Repository:       manager.config.Repository,
		StateBranch:      manager.config.StateBranch,
		Sequence:         nextSequence,
		PriorStateCommit: manager.head,
		ExecutionOwner:   "hosted",
		Release:          manager.config.Release,
		Controller:       manager.config.Controller,
		CreatedAt:        manager.config.Now().UTC(),
	}
	encoded, err := hostedstate.Export(hostedstate.ExportOptions{
		Root: stage, Paths: paths, Metadata: metadata, Secret: manager.config.Secret, Limits: manager.config.Limits,
	})
	if err != nil {
		return err
	}
	if err := verifyStagedFilesUnchanged(manager.config.RuntimeRoot, stage, paths); err != nil {
		return err
	}

	commit, err := manager.createCommit(ctx, encoded, nextSequence, detail)
	if err != nil {
		return err
	}
	if err := manager.pushCAS(ctx, commit); err != nil {
		return err
	}
	confirmed, exists, err := manager.remoteHead(ctx)
	if err != nil {
		return fmt.Errorf("verify hosted state push: %w", err)
	}
	if !exists || confirmed != commit {
		return errors.New("hosted state push did not establish the exact expected remote commit")
	}
	manager.head = commit
	manager.sequence = nextSequence
	if err := writeBundleMirror(manager.config.CheckoutDir, encoded); err != nil {
		return fmt.Errorf("mirror confirmed hosted state bundle: %w", err)
	}
	return nil
}

func (manager *Manager) Sequence() uint64 {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.sequence
}

func (manager *Manager) HeadSHA() string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.head
}

func normalizeConfig(ctx context.Context, config Config) (Config, error) {
	var err error
	checkoutInput := strings.TrimSpace(config.CheckoutDir)
	if checkoutInput == "" {
		return Config{}, errors.New("hosted state checkout directory is required")
	}
	config.CheckoutDir, err = filepath.Abs(checkoutInput)
	if err != nil {
		return Config{}, fmt.Errorf("resolve hosted state checkout directory: %w", err)
	}
	runtimeInput := strings.TrimSpace(config.RuntimeRoot)
	if runtimeInput == "" {
		return Config{}, errors.New("hosted runtime root is required")
	}
	config.RuntimeRoot, err = filepath.Abs(runtimeInput)
	if err != nil {
		return Config{}, fmt.Errorf("resolve hosted runtime root: %w", err)
	}
	if pathsOverlap(config.CheckoutDir, config.RuntimeRoot) {
		return Config{}, errors.New("hosted runtime root and state-branch checkout must be disjoint")
	}
	info, err := os.Lstat(config.CheckoutDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Config{}, errors.New("hosted state checkout must be an ordinary directory")
	}
	if resolved, err := filepath.EvalSymlinks(config.CheckoutDir); err != nil || !samePath(resolved, config.CheckoutDir) {
		return Config{}, errors.New("hosted state checkout must not resolve through a link or reparse point")
	}
	if config.Remote == "" {
		config.Remote = defaultRemote
	}
	if !safeRemoteName.MatchString(config.Remote) {
		return Config{}, errors.New("hosted state Git remote name is unsafe")
	}
	if config.StateBranch == "" || strings.HasPrefix(config.StateBranch, "-") || strings.HasPrefix(config.StateBranch, "refs/") {
		return Config{}, errors.New("hosted state branch must be a non-empty branch name without refs/ prefix")
	}
	if config.TrustedWorkflowPath == "" {
		config.TrustedWorkflowPath = config.Controller.WorkflowPath
	}
	if config.TrustedWorkflowPath == "" {
		return Config{}, errors.New("trusted hosted workflow path is required")
	}
	if config.CommandTimeout == 0 {
		config.CommandTimeout = defaultCommandTimeout
	}
	if config.CommandTimeout < time.Second || config.CommandTimeout > 10*time.Minute {
		return Config{}, errors.New("hosted Git command timeout must be between one second and ten minutes")
	}
	if config.GitExecutable == "" {
		config.GitExecutable = "git"
	}
	config.Secret = append([]byte(nil), config.Secret...)
	if config.RestoreRelease != nil {
		copy := *config.RestoreRelease
		config.RestoreRelease = &copy
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	probe := &Manager{config: config}
	if _, err := probe.git(ctx, nil, "check-ref-format", "refs/heads/"+config.StateBranch); err != nil {
		return Config{}, fmt.Errorf("invalid hosted state branch: %w", err)
	}
	if _, err := probe.git(ctx, nil, "remote", "get-url", config.Remote); err != nil {
		return Config{}, fmt.Errorf("hosted state remote is unavailable: %w", err)
	}
	return config, nil
}

func (manager *Manager) validateCommitShape(ctx context.Context, commit string) error {
	output, err := manager.git(ctx, nil, "ls-tree", "-z", commit)
	if err != nil {
		return err
	}
	parts := bytes.Split(output, []byte{0})
	if len(parts) != 2 || len(parts[1]) != 0 {
		return errors.New("hosted state commit tree must contain exactly state.json")
	}
	fields := bytes.Fields(parts[0])
	if len(fields) != 4 || string(fields[0]) != "100644" || string(fields[1]) != "blob" || string(fields[3]) != bundleFile || !validOID(string(fields[2])) {
		return errors.New("hosted state commit tree must contain state.json as one ordinary non-executable blob")
	}
	return nil
}

func (manager *Manager) localHead(ctx context.Context) (string, bool, error) {
	output, err := manager.git(ctx, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(string(output)), true, nil
}

func (manager *Manager) remoteHead(ctx context.Context) (string, bool, error) {
	output, err := manager.git(ctx, nil, "ls-remote", "--heads", manager.config.Remote, "refs/heads/"+manager.config.StateBranch)
	if err != nil {
		return "", false, fmt.Errorf("read remote hosted state head: %w", err)
	}
	line := strings.TrimSpace(string(output))
	if line == "" {
		return "", false, nil
	}
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[1] != "refs/heads/"+manager.config.StateBranch || !validOID(fields[0]) {
		return "", false, errors.New("remote hosted state ref response is ambiguous")
	}
	return fields[0], true, nil
}

func (manager *Manager) commitParents(ctx context.Context, commit string) ([]string, error) {
	output, err := manager.git(ctx, nil, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(output))
	if len(fields) < 1 || fields[0] != commit {
		return nil, errors.New("could not bind hosted state commit ancestry")
	}
	return fields[1:], nil
}

func (manager *Manager) authenticatedParentMetadata(ctx context.Context, commit string) (hostedstate.Metadata, error) {
	if err := manager.validateCommitShape(ctx, commit); err != nil {
		return hostedstate.Metadata{}, fmt.Errorf("validate authenticated parent commit: %w", err)
	}
	encoded, err := manager.git(ctx, nil, "show", commit+":"+bundleFile)
	if err != nil {
		return hostedstate.Metadata{}, fmt.Errorf("read authenticated parent bundle: %w", err)
	}
	bundle, err := hostedstate.Decode(encoded)
	if err != nil {
		return hostedstate.Metadata{}, fmt.Errorf("decode authenticated parent bundle: %w", err)
	}
	if bundle.Metadata.Repository != manager.config.Repository || bundle.Metadata.StateBranch != manager.config.StateBranch {
		return hostedstate.Metadata{}, errors.New("authenticated parent bundle has a different repository or state branch")
	}
	if bundle.Metadata.Controller.WorkflowPath != manager.config.TrustedWorkflowPath {
		return hostedstate.Metadata{}, errors.New("authenticated parent bundle has an untrusted workflow path")
	}
	parents, err := manager.commitParents(ctx, commit)
	if err != nil {
		return hostedstate.Metadata{}, err
	}
	if bundle.Metadata.Sequence == 1 {
		if len(parents) != 0 || bundle.Metadata.PriorStateCommit != "" {
			return hostedstate.Metadata{}, errors.New("authenticated parent sequence 1 is not a root commit")
		}
	} else if len(parents) != 1 || parents[0] != bundle.Metadata.PriorStateCommit {
		return hostedstate.Metadata{}, errors.New("authenticated parent ancestry does not match its signed predecessor")
	}
	temporary, err := os.MkdirTemp("", "hive-hosted-parent-")
	if err != nil {
		return hostedstate.Metadata{}, err
	}
	defer os.RemoveAll(temporary)
	expected := hostedstate.ExpectedState{
		Repository: bundle.Metadata.Repository, StateBranch: bundle.Metadata.StateBranch,
		Sequence: bundle.Metadata.Sequence, PriorStateCommit: bundle.Metadata.PriorStateCommit, Release: bundle.Metadata.Release,
	}
	if err := hostedstate.Restore(hostedstate.RestoreOptions{
		Bundle: encoded, Destination: filepath.Join(temporary, "state"), Secret: manager.config.Secret,
		Expected: expected, Limits: manager.config.Limits,
	}); err != nil {
		return hostedstate.Metadata{}, fmt.Errorf("authenticate parent hosted state: %w", err)
	}
	return bundle.Metadata, nil
}

func (manager *Manager) createCommit(ctx context.Context, encoded []byte, sequence uint64, detail string) (string, error) {
	blobOutput, err := manager.git(ctx, encoded, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	blob := strings.TrimSpace(string(blobOutput))
	if !validOID(blob) {
		return "", errors.New("git returned an invalid hosted state blob ID")
	}
	treeLine := []byte("100644 blob " + blob + "\t" + bundleFile + "\n")
	treeOutput, err := manager.git(ctx, treeLine, "mktree")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(treeOutput))
	if !validOID(tree) {
		return "", errors.New("git returned an invalid hosted state tree ID")
	}
	message := hostedCommitMessage(sequence, manager.config.Repository.ID, manager.config.Controller.RunID, detail)
	args := []string{"commit-tree", tree}
	if manager.head != "" {
		args = append(args, "-p", manager.head)
	}
	environment := []string{
		"GIT_AUTHOR_NAME=Hive Hosted Controller", "GIT_AUTHOR_EMAIL=hive-hosted@users.noreply.github.com",
		"GIT_COMMITTER_NAME=Hive Hosted Controller", "GIT_COMMITTER_EMAIL=hive-hosted@users.noreply.github.com",
	}
	commitOutput, err := manager.gitWithEnvironment(ctx, []byte(message), environment, args...)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(commitOutput))
	if !validOID(commit) {
		return "", errors.New("git returned an invalid hosted state commit ID")
	}
	parents, err := manager.commitParents(ctx, commit)
	if err != nil {
		return "", err
	}
	if manager.head == "" && len(parents) != 0 || manager.head != "" && (len(parents) != 1 || parents[0] != manager.head) {
		return "", errors.New("created hosted state commit does not have the exact expected parent")
	}
	return commit, nil
}

func (manager *Manager) pushCAS(ctx context.Context, commit string) error {
	ref := "refs/heads/" + manager.config.StateBranch
	args := []string{"push", "--porcelain"}
	if manager.head != "" {
		args = append(args, "--force-with-lease="+ref+":"+manager.head)
	}
	args = append(args, manager.config.Remote, commit+":"+ref)
	if _, err := manager.git(ctx, nil, args...); err != nil {
		return fmt.Errorf("compare-and-swap push hosted state: %w", err)
	}
	return nil
}

func (manager *Manager) git(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return manager.gitWithEnvironment(ctx, stdin, nil, args...)
}

func (manager *Manager) gitWithEnvironment(ctx context.Context, stdin []byte, environment []string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, manager.config.CommandTimeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, manager.config.GitExecutable, append([]string{"-C", manager.config.CheckoutDir}, args...)...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	command.Env = append(os.Environ(), environment...)
	var stdout, stderr boundedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("git %s timed out after %s", args[0], manager.config.CommandTimeout)
		}
		return nil, fmt.Errorf("git %s failed: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("git %s exceeded the %d-byte output limit", args[0], maxGitOutput)
	}
	return stdout.Bytes(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	length := len(value)
	remaining := maxGitOutput - buffer.Len()
	if remaining > 0 {
		if remaining > length {
			remaining = length
		}
		_, _ = buffer.Buffer.Write(value[:remaining])
	}
	if length > remaining {
		buffer.overflow = true
	}
	return length, nil
}

func hostedCommitMessage(sequence uint64, repositoryID int64, runID uint64, detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if detail == "" {
		detail = "state barrier"
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return fmt.Sprintf("hive hosted checkpoint %d\n\nHive-State-Sequence: %d\nHive-Repository-ID: %d\nHive-Controller-Run-ID: %d\nHive-Checkpoint-Detail: %s\n",
		sequence, sequence, repositoryID, runID, detail)
}

func writeBundleMirror(checkout string, encoded []byte) error {
	path := filepath.Join(checkout, bundleFile)
	temporary, err := os.CreateTemp(checkout, ".state-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
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
	return os.Rename(temporaryName, path)
}

func prepareRuntimeRootForNewBranch(root string) error {
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("hosted runtime root must be an ordinary directory")
		}
		if err := os.Chmod(root, 0o700); err != nil {
			return err
		}
		// Setup may have created the initial durable configuration before the
		// state branch exists. Classify it now so sequence 1 cannot silently
		// capture an unsafe or unrecognized file.
		_, err := PortableInventory(root)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.MkdirAll(root, 0o700)
}

func pathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if samePath(left, right) {
		return true
	}
	separator := string(filepath.Separator)
	leftPrefix := left + separator
	rightPrefix := right + separator
	if pathCaseInsensitive() {
		leftPrefix, rightPrefix = strings.ToLower(leftPrefix), strings.ToLower(rightPrefix)
		left, right = strings.ToLower(left), strings.ToLower(right)
	}
	return strings.HasPrefix(right, leftPrefix) || strings.HasPrefix(left, rightPrefix)
}

func samePath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if pathCaseInsensitive() {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func validOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := strconv.ParseUint(value[:16], 16, 64)
	if err != nil || value != strings.ToLower(value) {
		return false
	}
	for _, character := range value[16:] {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}
