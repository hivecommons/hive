package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/worksource"
)

// This file is the dashboard half of the Spektacular stage runner
// (hivecommons/hive#8303). The runner itself lives in pkg/spektacular and is
// wired in at boot (cmd/hive), because pkg/dashboard's internal-import
// ratchet keeps this package free of spektacular, escalate, and outputschema.
// Everything here is spelled in primitives: *Server satisfies
// spektacular.LeaseRegistry structurally, and the runner reaches the hub
// through the StageRunner interface below.

// StageRunner is what the contribute hub's cleanup loop drives once per tick
// when a runner has been installed with SetStageRunner.
type StageRunner interface {
	Tick(ctx context.Context, now time.Time)
}

// Attribute keys exchanged with the stage runner. They mirror the Attr*
// constants in pkg/spektacular (asserted equal by this package's tests) so the
// two sides agree without an import.
const (
	stageAttrRunKey          = "run_key"
	stageAttrStage           = "stage"
	stageAttrGen             = "gen"
	stageAttrReceipt         = "receipt"
	stageAttrReason          = "reason"
	stageAttrSeverity        = "severity"
	stageAttrAttempts        = "attempts"
	stageAttrPath            = "path"
	stageAttrTriageVerdict   = "triage_verdict"
	stageAttrTriageRationale = "triage_rationale"
)

const (
	runAdmissionIdentity   = "hive-triage"
	runAdmissionTaskPrefix = "run-admit-"
)

// runReceiptsDir is where AdvanceStageLease persists one stage receipt per
// advanced generation: <dir>/<run key>/<stage>-gen<gen>.json. Tests redirect
// it the same way taskLeasesFile is redirected.
var runReceiptsDir = "/data/runs/receipts"

// receiptFileMode / receiptDirMode are the permissions for persisted
// receipts; they carry no secrets, so group/other may read them.
const (
	receiptFileMode = 0o644
	receiptDirMode  = 0o755
)

// SetStageRunner installs the runner the hub's cleanup loop ticks. nil
// removes it. Called once at boot when runs.spektacular.enabled is set; the
// Features toggle therefore takes effect on the next boot, like the other
// feature switches.
func (s *Server) SetStageRunner(r StageRunner) {
	if s == nil {
		return
	}
	s.stageRunnerMu.Lock()
	defer s.stageRunnerMu.Unlock()
	s.stageRunner = r
}

// tickStageRunner runs one tick of the installed runner and reports whether
// one was installed.
func (s *Server) tickStageRunner(now time.Time) bool {
	if s == nil {
		return false
	}
	s.stageRunnerMu.Lock()
	r := s.stageRunner
	s.stageRunnerMu.Unlock()
	if r == nil {
		return false
	}
	r.Tick(context.Background(), now)
	return true
}

// Artifact address spellings the dashboard reduces to the bare name. They
// mirror spektacular.ArtifactKey (asserted equal by this package's tests)
// without importing it: Spektacular's `file` verbs address a spec as
// `<name>.md` and a plan as `<name>/plan.md`, but the bare name is the only
// stable join key across stages (jumppad-labs/spektacular#45, #46).
const (
	artifactPathSeparator   = "/"
	artifactExtMarkdown     = ".md"
	artifactExtMarkdownLong = ".markdown"
)

// bareArtifactName strips a document path segment and a markdown extension
// from an artifact address so `000057_git-commit.md`,
// `000057_git-commit/plan.md` and `000057_git-commit` all key the same run.
func bareArtifactName(name string) string {
	key := strings.TrimSpace(name)
	// `<name>/plan.md`: drop the document segment, but only when it is a
	// markdown document, so an issue-style key such as `org/repo#42` is
	// left alone.
	if i := strings.LastIndex(key, artifactPathSeparator); i >= 0 && hasMarkdownExt(key[i+1:]) {
		key = key[:i]
	}
	if ext := markdownExt(key); ext != "" {
		key = key[:len(key)-len(ext)]
	}
	return strings.TrimSpace(key)
}

// markdownExt returns the markdown extension name carries, or "".
func markdownExt(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range []string{artifactExtMarkdown, artifactExtMarkdownLong} {
		if strings.HasSuffix(lower, ext) {
			return ext
		}
	}
	return ""
}

func hasMarkdownExt(name string) bool { return markdownExt(name) != "" }

