# Security notes: log scrubbing and secret redaction

Hive wraps its structured `slog` handler with `pkg/logscrub`. The wrapper redacts recognized token-like strings before log records reach the underlying handler.

## What is scrubbed

The current redaction pattern replaces matches with `[REDACTED]` in:

- log messages,
- string-valued attributes,
- string attributes inside slog groups, and
- attributes attached with `logger.With(...)`.

Recognized patterns are:

- GitHub token prefixes `ghs_`, `ghp_`, `gho_`, and `github_pat_` followed by at least ten token characters.
- JWT-like strings beginning with `eyJ` and containing three base64url segments.

The cost endpoint also redacts the configured gateway API key from native-cost probe errors before returning the error text.

## Limits

- Scrubbing is pattern-based, not a general secret scanner. A secret with another shape can still appear if code logs it directly.
- Non-string slog values are passed through unchanged unless they are inside a group containing string attributes.
- Redaction happens in the Hive logging path. Data written by external tools, agent CLIs, terminal transcripts, or third-party proxies is not automatically covered unless it flows through Hive's scrubbed logger.
- False positives are replaced with `[REDACTED]`; there is no runtime allow-list or custom pattern configuration today.

## Operator checks

To validate a deployment, emit a test log through Hive code or a local unit test using a fake `ghp_...` value and confirm that only `[REDACTED]` reaches the configured log sink. Never test with a real credential.
