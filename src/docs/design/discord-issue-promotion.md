# Discord reaction-consensus issue promotion

Status: Proposed — discussion on #6239.

RFC credit: this design turns the adopter RFC in
[#6239](https://github.com/hivecommons/hive/issues/6239) into a reviewable plan
without implementing it. The core insight from the RFC is that Discord consensus
should not create a new Hive work queue. It should only apply the same approval
label that Hive already knows how to honor.

Every code reference below was checked against `v4` while writing this page.
Line numbers may drift; the named symbols are the stable handles.

---

## Problem

A project may want its community to decide which linked GitHub issues are ready
for Hive automation. Hive already has the downstream gate: `IssueFilterConfig`
(`src/pkg/config/issue_filter.go:38`) carries `RequireLabels`, and its comment
states that open issues are eligible only when they carry at least one required
label (`src/pkg/config/issue_filter.go:5`). That means an operator can configure
`project.issue_filter.require_labels: [community-approved]` and let the normal
issue enumeration, duplicate-claim guard, actionable issue accounting, and agent
assignment paths continue unchanged.

The missing piece is the label write. Today the in-repo Discord bot is
operator-command oriented: `bot.js` constructs a client with `Guilds`,
`GuildMessages`, and `MessageContent` intents only (`discord/bot.js:20`), and it
registers `ClientReady` plus `MessageCreate` handlers, not reaction handlers
(`discord/bot.js:72`, `discord/bot.js:86`). It talks to the dashboard via
`DashboardBridge`, which connects to `/api/events` and command endpoints
(`discord/lib/dashboard-bridge.js:32`), and the bot README explicitly says it
talks to Hive's dashboard API and does not need cluster access
(`discord/README.md:1`). Its config is environment-first with a `discord:` block
fallback in `hive-project.yaml` (`discord/lib/config.js:6`,
`discord/lib/config.js:18`).

Labeling a GitHub issue is a privileged write. Hive already has GitHub App
credentials and label-writing code: `QueuePRAutoMerge` audits a mediated action
with `recordCreationAudit` and then calls `Issues.AddLabelsToIssue`
(`src/pkg/github/client.go:1085`, `src/pkg/github/client.go:1088`). The reusable
`AddLabels` wrapper also calls `Issues.AddLabelsToIssue`
(`src/pkg/github/client.go:1116`). `recordCreationAudit` is the existing audit
convention for mediated creations and label-adjacent actions
(`src/pkg/github/attribution.go:366`).

## Proposed design

Recommend **Option B** from the RFC: the Discord bot remains credential-free and
Hive performs the label write through a narrow authenticated dashboard endpoint.

Flow:

1. The bot watches only configured channels for GitHub issue links and reaction
   updates.
2. On each relevant reaction event, the bot posts the message URL, channel ID,
   issue URL, emoji, and observed reactor set to the dashboard endpoint. It does
   not send a GitHub token and does not claim the threshold passed.
3. Hive validates the dashboard caller, repo allowlist, channel allowlist, emoji,
   distinct-human threshold, optional role gate, and per-channel rate limit.
4. Hive records the evidence that crossed the threshold, writes the configured
   label through the existing GitHub client, and follows the
   `recordCreationAudit` convention with details including repo, issue number,
   label, Discord channel, message ID, emoji, threshold, and counted user IDs.
5. Normal issue intake sees the label through
   `project.issue_filter.require_labels` and decides whether the issue is now
   eligible.

The threshold check belongs in Hive, not in the bot. That makes reaction
evidence auditable server-side and prevents a compromised or buggy bot from
asserting consensus that Hive cannot independently explain.

The channel list fails closed: no configured channels means the feature is
disabled, never "all channels the bot can see".

## Configuration shape

```yaml
discord:
  issue_promotion:
    enabled: false
    emoji: "❤️"
    threshold: 3
    label: community-approved
    channels: ["123456789012345678"]
    require_role: null
    max_per_hour: 5
```

The intended composition with issue intake is explicit:

```yaml
project:
  issue_filter:
    require_labels: [community-approved]
```

If `discord.issue_promotion.label` is not also listed under
`project.issue_filter.require_labels`, the label may still be useful for human
triage, but it will not gate Hive intake. The dashboard should warn about that
configuration mismatch rather than silently accepting a no-op promotion flow.

## Security and abuse considerations

This feature lets Discord users cause Hive to spend operator money. Treat that
as privilege escalation from chat membership to automation intake.

Required controls:

- **No GitHub credential in the bot.** The bot is exposed to untrusted chat
  content; repo-write credentials stay in Hive.
- **Server-side repo allowlist.** Hive accepts only issues in configured project
  repos. The bot may parse the issue URL, but Hive must not trust it.
- **Server-side channel allowlist.** A configured empty list disables the
  feature. Channels outside the list are ignored even if the bot can read them.
- **Distinct humans.** Count unique non-bot reactors. The message author's own
  reaction should not count.
- **Optional role gate.** If configured, only users with the role count.
- **Idempotency.** Once the label is applied, later reaction removals do not
  unlabel; agents may already be working.
- **Rate limiting.** Enforce a per-channel cap per hour before writing labels.
- **Audit.** Persist the Discord message, channel, emoji, threshold, counted
  user IDs, repo, issue, and label using the `recordCreationAudit` convention.

## Discord implementation gotcha

Reaction handling requires `GatewayIntentBits.GuildMessageReactions`, and that
intent must be enabled in the Discord developer portal as well as in code. It
also needs `Partials.Message`, `Partials.Reaction`, and `Partials.User`; without
partials, reactions on uncached messages, including older issue links that gain
support over time, appear to work in fresh-message tests but are silently missed
in production.

## Phased implementation plan

1. **Smallest first PR:** add config parsing, validation, and docs only. Validate
   that `enabled: true` requires at least one channel, a non-empty label, and a
   positive threshold; add a warning when the label is absent from
   `project.issue_filter.require_labels`.
2. Add the dashboard endpoint behind owner/admin authentication with no Discord
   bot changes. Unit-test repo allowlist, channel fail-closed behavior,
   idempotency, audit fields, and rate limiting.
3. Add Discord reaction collection with the required intent and partials. The bot
   reports evidence to Hive but never writes to GitHub.
4. Add operator-facing observability: last promoted issue, recent denied
   promotions, and clear errors for missing intents or partials.
5. Consider a generic webhook producer only after the Discord-specific path has
   shipped and produced enough evidence to justify abstraction.

## Open questions from the RFC

1. **Where does the threshold check live?** Recommended answer: Hive. The bot can
   gather evidence, but Hive must verify the distinct-human threshold and write
   the audit record.
2. **Same bot process or separate module?** Recommended answer: same package,
   separate opt-in module. Reuse the existing config loader and dashboard bridge,
   but keep command handling and community promotion code isolated because their
   trust models differ.
3. **Does an unlabel/undo path exist?** Recommended answer: no automatic
   unlabel. Add a manual "promotion revoked" audit/comment path later if
   operators need a visible reversal record.
4. **Multi-spoke behavior.** Recommended answer: the Hive whose server-side
   `project.repos` contains the linked repo may promote it. If multiple hives
   match, each should use its own label and audit trail; shared labels should be
   documented as operator coordination, not automatic leader election.
5. **Discord-specific or generic webhook intake?** Recommended answer: start
   Discord-specific. The hard parts are Discord reaction evidence, intents,
   partials, and reactor identity; abstracting before those are proven would hide
   the risk rather than reduce it.

## Non-goals

This design does not implement code, add a dispatch API, create a second queue,
or give the Discord bot GitHub credentials. It only proposes a safe path from
community reaction consensus to the existing label-gated issue intake.
