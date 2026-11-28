package dashboard

// Per-contributor provider-quota hold.
//
// A contributor whose provider account has run out of quota (a weekly plan
// limit, a spent budget) cannot run any task until the provider resets it.
// A relay that does not know to stand down keeps asking for work, and every
// task it is handed fails in seconds:
//
//	headless CLI exited with error: code 1: You've hit your weekly limit · resets Dec 1, 3pm (UTC)
//
// One contributor on a production hive failed twelve tasks this way in thirty
// hours. Each failure booked the ten-minute failure cooldown on the ISSUE and
// counted toward its quarantine, although nothing was wrong with the issue,
// and the contributor's own failure-streak pause is ten minutes too, so the
// two ended together and the same contributor took the same issue again.
//
// The relay's own quota hold (quota_hold, capability quota_hold_v1) is the
// first line: a relay that recognises the provider's refusal stops asking.
// This file is the hub's side, for every relay. When a task_failed reason is
// the provider saying the account is out of quota for now, the hub holds that
// contributor out of ALL repositories until the reset the provider stated,
// and does not count the failure against the issue: the issue gets the
// ten-minute release cooldown (bookReleaseCooldownKey, "let go, not failed")
// with no step toward quarantine, so another contributor is offered it once
// that window passes.
//
// Deliberate boundaries:
//   - The wording is matched only where the relay puts a CLI diagnostic:
//     after `headless CLI exited with error: code N: `, or at the start of
//     the reason. The same words inside a longer agent-written sentence do
//     not count. An authorization refusal (API Error: 403, "not allowed to
//     access model") is never quota: it does not expire.
//   - Only failures feed it. A completion with evidence behind it (a
//     verified pull request, a no_work_needed or blocked verdict, or a relay
//     that does not say how it decided, which includes the headless path's
//     exit code 0) clears the hold and its count: the account answered. An
//     evidence-less completion (isEvidenceLessCompletion) does not.
//   - The key is identityOf: the contributor plus the session label, which
//     defaults to the backend name. A quota belongs to one provider account,
//     so the contributor's relay on another backend stays usable.
//   - With a reset time the hub can read, the hold ends then. Without one it
//     lasts 30 minutes, then 1, 2 and 6 hours as the failures repeat. Either
//     way it lasts at least contributorQuotaHoldMin, five minutes longer
//     than the issue's release cooldown, so the two never end together and
//     the contributor is not back in time to retake the issue they just
//     dropped.
//   - The relay's own report does not fight the hub's. While either says
//     held, the contributor is treated as held. A relay "released" lifts a
//     hold the hub only guessed the length of, never before
//     contributorQuotaHoldMin has passed and never one that carries the
//     provider's stated reset.
//   - The reason is the relay's own text, and acting on it is bounded: it
//     can only take the sender out of the queue, and the issue still waits
//     its ten minutes, so a relay cannot use it to keep an issue hot.
//   - The failure streak (contribute_failure_streak.go) does not count such a
//     failure, and is reset, so its ten-minute pause never stacks on this one.
//   - Fix-pr tasks are not covered: the fix queue dispatches and settles them
//     on its own path (contribute_fixqueue.go).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	// A reset time names the zone of the contributor's CLI, so the hub needs
	// the zone database whatever image it runs in. The hive binary already
	// embeds it through pkg/hub; this keeps the package self-sufficient.
	_ "time/tzdata"
)

const (
	// contributorQuotaHoldMaxWait bounds a stated reset: a wait longer than
	// this reads as a time the hub misparsed, not as a promise.
	contributorQuotaHoldMaxWait = 8 * 24 * time.Hour
	// contributorQuotaHoldMin is the shortest a quota hold lasts, whatever
	// reset the reason states and whatever the relay reports: the release
	// cooldown the failed task's issue is given, plus a margin. Were the
	// two equal they would end together and the same relay could retake
	// the issue it just dropped; with the margin others are offered it
	// first.
	contributorQuotaHoldMin = (failedTaskCooldownMinutes + contributorQuotaHoldMarginMinutes) * time.Minute
	// contributorQuotaHoldMarginMinutes is that margin.
	contributorQuotaHoldMarginMinutes = 5
	// contributorQuotaResetJustPassed is how recently a clock-only reset
	// ("resets 3pm") may have passed and still be the one the provider
	// meant: the message was printed just before it and read just after.
	// Such a reset is not read as tomorrow's.
	contributorQuotaResetJustPassed = 10 * time.Minute
	// contributorQuotaHoldMemory is how long a record outlives its hold.
	// While it is kept the next quota failure escalates; once it is dropped
	// the next one starts again at the first rung.
	contributorQuotaHoldMemory = 24 * time.Hour
	// contributorQuotaHoldLimit bounds the ledger, as for the push holds.
	contributorQuotaHoldLimit = 1024
	// contributorQuotaHoldsFileName is the on-disk ledger, beside the no-PR
	// streak ledger.
	contributorQuotaHoldsFileName = "contributor-quota-holds.json"
	// contributorQuotaHoldReason names the refusal in the hub log and the
	// decision ring.
	contributorQuotaHoldReason = "contributor-quota-hold"
)

