# ioscan: scanning untrusted text before agents read it

Issue titles, bodies, labels, author names and comments are written by
anyone on GitHub. `ioscan` scans that text before Hive builds it into an
agent's task, and checks what agents send out for planted secrets. It is on
by default.

## What it does

* **Cleans and matches.** Removes invisible Unicode characters and
  look-alike letters, decodes base64 blocks, then matches the text against
  rules for known prompt-injection phrasing, dangerous instructions and
  secret-shaped strings. These rules always run when `ioscan` is enabled.
* **Withholds matched text.** Text that trips a rule is replaced with
  `[ioscan: content withheld — ...]`. The original never reaches the agent.
* **Plants canaries.** Each task gets its own marker string. Hive scans
  everything the agent sends out for that marker, including copies that are
  base64-, hex- or percent-encoded, reversed, re-cased, split across
  separators or written with look-alike characters. A hit writes an
  `ioscan_canary_leak` audit entry and, in `closed` mode, blocks the request.
  Encodings outside that list are not caught. Git push bodies are not
  inspected; in `closed` mode they are refused.
* **Optional classifier.** A language model can score already-cleaned text
  for injections written in plain English. It is off by default. When it
  errors or times out, the deterministic rules still apply.

## What it does not do

Rules match specific phrasings. A measured test against 43 attack payloads
withheld 42% and let 58% reach the agent; paraphrased instructions, split
payloads and most encodings got through. Details and numbers:
[ioscan-red-team.md](ioscan-red-team.md). The control that stops a persuaded
agent from merging code is the network proxy, which denies pull-request
creation and merge calls for every agent, not `ioscan`.

## Configuration

```yaml
ioscan:
  enabled: true          # default true
  fail_mode: open        # default: open at ACMM L1–L4, closed at L5–L6
  canaries: true         # default true
  classifier:
    enabled: false       # default false
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

## Related checks that do not depend on ioscan

* Prompt and agent-definition sources must be on the repository allowlist;
  unlisted sources are refused.
* If a prompt fetch fails, Hive uses the last cached or built-in template.
* Pull-request claim and trust checks refuse when duplicate work or
  privilege escalation is possible.
