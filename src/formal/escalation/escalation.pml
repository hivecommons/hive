/*
 * escalation.pml — protocol-level Spin model of the hive fix-loop escalation
 * lifecycle and the reviewer adjudication lane (post-#5485 machinery).
 *
 * Models ONE hive-authored PR interacting with:
 *   Sweeper   — runEscalationSweep + Store.Sweep     (cmd/hive/main.go, pkg/escalation)
 *   Reaper    — reapStuckRedPRs + Store.TryReEngage  (cmd/hive/main.go, pkg/escalation)
 *   Agent     — FIX-BEFORE-NEW fix lane pushes        (pkg/scheduler/policy_overlays.go)
 *   Reviewer  — reviewer lane adjudication            (pkg/scheduler/reviewer_lane.go)
 *   Human     — owner of the needs-human label queue
 *   CI        — per-SHA red/green verdicts
 *   Merger    — merges green PRs
 *   GenBumper — MachineryVersion bumps (amnesty generations)
 *
 * See README.md in this directory for the model <-> code mapping, the list of
 * abstractions, and the verification results.
 *
 * Property-specific instrumentation is compiled in per run (see run.sh) so
 * each exhaustive verification only carries the monitor state it needs:
 *   -DMON_AMNESTY  — P1 (one-shot amnesty) + P2 (no instant re-escalation)
 *   -DMON_BUDGET   — P3 (re-engagement budget bounds)
 *   -DMON_ADJ      — invariant w_onepass (one adjudication per PR, ever)
 *   -DMON_PENDING  — witness w_pending (ledger wipe on a PENDING observation)
 *   -DWATCHER      — merge-request watcher re-engage path + invariant
 *                    w_watcher (pkg/github/merge_request_watcher.go)
 *   -DACMM6        — reviewer may close after recommend-close (level >= 6)
 *
 * The G1/G3/G4 fixes (#5511) are the modeled DEFAULT, matching the shipped Go:
 *   G1 — Store.Sweep reconciles the reviewer's label-only verdict into the
 *        ledger (Escalated/RedSHAs reset when reviewer-passed is present and
 *        needs-human absent) — see the Sweeper's reconciliation step. This is
 *        what the former -DPATCH_REVIEWER run verified; it is now shipped.
 *   G3 — Store.TryReEngage refuses escalated entries (post-amnesty guard).
 *   G4 — a RECOMMEND-CLOSE verdict below ACMM 6 marks the PR
 *        (reviewer-recommend-close label) and the work list excludes it.
 */

#define NSHA        4   /* distinct head SHAs a PR may ever have (bounded pushes) */
#define THRESHOLD   3   /* escalation.DefaultThreshold */
#define MAX_REENG   6   /* escalation.MaxReEngagements */
#define GEN_MAX     3   /* machinery generations modeled (MachineryVersion bumps) */

/* CI verdict of the current head SHA */
#define CI_PENDING  0
#define CI_RED      1
#define CI_GREEN    2

/* PR lifecycle */
#define PR_OPEN     0
#define PR_MERGED   1
#define PR_CLOSED   2
#define PR_HUMAN    3   /* a human took ownership out-of-band (terminal) */

#define TERMINAL    (prState != PR_OPEN)

/* ---------------- forge / PR state ---------------- */
byte ci      = CI_PENDING;
byte prState = PR_OPEN;
byte minted  = 1;   /* SHAs minted so far; the CURRENT head SHA id is always
                     * the most recently minted one (SHAs strictly increase) */

bool needsHuman       = false;  /* escalation.NeedsHumanLabel on the PR */
bool reviewerPassed   = false;  /* escalation.ReviewerPassedLabel on the PR */
bool recommendedClose = false;  /* scheduler.ReviewerRecommendCloseLabel: the
                                 * recommend-close verdict was delivered (#5511
                                 * G4); the work list excludes marked rows */

/* ---------------- escalation ledger (escalation.Entry) ---------------- */
bool entryExists = false;
bool headCounted = false;  /* containsSHA(Entry.RedSHAs, head): whether the
                            * CURRENT head SHA is already in RedSHAs. Sound
                            * reduction: head SHAs are never reused, so only
                            * the current head's membership is ever queried. */
byte attempts    = 0;      /* len(Entry.RedSHAs) */
bool escalated   = false;  /* Entry.Escalated */
byte curRedSha   = 0;      /* Entry.CurRedSHA (0 = unset) */
bool stale       = false;  /* now - Entry.FirstRedAt >= RedPRStaleAfter */
byte reeng       = 0;      /* Entry.ReEngagements */
byte entryGen    = 0;      /* Entry.Machinery (Go zero value 0 on &Entry{}) */

