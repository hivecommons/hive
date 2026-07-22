package visualhive

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	PullRequestSourceBindingSchema     = "hive.visual-hive-pr-source.v2"
	PullRequestRuntimeSchema           = "visual-hive.playwright-runtime.v1"
	PullRequestRunnerAttestationSchema = "hive.visual-hive-runner-attestation.v1"
	PullRequestRunnerExecutionSchema   = "hive.visual-hive-runner-execution.v1"
	PullRequestSourceBindingPath       = ".visual-hive/proof/pr/source-binding.json"
	PullRequestRuntimePath             = ".visual-hive/proof/pr/runtime.json"
	PullRequestPlanPath                = ".visual-hive/plan.json"
	PullRequestReportPath              = ".visual-hive/report.json"
	PullRequestConfigPath              = ".visual-hive/proof/pr/inputs/visual-hive.config.yaml"
	PullRequestWorkflowPath            = ".visual-hive/proof/pr/inputs/visual-hive-pr.yml"
	PullRequestChangedFilesPath        = ".visual-hive/changed-files.pr.txt"
	PullRequestPlanContractsPath       = ".visual-hive/evaluated-plan-contracts.txt"
	PullRequestEvaluationScopesPath    = ".visual-hive/evaluated-contracts.txt"
	PullRequestRunnerAttestationPath   = ".visual-hive/proof/pr/runner-execution-attestation.json"
	PullRequestPipelineExitPath        = ".visual-hive/pipeline-exit-code.txt"
	PullRequestRunnerOutcomePath       = ".visual-hive/hive-runner-outcome.json"
	pullRequestBaselinePrefix          = ".visual-hive/proof/pr/baselines/"
	maxPullRequestBaselineFiles        = 200
	maxPullRequestBaselineBytes        = int64(20 << 20)
	maxPullRequestBaselinesBytes       = int64(500 << 20)
)

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// PullRequestParseOptions is deliberately separate from ValidationOptions.
// Parsing validates claimed identities but never asserts that GitHub actually
// supplied them and never establishes provenance or lifecycle authority. Only
// pkg/github's live fetch-and-verify path may seal those claims.
type PullRequestParseOptions struct {
	Now                        time.Time
	MaxACMM                    int
	ExpectedProducerGitCommit  string
	ExpectedRepository         string
	ExpectedRepositoryID       string
	ExpectedHeadSHA            string
	ExpectedHeadRef            string
	ExpectedWorkflowName       string
	ExpectedWorkflowRunID      string
	ExpectedWorkflowRunAttempt string
	ExpectedSourceArtifactID   string
}

func ParsePullRequestBundle(manifestPath string, options PullRequestParseOptions) (*ValidatedBundle, error) {
	headRef := strings.TrimSpace(options.ExpectedHeadRef)
	if !strings.HasPrefix(headRef, "refs/heads/") {
		headRef = "refs/heads/" + strings.TrimPrefix(headRef, "refs/heads/")
	}
	if !exactCommit.MatchString(strings.TrimSpace(options.ExpectedProducerGitCommit)) ||
		!exactCommit.MatchString(strings.TrimSpace(options.ExpectedHeadSHA)) ||
		strings.TrimSpace(options.ExpectedRepository) == "" || strings.TrimSpace(options.ExpectedRepositoryID) == "" ||
		strings.TrimSpace(options.ExpectedWorkflowName) == "" || strings.TrimSpace(options.ExpectedWorkflowRunID) == "" ||
		strings.TrimSpace(options.ExpectedWorkflowRunAttempt) == "" || strings.TrimSpace(options.ExpectedSourceArtifactID) == "" ||
		headRef == "refs/heads/" || strings.HasPrefix(headRef, "refs/pull/") {
		return nil, fmt.Errorf("exact PR bundle repository, head, workflow, run, attempt, source artifact, and producer identities are required")
	}
	for label, value := range map[string]string{
		"repository id": options.ExpectedRepositoryID, "workflow run id": options.ExpectedWorkflowRunID,
		"workflow run attempt": options.ExpectedWorkflowRunAttempt, "source artifact id": options.ExpectedSourceArtifactID,
	} {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != value {
			return nil, fmt.Errorf("exact positive decimal %s is required", label)
		}
	}
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: options.Now, MaxACMM: options.MaxACMM, VerifiedProvenance: false,
		ExpectedProducerGitCommit: options.ExpectedProducerGitCommit,
		ExpectedRepository:        options.ExpectedRepository, ExpectedRepositoryID: options.ExpectedRepositoryID,
		ExpectedWorkflowRunID: options.ExpectedWorkflowRunID, ExpectedWorkflowRunAttempt: options.ExpectedWorkflowRunAttempt,
		profile: bundleValidationPullRequestLocal,
	})
	if err != nil {
		return nil, err
	}
	manifest := bundle.Manifest
	if manifest.Source.CommitSHA != options.ExpectedHeadSHA || manifest.Source.Ref != headRef ||
		manifest.Source.WorkflowName != options.ExpectedWorkflowName || manifest.Source.WorkflowArtifactID != options.ExpectedSourceArtifactID {
		return nil, fmt.Errorf("pull request bundle manifest does not match the exact independently verified head, workflow, or source artifact")
	}
	if bundle.Validation.Trusted || bundle.Validation.Authoritative {
		return nil, fmt.Errorf("pull request bundle unexpectedly acquired lifecycle authority")
	}
	return bundle, nil
}

