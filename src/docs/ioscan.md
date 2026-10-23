# ioscan: scanning untrusted text before agents read it

Last reviewed: 2026-10-23

Issue titles, bodies, labels, author names and comments are written by
anyone on GitHub. `ioscan` scans that text before Hive builds it into an
agent's task, and checks what agents send out for planted secrets. It is on
by default.

## What it does

* **Cleans and matches.** Removes invisible Unicode characters and
  look-alike letters, then matches the text against rules for known
  prompt-injection phrasing, dangerous instructions and secret-shaped
  strings. These rules always run when `ioscan` is enabled.
* **Peels encodings.** Base64 is decoded up to three layers deep. Hex
  escapes, HTML entities, percent encoding, rot13 and hyphen or underscore
  slugs (`ignore-all-previous-instructions`) are each turned back into
  plain text and rescanned. A hit in a decoded layer is reported as
  `injection.obfuscated` at high.
* **Tells use from mention.** A short quoted phrase on one line that the
  text frames as something being talked about ("issues containing '...'",
  "the string '...'") is recorded at low and not withheld. A critical rule
  such as `rm -rf` is never softened this way. A quoted phrase with no such
  framing, or a fenced block, is still a payload.
* **Withholds matched text.** Text that trips a rule is replaced with
  `[ioscan: content withheld — ...]`. The original never reaches the agent.
  How strong a match has to be depends on who wrote the text and where.
  See [Block thresholds](#block-thresholds).
* **Reads a thread as one unit.** Where Hive hands an agent several
  comments from the same issue or PR, it scans them one at a time and then
  once more joined in order. A payload split across two comments matches on
  the joined text, and both halves are withheld. The joined scan covers the
  most recent 64 comments or 128 KB, whichever is smaller.
* **Plants canaries.** Each task gets its own marker string. Hive scans
  everything the agent sends out for that marker, including copies that are
  base64-, hex- or percent-encoded, reversed, re-cased, split across
  separators or written with look-alike characters. A hit writes an
  `ioscan_canary_leak` audit entry and, in `closed` mode, blocks the request.
  Encodings outside that list are not caught. Git push bodies are not
  inspected; in `closed` mode they are refused.
* **Semantic classifier.** A language model scores the already-cleaned text
  for injections written in plain English that no rule matched. It is on by
  default at autonomy levels L4–L6 and off below. When it cannot answer,
  the rules have already run; what happens next depends on its fail mode.
  See [Classifier](#classifier).

## What it does not do

Rules match specific phrasings. A measured test against 63 attack payloads
withheld 97% and let 3% reach the agent, with one wrongly withheld benign
case in twelve. A paraphrase that avoids every rule still passes; the
classifier is the control for that. Details and numbers:
[ioscan-red-team.md](ioscan-red-team.md). The control that stops a persuaded
agent from merging code is the network proxy, which denies pull-request
creation and merge calls for every agent, not `ioscan`.

## Configuration

```yaml
ioscan:
  enabled: true          # default true
  fail_mode: open        # default: open at ACMM L1–L4, closed at L5–L6
  canaries: true         # default true
  block_at:              # per-surface line for untrusted authors
    issue_body: medium   # default medium on every surface
    comment: medium
    pr_body: medium
    review: medium
  classifier:
    enabled: true        # default: on at ACMM L4–L6, off at L1–L3
    fail_mode: closed    # default: follows ioscan.fail_mode (closed at L5–L6)
    model: ""            # empty uses the governor's default model
    warn_threshold: 0.55
    block_threshold: 0.85
```

* `fail_mode: open` withholds matched text and lets the task proceed.
  `closed` blocks the task on a Critical finding or a canary leak. The
  default is `open` at autonomy levels L1–L4 and `closed` at L5–L6, where
  agents can merge. An explicit value in the hive config always wins. In
  `closed` mode a Critical false positive stalls that queue item until an
  operator clears it.
* `fail_mode` and the classifier are owner-only settings, shown in the
  dashboard under Governor Config → Security.
* `block_at` is set in the hive config file. A typo in a surface name or a
  severity is a config error at load time, never a silent default.

## Block thresholds

Every rule has a severity: low, medium, high or critical. Text is withheld
when a finding reaches the line for its surface and author. The four
surfaces are `issue_body` (issue title, body and labels), `comment`,
`pr_body` (PR title, body and author login) and `review` (review bodies and
inline review-thread comments).

| Author | Line | What it means |
|---|---|---|
| Untrusted (anyone else) | `block_at` for the surface, default **medium** | Any injection or dangerous-instruction rule that fires withholds the text. |
| Trusted | **high**, always | Only a strong match withholds the text. |

A critical finding withholds text on every line. A secret-shaped string
below critical never withholds input on its own; it is reported, not acted
on.

Trusted means the hive's own identities, GitHub Apps and bot accounts, and
for pull requests a repository owner or member. Collaborators, contributors
and unknown authors are untrusted. Review-bot threads are always untrusted,
because the bot quotes text written by anyone.

Why the default moved to medium: in the red-team test, 19% of attacks
matched a rule that was recorded and then handed to the agent anyway. The
medium line withholds those. The cost is more withheld text from strangers
when a rule misfires; raise one surface at a time with `block_at` if that
happens on your hive.

## Classifier

The rules catch phrasings; the classifier reads intent. After the rules have
cleaned a piece of untrusted text, the classifier asks a small model for a
score from 0 to 1. Above `warn_threshold` it files an advisory. Above
`block_threshold` it withholds the text, or in `closed` mode blocks the
task.

**Defaults by level.** From L4 up agents open pull requests, so a paraphrase
that slips past the rules has somewhere to go. The classifier is therefore
on by default at L4–L6 and off at L1–L3. Set `classifier.enabled` to
override either way.

**Fail mode.** When the classifier cannot answer (endpoint down, timeout,
unreadable reply), `open` passes the rule-cleaned text on and records a skip.
`closed` withholds the text with an `injection.semantic_classifier_unavailable`
marker and records a fail-closed entry. `classifier.fail_mode` left unset
follows `ioscan.fail_mode`, so it is `closed` at L5–L6 and `open` below. At
L5–L6 a hive with no model endpoint configured therefore withholds all
untrusted text until one is set, or until the owner sets
`classifier.enabled: false` or `classifier.fail_mode: open`. The boot log
says so in one line.

**What still passes.** The classifier has a budget of 64 calls per scheduler
pass. Text beyond that budget is passed on with a `budget_exhausted` skip in
the audit log, in either fail mode. Review-bot threads are not sent to the
classifier; the rules alone cover them.

**Cost.** One call per untrusted text per pass, cached by content, with an
8,000-character cap per call. At the default model that is on the order of
a few thousand input tokens per call; a hive that sees 200 new or edited
untrusted texts a day spends well under a dollar a day. Pick a cheaper model
with `classifier.model` or turn it off with `classifier.enabled: false` if
that is too much.

## Related checks that do not depend on ioscan

* Prompt and agent-definition sources must be on the repository allowlist;
  unlisted sources are refused.
* If a prompt fetch fails, Hive uses the last cached or built-in template.
* Pull-request claim and trust checks refuse when duplicate work or
  privilege escalation is possible.
