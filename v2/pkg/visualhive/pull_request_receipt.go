package visualhive

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	// PullRequestCheckReceiptSchema is the only receipt admitted by the
	// pull-request check-evidence lifecycle path.
	PullRequestCheckReceiptSchema = "hive.visual-hive-pr-check-receipt.v1"

	pullRequestCheckReplayNamespace = "visual-hive-pr-check-receipt:v1:"
)

// PullRequestCheckAuthority makes the deliberately narrow authority of this
// receipt explicit. The GitHub verifier, not this package, seals these fields.
type PullRequestCheckAuthority struct {
	CheckEvidenceOnly     bool `json:"checkEvidenceOnly"`
	ResolveIssues         bool `json:"resolveIssues"`
	WriteBaselines        bool `json:"writeBaselines"`
	MergePullRequest      bool `json:"mergePullRequest"`
	WriteTargetRepository bool `json:"writeTargetRepository"`
}

type PullRequestCheckArtifactIdentity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type PullRequestCheckWorkflowIdentity struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	Path         string                  `json:"path"`
	Event        string                  `json:"event"`
	RunID        string                  `json:"runId"`
	RunAttempt   string                  `json:"runAttempt"`
	CheckSuiteID string                  `json:"checkSuiteId"`
	JobID        string                  `json:"jobId"`
	CheckRunID   string                  `json:"checkRunId"`
	AppID        string                  `json:"appId"`
	Definition   PullRequestFileIdentity `json:"definition"`
}

type PullRequestCheckBundleIdentity struct {
	SchemaVersion  string               `json:"schemaVersion"`
	BundleID       string               `json:"bundleId"`
	OverallSHA256  string               `json:"overallSha256"`
	ManifestSHA256 string               `json:"manifestSha256"`
	ArtifactIndex  ArtifactIndexBinding `json:"artifactIndex"`
}

type PullRequestCheckResultIdentity struct {
	State      string `json:"state"`
	Conclusion string `json:"conclusion"`
	Summary    string `json:"summary"`
	RunURL     string `json:"runUrl"`
}

// PullRequestCheckReceiptIdentity is the stable payload that the independently
// verified GitHub type seals. This local data structure is not an
// authority-bearing capability.
type PullRequestCheckReceiptIdentity struct {
	SchemaVersion  string                           `json:"schemaVersion"`
	Source         PullRequestSourceBinding         `json:"source"`
	Workflow       PullRequestCheckWorkflowIdentity `json:"workflow"`
	SourceArtifact PullRequestCheckArtifactIdentity `json:"sourceArtifact"`
	BundleArtifact PullRequestCheckArtifactIdentity `json:"bundleArtifact"`
	Producer       Producer                         `json:"producer"`
	Bundle         PullRequestCheckBundleIdentity   `json:"bundle"`
	Check          PullRequestCheckResultIdentity   `json:"check"`
	Authority      PullRequestCheckAuthority        `json:"authority"`
	ReplayKey      string                           `json:"replayKey"`
}

// PullRequestCheckReceiptSnapshot is safe to persist or display. Only the
// private verified type in pkg/github may assert that its digest is a seal over
// independently verified provenance.
type PullRequestCheckReceiptSnapshot struct {
	Identity      PullRequestCheckReceiptIdentity `json:"identity"`
	ReceiptSHA256 string                          `json:"receiptSha256"`
}

