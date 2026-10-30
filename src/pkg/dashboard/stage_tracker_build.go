package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// buildStageTracker assembles the response from status and store at now.
func (s *Server) buildStageTracker(status *StatusPayload, store *stagelabels.Store, now time.Time) StageTrackerResponse {
	overrides := s.stageDwellOverrides()
	resp := StageTrackerResponse{GeneratedAt: now, Stages: stageTrackerStages(overrides), Items: []StageTrackerItem{}}
	if s.deps != nil && s.deps.StageEngineStatus != nil {
		st := s.deps.StageEngineStatus()
		resp.Engine = &st
	}
	qualify := stageTrackerRepoQualifier(status)
	resp.RecentlyMerged = s.stageTrackerRecentlyMerged(now, qualify)
	resp.RecentlyClosed = s.stageTrackerRecentlyClosed(now)
	resp.MergedHoldMS = int(s.stageTrackerMergedHold() / time.Millisecond)
	resp.Merged24h = s.stageTrackerMergedSince(now, stageTrackerMergedDayWindow, qualify)
	resp.Merged = s.mergedSummaryNow(now)
	if status == nil {
		return resp
	}
	resp.HiveID = status.HiveID
	qualifyRepo := stageTrackerRepoQualifier(status)
	superseded := map[string]github.ReviewQueueSuperseded{}
	entries := append([]AuditEntry(nil), s.supersessionAuditEntries(now)...)
	for i := range entries {
		entries[i].Repo = qualifyRepo(entries[i].Repo)
	}
	for key, info := range supersededGraceStates(entries, "", s.supersessionGracePeriod()) {
		superseded[key] = info
	}
	var cfg *config.Config
	if s.deps != nil {
		cfg = s.deps.Config
	}
	resp.NeedsHumanLabels = needsHumanPresetLabels(cfg, status.HiveID)
	verifier, verifierErr := s.reviewEvidenceVerifier()
	var evidenceVerifier *github.ReviewEvidenceVerifier
	if verifierErr == nil {
		evidenceVerifier = &verifier
	}
	// The reviewer's decision question for a parked PR comes from the
	// local verdict artifact; a missing or unreadable artifact just means
	// the popover falls back to the reason-derived question.
	var verdictArtifact *review.Artifact
	if art, err := review.LoadArtifact(""); err == nil {
		verdictArtifact = &art
	}
	verdictFor := stageTrackerDecisionVerdictLookup(verdictArtifact)
	// The planner's wait records (review.DispatchState.Waits): a review-
	// pending head the swarm keeps passing over for lack of a reviewer
	// says so in its caption instead of a bare "stalled" pill
	// (hivecommons/hive#15229). Unreadable state just leaves the caption
	// as it was.
	var dispatchState review.DispatchState
	if st, err := review.LoadDispatchState(""); err == nil {
		dispatchState = st
	}
	sentinelLabel := s.sentinelLabel()
	bctx := s.blockerOptionsContext()
	lookup := func(repo string, number int) (stagelabels.Record, bool) {
		if store == nil {
			return stagelabels.Record{}, false
		}
		rec, ok := store.Get(stagelabels.Key(repo, number))
		if ok && rec.Stage == stagelabels.None {
			// A stage-less record only remembers the blocked sweep's lift
			// (hivecommons/hive#15779): the row is still unstaged, but its
			// since-time starts at the lift rather than at creation.
			return stagelabels.Record{UnblockedAt: rec.UnblockedAt}, false
		}
		return rec, ok
	}
	presence, maintainerPolicy := s.stageTrackerPresence(), s.stageTrackerMaintainerPolicy()
	fixWork := s.stageTrackerFixWorkLookup()
	sweep := s.stageTrackerSweepState()
	soakWindow := s.stageTrackerSoakWindow()
	attempts := s.stageTrackerMergeAttempts()
	workers := stageTrackerWorkersFromStatus(status)
	isClankerOwner := stageTrackerIsClankerOwner()
	adoptionClock := s.stageTrackerAdoptionClock()
	rerouteCandidates := s.stageTrackerRerouteCandidates()
	pushStaleAfter := s.stageTrackerWaitingPushStale()
	finish := func(it *StageTrackerItem, labels []string, stage stagelabels.Stage, rec stagelabels.Record, hasRec bool, draft bool) {
		// "Needs human decision" is read from the item's current labels, not
		// a store record that outlived them: once the park label is gone
		// (a `/hive decision` the engine has not re-derived yet) the row
		// must stop asking (hivecommons/docs#419).
		if hasRec && stageTrackerStaleDecisionRecord(rec, labels) {
			rec, hasRec = stagelabels.Record{}, false
		}
		// Likewise a 015 record on an issue whose labels already say
		// triage was accepted (the row menu's Accept triage): 010 until the
		// engine re-derives it.
		if hasRec && s.stageTrackerAcceptedTriageRecord(rec, labels) {
			rec, hasRec = stagelabels.Record{}, false
			if stage == stagelabels.None || stage == stagelabels.TriageNeeded {
				stage = stagelabels.Actionable
			}
		}
		if hasRec && rec.Stage != stagelabels.None {
			stage = rec.Stage
			t := rec.StageSinceAt()
			it.StageSince = &t
			it.Reason = rec.Reason
			stageTrackerRegression(it, rec)
			it.NudgeCount = rec.NudgeCount
			if !rec.LastNudgeAt.IsZero() {
				t2 := rec.LastNudgeAt
				it.LastNudgeAt = &t2
			}
			it.AdoptedBy = rec.AdoptedBy
			it.AdoptedByState = stageTrackerAdoptedByState(rec)
			it.SupersededBy = stageTrackerSupersededBy(rec)
			it.SoakRemainingSeconds = int64(stageTrackerSoakRemaining(rec, now) / time.Second)
			it.AdmittedBy = rec.Admission(it.Head)
			if total, unsigned, ok := rec.DCOFor(it.Head); ok {
				it.DCOTotal, it.DCOUnsigned = total, unsigned
			}
		}
		it.Hold = stageTrackerHoldFrom(rec, hasRec, labels)
		var attempt github.MergeAttempt
		var hasAttempt bool
		if it.Kind == "pr" {
			// A merge call in flight (or just back, merged) lights 100
			// "Merging" whatever the record says (hivecommons/hive#15245).
			attempt, hasAttempt = attempts[stageTrackerMergeAttemptKey(it.Repo, it.Number)]
			stage = stageTrackerMergeAttemptStage(stage, attempt, hasAttempt)
		}
		it.Stage = stage.Number()
		it.StageSlug = stage.Slug()
		it.StageLabel = stage.Label()
		if it.Kind == "pr" {
			it.MergeGate = stageTrackerMergeGateAttempt(stageTrackerMergeGate(it.Repo, it.Number, stage, rec, hasRec, sweep, now), attempt, hasAttempt, it.Head, stage, rec, hasRec)
			it.Soak = stageTrackerSoakFrom(rec, hasRec, it.Head, soakWindow, now)
		}
		if m := trackerPRNumberRE.FindStringSubmatch(it.Reason); m != nil && it.Kind == "issue" {
			it.LinkedPR, _ = strconv.Atoi(m[1])
		}
		// An issue wearing a PR stage (050+) is riding its covering PR; its
		// own parking bay 045 is not a mirror.
		it.Mirrored = it.Kind == "issue" && stage != stagelabels.None && !stage.Applies(stagelabels.IssueItem)
		it.Modifiers, it.DeferredModifiers = stageTrackerModifiers(labels, status.HiveID, rec, hasRec, overrides, now, draft, it.Reason, it.Kind, stage, it.Mirrored)
		// 045/085 have their own chevron word ("Needs human decision"); the
		// chip only adds information when needs-decision modifies another
		// stage.
		if stage == stagelabels.NeedsDecision || stage == stagelabels.IssueNeedsDecision {
			it.Modifiers = slices.DeleteFunc(it.Modifiers, func(m string) bool { return m == modNeedsDecision })
		}
		// Likewise 015 says "Triage needed" itself, and a needs-triage left
		// on a claimed / covered issue is stale, not a signal: 015 is not a
		// gate, the stage word is the truth, and the reconciler removes the
		// label on its next pass (hivecommons/hive#14586). The chip stays
		// only before the claim (010 / 017 / 045).
		if stage == stagelabels.TriageNeeded || stage.SettlesTriage() {
			if stage != stagelabels.TriageNeeded && slices.Contains(it.Modifiers, modNeedsTriage) {
				it.StaleLabels = append(it.StaleLabels, modNeedsTriage)
			}
			it.Modifiers = slices.DeleteFunc(it.Modifiers, func(m string) bool { return m == modNeedsTriage })
		}
		stageTrackerApplyMergeTrain(it)
		it.Caption, it.Actor = stageTrackerCaption(stage, it.Reason, it.Author, it.Kind, it.Mirrored, rec, hasRec, it.LinkedPR, it.Head)
		if c := stageTrackerSoakCaption(rec, now); c != "" {
			it.Caption = c
		}
		if c := stageTrackerClaimConflictCaption(it); c != "" {
			it.Caption, it.Actor = c, StageActorHuman
		}
		if hasRec && rec.Stage != stagelabels.None {
			if at := stageTrackerNoteChangedAt(rec, now); !at.IsZero() {
				it.NoteChangedAt = &at
			}
		}
		if hasAttempt && (attempt.InFlight() || attempt.Merged) {
			it.Caption, it.Actor = stageTrackerMergingCaption(attempt), StageActorHive
		}
		// Adoption is clanker-only (a human's PR is never adopted), so a
		// human-authored 072 row keeps its plain caption. The one exception
		// is a person's PR the engine was asked to adopt and refused
		// (hivecommons/hive#15202): that row says "not adopted" wherever it sits.
		personRefused := hasRec && it.Kind == "pr" && rec.AdoptionBlocked(it.Head) == stagelabels.AdoptBlockedAuthorPerson
		if personRefused || (stage == stagelabels.ReturnedToContributor && it.Kind == "pr" && it.AuthorKind != authorKindHuman) {
			if it.Adoption = stageTrackerAdoption(*it, rec, hasRec, presence, maintainerPolicy, adoptionClock, now); it.Adoption != nil {
				it.Caption = stageTrackerAdoptionCaption(it.Caption, it.Adoption)
			}
		}
		it.Push = stageTrackerPushFor(*it, stage, rec, hasRec, pushStaleAfter, now)
		pushed := it.Push != nil && it.Push.State == pushStatePushed
		if pushed {
			// The ball is back with the hive: the row says what the push
			// queued, and its note ages from the push (a new hive line).
			it.Caption = stageTrackerPushCaption(it.Push, now)
			if it.NoteChangedAt == nil || it.Push.Since.After(*it.NoteChangedAt) {
				at := *it.Push.Since
				it.NoteChangedAt = &at
			}
		}
		// A pushed row no longer waits on its author: no presence marker,
		// no rework pickup word, no adoption clock — the pill names the
		// push instead.
		if pushed {
			it.Adoption = nil
		}
		stageTrackerApplyTrainEjection(it, rec, hasRec, now)
		if login := stageTrackerWaitingOn(*it, stage); login != "" && !pushed {
			it.Presence = stageTrackerPresenceFor(*it, login, stage, rec, hasRec, presence, adoptionClock, now)
			it.Rework = stageTrackerReworkFor(*it, login, stage, rec, hasRec, fixWork, now)
		}
		if it.Takeover = stageTrackerTakeover(*it, rec, hasRec, presence, rerouteCandidates, adoptionClock, now); it.Takeover != nil {
			it.Caption = stageTrackerTakeoverCaption(it.Caption, it.Takeover)
		}
		base, approver := stagelabels.HumanApprovedReason(it.Reason)
		it.HumanApprover = approver
		it.ManualMerge = stageTrackerManualMergeReason(base)
		if stage == stagelabels.ReadyForHumanMerge {
			it.HumanMergeReason, _, it.HumanMergeReasonText = stageTrackerHumanMergeReason(it.Reason)
			// An untrusted author gets the specific why-and-how-to-fix
			// instead of the generic trust sentence (hivecommons/hive#15078).
			if it.HumanMergeReason == stageengineprstage.HumanMergeReasonUntrustedAuthor {
				if why := stageTrackerUntrustedAuthorWhy(base, it.Author); why != "" {
					it.HumanMergeReasonText = "Hive won't merge it alone — " + why
				}
			}
			// A person already took the trust call for this head (#15136) —
			// unless the breaking-change rule holds, which no approval
			// releases (hivecommons/hive#15442).
			if login := stagelabels.AdmissionLogin(it.AdmittedBy); login != "" && it.HumanMergeReason != stageengineprstage.HumanMergeReasonBreakingChange {
				it.HumanMergeReasonText = "@" + login + " approved it; hive merges it on its next pass"
			}
		}
		if slices.Contains(it.Modifiers, modUnauthorized) || slices.Contains(it.DeferredModifiers, modUnauthorized) {
			it.UnauthorizedStep, it.Unauthorized = stageTrackerUnauthorized(it.Reason)
		}
		// A foreign bot's PR the lane filed under 070: nobody in the hive
		// is fixing it, and the caption must not claim so.
		if stage == stagelabels.Fixing && it.Kind == "pr" && stageTrackerBotAuthor(it.Author, it.AuthorKind) {
			it.Caption, it.Actor = stageTrackerBotFixingCaption(*it), StageActorBot
		}
		it.History = stageTrackerHistory(rec)
	}
	seen := map[string]bool{}
	// Keyed on (repo, kind, number): the cross-kind collision (a PR echoed by
	// the issues feed) is resolved before add, with the PR record winning.
	add := func(it StageTrackerItem) {
		key := it.Repo + "/" + it.Kind + "#" + strconv.Itoa(it.Number)
		if seen[key] {
			return
		}
		seen[key] = true
		resp.Items = append(resp.Items, it)
	}
	prState := s.stageTrackerPRStateResolver(status, resp.RecentlyMerged)
	prOpen := func(repo string, number int) bool { return prState(repo, number) == blockedOnPRStateOpen }
	supersedes := stageTrackerSupersedesIndex(store, status.GitHubBaseURL)
	lineage := stageTrackerLineageIndex(store, status.GitHubBaseURL)
	issueFilter := s.issueFilterConfig()
	addIssue := func(repoName string, issue github.Issue, bucket, bucketReason string, progress *github.IssueChildren, advisoryHint *github.IssueAdvisory) {
		// A closed issue has no stage; a stage label it closed wearing is
		// the closed-issue sweep's to remove, not a row to render
		// (hivecommons/homebrew-hive#127).
		if strings.EqualFold(issue.State, "closed") {
			return
		}
		issue.Repo = nonEmpty(issue.Repo, repoName)
		rec, ok := lookup(repoName, issue.Number)
		it := StageTrackerItem{Repo: repoName, Number: issue.Number, Kind: "issue", Title: issue.Title, Author: issue.Author, Lane: laneIssue,
			Labels: append([]string(nil), issue.Labels...), CreatedAt: timePtr(issue.CreatedAt), UpdatedAt: timePtr(issue.UpdatedAt),
			Bucket: bucket, BucketReason: bucketReason,
			URL: overviewItemURL(issue.URL, issue.Repo, issue.Number, "issue", status.GitHubBaseURL)}
		it.Children = issue.Children
		if it.Children == nil {
			it.Children = progress
		}
		if it.Children == nil {
			it.Children = github.TrackerTaskProgress(issue.Body)
		}
		it.IsTracker = github.RendersAsTracker(issue.IsTracker, issue.Labels, it.Children)
		it.ClaimReference = issue.ClaimContext != nil && !issue.ClaimContext.MergedPR && issue.ClaimContext.Reference && issue.ClaimContext.Decision != github.ClaimDecisionReleasedRedStalePR
		if issue.AuthorIsHuman {
			it.AuthorKind, it.AuthorKindSource = authorKindHuman, authorKindSrcInfer
		} else if strings.HasSuffix(strings.ToLower(issue.Author), botLoginSuffix) {
			it.AuthorKind, it.AuthorKindSource = authorKindHive, authorKindSrcInfer
		}
		if l, ok := config.FirstOriginLabel(issue.Labels); ok {
			it.Origin = config.OriginSourceOf(l)
		} else {
			for _, l := range issue.Labels {
				if canonical, ok := config.CanonicalAgentLaneLabel(l); ok {
					it.Origin = config.OriginSourceOf(canonical)
					break
				}
			}
		}
		stage := labelStage(issue.Labels)
		if stage == stagelabels.None && !(ok && rec.Stage != stagelabels.None) {
			stage = bucketStage(bucket)
		}
		// A held issue is "Open · on hold" whatever stage label or store
		// record the hold caught it with; the reconciler drops both on its
		// next cycle (hivecommons/hive#13012).
		if len(holdLabels(issue.Labels, status.HiveID)) > 0 {
			stage, rec, ok = stagelabels.None, stagelabels.Record{}, false
		}
		finish(&it, issue.Labels, stage, rec, ok, false)
		stageTrackerBlockedBy(&it, issue.OpenBlockers())
		stageTrackerBlockedOnPR(&it, issue.Body, bucketReason, status.GitHubBaseURL, prState)
		stageTrackerSinceUnblock(&it, rec, ok)
		advisory := stageTrackerAdvisoryFor(issue.Labels, issue.Body, advisoryHint)
		if advisory == nil {
			applyStageTrackerNew(&it, stageTrackerNewState(it, ok, issueFilter, status.Governor, now))
		}
		if it.Stage == trackerNumberClaimed {
			it.ClaimantKind = s.stageTrackerClaimantKind(repoName, issue.Number)
		}
		if stageTrackerClaimantStage(it.Kind, it.Stage) {
			var worker *stageTrackerWorker
			if w, ok := stageTrackerWorkerOn(workers, repoName, issue.Number); ok {
				worker = &w
			}
			it.Claimant = stageTrackerIssueClaimant(stageTrackerClaimInputs{Ledger: s.stageTrackerLedgerClaim(repoName, issue.Number),
				MarkerKind: issue.ClaimKind, MarkerHolder: issue.ClaimedBy, MarkerHive: issue.ClaimHive, Assignees: issue.Assignees,
				Worker: worker, Since: it.StageSince, HiveID: status.HiveID, IsClankerOwner: isClankerOwner})
		}
		for _, lp := range issue.LinkedPRs {
			// The claims index may still say "open" for a PR the hive closed
			// since (close-first supersession, hivecommons/hive#15135); the
			// snapshot and caches are asked before a closed PR becomes a
			// sub-row.
			if lp.Number > 0 && !lp.Merged && !strings.EqualFold(lp.State, "closed") && !stageTrackerLinkedPRFinished(prState(nonEmpty(lp.Repo, repoName), lp.Number)) {
				it.LinkedPRs = append(it.LinkedPRs, StageTrackerLinkedPR{Number: lp.Number, URL: lp.URL})
			}
		}
		if slices.Contains(it.Modifiers, modNeedsHuman) || it.Stage == stagelabels.IssueNeedsDecision.Number() {
			it.Blocker = stageTrackerBlockerFor(repoName, issue.Number)
		}
		it.DecisionNeeded = stageTrackerDecisionNeededWith(it, nil, sentinelLabel, now, bctx)
		it.StageActor = stageTrackerStageActor(it, workers, now)
		applyStageTrackerAdvisory(&it, advisory)
		add(it)
	}
	addPR := func(repoName string, pr github.PullRequest, verdict *github.MergeVerdict, bucket, bucketReason string) {
		pr.Repo = nonEmpty(pr.Repo, repoName)
		rec, ok := lookup(repoName, pr.Number)
		if ok && rec.Superseded() {
			// hivecommons/hive#14471: the hive's adoption of this PR merged
			// and the original was told. Closed or not (the App may lack
			// permission on a fork PR), it is finished work, not a row.
			return
		}
		it := StageTrackerItem{Repo: repoName, Number: pr.Number, Kind: "pr", Title: pr.Title, Author: pr.Author, Head: pr.HeadSHA,
			Labels: append([]string(nil), pr.Labels...), CreatedAt: timePtr(pr.CreatedAt), UpdatedAt: timePtr(pr.UpdatedAt),
			Bucket: bucket, BucketReason: bucketReason,
			URL: overviewItemURL(pr.URL, pr.Repo, pr.Number, "pr", status.GitHubBaseURL)}
		it.BaseRef = pr.BaseRef
		it.ciRed = strings.EqualFold(pr.CIStatus, "failure") || len(pr.FailingChecks) > 0
		it.AuthorKind, it.AuthorKindSource = stageTrackerAuthorKind(pr.Labels, pr.Author, pr.AppAuthored, pr.HiveAttributed)
		if it.AuthorKindSource == authorKindSrcLabel {
			resp.AuthorKindLabels = true
		}
		it.Tier = stageTrackerTier(pr.Labels)
		if verdict != nil && it.Tier == "" {
			it.Tier = strings.ToLower(strings.TrimSpace(verdict.Tier))
		}
		it.Lane = laneContributor
		if it.AuthorKind == authorKindHive || pr.AppAuthored {
			it.Lane = laneHive
		}
		finish(&it, pr.Labels, labelStage(pr.Labels), rec, ok, pr.Draft)
		if it.Stage == stagelabels.ReturnedToContributor.Number() && it.AuthorKind == authorKindClanker {
			var requesters []string
			if pr.Protection != nil {
				requesters = pr.Protection.ChangesRequestedBy
			}
			it.Caption = stageTrackerReturnedToHiveCaption(it.Caption, it.Author, requesters)
		}
		applyStageTrackerNew(&it, stageTrackerNewState(it, ok, issueFilter, status.Governor, now))
		// After the New caption: an unclassified clanker PR whose clanker
		// is offline says what that means for it.
		it.Caption = stageTrackerOfflineAuthorCaption(it.Caption, it.Presence, it.Stage, adoptionClock, now)
		it.SentinelAlert, it.SentinelFindings, it.SentinelReason = s.stageTrackerSentinel(evidenceVerifier, repoName, pr.Number, pr.HeadSHA, pr.Labels)
		// The sentinel chip is a merge-gate pill too (the trusted-author
		// notice and the real alert alike): sentinel_alert stays truthful
		// for the gate word and the Decide… preface, but before the gate
		// the chip is held with the other deferred pills.
		if it.SentinelAlert && stageTrackerPillsDeferred(it.Kind, it.Stage, it.Mirrored) {
			it.DeferredModifiers = append([]string{modSentinel}, it.DeferredModifiers...)
		}
		it.LinkedIssues = stageTrackerPRLinkedIssues(pr, repoName, status.GitHubBaseURL)
		if sup, ok := supersedes[stagelabels.Key(repoName, pr.Number)]; ok {
			it.Supersedes = &sup
		}
		it.Lineage = stageTrackerLineageFor(lineage, repoName, pr.Number, pr.HeadRef, status.GitHubBaseURL)
		info, found := superseded[github.ReviewQueueKey(qualifyRepo(repoName), pr.Number)]
		if !found {
			info, found = superseded[github.ReviewQueueKey(repoName, pr.Number)]
		}
		if found && !labelSet(pr.Labels)[github.SupersessionKeepOpenLabel] {
			it.Superseded = &info
			it.Caption = fmt.Sprintf("Superseded by #%d — decide", info.CloserPR)
			if info.KeepReason != "" {
				it.Caption += ": " + info.KeepReason
			}
			it.Actor = "maintainer"
			if it.Stage == trackerNumberFixing && stageTrackerBotAuthor(it.Author, it.AuthorKind) {
				// docs#500: a conflicting bot PR already replaced by a
				// merged one is not a maintainer's decision to make; the
				// bot's next run supersedes it (hivecommons/docs#500).
				it.Caption, it.Actor = stageTrackerBotFixingCaption(it), StageActorBot
			}
		}
		var agg *review.Aggregate
		if a, ok := verdictFor(repoName, pr.Number, pr.HeadSHA, stageTrackerParkedOnDecision(it)); ok {
			agg = &a
		}
		if it.Stage == trackerNumberReviewPending && it.Superseded == nil {
			if clause := stageTrackerReviewWaitClause(agg, dispatchState, qualifyRepo(repoName), repoName, pr.Number, pr.HeadSHA, now); clause != "" {
				it.Caption = joinClause(it.Caption, clause)
			}
		}
		if n, ok := stageTrackerFixerExhaustedAttempts(it.Reason); ok && it.Stage == trackerNumberNeedsDecision {
			it.Caption, it.Actor = stageTrackerFixerExhaustedCaption(n, stageTrackerFixerExhaustedSummary(agg)), "maintainer"
		}
		it.DecisionNeeded = stageTrackerDecisionNeededWith(it, agg, sentinelLabel, now, bctx)
		it.StageActor = stageTrackerStageActor(it, workers, now)
		stageTrackerApplyFixerStatus(&it, review.FixDispatchStatusFor(dispatchState, qualifyRepo(repoName), pr.Number, pr.HeadSHA), workers)
		if stageTrackerClaimantStage(it.Kind, it.Stage) {
			var worker *stageTrackerWorker
			if w, ok := stageTrackerWorkerOn(workers, repoName, pr.Number); ok {
				worker = &w
			}
			it.Claimant = stageTrackerPRClaimant(stageTrackerClaimInputs{Ledger: s.stageTrackerLedgerClaim(repoName, pr.Number), HiveID: status.HiveID,
				Worker: worker, Since: it.StageSince, Labels: pr.Labels, Author: it.Author, AuthorKind: it.AuthorKind, HiveAgent: pr.HiveAgent})
		}
		add(it)
	}
	for _, repo := range status.Repos {
		repoName := overviewRepoName(repo)
		// The repo's canonical open-items set (open_items.go): the same
		// items, deduplicated the same way, that the Overview tile and its
		// sidebar pill count, so the tracker's unfiltered total is the
		// tile's issues + PRs by construction.
		for _, it := range repoOpenItems(repo) {
			switch {
			case it.Kind == "issue" && !it.Sidelined:
				addIssue(repoName, it.Issue, "", "", nil, nil)
			case it.Kind == "pr" && !it.Sidelined:
				addPR(repoName, it.PR.pr, it.PR.verdict, "", "")
			case it.Kind == "issue":
				// The enumeration's sidelined issues: parked in a work-detail
				// bucket, the open items the first tracker never showed.
				w := it.WorkIssue
				addIssue(repoName, github.Issue{Repo: w.Repo, Number: w.Number, Title: w.Title, Author: w.Author, AuthorIsHuman: w.AuthorIsHuman, Labels: w.Labels, Assignees: w.Assignees,
					CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, URL: w.URL, ClaimKind: w.ClaimKind, ClaimedBy: w.ClaimedBy, ClaimHive: w.ClaimHive, LinkedPRs: w.LinkedPRs, ClaimContext: w.ClaimContext, IsTracker: w.IsTracker, Children: w.Children}, w.Bucket, w.Reason, w.TrackerProgress, w.Advisory)
			default:
				w := it.WorkPR
				addPR(repoName, github.PullRequest{Repo: w.Repo, Number: w.Number, Title: w.Title, Author: w.Author, Labels: w.Labels, Draft: w.Draft,
					AppAuthored: w.AppAuthored, HiveAttributed: w.HiveAttributed, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, URL: w.URL, BaseRef: w.BaseRef, LinkedIssues: w.LinkedIssues, ReferencedIssues: w.ReferencedIssues}, nil, w.Bucket, w.Reason)
			}
		}
	}
	resp.OpenIssues, resp.OpenPRs = stageTrackerOpenCounts(resp.Items)
	stageTrackerMergeTrainPostPass(resp.Items, mergeTrainQueueOptionsFrom(cfg, sweep, now), &s.mergeTrains)
	stageTrackerMergeTrainStallWarnings(&s.mergeTrains, resp.Items, now)
	stageTrackerLineageFillAuthors(resp.Items)
	s.applyOperatorActions(resp.Items, now)
	// A row the operator just closed from the dashboard leaves at once,
	// before GitHub's list catches up (hivecommons/hive#15436).
	resp.Items = stageTrackerDropOperatorClosed(resp.Items, now, operatorActionClosedGrace())
	resp.OpenIssues, resp.OpenPRs = stageTrackerOpenCounts(resp.Items)
	s.applyDecidedProjection(resp.Items, now)
	applyRebaseAttempts(resp.Items)
	s.applyRebaseMergeFollowThrough(resp.Items, now)
	resp.NeedsHuman, resp.OnHold = stageTrackerHumanCounts(resp.Items)
	resp.WaitingOnHuman = stageTrackerWaitingOnHuman(resp.Items)
	preparationState := s.stageTrackerPRStateResolver(nil, resp.Merged24h)
	resp.BasePreparation = s.stageTrackerPreparation(resp.Items, now, func(repo string, number int) string {
		for _, merged := range resp.Merged24h {
			if merged.Number == number && strings.EqualFold(merged.Repo, repo) {
				return blockedOnPRStateMerged
			}
		}
		if state := preparationState(repo, number); state != "" {
			return state
		}
		for _, closed := range resp.RecentlyClosed {
			if closed.Number == number && strings.EqualFold(closed.Repo, repo) {
				return blockedOnPRStateClosed
			}
		}
		return ""
	})
	stageTrackerResolveLinkedPRs(resp.Items, prOpen)
	stageTrackerMarkClosedBy(resp.Items, s.mergedLedger().ClosedBy)
	stageTrackerResolveActors(resp.Items)
	sort.SliceStable(resp.Items, stageTrackerLess(resp.Items))
	return resp
}

