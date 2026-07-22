package integrated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// SetupAuthorizationSchema is the canonical payload signed by Hive's
	// out-of-band exact-head commit status.
	SetupAuthorizationSchema = "hive.setup-pr-authorization.v1"
	// SetupAuthorizationDiffAlgorithm is deliberately object based. Unlike a
	// textual patch, this encoding does not depend on Git's hunk heuristics,
	// attributes, locale, or installed Git version.
	SetupAuthorizationDiffAlgorithm = "hive.setup-diff-tree-blobs.v1+sha256"
	// SetupAuthorizationContextPrefix namespaces the legacy commit status that
	// authorizes one exact managed setup pull request.
	SetupAuthorizationContextPrefix = "hive/setup-authorized/"

	setupAuthorizationDiffDomain = "hive.setup-diff-tree-blobs.v1"
)

var (
	setupAuthorizationSHA     = regexp.MustCompile(`^[a-f0-9]{40}$`)
	setupAuthorizationDigest  = regexp.MustCompile(`^[a-f0-9]{64}$`)
	setupAuthorizationID      = regexp.MustCompile(`^[1-9][0-9]*$`)
	setupAuthorizationRepo    = regexp.MustCompile(`^[a-z0-9_.-]+/[a-z0-9_.-]+$`)
	setupAuthorizationRefPart = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	setupAuthorizationOID     = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
)

// SetupAuthorizationFile binds one required regular file to its raw Git blob.
// BlobOID is included for repository auditability, while ContentSHA256 avoids
// treating Git's object hash as the content-integrity boundary.
type SetupAuthorizationFile struct {
	Path          string `json:"path"`
	Mode          string `json:"mode"`
	BlobOID       string `json:"blob_oid"`
	Size          int64  `json:"size"`
	ContentSHA256 string `json:"content_sha256"`
}

// SetupAuthorizationBinding is serialized with encoding/json in this exact
// field order and without a trailing newline. Workflow-side implementations
// must construct properties in the same order before JSON.stringify. All
// strings are restricted to an ASCII subset, so Go and JavaScript escaping are
// identical.
type SetupAuthorizationBinding struct {
	SchemaVersion  string                   `json:"schema_version"`
	Repository     string                   `json:"repository"`
	RepositoryID   string                   `json:"repository_id"`
	PullRequest    int                      `json:"pull_request"`
	HeadRef        string                   `json:"head_ref"`
	HeadSHA        string                   `json:"head_sha"`
	BaseRef        string                   `json:"base_ref"`
	BaseSHA        string                   `json:"base_sha"`
	AuthorizerID   string                   `json:"authorizer_id"`
	DiffAlgorithm  string                   `json:"diff_algorithm"`
	DiffSHA256     string                   `json:"diff_sha256"`
	RequiredFiles  []SetupAuthorizationFile `json:"required_files"`
	RequiredAbsent []string                 `json:"required_absent"`
}

// SetupAuthorizationRequest contains every live value needed to bind one
// setup/upgrade pull request. RequiredPresent and RequiredAbsent are exact Git
// paths, not globs.
type SetupAuthorizationRequest struct {
	CheckoutDir     string
	Repository      string
	RepositoryID    string
	PullRequest     int
	HeadRef         string
	HeadSHA         string
	BaseRef         string
	BaseSHA         string
	AuthorizerID    int64
	RequiredPresent []string
	RequiredAbsent  []string
}

// SetupDiffEvidence is the stable digest and exact no-renames path set for a
// base/head pair.
type SetupDiffEvidence struct {
	Algorithm    string   `json:"algorithm"`
	SHA256       string   `json:"sha256"`
	ChangedPaths []string `json:"changed_paths"`
	ChangeCount  int      `json:"change_count"`
}

type setupDiffRecord struct {
	Status  string
	OldMode string
	NewMode string
	OldOID  string
	NewOID  string
	Path    []byte
}

// BuildSetupAuthorizationBinding reads only immutable commit objects. It never
// trusts the checkout worktree or index after the managed head is committed.
func BuildSetupAuthorizationBinding(ctx context.Context, request SetupAuthorizationRequest) (SetupAuthorizationBinding, error) {
	binding, _, err := BuildSetupAuthorizationBindingWithDiff(ctx, request)
	return binding, err
}

