package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

const repoHoldPermissionCacheTTL = 2 * time.Minute

type repoHoldRequest struct {
	Held bool   `json:"held"`
	Type string `json:"type"`
}

type repoHoldResponse struct {
	OK           bool     `json:"ok"`
	Repo         string   `json:"repo"`
	Number       int      `json:"number"`
	Held         bool     `json:"held"`
	HoldLabel    string   `json:"hold_label"`
	Removed      []string `json:"removed"`
	MinStatusSeq uint64   `json:"minStatusSeq"`
}

type repoHoldPermissionCacheEntry struct {
	allowed bool
	at      time.Time
}

func (s *Server) canonicalHoldLabel() string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return github.CanonicalHiveHoldLabel("")
	}
	return github.CanonicalHiveHoldLabel(s.deps.Config.HiveID)
}

func repoHoldKey(owner, repo, user string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(repo) + "\x00" + strings.ToLower(user)
}

func repoHoldPermissionLevelAllows(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "admin", "maintain", "write":
		return true
	default:
		return false
	}
}

func (s *Server) canToggleRepoHold(r *http.Request, owner, repo string) (bool, int, string) {
	if isOwnerRole(r.Header.Get("X-Hive-Role")) && r.Header.Get(ownerRoleVerifiedHeader) == "true" {
		return true, http.StatusOK, ""
	}
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if user == "" {
		return false, http.StatusUnauthorized, "GitHub user session required"
	}
	if strings.EqualFold(owner, user) {
		return true, http.StatusOK, ""
	}
	if s == nil || s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		return false, http.StatusServiceUnavailable, "GitHub client not configured"
	}
	key := repoHoldKey(owner, repo, user)
	now := time.Now()
	s.repoHoldPermMu.Lock()
	if s.repoHoldPermCache == nil {
		s.repoHoldPermCache = make(map[string]repoHoldPermissionCacheEntry)
	}
	if ent, ok := s.repoHoldPermCache[key]; ok && now.Sub(ent.at) < repoHoldPermissionCacheTTL {
		s.repoHoldPermMu.Unlock()
		if ent.allowed {
			return true, http.StatusOK, ""
		}
		return false, http.StatusForbidden, "write permission required"
	}
	s.repoHoldPermMu.Unlock()

	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	ctx, cancel := context.WithTimeout(ctx, repoPermissionTimeout)
	defer cancel()
	level, _, err := s.deps.GHClient.GoGitHub().Repositories.GetPermissionLevel(ctx, owner, repo, user)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("repo hold permission lookup failed", "repo", owner+"/"+repo, "user", user, "error", err)
		}
		return false, http.StatusForbidden, "write permission required"
	}
	allowed := repoHoldPermissionLevelAllows(level.GetPermission())
	s.repoHoldPermMu.Lock()
	s.repoHoldPermCache[key] = repoHoldPermissionCacheEntry{allowed: allowed, at: now}
	s.repoHoldPermMu.Unlock()
	if !allowed {
		return false, http.StatusForbidden, "write permission required"
	}
	return true, http.StatusOK, ""
}

func (s *Server) parseRepoItemPath(w http.ResponseWriter, r *http.Request) (owner, repo string, number int, ok bool) {
	owner = strings.TrimSpace(r.PathValue("owner"))
	repo = strings.TrimSpace(r.PathValue("repo"))
	n, err := strconv.Atoi(r.PathValue("number"))
	if owner == "" || repo == "" || strings.Contains(owner, "/") || strings.Contains(repo, "/") || err != nil || n <= 0 {
		jsonError(w, "invalid repository item", http.StatusBadRequest)
		return "", "", 0, false
	}
	return owner, repo, n, true
}

func holdCausingLabels(labels []string, canonical string) []string {
	out := make([]string, 0, len(labels))
	seen := make(map[string]bool)
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.EqualFold(trimmed, canonical) || github.HasHoldLabel([]string{trimmed}) {
			if !seen[lower] {
				seen[lower] = true
				out = append(out, trimmed)
			}
		}
	}
	return out
}

func (s *Server) currentItemLabels(ctx context.Context, owner, repo string, number int) ([]string, error) {
	issue, _, err := s.deps.GHClient.GoGitHub().Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.GetName())
	}
	return labels, nil
}

func repoHoldItemNumber(raw any) int {
	switch v := raw.(type) {
	case github.Issue:
		return v.Number
	case *github.Issue:
		if v != nil {
			return v.Number
		}
	case github.PullRequest:
		return v.Number
	case *github.PullRequest:
		if v != nil {
			return v.Number
		}
	case FrontendPR:
		return v.Number
	case *FrontendPR:
		if v != nil {
			return v.Number
		}
	case github.HoldItem:
		return v.Number
	case *github.HoldItem:
		if v != nil {
			return v.Number
		}
	case map[string]any:
		switch n := v["number"].(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		case json.Number:
			i, _ := n.Int64()
			return int(i)
		}
	}
	return 0
}

