---
name: jev-decide
description: Offload quick yes/no, pick-one and rubric-score judgments to Jev, a cheap confidence-scored typed-decision model, instead of reasoning them out yourself. Use for duplicate checks, "which of these N is relevant", safety/secret-touch checks, rubric scoring and gating low-value actions. Never for generating code or prose.
---

# jev_decide — typed decisions from Jev

This hive has enabled **Jev** (TypeSafe AI's typed-decision model) for you.
It answers ONE narrow question at a time with a typed answer and a calibrated
probability, for a tiny fraction of the tokens you would spend deliberating.
Every call is proxied, budgeted and audited by the hive; you never hold a key.

## When to use it

Use `hive jev decide` instead of reasoning at length when the judgment is
small, well-posed and has a closed answer set:

- **Duplicate check** — "Is issue B a duplicate of issue A?" (`choice`: yes/no,
  or `probability`)
- **Relevance pick** — "Which of these N files/issues is the one this task is
  about?" (`choice` over the candidates)
- **Safety gate** — "Does this diff touch secrets, credentials or CI
  permissions?" / "Is this PR safe to merge given its checks?" (`choice`)
- **Rubric score** — "How actionable is this review comment?" against ordered
  levels you write (`score`: position 0…N-1 along your levels)
- **Low-value action gate** — "Is posting this comment worth it?" (`probability`:
  P(yes), 0–1)

Do **not** use it to write code, prose, commit messages or reviews. It decides;
it does not generate. Do not send it whole repositories: pass a compact `state`
with only the facts the decision needs. Ask one narrow thing per call; if a
judgment weighs several factors, ask one call per factor and combine in your
own logic.

Treat the answer as advice. Below ~0.7 confidence, fall back to your own
judgment or gather more state and ask again.

## How to call it

```bash
# pick one (option descriptions are optional but sharpen the answer)
hive jev decide --type choice \
  --question "Which issue is this PR resolving?" \
  --option "123=proxy connection leak" --option "456=dashboard tooltip clipping" --option none \
  --state '{"pr_title":"fix: proxy leak","pr_body":"..."}'

# yes / no as a choice
hive jev decide --type choice --question "Does this diff modify secrets, credentials or CI permissions?" \
  --option yes --option no --state-file /tmp/diff-summary.json

# score against an ordered rubric (2-10 levels, lowest first)
git diff --stat | jq -Rs '{diff_stat: .}' | hive jev decide --type score --state-stdin \
  --question "How risky is this change to merge without a human review?" \
  --level "docs or test-only" --level "isolated code change" --level "cross-cutting or security-sensitive"

# probability that a statement is true
hive jev decide --type probability --question "This comment adds information the author does not already have." \
  --state '{"comment":"...","thread":["..."]}'
```

Output is one JSON object on stdout:

```json
{"answer":"123","confidence":0.93,"probabilities":{"123":0.93,"456":0.05,"none":0.02},"model":"typesafe/jev-1.13","input_tokens":412}
```

- `choice`: `answer` is the option; `probabilities` per option; `confidence` from Jev.
- `score`: `answer` is the position along your levels (may fall between two);
  `probabilities` per level number; `legend` maps numbers back to your text.
- `probability`: `answer` is P(yes); `probabilities` is `{"yes","no"}`;
  `confidence` is |2·P(yes) − 1| (0 at a coin flip, 1 when certain).

Exit code 0 means an answer was returned; 1 means the hive refused or the
provider failed (the reason is on stderr) — carry on without Jev in that case;
2 means you called it wrong.

Limits: question ≤ 4 KiB, state ≤ 64 KiB, 2–32 options of ≤ 256 bytes, 2–10 levels.
