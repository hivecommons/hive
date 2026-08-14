package hub

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Per-hive security env reconcile — makes the fleet's key posture code-owned.
//
// THE GAP THIS CLOSES. The C2/N1/N2/N3 work moved every spoke off the shared
// master onto derived, per-hive sub-keys, and the provisioning template
// (saas_provision.go) renders all five vars. But that template is `kubectl
// apply`ed ONLY inside provisionHive — at provision/assign time. Hives
// provisioned BEFORE those vars entered the template never received them, and
// nothing re-asserts them afterwards.
//
// What actually put the vars on the live fleet was an out-of-band `kubectl
// patch`, not any code path: a live spoke's
// kubectl.kubernetes.io/last-applied-configuration still lists only the
// pre-cutover env shape (HIVE_GITHUB_TOKEN, DASHBOARD_AUTH_TOKEN, HIVE_ID,
// HIVE_LEVEL, HIVE_HUB_URL, HIVE_HUB_SECRET) while the live .spec has the new
// vars appended after HIVE_HUB_SECRET — and in a different order from the
// template, which emits them instead of the master. So the fleet's security
// posture is held in place by a manual edit that no controller maintains: any
// re-provision, restore, or manifest reapply silently reverts a spoke to
// master-derived fallbacks (spokeDomainKey / TerminalSigningKey /
// SpokeSSOPublicKey all fall through to HIVE_HUB_SECRET when their dedicated
// var is absent, which is precisely the fleet-uniform sharing N1/N3 removed).
//
// Four spokes were missed by that manual patch entirely and run on master-only
// fallbacks today, including a real external user's hive. This sweep repairs
// them and, more importantly, means the next drift is repaired automatically
// instead of by hand.
//
// SCOPE / SAFETY. This lane is ADDITIVE ONLY. It never removes HIVE_HUB_SECRET.
// Stripping the master from spoke Deployments is a separate, fleet-visible
// cutover with its own hard preconditions (every spoke must be on an image that
// reads the dedicated vars, and the readiness counts below must be zero across
// a full sweep); doing it here would couple a silent repair to a breaking
// change.
//
// This mirrors the NET_ADMIN reconcile (netadmin_reconcile.go) deliberately:
// same cluster access path, same unreachable-cluster suppression, same
// idempotent-check-then-patch shape. Divergence between two sweeps that both
// mutate the hive Deployment would be its own maintenance hazard.

const (
	// envSessionPublicKey is the spoke-side env var carrying the Ed25519 PUBLIC
	// key for hub session cookies (audit N2). Unlike the others there is no Go
	// constant for it — only the Node proxy reads it (v2/proxy/server.js reads
	// process.env.HIVE_SESSION_PUBLIC_KEY) — so it is named here rather than
	// spelled as a literal at each use, so the check and the patch can never
	// disagree.
	envSessionPublicKey = "HIVE_SESSION_PUBLIC_KEY"

	// envHubSecretMaster is the MASTER secret still present on every spoke. This
	// lane never writes or removes it; the name exists only so the readiness
	// surface can COUNT how many spokes still carry it, which is the gating
	// signal for the later removal step.
	envHubSecretMaster = "HIVE_HUB_SECRET"

	// perHiveEnvReconcileInterval throttles the sweep. Like the NET_ADMIN drift
	// this is static remediation, not a hot path: a converged hive stays
	// converged until it is re-provisioned or restored, and a hive that IS
	// drifted is not made worse by waiting one interval. The SHA poller ticks
	// every 2 min; we run at most once per this window.
	perHiveEnvReconcileInterval = 15 * time.Minute

	// perHiveEnvKubectlTimeout bounds each per-hive kubectl get/patch so one
	// unreachable cluster cannot stall the whole sweep. Matches
	// netAdminKubectlTimeout.
	perHiveEnvKubectlTimeout = 15 * time.Second

	// perHiveEnvMaxPatchesPerCycle caps how many spokes this lane may PATCH in a
	// single sweep.
	//
	// RATE LIMIT RATIONALE. Adding an env var mutates the podspec, so every
	// patch triggers a rolling restart of that hive's pod — which kills the
	// tmux session and interrupts whatever agents are mid-run on that spoke.
	// The fleet is ~70 hosted spokes. Patching them all in one cycle would roll
	// every pod at once: every running agent on the platform interrupted
	// simultaneously, ~70 pods pulling images and re-scheduling together, and
	// no way to notice a bad patch before it has reached the whole fleet.
	//
	// 3 per cycle at a 15-minute interval converges a fully-drifted 70-hive
	// fleet in about 6 hours — fast enough that a security gap does not linger
	// for days, slow enough that at most 3 tenants are disturbed at a time and
	// an operator watching the logs has ~15 minutes to stop the hub between
	// batches if a patch misbehaves. Today only 4 spokes are drifted, so the
	// real convergence is two cycles (~15 minutes).
	//
	// The cap counts PATCHES, not hives examined: the sweep still READS every
	// hive each cycle (a read is cheap and does not roll a pod), so the
	// readiness counts below reflect the whole fleet even while remediation is
	// rate-limited.
	perHiveEnvMaxPatchesPerCycle = 3

	// hiveContainerEnvPath is the JSON-Patch path to the hive container's env
	// list. containers[0] is the hive container in the provisioning template.
	hiveContainerEnvPath = "/spec/template/spec/containers/0/env"
)

