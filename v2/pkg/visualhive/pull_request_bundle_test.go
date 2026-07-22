package visualhive

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

const (
	testPRBaseSHA             = "1111111111111111111111111111111111111111"
	testPRHeadSHA             = "2222222222222222222222222222222222222222"
	testPRRepository          = "owner/repo"
	testPRRepositoryID        = "123"
	testPRHeadRef             = "refs/heads/feature/visual-fix"
	testPRWorkflowName        = "Visual Hive PR"
	testPRWorkflowPath        = ".github/workflows/visual-hive-pr.yml"
	testPRWorkflowRunID       = "42"
	testPRWorkflowRunAttempt  = "2"
	testPRSourceArtifactID    = "99"
	testPRSourceArtifactName  = "visual-hive-pr-source"
	testPRBundleArtifactName  = "visual-hive-pr-bundle-v3"
	testPRContractID          = "visual.contract"
	testPRTargetID            = "web"
	testPRChangedFile         = "src/app.tsx"
	testPRGeneratedSpecPath   = ".visual-hive/generated/visual-hive.generated.spec.ts"
	testPRGeneratedConfigPath = ".visual-hive/generated/visual-hive.generated.config.cjs"
	testPRBaselinePath        = ".visual-hive/proof/pr/baselines/visual.contract__home__desktop.png"
	testPRBaselineSourcePath  = "visual.contract__home__desktop.png"
)

func TestValidateBundleStillRejectsPullRequestWorkflowEvents(t *testing.T) {
	for _, event := range []string{"pull_request", "pull_request_target"} {
		t.Run(event, func(t *testing.T) {
			manifestPath, _ := writeTestPullRequestManifest(t)
			manifest := readManifest(t, manifestPath)
			manifest.Source.Event = event
			resignTestManifest(t, manifestPath, manifest)

			_, err := ValidateBundle(manifestPath, ValidationOptions{
				Now:                       testPRNow(),
				MaxACMM:                   3,
				VerifiedProvenance:        true,
				ExpectedProducerGitCommit: testV3ProducerCommit,
			})
			if err == nil || !strings.Contains(err.Error(), "non-PR workflow") {
				t.Fatalf("default validator accepted %s evidence: %v", event, err)
			}
		})
	}
}

func TestParsePullRequestBundleAcceptsOnlyExactNonAuthoritativeHeadEvidence(t *testing.T) {
	manifestPath, _ := writeTestPullRequestManifest(t)
	bundle, err := ParsePullRequestBundle(manifestPath, testPullRequestParseOptions())
	if err != nil {
		t.Fatalf("exact PR bundle was rejected: %v", err)
	}
	if bundle.provenanceVerified || bundle.Validation.Trusted || bundle.Validation.Authoritative {
		t.Fatalf("PR bundle acquired authority: %+v", bundle.Validation)
	}
	if bundle.Manifest.Source.Event != "pull_request" || bundle.Manifest.Source.Trusted || bundle.Manifest.Scan.Scope != "changed-files" || bundle.Manifest.Scan.AuthoritativeForResolution {
		t.Fatalf("accepted PR bundle lost its constrained provenance: %+v", bundle.Manifest)
	}
}

func TestParsePullRequestBundleRejectsWrongOrAuthoritativeEvidence(t *testing.T) {
	otherCommit := strings.Repeat("3", 40)
	tests := []struct {
		name    string
		mutate  func(*Manifest, *PullRequestParseOptions)
		wantErr string
	}{
		{name: "pull request target", mutate: func(m *Manifest, _ *PullRequestParseOptions) { m.Source.Event = "pull_request_target" }, wantErr: "untrusted pull_request"},
		{name: "merge ref", mutate: func(m *Manifest, _ *PullRequestParseOptions) { m.Source.Ref = "refs/pull/17/merge" }, wantErr: "non-merge head"},
		{name: "wrong repository", mutate: func(_ *Manifest, o *PullRequestParseOptions) { o.ExpectedRepository = "other/repo" }, wantErr: "repository"},
		{name: "wrong head", mutate: func(_ *Manifest, o *PullRequestParseOptions) { o.ExpectedHeadSHA = otherCommit }, wantErr: "exact independently verified head"},
		{name: "wrong head ref", mutate: func(_ *Manifest, o *PullRequestParseOptions) { o.ExpectedHeadRef = "refs/heads/other" }, wantErr: "exact independently verified head"},
		{name: "wrong workflow", mutate: func(_ *Manifest, o *PullRequestParseOptions) { o.ExpectedWorkflowName = "Other Workflow" }, wantErr: "head, workflow, or source artifact"},
		{name: "wrong run", mutate: func(_ *Manifest, o *PullRequestParseOptions) { o.ExpectedWorkflowRunID = "43" }, wantErr: "workflow run"},
		{name: "wrong attempt", mutate: func(_ *Manifest, o *PullRequestParseOptions) { o.ExpectedWorkflowRunAttempt = "3" }, wantErr: "run attempt"},
		{name: "wrong source artifact", mutate: func(_ *Manifest, o *PullRequestParseOptions) { o.ExpectedSourceArtifactID = "100" }, wantErr: "source artifact"},
		{name: "wrong producer", mutate: func(_ *Manifest, o *PullRequestParseOptions) { o.ExpectedProducerGitCommit = otherCommit }, wantErr: "producer commit"},
		{name: "trusted source claim", mutate: func(m *Manifest, _ *PullRequestParseOptions) { m.Source.Trusted = true }, wantErr: "untrusted pull_request"},
		{name: "failed conclusion", mutate: func(m *Manifest, _ *PullRequestParseOptions) { m.Source.Conclusion = "failure" }, wantErr: "successful attested"},
		{name: "no attestation", mutate: func(m *Manifest, _ *PullRequestParseOptions) { m.Provenance.AttestationRequired = false }, wantErr: "successful attested"},
		{name: "authoritative", mutate: func(m *Manifest, _ *PullRequestParseOptions) {
			m.Scan.Scope, m.Scan.AuthoritativeForResolution = "full", true
		}, wantErr: "non-authoritative changed-files"},
		{name: "full scan", mutate: func(m *Manifest, _ *PullRequestParseOptions) { m.Scan.Scope = "full" }, wantErr: "non-authoritative changed-files"},
		{name: "empty evaluated contracts", mutate: func(m *Manifest, _ *PullRequestParseOptions) { m.Scan.EvaluatedContracts = []string{} }, wantErr: "actual evaluated contract"},
		{name: "empty evaluated files", mutate: func(m *Manifest, _ *PullRequestParseOptions) { m.Scan.EvaluatedFiles = []string{} }, wantErr: "actual evaluated contract and file"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifestPath, _ := writeTestPullRequestManifest(t)
			manifest := readManifest(t, manifestPath)
			options := testPullRequestParseOptions()
			test.mutate(&manifest, &options)
			resignTestManifest(t, manifestPath, manifest)

			_, err := ParsePullRequestBundle(manifestPath, options)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected %q rejection, got %v", test.wantErr, err)
			}
		})
	}
}