type PullRequestFileIdentity struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type PullRequestRevisionIdentity struct {
	Repository   string `json:"repository"`
	RepositoryID string `json:"repositoryId"`
	Ref          string `json:"ref"`
	SHA          string `json:"sha"`
}

type PullRequestWorkflowIdentity struct {
	Name           string                  `json:"name"`
	Path           string                  `json:"path"`
	Event          string                  `json:"event"`
	RunID          string                  `json:"runId"`
	RunAttempt     string                  `json:"runAttempt"`
	Definition     PullRequestFileIdentity `json:"definition"`
	ProducerCommit string                  `json:"producerCommit"`
	SourceName     string                  `json:"sourceArtifactName"`
	BundleName     string                  `json:"bundleArtifactName"`
}

type PullRequestRuntimeIdentity struct {
	Receipt                PullRequestFileIdentity `json:"receipt"`
	GeneratedSpec          PullRequestFileIdentity `json:"generatedSpec"`
	GeneratedConfig        PullRequestFileIdentity `json:"generatedConfig"`
	StableSHA256           string                  `json:"stableSha256"`
	ExecutionBindingSHA256 string                  `json:"executionBindingSha256"`
}

type PullRequestBaselineIdentity struct {
	SHA256 string                    `json:"sha256"`
	Files  []PullRequestFileIdentity `json:"files"`
}

type PullRequestSourceBinding struct {
	SchemaVersion     string                      `json:"schemaVersion"`
	Repository        string                      `json:"repository"`
	RepositoryID      string                      `json:"repositoryId"`
	PullRequest       int                         `json:"pullRequest"`
	Base              PullRequestRevisionIdentity `json:"base"`
	Head              PullRequestRevisionIdentity `json:"head"`
	Workflow          PullRequestWorkflowIdentity `json:"workflow"`
	Plan              PullRequestFileIdentity     `json:"plan"`
	Report            PullRequestFileIdentity     `json:"report"`
	Config            PullRequestFileIdentity     `json:"config"`
	ChangedFiles      PullRequestFileIdentity     `json:"changedFiles"`
	PlanContracts     PullRequestFileIdentity     `json:"planContracts"`
	EvaluationScopes  PullRequestFileIdentity     `json:"evaluationScopes"`
	RunnerAttestation PullRequestFileIdentity     `json:"runnerAttestation"`
	Runtime           PullRequestRuntimeIdentity  `json:"runtime"`
	Baselines         PullRequestBaselineIdentity `json:"baselines"`
}

type PullRequestSourceExpectations struct {
	Repository               string
	RepositoryID             string
	PullRequest              int
	Base                     PullRequestRevisionIdentity
	Head                     PullRequestRevisionIdentity
	WorkflowName             string
	WorkflowPath             string
	WorkflowRunID            string
	WorkflowRunAttempt       string
	WorkflowDefinitionSHA256 string
	ProducerCommit           string
	SourceArtifactName       string
	BundleArtifactName       string
	ConfigSHA256             string
}

// ParsePullRequestSourceBinding is the normative strict parser shared by the
// generated-workflow round-trip test and the verifier. Unknown fields fail.
func ParsePullRequestSourceBinding(data []byte) (PullRequestSourceBinding, error) {
	var binding PullRequestSourceBinding
	if err := decodeStrict(bytes.NewReader(data), &binding); err != nil {
		return PullRequestSourceBinding{}, fmt.Errorf("decode PR source binding: %w", err)
	}
	if binding.SchemaVersion != PullRequestSourceBindingSchema {
		return PullRequestSourceBinding{}, fmt.Errorf("unsupported PR source binding schema %q", binding.SchemaVersion)
	}
	return binding, nil
}

type playwrightRuntimeReceipt struct {
	SchemaVersion    string                   `json:"schemaVersion"`
	ExecutionBinding map[string]string        `json:"executionBinding"`
	CapturedAt       string                   `json:"capturedAt"`
	Browser          playwrightRuntimeBrowser `json:"browser"`
	Environment      playwrightEnvironment    `json:"environment"`
}

type playwrightRuntimeBrowser struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type playwrightEnvironment struct {
	OS                string                  `json:"os"`
	Architecture      string                  `json:"architecture"`
	NodeVersion       string                  `json:"nodeVersion"`
	PlaywrightVersion string                  `json:"playwrightVersion"`
	Locale            string                  `json:"locale"`
	Timezone          string                  `json:"timezone"`
	UserAgent         string                  `json:"userAgent"`
	DeviceScaleFactor json.Number             `json:"deviceScaleFactor"`
	Fonts             []playwrightRuntimeFont `json:"fonts"`
}

type playwrightRuntimeFont struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
}