// deploymentEnvVar is the minimal shape we read back from a Deployment's
// container env list. Only name/value matter here: a var sourced from a
// secretKeyRef/fieldRef has no literal .value, and we deliberately treat that
// as "not the value we expect" rather than trying to chase the reference —
// none of the five vars is ever provisioned as a reference.
type deploymentEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// desiredPerHiveEnv returns the five security env vars a spoke for hiveID must
// carry, derived EXACTLY as the v4 provisioning template derives them
// (saas_provision.go, the HeartbeatKey/SessionKey/SSOPublicKey/
// SessionPublicKey/TerminalKey block). Provision-time and reconcile-time
// derivation MUST agree — if they drifted, this sweep would fight provisioning
// and roll a pod every cycle forever — so both sides call the same helpers in
// hub_keys.go rather than re-implementing the formulas.
//
// GENERATIONS. The master used here is the CURRENT generation's secret
// (provisionCurrentSecret), not the raw file/env master. That single change is
// the whole of "rotate spoke-held material without re-provisioning": after a
// rotation every spoke still carries generation-N material, perHiveEnvDrift
// sees a var present with a DIFFERENT value — the case its own doc comment
// names as "a rotated master" — and the existing rate limiter walks the fleet
// onto the new generation at 3 patches per 15-minute cycle. Nothing about the
// sweep's cadence or aggressiveness changes; the drift detector was already
// correct for this case.
//
// Until a rotation can actually happen (follow-on PR #4 adds the persistence
// and the admin endpoint) there is exactly one generation, so
// provisionCurrentSecret() == provisionMasterSecret() and every value below is
// byte-identical to what this function returned before. This is a read-path
// change in effect.
//
// FAIL-SAFE: returns nil when any derived value is empty. derivePerHiveKey
// returns "" for an empty master OR an empty hiveID, and deriveDomainKey
// returns "" for an empty master, by design (a keyless caller must fail closed).
// If we wrote those through, a spoke would get e.g. HIVE_HEARTBEAT_KEY="" —
// strictly WORSE than absent, because the spoke-side resolvers
// (spokeDomainKey, TerminalSigningKey, SpokeSSOPublicKey) test for a non-empty
// value before falling back to the master: an empty var falls back exactly like
// an absent one, but it also makes the readiness count below report the hive as
// converged when it is not. So a hive whose ID does not resolve, or a hub with
// no master secret configured, is SKIPPED and logged — never patched.
func desiredPerHiveEnv(hiveID string) map[string]string {
	hiveID = strings.TrimSpace(hiveID)
	if hiveID == "" {
		return nil
	}
	// The CURRENT generation's secret. Empty when no generation is configured —
	// including the nil-set case, since currentSecret() on a nil *generationSet
	// returns "". The guard below is unchanged: an empty master must still
	// short-circuit before anything is derived.
	master := provisionCurrentSecret()
	if master == "" {
		return nil
	}
	want := map[string]string{
		EnvHeartbeatKey:     provisionHeartbeatKey(hiveID),
		EnvTerminalKey:      provisionTerminalKey(hiveID),
		EnvSessionKey:       deriveDomainKey(master, infoSessionKey),
		EnvSSOPublicKey:     ssoPublicKeyFromSeed(deriveDomainKey(master, infoSSOEd25519Seed)),
		envSessionPublicKey: provisionSessionPublicKey(),
	}
	for _, v := range want {
		if v == "" {
			// Belt and braces: master and hiveID are both non-empty above, so
			// this should be unreachable — but an empty value is the one
			// outcome that must never reach a Deployment, and a future
			// derivation change (e.g. an Ed25519 expansion failing on a
			// malformed seed, which ssoPublicKeyFromSeed reports as "") must
			// fail closed here rather than silently blank a spoke's key.
			return nil
		}
	}
	return want
}

