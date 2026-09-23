package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/taskmcp"
)

type dashboardTaskMCPProvider struct{ server *Server }

type activeLaunchLookup interface {
	ActiveLaunches() []taskmcp.LaunchScope
}

type taskMCPSnapshot struct {
	assign     WSTaskAssign
	labels     []string
	generation uint64
	assignedAt time.Time
}

// handleContributeMCP authenticates, in order: a remote-contributor lease
// bearer, a hub-launched agent's per-launch token (#8348), and finally the
// dashboard token — the last is for the dashboard's own UI calls only; hub
// launches never receive it.
func (s *Server) handleContributeMCP(w http.ResponseWriter, r *http.Request) {
	var leaseOK bool
	r, leaseOK = s.authenticateTaskMCPLease(r)
	if !leaseOK {
		r, leaseOK = s.authenticateTaskMCPLaunch(r)
	}
	if !leaseOK && !s.authorizeTaskMCP(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	taskmcp.NewHandler(dashboardTaskMCPProvider{server: s}).ServeHTTP(w, r)
}

func (s *Server) authorizeTaskMCP(r *http.Request) bool {
	if s == nil || strings.TrimSpace(s.authToken) == "" {
		return false
	}
	token := bearerToken(r)
	return secureCompare(token, s.authToken)
}

func (p dashboardTaskMCPProvider) Scope(r *http.Request, args map[string]any) (taskmcp.Scope, error) {
	if lease, ok := taskMCPLeaseFromRequest(r); ok {
		taskID := strings.TrimSpace(stringArg(args, "task_id"))
		if taskID == "" {
			taskID = lease.Claims.TaskID
		}
		repo := strings.TrimSpace(stringArg(args, "repo"))
		if repo == "" {
			repo = lease.Claims.Repo
		}
		number := numberArg(args["number"])
		if number == 0 {
			number = lease.Claims.Number
		}
		tool := stringArg(args, "_tool")
		if !lease.Claims.Matches(taskID, repo, number) {
			if p.server != nil && p.server.logger != nil {
				p.server.logger.Warn("[task-mcp] lease scope refusal", "lease_id", lease.Claims.ID, "tool", tool, "repo", repo)
			}
			return taskmcp.Scope{}, taskmcp.RefusalError{Err: taskmcp.LeaseTokenWrongScopeError(taskID, repo, number), Data: taskmcp.RefusalData{
				Type: "refusal", Code: "outside_lease_scope", LeaseID: lease.Claims.ID, Tool: tool, Repo: repo,
				Reason: "requested task or repository is outside this lease scope",
			}}
		}
		if lease.Claims.Stage != "" && lease.Claims.Stage != lease.CurrentStage {
			if p.server != nil && p.server.logger != nil {
				p.server.logger.Warn("[task-mcp] lease stage mismatch", "lease_id", lease.Claims.ID, "tool", tool, "repo", repo, "claim_stage", lease.Claims.Stage, "current_stage", lease.CurrentStage)
			}
			if p.server != nil {
				p.server.AgentAuditSink().Record("system", agent.AuditTaskMCPStageMismatch, lease.Claims.TaskID,
					agent.Fields("lease_id", lease.Claims.ID, "tool", tool, "repo", repo, "reason", "stage_mismatch", "stage_claimed", lease.Claims.Stage, "stage_current", lease.CurrentStage))
			}
			return taskmcp.Scope{}, taskmcp.RefusalError{Err: fmt.Errorf("%w: task MCP lease stage mismatch", taskmcp.ErrForbidden), Data: taskmcp.RefusalData{
				Type: "refusal", Code: "stage_mismatch", LeaseID: lease.Claims.ID, Tool: tool, Repo: repo,
				Reason: "task MCP lease was minted for a different run stage",
			}}
		}
		if p.server == nil || p.server.contributeHub == nil || !p.server.contributeHub.allowTaskMCPCall(lease.Claims.ID, time.Now()) {
			return taskmcp.Scope{}, taskmcp.RefusalError{Err: fmt.Errorf("%w: task MCP lease rate limit exceeded", taskmcp.ErrForbidden), Data: taskmcp.RefusalData{
				Type: "refusal", Code: "rate_limited", LeaseID: lease.Claims.ID, Tool: tool, Repo: repo,
				Reason: "task MCP lease rate limit exceeded",
			}}
		}
		if p.server != nil && p.server.logger != nil {
			p.server.logger.Info("[task-mcp] lease tool call", "lease_id", lease.Claims.ID, "tool", tool, "repo", repo)
		}
		return taskmcp.Scope{TaskID: lease.Claims.TaskID, Repo: lease.Claims.Repo, Number: lease.Claims.Number, Stage: lease.Claims.Stage}, nil
	}
	snap, err := p.snapshot(r, args)
	if err != nil {
		return taskmcp.Scope{}, err
	}
	return taskmcp.Scope{TaskID: snap.assign.TaskID, Repo: snap.assign.Repo, Number: snap.assign.Number, Stage: snap.assign.Stage}, nil
}

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, _ := args[key].(string)
	return v
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
			Stage:      snap.assign.Stage,
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
			Stage:      snap.assign.Stage,
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

func (p dashboardTaskMCPProvider) CIHealth(_ context.Context, scope taskmcp.Scope, page taskmcp.PageRequest) (taskmcp.CIHealthData, taskmcp.PageInfo, error) {
	input := p.ciHealthSnapshot(scope)
	data, info := taskmcp.BuildCIHealth(input, time.Now(), page)
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
	q := r.URL.Query()
	if taskID == "" {
		taskID = q.Get("task_id")
	}
	if strings.TrimSpace(repo) == "" {
		repo = q.Get("repo")
	}
	if number <= 0 {
		number = queryNumber(q.Get("number"))
	}
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
	if lookup := p.activeLaunchLookup(); lookup != nil {
		for _, launch := range lookup.ActiveLaunches() {
			if launchMatches(launch, taskID, repo, number) {
				assign := WSTaskAssign{
					TaskID: launch.TaskID,
					Kind:   "issue",
					Repo:   launch.Repo,
					Number: launch.Number,
					Role:   launch.Agent,
					Key:    taskKey(launch.Repo, launch.Number),
				}
				return taskMCPSnapshot{
					assign:     assign,
					labels:     append([]string(nil), launch.Labels...),
					generation: launch.Generation,
					assignedAt: launch.StartedAt,
				}, nil
			}
		}
	}
	return taskMCPSnapshot{}, fmt.Errorf("%w: no active scoped task", taskmcp.ErrForbidden)
}

func (p dashboardTaskMCPProvider) activeLaunchLookup() activeLaunchLookup {
	if p.server == nil || p.server.deps == nil || p.server.deps.AgentMgr == nil {
		if p.server != nil && p.server.deps != nil && p.server.deps.TaskMCPActiveLaunches != nil {
			return activeLaunchFunc(p.server.deps.TaskMCPActiveLaunches)
		}
		return nil
	}
	return p.server.deps.AgentMgr
}

type activeLaunchFunc func() []taskmcp.LaunchScope

func (f activeLaunchFunc) ActiveLaunches() []taskmcp.LaunchScope { return f() }

func launchMatches(launch taskmcp.LaunchScope, taskID, repo string, number int) bool {
	if strings.TrimSpace(taskID) != "" && launch.TaskID != strings.TrimSpace(taskID) {
		return false
	}
	if strings.TrimSpace(repo) != "" && !strings.EqualFold(launch.Repo, strings.TrimSpace(repo)) {
		return false
	}
	if number > 0 && launch.Number != number {
		return false
	}
	return true
}

func queryNumber(raw string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n
}

func taskKey(repo string, number int) string {
	if strings.TrimSpace(repo) == "" || number <= 0 {
		return strings.TrimSpace(repo)
	}
	return fmt.Sprintf("%s#%d", repo, number)
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

func (p dashboardTaskMCPProvider) ciHealthSnapshot(scope taskmcp.Scope) taskmcp.CICacheInput {
	var input taskmcp.CICacheInput
	cfg := p.config()
	if cfg != nil {
		input.RequiredChecks = append([]string(nil), cfg.AutoMerge.RequiredChecks...)
		input.DefaultBranch = cfg.Project.PrimaryRepo
	}
	input.States = map[string]string{}
	input.FailingPRs = map[string]int{}
	input.LastGreenSHA = map[string]string{}
	if p.server == nil || p.server.status == nil {
		return input
	}
	p.server.statusMu.RLock()
	defer p.server.statusMu.RUnlock()
	if ts, err := time.Parse(time.RFC3339, p.server.status.Timestamp); err == nil {
		input.CachedAt = ts
	}
	for _, repo := range p.server.status.Repos {
		if !strings.EqualFold(repo.Full, scope.Repo) && !strings.EqualFold(repo.Name, scope.Repo) {
			continue
		}
		for _, raw := range repo.OpenPrs {
			var m map[string]any
			b, err := json.Marshal(raw)
			if err != nil || json.Unmarshal(b, &m) != nil {
				continue
			}
			for _, check := range stringSliceFromAny(firstPresent(m, "failing_checks", "failingChecks")) {
				if strings.TrimSpace(check) == "" {
					continue
				}
				input.FailingPRs[check]++
				if input.States[check] == "" {
					input.States[check] = "failure"
				}
			}
			for _, check := range stringSliceFromAny(firstPresent(m, "pending_checks", "pendingChecks")) {
				if strings.TrimSpace(check) != "" && input.States[check] == "" {
					input.States[check] = "pending"
				}
			}
		}
	}
	for _, check := range input.RequiredChecks {
		if input.States[check] == "" {
			input.States[check] = "unknown"
		}
	}
	return input
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
