package delegatedecision

// Real parked issues the owner judged by hand (hivecommons/hive: "Let hive
// decide" should decide the way he would). Bodies are trimmed to the parts
// the classifiers read; titles, labels and authors are verbatim.

var fixtureHive15021 = Input{
	Kind: KindIssue, Author: "app/hivecommons-hive",
	Title:  `[selfreport] agent_login_required: login required detected`,
	Labels: []string{"help wanted", "triage/accepted", "approved-direction"},
	Body: `<!-- hive-fleet-fingerprint:929efbb7d686368f1108570c -->
<!-- hive-fleet-instance:c1a14572726a1483 -->
<!-- hive:selfreport fp=929efbb7d686368f1108570c hive=hosted-ibm-alchemy-logg-ptoo -->

A hive hit an unusual runtime error (class ` + "`" + `agent_login_required` + "`" + `) and is reporting it so the same defect across the fleet lands in one issue.

## Evidence

| Field | Value |
|---|---|
| hive | hosted-ibm-alchemy-logg-ptoo |
| hive name | — |
| spoke | [hosted-ibm-alchemy-logg-ptoo](https://hosted-ibm-alchemy-logg-ptoo.apps.fmaas-vllm-d.fmaas.res.ibm.com) |
| anonymous instance | c1a14572726a1483 |
| owner(s) | @clubanderson |
| hive version | 5.186.4-673-g4d1642c76 |
| hive commit | 4d1642c |
| cluster | vllm-d |
| mode | quiet |
| component | agent-runtime |
| scope | login required detected |
| message | login required detected |
| count + window | 5 in 10m0s |
| first seen | 2026-10-28T17:22:09Z |
| reported at | 2026-10-28T17:27:14Z |
| repos | — |
| owners | — |
| repo | — |
| ref | — |
| self-recovered | no |

<!-- hive:selfreport-affected -->
**Affected hives (1):**
- hosted-ibm-alchemy-logg-ptoo (instance c1a14572726a1483) — [open](https://hosted-ibm-alchemy-logg-ptoo.apps.fmaas-vllm-d.fmaas.res.ibm.com) — owner(s): @clubanderson — seen 1, last 2026-10-28T17:27:14Z
seen_count: 1
<!-- /hive:selfreport-affected -->

<details><summary>Diagnostics (scrubbed)</summary>

` + "`" + `` + "`" + `` + "`" + `
WARN agent=quality pattern=(?i)Please run /login sightings=2
` + "`" + `` + "`" + `` + "`" + `
</details>`,
}

var fixtureHive15329 = Input{
	Kind: KindIssue, Author: "hanthor",
	Title:  `📝 RFC: admission-side #5117 — hold unacknowledged hive-filed proposals before any kick`,
	Labels: []string{"enhancement", "help wanted", "architecture discussion", "kind/design", "stage/045-needs-decision"},
	Body: `## Use case

A hive's planning lanes (architect, strategist, outreach) file issues that propose a direction. Today those issues enter the actionable set like any other, so an implementer lane can pick one up and open a PR the same day. #5117 catches this, but only on the PR side: the hold lands after the run is spent, and the PR then sits in the queue until a human closes it.

I'd like an opt-in admission gate: a hive-filed issue that is a *proposal*, and that no human has acknowledged, does not become actionable. Everything else is admitted exactly as today.

## Evidence (tuna-os/tunaOS, 2026-10-29)

- 218 open issues; the hive filed 171 of them. 127 are ` + "`" + `[architect]` + "`" + `/` + "`" + `[strategist]` + "`" + `/` + "`" + `[outreach]` + "`" + ` proposals, and **none** carried ` + "`" + `approved-direction` + "`" + `.
- 130 open PRs, 101 of them opened in the last 7 days. About 15 are ` + "`" + `docs(outreach)` + "`" + `/` + "`" + `docs(metrics)` + "`" + ` PRs that implement those proposals; nobody asked for them.
- ` + "`" + `issue_filter.reporter_trust` + "`" + ` (#9665) can't express this. It judges human reporters only, and hands hive-filed issues to #5117 by design (` + "`" + `fetchIssues` + "`" + ` comment). The App is reported as ` + "`" + `CONTRIBUTOR` + "`" + ` on its own issues, so turning reporter trust on would latch all 171 hive-filed issues, including scanner bug reports that should keep flowing.

## Proposal

` + "`" + `` + "`" + `` + "`" + `yaml
project:
  issue_filter:
    agent_proposal:
      labels: [agent/architect, agent/strategist, agent/outreach]
      title_prefixes: ['[architect]', '[strategist]', '[outreach]']
` + "`" + `` + "`" + `` + "`" + ``,
}