func TestParsePullRequestSourceBindingStrictRoundTrip(t *testing.T) {
	binding := testPullRequestSourceBinding()
	data, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParsePullRequestSourceBinding(data)
	if err != nil {
		t.Fatalf("strict source binding round trip failed: %v", err)
	}
	if !reflect.DeepEqual(parsed, binding) {
		t.Fatalf("source binding changed on round trip:\nwant: %#v\n got: %#v", binding, parsed)
	}

	var document map[string]interface{}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["unexpectedAuthority"] = true
	data, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePullRequestSourceBinding(data); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown source binding field was accepted: %v", err)
	}
}

func TestVerifyPullRequestSourceBindingCompleteFixture(t *testing.T) {
	fixture := writeCompleteTestPullRequestBundle(t, nil)
	bundle := validateAndVerifyTestPullRequestSource(t, fixture)
	binding, err := bundle.VerifyPullRequestSourceBinding(fixture.Expectations)
	if err != nil {
		t.Fatalf("complete exact PR source binding was rejected: %v", err)
	}
	if !reflect.DeepEqual(binding, fixture.Binding) {
		t.Fatalf("verified source binding differs from the sealed receipt")
	}
}

func TestVerifyPullRequestSourceBindingRejectsAbsentRuntimeAndSourceDrift(t *testing.T) {
	t.Run("absent runtime", func(t *testing.T) {
		fixture := writeCompleteTestPullRequestBundle(t, nil)
		bundle, err := ParsePullRequestBundle(fixture.ManifestPath, testPullRequestParseOptions())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(fixture.SourceRoot, filepath.FromSlash(PullRequestRuntimePath))); err != nil {
			t.Fatal(err)
		}
		if err := bundle.VerifySourceArtifact(fixture.SourceRoot); err == nil || !strings.Contains(err.Error(), "source artifact entry") {
			t.Fatalf("source artifact without runtime receipt was accepted: %v", err)
		}
	})

	for _, path := range []string{
		PullRequestConfigPath, PullRequestPlanPath, PullRequestReportPath, PullRequestChangedFilesPath,
		PullRequestPlanContractsPath, PullRequestEvaluationScopesPath,
		PullRequestRuntimePath, PullRequestWorkflowPath, PullRequestRunnerAttestationPath, PullRequestPipelineExitPath, PullRequestRunnerOutcomePath,
		testPRGeneratedSpecPath, testPRGeneratedConfigPath,
		testPRBaselinePath, PullRequestSourceBindingPath,
	} {
		t.Run("drift "+filepath.Base(path), func(t *testing.T) {
			fixture := writeCompleteTestPullRequestBundle(t, nil)
			bundle := validateAndVerifyTestPullRequestSource(t, fixture)
			target := filepath.Join(fixture.SourceRoot, filepath.FromSlash(path))
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := bundle.VerifyPullRequestSourceBinding(fixture.Expectations); err == nil || !strings.Contains(err.Error(), "changed after verification") {
				t.Fatalf("post-verification source drift for %s was accepted: %v", path, err)
			}
		})
	}
}

