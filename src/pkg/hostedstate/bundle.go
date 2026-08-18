// Package hostedstate creates authenticated, portable snapshots of Hive's
// hosted-controller state. A bundle is deliberately self-contained so the
// controller can move state between ephemeral runners without granting the
// target repository authority over lifecycle state.
package hostedstate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// SchemaVersion is the only hosted-state schema accepted by this package.
	SchemaVersion = "hive.hosted-state.v2"

	defaultMaxFiles      = 5000
	defaultMaxTotalBytes = int64(32 << 20)
	minimumSecretBytes   = 32
)

// Limits bounds a bundle before file contents are held in memory. Zero values
// select the production defaults. It is exported so callers can impose a
// stricter policy and so boundary behavior can be tested without large files.
type Limits struct {
	MaxFiles      int
	MaxTotalBytes int64
}

// DefaultLimits returns the production resource limits.
func DefaultLimits() Limits {
	return Limits{MaxFiles: defaultMaxFiles, MaxTotalBytes: defaultMaxTotalBytes}
}

// Metadata binds the portable state to one repository, one monotonic state
// transition, one integrated release, and one hosted controller execution.
type Metadata struct {
	Repository       RepositoryIdentity `json:"repository"`
	StateBranch      string             `json:"stateBranch"`
	Sequence         uint64             `json:"sequence"`
	PriorStateCommit string             `json:"priorStateCommit"`
	ExecutionOwner   string             `json:"executionOwner"`
	Release          ReleaseIdentity    `json:"release"`
	Controller       ControllerIdentity `json:"controller"`
	CreatedAt        time.Time          `json:"createdAt"`
}

type RepositoryIdentity struct {
	FullName string `json:"fullName"`
	ID       int64  `json:"id"`
}

type ReleaseIdentity struct {
	Version                    string `json:"version"`
	HiveCommit                 string `json:"hiveCommit"`
	VisualHiveCommit           string `json:"visualHiveCommit"`
	DistributionManifestSHA256 string `json:"distributionManifestSha256"`
	HostedControllerProtocol   int    `json:"hostedControllerProtocol"`
}

type ControllerIdentity struct {
	RunID          uint64 `json:"runId"`
	Attempt        uint64 `json:"attempt"`
	Event          string `json:"event"`
	WorkflowPath   string `json:"workflowPath"`
	WorkflowSHA    string `json:"workflowSha"`
	DefaultHeadSHA string `json:"defaultHeadSha"`
}

type File struct {
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	ContentBase64 string `json:"contentBase64"`
}

// Bundle is the signed wire representation. Signature is HMAC-SHA256 over the
// canonical JSON encoding of schemaVersion, metadata, and files, in that order.
type Bundle struct {
	SchemaVersion string   `json:"schemaVersion"`
	Metadata      Metadata `json:"metadata"`
	Files         []File   `json:"files"`
	Signature     string   `json:"signature"`
}

type unsignedBundle struct {
	SchemaVersion string   `json:"schemaVersion"`
	Metadata      Metadata `json:"metadata"`
	Files         []File   `json:"files"`
}

// ExportOptions selects every state-relative file that is allowed to exist in
// Root. Export fails if it finds any unlisted filesystem entry other than a
// directory, preventing accidental leakage of new controller state.
type ExportOptions struct {
	Root     string
	Paths    []string
	Metadata Metadata
	Secret   []byte
	Limits   Limits
}

// ExpectedState is the caller-owned trust context used during restore. Bundle
// contents never choose their own repository, sequence, predecessor, or
// release identity.
type ExpectedState struct {
	Repository       RepositoryIdentity
	StateBranch      string
	Sequence         uint64
	PriorStateCommit string
	Release          ReleaseIdentity
}

type RestoreOptions struct {
	Bundle      []byte
	Destination string
	Secret      []byte
	Expected    ExpectedState
	Limits      Limits
}