// Ordering tiers within a stage (stageTrackerAttentionTier; the dashboard's
// STAGE_TRACKER_TIER_* carry the same values).
const (
	stageTrackerTierAttention = 0
	stageTrackerTierNormal    = 1
	stageTrackerTierSunk      = 2
)

// stageTrackerAttentionStages are the stages that park an item on a person
// (dashboard STAGE_TRACKER_ATTENTION_STAGES): issue needs-decision, PR
// needs-author / returned-to-contributor, PR needs-decision and
// ready-for-human-merge.
var stageTrackerAttentionStages = []int{stagelabels.IssueNeedsDecision.Number(), stagelabels.NeedsAuthor.Number(), stagelabels.ReturnedToContributor.Number(), stagelabels.NeedsDecision.Number(), stagelabels.ReadyForHumanMerge.Number()}

// stageTrackerHumanMods are the modifiers that park an item on a person
// (dashboard STAGE_TRACKER_HUMAN_MODS).
var stageTrackerHumanMods = []string{modHold, modUnauthorized, modNeedsHuman, modNeedsDecision, modNeedsRebase, modBlocked, modStalled}

// stageTrackerNeedsHumanStages are the stages where only a person can
// advance the item (dashboard STAGE_TRACKER_NEEDS_HUMAN_STAGES): 015
// triage-needed, 045 issue needs-decision, 085 PR needs-decision, 090
// ready-for-human-merge; 071 needs-author and 072 returned-to-contributor
// count only when the author is a human (stageTrackerNeedsHumanAuthorStages).
var stageTrackerNeedsHumanStages = []int{stagelabels.TriageNeeded.Number(), stagelabels.IssueNeedsDecision.Number(), stagelabels.NeedsDecision.Number(), stagelabels.ReadyForHumanMerge.Number()}

