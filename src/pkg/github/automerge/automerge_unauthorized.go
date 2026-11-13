package automerge

// Lanes the App is not permitted to advance (hivecommons/hive#14589).
//
// Some lane steps are App API calls the installation may lack the permission
// for: approving held workflow runs needs `actions: write`, which an operator
// may decline to grant. GitHub answers 403 "Resource not accessible by
// integration", the lane cannot take the step, and without this file the PR
// sits at its previous reason (`required-check-missing:<ctx>`) looking like
// CI is merely slow while the sweep WARNs every tick. The owner's decision is
// to say so plainly instead of widening the App: the lane marks the PR
// `unauthorized` — label hive:unauthorized, one comment per step naming the
// missing permission and the exact human step, reason `unauthorized:<step>`
// so the stage engine parks it at 085 and the tracker's pill column shows
// the word — and clears all of it in the same tick the step stops being
// needed. Every call site that hits such a 403 routes through
// markUnauthorized; clearUnauthorized runs where the lane observes the step
// is done (or no longer applies).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const (
	// unauthorizedLabel is the GitHub label the lane adds; it lives outside
	// the stage/ family so the reconciler never strips it.
	unauthorizedLabel = stagelabels.UnauthorizedLabel
	// unauthorizedLabelColor / unauthorizedLabelDescription provision the
	// label on first use so it does not appear in GitHub's random colour.
	unauthorizedLabelColor       = "6e7781"
	unauthorizedLabelDescription = "The hive's GitHub App lacks the permission for the next step; a maintainer must take it (see the hive:unauthorized comment)"
	// unauthorizedMarkerPrefix tags the one comment per (PR, step); a
	// different blocked step gets its own note while a new push rewrites
	// the existing one with the attempt count.
	unauthorizedMarkerPrefix = "<!-- hive:unauthorized"
	// unauthorizedReasonPrefix is the lane reason the stage engine parks at 085.
	unauthorizedReasonPrefix = prstage.StageReasonUnauthorizedPrefix
	// unauthorizedStepApproveWorkflowRuns names the held-run approval step.
	unauthorizedStepApproveWorkflowRuns = prstage.UnauthorizedStepApproveWorkflowRuns
	// unauthorizedPermissionActionsWrite is the App permission that step needs.
	unauthorizedPermissionActionsWrite = "actions: write"
	// resourceNotAccessibleMessage is GitHub's 403 body for a call the
	// installation token is not permitted to make.
	resourceNotAccessibleMessage = "resource not accessible by integration"
	// unauthorizedRESTCaller tags the label and comment writes.
	unauthorizedRESTCaller = "hive:lane_unauthorized"
)

// unauthorizedState is what this process remembers about a PR it marked:
// the head and step the mark covers, so the same head logs once and a
// different head or step re-marks.
type unauthorizedState struct {
	Head string
	Step string
}

// unauthorizedKey is the per-PR key of Engine.unauthorized.
func (c *Engine) unauthorizedKey(owner, repo string, number int) string {
	return c.evaluatedHeadKey(owner, repo, number)
}

// isResourceNotAccessible reports whether err is GitHub's 403 for an API
// call the App installation is not permitted to make. A 403 with any other
// body (rate limit, archived repository, SSO) is not a permission gap.
func isResourceNotAccessible(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusForbidden {
		return false
	}
	msg := ghErr.Message
	if msg == "" {
		msg = ghErr.Error()
	}
	return strings.Contains(strings.ToLower(msg), resourceNotAccessibleMessage)
}

// unauthorizedReason is the lane reason for a blocked step.
func unauthorizedReason(step string) string {
	return unauthorizedReasonPrefix + step
}

// unauthorizedStep returns the step this process has marked the PR
// unauthorized for at head, or "" when it has not (or marked another head).
func (c *Engine) unauthorizedStep(owner, repo string, number int, headSHA string) string {
	if c == nil {
		return ""
	}
	c.unauthorizedMu.Lock()
	defer c.unauthorizedMu.Unlock()
	st, ok := c.unauthorized[c.unauthorizedKey(owner, repo, number)]
	if !ok || !strings.EqualFold(st.Head, headSHA) {
		return ""
	}
	return st.Step
}