// Export returns the canonical JSON encoding of an authenticated state bundle.
func Export(opts ExportOptions) ([]byte, error) {
	limits, err := normalizedLimits(opts.Limits)
	if err != nil {
		return nil, err
	}
	if err := validateSecret(opts.Secret); err != nil {
		return nil, err
	}
	metadata := opts.Metadata
	metadata.CreatedAt = metadata.CreatedAt.UTC()
	if err := validateMetadata(metadata); err != nil {
		return nil, err
	}

	paths, selected, err := validateSelectedPaths(opts.Paths, limits)
	if err != nil {
		return nil, err
	}
	root, rootDevice, err := validateStateRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	if err := rejectUnlistedEntries(root, rootDevice, selected); err != nil {
		return nil, err
	}

	files := make([]File, 0, len(paths))
	var total int64
	for _, rel := range paths {
		entry, err := readSelectedFile(root, rootDevice, rel, limits.MaxTotalBytes-total)
		if err != nil {
			return nil, err
		}
		total += entry.Size
		if total > limits.MaxTotalBytes {
			return nil, fmt.Errorf("hosted state exceeds %d-byte limit", limits.MaxTotalBytes)
		}
		files = append(files, entry)
	}

	unsigned := unsignedBundle{SchemaVersion: SchemaVersion, Metadata: metadata, Files: files}
	canonical, err := json.Marshal(unsigned)
	if err != nil {
		return nil, fmt.Errorf("encode canonical hosted state: %w", err)
	}
	bundle := Bundle{
		SchemaVersion: unsigned.SchemaVersion,
		Metadata:      unsigned.Metadata,
		Files:         unsigned.Files,
		Signature:     sign(canonical, opts.Secret),
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encode hosted state bundle: %w", err)
	}
	return encoded, nil
}

// Decode verifies strict JSON syntax but does not authenticate the bundle. It
// is intended for diagnostics only; use Restore before trusting any content.
func Decode(data []byte) (Bundle, error) {
	var bundle Bundle
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, fmt.Errorf("decode hosted state bundle: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Bundle{}, fmt.Errorf("decode hosted state bundle: %w", err)
	}
	return bundle, nil
}

// Restore authenticates and completely validates a bundle before creating any
// destination content. Files are built in a private sibling directory and
// atomically renamed into place. On every error, no partial restored state is
// left behind.
func Restore(opts RestoreOptions) error {
	limits, err := normalizedLimits(opts.Limits)
	if err != nil {
		return err
	}
	if err := validateSecret(opts.Secret); err != nil {
		return err
	}
	bundle, err := Decode(opts.Bundle)
	if err != nil {
		return err
	}
	contents, err := authenticateAndValidate(bundle, opts.Secret, opts.Expected, limits)
	if err != nil {
		return err
	}

	destination, existed, err := validateEmptyDestination(opts.Destination)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(destination)+".hive-restore-")
	if err != nil {
		return fmt.Errorf("create restore staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o700); err != nil {
		return fmt.Errorf("protect restore staging directory: %w", err)
	}
	for i, file := range bundle.Files {
		path := filepath.Join(stage, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create restore directory for %q: %w", file.Path, err)
		}
		if err := enforcePrivateDirectories(stage, filepath.Dir(path)); err != nil {
			return err
		}
		handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create restored file %q: %w", file.Path, err)
		}
		if _, err := handle.Write(contents[i]); err != nil {
			_ = handle.Close()
			return fmt.Errorf("write restored file %q: %w", file.Path, err)
		}
		if err := handle.Sync(); err != nil {
			_ = handle.Close()
			return fmt.Errorf("sync restored file %q: %w", file.Path, err)
		}
		if err := handle.Close(); err != nil {
			return fmt.Errorf("close restored file %q: %w", file.Path, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("protect restored file %q: %w", file.Path, err)
		}
	}

	// Rename is atomic on supported filesystems when destination does not exist,
	// and on POSIX it also replaces an empty destination directory atomically.
	if err := os.Rename(stage, destination); err == nil {
		committed = true
		return nil
	} else if !existed {
		return fmt.Errorf("commit restored hosted state: %w", err)
	}

	// Windows cannot replace an existing empty directory. Revalidate it before
	// the narrow compatibility fallback, and recreate an empty private directory
	// if the final rename fails. It never exposes partial file content.
	if _, _, err := validateEmptyDestination(destination); err != nil {
		return fmt.Errorf("destination changed before restore commit: %w", err)
	}
	if err := os.Remove(destination); err != nil {
		return fmt.Errorf("prepare empty destination for restore commit: %w", err)
	}
	if err := os.Rename(stage, destination); err != nil {
		_ = os.Mkdir(destination, 0o700)
		return fmt.Errorf("commit restored hosted state: %w", err)
	}
	committed = true
	return nil
}