// stageTrackerNeedsHumanAuthorStages park the PR on its author; a human
// author must act, a clanker's or hive's PR is re-kicked by the hive.
var stageTrackerNeedsHumanAuthorStages = []int{stagelabels.NeedsAuthor.Number(), stagelabels.ReturnedToContributor.Number()}

// stageTrackerNeedsHumanMods are the pills that mean "a person must decide
// or act" (dashboard STAGE_TRACKER_NEEDS_HUMAN_MODS). hold is deliberately
// absent (On hold is its own chip); stalled, regressed, blocked,
// needs-rebase and draft are the hive's or nobody's to move.
var stageTrackerNeedsHumanMods = []string{modNeedsHuman, modNeedsDecision, modUnauthorized}

// stageTrackerNeedsHuman is the ONE definition of "Needs human" behind the
// payload's per-item flag, its count and the SPA's chip + filter
// (dashboard stageTrackerItemNeedsHuman mirrors it): a human decision or
// action is the only thing that advances the item. A hold wins: a held
// item is paused, not waiting on a person (hivecommons/hive#15008).
func stageTrackerNeedsHuman(it StageTrackerItem) bool {
	// An advisory waits on nobody: reading and closing it is a courtesy,
	// not a decision the pipeline is parked on (hivecommons/hive#15381).
	if stageTrackerOnHold(it) || it.Advisory != nil {
		return false
	}
	if slices.ContainsFunc(it.Modifiers, func(m string) bool { return slices.Contains(stageTrackerNeedsHumanMods, m) }) {
		return true
	}
	if slices.Contains(stageTrackerNeedsHumanStages, it.Stage) {
		return true
	}
	return slices.Contains(stageTrackerNeedsHumanAuthorStages, it.Stage) && it.AuthorKind == authorKindHuman
}