// markUnauthorized records that the lane's next step for this PR head needs
// permission the App lacks. It adds the label when the PR does not wear it,
// upserts the one comment per (PR, step), logs once per (head, step) at
// INFO, and returns the lane reason to report. Idempotent across ticks and
// restarts: the label and the comment marker on the PR are the durable
// state, the in-memory map only quiets the log. Label and comment failures
// are logged, never returned: the reason still flips so the stage engine
// parks the PR at 085.
func (c *Engine) markUnauthorized(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, headSHA, step, permission, humanAction string) string {
	reason := unauthorizedReason(step)
	if c == nil || c.gh == nil || number <= 0 {
		return reason
	}
	key := c.unauthorizedKey(owner, repo, number)
	c.unauthorizedMu.Lock()
	if c.unauthorized == nil {
		c.unauthorized = make(map[string]unauthorizedState)
	}
	prev, had := c.unauthorized[key]
	c.unauthorized[key] = unauthorizedState{Head: headSHA, Step: step}
	c.unauthorizedMu.Unlock()
	if had && strings.EqualFold(prev.Head, headSHA) && prev.Step == step {
		return reason
	}
	c.info("lane unauthorized", "repo", displayRepo, "pr", number, "head", headSHA, "step", step, "permission", permission)
	ctx = hgithub.WithRESTCaller(ctx, unauthorizedRESTCaller)
	if pr == nil || !hasLabel(labelNames(pr.Labels), unauthorizedLabel) {
		if err := c.addUnauthorizedLabel(ctx, owner, repo, number); err != nil {
			c.warn("lane unauthorized: could not add label", "repo", displayRepo, "pr", number, "label", unauthorizedLabel, "error", err)
		}
	}
	if err := c.commentUnauthorized(ctx, owner, repo, number, headSHA, step, permission, humanAction); err != nil {
		c.warn("lane unauthorized: could not post comment", "repo", displayRepo, "pr", number, "head", headSHA, "step", step, "error", err)
	}
	return reason
}

// clearUnauthorized removes the mark when the step it covered is no longer
// needed: the in-memory state for this PR goes, and the label comes off the
// PR when it wears it. It is a no-op when the PR carries no mark (so every
// lane tick may call it), and leaves a mark for a different step alone.
func (c *Engine) clearUnauthorized(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, step string) {
	if c == nil || c.gh == nil || number <= 0 {
		return
	}
	key := c.unauthorizedKey(owner, repo, number)
	c.unauthorizedMu.Lock()
	st, had := c.unauthorized[key]
	if had && st.Step != step {
		c.unauthorizedMu.Unlock()
		return
	}
	delete(c.unauthorized, key)
	c.unauthorizedMu.Unlock()
	labelled := pr != nil && hasLabel(labelNames(pr.Labels), unauthorizedLabel)
	if !had && !labelled {
		return
	}
	if labelled && hgithub.IntegrationReadOnly(ctx) {
		// The read-only lane probe refuses the DELETE; a write-capable lane
		// clears the label, so retrying here would only warn every cycle.
		c.debug("lane unauthorized: label removal left to a write-capable lane", "repo", displayRepo, "pr", number, "label", unauthorizedLabel, "step", step)
		return
	}
	if labelled {
		if _, err := c.gh.Issues.RemoveLabelForIssue(hgithub.WithRESTCaller(ctx, unauthorizedRESTCaller), owner, repo, number, url.PathEscape(unauthorizedLabel)); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
			c.warn("lane unauthorized: could not remove label", "repo", displayRepo, "pr", number, "label", unauthorizedLabel, "error", err)
			return
		}
	}
	c.info("lane unauthorized cleared", "repo", displayRepo, "pr", number, "head", pr.GetHead().GetSHA(), "step", step)
}