// contributorQuotaHoldLadder is how long a hold lasts when the provider gave
// no reset time the hub could read: one rung per consecutive quota failure,
// the last rung for every later one.
var contributorQuotaHoldLadder = []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 6 * time.Hour}

// providerQuotaWordings are the provider phrases that mean "out of quota for
// now". They mirror QUOTA_EXHAUSTION_PATTERNS in bin/lib/pane-classifier.js:
// only wordings for a quota that expires, never one for "you may not".
var providerQuotaWordings = []string{
	"exceeded your monthly quota",
	"used all your copilot free chat requests",
	"individual quota reached",
	"budget_exceeded",
	"budget has been exceeded",
	"provider spending limit reached",
	"refused the request on a spending limit",
	"gone over your budget allowance",
	"upgrade your subscription to increase your limits",
}

var (
	// providerQuotaHeadlessPrefixRE is what the relay puts in front of a
	// failed headless CLI's last output line.
	providerQuotaHeadlessPrefixRE = regexp.MustCompile(`(?i)^headless CLI exited with error: (?:code -?\d+|signal \w+): `)
	// providerQuotaHitLimitRE is the refusals the CLIs print, with the error
	// glyph or "ERROR:" either may put first: Claude Code's "You've hit your
	// limit", the same with one or two names from its own set ("weekly",
	// "5-hour", "Opus weekly", "monthly spend", "usage credit"), "You've hit
	// your org's monthly spend limit"
	// (or channel's, or usage), "You're out of usage credits", and Codex's
	// "You've hit your usage limit.". Any other limit (rate, context window,
	// max-turns) is not a quota.
	providerQuotaHitLimitRE = regexp.MustCompile(`(?i)^(?:ERROR:\s+|[■●⚠]\s*)?(?:` +
		`You[’']ve hit your (?:(?:org|channel)[’']s monthly (?:spend|usage) |(?:(?:5-hour|weekly|session|daily|monthly|usage|spend|credits?|opus|sonnet|haiku|fable) ){0,2})limit\b|` +
		`You[’']re out of usage credits\b)`)
	// providerQuotaRelayHoldRE is the reason a relay that parked itself on a
	// quota banner reports.
	providerQuotaRelayHoldRE = regexp.MustCompile(`(?i)^provider quota exhausted for \S`)
	// providerQuotaChromeRE is the chrome a CLI prints an API refusal under:
	// "API Error:" or agy's ⚠ banner.
	providerQuotaChromeRE = regexp.MustCompile(`(?i)^(?:[■●⚠]\s*)?API Error:|^⚠\s.*\bquota reached\b`)
	// providerQuotaAuthorizationRE is a refusal that is not about quota.
	providerQuotaAuthorizationRE = regexp.MustCompile(`(?i)\bAPI Error: 403\b|\bnot allowed to access model\b|\bteam not allowed to access\b`)

	// providerQuotaResetInRE is "Resets in 4h42m28s": the run of
	// number-and-unit pairs after "resets in". providerQuotaResetAtRE is the
	// clock forms Claude Code prints: "resets 3pm (UTC)", "resets Dec 1, 3pm
	// (UTC)" and, when the reset is in another calendar year, "resets Jan 2,
	// 2027, 9am (UTC)", minutes optional, the zone being the CLI's own
	// ("UTC", or an IANA name such as "America/New_York").
	providerQuotaResetInRE     = regexp.MustCompile(`(?i)\bresets?\s+in\s+((?:\d+\s*[a-z]+\s*)+)`)
	providerQuotaResetInPartRE = regexp.MustCompile(`(?i)(\d+)\s*([a-z]+)`)
	providerQuotaResetAtRE     = regexp.MustCompile(`(?i)\bresets\s+(?:([a-z]{3,9})\s+(\d{1,2}),\s*(?:(\d{4}),\s*)?)?(\d{1,2})(?::(\d{2}))?\s*([ap]m)\s*\(([A-Za-z0-9_+/-]+)\)`)
)

