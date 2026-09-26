package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/worksource"
)

const runDetailEventsFile = "events.jsonl"

var errRunDetailNotFound = errors.New("run not found")

type RunDetail struct {
	OK        bool             `json:"ok"`
	Key       string           `json:"key"`
	Run       Run              `json:"run"`
	Issue     RunDetailIssue   `json:"issue"`
	RepoURL   string           `json:"repo_url,omitempty"`
	Plan      *RunDetailPlan   `json:"plan,omitempty"`
	PRs       []RunDetailPR    `json:"prs,omitempty"`
	Stages    []RunDetailStage `json:"stages"`
	Events    []RunDetailEvent `json:"events"`
	RawLog    string           `json:"raw_log"`
	Notes     []string         `json:"notes,omitempty"`
	Generated string           `json:"generated_at"`
}

type RunDetailIssue struct {
	Key    string `json:"key"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
}

type RunDetailPlan struct {
	EpicID   string               `json:"epic_id"`
	URL      string               `json:"url"`
	Title    string               `json:"title,omitempty"`
	Status   string               `json:"status,omitempty"`
	Approved bool                 `json:"approved"`
	Waves    []RunDetailPlanWave  `json:"waves,omitempty"`
	Tasks    []planning.PlanChild `json:"tasks,omitempty"`
}

type RunDetailPlanWave struct {
	Wave  string               `json:"wave"`
	Tasks []planning.PlanChild `json:"tasks"`
}

type RunDetailPR struct {
	URL        string `json:"url"`
	Verified   bool   `json:"verified"`
	Reason     string `json:"reason,omitempty"`
	Source     string `json:"source,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Actor      string `json:"actor,omitempty"`
	Model      string `json:"model,omitempty"`
	Backend    string `json:"backend,omitempty"`
	Summary    string `json:"summary,omitempty"`
	ReportedAt string `json:"reported_at,omitempty"`
}

type RunDetailStage struct {
	Name            string                   `json:"name"`
	Status          string                   `json:"status"`
	Gen             uint64                   `json:"gen,omitempty"`
	Actor           string                   `json:"actor,omitempty"`
	StartedAt       string                   `json:"started_at,omitempty"`
	EndedAt         string                   `json:"ended_at,omitempty"`
	Duration        string                   `json:"duration,omitempty"`
	ReceiptSHA      string                   `json:"receipt_sha,omitempty"`
	Receipt         *RunDetailReceipt        `json:"receipt,omitempty"`
	Narrative       []string                 `json:"narrative,omitempty"`
	Interview       []RunDetailInterview     `json:"interview,omitempty"`
	Documents       []RunDetailStageDocument `json:"documents,omitempty"`
	Prompt          *RunDetailTextBlock      `json:"prompt,omitempty"`
	AgentTranscript *RunDetailTextBlock      `json:"agent_transcript,omitempty"`
	StatusHistory   []RunDetailStageStatus   `json:"status_history,omitempty"`
	Missing         []string                 `json:"missing,omitempty"`
	Transcript      []RunDetailTranscript    `json:"transcript,omitempty"`
}

type RunDetailReceipt struct {
	Path        string          `json:"path"`
	File        string          `json:"file"`
	Digest      string          `json:"digest,omitempty"`
	Stage       string          `json:"stage,omitempty"`
	Generation  uint64          `json:"generation,omitempty"`
	StartedAt   string          `json:"started_at,omitempty"`
	EndedAt     string          `json:"ended_at,omitempty"`
	ResultClass string          `json:"result_class,omitempty"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

type RunDetailTranscript struct {
	At     string         `json:"at,omitempty"`
	Kind   string         `json:"kind"`
	Label  string         `json:"label"`
	Text   string         `json:"text,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
}

type RunDetailEvent struct {
	At     string            `json:"at,omitempty"`
	Kind   string            `json:"kind"`
	Stage  string            `json:"stage,omitempty"`
	Gen    uint64            `json:"gen,omitempty"`
	Actor  string            `json:"actor,omitempty"`
	Text   string            `json:"text,omitempty"`
	Attrs  map[string]string `json:"attrs,omitempty"`
	Source string            `json:"source,omitempty"`
}

