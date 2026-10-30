package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Operator row actions for a PR waiting on a contributor's clanker
// (hivecommons/hive#15038): "Adopt now" and "Reroute to another clanker".
//
// Both ride POST /api/repos/{o}/{r}/items/{n}/decision:
//
//	{"decision":"adopt","force":true,"type":"pr"}   → Adopt now
//	{"decision":"reroute","to":"<login>|any","type":"pr"} → Reroute
//
// Adopt now leaves a durable mark for the automerge sweep (the stage
// record's AdoptForcedBy plus the adoption/supersede label) and nudges the
// sweep so the adoption engine's existing supersede path runs at once —
// with maintainer_policy, presence and the grace window bypassed
// (automerge_contributor_adopt.go). Reroute hands the PR's follow-up work
// to another online clanker through the contributor fix queue
// (contribute_reroute.go) and arms a pickup timer that falls back to Adopt
// now after governor.reroute.pickup_timeout.

const (
	repoItemDecisionReroute        = "reroute"
	auditRepoItemDecisionAdoptNow  = "repo_item_decision_adopt_now"
	auditRepoItemDecisionReroute   = "repo_item_decision_reroute"
	auditRepoItemRerouteFallback   = "repo_item_reroute_fallback"
	rowActionRerouted              = "rerouted"
	repoItemDecisionAdoptNowAction = "adopt-now"
	// repoItemRerouteMarker opens the note a reroute leaves on the PR.
	repoItemRerouteMarker = "<!-- hive:rerouted"
	// repoItemAdoptNowWait is how long the handler waits for the nudged sweep
	// to record the superseding PR before answering with the queued form of
	// the toast; the sweep normally lands well inside it.
	repoItemAdoptNowWait = 20 * time.Second
	repoItemAdoptNowPoll = 500 * time.Millisecond
	// repoItemRerouteFallbackAlert prefixes the system alert the pickup
	// fallback raises.
	repoItemRerouteFallbackAlert = "reroute-fallback:"
)

// repoItemPRFacts are what both actions need to know about the PR.
type repoItemPRFacts struct {
	Author, HeadSHA, Title, URL string
	OpenedAt                    time.Time
}

func (s *Server) repoItemPRFacts(ctx context.Context, repoFull string, number int) (repoItemPRFacts, error) {
	owner, repoName, _ := strings.Cut(repoFull, "/")
	pr, _, err := s.deps.GHClient.GoGitHub().PullRequests.Get(ctx, owner, repoName, number)
	if err != nil {
		return repoItemPRFacts{}, fmt.Errorf("reading PR %s#%d: %w", repoFull, number, err)
	}
	if st := pr.GetState(); st != "" && !strings.EqualFold(st, "open") {
		return repoItemPRFacts{}, fmt.Errorf("%s#%d is not open", repoFull, number)
	}
	return repoItemPRFacts{
		Author: strings.TrimSpace(pr.GetUser().GetLogin()), HeadSHA: pr.GetHead().GetSHA(),
		Title: pr.GetTitle(), URL: pr.GetHTMLURL(), OpenedAt: pr.GetCreatedAt().Time,
	}, nil
}

// ensureStageRecord returns the PR's stage record, observing it at 072 when
// the sweep has not recorded it yet (the mark must have a record to live on).
func ensureStageRecord(store *stagelabels.Store, key string) stagelabels.Record {
	if rec, ok := store.Get(key); ok {
		return rec
	}
	store.Observe(key, stagelabels.ReturnedToContributor, "operator-action")
	rec, _ := store.Get(key)
	return rec
}

