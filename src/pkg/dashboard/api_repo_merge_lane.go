package dashboard

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/mergelane"
)

// mergeLaneStorePath is the lane record the view reads; tests point it at a
// temp file.
var mergeLaneStorePath = mergelane.DefaultStorePath

// mergeLaneBranchRules reads a lane branch's rules live, the same lane-only
// read the gate makes (Metadata read only). Tests replace it.
var mergeLaneBranchRules = func(ctx context.Context, s *Server, fullRepo, branch string) github.BranchRulesResult {
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		return github.BranchRulesResult{UpToDate: github.UpToDateUnknown, Reason: "GitHub client unavailable"}
	}
	owner, name := s.deps.GHClient.SplitRepo(fullRepo)
	set, known := s.deps.Config.AutoMerge.RequiredCheckSet()
	return github.ReadBranchRules(github.WithRESTCaller(ctx, "hive:merge_lane_view"), s.deps.GHClient.GoGitHub(), owner, name, branch, set, known)
}

// UpToDateRuleRecommendation is shown for every lane GitHub does not enforce
// up-to-date branches on (R30).
const UpToDateRuleRecommendation = "Turn on GitHub's \"Require branches to be up to date before merging\" rule for this branch. " +
	"It closes the one-API-call window between Hive's final re-check and the merge on the server. " +
	"It costs one CI run per merge, because the lane only ever updates the pull request at the front."