func authenticateAndValidate(bundle Bundle, secret []byte, expected ExpectedState, limits Limits) ([][]byte, error) {
	if bundle.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported hosted state schema %q", bundle.SchemaVersion)
	}
	if len(bundle.Files) > limits.MaxFiles {
		return nil, fmt.Errorf("hosted state contains %d files, limit is %d", len(bundle.Files), limits.MaxFiles)
	}
	unsigned := unsignedBundle{SchemaVersion: bundle.SchemaVersion, Metadata: bundle.Metadata, Files: bundle.Files}
	canonical, err := json.Marshal(unsigned)
	if err != nil {
		return nil, fmt.Errorf("encode canonical hosted state: %w", err)
	}
	wantSignature, err := decodeSignature(bundle.Signature)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	if !hmac.Equal(wantSignature, mac.Sum(nil)) {
		return nil, errors.New("hosted state signature does not match")
	}
	if err := validateMetadata(bundle.Metadata); err != nil {
		return nil, err
	}
	if err := validateExpected(bundle.Metadata, expected); err != nil {
		return nil, err
	}

	contents := make([][]byte, len(bundle.Files))
	seen := make(map[string]struct{}, len(bundle.Files))
	seenFolded := make(map[string]string, len(bundle.Files))
	previous := ""
	var total int64
	for i, file := range bundle.Files {
		path, err := validateRelativePath(file.Path)
		if err != nil {
			return nil, fmt.Errorf("invalid bundle file path: %w", err)
		}
		if path != file.Path {
			return nil, fmt.Errorf("bundle file path %q is not canonical", file.Path)
		}
		if i > 0 && file.Path <= previous {
			return nil, errors.New("hosted state file inventory is not strictly sorted")
		}
		previous = file.Path
		if err := registerPath(file.Path, seen, seenFolded); err != nil {
			return nil, err
		}
		if file.Size < 0 || file.Size > limits.MaxTotalBytes-total {
			return nil, fmt.Errorf("hosted state exceeds %d-byte limit", limits.MaxTotalBytes)
		}
		content, err := base64.StdEncoding.DecodeString(file.ContentBase64)
		if err != nil {
			return nil, fmt.Errorf("decode content for %q: %w", file.Path, err)
		}
		if int64(len(content)) != file.Size {
			return nil, fmt.Errorf("size mismatch for %q", file.Path)
		}
		digest := sha256.Sum256(content)
		if !strings.EqualFold(file.SHA256, hex.EncodeToString(digest[:])) || file.SHA256 != strings.ToLower(file.SHA256) {
			return nil, fmt.Errorf("SHA-256 mismatch for %q", file.Path)
		}
		total += file.Size
		contents[i] = content
	}
	return contents, nil
}

func validateExpected(actual Metadata, expected ExpectedState) error {
	if actual.Repository != expected.Repository {
		return errors.New("hosted state repository identity does not match")
	}
	if actual.StateBranch != expected.StateBranch {
		return errors.New("hosted state branch does not match")
	}
	if actual.Sequence != expected.Sequence {
		return errors.New("hosted state sequence does not match")
	}
	if actual.PriorStateCommit != expected.PriorStateCommit {
		return errors.New("hosted state prior commit does not match")
	}
	if actual.Release != expected.Release {
		return errors.New("hosted state release identity does not match")
	}
	return nil
}