// handleRepoItemAdoptNow is {"decision":"adopt","force":true}.
func (s *Server) handleRepoItemAdoptNow(ctx context.Context, w http.ResponseWriter, repoFull string, number int, user, comment string) {
	gh := s.deps.GHClient
	facts, err := s.repoItemPRFacts(ctx, repoFull, number)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if gh.IsHiveAuthorLogin(facts.Author) {
		jsonError(w, "this PR is the hive's own; adoption applies to contributor PRs", http.StatusBadRequest)
		return
	}
	store := s.stageStoreOrShared()
	key := stagelabels.Key(repoFull, number)
	now := time.Now()
	ensureStageRecord(store, key)
	if rec, _ := store.Get(key); rec.AdoptedBy > 0 && rec.Adopted(facts.HeadSHA) {
		jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: repoItemDecisionAdopt, Status: "already-adopted",
			Result: &RowActionResult{Target: rowActionTarget(repoFull, "pr", number), Action: rowActionNoop,
				Outcome: fmt.Sprintf("Already adopted → #%d (hive's copy).", rec.AdoptedBy), Next: fmt.Sprintf("The contributor PR closes when #%d merges.", rec.AdoptedBy)}})
		return
	}
	store.NoteAdoptionForced(key, user, now)
	if err := gh.CreateIssueComment(ctx, repoFull, number, renderRepoItemAdoptNowComment(user, comment)); err != nil {
		store.ClearAdoptionForced(key)
		jsonError(w, "posting adoption note: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := gh.EnsureIssueLabel(ctx, repoFull, delegatedecision.LabelAdoptSupersede, adoptRequestLabelColor, adoptRequestLabelDesc); err != nil {
		jsonError(w, "ensuring adoption request label: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := gh.AddLabels(ctx, repoFull, number, []string{delegatedecision.LabelAdoptSupersede}); err != nil {
		jsonError(w, "queuing forced adoption: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.audit.LogRecord(user, auditRepoItemDecisionAdoptNow, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "type", "pr", "action", repoItemDecisionAdoptNowAction, "label", delegatedecision.LabelAdoptSupersede), "", repoFull, number)
	superseding := s.nudgeSweepAndAwaitAdoption(ctx, store, key, repoFull, number)
	jsonResponse(w, repoItemAdoptNowResponse(repoFull, number, superseding))
}

// nudgeSweepAndAwaitAdoption asks the lanes for a pass over the PR and waits
// briefly for the superseding PR number; 0 when the pass has not landed yet.
func (s *Server) nudgeSweepAndAwaitAdoption(ctx context.Context, store *stagelabels.Store, key, repoFull string, number int) int {
	nudged := false
	if s.deps != nil && s.deps.SweepNudge != nil {
		s.deps.SweepNudge(repoFull, number)
		nudged = true
	}
	if s.deps != nil && s.deps.WebhookWake != nil {
		s.deps.WebhookWake(fmt.Sprintf("adopt-now %s#%d", repoFull, number))
		nudged = true
	}
	if !nudged {
		return 0
	}
	wait := s.adoptNowWait
	if wait == 0 {
		wait = repoItemAdoptNowWait
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if rec, ok := store.Get(key); ok && rec.AdoptedBy > 0 {
			return rec.AdoptedBy
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(repoItemAdoptNowPoll):
		}
	}
	rec, _ := store.Get(key)
	return rec.AdoptedBy
}

// repoItemDecisionStatusAdoptionQueued is Adopt now's status when the sweep
// has not opened the copy within the handler's wait: queued, not done. The
// earlier "adoption-forced" wording claimed the pass now running would open
// it, which the tracker then contradicted for minutes.
const repoItemDecisionStatusAdoptionQueued = "adoption-queued"

func repoItemAdoptNowResponse(repoFull string, number, superseding int) repoItemDecisionResponse {
	out := repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: repoItemDecisionAdopt, Label: delegatedecision.LabelAdoptSupersede,
		Result: &RowActionResult{Target: rowActionTarget(repoFull, "pr", number), Action: rowActionAdopted}}
	if superseding > 0 {
		out.Status = "adopted-now"
		out.Result.Outcome = fmt.Sprintf("Adopted → #%d (hive's copy), contributor PR will close when #%d merges.", superseding, superseding)
		out.Result.Next = fmt.Sprintf("The hive rebases and fixes CI on #%d; the author's commits and sign-off are preserved.", superseding)
		return out
	}
	out.Status = repoItemDecisionStatusAdoptionQueued
	out.Result.Outcome = "Adopting… (queued): the hive opens its copy on its next stage pass; any earlier approval of this head no longer holds it."
	out.Result.Next = "The row reads \"Adopting… (queued)\" until the pass lands, then \"Adopted → #N\". The contributor PR closes as superseded (an agent's PR in the adopting pass, a person's when the hive's copy merges); the author's commits and sign-off are preserved."
	return out
}

// renderRepoItemAdoptNowComment is the attributed note Adopt now leaves
// before the engine's hive:adopted marker follows with the superseding PR.
func renderRepoItemAdoptNowComment(user, comment string) string {
	body := fmt.Sprintf("%s by=%s force=true -->\n@%s asked the hive to adopt this pull request now, without waiting for its author's hive. The hive opens its own PR carrying the author's commits and sign-off unchanged on the pass now running; this PR closes when that copy merges.", repoItemDecisionAdoptMarker, user, user)
	if comment != "" {
		body += "\n\n> " + strings.ReplaceAll(comment, "\n", "\n> ")
	}
	return body
}

// rerouteReason is the return reason shipped to the target clanker: the
// PR's stage record reason (the 072 cause) or the operator's comment.
func rerouteReason(rec stagelabels.Record, comment string) (reason, check string) {
	reason = strings.TrimSpace(rec.Reason)
	if c := strings.TrimSpace(comment); c != "" {
		if reason == "" {
			reason = c
		} else {
			reason = reason + " — " + c
		}
	}
	if r := strings.TrimSpace(rec.Reason); strings.HasPrefix(r, "check-failing:") {
		check = strings.TrimPrefix(r, "check-failing:")
	}
	return reason, check
}

// handleRepoItemReroute is {"decision":"reroute","to":"<login>|any"}.
func (s *Server) handleRepoItemReroute(ctx context.Context, w http.ResponseWriter, repoFull string, number int, user, comment, to string) {
	if s.contributeHub == nil {
		jsonError(w, RerouteNoneOnlineReason, http.StatusConflict)
		return
	}
	if !s.stageAdoptionEnabled() {
		jsonError(w, "reroute requires governor.labels.stage.adoption to be enabled on this hive (its fallback is Adopt now)", http.StatusConflict)
		return
	}
	gh := s.deps.GHClient
	facts, err := s.repoItemPRFacts(ctx, repoFull, number)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	store := s.stageStoreOrShared()
	key := stagelabels.Key(repoFull, number)
	rec := ensureStageRecord(store, key)
	reason, check := rerouteReason(rec, comment)
	out, err := s.contributeHub.Reroute(RerouteRequest{
		Repo: repoFull, Number: number, HeadSHA: facts.HeadSHA, Title: facts.Title, URL: facts.URL, OpenedAt: facts.OpenedAt,
		Author: facts.Author, To: to, By: user, Reason: reason, Check: check,
		HumanChangesRequested: strings.Contains(rec.Reason, "changes_requested") || strings.Contains(rec.Reason, "changes-requested"),
	})
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrNoRerouteTarget) {
			code = http.StatusConflict
		}
		jsonError(w, err.Error(), code)
		return
	}
	now := time.Now()
	store.NoteReroute(key, facts.HeadSHA, out.To, user, reason, now)
	if err := gh.CreateIssueComment(ctx, repoFull, number, renderRepoItemRerouteComment(facts.Author, out.To, user, reason, s.reroutePickupTimeout())); err != nil {
		jsonError(w, "posting reroute note: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.audit.LogRecord(user, auditRepoItemDecisionReroute, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "type", "pr", "to", out.To, "mode", out.Mode, "relay_failed", strconv.FormatBool(out.RelayFailed)), "", repoFull, number)
	s.armReroutePickup(repoFull, number, facts.HeadSHA, out.To, user)
	jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: repoItemDecisionReroute, Status: "rerouted",
		Result: repoItemRerouteResult(repoFull, number, out, s.reroutePickupTimeout())})
}

