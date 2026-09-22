package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type runSnapshot struct {
	Key            string         `json:"key"`
	Title          string         `json:"title"`
	Repo           string         `json:"repo"`
	Stage          string         `json:"stage"`
	Gen            uint64         `json:"gen"`
	StageStartedAt string         `json:"stage_started_at,omitempty"`
	WaitingOn      string         `json:"waiting_on"`
	WaitingSince   string         `json:"waiting_since,omitempty"`
	Assignee       string         `json:"assignee,omitempty"`
	LastReceipt    string         `json:"last_receipt,omitempty"`
	PlanEpicID     string         `json:"plan_epic_id,omitempty"`
	Stages         []runStageSnap `json:"stages"`
}

type runStageSnap struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Gen     uint64 `json:"gen"`
	Receipt string `json:"receipt,omitempty"`
}

type runSnapshotList []runSnapshot

func (l *runSnapshotList) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		*l = nil
		return nil
	}
	if data[0] == '[' {
		var runs []runSnapshot
		if err := json.Unmarshal(data, &runs); err != nil {
			return err
		}
		*l = runs
		return nil
	}
	// /api/status currently exposes runs as a summary object. Keep SSE parsing
	// compatible while allowing event payloads that carry the full run array.
	*l = nil
	return nil
}

type pendingCheckpointKey struct {
	backend string
	runKey  string
}

type pendingCheckpoint struct {
	RunKey  string
	Stage   string
	Authors map[string]struct{}
}

func (s *Service) registerRunsCommand() {
	s.RegisterCommand("runs", func(ctx context.Context, args string) (string, error) {
		return s.cmdRuns(ctx, args)
	})
}

func (s *Service) cmdRuns(ctx context.Context, args string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return s.cmdRunsList(ctx)
	}
	switch strings.ToLower(fields[0]) {
	case "approve":
		if len(fields) != 2 {
			return "❌ Usage: `!runs approve <key>`", nil
		}
		return s.cmdRunsApprove(ctx, fields[1])
	case "reject":
		if len(fields) < 3 {
			return "❌ Usage: `!runs reject <key> <reason>`", nil
		}
		return s.cmdRunsReject(ctx, fields[1], strings.TrimSpace(strings.Join(fields[2:], " ")))
	default:
		if len(fields) != 1 {
			return "❌ Usage: `!runs [key]` | `!runs approve <key>` | `!runs reject <key> <reason>`", nil
		}
		return s.cmdRunsShow(ctx, fields[0])
	}
}

func (s *Service) cmdRunsList(ctx context.Context) (string, error) {
	runs, err := s.fetchRuns(ctx)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load runs: %s", err), nil
	}
	if len(runs) == 0 {
		return "ℹ️ No active runs.", nil
	}
	lines := []string{"**Active runs**"}
	for _, run := range runs {
		lines = append(lines, fmt.Sprintf("- `%s` — %s · waiting_on=%s · age %s",
			run.Key, run.Stage, emptyDefault(run.WaitingOn, "none"), runAge(run)))
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) cmdRunsShow(ctx context.Context, key string) (string, error) {
	run, err := s.fetchRun(ctx, key)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load run `%s`: %s", key, err), nil
	}
	lines := []string{
		fmt.Sprintf("**Run `%s`** — %s", run.Key, emptyDefault(run.Title, run.Key)),
		fmt.Sprintf("Repo: %s", emptyDefault(run.Repo, "unknown")),
		fmt.Sprintf("Stage: %s (gen %d), waiting_on=%s, age %s", run.Stage, run.Gen, emptyDefault(run.WaitingOn, "none"), runAge(run)),
	}
	if run.Assignee != "" {
		lines = append(lines, fmt.Sprintf("Assignee: %s", run.Assignee))
	}
	if run.PlanEpicID != "" {
		lines = append(lines, fmt.Sprintf("Plan: %s", run.PlanEpicID))
	}
	artifacts := runArtifactLines(run)
	if len(artifacts) == 0 {
		lines = append(lines, "Artifacts: none reported")
	} else {
		lines = append(lines, append([]string{"Artifacts:"}, artifacts...)...)
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) cmdRunsApprove(ctx context.Context, key string) (string, error) {
	if err := requireCommandOwner(ctx); err != nil {
		return err.Error(), nil
	}
	run, err := s.fetchRun(ctx, key)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load run `%s`: %s", key, err), nil
	}
	if run.PlanEpicID == "" {
		return fmt.Sprintf("❌ Run `%s` has no plan_epic_id to approve.", key), nil
	}
	if err := s.dashboardPost(ctx, "/api/plan/"+url.PathEscape(run.PlanEpicID)+"/approve", nil); err != nil {
		return fmt.Sprintf("❌ Failed to approve run `%s`: %s", key, err), nil
	}
	s.clearPendingCheckpoint(key)
	return fmt.Sprintf("✅ Approved run `%s` plan `%s`.", key, run.PlanEpicID), nil
}

