package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/taskmcp"
)

type dashboardTaskMCPProvider struct{ server *Server }

type taskMCPSnapshot struct {
	assign     WSTaskAssign
	labels     []string
	generation uint64
	assignedAt time.Time
}

func (s *Server) handleContributeMCP(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeTaskMCP(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	taskmcp.NewHandler(dashboardTaskMCPProvider{server: s}).ServeHTTP(w, r)
}

func (s *Server) authorizeTaskMCP(r *http.Request) bool {
	if s == nil || strings.TrimSpace(s.authToken) == "" {
		return false
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	return secureCompare(token, s.authToken)
}

func (p dashboardTaskMCPProvider) Scope(r *http.Request, args map[string]any) (taskmcp.Scope, error) {
	snap, err := p.snapshot(r, args)
	if err != nil {
		return taskmcp.Scope{}, err
	}
	return taskmcp.Scope{TaskID: snap.assign.TaskID, Repo: snap.assign.Repo, Number: snap.assign.Number}, nil
}

func (p dashboardTaskMCPProvider) TaskContext(_ context.Context, scope taskmcp.Scope) (taskmcp.TaskContextData, error) {
	snap, err := p.snapshotForScope(scope)
	if err != nil {
		return taskmcp.TaskContextData{}, err
	}
	return taskmcp.TaskContextData{
		Assignment: taskmcp.AssignmentData{
			TaskID:     snap.assign.TaskID,
			Kind:       snap.assign.Kind,
			Role:       snap.assign.Role,
			Repo:       snap.assign.Repo,
			Number:     snap.assign.Number,
			Key:        snap.assign.Key,
			SourceType: snap.assign.SourceType,
			ExternalID: snap.assign.ExternalID,
			URL:        snap.assign.URL,
			Complexity: snap.assign.Complexity,
		},
		Data:   taskmcp.ServedText{Title: snap.assign.Title},
		Labels: append([]string(nil), snap.labels...),
		Lease: taskmcp.LeaseData{
			Generation: snap.generation,
			AgeSeconds: leaseAgeSeconds(snap.assignedAt),
		},
		Policies: p.policyData(snap),
	}, nil
}

func (p dashboardTaskMCPProvider) RelatedWork(_ context.Context, scope taskmcp.Scope, page taskmcp.PageRequest) (taskmcp.RelatedWorkData, taskmcp.PageInfo, error) {
	scope, current, candidates := p.relatedWorkSnapshot(scope)
	window := time.Duration(config.DefaultTaskMCPRelatedWorkRecencyDays) * 24 * time.Hour
	if cfg := p.config(); cfg != nil {
		window = time.Duration(cfg.Hub.TaskMCPRelatedWorkRecencyDaysOrDefault()) * 24 * time.Hour
	}
	data, info := taskmcp.FilterRelatedWork(scope, current, candidates, window, time.Now(), page)
	return data, info, nil
}

func (p dashboardTaskMCPProvider) CIHealth(_ context.Context, _ taskmcp.Scope, page taskmcp.PageRequest) (taskmcp.CIHealthData, taskmcp.PageInfo, error) {
	data, info := taskmcp.PaginateChecks(nil, page)
	return data, info, nil
}

func (p dashboardTaskMCPProvider) snapshot(r *http.Request, args map[string]any) (taskMCPSnapshot, error) {
	if p.server == nil || p.server.contributeHub == nil {
		return taskMCPSnapshot{}, fmt.Errorf("%w: contribute hub unavailable", taskmcp.ErrForbidden)
	}
	taskID := strings.TrimSpace(r.Header.Get(taskmcp.HeaderTaskID))
	if taskID == "" {
		taskID, _ = args["task_id"].(string)
	}
	repo, _ := args["repo"].(string)
	number := numberArg(args["number"])
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(repo) == "" {
		return taskMCPSnapshot{}, fmt.Errorf("%w: task_id and repo are required", taskmcp.ErrForbidden)
	}
	return p.snapshotMatching(taskID, repo, number)
}

func (p dashboardTaskMCPProvider) snapshotForScope(scope taskmcp.Scope) (taskMCPSnapshot, error) {
	return p.snapshotMatching(scope.TaskID, scope.Repo, scope.Number)
}

func (p dashboardTaskMCPProvider) snapshotMatching(taskID, repo string, number int) (taskMCPSnapshot, error) {
	h := p.server.contributeHub
	h.mu.RLock()
	defer h.mu.RUnlock()
	var found *taskMCPSnapshot
	for _, c := range h.connections {
		c.mu.Lock()
		if c.currentTask != nil {
			assign := *c.currentTask
			matches := true
			if taskID != "" && assign.TaskID != taskID {
				matches = false
			}
			if strings.TrimSpace(repo) != "" && !strings.EqualFold(assign.Repo, strings.TrimSpace(repo)) {
				matches = false
			}
			if number > 0 && assign.Number != number {
				matches = false
			}
			if matches {
				snap := taskMCPSnapshot{assign: assign, labels: append([]string(nil), c.currentLabels...), generation: c.currentTaskGen, assignedAt: c.taskAssignedAt}
				found = &snap
			}
		}
		c.mu.Unlock()
		if found != nil {
			return *found, nil
		}
	}
	return taskMCPSnapshot{}, fmt.Errorf("%w: no active scoped task", taskmcp.ErrForbidden)
}

func (p dashboardTaskMCPProvider) policyData(snap taskMCPSnapshot) taskmcp.PolicyData {
	policy := taskmcp.PolicyData{CacheOnly: true, HubLaunchedOnly: true, StreamableHTTP: true, RoutePrefix: "/api/contribute/", ServedTextSchema: "data", RepoScoped: true}
	cfg := p.config()
	if cfg == nil {
		return policy
	}
	key := snap.assign.Key
	if key == "" && snap.assign.Repo != "" && snap.assign.Number > 0 {
		key = fmt.Sprintf("%s#%d", snap.assign.Repo, snap.assign.Number)
	}
	for _, held := range cfg.Hub.ContributeQueueHold {
		if held == key {
			policy.Hold.Held = true
			policy.Hold.Reason = cfg.Hub.ContributeQueueHoldReasons[key]
			break
		}
	}
	if cfg.ACMMLevel != nil {
		policy.LevelGate.ACMMLevel = *cfg.ACMMLevel
	}
	policy.LevelGate.Complexity = snap.assign.Complexity
	policy.Standby = standbyPolicyData(cfg)
	return policy
}

func (p dashboardTaskMCPProvider) config() *config.Config {
	if p.server == nil || p.server.deps == nil {
		return nil
	}
	return p.server.deps.Config
}

func standbyPolicyData(cfg *config.Config) []taskmcp.StandbyPolicyData {
	if cfg == nil {
		return nil
	}
	out := make([]taskmcp.StandbyPolicyData, 0, len(cfg.Agents))
	for name, agent := range cfg.Agents {
		if agent.Standby == nil {
			continue
		}
		floor := agent.Standby.MinModelCapability
		if floor == "" {
			floor = config.StandbyDefaultFloor
		}
		out = append(out, taskmcp.StandbyPolicyData{Lane: name, Enabled: agent.Standby.Enabled, Floor: floor, DailyCap: agent.Standby.DailyCapPerContributor})
	}
	return out
}

func (p dashboardTaskMCPProvider) relatedWorkSnapshot(scope taskmcp.Scope) (taskmcp.Scope, taskmcp.RelatedItem, []taskmcp.RelatedItem) {
	if p.server == nil || p.server.status == nil {
		return taskmcp.Scope{}, taskmcp.RelatedItem{}, nil
	}
	snap, err := p.snapshotForScope(scope)
	if err != nil {
		return taskmcp.Scope{}, taskmcp.RelatedItem{}, nil
	}
	scope = taskmcp.Scope{TaskID: snap.assign.TaskID, Repo: snap.assign.Repo, Number: snap.assign.Number}
	p.server.statusMu.RLock()
	defer p.server.statusMu.RUnlock()
	var current taskmcp.RelatedItem
	var candidates []taskmcp.RelatedItem
	for _, repo := range p.server.status.Repos {
		if !strings.EqualFold(repo.Full, scope.Repo) && !strings.EqualFold(repo.Name, scope.Repo) {
			continue
		}
		for _, raw := range repo.ActionableIssues {
			item, ok := relatedItemFromRaw("issue", repo.Full, raw)
			if !ok {
				continue
			}
			if item.Number == scope.Number {
				current = item
				continue
			}
			candidates = append(candidates, item)
		}
		for _, raw := range repo.OpenPrs {
			if item, ok := relatedItemFromRaw("pull_request", repo.Full, raw); ok {
				candidates = append(candidates, item)
			}
		}
		for _, raw := range repo.HeldIssues {
			if item, ok := relatedItemFromRaw("issue", repo.Full, raw); ok {
				candidates = append(candidates, item)
			}
		}
		for _, raw := range repo.HeldPrs {
			if item, ok := relatedItemFromRaw("pull_request", repo.Full, raw); ok {
				candidates = append(candidates, item)
			}
		}
	}
	if current.Repo == "" {
		current = taskmcp.RelatedItem{Kind: "issue", Repo: scope.Repo, Number: scope.Number}
	}
	return scope, current, candidates
}

func relatedItemFromRaw(kind, fallbackRepo string, raw any) (taskmcp.RelatedItem, bool) {
	var m map[string]any
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &m) != nil {
		return taskmcp.RelatedItem{}, false
	}
	item := taskmcp.RelatedItem{
		Kind:   kind,
		Repo:   stringFromMap(m, "repo", fallbackRepo),
		Number: intFromMap(m, "number"),
		State:  stringFromMap(m, "state", "open"),
		Author: stringFromMap(m, "author", ""),
		URL:    stringFromMap(m, "url", ""),
		Data: taskmcp.ServedText{
			Title: stringFromMap(m, "title", ""),
			Body:  stringFromMap(m, "body", ""),
		},
		Files: stringSliceFromAny(firstPresent(m, "files", "changed_files")),
	}
	if item.Repo == "" || item.Number == 0 {
		return taskmcp.RelatedItem{}, false
	}
	if merged := timeFromMap(m, "merged_at"); !merged.IsZero() {
		item.MergedAt = &merged
	}
	if updated := timeFromMap(m, "updated_at"); !updated.IsZero() {
		item.UpdatedAt = &updated
	}
	item.Reasons = stringSliceFromAny(m["reasons"])
	return item, true
}

func firstPresent(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return nil
}

func stringFromMap(m map[string]any, key, fallback string) string {
	if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func intFromMap(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

func timeFromMap(m map[string]any, key string) time.Time {
	if s, ok := m[key].(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func leaseAgeSeconds(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return int64(time.Since(t).Seconds())
}

func numberArg(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		parsed, _ := strconv.Atoi(n)
		return parsed
	default:
		return 0
	}
}
