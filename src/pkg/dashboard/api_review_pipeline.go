package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/pipeline"
)

// reviewPipelineResponse is one page of review pipeline cards.
type reviewPipelineResponse struct {
	GeneratedAt time.Time       `json:"generated_at"`
	SnapshotAt  time.Time       `json:"snapshot_at,omitzero"`
	Total       int             `json:"total"`
	Limit       int             `json:"limit"`
	Offset      int             `json:"offset"`
	HasMore     bool            `json:"has_more"`
	Cards       []pipeline.Card `json:"cards"`
}

// handleReviewPipeline serves
// GET /api/review/pipeline?repo=R&stage=S&limit=N&offset=M: one card per PR in
// the review queue, placed in its review pipeline stage by pipeline.Derive
// (hivecommons/hive#11086). Cards keep review-queue order; repo and stage
// filter before paging, and paging follows /api/review/queue.
//
// Like the queue it never calls GitHub: it reads the last enumeration
// snapshot, the verdict artifact, the review dispatch state and the
// review-links ledger. The queue holds open PRs only, so merged and abandoned
// cards do not appear here yet.
func (s *Server) handleReviewPipeline(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := reviewQueuePaging(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	repoFilter := strings.TrimSpace(r.URL.Query().Get("repo"))
	var stageFilter *pipeline.Stage
	if v := strings.TrimSpace(r.URL.Query().Get("stage")); v != "" {
		st, parseErr := pipeline.ParseStage(v)
		if parseErr != nil {
			jsonError(w, "invalid stage: "+parseErr.Error(), http.StatusBadRequest)
			return
		}
		stageFilter = &st
	}

	artifact, err := review.LoadArtifact("")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			jsonError(w, "review verdicts unreadable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		artifact = review.Artifact{}
	}
	dispatch, err := review.LoadDispatchState("")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			jsonError(w, "review dispatch state unreadable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		dispatch = review.DispatchState{}
	}
	links, err := ghpkg.LoadReviewLinks("")
	if err != nil {
		jsonError(w, "review links unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}

	opts := ghpkg.ReviewQueueOptions{
		Verdicts:     artifact,
		ChangedPaths: ghpkg.CachedPRChangedPaths,
		Now:          time.Now(),
	}
	if s != nil && s.deps != nil && s.deps.Config != nil {
		opts.Org = s.deps.Config.Project.Org
		opts.AIAuthor = s.deps.Config.EffectiveAIAuthor()
	}
	actionable := s.lastActionableForPRModels()
	queue := ghpkg.ReviewQueueFromActionable(actionable, opts)

	cards := make([]pipeline.Card, 0, len(queue))
	for _, entry := range queue {
		if repoFilter != "" && !pipeline.SameRepo(repoFilter, entry.Repo) {
			continue
		}
		in := pipeline.Inputs{
			PR:       entry,
			Verdicts: artifact.Items,
			Dispatch: dispatch,
		}
		if link, ok := lookupReviewLink(links, entry.Repo, reviewPipelineBareRepo(entry.Repo), entry.Number); ok {
			in.ReviewLink = &link
		}
		card := pipeline.Derive(in)
		card.Trigger = s.reviewCardTrigger(card)
		if stageFilter != nil && card.Stage != *stageFilter {
			continue
		}
		cards = append(cards, card)
	}

	resp := reviewPipelineResponse{
		GeneratedAt: opts.Now.UTC(),
		SnapshotAt:  actionable.GeneratedAt,
		Total:       len(cards),
		Limit:       limit,
		Offset:      offset,
		Cards:       []pipeline.Card{},
	}
	if offset < len(cards) {
		end := min(offset+limit, len(cards))
		resp.Cards = cards[offset:end]
	}
	resp.HasMore = offset+len(resp.Cards) < len(cards)
	jsonResponse(w, resp)
}

func reviewPipelineBareRepo(full string) string {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[i+1:]
	}
	return full
}

// reviewLoopMarker stamps the loop-safety comment so a repeated click does not
// post a second one, like the sentinel marker.
const reviewLoopMarker = "<!-- hive-review-loop -->"

func reviewLoopComment(reason, user string) string {
	return reviewLoopMarker + "\n## Sent to a human\n\nThe automatic review/fix loop is stopping here: " +
		reason + ". @" + user + " handed this PR to a human with the `" + config.DefaultNeedsHumanLabel + "` label."
}

// handleReviewPipelineSendToHuman serves
// POST /api/review/pipeline/{owner}/{repo}/{number}/send-to-human: the
// one-click action behind a card's loop_warning (hivecommons/hive#11090). It
// applies needs-human and posts one marker comment saying why, and is safe to
// repeat. Body: {"reason": "..."} (optional).
func (s *Server) handleReviewPipelineSendToHuman(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if !requireMergerOrOwnerRole(w, r) {
		return
	}
	if user == "" {
		jsonError(w, "authenticated GitHub user required", http.StatusForbidden)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		jsonError(w, "invalid pull request number", http.StatusBadRequest)
		return
	}
	owner := strings.TrimSpace(r.PathValue("owner"))
	repoName := strings.TrimSpace(r.PathValue("repo"))
	if owner == "" || repoName == "" || strings.Contains(owner, "/") || strings.Contains(repoName, "/") {
		jsonError(w, "invalid repository", http.StatusBadRequest)
		return
	}
	repo := owner + "/" + repoName
	if !s.prQueueRepoAllowed(repo) {
		jsonError(w, "repository is not managed by this hive", http.StatusForbidden)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			jsonError(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "the review loop is at its safety cap"
	}

	gh := s.deps.GHClient
	if err := gh.AddLabels(r.Context(), repo, number, []string{config.DefaultNeedsHumanLabel}); err != nil {
		jsonError(w, fmt.Sprintf("applying %s: %v", config.DefaultNeedsHumanLabel, err), http.StatusBadGateway)
		return
	}
	posted, err := gh.IssueCommentsContain(r.Context(), repo, number, reviewLoopMarker)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !posted {
		if err := gh.CreateIssueComment(r.Context(), repo, number, reviewLoopComment(reason, user)); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	s.auditFromRequest(r, "review-loop-send-to-human", auditDetail("repo", repo, "pr", strconv.Itoa(number), "reason", reason), "")
	jsonResponse(w, map[string]any{
		"status": "sent_to_human",
		"repo":   repo,
		"number": number,
		"label":  config.DefaultNeedsHumanLabel,
		"posted": !posted,
	})
}
