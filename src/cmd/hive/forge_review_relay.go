package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The forge relay's review queue (hivecommons/hive#20292).
//
// A reviewer on a forge-only hive records its verdict with a record_verdict
// review request, which writes nothing to the forge: the verdict lands in
// the report dir review.Collect reads, and the eval cycle aggregates it into
// the [hive-reviewed: ...] annotation. Without it the reviewer could never
// say it had judged a PR, so the same PR came back on every kick.
//
// record_verdict is served by the GitHub review watcher's own code
// (github.Client.ServeRecordVerdict) on a client with no GitHub REST client,
// whose PR reads go through the repo's forge (forgeReviewedPRReader). Every
// admission check of the GitHub path holds: the review authorizer on the
// request file's owning UID, the lane write allowlist, the repo pause,
// review.require_labels, the verdict's repo and number, the dispatch binding
// and the advisory rule. The repo must also pass the forge relay's
// repoRefusal, as every other request kind here does.
//
// approve, request_changes, comment and the thread events post to the forge
// and are still answered with forgeRelayReviewUnsupported.

// forgeReviewedPRReader reads the reviewed PR through the repo's forge read
// adapter (forge.ReadAPI.PullView) for the verdict writer.
type forgeReviewedPRReader struct {
	cfg func() *config.Config
}

func (r forgeReviewedPRReader) ReviewedPR(ctx context.Context, repo string, number int) (github.ReviewedPR, error) {
	cfg := r.cfg()
	if cfg == nil {
		return github.ReviewedPR{}, errors.New("reviewed PR: no config")
	}
	canonical := canonicalManagedRepo(cfg, repo)
	if canonical == "" {
		return github.ReviewedPR{}, fmt.Errorf("reviewed PR: repo %q is not managed by this hive", repo)
	}
	api, err := forgeReadAPIForRepo(cfg, canonical)
	if err != nil {
		return github.ReviewedPR{}, err
	}
	pv, err := api.PullView(ctx, canonical, number)
	if err != nil {
		return github.ReviewedPR{}, err
	}
	return github.ReviewedPR{
		HeadSHA: strings.TrimSpace(pv.HeadSHA),
		Title:   pv.Title,
		Body:    pv.Body,
		Author:  pv.Author,
		Base:    pv.Base,
		Labels:  pv.Labels,
	}, nil
}

// newForgeVerdictClient builds the REST-less client the review queue records
// verdicts through.
func newForgeVerdictClient(cfg func() *config.Config, logger *slog.Logger) *github.Client {
	return github.NewReviewVerdictClient(forgeReviewedPRReader{cfg: cfg}, logger)
}

// withReviewRelay turns on record_verdict on the review queue with authz,
// the manager's review authorizer, and wake, told when a verdict bound to a
// review dispatch is recorded. A relay without it denies every verdict
// (fail closed).
func (r *forgeRequestRelay) withReviewRelay(authz github.ReviewRequestAuthorizer, wake github.ReviewVerdictHook) *forgeRequestRelay {
	r.reviewAuthz = authz
	r.verdicts.SetReviewVerdictHook(wake)
	return r
}

// syncForgeVerdictClient installs the review settings the verdict writer
// reads from cfg, on every request so a reload applies to the next one.
func (r *forgeRequestRelay) syncForgeVerdictClient(cfg *config.Config) {
	c := r.verdicts
	installReviewVerdictSettings(c, cfg, r.logger)
	c.SetWriteAllowlistFunc(cfg.AgentMayWrite)
	c.SetRepoPausedFunc(cfg.IsRepoPaused)
	c.SetReviewRequireLabels(func() []string { return cfg.Review.RequireLabels })
	c.SetHiveIdentity(hiveIdentity(cfg))
	if r.pushAudit != nil {
		c.SetAttributionAuditRecord(r.pushAudit)
	}
}

// handleReviewRequest serves a record_verdict review request and answers
// every other review event with a reason, so the agent stops waiting on it.
func (r *forgeRequestRelay) handleReviewRequest(ctx context.Context, path string) {
	var req github.ReviewRequest
	fail := func(msg string) {
		writeRelayResult(path, github.ReviewResponse{OK: false, Number: req.Number, Error: msg, At: r.stamp()})
	}
	if !r.readRequest(path, &req, fail) {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(req.Event), github.ReviewEventRecordVerdict) {
		fail(forgeRelayReviewUnsupported)
		r.settle(path, ".rejected", false)
		r.logger.Warn("forge request relay: review request REJECTED (not served on a forge-only hive)", "agent", req.Agent, "repo", req.Repo, "number", req.Number, "event", req.Event)
		return
	}
	cfg := r.cfg()
	if reason := repoRefusal(cfg, req.Agent, req.Repo); reason != "" {
		fail("review repo rejected: " + reason)
		r.settle(path, ".rejected", false)
		return
	}
	r.syncForgeVerdictClient(cfg)
	out := r.verdicts.ServeRecordVerdict(ctx, req, github.RequestFileOwnerUID(path), r.reviewAuthz, r.now())
	writeRelayResult(path, out.Response)
	r.settle(path, out.Suffix, out.Retry)
	if !out.Response.OK {
		r.logger.Warn("forge request relay: verdict request refused", "agent", req.Agent, "repo", req.Repo, "number", req.Number, "error", out.Response.Error)
	}
}