// perHiveEnvNames lists the five reconciled vars in a stable order, for
// deterministic patches and log lines.
func perHiveEnvNames() []string {
	return []string{
		EnvHeartbeatKey,
		EnvTerminalKey,
		EnvSessionKey,
		EnvSSOPublicKey,
		envSessionPublicKey,
	}
}

// perHiveEnvDrift compares a Deployment's live container env list against the
// desired values and returns the names of the vars that are absent or wrong, in
// perHiveEnvNames order. An empty result means CONVERGED — the caller must then
// issue no patch at all, because any patch rolls the pod.
//
// This is the pure decision function the sweep gates on: no kubectl, fully
// unit-testable. `live` is the Deployment's containers[0].env as returned by
// `kubectl get deploy hive -o jsonpath={...env}`.
//
// A var present with a DIFFERENT value counts as drift and is corrected — that
// is the case a stale hive ID or a rotated master produces, and leaving it
// would mean the hub rejects that spoke's heartbeats forever.
func perHiveEnvDrift(live []deploymentEnvVar, want map[string]string) []string {
	if len(want) == 0 {
		return nil
	}
	have := make(map[string]string, len(live))
	for _, e := range live {
		have[e.Name] = e.Value
	}
	var drift []string
	for _, name := range perHiveEnvNames() {
		if got, ok := have[name]; !ok || got != want[name] {
			drift = append(drift, name)
		}
	}
	return drift
}