// addUnauthorizedLabel provisions the label once per repository (colour and
// description) and adds it to the PR.
func (c *Engine) addUnauthorizedLabel(ctx context.Context, owner, repo string, number int) error {
	c.unauthorizedMu.Lock()
	if c.unauthorizedLabelReady == nil {
		c.unauthorizedLabelReady = make(map[string]bool)
	}
	ready := c.unauthorizedLabelReady[owner+"/"+repo]
	c.unauthorizedMu.Unlock()
	if !ready {
		_, _, err := c.gh.Issues.CreateLabel(ctx, owner, repo, &gh.Label{
			Name:        gh.Ptr(unauthorizedLabel),
			Color:       gh.Ptr(unauthorizedLabelColor),
			Description: gh.Ptr(unauthorizedLabelDescription),
		})
		// 422 means it already exists; anything else is best effort — the
		// add below creates the label (uncoloured) when it is missing.
		if err == nil || isGitHubStatus(err, http.StatusUnprocessableEntity) {
			c.unauthorizedMu.Lock()
			c.unauthorizedLabelReady[owner+"/"+repo] = true
			c.unauthorizedMu.Unlock()
		}
	}
	_, _, err := c.gh.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{unauthorizedLabel})
	return err
}

// unauthorizedMarker is the first line of the comment for a step. One
// comment per PR and step (hivecommons/hive#17066): a new head rewrites it
// with the attempt count and the heads it covers instead of posting again.
func unauthorizedMarker(step string) string {
	return unauthorizedMarkerPrefix + " step=" + step + " -->"
}

// unauthorizedLegacyMarker is the per-head first line comments carried
// before hivecommons/hive#17066; the upsert adopts the latest one.
func unauthorizedLegacyMarker(headSHA, step string) string {
	return unauthorizedMarkerPrefix + " head=" + strings.ToLower(headSHA) + " step=" + step + " -->"
}

// unauthorizedAttemptsPrefix / unauthorizedAttemptsSuffix frame the hidden
// line that records the attempt count and the heads the note covers:
//
//	<!-- hive:unauthorized-attempts n=3 heads=aaa,bbb,ccc -->
const (
	unauthorizedAttemptsPrefix = "<!-- hive:unauthorized-attempts n="
	unauthorizedAttemptsSuffix = " -->"
	// unauthorizedHeadsCap bounds the heads the note remembers; the
	// attempt count keeps growing past it.
	unauthorizedHeadsCap = markerHistoryCap
)

// unauthorizedAttempts is the state an existing note carries.
type unauthorizedAttempts struct {
	Count int
	Heads []string
}

// isUnauthorizedCommentFor reports whether body is the hive:unauthorized
// note for step, in either the current or the per-head legacy form.
func isUnauthorizedCommentFor(step string) func(body string) bool {
	marker := unauthorizedMarker(step)
	suffix := " step=" + step + " -->"
	return func(body string) bool {
		first, _, _ := strings.Cut(body, "\n")
		if first == marker {
			return true
		}
		return strings.HasPrefix(first, unauthorizedMarkerPrefix+" head=") && strings.HasSuffix(first, suffix)
	}
}

// parseUnauthorizedAttempts reads the attempt state from an existing note.
// A legacy per-head note counts as one attempt on the head its marker
// names; a note with no record counts as one attempt on no known head.
func parseUnauthorizedAttempts(body string) unauthorizedAttempts {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, unauthorizedAttemptsPrefix) || !strings.HasSuffix(line, unauthorizedAttemptsSuffix) {
			continue
		}
		fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, unauthorizedAttemptsPrefix), unauthorizedAttemptsSuffix))
		if len(fields) == 0 {
			break
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil || n < 1 {
			n = 1
		}
		out := unauthorizedAttempts{Count: n}
		for _, f := range fields[1:] {
			if heads, ok := strings.CutPrefix(f, "heads="); ok {
				for _, h := range strings.Split(heads, ",") {
					if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
						out.Heads = append(out.Heads, h)
					}
				}
			}
		}
		return out
	}
	first, _, _ := strings.Cut(body, "\n")
	if rest, ok := strings.CutPrefix(first, unauthorizedMarkerPrefix+" head="); ok {
		head, _, _ := strings.Cut(rest, " ")
		if head != "" {
			return unauthorizedAttempts{Count: 1, Heads: []string{strings.ToLower(head)}}
		}
	}
	return unauthorizedAttempts{Count: 1}
}

// covers reports whether the note already names headSHA.
func (a unauthorizedAttempts) covers(headSHA string) bool {
	return slices.Contains(a.Heads, strings.ToLower(headSHA))
}

