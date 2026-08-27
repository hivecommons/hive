# ioscan

`ioscan` protects the untrusted-text boundary before GitHub issue, PR, label, author, and comment text enters an agent kick. The implementation is `src/pkg/ioscan`, gated by the `ioscan:` block in `src/hive.yaml.example` and wired into the kick path from `src/cmd/hive/main.go`.

## Configuration

```yaml
ioscan:
  enabled: true                 # default: true; scans untrusted text before agent kicks
  fail_mode: open               # open (default) redacts; closed blocks Critical injection kicks and canary leaks
  canaries: false               # default: false; plant per-kick exfiltration canaries and scan agent egress
  classifier:
    enabled: false              # default: false; optional LLM judge for semantic plain-English injections
    model: ""                   # empty uses governor.litellm default_model, then gpt-4o-mini
    warn_threshold: 0.55        # advisory bead/audit threshold
    block_threshold: 0.85       # open=redact, closed=block kick
```

Scanning is on by default. `fail_mode` and the classifier are owner-only settings; the dashboard exposes them under Governor Config → Security.

## What it does

- **Deterministic rules** normalize Unicode steganography, decode suspicious base64, and detect known prompt-injection phrasing, dangerous directives, and secret shapes. These rules are always the floor when `ioscan` is enabled.
- **Redaction.** Blocked text is replaced with `[ioscan: content withheld — ...]`; the raw segment is not injected into the kick.
- **Canaries** (`ioscan.canaries`) plant per-kick exfiltration markers and scan agent egress. A trip writes an `ioscan_canary_leak` audit entry and, in `fail_mode: closed`, blocks the affected path.
- **Semantic classification** (`ioscan.classifier.enabled`) sends only already-redacted untrusted segments to an OpenAI-compatible LLM judge. The classifier fails open on errors and timeouts, so deterministic rules and canaries remain in force. Scores above `block_threshold` redact under `fail_mode: open` and block the kick under `fail_mode: closed`.

The classifier sits behind `Classifier.Score(ctx, text)` so a local ONNX/DeBERTa backend can replace the current LLM judge without changing the scheduler path.

## Related boundaries

These fail-closed behaviors are separate from `ioscan` and apply regardless of its setting:

- Prompt and agent-definition sourcing fail closed on unallowlisted GitHub repositories.
- Prompt fetch failures fail open to the last cached or embedded template, so kicks are not blanked by transient GitHub/API outages.
- GitHub PR claim and trust checks generally fail closed where duplicate work or privilege escalation is possible.