// BuildSetupAuthorizationBindingWithDiff also returns the exact no-renames
// changed-path set so the setup caller can enforce its independently reviewed
// managed-path allowlist without hashing every old/new blob a second time.
func BuildSetupAuthorizationBindingWithDiff(ctx context.Context, request SetupAuthorizationRequest) (SetupAuthorizationBinding, SetupDiffEvidence, error) {
	repository := strings.ToLower(strings.TrimSpace(request.Repository))
	headSHA := strings.ToLower(strings.TrimSpace(request.HeadSHA))
	baseSHA := strings.ToLower(strings.TrimSpace(request.BaseSHA))
	if strings.TrimSpace(request.CheckoutDir) == "" {
		return SetupAuthorizationBinding{}, SetupDiffEvidence{}, fmt.Errorf("setup authorization checkout directory is required")
	}
	if !setupAuthorizationRepo.MatchString(repository) || !setupAuthorizationID.MatchString(strings.TrimSpace(request.RepositoryID)) || request.PullRequest <= 0 || request.AuthorizerID <= 0 {
		return SetupAuthorizationBinding{}, SetupDiffEvidence{}, fmt.Errorf("setup authorization repository, immutable repository ID, pull request, and authorizer ID are required")
	}
	if !validSetupAuthorizationRef(request.HeadRef) || !validSetupAuthorizationRef(request.BaseRef) || !setupAuthorizationSHA.MatchString(headSHA) || !setupAuthorizationSHA.MatchString(baseSHA) {
		return SetupAuthorizationBinding{}, SetupDiffEvidence{}, fmt.Errorf("setup authorization requires safe refs and exact lowercase 40-character base/head SHAs")
	}

	diff, err := ComputeSetupDiffEvidence(ctx, request.CheckoutDir, baseSHA, headSHA)
	if err != nil {
		return SetupAuthorizationBinding{}, SetupDiffEvidence{}, err
	}
	if diff.ChangeCount == 0 {
		return SetupAuthorizationBinding{}, SetupDiffEvidence{}, fmt.Errorf("setup authorization refuses an empty base/head diff")
	}
	required, absent, err := computeSetupRequiredFileEvidence(ctx, request.CheckoutDir, headSHA, request.RequiredPresent, request.RequiredAbsent)
	if err != nil {
		return SetupAuthorizationBinding{}, SetupDiffEvidence{}, err
	}
	binding := SetupAuthorizationBinding{
		SchemaVersion: SetupAuthorizationSchema,
		Repository:    repository, RepositoryID: strings.TrimSpace(request.RepositoryID), PullRequest: request.PullRequest,
		HeadRef: strings.TrimSpace(request.HeadRef), HeadSHA: headSHA, BaseRef: strings.TrimSpace(request.BaseRef), BaseSHA: baseSHA,
		AuthorizerID: strconv.FormatInt(request.AuthorizerID, 10), DiffAlgorithm: diff.Algorithm, DiffSHA256: diff.SHA256,
		RequiredFiles: required, RequiredAbsent: absent,
	}
	if err := binding.Validate(); err != nil {
		return SetupAuthorizationBinding{}, SetupDiffEvidence{}, err
	}
	return binding, diff, nil
}

// CanonicalJSON returns the exact bytes hashed into the authorization context.
func (binding SetupAuthorizationBinding) CanonicalJSON() ([]byte, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return nil, fmt.Errorf("encode setup authorization binding: %w", err)
	}
	return data, nil
}