func TestVerifyPullRequestSourceBindingRejectsMixedExecutionAndInventories(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testPRSourceDocuments)
		wantErr string
	}{
		{
			name: "no-op plan",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Plan.Items = []testPRPlanItem{}
			},
			wantErr: "no-contract PR plan",
		},
		{
			name: "plan schema drift",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Plan.SchemaVersion = 2
			},
			wantErr: "plan schema",
		},
		{
			name: "plan changed-file drift",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Plan.EffectiveChangedFiles = []string{"other.ts"}
			},
			wantErr: "partition",
		},
		{
			name: "omitted ignored changed file",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Changed = []byte("docs/guide.md\n" + testPRChangedFile + "\n")
				documents.Plan.ChangedFiles = []string{"docs/guide.md", testPRChangedFile}
				documents.Plan.EffectiveChangedFiles = []string{testPRChangedFile}
				documents.Report.ChangedFiles = []string{"docs/guide.md", testPRChangedFile}
			},
			wantErr: "partition",
		},
		{
			name: "extra ignored changed file",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Plan.IgnoredChangedFiles = []interface{}{map[string]interface{}{
					"file": "docs/guide.md", "pattern": "docs/**", "reason": "documentation-only change",
				}}
			},
			wantErr: "outside the exact runner-owned diff",
		},
		{
			name: "ignored changed file lacks producer rule",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Plan.IgnoredChangedFiles = []interface{}{map[string]interface{}{
					"file": testPRChangedFile, "reason": "ignored",
				}}
				documents.Plan.EffectiveChangedFiles = []string{}
			},
			wantErr: "ignored changed-file identity",
		},
		{
			name: "noncanonical changed-file receipt",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Changed = []byte(" " + testPRChangedFile + "\n")
			},
			wantErr: "plan/report changed-file",
		},
		{
			name: "missing evaluated plan contract",
			mutate: func(documents *testPRSourceDocuments) {
				documents.PlanContracts = []byte("other.contract\n")
			},
			wantErr: "evaluated plan-contract receipt",
		},
		{
			name: "duplicated evaluated plan contract",
			mutate: func(documents *testPRSourceDocuments) {
				documents.PlanContracts = []byte(testPRContractID + "\n" + testPRContractID + "\n")
			},
			wantErr: "evaluated plan-contract receipt",
		},
		{
			name: "full scopes omit plan contract",
			mutate: func(documents *testPRSourceDocuments) {
				documents.EvaluationScopes = []byte("provider-governance\nworkflow-safety\n")
			},
			wantErr: "omits an executed plan contract",
		},
		{
			name: "full scopes contain arbitrary extra",
			mutate: func(documents *testPRSourceDocuments) {
				documents.EvaluationScopes = []byte(testPRContractID + "\nattacker-scope\nprovider-governance\n")
			},
			wantErr: "non-deterministic lifecycle scope",
		},
		{
			name: "runtime report execution mismatch",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Report.ExecutionBinding["payloadSha256"] = strings.Repeat("e", 64)
			},
			wantErr: "authenticated runner execution binding",
		},
		{
			name: "mixed contract inventory",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Report.SelectedContracts = []string{"other.contract"}
				documents.Report.Results[0].ContractID = "other.contract"
			},
			wantErr: "selected-contract inventory",
		},
		{
			name: "mixed target inventory",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Report.SelectedTargets[0].ID = "other-target"
				documents.Report.Results[0].TargetID = "other-target"
			},
			wantErr: "contract/target",
		},
		{
			name: "duplicated contract target pair",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Plan.Items = append(documents.Plan.Items, documents.Plan.Items[0])
				documents.Report.Results = append(documents.Report.Results, documents.Report.Results[0])
			},
			wantErr: "duplicated",
		},
		{
			name: "report provider drift",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Report.Repository.Provider = "attacker"
			},
			wantErr: "report repository",
		},
		{
			name: "report branch drift",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Report.Repository.Branch = "other"
			},
			wantErr: "report repository",
		},
		{
			name: "present report PR number drift",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Report.Repository.PullRequestNumber = 18
			},
			wantErr: "report repository",
		},
		{
			name: "report baseline inventory drift",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Report.Results[0].ScreenshotAssertions[0].BaselinePath = "other.png"
			},
			wantErr: "screenshot baselines",
		},
		{
			name: "non-passing baseline assertion",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Report.Results[0].ScreenshotAssertions[0].Status = "failed"
			},
			wantErr: "non-passing",
		},
		{
			name: "unrecognized execution binding field",
			mutate: func(documents *testPRSourceDocuments) {
				documents.Runtime.ExecutionBinding["runtimeStableSha256"] = strings.Repeat("f", 64)
				documents.Report.ExecutionBinding["runtimeStableSha256"] = strings.Repeat("f", 64)
			},
			wantErr: "authenticated runner execution payload",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := writeCompleteTestPullRequestBundle(t, test.mutate)
			bundle := validateAndVerifyTestPullRequestSource(t, fixture)
			_, err := bundle.VerifyPullRequestSourceBinding(fixture.Expectations)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected %q rejection, got %v", test.wantErr, err)
			}
		})
	}
}

func TestVerifyPullRequestSourceBindingAcceptsExactIgnoredFilePartition(t *testing.T) {
	fixture := writeCompleteTestPullRequestBundle(t, func(documents *testPRSourceDocuments) {
		documents.Changed = []byte("docs/guide.md\n" + testPRChangedFile + "\n")
		documents.Plan.ChangedFiles = []string{"docs/guide.md", testPRChangedFile}
		documents.Plan.EffectiveChangedFiles = []string{testPRChangedFile}
		documents.Plan.IgnoredChangedFiles = []interface{}{map[string]interface{}{
			"file": "docs/guide.md", "pattern": "docs/**", "reason": "documentation-only change",
		}}
		documents.Report.ChangedFiles = []string{"docs/guide.md", testPRChangedFile}
	})
	bundle := validateAndVerifyTestPullRequestSource(t, fixture)
	if _, err := bundle.VerifyPullRequestSourceBinding(fixture.Expectations); err != nil {
		t.Fatalf("exact full/effective/ignored producer partition was rejected: %v", err)
	}
}

func TestVerifyPullRequestSourceBindingRejectsForgedRunnerAttestationMAC(t *testing.T) {
	fixture := writeCompleteTestPullRequestBundle(t, nil)
	attestationPath := filepath.Join(fixture.SourceRoot, filepath.FromSlash(PullRequestRunnerAttestationPath))
	attestationData, err := os.ReadFile(attestationPath)
	if err != nil {
		t.Fatal(err)
	}
	var attestation pullRequestRunnerAttestation
	if err := json.Unmarshal(attestationData, &attestation); err != nil {
		t.Fatal(err)
	}
	attestation.MACSHA256 = strings.Repeat("f", 64)
	attestationData = marshalTestJSON(t, attestation)
	writeTestData(t, attestationPath, attestationData)
	binding := fixture.Binding
	binding.RunnerAttestation = testPRFileIdentity(PullRequestRunnerAttestationPath, attestationData)
	writeTestData(t, filepath.Join(fixture.SourceRoot, filepath.FromSlash(PullRequestSourceBindingPath)), marshalTestJSON(t, binding))
	rebuildTestPRSourceIndex(t, fixture.ManifestPath, fixture.SourceRoot)
	bundle := validateAndVerifyTestPullRequestSource(t, fixture)
	if _, err := bundle.VerifyPullRequestSourceBinding(fixture.Expectations); err == nil || !strings.Contains(err.Error(), "HMAC verification failed") {
		t.Fatalf("forged runner execution attestation was accepted: %v", err)
	}
}