var providerQuotaResetUnits = map[string]time.Duration{
	"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute, "s": time.Second,
}

var providerQuotaResetMonths = map[string]time.Month{
	"jan": time.January, "january": time.January,
	"feb": time.February, "february": time.February,
	"mar": time.March, "march": time.March,
	"apr": time.April, "april": time.April,
	"may": time.May,
	"jun": time.June, "june": time.June,
	"jul": time.July, "july": time.July,
	"aug": time.August, "august": time.August,
	"sep": time.September, "sept": time.September, "september": time.September,
	"oct": time.October, "october": time.October,
	"nov": time.November, "november": time.November,
	"dec": time.December, "december": time.December,
}

// providerQuotaDiagnostic returns the part of a task_failed reason where the
// relay puts a CLI's own last words: what follows the headless-exit prefix,
// or the whole reason when there is none.
func providerQuotaDiagnostic(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if loc := providerQuotaHeadlessPrefixRE.FindStringIndex(reason); loc != nil {
		return strings.TrimSpace(reason[loc[1]:])
	}
	return reason
}

// isProviderQuotaExhaustion reports whether a task_failed reason is the
// provider saying the contributor's account is out of quota for now. See the
// file comment for where the wording must stand and what never counts.
func isProviderQuotaExhaustion(reason string) bool {
	diag := providerQuotaDiagnostic(reason)
	if diag == "" || providerQuotaAuthorizationRE.MatchString(diag) {
		return false
	}
	if providerQuotaHitLimitRE.MatchString(diag) || providerQuotaRelayHoldRE.MatchString(diag) {
		return true
	}
	lower := strings.ToLower(diag)
	chromed := providerQuotaChromeRE.MatchString(diag)
	bare := strings.TrimLeft(lower, "■●⚠ ")
	for _, wording := range providerQuotaWordings {
		if strings.HasPrefix(bare, wording) || (chromed && strings.Contains(lower, wording)) {
			return true
		}
	}
	return false
}

