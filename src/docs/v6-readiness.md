# v6 readiness bar

This page defines what the `v6` line must demonstrate before any build from
`v6` reaches an operator through a release channel. It is the v6 counterpart
of the [v5 GA readiness bar](v5-ga.md), written at line-open time instead of
being retrofitted later (the gap
[#5622](https://github.com/hivecommons/hive/issues/5622) had to close for v5).

See the maintainer [v6 readiness live-exercise runbook](v6-readiness-runbook.md) for the step-by-step evidence template that closes the live rows below.

Status today: `v6` publishes **no channel**
(see the release-lines table in [ROADMAP.md](../../ROADMAP.md)). Every track
named on the line epic
([#7563](https://github.com/hivecommons/hive/issues/7563)) has merged code,
but **merged is not exercised** — no v6 surface has yet run against a live
hive. This bar is what turns one into the other.

## 1. Channel binding

`v6` earns its first moving tag (an `edge`-successor or a `v6-edge` channel —
the name is a maintainer decision recorded here) only when:

- [ ] The **v5 GA bar's Release-train rows are green**
      ([#6016](https://github.com/hivecommons/hive/issues/6016)). v6 does not
      compete with v5 GA evidence for maintainer or agent attention; this
      mirrors the accepted priority in
      [#7577](https://github.com/hivecommons/hive/issues/7577) and
      [ROADMAP.md](../../ROADMAP.md#v6--dashboard-optional-operation-line-open).
- [ ] `v6` is present in `.github/release-lines.yml` and the release-line
      guard is green on `v6`.
- [ ] Every **shipped surface** below has its guard-invariant conformance row
      and at least one live-exercise row checked with linked evidence.

## 2. Guard-invariant conformance (per surface)

The line's single non-negotiable
([#7563](https://github.com/hivecommons/hive/issues/7563)): every
non-dashboard surface routes through the *same* authorization and safety
machinery — the dashboard role floor, the mode ladder and capabilities checked
at the proxy, `Converse` for replies, `ioscan` enforcement on all inbound
text, and canary/secret scrubbing on all outbound text. No surface grows its
own authz.

A surface's conformance row is checked only with a link to a test (or test
suite) that fails if that surface bypasses any of the five mechanisms:

| Surface | Shipped in | Conformance evidence |
| --- | --- | --- |
| GitHub @-mention triggers | #7582 / #7597 / #7623 | ✅ [`src/pkg/mention/conformance_v6_test.go`](../pkg/mention/conformance_v6_test.go) ([#8041](https://github.com/hivecommons/hive/issues/8041)) |
| GitHub Actions trigger (comment relay) | #8206 phase 1 | ✅ [`src/pkg/mention/conformance_v6_actions_test.go`](../pkg/mention/conformance_v6_actions_test.go) ([#8206](https://github.com/hivecommons/hive/issues/8206)) |
| GitHub Actions trigger (hub OIDC dispatch) | #8221 phase 2 | ✅ [`src/pkg/mention/conformance_v6_actions_oidc_test.go`](../pkg/mention/conformance_v6_actions_oidc_test.go) ([#8221](https://github.com/hivecommons/hive/issues/8221)) |
| Slack (Socket Mode) | #7585 | ✅ [`src/pkg/slack/conformance_v6_test.go`](../pkg/slack/conformance_v6_test.go) ([#8042](https://github.com/hivecommons/hive/issues/8042)) |
| Discord (spine port + reliability) | #7572 / #7586 | ✅ [`src/pkg/discord/conformance_v6_test.go`](../pkg/discord/conformance_v6_test.go) ([#8043](https://github.com/hivecommons/hive/issues/8043)) |
| Microsoft Teams | #7621 | ✅ [`src/pkg/msteams/conformance_v6_test.go`](../pkg/msteams/conformance_v6_test.go) ([#8044](https://github.com/hivecommons/hive/issues/8044)) |
| Matrix | #7617 | ✅ [`src/pkg/matrix/conformance_v6_test.go`](../pkg/matrix/conformance_v6_test.go) ([#8045](https://github.com/hivecommons/hive/issues/8045)) |
| Telegram | #7616 | ✅ [`src/pkg/telegram/conformance_v6_test.go`](../pkg/telegram/conformance_v6_test.go) ([#8046](https://github.com/hivecommons/hive/issues/8046)) |
| Dashboard chat | #8304 | ✅ [`src/pkg/dashchat/conformance_v6_test.go`](../pkg/dashchat/conformance_v6_test.go) ([dashboard chat](dashboard-chat.md), [#7683](https://github.com/hivecommons/hive/issues/7683)) |
| Email escalation (outbound + reply-to-act) | #7613 / #7618 | ✅ [`src/pkg/escalate/conformance_v6_email_test.go`](../pkg/escalate/conformance_v6_email_test.go) ([#8047](https://github.com/hivecommons/hive/issues/8047)) |
| Push / on-call (ntfy / Pushover / PagerDuty) | #7613 / #7618 | ✅ [`src/pkg/escalate/conformance_v6_test.go`](../pkg/escalate/conformance_v6_test.go) ([#8048](https://github.com/hivecommons/hive/issues/8048)) |

**Egress-only surfaces.** A surface with no inbound path conforms to the four
inbound mechanisms (`ioscan`, `Converse`, the role floor, the mode ladder) by
*unreachability* rather than by a check. That counts as conformance only where
the unreachability is itself pinned, so the test must fail when the surface
stops being egress-only — not merely pass today. The push / on-call row works
this way: its test asserts outbound scrubbing behaviourally against all three
providers, and parses the surface package to fail if an inbound entry point
(an ntfy action handler, a PagerDuty webhook, a Pushover receipt callback) or
a state-reaching dependency appears. When one does, the guards have to be
wired and asserted for real before the row can go back to green.

Email is the same shape with a shorter fuse: outbound mail is shipped, and
inbound reply-to-act is designed but deliberately unshipped
([escalation-surfaces.md](design/escalation-surfaces.md), "Inbound
reply-to-act (phase 2, explicitly later)"), which makes its egress-only status
a *plan* rather than a property. Its test therefore pins the pieces an inbound
path arrives in — a mail-reading dependency, a poller or reply entry point,
inbound fields on `EmailConfig`, or a sender allowlist invented at the surface
instead of derived from the role floor — and fails on any of them appearing
without `ioscan`. The row goes back to ⬜ until the reply path asserts the
four inbound mechanisms positively.

## 3. Live exercise (per surface)

Checked only with linked evidence of one real round-trip against a live hive —
not a unit test:

| Surface | Exercise definition | Evidence |
| --- | --- | --- |
| GitHub @-mention triggers | A human mentions the App on a real issue/PR; the kick runs; the 👀 ack and audit entries are linked. | ⬜ |
| GitHub Actions trigger (comment relay) | One manual `workflow_dispatch` of `hive-action-smoke` against a real issue; kick recorded with `source=action`; 👀 ack and audit are linked. | ⬜ |
| GitHub Actions trigger (hub OIDC dispatch) | One manual `workflow_dispatch` of `hive-action-smoke` with `transport=oidc`; kick recorded with `source=action, transport=oidc`; Actions receipt: one `workflow_dispatch` of `hive-action-smoke` returns a receipt; audit is linked. | ⬜ |
| Slack | One command round-trip and one notification delivery over Socket Mode from a pull-only cluster. | ⬜ |
| Discord | Reconnect/backoff observed across one induced disconnect; notification parity spot-checked. | ⬜ |
| Teams / Matrix / Telegram | One command round-trip and one notification delivery each. | ⬜ |
| Dashboard chat | Conformance passes; one live round trip of `!status` from the panel is linked. | ⬜ Pending live exercise; code landed in [#8326](https://github.com/hivecommons/hive/pull/8326) and conformance is tracked by the row above. |
| Inception via chat | One live greenfield run reaches `complete` entirely through the chat spine. | ⬜ Pending live exercise; code landed in [#8333](https://github.com/hivecommons/hive/pull/8333) / [#8393](https://github.com/hivecommons/hive/pull/8393). Exercise with `just runs-e2e` against a live hive once #8466 lands. |
| Runs via chat | One live run reaches a human gate and is approved from the chat spine; the next runs snapshot shows it advancing. | ⬜ Pending live exercise; code landed in [#8351](https://github.com/hivecommons/hive/pull/8351) / [#8365](https://github.com/hivecommons/hive/pull/8365). Exercise with `just runs-e2e` against a live hive once #8466 lands. |
| Email | One HUMAN DECISION NEEDED escalation delivered; one allowlisted inbound reply acted on (or reply-to-act explicitly deferred here). | ⬜ |
| Push / on-call | One `requires_human` verdict pages a real device via at least one provider. | ⬜ |

## 4. Scope discipline

- New v6 tracks enter through the line epic
  ([#7563](https://github.com/hivecommons/hive/issues/7563)), which names
  them before implementation PRs open. RFCs proposing v6-flavored work (for
  example [#7620](https://github.com/hivecommons/hive/issues/7620),
  [#7629](https://github.com/hivecommons/hive/issues/7629)) are measured
  against this bar: work that adds surfaces also adds its conformance and
  exercise rows here in the same PR.
- Changes to this bar go through a PR touching this file, so readiness is
  readable from one page instead of diffed from prose — the failure mode
  [#5622](https://github.com/hivecommons/hive/issues/5622) recorded for v5.

## Tracker checklist

The live tracker is
[#7683 — v6 readiness bar](https://github.com/hivecommons/hive/issues/7683).
Keep its checklist in sync with the tables above. Rows are checked only with
linked evidence (PR, workflow run, message screenshot/audit entry, pager
event), never on "merged".