// VerifyPullRequestSourceBinding cross-binds source-only identities after the
// complete artifact index has been verified. It requires a real Playwright
// runtime receipt and an actual evaluated plan; an ignored-files/no-contract
// PR cannot become an admissible rerun verdict.
func (bundle *ValidatedBundle) VerifyPullRequestSourceBinding(expected PullRequestSourceExpectations) (PullRequestSourceBinding, error) {
	if bundle == nil || bundle.validationProfile != bundleValidationPullRequestLocal || !bundle.sourceVerified || bundle.Validation.Trusted || bundle.Validation.Authoritative {
		return PullRequestSourceBinding{}, fmt.Errorf("verified non-authoritative PR source bundle is required")
	}
	if expected.PullRequest <= 0 || expected.SourceArtifactName == "" || expected.BundleArtifactName == "" || expected.SourceArtifactName == expected.BundleArtifactName {
		return PullRequestSourceBinding{}, fmt.Errorf("distinct exact PR source and bundle artifact names are required")
	}
	for label, value := range map[string]string{"workflow definition": expected.WorkflowDefinitionSHA256, "config": expected.ConfigSHA256} {
		if !hexDigest.MatchString(value) {
			return PullRequestSourceBinding{}, fmt.Errorf("exact %s SHA-256 identity is required", label)
		}
	}
	bindingData, _, _, err := bundle.readVerifiedSourceFile(PullRequestSourceBindingPath)
	if err != nil {
		return PullRequestSourceBinding{}, fmt.Errorf("verify immutable PR source binding: %w", err)
	}
	binding, err := ParsePullRequestSourceBinding(bindingData)
	if err != nil {
		return PullRequestSourceBinding{}, err
	}
	if !equalPullRequestSourceIdentity(binding, expected) {
		return PullRequestSourceBinding{}, fmt.Errorf("PR source binding does not match independently verified PR, workflow, artifact, or identity pins")
	}
	if binding.Plan.Path != PullRequestPlanPath || binding.Report.Path != PullRequestReportPath ||
		binding.Config.Path != PullRequestConfigPath || binding.ChangedFiles.Path != PullRequestChangedFilesPath ||
		binding.PlanContracts.Path != PullRequestPlanContractsPath || binding.EvaluationScopes.Path != PullRequestEvaluationScopesPath ||
		binding.RunnerAttestation.Path != PullRequestRunnerAttestationPath ||
		binding.Runtime.Receipt.Path != PullRequestRuntimePath || binding.Workflow.Definition.Path != PullRequestWorkflowPath ||
		!canonicalPullRequestGeneratedPaths(binding.Runtime.GeneratedSpec.Path, binding.Runtime.GeneratedConfig.Path) {
		return PullRequestSourceBinding{}, fmt.Errorf("PR source binding uses an unexpected fixed evidence path")
	}
	for label, identity := range map[string]PullRequestFileIdentity{
		"plan": binding.Plan, "report": binding.Report, "config": binding.Config, "changed files": binding.ChangedFiles,
		"plan contracts": binding.PlanContracts, "evaluation scopes": binding.EvaluationScopes,
		"runner attestation": binding.RunnerAttestation,
		"runtime":            binding.Runtime.Receipt, "generated spec": binding.Runtime.GeneratedSpec, "generated config": binding.Runtime.GeneratedConfig,
		"workflow definition": binding.Workflow.Definition,
	} {
		if err := bundle.verifyBoundSourceFile(identity); err != nil {
			return PullRequestSourceBinding{}, fmt.Errorf("%s identity: %w", label, err)
		}
	}
	if len(binding.Baselines.Files) == 0 || len(binding.Baselines.Files) > maxPullRequestBaselineFiles ||
		!sort.SliceIsSorted(binding.Baselines.Files, func(i, j int) bool { return binding.Baselines.Files[i].Path < binding.Baselines.Files[j].Path }) {
		return PullRequestSourceBinding{}, fmt.Errorf("PR baseline identity requires a sorted non-empty file inventory")
	}
	baselineRecords := make([]string, 0, len(binding.Baselines.Files))
	var baselineBytes int64
	for index, identity := range binding.Baselines.Files {
		sourcePath := strings.TrimPrefix(identity.Path, pullRequestBaselinePrefix)
		if sourcePath == identity.Path || !canonicalPullRequestRepositoryPath(sourcePath) ||
			!strings.HasSuffix(strings.ToLower(sourcePath), ".png") ||
			identity.Bytes <= int64(len(pngSignature)) || identity.Bytes > maxPullRequestBaselineBytes ||
			(index > 0 && binding.Baselines.Files[index-1].Path == identity.Path) {
			return PullRequestSourceBinding{}, fmt.Errorf("PR baseline identity is outside the canonical sealed PNG evidence directory")
		}
		if err := bundle.verifyBoundSourceFile(identity); err != nil {
			return PullRequestSourceBinding{}, fmt.Errorf("baseline identity: %w", err)
		}
		data, _, _, err := bundle.readVerifiedSourceFile(identity.Path)
		if err != nil {
			return PullRequestSourceBinding{}, fmt.Errorf("read exact PR baseline identity: %w", err)
		}
		if !bytes.HasPrefix(data, pngSignature) {
			return PullRequestSourceBinding{}, fmt.Errorf("PR baseline identity is not an exact PNG")
		}
		baselineBytes += identity.Bytes
		if baselineBytes > maxPullRequestBaselinesBytes {
			return PullRequestSourceBinding{}, fmt.Errorf("PR baseline identity inventory exceeds its byte bound")
		}
		baselineRecords = append(baselineRecords, identity.Path+"\x00"+identity.SHA256+"\x00"+strconv.FormatInt(identity.Bytes, 10))
	}
	if digest([]byte(strings.Join(baselineRecords, "\n"))) != binding.Baselines.SHA256 {
		return PullRequestSourceBinding{}, fmt.Errorf("PR baseline aggregate identity mismatch")
	}
	if err := bundle.verifyRunnerExecutionAttestation(binding); err != nil {
		return PullRequestSourceBinding{}, err
	}
	if err := bundle.verifyRuntimeAndExecutionBinding(binding); err != nil {
		return PullRequestSourceBinding{}, err
	}
	if err := bundle.verifyPlanReportContractBinding(binding); err != nil {
		return PullRequestSourceBinding{}, err
	}
	return binding, nil
}

