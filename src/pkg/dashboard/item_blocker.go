package dashboard

// Structured blockers on tracker rows (hivecommons/hive#15099).
//
// An agent that parks an issue because the hive CANNOT act — the GitHub
// App has no `workflows` permission, a CLA must be signed — used to leave
// the same bare `needs-human` as an agent asking "which way?". The
// dashboard then offered "Let hive decide", the hive un-parked, the
// scanner re-verified and re-parked for the same reason, forever
// (hivecommons/hive#14976). Here the blocker is read from pkg/blocker's
// ledger (fed by the `<!-- hive:blocker -->` marker or the prose it
// inferred from, via the issue_comment webhook and the task-list sweep),
// carried on the row as `blocker`, and drives the pill, the popover, the
// delegated decision's no-effect answer and the global capability summary.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/blocker"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// StageTrackerBlocker is the structured blocker on a tracker row.
type StageTrackerBlocker struct {
	// Kind is the pkg/blocker kind ("capability:workflows-permission").
	Kind string `json:"kind"`
	// Short is the pill text after "Blocked · ".
	Short string `json:"short"`
	// Detail is the reporting sentence from the agent's comment.
	Detail string `json:"detail,omitempty"`
	// Permission is the GitHub App permission the kind is missing ("" for
	// human-only kinds); Capability says a grant can clear it.
	Permission string     `json:"permission,omitempty"`
	Capability bool       `json:"capability"`
	Since      *time.Time `json:"since,omitempty"`
	Source     string     `json:"source,omitempty"`
	// Reapplied is the loop guard: the same reason came back after a
	// delegated un-park, so "Let hive decide" is disabled with a reason.
	Reapplied bool `json:"reapplied,omitempty"`
}

// blockerLedger returns the ledger the dashboard reads; tests swap it.
var blockerLedger = func() *blocker.Ledger { return blocker.Shared() }

// stageTrackerBlockerFor reads the row's blocker from the ledger, nil when
// none is recorded.
func stageTrackerBlockerFor(repo string, number int) *StageTrackerBlocker {
	e, ok := blockerLedger().Get(repo, number)
	if !ok || !e.Blocks() {
		return nil
	}
	return stageTrackerBlockerFromEntry(e)
}

func stageTrackerBlockerFromEntry(e blocker.Entry) *StageTrackerBlocker {
	b := e.Blocker
	out := &StageTrackerBlocker{
		Kind: string(b.Kind), Short: b.Kind.Short(), Detail: strings.TrimSpace(b.Detail),
		Permission: b.Kind.Permission(), Capability: b.Kind.IsCapability(), Source: b.Source, Reapplied: e.Reapplied,
	}
	if !b.Since.IsZero() {
		since := b.Since
		out.Since = &since
	}
	return out
}

// Blocker popover words (plain, no jargon).
const (
	decisionAskedByBlocker          = "the hive (it cannot do this itself)"
	decisionWhyBlockerCapability    = "No decision grants the App the permission; grant it and re-check instead"
	decisionWhyBlockerHumanOnly     = "Only a person can do this step"
	decisionWhyBlockerReasked       = "Hive already decided once and the item was parked again for the same reason; deciding again would repeat the loop"
	decisionWhyPatchNoIdentity      = "Hive has no human-authorised identity that may push workflow files; grant the App the permission or apply the change by hand"
	decisionWhyPatchNotCapability   = "Nothing to prepare: this step is a person's to take"
	decisionWhyRecheckNotCapability = "No App permission is involved"
	decisionWhyNoAppSettings        = "The App settings page is not known for this installation"
)

// blockerActionLabels is the SPA-facing label per action id, kept here so
// the popover and the delegated no-effect result print the same words.
func blockerActionLabel(action string, perm string) string {
	switch action {
	case blocker.ActionAppSettings:
		return "Grant the App `" + perm + ": write`"
	case blocker.ActionRecheck:
		return "Re-check permissions"
	case blocker.ActionPatch:
		return "Prepare the patch for a person to push"
	case blocker.ActionClosePartial:
		return "Mark partially-addressed and close"
	case blocker.ActionWait:
		return "Keep waiting"
	}
	return action
}

// blockerOptionsContext is what the option builder needs from the server.
type blockerOptionsContext struct {
	AppSettingsURL string
	PatchFeasible  bool
	PatchWhy       string
}