// Digest returns the SHA-256 of CanonicalJSON.
func (binding SetupAuthorizationBinding) Digest() (string, error) {
	data, err := binding.CanonicalJSON()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// StatusContext is the exact context Hive creates on the authorized head.
func (binding SetupAuthorizationBinding) StatusContext() (string, error) {
	digest, err := binding.Digest()
	if err != nil {
		return "", err
	}
	return SetupAuthorizationContextPrefix + digest, nil
}

// Validate rejects alternate encodings, unsorted sets, and incomplete
// bindings. This makes a single canonical byte representation mandatory.
func (binding SetupAuthorizationBinding) Validate() error {
	if binding.SchemaVersion != SetupAuthorizationSchema || binding.DiffAlgorithm != SetupAuthorizationDiffAlgorithm {
		return fmt.Errorf("unsupported setup authorization schema or diff algorithm")
	}
	if binding.Repository != strings.ToLower(strings.TrimSpace(binding.Repository)) || !setupAuthorizationRepo.MatchString(binding.Repository) ||
		!setupAuthorizationID.MatchString(binding.RepositoryID) || binding.PullRequest <= 0 || !setupAuthorizationID.MatchString(binding.AuthorizerID) {
		return fmt.Errorf("invalid setup authorization repository, repository ID, pull request, or authorizer ID")
	}
	if !validSetupAuthorizationRef(binding.HeadRef) || !validSetupAuthorizationRef(binding.BaseRef) ||
		!setupAuthorizationSHA.MatchString(binding.HeadSHA) || !setupAuthorizationSHA.MatchString(binding.BaseSHA) ||
		!setupAuthorizationDigest.MatchString(binding.DiffSHA256) {
		return fmt.Errorf("invalid setup authorization refs, SHAs, or diff digest")
	}
	if len(binding.RequiredFiles) == 0 && len(binding.RequiredAbsent) == 0 {
		return fmt.Errorf("setup authorization requires at least one exact required or absent file")
	}
	previous := ""
	present := make(map[string]struct{}, len(binding.RequiredFiles))
	for index, file := range binding.RequiredFiles {
		if err := validateSetupAuthorizationPath(file.Path); err != nil {
			return err
		}
		if index > 0 && file.Path <= previous {
			return fmt.Errorf("setup authorization required files must be strictly path-sorted and unique")
		}
		if file.Mode != "100644" || !setupAuthorizationOID.MatchString(file.BlobOID) || file.Size < 0 || !setupAuthorizationDigest.MatchString(file.ContentSHA256) {
			return fmt.Errorf("required setup file %s has invalid mode, blob, size, or content digest", file.Path)
		}
		previous = file.Path
		present[file.Path] = struct{}{}
	}
	previous = ""
	for index, absent := range binding.RequiredAbsent {
		if err := validateSetupAuthorizationPath(absent); err != nil {
			return err
		}
		if index > 0 && absent <= previous {
			return fmt.Errorf("setup authorization absent paths must be strictly sorted and unique")
		}
		if _, ok := present[absent]; ok {
			return fmt.Errorf("setup authorization path %s cannot be both required and absent", absent)
		}
		previous = absent
	}
	return nil
}

// ComputeSetupDiffEvidence hashes a length-prefixed canonical stream:
//
//	domain, record count, then for each path-sorted raw diff-tree record:
//	status, old mode, new mode, old OID, new OID, raw path, and the exact old
//	and new blob objects (type marker, uint64 length, raw bytes).
//
// Missing sides are encoded as type "missing" with zero bytes; gitlinks are
// encoded as type "gitlink" with zero bytes and remain bound by their OID.
// Every string/byte field is prefixed by an unsigned 64-bit big-endian length.
// Renames are disabled, so they canonically become one deletion and one add.
func ComputeSetupDiffEvidence(ctx context.Context, checkoutDir, baseSHA, headSHA string) (SetupDiffEvidence, error) {
	baseSHA = strings.ToLower(strings.TrimSpace(baseSHA))
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	if strings.TrimSpace(checkoutDir) == "" || !setupAuthorizationSHA.MatchString(baseSHA) || !setupAuthorizationSHA.MatchString(headSHA) {
		return SetupDiffEvidence{}, fmt.Errorf("canonical setup diff requires a checkout and exact lowercase 40-character base/head SHAs")
	}
	for _, sha := range []string{baseSHA, headSHA} {
		if _, err := runSetupAuthorizationGit(ctx, checkoutDir, "cat-file", "-e", sha+"^{commit}"); err != nil {
			return SetupDiffEvidence{}, fmt.Errorf("verify setup authorization commit %s: %w", sha, err)
		}
	}
	raw, err := runSetupAuthorizationGit(ctx, checkoutDir, "diff-tree", "--no-commit-id", "--raw", "-r", "-z", "--no-renames", baseSHA, headSHA, "--")
	if err != nil {
		return SetupDiffEvidence{}, fmt.Errorf("read canonical setup diff-tree: %w", err)
	}
	records, err := parseSetupDiffTree(raw)
	if err != nil {
		return SetupDiffEvidence{}, err
	}
	sort.Slice(records, func(i, j int) bool { return bytes.Compare(records[i].Path, records[j].Path) < 0 })
	for index := 1; index < len(records); index++ {
		if bytes.Equal(records[index-1].Path, records[index].Path) {
			return SetupDiffEvidence{}, fmt.Errorf("canonical setup diff contains duplicate path %q", records[index].Path)
		}
	}
	hasher := sha256.New()
	writeSetupLengthPrefixed(hasher, []byte(setupAuthorizationDiffDomain))
	writeSetupUint64(hasher, uint64(len(records)))
	paths := make([]string, 0, len(records))
	for _, record := range records {
		if !utf8.Valid(record.Path) {
			return SetupDiffEvidence{}, fmt.Errorf("canonical setup diff path is not valid UTF-8")
		}
		pathValue := string(record.Path)
		if err := validateSetupDiffPath(pathValue); err != nil {
			return SetupDiffEvidence{}, err
		}
		for _, field := range [][]byte{[]byte(record.Status), []byte(record.OldMode), []byte(record.NewMode), []byte(record.OldOID), []byte(record.NewOID), record.Path} {
			writeSetupLengthPrefixed(hasher, field)
		}
		if err := hashSetupDiffObject(ctx, checkoutDir, hasher, record.OldMode, record.OldOID); err != nil {
			return SetupDiffEvidence{}, fmt.Errorf("hash old object for %s: %w", pathValue, err)
		}
		if err := hashSetupDiffObject(ctx, checkoutDir, hasher, record.NewMode, record.NewOID); err != nil {
			return SetupDiffEvidence{}, fmt.Errorf("hash new object for %s: %w", pathValue, err)
		}
		paths = append(paths, pathValue)
	}
	return SetupDiffEvidence{
		Algorithm: SetupAuthorizationDiffAlgorithm, SHA256: hex.EncodeToString(hasher.Sum(nil)), ChangedPaths: paths, ChangeCount: len(records),
	}, nil
}

func computeSetupRequiredFileEvidence(ctx context.Context, checkoutDir, headSHA string, requiredPresent, requiredAbsent []string) ([]SetupAuthorizationFile, []string, error) {
	present, err := sortedSetupAuthorizationPaths(requiredPresent)
	if err != nil {
		return nil, nil, err
	}
	absent, err := sortedSetupAuthorizationPaths(requiredAbsent)
	if err != nil {
		return nil, nil, err
	}
	presentSet := make(map[string]struct{}, len(present))
	files := make([]SetupAuthorizationFile, 0, len(present))
	for _, filePath := range present {
		presentSet[filePath] = struct{}{}
		entry, err := setupAuthorizationTreeEntry(ctx, checkoutDir, headSHA, filePath)
		if err != nil {
			return nil, nil, err
		}
		if entry.Mode != "100644" || entry.Type != "blob" {
			return nil, nil, fmt.Errorf("required setup file %s must be a non-executable regular Git blob (mode 100644)", filePath)
		}
		size, digest, err := setupAuthorizationBlobDigest(ctx, checkoutDir, entry.OID)
		if err != nil {
			return nil, nil, fmt.Errorf("hash required setup file %s: %w", filePath, err)
		}
		files = append(files, SetupAuthorizationFile{Path: filePath, Mode: entry.Mode, BlobOID: entry.OID, Size: size, ContentSHA256: digest})
	}
	for _, absentPath := range absent {
		if _, ok := presentSet[absentPath]; ok {
			return nil, nil, fmt.Errorf("setup authorization path %s cannot be both required and absent", absentPath)
		}
		entry, found, err := findSetupAuthorizationTreeEntry(ctx, checkoutDir, headSHA, absentPath)
		if err != nil {
			return nil, nil, err
		}
		if found {
			return nil, nil, fmt.Errorf("setup authorization requires %s to be absent, found %s %s", absentPath, entry.Mode, entry.Type)
		}
	}
	return files, absent, nil
}

type setupTreeEntry struct {
	Mode string
	Type string
	OID  string
}

func setupAuthorizationTreeEntry(ctx context.Context, checkoutDir, headSHA, filePath string) (setupTreeEntry, error) {
	entry, found, err := findSetupAuthorizationTreeEntry(ctx, checkoutDir, headSHA, filePath)
	if err != nil {
		return setupTreeEntry{}, err
	}
	if !found {
		return setupTreeEntry{}, fmt.Errorf("required setup file %s is absent at head %s", filePath, headSHA)
	}
	return entry, nil
}

func findSetupAuthorizationTreeEntry(ctx context.Context, checkoutDir, headSHA, filePath string) (setupTreeEntry, bool, error) {
	if err := validateSetupAuthorizationPath(filePath); err != nil {
		return setupTreeEntry{}, false, err
	}
	raw, err := runSetupAuthorizationGit(ctx, checkoutDir, "ls-tree", "-z", headSHA, "--", ":(literal)"+filePath)
	if err != nil {
		return setupTreeEntry{}, false, fmt.Errorf("read setup file %s at %s: %w", filePath, headSHA, err)
	}
	if len(raw) == 0 {
		return setupTreeEntry{}, false, nil
	}
	if raw[len(raw)-1] != 0 || bytes.Count(raw, []byte{0}) != 1 {
		return setupTreeEntry{}, false, fmt.Errorf("setup file %s resolved ambiguously in the head tree", filePath)
	}
	entry := raw[:len(raw)-1]
	metadata, actualPath, found := bytes.Cut(entry, []byte{'\t'})
	if !found || string(actualPath) != filePath {
		return setupTreeEntry{}, false, fmt.Errorf("setup file %s did not resolve to its exact literal path", filePath)
	}
	fields := strings.Fields(string(metadata))
	if len(fields) != 3 || !setupAuthorizationOID.MatchString(fields[2]) {
		return setupTreeEntry{}, false, fmt.Errorf("setup file %s has malformed Git tree metadata", filePath)
	}
	return setupTreeEntry{Mode: fields[0], Type: fields[1], OID: fields[2]}, true, nil
}

func setupAuthorizationBlobDigest(ctx context.Context, checkoutDir, oid string) (int64, string, error) {
	sizeData, err := runSetupAuthorizationGit(ctx, checkoutDir, "cat-file", "-s", oid)
	if err != nil {
		return 0, "", err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeData)), 10, 64)
	if err != nil || size < 0 {
		return 0, "", fmt.Errorf("Git blob %s returned invalid size %q", oid, sizeData)
	}
	hasher := sha256.New()
	written, err := streamSetupAuthorizationGit(ctx, checkoutDir, hasher, "cat-file", "blob", oid)
	if err != nil {
		return 0, "", err
	}
	if written != size {
		return 0, "", fmt.Errorf("Git blob %s streamed %d bytes, expected %d", oid, written, size)
	}
	return size, hex.EncodeToString(hasher.Sum(nil)), nil
}