// stageTrackerOnHold mirrors the dashboard's stageTrackerItemOnHold: the
// row wears the hold pill.
func stageTrackerOnHold(it StageTrackerItem) bool {
	return slices.Contains(it.Modifiers, modHold)
}

// stageTrackerHumanCounts sets every item's NeedsHuman / OnHold flag and
// returns the two totals for the payload header.
func stageTrackerHumanCounts(items []StageTrackerItem) (needsHuman, onHold int) {
	for i := range items {
		items[i].NeedsHuman = stageTrackerNeedsHuman(items[i])
		items[i].OnHold = stageTrackerOnHold(items[i])
		if items[i].NeedsHuman {
			needsHuman++
		}
		if items[i].OnHold {
			onHold++
		}
	}
	return needsHuman, onHold
}

// stageTrackerSunkMods are the modifiers that sink a row to the bottom of
// its stage (dashboard STAGE_TRACKER_SUNK_MODS): a person parked it, or it
// is a draft nobody should act on yet.
var stageTrackerSunkMods = []string{modHold, modDraft}

// stageTrackerRegressionHandled mirrors the dashboard's
// stageTrackerRegressionHandled: a regressed item with no human-gate
// modifier on a hive-driven stage is the hive's to work through.
func stageTrackerRegressionHandled(it StageTrackerItem) bool {
	if !slices.Contains(it.Modifiers, modRegressed) {
		return false
	}
	if slices.ContainsFunc(it.Modifiers, func(m string) bool { return slices.Contains(stageTrackerHumanMods, m) }) {
		return false
	}
	return !slices.Contains(stageTrackerAttentionStages, it.Stage)
}

