package releasesentinel

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Pre-tag failures (hivecommons/hive#9585, phase 2).
//
// A release can fail before any v<version> tag exists: #5875 (Actions was not
// permitted to open the release PR) and #6804 (the release-gate push needed a
// workflows permission) both failed inside the release workflow itself, so
// there was no tag to scope a record to. The sentinel watches the latest
// completed run of each configured release workflow and classifies a
// blocking one with the same classifier as a tagged release:
//
//   - policy (a setting, permission or secret): escalated to a human once,
//     and not again until a later run of that workflow succeeds.
//   - fixable: left to the normal ci-maintainer CI-failure path. The sentinel
//     never dispatches a round from this path, so it cannot double-dispatch a
//     failure another lane already owns.

// KindPreTag is Record.Kind for a release workflow that failed before a tag.
const KindPreTag = "pre_tag"

// preTagKeyPrefix namespaces pre-tag records in the state file so they can
// never collide with a v<version> key.
const preTagKeyPrefix = "pre-tag:"

func preTagKey(workflow string) string {
	return preTagKeyPrefix + strings.ToLower(strings.TrimSpace(workflow))
}

// PreTagSource lists release-workflow runs. It only reads.
type PreTagSource interface {
	// LatestReleaseWorkflowRuns returns the latest completed run of each
	// workflow in names (matched against the workflow name or file name).
	// Run.Name is the workflow's name.
	LatestReleaseWorkflowRuns(ctx context.Context, names []string) ([]Run, error)
}

// evaluatePreTag classifies the latest run of every release workflow. It
// mutates records and res; it never dispatches.
func (s *Sentinel) evaluatePreTag(ctx context.Context, now time.Time, tag Tag, hasTag bool, records map[string]*Record, res *Result) error {
	runs, err := s.preTag.LatestReleaseWorkflowRuns(ctx, s.opts.ReleaseWorkflows)
	if err != nil {
		return fmt.Errorf("list release workflow runs: %w", err)
	}
	for _, r := range runs {
		if !IsCompleted(r.Status) || strings.TrimSpace(r.Name) == "" {
			continue
		}
		if hasTag && r.HeadSHA == tag.SHA {
			// A run on the tagged commit belongs to the tag's own record.
			continue
		}
		key := preTagKey(r.Name)
		rec := records[key]
		if !IsBlockingConclusion(r.Conclusion) {
			if rec != nil && rec.State != StateGreen {
				rec.RunID, rec.SHA = r.ID, r.HeadSHA
				rec.Escalated, rec.EscalationReason, rec.BlockingRuns = false, "", nil
				rec.transition(now, StateGreen, fmt.Sprintf("release workflow run %d concluded %s", r.ID, r.Conclusion))
			}
			continue
		}
		if rec != nil && rec.RunID == r.ID {
			// Already classified this run.
			continue
		}
		d, err := s.source.Details(ctx, r.ID)
		if err != nil {
			return fmt.Errorf("fetch evidence for release workflow run %d: %w", r.ID, err)
		}
		if rec == nil {
			rec = &Record{Kind: KindPreTag, Repo: s.opts.Repo, Workflow: r.Name, State: StateAwaitingCI, FirstSeen: now}
			records[key] = rec
		}
		br := BlockingRun{Run: r, RunDetails: d}
		rec.RunID, rec.SHA, rec.BlockingRuns = r.ID, r.HeadSHA, refs([]Run{r})
		class, why := Classify(br)
		if class != ClassPolicy {
			rec.Escalated, rec.EscalationReason = false, ""
			rec.transition(now, StateAwaitingCI, fmt.Sprintf("fixable pre-tag failure in run %d, left to the CI-failure path", r.ID))
			res.PreTagFixable = append(res.PreTagFixable, r.Name)
			continue
		}
		if rec.State == StateFailed && rec.Escalated {
			// The same setting is still blocking every run; a human was told
			// on the first one. Do not page again for each re-run.
			rec.UpdatedAt = now
			continue
		}
		rec.Escalated = true
		rec.EscalationReason = EscalationPolicy
		rec.transition(now, StateFailed, string(EscalationPolicy)+": "+why)
		s.escalator.Escalate(ctx, Escalation{
			Repo: s.opts.Repo, SHA: r.HeadSHA, Reason: EscalationPolicy, Detail: why,
			Blocking: []BlockingRun{br}, Workflow: r.Name,
		})
		res.PreTagEscalated = append(res.PreTagEscalated, r.Name)
	}
	return nil
}