func validatePullRequestCheckIdentity(identity PullRequestCheckReceiptIdentity) error {
	if identity.SchemaVersion != PullRequestCheckReceiptSchema || identity.Source.SchemaVersion != PullRequestSourceBindingSchema || identity.Source.PullRequest <= 0 {
		return fmt.Errorf("PR check receipt schema or pull request identity is invalid")
	}
	if !validPullRequestRepository(identity.Source.Repository) || !validPositiveDecimal(identity.Source.RepositoryID) ||
		!validPullRequestRevision(identity.Source.Base) || !validPullRequestRevision(identity.Source.Head) ||
		!strings.EqualFold(identity.Source.Repository, identity.Source.Base.Repository) || identity.Source.RepositoryID != identity.Source.Base.RepositoryID {
		return fmt.Errorf("PR check receipt repository, base, or head identity is invalid")
	}
	if identity.Workflow.Event != "pull_request" || identity.Source.Workflow.Event != "pull_request" ||
		!validPositiveDecimal(identity.Workflow.ID) || !validPositiveDecimal(identity.Workflow.RunID) || !validPositiveDecimal(identity.Workflow.RunAttempt) ||
		!validPositiveDecimal(identity.Workflow.CheckSuiteID) || !validPositiveDecimal(identity.Workflow.JobID) || !validPositiveDecimal(identity.Workflow.CheckRunID) || !validPositiveDecimal(identity.Workflow.AppID) ||
		identity.Workflow.Name == "" || identity.Workflow.Path == "" || strings.ContainsRune(identity.Workflow.Path, '\x00') ||
		identity.Workflow.Name != identity.Source.Workflow.Name || identity.Workflow.Path != identity.Source.Workflow.Path ||
		identity.Workflow.RunID != identity.Source.Workflow.RunID || identity.Workflow.RunAttempt != identity.Source.Workflow.RunAttempt ||
		identity.Workflow.Definition != identity.Source.Workflow.Definition || identity.Workflow.Definition.Path != PullRequestWorkflowPath ||
		!validPullRequestFileIdentity(identity.Workflow.Definition) {
		return fmt.Errorf("PR check receipt workflow definition or run identity is invalid")
	}
	if !validPositiveDecimal(identity.SourceArtifact.ID) || !validPositiveDecimal(identity.BundleArtifact.ID) || identity.SourceArtifact.ID == identity.BundleArtifact.ID ||
		identity.SourceArtifact.Name == "" || identity.BundleArtifact.Name == "" || identity.SourceArtifact.Name == identity.BundleArtifact.Name ||
		identity.SourceArtifact.Name != identity.Source.Workflow.SourceName || identity.BundleArtifact.Name != identity.Source.Workflow.BundleName ||
		identity.SourceArtifact.Name != "visual-hive-pr-source-"+identity.Workflow.RunID+"-"+identity.Workflow.RunAttempt ||
		identity.BundleArtifact.Name != "visual-hive-pr-bundle-"+identity.Workflow.RunID+"-"+identity.Workflow.RunAttempt {
		return fmt.Errorf("PR check receipt requires distinct exact source and bundle artifact identities")
	}
	if identity.Producer.Name == "" || identity.Producer.Version == "" || !exactCommit.MatchString(identity.Producer.GitCommit) ||
		identity.Producer.GitCommit != identity.Source.Workflow.ProducerCommit {
		return fmt.Errorf("PR check receipt producer identity is invalid")
	}
	index := identity.Bundle.ArtifactIndex
	if identity.Bundle.SchemaVersion != ManifestSchemaV3 || identity.Bundle.BundleID == "" || !hexDigest.MatchString(identity.Bundle.OverallSHA256) ||
		!hexDigest.MatchString(identity.Bundle.ManifestSHA256) || index.Path == "" || index.SourcePath == "" || !hexDigest.MatchString(index.SHA256) ||
		index.SchemaVersion != artifactIndexSchemaVersion || !index.ContentAddressed || !index.Complete || index.ArtifactCount <= 0 || index.TotalBytes <= 0 {
		return fmt.Errorf("PR check receipt bundle v3, manifest, or index identity is invalid")
	}
	if !validPullRequestFileIdentity(identity.Source.Plan) || !validPullRequestFileIdentity(identity.Source.Report) ||
		!validPullRequestFileIdentity(identity.Source.Config) || !validPullRequestFileIdentity(identity.Source.ChangedFiles) ||
		!validPullRequestFileIdentity(identity.Source.PlanContracts) || !validPullRequestFileIdentity(identity.Source.EvaluationScopes) ||
		!validPullRequestFileIdentity(identity.Source.RunnerAttestation) ||
		!validPullRequestFileIdentity(identity.Source.Runtime.Receipt) || !validPullRequestFileIdentity(identity.Source.Runtime.GeneratedSpec) ||
		!validPullRequestFileIdentity(identity.Source.Runtime.GeneratedConfig) || !hexDigest.MatchString(identity.Source.Runtime.StableSHA256) ||
		!hexDigest.MatchString(identity.Source.Runtime.ExecutionBindingSHA256) || !hexDigest.MatchString(identity.Source.Baselines.SHA256) || len(identity.Source.Baselines.Files) == 0 {
		return fmt.Errorf("PR check receipt plan, config, runtime, execution, or baseline identity is invalid")
	}
	if identity.Source.Plan.Path != PullRequestPlanPath || identity.Source.Report.Path != PullRequestReportPath ||
		identity.Source.Config.Path != PullRequestConfigPath || identity.Source.ChangedFiles.Path != PullRequestChangedFilesPath ||
		identity.Source.PlanContracts.Path != PullRequestPlanContractsPath || identity.Source.EvaluationScopes.Path != PullRequestEvaluationScopesPath ||
		identity.Source.RunnerAttestation.Path != PullRequestRunnerAttestationPath ||
		identity.Source.Runtime.Receipt.Path != PullRequestRuntimePath ||
		!strings.HasPrefix(identity.Source.Runtime.GeneratedSpec.Path, ".visual-hive/") || !strings.HasPrefix(identity.Source.Runtime.GeneratedConfig.Path, ".visual-hive/") {
		return fmt.Errorf("PR check receipt uses an unexpected fixed source evidence path")
	}
	if !sort.SliceIsSorted(identity.Source.Baselines.Files, func(i, j int) bool {
		return identity.Source.Baselines.Files[i].Path < identity.Source.Baselines.Files[j].Path
	}) {
		return fmt.Errorf("PR check receipt baseline identities are not deterministically sorted")
	}
	for index := range identity.Source.Baselines.Files {
		if !validPullRequestFileIdentity(identity.Source.Baselines.Files[index]) ||
			!strings.HasPrefix(identity.Source.Baselines.Files[index].Path, ".visual-hive/proof/pr/baselines/") ||
			(index > 0 && identity.Source.Baselines.Files[index-1].Path == identity.Source.Baselines.Files[index].Path) {
			return fmt.Errorf("PR check receipt baseline file identity is invalid or duplicated")
		}
	}
	if identity.Check.State != "success" || identity.Check.Conclusion != "success" || strings.TrimSpace(identity.Check.Summary) == "" ||
		len(identity.Check.Summary) > 4096 || strings.ContainsRune(identity.Check.Summary, '\x00') || !validPullRequestRunURL(identity.Check.RunURL) {
		return fmt.Errorf("PR check receipt result identity is invalid")
	}
	wantedAuthority := PullRequestCheckAuthority{CheckEvidenceOnly: true}
	if identity.Authority != wantedAuthority {
		return fmt.Errorf("PR check receipt attempted to acquire authority beyond check evidence")
	}
	if !hexDigest.MatchString(identity.ReplayKey) || identity.ReplayKey != PullRequestCheckReplayKey(identity) {
		return fmt.Errorf("PR check receipt replay identity is invalid")
	}
	return nil
}

