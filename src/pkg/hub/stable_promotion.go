package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var stablePromotionPath = "/data/saas/stable-promotion.json"

const stablePromotionFileMode = 0o644
const stablePromotionSoakHours = 24

type StablePromotionState struct {
	AutoPromote bool   `json:"auto_promote"`
	UpdatedBy   string `json:"updated_by,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

type StablePromotionBuild struct {
	SHA        string `json:"sha,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Generation int    `json:"generation,omitempty"`
	BuiltAt    string `json:"built_at,omitempty"`
	PromotedAt string `json:"promoted_at,omitempty"`
}

type MaintainedHiveSummary struct {
	ID               string `json:"id"`
	ImageRef         string `json:"image_ref,omitempty"`
	GitHash          string `json:"git_hash,omitempty"`
	LastHeartbeatAt  string `json:"last_heartbeat_at,omitempty"`
	Healthy          bool   `json:"healthy"`
	CrashRestarts24h int    `json:"crash_restarts_24h"`
}

type StablePromotionStatus struct {
	AutoPromote     bool                    `json:"auto_promote"`
	PausedBy        string                  `json:"paused_by"`
	PausedAt        *string                 `json:"paused_at"`
	SoakHours       int                     `json:"soak_hours"`
	Candidate       StablePromotionBuild    `json:"candidate"`
	Stable          StablePromotionBuild    `json:"stable"`
	EligibleAt      *string                 `json:"eligible_at"`
	EligibleBuild   *StablePromotionBuild   `json:"eligible_build,omitempty"`
	MaintainedHives []MaintainedHiveSummary `json:"maintained_hives,omitempty"`
}

var stablePromotionMu sync.Mutex

func defaultStablePromotionState() StablePromotionState {
	return StablePromotionState{AutoPromote: true}
}

func loadStablePromotionState() StablePromotionState {
	stablePromotionMu.Lock()
	defer stablePromotionMu.Unlock()
	data, err := os.ReadFile(stablePromotionPath)
	if err != nil {
		return defaultStablePromotionState()
	}
	var st StablePromotionState
	if err := json.Unmarshal(data, &st); err != nil {
		return defaultStablePromotionState()
	}
	return st
}

func saveStablePromotionState(st StablePromotionState) error {
	stablePromotionMu.Lock()
	defer stablePromotionMu.Unlock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(stablePromotionPath), 0o755); err != nil {
		return err
	}
	tmpPath := stablePromotionPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, stablePromotionFileMode); err != nil {
		return err
	}
	return os.Rename(tmpPath, stablePromotionPath)
}

func (s *HubServer) stablePromotionStatus(targets []ChannelTarget) StablePromotionStatus {
	state := loadStablePromotionState()
	status := StablePromotionStatus{
		AutoPromote: state.AutoPromote,
		SoakHours:   stablePromotionSoakHours,
	}
	if !state.AutoPromote {
		status.PausedBy = state.UpdatedBy
		if state.UpdatedAt != "" {
			pausedAt := state.UpdatedAt
			status.PausedAt = &pausedAt
		}
	}
	for _, t := range targets {
		switch t.Channel {
		case ReleaseChannelCandidate:
			sha := t.SHA
			if imageSHA := channelRevisionSHA(ReleaseChannelCandidate, s.logger); imageSHA != "" {
				sha = imageSHA
			}
			status.Candidate = StablePromotionBuild{SHA: sha, Digest: t.Digest, Generation: ghcrTagGeneration(ghcrRepoSpoke, ReleaseChannelCandidate, s.logger), BuiltAt: t.CommittedAt}
		case ReleaseChannelStable:
			sha := t.SHA
			if imageSHA := channelRevisionSHA(ReleaseChannelStable, s.logger); imageSHA != "" {
				sha = imageSHA
			}
			status.Stable = StablePromotionBuild{SHA: sha, Digest: t.Digest, Generation: ghcrTagGeneration(ghcrRepoSpoke, ReleaseChannelStable, s.logger), PromotedAt: t.CommittedAt}
		}
	}
	if build, eligibleAt := stablePromotionEligibleBuild(status.Stable.Generation, time.Now().UTC(), s.logger); eligibleAt != "" {
		status.EligibleAt = &eligibleAt
		if build.Generation != 0 || build.SHA != "" {
			status.EligibleBuild = &build
		}
	} else if status.Candidate.Generation > status.Stable.Generation {
		if eligible := stablePromotionEligibleAt(status.Candidate.BuiltAt); eligible != "" {
			status.EligibleAt = &eligible
			candidate := status.Candidate
			status.EligibleBuild = &candidate
		}
	}
	status.MaintainedHives = s.maintainedCandidateHives(status.Candidate)
	return status
}

