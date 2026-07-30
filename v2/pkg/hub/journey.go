package hub

import (
	"fmt"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// User-journey nudges.
//
// Every hive owner walks the same adoption path:
//
//	Stage 1  Install the GitHub App          URGENT   — days, not weeks
//	Stage 2  Assign a method/model, OR run   MEDIUM   — a few weeks
//	         the contributor relay
//	Stage 3  Raise the ACMM level            SLOW     — months, never forced
//
// This file computes, server-side, which stage each hive is stalled on and
// turns that into an escalating banner delivered over the existing per-hive
// hub-banner channel (see hubBanners in server.go and the heartbeat response
// assembly in handleHeartbeat). It NEVER de-provisions anything: the strongest
// action it takes is warning that a human may de-provision the spoke.
// ─────────────────────────────────────────────────────────────────────────────

// ── Tunable thresholds ──────────────────────────────────────────────────────
//
// Every duration the journey nudger uses lives in this block so the whole
// escalation policy can be retuned in one place. Nothing below this comment
// hard-codes a bare duration.

const (
	// Stage 1 (GitHub App) is urgent: the spoke cannot open issues or PRs
	// without it, so an uninstalled App means the hive is burning cluster
	// capacity while producing nothing. The product policy is "immediately,
	// and never outstanding more than 1-2 weeks", so the escalation runs
	// gentle → firm → de-provision warning inside a two-week envelope.

	// stage1GentleAfter is how long after registration the first, friendly
	// "don't forget to install the App" nudge goes out.
	stage1GentleAfter = 2 * 24 * time.Hour // 2 days

	// stage1FirmAfter is when the tone hardens — the hive has now been idle
	// for a full week with nothing to show for it.
	stage1FirmAfter = 7 * 24 * time.Hour // 1 week

	// stage1WarnAfter is when the spoke is told, explicitly, that it is a
	// candidate for de-provisioning. Set at the far end of the product's
	// "1-2 weeks" window.
	stage1WarnAfter = 14 * 24 * time.Hour // 2 weeks

	// stage2GraceAfter is how long a hive may sit with the App installed but
	// no method/model assigned and no contributor relay activity before it is
	// warned. The product allows ~2-3 weeks after stage 1 is satisfied; this
	// is the gentle end of that window.
	stage2GraceAfter = 14 * 24 * time.Hour // 2 weeks

	// stage2WarnAfter is the far end of the same 2-3 week window, at which
	// point a de-provision warning is permitted — but ONLY when neither a
	// method/model nor the relay is in use.
	stage2WarnAfter = 21 * 24 * time.Hour // 3 weeks

	// stage3SuggestAfter is how long a hive rests at a given ACMM level before
	// it is invited (never pushed) to consider the next one. ACMM movement is
	// measured in months, so this is deliberately slow and never escalates.
	stage3SuggestAfter = 30 * 24 * time.Hour // ~1 month
)

// Re-send intervals. A banner is re-delivered only after its stage's interval
// has elapsed since the last send, so a hive stalled for months sees a steady
// drumbeat rather than one banner per heartbeat.
const (
	// stage1ResendInterval is short because stage 1 is urgent and the whole
	// window is only two weeks — a weekly reminder would fire twice total.
	stage1ResendInterval = 3 * 24 * time.Hour // every 3 days

	// stage2ResendInterval is weekly: medium urgency over a 2-3 week window.
	stage2ResendInterval = 7 * 24 * time.Hour // weekly

	// stage3ResendInterval is monthly, matching the "gentle reminder about
	// once a month" policy for ACMM graduation.
	stage3ResendInterval = 30 * 24 * time.Hour // monthly
)

// journeyBannerIDPrefix namespaces journey banner IDs so they are visually
// distinct from admin-sent banners (bannerIDPrefix, "hub-banner-") in logs and
// in the spoke's banner-dismiss state. The spoke keys dismissal off the banner
// ID, so an ID that changes per stage+severity means each escalation step
// re-appears even if the owner dismissed the previous one.
const journeyBannerIDPrefix = "journey-"

// journeyStatePath is the on-disk file holding per-hive nudge state (what was
// last sent, when) and admin snoozes. A var, not a const, so tests can point it
// at a temp file — same pattern as hubBannersPath.
var journeyStatePath = "/data/saas/journey-state.json"

// maxJourneySnoozeDuration bounds how far in the future an admin may snooze a
// hive's nudges. Long enough to cover a quarter of legitimate exemption
// (vendor pilots, hives parked for a conference), short enough that a snooze
// set and forgotten eventually lapses back into the normal cadence.
const maxJourneySnoozeDuration = 90 * 24 * time.Hour

// JourneyStage identifies a step on the adoption path. The zero value means
// "no stage outstanding" — every stage is satisfied.
type JourneyStage int

const (
	// StageNone means the hive has satisfied every stage we track. Stage 3 is
	// never fully "done" (there is always a next ACMM level until L6), so this
	// is reached only by a hive at the maximum level.
	StageNone JourneyStage = iota
	// StageGitHubApp — the GitHub App is not installed yet.
	StageGitHubApp
	// StageMethodModel — no agent has a method/model, and the contributor
	// relay is not carrying the load either.
	StageMethodModel
	// StageACMMLevel — the hive works, but has been resting at its current
	// ACMM level long enough to be worth a graduation suggestion.
	StageACMMLevel
)

// String renders a stage as a short stable token used in the registry JSON and
// in the My Hives table.
func (s JourneyStage) String() string {
	switch s {
	case StageGitHubApp:
		return "github-app"
	case StageMethodModel:
		return "method-model"
	case StageACMMLevel:
		return "acmm-level"
	default:
		return "none"
	}
}

// JourneySeverity describes how hard a nudge pushes. It maps onto the banner
// colors the spoke already understands (validBannerColors in saas.go).
type JourneySeverity int

const (
	// SeverityNone — nothing to say yet.
	SeverityNone JourneySeverity = iota
	// SeverityGentle — a friendly, informational reminder.
	SeverityGentle
	// SeverityFirm — this is overdue and needs attention.
	SeverityFirm
	// SeverityDeprovisionWarning — the owner is told a human may reclaim the
	// spoke. NEVER produced for StageACMMLevel; see nextNudge.
	SeverityDeprovisionWarning
)

// String renders a severity as a stable token for the API and the UI.
func (s JourneySeverity) String() string {
	switch s {
	case SeverityGentle:
		return "gentle"
	case SeverityFirm:
		return "firm"
	case SeverityDeprovisionWarning:
		return "deprovision-warning"
	default:
		return "none"
	}
}

// bannerColor maps a severity onto one of the four colors the spoke dashboard
// accepts. Anything unknown degrades to the calmest color rather than to an
// invalid one the spoke would refuse to render.
func (s JourneySeverity) bannerColor() string {
	switch s {
	case SeverityFirm:
		return "amber"
	case SeverityDeprovisionWarning:
		return "amber"
	case SeverityGentle:
		return "blue"
	default:
		return "gray"
	}
}

// acmmLevelInfo describes one ACMM level for nudge copy. Names and summaries
// are taken from the authoritative pack definitions in
// v2/pkg/config/packs/level-*.yaml so a suggestion always names the real next
// level and what adopting it actually changes.
type acmmLevelInfo struct {
	Name    string
	Summary string
}

// acmmLevels maps a level number to its name and a one-line summary of what
// moving there entails. Kept in sync with v2/pkg/config/packs/level-*.yaml.
var acmmLevels = map[int]acmmLevelInfo{
	1: {"Assisted", "an interactive brainstorm agent turns ideas into structured facts and scaffolding, with every action human-approved"},
	2: {"Instructed", "five agents (supervisor, scanner, quality, guide, brainstorm) publish advisory findings to your dashboard and a tracking issue — still no GitHub writes"},
	3: {"Measured", "the quality agent starts opening issues and hold-gated PRs about testing gaps, and ci-maintainer joins to watch build health — nothing merges without you"},
	4: {"Adaptive", "every agent files GitHub issues in its lane (bugs, docs gaps, workflow breakage) and a sec-check agent joins for vulnerabilities — issues only, still no PRs"},
	5: {"Semi-Automated", "agents open pull requests as well as issues, all carrying a 'hold' label for you to batch-review, and architect plus strategist join — the system proposes, you dispose"},
	6: {"Fully Autonomous", "agents open issues, raise PRs, and auto-merge them on green CI with no hold label, plus an outreach agent for community work — the highest trust setting"},
}

// maxACMMLevel is the top of the 6-level model. A hive here has nowhere to
// graduate to, so it gets no ACMM nudge at all.
const maxACMMLevel = 6

// acmmLevelLabel renders a level as "L3 Measured", falling back to a bare
// "L3" for any level outside the known table so copy never reads "L9 <nil>".
func acmmLevelLabel(level int) string {
	if info, ok := acmmLevels[level]; ok {
		return fmt.Sprintf("L%d %s", level, info.Name)
	}
	return fmt.Sprintf("L%d", level)
}

// JourneyState is the per-hive record of what we last told this owner. It is
// persisted so a hub restart does not restart the whole escalation ladder and
// re-spam every hive.
type JourneyState struct {
	// LastStage / LastSeverity are the stage+severity of the most recent
	// banner actually delivered. A change in either forces an immediate
	// re-send regardless of the interval — an escalation should not wait.
	LastStage    string `json:"last_stage,omitempty"`
	LastSeverity string `json:"last_severity,omitempty"`
	// LastSentAt is when that banner was queued (RFC3339).
	LastSentAt string `json:"last_sent_at,omitempty"`
	// LastACMMLevel is the level the hive sat at when the last ACMM nudge
	// went out. If the owner graduates, the level changes and the next
	// suggestion is allowed immediately rather than a month later.
	LastACMMLevel int `json:"last_acmm_level,omitempty"`
	// SnoozedUntil (RFC3339) suppresses ALL journey nudges for this hive
	// until the given time. Set by an admin for legitimately exempt hives.
	SnoozedUntil string `json:"snoozed_until,omitempty"`
	// SnoozedBy / SnoozeReason record who granted the exemption and why, so a
	// silent hive is explainable months later.
	SnoozedBy    string `json:"snoozed_by,omitempty"`
	SnoozeReason string `json:"snooze_reason,omitempty"`
}

// snoozeActive reports whether an admin snooze is currently suppressing nudges.
// An unparseable timestamp is treated as "not snoozed" so corrupt state fails
// toward nudging rather than toward permanent silence.
func (js *JourneyState) snoozeActive(now time.Time) bool {
	if js == nil || js.SnoozedUntil == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, js.SnoozedUntil)
	if err != nil {
		return false
	}
	return now.Before(until)
}

