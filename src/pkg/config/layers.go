// Package config — spoke configuration layering.
//
// ─────────────────────────────────────────────────────────────────────────
// PRECEDENCE ORDER — HIGHEST TO LOWEST
// ─────────────────────────────────────────────────────────────────────────
//
//  1. config.env              /etc/hive/config.env            env overrides
//  2. Per-agent overlays      /data/agent-configs/*.yaml       agents only
//  3. Dashboard overlay       /data/hive.yaml.dashboard        PVC
//  4. ConfigMap seed          /etc/hive-seed → /etc/hive/hive.yaml
//
// with exactly THREE places a lower layer affects the result:
//
//	A. hub.is_public   — the seed's value is applied over the overlay's. This
//	                     is the ONLY unconditional seed-wins assignment in the
//	                     whole merge.
//	B. acmm_level      — the seed is a FALLBACK, used only when the overlay
//	                     has no level at all (issue #1856).
//	C. github.*        — the seed wins only as an anti-wipe RATCHET, when the
//	                     seed holds real App credentials and the overlay's look
//	                     like a placeholder. A safety catch, not precedence.
//
// Everything else: the highest layer that sets the field wins.
//
// ─────────────────────────────────────────────────────────────────────────
// WHY THIS FILE EXISTS
// ─────────────────────────────────────────────────────────────────────────
//
// Until now this order lived in one place only: a Python heredoc inside
// src/deploy/entrypoint.sh, run once at pod boot, whose sole externally visible
// trace was a log line:
//
//	[entrypoint] K8s mode — dashboard overlay merged over ConfigMap seed
//	(overlay wins for acmm_level and github app creds; ConfigMap wins for
//	hub.is_public)
//
// An operator could not answer "which layer sets this field?" without reading
// pod logs. That cost real hours: a GitHub Enterprise hive was repaired only on
// the fourth attempt, after patching the hub's meta.json, then clusters.json,
// then the spoke's ConfigMap — three layers INERT for that field — before
// patching the dashboard overlay, the one that actually wins.
//
// MergeLayers is a behaviour-preserving transcription of that heredoc into Go,
// so the rule is stated once, in code, and is testable. It deliberately does
// NOT change which value wins for any field: ~50 live hives depend on current
// behaviour, and a precedence change is a migration, not a refactor.
//
// ─────────────────────────────────────────────────────────────────────────
// THE SEED DECIDES ALMOST NOTHING, AND NOTHING CAN WRITE IT
// ─────────────────────────────────────────────────────────────────────────
//
// Measured read-only on the live fleet — ConfigMap seed vs running config:
//
//	hosted-aslom-hive-agent          559 B of 12,380 B   (4.5%)
//	hosted-projectbluefin-knuckle  1,661 B of 14,609 B   (11%)
//	hosted-kubestellar-console     1,051 B of 17,564 B   (6%)
//
// The seed omits whole top-level blocks (policies, data, knowledge,
// notifications, hive_id) that the running config carries.
//
// Its one exclusive win, hub.is_public, is owned by a component that CANNOT
// WRITE THIS LAYER:
//
//   - hub → ConfigMap: a single reference in pkg/hub, saas_provision.go:1282,
//     and it is a "get". There is no write path.
//   - spoke → ConfigMap: the spoke Role (saas_provision.go:1478-1518) grants
//     deployments and secrets. "configmaps" appears in no rule, so the spoke
//     cannot write it even if it tried.
//
// And the hub re-asserts is_public over the heartbeat anyway (server.go:1271
// pushes whenever it disagrees with the spoke; server.go:1077 keeps a hub-set
// public flag from being reverted), so the seed's win survives at most one
// beat.
//
// This is why every config fix this week travelled by heartbeat into the PVC
// overlay rather than through the ConfigMap — #2354 (ACMM), #2360 (forge),
// #2363 (GHE App IDs). The overlay is the only writable channel. The
// proliferation of bespoke delivery latches is a rational response to having
// one road, not duplicated effort.
package config

import (
	"log"
	"os"
)

// Layer identifies a configuration source. Ordered lowest → highest, so a
// numerically greater Layer wins under the default rule.
type Layer int

const (
	// LayerUnset means no layer supplied a value for the field.
	LayerUnset Layer = iota
	// LayerSeed is the ConfigMap seed, copied to /etc/hive/hive.yaml by the
	// copy-config init container on every pod boot.
	LayerSeed
	// LayerDashboardOverlay is /data/hive.yaml.dashboard on the PVC, written by
	// Config.Save() on every dashboard edit. This is the layer that decides
	// nearly everything, and the only one the running system can write.
	LayerDashboardOverlay
	// LayerAgentOverlay is /data/agent-configs/<name>.yaml. Contributes agents
	// only, never top-level fields.
	LayerAgentOverlay
	// LayerConfigEnv is config.env, applied last by LoadWithOverrides.
	LayerConfigEnv
)