type mergeLaneFrontView struct {
	PR         int    `json:"pr"`
	Path       string `json:"path,omitempty"`
	Stage      string `json:"stage"`
	StageLabel string `json:"stage_label"`
	EnteredAt  string `json:"entered_at,omitempty"`
	DeadlineAt string `json:"deadline_at,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type mergeLaneWaiterView struct {
	PR         int    `json:"pr"`
	Position   int    `json:"position"`
	Path       string `json:"path,omitempty"`
	EligibleAt string `json:"eligible_at,omitempty"`
	Reason     string `json:"reason"`
}

type mergeLaneExitView struct {
	PR     int    `json:"pr"`
	Reason string `json:"reason"`
	At     string `json:"at,omitempty"`
}

type mergeLaneReasonView struct {
	PR     int    `json:"pr,omitempty"`
	Reason string `json:"reason"`
}

type mergeLaneView struct {
	Branch           string                `json:"branch"`
	Front            *mergeLaneFrontView   `json:"front,omitempty"`
	Waiting          []mergeLaneWaiterView `json:"waiting"`
	LastExit         *mergeLaneExitView    `json:"last_exit,omitempty"`
	Enforcement      string                `json:"enforcement"`
	EnforcementLabel string                `json:"enforcement_label"`
	// EnforcementReason is set when the branch rules could not be read.
	EnforcementReason string `json:"enforcement_reason,omitempty"`
	NativeMergeQueue  bool   `json:"native_merge_queue"`
	// RecommendUpToDateRule is true unless GitHub enforces up-to-date
	// branches on this branch; Recommendation then says why and what it costs.
	RecommendUpToDateRule bool                  `json:"recommend_up_to_date_rule"`
	Recommendation        string                `json:"recommendation,omitempty"`
	Reasons               []mergeLaneReasonView `json:"reasons"`
}

func mergeLaneStageLabel(stage string) string {
	switch stage {
	case mergelane.StageUpdating:
		return "updating"
	case mergelane.StageWaitingChecks:
		return "waiting for checks"
	case mergelane.StageValidating:
		return "final re-check"
	case mergelane.StageMerging:
		return "merging"
	}
	return stage
}

func mergeLaneEnforcementLabel(e github.UpToDateEnforcement) string {
	switch e {
	case github.UpToDateServerEnforced:
		return "server-enforced"
	case github.UpToDateHiveChecked:
		return "Hive-checked"
	}
	return "unknown"
}

func laneTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// buildMergeLaneView turns one durable lane record and its live branch rules
// into the view R29/R30/R14 describe.
func buildMergeLaneView(rec mergelane.Record, rules github.BranchRulesResult) mergeLaneView {
	v := mergeLaneView{Branch: rec.Branch, Waiting: []mergeLaneWaiterView{}, Reasons: []mergeLaneReasonView{}}
	if rec.Front != nil {
		v.Front = &mergeLaneFrontView{
			PR:         rec.Front.PR,
			Path:       rec.Front.Path,
			Stage:      rec.Front.Stage,
			StageLabel: mergeLaneStageLabel(rec.Front.Stage),
			EnteredAt:  laneTime(rec.Front.EnteredAt),
			DeadlineAt: laneTime(rec.Front.DeadlineAt),
			Reason:     rec.Front.LastReason,
		}
		if rec.Front.LastReason != "" {
			v.Reasons = append(v.Reasons, mergeLaneReasonView{PR: rec.Front.PR, Reason: rec.Front.LastReason})
		}
	}
	for i, w := range rec.Waiting {
		v.Waiting = append(v.Waiting, mergeLaneWaiterView{
			PR: w.PR, Position: i + 1, Path: w.Path, EligibleAt: laneTime(w.EligibleAt), Reason: mergelane.ReasonNotAtFront,
		})
		v.Reasons = append(v.Reasons, mergeLaneReasonView{PR: w.PR, Reason: mergelane.ReasonNotAtFront})
	}
	if rec.LastExit != nil {
		v.LastExit = &mergeLaneExitView{PR: rec.LastExit.PR, Reason: rec.LastExit.Reason, At: laneTime(rec.LastExit.At)}
		v.Reasons = append(v.Reasons, mergeLaneReasonView{PR: rec.LastExit.PR, Reason: rec.LastExit.Reason})
	}
	enforcement := rules.UpToDate
	if enforcement == "" {
		enforcement = github.UpToDateUnknown
	}
	v.Enforcement = string(enforcement)
	v.EnforcementLabel = mergeLaneEnforcementLabel(enforcement)
	if enforcement == github.UpToDateUnknown {
		v.EnforcementReason = rules.Reason
	}
	if rules.MergeQueueKnown && rules.MergeQueue {
		v.NativeMergeQueue = true
		v.Reasons = append(v.Reasons, mergeLaneReasonView{Reason: mergelane.NativeMergeQueueReason(rec.Branch)})
	}
	if !rules.Known && rules.Reason != "" {
		v.Reasons = append(v.Reasons, mergeLaneReasonView{Reason: "required checks unknown, no merge: " + rules.Reason})
	}
	if enforcement != github.UpToDateServerEnforced {
		v.RecommendUpToDateRule = true
		v.Recommendation = UpToDateRuleRecommendation
	}
	return v
}

// handleRepoMergeLaneGet reports the serialized lane's state for one repo,
// one entry per target branch (#10892). Read-only, so any authenticated role
// may call it. A direct repo gets its strategy and nothing else, and makes no
// GitHub call.
func (s *Server) handleRepoMergeLaneGet(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo == "" {
		jsonError(w, "repo is required", http.StatusBadRequest)
		return
	}
	if !s.watchesRepo(repo) {
		jsonError(w, "repo "+repo+" is not in project.repos", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	shown := repo
	if normalized, ok := config.NormalizeRepoForOrg(cfg.Project.Org, repo); ok {
		shown = normalized
	}
	strategy := cfg.RepoMergeStrategy(repo)
	out := map[string]any{"ok": true, "repo": shown, "merge_strategy": strategy}
	if strategy != config.MergeStrategyHiveSerialized {
		jsonResponse(w, out)
		return
	}
	fullRepo := config.QualifyRepo(cfg.Project.Org, repo)
	views := []mergeLaneView{}
	store, err := mergelane.OpenStore(mergeLaneStorePath)
	var records []mergelane.Record
	if err == nil {
		records, err = store.Lanes(fullRepo)
	}
	if err != nil {
		out["lane_error"] = err.Error()
	}
	for _, rec := range records {
		views = append(views, buildMergeLaneView(rec, mergeLaneBranchRules(r.Context(), s, fullRepo, rec.Branch)))
	}
	out["lanes"] = views
	jsonResponse(w, out)
}