var fixtureHive15335 = Input{
	Kind: KindIssue, Author: "app/hivecommons-hive",
	Title:  `[quality] Gate config: opt hivecommons/hive into auto_merge.require_current_base — stale-green merge of #15236 broke v5 for 20 min`,
	Labels: []string{"help wanted", "testing", "agent/quality", "approved-direction", "stage/010-actionable"},
	Body: `## Finding

` + "`" + `v5` + "`" + ` was uncompilable in ` + "`" + `pkg/dashboard` + "`" + ` tests from 05:25Z to 05:46Z on 2026-10-29 (incident #15322). The cause was a stale-green merge, which the existing gate ` + "`" + `auto_merge.require_current_base` + "`" + ` (#13634) exists to prevent — but the hive that merges into ` + "`" + `hivecommons/hive` + "`" + ` does not have this repository listed, so the gate never ran:

- #15267 (merged 03:30:40Z) added the ` + "`" + `open` + "`" + ` argument to ` + "`" + `stageTrackerResolveLinkedPRs` + "`" + `.
- #15236's last CI run finished **02:15Z** on head ` + "`" + `56331a1b` + "`" + ` (committed 02:02Z), against a base that pre-dated #15267.
- #15236 was merged by ` + "`" + `hivecommons-hive[bot]` + "`" + ` at **05:25:23Z** with no re-run on a current merge base. Its new test still used the one-argument call, so every ` + "`" + `test (dashboard …)` + "`" + ` shard, ` + "`" + `test` + "`" + ` and ` + "`" + `coverage` + "`" + ` went red on ` + "`" + `v5` + "`" + ` until #15301 (2f05d8e) landed the fix at 05:46Z.

Blast radius of the 20-minute window: #15314, #15315, #15316, #15317, #15300, #15304 ran red against the broken base; #15321 and #15324 were opened as redundant fixes; one shared-CI incident was filed and closed. All of that was avoidable: with the gate on, #15236 would have been updated onto ` + "`" + `v5` + "`" + ` and re-tested, and the semantic conflict would have turned **that one PR** red instead of the base.

Evidence: ` + "`" + `gh api repos/hivecommons/hive/pulls/15236` + "`" + ` (` + "`" + `merged_at` + "`" + `, ` + "`" + `merged_by` + "`" + `), ` + "`" + `gh api repos/hivecommons/hive/commits/56331a1b.../check-runs` + "`" + ` (latest dashboard shard ` + "`" + `completed_at` + "`" + ` 02:15:24Z), ` + "`" + `gh api repos/hivecommons/hive/pulls/15267` + "`" + ` (` + "`" + `merged_at` + "`" + ` 03:30:40Z); ` + "`" + `src/pkg/github/automerge/automerge_current_base.go:27-33` + "`" + `, ` + "`" + `src/docs/operator-reference.md:432` + "`" + ` (default ` + "`" + `unset (off)` + "`" + `).

## Recommendation

Opt this repository into the gate in the operator config of the hive that merges here:

` + "`" + `` + "`" + `` + "`" + `yaml
auto_merge:
  require_current_base:
    - hivecommons/hive
` + "`" + `` + "`" + `` + "`" + `

Cost is one compare call per merge candidate; a behind head gets one GitHub update-branch merge commit and merges once CI is green on it (no author commit rewritten). #15308 (native merge queues) would also close this class once it lands, but it is still in review and this is a one-line config change available today.

Gate check: this is filed against the gate's configuration, not as a new finding class — the gate exists and is wired (` + "`" + `src/cmd/hive/main.go:2572` + "`" + `, ` + "`" + `:9434` + "`" + `); it is simply off for this repo.

Risk: governance — this changes merge behaviour for every lane on the primary repo, and the config lives in the operator's deployment, which the hive cannot edit.

## Priority
- Impact: high (base-breaking merges stall every open PR and spawn duplicate fix PRs)`,
}

