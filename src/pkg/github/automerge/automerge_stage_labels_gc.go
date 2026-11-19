package automerge

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Model-label garbage collection (hivecommons/hive#17620, RFC phase 4
// follow-up). ensureStageLabels creates a bound model's label vocabulary in
// the repository; this file remembers what it created and, when the
// repository is unbound or rebound to a model with a different label set,
// deletes the labels that went stale — and only those:
//
//   - the label is in the repository's provisioned record, i.e. this hive
//     created (or adopted by recolouring) it for a model; a label the
//     repository had on its own is never touched;
//   - it is not in the built-in stage family (stagelabels.LabelDefsFor and
//     the retired spellings in stagelabels.Renames);
//   - no open issue or PR wears it (one search per label; a failed search
//     keeps the label);
//   - workflow.label_gc is on (stageengine.Options.WorkflowLabelGC).
//
// A label kept for any of these reasons stays in the record so a later
// rebinding retries it. Every deletion is audited as
// AuditActionWorkflowLabelRemoved through the transport. With no
// ProvisionedStore wired nothing is remembered and nothing is deleted, so
// an unbound repository on a hive without the workflow store sees no
// change at all.

// workflowLabelAuditor is the optional Transport hook that records one
// model-label deletion in the audit log (hgithub.Client implements it).
type workflowLabelAuditor interface {
	RecordWorkflowLabelRemoved(repo, label, model string)
}

// workflowLabelGCOutcome is what one reconcile pass did, for tests and
// the log line.
type workflowLabelGCOutcome struct {
	Removed []string
	Kept    []string
}

// workflowProvisionedStore is the per-repository label memory, or nil.
func (c *Engine) workflowProvisionedStore() workflowmodel.ProvisionedStore {
	if c == nil || c.stageEngine == nil {
		return nil
	}
	return c.stageEngine.WorkflowProvisioned
}

// workflowLabelGCEnabled is the live workflow.label_gc (default true).
func (c *Engine) workflowLabelGCEnabled() bool {
	if c == nil || c.stageEngine == nil || c.stageEngine.WorkflowLabelGC == nil {
		return true
	}
	return c.stageEngine.WorkflowLabelGC()
}

// reconcileWorkflowLabels runs after the model's labels are provisioned:
// it diffs the repository's provisioned record against the current
// binding (p and its model label defs), deletes what is stale and safe to
// delete, and rewrites the record. existing is the live label list
// ensureStageLabels fetched (lower-cased name → label) and is updated in
// place. Errors are logged, never returned: provisioning already
// succeeded and a failed clean-up must not stop the stage pass.
func (c *Engine) reconcileWorkflowLabels(ctx context.Context, owner, repo string, p *workflowmodel.Profile, model []workflowmodel.LabelDef, existing map[string]*gh.Label, kinds authorkind.Names) workflowLabelGCOutcome {
	var out workflowLabelGCOutcome
	store := c.workflowProvisionedStore()
	if store == nil {
		return out
	}
	full := strings.ToLower(owner + "/" + repo)
	prev, err := store.LoadProvisioned(full)
	if err != nil {
		c.warn("workflow label memory unreadable; model labels left in place", "repo", full, "err", err)
		return out
	}
	if prev == nil && p == nil {
		return out
	}
	current := map[string]bool{}
	keep := make([]string, 0, len(model))
	for _, def := range model {
		current[strings.ToLower(def.Name)] = true
		keep = append(keep, def.Name)
	}
	builtin := map[string]bool{}
	for _, def := range stagelabels.LabelDefsFor(kinds) {
		builtin[strings.ToLower(def.Name)] = true
	}
	for old, stage := range stagelabels.Renames {
		builtin[strings.ToLower(old)] = true
		builtin[strings.ToLower(stage.Label())] = true
	}
	modelKey := "unbound"
	if p != nil {
		modelKey = p.Name + "@" + p.Version
	}
	if prev != nil {
		seen := map[string]bool{}
		for _, name := range prev.Labels {
			lower := strings.ToLower(name)
			if current[lower] || builtin[lower] || seen[lower] {
				continue
			}
			seen[lower] = true
			live, present := existing[lower]
			if !present {
				continue
			}
			if reason := c.deleteStaleWorkflowLabel(ctx, owner, repo, live.GetName(), modelKey); reason != "" {
				c.warn("stale workflow model label left in place", "repo", full, "label", live.GetName(), "workflow", modelKey, "reason", reason)
				keep = append(keep, live.GetName())
				out.Kept = append(out.Kept, live.GetName())
				continue
			}
			delete(existing, lower)
			out.Removed = append(out.Removed, live.GetName())
		}
	}
	if len(out.Removed) > 0 {
		c.info("stale workflow model labels removed", "repo", full, "workflow", modelKey, "labels", strings.Join(out.Removed, ","))
	}
	if p == nil && len(keep) == 0 {
		if err := store.DeleteProvisioned(full); err != nil {
			c.warn("workflow label memory not cleared", "repo", full, "err", err)
		}
		return out
	}
	rec := workflowmodel.ProvisionedLabels{Repo: full, Labels: keep, UpdatedAt: c.now()}
	if p != nil {
		rec.Name, rec.Version, rec.Hash = p.Name, p.Version, p.Hash
	}
	if err := store.SaveProvisioned(rec); err != nil {
		c.warn("workflow label memory not written", "repo", full, "err", err)
	}
	return out
}

// deleteStaleWorkflowLabel deletes name from the repository when the knob
// allows and nothing open wears it; it returns "" on deletion and the
// reason the label was kept otherwise.
func (c *Engine) deleteStaleWorkflowLabel(ctx context.Context, owner, repo, name, modelKey string) string {
	if !c.workflowLabelGCEnabled() {
		return "workflow.label_gc is off"
	}
	inUse, err := c.workflowLabelInUse(ctx, owner, repo, name)
	if err != nil {
		return "search failed: " + err.Error()
	}
	if inUse > 0 {
		return fmt.Sprintf("worn by %d open item(s)", inUse)
	}
	if _, err := c.gh.Issues.DeleteLabel(ctx, owner, repo, name); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
		return "delete failed: " + err.Error()
	}
	if a, ok := c.transport.(workflowLabelAuditor); ok {
		a.RecordWorkflowLabelRemoved(owner+"/"+repo, name, modelKey)
	}
	return ""
}

// workflowLabelInUse counts the open issues and PRs wearing name.
func (c *Engine) workflowLabelInUse(ctx context.Context, owner, repo, name string) (int, error) {
	q := fmt.Sprintf(`repo:%s/%s is:open label:"%s"`, owner, repo, strings.ReplaceAll(name, `"`, `\"`))
	res, _, err := c.gh.Search.Issues(ctx, q, &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: 1}})
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, nil
	}
	return res.GetTotal(), nil
}

// EnsureStageLabels provisions owner/repo's label vocabulary now — the
// built-in family plus the bound workflow model's LabelDefs — through the
// same memoised path the sweep's stage pass uses (ensureStageLabels), so a
// dashboard action that turns model-based derivation on can create the
// bound models' labels immediately instead of waiting for the next PR that
// needs a label write. The shared tracker's memo is honoured and updated:
// a repository already provisioned for the current profile hash is a
// no-op, and the sweep will not re-list it afterwards.
func (c *Engine) EnsureStageLabels(ctx context.Context, owner, repo string) error {
	if c == nil || c.gh == nil {
		return fmt.Errorf("stage labels: no GitHub transport")
	}
	return c.ensureStageLabels(ctx, owner, repo, c.stageTrackerOrShared(), c.stagePolicy().AuthorKindLabels)
}