func parseSetupDiffTree(raw []byte) ([]setupDiffRecord, error) {
	records := make([]setupDiffRecord, 0)
	for len(raw) > 0 {
		headerEnd := bytes.IndexByte(raw, 0)
		if headerEnd < 0 {
			return nil, fmt.Errorf("canonical setup diff-tree ended inside a record header")
		}
		header := raw[:headerEnd]
		raw = raw[headerEnd+1:]
		pathEnd := bytes.IndexByte(raw, 0)
		if pathEnd < 0 {
			return nil, fmt.Errorf("canonical setup diff-tree ended inside a path")
		}
		pathBytes := append([]byte(nil), raw[:pathEnd]...)
		raw = raw[pathEnd+1:]
		fields := bytes.Fields(header)
		if len(fields) != 5 || len(fields[0]) != 7 || fields[0][0] != ':' || len(fields[1]) != 6 {
			return nil, fmt.Errorf("canonical setup diff-tree returned malformed header %q", header)
		}
		status := string(fields[4])
		if len(status) != 1 || !strings.Contains("ADMTUX", status) {
			return nil, fmt.Errorf("canonical setup diff-tree returned unsupported no-renames status %q", status)
		}
		oldOID, newOID := string(fields[2]), string(fields[3])
		if !setupAuthorizationOID.MatchString(oldOID) || !setupAuthorizationOID.MatchString(newOID) {
			return nil, fmt.Errorf("canonical setup diff-tree returned malformed object IDs")
		}
		records = append(records, setupDiffRecord{
			Status: status, OldMode: string(fields[0][1:]), NewMode: string(fields[1]), OldOID: oldOID, NewOID: newOID, Path: pathBytes,
		})
	}
	return records, nil
}

