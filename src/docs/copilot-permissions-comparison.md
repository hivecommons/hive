# Hive trust levels vs GitHub Copilot assisted permissions

Last reviewed: 2026-10-27

GitHub made Copilot "assisted permissions" generally available on 2026-10-26.
This page says what that feature is, what Hive does in the same space, and
where each does something the other does not. It is a comparison, not a
ranking. The two tools answer different questions.

**Terms:** ACMM = AI Codebase Maturity Model, Hive's autonomy levels.
PR = pull request; CI = continuous integration (automated checks).
**Crux** is Hive's agent runtime. A **shadow gate** records what it would
block without blocking; an enforcing gate acts on that decision.

## What Copilot assisted permissions is

Source: the GitHub changelog entry of 2026-10-26,
<https://github.blog/changelog/2026-10-26-github-copilot-assisted-permissions-generally-available>,
and the docs page it links,
<https://docs.github.com/copilot/concepts/customizations/allowing-safe-commands>.

- It is a permission mode for one Copilot session in Copilot CLI, the Copilot
  app, and VS Code. The three modes are manual approval, assisted, and
  unconditional approval (`--allow-all` / `--yolo`).
- When Copilot wants to run a shell command, touch a file path, or open a URL,
  a second language model looks at the request. If that model is confident the
  action is safe, the action runs with no prompt. If not, the user is asked.
- In non-interactive runs (`copilot -p ... --assisted-approval`, or
  `COPILOT_ASSISTED_APPROVAL=true` in CI), there is nobody to ask, so anything
  the review model will not clear is denied and the reason is handed back to
  Copilot so it can try another route.
- Existing allow and deny rules run first. Enterprise deny rules still apply.
  Enterprise admins can block the unconditional mode while leaving assisted
  on, disable assisted on its own, and set the default mode for new sessions.
- GitHub's own docs say it "is not a substitute for sandboxing or access
  restrictions" and that the review model "can make mistakes and approve
  dangerous or unintended actions".

In short: it decides, per action, inside one developer's session, whether to
show a prompt. It does not know about pull requests, merges, or repositories.

## What Hive does

Hive runs fleets of agents against repositories without a person at the
keyboard. Its controls are about what reaches GitHub, not about prompts.

At a glance:

- The ACMM level sets how much work may be delegated, from observation to
  eligible unattended merges at L6.
- Network and credential controls restrict agent writes outside the model.
- CI, holds, branch rules and configured review gates decide whether a
  particular PR can merge. Pausing an agent is not a universal merge stop.