// next is the state after one more attempt on headSHA: the count grows and
// the head joins the remembered set (oldest dropped past the cap).
func (a unauthorizedAttempts) next(headSHA string) unauthorizedAttempts {
	heads := append(slices.Clone(a.Heads), strings.ToLower(headSHA))
	if len(heads) > unauthorizedHeadsCap {
		heads = heads[len(heads)-unauthorizedHeadsCap:]
	}
	return unauthorizedAttempts{Count: a.Count + 1, Heads: heads}
}

// record renders the hidden attempt line.
func (a unauthorizedAttempts) record() string {
	return unauthorizedAttemptsPrefix + strconv.Itoa(a.Count) + " heads=" + strings.Join(a.Heads, ",") + unauthorizedAttemptsSuffix
}

// unauthorizedComment is the comment body: the marker, then the permission
// the App lacks, the step it therefore cannot take and what a person does,
// then the attempt record. A first attempt shows no count; later ones say
// how many heads have hit the same block.
func unauthorizedComment(headSHA, step, permission, humanAction string, attempts unauthorizedAttempts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nThe hive's GitHub App lacks `%s`, so it cannot %s for head `%s`. %s Hive will resume automatically once the step is done; this label and note clear themselves.",
		unauthorizedMarker(step), permission, strings.ReplaceAll(step, "-", " "), shortSHA(headSHA), strings.TrimSpace(humanAction))
	if attempts.Count > 1 {
		short := make([]string, 0, len(attempts.Heads))
		for _, h := range attempts.Heads {
			short = append(short, "`"+shortSHA(h)+"`")
		}
		fmt.Fprintf(&b, "\n\n<sub>Attempt %d: the same block hit heads %s.</sub>", attempts.Count, strings.Join(short, ", "))
	}
	b.WriteString("\n")
	b.WriteString(attempts.record())
	b.WriteString("\n")
	return b.String()
}

// unauthorizedCommentCoversHead reports whether comments carry the hive's
// note for step naming headSHA: a previous process already learnt the App
// cannot take the step on this head.
func unauthorizedCommentCoversHead(comments []*gh.IssueComment, step, headSHA string) bool {
	match := isUnauthorizedCommentFor(step)
	for _, comment := range comments {
		if comment == nil || !match(comment.GetBody()) {
			continue
		}
		if parseUnauthorizedAttempts(comment.GetBody()).covers(headSHA) {
			return true
		}
	}
	return false
}

// commentUnauthorized keeps one note per (PR, step): a note already naming
// this head is left alone; a note for an earlier head is rewritten in place
// with the attempt count; no note posts one through the effects claim.
func (c *Engine) commentUnauthorized(ctx context.Context, owner, repo string, number int, headSHA, step, permission, humanAction string) error {
	existing, err := c.findOwnMarkerComment(ctx, owner, repo, number, isUnauthorizedCommentFor(step))
	if err != nil {
		return err
	}
	if existing != nil {
		attempts := parseUnauthorizedAttempts(existing.GetBody())
		if attempts.covers(headSHA) {
			return nil
		}
		return c.editMarkerComment(ctx, owner, repo, existing.GetID(), unauthorizedComment(headSHA, step, permission, humanAction, attempts.next(headSHA)))
	}
	marker := unauthorizedMarker(step)
	body := unauthorizedComment(headSHA, step, permission, humanAction, unauthorizedAttempts{Count: 1, Heads: []string{strings.ToLower(headSHA)}})
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"head_sha": headSHA, "body": effects.StableDigest(marker)},
	}, func(ctx context.Context) (effects.Result, error) {
		comment, _, apiErr := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		c.noteOwnStageComment(owner, repo, number)
		return effects.Result{Provenance: comment.GetHTMLURL()}, nil
	})
	return err
}

// approveWorkflowRunsHumanAction is the human step for held runs: the
// Actions tab's "Approve and run", with the run ids and the contexts they
// would produce.
func approveWorkflowRunsHumanAction(owner, repo, missingContext string, runIDs []int64) string {
	ids := make([]string, 0, len(runIDs))
	for _, id := range runIDs {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	ctxNote := ""
	if missingContext != "" {
		ctxNote = " (required check `" + missingContext + "`)"
	}
	return fmt.Sprintf("A maintainer must click **Approve and run** on the PR's checks banner or the Actions tab of %s/%s%s — runs %s — or grant the App `%s` to let the hive do it.",
		owner, repo, ctxNote, strings.Join(ids, ", "), unauthorizedPermissionActionsWrite)
}
