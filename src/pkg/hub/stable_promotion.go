package hub

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
			status.Candidate = StablePromotionBuild{SHA: sha, Digest: t.Digest, BuiltAt: t.CommittedAt}
			if eligible := stablePromotionEligibleAt(t.CommittedAt); eligible != "" {
				status.EligibleAt = &eligible
			}
		case ReleaseChannelStable:
			sha := t.SHA
			if imageSHA := channelRevisionSHA(ReleaseChannelStable, s.logger); imageSHA != "" {
				sha = imageSHA
			}
			status.Stable = StablePromotionBuild{SHA: sha, Digest: t.Digest, PromotedAt: t.CommittedAt}
		}
	}
	status.MaintainedHives = s.maintainedCandidateHives(status.Candidate)
	return status
}

// stablePromotionEligibleAt is when the 24-hour lineage soak (rule 1 in
// docs/stable-soak-policy.md) is satisfied for a candidate built at builtAt,
// RFC3339 UTC, or "" when builtAt is unknown. The lineage starts at the oldest
// build after stable, which is never newer than the current candidate, so
// this is the latest the soak can complete while that lineage is pending.
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

// stableNextPromotionAt is the hub's ETA for the next promotion into the
// stable channel (#10256), from the same channel targets and soak rule the
// release-channel block's eligible_at uses, so the spoke and the hub card
// cannot disagree. Returns "" (unknown) when stable auto-promotion is
// paused, either channel is unresolved, or nothing is queued (candidate and
// stable are the same build).
func stableNextPromotionAt(targets []ChannelTarget) string {
	if !loadStablePromotionState().AutoPromote {
		return ""
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
		return ""
	}
	if candidate.Digest == stable.Digest || sameCommit(candidate.SHA, stable.SHA) {
		return ""
	}
	return stablePromotionEligibleAt(candidate.CommittedAt)
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
