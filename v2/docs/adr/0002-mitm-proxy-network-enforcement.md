# ADR-0002: MITM proxy network enforcement

Status: Accepted (retroactive)

## Context

Hive agents can run powerful coding CLIs in tmux sessions. A malicious prompt or
compromised agent may try to bypass shell wrappers and call GitHub APIs directly.
The reference architecture therefore defines three guardrail layers keyed by the
same per-agent mode: CLI tool-deny, scoped credentials, and a MITM GitHub proxy
([architecture §5](../architecture.md#5-layered-guardrails-defense-in-depth)).
The deterministic pipeline and merge gate produce the list of PRs that may merge
([architecture §4](../architecture.md#4-the-deterministic-pipeline)).

## Decision

Keep a local MITM proxy in the agent egress path for `api.github.com`. The proxy
classifies REST and GraphQL requests by method/path/body and requires the
minimum ACMM mode for each write. It denies unknown writes by default, hard-denies the REST
`POST /pulls` and `PUT /pulls/{n}/merge` paths so those operations use Hive
relays, and applies a configured repo allowlist. GraphQL mutations are
mode-classified to the closest REST capability tier. Read-only GitHub requests
remain available
at advisory mode; `github.com` is tunneled opaquely for OAuth/git smart HTTP as
documented in the architecture.

## Consequences

Network enforcement remains independent of prompt instructions and CLI wrapper
behavior, so a bypass in one layer is still caught by another. The proxy also
provides an auditable block point (`X-Hive-Proxy-Blocked`) and a place to close
GraphQL/REST parity gaps. The trade-off is operational complexity: the container
must install/trust a local CA, iptables redirection must work, and future GitHub
write paths need explicit rules or relay handling.
