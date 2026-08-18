package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/kubestellar/hive/pkg/scheduler"
)

// GovernedSourceContextOptions binds one Scheduler composition to the exact
// model base sealed by repair Worker and to the independently verified Visual
// Hive evidence directory. The proposal child never receives either path.
type GovernedSourceContextOptions struct {
	Context                   context.Context
	Worktree                  string
	Repository                string
	BaseSHA                   string
	BaseTreeSHA               string
	EvidenceRoot              string
	AllowedPaths              []string
	AffectedContracts         []string
	RelevanceHints            []string
	ManifestSHA256            string
	VerificationReceiptSHA256 string
}

// GovernedSourceContextComposer is the production Worker-owned implementation
// of Scheduler's sealed source/evidence seams. It reads source only through Git
// object IDs from the exact sealed tree and evidence only through the verified
// ordinary-file root.
type GovernedSourceContextComposer struct {
	ctx                       context.Context
	worktree                  string
	repository                string
	baseSHA                   string
	baseTreeSHA               string
	allowedPaths              []string
	affectedContracts         []string
	relevanceHints            []string
	manifestSHA256            string
	verificationReceiptSHA256 string
	evidence                  *GovernedEvidenceArtifactReader
}

var (
	_ scheduler.WorkerSourceContextComposer  = (*GovernedSourceContextComposer)(nil)
	_ scheduler.WorkerEvidenceArtifactReader = (*GovernedSourceContextComposer)(nil)
)

func NewGovernedSourceContextComposer(options GovernedSourceContextOptions) (*GovernedSourceContextComposer, error) {
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	worktree, err := filepath.Abs(strings.TrimSpace(options.Worktree))
	if err != nil || strings.TrimSpace(options.Worktree) == "" {
		return nil, errors.New("governed source composition requires the Worker worktree")
	}
	repository := strings.ToLower(strings.TrimSpace(options.Repository))
	baseSHA := strings.ToLower(strings.TrimSpace(options.BaseSHA))
	baseTreeSHA := strings.ToLower(strings.TrimSpace(options.BaseTreeSHA))
	manifestSHA := strings.ToLower(strings.TrimSpace(options.ManifestSHA256))
	receiptSHA := strings.ToLower(strings.TrimSpace(options.VerificationReceiptSHA256))
	if repository == "" || !validGitObjectOID(baseSHA) || !validGitObjectOID(baseTreeSHA) ||
		!validSHA256Digest(manifestSHA) || !validSHA256Digest(receiptSHA) || len(options.AllowedPaths) == 0 {
		return nil, errors.New("governed source composition bindings are incomplete")
	}
	resolvedTree, err := runGitExact(ctx, worktree, "rev-parse", baseSHA+"^{tree}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(resolvedTree), baseTreeSHA) {
		return nil, errors.New("Worker base commit does not resolve to the admitted exact tree")
	}
	evidence, err := NewGovernedEvidenceArtifactReader(options.EvidenceRoot)
	if err != nil {
		return nil, err
	}
	return &GovernedSourceContextComposer{
		ctx: ctx, worktree: worktree, repository: repository, baseSHA: baseSHA, baseTreeSHA: baseTreeSHA,
		allowedPaths:      append([]string(nil), options.AllowedPaths...),
		affectedContracts: append([]string(nil), options.AffectedContracts...),
		relevanceHints:    append([]string(nil), options.RelevanceHints...),
		manifestSHA256:    manifestSHA, verificationReceiptSHA256: receiptSHA, evidence: evidence,
	}, nil
}

func (composer *GovernedSourceContextComposer) Close() error {
	if composer == nil || composer.evidence == nil {
		return nil
	}
	return composer.evidence.Close()
}

func (composer *GovernedSourceContextComposer) ComposeGovernedSourceContext(request scheduler.WorkerSourceContextRequest) (scheduler.WorkerSealedSourceContext, error) {
	if composer == nil || composer.evidence == nil {
		return scheduler.WorkerSealedSourceContext{}, errors.New("governed source composer is not configured")
	}
	if strings.ToLower(strings.TrimSpace(request.Repository)) != composer.repository ||
		!strings.EqualFold(strings.TrimSpace(request.BaseSHA), composer.baseSHA) ||
		!strings.EqualFold(strings.TrimSpace(request.BaseTreeSHA), composer.baseTreeSHA) ||
		!strings.EqualFold(strings.TrimSpace(request.ManifestSHA256), composer.manifestSHA256) ||
		!strings.EqualFold(strings.TrimSpace(request.VerificationReceiptSHA256), composer.verificationReceiptSHA256) ||
		!reflect.DeepEqual(request.AllowedPaths, composer.allowedPaths) ||
		!reflect.DeepEqual(request.AffectedContracts, composer.affectedContracts) {
		return scheduler.WorkerSealedSourceContext{}, errors.New("Scheduler source request differs from the Worker-owned admitted bindings")
	}
	content, err := buildRepairSourceContext(composer.ctx, composer.worktree, composer.baseTreeSHA, composer.allowedPaths, composer.relevanceHints)
	if err != nil {
		return scheduler.WorkerSealedSourceContext{}, err
	}
	document, err := decodeGovernedRepairSourceContext(content)
	if err != nil {
		return scheduler.WorkerSealedSourceContext{}, err
	}
	if document.SourceTreeOID != composer.baseTreeSHA {
		return scheduler.WorkerSealedSourceContext{}, errors.New("Worker source context inventory differs from the admitted exact tree")
	}
	files := make([]scheduler.WorkerSourceBlobIdentity, 0, len(document.Files))
	for _, file := range document.Files {
		files = append(files, scheduler.WorkerSourceBlobIdentity{
			Path: file.Path, BlobOID: file.GitOID, ContentSHA256: file.SHA256, Bytes: int64(file.Bytes),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	digest := sha256.Sum256([]byte(content))
	return scheduler.WorkerSealedSourceContext{
		Content: content, SHA256: hex.EncodeToString(digest[:]), BaseSHA: composer.baseSHA, BaseTreeSHA: composer.baseTreeSHA,
		ManifestSHA256: composer.manifestSHA256, VerificationReceiptSHA256: composer.verificationReceiptSHA256, Files: files,
	}, nil
}

func (composer *GovernedSourceContextComposer) ReadGovernedEvidenceArtifacts(request scheduler.WorkerEvidenceArtifactRequest) (scheduler.WorkerEvidenceArtifactReadResult, error) {
	if composer == nil || composer.evidence == nil {
		return scheduler.WorkerEvidenceArtifactReadResult{}, errors.New("governed evidence composer is not configured")
	}
	return composer.evidence.ReadGovernedEvidenceArtifacts(request)
}

func decodeGovernedRepairSourceContext(content string) (repairSourceContextDocument, error) {
	if !strings.HasPrefix(content, repairSourceContextPrefix) || !strings.HasSuffix(content, repairSourceContextSuffix) {
		return repairSourceContextDocument{}, errors.New("Worker source context has an unexpected envelope")
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(content, repairSourceContextPrefix), repairSourceContextSuffix)
	var document repairSourceContextDocument
	if err := json.Unmarshal([]byte(encoded), &document); err != nil {
		return repairSourceContextDocument{}, fmt.Errorf("decode Worker source context inventory: %w", err)
	}
	if !validGitObjectOID(document.SourceTreeOID) || document.SourceTreeOID != strings.ToLower(strings.TrimSpace(document.SourceTreeOID)) || document.IncludedFiles != len(document.Files) {
		return repairSourceContextDocument{}, errors.New("Worker source context inventory is internally inconsistent")
	}
	return document, nil
}

func validSHA256Digest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
