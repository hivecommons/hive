# ADR-0008: Redact untrusted kick input with visible markers

Status: Accepted (retroactive)

**Behavioral terms:** a **kick** is an agent work prompt. A **canary** is a
secret test marker whose appearance in outgoing (**egress**) text signals a
leak. **Fail open** continues work when a check cannot complete; **fail closed**
blocks it. A **semantic classifier** is a model-based check of meaning, used
in addition to fixed matching rules.

## Context

Hive builds agent kicks from issue titles, labels, comments, and other text that
may be controlled by an attacker. That text can carry prompt-injection phrases,
hidden Unicode, base64-encoded instructions, destructive commands, or secret
shapes. The ioscan package provides a pure scanner for input and output text
with structured findings and block decisions
([ioscan scanner](../../pkg/ioscan/ioscan.go)).

## Decision

### Kick-input contract

Scan untrusted kick-path text before injection and redact blocked content rather
than silently dropping the item or passing the raw payload through. `EnforceInput`
returns benign text byte-for-byte, but replaces blocked text with a visible
marker of the form `[ioscan: content withheld — ...]` that names the fired rules
without echoing the payload ([enforcement](../../pkg/ioscan/enforce.go)). The
scheduler applies this to issue text and labels and writes an audit entry for
blocked findings when an audit sink is attached
([scheduler enforcement](../../pkg/scheduler/ioscan_enforce.go)).

The v4 base also includes the fail-safe default-on mode: absent `ioscan:`
configuration scans by default, while an explicit `ioscan.enabled: false`
opts out ([config](../../pkg/config/ioscan_config.go)).

### Canary-based output protection

The current [ACMM packs](../acmm-policy-matrix.md#ioscan-hardening-defaults-per-level)
default canaries on at every level; an explicit `ioscan.canaries` value overrides
that default. With `ioscan.canaries: true`, Hive prepends a
random `HIVE-CANARY-...` marker to each kick with instructions that the agent
must never repeat it. Active canaries are kept in memory and persisted at
`/data/ioscan-canaries.json`; the GitHub MITM proxy scans outgoing issue/PR/
comment writes, and advisory finding ingestion scans collected agent reports. A
leak records an `ioscan_canary_leak` audit event plus a critical advisory bead;
with `ioscan.fail_mode: closed`, the proxy rejects the GitHub write with 403.
Opaque git pushes cannot be inspected safely, so canary fail-closed mode blocks
`git-receive-pack` pushes rather than allowing an unscannable exfiltration path.

### Publication scrubbing

Agent-derived advisory text is also scrubbed with `pkg/logscrub` before GitHub
publication so GitHub tokens, AWS access keys, bearer tokens, JWTs, and PEM
private-key blocks are redacted rather than echoed into comments.

### Optional semantic classifier

Hive also supports an optional semantic prompt-injection classifier under
`ioscan.classifier`. It runs after deterministic redaction, scrubs canaries
before any model call, caps segment size, caches verdicts by content hash, and
uses a per-eval budget. The first implementation is an LLM judge over the same
OpenAI-compatible model plumbing used by trajectory review; the interface is
small enough for a future local ONNX/DeBERTa backend. Classifier failures and
budget exhaustion fail open. Successful high-risk scores redact in open mode and
reuse the existing fail-closed kick-block/audit path in closed mode.

## Consequences

Agents can still see that an issue or label existed, but they do not receive the
raw injection or secret-bearing text. Visible markers make the behavior
auditable and reduce confusion compared with silent omission. The trade-off is
false positives: a legitimate title that matches a high-severity rule is
withheld until an operator investigates or disables ioscan explicitly. Output
enforcement also exists as a pure helper. The original decision concerned
kick-input redaction; the sections above distinguish the additional wired
canary, publication-scrubbing and optional classifier protections.