// blockerDecisionOptions lists the popover's options for a blocked row, in
// the order the owner asked for: (a) grant + re-check, (b) prepare the
// patch, (c) mark partially-addressed and close, (d) keep waiting, then
// the row's usual "Let hive decide" disabled with the reason, and hold.
func blockerDecisionOptions(b StageTrackerBlocker, held bool, ctx blockerOptionsContext) []StageTrackerDecisionOption {
	var opts []StageTrackerDecisionOption
	kind := blocker.Kind(b.Kind)
	if b.Capability {
		settings := decisionOption(blocker.ActionAppSettings, blockerActionLabel(blocker.ActionAppSettings, b.Permission), ctx.AppSettingsURL != "", decisionWhyNoAppSettings)
		settings.URL = ctx.AppSettingsURL
		settings.Consequence = "opens the App settings page in a new tab; the hive re-tries on its own once it sees `" + b.Permission + ": write`"
		recheck := decisionOption(blocker.ActionRecheck, blockerActionLabel(blocker.ActionRecheck, b.Permission), true, "")
		recheck.Consequence = "probes the installation now and releases every item blocked on `" + b.Permission + "` if it is granted"
		patch := decisionOption(blocker.ActionPatch, blockerActionLabel(blocker.ActionPatch, b.Permission), ctx.PatchFeasible, nonEmpty(ctx.PatchWhy, decisionWhyPatchNoIdentity))
		patch.Consequence = "the hive prepares the change for a person to push under their own identity"
		opts = append(opts, settings, recheck, patch)
	}
	closePartial := decisionOption(blocker.ActionClosePartial, blockerActionLabel(blocker.ActionClosePartial, ""), !held, decisionWhyHeld)
	closePartial.Consequence = "labels the issue partially-addressed, posts your note on what remains and closes it"
	wait := decisionOption(blocker.ActionWait, blockerActionLabel(blocker.ActionWait, ""), true, "")
	wait.Consequence = "changes nothing; the row stays blocked until the blocker clears"
	opts = append(opts, closePartial, wait)
	why := decisionWhyBlockerHumanOnly
	switch {
	case kind == blocker.HumanOnlyReasked || b.Reapplied:
		why = decisionWhyBlockerReasked
	case b.Capability:
		why = decisionWhyBlockerCapability
	}
	opts = append(opts, decisionOption(decisionActionDelegate, "Let hive decide", false, why), decisionHoldOption(held))
	return opts
}

// decisionForBlocker is the decision_needed for a row parked on a blocker.
func decisionForBlocker(it StageTrackerItem, held bool, ctx blockerOptionsContext) *StageTrackerDecision {
	b := it.Blocker
	kind := blocker.Kind(b.Kind)
	d := &StageTrackerDecision{Since: b.Since, AskedBy: decisionAskedByBlocker, BlockerReason: "blocker:" + b.Kind}
	if d.Since == nil {
		d.Since = it.StageSince
	}
	d.Question = kind.CantDo() + " " + blockerQuestionTail(b)
	d.Blocker = "Blocked · " + b.Short
	if b.Detail != "" {
		d.Blocker += " — " + b.Detail
	}
	if b.Reapplied && kind != blocker.HumanOnlyReasked {
		d.Blocker += " The hive already un-parked this once on a delegated decision and an agent parked it again for the same reason."
	}
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	d.Options = blockerDecisionOptions(*b, held, ctx)
	return d
}

// blockerQuestionTail is the "Options: …" sentence after CantDo.
func blockerQuestionTail(b *StageTrackerBlocker) string {
	if b.Capability {
		return "Options: (a) grant the App `" + b.Permission + ": write` — hive re-tries on its own once it sees the permission, or press Re-check permissions · (b) hive prepares the patch for a person to push, where it can · (c) mark partially-addressed and close · (d) keep waiting."
	}
	return "Options: (a) a person takes the step on GitHub and removes `needs-human` · (b) mark partially-addressed and close · (c) keep waiting."
}

// blockerOptionsContext resolves the App settings link and whether the
// hive has a human-authorised identity that could push the patch. Today
// no such path exists: the shared-UID App read token is read-only by
// construction and the dashboard holds no operator PAT, so (b) is
// disabled with that reason rather than greyed out silently.
func (s *Server) blockerOptionsContext() blockerOptionsContext {
	ctx := blockerOptionsContext{PatchWhy: decisionWhyPatchNoIdentity}
	if s != nil {
		s.githubAppMu.RLock()
		ctx.AppSettingsURL = strings.TrimSpace(s.githubAppInstallURL)
		s.githubAppMu.RUnlock()
		if ctx.AppSettingsURL == "" && s.deps != nil && s.deps.Config != nil {
			ctx.AppSettingsURL = s.deps.Config.GitHub.AppInstallURL()
		}
	}
	return ctx
}