func canonicalPullRequestGeneratedPaths(specPath, configPath string) bool {
	if !canonicalPullRequestRepositoryPath(specPath) || !canonicalPullRequestRepositoryPath(configPath) || !strings.HasPrefix(specPath, ".visual-hive/generated/") || specPath == configPath {
		return false
	}
	return configPath == pathpkg.Join(pathpkg.Dir(specPath), "visual-hive.generated.config.cjs")
}

func canonicalPullRequestRepositoryPath(value string) bool {
	if value == "" || value == "." || pathpkg.IsAbs(value) || pathpkg.Clean(value) != value || strings.Contains(value, "\\") || strings.HasPrefix(value, "../") ||
		(len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && value[2] == '/') {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.TrimSpace(part) != part {
			return false
		}
		for _, character := range part {
			if character < 0x20 || character == 0x7f {
				return false
			}
		}
	}
	return true
}

func equalPullRequestSourceIdentity(binding PullRequestSourceBinding, expected PullRequestSourceExpectations) bool {
	return strings.EqualFold(binding.Repository, expected.Repository) && binding.RepositoryID == expected.RepositoryID && binding.PullRequest == expected.PullRequest &&
		binding.Base == expected.Base && binding.Head == expected.Head &&
		binding.Workflow.Name == expected.WorkflowName && binding.Workflow.Path == expected.WorkflowPath &&
		binding.Workflow.Event == "pull_request" && binding.Workflow.RunID == expected.WorkflowRunID && binding.Workflow.RunAttempt == expected.WorkflowRunAttempt &&
		binding.Workflow.Definition.SHA256 == expected.WorkflowDefinitionSHA256 && binding.Workflow.ProducerCommit == expected.ProducerCommit &&
		binding.Workflow.SourceName == expected.SourceArtifactName && binding.Workflow.BundleName == expected.BundleArtifactName &&
		binding.Config.SHA256 == expected.ConfigSHA256
}

func (bundle *ValidatedBundle) verifyBoundSourceFile(identity PullRequestFileIdentity) error {
	if identity.Path == "" || !hexDigest.MatchString(identity.SHA256) || identity.Bytes <= 0 {
		return fmt.Errorf("file identity is incomplete")
	}
	_, size, sha, err := bundle.readVerifiedSourceFile(identity.Path)
	if err != nil {
		return err
	}
	if size != identity.Bytes || sha != identity.SHA256 {
		return fmt.Errorf("file identity does not match the immutable source index")
	}
	return nil
}

func (bundle *ValidatedBundle) readVerifiedSourceJSON(path string, target interface{}) error {
	data, _, _, err := bundle.readVerifiedSourceFile(path)
	if err != nil {
		return err
	}
	if err := decodeStrict(bytes.NewReader(data), target); err != nil {
		return err
	}
	return nil
}

func (bundle *ValidatedBundle) readVerifiedSourceJSONLoose(path string, target interface{}) error {
	data, _, _, err := bundle.readVerifiedSourceFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return err
	}
	return nil
}

type pullRequestRunnerAttestation struct {
	SchemaVersion       string `json:"schemaVersion"`
	Algorithm           string `json:"algorithm"`
	KeyCommitmentSHA256 string `json:"keyCommitmentSha256"`
	KeyBase64           string `json:"keyBase64"`
	PayloadSHA256       string `json:"payloadSha256"`
	PayloadBase64       string `json:"payloadBase64"`
	MACSHA256           string `json:"macSha256"`
}

type pullRequestRunnerExecutionPayload struct {
	SchemaVersion    string                  `json:"schemaVersion"`
	Repository       string                  `json:"repository"`
	PullRequest      int                     `json:"pullRequest"`
	BaseRef          string                  `json:"baseRef"`
	HeadRef          string                  `json:"headRef"`
	HeadSHA          string                  `json:"headSha"`
	RunID            string                  `json:"runId"`
	RunAttempt       string                  `json:"runAttempt"`
	Workflow         string                  `json:"workflow"`
	ProducerCommit   string                  `json:"producerCommit"`
	ExecutionBinding map[string]string       `json:"executionBinding"`
	Report           PullRequestFileIdentity `json:"report"`
	Runtime          PullRequestFileIdentity `json:"runtime"`
	GeneratedSpec    PullRequestFileIdentity `json:"generatedSpec"`
	GeneratedConfig  PullRequestFileIdentity `json:"generatedConfig"`
	PipelineExit     PullRequestFileIdentity `json:"pipelineExit"`
	RunnerOutcome    PullRequestFileIdentity `json:"runnerOutcome"`
}