func repoHoldItemString(raw any, field string) string {
	switch v := raw.(type) {
	case github.Issue:
		switch field {
		case "repo":
			return v.Repo
		case "title":
			return v.Title
		case "url":
			return v.URL
		}
	case *github.Issue:
		if v != nil {
			return repoHoldItemString(*v, field)
		}
	case github.PullRequest:
		switch field {
		case "repo":
			return v.Repo
		case "title":
			return v.Title
		case "url":
			return v.URL
		}
	case *github.PullRequest:
		if v != nil {
			return repoHoldItemString(*v, field)
		}
	case FrontendPR:
		return repoHoldItemString(v.PullRequest, field)
	case *FrontendPR:
		if v != nil {
			return repoHoldItemString(*v, field)
		}
	case github.HoldItem:
		switch field {
		case "repo":
			return v.Repo
		case "title":
			return v.Title
		case "url":
			return v.URL
		}
	case *github.HoldItem:
		if v != nil {
			return repoHoldItemString(*v, field)
		}
	case map[string]any:
		if s, _ := v[field].(string); s != "" {
			return s
		}
	}
	return ""
}

func repoHoldItemLabels(raw any) []string {
	switch v := raw.(type) {
	case github.Issue:
		return append([]string(nil), v.Labels...)
	case *github.Issue:
		if v != nil {
			return append([]string(nil), v.Labels...)
		}
	case github.PullRequest:
		return append([]string(nil), v.Labels...)
	case *github.PullRequest:
		if v != nil {
			return append([]string(nil), v.Labels...)
		}
	case FrontendPR:
		return append([]string(nil), v.Labels...)
	case *FrontendPR:
		if v != nil {
			return append([]string(nil), v.Labels...)
		}
	case github.HoldItem:
		return append([]string(nil), v.Labels...)
	case *github.HoldItem:
		if v != nil {
			return append([]string(nil), v.Labels...)
		}
	case map[string]any:
		switch labels := v["labels"].(type) {
		case []string:
			return append([]string(nil), labels...)
		case []any:
			out := make([]string, 0, len(labels))
			for _, label := range labels {
				out = append(out, fmt.Sprint(label))
			}
			return out
		}
	}
	return nil
}

func repoHoldSetItemLabels(raw any, labels []string) any {
	labels = append([]string(nil), labels...)
	switch v := raw.(type) {
	case github.Issue:
		v.Labels = labels
		return v
	case *github.Issue:
		if v != nil {
			cp := *v
			cp.Labels = labels
			return cp
		}
	case github.PullRequest:
		v.Labels = labels
		return v
	case *github.PullRequest:
		if v != nil {
			cp := *v
			cp.Labels = labels
			return cp
		}
	case FrontendPR:
		v.Labels = labels
		return v
	case *FrontendPR:
		if v != nil {
			cp := *v
			cp.Labels = labels
			return cp
		}
	case github.HoldItem:
		v.Labels = labels
		return v
	case *github.HoldItem:
		if v != nil {
			cp := *v
			cp.Labels = labels
			return cp
		}
	case map[string]any:
		cp := make(map[string]any, len(v)+1)
		for k, val := range v {
			cp[k] = val
		}
		cp["labels"] = labels
		return cp
	}
	return raw
}

func repoHoldLabelsAfter(raw any, held bool, label string, removed []string, canonical string) []string {
	labels := repoHoldItemLabels(raw)
	if held {
		for _, existing := range labels {
			if strings.EqualFold(existing, label) {
				return labels
			}
		}
		return append(labels, label)
	}
	remove := make(map[string]bool)
	for _, l := range removed {
		remove[strings.ToLower(strings.TrimSpace(l))] = true
	}
	if len(remove) == 0 {
		for _, l := range holdCausingLabels(labels, canonical) {
			remove[strings.ToLower(strings.TrimSpace(l))] = true
		}
	}
	out := labels[:0]
	for _, l := range labels {
		if !remove[strings.ToLower(strings.TrimSpace(l))] {
			out = append(out, l)
		}
	}
	return append([]string(nil), out...)
}

func repoHoldRemoveFirst(items []any, number int) ([]any, any, bool) {
	for i, item := range items {
		if repoHoldItemNumber(item) == number {
			removed := item
			copy(items[i:], items[i+1:])
			return items[:len(items)-1], removed, true
		}
	}
	return items, nil, false
}