func TestVerifyPullRequestSourceBindingRejectsClaimedStableRuntimeDigest(t *testing.T) {
	fixture := writeCompleteTestPullRequestBundle(t, nil)
	claimed := strings.Repeat("f", 64)
	binding := fixture.Binding
	binding.Runtime.StableSHA256 = claimed
	writeTestData(t, filepath.Join(fixture.SourceRoot, filepath.FromSlash(PullRequestSourceBindingPath)), marshalTestJSON(t, binding))
	rebuildTestPRSourceIndex(t, fixture.ManifestPath, fixture.SourceRoot)
	bundle := validateAndVerifyTestPullRequestSource(t, fixture)
	if _, err := bundle.VerifyPullRequestSourceBinding(fixture.Expectations); err == nil || !strings.Contains(err.Error(), "runtime stable identity") {
		t.Fatalf("caller-claimed stable runtime digest was accepted: %v", err)
	}
}

func TestVerifyPullRequestSourceBindingRejectsNoncanonicalGeneratedConfigPath(t *testing.T) {
	fixture := writeCompleteTestPullRequestBundle(t, nil)
	alternatePath := ".visual-hive/generated/attacker.config.cjs"
	data, err := os.ReadFile(filepath.Join(fixture.SourceRoot, filepath.FromSlash(testPRGeneratedConfigPath)))
	if err != nil {
		t.Fatal(err)
	}
	writeTestData(t, filepath.Join(fixture.SourceRoot, filepath.FromSlash(alternatePath)), data)
	binding := fixture.Binding
	binding.Runtime.GeneratedConfig = testPRFileIdentity(alternatePath, data)
	writeTestData(t, filepath.Join(fixture.SourceRoot, filepath.FromSlash(PullRequestSourceBindingPath)), marshalTestJSON(t, binding))
	rebuildTestPRSourceIndex(t, fixture.ManifestPath, fixture.SourceRoot)
	bundle := validateAndVerifyTestPullRequestSource(t, fixture)
	if _, err := bundle.VerifyPullRequestSourceBinding(fixture.Expectations); err == nil || !strings.Contains(err.Error(), "unexpected fixed evidence path") {
		t.Fatalf("noncanonical generated config path was accepted: %v", err)
	}
}

func TestVerifyPullRequestSourceBindingRejectsNoncanonicalBaselinePath(t *testing.T) {
	fixture := writeCompleteTestPullRequestBundle(t, nil)
	binding := fixture.Binding
	binding.Baselines.Files[0].Path = pullRequestBaselinePrefix + " baseline.png"
	binding.Baselines.SHA256 = testPRBaselineDigest(binding.Baselines.Files)
	writeTestData(t, filepath.Join(fixture.SourceRoot, filepath.FromSlash(PullRequestSourceBindingPath)), marshalTestJSON(t, binding))
	rebuildTestPRSourceIndex(t, fixture.ManifestPath, fixture.SourceRoot)
	bundle := validateAndVerifyTestPullRequestSource(t, fixture)
	if _, err := bundle.VerifyPullRequestSourceBinding(fixture.Expectations); err == nil || !strings.Contains(err.Error(), "canonical sealed PNG") {
		t.Fatalf("noncanonical baseline path was accepted: %v", err)
	}
}

type testPRSourceFixture struct {
	ManifestPath string
	SourceRoot   string
	Binding      PullRequestSourceBinding
	Expectations PullRequestSourceExpectations
}

type testPRSourceDocuments struct {
	Plan             testPRPlan
	Report           testPRReport
	Runtime          playwrightRuntimeReceipt
	Config           []byte
	Changed          []byte
	PlanContracts    []byte
	EvaluationScopes []byte
	Workflow         []byte
	Spec             []byte
	PWConfig         []byte
	Baseline         []byte
	PipelineExit     []byte
	RunnerOutcome    []byte
}

type testPRPlan struct {
	SchemaVersion         int                `json:"schemaVersion"`
	Project               string             `json:"project"`
	Mode                  string             `json:"mode"`
	GeneratedAt           string             `json:"generatedAt"`
	ChangedFiles          []string           `json:"changedFiles"`
	EffectiveChangedFiles []string           `json:"effectiveChangedFiles"`
	IgnoredChangedFiles   []interface{}      `json:"ignoredChangedFiles"`
	Targets               []testPRPlanTarget `json:"targets"`
	Items                 []testPRPlanItem   `json:"items"`
	Excluded              []interface{}      `json:"excluded"`
	Mutation              testPRMutationPlan `json:"mutation"`
	ProviderPolicy        []interface{}      `json:"providerPolicy"`
}

type testPRPlanTarget struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	PRSafe bool   `json:"prSafe"`
	Cost   string `json:"cost"`
}

type testPRPlanItem struct {
	ContractID  string   `json:"contractId"`
	TargetID    string   `json:"targetId"`
	TargetURL   string   `json:"targetUrl"`
	Severity    string   `json:"severity"`
	Cost        string   `json:"cost"`
	Reasons     []string `json:"reasons"`
	Screenshots []string `json:"screenshots"`
}

type testPRMutationPlan struct {
	Enabled   bool          `json:"enabled"`
	Operators []interface{} `json:"operators"`
	MinScore  float64       `json:"minScore"`
	Reasons   []string      `json:"reasons"`
}

type testPRReport struct {
	SchemaVersion     int                    `json:"schemaVersion"`
	Project           string                 `json:"project"`
	Repository        testPRReportRepository `json:"repository"`
	Mode              string                 `json:"mode"`
	GeneratedAt       string                 `json:"generatedAt"`
	Status            string                 `json:"status"`
	ChangedFiles      []string               `json:"changedFiles"`
	SelectedTargets   []testPRReportTarget   `json:"selectedTargets"`
	SelectedContracts []string               `json:"selectedContracts"`
	ExcludedContracts []interface{}          `json:"excludedContracts"`
	TargetLifecycle   []interface{}          `json:"targetLifecycle"`
	GeneratedSpecPath string                 `json:"generatedSpecPath"`
	ExecutionBinding  map[string]string      `json:"executionBinding"`
	Results           []testPRReportResult   `json:"results"`
	Summary           testPRReportSummary    `json:"summary"`
	ConsoleErrors     []string               `json:"consoleErrors"`
	PageErrors        []interface{}          `json:"pageErrors"`
	Artifacts         []string               `json:"artifacts"`
	Reproduction      []string               `json:"reproductionCommands"`
}