// parseProviderQuotaReset reads the reset time a quota refusal states, as of
// now. It reports false when there is none it can read, so the caller falls
// back to its own ladder:
//
//   - "Resets in 4h42m28s": units d, h, m and s only ("1 month" is not one
//     minute);
//   - "resets 3pm (UTC)": the next 3pm in the named zone, unless that time
//     passed within contributorQuotaResetJustPassed, which is the reset the
//     provider meant and not tomorrow's;
//   - "resets Dec 1, 3pm (UTC)": this year's date, or next year's when this
//     year's has passed;
//   - "resets Jan 2, 2027, 9am (UTC)": that date; a past one is unreadable.
//
// The zone is resolved with time.LoadLocation; a name it does not know (an
// abbreviation such as "PST", or "Local") is unreadable. So is any wait over
// contributorQuotaHoldMaxWait.
func parseProviderQuotaReset(reason string, now time.Time) (time.Time, bool) {
	if m := providerQuotaResetInRE.FindStringSubmatch(reason); m != nil {
		var wait time.Duration
		for _, part := range providerQuotaResetInPartRE.FindAllStringSubmatch(m[1], -1) {
			unit, ok := providerQuotaResetUnits[strings.ToLower(part[2])]
			n, err := strconv.Atoi(part[1])
			if !ok || err != nil || n > 100000 {
				return time.Time{}, false
			}
			wait += time.Duration(n) * unit
		}
		if wait <= 0 || wait > contributorQuotaHoldMaxWait {
			return time.Time{}, false
		}
		return now.UTC().Add(wait), true
	}
	m := providerQuotaResetAtRE.FindStringSubmatch(reason)
	if m == nil || strings.EqualFold(m[7], "local") {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(m[7])
	if err != nil {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(m[4])
	if err != nil || hour < 1 || hour > 12 {
		return time.Time{}, false
	}
	minute := 0
	if m[5] != "" {
		if minute, err = strconv.Atoi(m[5]); err != nil || minute > 59 {
			return time.Time{}, false
		}
	}
	hour %= 12
	if strings.EqualFold(m[6], "pm") {
		hour += 12
	}
	local := now.In(loc)
	var reset time.Time
	switch {
	case m[1] == "":
		// Clock only: today's, or tomorrow's when today's is well past.
		today := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		previous, next := today.AddDate(0, 0, -1), today
		if today.Before(now) {
			previous, next = today, today.AddDate(0, 0, 1)
		}
		if now.Sub(previous) <= contributorQuotaResetJustPassed {
			return time.Time{}, false
		}
		reset = next
	default:
		month, ok := providerQuotaResetMonths[strings.ToLower(m[1])]
		day, err := strconv.Atoi(m[2])
		if !ok || err != nil {
			return time.Time{}, false
		}
		// The stated year, or this year and then the next.
		years := []int{local.Year(), local.Year() + 1}
		if m[3] != "" {
			year, err := strconv.Atoi(m[3])
			if err != nil {
				return time.Time{}, false
			}
			years = []int{year}
		}
		for _, year := range years {
			candidate := time.Date(year, month, day, hour, minute, 0, 0, loc)
			if candidate.Month() != month || candidate.Day() != day || candidate.Before(now) {
				continue
			}
			reset = candidate
			break
		}
		if reset.IsZero() {
			return time.Time{}, false
		}
	}
	if reset.Sub(now) > contributorQuotaHoldMaxWait {
		return time.Time{}, false
	}
	return reset.UTC(), true
}

// contributorQuotaHoldBackoff is how long hold number count lasts when the
// provider stated no reset.
func contributorQuotaHoldBackoff(count int) time.Duration {
	if count < 1 {
		count = 1
	}
	if count > len(contributorQuotaHoldLadder) {
		count = len(contributorQuotaHoldLadder)
	}
	return contributorQuotaHoldLadder[count-1]
}

// contributorQuotaHold is the in-memory and on-disk shape of one contributor
// identity's quota hold.
type contributorQuotaHold struct {
	// Identity is the identityOf key the hold is booked and enforced under.
	Identity      string `json:"identity"`
	ContributorID string `json:"contributor_id,omitempty"`
	Username      string `json:"username,omitempty"`
	// Count is how many quota failures in a row booked a hold, with no
	// completed task in between.
	Count  int       `json:"count"`
	HeldAt time.Time `json:"held_at"`
	Until  time.Time `json:"until"`
	// ResetStated is true when Until is the reset time the provider gave,
	// false when it is a rung of contributorQuotaHoldLadder or the
	// contributorQuotaHoldMin floor.
	ResetStated bool `json:"reset_stated,omitempty"`
	// Reason is the relay's failure reason: one line, token redacted, capped.
	Reason string `json:"reason,omitempty"`
	// skipLogged is the Until whose selectTask refusal has been logged.
	skipLogged time.Time
}

// forgotten reports whether the record's hold ended so long ago that its
// count no longer applies.
func (r contributorQuotaHold) forgotten(now time.Time) bool {
	return r.Count <= 0 || r.Until.IsZero() || !now.Before(r.Until.Add(contributorQuotaHoldMemory))
}

// contributorQuotaHoldState is the ledger, keyed by identity. quotaHoldMu
// guards quotaHolds; quotaHoldsFile overrides the ledger's path;
// quotaHoldSaveMu is held from a save's snapshot through its rename, so saves
// land in snapshot order.
type contributorQuotaHoldState struct {
	quotaHoldMu     sync.Mutex
	quotaHolds      map[string]contributorQuotaHold
	quotaHoldsFile  string
	quotaHoldSaveMu sync.Mutex
}

// bookContributorQuotaHold books a quota hold of c's identity at now. It
// returns the hold that now stands and whether this call booked it: a quota
// failure inside a standing hold returns that hold and false.
func (h *ContributeWSHub) bookContributorQuotaHold(c *ContributorConnection, reason string, now time.Time) (contributorQuotaHold, bool) {
	identity := identityOf(c)
	if h == nil || identity == "" {
		return contributorQuotaHold{}, false
	}
	h.quotaHoldMu.Lock()
	if h.quotaHolds == nil {
		h.quotaHolds = make(map[string]contributorQuotaHold)
	}
	rec := h.quotaHolds[identity]
	if rec.Count > 0 && now.Before(rec.Until) {
		h.quotaHoldMu.Unlock()
		return rec, false
	}
	if rec.forgotten(now) {
		rec = contributorQuotaHold{}
	}
	rec.Identity = identity
	rec.ContributorID = c.profile.ContributorID
	rec.Username = c.profile.GitHubUsername
	rec.Count++
	rec.HeldAt = now
	if reset, ok := parseProviderQuotaReset(reason, now); ok {
		rec.Until, rec.ResetStated = reset, true
	} else {
		rec.Until, rec.ResetStated = now.Add(contributorQuotaHoldBackoff(rec.Count)), false
	}
	// A reset a second away must not hand the contributor the same issue
	// back before its release cooldown has run. The floor is the hub's time,
	// not the provider's.
	if floor := now.Add(contributorQuotaHoldMin); rec.Until.Before(floor) {
		rec.Until, rec.ResetStated = floor, false
	}
	rec.Reason = normalizePushHoldReason(reason)
	rec.skipLogged = time.Time{}
	h.quotaHolds[identity] = rec
	h.pruneContributorQuotaHoldsLocked(now)
	h.quotaHoldMu.Unlock()
	h.saveContributorQuotaHolds()
	return rec, true
}

// pruneContributorQuotaHoldsLocked drops forgotten records and then, past
// contributorQuotaHoldLimit, the records whose holds ended longest ago.
// Callers must hold quotaHoldMu.
func (h *ContributeWSHub) pruneContributorQuotaHoldsLocked(now time.Time) {
	for key, rec := range h.quotaHolds {
		if rec.forgotten(now) {
			delete(h.quotaHolds, key)
		}
	}
	over := len(h.quotaHolds) - contributorQuotaHoldLimit
	if over <= 0 {
		return
	}
	keys := make([]string, 0, len(h.quotaHolds))
	for key := range h.quotaHolds {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := h.quotaHolds[keys[i]], h.quotaHolds[keys[j]]
		if !a.Until.Equal(b.Until) {
			return a.Until.Before(b.Until)
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys[:over] {
		delete(h.quotaHolds, key)
	}
}

// clearContributorQuotaHold forgets identity's quota hold and its count, and
// reports whether there was a record. Called when a task of that identity
// completes: the provider answered.
func (h *ContributeWSHub) clearContributorQuotaHold(identity string) bool {
	if h == nil || identity == "" {
		return false
	}
	h.quotaHoldMu.Lock()
	_, had := h.quotaHolds[identity]
	delete(h.quotaHolds, identity)
	h.quotaHoldMu.Unlock()
	if had {
		h.saveContributorQuotaHolds()
	}
	return had
}

// releaseGuessedContributorQuotaHold shortens identity's standing hold when
// the hub only guessed its length (no stated reset), keeping the count so the
// next quota failure still escalates. Called when the relay reports its own
// quota hold released: the relay knows its account better than a ladder
// does. The hold then ends now, or contributorQuotaHoldMin after it was
// booked if that is later. A hold with the provider's stated reset is left
// standing. It reports whether the hold was shortened.
func (h *ContributeWSHub) releaseGuessedContributorQuotaHold(identity string, now time.Time) bool {
	if h == nil || identity == "" {
		return false
	}
	h.quotaHoldMu.Lock()
	rec, ok := h.quotaHolds[identity]
	end := now
	if floor := rec.HeldAt.Add(contributorQuotaHoldMin); end.Before(floor) {
		end = floor
	}
	shortened := ok && rec.Count > 0 && !rec.ResetStated && end.Before(rec.Until)
	if shortened {
		rec.Until = end
		rec.skipLogged = time.Time{}
		h.quotaHolds[identity] = rec
	}
	h.quotaHoldMu.Unlock()
	if shortened {
		h.saveContributorQuotaHolds()
	}
	return shortened
}

// contributorQuotaHoldActive reports whether identity is quota-held at now,
// with the hold. A lapsed record is left in place so the next quota failure
// escalates.
func (h *ContributeWSHub) contributorQuotaHoldActive(identity string, now time.Time) (contributorQuotaHold, bool) {
	if h == nil || identity == "" {
		return contributorQuotaHold{}, false
	}
	h.quotaHoldMu.Lock()
	defer h.quotaHoldMu.Unlock()
	rec, ok := h.quotaHolds[identity]
	if !ok || rec.Count <= 0 || !now.Before(rec.Until) {
		return contributorQuotaHold{}, false
	}
	return rec, true
}

// contributorQuotaHoldSkip is contributorQuotaHoldActive for selectTask: the
// third return is true the first time a hold's refusal is seen, so the
// caller logs each hold once and not on every ready.
func (h *ContributeWSHub) contributorQuotaHoldSkip(identity string, now time.Time) (contributorQuotaHold, bool, bool) {
	if h == nil || identity == "" {
		return contributorQuotaHold{}, false, false
	}
	h.quotaHoldMu.Lock()
	defer h.quotaHoldMu.Unlock()
	rec, ok := h.quotaHolds[identity]
	if !ok || rec.Count <= 0 || !now.Before(rec.Until) {
		return contributorQuotaHold{}, false, false
	}
	first := !rec.skipLogged.Equal(rec.Until)
	if first {
		rec.skipLogged = rec.Until
		h.quotaHolds[identity] = rec
	}
	return rec, true, first
}

// contributorQuotaHoldNotice is what the relay prints for a quota hold, when
// it is booked and with every refusal while it stands: what happened, until
// when, and how to change it. It states the time the hold ends, never a
// duration, because it is repeated for as long as the hold lasts.
func contributorQuotaHoldNotice(hold contributorQuotaHold) string {
	until := hold.Until.UTC().Format(time.RFC3339)
	msg := "Your provider says this account is out of quota, so this hive will offer you no tasks until " + until
	if hold.ResetStated {
		msg += ", the reset time the provider gave."
	} else {
		msg += ": the provider gave no reset time this hive could read, so that is when it tries you again, waiting longer each time this repeats."
	}
	return msg + " The task that failed went back to the queue and does not count as a failure of its issue." +
		" To work sooner, run the relay on a backend that still has quota (it connects as its own session)." +
		" After a model change on the same backend, work resumes when this pause ends." +
		" Otherwise just wait: the relay stays connected and asks again by itself."
}

// holdContributorForQuota is called for a task_failed whose reason
// isProviderQuotaExhaustion accepted. It books the quota hold for c and, for
// a hold this call booked, logs it, records the decision and tells the relay
// in a `notice` frame. It reports whether a hold was booked.
func (h *ContributeWSHub) holdContributorForQuota(c *ContributorConnection, task *WSTaskAssign, reason string) bool {
	if h == nil || c == nil || c.profile == nil {
		return false
	}
	hold, booked := h.bookContributorQuotaHold(c, reason, time.Now())
	if !booked {
		return false
	}
	taskID, repo, number := "", "", 0
	if task != nil {
		taskID, repo, number = task.TaskID, task.Repo, task.Number
	}
	until := hold.Until.UTC().Format(time.RFC3339)
	// Info, not Warn: this is the hold working as designed, and a fleet of
	// relays on one exhausted plan would repeat it.
	h.logger.Info("[contribute-ws] "+contributorQuotaHoldReason+": provider quota exhausted; no work offered to this contributor, and the failure is not counted against the issue",
		"username", hold.Username, "identity", hold.Identity,
		"task", taskID, "repo", repo, "number", number,
		"hold_count", hold.Count, "held_until", until, "reset_stated", hold.ResetStated,
		"reason", hold.Reason)
	h.recordDecision(decisionUsername(c), decisionRefused, taskID, repo, number,
		contributorQuotaHoldReason+": the relay reported its provider quota exhausted; hold "+
			strconv.Itoa(hold.Count)+" until "+until)
	if c.ws != nil {
		if err := c.send(WSMessage{Type: "notice", Seq: h.nextSeq(), Message: contributorQuotaHoldNotice(hold)}); err != nil {
			h.logger.Warn("[contribute-ws] failed to send the quota hold notice", "username", hold.Username, "error", err)
		}
	}
	return true
}

// contributorHoldReason names the hold that keeps c from being handed work in
// repo at now: the quota hold (all repositories), or the push hold for that
// repository. It is empty when neither stands. selectTask reports each hold
// in its own way; this is for the paths that only need a yes or no, such as
// standby dispatch.
func (h *ContributeWSHub) contributorHoldReason(c *ContributorConnection, repo string, now time.Time) string {
	if _, held := h.contributorQuotaHoldActive(identityOf(c), now); held {
		return contributorQuotaHoldReason
	}
	if _, held := h.contributorRepoPushHoldActive(contributorFlapIdentity(c), repo, now); held {
		return contributorRepoPushHoldReason
	}
	return ""
}

// ContributorQuotaHoldView is one active quota hold as GET /api/contributors
// shows it, on the contributor it belongs to.
type ContributorQuotaHoldView struct {
	Reason string `json:"reason,omitempty"`
	Count  int    `json:"count"`
	HeldAt string `json:"held_at"`
	Until  string `json:"until"`
	// ResetStated says Until is the reset time the provider gave, not the
	// hub's own retry rung.
	ResetStated bool `json:"reset_stated,omitempty"`
	// Session is the relay session the hold was booked under, when the relay
	// declared one.
	Session string `json:"session,omitempty"`
}

// QuotaHolds returns the quota holds active at now, keyed by ContributorID
// and sorted by session.
func (h *ContributeWSHub) QuotaHolds(now time.Time) map[string][]ContributorQuotaHoldView {
	if h == nil {
		return nil
	}
	out := make(map[string][]ContributorQuotaHoldView)
	h.quotaHoldMu.Lock()
	for _, rec := range h.quotaHolds {
		if rec.Count <= 0 || rec.ContributorID == "" || !now.Before(rec.Until) {
			continue
		}
		_, session, _ := strings.Cut(rec.Identity, "#")
		out[rec.ContributorID] = append(out[rec.ContributorID], ContributorQuotaHoldView{
			Reason:      rec.Reason,
			Count:       rec.Count,
			HeldAt:      rec.HeldAt.UTC().Format(time.RFC3339),
			Until:       rec.Until.UTC().Format(time.RFC3339),
			ResetStated: rec.ResetStated,
			Session:     session,
		})
	}
	h.quotaHoldMu.Unlock()
	for _, views := range out {
		sort.Slice(views, func(i, j int) bool { return views[i].Session < views[j].Session })
	}
	return out
}

// contributorQuotaHoldsPath is where the ledger lives: the per-hub override,
// else the directory the no-PR streak ledger is written to.
func (h *ContributeWSHub) contributorQuotaHoldsPath() string {
	if h != nil && h.quotaHoldsFile != "" {
		return h.quotaHoldsFile
	}
	return filepath.Join(filepath.Dir(h.noPRStreaksPath()), contributorQuotaHoldsFileName)
}

func (h *ContributeWSHub) loadContributorQuotaHolds() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.quotaHoldMu.Lock()
	defer h.quotaHoldMu.Unlock()
	data, err := os.ReadFile(h.contributorQuotaHoldsPath())
	if err != nil {
		return
	}
	var records []contributorQuotaHold
	if json.Unmarshal(data, &records) != nil {
		return
	}
	now := time.Now()
	for _, rec := range records {
		if rec.Identity == "" || rec.forgotten(now) {
			continue
		}
		if h.quotaHolds == nil {
			h.quotaHolds = make(map[string]contributorQuotaHold)
		}
		h.quotaHolds[rec.Identity] = rec
	}
	h.pruneContributorQuotaHoldsLocked(now)
	h.logger.Info("[contribute-ws] loaded contributor quota holds", "count", len(h.quotaHolds))
}

func (h *ContributeWSHub) saveContributorQuotaHolds() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.quotaHoldSaveMu.Lock()
	defer h.quotaHoldSaveMu.Unlock()
	now := time.Now()
	h.quotaHoldMu.Lock()
	saved := make([]contributorQuotaHold, 0, len(h.quotaHolds))
	for _, rec := range h.quotaHolds {
		if rec.forgotten(now) {
			continue
		}
		saved = append(saved, rec)
	}
	h.quotaHoldMu.Unlock()
	sort.Slice(saved, func(i, j int) bool { return saved[i].Identity < saved[j].Identity })
	h.writeContributorHoldLedger(h.contributorQuotaHoldsPath(), saved, "contributor quota holds")
}