func (bundle *ValidatedBundle) verifyRunnerExecutionAttestation(binding PullRequestSourceBinding) error {
	attestationData, _, _, err := bundle.readVerifiedSourceFile(PullRequestRunnerAttestationPath)
	if err != nil {
		return fmt.Errorf("verify runner-owned execution attestation: %w", err)
	}
	if len(attestationData) == 0 || len(attestationData) > 2<<20 {
		return fmt.Errorf("runner-owned execution attestation exceeds its byte bound")
	}
	var attestation pullRequestRunnerAttestation
	if err := decodeStrict(bytes.NewReader(attestationData), &attestation); err != nil {
		return fmt.Errorf("decode runner-owned execution attestation: %w", err)
	}
	if attestation.SchemaVersion != PullRequestRunnerAttestationSchema || attestation.Algorithm != "HMAC-SHA256" ||
		!hexDigest.MatchString(attestation.KeyCommitmentSHA256) || !hexDigest.MatchString(attestation.PayloadSHA256) || !hexDigest.MatchString(attestation.MACSHA256) {
		return fmt.Errorf("runner-owned execution attestation schema, algorithm, or digest identity is invalid")
	}
	strictBase64 := base64.StdEncoding.Strict()
	key, err := strictBase64.DecodeString(attestation.KeyBase64)
	if err != nil || len(key) != 32 || strictBase64.EncodeToString(key) != attestation.KeyBase64 || bytes.Equal(key, make([]byte, 32)) {
		return fmt.Errorf("runner-owned execution attestation key is not one canonical nonzero 256-bit value")
	}
	payloadData, err := strictBase64.DecodeString(attestation.PayloadBase64)
	if err != nil || len(payloadData) == 0 || len(payloadData) > 1<<20 || strictBase64.EncodeToString(payloadData) != attestation.PayloadBase64 {
		return fmt.Errorf("runner-owned execution attestation payload is not canonical bounded base64")
	}
	if digest(key) != attestation.KeyCommitmentSHA256 || digest(payloadData) != attestation.PayloadSHA256 {
		return fmt.Errorf("runner-owned execution attestation key commitment or payload digest mismatch")
	}
	wantedMAC, err := hex.DecodeString(attestation.MACSHA256)
	if err != nil {
		return fmt.Errorf("decode runner-owned execution attestation MAC: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payloadData)
	if !hmac.Equal(mac.Sum(nil), wantedMAC) {
		return fmt.Errorf("runner-owned execution attestation HMAC verification failed")
	}
	var payload pullRequestRunnerExecutionPayload
	if err := decodeStrict(bytes.NewReader(payloadData), &payload); err != nil {
		return fmt.Errorf("decode authenticated runner execution payload: %w", err)
	}
	if payload.SchemaVersion != PullRequestRunnerExecutionSchema || !strings.EqualFold(payload.Repository, binding.Repository) || payload.PullRequest != binding.PullRequest ||
		payload.BaseRef != binding.Base.Ref || payload.HeadRef != binding.Head.Ref || payload.HeadSHA != binding.Head.SHA ||
		payload.RunID != binding.Workflow.RunID || payload.RunAttempt != binding.Workflow.RunAttempt || payload.Workflow != binding.Workflow.Name ||
		payload.ProducerCommit != binding.Workflow.ProducerCommit || !validPullRequestExecutionBinding(payload.ExecutionBinding) ||
		payload.Report != binding.Report || payload.Runtime != binding.Runtime.Receipt || payload.GeneratedSpec != binding.Runtime.GeneratedSpec || payload.GeneratedConfig != binding.Runtime.GeneratedConfig ||
		payload.PipelineExit.Path != PullRequestPipelineExitPath || payload.RunnerOutcome.Path != PullRequestRunnerOutcomePath {
		return fmt.Errorf("authenticated runner execution payload does not match the exact PR/source binding")
	}
	if err := bundle.verifyBoundSourceFile(payload.PipelineExit); err != nil {
		return fmt.Errorf("runner pipeline-exit identity: %w", err)
	}
	if err := bundle.verifyBoundSourceFile(payload.RunnerOutcome); err != nil {
		return fmt.Errorf("runner outcome identity: %w", err)
	}
	pipelineExit, _, _, err := bundle.readVerifiedSourceFile(PullRequestPipelineExitPath)
	if err != nil || !bytes.Equal(pipelineExit, []byte("0\n")) {
		return fmt.Errorf("authenticated runner pipeline exit is not exact success")
	}
	runnerOutcomeData, _, _, err := bundle.readVerifiedSourceFile(PullRequestRunnerOutcomePath)
	if err != nil {
		return fmt.Errorf("read authenticated runner outcome: %w", err)
	}
	var runnerOutcome struct {
		SchemaVersion string `json:"schemaVersion"`
		Outcome       string `json:"outcome"`
		Conclusion    string `json:"conclusion"`
	}
	if err := decodeStrict(bytes.NewReader(runnerOutcomeData), &runnerOutcome); err != nil || runnerOutcome.SchemaVersion != "hive.visual-runner-outcome.v1" ||
		runnerOutcome.Outcome != "success" || runnerOutcome.Conclusion != "success" {
		return fmt.Errorf("authenticated runner outcome is not exact success")
	}
	var report, runtime struct {
		ExecutionBinding map[string]string `json:"executionBinding"`
	}
	if err := bundle.readVerifiedSourceJSONLoose(PullRequestReportPath, &report); err != nil {
		return fmt.Errorf("read attested report execution binding: %w", err)
	}
	if err := bundle.readVerifiedSourceJSONLoose(PullRequestRuntimePath, &runtime); err != nil {
		return fmt.Errorf("read attested runtime execution binding: %w", err)
	}
	if !stringMapsEqual(payload.ExecutionBinding, report.ExecutionBinding) || !stringMapsEqual(payload.ExecutionBinding, runtime.ExecutionBinding) {
		return fmt.Errorf("authenticated runner execution binding does not match report and runtime evidence")
	}
	return nil
}

func validPullRequestExecutionBinding(binding map[string]string) bool {
	if len(binding) != 5 {
		return false
	}
	for _, key := range []string{"nonceSha256", "generatedSpecSha256", "generatedConfigSha256", "payloadSha256", "bindingMacSha256"} {
		if !hexDigest.MatchString(binding[key]) {
			return false
		}
	}
	return true
}

func (bundle *ValidatedBundle) verifyRuntimeAndExecutionBinding(binding PullRequestSourceBinding) error {
	var runtime playwrightRuntimeReceipt
	if err := bundle.readVerifiedSourceJSON(PullRequestRuntimePath, &runtime); err != nil {
		return fmt.Errorf("verify real Playwright runtime receipt: %w", err)
	}
	if runtime.SchemaVersion != PullRequestRuntimeSchema || strings.TrimSpace(runtime.CapturedAt) == "" || len(runtime.ExecutionBinding) == 0 ||
		runtime.Browser.Name == "" || runtime.Browser.Version == "" || runtime.Environment.OS == "" || runtime.Environment.Architecture == "" ||
		runtime.Environment.NodeVersion == "" || runtime.Environment.PlaywrightVersion == "" || runtime.Environment.Locale == "" || runtime.Environment.Timezone == "" ||
		runtime.Environment.UserAgent == "" || runtime.Environment.DeviceScaleFactor == "" || len(runtime.Environment.Fonts) == 0 {
		return fmt.Errorf("real Playwright runtime receipt is incomplete")
	}
	if !validPullRequestExecutionBinding(runtime.ExecutionBinding) {
		return fmt.Errorf("runtime execution binding contains an unrecognized identity")
	}
	if runtime.ExecutionBinding["generatedSpecSha256"] != binding.Runtime.GeneratedSpec.SHA256 || runtime.ExecutionBinding["generatedConfigSha256"] != binding.Runtime.GeneratedConfig.SHA256 {
		return fmt.Errorf("runtime execution binding does not match the indexed generated spec and config bytes")
	}
	stable := struct {
		Browser     playwrightRuntimeBrowser `json:"browser"`
		Environment playwrightEnvironment    `json:"environment"`
	}{runtime.Browser, runtime.Environment}
	stableData, err := json.Marshal(stable)
	if err != nil {
		return err
	}
	bindingData, err := json.Marshal(runtime.ExecutionBinding)
	if err != nil {
		return err
	}
	if digest(stableData) != binding.Runtime.StableSHA256 || digest(bindingData) != binding.Runtime.ExecutionBindingSHA256 {
		return fmt.Errorf("runtime stable identity or execution binding digest mismatch")
	}
	var report struct {
		ExecutionBinding map[string]string `json:"executionBinding"`
	}
	if err := bundle.readVerifiedSourceJSONLoose(PullRequestReportPath, &report); err != nil {
		return fmt.Errorf("verify deterministic report execution binding: %w", err)
	}
	if len(report.ExecutionBinding) == 0 || !stringMapsEqual(report.ExecutionBinding, runtime.ExecutionBinding) {
		return fmt.Errorf("runtime receipt and deterministic report execution bindings do not match")
	}
	return nil
}

func (bundle *ValidatedBundle) verifyPlanReportContractBinding(binding PullRequestSourceBinding) error {
	var plan struct {
		SchemaVersion         int      `json:"schemaVersion"`
		Mode                  string   `json:"mode"`
		ChangedFiles          []string `json:"changedFiles"`
		EffectiveChangedFiles []string `json:"effectiveChangedFiles"`
		IgnoredChangedFiles   []struct {
			File    string `json:"file"`
			Pattern string `json:"pattern"`
			Reason  string `json:"reason"`
		} `json:"ignoredChangedFiles"`
		Items []struct {
			ContractID string `json:"contractId"`
			TargetID   string `json:"targetId"`
		} `json:"items"`
	}
	if err := bundle.readVerifiedSourceJSONLoose(PullRequestPlanPath, &plan); err != nil {
		return fmt.Errorf("verify PR deterministic plan: %w", err)
	}
	if plan.SchemaVersion != 1 || plan.Mode != "pr" {
		return fmt.Errorf("PR plan schema or mode identity mismatch")
	}
	if len(plan.Items) == 0 {
		return fmt.Errorf("ignored-files/no-contract PR plan cannot produce an admissible v3 verdict")
	}
	contracts := make([]string, 0, len(plan.Items))
	planTargets := make(map[string]string, len(plan.Items))
	seen := map[string]bool{}
	for _, item := range plan.Items {
		id := strings.TrimSpace(item.ContractID)
		targetID := strings.TrimSpace(item.TargetID)
		if id == "" || targetID == "" || id != item.ContractID || targetID != item.TargetID || !canonicalPullRequestTextValue(id) || !canonicalPullRequestTextValue(targetID) || seen[id] {
			return fmt.Errorf("PR plan contract identity is empty or duplicated")
		}
		seen[id] = true
		planTargets[id] = targetID
		contracts = append(contracts, id)
	}
	sort.Strings(contracts)
	planContractData, _, _, err := bundle.readVerifiedSourceFile(PullRequestPlanContractsPath)
	if err != nil {
		return fmt.Errorf("verify PR evaluated plan-contract receipt: %w", err)
	}
	planContractReceipt, canonicalPlanContracts := splitSortedUniqueTextLines(string(planContractData))
	if !canonicalPlanContracts || !stringSlicesEqual(contracts, planContractReceipt) {
		return fmt.Errorf("PR plan and evaluated plan-contract receipt do not exactly match")
	}
	evaluationScopeData, _, _, err := bundle.readVerifiedSourceFile(PullRequestEvaluationScopesPath)
	if err != nil {
		return fmt.Errorf("verify PR full evaluation-scope receipt: %w", err)
	}
	evaluationScopes, canonicalEvaluationScopes := splitSortedUniqueTextLines(string(evaluationScopeData))
	manifestScopes := sortedUniqueValues(bundle.validatedManifest.Scan.EvaluatedContracts)
	if !canonicalEvaluationScopes || !canonicalPullRequestTextValues(bundle.validatedManifest.Scan.EvaluatedContracts) ||
		len(manifestScopes) != len(bundle.validatedManifest.Scan.EvaluatedContracts) || !stringSlicesEqual(evaluationScopes, manifestScopes) {
		return fmt.Errorf("PR full evaluation-scope receipt does not exactly match the bundle manifest")
	}
	planContractSet := make(map[string]bool, len(planContractReceipt))
	for _, contract := range planContractReceipt {
		planContractSet[contract] = true
	}
	for _, scope := range evaluationScopes {
		if !planContractSet[scope] && !allowedPullRequestLifecycleScope(scope) {
			return fmt.Errorf("PR full evaluation-scope receipt contains a non-deterministic lifecycle scope")
		}
	}
	for _, contract := range planContractReceipt {
		if !containsSortedString(evaluationScopes, contract) {
			return fmt.Errorf("PR full evaluation-scope receipt omits an executed plan contract")
		}
	}
	var report struct {
		SchemaVersion int `json:"schemaVersion"`
		Repository    struct {
			Provider          string `json:"provider"`
			Repository        string `json:"repository"`
			Branch            string `json:"branch"`
			BaseBranch        string `json:"baseBranch"`
			CommitSHA         string `json:"commitSha"`
			PullRequestNumber *int   `json:"pullRequestNumber"`
			RunID             string `json:"runId"`
			RunAttempt        string `json:"runAttempt"`
			Workflow          string `json:"workflow"`
		} `json:"repository"`
		Mode            string   `json:"mode"`
		Status          string   `json:"status"`
		ChangedFiles    []string `json:"changedFiles"`
		SelectedTargets []struct {
			ID string `json:"id"`
		} `json:"selectedTargets"`
		SelectedContracts []string `json:"selectedContracts"`
		GeneratedSpecPath string   `json:"generatedSpecPath"`
		Results           []struct {
			ContractID           string `json:"contractId"`
			TargetID             string `json:"targetId"`
			Status               string `json:"status"`
			ScreenshotAssertions []struct {
				BaselinePath string `json:"baselinePath"`
				Status       string `json:"status"`
			} `json:"screenshotAssertions"`
		} `json:"results"`
	}
	if err := bundle.readVerifiedSourceJSONLoose(PullRequestReportPath, &report); err != nil {
		return fmt.Errorf("verify PR deterministic report contract inventory: %w", err)
	}
	if report.SchemaVersion != 2 || report.Repository.Provider != "github-actions" ||
		!strings.EqualFold(report.Repository.Repository, binding.Repository) || report.Repository.Branch != binding.Head.Ref || report.Repository.BaseBranch != binding.Base.Ref ||
		report.Repository.CommitSHA != binding.Head.SHA || (report.Repository.PullRequestNumber != nil && *report.Repository.PullRequestNumber != binding.PullRequest) || report.Repository.Workflow != binding.Workflow.Name ||
		report.Repository.RunID != binding.Workflow.RunID || report.Repository.RunAttempt != binding.Workflow.RunAttempt ||
		report.Mode != "pr" || report.Status != "passed" || report.GeneratedSpecPath != binding.Runtime.GeneratedSpec.Path {
		return fmt.Errorf("PR report repository, mode, status, or generated spec identity mismatch")
	}
	selectedContracts := sortedUniqueValues(report.SelectedContracts)
	if !canonicalPullRequestTextValues(report.SelectedContracts) || len(selectedContracts) != len(report.SelectedContracts) || !stringSlicesEqual(selectedContracts, contracts) || len(report.Results) != len(contracts) {
		return fmt.Errorf("PR report selected-contract inventory does not exactly match the plan")
	}
	resultContracts := make([]string, 0, len(report.Results))
	resultTargets := map[string]bool{}
	baselineSources := map[string]bool{}
	for _, result := range report.Results {
		if result.Status != "passed" || !canonicalPullRequestTextValue(result.ContractID) || !canonicalPullRequestTextValue(result.TargetID) || planTargets[result.ContractID] == "" || planTargets[result.ContractID] != result.TargetID {
			return fmt.Errorf("PR report result does not match an exact passing plan contract/target")
		}
		resultContracts = append(resultContracts, result.ContractID)
		resultTargets[result.TargetID] = true
		for _, assertion := range result.ScreenshotAssertions {
			baselinePath := strings.TrimSpace(assertion.BaselinePath)
			if assertion.Status != "passed" || baselinePath == "" || baselinePath != assertion.BaselinePath || !canonicalPullRequestRepositoryPath(baselinePath) || baselineSources[baselinePath] {
				return fmt.Errorf("PR report screenshot baseline identity is empty, duplicated, or non-passing")
			}
			baselineSources[baselinePath] = true
		}
	}
	sort.Strings(resultContracts)
	if !stringSlicesEqual(resultContracts, contracts) {
		return fmt.Errorf("PR report result inventory does not exactly match the plan")
	}
	boundBaselineSources := make([]string, 0, len(binding.Baselines.Files))
	for _, identity := range binding.Baselines.Files {
		boundBaselineSources = append(boundBaselineSources, strings.TrimPrefix(identity.Path, pullRequestBaselinePrefix))
	}
	sort.Strings(boundBaselineSources)
	reportedBaselineSources := make([]string, 0, len(baselineSources))
	for baselinePath := range baselineSources {
		reportedBaselineSources = append(reportedBaselineSources, baselinePath)
	}
	sort.Strings(reportedBaselineSources)
	if len(reportedBaselineSources) == 0 || !stringSlicesEqual(reportedBaselineSources, boundBaselineSources) {
		return fmt.Errorf("PR report screenshot baselines do not exactly match the sealed baseline inventory")
	}
	reportTargetIDs := make([]string, 0, len(report.SelectedTargets))
	for _, target := range report.SelectedTargets {
		reportTargetIDs = append(reportTargetIDs, target.ID)
	}
	selectedTargets := sortedUniqueValues(reportTargetIDs)
	expectedTargets := make([]string, 0, len(resultTargets))
	for target := range resultTargets {
		expectedTargets = append(expectedTargets, target)
	}
	sort.Strings(expectedTargets)
	if !canonicalPullRequestTextValues(reportTargetIDs) || len(selectedTargets) != len(report.SelectedTargets) || !stringSlicesEqual(selectedTargets, expectedTargets) {
		return fmt.Errorf("PR report selected-target inventory does not exactly match plan results")
	}
	changed, _, _, err := bundle.readVerifiedSourceFile(PullRequestChangedFilesPath)
	if err != nil {
		return err
	}
	files, canonicalChangedLines := splitSortedUniqueLines(string(changed))
	planFiles := sortedUniqueValues(plan.ChangedFiles)
	effectivePlanFiles := sortedUniqueValues(plan.EffectiveChangedFiles)
	reportFiles := sortedUniqueValues(report.ChangedFiles)
	if !canonicalChangedLines || len(files) == 0 || !canonicalPullRequestRepositoryPaths(plan.ChangedFiles) || !canonicalPullRequestRepositoryPaths(plan.EffectiveChangedFiles) || !canonicalPullRequestRepositoryPaths(report.ChangedFiles) ||
		len(planFiles) != len(plan.ChangedFiles) || len(effectivePlanFiles) != len(plan.EffectiveChangedFiles) || len(reportFiles) != len(report.ChangedFiles) ||
		!stringSlicesEqual(files, planFiles) || !stringSlicesEqual(files, reportFiles) || !stringSlicesEqual(files, bundle.validatedManifest.Scan.EvaluatedFiles) {
		return fmt.Errorf("PR plan/report changed-file source and bundle evaluated-file identities do not match")
	}
	if plan.IgnoredChangedFiles == nil {
		return fmt.Errorf("PR plan ignored changed-file inventory is missing")
	}
	ignoredSet := make(map[string]bool, len(plan.IgnoredChangedFiles))
	ignoredPlanFiles := make([]string, 0, len(plan.IgnoredChangedFiles))
	for _, ignored := range plan.IgnoredChangedFiles {
		if !canonicalPullRequestRepositoryPath(ignored.File) || !canonicalPullRequestTextValue(ignored.Pattern) || !canonicalPullRequestTextValue(ignored.Reason) ||
			ignoredSet[ignored.File] || !containsSortedString(files, ignored.File) {
			return fmt.Errorf("PR plan ignored changed-file identity is invalid, duplicated, or outside the exact runner-owned diff")
		}
		ignoredSet[ignored.File] = true
		ignoredPlanFiles = append(ignoredPlanFiles, ignored.File)
	}
	expectedIgnoredFiles := make([]string, 0, len(ignoredSet))
	expectedEffectiveFiles := make([]string, 0, len(files)-len(ignoredSet))
	for _, file := range plan.ChangedFiles {
		if ignoredSet[file] {
			expectedIgnoredFiles = append(expectedIgnoredFiles, file)
		} else {
			expectedEffectiveFiles = append(expectedEffectiveFiles, file)
		}
	}
	if !stringSlicesEqual(ignoredPlanFiles, expectedIgnoredFiles) || !stringSlicesEqual(plan.EffectiveChangedFiles, expectedEffectiveFiles) {
		return fmt.Errorf("PR plan effective and ignored changed files do not exactly partition the runner-owned diff")
	}
	return nil
}

func sortedUniqueValues(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func canonicalPullRequestTextValue(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func canonicalPullRequestTextValues(values []string) bool {
	for _, value := range values {
		if !canonicalPullRequestTextValue(value) {
			return false
		}
	}
	return true
}

func canonicalPullRequestRepositoryPaths(values []string) bool {
	for _, value := range values {
		if !canonicalPullRequestRepositoryPath(value) {
			return false
		}
	}
	return true
}

func allowedPullRequestLifecycleScope(value string) bool {
	if value == "workflow-safety" || value == "provider-governance" {
		return true
	}
	const prefix = "testing-layer:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	layer, err := strconv.Atoi(strings.TrimPrefix(value, prefix))
	return err == nil && layer >= 0 && layer <= 11 && value == prefix+strconv.Itoa(layer)
}

func containsSortedString(values []string, target string) bool {
	position := sort.SearchStrings(values, target)
	return position < len(values) && values[position] == target
}

func stringMapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func splitSortedUniqueLines(value string) ([]string, bool) {
	seen := map[string]bool{}
	result := []string{}
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		if !canonicalPullRequestRepositoryPath(line) || seen[line] {
			return nil, false
		}
		seen[line] = true
		result = append(result, line)
	}
	sort.Strings(result)
	return result, true
}

func splitSortedUniqueTextLines(value string) ([]string, bool) {
	seen := map[string]bool{}
	result := []string{}
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		if !canonicalPullRequestTextValue(line) || seen[line] {
			return nil, false
		}
		seen[line] = true
		result = append(result, line)
	}
	sort.Strings(result)
	return result, true
}