func validateMetadata(metadata Metadata) error {
	repositoryParts := strings.Split(metadata.Repository.FullName, "/")
	if metadata.Repository.FullName != strings.TrimSpace(metadata.Repository.FullName) || len(repositoryParts) != 2 || repositoryParts[0] == "" || repositoryParts[1] == "" || strings.ContainsAny(metadata.Repository.FullName, "\r\n\x00") {
		return errors.New("repository full name must be a non-empty owner/name identity")
	}
	if metadata.Repository.ID <= 0 {
		return errors.New("repository numeric ID must be positive")
	}
	if err := validateText("state branch", metadata.StateBranch); err != nil {
		return err
	}
	if metadata.Sequence == 0 {
		return errors.New("hosted state sequence must be positive")
	}
	if metadata.Sequence == 1 && metadata.PriorStateCommit != "" {
		return errors.New("sequence 1 must not declare a prior state commit")
	}
	if metadata.Sequence > 1 && !validGitOID(metadata.PriorStateCommit) {
		return errors.New("sequence after 1 requires a valid prior state commit")
	}
	switch metadata.ExecutionOwner {
	case "hosted":
		if metadata.Controller.RunID == 0 || metadata.Controller.Attempt == 0 ||
			(metadata.Controller.Event != "schedule" && metadata.Controller.Event != "workflow_dispatch") {
			return errors.New("hosted execution requires a GitHub controller run, attempt, and trusted event")
		}
	case "bootstrap":
		if metadata.Sequence != 1 || metadata.PriorStateCommit != "" || metadata.Controller.RunID != 0 || metadata.Controller.Attempt != 0 || metadata.Controller.Event != "bootstrap" {
			return errors.New("bootstrap execution is allowed only for the orphan sequence-1 setup state")
		}
	default:
		return errors.New("execution owner must be hosted or the bounded initial bootstrap")
	}
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$`).MatchString(metadata.Release.Version) ||
		!validGitOID(metadata.Release.HiveCommit) || !validGitOID(metadata.Release.VisualHiveCommit) ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(metadata.Release.DistributionManifestSHA256) || metadata.Release.HostedControllerProtocol != 1 {
		return errors.New("release identity must bind an immutable integrated tag, full Hive and Visual Hive commits, manifest digest, and supported hosted controller protocol")
	}
	if err := validateText("controller event", metadata.Controller.Event); err != nil {
		return err
	}
	workflowPath, err := validateRelativePath(metadata.Controller.WorkflowPath)
	if err != nil || workflowPath != metadata.Controller.WorkflowPath {
		return errors.New("controller workflow path must be a safe slash-normalized relative path")
	}
	if !validGitOID(metadata.Controller.WorkflowSHA) || !validGitOID(metadata.Controller.DefaultHeadSHA) {
		return errors.New("controller workflow and default-head SHAs must be full Git object IDs")
	}
	if metadata.CreatedAt.IsZero() || metadata.CreatedAt.Location() != time.UTC {
		return errors.New("created timestamp must be a non-zero UTC timestamp")
	}
	return nil
}

func validateText(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s must be non-empty and contain no control delimiters", name)
	}
	return nil
}

func validGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func normalizedLimits(input Limits) (Limits, error) {
	limits := input
	if limits.MaxFiles == 0 {
		limits.MaxFiles = defaultMaxFiles
	}
	if limits.MaxTotalBytes == 0 {
		limits.MaxTotalBytes = defaultMaxTotalBytes
	}
	if limits.MaxFiles < 0 || limits.MaxTotalBytes < 0 {
		return Limits{}, errors.New("hosted state limits must be positive")
	}
	return limits, nil
}

func validateSecret(secret []byte) error {
	if len(secret) < minimumSecretBytes {
		return fmt.Errorf("hosted state HMAC secret must contain at least %d bytes", minimumSecretBytes)
	}
	return nil
}

func validateSelectedPaths(input []string, limits Limits) ([]string, map[string]struct{}, error) {
	if len(input) > limits.MaxFiles {
		return nil, nil, fmt.Errorf("selected %d hosted state files, limit is %d", len(input), limits.MaxFiles)
	}
	paths := make([]string, 0, len(input))
	selected := make(map[string]struct{}, len(input))
	folded := make(map[string]string, len(input))
	for _, candidate := range input {
		path, err := validateRelativePath(candidate)
		if err != nil {
			return nil, nil, err
		}
		if err := registerPath(path, selected, folded); err != nil {
			return nil, nil, err
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, selected, nil
}

func registerPath(path string, exact map[string]struct{}, folded map[string]string) error {
	if _, exists := exact[path]; exists {
		return fmt.Errorf("duplicate hosted state path %q", path)
	}
	key := strings.ToLower(path)
	if prior, exists := folded[key]; exists {
		return fmt.Errorf("case-fold collision between hosted state paths %q and %q", prior, path)
	}
	exact[path] = struct{}{}
	folded[key] = path
	return nil
}

func validateRelativePath(input string) (string, error) {
	if input == "" || strings.Contains(input, "\\") || strings.ContainsRune(input, '\x00') {
		return "", fmt.Errorf("hosted state path %q is empty, contains a backslash, or contains NUL", input)
	}
	if strings.HasPrefix(input, "/") || filepath.IsAbs(input) {
		return "", fmt.Errorf("hosted state path %q must be relative", input)
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(input)))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != input {
		return "", fmt.Errorf("hosted state path %q is not a safe canonical relative path", input)
	}
	for _, component := range strings.Split(input, "/") {
		if component == "" || component == "." || component == ".." ||
			strings.ContainsAny(component, `<>:"|?*`) || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") ||
			isWindowsReservedName(component) {
			return "", fmt.Errorf("hosted state path %q has an unsafe component", input)
		}
		for _, character := range component {
			if character < 0x20 || character == 0x7f {
				return "", fmt.Errorf("hosted state path %q contains a control character", input)
			}
		}
	}
	return cleaned, nil
}

