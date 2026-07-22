package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const maxBaselineCandidateBytes = 20 << 20

type baselineReport struct {
	Status  string `json:"status"`
	Summary struct {
		MissingBaselines int `json:"missingBaselines"`
		VisualDiffs      int `json:"visualDiffs"`
		ConsoleErrors    int `json:"consoleErrors"`
		PageErrors       int `json:"pageErrors"`
		FlowStepsFailed  int `json:"flowStepsFailed"`
	} `json:"summary"`
	VerdictSummary struct {
		VisualHiveVerdict string   `json:"visualHiveVerdict"`
		FailedBecause     []string `json:"failedBecause"`
	} `json:"verdictSummary"`
	VerdictContributions []struct {
		Kind   string `json:"kind"`
		Status string `json:"status"`
		Gating bool   `json:"gating"`
	} `json:"verdictContributions"`
	Results []struct {
		ScreenshotAssertions []struct {
			ContractID     string `json:"contractId"`
			ScreenshotName string `json:"screenshotName"`
			Name           string `json:"name"`
			Route          string `json:"route"`
			Viewport       string `json:"viewport"`
			Status         string `json:"status"`
			BaselinePath   string `json:"baselinePath"`
			ActualPath     string `json:"actualPath"`
		} `json:"screenshotAssertions"`
	} `json:"results"`
}

// DetectBaselineReview recognizes the one validation failure that a repair
// worker may safely hold for human review: a deterministic Visual Hive verdict
// blocked exclusively by missing screenshot baselines. It never copies or
// approves a baseline.
func DetectBaselineReview(repositoryRoot string) (*BaselineReview, bool, error) {
	return readBaselineReview(repositoryRoot, "local_validation")
}

// ReadHostedBaselineReview reads an already provenance-verified PR artifact.
// The caller must bind the artifact to the repository and exact PR head before
// invoking this function. The returned images remain untrusted review inputs.
func ReadHostedBaselineReview(artifactRoot string) (*BaselineReview, bool, error) {
	return readBaselineReview(artifactRoot, "github_actions")
}

func readBaselineReview(root, source string) (*BaselineReview, bool, error) {
	reportPath, err := findUniqueReport(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, false, fmt.Errorf("read Visual Hive report: %w", err)
	}
	var report baselineReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, false, fmt.Errorf("parse Visual Hive report: %w", err)
	}
	if report.Status != "failed" || report.VerdictSummary.VisualHiveVerdict != "blocked" || len(report.VerdictSummary.FailedBecause) != 0 {
		return nil, false, nil
	}
	if report.Summary.VisualDiffs != 0 || report.Summary.ConsoleErrors != 0 || report.Summary.PageErrors != 0 || report.Summary.FlowStepsFailed != 0 {
		return nil, false, nil
	}
	missingContribution := false
	for _, contribution := range report.VerdictContributions {
		if !contribution.Gating || contribution.Status == "passed" {
			continue
		}
		if contribution.Status != "blocked" {
			return nil, false, nil
		}
		switch contribution.Kind {
		case "deterministic_run", "contract_result":
		case "missing_baseline":
			missingContribution = true
		default:
			return nil, false, nil
		}
	}
	if !missingContribution {
		return nil, false, nil
	}
	candidates := []BaselineCandidate{}
	for _, result := range report.Results {
		for _, screenshot := range result.ScreenshotAssertions {
			switch screenshot.Status {
			case "failed":
				return nil, false, nil
			case "missing_baseline":
				baselinePath, platform, pathErr := safeBaselinePath(screenshot.BaselinePath)
				if pathErr != nil {
					return nil, false, pathErr
				}
				actualPath, locateErr := locateCandidate(root, screenshot.ActualPath)
				if locateErr != nil {
					return nil, false, locateErr
				}
				digest, bytes, inspectErr := inspectPNG(root, actualPath)
				if inspectErr != nil {
					return nil, false, inspectErr
				}
				name := strings.TrimSpace(screenshot.ScreenshotName)
				if name == "" {
					name = strings.TrimSpace(screenshot.Name)
				}
				if strings.TrimSpace(screenshot.ContractID) == "" || name == "" {
					return nil, false, fmt.Errorf("missing-baseline candidate lacks contract or screenshot identity")
				}
				candidates = append(candidates, BaselineCandidate{
					ContractID: screenshot.ContractID, ScreenshotName: name, Route: screenshot.Route, Viewport: screenshot.Viewport,
					Platform: platform, ActualPath: actualPath, BaselinePath: baselinePath, SHA256: digest, Bytes: bytes, Source: source,
				})
			}
		}
	}
	if len(candidates) == 0 || len(candidates) != report.Summary.MissingBaselines {
		return nil, false, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].BaselinePath < candidates[j].BaselinePath })
	return &BaselineReview{Status: BaselineReviewCandidateReady, Candidates: candidates}, true, nil
}