// JourneyStatus is the computed, read-only view of where a hive stands. It is
// attached to each registry entry for the My Hives table and the admin API.
type JourneyStatus struct {
	// Stage is the first unsatisfied stage, as a stable token.
	Stage string `json:"stage"`
	// StageNum is the same thing numerically, for sorting in the UI.
	StageNum int `json:"stageNum"`
	// Severity is how hard the current nudge pushes.
	Severity string `json:"severity"`
	// Message is the banner copy that would be (or was) delivered.
	Message string `json:"message,omitempty"`
	// StalledFor is a human-readable age of the current stall ("9 days").
	StalledFor string `json:"stalledFor,omitempty"`
	// ViaRelay marks a hive that satisfies stage 2 through the contributor
	// relay rather than by assigning a method/model. The UI shows this as a
	// distinct badge — such a hive is NOT silently reported as "assigned".
	ViaRelay bool `json:"viaRelay,omitempty"`
	// Snoozed is true while an admin exemption is in force.
	Snoozed bool `json:"snoozed,omitempty"`
	// SnoozedUntil echoes the exemption expiry for the UI tooltip.
	SnoozedUntil string `json:"snoozedUntil,omitempty"`
}

// ── Stage detection ─────────────────────────────────────────────────────────

// githubAppInstalled reports whether stage 1 is satisfied.
//
// Two spoke-reported signals feed this (both on RegistryEntry, populated in
// handleHeartbeat):
//
//   - PendingGitHubAppInstall — the spoke is actively waiting for an install to
//     complete, so the App is definitively not usable yet.
//   - GitHubAppRequired — the spoke's config demands App auth. Combined with a
//     recorded permission problem (GitHubAppPermIssue) this means the install
//     is missing or broken.
//
// A hive that never asked for App auth (GitHubAppRequired false) is treated as
// satisfied: it authenticates some other way and stage 1 does not apply.
func githubAppInstalled(h *RegistryEntry) bool {
	if h == nil {
		return true
	}
	if h.PendingGitHubAppInstall {
		return false
	}
	if h.GitHubAppRequired && strings.TrimSpace(h.GitHubAppPermIssue) != "" {
		return false
	}
	return true
}

