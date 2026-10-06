# Hive × GitHub Agentic Workflows

This is an **issue-only, manually dispatched, report-only on-ramp**, not a Hive
runtime backend. It runs deterministic admission, then the existing
`bin/issue-classifier.sh`, then a gh-aw engine for advisory judgment. It never
calls `enumerate-actionable.sh` (which can write labels/comments), `merge-gate.sh`,
or Hive's scheduler. The admission check deliberately refuses PRs, closed issues,
hold/LFX/blocked labels, and `needs-human`; refusals fail the pre-agent step so
normal Actions failure semantics prevent the engine from starting.

## Install into a repository already using gh-aw

1. Copy `hive.md` to `.github/workflows/hive.md`, `prepare.py` to
   `src/deploy/gh-aw/prepare.py`, and Hive's `bin/issue-classifier.sh` to the same
   path in the target repository. Keep these reviewed files on its default
   branch; **do not execute code checked out from an issue's linked PR**.
2. Optionally add `config/hive-project.yaml` with a `classification` section
   (see Hive's [project config example](../../../config/hive-project.yaml.example)).
   Missing configuration uses the classifier's built-in defaults. The sample
   overrides only the classifier's input/log paths, preserving deployed defaults.
3. Install the compiler and generate the Actions lock file:

   ```bash
   gh extension install github/gh-aw --pin v0.89.21
   gh aw compile hive
   ```

   Commit both Markdown and `.github/workflows/hive.lock.yml`. Review the generated
   permissions, checkout, pre-agent steps, and engine configuration before enabling.
   Compilation needs no model secret; actual runs require gh-aw's engine credentials
   (for the default Copilot engine, follow the
   [gh-aw setup guide](https://github.github.com/gh-aw/)). You may select another
   supported gh-aw engine; Hive's model recommendation is advisory, not auto-routing.
4. Run on the **trusted default branch** from Actions → Hive → Run workflow,
   supplying an open issue number from this repository. Read the advisory report
   in the run output. The workflow grants only contents/issue reads and no
   repository-write safe outputs. Untrusted issue text is data, not shell input.

The sample is not installed as an active workflow in Hive. The
[compile CI](../../../.github/workflows/gh-aw-compile.yml) stages it temporarily,
compiles without model credentials, and tests admission and the actual classifier.

## Graduate to Hive without rewriting workflows

Keep existing gh-aw workflows for per-repo reports and CI tasks. Add the
[Hive contributor relay](../../docs/contributor-relay.md) or a Hive deployment
when you need continuous queue processing, fleet visibility, ACMM authority,
budget governance, or hub/spoke compute. Give Hive only the repositories and
stages you intend it to own; avoid two agents claiming the same implementation
queue. gh-aw remains Actions plumbing, not Hive's scheduler or merge authority.

## Inverse integration: deliberately not wired

There is **no gh-aw `pkg/extwork` adapter or dispatch configuration today**.
A future adapter may dispatch an Actions workflow only in report-only/shadow
mode, subject to the same admission bar as external OMP: no repository-write
credential or dashboard token for the external worker, typed stage capabilities,
lease/idempotency/cancellation, correlation to the dispatched run, and receipts
that Hive verifies rather than trusts. Reject write-capable stages in code and
prove that refusal with conformance tests before advertising runtime support.
Do not treat this sample's Actions token or a successful run as an extwork lease,
receipt, merge authorization, or backend-tier promotion. See
[backend support tiers](../../docs/backend-support-tiers.md) and
[external execution](../../docs/integrations/clanker-flue.md).

Gallery submission targets [githubnext/agentics](https://github.com/githubnext/agentics)
as a community-maintained workflow link; reciprocal linking is a request, not a
claim of GitHub endorsement.