type runDetailPersistedEvent struct {
	TS       string         `json:"ts"`
	Kind     string         `json:"kind"`
	RunKey   string         `json:"run_key,omitempty"`
	TaskID   string         `json:"task_id,omitempty"`
	Stage    string         `json:"stage,omitempty"`
	Gen      uint64         `json:"gen,omitempty"`
	Actor    string         `json:"actor,omitempty"`
	Backend  string         `json:"backend,omitempty"`
	Model    string         `json:"model,omitempty"`
	Summary  string         `json:"summary,omitempty"`
	PRURL    string         `json:"pr_url,omitempty"`
	Verified bool           `json:"pr_verified,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Fields   map[string]any `json:"fields,omitempty"`
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("key"))
	if unescaped, err := url.PathUnescape(key); err == nil {
		key = strings.TrimSpace(unescaped)
	}
	detail, err := s.buildRunDetail(r, key)
	if errors.Is(err, errRunDetailNotFound) {
		jsonError(w, "run not found", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, detail)
}

func (s *Server) buildRunDetail(r *http.Request, key string) (RunDetail, error) {
	if key == "" {
		return RunDetail{}, errors.New("run key required")
	}
	run, err := s.lookupRunForDetail(r, key)
	if err != nil {
		return RunDetail{}, err
	}
	events := dedupeTimelineEvents(append(s.LifecycleTimeline().ByIssue(run.Key), s.LifecycleTimeline().ByIssue(run.LeaseKey)...))
	sort.Slice(events, func(i, j int) bool { return events[i].At < events[j].At })
	storageKeys := runDetailStorageKeys(run)
	receipts := readRunDetailReceipts(storageKeys...)
	captures := readRunDetailStageCaptures(storageKeys...)
	persisted := readRunDetailPersistedEvents(storageKeys...)
	taskRuns := readTaskRunsForIssueForDetail(s.taskRunLogPathForDetail(), run.Repo, runIssueNumber(run.Key))
	issue := s.runDetailIssue(run)
	if issue.Title == "" {
		issue.Title = firstRunNonEmpty(run.Title, issue.Key)
	}
	if run.Title == "" || run.Title == run.Key {
		run.Title = issue.Title
	}
	detailEvents := buildRunDetailEvents(events, persisted, taskRuns)
	notes := []string{}
	if len(persisted) == 0 {
		notes = append(notes, "Relay-side progress transcript was not recorded for this run unless it appears in the task run log; future runs persist progress and completion payloads under the run receipt directory.")
	}
	if len(receipts) == 0 {
		notes = append(notes, "No stage receipt files were found for this run.")
	}
	return RunDetail{
		OK: true, Key: run.Key, Run: run, Issue: issue, RepoURL: githubRepoURL(run.Repo),
		Plan: s.runDetailPlan(run.PlanEpicID), PRs: buildRunDetailPRs(events, persisted, taskRuns, run),
		Stages: buildRunDetailStages(run, events, receipts, captures, persisted, taskRuns), Events: detailEvents,
		RawLog: buildRunDetailRawLog(detailEvents, receipts), Notes: notes, Generated: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func (s *Server) lookupRunForDetail(r *http.Request, key string) (Run, error) {
	runs, err := s.activeRuns(true)
	if err != nil {
		return Run{}, err
	}
	for _, run := range runs {
		if run.Key == key || run.LeaseKey == key {
			_ = s.populateRunBurndown(r, &run)
			return run, nil
		}
	}
	if run, ok := s.queuedRunByKey(key); ok {
		return run, nil
	}
	return Run{}, errRunDetailNotFound
}

func (s *Server) taskRunLogPathForDetail() string {
	if s != nil && s.contributeHub != nil {
		return s.contributeHub.taskRunLogPath()
	}
	return taskRunLogPath
}

func (s *Server) runDetailIssue(run Run) RunDetailIssue {
	repo, number := run.Repo, runIssueNumber(run.Key)
	if ref, ok := worksource.ParseKey(run.Key); ok {
		repo, number = ref.Repo, ref.Number
	}
	out := RunDetailIssue{Key: run.Key, Repo: repo, Number: number, URL: githubIssueURL(repo, number)}
	if title, u := s.cachedIssueTitleURL(repo, number); title != "" || u != "" {
		out.Title = title
		out.URL = firstRunNonEmpty(u, out.URL)
	}
	return out
}

func (s *Server) cachedIssueTitleURL(repo string, number int) (string, string) {
	if s == nil || number <= 0 {
		return "", ""
	}
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		return "", ""
	}
	for _, fr := range status.Repos {
		if repo != "" && !strings.EqualFold(firstRunNonEmpty(fr.Full, fr.Name), repo) && !strings.EqualFold(fr.Name, repo) {
			continue
		}
		items := append(append([]any{}, fr.ActionableIssues...), fr.HeldIssues...)
		for _, item := range items {
			if n, title, u := issueItemFields(item); n == number {
				return title, u
			}
		}
	}
	return "", ""
}

func issueItemFields(item any) (int, string, string) {
	data, err := json.Marshal(item)
	if err != nil {
		return 0, "", ""
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, "", ""
	}
	return runDetailIntFromAny(firstAny(m, "number", "Number")), runDetailStringFromAny(firstAny(m, "title", "Title")), runDetailStringFromAny(firstAny(m, "html_url", "HTMLURL", "url", "URL"))
}

func (s *Server) runDetailPlan(epicID string) *RunDetailPlan {
	if s == nil || s.deps == nil || strings.TrimSpace(epicID) == "" {
		return nil
	}
	for _, store := range s.deps.BeadStores {
		tree, err := planning.GetPlanTree(store, epicID)
		if err != nil || tree == nil {
			continue
		}
		out := &RunDetailPlan{EpicID: tree.EpicID, URL: "/api/plan/" + url.PathEscape(tree.EpicID), Title: tree.EpicTitle, Status: tree.PlanStatus, Approved: tree.Approved, Tasks: append([]planning.PlanChild(nil), tree.Children...)}
		byWave := map[string][]planning.PlanChild{}
		for _, child := range tree.Children {
			if child.Wave != "" {
				byWave[child.Wave] = append(byWave[child.Wave], child)
			}
		}
		waves := make([]string, 0, len(byWave))
		for wave := range byWave {
			waves = append(waves, wave)
		}
		sort.Strings(waves)
		for _, wave := range waves {
			out.Waves = append(out.Waves, RunDetailPlanWave{Wave: wave, Tasks: byWave[wave]})
		}
		return out
	}
	return &RunDetailPlan{EpicID: epicID, URL: "/api/plan/" + url.PathEscape(epicID)}
}

func runDetailStorageKeys(run Run) []string {
	return uniqueRunDetailKeys(run.Key, run.LeaseKey, runKeyOfLease(run.LeaseKey, run.Repo), runKeyOfLease(run.Key, run.Repo), bareRunDetailKey(run.Key), bareRunDetailKey(run.LeaseKey))
}

func uniqueRunDetailKeys(keys ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key != "" && !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

func bareRunDetailKey(key string) string {
	key = strings.TrimSpace(key)
	if before, after, ok := strings.Cut(key, "!"); ok {
		_ = before
		key = after
	}
	if base, stage, ok := strings.Cut(key, ":"); ok && validStage(stage) {
		key = base
	}
	return bareArtifactName(key)
}

func readRunDetailReceipts(runKeys ...string) []RunDetailReceipt {
	var out []RunDetailReceipt
	for _, runKey := range uniqueRunDetailKeys(runKeys...) {
		dir := filepath.Join(runReceiptsDir, sanitizeReceiptSegment(runKey))
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".transcript.json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			rec := RunDetailReceipt{Path: path, File: entry.Name(), Raw: append(json.RawMessage(nil), data...)}
			var m map[string]any
			if json.Unmarshal(data, &m) == nil {
				rec.Digest = runDetailStringFromAny(firstAny(m, "output_digest", "receipt", "digest"))
				rec.Stage = runDetailStringFromAny(firstAny(m, "stage"))
				rec.Generation = uint64(runDetailIntFromAny(firstAny(m, "generation", "gen")))
				rec.StartedAt = runDetailStringFromAny(firstAny(m, "started_at", "startedAt"))
				rec.EndedAt = runDetailStringFromAny(firstAny(m, "ended_at", "endedAt"))
				rec.ResultClass = runDetailStringFromAny(firstAny(m, "result_class", "resultClass"))
			}
			if rec.Stage == "" {
				rec.Stage, rec.Generation = parseReceiptFile(entry.Name())
			}
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

func parseReceiptFile(name string) (string, uint64) {
	stage, raw, ok := strings.Cut(strings.TrimSuffix(name, ".json"), "-gen")
	if !ok {
		return "", 0
	}
	gen, _ := strconv.ParseUint(raw, 10, 64)
	return stage, gen
}

func readRunDetailPersistedEvents(runKeys ...string) []runDetailPersistedEvent {
	var out []runDetailPersistedEvent
	for _, runKey := range uniqueRunDetailKeys(runKeys...) {
		data, err := os.ReadFile(runDetailEventsPath(runKey))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev runDetailPersistedEvent
			if json.Unmarshal([]byte(line), &ev) == nil {
				out = append(out, ev)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

func readTaskRunsForIssueForDetail(path, repo string, number int) []TaskRunRecord {
	if strings.TrimSpace(repo) == "" || number <= 0 {
		return nil
	}
	taskRunMu.Lock()
	data, err := os.ReadFile(path)
	taskRunMu.Unlock()
	if err != nil {
		return nil
	}
	var out []TaskRunRecord
	for _, line := range strings.Split(string(data), "\n") {
		var rec TaskRunRecord
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Number == number && strings.EqualFold(rec.Repo, repo) {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

func buildRunDetailStages(run Run, events []timeline.Event, receipts []RunDetailReceipt, captures []RunDetailStageCapture, persisted []runDetailPersistedEvent, taskRuns []TaskRunRecord) []RunDetailStage {
	byStage, order := map[string]*RunDetailStage{}, append([]string(nil), orderedLeaseStages...)
	for _, st := range run.Stages {
		if st.Name != "" {
			byStage[st.Name] = &RunDetailStage{Name: st.Name, Status: st.Status, Gen: st.Gen, Actor: st.Actor, ReceiptSHA: st.Receipt}
		}
	}
	for _, name := range order {
		if byStage[name] == nil {
			byStage[name] = &RunDetailStage{Name: name, Status: "pending"}
		}
	}
	for _, ev := range events {
		stage := eventStage(ev)
		if stage == "" {
			continue
		}
		st := ensureRunDetailStage(byStage, &order, stage)
		st.Actor = firstRunNonEmpty(ev.Agent, st.Actor)
		if gen := eventGen(ev); gen > 0 {
			st.Gen = gen
		}
		at := formatRunTime(time.UnixMilli(ev.At))
		if ev.Kind == timeline.KindStageReceipt || ev.Kind == timeline.KindStageCompleted {
			st.EndedAt = firstRunNonEmpty(st.EndedAt, at)
		} else if ev.Kind == timeline.KindProgress {
			st.StartedAt = firstRunNonEmpty(st.StartedAt, at)
		} else if ev.Kind == timeline.KindBlocked {
			st.Status = "blocked"
		}
		st.Transcript = append(st.Transcript, transcriptFromTimeline(ev))
	}
	for _, rec := range receipts {
		st := ensureRunDetailStage(byStage, &order, firstRunNonEmpty(rec.Stage, StageSpec))
		st.Receipt, st.ReceiptSHA = &rec, firstRunNonEmpty(st.ReceiptSHA, rec.Digest)
		if rec.Generation > 0 && rec.Generation >= st.Gen {
			st.Gen = rec.Generation
		}
		st.StartedAt, st.EndedAt = firstRunNonEmpty(st.StartedAt, rec.StartedAt), firstRunNonEmpty(st.EndedAt, rec.EndedAt)
		st.Narrative = append(st.Narrative, "Receipt written: "+rec.File)
	}
	for _, cap := range captures {
		st := ensureRunDetailStage(byStage, &order, firstRunNonEmpty(cap.Stage, StageSpec))
		if cap.Generation > 0 && st.Gen > 0 && cap.Generation < st.Gen {
			continue
		}
		if cap.Generation > 0 && cap.Generation >= st.Gen {
			st.Gen = cap.Generation
		}
		st.StartedAt, st.EndedAt = firstRunNonEmpty(cap.StartedAt, st.StartedAt), firstRunNonEmpty(cap.EndedAt, st.EndedAt)
		st.Prompt = cap.Prompt
		st.AgentTranscript = cap.AgentTranscript
		st.StatusHistory = append(st.StatusHistory, cap.StatusHistory...)
		st.Interview = append(st.Interview, cap.Interview...)
		st.Documents = append(st.Documents, cap.Documents...)
		st.Missing = append(st.Missing, cap.Notes...)
		st.Narrative = append(st.Narrative, runDetailCaptureNarrative(cap)...)
	}
	for _, ev := range persisted {
		st := ensureRunDetailStage(byStage, &order, firstRunNonEmpty(ev.Stage, StageImplement))
		st.Actor = firstRunNonEmpty(ev.Actor, st.Actor)
		st.Transcript = append(st.Transcript, transcriptFromPersisted(ev))
	}
	for _, rec := range taskRuns {
		st := ensureRunDetailStage(byStage, &order, StageImplement)
		st.Transcript = append(st.Transcript, transcriptFromTaskRun(rec))
	}
	var out []RunDetailStage
	for _, name := range order {
		if st := byStage[name]; st != nil {
			if st.StartedAt != "" && st.EndedAt != "" {
				if a, errA := time.Parse(time.RFC3339, st.StartedAt); errA == nil {
					if b, errB := time.Parse(time.RFC3339, st.EndedAt); errB == nil && b.After(a) {
						st.Duration = b.Sub(a).String()
					}
				}
			}
			sort.SliceStable(st.Transcript, func(i, j int) bool { return st.Transcript[i].At < st.Transcript[j].At })
			if (st.Name == StageSpec || st.Name == StagePlan) && st.Receipt != nil && len(st.Documents) == 0 && len(st.AgentTranscriptText()) == 0 {
				st.Missing = append(st.Missing, "Spec/plan document, interview history, and agent transcript were not captured for this older run because the executor worktree was swept before capture was added in this version.")
			}
			out = append(out, *st)
		}
	}
	return out
}

func (s RunDetailStage) AgentTranscriptText() string {
	if s.AgentTranscript == nil {
		return ""
	}
	return s.AgentTranscript.Text
}

func runDetailCaptureNarrative(cap RunDetailStageCapture) []string {
	var out []string
	out = append(out, "Prepared hub executor worktree and wrote the stage prompt.")
	if cap.Prompt != nil && cap.Prompt.Text != "" {
		out = append(out, "Sent the prompt to the "+firstRunNonEmpty(cap.Backend, "agent")+" backend.")
	}
	for i, st := range cap.StatusHistory {
		label := firstRunNonEmpty(st.Step, "status check")
		if st.DocumentStatus != "" {
			label += " (" + st.DocumentStatus + ")"
		}
		out = append(out, fmt.Sprintf("Step %d: %s observed at %s.", i+1, label, firstRunNonEmpty(st.At, cap.CapturedAt)))
	}
	if len(cap.Documents) > 0 {
		out = append(out, "Captured the final "+firstRunNonEmpty(cap.Stage, "stage")+" document.")
	}
	if cap.AgentTranscript != nil && cap.AgentTranscript.Text != "" {
		out = append(out, "Captured the bounded agent transcript.")
	}
	return out
}

func ensureRunDetailStage(byStage map[string]*RunDetailStage, order *[]string, name string) *RunDetailStage {
	if st := byStage[name]; st != nil {
		return st
	}
	st := &RunDetailStage{Name: name, Status: "observed"}
	byStage[name], *order = st, append(*order, name)
	return st
}

func transcriptFromTimeline(ev timeline.Event) RunDetailTranscript {
	label := string(ev.Kind)
	if ev.Attrs != nil {
		label = firstRunNonEmpty(ev.Attrs["event"], ev.Attrs["reason"], label)
	}
	return RunDetailTranscript{At: formatRunTime(time.UnixMilli(ev.At)), Kind: "timeline", Label: label, Fields: stringMapAny(ev.Attrs)}
}

func transcriptFromPersisted(ev runDetailPersistedEvent) RunDetailTranscript {
	fields := ev.Fields
	if fields == nil {
		fields = map[string]any{}
	}
	for k, v := range map[string]string{"task_id": ev.TaskID, "backend": ev.Backend, "model": ev.Model, "pr_url": ev.PRURL, "reason": ev.Reason} {
		if v != "" {
			fields[k] = v
		}
	}
	return RunDetailTranscript{At: ev.TS, Kind: ev.Kind, Label: firstRunNonEmpty(ev.Summary, ev.Kind), Text: ev.Summary, Fields: fields}
}

func transcriptFromTaskRun(rec TaskRunRecord) RunDetailTranscript {
	text := strings.TrimSpace(strings.Join(rec.PaneTail, "\n"))
	if text == "" {
		text = firstRunNonEmpty(rec.VerdictReason, rec.Reason, rec.Scenario)
	}
	return RunDetailTranscript{At: rec.TS, Kind: "task_run", Label: rec.Outcome, Text: text, Fields: map[string]any{"task_id": rec.TaskID, "backend": rec.Backend, "model": rec.Model, "outcome": rec.Outcome, "verdict": rec.Verdict, "completion_signal": rec.CompletionSignal, "pr_url": firstRunNonEmpty(rec.ReportedPRURL, rec.PRURL), "pr_verified": rec.PRVerified, "pr_verify_reason": rec.PRVerifyReason}}
}

func buildRunDetailEvents(events []timeline.Event, persisted []runDetailPersistedEvent, taskRuns []TaskRunRecord) []RunDetailEvent {
	var out []RunDetailEvent
	for _, ev := range events {
		out = append(out, RunDetailEvent{At: formatRunTime(time.UnixMilli(ev.At)), Kind: string(ev.Kind), Stage: eventStage(ev), Gen: eventGen(ev), Actor: ev.Agent, Attrs: ev.Attrs, Source: "timeline"})
	}
	for _, ev := range persisted {
		out = append(out, RunDetailEvent{At: ev.TS, Kind: ev.Kind, Stage: ev.Stage, Gen: ev.Gen, Actor: ev.Actor, Text: ev.Summary, Source: "receipt_event"})
	}
	for _, rec := range taskRuns {
		out = append(out, RunDetailEvent{At: rec.TS, Kind: "task_run", Stage: StageImplement, Gen: rec.TaskGen, Actor: rec.Username, Text: firstRunNonEmpty(rec.VerdictReason, rec.Reason, rec.Scenario), Source: "task_run_log"})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

func buildRunDetailPRs(events []timeline.Event, persisted []runDetailPersistedEvent, taskRuns []TaskRunRecord, run Run) []RunDetailPR {
	seen, out := map[string]bool{}, []RunDetailPR{}
	add := func(pr RunDetailPR) {
		if strings.TrimSpace(pr.URL) != "" && !seen[pr.URL] {
			seen[pr.URL], out = true, append(out, pr)
		}
	}
	for _, rec := range taskRuns {
		add(RunDetailPR{URL: firstRunNonEmpty(rec.ReportedPRURL, rec.PRURL), Verified: rec.PRVerified, Reason: rec.PRVerifyReason, Source: "task_run_log", Stage: StageImplement, Actor: rec.Username, Model: rec.Model, Backend: rec.Backend, Summary: rec.VerdictReason, ReportedAt: rec.TS})
	}
	for _, ev := range persisted {
		add(RunDetailPR{URL: ev.PRURL, Verified: ev.Verified, Reason: ev.Reason, Source: "receipt_event", Stage: ev.Stage, Actor: ev.Actor, Model: ev.Model, Backend: ev.Backend, Summary: ev.Summary, ReportedAt: ev.TS})
	}
	for _, ev := range events {
		if ev.Attrs != nil {
			add(RunDetailPR{URL: firstRunNonEmpty(ev.Attrs["pr_url"], ev.Attrs["url"]), Verified: ev.Attrs["pr_verified"] == "true", Reason: ev.Attrs["reason"], Source: "timeline", Stage: eventStage(ev), Actor: ev.Agent, ReportedAt: formatRunTime(time.UnixMilli(ev.At))})
		}
	}
	for _, wave := range run.ReviewWaves {
		for _, pr := range wave.PRs {
			add(RunDetailPR{URL: pr.URL, Verified: true, Source: "plan", Stage: StageImplement, Summary: pr.Title})
		}
	}
	return out
}

func buildRunDetailRawLog(events []RunDetailEvent, receipts []RunDetailReceipt) string {
	var lines []string
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		lines = append(lines, string(b))
	}
	for _, rec := range receipts {
		lines = append(lines, fmt.Sprintf("%s receipt %s digest=%s", rec.EndedAt, rec.File, rec.Digest))
	}
	return strings.Join(lines, "\n")
}

func appendRunDetailEvent(runKey string, ev runDetailPersistedEvent) {
	if runKey = strings.TrimSpace(runKey); runKey == "" {
		return
	}
	if ev.TS == "" {
		ev.TS = time.Now().UTC().Format(time.RFC3339)
	}
	if os.MkdirAll(runReceiptsDir, receiptDirMode) != nil {
		return
	}
	for _, key := range uniqueRunDetailKeys(runKey, bareRunDetailKey(runKey)) {
		cp := ev
		if cp.RunKey == "" {
			cp.RunKey = key
		}
		data, err := json.Marshal(cp)
		if err != nil {
			return
		}
		f, err := os.OpenFile(runDetailEventsPath(key), os.O_CREATE|os.O_WRONLY|os.O_APPEND, receiptFileMode)
		if err != nil {
			continue
		}
		_, _ = f.Write(append(data, '\n'))
		_ = f.Close()
	}
}

func runDetailEventsPath(runKey string) string {
	return filepath.Join(runReceiptsDir, sanitizeReceiptSegment(runKey)+"."+runDetailEventsFile)
}

func eventStage(ev timeline.Event) string {
	if ev.Attrs == nil {
		return ""
	}
	return firstRunNonEmpty(ev.Attrs[stageAttrStage], ev.Attrs["stage"], ev.Attrs["stage_from"], ev.Attrs["stage_to"])
}

func eventGen(ev timeline.Event) uint64 {
	if ev.Attrs == nil {
		return 0
	}
	gen, _ := strconv.ParseUint(ev.Attrs["gen"], 10, 64)
	return gen
}

func dedupeTimelineEvents(in []timeline.Event) []timeline.Event {
	seen, out := map[string]bool{}, []timeline.Event{}
	for _, ev := range in {
		key := fmt.Sprintf("%s/%s/%d", ev.IssueRef, ev.Kind, ev.At)
		if !seen[key] {
			seen[key], out = true, append(out, ev)
		}
	}
	return out
}

func runIssueNumber(key string) int {
	if ref, ok := worksource.ParseKey(key); ok {
		return ref.Number
	}
	_, raw, ok := strings.Cut(key, "#")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(raw)
	return n
}

func githubIssueURL(repo string, number int) string {
	if repo == "" || number <= 0 {
		return ""
	}
	return "https://github.com/" + repo + "/issues/" + strconv.Itoa(number)
}

func githubRepoURL(repo string) string {
	if repo == "" {
		return ""
	}
	return "https://github.com/" + repo
}

func firstAny(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

func runDetailIntFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	default:
		return 0
	}
}

func runDetailStringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func stringMapAny(in map[string]string) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