// stageTrackerNeedsAttention mirrors the dashboard's
// stageTrackerNeedsAttention: the item shows a pill or sits on a stage
// parked on a person. Only the shown Modifiers count, so a merge-gate pill
// held back until the gate never promotes a row.
func stageTrackerNeedsAttention(it StageTrackerItem) bool {
	if it.Advisory != nil || stageTrackerRegressionHandled(it) {
		return false
	}
	return len(it.Modifiers) > 0 || slices.Contains(stageTrackerAttentionStages, it.Stage)
}

// stageTrackerAttentionTier is the item's ordering tier within its stage
// (the dashboard's stageTrackerAttentionTier agrees): rows parked on a
// person first, then the rest, then rows nobody should act on — hold
// (incl. do-not-merge/*) and draft. The direction toggle never flips it.
func stageTrackerAttentionTier(it StageTrackerItem) int {
	if slices.ContainsFunc(it.Modifiers, func(m string) bool { return slices.Contains(stageTrackerSunkMods, m) }) {
		return stageTrackerTierSunk
	}
	if stageTrackerNeedsAttention(it) {
		return stageTrackerTierAttention
	}
	return stageTrackerTierNormal
}

// stageTrackerLess orders the payload: stage descending, then
// attention-first within the stage (stageTrackerAttentionTier), then the
// item that has waited longest, then repo and number — the dashboard's
// default view, so chat and hub consumers read the same order.
func stageTrackerLess(items []StageTrackerItem) func(i, j int) bool {
	return func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Stage != b.Stage {
			return a.Stage > b.Stage
		}
		if ta, tb := stageTrackerAttentionTier(a), stageTrackerAttentionTier(b); ta != tb {
			return ta < tb
		}
		// Within a tier the item that has waited longest comes first: it is
		// the one most likely to need a human.
		at, bt := trackerSince(a), trackerSince(b)
		if at.IsZero() != bt.IsZero() {
			return !at.IsZero()
		}
		if !at.Equal(bt) {
			return at.Before(bt)
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Number < b.Number
	}
}