func (s *Service) cmdRunsReject(ctx context.Context, key, reason string) (string, error) {
	if err := requireCommandOwner(ctx); err != nil {
		return err.Error(), nil
	}
	if strings.TrimSpace(reason) == "" {
		return "❌ Usage: `!runs reject <key> <reason>`", nil
	}
	run, err := s.fetchRun(ctx, key)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load run `%s`: %s", key, err), nil
	}
	if run.PlanEpicID == "" {
		return fmt.Sprintf("❌ Run `%s` has no plan_epic_id to reject.", key), nil
	}
	if err := s.dashboardPost(ctx, "/api/plan/"+url.PathEscape(run.PlanEpicID)+"/reject", nil); err != nil {
		return fmt.Sprintf("❌ Failed to reject run `%s`: %s", key, err), nil
	}
	s.clearPendingCheckpoint(key)
	return fmt.Sprintf("✅ Rejected run `%s` plan `%s`: %s", key, run.PlanEpicID, reason), nil
}

func (s *Service) fetchRuns(ctx context.Context) ([]runSnapshot, error) {
	data, err := s.dashboardGet(ctx, "/api/runs")
	if err != nil {
		return nil, err
	}
	var runs []runSnapshot
	if err := json.Unmarshal(data, &runs); err != nil {
		return nil, err
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Key < runs[j].Key })
	return runs, nil
}

func (s *Service) fetchRun(ctx context.Context, key string) (runSnapshot, error) {
	data, err := s.dashboardGet(ctx, "/api/runs/"+url.PathEscape(key))
	if err != nil {
		return runSnapshot{}, err
	}
	var run runSnapshot
	if err := json.Unmarshal(data, &run); err != nil {
		return runSnapshot{}, err
	}
	return run, nil
}

func (s *Service) handlePendingCheckpointReply(ctx context.Context, msg Message, content string) bool {
	parts := strings.SplitN(strings.TrimSpace(content), " ", 2)
	if len(parts) == 0 {
		return false
	}
	verb := strings.ToLower(parts[0])
	if verb != "approve" && verb != "reject" {
		return false
	}
	if len(s.allowedUsers) == 0 {
		return false
	}
	role, ok := s.allowedUsers[msg.AuthorID]
	if !ok {
		return false
	}
	matches := s.pendingCheckpointsForAuthor(msg.AuthorID)
	if len(matches) == 0 {
		return false
	}
	if !config.RoleAtLeast(role, config.RoleOwner) {
		s.logger.Warn("chat: refusing run checkpoint reply from non-owner", "user_id", msg.AuthorID, "verb", verb)
		s.enqueue("❌ owner role required")
		return true
	}
	if len(matches) > 1 {
		sort.Slice(matches, func(i, j int) bool { return matches[i].RunKey < matches[j].RunKey })
		keys := make([]string, 0, len(matches))
		for _, checkpoint := range matches {
			keys = append(keys, checkpoint.RunKey)
		}
		s.enqueue(fmt.Sprintf("❌ Multiple pending run decisions: %s. Use `!runs approve <key>` or `!runs reject <key> <reason>`.", strings.Join(keys, ", ")))
		return true
	}
	runKey := matches[0].RunKey
	commandArgs := "approve " + runKey
	if verb == "reject" {
		reason := ""
		if len(parts) > 1 {
			reason = strings.TrimSpace(parts[1])
		}
		if reason == "" {
			s.enqueue(fmt.Sprintf("❌ Usage: `!runs reject %s <reason>`", runKey))
			return true
		}
		commandArgs = "reject " + runKey + " " + reason
	}
	ctx = context.WithValue(ctx, commandRoleContextKey{}, role)
	reply, err := s.cmdRuns(ctx, commandArgs)
	if err != nil {
		reply = fmt.Sprintf("❌ %s", err)
	}
	if reply != "" {
		s.enqueue(reply)
	}
	return true
}

