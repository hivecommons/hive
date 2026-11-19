# Reviewer-stated rules from review feedback

When a person reviews a PR one of the hive's agents opened, most comments are corrections: they apply to that PR and the agent fixes it
([#19027](https://github.com/hivecommons/hive/issues/19027)). Some comments state a rule or a source of truth meant to hold for later work,
for example "`.project/maintainers.yaml` is the source of truth for ownership and maintainership; capture this in the assessment knowledge
base". A fix does not help the next analysis. The review-feedback sweep therefore captures such rules itself, deterministically, instead of
leaving it to the agent's discretion ([#19046](https://github.com/hivecommons/hive/issues/19046)).

## What happens

1. **Classify.** Each human comment on the PR is matched against phrase signals. A comment with at least one is a rule; the rest are
   corrections and are handled as before. The built-in signals are `source-of-truth`, `authoritative`, `always` ("should always", "must
   always"), `never` ("should never", "must never"), `going-forward` ("going forward", "moving forward", "from now on", "in future", "next
   time", "future assessments"), `capture-request` ("capture this", "document this", "record it"), `assessment-criteria` ("assessment
   criteria", "assessment knowledge") and `knowledge-base`. The owner can switch built-in signals off and add plain phrases of their own (see
   [Settings](#settings)). Block quotes (`>` lines), fenced code and questions never count: a reply that quotes the hive and asks "can the
   hive add this to the knowledge base?" is not a rule ([#19135](https://github.com/hivecommons/hive/issues/19135)). Comment text is data:
   it is matched and quoted, never executed, fetched or followed.
2. **Extract.** The rule text is the sentences that carry a signal, without clauses that only ask for the capture, bounded to 600 characters.
   A short rule sentence that refers back ("we should consider this in future") is recorded with the rest of the comment, so the rule keeps
   what "this" is.
3. **Gate on trust.** A rule changes what every later run is told, so only reviewers whose GitHub association is in
   `review_feedback.rules.trusted_associations` (env `HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS`, default `OWNER,MEMBER,COLLABORATOR`)
   are captured at once. On a repo of an organisation the hive is not part of, an org member whose membership is private shows as
   `CONTRIBUTOR`, so their rules are queued unless the owner trusts `CONTRIBUTOR`. Any other reviewer's rule is
   queued as a draft knowledge entry that a person approves. A trusted reviewer restating a queued rule activates it.
4. **Persist.** The rule gets an id (`R-1`, `R-2`, ...) and is written to the `review-rules` local knowledge channel as an entry tagged `pin`,
   with provenance: the PR, the comment id and URL (the entry's `source`), the reviewer, their association, the time, and whether the hive or
   the agent wrote the text. The same comment id is never captured twice, across restarts too. A restatement with near-identical text and the
   same scope (token overlap at or above `HIVE_REVIEW_FEEDBACK_RULE_MERGE_THRESHOLD`, default 0.8) merges into the existing rule and adds its
   comment to the provenance. At most `HIVE_REVIEW_FEEDBACK_RULES_MAX` (default 200) rules are live; past that, capture is refused and logged.
5. **Say so.** The sweep posts one reply on the PR: "Recorded as rule R-n in the hive's knowledge, applies to ...", with the recorded text, the
   source comment and how to retract it, so the reviewer sees it was captured and can object. The agent's brief lists the recorded rules, tells
   it not to record them again, and asks it to name the ids in its reply.
6. **Load.** The kick primer carries every approved `pin` entry whose scope admits the kick, whatever the kick's keywords are, ahead of ranked
   knowledge and exempt from the max-facts trim (at most 20 pinned entries per kick). This holds for a kick with no issues or no extractable
   keywords too: it gets its pinned rules and nothing else, since ranked knowledge still needs keywords. A rule applies to the authoring agent's lane by default,
   so the quality agent receives its rules on its next kick; a rule scoped to a workflow reaches the lane or workflow model of that name. The
   usual lifecycle filter and `knowledge.agent_scopes` still apply, so drafts, retracted rules and out-of-scope agents never see a rule.

Capture reads its own candidate set, not the routed burst: every human comment on the hive's open PRs in the attribution window (inline,
review body, conversation), whether or not it was already handled, sits in a resolved thread, or addresses the hive. A comment is read once
it has been quiet for the sweep's debounce. The rule index keeps the ids it has evaluated, separately from the sweep's handled ids, so the
first pass after an upgrade is a one-time backfill over the comments still visible on those PRs and later passes read only new comments. So
capture also happens when the per-PR cap was reached, no agent could take the kick, or no kick is due at all. It does not change the sweep's
debounce, cap, routing or thread behavior.

## The agent's structured block

The kicked agent may end its reply with one hidden line to sharpen a rule, or to state one the classifier missed:

```text
<!-- hive:review-rules {"rules":[{"text":"...","scope":"agent","applies_to":"quality","evidence_url":"https://github.com/o/r/pull/1#discussion_r1"}]} -->
```

`scope` is `agent` (default, the authoring agent), `workflow` (`applies_to` names it), `repo` (`applies_to` is `owner/name`) or `global`.
Every entry is validated field by field: no unknown fields, bounded text, a GitHub PR or issue URL as `evidence_url`, a restricted
`applies_to`, at most 10 rules per reply. `evidence_url` must name a review comment the hive saw: the reviewer's trust comes from that
comment, never from the block, so an agent cannot raise a rule's authority. A reply is applied once. If a refined rule becomes a near
duplicate of another, the two merge and the older id survives. A `repo` rule is scoped to its lane and tagged `repo:<owner/name>`; it is
not pinned, so it reaches kicks by relevance.

## Rules that belong in someone else's repo

The hive does not write to repos it does not manage. When a rule's proper home is the reviewed project's own files (the reviewer asks for it
"in your docs" or "in your repo" on a repo the hive does not manage, or the agent's block gives a `repo` scope for such a repo), the hive still
records it in its own knowledge and posts exactly one needs-human note per rule on the PR. The note carries the exact proposed text and the
source comment, so a maintainer can place it. It is posted once and retried on a later pass if posting failed.

## Seeing and retracting rules

Rules are ordinary knowledge entries in the `review-rules` channel (`/data/vaults/review-rules`, override with
`HIVE_REVIEW_FEEDBACK_RULES_VAULT`), so the dashboard's knowledge list shows each rule with its source and the provenance lines in its body.
The rule index lives in the sweep's state directory (`HIVE_REVIEW_FEEDBACK_DIR`, file `rules.json`). At boot the channel is created and
connected when it or the vaults directory exists. On a host without a vaults directory the sweep creates the channel on its first capture;
the primer is registered with its path at boot, opens it on the first kick after it appears and connects it to the knowledge API then, so
neither kicks nor the dashboard wait for a restart. After a change, the primer reads the channel within the vault's usual refresh interval.

- Retract a rule: `hivectl knowledge state rule-r-1 deprecated`. Deleting the entry file does the same. The sweep reconciles on its next pass
  (every five minutes) and records the change in its log.
- Confirm a queued rule: `hivectl knowledge state rule-r-2 approved`.
- Capture log lines: `review feedback: reviewer-stated rule recorded in the hive's knowledge`, with the rule, reviewer, association, scope and
  the signals that matched.

## Settings

Which phrases count and whose rules are recorded at once are the owner's preferences, per hive. Edit them in **Settings, Features, Review,
Reviewer-stated rules** (owner only; applies on the sweep's next pass without a restart; audited as `config_review_feedback_rules`), or in
hive.yaml. A value saved there wins over its env var; an unsaved one keeps the env var, then the default. Changing a preference applies to
comments read from then on; a comment already read is not read again.

| hive.yaml | Env var | Default | Meaning |
|---|---|---|---|
| `review_feedback.rules.enabled` | `HIVE_REVIEW_FEEDBACK_RULES` | on | Off stops rule capture; the sweep keeps running and recorded rules keep reaching agents until retracted. With the env var off at boot, a rule captured after switching on in Settings reaches kicks after the next restart. |
| `review_feedback.rules.trusted_associations` | `HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS` | `OWNER,MEMBER,COLLABORATOR` | Associations whose rules are captured at once; others are queued. |
| `review_feedback.rules.extra_phrases` | `HIVE_REVIEW_FEEDBACK_RULE_EXTRA_PHRASES` (comma-separated) | none | Plain phrases (3 to 80 characters, at most 20) that also mark a rule, matched case-insensitively as whole words, for example "worth considering". |
| `review_feedback.rules.disabled_signals` | `HIVE_REVIEW_FEEDBACK_RULE_DISABLED_SIGNALS` (comma-separated) | none | Built-in signal names that no longer mark a rule. |

These stay env-only:

| Variable | Default | Meaning |
|---|---|---|
| `HIVE_REVIEW_FEEDBACK_RULES_MAX` | `200` | Bound on live (active or queued) rules. |
| `HIVE_REVIEW_FEEDBACK_RULE_MERGE_THRESHOLD` | `0.8` | Text similarity at which same-scope rules merge. |
| `HIVE_REVIEW_FEEDBACK_RULES_VAULT` | `/data/vaults/review-rules` | Knowledge channel the rules are written to. |
| `HIVE_REVIEW_FEEDBACK_DIR` | `/data/review-feedback` | State directory shared with the sweep. |

Implementation: `src/pkg/reviewrules/`, `src/cmd/hive/review_feedback_wire.go`, and the pinned-entry support in `src/pkg/knowledge/primer_pinned.go`.
