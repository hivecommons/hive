# Security notes: log scrubbing and secret redaction

> **Scope:** this page covers log redaction only. For the operator-facing security model (Ed25519 sessions/SSO, key rotation, forced proxy egress and `CAP_NET_ADMIN`, supply chain), see [security-model.md](security-model.md); for the attacker-oriented view, see [security-threat-model.md](security-threat-model.md).

Hive wraps its structured `slog` handler with `pkg/logscrub`. The wrapper redacts recognized token-like strings before log records reach the underlying handler.

**This covers the Go process only.** The contributor relay is a separate Node process and does not use `pkg/logscrub` at all — it carries its own, independently written redaction. The two are described separately below, because they do not cover the same things. See [Two redaction layers, not one](#two-redaction-layers-not-one).

## What is scrubbed

The current redaction pattern replaces matches with `[REDACTED]` in:

- log messages,
- string-valued attributes,
- string attributes inside slog groups, and
- attributes attached with `logger.With(...)`.

Recognized patterns are:

- GitHub token prefixes `ghs_`, `ghp_`, `gho_`, `ghu_`, `ghr_`, and `github_pat_` followed by at least ten token characters.
- JWT-like strings beginning with `eyJ` and containing three base64url segments.
- Hive canary values (`HIVE-CANARY-` followed by 48 hex characters).
- AWS access key IDs (`AKIA`/`ASIA` followed by 16 uppercase alphanumerics).
- `Bearer` authorization values of 16 characters or more.
- PEM private-key blocks — RSA, EC, OpenSSH, DSA, encrypted, and PGP private-key blocks — redacted whole, including multi-line bodies.

The GitHub and JWT shapes live in the exported `logscrub.TokenPattern`, which other packages (for example `pkg/ioscan`) reuse rather than duplicating; keep that the single source of truth.

The cost endpoint also redacts the configured gateway API key from native-cost probe errors before returning the error text.

## Two redaction layers, not one

`pkg/logscrub` protects the **Go process**. The contributor relay (`bin/contributor-relay.js`) is a **separate Node process** that never loads it, and redacts on its own with a `redactTokens()` function. Both exist and both work — but they are separate implementations with separate pattern lists, and reading only the section above would leave you assuming a coverage the relay does not have.

The relay applies `redactTokens()` to agent output before it leaves the host: to the captured tmux tail, and to the combined stdout/stderr tail of a headless task. See [Contributor relay](contributor-relay.md).

### Where they differ

| Shape | `pkg/logscrub` (Go) | relay `redactTokens()` (Node) |
| --- | :---: | :---: |
| `ghs_` `ghp_` `gho_` `ghu_` `ghr_` | yes | yes |
| `github_pat_` | yes | yes |
| JWTs (`eyJ…` triples) | yes | **no** |
| `HIVE-CANARY-…` | yes | **no** |
| AWS `AKIA`/`ASIA` keys | yes | **no** |
| `Bearer <value>` | yes | **no** |
| PEM private-key blocks | yes | **no** |
| Backend provider credentials | no | Pi backend only |

Three further differences worth knowing:

- **Minimum match length.** The Go patterns require 10 or more characters after the prefix; the relay requires 36 or more. A short or truncated token-shaped string is redacted by Go and passed through by the relay. The relay's bound is deliberately `{36,}` rather than `{36}` so a *longer* token is not redacted only in its first 36 characters, leaking its tail ([#4267](https://github.com/hivecommons/hive/issues/4267)).
- **Character class.** Go accepts `[A-Za-z0-9_]` after every prefix. The relay accepts only `[A-Za-z0-9]` for the `gh*_` prefixes (underscores allowed for `github_pat_` alone).
- **Placeholder text.** Go writes `[REDACTED]`; the relay writes `<prefix>_***REDACTED***`, keeping the prefix visible. Alerting that greps for one will not match the other.

When the relay's backend is Pi, `redactTokens()` additionally strips the configured provider credential values by literal substring match — a mechanism neither layer applies to GitHub tokens.

**The practical reading:** GitHub token material is covered on both paths. Everything else in the Go list — JWTs, canaries, AWS keys, `Bearer` headers, private keys — is redacted in the hive's own logs and **not** in relay-forwarded agent output. If an agent's terminal prints a JWT or a private key, the relay will forward it.

## When a redaction reaches an agent that is reading code

Redaction protects a log sink. It is a hazard for an agent, because a masked
line is indistinguishable from source: the placeholder sits exactly where the
characters the agent is reasoning about used to be.

This is not hypothetical. On [#8067](https://github.com/hivecommons/hive/issues/8067)
a reviewer agent quoted

```
printf 'header = "Authorization: ******"\n' "$TOKEN"
```

and reported that the format string contained no `%s`, so the credential was
never sent. The file said `Authorization: Bearer %s`. The reasoning was sound
and the conclusion was wrong, because a scrubber — not this one; Go writes
`[REDACTED]`, never a run of asterisks — had rewritten the text on the way in.
The review was published, so the mask reached the PR author looking like their
own code.

Two mitigations, in order of how much they are worth relying on:

- **The relay refuses to publish a review whose evidence is masked text.**
  `redactedQuoteRefusal`, `pkg/github/review_redaction_guard.go:82`, inspects
  the fenced blocks and inline code spans of a review body before the
  review-request watcher submits it. A body that quotes a redaction marker is
  quarantined with an error naming the quotation to re-read, and no review is
  posted. Prose *about* redaction is untouched — only text quoted as code
  counts. The marker vocabulary is `FindRedactionMarker`,
  `pkg/logscrub/redaction.go:55`, which recognizes hive's own `[REDACTED]` and
  `[REDACTED-JWT]` alongside the `<redacted>`, `***REDACTED***` and bare
  `******` forms other layers emit — the layer that masks the text is not
  always one of ours.
- **The reviewer prompts say so.** `groundingSection`
  (`pkg/review/prompts.go:71`) and the reviewer policies
  (`policies/reviewer-queue.md`, `policies/reviewer-advisory.md`) tell the
  reviewer that a mask is a hive artifact, never a defect, and that a line
  carrying one must be re-read from the repository before it is quoted.

Note the design principle this shares with the turn model: `Clone`/`Step`
deliberately do **not** scrub, precisely so an agent never reads redacted
content back as fact (see
[design/reentrant-turn-model.md](design/reentrant-turn-model.md)). Scrub on the
way *out*, to a sink; never on the way *in*, to a reasoner.

## Limits

- Scrubbing is pattern-based, not a general secret scanner. A secret with another shape can still appear if code logs it directly.
- A redaction that lands in text an agent reads is a correctness hazard, not just a lossy log. See the section above; `pkg/logscrub` cannot tell whether its caller is writing to a sink or to a model.
- Non-string slog values are passed through unchanged unless they are inside a group containing string attributes.
- `pkg/logscrub` redaction happens in the Hive logging path. Data written by external tools, agent CLIs, or third-party proxies is not automatically covered unless it flows through Hive's scrubbed logger. Agent terminal output forwarded by the contributor relay is a partial exception — it is redacted, but by the relay's own narrower pattern list, not by `pkg/logscrub`.
- The two layers can drift. They are separate implementations with no shared source of truth, so a pattern added to one does not appear in the other; the table above is accurate as of writing and worth re-checking against both implementations before relying on it.
- False positives are replaced with `[REDACTED]`; there is no runtime allow-list or custom pattern configuration today.

## Operator checks

To validate a deployment, emit a test log through Hive code or a local unit test using a fake `ghp_...` value and confirm that only `[REDACTED]` reaches the configured log sink. Never test with a real credential.