// blockerNoEffectResult is the delegated decision's answer on a blocked
// item: nothing is written, and the toast says what would unblock it.
func blockerNoEffectResult(target RowActionTarget, b StageTrackerBlocker, held bool, ctx blockerOptionsContext) RowActionResult {
	kind := blocker.Kind(b.Kind)
	res := RowActionResult{Action: rowActionNoEffect, Target: target, Why: "blocker:" + b.Kind}
	res.Outcome = kind.CantDo()
	if b.Detail != "" {
		res.Outcome += " (" + b.Detail + ")"
	}
	res.Next = blockerQuestionTail(&b)
	res.Options = blockerDecisionOptions(b, held, ctx)
	return res
}

// delegatedBlockerShortCircuit is what decideDelegatedOne checks before
// deciding: the ledger's blocker on the item (refreshed from the comments
// the gather already read), or none.
func (s *Server) delegatedBlocker(ctx context.Context, repoFull string, number int, comments []commentText) *StageTrackerBlocker {
	l := blockerLedger()
	for _, c := range comments {
		l.Observe(repoFull, number, c.Body, c.At)
	}
	return stageTrackerBlockerFor(repoFull, number)
}

// commentText is a comment body with its time, as the gather lists them.
type commentText struct {
	Body string
	At   time.Time
}

// noteDelegatedUnpark records on the ledger that "Let hive decide" lifted
// the park, so a re-park for the same reason trips the loop guard.
func (s *Server) noteDelegatedUnpark(repoFull string, number int) {
	blockerLedger().NoteDelegatedUnpark(repoFull, number, delegatedDecisionNow())
}

// Capability summary for /api/status and the ⚙ Security/Features card.

// FrontendCapabilities is `capabilities` on /api/status: the GitHub App
// permissions items are blocked on and how many.
type FrontendCapabilities struct {
	Missing      []string       `json:"missing"`
	BlockedItems int            `json:"blocked_items"`
	ByPermission map[string]int `json:"by_permission,omitempty"`
}

func (s *Server) capabilitiesStatus() *FrontendCapabilities {
	c := blockerLedger().Capabilities()
	out := &FrontendCapabilities{Missing: c.Missing, BlockedItems: c.BlockedItems}
	if len(c.ByPermission) > 0 {
		out.ByPermission = c.ByPermission
	}
	if out.Missing == nil {
		out.Missing = []string{}
	}
	return out
}

// Endpoints.

// blockerRecheckResponse is POST …/items/{n}/blocker/recheck's answer.
type blockerRecheckResponse struct {
	OK         bool            `json:"ok"`
	Repo       string          `json:"repo"`
	Number     int             `json:"number"`
	Permission string          `json:"permission"`
	Granted    bool            `json:"granted"`
	Released   int             `json:"released"`
	Result     RowActionResult `json:"result"`
}

// installationPermissionProbe asks the App installation for its granted
// permissions; tests swap it.
var installationPermissionProbe = func(ctx context.Context, auth *ghpkg.AppAuth) (*ghpkg.InstallationInfo, error) {
	if auth == nil {
		return nil, errors.New("GitHub App auth not configured")
	}
	return auth.VerifyInstallation(ctx)
}

