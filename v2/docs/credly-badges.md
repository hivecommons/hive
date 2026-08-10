# Credly badges — integration design (planned, not yet built)

> **Planned feature.** Hive currently ships only the contributor-card placeholder and milestone mapping. No live Credly API calls, credentials, or badge issuance exist yet.

Status: **design only.** The contributor "Me" card (Leaderboard tab of
`/contribute`) shows a **Heraldry** section that mirrors a contributor's OWN
public Credly badges (self-linked via the dossier's `credly_name` field — see
`dossier.go`), but no hive-issued badges exist yet; issuing them requires the
operator setup below. Once the operator provisions templates and issuance goes
live, hive-issued badges appear in the same Heraldry hall automatically — same
public profile JSON, same mount.

## Why placeholder-first

Issuing verifiable Credly badges requires assets and credentials that only the
operator can provision: a Credly (Acclaim) organization, badge **templates**, and
an **Issuer API token**. None of these can be hardcoded or shipped in the repo, so
the live integration is deferred. The card ships the Heraldry mount (and the
milestone → badge mapping below) now, so the wiring point is obvious when the
integration lands.

## What the operator must set up first

1. **Credly organization** — an issuing org on `credly.com` (formerly Acclaim).
2. **Badge templates** — one template per badge we intend to issue (see mapping
   below). Each template has a stable **template id**.
3. **Issuer API token** — an organization API token for the Issuer API
   (`https://api.credly.com/v1/organizations/{org_id}/badges`).

These are supplied to Hive **via configuration / environment only** — never
hardcoded, never committed:

| Purpose            | Env var (proposed)          |
| ------------------ | --------------------------- |
| Org id             | `HIVE_CREDLY_ORG_ID`        |
| Issuer API token   | `HIVE_CREDLY_API_TOKEN`     |
| Template id map    | `HIVE_CREDLY_TEMPLATES` (JSON: `milestoneID → templateID`) |

If `HIVE_CREDLY_API_TOKEN` is unset, the feature stays in placeholder mode — the
card renders exactly as it does today.

## Milestone → badge template mapping

The Me card derives milestones from **real thresholds** (see `me_profile.go` and
`contributorAutoPromoteAt` / `contributorTrustedAt` in `api_contribute.go`). Each
attained milestone maps to one Credly badge template:

| Milestone id (from the profile API) | Meaning (real threshold)        | Credly badge      |
| ----------------------------------- | ------------------------------- | ----------------- |
| `tier-contributor`                  | Reached Contributor (5 PR-tasks)| Contributor badge |
| `tier-trusted`                      | Reached Trusted (20 PR-tasks)   | Trusted badge     |
| `tier-merger`                       | Reached Merger (maintainer/owner-granted) | Merger badge |
| `tier-advisor`                      | Reached Advisor (maintainer-granted) | Advisor badge |
| `tasks-25`                          | 25 tasks shipped                | 25-task milestone |
| `tasks-100`                         | 100 tasks shipped               | 100-task milestone|

Keep this table in sync with the badge templates when they are created.

## When to issue

Issue a badge **once**, at the moment the milestone is first attained — i.e. on:

- **Tier promotion** — when a contributor crosses `newcomer → contributor`
  (auto, `TasksWithPR >= contributorAutoPromoteAt`) or is granted
  `trusted` / `merger` / `advisor` by a maintainer/owner.
- **Task-shipped landmark** — when `TasksCompleted` first reaches a landmark in
  `taskShippedMilestones`.

Issuance is **idempotent**: record the issued Credly badge id on the contributor
profile so re-running the promotion path never issues a duplicate. The natural
hook is the same promotion code that already fires the `promoted` event
(`contribute_ws.go`).

## Sharing

Once badges are live, **Credly's own native "Share to LinkedIn"** is the canonical
share path for a *verified* badge (it links back to the verifiable badge page).
Until then — and for contributors who simply want to share a milestone — the Me
card offers a **no-OAuth LinkedIn share link** (LinkedIn's
`share-offsite` dialog, pre-filled with the real achievement text). That interim
share ships today; it needs no credentials and makes no LinkedIn API call.

## Non-goals for the current change

- No live Credly Issuer API call.
- No credentials in the repo or in the page.
- No badge issuance. The mapping and the "coming soon" UI only.