Use the [comparison table](#comparison) for the short version. Maintainers can
expand the mechanism/evidence layer below; it preserves implementation details
and the limits of the observations.

<details>
<summary>Hive mechanisms and observed evidence (technical reference)</summary>

- **Trust levels (ACMM L1-L6).** The level decides which agents run and in
  which policy mode. Ordinary advisory agents write nothing to GitHub; the
  reviewer has a separate comment/review-only `converse` capability from L2.
  Measured agents
  may open issues. Hold-gated agents may open PRs, and every PR carries a
  `hold` label a human must remove. Full agents may merge on green CI. L1 and
  L2 are advisory, L3 and L4 add issues and hold-gated PRs, L5 opens PRs under
  `hold`, and only L6 merges. See [ACMM policy matrix](acmm-policy-matrix.md).
- **Lanes.** Each piece of work is assigned to a lane, the agent that owns it
  (scanner, reviewer, ci-maintainer, and so on). GitHub writes are allowlisted
  per lane in `write_surface.allowlist`, and a lane can be listed in
  `write_surface.enforce`. See [GitHub write surface](github-write-surface.md).
- **The policy proxy.** Every agent's GitHub traffic passes through a
  man-in-the-middle proxy (`pkg/proxy`). For an enforced lane it refuses every
  REST `POST`, `PUT`, `PATCH`, `DELETE`, every GraphQL mutation, and `git push`,
  returns `403`, and writes an `agent_write_refused` audit entry. Reads and
  the hive's own relays keep working. The proxy also applies repo pause, agent
  repo scope, the auto-merge policy, and the ACMM mode gate. The refusals are
  counted. Read on 2026-10-27 from `GET /api/evidence/write-counters` on the
  three hosted hives (hivecommons, kubestellar, projectbluefin), since the
  counter started on 2026-10-12: 538 direct agent write attempts, 123 refused,
  all with reason `mode_denied`. Hub admins see the same counters pooled in
  `GET /api/hub/evidence` (`write_counters[]` with `block_rate`).
- **Review gates.** `bin/merge-gate.sh` is deterministic: required CI green,
  not a draft, no `hold`, and an approved review for community PRs. The
  reviewer swarm adds verdicts after the change; `review.require_approval`
  makes a same-head approval required at L6. See [review swarm](review-swarm.md)
  and [running at level 6](running-at-level-6.md).
- **Hold and approval.** Below L6 every agent PR gets `hold`; a human removes
  it. `needs-human` is a triage signal, not a universal merge stop. At L6 a
  `reject` or `changes_requested` verdict needs a fix, a trusted human's
  exact-head GitHub Approve override of the enforcing sweep gate, or a direct
  human merge. Other merge requirements remain. See
  [labels and control signals](labels-and-control-signals.md).
- **Shadow gates.** `governor.review_gate.mode: shadow` (the default) and
  `intent.shadow` evaluate a gate and record what it would have held, and never
  block **by virtue of the shadow setting**. `review.require_approval: true`
  still forces review enforcement; a shadow setting cannot weaken it.
  The record is in each evidence bundle under `shadow`. The study that
  would turn those records into an accuracy figure is planned and not yet
  running ([ablation plan](research/ablation-plan-2026-10.md)). Enforcement from
  a shadow result is a config switch, not a separate feature.
- **Evidence.** Every mediated merge gets an evidence card; outcomes are
  published with n and 95% intervals. See [evidence](evidence.md) and
  [benchmarks](benchmarks/README.md).

</details>

## Comparison

| Dimension | Copilot assisted permissions | Hive |
|---|---|---|
| Unit of control | One action (command, path, URL) in one session | One GitHub write, one PR, one repository, one lane |
| Who decides | A second model, then the user | Deterministic config: level, lane allowlist, proxy rules, CI, labels |
| Where it runs | Inside the Copilot client | Outside the agent, in the proxy and governor; the agent cannot turn it off |
| Levels | Three modes per session (manual, assisted, allow-all) | Six repository levels, four policy modes per agent |
| Enterprise controls | Block allow-all, disable assisted, set default mode | Hub-managed level, repo pause, per-lane enforce, auto-merge toggle per repo |
| What it protects | The developer's machine and the paths and URLs Copilot touches | The repository's issues, PRs, branches, and merges |
| Unattended runs | Denies what the model will not clear; explanation goes back to the agent | Refuses at the proxy with `403` and a reason; audit entry written |
| Merge authority | None; Copilot has no view of merges | Only at L6, only on green CI, only in eligible lanes |
| Human approval step | A prompt per uncleared action | `hold` label and `needs-human` per PR; batch review |
| Measurement | None published | Refusal counters per tier, evidence cards, shadow-gate records |
| Model risk | The review model can approve a dangerous action (GitHub's own caveat) | Review verdicts come from models, but every merge still needs deterministic gates |
| Scope of engines | Copilot only | 15 CLIs through Crux, including Copilot CLI |

## What Copilot does that Hive does not

- Judge a shell command, file path, or URL before it runs. Hive sandboxes the
  agent and limits network egress, but it does not score individual commands.
- Reduce prompts for a developer sitting at a terminal. Hive has no interactive
  prompt step; its agents run unattended.
- Enterprise-managed defaults that apply to every developer's local session.
- Ship inside the editor and CLI people already use, with no deployment.

## What Hive does that Copilot does not

- Tie autonomy to repository maturity with a level a team can raise or lower.
- Refuse a GitHub write at the network layer so an agent cannot talk its way
  past the rule; the proxy is not a model and does not take explanations.
- Separate "may open a PR" from "may merge", and gate the merge on CI and
  review, not on a session mode.
- Put a `hold` on every agent PR below L6 so humans review in batches.
- Count refusals and publish merge outcomes with intervals.
- Run many agents from many vendors under one policy.

## How they fit together

Copilot CLI is one of the engines Hive drives (`copilot` in Crux
`CLIBackends`). An operator can run Copilot CLI inside Hive with
`--assisted-approval` for the command-level check, and still have every GitHub
write go through Hive's proxy and gates. The two layers do not overlap: one
watches commands, the other watches the repository.

## What this page does not claim

Hive has not measured how often Copilot's review model approves a dangerous
action, and GitHub does not publish that figure. The 123 refusals above are
counts from three hives over 15 days, not a rate for anyone else's fleet.
Neither tool stops a determined person with repository write access.