// runKeyOfLease derives the run key (Spektacular's bare artifact name, the
// workflow's data.name) from a lease's canonical work-item key. Run-stage
// items are keyed `<repo>!<runKey>:<stage>` (docs/work-sources.md); anything
// else is used verbatim, which only works when the operator named the run
// after the artifact. A run key spelled as a file address (`<name>.md`,
// `<name>/plan.md`) is reduced to the bare name so the lease joins the same
// artifact across spec, plan and implement.
func runKeyOfLease(key, repo string) string {
	k := key
	if repo != "" {
		k = strings.TrimPrefix(k, repo+"!")
	}
	if i := strings.LastIndex(k, ":"); i > 0 && validStage(k[i+1:]) {
		k = k[:i]
	}
	if bare := bareArtifactName(k); bare != "" {
		return bare
	}
	return k
}

func leaseWorkKey(l *taskLease) string {
	if l.key != "" {
		return l.key
	}
	return worksource.Ref{Repo: l.repo, Number: l.number}.Key()
}

// AdmitRun creates the first, unowned spec-stage lease for an issue that the
// triage pass promoted into a long-running run. It is idempotent for the
// repo/number run key and refuses admission unless the Spektacular runner is
// enabled, because that runner is what advances spec and plan stages.
func (s *Server) AdmitRun(repo string, number int, title string, now time.Time) error {
	return s.AdmitTriagedRun(repo, number, title, "", "", now)
}

// AdmitTriagedRun is AdmitRun plus the triage decision recorded on the lease.
func (s *Server) AdmitTriagedRun(repo string, number int, title, verdict, rationale string, now time.Time) error {
	if s == nil || s.contributeHub == nil {
		return errors.New("run lease registry unavailable")
	}
	if s.deps == nil || s.deps.Config == nil || !s.deps.Config.Runs.Spektacular.Enabled {
		return errors.New("runs.spektacular.enabled is required to admit run")
	}
	repo = strings.TrimSpace(repo)
	if repo == "" || number <= 0 {
		return errors.New("repo and issue number are required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	runKey := worksource.Ref{Repo: repo, Number: number}.Key()
	leaseKeyForRun := repo + "!" + runKey + ":" + StageSpec
	taskID := runAdmissionTaskPrefix + sanitizeReceiptSegment(runKey)
	h := s.contributeHub
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	if h.leases == nil {
		h.leases = make(map[string]*taskLease)
	}
	for _, l := range h.leases {
		if l != nil && l.stage != "" && runKeyOfLease(l.key, l.repo) == runKey && !l.expiresAt.IsZero() && now.Before(l.expiresAt) {
			return nil
		}
	}
	h.leases[leaseKey(runAdmissionIdentity, taskID)] = &taskLease{
		identity:        runAdmissionIdentity,
		taskID:          taskID,
		repo:            repo,
		number:          number,
		key:             leaseKeyForRun,
		title:           title,
		tier:            "triage",
		stage:           StageSpec,
		gen:             1,
		triageVerdict:   strings.TrimSpace(verdict),
		triageRationale: strings.TrimSpace(rationale),
		expiresAt:       now.Add(leaseTTL),
	}
	if err := h.saveLeasesLocked(); err != nil {
		delete(h.leases, leaseKey(runAdmissionIdentity, taskID))
		return fmt.Errorf("persisting admitted run lease for %s: %w", taskID, err)
	}
	return nil
}

// RunTriageFixRetired reports whether an owner reset retired this triaged run
// back to the direct-fix path. The scheduler treats this as a suppression unless
// the issue now carries an explicit run/spec override.
func (s *Server) RunTriageFixRetired(repo string, number int) bool {
	if s == nil || number <= 0 {
		return false
	}
	runKey := worksource.Ref{Repo: strings.TrimSpace(repo), Number: number}.Key()
	if runKey == "" {
		return false
	}
	issueRef := strings.TrimSpace(repo) + "!" + runKey + ":" + StageSpec
	for _, ev := range s.LifecycleTimeline().ByIssue(issueRef) {
		if ev.Attrs != nil && ev.Attrs[stageAttrReason] == runResetReasonTriageFix {
			return true
		}
	}
	return false
}

// VisitActiveStageLeases calls visit for every lease that carries a stage,
// expired or not: the runner decides what expiry means.
func (s *Server) VisitActiveStageLeases(visit func(runKey, key, stage, identity, taskID, repo string, gen uint64, expiresAt time.Time)) error {
	if s == nil || s.contributeHub == nil {
		return errors.New("run lease registry unavailable")
	}
	h := s.contributeHub
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	for _, l := range h.leases {
		if l == nil || l.stage == "" || l.expiresAt.IsZero() {
			continue
		}
		key := leaseWorkKey(l)
		visit(runKeyOfLease(key, l.repo), key, l.stage, l.identity, l.taskID, l.repo, l.gen, l.expiresAt)
	}
	return nil
}

// AdvanceStageLease persists the receipt for the lease's current generation,
// advances the lease to stage `to` through the same path the API uses (so
// the stage_completed hook, CEL trigger, and lease_stage_advanced audit fire
// exactly as a manual advance would), and records a stage_receipt timeline
// event on the lease's journey.
func (s *Server) AdvanceStageLease(identity, taskID, to string, now time.Time, receipt []byte, attrs map[string]string) error {
	if s == nil || s.contributeHub == nil {
		return errors.New("run lease registry unavailable")
	}
	h := s.contributeHub
	h.leaseMu.Lock()
	l := h.leaseForLocked(identity, taskID)
	var key, stage string
	var gen uint64
	if l != nil {
		key, stage, gen = leaseWorkKey(l), l.stage, l.gen
	}
	h.leaseMu.Unlock()
	if l == nil {
		return fmt.Errorf("lease not found for %s", taskID)
	}
	runKey := attrs[stageAttrRunKey]
	if runKey == "" {
		runKey = runKeyOfLease(key, l.repo)
	}
	path, err := writeStageReceipt(runKey, stage, gen, receipt)
	if err != nil {
		return err
	}
	advanced, err := h.advanceLeaseStage(identity, taskID, to, now)
	if err != nil {
		return err
	}
	eventAttrs := make(map[string]string, len(attrs)+5)
	for k, v := range attrs {
		eventAttrs[k] = v
	}
	if l.triageVerdict != "" {
		eventAttrs[stageAttrTriageVerdict] = l.triageVerdict
	}
	if l.triageRationale != "" {
		eventAttrs[stageAttrTriageRationale] = l.triageRationale
	}
	eventAttrs[stageAttrStage] = stage
	eventAttrs[stageAttrGen] = strconv.FormatUint(gen, 10)
	eventAttrs[stageAttrPath] = path
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: key,
		Kind:     timeline.KindStageReceipt,
		Agent:    advanced.identity,
		At:       now.UnixMilli(),
		Attrs:    eventAttrs,
	})
	return nil
}

