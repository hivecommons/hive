# ADR-0007: Mint short-lived scoped agent credentials

Status: Accepted (retroactive)

## Context

Several v4 security fixes tightened token handling, including stopping full
installation-token exposure to agents, avoiding host CLI/PAT leakage, and
making shared App-token caches owner-only
(`security(H3 follow-up): stop leaking full installation token to agents via
HIVE_GITHUB_TOKEN`, `fix(contributor): stop leaking host CLI configs + PAT to
hub`, `fix(security): H3 — shared App-token cache owner-only + fail-loud
fallbacks`). Hive still needs a way to give agents cloud or registry authority
without distributing shared long-lived credentials.

## Decision

Introduce the mint as an opt-in short-lived credential issuer
([mint package](https://github.com/kubestellar/hive/blob/v4/v2/pkg/mint/mint.go)). It signs scoped JWTs with bounded TTLs,
verification that fails closed, and a JWKS endpoint for downstream Workload
Identity Federation providers. Agent integration maps the same trust tiers used
by agent modes (`advisor`, `newcomer`, `contributor`, `trusted`) to explicit
scope strings such as `issues:read`, `contents:write`, and `pulls:merge`
([agent minting](https://github.com/kubestellar/hive/blob/v4/v2/pkg/mint/agent.go)).

The mint supplements, rather than replaces, the existing GitHub App token path.
When disabled, agent minting is a no-op. When enabled, empty agent identities,
unknown tiers, empty server secrets, bad signatures, and expired tokens all fail
toward least privilege or denial.

## Consequences

Leaked minted credentials have a bounded lifetime and a scope set tied to the
agent's trust tier, reducing blast radius compared with shared long-lived
tokens. JWKS-based verification also lets external brokers trust the hive
issuer without embedding private keys. The trade-off is another security
service and key lifecycle to operate, and the initial server still uses a
shared-secret caller gate until stronger caller identity verification is wired.
