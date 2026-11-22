# Prompt-injection defences: Hive compared with trentina

Last reviewed: 2026-11-21

[trentina](https://github.com/crunchtools/trentina) is an MCP and HTTP
gateway that sits between agents and everything they touch. This page lists
its prompt-injection defences, says which ones Hive already has, and names
the gaps worth closing. It came out of
[hivecommons/hive#19433](https://github.com/hivecommons/hive/issues/19433).

Hive's own controls are described in [security-model.md](security-model.md)
and [ioscan.md](ioscan.md). This page does not change them.

## Trentina's stance in one line

Scan once at every place text comes in, never edit text after judging it,
and make the policy deterministic so a persuaded agent cannot argue past it.

## Strategy by strategy

| Trentina strategy | What it does | Hive today |
|---|---|---|
| Layer 1: deterministic rules | Hidden HTML and CSS, invisible Unicode, encoded payloads, exfil URLs, fake model delimiters, directive phrases | Mostly covered. `ioscan` strips invisible and look-alike characters, peels base64, hex, entity, percent and rot13 layers, and tells mention from use. No rules for fake delimiters or exfil URLs. |
| Layer 2: local classifier | A small ONNX model scores text offline | Partly covered. Hive's classifier is a hosted model, not a local one. |
| Layer 3: quarantined judge | A tool-less model answers in a closed set of labels, so none of its prose reaches the agent | Covered in spirit. The `ioscan` classifier runs without tools and returns a score. |
| Layers share structure, not text | Findings pass between layers as counts, labels and scores | Covered. Withheld text is replaced by a notice naming the rules that fired. |
| Scan every ingress | Tool results, tool definitions, chat sync, model requests and replies, outbound HTTP | Gap. `ioscan` scans issue bodies, comments, PR bodies and reviews only. See [Scan coverage](ioscan.md#scan-coverage). |
| Tool pinning | Hash each MCP tool definition; hold a new or changed one until an operator approves | Gap. Hive does not pin MCP tool definitions. |
| Forgery detection | Text that fakes the gateway's verdicts, tool-call markup, or lines aimed at "the AI" | Gap. Canaries catch planted markers only. |
| Literal and semantic guards | Operator rules on call arguments and results, plus a judge that checks a call against a plain-English rule | Covered by containment. The policy proxy, repository allowlist, denied GitHub MCP writes and the push broker hold whatever the agent believes. |
| Egress controls | Fetches pinned to resolved public addresses, file reads confined, credential shapes caught outbound | Mostly covered. Forced egress, the credential-free sandbox and placeholder keys. No exfil-URL check on agent-written text. |
| Persistent blocklist | A refused source stays refused for a set time | Gap. An author whose text was withheld is not remembered. |
| Operator-only allowlist | The agent cannot widen trust; each call has a floor mode | Covered. Trust tiers and the repository allowlist are operator-set. |
| Measured corpora | Attack and benign corpora gate every image build | Partly covered. [ioscan-red-team.md](ioscan-red-team.md) measures attacks, but the benign set is small (twelve cases) and does not gate releases. |
| Honest known gaps | States openly that a model can launder an injection into clean prose | Partly covered. [security-model.md](security-model.md#11-what-hive-does-not-do-yet) lists gaps; the paraphrase limit now sits there too. |

Not relevant to Hive: token minification, an OAuth server, the Matrix
bridge, federation and stdio fronting.

## Gaps worth closing

Each item is small enough for its own issue.

1. **Scan tool output.** Web fetches, MCP results and files in cloned
   repositories reach the agent without `ioscan`. The agent CLI reads them
   itself, so Hive needs a hook or proxy point to scan them. `GateInput` and
   `GateOutput` in `src/pkg/ioscan/hook.go` still carry
   `TODO(ioscan-integration)` notes and have no callers.
2. **Spoofed delimiters and forged notices.** Add an `ioscan` rule family for
   fake chat-template tokens (`<|im_start|>`, `[INST]`), forged
   `[ioscan: ...]` notices, fake tool-call markup and lines addressed to the
   AI. Add red-team cases for each.
3. **Exfil URLs.** Add a rule for Markdown images or links whose query string
   carries data or a placeholder. Run it on input and on agent output.
4. **Benign corpus gate.** Grow the benign set next to the red-team corpus
   and fail a release when the false-positive rate rises.
5. **MCP tool pinning.** Hash every tool definition an agent's MCP servers
   offer. Hold a new or changed one until an operator approves it. This
   needs a design issue first.
6. **Deny memory.** Optionally refuse, for a short time, more text from a
   source whose text was withheld at high or critical.

## What scanning cannot fix

A paraphrased injection that avoids every rule and fools the classifier
still reaches the agent. Trentina states the same limit. The answer is
containment: the proxy, the token tiers and the withheld merge permission
stop a persuaded agent from doing damage.
