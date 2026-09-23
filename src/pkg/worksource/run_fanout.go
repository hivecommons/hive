package worksource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	RunDocumentStatusFinal   = "final"
	RunDocumentStatusSkipped = "skipped"
	RunWaitingOnHuman        = "human"
)

type RunRepositoryStatus struct {
	Repo           string   `json:"repo"`
	Role           string   `json:"role,omitempty"`
	DocumentStatus string   `json:"document_status,omitempty"`
	Skipped        bool     `json:"skipped,omitempty"`
	Failed         bool     `json:"failed,omitempty"`
	PRs            []string `json:"prs,omitempty"`
}

type RunWaveStatus struct {
	Wave         int                   `json:"wave"`
	Repositories []RunRepositoryStatus `json:"repositories"`
}

type SpektacularRunStatus struct {
	RunKey string          `json:"run"`
	Title  string          `json:"title,omitempty"`
	Waves  []RunWaveStatus `json:"waves"`
}

type RunImplementationLease struct {
	RunKey string
	Wave   int
	Repo   string
	Role   string
	Stage  string
}

type RunRepoOverlap struct {
	Repo   string
	Hives  []string
	Reason string
}

type RunFanoutResult struct {
	Created        []RunImplementationLease
	Blocked        bool
	Refused        bool
	WaitingOn      string
	ForwardFix     bool
	RefusalMessage string
}

type RunFanoutLeaseCreator interface {
	CreateImplementationLease(ctx context.Context, lease RunImplementationLease) error
}

type RunFanoutOverlapIndex interface {
	Overlaps(ctx context.Context, repos []string) ([]RunRepoOverlap, error)
}

type RunFanoutAuditSink interface {
	RecordRunFanoutRefusal(ctx context.Context, runKey, reason string)
}

type RunFanoutRunner struct {
	StatusURL string
	Client    *http.Client
	Leases    RunFanoutLeaseCreator
	Overlaps  RunFanoutOverlapIndex
	Audit     RunFanoutAuditSink
}

func (r RunFanoutRunner) FanOutWave(ctx context.Context, runKey string, wave int) (RunFanoutResult, error) {
	if r.Leases == nil {
		return RunFanoutResult{}, fmt.Errorf("worksource/run: fan-out lease creator is nil")
	}
	status, err := r.FetchStatus(ctx)
	if err != nil {
		return RunFanoutResult{}, err
	}
	if runKey = strings.TrimSpace(runKey); runKey == "" {
		runKey = status.RunKey
	}
	if wave <= 0 {
		wave = firstRunWave(status)
	}
	result := forwardFixResult(status, wave)
	current, ok := statusWave(status, wave)
	if !ok {
		result.Blocked = true
		return result, nil
	}
	if !previousRunWaveFinal(status, wave) {
		result.Blocked = true
		return result, nil
	}
	candidates := runWaveLeaseCandidates(runKey, wave, current)
	repos := runLeaseRepos(candidates)
	if r.Overlaps != nil {
		overlaps, err := r.Overlaps.Overlaps(ctx, repos)
		if err != nil {
			return result, fmt.Errorf("worksource/run: overlap check: %w", err)
		}
		if len(overlaps) > 0 {
			msg := runOverlapMessage(overlaps)
			result.Refused = true
			result.RefusalMessage = msg
			if r.Audit != nil {
				r.Audit.RecordRunFanoutRefusal(ctx, runKey, msg)
			}
			return result, fmt.Errorf("worksource/run: constellation overlap: %s", msg)
		}
	}
	for _, lease := range candidates {
		if err := r.Leases.CreateImplementationLease(ctx, lease); err != nil {
			return result, fmt.Errorf("worksource/run: create implementation lease for %s wave %d: %w", lease.Repo, wave, err)
		}
		result.Created = append(result.Created, lease)
	}
	return result, nil
}

