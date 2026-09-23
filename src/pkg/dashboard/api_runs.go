package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	hubspoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/worksource"
)

type RunWaitingOn string

const (
	RunWaitingOnAgent  RunWaitingOn = "agent"
	RunWaitingOnRemote RunWaitingOn = "remote"
	RunWaitingOnHuman  RunWaitingOn = "human"
	RunWaitingOnCI     RunWaitingOn = "ci"
	RunWaitingOnNone   RunWaitingOn = "none"
)

type RunStage struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Gen     uint64 `json:"gen"`
	Receipt string `json:"receipt,omitempty"`
}

type Run struct {
	Key            string       `json:"key"`
	Title          string       `json:"title"`
	Repo           string       `json:"repo"`
	Stage          string       `json:"stage"`
	Gen            uint64       `json:"gen"`
	StageStartedAt string       `json:"stage_started_at,omitempty"`
	WaitingOn      RunWaitingOn `json:"waiting_on"`
	WaitingSince   string       `json:"waiting_since,omitempty"`
	Assignee       string       `json:"assignee,omitempty"`
	LastReceipt    string       `json:"last_receipt,omitempty"`
	PlanEpicID     string       `json:"plan_epic_id,omitempty"`
	Stages         []RunStage   `json:"stages"`
}

type RunSummary = Run

type runLeaseSnapshot struct {
	identity     string
	taskID       string
	repo         string
	number       int
	key          string
	stage        string
	gen          uint64
	expiresAt    time.Time
	title        string
	stageStarted time.Time
}

type currentTaskRunInfo struct {
	title     string
	startedAt time.Time
}

type runPlanSnapshot struct {
	epicID       string
	state        string
	waitingSince time.Time
}

type runHumanReviewHold struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	UpdatedAt time.Time `json:"updated_at"`
}

var runReviewDispatchStatePath = "/data/review-dispatch-state.json"

func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	runs, err := s.activeRuns(false)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, runs)
}

func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		jsonError(w, "run key required", http.StatusBadRequest)
		return
	}
	runs, err := s.activeRuns(true)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	for _, run := range runs {
		if run.Key == key {
			jsonResponse(w, run)
			return
		}
	}
	jsonError(w, "run not found", http.StatusNotFound)
}

func (s *Server) activeRuns(includeTimeline bool) ([]Run, error) {
	leases, err := s.activeRunLeaseSnapshots(time.Now())
	if err != nil {
		return nil, err
	}
	plans := s.runPlanSnapshots()
	holds := runHumanReviewHolds(runReviewDispatchStatePath)
	runs := make([]Run, 0, len(leases))
	for _, lease := range leases {
		run := runFromLease(lease, plans[lease.key], holds[lease.key])
		events := s.LifecycleTimeline().ByIssue(lease.key)
		run.LastReceipt = latestRunReceipt(events)
		if run.StageStartedAt == "" {
			run.StageStartedAt = formatRunTime(timelineStageTime(events, lease.stage))
		}
		if includeTimeline {
			run.Stages = mergeRunTimelineStages(run.Stages, events)
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StageStartedAt != runs[j].StageStartedAt {
			return runs[i].StageStartedAt < runs[j].StageStartedAt
		}
		return runs[i].Key < runs[j].Key
	})
	return runs, nil
}

func (s *Server) activeRunLeaseSnapshots(now time.Time) ([]runLeaseSnapshot, error) {
	if s == nil || s.contributeHub == nil {
		return nil, errors.New("run lease registry unavailable")
	}
	h := s.contributeHub
	infos := h.currentTaskInfos()
	out := []runLeaseSnapshot{}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	for _, l := range h.leases {
		if l == nil || l.stage == "" || l.expiresAt.IsZero() || now.After(l.expiresAt) {
			continue
		}
		key := l.key
		if key == "" {
			key = worksource.Ref{Repo: l.repo, Number: l.number}.Key()
		}
		info := infos[leaseKey(l.identity, l.taskID)]
		title := info.title
		if title == "" {
			title = key
		}
		out = append(out, runLeaseSnapshot{
			identity: l.identity, taskID: l.taskID, repo: l.repo, number: l.number,
			key: key, stage: l.stage, gen: l.gen, expiresAt: l.expiresAt,
			title: title, stageStarted: info.startedAt,
		})
	}
	return out, nil
}