type testPRReportRepository struct {
	Provider          string `json:"provider"`
	Repository        string `json:"repository"`
	Branch            string `json:"branch"`
	BaseBranch        string `json:"baseBranch"`
	CommitSHA         string `json:"commitSha"`
	PullRequestNumber int    `json:"pullRequestNumber"`
	RunID             string `json:"runId"`
	RunAttempt        string `json:"runAttempt"`
	Workflow          string `json:"workflow"`
}

type testPRReportTarget struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	PRSafe bool   `json:"prSafe"`
	Cost   string `json:"cost"`
}

type testPRReportResult struct {
	ContractID           string                      `json:"contractId"`
	TargetID             string                      `json:"targetId"`
	Status               string                      `json:"status"`
	DurationMS           int64                       `json:"durationMs"`
	Errors               []string                    `json:"errors"`
	Artifacts            []string                    `json:"artifacts"`
	ScreenshotAssertions []testPRScreenshotAssertion `json:"screenshotAssertions"`
}

type testPRScreenshotAssertion struct {
	BaselinePath string `json:"baselinePath"`
	Status       string `json:"status"`
}

type testPRReportSummary struct {
	Passed            int `json:"passed"`
	Failed            int `json:"failed"`
	ScreenshotsPassed int `json:"screenshotsPassed"`
	ScreenshotsFailed int `json:"screenshotsFailed"`
	BaselinesCreated  int `json:"baselinesCreated"`
	CreatedBaselines  int `json:"createdBaselines"`
	MissingBaselines  int `json:"missingBaselines"`
	VisualDiffs       int `json:"visualDiffs"`
	ConsoleErrors     int `json:"consoleErrors"`
	PageErrors        int `json:"pageErrors"`
}

func writeTestPullRequestManifest(t *testing.T) (string, string) {
	t.Helper()
	manifestPath, sourceRoot := writeTestV3Bundle(t, nil)
	manifest := readManifest(t, manifestPath)
	configureTestPullRequestManifest(&manifest)
	resignTestManifest(t, manifestPath, manifest)
	return manifestPath, sourceRoot
}

func configureTestPullRequestManifest(manifest *Manifest) {
	manifest.BundleID = "test-pr-v3"
	manifest.Source = Source{
		Repository: testPRRepository, RepositoryID: testPRRepositoryID, Ref: testPRHeadRef, CommitSHA: testPRHeadSHA,
		Event: "pull_request", WorkflowName: testPRWorkflowName, WorkflowRunID: testPRWorkflowRunID,
		WorkflowRunAttempt: testPRWorkflowRunAttempt, WorkflowArtifactID: testPRSourceArtifactID, Conclusion: "success", Trusted: false,
	}
	manifest.Mode = "pr"
	manifest.Verdict = "passed"
	manifest.Scan = Scan{
		Scope: "changed-files", AuthoritativeForResolution: false,
		EvaluatedContracts: []string{testPRContractID}, EvaluatedFiles: []string{testPRChangedFile},
		TestPlanVersion: "visual-hive.pr-plan.v1", ToolRegistryVersion: "visual-hive.tool-registry.v1",
	}
	manifest.ReplayProtection.Nonce = "test-pr-v3"
}

func testPullRequestParseOptions() PullRequestParseOptions {
	return PullRequestParseOptions{
		Now: testPRNow(), MaxACMM: 3, ExpectedProducerGitCommit: testV3ProducerCommit,
		ExpectedRepository: testPRRepository, ExpectedRepositoryID: testPRRepositoryID,
		ExpectedHeadSHA: testPRHeadSHA, ExpectedHeadRef: strings.TrimPrefix(testPRHeadRef, "refs/heads/"),
		ExpectedWorkflowName: testPRWorkflowName, ExpectedWorkflowRunID: testPRWorkflowRunID,
		ExpectedWorkflowRunAttempt: testPRWorkflowRunAttempt, ExpectedSourceArtifactID: testPRSourceArtifactID,
	}
}

func testPRNow() time.Time {
	return time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)
}

func resignTestManifest(t *testing.T, manifestPath string, manifest Manifest) {
	t.Helper()
	manifest.ReplayProtection.Key = replayKeyForManifest(manifest)
	manifest.OverallDigest = digestV3BundleContent(manifest)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, manifestPath, manifest)
}

