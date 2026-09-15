# ioscan

`ioscan` protects the untrusted-text boundary before GitHub issue, PR, label, author, and comment text enters an agent kick. The implementation is `src/pkg/ioscan`, gated by the `ioscan:` block in `src/hive.yaml.example` and wired into the kick path from `src/cmd/hive/main.go`.

## Configuration

```yaml
ioscan:
  enabled: true                 # default: true; scans untrusted text before agent kicks
  fail_mode: open               # default: open at L1–L4, closed at ACMM L5–L6 (set by the packs); open redacts, closed blocks Critical injection kicks and canary leaks
  canaries: true                # default: true (nil == on); set false to opt out. Plants per-kick exfiltration canaries and scans agent egress
  classifier:
    enabled: false              # default: false; optional LLM judge for semantic plain-English injections
    model: ""                   # empty uses governor.litellm default_model, then gpt-4o-mini
    warn_threshold: 0.55        # advisory bead/audit threshold
    block_threshold: 0.85       # open=redact, closed=block kick
```

Scanning is on by default. **Canaries are on by default** (`canaries: false` opts out) now that the egress scan is encoding-aware ([#6701](https://github.com/hivecommons/hive/issues/6701), [#6720](https://github.com/hivecommons/hive/issues/6720), [#7083](https://github.com/hivecommons/hive/issues/7083)). **`fail_mode` has no fixed global default: it resolves to `closed` at ACMM L5–L6** (the levels where agents can merge) via the L5/L6 packs' `governor.ioscan_fail_mode`, and to `open` at L1–L4. An explicit `fail_mode` in the hive config always wins. The tradeoff of the `closed` default is that, at L5–L6, every Critical false-positive stalls the affected queue item until an operator clears it — set `fail_mode: open` if that operational load is unacceptable. `fail_mode` and the classifier are owner-only settings; the dashboard exposes them under Governor Config → Security.

## What it does

- **Deterministic rules** normalize Unicode steganography, decode suspicious base64, and detect known prompt-injection phrasing, dangerous directives, and secret shapes. These rules are always the floor when `ioscan` is enabled.
- **Redaction.** Blocked text is replaced with `[ioscan: content withheld — ...]`; the raw segment is not injected into the kick.
- **Canaries** (`ioscan.canaries`) plant per-kick exfiltration markers and scan agent egress. A trip writes an `ioscan_canary_leak` audit entry and, in `fail_mode: closed`, blocks the affected path. The egress scan is encoding-aware: outbound bodies are normalized and decoded before matching, so a canary emitted base64-encoded, hex-encoded, percent-encoded, reversed, case-changed, homoglyph-substituted or split across separators still trips it, as does an intact 16-hex-character window of the token ([#6686](https://github.com/hivecommons/hive/issues/6686)). The transform list is finite — an encoding it does not model still passes — and `git-receive-pack` bodies stay opaque, refused outright only under `fail_mode: closed`.
- **Semantic classification** (`ioscan.classifier.enabled`) sends only already-redacted untrusted segments to an OpenAI-compatible LLM judge. The classifier fails open on errors and timeouts, so deterministic rules and canaries remain in force. Scores above `block_threshold` redact under `fail_mode: open` and block the kick under `fail_mode: closed`.

The classifier sits behind `Classifier.Score(ctx, text)` so a local ONNX/DeBERTa backend can replace the current LLM judge without changing the scheduler path.

## Related boundaries

These fail-closed behaviors are separate from `ioscan` and apply regardless of its setting:

- Prompt and agent-definition sourcing fail closed on unallowlisted GitHub repositories.
- Prompt fetch failures fail open to the last cached or embedded template, so kicks are not blanked by transient GitHub/API outages.
- GitHub PR claim and trust checks generally fail closed where duplicate work or privilege escalation is possible.
