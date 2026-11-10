package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Confirm a sentinel finding safe from the activity monitor's human-gate
// pill ("Confirm safe"). A sentinel alert is a human gate the hive never
// lifts on its own (api_repo_item_decide_delegated.go: "the sentinel label
// is cleared only by a person"), and until now the dashboard offered no
// way to settle it — the row showed `Approve` and `Let hive decide` but
// nothing that answered the alert itself. This endpoint is that answer: it
// posts the person's confirmation as a marker comment on the item and
// removes the configured sentinel label, in that order, so the audit trail
// on GitHub names who confirmed and why before the gate opens. Authorised
// like Hold and the decision endpoint (write permission on the repo or the
// verified owner role), attributed to the signed-in dashboard user in the
// audit log, idempotent when the label is already gone.

const (
	sentinelConfirmAuditAction  = "repo_item_sentinel_confirm"
	sentinelConfirmMarker       = "<!-- hive:sentinel-confirmed -->"
	sentinelConfirmMaxBody      = 16 << 10
	sentinelConfirmMaxComment   = 4000
	rowActionSentinelConfirmed  = "sentinel-confirmed"
	sentinelConfirmDefaultLabel = "sentinel-alert"
)

type repoSentinelConfirmRequest struct {
	Comment string `json:"comment"`
	Type    string `json:"type"`
}

type repoSentinelConfirmResponse struct {
	OK      bool     `json:"ok"`
	Repo    string   `json:"repo"`
	Number  int      `json:"number"`
	Label   string   `json:"label"`
	Removed []string `json:"removed"`
	// AlreadyClear reports that no sentinel label was on the item; the
	// confirmation comment is still posted so the record names the person.
	AlreadyClear bool `json:"alreadyClear"`
	// Result is the shared row-action outcome for the dashboard's toast.
	Result *RowActionResult `json:"result,omitempty"`
}

// sentinelConfirmComment is pure: the comment the confirmation posts. The
// marker lets the sentinel sweep and the delegate recognise a human
// confirmation; the text that follows is the operator's own words.
func sentinelConfirmComment(user, text string) string {
	body := sentinelConfirmMarker + "\n@" + user + " confirmed the sentinel finding safe from the dashboard."
	if t := strings.TrimSpace(text); t != "" {
		body += "\n\n" + t
	}
	return body
}

// sentinelLabelsOn is pure: the item's labels that are the sentinel label
// (case-insensitive), in their order on the item.
func sentinelLabelsOn(labels []string, sentinel string) []string {
	var out []string
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(sentinel)) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

func (s *Server) handleRepoItemSentinelConfirm(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repoName)
	if !allowed {
		jsonError(w, msg, status)
		return
	}
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if user == "" {
		jsonError(w, "GitHub user session required to confirm a sentinel finding", http.StatusUnauthorized)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	var body repoSentinelConfirmRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, sentinelConfirmMaxBody)).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	comment := strings.TrimSpace(body.Comment)
	if len(comment) > sentinelConfirmMaxComment {
		jsonError(w, fmt.Sprintf("comment exceeds %d characters", sentinelConfirmMaxComment), http.StatusBadRequest)
		return
	}
	itemType := strings.ToLower(strings.TrimSpace(body.Type))
	if itemType != rowActionTargetPR {
		itemType = rowActionTargetIssue
	}
	repoFull := owner + "/" + repoName
	if !s.watchesRepo(repoFull) {
		jsonError(w, "repository is not configured for this hive", http.StatusForbidden)
		return
	}
	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	sentinel := strings.TrimSpace(s.sentinelLabel())
	if sentinel == "" {
		sentinel = sentinelConfirmDefaultLabel
	}
	labels, err := s.currentItemLabels(ctx, owner, repoName, number)
	if err != nil {
		s.repoHoldGitHubError(w, "read labels", repoFull, number, sentinel, err)
		return
	}
	on := sentinelLabelsOn(labels, sentinel)
	// The comment first: should the label removal fail, GitHub still
	// carries who confirmed and why, and the retry is idempotent.
	if err := s.deps.GHClient.CreateIssueComment(ctx, repoFull, number, sentinelConfirmComment(user, comment)); err != nil { // writeback:solicited operator pressed Confirm safe on this row
		s.repoHoldGitHubError(w, "post confirmation comment", repoFull, number, sentinel, err)
		return
	}
	removed := []string{}
	for _, label := range on {
		if err := s.deps.GHClient.RemoveLabel(ctx, repoFull, number, label); err != nil { // writeback:solicited operator pressed Confirm safe on this row
			s.repoHoldGitHubError(w, "remove sentinel label", repoFull, number, label, err)
			return
		}
		removed = append(removed, label)
	}
	s.auditFromRequest(r, sentinelConfirmAuditAction, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "type", itemType, "label", sentinel, "removed", strings.Join(removed, ","), "comment", comment), "")
	s.invalidateStageTrackerForWebhook()
	target := rowActionTarget(repoFull, itemType, number)
	result := RowActionResult{Action: rowActionSentinelConfirmed, Target: target}
	if len(removed) > 0 {
		result.Outcome = "Sentinel finding confirmed safe by @" + user + ": `" + strings.Join(removed, "`, `") + "` removed and the confirmation commented"
		result.Next = "The gate is open: current review, CI, hold and queue requirements still apply."
	} else {
		result.Outcome = "Sentinel finding confirmed safe by @" + user + ": the confirmation is commented; no `" + sentinel + "` label was on the item"
		result.Next = "Nothing further to do for the sentinel; other gates still apply."
	}
	jsonResponse(w, repoSentinelConfirmResponse{OK: true, Repo: repoFull, Number: number, Label: sentinel, Removed: removed, AlreadyClear: len(on) == 0, Result: &result})
}