func writeCompleteTestPullRequestBundle(t *testing.T, mutate func(*testPRSourceDocuments)) testPRSourceFixture {
	t.Helper()
	manifestPath, sourceRoot := writeTestPullRequestManifest(t)
	documents := newTestPRSourceDocuments()
	if mutate != nil {
		mutate(&documents)
	}
	manifest := readManifest(t, manifestPath)
	manifest.Scan.EvaluatedContracts = testPRReceiptLines(documents.EvaluationScopes)
	manifest.Scan.EvaluatedFiles = testPRReceiptLines(documents.Changed)
	resignTestManifest(t, manifestPath, manifest)

	planData := marshalTestJSON(t, documents.Plan)
	reportData := marshalTestJSON(t, documents.Report)
	runtimeData := marshalTestJSON(t, documents.Runtime)
	files := map[string][]byte{
		PullRequestPlanPath:             planData,
		PullRequestReportPath:           reportData,
		PullRequestConfigPath:           documents.Config,
		PullRequestChangedFilesPath:     documents.Changed,
		PullRequestPlanContractsPath:    documents.PlanContracts,
		PullRequestEvaluationScopesPath: documents.EvaluationScopes,
		PullRequestRuntimePath:          runtimeData,
		PullRequestPipelineExitPath:     documents.PipelineExit,
		PullRequestRunnerOutcomePath:    documents.RunnerOutcome,
		PullRequestWorkflowPath:         documents.Workflow,
		testPRGeneratedSpecPath:         documents.Spec,
		testPRGeneratedConfigPath:       documents.PWConfig,
		testPRBaselinePath:              documents.Baseline,
	}
	for path, data := range files {
		writeTestData(t, filepath.Join(sourceRoot, filepath.FromSlash(path)), data)
	}

	binding := testPullRequestSourceBinding()
	binding.Plan = testPRFileIdentity(PullRequestPlanPath, planData)
	binding.Report = testPRFileIdentity(PullRequestReportPath, reportData)
	binding.Config = testPRFileIdentity(PullRequestConfigPath, documents.Config)
	binding.ChangedFiles = testPRFileIdentity(PullRequestChangedFilesPath, documents.Changed)
	binding.PlanContracts = testPRFileIdentity(PullRequestPlanContractsPath, documents.PlanContracts)
	binding.EvaluationScopes = testPRFileIdentity(PullRequestEvaluationScopesPath, documents.EvaluationScopes)
	binding.Workflow.Definition = testPRFileIdentity(PullRequestWorkflowPath, documents.Workflow)
	binding.Runtime.Receipt = testPRFileIdentity(PullRequestRuntimePath, runtimeData)
	binding.Runtime.GeneratedSpec = testPRFileIdentity(testPRGeneratedSpecPath, documents.Spec)
	binding.Runtime.GeneratedConfig = testPRFileIdentity(testPRGeneratedConfigPath, documents.PWConfig)
	binding.Runtime.StableSHA256 = testPRRuntimeStableDigest(t, documents.Runtime)
	binding.Runtime.ExecutionBindingSHA256 = digest(marshalTestJSON(t, documents.Runtime.ExecutionBinding))
	binding.Baselines.Files = []PullRequestFileIdentity{testPRFileIdentity(testPRBaselinePath, documents.Baseline)}
	binding.Baselines.SHA256 = testPRBaselineDigest(binding.Baselines.Files)
	attestationData := testPRRunnerAttestation(t, binding, documents.Runtime.ExecutionBinding,
		testPRFileIdentity(PullRequestPipelineExitPath, documents.PipelineExit), testPRFileIdentity(PullRequestRunnerOutcomePath, documents.RunnerOutcome))
	binding.RunnerAttestation = testPRFileIdentity(PullRequestRunnerAttestationPath, attestationData)
	writeTestData(t, filepath.Join(sourceRoot, filepath.FromSlash(PullRequestRunnerAttestationPath)), attestationData)
	bindingData := marshalTestJSON(t, binding)
	writeTestData(t, filepath.Join(sourceRoot, filepath.FromSlash(PullRequestSourceBindingPath)), bindingData)

	rebuildTestPRSourceIndex(t, manifestPath, sourceRoot)
	expectations := PullRequestSourceExpectations{
		Repository: testPRRepository, RepositoryID: testPRRepositoryID, PullRequest: 17,
		Base: binding.Base, Head: binding.Head, WorkflowName: testPRWorkflowName, WorkflowPath: testPRWorkflowPath,
		WorkflowRunID: testPRWorkflowRunID, WorkflowRunAttempt: testPRWorkflowRunAttempt,
		WorkflowDefinitionSHA256: binding.Workflow.Definition.SHA256, ProducerCommit: testV3ProducerCommit,
		SourceArtifactName: testPRSourceArtifactName, BundleArtifactName: testPRBundleArtifactName,
		ConfigSHA256: binding.Config.SHA256,
	}
	return testPRSourceFixture{ManifestPath: manifestPath, SourceRoot: sourceRoot, Binding: binding, Expectations: expectations}
}

func newTestPRSourceDocuments() testPRSourceDocuments {
	spec := []byte("// generated exact Playwright spec\n")
	pwConfig := []byte("module.exports = { reporter: 'json' };\n")
	runtime := playwrightRuntimeReceipt{
		SchemaVersion: PullRequestRuntimeSchema, CapturedAt: "2026-07-09T12:04:00.000Z",
		Browser:     playwrightRuntimeBrowser{Name: "chromium", Version: "126.0.0"},
		Environment: playwrightEnvironment{OS: "linux 6.8", Architecture: "x64", NodeVersion: "v22.14.0", PlaywrightVersion: "1.52.0", Locale: "en-US", Timezone: "UTC", UserAgent: "test-agent", DeviceScaleFactor: json.Number("1"), Fonts: []playwrightRuntimeFont{{Name: "Arial", Available: true}}},
	}
	executionBinding := map[string]string{
		"nonceSha256":           strings.Repeat("a", 64),
		"generatedSpecSha256":   digest(spec),
		"generatedConfigSha256": digest(pwConfig),
		"payloadSha256":         strings.Repeat("b", 64),
		"bindingMacSha256":      strings.Repeat("c", 64),
	}
	runtime.ExecutionBinding = cloneStringMap(executionBinding)
	return testPRSourceDocuments{
		Plan: testPRPlan{
			SchemaVersion: 1, Project: "demo", Mode: "pr", GeneratedAt: "2026-07-09T12:00:00.000Z",
			ChangedFiles: []string{testPRChangedFile}, EffectiveChangedFiles: []string{testPRChangedFile}, IgnoredChangedFiles: []interface{}{},
			Targets:  []testPRPlanTarget{{ID: testPRTargetID, Kind: "url", URL: "https://example.test", PRSafe: true, Cost: "cheap"}},
			Items:    []testPRPlanItem{{ContractID: testPRContractID, TargetID: testPRTargetID, TargetURL: "https://example.test", Severity: "high", Cost: "cheap", Reasons: []string{"runOn.pullRequest=true"}, Screenshots: []string{"home:/:desktop"}}},
			Excluded: []interface{}{}, Mutation: testPRMutationPlan{Enabled: false, Operators: []interface{}{}, MinScore: 0.8, Reasons: []string{"mutation.enabled=false"}}, ProviderPolicy: []interface{}{},
		},
		Report: testPRReport{
			SchemaVersion: 2, Project: "demo",
			Repository: testPRReportRepository{Provider: "github-actions", Repository: testPRRepository, Branch: "feature/visual-fix", BaseBranch: "main", CommitSHA: testPRHeadSHA, PullRequestNumber: 17, RunID: testPRWorkflowRunID, RunAttempt: testPRWorkflowRunAttempt, Workflow: testPRWorkflowName},
			Mode:       "pr", GeneratedAt: "2026-07-09T12:05:00.000Z", Status: "passed", ChangedFiles: []string{testPRChangedFile},
			SelectedTargets:   []testPRReportTarget{{ID: testPRTargetID, Kind: "url", URL: "https://example.test", PRSafe: true, Cost: "cheap"}},
			SelectedContracts: []string{testPRContractID}, ExcludedContracts: []interface{}{}, TargetLifecycle: []interface{}{}, GeneratedSpecPath: testPRGeneratedSpecPath,
			ExecutionBinding: cloneStringMap(executionBinding),
			Results: []testPRReportResult{{
				ContractID: testPRContractID, TargetID: testPRTargetID, Status: "passed", DurationMS: 25, Errors: []string{}, Artifacts: []string{testPRBaselinePath},
				ScreenshotAssertions: []testPRScreenshotAssertion{{BaselinePath: testPRBaselineSourcePath, Status: "passed"}},
			}},
			Summary: testPRReportSummary{Passed: 1, ScreenshotsPassed: 1}, ConsoleErrors: []string{}, PageErrors: []interface{}{}, Artifacts: []string{testPRBaselinePath}, Reproduction: []string{"visual-hive run --ci"},
		},
		Runtime:          runtime,
		Config:           []byte("project:\n  name: demo\nvisual:\n  updateSnapshots: false\n"),
		Changed:          []byte(testPRChangedFile + "\n"),
		PlanContracts:    []byte(testPRContractID + "\n"),
		EvaluationScopes: []byte(testPRContractID + "\nprovider-governance\nworkflow-safety\n"),
		Workflow:         []byte("name: Visual Hive PR\non:\n  pull_request:\n"),
		Spec:             spec,
		PWConfig:         pwConfig,
		Baseline:         []byte("\x89PNG\r\n\x1a\nsealed baseline evidence bytes"),
		PipelineExit:     []byte("0\n"),
		RunnerOutcome:    []byte("{\"schemaVersion\":\"hive.visual-runner-outcome.v1\",\"outcome\":\"success\",\"conclusion\":\"success\"}\n"),
	}
}