func isWindowsReservedName(component string) bool {
	base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

func validateStateRoot(input string) (string, uint64, error) {
	if input == "" {
		return "", 0, errors.New("hosted state root is required")
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", 0, fmt.Errorf("resolve hosted state root: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", 0, fmt.Errorf("inspect hosted state root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", 0, errors.New("hosted state root must be an ordinary directory, not a symlink")
	}
	if err := validateSafePermissions(abs, info, true); err != nil {
		return "", 0, err
	}
	device, hasDevice, _, _, err := platformFileMetadata(abs, info)
	if err != nil {
		return "", 0, fmt.Errorf("inspect hosted state root identity: %w", err)
	}
	if !hasDevice {
		device = 0
	}
	return abs, device, nil
}

func rejectUnlistedEntries(root string, rootDevice uint64, selected map[string]struct{}) error {
	found := make(map[string]struct{}, len(selected))
	err := filepath.Walk(root, func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, err := validateRelativePath(rel); err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("hosted state entry %q is a symlink", rel)
		}
		if info.IsDir() {
			if err := validateSafePermissions(path, info, true); err != nil {
				return err
			}
			if err := validateDevice(path, info, rootDevice); err != nil {
				return err
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("hosted state entry %q is not a regular file", rel)
		}
		if _, ok := selected[rel]; !ok {
			return fmt.Errorf("hosted state contains unlisted file %q", rel)
		}
		found[rel] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("inspect hosted state rooted at %q: %w", root, err)
	}
	for path := range selected {
		if _, ok := found[path]; !ok {
			return fmt.Errorf("selected hosted state file %q does not exist", path)
		}
	}
	return nil
}

func readSelectedFile(root string, rootDevice uint64, rel string, remaining int64) (File, error) {
	path := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	if err != nil {
		return File{}, fmt.Errorf("inspect selected hosted state file %q: %w", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("selected hosted state file %q is not an ordinary regular file", rel)
	}
	if err := validateSafePermissions(path, info, false); err != nil {
		return File{}, err
	}
	if err := validateDeviceAndLinks(path, info, rootDevice); err != nil {
		return File{}, err
	}
	if info.Size() < 0 || info.Size() > remaining {
		return File{}, fmt.Errorf("hosted state exceeds total byte limit while reading %q", rel)
	}
	handle, err := os.Open(path)
	if err != nil {
		return File{}, fmt.Errorf("open selected hosted state file %q: %w", rel, err)
	}
	defer handle.Close()
	openedInfo, err := handle.Stat()
	if err != nil {
		return File{}, fmt.Errorf("restat selected hosted state file %q: %w", rel, err)
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return File{}, fmt.Errorf("selected hosted state file %q changed while opening", rel)
	}
	if err := validateDeviceAndLinks(path, openedInfo, rootDevice); err != nil {
		return File{}, err
	}
	content, err := io.ReadAll(io.LimitReader(handle, remaining+1))
	if err != nil {
		return File{}, fmt.Errorf("read selected hosted state file %q: %w", rel, err)
	}
	if int64(len(content)) > remaining || int64(len(content)) != openedInfo.Size() {
		return File{}, fmt.Errorf("selected hosted state file %q changed size or exceeded the limit", rel)
	}
	digest := sha256.Sum256(content)
	return File{
		Path:          rel,
		SHA256:        hex.EncodeToString(digest[:]),
		Size:          int64(len(content)),
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}, nil
}

func validateDevice(path string, info fs.FileInfo, rootDevice uint64) error {
	device, hasDevice, _, _, err := platformFileMetadata(path, info)
	if err != nil {
		return fmt.Errorf("inspect device for %q: %w", path, err)
	}
	if hasDevice && device != rootDevice {
		return fmt.Errorf("hosted state entry %q escapes the root device", path)
	}
	return nil
}

func validateDeviceAndLinks(path string, info fs.FileInfo, rootDevice uint64) error {
	device, hasDevice, links, hasLinks, err := platformFileMetadata(path, info)
	if err != nil {
		return fmt.Errorf("inspect identity for %q: %w", path, err)
	}
	if hasDevice && device != rootDevice {
		return fmt.Errorf("hosted state file %q escapes the root device", path)
	}
	if hasLinks && links != 1 {
		return fmt.Errorf("hosted state file %q has %d hard links", path, links)
	}
	return nil
}

func validateSafePermissions(path string, info fs.FileInfo, directory bool) error {
	if !permissionSafetyApplicable() {
		return nil
	}
	if info.Mode().Perm()&0o077 != 0 {
		kind := "file"
		if directory {
			kind = "directory"
		}
		return fmt.Errorf("hosted state %s %q grants group or other permissions", kind, path)
	}
	return nil
}

func validateEmptyDestination(input string) (string, bool, error) {
	if input == "" {
		return "", false, errors.New("restore destination is required")
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", false, fmt.Errorf("resolve restore destination: %w", err)
	}
	if filepath.Clean(abs) == filepath.Clean(filepath.Dir(abs)) {
		return "", false, errors.New("restore destination must not be a filesystem root")
	}
	parent := filepath.Dir(abs)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", false, fmt.Errorf("inspect restore destination parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return "", false, errors.New("restore destination parent must be an ordinary directory")
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return abs, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect restore destination: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", false, errors.New("restore destination must be absent or an ordinary empty directory")
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", false, fmt.Errorf("inspect restore destination contents: %w", err)
	}
	if len(entries) != 0 {
		return "", false, errors.New("restore destination must be empty")
	}
	return abs, true, nil
}

func enforcePrivateDirectories(root, leaf string) error {
	current := leaf
	for {
		if err := os.Chmod(current, 0o700); err != nil {
			return fmt.Errorf("protect restored directory: %w", err)
		}
		if current == root {
			return nil
		}
		next := filepath.Dir(current)
		if next == current {
			return errors.New("restored directory escaped staging root")
		}
		current = next
	}
}

func sign(canonical, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(canonical)
	return "hmac-sha256:" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func decodeSignature(value string) ([]byte, error) {
	const prefix = "hmac-sha256:"
	if !strings.HasPrefix(value, prefix) {
		return nil, errors.New("hosted state signature has an unsupported algorithm")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("hosted state signature is malformed")
	}
	return decoded, nil
}
