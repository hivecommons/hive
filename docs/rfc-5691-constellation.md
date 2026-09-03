# RFC #5691: Constellation hubs and coordinated spokes

Status: accepted design for v5 phase-1 delivery  
Refs: #5691, #5698, #5705

## Summary

A constellation is a set of subject-scoped Hive spokes registered with one hub
and operated as one fleet. The hub remains a registry and recommendation layer,
not a mandatory control plane: a stale hub should make the fleet less informed,
not down. This RFC defines the data the hub needs to coordinate several spokes
without taking over their scheduling or holding their credentials.

The first v5 phase is already partially delivered by PR #5705: the hub derives
repo-ownership overlaps from heartbeat registry data and exposes those overlaps
in `/api/registry` and fleet warnings. This document records that as phase 1 and
specifies the remaining contracts.

## Goals

- Make repo ownership visible across all registered spokes.
- Let each spoke advertise a short charter/scope so ownership and routing
  decisions are explainable.
- Publish fleet-wide provider headroom and GitHub App budget pressure without
  making the hub directly probe provider credentials.
- Document supported shared-credential deployment patterns and their
  Kubernetes constraints.
- Add an optional hub route recommendation API after overlap, charter, and
  headroom data exist.

## Non-goals

- The hub does not schedule agent work onto spokes.
- The hub does not hold provider credential material.
- The hub does not mutate spoke repo config by default.
- The hub does not make provider quota by itself; splitting a hive divides the
  same pools unless the operator gives spokes separate credentials or App
  installations.

## Current v5 baseline

- Heartbeats already register `Org`, `Repos`, `PrimaryRepo`, agent summaries,
  governor state, provider-limit signals, GitHub App identity, dashboard URL,
  and related health fields.
- The hub stores registry entries and exposes `/api/registry`, fleet UI pages,
  and bulk operations. Bulk operations use heartbeat-delivered project config,
  which preserves pull-only spokes.
- GitHub setup and webhooks already find candidate hives by org/repo, but they
  do not own global routing policy.
- PR #5705 added read-side overlap detection from existing heartbeat data. That
  is the correct first cut because it adds no new protocol or control-plane
  semantics.
- Provider quota is still spoke-local except for coarse provider-limit reason
  fields. RFC #5698 defines the normalized headroom shape this RFC reuses.

## Registry model

The hub maintains a normalized repo-ownership index keyed by:

```text
github_host / org / repo
```

Every index entry contains all claiming spokes, the source fields that produced
the claim (`repos` or `primary_repo`), timestamps, and warning state. Overlap is
a warning by default because intentional shared repos exist. Operators may add
an allowlist entry with a reason to silence a known overlap. Automatic routing,
when implemented, must refuse to assign an already claimed repo unless the
request explicitly allows a shared assignment.

## Spoke charters

Each spoke may report a short `charter` string alongside the existing repo list.
The charter describes the scope of work, for example "OS, images, installers,
and packaging" or "desktop apps and docs". The spoke value should live near the
spoke's project/repo config so it changes with the repo set.

The hub may also store an override with provenance:

```yaml
charter:
  value: desktop apps, app distribution, and docs
  source: spoke | hub
  updated_at: 2026-09-03T00:00:00Z
  updated_by: hub-operator
```

The registry API and UI show both the effective value and its source. Values are
operator-visible metadata and must be length-limited and sanitized like other
spoke-reported display fields.

## Fleet provider headroom

Spokes publish provider headroom to the hub by heartbeat after RFC #5698's
normalized capacity reading exists. The hub aggregates by a quota pool key and
never calls provider usage endpoints itself.

Pool identity is explicit when possible:

1. Operator-supplied `pool_id` wins.
2. Credential-source-derived identity, such as a mount path or secret reference,
   is used when available because shared credentials define shared quota.
3. Provider-only aggregation is a last resort and is marked low confidence.

Headroom is a list of limit windows, not one number. Unscoped limits constrain
provider availability; scoped limits constrain only matching model classes.
Consumers decide their own projection: availability can use unscoped exhaustion
thresholds, while pacing compares each limit's remaining headroom to its reset
time. The hub stores freshness and error category so stale, unknown,
unauthenticated, rate-limited, and exhausted states remain distinguishable.

## GitHub App rate-limit visibility

A constellation should prefer separate GitHub App installations per spoke when
operators need strict isolation. For shared installations, each spoke reports
installation rate-limit remaining and reset time when it already has that data.
The hub renders budget pressure keyed by App/installation ID.

The cheap cooperative mitigation is to stagger and jitter initial enumeration,
especially when a new spoke comes online with many repos. Full cooperative
budget negotiation is deferred until there is steady-state evidence that display
and stagger are insufficient.

## Shared credential guidance

A supported deployment may share one credential directory across several spokes
when those spokes are intended to consume one provider account. This extends the
existing one-directory-many-agents pattern inside a single hive. Operators must
understand these constraints:

- The directory is sensitive credential material and remains spoke-side; the hub
  never stores or proxies it.
- Kubernetes `ReadWriteOnce` volumes bind to one node, so cross-spoke sharing may
  require co-scheduling or a storage class that supports the desired access
  mode.
- PVCs are namespaced; cross-namespace sharing needs an explicit storage design,
  not an accidental hostPath copy.
- Pool identity for headroom should be set explicitly when the deployment cannot
  derive it safely from the credential source.

## Route recommendation API

After overlap, charter, and fleet headroom data exist, the hub may add a
recommendation endpoint:

```http
POST /api/route
{
  "github_host": "github.com",
  "org": "tuna-os",
  "repo": "installer"
}
```

The response ranks candidate spokes with reasons: charter match, existing org
coverage, repo overlap status, headroom, App budget pressure, and freshness. It
may return `unassigned` when confidence is low. It does not mutate spoke config
unless a future explicit per-spoke delegation mode is designed.

## Rollout plan

1. **Overlap detection — delivered by #5705.** Build and surface read-side repo
   overlap warnings from existing registry data.
2. **Charters.** Add optional spoke charter metadata to heartbeat, registry
   storage, registry API, and fleet UI.
3. **Fleet headroom.** Reuse RFC #5698's normalized capacity readings in
   heartbeat and hub aggregation.
4. **GitHub App budget display.** Surface remaining/reset data and jitter first
   enumeration for shared installations.
5. **Shared-credential docs/config.** Document supported patterns and add only
   spoke-side config knobs; never put credential material in the hub.
6. **Route recommendations.** Add a read/recommend endpoint with refusal for
   already claimed repos unless explicitly shared.

## Acceptance criteria for closing #5691

This RFC is the accepted constellation design artifact. It records the phase-1
implementation already merged in #5705 and defines the remaining v5-compatible
contracts without changing the hub into a hard control plane. Implementation
work after this document should be tracked in narrower follow-up issues.