// String names the layer. These strings are part of the
// /api/config/provenance contract — keep them stable.
func (l Layer) String() string {
	switch l {
	case LayerSeed:
		return "configmap-seed"
	case LayerDashboardOverlay:
		return "dashboard-overlay"
	case LayerAgentOverlay:
		return "agent-overlay"
	case LayerConfigEnv:
		return "config-env"
	default:
		return "unset"
	}
}

// Path returns where an operator must write to change a field won by this
// layer. This is the answer to the question that took four attempts during the
// GitHub Enterprise incident.
//
// LayerSeed is environment-aware, gated on the same IsKubernetesPod() the
// layering itself already uses (see config.go). In Kubernetes the seed really
// is the ConfigMap. Outside Kubernetes there is no ConfigMap at all — the seed
// is RuntimeConfigFile, a plain file on the PVC that Save() rewrites on every
// dashboard save (see saveLocked, config.go:5141) — so naming the ConfigMap
// there points an operator at a layer that does not exist (#4971).
func (l Layer) Path() string {
	switch l {
	case LayerSeed:
		if !IsKubernetesPod() {
			return RuntimeConfigFile
		}
		return "ConfigMap hive-config (key hive.yaml)"
	case LayerDashboardOverlay:
		return DashboardOverlayFile
	case LayerAgentOverlay:
		return "/data/agent-configs/<agent>.yaml"
	case LayerConfigEnv:
		return "/etc/hive/config.env"
	default:
		return ""
	}
}

// Writer describes who, if anyone, can write a layer at runtime. Reported by
// the provenance API so an operator can see not just which layer won but
// whether editing it is even possible.
//
// In Kubernetes, the ConfigMap's answer — "nobody" — is the single most
// useful fact this package exposes. Nothing in the system writes it: the hub
// only reads it, and the spoke's RBAC omits configmaps entirely. An operator
// who edits it and restarts will observe no change, which is exactly what
// happened four times in a row during the GHE incident.
//
// Outside Kubernetes there is no ConfigMap and no RBAC to omit — LayerSeed is
// RuntimeConfigFile, which the spoke rewrites on every save (saveLocked,
// config.go:5141) and the entrypoint restores over the boot config on every
// restart (entrypoint.sh:267-311, Docker/LXC branch). Reporting "nobody" there
// is the same misdirection in the opposite direction: it hides the one file
// that actually decides the value (#4971).
func (l Layer) Writer() string {
	switch l {
	case LayerSeed:
		if !IsKubernetesPod() {
			return "spoke (Config.Save)"
		}
		return "nobody (hub has read-only access; spoke RBAC omits configmaps)"
	case LayerDashboardOverlay:
		return "spoke (Config.Save) and hub (via heartbeat delivery)"
	case LayerAgentOverlay:
		return "spoke (dashboard agent edits)"
	case LayerConfigEnv:
		return "cluster admin (pod spec / mounted file)"
	default:
		return "n/a"
	}
}

// Writable reports whether any running component can write this layer.
//
// Outside Kubernetes LayerSeed is RuntimeConfigFile, which the spoke writes
// directly (see Writer), so it is writable there even though the same layer
// is genuinely unwritable ("nobody") in Kubernetes (#4971).
func (l Layer) Writable() bool {
	if l == LayerSeed {
		return !IsKubernetesPod()
	}
	return l != LayerUnset
}

// seedKeys carries key-presence facts that survive only in raw YAML, so the
// merge can be exactly faithful to the heredoc. See MergeLayersYAML.
type seedKeys struct {
	isPublicPresent bool
}

// MergeLayers applies the dashboard overlay over the ConfigMap seed and
// returns the merged config plus provenance naming the winning layer per
// field.
//
// It transcribes the entrypoint's boot-time merge. overlay may be nil (no
// overlay on disk), in which case the seed is returned untouched, exactly as
// at boot.
//
// FIDELITY: prefer MergeLayersYAML on the boot path. The heredoc gates
// hub.is_public on the KEY BEING PRESENT in the seed YAML, a distinction lost
// once YAML is parsed into HubConfig.IsPublic (a plain bool, where absent and
// explicit-false are both false). MergeLayersYAML preserves it; this function
// assumes the key is present, which holds for every provisioned seed on the
// fleet. See TestIsPublicKeyPresenceFidelity.
//
// Agent overlays and config.env are applied by LoadWithOverrides AFTER this
// function, which is why they outrank the dashboard overlay.
func MergeLayers(seed, overlay *Config) (*Config, *Provenance) {
	return mergeLayers(seed, overlay, seedKeys{isPublicPresent: true})
}