func testPullRequestSourceBinding() PullRequestSourceBinding {
	placeholder := PullRequestFileIdentity{Path: ".visual-hive/placeholder", SHA256: strings.Repeat("d", 64), Bytes: 1}
	return PullRequestSourceBinding{
		SchemaVersion: PullRequestSourceBindingSchema, Repository: testPRRepository, RepositoryID: testPRRepositoryID, PullRequest: 17,
		Base:     PullRequestRevisionIdentity{Repository: testPRRepository, RepositoryID: testPRRepositoryID, Ref: "main", SHA: testPRBaseSHA},
		Head:     PullRequestRevisionIdentity{Repository: testPRRepository, RepositoryID: testPRRepositoryID, Ref: strings.TrimPrefix(testPRHeadRef, "refs/heads/"), SHA: testPRHeadSHA},
		Workflow: PullRequestWorkflowIdentity{Name: testPRWorkflowName, Path: testPRWorkflowPath, Event: "pull_request", RunID: testPRWorkflowRunID, RunAttempt: testPRWorkflowRunAttempt, Definition: placeholder, ProducerCommit: testV3ProducerCommit, SourceName: testPRSourceArtifactName, BundleName: testPRBundleArtifactName},
		Plan:     placeholder, Report: placeholder, Config: placeholder, ChangedFiles: placeholder, PlanContracts: placeholder, EvaluationScopes: placeholder, RunnerAttestation: placeholder,
		Runtime:   PullRequestRuntimeIdentity{Receipt: placeholder, GeneratedSpec: placeholder, GeneratedConfig: placeholder, StableSHA256: strings.Repeat("e", 64), ExecutionBindingSHA256: strings.Repeat("f", 64)},
		Baselines: PullRequestBaselineIdentity{SHA256: strings.Repeat("a", 64), Files: []PullRequestFileIdentity{placeholder}},
	}
}

func testPRFileIdentity(path string, data []byte) PullRequestFileIdentity {
	return PullRequestFileIdentity{Path: path, SHA256: digest(data), Bytes: int64(len(data))}
}

func testPRRuntimeStableDigest(t *testing.T, runtime playwrightRuntimeReceipt) string {
	t.Helper()
	stable := struct {
		Browser     playwrightRuntimeBrowser `json:"browser"`
		Environment playwrightEnvironment    `json:"environment"`
	}{runtime.Browser, runtime.Environment}
	return digest(marshalTestJSON(t, stable))
}

func testPRBaselineDigest(files []PullRequestFileIdentity) string {
	records := make([]string, 0, len(files))
	for _, identity := range files {
		records = append(records, identity.Path+"\x00"+identity.SHA256+"\x00"+strconv.FormatInt(identity.Bytes, 10))
	}
	return digest([]byte(strings.Join(records, "\n")))
}

func testPRRunnerAttestation(t *testing.T, binding PullRequestSourceBinding, executionBinding map[string]string, pipelineExit, runnerOutcome PullRequestFileIdentity) []byte {
	t.Helper()
	payload := pullRequestRunnerExecutionPayload{
		SchemaVersion: PullRequestRunnerExecutionSchema, Repository: binding.Repository, PullRequest: binding.PullRequest,
		BaseRef: binding.Base.Ref, HeadRef: binding.Head.Ref, HeadSHA: binding.Head.SHA, RunID: binding.Workflow.RunID,
		RunAttempt: binding.Workflow.RunAttempt, Workflow: binding.Workflow.Name, ProducerCommit: binding.Workflow.ProducerCommit,
		ExecutionBinding: cloneStringMap(executionBinding), Report: binding.Report, Runtime: binding.Runtime.Receipt,
		GeneratedSpec: binding.Runtime.GeneratedSpec, GeneratedConfig: binding.Runtime.GeneratedConfig, PipelineExit: pipelineExit, RunnerOutcome: runnerOutcome,
	}
	payloadData := marshalTestJSON(t, payload)
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payloadData)
	attestation := pullRequestRunnerAttestation{
		SchemaVersion: PullRequestRunnerAttestationSchema, Algorithm: "HMAC-SHA256", KeyCommitmentSHA256: digest(key),
		KeyBase64: base64.StdEncoding.EncodeToString(key), PayloadSHA256: digest(payloadData), PayloadBase64: base64.StdEncoding.EncodeToString(payloadData),
		MACSHA256: hex.EncodeToString(mac.Sum(nil)),
	}
	return marshalTestJSON(t, attestation)
}

