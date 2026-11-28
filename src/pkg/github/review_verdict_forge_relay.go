package github

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review"
)

// NewReviewVerdictClient returns a Client with no GitHub REST client, for a
// request relay on another forge that records review verdicts
// (hivecommons/hive#20292). Every PR read the verdict writer and the
// review.require_labels gate make goes through reader. Only
// ServeRecordVerdict and the setters it reads are meant for it: every other
// method either needs GitHub or does nothing.
func NewReviewVerdictClient(reader ReviewedPRReader, logger *slog.Logger) *Client {
	c := &Client{logger: logger}
	c.SetReviewedPRReader(reader)
	return c
}

// RecordVerdictOutcome is how a relay settles one record_verdict request:
// the result to write next to it, and the suffix the request is renamed
// with ("" removes it). Retry marks a transient failure the request is kept
// for, with Suffix empty.
type RecordVerdictOutcome struct {
	Response ReviewResponse
	Suffix   string
	Retry    bool
}

// ServeRecordVerdict admits and records one record_verdict request for a
// relay with no GitHub REST client. The checks are the review watcher's
// (handleOneReviewRequest) for that event, in the same order: shape and
// verdict validation, the review authorizer on the request file's owning
// UID, the lane write allowlist, the repo pause, review.require_labels, and
// then the verdict writer's own repo and number match, dispatch binding and
// advisory rule. Keep the two in step.
//
// As on GitHub, a verdict the writer discards (no dispatch and not a
// reviewer, the PR's author, no head SHA) still answers OK: the request was
// admitted, and the writer logs why nothing was recorded.
func (c *Client) ServeRecordVerdict(ctx context.Context, req ReviewRequest, fileUID int, authz ReviewRequestAuthorizer, now time.Time) RecordVerdictOutcome {
	stamp := now.UTC().Format(time.RFC3339)
	refuse := func(suffix, msg string) RecordVerdictOutcome {
		return RecordVerdictOutcome{Response: ReviewResponse{OK: false, Number: req.Number, Error: msg, At: stamp}, Suffix: suffix}
	}
	deny := func(reason string) RecordVerdictOutcome {
		c.logger.Warn("review verdict relay: DENIED (policy)",
			slog.String("agent", req.Agent), slog.String("repo", req.Repo),
			slog.Int("number", req.Number), slog.String("reason", reason))
		return refuse(".denied", "authorization denied: "+reason)
	}

	var shapeErr string
	switch {
	case !strings.EqualFold(strings.TrimSpace(req.Event), ReviewEventRecordVerdict):
		shapeErr = "this relay serves only record_verdict review requests"
	case strings.TrimSpace(req.Repo) == "" || req.Number <= 0:
		shapeErr = "review request requires repo and number"
	case strings.TrimSpace(req.Report) == "":
		shapeErr = "record_verdict requires a verdict report"
	case len(req.Comments) > 0:
		shapeErr = "inline comments are only valid on a PR-level review (approve|request_changes|comment)"
	}
	if shapeErr == "" {
		if _, err := review.ValidateReportsFor([]byte(strings.TrimSpace(req.Report)), c.perspectives); err != nil {
			shapeErr = malformedVerdictRefusal(err)
		}
	}
	if shapeErr != "" {
		return refuse(".bad", shapeErr)
	}

	if authz == nil {
		return deny("no authorizer configured (fail closed)")
	}
	if err := authz(req.Agent, fileUID); err != nil {
		return deny(err.Error())
	}
	if reason, refused := c.refuseWrite(req.Agent, WriteOpReview, req.Repo, req.Number); refused {
		return deny(reason)
	}
	if c.RepoIsPaused(req.Repo) {
		return deny(RepoPausedReason(req.Repo))
	}
	if admitted, err := c.reviewLabelGateAdmits(ctx, req.Repo, req.Number); err != nil {
		out := refuse("", "review.require_labels: could not read PR labels: "+err.Error())
		out.Retry = true
		return out
	} else if !admitted {
		c.recordWriteAudit(AuditActionReviewLabelGateRefused, c.attributionMeta(req.Agent),
			WriteTarget{Repo: req.Repo, Number: req.Number},
			"event", strings.TrimSpace(req.Event), "require_labels", strings.Join(c.reviewRequireLabels(), ","))
		return deny(config.ReviewRequireLabelsSkipReason + " (" + strings.Join(c.reviewRequireLabels(), ", ") + ")")
	}

	c.recordReviewEvidence(ctx, req, c.recordReviewVerdict(ctx, req, ""), nil, now)
	c.logger.Info("review verdict relay: verdict request served",
		slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("agent", req.Agent))
	return RecordVerdictOutcome{Response: ReviewResponse{OK: true, Number: req.Number, State: ReviewEventRecordVerdict, At: stamp}}
}