// githubAppBlockedByOperator reports whether this hive's GitHub App stage is
// stalled for a reason ONLY the hub operator can fix — the App private key was
// never delivered to the spoke, or the key it holds belongs to a different App
// so GitHub rejects every JWT it signs.
//
// The owner cannot see, supply, or correct that key. Nudging them to "install
// the GitHub App" is wrong, and escalating to a de-provision warning would
// penalise a user for OUR failure. So when this is true the journey system
// stays silent on stage 1 entirely: the hive is not on the adoption journey,
// it is waiting on us.
//
// It reads the spoke-reported classification. An empty or unrecognised value
// (a spoke too old to report it) is NOT treated as operator-blocked — that
// would silence genuine "you have not installed the App" nudges for the whole
// fleet. Unknown means "fall through to the existing behaviour".
func githubAppBlockedByOperator(h *RegistryEntry) bool {
	if h == nil {
		return false
	}
	return appStateIsOperatorSide(h.GitHubAppState)
}

// appStateIsOperatorSide is the token-level predicate, so callers holding a
// different entry type (MyHiveEntry in drift.go) share one definition.
func appStateIsOperatorSide(state string) bool {
	return operatorSideAppStates[strings.TrimSpace(state)]
}

// operatorSideAppStates are the github.AppAuthState wire tokens that describe
// a credential failure only the hub operator can repair. Kept as literal
// tokens rather than importing pkg/github so the hub does not take a
// dependency on the spoke's GitHub client; pkg/github's
// TestAppAuthState_WireRoundTrip locks the token strings on the other side.
//
//   - "key-missing" — no App private key reached this spoke at all.
//   - "key-invalid" — a key is present but GitHub rejects the JWT it signs,
//     i.e. it belongs to a different App than the hive claims to be.
var operatorSideAppStates = map[string]bool{
	appStateKeyMissingToken: true,
	appStateKeyInvalidToken: true,
}