func trackerSince(it StageTrackerItem) time.Time {
	if it.StageSince != nil {
		return *it.StageSince
	}
	return time.Time{}
}

// stageTrackerClaimantKind maps the live claim holder's ledger kind onto
// the tracker's author kinds: a person is human, one of the hive's own
// agents is hive, a relay contributor or outside bot is clanker. Empty when
// the ledger is off or holds no claim.
func (s *Server) stageTrackerClaimantKind(repo string, number int) string {
	l := s.claimsLedger()
	if l == nil {
		return ""
	}
	c, ok := l.Lookup(repo, number)
	if !ok {
		return ""
	}
	switch c.Kind {
	case claims.KindHuman:
		return authorKindHuman
	case claims.KindAgent:
		return authorKindHive
	case claims.KindContributor, claims.KindExternal:
		return authorKindClanker
	}
	return ""
}

// stageTrackerOpenCounts counts the tracker's items by kind.
func stageTrackerOpenCounts(items []StageTrackerItem) (issues, prs int) {
	for _, it := range items {
		if it.Kind == "pr" {
			prs++
		} else {
			issues++
		}
	}
	return issues, prs
}

func repoWorkIssue(raw any) (github.RepoWorkIssue, bool) {
	switch v := raw.(type) {
	case github.RepoWorkIssue:
		return v, true
	case *github.RepoWorkIssue:
		if v != nil {
			return *v, true
		}
	}
	return github.RepoWorkIssue{}, false
}