func (h *ContributeWSHub) currentTaskInfos() map[string]currentTaskRunInfo {
	out := map[string]currentTaskRunInfo{}
	if h == nil {
		return out
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.connections {
		if c == nil {
			continue
		}
		c.mu.Lock()
		if c.currentTask != nil {
			out[leaseKey(identityOf(c), c.currentTask.TaskID)] = currentTaskRunInfo{
				title:     c.currentTask.Title,
				startedAt: c.taskAssignedAt,
			}
		}
		c.mu.Unlock()
	}
	return out
}

func runFromLease(lease runLeaseSnapshot, plan runPlanSnapshot, hold runHumanReviewHold) Run {
	started := formatRunTime(lease.stageStarted)
	run := Run{
		Key: lease.key, Title: redactTokens(lease.title), Repo: lease.repo,
		Stage: lease.stage, Gen: lease.gen, StageStartedAt: started,
		WaitingOn: RunWaitingOnAgent, Assignee: lease.identity,
		PlanEpicID: plan.epicID,
		Stages:     leaseRunStages(lease.stage, lease.gen),
	}
	if plan.epicID != "" && (plan.state == planning.PlanStateReview ||
		plan.state == planning.PlanStateStuck || plan.state == planning.PlanStateDesignReview || plan.state == planning.PlanStateDesignStuck) {
		run.WaitingOn = RunWaitingOnHuman
		run.WaitingSince = formatRunTime(plan.waitingSince)
	}
	if !hold.UpdatedAt.IsZero() {
		run.WaitingOn = RunWaitingOnHuman
		run.WaitingSince = formatRunTime(hold.UpdatedAt)
	}
	return run
}

func leaseRunStages(current string, gen uint64) []RunStage {
	out := make([]RunStage, 0, len(orderedLeaseStages))
	seenCurrent := false
	for _, name := range orderedLeaseStages {
		status := "pending"
		stageGen := uint64(0)
		switch {
		case name == current:
			status = "current"
			stageGen = gen
			seenCurrent = true
		case !seenCurrent:
			status = "completed"
		}
		out = append(out, RunStage{Name: name, Status: status, Gen: stageGen})
	}
	return out
}

func mergeRunTimelineStages(stages []RunStage, events []timeline.Event) []RunStage {
	if len(events) == 0 {
		return stages
	}
	out := append([]RunStage{}, stages...)
	for _, ev := range events {
		name := string(ev.Kind)
		if name == "" {
			continue
		}
		gen := uint64(0)
		receipt := ""
		if ev.Attrs != nil {
			if raw := ev.Attrs["gen"]; raw != "" {
				gen, _ = strconv.ParseUint(raw, 10, 64)
			}
			receipt = firstRunNonEmpty(ev.Attrs["receipt"], ev.Attrs["receipt_digest"], ev.Attrs["path"], ev.Attrs["digest"])
		}
		out = append(out, RunStage{Name: name, Status: "observed", Gen: gen, Receipt: receipt})
	}
	return out
}

func latestRunReceipt(events []timeline.Event) string {
	for _, ev := range events {
		if ev.Attrs == nil {
			continue
		}
		if receipt := firstRunNonEmpty(ev.Attrs["receipt"], ev.Attrs["receipt_digest"], ev.Attrs["path"], ev.Attrs["digest"]); receipt != "" {
			return receipt
		}
	}
	return ""
}

func timelineStageTime(events []timeline.Event, stage string) time.Time {
	for _, ev := range events {
		if string(ev.Kind) == stage && ev.At > 0 {
			return time.UnixMilli(ev.At)
		}
	}
	return time.Time{}
}

func (s *Server) runPlanSnapshots() map[string]runPlanSnapshot {
	out := map[string]runPlanSnapshot{}
	if s == nil || s.deps == nil {
		return out
	}
	for _, p := range planning.ListPlans(s.deps.BeadStores) {
		if p.IssueRepo == "" || p.IssueNumber == "" {
			continue
		}
		key := p.IssueRepo + "#" + p.IssueNumber
		out[key] = runPlanSnapshot{epicID: p.EpicID, state: p.State}
	}
	for _, store := range s.deps.BeadStores {
		if store == nil {
			continue
		}
		for _, b := range store.List(beads.ListFilter{}) {
			if b.Type != beads.TypeEpic || b.Meta(planning.MetaPlanStatus) == "" {
				continue
			}
			repo, number := b.Meta(planning.MetaIssueRepo), b.Meta(planning.MetaIssueNumber)
			if repo == "" || number == "" {
				continue
			}
			snap := out[repo+"#"+number]
			snap.waitingSince = b.UpdatedAt.Time
			out[repo+"#"+number] = snap
		}
	}
	return out
}

func runHumanReviewHolds(path string) map[string]runHumanReviewHold {
	out := map[string]runHumanReviewHold{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var state struct {
		Human []runHumanReviewHold `json:"requires_human"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return out
	}
	for _, hold := range state.Human {
		if hold.Repo == "" || hold.Number == 0 {
			continue
		}
		out[worksource.Ref{Repo: hold.Repo, Number: hold.Number}.Key()] = hold
	}
	return out
}

func (s *Server) HeartbeatRunsSummary() *hubspoke.RunsSummary {
	runs, err := s.activeRuns(false)
	if err != nil {
		return nil
	}
	return heartbeatRunsSummary(runs, func(key string) []timeline.Event {
		if s == nil {
			return nil
		}
		return s.LifecycleTimeline().ByIssue(key)
	}, time.Now())
}

func heartbeatRunsSummary(runs []Run, eventsFor func(string) []timeline.Event, now time.Time) *hubspoke.RunsSummary {
	active := len(runs)
	waiting := 0
	var oldestWait *int64
	var lastCompleted string
	for _, run := range runs {
		if run.WaitingOn == RunWaitingOnHuman {
			waiting++
			if t, err := time.Parse(time.RFC3339, run.WaitingSince); err == nil {
				secs := int64(now.Sub(t).Seconds())
				if secs < 0 {
					secs = 0
				}
				if oldestWait == nil || secs > *oldestWait {
					v := secs
					oldestWait = &v
				}
			}
		}
		if eventsFor != nil {
			for _, ev := range eventsFor(run.Key) {
				if ev.Kind != timeline.KindStageCompleted || ev.At <= 0 {
					continue
				}
				at := time.UnixMilli(ev.At).UTC().Format(time.RFC3339)
				if at > lastCompleted {
					lastCompleted = at
				}
			}
		}
	}
	summary := &hubspoke.RunsSummary{
		Active:         &active,
		WaitingOnHuman: &waiting,
	}
	if oldestWait != nil {
		summary.OldestWaitSeconds = oldestWait
	}
	if lastCompleted != "" {
		summary.LastStageCompletedAt = &lastCompleted
	}
	return summary
}

func formatRunTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func firstRunNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