// perHiveEnvPatchJSON builds the JSON-Patch body that converges the env list.
//
// It replaces the WHOLE env array rather than emitting per-var add/replace ops.
// A JSON-Patch `add` to `/…/env/-` appends, and `replace` needs the numeric
// index of the existing entry — so a per-var patch would have to encode live
// indices, which race any concurrent write to the Deployment and silently
// clobber the wrong var if the list shifted between our read and our patch.
// Replacing the array from the list we just read keeps the operation atomic in
// intent and preserves every var we did not touch, in its original order, with
// the reconciled ones appended or updated in place.
//
// Order is preserved for untouched vars and the appended ones follow
// perHiveEnvNames order, so a converged Deployment produces a byte-identical
// array on the next sweep and perHiveEnvDrift stays empty — no patch loop.
func perHiveEnvPatchJSON(live []deploymentEnvVar, want map[string]string) (string, error) {
	merged := make([]deploymentEnvVar, 0, len(live)+len(want))
	seen := make(map[string]bool, len(want))
	for _, e := range live {
		if v, ok := want[e.Name]; ok {
			e.Value = v
			seen[e.Name] = true
		}
		merged = append(merged, e)
	}
	for _, name := range perHiveEnvNames() {
		if !seen[name] {
			merged = append(merged, deploymentEnvVar{Name: name, Value: want[name]})
		}
	}
	patch := []map[string]any{{
		"op":    "replace",
		"path":  hiveContainerEnvPath,
		"value": merged,
	}}
	b, err := json.Marshal(patch)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// perHiveEnvPatchAllowed reports whether the sweep may issue another patch this
// cycle, given how many it has already issued. Extracted as a named predicate
// (rather than an inline `>=`) so the rate limit is a single testable decision
// that the sweep and its test share — a test that re-implemented the comparison
// would keep passing if the sweep's own check were removed.
func perHiveEnvPatchAllowed(patchedThisCycle int) bool {
	return patchedThisCycle < perHiveEnvMaxPatchesPerCycle
}

// perHiveEnvSweepEligible reports whether a hive with this lifecycle status
// should be examined by the sweep. Extracted as a named predicate — the same
// shape as perHiveEnvPatchAllowed — so the selection rule is a single testable
// decision the sweep and its test share, rather than an inline comparison a
// test could only re-implement (and therefore keep passing after the sweep's
// own check broke).
//
// WHY THIS EXISTS. The original guard was `h.Status != "running"`, copied from
// reconcileNetAdmin. It selected NOTHING: across the live 66-hive registry the
// status distribution is 30 "available", 28 "", 8 "assigned" — zero "running".
// The two code paths that do write "running" (the provision watcher in
// saas_provision.go, and the stale-"error" clear on heartbeat in server.go)
// only fire from the transient "provisioning"/"error" states, so a hive in
// steady state never reaches or holds it. The sweep therefore `continue`d on
// every hive of every cycle and had never patched a spoke; the fleet's key
// posture was still held in place by the out-of-band `kubectl patch` this lane
// exists to replace, and the four known-drifted spokes never converged.
//
// WHAT IT SHOULD HAVE EXPRESSED. Not "is running" but "is deployed" — does
// this hive plausibly have a hive Deployment to reconcile? The sibling sweeps
// over the same listSaaSHives() (triggerAutoUpgrades, sweepOrphanedUpgrades,
// sweepStuckAssignments) apply NO status filter at all and gate instead on the
// resources they actually need. This predicate follows that convention, only
// naming the one status that is genuinely premature.
//
//   - "" (28 live) — the common steady state; a provisioned hive that has
//     simply never had a status written. Must be swept: two of the four
//     drifted spokes sit here.
//   - "available" (30 live) — an unclaimed pre-provisioned placeholder. It is
//     genuinely deployed, its Deployment is live, and it must carry correct
//     per-hive keys BEFORE it is claimed — a placeholder that is assigned to a
//     user while running on master-derived fallbacks hands that user a spoke
//     with fleet-shared keys, exactly what N1/N3 removed.
//   - "assigned" (8 live) — claimed and deployed. Obviously swept.
//   - "running" — kept so the pre-existing intent still selects, not because
//     anything reaches it.
//   - "error" — SWEPT. The status is frequently stale (only a heartbeat clears
//     it), the Deployment usually still exists, and an unconverged key is a
//     plausible CAUSE of the error rather than a reason to leave it drifted.
//
// Only "provisioning" is excluded: the namespace and Deployment are still
// being applied, so a read would fail anyway and the provisioning template
// renders all five vars correctly at creation. This is a cheap-cycle
// optimisation, not a safety gate — the sweep already handles a missing
// Deployment safely (a failed `kubectl get` logs Debug and continues WITHOUT
// recording an observation, so an unread hive can never count as converged).
func perHiveEnvSweepEligible(status string) bool {
	return strings.TrimSpace(status) != "provisioning"
}

// perHiveEnvGeneration classifies which live generation a spoke's Deployment env
// was derived from, by re-deriving the per-hive heartbeat key under each
// acceptable generation and comparing.
//
// WHY THE HEARTBEAT KEY IS THE PROBE. Of the five reconciled vars it is the one
// that is BOTH per-hive and a direct HMAC of the master, so a match identifies
// the generation AND the hive — two spokes on the same generation produce
// different values, so a stale-hive-ID drift can never be misread as a
// generation match. The session/SSO public keys are fleet-uniform and would
// answer a weaker question.
//
// Returns (id, true) for the generation that produced the live value, and
// (0, false) when NO live generation explains it. That second case is
// deliberately not folded into "previous": a value derived from a generation
// the hub has already retired, or from a stale hive ID, is UNATTRIBUTED and
// must not be counted as being on any generation — otherwise a spoke stuck on
// a retired key would silently inflate a "converged" count. It fails closed.
//
// gens is the acceptable set in current-first order, so the common case (a
// converged spoke) matches on the first attempt.
func perHiveEnvGeneration(gens []keyGeneration, live []deploymentEnvVar, hiveID string) (int, bool) {
	hiveID = strings.TrimSpace(hiveID)
	if hiveID == "" || len(gens) == 0 {
		return 0, false
	}
	var presented string
	for _, e := range live {
		if e.Name == EnvHeartbeatKey {
			presented = e.Value
			break
		}
	}
	if presented == "" {
		// Absent or blank. An absent heartbeat key is drift, already reported
		// by perHiveEnvDrift; it is not evidence of any generation.
		return 0, false
	}
	for _, g := range gens {
		expect := derivePerHiveKey(g.Secret, infoHeartbeatKey, hiveID)
		if expect != "" && secureCompareHub(presented, expect) {
			return g.ID, true
		}
	}
	return 0, false
}

// perHiveEnvObservation is one hive's live posture, recorded by the sweep from
// its OWN Deployment read. See perHiveEnvSnapshot for why this is not sourced
// from heartbeat recency.
type perHiveEnvObservation struct {
	// MissingVars are the reconciled vars absent or wrong on this hive.
	MissingVars []string
	// HasMasterSecret is true when the Deployment still injects
	// HIVE_HUB_SECRET, the master this whole line of work exists to remove.
	HasMasterSecret bool
	// Generation is the live generation ID whose material this hive carries, or
	// 0 when the hub could not attribute the hive's key to any acceptable
	// generation. Zero is the fail-closed value: it is counted as neither
	// current nor previous.
	Generation int
	// Observed is when the Deployment was last successfully read.
	Observed time.Time
}

// recordPerHiveEnvObservation stores one hive's Deployment-sourced posture.
func (s *HubServer) recordPerHiveEnvObservation(hiveID string, obs perHiveEnvObservation) {
	if s == nil || hiveID == "" {
		return
	}
	s.perHiveEnvMu.Lock()
	defer s.perHiveEnvMu.Unlock()
	if s.perHiveEnvSeen == nil {
		s.perHiveEnvSeen = make(map[string]perHiveEnvObservation)
	}
	s.perHiveEnvSeen[hiveID] = obs
}

// forgetPerHiveEnvObservation drops a hive that no longer exists, so a
// deprovisioned hive cannot hold the counts non-zero forever.
func (s *HubServer) forgetPerHiveEnvObservation(hiveID string) {
	if s == nil {
		return
	}
	s.perHiveEnvMu.Lock()
	defer s.perHiveEnvMu.Unlock()
	delete(s.perHiveEnvSeen, hiveID)
}

// PerHiveEnvStatus is the Deployment-sourced convergence view exposed on
// GET /api/saas/admin/auth-rollout.
type PerHiveEnvStatus struct {
	// ObservedHives is how many hives the hub has successfully READ a
	// Deployment for. This is the denominator, and it is deliberately a count
	// of successful reads rather than of hives that exist: a hive on an
	// unreachable cluster is not evidence of anything either way.
	ObservedHives int `json:"observed_hives"`
	// MissingPerHiveEnv is how many observed hives are missing (or carry a
	// wrong value for) at least one of the five reconciled security vars.
	// Convergence means this reaches zero and STAYS zero across a full sweep.
	MissingPerHiveEnv int `json:"missing_per_hive_env"`
	// MissingHives names those laggards so an operator can go look at them
	// rather than guessing.
	MissingHives []string `json:"missing_hives,omitempty"`
	// StillCarryingMaster is how many observed hives still inject
	// HIVE_HUB_SECRET. This lane never removes it — the count exists to gate
	// the LATER removal step, which must not begin while any spoke would lose
	// its only working key.
	StillCarryingMaster int `json:"still_carrying_master"`
	// PerHiveEnvConverged is true only when at least one hive was observed and
	// none is missing a var. Fails CLOSED on zero observations: "no evidence"
	// must never read as "safe to proceed".
	PerHiveEnvConverged bool `json:"per_hive_env_converged"`
	// ConsideredHives is how many hives the LAST sweep's status filter
	// admitted, and SkippedByStatus how many it rejected. These make the
	// sweep's own selection auditable: ConsideredHives == 0 on a non-empty
	// fleet means the sweep is patching nobody regardless of how healthy the
	// counts above look. That is precisely the state this lane shipped in —
	// the filter matched no hive, so ObservedHives stayed 0 and the surface
	// was indistinguishable from "nothing to do".
	ConsideredHives int `json:"considered_hives"`
	SkippedByStatus int `json:"skipped_by_status"`
	// KeyGenerations is the per-generation breakdown of the observed fleet. It
	// is the surface an operator watches during a rotation to decide when the
	// previous generation can be retired.
	KeyGenerations KeyGenerationStatus `json:"key_generations"`
}

// KeyGenerationStatus is the rotation-readiness view: how many observed spokes
// carry material from which master generation.
//
// SOURCED FROM DEPLOYMENT READS, NOT HEARTBEATS — for the reason spelled out at
// length on PerHiveEnvSnapshot. A paused spoke still has a Deployment, so it is
// still counted and still blocks retirement; it cannot fall out of the
// denominator by going quiet. Gating retirement on a heartbeat-sourced count
// would let a paused spoke sit on a key the hub has stopped accepting.
type KeyGenerationStatus struct {
	// Current is the ID of the generation that MINTS. 0 when the hub has no
	// generation set configured at all.
	Current int `json:"current"`
	// LiveGenerations lists the IDs the hub currently accepts for VERIFY,
	// current first. Never includes secret material — an ID names a key, it is
	// not a key.
	LiveGenerations []int `json:"live_generations,omitempty"`
	// SpokesOnCurrent is how many observed hives carry material derived from
	// the current generation.
	SpokesOnCurrent int `json:"spokes_on_current"`
	// SpokesOnPrevious is how many observed hives carry material from a live
	// but non-current generation — the ones a rotation is still walking.
	SpokesOnPrevious int `json:"spokes_on_previous"`
	// SpokesUnattributed is how many observed hives carry a heartbeat key that
	// matches NO live generation. These are not on "previous"; they are on
	// something the hub no longer accepts, or their hive ID has drifted. They
	// are broken now, not merely lagging, and they are counted separately so
	// they can never be mistaken for either converged or converging.
	SpokesUnattributed int `json:"spokes_unattributed"`
	// PreviousVerifyUntil is when the most recent previous generation stops
	// being accepted, RFC3339. Empty when there is no previous generation.
	PreviousVerifyUntil string `json:"previous_verify_until,omitempty"`
	// SafeToRetirePrevious is true only when a previous generation exists, at
	// least one hive was observed, NO observed hive is on a previous generation
	// or unattributed, and the previous generation's VerifyUntil has passed.
	//
	// Fails CLOSED on zero observations, exactly like PerHiveEnvConverged: "the
	// hub has read nothing" must never render as "safe to retire".
	SafeToRetirePrevious bool `json:"safe_to_retire_previous"`
}

// PerHiveEnvSnapshot reports fleet convergence for the five per-hive security
// env vars.
//
// WHY THIS IS NOT SOURCED FROM HEARTBEAT RECENCY. The sibling signal in this
// file's neighbour, AuthRolloutReadiness, is built from noteHeartbeatAuthPath
// observations and drops any hive not seen within authRolloutStaleAfter (24h).
// Its own doc comment records the consequence: it cannot distinguish "hive
// absent" from "hive never existed", so a hive that is merely PAUSED,
// mid-upgrade, or on a temporarily unreachable cluster silently leaves the
// denominator — and the fleet then reads ready while that spoke is still
// unconverged. For a signal that gates deleting a compat lane, that failure
// mode is the whole risk.
//
// These counts therefore come from the hub's OWN Deployment reads in
// reconcilePerHiveEnv, not from anything the spoke sends. A paused spoke still
// has a Deployment, so it is still read, still counted, and still blocks
// convergence until it is actually fixed. A spoke cannot fall out of the
// denominator by going quiet; it falls out only when the hub can no longer read
// it (in which case it was never counted as converged either) or when the hive
// is deprovisioned and explicitly forgotten.
func (s *HubServer) PerHiveEnvSnapshot() PerHiveEnvStatus {
	out := PerHiveEnvStatus{}
	if s == nil {
		return out
	}
	s.perHiveEnvMu.RLock()
	defer s.perHiveEnvMu.RUnlock()
	out.ConsideredHives = s.perHiveEnvConsidered
	out.SkippedByStatus = s.perHiveEnvSkippedByStatus
	gs := s.keyGenerations
	now := time.Now()
	current, hasCurrent := gs.currentGeneration()
	if hasCurrent {
		out.KeyGenerations.Current = current.ID
	}
	// LiveGenerations reports what the hub currently ACCEPTS, so it comes from
	// acceptableGenerations — an expired previous generation is already gone
	// from it, which is the finiteness promise made visible.
	acceptable := gs.acceptableGenerations(now)
	for _, g := range acceptable {
		out.KeyGenerations.LiveGenerations = append(out.KeyGenerations.LiveGenerations, g.ID)
	}

	// previousVerifyUntil, by contrast, comes from the RAW set. It must not be
	// sourced from `acceptable`: retirement is precisely the state where the
	// window has already closed, so an acceptable-sourced read would drop the
	// previous generation from view at the exact moment it becomes retirable
	// and SafeToRetirePrevious could never be true. The entry still sits in the
	// set until something removes it; this surface is what says it may be.
	var previousVerifyUntil time.Time
	hasPrevious := false
	if gs != nil {
		for _, g := range gs.Generations {
			if g.ID == gs.Current {
				continue
			}
			hasPrevious = true
			if previousVerifyUntil.IsZero() || g.VerifyUntil.After(previousVerifyUntil) {
				previousVerifyUntil = g.VerifyUntil
			}
		}
	}

	for id, obs := range s.perHiveEnvSeen {
		out.ObservedHives++
		if len(obs.MissingVars) > 0 {
			out.MissingPerHiveEnv++
			out.MissingHives = append(out.MissingHives, id)
		}
		if obs.HasMasterSecret {
			out.StillCarryingMaster++
		}
		switch {
		case hasCurrent && obs.Generation == out.KeyGenerations.Current:
			out.KeyGenerations.SpokesOnCurrent++
		case obs.Generation > 0:
			out.KeyGenerations.SpokesOnPrevious++
		default:
			out.KeyGenerations.SpokesUnattributed++
		}
	}
	sort.Strings(out.MissingHives)
	out.PerHiveEnvConverged = out.ObservedHives > 0 && out.MissingPerHiveEnv == 0

	if !previousVerifyUntil.IsZero() {
		out.KeyGenerations.PreviousVerifyUntil = previousVerifyUntil.UTC().Format(time.RFC3339)
	}
	// windowClosed follows acceptableGenerations' rule exactly: a ZERO
	// VerifyUntil means ALREADY EXPIRED, not "never expires". Reading it the
	// other way here would be worse than inconsistent — a hand-edited
	// generations file with the field stripped would report the window as
	// permanently open and pin the previous generation live forever, which is
	// the F1/F2 failure mode this design exists to make impossible.
	windowClosed := previousVerifyUntil.IsZero() || !now.Before(previousVerifyUntil)

	// Every clause is required. Dropping "hasPrevious" would report a fresh,
	// never-rotated hub as safe to retire a generation it does not have;
	// dropping the observation floor would report safety from zero evidence;
	// and counting SpokesUnattributed here (rather than ignoring it) is what
	// stops a spoke on an ALREADY-retired key from reading as retirable.
	out.KeyGenerations.SafeToRetirePrevious = hasPrevious &&
		out.ObservedHives > 0 &&
		out.KeyGenerations.SpokesOnPrevious == 0 &&
		out.KeyGenerations.SpokesUnattributed == 0 &&
		windowClosed
	return out
}

// reconcilePerHiveEnvIfDue runs the sweep only if perHiveEnvReconcileInterval
// has elapsed, so the frequent SHA poller can call it every tick. Mirrors
// reconcileNetAdminIfDue.
func (s *HubServer) reconcilePerHiveEnvIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastPerHiveEnvReconcile.IsZero() ||
		time.Since(s.lastPerHiveEnvReconcile) >= perHiveEnvReconcileInterval
	if due {
		s.lastPerHiveEnvReconcile = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.reconcilePerHiveEnv()
}

// reconcilePerHiveEnv sweeps every hub-managed hosted hive, records its live
// env posture, and patches in the five security vars where they are absent or
// wrong — at most perHiveEnvMaxPatchesPerCycle per cycle.
//
// Idempotent: a converged hive produces no patch and therefore no rollout.
// Non-fatal: an unreachable cluster, an unreadable Deployment, or a hive
// without a resolvable ID is skipped and logged, never patched.
func (s *HubServer) reconcilePerHiveEnv() {
	hives := listSaaSHives()
	live := make(map[string]bool, len(hives))
	patched := 0
	deferredByRateLimit := 0
	// Selection accounting, published to the readiness surface. A sweep that
	// selects NOBODY is the exact failure this lane shipped with and nothing
	// made it visible: every counter downstream of the filter stayed at zero,
	// which is indistinguishable from a converged fleet. Recording the
	// considered/skipped split makes "the filter matches nothing" readable
	// instead of silent.
	consideredHives := 0
	skippedByStatus := 0

	for _, h := range hives {
		if !perHiveEnvSweepEligible(h.Status) {
			skippedByStatus++
			continue
		}
		consideredHives++
		cluster := s.clusterForHive(&h)
		if cluster == nil {
			continue
		}
		if s.clusterRecentlyUnreachable(cluster.ID) {
			continue
		}
		live[h.ID] = true

		// FAIL-SAFE, checked BEFORE any cluster call: a hive with no resolvable
		// ID, or a hub with no master secret, derives to empty values. Skip and
		// log rather than write "" into a Deployment — see desiredPerHiveEnv.
		want := desiredPerHiveEnv(h.ID)
		if want == nil {
			s.logger.Warn("per-hive env reconcile: skipping hive with no derivable keys — NOT patching (would write empty values)",
				"hive_id", h.ID, "cluster", cluster.ID,
				"has_master", provisionMasterSecret() != "")
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), perHiveEnvKubectlTimeout)
		getCmd := kubectlForClusterContext(ctx, cluster, "get", "deployment", "hive",
			"-n", hiveHostedNamespacePrefix+h.ID, "-o",
			"jsonpath={.spec.template.spec.containers[0].env}")
		out, err := getCmd.Output()
		cancel()
		if err != nil {
			// Deployment missing, cluster unreachable, or transient kubectl
			// error — all non-fatal, retried next sweep. Do NOT record an
			// observation: an unread hive must not count as converged.
			s.logger.Debug("per-hive env reconcile: could not read hive deployment env",
				"hive_id", h.ID, "cluster", cluster.ID, "error", err)
			continue
		}
		s.markClusterReachable(cluster.ID)

		var liveEnv []deploymentEnvVar
		if err := json.Unmarshal(out, &liveEnv); err != nil {
			s.logger.Debug("per-hive env reconcile: could not parse hive deployment env",
				"hive_id", h.ID, "cluster", cluster.ID, "error", err)
			continue
		}

		drift := perHiveEnvDrift(liveEnv, want)
		hasMaster := false
		for _, e := range liveEnv {
			if e.Name == envHubSecretMaster {
				hasMaster = true
				break
			}
		}
		// Attribute this hive to a generation from the SAME read that produced
		// the drift list, so the two can never describe different moments.
		gen, _ := perHiveEnvGeneration(s.keyGenerations.acceptableGenerations(time.Now()), liveEnv, h.ID)
		// Record the observation from THIS read regardless of whether we go on
		// to patch — the readiness surface must reflect the whole fleet even
		// while remediation is rate-limited.
		s.recordPerHiveEnvObservation(h.ID, perHiveEnvObservation{
			MissingVars:     drift,
			HasMasterSecret: hasMaster,
			Generation:      gen,
			Observed:        time.Now(),
		})

		if len(drift) == 0 {
			// CONVERGED — issue no patch. This is the branch that prevents a
			// fleet-wide rolling-restart storm on every sweep.
			s.logger.Debug("per-hive env reconcile: hive already converged",
				"hive_id", h.ID, "cluster", cluster.ID)
			continue
		}

		if !perHiveEnvPatchAllowed(patched) {
			deferredByRateLimit++
			continue
		}

		patchBody, perr := perHiveEnvPatchJSON(liveEnv, want)
		if perr != nil {
			s.logger.Warn("per-hive env reconcile: could not build patch",
				"hive_id", h.ID, "cluster", cluster.ID, "error", perr)
			continue
		}

		pctx, pcancel := context.WithTimeout(context.Background(), perHiveEnvKubectlTimeout)
		patchCmd := kubectlForClusterContext(pctx, cluster, "patch", "deployment", "hive",
			"-n", hiveHostedNamespacePrefix+h.ID, "--type=json", "-p", patchBody)
		pout, err := patchCmd.CombinedOutput()
		pcancel()
		if err != nil {
			s.markClusterUnreachable(cluster.ID)
			s.logger.Warn("per-hive env reconcile: patch failed — will retry next sweep",
				"hive_id", h.ID, "cluster", cluster.ID,
				"missing", strings.Join(drift, ","),
				"output", strings.TrimSpace(string(pout)), "error", err)
			continue
		}
		s.markClusterReachable(cluster.ID)
		patched++
		// Log the var NAMES only. Never the values — these are signing keys.
		s.logger.Info("reconciled per-hive security env onto hive deployment (rolls the pod once)",
			"hive_id", h.ID, "cluster", cluster.ID,
			"reconciled", strings.Join(drift, ","),
			"patched_this_cycle", patched)
	}

	// Drop hives that no longer exist so a deprovisioned spoke cannot pin the
	// counts non-zero forever — the failure mode a too-long stale window has.
	s.perHiveEnvMu.Lock()
	for id := range s.perHiveEnvSeen {
		if !live[id] {
			delete(s.perHiveEnvSeen, id)
		}
	}
	s.perHiveEnvConsidered = consideredHives
	s.perHiveEnvSkippedByStatus = skippedByStatus
	s.perHiveEnvMu.Unlock()

	// A sweep that admitted no hives at all is a BUG signal, not a quiet
	// no-op: the registry is never legitimately empty on a hub that hosts
	// spokes. Log it at Warn so the condition that made this lane dead code
	// for its whole production life cannot recur silently.
	if consideredHives == 0 && len(hives) > 0 {
		s.logger.Warn("per-hive env reconcile: status filter selected NO hives — sweep is a no-op, key drift will not be repaired",
			"hives_in_registry", len(hives), "skipped_by_status", skippedByStatus)
	}

	if deferredByRateLimit > 0 {
		s.logger.Info("per-hive env reconcile: rate limit reached, remaining hives deferred to next cycle",
			"patched", patched, "deferred", deferredByRateLimit,
			"max_per_cycle", perHiveEnvMaxPatchesPerCycle,
			"next_cycle_in", perHiveEnvReconcileInterval.String())
	}
}