type stablePromotionWorkflowRun struct {
	RunNumber  int    `json:"run_number"`
	HeadSHA    string `json:"head_sha"`
	UpdatedAt  string `json:"updated_at"`
	CreatedAt  string `json:"created_at"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

const stablePromotionRunsPerPage = 100
const stablePromotionRunPages = 5

// stablePromotionFetchRuns lists recent docker.yml runs on v5. Callers go
// through stablePromotionRuns, which caches the answer.
var stablePromotionFetchRuns = func(logger *slog.Logger) []stablePromotionWorkflowRun {
	client := &http.Client{Timeout: 5 * time.Second}
	var runs []stablePromotionWorkflowRun
	for page := 1; page <= stablePromotionRunPages; page++ {
		runsURL := fmt.Sprintf("%s/repos/hivecommons/hive/actions/workflows/docker.yml/runs?branch=v5&per_page=%d&page=%d", githubAPIBase, stablePromotionRunsPerPage, page)
		req, err := http.NewRequest(http.MethodGet, runsURL, nil)
		if err != nil {
			return runs
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		authGitHubRequest(req)
		resp, err := client.Do(req)
		if err != nil {
			logger.Warn("stable promotion: GitHub runs fetch failed", "error", err)
			return runs
		}
		var body struct {
			WorkflowRuns []stablePromotionWorkflowRun `json:"workflow_runs"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, ghcrManifestMaxBytes)).Decode(&body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			logger.Warn("stable promotion: GitHub runs fetch returned non-OK", "status", resp.StatusCode)
			return runs
		}
		if err != nil {
			logger.Warn("stable promotion: GitHub runs response was not decodable", "error", err)
			return runs
		}
		if len(body.WorkflowRuns) == 0 {
			break
		}
		runs = append(runs, body.WorkflowRuns...)
		if len(body.WorkflowRuns) < stablePromotionRunsPerPage {
			break
		}
	}
	if runs == nil {
		runs = []stablePromotionWorkflowRun{}
	}
	return runs
}

// stablePromotionEligibleBuild returns the newest verified build newer than
// stableGeneration that has already soaked (eligible now), or else the
// verified build that will finish soaking soonest and when. Runs and GHCR
// verifications are cached, and at most stablePromotionMaxVerifiedRuns runs
// are verified per call.
func stablePromotionEligibleBuild(stableGeneration int, now time.Time, logger *slog.Logger) (StablePromotionBuild, string) {
	type pending struct {
		run      stablePromotionWorkflowRun
		built    time.Time
		eligible time.Time
	}
	var soaked, soaking []pending
	for _, run := range stablePromotionRuns(logger) {
		if run.Status != "" && run.Status != "completed" {
			continue
		}
		if run.Conclusion != "" && run.Conclusion != "success" {
			continue
		}
		if run.RunNumber <= stableGeneration {
			continue
		}
		builtAt := run.UpdatedAt
		if builtAt == "" {
			builtAt = run.CreatedAt
		}
		built, err := time.Parse(time.RFC3339, builtAt)
		if err != nil {
			continue
		}
		p := pending{run: run, built: built.UTC(), eligible: built.Add(time.Duration(stablePromotionSoakHours) * time.Hour).UTC()}
		if !p.eligible.After(now) {
			soaked = append(soaked, p)
		} else {
			soaking = append(soaking, p)
		}
	}
	// Runs arrive newest-first; prefer the newest soaked build.
	sort.SliceStable(soaked, func(i, j int) bool { return soaked[i].run.RunNumber > soaked[j].run.RunNumber })
	sort.SliceStable(soaking, func(i, j int) bool { return soaking[i].eligible.Before(soaking[j].eligible) })

	verified := 0
	for i, group := range [][]pending{soaked, soaking} {
		for _, p := range group {
			if verified >= stablePromotionMaxVerifiedRuns {
				return StablePromotionBuild{}, ""
			}
			verified++
			short := shortSHA(p.run.HeadSHA)
			v := stablePromotionVerifyBuild(p.run.RunNumber, short, logger)
			if !v.ok {
				continue
			}
			build := StablePromotionBuild{SHA: short, Digest: v.digest, Generation: p.run.RunNumber, BuiltAt: p.built.Format(time.RFC3339)}
			if i == 0 {
				return build, now.UTC().Format(time.RFC3339)
			}
			return build, p.eligible.Format(time.RFC3339)
		}
	}
	return StablePromotionBuild{}, ""
}

// stablePromotionEligibleAt is when a build crosses the stable channel's
// 24-hour line, or "" when builtAt is unknown.
func stablePromotionEligibleAt(builtAt string) string {
	if builtAt == "" {
		return ""
	}
	built, err := time.Parse(time.RFC3339, builtAt)
	if err != nil {
		return ""
	}
	return built.Add(time.Duration(stablePromotionSoakHours) * time.Hour).UTC().Format(time.RFC3339)
}

const (
	stableNextUpdateStatusQueued  = "queued"
	stableNextUpdateStatusNone    = "none"
	stableNextUpdateStatusPaused  = "paused"
	stableNextUpdateStatusUnknown = "unknown"
)

// stableNextPromotionAt is the hub's ETA for the next promotion into the
// stable channel (#10256), from the same serialized per-build soak rule the
// release-channel block's eligible_at uses, so the spoke and the hub card cannot
// disagree. Returns "" when no ETA is currently knowable; callers that need to
// distinguish "none queued" from "unknown" should use stableNextPromotion.
func stableNextPromotionAt(targets []ChannelTarget) string {
	at, _ := (&HubServer{logger: slog.Default()}).stableNextPromotion(targets)
	return at
}