const (
	// appStateKeyMissingToken / appStateKeyInvalidToken mirror
	// github.AppStateKeyMissing.String() and github.AppStateKeyInvalid.String().
	appStateKeyMissingToken = "key-missing"
	appStateKeyInvalidToken = "key-invalid"
)

// methodModelAssigned reports whether any agent on this hive has a method
// (backend) or model assigned.
//
// AgentsWithModel is a pointer so that a spoke too old to report it (nil) is
// distinguishable from a spoke reporting a genuine zero. A nil value means "we
// cannot tell", and the caller must not treat that as an unsatisfied stage —
// we never threaten a hive over a signal we did not receive.
func methodModelAssigned(h *RegistryEntry) (assigned bool, known bool) {
	if h == nil || h.AgentsWithModel == nil {
		return false, false
	}
	return *h.AgentsWithModel > 0, true
}

// relayInUse reports whether the contributor relay is carrying this hive's
// work, which the product accepts as a full substitute for a method/model.
//
// Two signals, strongest first:
//
//   - A leaderboard entry with a non-empty CurrentTask means a human
//     contributor is actively working a task right now — the relay is
//     unambiguously in use.
//   - ContributorCount (registered profiles on the spoke's disk) > 0 means
//     people have signed up to this hive's relay. This is durable across the
//     gaps when nobody happens to be connected, which matters because a nudge
//     must not fire just because the relay is idle at 3am.
//
// ActiveContributors alone is deliberately NOT sufficient on its own — it is a
// live-WebSocket count that drops to zero whenever contributors disconnect —
// but a non-zero value does confirm use.
func relayInUse(h *RegistryEntry) bool {
	if h == nil {
		return false
	}
	if h.ContributorCount > 0 || h.ActiveContributors > 0 {
		return true
	}
	for _, e := range h.Leaderboard {
		if strings.TrimSpace(e.CurrentTask) != "" {
			return true
		}
	}
	return false
}

