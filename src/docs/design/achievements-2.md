# Achievement system 2.0: teamwork tiers and local always wins

Issue [#8832](https://github.com/hivecommons/hive/issues/8832) asks Hive to use
achievements, dossiers, and leaderboards to reinforce open-source collaboration
instead of letting the people with the most paid inference take every badge. This
document is written as the Spektacular-readable spec record for that work: it
states the user-visible outcomes, the verifiable signals, the guardrails, and
the slices that implementation PRs must follow.

## Status

**Partly shipped target.** Phase 1 is intentionally safe: derive new badges from
signals Hive already stores and expose them in dossiers and contribution
leaderboards. Later phases can add Jam/Spektacular run joins, stronger social
anti-gaming, and operator tuning.

## Problem

Hive already rewards useful activity, but raw contribution volume and model
spend can dominate recognition. The next milestone for achievements and dossiers
is a trust/teamwork rebalance:

- reward speks and people contributing to each other's hives;
- make the best recognition team-heavy, not wallet-heavy;
- make local-model skill a first-class achievement track;
- keep the experience healthy: no addictive loops, no FOMO, no punishment for
  logging off, and no incentives to spam maintainers.

The Bluefin discussion that motivated this design frames Hive as community
inference for open source: contributors donate hardware, paid access, reviews,
and specs; specs pace the work; local-model expertise is valuable because it is
accessible to patient people with consumer hardware; and the most useful future
is people helping each other's hives rather than only maximizing their own
throughput (<https://github.com/ublue-os/bluefin/discussions/4946>).

## Goals

1. Present achievements in four collaboration tiers using generic Hive language:
   **Solo**, **Dual**, **Fireteam**, and **Raid**.
2. Map each tier to concrete, auditable GitHub/Hive signals instead of hidden
   scoring.
3. Add a fully scoped **local-model track** where sustained consumer-hardware
   work is proud and visible.
4. Add a **mastery tier** that requires both paid and local model practice, with
   top rewards gated on local mastery: **local always wins**.
5. Surface the result in dossiers and leaderboards without changing assignment,
   gate, or merge semantics.
6. Make anti-gaming checks explicit: collusion, sock puppets, self-review, and
   repeated low-value actions should not be good strategies.

## Non-goals

- No paid-model shaming. Paid inference remains useful; it simply cannot be the
  only path to top recognition.
- No automatic maintainer trust or permission escalation. Achievements are
  presentation and discovery, not authorization.
- No daily quests, streaks, randomized rewards, limited-time badges, loot boxes,
  or engagement-pressure mechanics.
- No new GitHub credential path in phase 1.
- No requirement that contributors design work around the tier names. The names
  are presentation for players; the work remains normal GitHub, chat, Spek, and
  Hive SDLC.

## Scout pass: dark-pattern guardrails

The design was checked against common game and gamification dark patterns:
variable-ratio rewards, loss aversion, FOMO timers, daily login pressure,
pay-to-win advantages, artificial scarcity, and coercive social pressure.
Sources surveyed:

- Zagal, Björk, and Lewis, “Dark Patterns in the Design of Games,” catalogues
  game mechanics that create unwanted negative experiences by exploiting player
  psychology (<https://doi.org/10.1145/2282338.2282362>).
- Aagaard et al., “A Game of Dark Patterns: Designing Healthy, Highly-Engaging
  Mobile Games,” CHI 2022, discusses dark patterns in mobile games and the need
  for healthier engagement design (<https://doi.org/10.1145/3491101.3519837>).
- Niknejad et al., “Level Up or Game Over: Exploring How Dark Patterns Shape
  Mobile Games,” surveys mobile-game dark patterns including scarcity, timers,
  social pressure, and monetization pressure (<https://arxiv.org/abs/2412.05039>).
- Nyström, “Exploring the Darkness of Gamification: You Want It Darker?”, a
  literature review of negative gamification and persuasive-technology effects
  (<https://www.diva-portal.org/smash/record.jsf?pid=diva2:1518853>).

Hive therefore **never** does the following for achievements:

1. **No variable-ratio rewards.** Badges have deterministic, documented
   thresholds. There are no random drops.
2. **No loss-aversion traps.** Progress does not decay because someone takes a
   weekend, vacation, or caregiving break.
3. **No FOMO timers.** No expiring seasonal badge, daily login, or limited window
   is required for core recognition.
4. **No daily streak pressure.** Sustained work can be measured across days, but
   missed days are not failures and do not reset progress.
5. **No pay-to-win.** Paid models can count toward paid-model practice, but top
   mastery requires local-model mastery too.
6. **No artificial scarcity.** Badges are not capped to the first N people.
7. **No coercive social pressure.** Team badges require natural SDLC evidence;
   they do not require tagging friends, begging for reactions, or forming
   private grinding groups.
8. **No maintainer spam loops.** Review and collaboration signals count only when
   attached to real PRs, specs, runs, or merged outcomes.

Every phase must include an acceptance check that new achievements still obey
this list.

## Tier model

The tiers are cumulative presentation groups. A contributor can earn lower-tier
badges forever; higher tiers ask for broader collaboration and stronger evidence.

### Solo: honing the craft

Solo recognizes reliable individual contribution.

Signals:

- authored PRs opened and merged;
- PR ratio: merged PRs divided by opened PRs, with a minimum denominator before
  display;
- review fixes incorporated into the author's own PRs;
- Spek/spec authored or revised by the contributor;
- local or paid model run attribution from Hive trailers.

Initial badges:

| Badge | Threshold | Evidence |
| --- | --- | --- |
| First Useful Change | 1 merged PR | GitHub PR merged by login/agent attribution |
| Steady Hand | 5 opened PRs and at least 60% merged | PR open/merge counts |
| Spec Starter | 1 Spek/design/spec artifact authored | Spektacular receipt or design/spec file attribution |
| Local First Steps | 1 local-model attributed contribution | PR attribution footer backend/model |

### Dual: two-person trust loops

Dual recognizes reciprocal collaboration between two distinct people.

Signals:

- first review of another contributor's PR;
- another contributor reviews the first contributor's PR;
- non-author review that leads to a merged PR;
- cross-hive contribution where author and reviewer belong to different hives;
- comments or suggestions accepted through Jam/Spektacular when available.

Initial badges:

| Badge | Threshold | Evidence |
| --- | --- | --- |
| First Review Handshake | Review someone else's PR once | GitHub review/comment by non-author |
| Reciprocal Trust | Both people review each other's PRs at least once | Pairwise review edges, author != reviewer |
| Cross-Hive Neighbor | Contribute or review in another hive | Hive/repo ownership from contribution metadata |

### Fireteam: three-person SDLC coverage

Fireteam recognizes the basic triad needed to take a spec from zero to done.
At least three distinct humans/operators must cover planning, implementation,
and review/merge evidence for the same spec or run.

Signals:

- spec author/reviser;
- implementer/PR author;
- reviewer/approver or maintainer who merges;
- Spektacular run key or GitHub issue/PR linkage tying the work together;
- no self-review edge for the counted review role.

Initial badges:

| Badge | Threshold | Evidence |
| --- | --- | --- |
| Full SDLC Fireteam | 3 distinct contributors cover spec, implementation, review | Spek/run/issue/PR join |
| Three-Person Done | A spec-linked PR merges with three-role coverage | Merged PR + run receipt/issue link |
| Fireteam Regular | 3 completed fireteam works | Repeated verified fireteam completions |

### Raid: two fireteams, complex specs, or swarms

Raid recognizes complex collaborative delivery. It is intentionally rarer and
team-heavy.

Signals:

- at least six distinct contributors across a complex spec, a sequence of specs,
  or a swarm;
- multiple PRs linked to the same issue/spec/run sequence;
- swarm player data and achievement events;
- full-SDLC coverage repeated or parallelized across the sequence.

Initial badges:

| Badge | Threshold | Evidence |
| --- | --- | --- |
| Six-Person Swarm | 6 distinct contributors on one issue/spec sequence | Issue/PR/run/swarm join |
| Raid Clear | Complex spec or spec sequence completes with two fireteams | Spek sequence + merged PRs |
| Open Source Sherpa | Help at least 5 distinct people across hives | Cross-person/cross-hive contribution graph |

## Local-model achievement track

Local-model achievements use existing Hive attribution where possible. PR
footers already use the shape `— hive: agent=… backend=… model=…`; backends and
models known to be local runners count as local. Examples include Ollama,
llama.cpp-compatible runners, local OpenAI-compatible endpoints configured as
local, and other provider keys Hive classifies as local. Ambiguous provider data
must be shown as unknown, not guessed.

Thresholds are scoped for a patient contributor with consumer hardware. The
track values persistence and useful outcomes, not raw tokens per second.

| Badge | Threshold | Notes |
| --- | --- | --- |
| Local Spark | 1 accepted local-model contribution | First visible local attribution |
| Two-Day Localist | Local-model contributions on 2 distinct calendar days | Missed days do not reset anything |
| Patient Builder | At least 2 local-model contributions that reach merged PRs or accepted reviews | Useful outcome over volume |
| Local Reviewer | 3 non-author PR reviews using local attribution | Avoids self-review |
| Consumer Cluster | Local work appears on 3 specs/issues/PRs | Breadth without huge hardware |
| Local Steward | 5 merged or accepted outcomes with local attribution | Top local track for phase 1 |

The UI should include explanatory copy: local-model work may be slower, and that
is the point. Two days of thoughtful local work is more valuable to the project
than one purchased burst when it improves specs, reviews, or merged code.

## Mastery tier: local always wins

Mastery requires both local and paid-model practice so contributors with access
to paid inference still have an incentive to learn local workflows.

| Badge | Threshold | Rule |
| --- | --- | --- |
| Hybrid Apprentice | At least 1 local and 1 paid/remote attributed contribution | Shows both modes |
| Hybrid Operator | 3 local outcomes and 3 paid/remote outcomes | Balanced practice |
| Local Always Wins | Local Steward plus Hybrid Operator | Top mastery requires local mastery |
| Hive Mastery | Fireteam Regular plus Local Always Wins | Teamwork + local mastery |

Paid/remote outcomes can use Claude, Copilot, OpenAI, Gemini, or other hosted
backends. Unknown attribution does not satisfy either side.

## Anti-gaming measures

- **Distinct actor requirements.** Dual, Fireteam, and Raid badges require
  distinct contributors. Author, reviewer, spec author, and merger roles are
  deduplicated by durable login/operator identity.
- **No self-review credit.** Reviews by the PR author, their own agent lane, or a
  bot acting on their behalf do not count as review-role evidence.
- **Minimum useful outcome.** Higher-tier credit needs a merged PR, accepted
  review/suggestion, final Spek receipt, or maintainer action. Mere comments do
  not farm badges.
- **Pair caps.** Repeated credit from the same two accounts is capped until the
  pair works with additional people, reducing collusion incentives.
- **Sock-puppet heuristics.** Future phases may flag suspicious clusters sharing
  identical attribution, tokens, or timing; the first slice only avoids creating
  a reward that would benefit from sock-puppet volume.
- **Maintainer override.** Dossier badges are computed evidence, but maintainers
  can hide or annotate a badge if abuse is confirmed.
- **Transparent thresholds.** Since thresholds are public, abuse review focuses
  on evidence quality rather than hidden scoring.

## Dossier and leaderboard changes

Dossiers should show:

- a compact tier summary: Solo, Dual, Fireteam, Raid;
- a local-model panel with local badges and the “local always wins” mastery
  state;
- evidence snippets for each badge: PR, review, issue, run, or spec key;
- unknown attribution as unknown, never as local or paid;
- guardrail copy stating that badges never decay and are not authorization.

Leaderboards should:

- group or filter achievements by tier;
- prefer team-heavy achievements for top recognition;
- expose local-model and mastery counts separately from raw contribution volume;
- keep existing leaderboards usable for operators who only want current metrics.

## Data sources

Phase 1 must use existing data only:

- GitHub PR, review, author, merge, and issue linkage data already collected for
  dashboard contribution views;
- PR attribution trailers parsed by `src/pkg/github/attribution.go`, especially
  `agent`, `backend`, and `model` from `— hive: agent=… backend=… model=…`;
- existing dashboard achievement/dossier/leaderboard records;
- swarm player and achievement data from the existing swarm dashboard;
- Spektacular run/campaign data already projected under `/api/runs` and
  `/api/campaigns` when present.

Later phases may add durable Spek role receipts, Jam accepted-suggestion events,
and cross-hive identity joins, but they are not required for the first slice.

## Phased rollout

### Phase 0: design

Land this document and index it from `src/docs/design/README.md`.

Acceptance criteria:

- goals, non-goals, tier model, local track, mastery, data sources,
  anti-gaming, rollout, and dark-pattern guardrails are explicit;
- sources for dark-pattern scout pass are cited;
- implementation can be cut into phase 1 without inventing new stores.

### Phase 1: computed badges from existing data

Implement Solo, Dual, Fireteam/Raid placeholders where evidence exists, the
local-model track, and mastery badges in the existing dashboard achievement
system. Surface them in dossiers and the leaderboard UI.

Acceptance criteria:

- no new database or credential path;
- badge thresholds are named constants;
- local-vs-paid detection uses existing attribution parsing and fails unknown;
- tests cover local, paid, hybrid mastery, self-review exclusion, and tier
  grouping;
- changelog fragment announces the user-visible achievement additions;
- guardrail review confirms no streaks, timers, random rewards, or pay-to-win
  rewards were added.

### Phase 2: Spek/Jam role evidence

Join final Spektacular receipts and accepted Jam suggestions to Fireteam/Raid
roles so a spec can prove planning, implementation, and review coverage without
manual inference.

Acceptance criteria:

- Fireteam badge requires three distinct roles on one spec/run;
- Raid badge can follow a sequence of specs or a swarm;
- dossiers show the spec/run evidence chain.

### Phase 3: cross-hive and abuse review

Add cross-hive identity joins, pair caps, abuse annotations, and operator-tunable
thresholds after real tester feedback.

Acceptance criteria:

- maintainers can explain or hide abused badges;
- cross-hive badges avoid rewarding sock-puppet repos;
- no guardrail is weakened.

## Open questions

- Which local backend names should be canonical beyond existing provider keys?
- Should cross-hive achievements wait for organization-level identity mapping?
- Should the Raid tier require maintainer confirmation for the first release?
