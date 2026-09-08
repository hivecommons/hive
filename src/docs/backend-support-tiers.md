# Agent-backend support tiers and acceptance bar (planning draft)

> **Status: planning draft — hold-gated.** This document proposes an explicit
> acceptance bar for new agent backends. It codifies criteria that today exist
> only as reviewer tribal knowledge and precedent scattered across
> [docs/backend-setup.md](../../docs/backend-setup.md),
> [sandbox-isolation.md](sandbox-isolation.md), `config/backends.conf`, and old
> PR threads. Nothing here changes runtime behavior. Tracking issue:
> [#6231](https://github.com/hivecommons/hive/issues/6231).

## Why this document exists

The backend matrix has grown to 11 shell-known CLIs (`KNOWN_BACKENDS` in
`config/backends.conf`) plus gateway backends, spread across three de-facto
support classes that are enforced in code but stated nowhere:

| Tier | Backends today | Enforcement point |
| --- | --- | --- |
| **T1 — Headless-pod** | `claude`, `litellm`, `copilot`, `codex`, `goose` | `HEADLESS_BACKENDS` allowlist (`Justfile`) |
| **T2 — Attended/relay with confinement floor** | `opencode` (host-state deny-list); `claude`/`codex` sandbox narrowing on the local path | `config/backends.conf` per-backend permission flags |
| **T3 — Refuse-to-launch unconfined** | `goose`, `agy`, `bob`, `pi`, `aider`, `kilo` on the local path | #4918 refusal gate (`HIVE_<BACKEND>_DANGEROUSLY_RUN_UNCONFINED=1`) |

Because the criteria are implicit, every backend PR must reconstruct them.
PR #6222 (Muse Code) is the motivating example: half its body argues which
class muse belongs in, proves the CLI honors its approval flag rather than
silently ignoring it, and distinguishes itself from the opencode/kilo
precedents. That argument was correct — and it should not have to be invented
per PR.

## Proposed acceptance bar for a new backend

A PR adding backend `<name>` must state, in its body, the tier it claims and
the evidence for each applicable criterion:

### All tiers (T1–T3)

1. **Launch contract** — the exact binary, subcommand shape, and unattended
   flags (`backend_perm_flag`), with a transcript showing an invalid value for
   each safety-relevant flag is *rejected* (non-zero exit), not ignored.
   (This is the "`agy --sandbox` lesson": a flag the CLI ignores is not a
   control.)
2. **List parity** — `KNOWN_BACKENDS` (`config/backends.conf`) and
   `CLIBackends` (`src/pkg/config/config.go`) updated together, or the
   exception documented in `cliBackendExceptions`;
   `TestShellAndGoCLIBackendListsAgree` passes.
3. **Documentation** — a row in `docs/backend-setup.md` and the per-backend
   confinement matrix in `sandbox-isolation.md`, written to the same level of
   detail as existing rows (auth mechanism, credential storage path, headless
   verification status).
4. **Pinned install** — if added to `src/Dockerfile.contributor`, the CLI is
   installed from a pinned, checksummed release (precedent: agy, #5048).

### Tier 2 (attended/relay with floor) additionally

5. **Confinement posture stated** — either an OS-enforced sandbox that is on
   by default (claude/codex/muse class) with the unattended flags shown to
   leave it intact, or a host-state command deny-list floor (opencode class)
   covering the #4918 command families (privilege escalation and
   boot/deployment tools).

### Tier 1 (headless-pod) additionally

6. **Unattended credential verification** — evidence that a fresh, TTY-less
   pod can authenticate from staged credentials or env-injected keys, with the
   storage path named. "Works on a host that already signed in" is T2
   evidence, not T1 (this is why agy/opencode/kilo sit outside
   `HEADLESS_BACKENDS` — see the closed #5406).
7. **Allowlist update** — `HEADLESS_BACKENDS` in the `Justfile`, with the
   contribute-k8s subset reviewed deliberately.

### Explicit non-goals

- A backend with **no sandbox, no deny-list floor, and no unattended-credential
  story** can still land at T3 behind the
  `HIVE_<BACKEND>_DANGEROUSLY_RUN_UNCONFINED=1` gate — the bar for T3 is
  honesty about posture, not confinement itself.
- Tier assignment is per-path: a backend may be T2 on the container path and
  T3 on the local path (the common shape today).

## Tier movement

Promotion (T3→T2, T2→T1) is a normal PR that supplies the missing evidence for
the higher tier's criteria — the same bar as initial acceptance. Demotion
happens when a criterion is discovered to be unmet (e.g., a flag found to be
ignored); the discovering issue should cite the criterion number above.

## Open questions for maintainers

1. Should the bar live here (`src/docs/`) or in CONTRIBUTING.md, given
   third-party contributors are the audience?
2. Should T1 admission require a CI smoke (backend-smoke) run, or is manual
   transcript evidence acceptable for CLIs whose credentials cannot exist in
   CI?
3. Is `gemini` (Go-side only, per `cliBackendExceptions`) T2 or T3 on the
   local path? Its row predates the #4918 gate taxonomy.
