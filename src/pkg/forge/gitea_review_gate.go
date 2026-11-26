package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/reviewgate"
)

// Open change-request gate on Forgejo / Gitea (hivecommons/hive#20067). The
// review states match GitHub's under other spellings (REQUEST_CHANGES,
// dismissed=true), so the rules are pkg/reviewgate's: a writer's latest
// opinionated review that requests changes refuses a merge and an approval
// label until that reviewer approves or the review is dismissed.

// gtReview is the subset of a Gitea pull request review the gate reads.
type gtReview struct {
	ID          int64      `json:"id"`
	User        *gtUser    `json:"user"`
	State       string     `json:"state"`
	CommitID    string     `json:"commit_id"`
	SubmittedAt *time.Time `json:"submitted_at"`
	Dismissed   bool       `json:"dismissed"`
	HTMLURL     string     `json:"html_url"`
}

// gtPermission is the collaborator permission answer.
type gtPermission struct {
	Permission string `json:"permission"`
	RoleName   string `json:"role_name"`
}

// giteaApprovalLabels are the labels that approve a PR for merging.
var giteaApprovalLabels = []string{"lgtm", "approved"}

// reviews lists every review on owner/name#number. A 404 (not a pull
// request) is no reviews.
func (f *giteaForge) reviews(ctx context.Context, owner, name string, number int) ([]reviewgate.Review, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", url.PathEscape(owner), url.PathEscape(name), number)
	var out []reviewgate.Review
	err := f.paginate(ctx, endpoint, nil, func(body []byte) (int, error) {
		var page []gtReview
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("decoding reviews: %w", err)
		}
		for _, rv := range page {
			r := reviewgate.Review{ID: rv.ID, State: rv.State, CommitID: rv.CommitID, Dismissed: rv.Dismissed, URL: rv.HTMLURL}
			if rv.User != nil {
				r.Reviewer = rv.User.Login
			}
			if rv.SubmittedAt != nil {
				r.SubmittedAt = *rv.SubmittedAt
			}
			out = append(out, r)
		}
		return len(page), nil
	})
	if isGiteaStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	return out, err
}

// writerCheck answers reviewgate.WriterFunc from the collaborator permission
// endpoint: owner, admin and write count; a 404 is not a writer.
func (f *giteaForge) writerCheck(owner, name string) reviewgate.WriterFunc {
	return func(ctx context.Context, login string) (bool, error) {
		endpoint := fmt.Sprintf("/repos/%s/%s/collaborators/%s/permission", url.PathEscape(owner), url.PathEscape(name), url.PathEscape(login))
		var p gtPermission
		if err := f.getJSON(ctx, endpoint, nil, &p); err != nil {
			if isGiteaStatus(err, http.StatusNotFound) {
				return false, nil
			}
			return false, err
		}
		return reviewgate.WriteOrAbove(p.Permission) || reviewgate.WriteOrAbove(p.RoleName), nil
	}
}

// gateChangesRequested refuses with a *reviewgate.BlockedError while
// owner/name#number has an open change request from a writer that no
// operator override covers, or when its reviews cannot be read. Each gated
// attempt is logged with the reviewer and the review ID.
func (f *giteaForge) gateChangesRequested(ctx context.Context, owner, name string, number int, action string) error {
	slug := owner + "/" + name
	reviews, err := f.reviews(ctx, owner, name, number)
	if err != nil {
		slog.Default().Warn("could not read reviews; refusing to approve, label or merge",
			"forge", f.label, "repo", slug, "pr", number, "action", action, "outcome", reviewgate.OutcomeUnreadable, "error", err)
		return &reviewgate.BlockedError{Repo: slug, Number: number, Cause: err}
	}
	d := reviewgate.Decide(slug, number, reviewgate.Evaluate(ctx, reviews, f.writerCheck(owner, name)))
	if len(d.Blocks) == 0 {
		return nil
	}
	args := []any{"forge", f.label, "repo", slug, "pr", number, "action", action,
		"reviewers", reviewgate.Reviewers(d.Blocks), "review_ids", reviewgate.ReviewIDs(d.Blocks)}
	if d.Allowed() {
		slog.Default().Warn("open change request overridden by operator decision",
			append(args, "outcome", reviewgate.OutcomeOverridden, "actor", d.Override.Actor, "reason", d.Override.Reason)...)
		return nil
	}
	slog.Default().Warn("open change request blocks hive approval, label or merge", append(args, "outcome", reviewgate.OutcomeBlocked)...)
	return &reviewgate.BlockedError{Repo: slug, Number: number, Blocks: d.Blocks}
}

// approvesForMerge reports whether labels include an approval label.
func approvesForMerge(labels []string) bool {
	for _, l := range labels {
		for _, a := range giteaApprovalLabels {
			if strings.EqualFold(strings.TrimSpace(l), a) {
				return true
			}
		}
	}
	return false
}
