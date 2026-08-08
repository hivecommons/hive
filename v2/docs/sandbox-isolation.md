# Sandbox isolation

Hive uses layered isolation so agent autonomy is earned and bounded even when
multiple backends run in the same installation.

Current controls include:

- backend-specific tool-deny settings by policy mode;
- scoped GitHub credentials and token minting;
- per-agent UID attribution where supported;
- MITM GitHub proxy rules that enforce write permissions at the network
  boundary; and
- trajectory review for post-run drift or escape signals.

See the [security threat model](security-threat-model.md),
[reference architecture](architecture.md#5-layered-guardrails-defense-in-depth),
and [ADR-0002](adr/0002-mitm-proxy-network-enforcement.md) for the detailed
runtime model. The roadmap tracks future work toward credential-free,
no-network sandbox kicks.
