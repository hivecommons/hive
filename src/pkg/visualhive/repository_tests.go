package visualhive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	RepositoryTestFailureKind = "repository_test_failure"
	repositoryTestPlanVersion = "hive.repository-tests.v1"
	maxRepositoryTestBytes    = 1 << 20
)

var safeRepositoryTestCommand = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=, -]+$`)

// RepositoryTestResult is one exact command outcome recorded by the generated
// Hive workflow. Command is presentation-only; repair execution continues to
// use the structured, installed Config.TestCommands argv.
type RepositoryTestResult struct {
	ExitCode int    `json:"exit_code"`
	Command  string `json:"command"`
	Contract string `json:"contract"`
	Digest   string `json:"digest"`
}

func NewRepositoryTestResult(command string, exitCode int) (RepositoryTestResult, error) {
	if exitCode < 0 || exitCode > 255 || command == "" || command != strings.TrimSpace(command) || len(command) > 4096 || !safeRepositoryTestCommand.MatchString(command) || secretValue.MatchString(command) || absolutePath.MatchString(command) {
		return RepositoryTestResult{}, fmt.Errorf("repository test command or exit code is unsafe")
	}
	digestValue := sha256.Sum256([]byte(command))
	digestText := hex.EncodeToString(digestValue[:])
	return RepositoryTestResult{
		ExitCode: exitCode,
		Command:  command,
		Contract: "repository-test:" + digestText[:24],
		Digest:   digestText,
	}, nil
}

// LoadRepositoryTestResults parses the bounded diagnostic command ledger used
// by PR review and legacy tests. Production lifecycle authority comes from the
// GitHub runner-owned job/step records passed to BuildRepositoryTestBundle.
func LoadRepositoryTestResults(root string) ([]RepositoryTestResult, int, error) {
	ledger, err := readRepositoryTestFile(root, "repository-tests.tsv")
	if err != nil {
		return nil, 0, err
	}
	summary, err := readRepositoryTestFile(root, "repository-test-exit-code.txt")
	if err != nil {
		return nil, 0, err
	}
	summaryValue := strings.TrimSpace(string(summary))
	if summaryValue == "" || strings.ContainsAny(summaryValue, "\r\n\t ") {
		return nil, 0, fmt.Errorf("repository test summary is not one decimal exit code")
	}
	overall, err := strconv.Atoi(summaryValue)
	if err != nil || overall < 0 || overall > 255 {
		return nil, 0, fmt.Errorf("repository test summary has an invalid exit code")
	}

	lines := strings.Split(strings.ReplaceAll(string(ledger), "\r\n", "\n"), "\n")
	results := make([]RepositoryTestResult, 0, len(lines))
	seen := map[string]bool{}
	firstFailure := 0
	for index, line := range lines {
		if line == "" && index == len(lines)-1 {
			continue
		}
		if line == "" {
			return nil, 0, fmt.Errorf("repository test ledger contains an empty row")
		}
		codeText, command, ok := strings.Cut(line, "\t")
		if !ok || strings.ContainsRune(command, '\t') {
			return nil, 0, fmt.Errorf("repository test ledger row %d is not code-tab-command", index+1)
		}
		code, parseErr := strconv.Atoi(codeText)
		if parseErr != nil || code < 0 || code > 255 {
			return nil, 0, fmt.Errorf("repository test ledger row %d has an invalid exit code", index+1)
		}
		if seen[command] {
			return nil, 0, fmt.Errorf("repository test ledger repeats command %q", command)
		}
		seen[command] = true
		result, resultErr := NewRepositoryTestResult(command, code)
		if resultErr != nil {
			return nil, 0, fmt.Errorf("repository test ledger row %d has an unsafe command", index+1)
		}
		results = append(results, result)
		if firstFailure == 0 && code != 0 {
			firstFailure = code
		}
	}
	if overall != firstFailure {
		return nil, 0, fmt.Errorf("repository test summary %d does not match first non-zero ledger result %d", overall, firstFailure)
	}
	return results, overall, nil
}

// EncodeRepositoryTestEvidence is the canonical presentation encoding of
// runner-owned GitHub job/step outcomes. Its digest binds the structured
// results Hive obtained from the Actions API; target workspace files are not
// an authority input.
func EncodeRepositoryTestEvidence(results []RepositoryTestResult, overall int) ([]byte, []byte, string, error) {
	if overall < 0 || overall > 255 {
		return nil, nil, "", fmt.Errorf("repository test summary has an invalid exit code")
	}
	var ledger strings.Builder
	seen := map[string]bool{}
	firstFailure := 0
	for index, result := range results {
		expected, err := NewRepositoryTestResult(result.Command, result.ExitCode)
		if err != nil || expected.Contract != result.Contract || expected.Digest != result.Digest || seen[result.Command] {
			return nil, nil, "", fmt.Errorf("repository test result %d has invalid canonical identity", index+1)
		}
		seen[result.Command] = true
		fmt.Fprintf(&ledger, "%d\t%s\n", result.ExitCode, result.Command)
		if firstFailure == 0 && result.ExitCode != 0 {
			firstFailure = result.ExitCode
		}
	}
	if overall != firstFailure {
		return nil, nil, "", fmt.Errorf("repository test summary %d does not match first non-zero result %d", overall, firstFailure)
	}
	ledgerBytes := []byte(ledger.String())
	summaryBytes := []byte(strconv.Itoa(overall) + "\n")
	digestValue := sha256.New()
	_, _ = digestValue.Write([]byte("hive.repository-tests.v1\x00"))
	_, _ = digestValue.Write(ledgerBytes)
	_, _ = digestValue.Write([]byte{0})
	_, _ = digestValue.Write(summaryBytes)
	return ledgerBytes, summaryBytes, hex.EncodeToString(digestValue.Sum(nil)), nil
}

// BuildRepositoryTestBundle derives a separate, provenance-bound lifecycle
// bundle from canonical GitHub job/step evidence and an already independently
// verified Visual Hive production bundle. It never alters Visual Hive's
// verdict. Failed commands become repairable findings; only a completely
// green plan emits authoritative absent observations that may close a finding.
func BuildRepositoryTestBundle(parent *ValidatedBundle, results []RepositoryTestResult, overall int, expectedEvidenceDigest string) (*ValidatedBundle, error) {
	// Source.Trusted is an explicitly advisory producer claim. Repository-test
	// authority additionally requires Hive's private provenance and complete
	// source-verification state; the public Trusted field alone is insufficient.
	if parent == nil || parent.Manifest.SchemaVersion != ManifestSchemaV3 || !parent.Validation.Trusted || !parent.provenanceVerified || !parent.sourceVerified {
		return nil, fmt.Errorf("independently verified complete-source parent Visual Hive bundle is required for repository test evidence")
	}
	if parent.Manifest.Source.Event != "workflow_dispatch" || parent.Manifest.Source.WorkflowRunID == "" || parent.Manifest.Source.CommitSHA == "" || parent.Manifest.Source.Repository == "" || parent.Manifest.Source.RepositoryID == "" {
		return nil, fmt.Errorf("parent Visual Hive bundle lacks exact production workflow provenance")
	}
	ledger, summary, evidenceDigest, err := EncodeRepositoryTestEvidence(results, overall)
	if err != nil {
		return nil, err
	}
	if expectedEvidenceDigest == "" || expectedEvidenceDigest != evidenceDigest {
		return nil, fmt.Errorf("runner-controlled repository test artifact digest mismatch")
	}

	generatedAt := parent.Manifest.GeneratedAt.UTC()
	if generatedAt.IsZero() {
		return nil, fmt.Errorf("parent Visual Hive bundle has no generation timestamp")
	}
	authoritative := overall == 0
	observations := make([]Observation, 0, len(results))
	evaluated := make([]string, 0, len(results))
	for _, result := range results {
		if !authoritative && result.ExitCode == 0 {
			continue
		}
		state := "present"
		if authoritative {
			state = "absent"
		}
		identity := sha256.Sum256([]byte(strings.ToLower(parent.Manifest.Source.Repository) + "\x00" + result.Contract))
		repositoryFingerprint := hex.EncodeToString(identity[:])
		body := fmt.Sprintf("The exact hosted repository test plan returned exit %d for `%s`. Hive verified the GitHub runner-owned job and command-step result for the production workflow at commit `%s`; target-written ledgers and artifact text were not authority inputs. Repair the underlying repository failure; do not remove, skip, or weaken the command.", result.ExitCode, result.Command, parent.Manifest.Source.CommitSHA)
		observations = append(observations, Observation{
			Fingerprint:           "repository-test:" + result.Digest,
			RepositoryFingerprint: repositoryFingerprint,
			PublicationRole:       "canonical",
			RootCauseKey:          "repository-test-failure/" + result.Digest,
			BlockedByRootKeys:     []string{},
			State:                 state,
			IssueKind:             RepositoryTestFailureKind,
			Severity:              "high",
			OwningAgentHint:       "hive/test-repair",
			Title:                 "[Hive] Repository test failed: " + truncateRepositoryTestCommand(result.Command, 180),
			Body:                  body,
			Labels:                []string{"repository-test-failure", "hive/test-repair"},
			SourceArtifacts:       []string{".visual-hive/repository-tests.tsv", ".visual-hive/repository-test-exit-code.txt"},
			AffectedContracts:     []string{result.Contract},
			ValidationCommand:     result.Command,
			ObservedAt:            generatedAt.Format(time.RFC3339Nano),
			FirstSeenAt:           generatedAt.Format(time.RFC3339Nano),
			SourceArtifact:        ".visual-hive/repository-tests.tsv",
		})
		evaluated = append(evaluated, result.Contract)
	}
	sort.Slice(observations, func(left, right int) bool {
		return observations[left].RepositoryFingerprint < observations[right].RepositoryFingerprint
	})
	sort.Strings(evaluated)

	files := []File{
		repositoryTestFileRecord("repository-tests.tsv", ledger),
		repositoryTestFileRecord("repository-test-exit-code.txt", summary),
	}
	parentKey := strings.TrimSpace(parent.Manifest.ReplayProtection.Key)
	if parentKey == "" {
		return nil, fmt.Errorf("parent Visual Hive bundle has no replay key")
	}
	replayDigest := sha256.Sum256([]byte(parentKey + "\x00" + repositoryTestPlanVersion + "\x00" + evidenceDigest))
	replayKey := "repository-tests:" + hex.EncodeToString(replayDigest[:])
	manifest := Manifest{
		SchemaVersion:     ManifestSchema,
		DigestAlgorithm:   PublicationDigestAlgorithm,
		GeneratedAt:       generatedAt,
		ExpiresAt:         parent.Manifest.ExpiresAt,
		Producer:          Producer{Name: "hive-repository-test-adapter", Version: "1", GitCommit: parent.Manifest.Producer.GitCommit},
		Source:            parent.Manifest.Source,
		Project:           parent.Manifest.Project,
		Mode:              "full",
		Verdict:           map[bool]string{true: "passed", false: "failed"}[authoritative],
		ACMMRequest:       parent.Manifest.ACMMRequest,
		ExternalCallsMade: 0,
		Scan: Scan{
			Scope: "full", AuthoritativeForResolution: authoritative,
			EvaluatedContracts: evaluated, TestPlanVersion: repositoryTestPlanVersion,
			ToolRegistryVersion: parent.Manifest.Scan.ToolRegistryVersion,
		},
		Observations: observations,
		Files:        files,
		ReplayProtection: ReplayProtection{
			Nonce: hex.EncodeToString(replayDigest[:16]), Key: replayKey,
		},
		Safety: Safety{
			AtomicWrite: true, PathsAreRelative: true, DigestsRequired: true,
			ProducerCountersAreAdvisory: true, ProducerTrustClaimIsAdvisory: true,
			AbsenceRequiresAuthoritativeScan: true,
		},
	}
	manifest.BundleID = "repository-tests-" + hex.EncodeToString(replayDigest[:16])
	manifest.OverallDigest = digestBundleContent(manifest, nil)
	manifest.Provenance = Provenance{Kind: "github-actions-runner-outcome-artifact-derived", SubjectDigest: manifest.OverallDigest, AttestationRequired: true}
	validation := Validation{
		SchemaVersion: "hive.visual-hive-validation.v1", Status: "passed", BundleID: manifest.BundleID,
		Project: manifest.Project, Digest: manifest.OverallDigest, Files: len(files),
		Bytes: int64(len(ledger) + len(summary)), Trusted: true, Authoritative: authoritative,
		Observations: len(observations),
	}
	return &ValidatedBundle{Manifest: manifest, Validation: validation}, nil
}

func readRepositoryTestFile(root, name string) ([]byte, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("verified repository test evidence root is required")
	}
	target := filepath.Join(root, name)
	info, err := os.Lstat(target)
	if err != nil {
		return nil, fmt.Errorf("verified repository test evidence is missing %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxRepositoryTestBytes {
		return nil, fmt.Errorf("verified repository test evidence %s has an invalid size or type", name)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, 0) >= 0 || secretValue.Match(data) {
		return nil, fmt.Errorf("verified repository test evidence %s contains an unsafe value", name)
	}
	return data, nil
}

func repositoryTestFileRecord(name string, data []byte) File {
	digestValue := sha256.Sum256(data)
	return File{
		Path: name, SourcePath: ".visual-hive/" + name,
		SHA256: hex.EncodeToString(digestValue[:]), Size: int64(len(data)), MediaType: "text/plain",
	}
}

func truncateRepositoryTestCommand(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}
