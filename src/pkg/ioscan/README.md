# ioscan

`pkg/ioscan` protects the untrusted-text boundary before GitHub issue, PR, label,
author, and comment text enters an agent kick.

- Deterministic rules normalize Unicode steganography, decode suspicious base64,
  detect known prompt-injection phrasing, dangerous directives, and secret shapes.
- Blocked text is replaced with `[ioscan: content withheld — ...]`; the raw
  segment is not injected.
- Optional canaries (`ioscan.canaries`) plant per-kick exfiltration markers and
  block/report leaks.
- Optional semantic classification (`ioscan.classifier.enabled`) sends only
  already-redacted untrusted segments to an OpenAI-compatible LLM judge. The
  classifier fails open on errors/timeouts; deterministic rules and canaries
  remain the floor. Scores above `block_threshold` redact in `fail_mode: open`
  and block the kick in `fail_mode: closed`.

The classifier is behind `Classifier.Score(ctx, text)` so a local ONNX/DeBERTa
backend can replace the current LLM judge without changing the scheduler path.