func findUniqueReport(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	matches := []string{}
	err = filepath.WalkDir(root, func(candidate string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !strings.EqualFold(entry.Name(), "report.json") {
			return nil
		}
		relative, relErr := filepath.Rel(root, candidate)
		if relErr != nil {
			return relErr
		}
		normalized := filepath.ToSlash(relative)
		if normalized == "report.json" || normalized == ".visual-hive/report.json" {
			matches = append(matches, candidate)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", os.ErrNotExist
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected one .visual-hive/report.json, found %d", len(matches))
	}
	return matches[0], nil
}

func safeBaselinePath(value string) (string, string, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	lower := strings.ToLower(normalized)
	const marker = "visual-hive.baselines/"
	index := strings.Index(lower, marker)
	if index < 0 {
		return "", "", fmt.Errorf("baseline candidate path is outside visual-hive.baselines")
	}
	relative := path.Clean(normalized[index:])
	if relative == "." || strings.HasPrefix(relative, "../") || strings.Contains(relative, "/../") || !strings.HasSuffix(strings.ToLower(relative), ".png") {
		return "", "", fmt.Errorf("invalid baseline candidate path %q", relative)
	}
	parts := strings.Split(relative, "/")
	if len(parts) < 3 || parts[0] != "visual-hive.baselines" {
		return "", "", fmt.Errorf("invalid baseline candidate path %q", relative)
	}
	platform := strings.ToLower(parts[1])
	switch platform {
	case "linux", "win32", "darwin":
	default:
		return "", "", fmt.Errorf("unsupported baseline platform %q", platform)
	}
	return relative, platform, nil
}

func locateCandidate(root, reported string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(reported) {
		candidate := filepath.Clean(reported)
		if insideRoot(root, candidate) {
			if _, statErr := os.Lstat(candidate); statErr == nil {
				return candidate, nil
			}
		}
	}
	normalized := strings.ReplaceAll(strings.TrimSpace(reported), "\\", "/")
	const marker = ".visual-hive/artifacts/screenshots/"
	index := strings.Index(strings.ToLower(normalized), marker)
	if index < 0 {
		return "", fmt.Errorf("actual screenshot path is outside .visual-hive/artifacts/screenshots")
	}
	relative := path.Clean(normalized[index:])
	if strings.HasPrefix(relative, "../") || strings.Contains(relative, "/../") {
		return "", fmt.Errorf("invalid actual screenshot path")
	}
	relatives := []string{relative, strings.TrimPrefix(relative, ".visual-hive/")}
	for _, candidateRelative := range relatives {
		direct := filepath.Join(root, filepath.FromSlash(candidateRelative))
		if _, statErr := os.Lstat(direct); statErr == nil {
			return direct, nil
		}
	}
	matches := []string{}
	err = filepath.WalkDir(root, func(candidate string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			normalizedCandidate := filepath.ToSlash(candidate)
			for _, candidateRelative := range relatives {
				if strings.HasSuffix(normalizedCandidate, candidateRelative) {
					matches = append(matches, candidate)
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected one actual screenshot for %q, found %d", relative, len(matches))
	}
	return matches[0], nil
}

func inspectPNG(root, candidate string) (string, int64, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", 0, err
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil || !insideRoot(root, candidate) {
		return "", 0, fmt.Errorf("baseline candidate is outside the evidence root")
	}
	info, err := os.Lstat(candidate)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 8 || info.Size() > maxBaselineCandidateBytes {
		return "", 0, fmt.Errorf("baseline candidate is not a bounded regular PNG")
	}
	data, err := os.ReadFile(candidate)
	if err != nil {
		return "", 0, err
	}
	pngSignature := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	if len(data) < len(pngSignature) || string(data[:len(pngSignature)]) != string(pngSignature) {
		return "", 0, fmt.Errorf("baseline candidate is not a PNG")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), info.Size(), nil
}

func insideRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