// RetryStageLease mints a new generation of the lease's current stage; it is
// the reclaim path, so an expired lease is its expected input.
func (s *Server) RetryStageLease(identity, taskID string, now time.Time) error {
	if s == nil || s.contributeHub == nil {
		return errors.New("run lease registry unavailable")
	}
	_, err := s.contributeHub.retryLeaseStage(identity, taskID, now)
	return err
}

// RefuseStageLease records that the runner declined to advance a stage (a
// final artifact went back to draft, or the document it was bound to
// vanished) so an operator can see why the run is parked.
func (s *Server) RefuseStageLease(taskID string, attrs map[string]string) {
	if s == nil {
		return
	}
	fields := make([]any, 0, 2*len(attrs))
	for k, v := range attrs {
		fields = append(fields, k, v)
	}
	s.AgentAuditSink().Record("system", agent.AuditLeaseStageRefused, taskID, agent.Fields(fields...))
	s.logger.Warn("[spektacular] refusing to advance stage", "task", taskID,
		"run", attrs[stageAttrRunKey], "stage", attrs[stageAttrStage], "gen", attrs[stageAttrGen], "reason", attrs[stageAttrReason])
}

// EscalateStageLease turns the runner's decision event into an audit entry
// and a timeline block so the run shows up as waiting on a human.
func (s *Server) EscalateStageLease(runKey string, at time.Time, attrs map[string]string) {
	if s == nil {
		return
	}
	fields := make([]any, 0, 2*len(attrs))
	for k, v := range attrs {
		fields = append(fields, k, v)
	}
	s.AgentAuditSink().Record("system", agent.AuditLeaseStageEscalated, runKey, agent.Fields(fields...))
	eventAttrs := make(map[string]string, len(attrs))
	for k, v := range attrs {
		eventAttrs[k] = v
	}
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: runKey,
		Kind:     timeline.KindBlocked,
		At:       at.UnixMilli(),
		Attrs:    eventAttrs,
	})
	s.logger.Warn("[spektacular] stage escalated", "run", runKey,
		"stage", attrs[stageAttrStage], "gen", attrs[stageAttrGen], "attempts", attrs[stageAttrAttempts],
		"severity", attrs[stageAttrSeverity], "reason", attrs[stageAttrReason])
}

// writeStageReceipt persists the receipt bytes and returns the path.
func writeStageReceipt(runKey, stage string, gen uint64, receipt []byte) (string, error) {
	dir := filepath.Join(runReceiptsDir, sanitizeReceiptSegment(runKey))
	if err := os.MkdirAll(dir, receiptDirMode); err != nil {
		return "", fmt.Errorf("creating receipt dir: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-gen%d.json", stage, gen))
	if err := os.WriteFile(path, receipt, receiptFileMode); err != nil {
		return "", fmt.Errorf("writing receipt: %w", err)
	}
	return path, nil
}

