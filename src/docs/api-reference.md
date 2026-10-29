# Dashboard REST API reference

Pragmatic v1 endpoint index compiled by hand from route registrations in `src/pkg/dashboard/*.go` and `src/pkg/hub/*.go` (tests excluded). There is no generator; update the relevant area file under `api-reference/` in the same change that adds or renames a route. It lists method, path, coarse auth level, and one-line purpose. Request/response schemas are intentionally not hand-written here; see the handler source for exact payloads and validation.

Auth levels are derived from dashboard middleware (`isPublicPath`, dashboard token/session auth, and the `/api/v1` GitHub-token wrapper) or from hub route wrappers such as `requireAuth` (rows marked `Hub auth`) and `requireAdmin` (rows marked `Hub admin`). Hub rows marked handler-specific have no dashboard middleware; check the named handler for bearer secrets, admin checks, or public behavior.

History point limits, timestamp cutoffs, incremental cost reads and ETag/304 behavior are documented in [Dashboard history reads](dashboard-history-api.md).

Each section below links to its route table. Existing section anchors remain here for older links. Area files group related route prefixes and handlers; add routes and repair citations in those files.

Run `bash src/scripts/check-api-reference-citations.sh --fix` to repair drift across the reference, or pass an area file to limit the repair. Only files with drifted, resolvable citations are rewritten.

## Health, status, events

[Open route reference](api-reference/health-status-events.md#health-status-events).

## Snapshots and style

[Open route reference](api-reference/snapshots-style.md#snapshots-and-style).

## Auth and external accounts

[Open route reference](api-reference/auth-external-accounts.md#auth-and-external-accounts).

## Configuration

[Open route reference](api-reference/configuration.md#configuration).

## Agents and controls

[Open route reference](api-reference/agents-controls.md#agents-and-controls).

### Kick is asynchronous

[Open route reference](api-reference/agents-controls.md#kick-is-asynchronous).

## Packs and ACMM

[Open route reference](api-reference/packs-acmm.md#packs-and-acmm).

## Cost, tokens, telemetry

[Open route reference](api-reference/cost-tokens-telemetry.md#cost-tokens-telemetry).

## Knowledge

[Open route reference](api-reference/knowledge.md#knowledge).

## Contribute

[Open route reference](api-reference/contribute.md#contribute).

### Issue claims (hivecommons/hive#8380)

[Open route reference](api-reference/contribute.md#issue-claims-hivecommonshive8380).

### `/api/v1` contributor subpaths

[Open route reference](api-reference/contribute.md#apiv1-contributor-subpaths).

## Nous / strategy lab

[Open route reference](api-reference/nous-strategy-lab.md#nous--strategy-lab).

## Inception

[Open route reference](api-reference/inception.md#inception).

## Beads

[Open route reference](api-reference/beads.md#beads).

## Dashboard miscellaneous

[Open route reference](api-reference/dashboard-miscellaneous.md#dashboard-miscellaneous).

## Hub SaaS

[Open route reference](api-reference/hub-saas.md#hub-saas).

## Hub server

[Open route reference](api-reference/hub-server.md#hub-server).