func (s *HubServer) stableNextPromotion(targets []ChannelTarget) (string, string) {
	if !loadStablePromotionState().AutoPromote {
		return "", stableNextUpdateStatusPaused
	}
	var candidate, stable *ChannelTarget
	for i := range targets {
		switch targets[i].Channel {
		case ReleaseChannelCandidate:
			candidate = &targets[i]
		case ReleaseChannelStable:
			stable = &targets[i]
		}
	}
	if candidate == nil || stable == nil || candidate.Digest == "" || stable.Digest == "" {
		return "", stableNextUpdateStatusUnknown
	}
	status := s.stablePromotionStatus(targets)
	if status.EligibleAt != nil && *status.EligibleAt != "" {
		return *status.EligibleAt, stableNextUpdateStatusQueued
	}
	if candidate.Digest == stable.Digest || sameCommit(candidate.SHA, stable.SHA) {
		return "", stableNextUpdateStatusNone
	}
	return "", stableNextUpdateStatusUnknown
}

func (s *HubServer) channelTargetsWithStablePromotion(targets []ChannelTarget) []ChannelTarget {
	out := append([]ChannelTarget(nil), targets...)
	status := s.stablePromotionStatus(out)
	for i := range out {
		if out[i].Channel == ReleaseChannelStable {
			st := status
			out[i].StablePromotion = &st
			break
		}
	}
	return out
}

// stablePromotionFromTargets returns the status channelTargetsWithStablePromotion
// already attached to the stable row, computing it only when there is none.
func (s *HubServer) stablePromotionFromTargets(targets []ChannelTarget) StablePromotionStatus {
	for _, t := range targets {
		if t.Channel == ReleaseChannelStable && t.StablePromotion != nil {
			return *t.StablePromotion
		}
	}
	return s.stablePromotionStatus(targets)
}

func (s *HubServer) maintainedCandidateHives(candidate StablePromotionBuild) []MaintainedHiveSummary {
	s.mu.Lock()
	hives := make([]RegistryEntry, len(s.registry.Hives))
	copy(hives, s.registry.Hives)
	s.mu.Unlock()

	saasByID := make(map[string]SaaSHive)
	for _, sh := range listSaaSHives() {
		saasByID[sh.ID] = sh
	}
	now := time.Now()
	out := make([]MaintainedHiveSummary, 0)
	for _, h := range hives {
		sh := saasByID[h.ID]
		if sh.Status == statusAvailable {
			continue
		}
		channel, resolved, _ := ResolveSpokeReleaseChannel(h.ImageRef, sh.TrackedChannel)
		if !resolved || channel != ReleaseChannelCandidate {
			continue
		}
		if candidate.SHA != "" && h.GitHash != "" && !sameCommit(h.GitHash, candidate.SHA) {
			continue
		}
		crashRestarts := recentAgentRestarts(h.Agents)
		out = append(out, MaintainedHiveSummary{
			ID:               h.ID,
			ImageRef:         h.ImageRef,
			GitHash:          h.GitHash,
			LastHeartbeatAt:  h.LastHeartbeat,
			Healthy:          stablePromotionHeartbeatHealthy(h, now) && crashRestarts == 0,
			CrashRestarts24h: crashRestarts,
		})
	}
	return out
}

func recentAgentRestarts(agents []AgentSummary) int {
	total := 0
	for _, a := range agents {
		total += a.Restarts.Last24h
	}
	return total
}

func stablePromotionHeartbeatHealthy(h RegistryEntry, now time.Time) bool {
	if !h.Online || h.StatsStale || h.LastHeartbeat == "" {
		return false
	}
	seen, err := time.Parse(time.RFC3339, h.LastHeartbeat)
	if err != nil {
		return false
	}
	return now.Sub(seen) <= time.Duration(stablePromotionSoakHours)*time.Hour
}

func (s *HubServer) handleGetStablePromotion(w http.ResponseWriter, _ *http.Request) {
	targets := getChannelTargets(getDisplaySHAs(), s.logger)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.stablePromotionStatus(targets))
}

func (s *HubServer) handleSetStablePromotion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoPromote bool `json:"auto_promote"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	username := s.getRealAuthUser(r)
	eventAt := time.Now().UTC().Format(time.RFC3339)
	state := StablePromotionState{
		AutoPromote: body.AutoPromote,
		UpdatedBy:   username,
		UpdatedAt:   eventAt,
	}
	if body.AutoPromote {
		state.UpdatedBy = ""
		state.UpdatedAt = ""
	}
	if err := saveStablePromotionState(state); err != nil {
		s.logger.Error("failed to persist stable promotion state", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to persist stable promotion state")
		return
	}
	s.logger.Info("audit: stable auto-promotion toggled",
		"auto_promote", body.AutoPromote, "by", username, "at", eventAt)
	targets := getChannelTargets(getDisplaySHAs(), s.logger)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.stablePromotionStatus(targets))
}