byte gen = 1;              /* escalation.MachineryVersion (current generation) */

/* escalatedPRs map handed by runEscalationSweep to reapStuckRedPRs and to
 * writeMergeEligible (ci-failing.json row.Escalated). Refreshed by the sweep. */
bool escalatedView = false;

/* ---------------- property monitors (instrumentation, not machinery) ------ */
#ifdef MON_AMNESTY
bool amnestiedAtGen[GEN_MAX + 1];  /* per entry LIFETIME; cleared on delete */
bool amnestyThisPass   = false;    /* an amnesty fired in the current sweep pass */
bool amnestyLive       = false;    /* some amnesty happened this entry lifetime */
byte newShaSinceAmnesty = 0;       /* distinct-SHA appends after the amnesty pass
                                    * (saturates at THRESHOLD) */
#endif
#ifdef MON_BUDGET
byte totalReengThisSha = 0;  /* successful re-engagements for the current red
                              * SHA, NOT reset by amnesty (only by SHA change /
                              * entry delete); saturates just past its bound */
byte amnestiesThisSha  = 0;  /* amnesties granted while current red SHA unchanged */
#endif
#ifdef MON_ADJ
byte adjudications = 0;      /* reviewer adjudication passes (saturates at 2) */
#endif
#ifdef MON_PENDING
bool wipedPendingWithHistory = false; /* violation flag: sweep deleted a
                                       * non-empty entry on a PENDING (not
                                       * green) observation (must stay false
                                       * post-G2 fix, #5617) */
#endif
#ifdef WATCHER
bool mergeReqPending           = false; /* a stale merge request sits in the watcher dir */
bool watcherReEngagedEscalated = false; /* violation flag: re-engage budget was
                                         * granted while the entry was escalated
                                         * (must stay false post-G3 fix) */
#endif

/* ---------------- ledger helpers ---------------- */

/* Sweep's delete(s.entries, key): green convergence, or prune on close/merge. */
inline entry_clear() {
	entryExists = false;
	headCounted = false; attempts = 0;
	escalated = false;
	curRedSha = 0; stale = false; reeng = 0;
	entryGen = 0;
#ifdef MON_AMNESTY
	amnestiedAtGen[0] = false; amnestiedAtGen[1] = false;
	amnestiedAtGen[2] = false; amnestiedAtGen[3] = false;
	amnestyLive = false; amnestyThisPass = false;
	newShaSinceAmnesty = 0;
#endif
#ifdef MON_BUDGET
	totalReengThisSha = 0; amnestiesThisSha = 0;
#endif
}

/* Machinery amnesty — the shared block in Store.Sweep (escalation.go:181-186)
 * and Store.TryReEngage (escalation.go:372-377):
 *   if e.Machinery < MachineryVersion {
 *       e.Machinery = MachineryVersion; e.ReEngagements = 0
 *       e.Escalated = false;            e.RedSHAs = nil
 *   }
 */
inline amnesty_check() {
	if
	:: entryGen < gen ->
#ifdef MON_AMNESTY
		/* P1: one-shot per generation, per entry lifetime. Amnesty stamps
		 * entryGen = gen and gen is monotonic, so a second amnesty at the
		 * same generation for the same (undeleted) entry is unreachable. */
		assert(!amnestiedAtGen[gen]);
		amnestiedAtGen[gen] = true;
		amnestyThisPass = true;
		amnestyLive = true;
		newShaSinceAmnesty = 0;
#endif
#ifdef MON_BUDGET
		if
		:: amnestiesThisSha < GEN_MAX -> amnestiesThisSha++
		:: else -> skip
		fi;
#endif
		entryGen = gen;
		reeng = 0;
		escalated = false;
		headCounted = false; attempts = 0  /* RedSHAs = nil */
	:: else -> skip
	fi
}

/* Store.TryReEngage (escalation.go:352-385). obsSha == 0 models the merge
 * watcher's empty head SHA ("reuse the tracked red SHA"). Sets ok. */