// sanitizeReceiptSegment keeps a run key usable as one directory name.
func sanitizeReceiptSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// findRunEpic locates the epic bound to runKey across the bead stores.
func (s *Server) findRunEpic(runKey string) (*beads.Store, *beads.Bead) {
	if s == nil || s.deps == nil {
		return nil, nil
	}
	for _, store := range s.deps.BeadStores {
		if store == nil {
			continue
		}
		for _, b := range store.List(beads.ListFilter{}) {
			if b.Type == beads.TypeEpic && (b.Meta(planning.MetaRunKey) == runKey || b.ExternalRef == runKey) {
				return store, b
			}
		}
	}
	return nil, nil
}

// runPlanSource is the MetaSource value stamped on epics minted from a
// Spektacular plan.
const runPlanSource = "spektacular"

// ImportRunPlan admits a final Spektacular plan as a DRAFT Hive plan: the
// task-list text the runner rendered from Spektacular's export is decomposed
// with AutoApprove false, so children stay gated until ApprovePlan and no
// model is asked to redecompose an already-structured plan. The epic is found
// by its run_key metadata (or created bound to the run key). A run whose epic
// already carries a plan is left alone (idempotent across ticks).
func (s *Server) ImportRunPlan(runKey, repo, taskList string) error {
	if s == nil {
		return errors.New("no server")
	}
	store, epic := s.findRunEpic(runKey)
	if epic == nil {
		var name string
		store, name = s.planEpicStore()
		if store == nil {
			return errors.New("no bead store configured for plan import")
		}
		created, err := store.Create(runKey, beads.TypeEpic, beads.PriorityMedium, name, runKey)
		if err != nil {
			return fmt.Errorf("creating run epic: %w", err)
		}
		for key, value := range map[string]string{
			planning.MetaRunKey:    runKey,
			planning.MetaIssueRepo: repo,
			planning.MetaSource:    runPlanSource,
		} {
			if value == "" {
				continue
			}
			if err := store.SetMetadata(created.ID, key, value); err != nil {
				return fmt.Errorf("tagging run epic: %w", err)
			}
		}
		epic, _ = store.Get(created.ID)
	}
	if epic.Meta(planning.MetaPlanStatus) != "" {
		return nil
	}
	if _, err := planning.DecomposeFromOutput(store, epic, taskList, planning.Options{AutoApprove: false}); err != nil {
		return fmt.Errorf("importing spektacular plan: %w", err)
	}
	return nil
}

// runPlanApproved reports whether the run's imported plan has been approved.
func (s *Server) runPlanApproved(runKey string) bool {
	_, epic := s.findRunEpic(runKey)
	return epic != nil && epic.Meta(planning.MetaPlanStatus) == planning.PlanStatusApproved
}

// runStageAccessor is the dashboard's worksource.RunStageLeaseAccessor: the
// registry's current stage per run is offerable, except that `implement` is
// listed only once the imported plan is approved through ApprovePlan.
type runStageAccessor struct {
	s *Server
}

// RunStageAccessor exposes the lease registry to worksource.NewRunStageSource.
func (s *Server) RunStageAccessor() worksource.RunStageLeaseAccessor {
	return &runStageAccessor{s: s}
}

func (a *runStageAccessor) PendingRunStages(_ context.Context) ([]worksource.RunStage, error) {
	s := a.s
	out := []worksource.RunStage{}
	err := s.VisitActiveStageLeases(func(runKey, _, stage, _, _, repo string, _ uint64, expiresAt time.Time) {
		if time.Now().After(expiresAt) {
			return
		}
		if stage == StageImplement && !s.runPlanApproved(runKey) {
			return
		}
		out = append(out, worksource.RunStage{RunKey: runKey, Stage: stage, Repo: repo, Title: runKey})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StageHasLiveLease reports whether a connection is currently working that
// exact run stage, in which case it must not be offered again.
func (a *runStageAccessor) StageHasLiveLease(_ context.Context, runKey, stage string) (bool, error) {
	s := a.s
	if s == nil || s.contributeHub == nil {
		return false, errors.New("run lease registry unavailable")
	}
	infos := s.contributeHub.currentTaskInfos()
	live := false
	err := s.VisitActiveStageLeases(func(rk, _, st, identity, taskID, _ string, _ uint64, _ time.Time) {
		if rk != runKey || st != stage {
			return
		}
		if _, ok := infos[leaseKey(identity, taskID)]; ok {
			live = true
		}
	})
	return live, err
}