func (r RunFanoutRunner) FetchStatus(ctx context.Context) (SpektacularRunStatus, error) {
	if strings.TrimSpace(r.StatusURL) == "" {
		return SpektacularRunStatus{}, fmt.Errorf("worksource/run: status URL is required")
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.StatusURL, nil)
	if err != nil {
		return SpektacularRunStatus{}, fmt.Errorf("worksource/run: status request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return SpektacularRunStatus{}, fmt.Errorf("worksource/run: status fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return SpektacularRunStatus{}, fmt.Errorf("worksource/run: status fetch returned %s", resp.Status)
	}
	var status SpektacularRunStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return SpektacularRunStatus{}, fmt.Errorf("worksource/run: status decode: %w", err)
	}
	return status, nil
}

func firstRunWave(status SpektacularRunStatus) int {
	first := 0
	for _, wave := range status.Waves {
		if wave.Wave > 0 && (first == 0 || wave.Wave < first) {
			first = wave.Wave
		}
	}
	return first
}

func statusWave(status SpektacularRunStatus, n int) (RunWaveStatus, bool) {
	for _, wave := range status.Waves {
		if wave.Wave == n {
			return wave, true
		}
	}
	return RunWaveStatus{}, false
}

func previousRunWaveFinal(status SpektacularRunStatus, wave int) bool {
	prev := 0
	for _, candidate := range status.Waves {
		if candidate.Wave < wave && candidate.Wave > prev {
			prev = candidate.Wave
		}
	}
	if prev == 0 {
		return true
	}
	prior, ok := statusWave(status, prev)
	if !ok {
		return true
	}
	for _, repo := range prior.Repositories {
		if repo.Skipped || strings.EqualFold(repo.DocumentStatus, RunDocumentStatusSkipped) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(repo.DocumentStatus), RunDocumentStatusFinal) {
			return false
		}
	}
	return true
}

func forwardFixResult(status SpektacularRunStatus, beforeWave int) RunFanoutResult {
	for _, wave := range status.Waves {
		if beforeWave > 0 && wave.Wave > beforeWave {
			continue
		}
		for _, repo := range wave.Repositories {
			documentStatus := strings.ToLower(strings.TrimSpace(repo.DocumentStatus))
			if repo.Failed || documentStatus == "failed" || documentStatus == "error" {
				return RunFanoutResult{WaitingOn: RunWaitingOnHuman, ForwardFix: true}
			}
		}
	}
	return RunFanoutResult{}
}

func runWaveLeaseCandidates(runKey string, waveNumber int, wave RunWaveStatus) []RunImplementationLease {
	seen := map[string]bool{}
	var leases []RunImplementationLease
	for _, repo := range wave.Repositories {
		name := strings.TrimSpace(repo.Repo)
		key := strings.ToLower(name)
		documentStatus := strings.ToLower(strings.TrimSpace(repo.DocumentStatus))
		if name == "" || seen[key] || repo.Failed || repo.Skipped ||
			documentStatus == RunDocumentStatusFinal || documentStatus == RunDocumentStatusSkipped {
			continue
		}
		seen[key] = true
		leases = append(leases, RunImplementationLease{
			RunKey: runKey,
			Wave:   waveNumber,
			Repo:   name,
			Role:   strings.TrimSpace(repo.Role),
			Stage:  RunStageImplement,
		})
	}
	return leases
}

func runLeaseRepos(leases []RunImplementationLease) []string {
	repos := make([]string, 0, len(leases))
	for _, lease := range leases {
		repos = append(repos, lease.Repo)
	}
	sort.Strings(repos)
	return repos
}

func runOverlapMessage(overlaps []RunRepoOverlap) string {
	parts := make([]string, 0, len(overlaps))
	for _, overlap := range overlaps {
		if overlap.Reason != "" {
			parts = append(parts, overlap.Reason)
			continue
		}
		repo := strings.TrimSpace(overlap.Repo)
		if repo == "" {
			repo = "repository"
		}
		if len(overlap.Hives) > 0 {
			parts = append(parts, repo+" claimed by "+strings.Join(overlap.Hives, ", "))
		} else {
			parts = append(parts, repo+" has overlapping spoke claims")
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