func mergeLayers(seed, overlay *Config, keys seedKeys) (*Config, *Provenance) {
	prov := NewProvenance()
	if seed == nil {
		if overlay != nil {
			prov.recordAll(overlay, LayerDashboardOverlay)
		}
		return overlay, prov
	}

	// A missing or implausible overlay means the seed is used as-is. This is
	// the dangerous path: seeds on the live fleet are 4–11% of the running
	// config, so booting on a bare seed can bring a hive up with a fraction of
	// its agent roster. Callers MUST surface OverlayRejected loudly.
	if overlay == nil || !OverlayIsPlausible(overlay) {
		prov.recordAll(seed, LayerSeed)
		if overlay != nil {
			prov.OverlayRejected = true
			prov.OverlayRejectReason = overlayRejectReason(overlay)
			// A rejection is severe: the hive is about to run on the bare
			// ConfigMap seed, which omits whole config blocks and can mean a
			// fraction of the agent roster. Say so at ERROR, naming the cause
			// and the file, so this is diagnosable from `kubectl logs` alone
			// rather than by inference from missing agents.
			log.Printf("ERROR: dashboard overlay REJECTED (%s) — falling back to the "+
				"ConfigMap seed, which may omit agents, policies, data, knowledge and "+
				"notifications. Overlay: %s. Inspect with GET /api/config/provenance.",
				prov.OverlayRejectReason, DashboardOverlayFile)
		}
		return seed, prov
	}

	// Start from the overlay: the default rule is "highest layer wins". This
	// mirrors the heredoc, which mutates the overlay dict and writes it back
	// over the seed path.
	merged := *overlay
	prov.recordAll(seed, LayerSeed)
	prov.recordAll(overlay, LayerDashboardOverlay)

	// ── A: hub.is_public — the seed wins (the only unconditional one) ─────
	// entrypoint.sh:131-133. Hub-managed and re-asserted every heartbeat, so
	// the seed merely mirrors hub intent; editing the ConfigMap to change it
	// is ineffective (see Layer.Writer).
	if keys.isPublicPresent {
		merged.Hub.IsPublic = seed.Hub.IsPublic
		prov.Set("hub.is_public", LayerSeed)
	}

	// ── B: acmm_level — the seed is only a FALLBACK ───────────────────────
	// The seed carries the provision-time level and is never updated on a
	// level change, so letting it win reverted a hive to its provisioned level
	// on every restart (issue #1856).
	if overlay.ACMMLevel == nil && seed.ACMMLevel != nil {
		merged.ACMMLevel = seed.ACMMLevel
		prov.Set("acmm_level", LayerSeed)
	} else if overlay.ACMMLevel != nil {
		prov.Set("acmm_level", LayerDashboardOverlay)
	}

	// ── C: the GitHub App anti-wipe ratchet ───────────────────────────────
	// Dashboard- or hub-installed App credentials live ONLY in the overlay;
	// the seed keeps whatever placeholder it was provisioned with, so the
	// overlay normally wins. But a missing/truncated/reverted overlay must
	// never silently downgrade a hive with a live App back to placeholder
	// creds — observed in production as a 401 loop after a pod restart.
	if SeedWinsGitHubApp(seed, overlay) {
		merged.GitHub = seed.GitHub
		prov.SetPrefix("github.", LayerSeed)
		prov.GitHubRatchetFired = true
	}

	return &merged, prov
}

// OverlayIsPlausible reports whether an overlay looks like a full hive config,
// mirroring the guard in the entrypoint heredoc and validateSaveGuard.
//
// This guard is load-bearing and sharp: an overlay failing it is DISCARDED and
// the hive boots on the bare ConfigMap seed. Because live seeds are 4–11% of
// the running config and omit whole blocks, a trip can bring a hive up with a
// fraction of its agents. Treat any change here as fleet-affecting, and never
// let a trip be silent — see Provenance.OverlayRejected.
func OverlayIsPlausible(overlay *Config) bool {
	return overlayRejectReason(overlay) == ""
}

