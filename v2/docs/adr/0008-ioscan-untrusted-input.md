# ADR-0008: Redact untrusted kick input with visible markers

Status: Accepted (retroactive)

## Context

Hive builds agent kicks from issue titles, labels, comments, and other text that
may be controlled by an attacker. That text can carry prompt-injection phrases,
hidden Unicode, base64-encoded instructions, destructive commands, or secret
shapes. The ioscan package provides a pure scanner for input and output text
with structured findings and block decisions
([ioscan scanner](../../pkg/ioscan/ioscan.go)).

## Decision

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
opts out ([config](../../pkg/config/config.go)).

Operators may additionally enable `ioscan.canaries: true`. Hive then prepends a
random `HIVE-CANARY-...` marker to each kick with instructions that the agent
must never repeat it. Active canaries are kept in memory and persisted at
`/data/ioscan-canaries.json`; the GitHub MITM proxy scans outgoing issue/PR/
comment writes, and advisory finding ingestion scans collected agent reports. A
leak records an `ioscan_canary_leak` audit event plus a critical advisory bead;
with `ioscan.fail_mode: closed`, the proxy rejects the GitHub write with 403.
Opaque git pushes cannot be inspected safely, so canary fail-closed mode blocks
`git-receive-pack` pushes rather than allowing an unscannable exfiltration path.

Agent-derived advisory text is also scrubbed with `pkg/logscrub` before GitHub
publication so GitHub tokens, AWS access keys, bearer tokens, JWTs, and PEM
private-key blocks are redacted rather than echoed into comments.

## Consequences

Agents can still see that an issue or label existed, but they do not receive the
raw injection or secret-bearing text. Visible markers make the behavior
auditable and reduce confusion compared with silent omission. The trade-off is
false positives: a legitimate title that matches a high-severity rule is
withheld until an operator investigates or disables ioscan explicitly. Output
enforcement exists as a pure helper, but the current ADR records the kick-input
path that is wired in this base.