inline try_reengage(obsSha, ok) {
	if
	:: !entryExists -> entryExists = true; entryGen = 0  /* &Entry{}: Machinery=0 */
	:: else -> skip
	fi;
	if
	:: obsSha != 0 && obsSha != curRedSha ->  /* branch moved: sync + reset */
		curRedSha = obsSha; stale = false; reeng = 0;
#ifdef MON_BUDGET
		totalReengThisSha = 0; amnestiesThisSha = 0;
#endif
	:: else -> skip
	fi;
	amnesty_check();
	if
	/* #5511 G3 fix (shipped): an escalated (needs-human) entry is out of the
	 * automated lane — refuse without burning budget. The amnesty above may
	 * have just un-escalated an older-generation entry, which then proceeds. */
	:: escalated -> ok = false
	:: !escalated && reeng >= MAX_REENG -> ok = false
	:: else ->
		reeng++;
		/* P3a: the per-SHA budget cap holds at every increment site. */
		assert(reeng <= MAX_REENG);
#ifdef MON_BUDGET
		if
		:: totalReengThisSha <= GEN_MAX * MAX_REENG -> totalReengThisSha++
		:: else -> skip
		fi;
		/* P3b: total nudges for one unchanged red SHA never exceed one full
		 * budget per amnesty granted while that SHA was current (+1 initial). */
		assert(totalReengThisSha <= (amnestiesThisSha + 1) * MAX_REENG);
#endif
		ok = true
	fi
}

/* ---------------- processes ---------------- */

/* One Store.Sweep pass over this PR's observation, as driven by
 * runEscalationSweep (main.go:7631). Red := (CIStatus == "failure"),
 * Pending := (CIStatus == "pending"). Since the #5617 G2 fix a PENDING
 * observation is a NO-OP: only green clears the entry, only red counts
 * (witness w_pending now holds). The escalatedPRs view consumed by the
 * reaper and by ci-failing.json is refreshed at the end of the pass, matching
 * the same-tick ordering sweep -> writeMergeEligible -> reap in main.go. */
active proctype Sweeper() {
	do
	:: atomic { prState == PR_OPEN ->
#ifdef MON_AMNESTY
		amnestyThisPass = false;
#endif
		if
		:: ci == CI_PENDING ->  /* G2 fix (#5617): pending is a no-op
			 * observation — the entry (and its distinct-SHA history)
			 * survives; only green clears, only red counts. The pre-fix
			 * code deleted the entry here (witness w_pending); the
			 * MON_PENDING flag can no longer be set on this path. */
			skip
		:: ci == CI_GREEN ->  /* green: Go's green branch -> delete entry */
#ifdef MON_PENDING
			if
			:: entryExists && ci == CI_PENDING && attempts > 0 ->
				wipedPendingWithHistory = true
			:: else -> skip
			fi;
#endif
			entry_clear()
		:: else ->
			if
			:: !entryExists ->  /* &Entry{Machinery: MachineryVersion} */
				entryExists = true; entryGen = gen
			:: else -> skip
			fi;
			amnesty_check();
			/* #5511 G1 fix (shipped): reviewer-verdict reconciliation. The
			 * reviewer's REPAIR/DE-ESCALATE verdict is label edits only; if
			 * the ledger still says Escalated but the PR no longer carries
			 * needs-human and does carry reviewer-passed, sync the verdict:
			 * reset the entry the way amnesty does (un-escalate, restart the
			 * distinct-SHA ledger, fresh budget, Machinery untouched) so the
			 * PR re-enters the normal lifecycle. Escalated is cleared, so the
			 * reset cannot repeat — fresh reds re-accumulate and a later
			 * re-escalation fires normally; reviewer-passed then routes it to
			 * the human, not back to the reviewer. */
			if
			:: escalated && reviewerPassed && !needsHuman ->
				escalated = false;
				headCounted = false; attempts = 0;  /* RedSHAs = nil */
				reeng = 0
			:: else -> skip
			fi;
			if
			:: !headCounted ->  /* unseen red SHA: append to RedSHAs */
				headCounted = true;
				attempts++;
#ifdef MON_AMNESTY
				if
				:: amnestyLive && !amnestyThisPass && newShaSinceAmnesty < THRESHOLD ->
					newShaSinceAmnesty++
				:: else -> skip
				fi
#endif
			:: else -> skip
			fi;
			if
			:: minted != curRedSha ->  /* branch moved: reset staleness + budget */
				curRedSha = minted; stale = false; reeng = 0;
#ifdef MON_BUDGET
				totalReengThisSha = 0; amnestiesThisSha = 0;
#endif
			:: else -> skip
			fi;
			if
			:: !escalated && (attempts >= THRESHOLD || reeng >= MAX_REENG) ->
				/* NewlyEscala: comment + needs-human label + MarkEscalated */
#ifdef MON_AMNESTY
				/* P2a: escalation never fires in the pass that granted amnesty. */
				assert(!amnestyThisPass);
				/* P2b: post-amnesty escalation needs THRESHOLD-1 genuinely new
				 * red SHAs, or a full fresh re-engagement budget (amnesty and
				 * SHA changes both zero `reeng`, so reeng >= MAX_REENG here
				 * implies every one of those nudges fired post-amnesty) —
				 * never zero post-amnesty evidence. */
				if
				:: amnestyLive ->
					assert(newShaSinceAmnesty >= THRESHOLD - 1
					       || reeng >= MAX_REENG)
				:: else -> skip
				fi;
#endif
				escalated = true;
				needsHuman = true
			:: else -> skip
			fi
		fi;
		escalatedView = escalated }
	:: atomic { TERMINAL ->  /* PR left the open set: prune (Sweep's seen map) */
		entry_clear(); escalatedView = false; break }
	od
}