// overlayRejectReason returns why an overlay is implausible, or "" if it is
// fine. Returning the reason (rather than a bare bool) is what lets the boot
// path log ERROR with a cause and lets provenance show it.
//
// ─────────────────────────────────────────────────────────────────────────
// WHAT THIS GUARD IS FOR, AND WHAT IT MUST NOT DO
// ─────────────────────────────────────────────────────────────────────────
//
// The guard exists to catch a TRUNCATED or CORRUPT overlay — a file cut off
// mid-write when the pod was SIGKILLed, or one that is not a hive config at
// all. Rejecting such a file is correct.
//
// It must NOT reject a structurally sound overlay that merely describes an
// unusual hive, because the consequence of rejection is severe and silent: the
// hive boots on the bare ConfigMap seed, which on the live fleet is 4–11% of
// the running config and omits whole top-level blocks (policies, data,
// knowledge, notifications, hive_id). A false rejection can bring a hive up
// with a fraction of its agent roster.
//
// The original predicate was "project.org non-empty AND len(agents) > 0". The
// agent-count half is wrong now, and demonstrably so:
//
//	#2361 shipped agent TOMBSTONES (Config.RemovedAgents), which make
//	deliberate deletion durable. An operator who deletes their last agent
//	produces a legitimately zero-agent overlay. The old guard classifies that
//	correct state as corrupt, discards it, and boots from the seed — which
//	still lists the deleted agents. The deletion silently un-does itself,
//	which is the exact bug #2361 was written to fix.
//
// Verified against the code as merged: an all-tombstoned config is also
// refused by validateSaveGuard ("no agents configured"), so the deletion
// cannot even be persisted. See TestZeroAgentOverlayWithTombstonesIsValid.
//
// The replacement test is STRUCTURAL AUTHENTICITY, not richness: does this
// file look like a hive config that was written completely? project.org is
// kept because Config.Save never writes an overlay without it and it is the
// cheapest true corruption signal. The agent-count check is replaced by a
// tombstone-aware one: zero agents is valid when the overlay explains itself
// with RemovedAgents, and suspicious only when it does not.
func overlayRejectReason(overlay *Config) string {
	if overlay == nil {
		return "overlay absent"
	}
	// project.org is the corruption canary. Config.Save() refuses to write an
	// overlay without it (validateSaveGuard), so an overlay lacking it was
	// never written by us — it is truncated, hand-edited, or another file.
	if overlay.Project.Org == "" {
		return "overlay has no project.org — the file is truncated or is not a hive config"
	}
	// Zero agents is VALID when the overlay records deliberate deletions.
	// Without that record it is more likely a truncated write than a real
	// hive, so it is still rejected — but the reason now names the
	// distinction instead of treating every empty roster as corruption.
	if len(overlay.Agents) == 0 && len(overlay.RemovedAgents) == 0 {
		return "overlay has no agents and no removed_agents tombstones — " +
			"likely a truncated write (a deliberately emptied roster records tombstones)"
	}
	return ""
}

// SeedWinsGitHubApp reports whether the seed's github block must be preserved
// over the overlay's — ratchet C. True only when the seed holds real App
// credentials and the overlay's look like a placeholder.
func SeedWinsGitHubApp(seed, overlay *Config) bool {
	if seed == nil || overlay == nil {
		return false
	}
	return !gitHubLooksPlaceholder(seed.GitHub) && gitHubLooksPlaceholder(overlay.GitHub)
}

// gitHubLooksPlaceholder mirrors _looks_placeholder() in the heredoc: an App
// is a placeholder when either identifier is absent, or the key file path is
// literally marked as a placeholder.
func gitHubLooksPlaceholder(gh GitHubConfig) bool {
	if gh.AppID == 0 || gh.InstallationID == 0 {
		return true
	}
	return containsFold(gh.KeyFile, "PLACEHOLDER")
}

// DanglingKeyFile reports whether gh names a key file, in either of the two
// locations a hive's App key is ever delivered to, that does not exist.
// The entrypoint warns rather than fails here, because the alternative is a
// silent 401 loop against GitHub. Surfaced through provenance so the condition
// is visible without reading pod logs.
//
// BOTH prefixes, not just the PVC (#4368). /secrets is the provisioning mount,
// and the provisioning template used to seed key_file=/secrets/gh-app-key.pem
// for every App-using hive while creating that Secret entry only for hives
// provisioned with an inline private key. Four hives shipped naming a file that
// would never exist — under the one prefix this predicate did not look at, so
// the detector written for precisely that symptom returned false for every one
// of them. A path outside both prefixes is an operator's own location and is
// left alone: they are entitled to keep a key somewhere this build has never
// heard of.
func DanglingKeyFile(gh GitHubConfig) bool {
	if !hasPrefix(gh.KeyFile, "/data/") && !hasPrefix(gh.KeyFile, "/secrets/") {
		return false
	}
	_, err := os.Stat(gh.KeyFile)
	return os.IsNotExist(err)
}

// hasPrefix is strings.HasPrefix, spelled out because this file imports only
// log and os — the same reason the original code compared the prefix by slice
// rather than calling into strings. Length-checked first, so a KeyFile shorter
// than the prefix returns false instead of panicking on the slice.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// containsFold reports whether s contains substr, ASCII case-insensitively.
func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + ('a' - 'A')
		}
		return b
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if lower(s[i+j]) != lower(substr[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