func repoItemRerouteResult(repoFull string, number int, out RerouteOutcome, timeout time.Duration) *RowActionResult {
	res := &RowActionResult{Target: rowActionTarget(repoFull, "pr", number), Action: rowActionRerouted}
	switch {
	case out.RelayFailed:
		res.Outcome = fmt.Sprintf("Rerouted → @%s (queued; its relay refused the interrupt: %s).", out.To, nonEmptyString(out.RelayError, "busy"))
	case out.Mode == "queued":
		res.Outcome = fmt.Sprintf("Rerouted → @%s (queued; %d ahead).", out.To, maxInt(out.QueueDepth-1, 0))
	default:
		res.Outcome = fmt.Sprintf("Rerouted → @%s (dispatched now, %s).", out.To, out.Mode)
	}
	res.Next = fmt.Sprintf("@%s's hive fixes the PR or opens a replacement carrying the author's commits and sign-off. If it has not picked the work up within %s the hive falls back to Adopt now.", out.To, formatRerouteWindow(timeout))
	return res
}

func nonEmptyString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func formatRerouteWindow(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}

// renderRepoItemRerouteComment is the note the author (and the target) get.
func renderRepoItemRerouteComment(author, to, by, reason string, timeout time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s to=@%s by=@%s reason=%s -->\n", repoItemRerouteMarker, to, by, strings.ReplaceAll(nonEmptyString(reason, "waiting-on-author"), "\n", " "))
	fmt.Fprintf(&b, "@%s — @%s has rerouted the follow-up work on this pull request to @%s's hive rather than wait for yours.", author, by, to)
	if reason != "" {
		fmt.Fprintf(&b, " Reason: %s.", reason)
	}
	fmt.Fprintf(&b, " @%s will either push the fix here (if your branch allows it) or open a replacement PR that carries your commits and sign-off unchanged and supersedes this one. ", to)
	fmt.Fprintf(&b, "If @%s's hive has not picked the work up within %s, the hive adopts the PR itself (`hive:adopted`). Push a new head here if you would rather finish it yourself.", to, formatRerouteWindow(timeout))
	return b.String()
}

