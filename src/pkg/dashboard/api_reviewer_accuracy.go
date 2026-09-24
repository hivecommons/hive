package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

const (
	reviewerAccuracyDefaultWindowDays = 30
	reviewerAccuracyMaxWindowDays     = 90
	reviewerAccuracyWindowEnv         = "HIVE_REVIEWER_ACCURACY_WINDOW_DAYS"
)

func (s *Server) handleReviewerAccuracy(w http.ResponseWriter, r *http.Request) {
	days, err := reviewerAccuracyWindowDays(r.URL.Query().Get("days"))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	artifact, err := review.LoadArtifact("")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			jsonError(w, "review verdicts unreadable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		artifact = review.Artifact{}
	}
	ledger, err := review.LoadOutcomeLedger("")
	if err != nil {
		jsonError(w, "review outcomes ledger unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	window := time.Duration(days) * 24 * time.Hour
	resp := review.SummarizeReviewerAccuracy(time.Now(), window, artifact, ledger, s.reviewerAccuracyEvidence())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func reviewerAccuracyWindowDays(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		raw = os.Getenv(reviewerAccuracyWindowEnv)
	}
	if strings.TrimSpace(raw) == "" {
		return reviewerAccuracyDefaultWindowDays, nil
	}
	days, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || days < 1 || days > reviewerAccuracyMaxWindowDays {
		return 0, errReviewerAccuracyWindow{}
	}
	return days, nil
}

type errReviewerAccuracyWindow struct{}

func (errReviewerAccuracyWindow) Error() string {
	return "days must be an integer between 1 and " + strconv.Itoa(reviewerAccuracyMaxWindowDays)
}

func (s *Server) reviewerAccuracyEvidence() map[string]review.ReviewerAccuracyEvidence {
	out := map[string]review.ReviewerAccuracyEvidence{}
	actionable := s.lastActionableForPRModels()
	add := func(pr ghpkg.PullRequest) {
		if pr.Repo == "" || pr.Number <= 0 {
			return
		}
		ev := review.ReviewerAccuracyEvidence{
			ReworkedAfterApproval:     pr.Rework.HumanChangeRequests > 0 || pr.Rework.FixAttempts > 0 || pr.Rework.FollowUpCommits > 0,
			MergedUnchangedAfterBlock: !pr.MergedAt.IsZero() && !pr.Rework.FirstReviewAt.IsZero() && pr.Rework.FixAttempts == 0 && pr.Rework.FollowUpCommits == 0,
		}
		if !ev.ReworkedAfterApproval && !ev.MergedUnchangedAfterBlock && !ev.MaintainerOverride {
			return
		}
		out[pr.Repo+"#"+strconv.Itoa(pr.Number)] = ev
		if !strings.Contains(pr.Repo, "/") && s != nil && s.deps != nil && s.deps.Config != nil {
			full := strings.Trim(strings.TrimSpace(s.deps.Config.Project.Org)+"/"+strings.TrimSpace(pr.Repo), "/")
			if strings.Contains(full, "/") {
				out[full+"#"+strconv.Itoa(pr.Number)] = ev
			}
		}
	}
	for _, pr := range actionable.PRs.Attributed {
		add(pr)
	}
	for _, pr := range actionable.PRs.Items {
		add(pr)
	}
	return out
}