func repoWorkPR(raw any) (github.RepoWorkPR, bool) {
	switch v := raw.(type) {
	case github.RepoWorkPR:
		return v, true
	case *github.RepoWorkPR:
		if v != nil {
			return *v, true
		}
	}
	return github.RepoWorkPR{}, false
}

// stageTrackerRESTCaller tags the endpoint's requests in the REST
// accounting (/api/gh-rate-limits). Serving the tracker is Charged 0; the
// only GitHub reads are the memoised issues GETs that title untitled links
// (stageTrackerEnrichSoftLinks).
const stageTrackerRESTCaller = "dashboard:stage-tracker"

// stageTrackerCache is the memoised payload: the encoded body, its ETag and
// when it was built.
type stageTrackerCache struct {
	body        []byte
	etag        string
	generatedAt time.Time
	// stale is set by invalidateStageTrackerCache: the body is still served
	// while the next request rebuilds, so an invalidation never blocks and a
	// reader never waits behind a build it did not start.
	stale bool
}

// stageTrackerBuild is the single in-flight /api/stage-tracker rebuild. done
// is closed when the cache has been updated (or the build failed).
type stageTrackerBuild struct {
	done chan struct{}
}

// stageTrackerSource values for the X-Hive-Stage-Tracker-Source header and
// the payload's source field.
const (
	stageTrackerSourceFreshName = "fresh"
	stageTrackerSourceCacheName = "cache"
	// stageTrackerSourceStaleName marks a body served from the cache while a
	// rebuild is in flight or after an invalidation: still the last complete
	// tracker, just older than the status it will be rebuilt from.
	stageTrackerSourceStaleName = "stale"
)

func (s *Server) stageTrackerCacheTTL() time.Duration {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.StageTrackerCacheTTL()
	}
	return (*config.Config)(nil).StageTrackerCacheTTL()
}

// stageTrackerPayload returns the cached body when it is younger than the
// TTL and not invalidated, else builds, encodes and caches a new one. fresh
// reports a rebuild by this call.
//
// The build runs OUTSIDE stageTrackerCacheMu and at most one runs at a time:
// the mutex only guards the cached fields, so invalidateStageTrackerCache
// (every webhook, every status publish) returns at once instead of queueing
// behind a build that walks the evidence tree on NFS. Requests that arrive
// while a build is in flight are served the previous body as "stale"; only a
// request with no body to fall back on waits, and only until the in-flight
// build finishes or its own context ends. Before this, the mutex was held
// across the build, so every webhook handler, the governor's publish and
// every tracker poll queued behind it in FIFO order — and because each
// publish and webhook re-invalidated the cache, the queue never drained
// (hivecommons/hive#14681: unbounded blocked handlers, connections and
// threads).
func (s *Server) stageTrackerPayload(status *StatusPayload, now time.Time) (body []byte, etag string, generatedAt time.Time, fresh bool) {
	var source string
	body, etag, generatedAt, source = s.stageTrackerPayloadSource(context.Background(), status, now)
	return body, etag, generatedAt, source == stageTrackerSourceFreshName
}

// stageTrackerPayloadSource is stageTrackerPayload reporting where the body
// came from (stageTrackerSource*Name; "" with a nil body when the build
// failed or ctx ended while waiting on an in-flight build).
func (s *Server) stageTrackerPayloadSource(ctx context.Context, status *StatusPayload, now time.Time) (body []byte, etag string, generatedAt time.Time, source string) {
	ttl := s.stageTrackerCacheTTL()
	s.stageTrackerCacheMu.Lock()
	s.stageTrackerServed = true
	c := s.stageTrackerCache
	if len(c.body) > 0 && !c.stale && now.Sub(c.generatedAt) < ttl {
		s.stageTrackerCacheMu.Unlock()
		return c.body, c.etag, c.generatedAt, stageTrackerSourceCacheName
	}
	if inflight := s.stageTrackerBuild; inflight != nil {
		s.stageTrackerCacheMu.Unlock()
		if len(c.body) > 0 {
			return c.body, c.etag, c.generatedAt, stageTrackerSourceStaleName
		}
		select {
		case <-inflight.done:
		case <-ctx.Done():
			return nil, "", now, ""
		}
		s.stageTrackerCacheMu.Lock()
		c = s.stageTrackerCache
		s.stageTrackerCacheMu.Unlock()
		if len(c.body) == 0 {
			return nil, "", now, ""
		}
		return c.body, c.etag, c.generatedAt, stageTrackerSourceCacheName
	}
	inflight := &stageTrackerBuild{done: make(chan struct{})}
	s.stageTrackerBuild = inflight
	gen := s.stageTrackerGen
	s.stageTrackerCacheMu.Unlock()

	built, ok := s.encodeStageTracker(status, now, ttl)

	s.stageTrackerCacheMu.Lock()
	if ok {
		// An invalidation that landed during the build means newer status
		// exists; keep serving this body but let the next request rebuild.
		built.stale = s.stageTrackerGen != gen
		s.stageTrackerCache = built
	}
	s.stageTrackerBuild = nil
	close(inflight.done)
	s.stageTrackerCacheMu.Unlock()
	if !ok {
		return nil, "", now, ""
	}
	return built.body, built.etag, built.generatedAt, stageTrackerSourceFreshName
}