func (s *Service) pendingCheckpointsForAuthor(author string) []*pendingCheckpoint {
	backend := ""
	if s.backend != nil {
		backend = s.backend.Name()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*pendingCheckpoint
	for key, checkpoint := range s.pendingCheckpoints {
		if key.backend != backend {
			continue
		}
		if _, ok := checkpoint.Authors[author]; ok {
			cp := *checkpoint
			out = append(out, &cp)
		}
	}
	return out
}

func (s *Service) diffRuns(prev, cur []runSnapshot) {
	prevMap := make(map[string]runSnapshot, len(prev))
	curMap := make(map[string]runSnapshot, len(cur))
	for _, run := range prev {
		prevMap[run.Key] = run
	}
	for _, run := range cur {
		curMap[run.Key] = run
		old, existed := prevMap[run.Key]
		if run.WaitingOn == "human" && (!existed || old.WaitingOn != "human" || old.Stage != run.Stage || old.Gen != run.Gen) {
			s.enqueueRunCheckpoint(run)
		}
		if run.WaitingOn != "human" {
			s.clearPendingCheckpoint(run.Key)
		}
	}
	for key, old := range prevMap {
		if _, ok := curMap[key]; !ok && old.WaitingOn == "human" {
			s.clearPendingCheckpoint(key)
		}
	}
}

func (s *Service) enqueueRunCheckpoint(run runSnapshot) {
	authors := s.ownerAuthors()
	if len(authors) == 0 {
		s.logger.Warn("chat: run checkpoint reached human gate with no owner allowlist", "run", run.Key)
		return
	}
	s.mu.Lock()
	s.pendingCheckpoints[s.pendingCheckpointKey(run.Key)] = &pendingCheckpoint{
		RunKey:  run.Key,
		Stage:   run.Stage,
		Authors: authors,
	}
	s.mu.Unlock()
	s.enqueue(fmt.Sprintf("Run %s stage %s needs a decision: %s. Reply approve or reject <reason>.%s",
		run.Key, emptyDefault(run.Stage, "unknown"), oneLineRunSummary(run), formatPromptArtifacts(run)))
}

func (s *Service) ownerAuthors() map[string]struct{} {
	authors := map[string]struct{}{}
	for author, role := range s.allowedUsers {
		if config.RoleAtLeast(role, config.RoleOwner) {
			authors[author] = struct{}{}
		}
	}
	return authors
}

func (s *Service) pendingCheckpointKey(runKey string) pendingCheckpointKey {
	backend := ""
	if s.backend != nil {
		backend = s.backend.Name()
	}
	return pendingCheckpointKey{backend: backend, runKey: runKey}
}

func (s *Service) clearPendingCheckpoint(runKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pendingCheckpoints, s.pendingCheckpointKey(runKey))
}

func (s *Service) syncRunsFromSSE(ctx context.Context) {
	runs, err := s.fetchRuns(ctx)
	if err != nil {
		s.logger.Debug("chat: failed to refresh runs for checkpoint diff", "error", err)
		return
	}
	s.mu.Lock()
	prevMap := s.lastRuns
	curMap := runSliceMap(runs)
	s.lastRuns = curMap
	s.mu.Unlock()
	if prevMap == nil {
		return
	}
	s.diffRuns(runMapSlice(prevMap), runs)
}

func runSliceMap(runs []runSnapshot) map[string]runSnapshot {
	out := make(map[string]runSnapshot, len(runs))
	for _, run := range runs {
		out[run.Key] = run
	}
	return out
}

func runMapSlice(m map[string]runSnapshot) []runSnapshot {
	out := make([]runSnapshot, 0, len(m))
	for _, run := range m {
		out = append(out, run)
	}
	return out
}

func oneLineRunSummary(run runSnapshot) string {
	summary := emptyDefault(run.Title, run.Key)
	if run.Repo != "" {
		summary += " (" + run.Repo + ")"
	}
	if len([]rune(summary)) > 140 {
		summary = string([]rune(summary)[:140]) + "…"
	}
	return summary
}

func formatPromptArtifacts(run runSnapshot) string {
	artifacts := runArtifactLines(run)
	if len(artifacts) == 0 {
		return ""
	}
	return "\n" + strings.Join(artifacts, "\n")
}

func runArtifactLines(run runSnapshot) []string {
	seen := map[string]struct{}{}
	var lines []string
	add := func(label, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		lines = append(lines, fmt.Sprintf("- %s: %s", label, value))
	}
	add("last receipt", run.LastReceipt)
	for _, stage := range run.Stages {
		add(stage.Name+" receipt", stage.Receipt)
	}
	return lines
}

func runAge(run runSnapshot) string {
	for _, raw := range []string{run.WaitingSince, run.StageStartedAt} {
		if raw == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			continue
		}
		return formatRunDuration(time.Since(at))
	}
	return "unknown"
}

func formatRunDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return "<1m"
	}
}

func emptyDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
