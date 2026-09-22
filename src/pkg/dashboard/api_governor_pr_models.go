package dashboard

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	governorPRModelsWindow7d  = "7d"
	governorPRModelsWindow30d = "30d"
	governorPRModelsWindowAll = "all"

	governorPRModelsDays7  = 7
	governorPRModelsDays30 = 30
)

type governorPRModelsResponse struct {
	Window  string                  `json:"window"`
	Total   int                     `json:"total"`
	Buckets []governorPRModelBucket `json:"buckets"`
	Unknown int                     `json:"unknown"`
}

type governorPRModelBucket struct {
	Model          string                       `json:"model"`
	Backend        string                       `json:"backend"`
	PRs            int                          `json:"prs"`
	Merged         int                          `json:"merged"`
	ClosedUnmerged int                          `json:"closed_unmerged"`
	Open           int                          `json:"open"`
	Agents         []governorPRModelAgentBucket `json:"agents,omitempty"`
}

type governorPRModelAgentBucket struct {
	Agent          string `json:"agent"`
	Backend        string `json:"backend"`
	PRs            int    `json:"prs"`
	Merged         int    `json:"merged"`
	ClosedUnmerged int    `json:"closed_unmerged"`
	Open           int    `json:"open"`
}

func (s *Server) handleGovernorPRModels(w http.ResponseWriter, r *http.Request) {
	window := normalizeGovernorPRModelsWindow(r.URL.Query().Get("window"))
	actionable := s.lastActionableForPRModels()
	resp := aggregateGovernorPRModels(actionable.PRs.Attributed, window, time.Now())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) lastActionableForPRModels() *ghpkg.ActionableResult {
	if s == nil || s.deps == nil || s.deps.Scheduler == nil {
		return &ghpkg.ActionableResult{}
	}
	if a := s.deps.Scheduler.GetLastActionable(); a != nil {
		return a
	}
	return &ghpkg.ActionableResult{}
}

func normalizeGovernorPRModelsWindow(window string) string {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case governorPRModelsWindow30d:
		return governorPRModelsWindow30d
	case governorPRModelsWindowAll:
		return governorPRModelsWindowAll
	default:
		return governorPRModelsWindow7d
	}
}

func aggregateGovernorPRModels(prs []ghpkg.PullRequest, window string, now time.Time) governorPRModelsResponse {
	window = normalizeGovernorPRModelsWindow(window)
	cutoff := time.Time{}
	switch window {
	case governorPRModelsWindow7d:
		cutoff = now.AddDate(0, 0, -governorPRModelsDays7)
	case governorPRModelsWindow30d:
		cutoff = now.AddDate(0, 0, -governorPRModelsDays30)
	}

	type bucketState struct {
		governorPRModelBucket
		agents map[string]*governorPRModelAgentBucket
	}
	buckets := map[string]*bucketState{}
	resp := governorPRModelsResponse{Window: window}
	for _, pr := range prs {
		if !pr.HiveAttributed {
			continue
		}
		if !cutoff.IsZero() && pr.CreatedAt.Before(cutoff) {
			continue
		}
		model := ghpkg.NormalizeAttributionModel(pr.HiveModel)
		backend := ghpkg.NormalizeAttributionValue(pr.HiveBackend)
		agent := ghpkg.NormalizeAttributionValue(pr.HiveAgent)
		if agent == "unknown" {
			agent = ""
		}
		key := model + "\x00" + backend
		b := buckets[key]
		if b == nil {
			b = &bucketState{
				governorPRModelBucket: governorPRModelBucket{Model: model, Backend: backend},
				agents:                map[string]*governorPRModelAgentBucket{},
			}
			buckets[key] = b
		}
		outcome := governorPRModelOutcome(pr)
		addOutcome(&b.PRs, &b.Merged, &b.ClosedUnmerged, &b.Open, outcome)
		agentKey := agent + "\x00" + backend
		ab := b.agents[agentKey]
		if ab == nil {
			ab = &governorPRModelAgentBucket{Agent: agent, Backend: backend}
			b.agents[agentKey] = ab
		}
		addOutcome(&ab.PRs, &ab.Merged, &ab.ClosedUnmerged, &ab.Open, outcome)
		resp.Total++
		if model == "unknown" {
			resp.Unknown++
		}
	}

	resp.Buckets = make([]governorPRModelBucket, 0, len(buckets))
	for _, b := range buckets {
		for _, ab := range b.agents {
			b.Agents = append(b.Agents, *ab)
		}
		sort.Slice(b.Agents, func(i, j int) bool {
			if b.Agents[i].PRs != b.Agents[j].PRs {
				return b.Agents[i].PRs > b.Agents[j].PRs
			}
			return b.Agents[i].Agent < b.Agents[j].Agent
		})
		resp.Buckets = append(resp.Buckets, b.governorPRModelBucket)
	}
	sort.Slice(resp.Buckets, func(i, j int) bool {
		if resp.Buckets[i].PRs != resp.Buckets[j].PRs {
			return resp.Buckets[i].PRs > resp.Buckets[j].PRs
		}
		if resp.Buckets[i].Model != resp.Buckets[j].Model {
			return resp.Buckets[i].Model < resp.Buckets[j].Model
		}
		return resp.Buckets[i].Backend < resp.Buckets[j].Backend
	})
	return resp
}

func governorPRModelOutcome(pr ghpkg.PullRequest) string {
	if !pr.MergedAt.IsZero() {
		return "merged"
	}
	if strings.EqualFold(pr.State, "closed") || !pr.ClosedAt.IsZero() {
		return "closed_unmerged"
	}
	return "open"
}

func addOutcome(total, merged, closedUnmerged, open *int, outcome string) {
	(*total)++
	switch outcome {
	case "merged":
		(*merged)++
	case "closed_unmerged":
		(*closedUnmerged)++
	default:
		(*open)++
	}
}
