# Sandbox isolation and agent guardrails

Hive's v2 isolation model is defense in depth rather than a single sandbox switch. Agents run as long-lived CLI sessions, but the deterministic pipeline and proxy layers decide what work they see and what writes they can perform.

## Isolation layers

1. **Policy mode** — ACMM selects advisory, measured, hold-gated, or full behavior per agent.
2. **Deterministic admission** — Go and shell checks classify work, apply holds, and decide whether an agent is kicked.
3. **Scoped credentials** — contributor relays and spoke agents use the GitHub identity and token scope appropriate to that actor; a delegated ClankeR role does not grant spoke secrets.
4. **MITM GitHub proxy** — GitHub API writes are attributed and constrained according to the current mode.
5. **Merge gates** — hold labels, green checks, self-merge bans, and auto-merge sweeps are enforced outside the LLM prompt.

## Operator notes

- Prefer the least-capable ACMM level that matches the project phase.
- Keep privileged delegated roles (`ci-maintainer`, `sec-check`, `architect`) behind explicit grants.
- Use the docs in this index for concrete setup: [ACMM policy matrix](acmm-policy-matrix.md), [Agent configuration](agent-configuration.md), and [Contributor trust tiers and delegated agent roles](contributor-trust-and-roles.md).