var fixtureRationguard418 = Input{
	Kind: KindIssue, Author: "app/hivecommons-hive",
	Title:  `[scanner] Gate proposal: ECMA-48 conformance test for stripTerminalEscapes (3rd recogniser gap: 8-bit ST U+009C over-strips)`,
	Labels: []string{"help wanted", "agent/scanner", "approved-direction", "stage/010-actionable"},
	Body: `## Summary

Three ` + "`" + `stripTerminalEscapes` + "`" + ` recogniser gaps have now been found one at a time in ` + "`" + `src/sanitize.ts` + "`" + `, each fixed by adding one more regex and one more pinned assertion:

1. #414 — Fs/Fp two-byte finals (` + "`" + `ESC c` + "`" + `, ` + "`" + `ESC 7` + "`" + `, ` + "`" + `ESC 8` + "`" + `) leaked past the two-byte rule (fixed in #415).
2. #416 — nF intermediate-byte sequences (` + "`" + `ESC # 8` + "`" + `, ` + "`" + `ESC % G` + "`" + `) leaked their payload (fixed in #417).
3. **New, third instance (not filed separately per the three-of-a-class rule):** the string terminator recognised by ` + "`" + `replaceControlStrings` + "`" + ` is only BEL or 7-bit ST (` + "`" + `ESC \` + "`" + `). The 8-bit ST **U+009C** that ` + "`" + `stringifyForTerminal` + "`" + `'s docblock already acknowledges emulators honour is not a terminator. An OSC/APC/PM closed by U+009C is therefore treated as *unterminated*, and the span runs to the **next** BEL / ` + "`" + `ESC \` + "`" + ` anywhere later in the buffer — swallowing legitimate text in between.

   Reproduction against the current ` + "`" + `replaceControlStrings` + "`" + ` logic (10e3012):`,
}

var fixtureSite296 = Input{
	Kind: KindIssue, Author: "app/hivecommons-hive",
	Title:  `[quality] Gate proposal: shell-checker arms that survive mutation (mutation harness for scripts/*.sh)`,
	Labels: []string{"testing", "agent/quality"},
	Body: `## Finding

` + "`" + `scripts/*.sh` + "`" + ` (check-links.sh, check-redirects.sh, make-redirects.sh) are the only gates in this repository without a "branch runs but is never asserted" gate. The JS gates got one in #254 (branch-coverage floor, ` + "`" + `scripts/coverage-floor.test.mjs` + "`" + `); the shell checkers are outside it, and the only thing that exercises them in CI is their own self-test (` + "`" + `links.yml` + "`" + ` test job).

This is now the third time a freshly-landed shell-checker change shipped with arms that no self-test assertion distinguishes from a broken one:

1. #248 — first instance (check-links arm unasserted).
2. #257 — 4 arms survived hand-written mutants at f930ed0 (check-links 410 handling, two ` + "`" + `SOFT_404_RE` + "`" + ` alternatives, fonts preconnect skip; make-redirects ` + "`" + `html_escape` + "`" + ` of ` + "`" + `< > "` + "`" + `). Self-tests stayed 106/28 green under every mutant.
3. #295 @0dada63 — ` + "`" + `has_id` + "`" + ` landed with three arms the new self-test case does not pin. Each mutant below keeps ` + "`" + `check-links.test.sh` + "`" + ` at 119/119:
   - escape only ` + "`" + `.` + "`" + ` instead of every ERE metacharacter → ` + "`" + `#a+b` + "`" + ` is satisfied by ` + "`" + `id="aab"` + "`" + `, ` + "`" + `#q(1)` + "`" + ` by ` + "`" + `id="q1"` + "`" + ` (false pass);
   - drop the ` + "`" + `([^>]*[[:space:]])?` + "`" + ` group → ` + "`" + `<a class="x" href="#top" name="with-attrs">` + "`" + ` is reported missing (false failure on a legitimate named anchor);
   - use the unescaped id in the ` + "`" + `name=` + "`" + ` arm → ` + "`" + `#n.m` + "`" + ` is satisfied by ` + "`" + `<a name="nzm">` + "`" + `.

   A PR pinning these three arms is open separately; it does not change the fact that nothing stops the fourth instance.

Per the quality lane's three-of-a-class rule I am filing the missing gate instead of a third per-site finding.

## Recommendation`,
}