func repoHoldSummaryMatches(item any, repoFull string, number int, itemType string) bool {
	if repoHoldItemNumber(item) != number {
		return false
	}
	if repo := repoHoldItemString(item, "repo"); repo != "" && !repoHoldRepoRefMatches(repo, repoFull) {
		return false
	}
	if hi, ok := item.(github.HoldItem); ok && hi.Type != "" && !strings.EqualFold(hi.Type, itemType) {
		return false
	}
	if hi, ok := item.(*github.HoldItem); ok && hi != nil && hi.Type != "" && !strings.EqualFold(hi.Type, itemType) {
		return false
	}
	if m, ok := item.(map[string]any); ok {
		if typ, _ := m["type"].(string); typ != "" && !strings.EqualFold(typ, itemType) {
			return false
		}
	}
	return true
}

func repoHoldRepoRefMatches(repoRef, repoFull string) bool {
	repoRef = strings.ToLower(strings.TrimSpace(repoRef))
	repoFull = strings.ToLower(strings.TrimSpace(repoFull))
	if repoRef == "" || repoFull == "" {
		return false
	}
	if repoRef == repoFull {
		return true
	}
	if strings.Contains(repoRef, "/") {
		return false
	}
	_, name, ok := strings.Cut(repoFull, "/")
	return ok && repoRef == name
}

func repoHoldSummaryHas(items []any, repoFull string, number int, itemType string) bool {
	for _, item := range items {
		if repoHoldSummaryMatches(item, repoFull, number, itemType) {
			return true
		}
	}
	return false
}

func repoHoldRemoveSummary(items []any, repoFull string, number int, itemType string) ([]any, bool) {
	for i, item := range items {
		if repoHoldSummaryMatches(item, repoFull, number, itemType) {
			copy(items[i:], items[i+1:])
			return items[:len(items)-1], true
		}
	}
	return items, false
}

func repoHoldItemForSummary(raw any, repoFull string, number int, itemType, label string) github.HoldItem {
	item := github.HoldItem{
		Number: number,
		Repo:   repoFull,
		Title:  repoHoldItemString(raw, "title"),
		Type:   itemType,
		URL:    repoHoldItemString(raw, "url"),
		Labels: repoHoldLabelsAfter(raw, true, label, nil, label),
	}
	if item.Repo == "" {
		item.Repo = repoFull
	}
	if item.Number == 0 {
		item.Number = number
	}
	return item
}

func repoHoldAdjustBreakdown(repo *FrontendRepo, itemType string, held bool) {
	if repo == nil || repo.WorkBreakdown == nil {
		return
	}
	deltaHold, deltaActionable := 1, -1
	if !held {
		deltaHold, deltaActionable = -1, 1
	}
	if itemType == "pr" {
		repo.WorkBreakdown.PRs.Hold += deltaHold
		repo.WorkBreakdown.PRs.Actionable += deltaActionable
		if repo.WorkBreakdown.PRs.Hold < 0 {
			repo.WorkBreakdown.PRs.Hold = 0
		}
		if repo.WorkBreakdown.PRs.Actionable < 0 {
			repo.WorkBreakdown.PRs.Actionable = 0
		}
		return
	}
	repo.WorkBreakdown.Issues.Hold += deltaHold
	repo.WorkBreakdown.Issues.Actionable += deltaActionable
	if repo.WorkBreakdown.Issues.Hold < 0 {
		repo.WorkBreakdown.Issues.Hold = 0
	}
	if repo.WorkBreakdown.Issues.Actionable < 0 {
		repo.WorkBreakdown.Issues.Actionable = 0
	}
}

func repoHoldStatusRepoMatches(repo FrontendRepo, full string) bool {
	full = strings.ToLower(strings.TrimSpace(full))
	return strings.ToLower(strings.TrimSpace(repo.Full)) == full || strings.ToLower(strings.TrimSpace(repo.Name)) == full
}

