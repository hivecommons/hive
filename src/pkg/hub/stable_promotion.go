package hub

import (
	"context"
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
	// BuiltAt is the committer date of SHA (RFC3339 UTC). For the stable row
	// this used to be published as "promoted_at", which it never was: the
	// promote-stable workflow moves the :stable tag hours after the commit
	// lands, so operators read a commit time as a promotion time.
	BuiltAt string `json:"built_at,omitempty"`
}

// channelBuild folds one ChannelTarget snapshot into a StablePromotionBuild.
// The live revision label can run ahead of the cached ChannelTarget (the two
// caches refresh independently). When it does, the target's digest and commit
// date describe the OLDER build, so they are dropped rather than published next
// to the newer SHA as if they were one record.
func channelBuild(t ChannelTarget, liveSHA string) StablePromotionBuild {
	b := StablePromotionBuild{SHA: t.SHA, Digest: t.Digest, BuiltAt: t.CommittedAt}
	if liveSHA == "" || sameCommit(liveSHA, t.SHA) {
		return b
	}
	b.SHA = liveSHA
	b.Digest = ""
	b.BuiltAt = ""
	return b
}

type MaintainedHiveSummary struct {
	ID       string `json:"id"`
	ImageRef string `json:"image_ref,omitempty"`
	GitHash  string `json:"git_hash,omitempty"`
	// Generation is the docker.yml run number of the build the hive is
	// running (0 when unknown), so the promotion gate can accept a hive on a
	// later candidate as smoke evidence for an older eligible build.
	Generation       int    `json:"generation,omitempty"`
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

	// LastRunsFetch* report the most recent GitHub docker.yml runs fetch;
	// LastRunsFetchOK is null until a fetch has run (#11684).
	LastRunsFetchOK    *bool   `json:"last_runs_fetch_ok"`
	LastRunsFetchError string  `json:"last_runs_fetch_error,omitempty"`
	LastRunsFetchAt    *string `json:"last_runs_fetch_at"`
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
			status.Candidate = channelBuild(t, channelRevisionSHA(ReleaseChannelCandidate, s.logger))
			status.Candidate.Generation = ghcrTagGeneration(ghcrRepoSpoke, ReleaseChannelCandidate, s.logger)
		case ReleaseChannelStable:
			status.Stable = channelBuild(t, channelRevisionSHA(ReleaseChannelStable, s.logger))
			status.Stable.Generation = ghcrTagGeneration(ghcrRepoSpoke, ReleaseChannelStable, s.logger)
		}
	}
	if build, eligibleAt := stablePromotionEligibleBuild(status.Stable.Generation, status.Stable.BuiltAt, time.Now().UTC(), s.logger); eligibleAt != "" {
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
	status.LastRunsFetchOK, status.LastRunsFetchError, status.LastRunsFetchAt = stablePromotionRunsFetchState()
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

// stablePromotionRunsFetchTimeout bounds the whole runs fetch (every page and
// any GitHub pacing wait in front of it). A 5s budget per page timed out on
// every cycle once pacing delayed a slot (#11684).
var stablePromotionRunsFetchTimeout = 30 * time.Second

// stablePromotionRunsCutoff marks where paging can stop: runs arrive
// newest-first, so once a page reaches the current stable build (by generation
// or by its commit time) older pages cannot hold an eligible build.
type stablePromotionRunsCutoff struct {
	generation int
	builtAt    time.Time
}

func newStablePromotionRunsCutoff(generation int, builtAt string) stablePromotionRunsCutoff {
	c := stablePromotionRunsCutoff{generation: generation}
	if t, err := time.Parse(time.RFC3339, builtAt); err == nil {
		c.builtAt = t.UTC()
	}
	return c
}

func (c stablePromotionRunsCutoff) reached(run stablePromotionWorkflowRun) bool {
	if c.generation > 0 && run.RunNumber > 0 && run.RunNumber <= c.generation {
		return true
	}
	if c.builtAt.IsZero() {
		return false
	}
	created, err := time.Parse(time.RFC3339, run.CreatedAt)
	return err == nil && created.Before(c.builtAt)
}

// stablePromotionFetchRuns lists recent docker.yml runs on v5, newest-first,
// stopping at the page that reaches cutoff. Any error fails the whole fetch:
// a truncated list must not drive eligibility. Callers go through
// stablePromotionRuns, which caches the answer and keeps serving the last good
// list when a refresh fails.
var stablePromotionFetchRuns = func(cutoff stablePromotionRunsCutoff, _ *slog.Logger) ([]stablePromotionWorkflowRun, error) {
	client := hubGitHubHTTPClient()
	ctx, cancel := context.WithTimeout(hubGitHubCallerContext(context.Background(), "stable_promotion"), stablePromotionRunsFetchTimeout)
	defer cancel()
	runs := []stablePromotionWorkflowRun{}
	for page := 1; page <= stablePromotionRunPages; page++ {
		runsURL := fmt.Sprintf("%s/repos/hivecommons/hive/actions/workflows/docker.yml/runs?branch=v5&per_page=%d&page=%d", githubAPIBase, stablePromotionRunsPerPage, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, runsURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		authGitHubRequest(req)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		var body struct {
			WorkflowRuns []stablePromotionWorkflowRun `json:"workflow_runs"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, ghcrManifestMaxBytes)).Decode(&body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("page %d: GitHub returned status %d", page, resp.StatusCode)
		}
		if err != nil {
			return nil, fmt.Errorf("page %d: undecodable response: %w", page, err)
		}
		if len(body.WorkflowRuns) == 0 {
			break
		}
		runs = append(runs, body.WorkflowRuns...)
		if len(body.WorkflowRuns) < stablePromotionRunsPerPage || cutoff.reached(body.WorkflowRuns[len(body.WorkflowRuns)-1]) {
			break
		}
	}
	return runs, nil
}

// stablePromotionEligibleBuild returns the newest verified build newer than
// stableGeneration that has already soaked (eligible now), or else the
// verified build that will finish soaking soonest and when. Runs and GHCR
// verifications are cached, and at most stablePromotionMaxVerifiedRuns runs
// are verified per call.
func stablePromotionEligibleBuild(stableGeneration int, stableBuiltAt string, now time.Time, logger *slog.Logger) (StablePromotionBuild, string) {
	type pending struct {
		run      stablePromotionWorkflowRun
		built    time.Time
		eligible time.Time
	}
	var soaked, soaking []pending
	for _, run := range stablePromotionRuns(newStablePromotionRunsCutoff(stableGeneration, stableBuiltAt), logger) {
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
	for _, group := range [][]pending{soaked, soaking} {
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
			// Report when the build actually crossed the soak line, also for
			// builds that already soaked. Returning now() made eligible_at
			// drift forward on every poll and hid a stalled promotion
			// workflow behind a perpetual "due now" (#10042, #10187).
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

// stableNextPromotion is the hub's ETA for the next promotion into the stable
// channel (#10256), from the same serialized per-build soak rule the
// release-channel block's eligible_at uses, so the spoke and hub card cannot
// disagree. The status distinguishes "none queued" from "unknown".
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

func (s *HubServer) stableNextPromotionFromCachedTargets(targets []ChannelTarget) (string, string) {
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
	if candidate.Digest == stable.Digest || sameCommit(candidate.SHA, stable.SHA) {
		return "", stableNextUpdateStatusNone
	}
	if eligible := stablePromotionEligibleAt(candidate.CommittedAt); eligible != "" {
		return eligible, stableNextUpdateStatusQueued
	}
	return "", stableNextUpdateStatusUnknown
}

func channelTargetsWithStablePromotionStatus(targets []ChannelTarget, status StablePromotionStatus) []ChannelTarget {
	out := append([]ChannelTarget(nil), targets...)
	for i := range out {
		if out[i].Channel == ReleaseChannelStable {
			st := status
			out[i].StablePromotion = &st
			break
		}
	}
	return out
}

func (s *HubServer) stablePromotionStatusFromCachedTargets(targets []ChannelTarget) StablePromotionStatus {
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
			status.Candidate = StablePromotionBuild{SHA: t.SHA, Digest: t.Digest, BuiltAt: t.CommittedAt}
		case ReleaseChannelStable:
			status.Stable = StablePromotionBuild{SHA: t.SHA, Digest: t.Digest, BuiltAt: t.CommittedAt}
		}
	}
	status.MaintainedHives = s.maintainedCandidateHivesCached(status.Candidate)
	return status
}

func (s *HubServer) maintainedCandidateHives(candidate StablePromotionBuild) []MaintainedHiveSummary {
	return s.maintainedCandidateHivesWithGeneration(candidate, true)
}

func (s *HubServer) maintainedCandidateHivesCached(candidate StablePromotionBuild) []MaintainedHiveSummary {
	return s.maintainedCandidateHivesWithGeneration(candidate, false)
}

func (s *HubServer) maintainedCandidateHivesWithGeneration(candidate StablePromotionBuild, resolveGeneration bool) []MaintainedHiveSummary {
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
		// Keep every candidate-channel hive, not only those on the exact
		// current candidate: on a busy merge day candidate moves faster than
		// spokes auto-update, so requiring an exact match left the smoke
		// gate with no evidence at all (#10042). The hive's build generation
		// lets the promotion script decide whether it is new enough.
		generation := 0
		if resolveGeneration && h.GitHash != "" {
			generation = ghcrTagGeneration(ghcrRepoSpoke, shortSHA(h.GitHash), s.logger)
		}
		if generation == 0 && candidate.SHA != "" && h.GitHash != "" && sameCommit(h.GitHash, candidate.SHA) {
			generation = candidate.Generation
		}
		crashRestarts := recentAgentRestarts(h.Agents)
		out = append(out, MaintainedHiveSummary{
			ID:               h.ID,
			ImageRef:         h.ImageRef,
			GitHash:          h.GitHash,
			Generation:       generation,
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