func testPRReceiptLines(data []byte) []string {
	text := strings.TrimSuffix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if text == "" {
		return []string{}
	}
	lines := strings.Split(text, "\n")
	sort.Strings(lines)
	return lines
}

func marshalTestJSON(t *testing.T, value interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func cloneStringMap(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func rebuildTestPRSourceIndex(t *testing.T, manifestPath, sourceRoot string) {
	t.Helper()
	indexPath := ".visual-hive/artifacts-index.json"
	index := ArtifactIndexReport{
		SchemaVersion: 1, Project: "demo", GeneratedAt: "2026-07-09T12:06:00.000Z", Root: ".visual-hive", ContentAddressed: true, Complete: true,
		Artifacts: []ArtifactIndexEntry{}, Warnings: []string{},
	}
	root := filepath.Join(sourceRoot, ".visual-hive")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == indexPath {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		kind, contentType := testPRArtifactKind(relative)
		index.Artifacts = append(index.Artifacts, ArtifactIndexEntry{Path: relative, Kind: kind, ContentType: contentType, Bytes: int64(len(data)), SHA256: digest(data), SafeToRender: true, Labels: []string{}})
		index.Summary.TotalBytes += int64(len(data))
		switch kind {
		case "json":
			index.Summary.JSON++
		case "markdown":
			index.Summary.Markdown++
		case "image":
			index.Summary.Image++
		case "text":
			index.Summary.Text++
		case "typescript":
			index.Summary.TypeScript++
		case "yaml":
			index.Summary.YAML++
		case "log":
			index.Summary.Log++
		default:
			index.Summary.Other++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(index.Artifacts, func(i, j int) bool { return index.Artifacts[i].Path < index.Artifacts[j].Path })
	index.Summary.ArtifactCount = int64(len(index.Artifacts))
	index.Summary.DiscoveredArtifactCount = index.Summary.ArtifactCount
	indexData := marshalTestJSON(t, index)
	writeTestData(t, filepath.Join(sourceRoot, filepath.FromSlash(indexPath)), indexData)

	manifest := readManifest(t, manifestPath)
	for position := range manifest.Files {
		if manifest.Files[position].SourcePath == indexPath {
			manifest.Files[position].SHA256 = digest(indexData)
			manifest.Files[position].Size = int64(len(indexData))
			writeTestData(t, filepath.Join(filepath.Dir(manifestPath), filepath.FromSlash(manifest.Files[position].Path)), indexData)
		}
	}
	manifest.ArtifactIndex.SHA256 = digest(indexData)
	manifest.ArtifactIndex.ArtifactCount = index.Summary.ArtifactCount
	manifest.ArtifactIndex.TotalBytes = index.Summary.TotalBytes
	resignTestManifest(t, manifestPath, manifest)
}

func testPRArtifactKind(path string) (string, string) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return "json", "application/json"
	case ".md":
		return "markdown", "text/markdown"
	case ".png":
		return "image", "image/png"
	case ".txt":
		return "text", "text/plain"
	case ".ts", ".tsx":
		return "typescript", "text/plain"
	case ".yaml", ".yml":
		return "yaml", "text/plain"
	case ".log":
		return "log", "text/plain"
	default:
		return "other", "application/octet-stream"
	}
}

func TestPullRequestBundleCannotEscalateThroughMutableValidationFields(t *testing.T) {
	fixture := writeCompleteTestPullRequestBundle(t, nil)
	bundle := validateAndVerifyTestPullRequestSource(t, fixture)
	// These fields remain exported for legacy reporting compatibility. Even a
	// hostile caller flipping them cannot change the private PR validation
	// profile established by the local parser.
	bundle.Validation.Trusted = true
	bundle.Validation.Authoritative = true
	bundle.provenanceVerified = true

	beadStore, err := beads.NewStore(filepath.Join(t.TempDir(), "beads"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Import(beadStore); err == nil || !strings.Contains(err.Error(), "never grants bead import authority") {
		t.Fatalf("mutable validation fields escalated a PR bundle into bead import: %v", err)
	}
	lifecycle, err := NewLifecycleStore(filepath.Join(t.TempDir(), "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{}); err == nil || !strings.Contains(err.Error(), "check evidence only") {
		t.Fatalf("mutable validation fields escalated a PR bundle into lifecycle authority: %v", err)
	}
	if beadStore.Count() != 0 || len(lifecycle.Snapshot().Findings) != 0 || len(lifecycle.Snapshot().Outbox) != 0 {
		t.Fatalf("rejected PR escalation changed normal Hive state")
	}
}

func validateAndVerifyTestPullRequestSource(t *testing.T, fixture testPRSourceFixture) *ValidatedBundle {
	t.Helper()
	bundle, err := ParsePullRequestBundle(fixture.ManifestPath, testPullRequestParseOptions())
	if err != nil {
		t.Fatalf("validate PR bundle: %v", err)
	}
	if err := bundle.VerifySourceArtifact(fixture.SourceRoot); err != nil {
		t.Fatalf("verify complete PR source artifact: %v", err)
	}
	if bundle.provenanceVerified || bundle.Validation.Trusted || bundle.Validation.Authoritative {
		t.Fatalf("local PR parse/source verification acquired authority: %+v", bundle.Validation)
	}
	return bundle
}