func (s *Server) applyRepoHoldToStatus(repoFull string, number int, itemType string, held bool, label string, removed []string) uint64 {
	s.statusMu.Lock()
	s.statusMutationEpoch++
	minStatusSeq := s.statusSeq + 1
	status := s.status
	if status == nil {
		s.statusMu.Unlock()
		return minStatusSeq
	}

	for ri := range status.Repos {
		repo := &status.Repos[ri]
		if !repoHoldStatusRepoMatches(*repo, repoFull) {
			continue
		}
		var moved any
		var didMove bool
		if held {
			if itemType == "pr" {
				repo.OpenPrs, moved, didMove = repoHoldRemoveFirst(repo.OpenPrs, number)
				if didMove {
					moved = repoHoldSetItemLabels(moved, repoHoldLabelsAfter(moved, true, label, nil, label))
					repo.HeldPrs = append(repo.HeldPrs, moved)
				}
			} else {
				repo.ActionableIssues, moved, didMove = repoHoldRemoveFirst(repo.ActionableIssues, number)
				if didMove {
					repo.HeldIssues = append(repo.HeldIssues, repoHoldItemForSummary(moved, repoFull, number, itemType, label))
				}
			}
			if didMove && !repoHoldSummaryHas(status.Hold.Items, repoFull, number, itemType) {
				status.Hold.Items = append(status.Hold.Items, repoHoldItemForSummary(moved, repoFull, number, itemType, label))
				if itemType == "pr" {
					status.Hold.PRs++
				} else {
					status.Hold.Issues++
				}
				status.Hold.Total++
			}
		} else {
			if itemType == "pr" {
				repo.HeldPrs, moved, didMove = repoHoldRemoveFirst(repo.HeldPrs, number)
				if didMove {
					moved = repoHoldSetItemLabels(moved, repoHoldLabelsAfter(moved, false, label, removed, label))
					repo.OpenPrs = append(repo.OpenPrs, moved)
				}
			} else {
				repo.HeldIssues, moved, didMove = repoHoldRemoveFirst(repo.HeldIssues, number)
				if didMove {
					moved = repoHoldSetItemLabels(moved, repoHoldLabelsAfter(moved, false, label, removed, label))
					repo.ActionableIssues = append(repo.ActionableIssues, moved)
				}
			}
			if didMove {
				status.Hold.Items, _ = repoHoldRemoveSummary(status.Hold.Items, repoFull, number, itemType)
				if itemType == "pr" {
					status.Hold.PRs = max(0, status.Hold.PRs-1)
				} else {
					status.Hold.Issues = max(0, status.Hold.Issues-1)
				}
				status.Hold.Total = max(0, status.Hold.Total-1)
			}
		}
		if didMove {
			repoHoldAdjustBreakdown(repo, itemType, held)
		}
		break
	}

	s.statusSeq++
	status.StatusSeq = s.statusSeq
	status.StatusInstance = strconv.FormatInt(s.startedAt.UnixNano(), 10)
	status.Timestamp = time.Now().UTC().Format(time.RFC3339)
	if status.TimeZone == "" {
		status.TimeZone = dashboardTimeZoneName()
	}
	s.lastFullBroadcast = time.Now()
	data, err := json.Marshal(status)
	s.statusMu.Unlock()

	if err != nil {
		if s.logger != nil {
			s.logger.Warn("failed to marshal hold-updated status for SSE", "error", err)
		}
	} else {
		s.broadcastFrame(fmt.Sprintf("data: %s\n\n", data))
	}
	return minStatusSeq
}

func (s *Server) handleRepoHoldPermission(w http.ResponseWriter, r *http.Request) {
	owner := strings.TrimSpace(r.PathValue("owner"))
	repo := strings.TrimSpace(r.PathValue("repo"))
	if owner == "" || repo == "" || strings.Contains(owner, "/") || strings.Contains(repo, "/") {
		jsonError(w, "invalid repository", http.StatusBadRequest)
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repo)
	if !allowed && status != http.StatusForbidden {
		jsonError(w, msg, status)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "allowed": allowed})
}

func (s *Server) handleRepoItemHold(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repoName)
	if !allowed {
		jsonError(w, msg, status)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	var body repoHoldRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	canonical := s.canonicalHoldLabel()
	repoFull := owner + "/" + repoName
	var removed []string
	if body.Held {
		if err := s.deps.GHClient.EnsureIssueLabel(ctx, repoFull, canonical, "8250df", "Hive dashboard hold: agents will not act on this item until an operator removes this label."); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := s.deps.GHClient.AddLabels(ctx, repoFull, number, []string{canonical}); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.auditFromRequest(r, "repo_item_hold_add", auditDetail("repo", repoFull, "number", strconv.Itoa(number), "label", canonical), "")
	} else {
		labels, err := s.currentItemLabels(ctx, owner, repoName, number)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		removed = holdCausingLabels(labels, canonical)
		for _, label := range removed {
			if err := s.deps.GHClient.RemoveLabel(ctx, repoFull, number, label); err != nil {
				jsonError(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
		s.auditFromRequest(r, "repo_item_hold_remove", auditDetail("repo", repoFull, "number", strconv.Itoa(number), "labels", strings.Join(removed, ",")), "")
	}
	itemType := strings.ToLower(strings.TrimSpace(body.Type))
	if itemType != "pr" {
		itemType = "issue"
	}
	minStatusSeq := s.applyRepoHoldToStatus(repoFull, number, itemType, body.Held, canonical, removed)
	jsonResponse(w, repoHoldResponse{
		OK:           true,
		Repo:         repoFull,
		Number:       number,
		Held:         body.Held,
		HoldLabel:    canonical,
		Removed:      removed,
		MinStatusSeq: minStatusSeq,
	})
}