// PullRequestCheckReplayKey binds only the immutable transport/provenance
// tuple. Content identities stay in the sealed receipt digest, so changed
// content under the same run and artifact IDs is a conflict rather than a new
// replay domain.
func PullRequestCheckReplayKey(identity PullRequestCheckReceiptIdentity) string {
	parts := []string{
		"hive.visual-hive-pr-check-replay.v1", strings.ToLower(identity.Source.Repository), identity.Source.RepositoryID,
		strconv.Itoa(identity.Source.PullRequest), strings.ToLower(identity.Source.Base.Repository), identity.Source.Base.RepositoryID, identity.Source.Base.Ref, identity.Source.Base.SHA,
		strings.ToLower(identity.Source.Head.Repository), identity.Source.Head.RepositoryID, identity.Source.Head.Ref, identity.Source.Head.SHA,
		identity.Workflow.ID, identity.Workflow.Name, identity.Workflow.Path, identity.Workflow.Event, identity.Workflow.RunID, identity.Workflow.RunAttempt,
		identity.Workflow.CheckSuiteID, identity.Workflow.JobID, identity.Workflow.CheckRunID, identity.Workflow.AppID,
		identity.SourceArtifact.ID, identity.SourceArtifact.Name, identity.BundleArtifact.ID, identity.BundleArtifact.Name,
	}
	return digest([]byte(strings.Join(parts, "\x00")))
}

func validPullRequestRevision(identity PullRequestRevisionIdentity) bool {
	lowerRef := strings.ToLower(identity.Ref)
	return validPullRequestRepository(identity.Repository) && validPositiveDecimal(identity.RepositoryID) &&
		identity.Ref != "" && strings.TrimSpace(identity.Ref) == identity.Ref && !strings.ContainsRune(identity.Ref, '\x00') &&
		!strings.HasPrefix(lowerRef, "refs/") && !strings.Contains(lowerRef, "refs/pull/") && !strings.Contains(lowerRef, "/merge") &&
		exactCommit.MatchString(identity.SHA)
}

func validPullRequestRepository(repository string) bool {
	if strings.TrimSpace(repository) != repository || strings.ContainsRune(repository, '\x00') || strings.Count(repository, "/") != 1 {
		return false
	}
	parts := strings.Split(repository, "/")
	return parts[0] != "" && parts[1] != ""
}

func validPullRequestFileIdentity(identity PullRequestFileIdentity) bool {
	return identity.Path != "" && strings.TrimSpace(identity.Path) == identity.Path && !strings.ContainsRune(identity.Path, '\x00') &&
		hexDigest.MatchString(identity.SHA256) && identity.Bytes > 0
}

func validPositiveDecimal(value string) bool {
	parsed, err := strconv.ParseInt(value, 10, 64)
	return err == nil && parsed > 0 && strconv.FormatInt(parsed, 10) == value
}

func validPullRequestRunURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" && len(value) <= 2048
}

func clonePullRequestCheckIdentity(identity PullRequestCheckReceiptIdentity) (PullRequestCheckReceiptIdentity, error) {
	data, err := json.Marshal(identity)
	if err != nil {
		return PullRequestCheckReceiptIdentity{}, fmt.Errorf("marshal PR check receipt identity: %w", err)
	}
	var cloned PullRequestCheckReceiptIdentity
	if err := json.Unmarshal(data, &cloned); err != nil {
		return PullRequestCheckReceiptIdentity{}, fmt.Errorf("clone PR check receipt identity: %w", err)
	}
	return cloned, nil
}