/* CI resolves the pending verdict of the current head SHA, nondeterministically
 * red or green. One resolution per SHA; a flake retry would be a new push. */
active proctype CI() {
	do
	:: atomic { prState == PR_OPEN && ci == CI_PENDING ->
		if
		:: ci = CI_RED
		:: ci = CI_GREEN
		fi }
	:: TERMINAL -> break
	od
}

/* Wall clock for Store.StaleRed: the current red SHA has gone unchanged for
 * RedPRStaleAfter. Only the tracked CurRedSHA can turn stale (StaleRed
 * requires e.CurRedSHA == headSHA). */
active proctype Timer() {
	do
	:: atomic { prState == PR_OPEN && ci == CI_RED && minted == curRedSha && !stale ->
		stale = true }
	:: TERMINAL -> break
	od
}

/* The fix lane: FIX-BEFORE-NEW (policy_overlays.go, addRedPRFixFirst) routes red PRs back to
 * their author agent, which pushes another fix attempt (a new head SHA).
 * Escalated rows are skipped: `if pr.Escalated { continue }` — row.Escalated
 * is the ledger verdict carried through ci-failing.json. */
active proctype Agent() {
	do
	:: atomic { prState == PR_OPEN && ci == CI_RED && !escalatedView && minted < NSHA ->
		/* P6b: the fix lane never observes (acts on) an escalated entry. */
		assert(!escalated);
		minted++; headCounted = false; ci = CI_PENDING; stale = false }
	:: TERMINAL -> break
	od
}

/* reapStuckRedPRs (main.go:7590): red + stale + not escalated -> TryReEngage.
 * The escalated check reads the same-tick escalatedPRs map from the sweep. */
active proctype Reaper() {
	bool ok;
	do
	:: atomic { prState == PR_OPEN && ci == CI_RED && !escalatedView && stale && minted == curRedSha ->
		/* P6a: the reaper never observes (acts on) an escalated entry. */
		assert(!escalated);
		try_reengage(minted, ok) }
	:: TERMINAL -> break
	od
}

/* Reviewer lane (reviewer_lane.go, ACMM >= 5): adjudicates escalated rows from
 * ci-failing.json, excluding rows already labeled reviewer-passed or
 * reviewer-recommend-close (#5511 G4 fix: every verdict class marks the row,
 * so "one reviewer pass per PR, ever" holds unconditionally). REPAIR and
 * DE-ESCALATE are indistinguishable at this abstraction (label swap + push);
 * the ledger sync for those verdicts happens SWEEP-side (G1 fix), not here —
 * the reviewer edits labels only, exactly like the real agent. RECOMMEND-CLOSE
 * closes at ACMM >= 6 (-DACMM6), else comments, adds the recommend-close
 * label, and leaves needs-human in place. */
active proctype Reviewer() {
	do
	:: atomic { prState == PR_OPEN && ci == CI_RED && escalatedView && !reviewerPassed && !recommendedClose ->
		/* P4a: a PR carrying reviewer-passed is never adjudicated again. */
		assert(!reviewerPassed);
#ifdef MON_ADJ
		if
		:: adjudications < 2 -> adjudications++
		:: else -> skip
		fi;
#endif
		if
		:: minted < NSHA ->  /* REPAIR / DE-ESCALATE: fix or rebase, push, relabel */
			reviewerPassed = true;
			needsHuman = false;
			minted++; headCounted = false; ci = CI_PENDING; stale = false
		:: true ->           /* RECOMMEND-CLOSE */
#ifdef ACMM6
			prState = PR_CLOSED
#else
			/* Comment posted; needs-human stays; a human closes/owns it.
			 * The verdict is marked (reviewer-recommend-close label) so the
			 * work list never re-serves the row (#5511 G4). */
			recommendedClose = true
#endif
		fi }
	:: TERMINAL -> break
	od
}

/* The human owner of the needs-human queue: eventually takes over (out-of-band
 * ownership) or closes. The queue IS the label — a PR without needs-human is
 * invisible to the human. */