func hashSetupDiffObject(ctx context.Context, checkoutDir string, hasher hash.Hash, mode, oid string) error {
	if mode == "000000" || allZeroSetupOID(oid) {
		writeSetupLengthPrefixed(hasher, []byte("missing"))
		writeSetupUint64(hasher, 0)
		return nil
	}
	if mode == "160000" {
		writeSetupLengthPrefixed(hasher, []byte("gitlink"))
		writeSetupUint64(hasher, 0)
		return nil
	}
	if mode != "100644" && mode != "100755" && mode != "120000" {
		return fmt.Errorf("unsupported Git mode %s", mode)
	}
	typeData, err := runSetupAuthorizationGit(ctx, checkoutDir, "cat-file", "-t", oid)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(typeData)) != "blob" {
		return fmt.Errorf("object %s for mode %s is not a blob", oid, mode)
	}
	sizeData, err := runSetupAuthorizationGit(ctx, checkoutDir, "cat-file", "-s", oid)
	if err != nil {
		return err
	}
	size, err := strconv.ParseUint(strings.TrimSpace(string(sizeData)), 10, 64)
	if err != nil {
		return fmt.Errorf("object %s returned invalid size %q", oid, sizeData)
	}
	writeSetupLengthPrefixed(hasher, []byte("blob"))
	writeSetupUint64(hasher, size)
	written, err := streamSetupAuthorizationGit(ctx, checkoutDir, hasher, "cat-file", "blob", oid)
	if err != nil {
		return err
	}
	if uint64(written) != size {
		return fmt.Errorf("object %s streamed %d bytes, expected %d", oid, written, size)
	}
	return nil
}