// encodeStageTracker builds and encodes the tracker for status. It holds no
// server lock: this is the slow part (one evidence read per PR, on NFS in
// hosted hives).
func (s *Server) encodeStageTracker(status *StatusPayload, now time.Time, ttl time.Duration) (stageTrackerCache, bool) {
	build := s.buildStageTracker
	if s.stageTrackerBuildFn != nil {
		build = s.stageTrackerBuildFn
	}
	resp := build(status, s.stageStoreOrShared(), now)
	s.stageTrackerEnrichSoftLinks(resp.Items, stageTrackerRepoQualifier(status), now)
	resp.CacheTTLSeconds = int64(ttl / time.Second)
	resp.Settings = s.stageTrackerSettings()
	// The encoded body carries source=cache: that is what every later hit
	// serves verbatim, and what the ETag validates. The building request
	// alone rewrites the marker to fresh on its way out (handleStageTracker).
	resp.Source = stageTrackerSourceCacheName
	// The ETag hashes the body with generated_at zeroed: the timestamp moves
	// on every rebuild, so including it would turn an unchanged tracker into
	// a 200 each time (the client revalidates with this ETag and gets a 304).
	var hashBuf bytes.Buffer
	hashResp := resp
	hashResp.GeneratedAt = time.Time{}
	hashResp.BasePreparation = slices.Clone(resp.BasePreparation)
	for i := range hashResp.BasePreparation {
		hashResp.BasePreparation[i].ObservedAt = time.Time{}
	}
	if err := json.NewEncoder(&hashBuf).Encode(hashResp); err != nil {
		logOverviewEncodeFailure(err)
		return stageTrackerCache{}, false
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(resp); err != nil {
		logOverviewEncodeFailure(err)
		return stageTrackerCache{}, false
	}
	return stageTrackerCache{body: buf.Bytes(), etag: webstatic.ETagFor(hashBuf.Bytes()), generatedAt: now}, true
}

// invalidateStageTrackerCache marks the memoised payload stale.
// UpdateStatusIfFresh calls it on every status publish, and every webhook
// re-read that changed a row calls it too, so tracker freshness follows the
// eval cycle and the webhook stream rather than the cache TTL alone. It only
// flips a flag under the cache mutex — it never waits for a build in
// progress — so callers on the webhook and publish paths return at once. It
// reports whether the tracker has ever been served: only then is an SSE
// nudge worth sending, which also keeps the /api/events stream free of extra
// frames for clients that never asked for the tracker.
func (s *Server) invalidateStageTrackerCache() (served bool) {
	s.stageTrackerCacheMu.Lock()
	s.stageTrackerCache.stale = true
	s.stageTrackerGen++
	served = s.stageTrackerServed
	s.stageTrackerCacheMu.Unlock()
	return served
}

// stageTrackerSSEFrame is the `stage-tracker` event /api/events sends after
// each status publish; the SPA refetches /api/stage-tracker on it instead of
// waiting for its poll. seq is the status sequence the tracker now reflects.
func stageTrackerSSEFrame(seq uint64) string {
	return "event: stage-tracker\ndata: {\"seq\":" + strconv.FormatUint(seq, 10) + "}\n\n"
}

var stageTrackerSourceCache = []byte(`"source":"cache"`)
var stageTrackerSourceFresh = []byte(`"source":"fresh"`)
var stageTrackerSourceStale = []byte(`"source":"stale"`)

// handleStageTracker serves GET /api/stage-tracker from the memoised payload.
func (s *Server) handleStageTracker(w http.ResponseWriter, r *http.Request) {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	if status == nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "initializing"})
		return
	}
	now := time.Now().UTC()
	body, etag, generatedAt, source := s.stageTrackerPayloadSource(r.Context(), status, now)
	if body == nil {
		if r.Context().Err() != nil {
			jsonError(w, "stage tracker build still in progress", http.StatusServiceUnavailable)
			return
		}
		jsonError(w, "stage tracker unavailable", http.StatusInternalServerError)
		return
	}
	remaining := s.stageTrackerCacheTTL() - now.Sub(generatedAt)
	if remaining < 0 || source == stageTrackerSourceStaleName {
		remaining = 0
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(int64(remaining/time.Second), 10))
	w.Header().Set("X-Hive-Stage-Tracker-Source", source)
	w.Header().Set("X-Hive-Stage-Tracker-Generated-At", generatedAt.UTC().Format(time.RFC3339Nano))
	w.Header().Set("Last-Modified", generatedAt.UTC().Format(http.TimeFormat))
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		github.RecordLocalServe(stageTrackerRESTCaller, r.URL.Path, http.StatusNotModified)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	github.RecordLocalServe(stageTrackerRESTCaller, r.URL.Path, http.StatusOK)
	switch source {
	case stageTrackerSourceFreshName:
		body = bytes.Replace(body, stageTrackerSourceCache, stageTrackerSourceFresh, 1)
	case stageTrackerSourceStaleName:
		body = bytes.Replace(body, stageTrackerSourceCache, stageTrackerSourceStale, 1)
	}
	if _, err := w.Write(body); err != nil {
		logOverviewEncodeFailure(err)
	}
}

// stageTrackerParkedOnDecision reports whether a PR row waits at 085 on a
// reviewer's decision, so the popover may quote the question from the
// newest decision-requested verdict on any head (hivecommons/hive#15499).
func stageTrackerParkedOnDecision(it StageTrackerItem) bool {
	if it.Kind != "pr" || (it.Stage != stagelabels.NeedsDecision.Number() && !slices.Contains(it.Modifiers, modNeedsDecision)) {
		return false
	}
	base, _ := stagelabels.HumanApprovedReason(it.Reason)
	lower := strings.ToLower(strings.TrimSpace(base))
	return lower == stageengine.StageReasonVerdictRequiresHuman || lower == stageengine.StageReasonDecisionRequested
}

// stageTrackerSinceUnblock ages an unstaged, no-longer-blocked issue from
// the blocked sweep's lift rather than from creation
// (hivecommons/hive#15779). A row blocked again keeps its own age.
func stageTrackerSinceUnblock(it *StageTrackerItem, rec stagelabels.Record, hasRec bool) {
	if hasRec || rec.UnblockedAt.IsZero() || it.BlockedOnPR != nil || len(it.BlockedOn) > 0 || slices.Contains(it.Modifiers, modBlocked) {
		return
	}
	t := rec.UnblockedAt
	it.StageSince = &t
}