// handleRepoItemBlockerRecheck probes the installation's permissions and,
// when the blocker's permission is granted, lifts every blocker of that
// kind so the items return to the agent queue on the next pass.
func (s *Server) handleRepoItemBlockerRecheck(w http.ResponseWriter, r *http.Request) {
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
		jsonError(w, "GitHub user session required", http.StatusUnauthorized)
		return
	}
	repoFull := owner + "/" + repoName
	b := stageTrackerBlockerFor(repoFull, number)
	if b == nil {
		jsonError(w, "no blocker is recorded on this item", http.StatusNotFound)
		return
	}
	if !b.Capability {
		jsonError(w, decisionWhyRecheckNotCapability, http.StatusConflict)
		return
	}
	ctx := context.Background()
	if s.deps != nil && s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	var auth *ghpkg.AppAuth
	if s.deps != nil {
		auth = s.deps.GHAppAuth
	}
	info, err := installationPermissionProbe(ctx, auth)
	if err != nil {
		jsonError(w, "could not read the App installation's permissions: "+err.Error(), http.StatusBadGateway)
		return
	}
	target := rowActionTarget(repoFull, rowActionTargetIssue, number)
	resp := blockerRecheckResponse{OK: true, Repo: repoFull, Number: number, Permission: b.Permission}
	if info.Granted(b.Permission, "write") {
		resp.Granted = true
		resp.Released = blockerLedger().ClearKind(blocker.Kind(b.Kind))
		resp.Result = RowActionResult{Action: rowActionReleased, Target: target,
			Outcome: fmt.Sprintf("The App now has `%s: write`; released %d item(s) blocked on it.", b.Permission, resp.Released),
			Next:    "The hive re-tries on its next pass. `needs-human` stays until an agent or a person lifts it; the stage pass no longer reads it as a blocker."}
		s.audit.LogRecord(user, auditRepoItemBlockerRecheck, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "permission", b.Permission, "granted", "true", "released", strconv.Itoa(resp.Released)), "", repoFull, number)
		s.invalidateStageTrackerCache()
		jsonResponse(w, resp)
		return
	}
	resp.Result = blockerNoEffectResult(target, *b, false, s.blockerOptionsContext())
	resp.Result.Outcome = fmt.Sprintf("Still missing: the App installation has no `%s: write` (granted: %s).", b.Permission, nonEmpty(info.Permissions[b.Permission], "none"))
	s.audit.LogRecord(user, auditRepoItemBlockerRecheck, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "permission", b.Permission, "granted", "false"), "", repoFull, number)
	jsonResponse(w, resp)
}

// handleRepoItemBlockerClosePartial marks the issue hive/partially-
// addressed and closes it as completed with a comment naming the blocker.
func (s *Server) handleRepoItemBlockerClosePartial(w http.ResponseWriter, r *http.Request) {
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
		jsonError(w, "GitHub user session required", http.StatusUnauthorized)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	repoFull := owner + "/" + repoName
	b := stageTrackerBlockerFor(repoFull, number)
	if b == nil {
		jsonError(w, "no blocker is recorded on this item", http.StatusNotFound)
		return
	}
	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	gh := s.deps.GHClient
	if err := gh.AddLabels(ctx, repoFull, number, []string{ghpkg.PartiallyAddressedLabel}); err != nil { // writeback:solicited operator chose "close as partially addressed" on the dashboard
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := gh.CreateIssueComment(ctx, repoFull, number, renderBlockerClosePartialComment(user, *b)); err != nil { // writeback:solicited operator-authored closing comment
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	err := gh.CloseIssue(ctx, repoFull, number, ghpkg.IssueCloseOptions{ // writeback:solicited operator closed the item from the dashboard
		OverrideReason:          "closed as partially addressed by @" + user + " from the Hive dashboard",
		SuppressOverrideComment: true,
		StateReason:             ghpkg.IssueStateReasonCompleted,
		AllowTrackingIssue:      true,
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	blockerLedger().Clear(repoFull, number)
	s.audit.LogRecord(user, auditRepoItemBlockerClosePartial, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "blocker", b.Kind), "", repoFull, number)
	s.invalidateStageTrackerCache()
	target := rowActionTarget(repoFull, rowActionTargetIssue, number)
	jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: blocker.ActionClosePartial, Status: "closed", Label: ghpkg.PartiallyAddressedLabel,
		Result: &RowActionResult{Action: rowActionClosed, Target: target,
			Outcome: "Closed as completed and marked `" + ghpkg.PartiallyAddressedLabel + "`; the rest (" + b.Short + ") is left to a person.",
			Next:    "Nothing more happens here. Reopen the issue if the remainder should be tracked again."}})
}

func renderBlockerClosePartialComment(user string, b StageTrackerBlocker) string {
	var sb strings.Builder
	sb.WriteString("Closing as **partially addressed** — @" + user + " decided from the Hive dashboard.\n\n")
	sb.WriteString(blocker.Kind(b.Kind).CantDo())
	if b.Detail != "" {
		sb.WriteString("\n\n> " + b.Detail)
	}
	sb.WriteString("\n\nThe remainder is left to a person; reopen if it should be tracked again.\n")
	return sb.String()
}

const (
	auditRepoItemBlockerRecheck      = "repo_item_blocker_recheck"
	auditRepoItemBlockerClosePartial = "repo_item_blocker_close_partial"
)