func runSetupAuthorizationGit(ctx context.Context, checkoutDir string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = checkoutDir
	command.Env = append(safeEnvironment(), "LC_ALL=C", "LANG=C")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, safeOutput(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func streamSetupAuthorizationGit(ctx context.Context, checkoutDir string, destination io.Writer, args ...string) (int64, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = checkoutDir
	command.Env = append(safeEnvironment(), "LC_ALL=C", "LANG=C")
	var stderr bytes.Buffer
	counter := &setupCountingWriter{destination: destination}
	command.Stdout, command.Stderr = counter, &stderr
	if err := command.Run(); err != nil {
		return counter.written, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, safeOutput(stderr.String()))
	}
	return counter.written, nil
}

type setupCountingWriter struct {
	destination io.Writer
	written     int64
}

func (writer *setupCountingWriter) Write(value []byte) (int, error) {
	n, err := writer.destination.Write(value)
	writer.written += int64(n)
	return n, err
}

func writeSetupLengthPrefixed(destination io.Writer, value []byte) {
	writeSetupUint64(destination, uint64(len(value)))
	_, _ = destination.Write(value)
}

func writeSetupUint64(destination io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = destination.Write(encoded[:])
}

func sortedSetupAuthorizationPaths(values []string) ([]string, error) {
	result := append([]string(nil), values...)
	for _, value := range result {
		if err := validateSetupAuthorizationPath(value); err != nil {
			return nil, err
		}
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index] == result[index-1] {
			return nil, fmt.Errorf("setup authorization path %s is duplicated", result[index])
		}
	}
	return result, nil
}

func validateSetupAuthorizationPath(value string) error {
	if value == "" || !utf8.ValidString(value) || !isSetupAuthorizationASCII(value) || strings.ContainsAny(value, "\\\x00\r\n\t") || strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("unsafe setup authorization path %q", value)
	}
	return nil
}

func validateSetupDiffPath(value string) error {
	if value == "" || strings.ContainsAny(value, "\\\x00\r\n\t") || strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("unsafe canonical setup diff path %q", value)
	}
	return nil
}

func validSetupAuthorizationRef(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && setupAuthorizationRefPart.MatchString(value) && !strings.HasPrefix(value, "/") && !strings.HasSuffix(value, "/") &&
		!strings.Contains(value, "..") && !strings.Contains(value, "//") && !strings.Contains(value, "@{") && !strings.HasSuffix(value, ".")
}

func isSetupAuthorizationASCII(value string) bool {
	for _, character := range value {
		if character < 0x20 || character > 0x7e || character == '<' || character == '>' || character == '&' {
			return false
		}
	}
	return true
}

func allZeroSetupOID(value string) bool {
	return strings.Trim(value, "0") == ""
}
