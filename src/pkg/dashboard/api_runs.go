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
	// Reason is the owner's explanation on a stage transition that carried one,
	// today only an owner reset (#8350); it is read back from the timeline
	// event so the run's history says why it stepped back.
	Reason string `json:"reason,omitempty"`
}

type RunWavePR struct {
	Repo  string `json:"repo"`
	Role  string `json:"role,omitempty"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

type RunReviewWave struct {
	Wave          int         `json:"wave"`
	PRs           []RunWavePR `json:"prs"`
	ApproveAction string      `json:"approve_action,omitempty"`
}

// runResetRequest is the body of POST /api/runs/{key}/reset (#8350).
type runResetRequest struct {
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// runResetResponse is what POST /api/runs/{key}/reset returns on success.
type runResetResponse struct {
	OK        bool   `json:"ok"`
	Key       string `json:"key"`
	StageFrom string `json:"stage_from"`
	Stage     string `json:"stage"`
	Gen       uint64 `json:"gen"`
	Reason    string `json:"reason"`
}

// auditActionRunStageReset is the dashboard audit action handleRunReset books
// against the requesting owner (the agent audit sink separately records the
// system-side lease_stage_reset with the same stage/gen fields).
const auditActionRunStageReset = "run_stage_reset"

type Run struct {
	Key            string       `json:"key"`
	Title          string       `json:"title"`
	Repo           string       `json:"repo"`
	Stage          string       `json:"stage"`
	Gen            uint64       `json:"gen"`
	StageStartedAt string       `json:"stage_started_at,omitempty"`
	WaitingOn      RunWaitingOn `json:"waiting_on"`
	WaitingReason  string       `json:"waiting_reason,omitempty"`
	WaitingSince   string       `json:"waiting_since,omitempty"`
	Assignee       string       `json:"assignee,omitempty"`
	// ClaimedBy / ClaimExpiresAt expose the issue claim recorded on the run's
	// lease (hivecommons/hive#8380). ClaimPosted says whether the claim
	// comment reached the forge or lives on the lease only. All omitempty:
	// absent while claims are off.
	ClaimedBy      string          `json:"claimed_by,omitempty"`
	ClaimExpiresAt string          `json:"claim_expires_at,omitempty"`
	ClaimPosted    bool            `json:"claim_posted,omitempty"`
	LastReceipt    string          `json:"last_receipt,omitempty"`
	PlanEpicID     string          `json:"plan_epic_id,omitempty"`
	Stages         []RunStage      `json:"stages"`
	ReviewWaves    []RunReviewWave `json:"review_waves,omitempty"`
}

type RunsSummary struct {
	Active         int `json:"active"`
	WaitingOnHuman int `json:"waiting_on_human"`
}

type runLeaseSnapshot struct {
	identity       string
	taskID         string
	repo           string
	number         int
	key            string
	stage          string
	gen            uint64
	expiresAt      time.Time
	title          string
	stageStarted   time.Time
	claimedBy      string
	claimExpiresAt time.Time
	claimPosted    bool
}

type currentTaskRunInfo struct {
	title     string
	startedAt time.Time
}

type runPlanSnapshot struct {
	epicID       string
	state        string
	reason       string
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

// handleRunReset serves POST /api/runs/{key}/reset (#8350): move a run's lease
// back to an earlier stage, minting a new generation so the relay working the
// old generation cannot resume it, and record why.
//
// OWNER-ONLY. This is the only backwards stage move; a read-write member being
// able to knock a run out of implement would undo an owner's plan approval
// from the other side, so it sits behind the same gate as approve/reject. The
// gate runs before anything else so an unverified caller learns nothing about
// which runs exist. Body: {"to": "<stage>", "reason": "<why>"}; both required.
func (s *Server) handleRunReset(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		jsonError(w, "run key required", http.StatusBadRequest)
		return
	}
	if s.contributeHub == nil {
		jsonError(w, "run lease registry unavailable", http.StatusServiceUnavailable)
		return
	}
	var body runResetRequest
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	body.To = strings.TrimSpace(body.To)
	body.Reason = sanitizeString(body.Reason)
	if body.To == "" || body.Reason == "" {
		jsonError(w, "to and reason are required", http.StatusBadRequest)
		return
	}
	now := time.Now()
	held, ok := s.contributeHub.runLeaseHolder(key, now)
	if !ok {
		jsonError(w, "run not found", http.StatusNotFound)
		return
	}
	lease, err := s.contributeHub.resetLeaseStage(held.identity, held.taskID, body.To, body.Reason, now)
	if err != nil {
		jsonError(w, err.Error(), runResetErrorStatus(err))
		return
	}
	s.auditFromRequest(r, auditActionRunStageReset, auditDetail(
		"run", key, "stage_from", held.stage, "stage_to", lease.stage,
		"reason", body.Reason, "gen", strconv.FormatUint(lease.gen, 10)), "")
	jsonResponse(w, runResetResponse{
		OK: true, Key: key, StageFrom: held.stage, Stage: lease.stage, Gen: lease.gen, Reason: body.Reason,
	})
}

// runResetErrorStatus maps a refused reset to its HTTP status: a run that has
// gone away is 404, one whose lease lapsed between lookup and reset is 409, a
// bad target stage or missing reason is 400, and a persist failure is 500 so
// the owner knows the registry, not the request, is the problem.
func runResetErrorStatus(err error) int {
	switch {
	case errors.Is(err, errLeaseNotFound):
		return http.StatusNotFound
	case errors.Is(err, errLeaseExpired):
		return http.StatusConflict
	case errors.Is(err, errLeaseStageInvalid), errors.Is(err, errLeaseResetReason):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
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
		if run.PlanEpicID != "" {
			run.ReviewWaves = s.planReviewWaves(run.PlanEpicID)
		}
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

func (s *Server) planReviewWaves(epicID string) []RunReviewWave {
	if s == nil || s.deps == nil || strings.TrimSpace(epicID) == "" {
		return nil
	}
	byWave := map[int][]RunWavePR{}
	for _, store := range s.deps.BeadStores {
		if store == nil {
			continue
		}
		tree, err := planning.GetPlanTree(store, epicID)
		if err != nil || tree == nil {
			continue
		}
		for _, child := range tree.Children {
			wave, err := strconv.Atoi(strings.TrimSpace(child.Wave))
			if err != nil || wave <= 0 || child.PRURL == "" {
				continue
			}
			repo := strings.TrimSpace(child.Repo)
			if repo == "" {
				repo = strings.TrimSpace(child.Title)
			}
			byWave[wave] = append(byWave[wave], RunWavePR{
				Repo: repo, Role: child.RepoRole, URL: child.PRURL, Title: child.Title,
			})
		}
		break
	}
	if len(byWave) == 0 {
		return nil
	}
	waves := make([]int, 0, len(byWave))
	for wave := range byWave {
		waves = append(waves, wave)
	}
	sort.Ints(waves)
	out := make([]RunReviewWave, 0, len(waves))
	for _, wave := range waves {
		out = append(out, RunReviewWave{Wave: wave, PRs: byWave[wave], ApproveAction: "approve_plan_wave"})
	}
	return out
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
		key := l.runKey()
		info := infos[leaseKey(l.identity, l.taskID)]
		title := info.title
		if title == "" {
			title = key
		}
		out = append(out, runLeaseSnapshot{
			identity: l.identity, taskID: l.taskID, repo: l.repo, number: l.number,
			key: key, stage: l.stage, gen: l.gen, expiresAt: l.expiresAt,
			title: title, stageStarted: info.startedAt,
			claimedBy: l.claimedBy, claimExpiresAt: l.claimExpiresAt, claimPosted: l.claimPosted,
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
		ClaimedBy: lease.claimedBy, ClaimExpiresAt: formatRunTime(lease.claimExpiresAt), ClaimPosted: lease.claimPosted,
		PlanEpicID: plan.epicID,
		Stages:     leaseRunStages(lease.stage, lease.gen),
	}
	if plan.epicID != "" && (plan.state == planning.PlanStateReview ||
		plan.state == planning.PlanStateStuck || plan.state == planning.PlanStateDesignReview || plan.state == planning.PlanStateDesignStuck) {
		run.WaitingOn = RunWaitingOnHuman
		run.WaitingReason = plan.reason
		run.WaitingSince = formatRunTime(plan.waitingSince)
	}
	if plan.reason == planning.WaitingReasonStalePlan {
		run.WaitingOn = RunWaitingOnHuman
		run.WaitingReason = plan.reason
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
		receipt, reason := "", ""
		if ev.Attrs != nil {
			if raw := ev.Attrs["gen"]; raw != "" {
				gen, _ = strconv.ParseUint(raw, 10, 64)
			}
			receipt = firstRunNonEmpty(ev.Attrs["receipt"], ev.Attrs["receipt_digest"], ev.Attrs["path"], ev.Attrs["digest"])
			reason = ev.Attrs["reason"]
		}
		out = append(out, RunStage{Name: name, Status: "observed", Gen: gen, Receipt: receipt, Reason: reason})
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
			snap.epicID = firstRunNonEmpty(snap.epicID, b.ID)
			if reason := b.Meta(planning.MetaRunWaitingReason); reason != "" {
				snap.reason = reason
			}
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

func summarizeRuns(runs []Run) RunsSummary {
	summary := RunsSummary{Active: len(runs)}
	for _, run := range runs {
		if run.WaitingOn == RunWaitingOnHuman {
			summary.WaitingOnHuman++
		}
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
