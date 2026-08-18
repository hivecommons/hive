package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kubestellar/hive/pkg/scheduler"
)

// GovernedEvidenceArtifactReader is rooted at the already verified Visual Hive
// evidence directory. It returns selected structured sourceArtifact bytes only
// after rechecking the path, ordinary-file identity, declared size, and SHA-256.
// It never reads logs or gives the proposal child a filesystem path.
type GovernedEvidenceArtifactReader struct {
	root     *os.Root
	rootPath string
}

var _ scheduler.WorkerEvidenceArtifactReader = (*GovernedEvidenceArtifactReader)(nil)

func NewGovernedEvidenceArtifactReader(verifiedEvidenceRoot string) (*GovernedEvidenceArtifactReader, error) {
	root, err := filepath.Abs(strings.TrimSpace(verifiedEvidenceRoot))
	if err != nil || strings.TrimSpace(verifiedEvidenceRoot) == "" {
		return nil, errors.New("verified Visual Hive evidence root is required")
	}
	if err := requireGovernedEvidenceDirectory(root); err != nil {
		return nil, err
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open verified Visual Hive evidence root: %w", err)
	}
	return &GovernedEvidenceArtifactReader{root: rootHandle, rootPath: root}, nil
}

func (reader *GovernedEvidenceArtifactReader) Close() error {
	if reader == nil || reader.root == nil {
		return nil
	}
	err := reader.root.Close()
	reader.root = nil
	return err
}

func (reader *GovernedEvidenceArtifactReader) ReadGovernedEvidenceArtifacts(request scheduler.WorkerEvidenceArtifactRequest) (scheduler.WorkerEvidenceArtifactReadResult, error) {
	if reader == nil || reader.root == nil {
		return scheduler.WorkerEvidenceArtifactReadResult{}, errors.New("governed evidence reader is not configured")
	}
	rootInfo, err := reader.root.Stat(".")
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return scheduler.WorkerEvidenceArtifactReadResult{}, errors.New("verified Visual Hive evidence root handle is no longer an ordinary directory")
	}
	result := scheduler.WorkerEvidenceArtifactReadResult{Artifacts: make([]scheduler.WorkerEvidenceArtifactContent, 0)}
	var total int64
	for _, receipt := range request.Artifacts {
		if !governedEvidenceJSONReceipt(receipt) || receipt.Bytes > scheduler.MaxGovernedEvidenceArtifactBytes ||
			len(result.Artifacts) >= scheduler.MaxGovernedEvidenceArtifactCount || total+receipt.Bytes > scheduler.MaxGovernedEvidenceTotalBytes {
			continue
		}
		relative, err := reader.resolve(receipt.Reference)
		if err != nil {
			return scheduler.WorkerEvidenceArtifactReadResult{}, err
		}
		content, err := readGovernedEvidenceFile(reader.root, reader.rootPath, relative, receipt.Bytes)
		if err != nil {
			return scheduler.WorkerEvidenceArtifactReadResult{}, fmt.Errorf("read governed evidence artifact %q: %w", receipt.Reference, err)
		}
		digest := sha256.Sum256(content)
		if hex.EncodeToString(digest[:]) != receipt.SHA256 || !utf8.Valid(content) || !json.Valid(content) {
			return scheduler.WorkerEvidenceArtifactReadResult{}, fmt.Errorf("governed evidence artifact %q failed SHA-256, UTF-8, or JSON verification", receipt.Reference)
		}
		result.Artifacts = append(result.Artifacts, scheduler.WorkerEvidenceArtifactContent{
			Reference: receipt.Reference, SHA256: receipt.SHA256, Bytes: receipt.Bytes,
			Kind: receipt.Kind, ContentType: receipt.ContentType, Content: string(content),
		})
		total += receipt.Bytes
	}
	return result, nil
}

func governedEvidenceJSONReceipt(receipt scheduler.EvidenceArtifact) bool {
	reference := strings.ToLower(strings.TrimSpace(receipt.Reference))
	contentType := strings.ToLower(strings.TrimSpace(receipt.ContentType))
	return strings.TrimSpace(receipt.Kind) == "json" && strings.HasSuffix(reference, ".json") &&
		(contentType == "application/json" || strings.HasSuffix(contentType, "+json")) && receipt.Bytes >= 0
}

func (reader *GovernedEvidenceArtifactReader) resolve(reference string) (string, error) {
	reference = filepath.ToSlash(strings.TrimSpace(reference))
	if reference == "" || strings.Contains(reference, "\\") {
		return "", errors.New("governed evidence artifact path is invalid")
	}
	rootName := filepath.ToSlash(filepath.Base(reader.rootPath))
	relative := reference
	if strings.HasPrefix(relative, rootName+"/") {
		relative = strings.TrimPrefix(relative, rootName+"/")
	}
	clean := filepath.ToSlash(filepath.Clean(relative))
	if clean == "." || clean == ".." || clean != relative || strings.HasPrefix(clean, "../") || filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return "", fmt.Errorf("unsafe governed evidence artifact path %q", reference)
	}
	relativePath := filepath.FromSlash(clean)
	if err := requireGovernedEvidenceParents(reader.root, filepath.Dir(relativePath)); err != nil {
		return "", err
	}
	return relativePath, nil
}

func requireGovernedEvidenceDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect verified Visual Hive evidence root: %w", err)
	}
	reparse, reparseErr := codexProviderPathIsReparsePoint(path)
	if reparseErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || reparse {
		return errors.New("verified Visual Hive evidence root must be an ordinary non-reparse directory")
	}
	return nil
}

func requireGovernedEvidenceParents(root *os.Root, parent string) error {
	if parent == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(parent, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, statErr := root.Lstat(current)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("governed evidence artifact path traverses a non-directory or symlink")
		}
	}
	return nil
}

func readGovernedEvidenceFile(root *os.Root, rootPath, relative string, expectedBytes int64) ([]byte, error) {
	before, err := root.Lstat(relative)
	if err != nil || before == nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != expectedBytes ||
		expectedBytes < 0 || expectedBytes > scheduler.MaxGovernedEvidenceArtifactBytes {
		return nil, errors.New("artifact is not the exact bounded ordinary file declared by its receipt")
	}
	absolute := filepath.Join(rootPath, relative)
	reparse, reparseErr := codexProviderPathIsReparsePoint(absolute)
	if reparseErr != nil || reparse {
		return nil, errors.New("artifact resolves through a reparse point")
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("artifact identity changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, scheduler.MaxGovernedEvidenceArtifactBytes+1))
	if err != nil || int64(len(content)) != expectedBytes {
		return nil, errors.New("artifact bytes changed while reading")
	}
	after, err := root.Lstat(relative)
	if err != nil || !os.SameFile(before, after) || after.Mode()&os.ModeSymlink != 0 || after.Size() != expectedBytes {
		return nil, errors.New("artifact identity changed after reading")
	}
	return content, nil
}
