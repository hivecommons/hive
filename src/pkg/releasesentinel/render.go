package releasesentinel

import (
	"fmt"
	"strings"
	"time"
)

const (
	// maxRenderedRuns bounds how many blocking runs one kick or notification
	// details; the rest are counted.
	maxRenderedRuns = 5
	// maxRenderedEvidence bounds evidence lines per run.
	maxRenderedEvidence = 3
)

// RenderRepairKick renders the targeted kick a repair round delivers. It is
// answerable on its own: it names the tag, the SHA, every blocking run with
// its failed step and evidence, the round budget and the deadline.
//
// The instructions deliberately route the fix through the normal PR path and
// forbid touching the tag: in this phase the sentinel never pushes or moves a
// tag, so a merged fix reaches users as the next patch release, which
// supersedes this one.
func RenderRepairKick(req RepairRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚀 RELEASE REPAIR - %s %s CI is red (round %d/%d)\n\n", req.Repo, req.Tag, req.Round, req.MaxRounds)
	fmt.Fprintf(&b, "The release sentinel found blocking workflow runs on the commit %s points at (%s).\n", req.Tag, shortSHA(req.SHA))
	b.WriteString("Repair it BEFORE claiming new issues or opening any other PR:\n")
	fmt.Fprintf(&b, "  1. Read the failing logs: gh run view <run-id> --repo %s --log-failed\n", req.Repo)
	b.WriteString("  2. Fix the cause on a new branch cut from the release branch, commit -s, and open a PR with hive-open-pr.\n")
	b.WriteString("     The merged fix ships as the next patch release, which supersedes this one.\n")
	b.WriteString("  3. Do NOT move, delete or re-create the tag, and do NOT push to the release branch directly.\n")
	b.WriteString("  4. If the cause is an org/repo setting, a token permission or a missing secret, do NOT attempt a\n")
	b.WriteString("     code workaround: report it as needs-human and stop.\n")
	fmt.Fprintf(&b, "\nround deadline: %s\n", req.Deadline.UTC().Format(time.RFC3339))
	writeRuns(&b, req.Blocking)
	return b.String()
}

// RenderEscalation renders the human notification for a failed release.
func RenderEscalation(e Escalation) (title, body string) {
	switch e.Reason {
	case EscalationPolicy:
		title = fmt.Sprintf("Release %s %s blocked by a setting an agent cannot fix", e.Repo, e.Tag)
	default:
		title = fmt.Sprintf("Release %s %s still red after %d repair round(s)", e.Repo, e.Tag, e.Round)
	}
	var b strings.Builder
	b.WriteString("The release sentinel stopped and needs a human.\n\n")
	fmt.Fprintf(&b, "tag: %s (%s)\nreason: %s\n%s\n", e.Tag, shortSHA(e.SHA), e.Reason, e.Detail)
	if e.Reason == EscalationPolicy {
		b.WriteString("\nNo repair round was dispatched and nothing was pushed: change the setting, then re-run the release.\n")
	}
	writeRuns(&b, e.Blocking)
	return title, b.String()
}

func writeRuns(b *strings.Builder, runs []BlockingRun) {
	if len(runs) == 0 {
		return
	}
	fmt.Fprintf(b, "\nblocking runs (%d):\n", len(runs))
	for i, r := range runs {
		if i >= maxRenderedRuns {
			fmt.Fprintf(b, "  ... and %d more\n", len(runs)-i)
			break
		}
		fmt.Fprintf(b, "  - %q run %d: %s", r.Name, r.ID, r.Conclusion)
		if r.URL != "" {
			fmt.Fprintf(b, " (%s)", r.URL)
		}
		b.WriteString("\n")
		if len(r.FailedJobs) > 0 {
			fmt.Fprintf(b, "    failed: %s\n", strings.Join(r.FailedJobs, ", "))
		}
		for j, ev := range r.Evidence {
			if j >= maxRenderedEvidence {
				break
			}
			fmt.Fprintf(b, "    evidence: %s\n", strings.ReplaceAll(ev, "\n", " "))
		}
	}
}