// stageSatisfiedAt returns the timestamp from which the current stage's clock
// runs, plus whether it is usable. Stage 1 measures from registration. Stages 2
// and 3 have no per-stage "satisfied at" timestamp in the registry, so they
// also measure from registration — which is conservative: it can only make a
// nudge fire later relative to when the previous stage was actually cleared,
// never earlier.
func stageSatisfiedAt(h *RegistryEntry) (time.Time, bool) {
	if h == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, h.RegisteredAt)
	if err != nil || t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}

// humanDuration renders a stall age for the UI, in the coarsest unit that is
// still informative.
func humanDuration(d time.Duration) string {
	const hoursPerDay = 24
	days := int(d.Hours() / hoursPerDay)
	switch {
	case days >= 2:
		return fmt.Sprintf("%d days", days)
	case days == 1:
		return "1 day"
	default:
		hours := int(d.Hours())
		if hours <= 1 {
			return "under an hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}
}

// nudge is a fully-formed decision: which stage, how hard, and exactly what to
// say. A zero Stage means "say nothing".
type nudge struct {
	Stage    JourneyStage
	Severity JourneySeverity
	Message  string
	// Resend is the minimum gap before this same stage+severity is sent again.
	Resend time.Duration
	// ViaRelay records that stage 2 was satisfied through the contributor
	// relay, for display purposes.
	ViaRelay bool
	// StalledFor is the age of the stall, for display.
	StalledFor time.Duration
}

// nextNudge computes the current journey decision for one hive.
//
// The stage is the FIRST unsatisfied stage, checked in order. Severity is a
// function of how long the stall has run, against the tunable thresholds at the
// top of this file.
//
// Invariant enforced here and asserted by tests: a StageACMMLevel nudge is
// never returned with SeverityDeprovisionWarning, and its copy never mentions
// de-provisioning. ACMM movement is an invitation, not a demand.
func nextNudge(h *RegistryEntry, now time.Time) nudge {
	if h == nil {
		return nudge{}
	}

	since, ok := stageSatisfiedAt(h)
	if !ok {
		// No trustworthy clock — never escalate against a timestamp we could
		// not parse.
		return nudge{}
	}
	age := now.Sub(since)
	if age < 0 {
		age = 0
	}

	// ── Stage 1: GitHub App ────────────────────────────────────────────────
	if !githubAppInstalled(h) {
		// Never nudge — and above all never threaten de-provisioning — when
		// the block is operator-side. The owner has done everything asked of
		// them; the missing piece is a private key the hub distributes. Say
		// nothing here and let the spoke's own banner explain that an admin is
		// required.
		if githubAppBlockedByOperator(h) {
			return nudge{}
		}
		n := nudge{Stage: StageGitHubApp, Resend: stage1ResendInterval, StalledFor: age}
		switch {
		case age >= stage1WarnAfter:
			n.Severity = SeverityDeprovisionWarning
			n.Message = fmt.Sprintf(
				"Action required: the KubeStellar Hive GitHub App is still not installed on %s, %s after this hive registered. "+
					"Without it your agents cannot read issues or open pull requests, so this spoke is consuming cluster capacity and producing nothing. "+
					"Install it from your hive dashboard (Settings → GitHub App) to start work immediately. "+
					"Spokes left in this state are candidates for de-provisioning by an administrator.",
				orgLabel(h), humanDuration(age))
		case age >= stage1FirmAfter:
			n.Severity = SeverityFirm
			n.Message = fmt.Sprintf(
				"Your hive still has no GitHub App installed after %s. Until it is installed, no agent can open an issue or a pull request — "+
					"the hive is running but cannot do any work. Install the KubeStellar Hive GitHub App on %s from Settings → GitHub App on your dashboard.",
				humanDuration(age), orgLabel(h))
		case age >= stage1GentleAfter:
			n.Severity = SeverityGentle
			n.Message = fmt.Sprintf(
				"Welcome aboard! One step left before your agents can work: install the KubeStellar Hive GitHub App on %s. "+
					"It grants the scoped, short-lived tokens agents use to read issues and raise pull requests. "+
					"You can do it from Settings → GitHub App on your dashboard.",
				orgLabel(h))
		default:
			return nudge{}
		}
		return n
	}

	// ── Stage 2: method/model, or the contributor relay ────────────────────
	//
	// The relay is a first-class substitute: a hive whose humans are picking up
	// tasks through /contribute has satisfied this stage as completely as one
	// that pointed an agent at a model.
	viaRelay := relayInUse(h)
	assigned, known := methodModelAssigned(h)
	if !viaRelay && !assigned {
		// `known == false` means the spoke is too old to report the signal. We
		// do not nudge — let alone threaten — on the basis of a value that was
		// never sent.
		if !known {
			return nudge{}
		}
		n := nudge{Stage: StageMethodModel, Resend: stage2ResendInterval, StalledFor: age}
		switch {
		case age >= stage2WarnAfter:
			n.Severity = SeverityDeprovisionWarning
			n.Message = fmt.Sprintf(
				"Action required: after %s, none of your agents has a method or model assigned, and no one is picking up tasks through your contributor relay. "+
					"Assign a backend and model to at least one agent (Agents → pick an agent → Model), or open your hive's /contribute page to your community — "+
					"either one counts. Spokes that stay idle past this point are candidates for de-provisioning by an administrator.",
				humanDuration(age))
		case age >= stage2GraceAfter:
			n.Severity = SeverityFirm
			n.Message = fmt.Sprintf(
				"Your hive has been up for %s but no agent has a method or model assigned yet, so nothing is running. "+
					"Pick a backend and model for an agent under Agents → Model. Prefer humans in the loop? "+
					"Running the contributor relay from your hive's /contribute page satisfies this step just as well.",
				humanDuration(age))
		default:
			return nudge{}
		}
		return n
	}

	// ── Stage 3: ACMM graduation — gentle, monthly, never a threat ─────────
	level := h.ACMMLevel
	if level >= maxACMMLevel {
		// Already at the top; there is nothing to graduate to.
		return nudge{ViaRelay: viaRelay}
	}
	if age < stage3SuggestAfter {
		return nudge{ViaRelay: viaRelay}
	}
	next, ok := acmmLevels[level+1]
	if !ok {
		// Unknown next level — say nothing rather than invent one.
		return nudge{ViaRelay: viaRelay}
	}
	return nudge{
		Stage:      StageACMMLevel,
		Severity:   SeverityGentle,
		Resend:     stage3ResendInterval,
		ViaRelay:   viaRelay,
		StalledFor: age,
		Message: fmt.Sprintf(
			"Your hive has been running at %s for a while now. When it feels right, consider graduating to L%d %s: %s. "+
				"There is no deadline and no obligation — move up only when your team is ready. You can review and apply the level from Config → ACMM Level.",
			acmmLevelLabel(level), level+1, next.Name, next.Summary),
	}
}

// orgLabel renders the hive's org (or its name as a fallback) for banner copy,
// so a message names the actual GitHub org the owner must install the App on.
func orgLabel(h *RegistryEntry) string {
	if h == nil {
		return "your organization"
	}
	if org := strings.TrimSpace(h.Org); org != "" {
		return org
	}
	if name := strings.TrimSpace(h.Name); name != "" {
		return name
	}
	return "your organization"
}

// shouldSend decides whether a computed nudge is due, given what was last sent.
//
// It fires when any of the following holds:
//   - nothing was ever sent for this hive;
//   - the stage or the severity changed (an escalation must not wait out the
//     interval — that is the whole point of escalating);
//   - the ACMM level changed since the last ACMM nudge (the owner graduated,
//     so the old suggestion is stale and the new one is immediately relevant);
//   - the stage's re-send interval has elapsed.
//
// An unparseable LastSentAt is treated as "never sent", which re-sends once and
// then repairs the state.
func shouldSend(n nudge, st *JourneyState, now time.Time) bool {
	if n.Stage == StageNone || n.Severity == SeverityNone {
		return false
	}
	if st == nil || st.LastSentAt == "" {
		return true
	}
	if st.LastStage != n.Stage.String() || st.LastSeverity != n.Severity.String() {
		return true
	}
	last, err := time.Parse(time.RFC3339, st.LastSentAt)
	if err != nil {
		return true
	}
	return !now.Before(last.Add(n.Resend))
}

// journeyBannerID builds the banner ID for a nudge. It embeds the stage and
// severity so that each escalation step is a NEW banner from the spoke's point
// of view — an owner who dismissed the gentle reminder still sees the firm one.
// For ACMM it also embeds the level, so a suggestion to move to L4 reappears
// after the owner reaches L3 even though stage and severity are unchanged.
func journeyBannerID(n nudge, level int) string {
	if n.Stage == StageACMMLevel {
		return fmt.Sprintf("%s%s-%s-l%d", journeyBannerIDPrefix, n.Stage, n.Severity, level)
	}
	return fmt.Sprintf("%s%s-%s", journeyBannerIDPrefix, n.Stage, n.Severity)
}

// isJourneyBanner reports whether a banner ID was generated by this subsystem.
// Used so journey nudges never overwrite a banner an admin sent by hand.
func isJourneyBanner(id string) bool {
	return strings.HasPrefix(id, journeyBannerIDPrefix)
}

// JourneyStatusFor computes the display-facing status for one hive. It is pure
// and side-effect free, so both the registry projection and the tests can call
// it freely.
func JourneyStatusFor(h *RegistryEntry, st *JourneyState, now time.Time) JourneyStatus {
	n := nextNudge(h, now)
	status := JourneyStatus{
		Stage:    n.Stage.String(),
		StageNum: int(n.Stage),
		Severity: n.Severity.String(),
		Message:  n.Message,
		ViaRelay: n.ViaRelay,
	}
	if n.Stage == StageMethodModel {
		// A hive being nudged for stage 2 is by definition not on the relay.
		status.ViaRelay = false
	}
	if n.StalledFor > 0 && n.Stage != StageNone {
		status.StalledFor = humanDuration(n.StalledFor)
	}
	if st.snoozeActive(now) {
		status.Snoozed = true
		status.SnoozedUntil = st.SnoozedUntil
	}
	return status
}