func (s *Server) reroutePickupTimeout() time.Duration {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.Governor.Reroute.EffectivePickupTimeout()
	}
	return 30 * time.Minute
}

// armReroutePickup starts the pickup window. When it lapses with the item
// still queued (not in flight on the target's relay and no new head), the
// hive falls back to Adopt now: the queued item is withdrawn, the stage
// record marks the forced adoption and the sweep is nudged.
func (s *Server) armReroutePickup(repoFull string, number int, head, to, by string) {
	after := s.rerouteAfterFunc
	if after == nil {
		after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	after(s.reroutePickupTimeout(), func() { s.reroutePickupLapsed(repoFull, number, head, to, by) })
}

// reroutePickupLapsed is the fallback body; it reports whether it fell back.
func (s *Server) reroutePickupLapsed(repoFull string, number int, head, to, by string) bool {
	store := s.stageStoreOrShared()
	key := stagelabels.Key(repoFull, number)
	rec, ok := store.Get(key)
	if !ok || rec.Rerouted(head) == "" || !strings.EqualFold(rec.RerouteTo, to) {
		// Re-rerouted, adopted, or the head moved: nothing to fall back from.
		return false
	}
	hub := s.contributeHub
	if hub != nil && hub.ReroutePickedUp(to, repoFull, number) {
		return false
	}
	ctx := context.Background()
	if s.deps != nil && s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	now := time.Now()
	if hub != nil && !hub.RerouteQueued(to, repoFull, number) && s.deps != nil && s.deps.GHClient != nil {
		// Neither queued nor in flight: the relay finished it (a new head)
		// or the dispatches failed out. A moved head ends the reroute.
		if facts, err := s.repoItemPRFacts(ctx, repoFull, number); err == nil && facts.HeadSHA != "" && facts.HeadSHA != head {
			store.ClearReroute(key, false, now)
			return false
		}
	}
	if hub != nil {
		hub.CancelReroute(to, repoFull, number)
	}
	store.ClearReroute(key, true, now)
	store.NoteAdoptionForced(key, by, now)
	if gh := s.deps.GHClient; gh != nil {
		msg := fmt.Sprintf("%s to=@%s by=@%s lapsed=true -->\n@%s's hive did not pick this up within %s; falling back to adoption — the hive takes it over now (asked by @%s). The author's commits and sign-off are preserved.",
			repoItemRerouteMarker, to, by, to, formatRerouteWindow(s.reroutePickupTimeout()), by)
		if err := gh.CreateIssueComment(ctx, repoFull, number, msg); err != nil {
			s.logger.Warn("reroute fallback note failed", "repo", repoFull, "pr", number, "error", err)
		}
		if err := gh.EnsureIssueLabel(ctx, repoFull, delegatedecision.LabelAdoptSupersede, adoptRequestLabelColor, adoptRequestLabelDesc); err == nil {
			if err := gh.AddLabels(ctx, repoFull, number, []string{delegatedecision.LabelAdoptSupersede}); err != nil {
				s.logger.Warn("reroute fallback label failed", "repo", repoFull, "pr", number, "error", err)
			}
		}
	}
	s.audit.LogRecord(by, auditRepoItemRerouteFallback, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "to", to, "action", repoItemDecisionAdoptNowAction), "", repoFull, number)
	s.AddSystemAlert(repoItemRerouteFallbackAlert+repoFull+"#"+strconv.Itoa(number), "warning",
		fmt.Sprintf("%s#%d: @%s did not pick up the rerouted work within %s — falling back to Adopt now (hive takes it over).", repoFull, number, to, formatRerouteWindow(s.reroutePickupTimeout())))
	if s.deps != nil && s.deps.SweepNudge != nil {
		s.deps.SweepNudge(repoFull, number)
	} else if s.deps != nil && s.deps.WebhookWake != nil {
		s.deps.WebhookWake(fmt.Sprintf("reroute-fallback %s#%d", repoFull, number))
	}
	return true
}