active proctype Human() {
	do
	:: atomic { prState == PR_OPEN && needsHuman ->
		if
		:: prState = PR_HUMAN
		:: prState = PR_CLOSED
		fi }
	:: TERMINAL -> break
	od
}

/* Merges a green PR (label-queued auto-merge sweep / merge watcher). */
active proctype Merger() {
	do
	:: atomic { prState == PR_OPEN && ci == CI_GREEN -> prState = PR_MERGED }
	:: TERMINAL -> break
	od
}

/* MachineryVersion bumps: a new fix-dispatch generation ships. Bounded. */
active proctype GenBumper() {
	do
	:: atomic { prState == PR_OPEN && gen < GEN_MAX -> gen++ }
	:: TERMINAL -> break
	od
}

#ifdef WATCHER
/* Merge-request watcher terminal path (merge_request_watcher.go): after
 * mergeRequestMaxAttempts failures on a red required check it calls the
 * mergeReEngage hook — TryReEngage with an empty head SHA — without consulting
 * the escalated set. The #5511 G3 fix guards STORE-side: TryReEngage refuses
 * escalated entries, so the watcher's call can no longer burn budget on a
 * needs-human PR (invariant w_watcher: budget is never granted while the
 * entry is escalated; a grant after machinery amnesty un-escalates first,
 * which is amnesty's intended semantics). */
active proctype MergeReqFiler() {
	if
	:: atomic { prState == PR_OPEN && !mergeReqPending -> mergeReqPending = true }
	:: TERMINAL -> skip
	fi
}
active proctype MergeWatcher() {
	bool ok;
	do
	:: atomic { prState == PR_OPEN && ci == CI_RED && mergeReqPending ->
		try_reengage(0, ok);
		if
		:: ok && escalated -> watcherReEngagedEscalated = true
		:: else -> skip
		fi;
		mergeReqPending = false }  /* request quarantined .exhausted */
	:: TERMINAL -> break
	od
}
#endif

/* ---------------- properties ---------------- */

/* P3 (LTL form): the re-engagement budget cap, as an invariant. */
ltl p3_cap    { [] (reeng <= MAX_REENG) }
#ifdef MON_BUDGET
/* P3 (LTL form): per unchanged red SHA, at most one full budget per modeled
 * generation — with GEN_MAX = 3 that is 3 * MAX_REENG = 18 total; a single
 * generation bump admits at most 2 * MAX_REENG = 12. */
ltl p3_total  { [] (totalReengThisSha <= GEN_MAX * MAX_REENG) }
#endif

/* P4b: the reviewer->human handoff. Once the reviewer has passed on a PR that
 * is (still or again) escalated, the PR must eventually reach the human queue
 * (needs-human) or leave the open set. HOLDS since the #5511 G1 fix: the
 * sweep syncs the reviewer's label verdict into the ledger, the PR re-enters
 * the automated lane, re-escalates on fresh evidence, and NewlyEscala
 * re-applies needs-human (the reviewer-passed exclusion then makes the queue
 * a human's). Failed on the pre-fix machinery — see README, gap G1. */
ltl p4_handoff { [] ((escalated && reviewerPassed) -> <> (needsHuman || TERMINAL)) }

/* P5: every escalated PR eventually reaches a terminal state (merged, closed,
 * or human-owned). HOLDS since the #5511 G1 fix (failed before it for the
 * same orphaned-PR reason — see README, gap G1). */
ltl p5_term   { [] (escalated -> <> TERMINAL) }

#ifdef MON_ADJ
/* "One reviewer pass per PR, ever" — for EVERY verdict class. HOLDS since the
 * #5511 G4 fix: a RECOMMEND-CLOSE verdict below ACMM 6 marks the row
 * (reviewer-recommend-close) and the work list excludes it, exactly like
 * reviewer-passed. Failed at ACMM 5 on the pre-fix machinery — gap G4. */
ltl w_onepass { [] (adjudications <= 1) }
#endif

#ifdef MON_PENDING
/* Ledger history survives until green: a PENDING observation never deletes the
 * entry. HOLDS since the #5617 G2 fix (pending is a no-op observation — only
 * green clears, only red counts); the pre-fix Store.Sweep treated pending as
 * !Red and wiped accumulated distinct-SHA attempt history — gap G2. */
ltl w_pending { [] (!wipedPendingWithHistory) }
#endif

#ifdef WATCHER
/* The merge watcher never burns re-engagement budget on an escalated
 * (needs-human) PR. HOLDS since the #5511 G3 fix (TryReEngage's escalated
 * guard); the pre-fix machinery granted budget through the watcher's
 * unguarded call — gap G3. */
ltl w_watcher { [] (!watcherReEngagedEscalated) }
#endif
