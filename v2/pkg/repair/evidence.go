package repair

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const maxVerdictEvidenceBytes = 2 << 20
const maxCoverageEvidenceBytes = 2 << 20
const maxTestCreationEvidenceBytes = 2 << 20

var ErrNoActionableEvidence = errors.New("verified Visual Hive evidence has no actionable contribution")

var evidenceANSIStyle = regexp.MustCompile(`\x1b\[[0-9;]*m`)

type verdictEvidence struct {
	SchemaVersion    string                 `json:"schemaVersion"`
	AllContributions []evidenceContribution `json:"allContributions"`
}

type evidenceContribution struct {
	Source     string `json:"source"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Gating     bool   `json:"gating"`
	Mode       string `json:"mode"`
	Operator   string `json:"operator"`
	ContractID string `json:"contractId"`
	TargetID   string `json:"targetId"`
	Reason     string `json:"reason"`
	Key        string `json:"key"`
	Authority  string `json:"authority"`
}

type coverageEvidence struct {
	MaintenanceFindings []coverageMaintenanceFinding `json:"maintenanceFindings"`
	Recommendations     []coverageRecommendation     `json:"recommendations"`
}

type coverageMaintenanceFinding struct {
	ID                string   `json:"id"`
	Kind              string   `json:"kind"`
	ContractID        string   `json:"contractId"`
	TargetID          string   `json:"targetId"`
	Route             string   `json:"route"`
	Viewport          string   `json:"viewport"`
	Message           string   `json:"message"`
	Evidence          []string `json:"evidence"`
	RecommendedAction string   `json:"recommendedAction"`
}

type coverageRecommendation struct {
	ID                   string   `json:"id"`
	Kind                 string   `json:"kind"`
	Title                string   `json:"title"`
	ContractID           string   `json:"contractId"`
	TargetID             string   `json:"targetId"`
	Route                string   `json:"route"`
	Viewport             string   `json:"viewport"`
	Rationale            []string `json:"rationale"`
	SuggestedTests       []string `json:"suggestedTests"`
	MaintenanceFindingID string   `json:"maintenanceFindingId"`
	SuggestedConfigYAML  string   `json:"suggestedConfigYaml"`
}

type testCreationEvidence struct {
	SchemaVersion   string                       `json:"schemaVersion"`
	GeneratedAt     string                       `json:"generatedAt"`
	Project         string                       `json:"project"`
	OutputResource  *testCreationOutputResource  `json:"outputResource,omitempty"`
	SourceArtifacts *testCreationSourceArtifacts `json:"sourceArtifacts"`
	Governance      *testCreationGovernance      `json:"governance"`
	Summary         *testCreationSummary         `json:"summary"`
	Recommendations []testCreationRecommendation `json:"recommendations"`
}

type testCreationRecommendation struct {
	ID                       string                         `json:"id"`
	GapID                    string                         `json:"gapId"`
	Source                   string                         `json:"source"`
	Kind                     string                         `json:"kind"`
	Priority                 string                         `json:"priority"`
	Title                    string                         `json:"title"`
	Rationale                []string                       `json:"rationale"`
	Affected                 *testCreationAffected          `json:"affected"`
	CurrentEvidence          []string                       `json:"currentEvidence"`
	Grounding                *testCreationGrounding         `json:"grounding"`
	SuggestedContract        *testCreationSuggestedContract `json:"suggestedContract"`
	SuggestedMutation        string                         `json:"suggestedMutation"`
	ValidationCommand        string                         `json:"validationCommand"`
	HiveOwner                string                         `json:"hiveOwner"`
	Layer                    *testCreationLayer             `json:"layer,omitempty"`
	TargetID                 string                         `json:"targetId,omitempty"`
	ContractID               string                         `json:"contractId,omitempty"`
	MutationOperator         string                         `json:"mutationOperator,omitempty"`
	CoverageRecommendationID string                         `json:"coverageRecommendationId,omitempty"`
	HandoffWorkItemID        string                         `json:"handoffWorkItemId,omitempty"`
	SuggestedTests           []string                       `json:"suggestedTests"`
	SuggestedConfigYAML      string                         `json:"suggestedConfigYaml,omitempty"`
	Artifacts                []string                       `json:"artifacts"`
	TrustedOnly              *bool                          `json:"trustedOnly"`
	ApplyMode                string                         `json:"applyMode"`
}

type testCreationOutputResource struct {
	ArtifactPath                string `json:"artifactPath"`
	EvidenceResourceID          string `json:"evidenceResourceId"`
	EvidenceResourceURI         string `json:"evidenceResourceUri"`
	EvidenceResourceTitle       string `json:"evidenceResourceTitle"`
	EvidenceResourceDescription string `json:"evidenceResourceDescription"`
	EvidenceReadToolName        string `json:"evidenceReadToolName,omitempty"`
}

type testCreationSourceArtifacts struct {
	EvidencePacket          string `json:"evidencePacket,omitempty"`
	CoverageRecommendations string `json:"coverageRecommendations,omitempty"`
	HandoffPacket           string `json:"handoffPacket,omitempty"`
}

type testCreationGovernance struct {
	VerdictAuthority string `json:"verdictAuthority"`
	AgentAuthority   string `json:"agentAuthority"`
	WritePolicy      string `json:"writePolicy"`
	SecretPolicy     string `json:"secretPolicy"`
}

type testCreationSummary struct {
	Total                       *int64 `json:"total"`
	High                        *int64 `json:"high"`
	Medium                      *int64 `json:"medium"`
	Low                         *int64 `json:"low"`
	FromTestingLayers           *int64 `json:"fromTestingLayers"`
	FromCoverageRecommendations *int64 `json:"fromCoverageRecommendations"`
	FromMutationSurvivors       *int64 `json:"fromMutationSurvivors"`
	FromHandoffWorkItems        *int64 `json:"fromHandoffWorkItems"`
}

type testCreationAffected struct {
	Route     string `json:"route,omitempty"`
	Component string `json:"component,omitempty"`
	Viewport  string `json:"viewport,omitempty"`
	State     string `json:"state,omitempty"`
}

type testCreationGrounding struct {
	Status            string   `json:"status"`
	Evidence          []string `json:"evidence"`
	UnresolvedReasons []string `json:"unresolvedReasons"`
}

type testCreationSuggestedContract struct {
	ID                    string   `json:"id"`
	Description           string   `json:"description"`
	TargetID              string   `json:"targetId,omitempty"`
	Route                 string   `json:"route,omitempty"`
	Viewport              string   `json:"viewport,omitempty"`
	Selectors             []string `json:"selectors"`
	MustNotExistSelectors []string `json:"mustNotExistSelectors"`
	TextMustExist         []string `json:"textMustExist"`
	TextMustNotExist      []string `json:"textMustNotExist"`
	MaskSelectors         []string `json:"maskSelectors"`
}

type testCreationLayer struct {
	ID     *int64 `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type testCreationSummaryCounts struct {
	Total                       int64
	High                        int64
	Medium                      int64
	Low                         int64
	FromTestingLayers           int64
	FromCoverageRecommendations int64
	FromMutationSurvivors       int64
	FromHandoffWorkItems        int64
}

// LoadEvidenceSummary reads only the deterministic verdict from an independently
// fetched source artifact. It deliberately does not expose arbitrary repository
// prose or large artifact payloads to the repair model.
func LoadEvidenceSummary(root string, finding visualhive.FindingLifecycle) (string, error) {
	issueKind := strings.ToLower(strings.TrimSpace(finding.IssueKind))
	if issueKind == visualhive.RepositoryTestFailureKind {
		return loadRepositoryTestEvidenceSummary(finding)
	}
	if strings.TrimSpace(root) == "" {
		return "", nil
	}
	verdictPath := filepath.Join(root, "verdict.json")
	info, err := os.Lstat(verdictPath)
	if err != nil {
		return "", fmt.Errorf("verified Visual Hive evidence is missing verdict.json: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxVerdictEvidenceBytes {
		return "", fmt.Errorf("verified Visual Hive verdict has an invalid size or type")
	}
	file, err := os.Open(verdictPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var verdict verdictEvidence
	decoder := json.NewDecoder(io.LimitReader(file, maxVerdictEvidenceBytes+1))
	if err := decoder.Decode(&verdict); err != nil {
		return "", fmt.Errorf("decode verified Visual Hive verdict: %w", err)
	}
	if len(verdict.AllContributions) > 2048 {
		return "", fmt.Errorf("verified Visual Hive verdict has too many contributions")
	}
	if issueKind == "test_adequacy_gap" {
		return loadTestCreationEvidenceSummary(root, finding)
	}

	if issueKind == "mutation_survivor" {
		return loadMutationSurvivorEvidenceSummary(verdict, finding)
	}
	contracts := make(map[string]bool, len(finding.AffectedContracts))
	for _, contract := range finding.AffectedContracts {
		contracts[strings.ToLower(strings.TrimSpace(contract))] = true
	}
	title := strings.ToLower(finding.Title)
	matched := make([]evidenceContribution, 0)
	for _, contribution := range verdict.AllContributions {
		if !contracts[strings.ToLower(strings.TrimSpace(contribution.ContractID))] {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(contribution.Status))
		if status != "failed" && status != "blocked" && status != "warning" {
			continue
		}
		signal := strings.ToLower(strings.TrimSpace(contribution.Kind))
		if signal == "" || (signal != issueKind && !strings.Contains(title, signal)) {
			continue
		}
		operator := strings.ToLower(strings.TrimSpace(contribution.Operator))
		if operator != "" && !containsEvidenceToken(title, operator) {
			continue
		}
		matched = append(matched, contribution)
	}
	if len(matched) == 0 {
		for _, contribution := range verdict.AllContributions {
			if contracts[strings.ToLower(strings.TrimSpace(contribution.ContractID))] && contribution.Gating && strings.EqualFold(contribution.Status, "failed") {
				matched = append(matched, contribution)
			}
		}
	}
	if len(matched) == 0 {
		coverageSummary, coverageErr := loadCoverageEvidenceSummary(root, finding, contracts)
		if coverageErr != nil {
			return "", coverageErr
		}
		if coverageSummary != "" {
			return coverageSummary, nil
		}
		return "", fmt.Errorf("%w for affected contracts", ErrNoActionableEvidence)
	}

	lines := make([]string, 0, len(matched))
	seen := map[string]bool{}
	for _, contribution := range matched {
		values := []string{contribution.Key, contribution.Source, contribution.Kind, contribution.Status, contribution.Operator, contribution.ContractID, contribution.TargetID, contribution.Reason}
		for index, value := range values {
			safe, err := safeEvidenceValue(value)
			if err != nil {
				return "", err
			}
			values[index] = safe
		}
		line := fmt.Sprintf("- key=%s source=%s kind=%s status=%s operator=%s contract=%s target=%s reason=%s", values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7])
		if !seen[line] {
			seen[line] = true
			lines = append(lines, line)
		}
	}
	sort.Strings(lines)
	summary := strings.Join(lines, "\n")
	if len(summary) > 16<<10 {
		return "", fmt.Errorf("verified Visual Hive evidence summary exceeds limit")
	}
	return string(bytes.Clone([]byte(summary))), nil
}

func loadRepositoryTestEvidenceSummary(finding visualhive.FindingLifecycle) (string, error) {
	command := strings.TrimSpace(finding.ValidationCommand)
	if command == "" || len(finding.AffectedContracts) != 1 {
		return "", fmt.Errorf("%w: repository test finding lacks one exact command contract", ErrNoActionableEvidence)
	}
	// The lifecycle finding was created only from independently queried GitHub
	// job-step conclusions. Reconstruct and validate its stable command identity;
	// never re-read a target-writable workspace ledger for repair prompting.
	result, err := visualhive.NewRepositoryTestResult(command, 1)
	if err != nil || finding.AffectedContracts[0] != result.Contract || finding.RootCauseKey != "repository-test-failure/"+result.Digest || finding.Fingerprint != "repository-test:"+result.Digest {
		return "", fmt.Errorf("%w: repository test finding identity does not match its verified command", ErrNoActionableEvidence)
	}
	safeCommand, safeErr := safeEvidenceValue(result.Command)
	if safeErr != nil {
		return "", safeErr
	}
	return fmt.Sprintf("- key=repository_test.%s source=github_actions_job_step kind=%s status=failed exit=nonzero contract=%s command=%s required_scope=smallest_fix_without_weakening_test_plan", result.Digest, visualhive.RepositoryTestFailureKind, result.Contract, safeCommand), nil
}

func loadMutationSurvivorEvidenceSummary(verdict verdictEvidence, finding visualhive.FindingLifecycle) (string, error) {
	operator, targetID, contracts, rootBound, err := mutationBindingForFinding(finding)
	if err != nil {
		return "", err
	}
	expectedKey := "mutation.mutation_survivor." + operator
	matched := make([]evidenceContribution, 0, 1)
	for _, contribution := range verdict.AllContributions {
		if contribution.Source != "mutation" || contribution.Kind != "mutation_survivor" || contribution.Status != "failed" || !contribution.Gating ||
			contribution.Operator != operator || contribution.Key != expectedKey || !contracts[contribution.ContractID] ||
			(rootBound && contribution.TargetID != targetID) {
			continue
		}
		matched = append(matched, contribution)
	}
	if len(matched) != 1 {
		return "", fmt.Errorf("%w for exact mutation operator %q (matched %d canonical contributions)", ErrNoActionableEvidence, operator, len(matched))
	}
	contribution := matched[0]
	values := []string{contribution.Key, contribution.Source, contribution.Kind, contribution.Status, contribution.Operator, contribution.ContractID, contribution.TargetID, contribution.Reason}
	for index, value := range values {
		safe, safeErr := safeEvidenceValue(value)
		if safeErr != nil {
			return "", safeErr
		}
		values[index] = safe
	}
	return fmt.Sprintf("- key=%s source=%s kind=%s status=%s operator=%s contract=%s target=%s reason=%s", values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7]), nil
}

func mutationBindingForFinding(finding visualhive.FindingLifecycle) (string, string, map[string]bool, bool, error) {
	contracts := make(map[string]bool, len(finding.AffectedContracts))
	for _, contract := range finding.AffectedContracts {
		if contract == "" || len(contract) > 512 || contracts[contract] {
			return "", "", nil, false, fmt.Errorf("%w: mutation survivor has invalid affected contracts", ErrNoActionableEvidence)
		}
		contracts[contract] = true
	}
	if len(contracts) == 0 {
		return "", "", nil, false, fmt.Errorf("%w: mutation survivor has no affected contracts", ErrNoActionableEvidence)
	}

	root := strings.TrimSpace(finding.RootCauseKey)
	if root != "" {
		parts := strings.Split(root, "/")
		if len(parts) != 4 || parts[0] != "mutation" {
			return "", "", nil, false, fmt.Errorf("%w: mutation survivor has incompatible root cause key", ErrNoActionableEvidence)
		}
		operator, err := url.PathUnescape(parts[1])
		if err != nil || !safeEvidenceOperator(operator) {
			return "", "", nil, false, fmt.Errorf("%w: mutation survivor root has an invalid operator", ErrNoActionableEvidence)
		}
		targetID, err := url.PathUnescape(parts[2])
		if err != nil || !safeMutationIdentity(targetID) {
			return "", "", nil, false, fmt.Errorf("%w: mutation survivor root has an invalid target", ErrNoActionableEvidence)
		}
		rootContracts := make(map[string]bool)
		for _, encoded := range strings.Split(parts[3], ",") {
			contract, decodeErr := url.PathUnescape(encoded)
			if decodeErr != nil || !safeMutationIdentity(contract) || rootContracts[contract] {
				return "", "", nil, false, fmt.Errorf("%w: mutation survivor root has invalid contracts", ErrNoActionableEvidence)
			}
			rootContracts[contract] = true
		}
		if len(rootContracts) != len(contracts) {
			return "", "", nil, false, fmt.Errorf("%w: mutation survivor root contracts do not match the finding", ErrNoActionableEvidence)
		}
		for contract := range rootContracts {
			if !contracts[contract] {
				return "", "", nil, false, fmt.Errorf("%w: mutation survivor root contracts do not match the finding", ErrNoActionableEvidence)
			}
		}
		return operator, targetID, rootContracts, true, nil
	}
	const prefix = "[visual hive] mutation survived:"
	title := strings.TrimSpace(finding.Title)
	lower := strings.ToLower(title)
	if !strings.HasPrefix(lower, prefix) {
		return "", "", nil, false, fmt.Errorf("%w: legacy mutation survivor title does not have the exact operator prefix", ErrNoActionableEvidence)
	}
	operator := strings.TrimSpace(title[len(prefix):])
	if !safeEvidenceOperator(operator) {
		return "", "", nil, false, fmt.Errorf("%w: legacy mutation survivor title has an invalid operator", ErrNoActionableEvidence)
	}
	return operator, "", contracts, false, nil
}

func safeMutationIdentity(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func safeEvidenceOperator(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '+' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func safeEvidenceValue(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.ContainsRune(value, '\x00') || len(value) > 4096 {
		return "", fmt.Errorf("verified Visual Hive evidence contains an unsafe value")
	}
	// Playwright failure messages may include deterministic terminal styling.
	// Remove only complete SGR sequences; every other control sequence remains
	// subject to the fail-closed control-character check below. Scan for secrets
	// after normalization so styling cannot split an otherwise detectable token.
	value = evidenceANSIStyle.ReplaceAllString(value, "")
	if err := validateRepairTextSecrets(value, "verified Visual Hive evidence"); err != nil {
		return "", err
	}
	var result strings.Builder
	result.Grow(len(value))
	for _, character := range value {
		switch character {
		case '\n':
			result.WriteString(`\n`)
		case '\r':
			result.WriteString(`\r`)
		case '\t':
			result.WriteString(`\t`)
		default:
			if character < 0x20 || character == 0x7f {
				return "", fmt.Errorf("verified Visual Hive evidence contains an unsafe control character")
			}
			result.WriteRune(character)
		}
	}
	return result.String(), nil
}

func containsEvidenceToken(value, token string) bool {
	value, token = strings.ToLower(value), strings.ToLower(strings.TrimSpace(token))
	if token == "" {
		return false
	}
	for offset := 0; offset <= len(value)-len(token); {
		index := strings.Index(value[offset:], token)
		if index < 0 {
			return false
		}
		index += offset
		beforeOK := index == 0 || !evidenceTokenCharacter(value[index-1])
		after := index + len(token)
		afterOK := after == len(value) || !evidenceTokenCharacter(value[after])
		if beforeOK && afterOK {
			return true
		}
		offset = index + 1
	}
	return false
}

func evidenceTokenCharacter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '_'
}

func loadTestCreationEvidenceSummary(root string, finding visualhive.FindingLifecycle) (string, error) {
	path := filepath.Join(root, "test-creation-plan.json")
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("verified Visual Hive test evidence is missing test-creation-plan.json: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxTestCreationEvidenceBytes {
		return "", fmt.Errorf("verified Visual Hive test-creation evidence has an invalid size or type")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxTestCreationEvidenceBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxTestCreationEvidenceBytes {
		return "", fmt.Errorf("verified Visual Hive test-creation evidence exceeds size limit")
	}
	var header struct {
		SchemaVersion string `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return "", fmt.Errorf("decode verified Visual Hive test-creation evidence: %w", err)
	}
	if header.SchemaVersion != "visual-hive.test-creation-plan.v2" {
		return "", fmt.Errorf("%w: verified Visual Hive test-creation evidence has unsupported schema %q", ErrNoActionableEvidence, header.SchemaVersion)
	}
	var evidence testCreationEvidence
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return "", fmt.Errorf("decode strict Visual Hive test-creation-plan v2: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("decode strict Visual Hive test-creation-plan v2: trailing JSON content")
	}
	if err := validateStrictTestCreationEvidence(evidence); err != nil {
		return "", fmt.Errorf("validate strict Visual Hive test-creation-plan v2: %w", err)
	}
	title := strings.ToLower(strings.TrimSpace(finding.Title))
	lines := make([]string, 0, 1)
	for _, recommendation := range evidence.Recommendations {
		if !groundedTestCreationRecommendation(recommendation) {
			continue
		}
		if !strings.Contains(title, strings.ToLower(strings.TrimSpace(recommendation.Title))) {
			continue
		}
		values := []string{
			recommendation.ID, recommendation.GapID, recommendation.Source, recommendation.Kind, recommendation.Priority,
			recommendation.Title, strings.Join(recommendation.Rationale, ","), strings.Join(recommendation.SuggestedTests, ","), strings.Join(recommendation.Artifacts, ","),
			strings.Join(recommendation.Grounding.Evidence, ","),
		}
		for index, value := range values {
			safe, err := safeEvidenceValue(value)
			if err != nil {
				return "", err
			}
			values[index] = safe
		}
		lines = append(lines, fmt.Sprintf("- key=test_creation.%s gap=%s source=%s kind=%s priority=%s title=%s rationale=%s suggested_tests=%s artifacts=%s grounding_evidence=%s required_scope=test_files_only", values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9]))
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("%w: verified Visual Hive test-creation evidence has no matching grounded unit-test recommendation", ErrNoActionableEvidence)
	}
	sort.Strings(lines)
	summary := strings.Join(lines, "\n")
	if len(summary) > 16<<10 {
		return "", fmt.Errorf("verified Visual Hive test-creation evidence summary exceeds limit")
	}
	return string(bytes.Clone([]byte(summary))), nil
}

func validateStrictTestCreationEvidence(evidence testCreationEvidence) error {
	if evidence.SchemaVersion != "visual-hive.test-creation-plan.v2" || !validTestCreationTimestamp(evidence.GeneratedAt) || !nonemptyTestCreationString(evidence.Project) {
		return fmt.Errorf("plan identity is incomplete")
	}
	if evidence.SourceArtifacts == nil || evidence.Governance == nil || evidence.Summary == nil || evidence.Recommendations == nil || len(evidence.Recommendations) > 2048 {
		return fmt.Errorf("plan is missing required source, governance, summary, or recommendations")
	}
	if evidence.Governance.VerdictAuthority != "visual_hive" || evidence.Governance.AgentAuthority != "advisory_test_generation_only" || evidence.Governance.WritePolicy != "no_config_or_test_files_written" || evidence.Governance.SecretPolicy != "redacted_values_names_only" {
		return fmt.Errorf("plan governance is invalid")
	}
	if evidence.OutputResource != nil {
		resource := evidence.OutputResource
		if resource.ArtifactPath != ".visual-hive/test-creation-plan.json" || resource.EvidenceResourceID != "test-creation-plan" || resource.EvidenceResourceURI != "visual-hive://test-creation-plan" || resource.EvidenceResourceTitle != "Test Creation Plan" || !nonemptyTestCreationString(resource.EvidenceResourceDescription) || (resource.EvidenceReadToolName != "" && resource.EvidenceReadToolName != "visual_hive_read_test_creation_plan") {
			return fmt.Errorf("plan output resource identity is invalid")
		}
	}
	for _, value := range []string{evidence.SourceArtifacts.EvidencePacket, evidence.SourceArtifacts.CoverageRecommendations, evidence.SourceArtifacts.HandoffPacket} {
		if !boundedTestCreationString(value) {
			return fmt.Errorf("plan source artifact path exceeds its bound")
		}
	}

	actual := testCreationSummaryCounts{Total: int64(len(evidence.Recommendations))}
	seen := make(map[string]bool, len(evidence.Recommendations))
	for index, recommendation := range evidence.Recommendations {
		if err := validateStrictTestCreationRecommendation(recommendation); err != nil {
			return fmt.Errorf("recommendation %d: %w", index, err)
		}
		if seen[recommendation.ID] {
			return fmt.Errorf("recommendation %d duplicates id %q", index, recommendation.ID)
		}
		seen[recommendation.ID] = true
		switch recommendation.Priority {
		case "high":
			actual.High++
		case "medium":
			actual.Medium++
		case "low":
			actual.Low++
		}
		switch recommendation.Source {
		case "testing_layer":
			actual.FromTestingLayers++
		case "coverage_recommendation":
			actual.FromCoverageRecommendations++
		case "mutation_survivor":
			actual.FromMutationSurvivors++
		case "handoff_work_item":
			actual.FromHandoffWorkItems++
		}
	}
	expected, err := testCreationSummaryValues(evidence.Summary)
	if err != nil {
		return err
	}
	if expected != actual {
		return fmt.Errorf("plan summary does not match its recommendations")
	}
	return nil
}

func validateStrictTestCreationRecommendation(recommendation testCreationRecommendation) error {
	if !nonemptyTestCreationString(recommendation.ID) || !nonemptyTestCreationString(recommendation.GapID) || !nonemptyTestCreationString(recommendation.Title) || !nonemptyTestCreationString(recommendation.SuggestedMutation) || !nonemptyTestCreationString(recommendation.ValidationCommand) {
		return fmt.Errorf("required identity or guidance is empty")
	}
	if !oneOfTestCreation(recommendation.Source, "testing_layer", "coverage_recommendation", "mutation_survivor", "handoff_work_item") || !oneOfTestCreation(recommendation.Kind, "unit_test", "accessibility_check", "api_contract", "selector_assertion", "screenshot", "flow", "mutation_mapping", "workflow_setup", "provider_review", "protected_canary", "history_review", "agent_handoff") || !oneOfTestCreation(recommendation.Priority, "low", "medium", "high") || !oneOfTestCreation(recommendation.HiveOwner, "quality", "tester", "ci-maintainer") || recommendation.ApplyMode != "advisory_no_write" || recommendation.TrustedOnly == nil {
		return fmt.Errorf("enumerated source, kind, priority, owner, trust, or apply mode is invalid")
	}
	if recommendation.Affected == nil || recommendation.Grounding == nil || recommendation.SuggestedContract == nil || recommendation.Rationale == nil || recommendation.CurrentEvidence == nil || recommendation.SuggestedTests == nil || recommendation.Artifacts == nil {
		return fmt.Errorf("required evidence, grounding, contract, or guidance field is missing")
	}
	for _, values := range [][]string{recommendation.Rationale, recommendation.CurrentEvidence, recommendation.SuggestedTests, recommendation.Artifacts} {
		if !validTestCreationStringList(values) {
			return fmt.Errorf("evidence or guidance list is invalid")
		}
	}
	for _, value := range []string{recommendation.Affected.Route, recommendation.Affected.Component, recommendation.Affected.Viewport, recommendation.Affected.State, recommendation.TargetID, recommendation.ContractID, recommendation.MutationOperator, recommendation.CoverageRecommendationID, recommendation.HandoffWorkItemID, recommendation.SuggestedConfigYAML} {
		if !boundedTestCreationString(value) {
			return fmt.Errorf("optional repository-specific value exceeds its bound")
		}
	}
	grounding := recommendation.Grounding
	if !oneOfTestCreation(grounding.Status, "grounded", "unresolved") || grounding.Evidence == nil || grounding.UnresolvedReasons == nil || !validTestCreationStringList(grounding.Evidence) || !validTestCreationStringList(grounding.UnresolvedReasons) {
		return fmt.Errorf("grounding is structurally invalid")
	}
	if grounding.Status == "grounded" {
		if len(grounding.Evidence) == 0 || len(grounding.UnresolvedReasons) != 0 {
			return fmt.Errorf("grounded recommendation lacks evidence or retains unresolved reasons")
		}
		for _, value := range grounding.Evidence {
			if !nonemptyTestCreationString(value) {
				return fmt.Errorf("grounded recommendation contains empty evidence")
			}
		}
	}
	contract := recommendation.SuggestedContract
	if !nonemptyTestCreationString(contract.ID) || !nonemptyTestCreationString(contract.Description) || !boundedTestCreationString(contract.TargetID) || !boundedTestCreationString(contract.Route) || !boundedTestCreationString(contract.Viewport) || contract.Selectors == nil || contract.MustNotExistSelectors == nil || contract.TextMustExist == nil || contract.TextMustNotExist == nil || contract.MaskSelectors == nil {
		return fmt.Errorf("suggested contract is structurally invalid")
	}
	for _, values := range [][]string{contract.Selectors, contract.MustNotExistSelectors, contract.TextMustExist, contract.TextMustNotExist, contract.MaskSelectors} {
		if !validTestCreationStringList(values) {
			return fmt.Errorf("suggested contract assertion list is invalid")
		}
	}
	if recommendation.Layer != nil && (recommendation.Layer.ID == nil || !nonemptyTestCreationString(recommendation.Layer.Name) || !oneOfTestCreation(recommendation.Layer.Status, "covered", "partial", "missing", "not_applicable", "unknown")) {
		return fmt.Errorf("testing layer identity is invalid")
	}
	return nil
}

func testCreationSummaryValues(summary *testCreationSummary) (testCreationSummaryCounts, error) {
	values := []*int64{summary.Total, summary.High, summary.Medium, summary.Low, summary.FromTestingLayers, summary.FromCoverageRecommendations, summary.FromMutationSurvivors, summary.FromHandoffWorkItems}
	for _, value := range values {
		if value == nil || *value < 0 {
			return testCreationSummaryCounts{}, fmt.Errorf("plan summary is missing a required non-negative count")
		}
	}
	return testCreationSummaryCounts{
		Total: *summary.Total, High: *summary.High, Medium: *summary.Medium, Low: *summary.Low,
		FromTestingLayers: *summary.FromTestingLayers, FromCoverageRecommendations: *summary.FromCoverageRecommendations,
		FromMutationSurvivors: *summary.FromMutationSurvivors, FromHandoffWorkItems: *summary.FromHandoffWorkItems,
	}, nil
}

func validTestCreationTimestamp(value string) bool {
	if !nonemptyTestCreationString(value) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func validTestCreationStringList(values []string) bool {
	if len(values) > 8192 {
		return false
	}
	for _, value := range values {
		if !boundedTestCreationString(value) {
			return false
		}
	}
	return true
}

func boundedTestCreationString(value string) bool {
	return len(value) <= 32768 && !strings.ContainsRune(value, '\x00')
}

func nonemptyTestCreationString(value string) bool {
	return boundedTestCreationString(value) && strings.TrimSpace(value) != ""
}

func oneOfTestCreation(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func groundedTestCreationRecommendation(recommendation testCreationRecommendation) bool {
	if strings.TrimSpace(recommendation.Source) != "testing_layer" || strings.TrimSpace(recommendation.Kind) != "unit_test" {
		return false
	}
	priority := strings.TrimSpace(recommendation.Priority)
	if priority != "high" && priority != "medium" {
		return false
	}
	if strings.TrimSpace(recommendation.ID) == "" || strings.TrimSpace(recommendation.GapID) == "" || strings.TrimSpace(recommendation.Title) == "" {
		return false
	}
	if recommendation.Rationale == nil || recommendation.SuggestedTests == nil || recommendation.Artifacts == nil || recommendation.Grounding == nil {
		return false
	}
	if recommendation.Grounding.Status != "grounded" || len(recommendation.Grounding.Evidence) == 0 || recommendation.Grounding.UnresolvedReasons == nil || len(recommendation.Grounding.UnresolvedReasons) != 0 {
		return false
	}
	for _, item := range recommendation.Grounding.Evidence {
		if strings.TrimSpace(item) == "" {
			return false
		}
	}
	return true
}

func loadCoverageEvidenceSummary(root string, finding visualhive.FindingLifecycle, contracts map[string]bool) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(finding.IssueKind))
	if kind != "missing_visual_coverage" && kind != "weak_visual_test" {
		return "", nil
	}
	path := filepath.Join(root, "coverage-recommendations.json")
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("verified Visual Hive coverage evidence is missing coverage-recommendations.json: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxCoverageEvidenceBytes {
		return "", fmt.Errorf("verified Visual Hive coverage evidence has an invalid size or type")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var evidence coverageEvidence
	if err := json.NewDecoder(io.LimitReader(file, maxCoverageEvidenceBytes+1)).Decode(&evidence); err != nil {
		return "", fmt.Errorf("decode verified Visual Hive coverage evidence: %w", err)
	}
	if len(evidence.MaintenanceFindings) > 2048 || len(evidence.Recommendations) > 2048 {
		return "", fmt.Errorf("verified Visual Hive coverage evidence has too many entries")
	}
	recommendations := make(map[string]string, len(evidence.Recommendations))
	for _, recommendation := range evidence.Recommendations {
		if id := strings.TrimSpace(recommendation.MaintenanceFindingID); id != "" {
			recommendations[id] = recommendation.SuggestedConfigYAML
		}
	}
	title := strings.ToLower(finding.Title)
	lines := make([]string, 0)
	for _, item := range evidence.Recommendations {
		if !contracts[strings.ToLower(strings.TrimSpace(item.ContractID))] {
			continue
		}
		itemKind, itemTitle := strings.ToLower(strings.TrimSpace(item.Kind)), strings.ToLower(strings.TrimSpace(item.Title))
		if (itemKind == "" || !strings.Contains(title, itemKind)) && (itemTitle == "" || !strings.Contains(title, itemTitle)) {
			continue
		}
		values := []string{item.ID, item.Kind, item.ContractID, item.TargetID, item.Route, item.Viewport, item.Title, strings.Join(item.Rationale, ","), strings.Join(item.SuggestedTests, ","), item.SuggestedConfigYAML}
		for index, value := range values {
			safe, err := safeEvidenceValue(value)
			if err != nil {
				return "", err
			}
			values[index] = safe
		}
		lines = append(lines, fmt.Sprintf("- key=coverage.%s source=coverage kind=%s status=warning contract=%s target=%s route=%s viewport=%s reason=%s rationale=%s suggested_tests=%s suggested_config=%s", values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9]))
	}
	for _, item := range evidence.MaintenanceFindings {
		if !contracts[strings.ToLower(strings.TrimSpace(item.ContractID))] || !strings.Contains(title, strings.ToLower(strings.TrimSpace(item.Kind))) {
			continue
		}
		values := []string{item.ID, item.Kind, item.ContractID, item.TargetID, item.Route, item.Viewport, item.Message, item.RecommendedAction, strings.Join(item.Evidence, ","), recommendations[strings.TrimSpace(item.ID)]}
		for index, value := range values {
			safe, err := safeEvidenceValue(value)
			if err != nil {
				return "", err
			}
			values[index] = safe
		}
		lines = append(lines, fmt.Sprintf("- key=coverage.%s source=coverage kind=%s status=warning contract=%s target=%s route=%s viewport=%s reason=%s action=%s evidence=%s suggested_config=%s", values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9]))
	}
	sort.Strings(lines)
	summary := strings.Join(lines, "\n")
	if len(summary) > 16<<10 {
		return "", fmt.Errorf("verified Visual Hive coverage evidence summary exceeds limit")
	}
	return string(bytes.Clone([]byte(summary))), nil
}
