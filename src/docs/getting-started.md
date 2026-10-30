# Zero to Automation: Getting Started with Hive

Hive is a team of AI agents that watch your project and help improve it — finding bugs, adding tests, writing docs. This guide gets you from access to a dashboard to your first findings. Later sections explain optional changes in permissions.

## The Hive Way

**ACMM** means **AI Codebase Maturity Model**. Its six levels connect agent permissions to the maturity of your project's tests, coverage, reviews, and security safeguards. They are not an issue-to-PR automation ladder or a schedule for promotion. Review output and strengthen safeguards before granting more authority.

## Trust > Level. Always.

You do not need to reach L6. At L6, permitted lanes can merge when automated checks pass, without per-PR human approval. Staying at L2, L3, L4, or L5 is a valid choice. A passing check is only useful if your tests can catch the mistakes you care about.

## Obtain a hive and check access

| Starting point | Action | Continue when |
|---|---|---|
| No hive; you want hosting | Open [Request a hive](https://hive.hivecommons.dev/get-started) and follow [Hosted Hub onboarding](hosted-hub.md). Sign in with GitHub, supply the repository URL, and request a starting level. | An admin has approved and provisioned it; **My Hives** shows its dashboard URL. |
| No hive; you will operate it | Choose [Docker Compose](quickstart-docker-compose.md), [Podman](quickstart-podman.md), or [Kubernetes](kubernetes-deployment.md). Complete that guide's runtime, storage, network, and login setup first. | Your container is running and you can open its dashboard. |
| You already have a dashboard URL | Sign in with the GitHub identity granted access by its owner. | You can see the welcome checklist and settings. |

Before Step 0, have:

- A repository on **GitHub.com or GitHub Enterprise**. GitLab, Gitea, and Forgejo are not supported for running a hive today.
- For the App route, someone with permission to install/authorize it for that repository. You can request a hosted hive before installing the App; the request is not an App authorization. A self-hosted PAT deployment instead needs the [documented token scopes](github-app-setup.md#personal-access-token-pat-scopes).
- Owner access for changing owner-only settings, or an owner who will perform those steps. Viewing a dashboard does not grant configuration rights.
- A supported agent-backend login/subscription or a model gateway and credentials. Hosting the hive does not automatically authenticate its coding agents.

The hosted wizard offers L1–L3 and currently recommends L3; **do not assume your hive starts at L2**. Check the applied level in the dashboard. For an observation-first session, choose L2 deliberately before enabling workers. At L3, quality is already allowed to open held PRs.

**First-session route:** [connect the App](#step-0--before-you-start-do-this-first) → [authenticate and set limits](#your-first-week-do-exactly-this) → set cadences **before** enabling agents → read findings. The [level-by-level reference](#l1--getting-started) is optional after that.

### Words used in the dashboard

- **PR (pull request):** a proposed code change; **CI:** automated build and test checks.
- **Bead:** a saved work item or finding in Hive, visible on the dashboard.
- **Cadence:** how often an agent may run; model **tokens** measure model input/output usage and affect cost.
- **Governor:** the scheduler that checks for work and dispatches tasks.
- **Heartbeat:** a periodic status/configuration exchange; it is not the agent's work schedule.

## What to expect (and what not to)

- Work appears on the configured schedule, not instantly. You may need days or weeks of reviewing output to judge whether it helps.
- Findings may be familiar, low-priority, or wrong. Decline or correct them; do not treat them all as tasks.
- At PR-capable levels below L6, agent PRs carry literal `hold` for human review; `hive-pause/<hive-id>` is a separate dashboard manual hold.
- Project knowledge and policy guidance can improve context over time, but agents do not replace human judgment and no level guarantees correct output.

---

## Step 0 — Before you start (do this first!)

None of the level guidance below works until your hive is connected to your git host. Do this in your **first session**:

**Self-hosted with an already configured PAT?** Verify repository access using
your runtime guide and the [required scopes](github-app-setup.md#personal-access-token-pat-scopes);
do not install a second App just to follow this checklist. The App instructions
below are the normal hosted path. The reclaim warning applies to hosted hives,
not an unmanaged standalone deployment.

1. **Install the Forge App.** On the supported hosts, **GitHub.com and GitHub Enterprise (GHE), this is a GitHub App**. It lets Hive access the repositories you authorize. GitLab, Gitea, and Forgejo are **not supported for running a hive** today — see [Forge setup: GitLab, Gitea, and Forgejo](forge-app-setup.md).
   - **From the dashboard (easiest):** click **Install Forge App** in the welcome checklist, or open **Governor Config → Forge App** and use the install link there. Grant the app access to your repo.
   - **On GitHub.com:** the install button sends you to `github.com/apps/<app-slug>` — pick your org/repo and approve.
   - **On GitHub Enterprise (IBM, corporate):** the same flow lives on your **GHE host**, not github.com — the install page is `https://<your-ghe-host>/github-apps/<app-slug>`. Make sure your hive is pointed at your GHE host URL (Governor Config → Forge App shows which host is configured).
   - **Self-hosting or creating the app yourself?** See the [GitHub App setup guide](github-app-setup.md) for app creation, permissions, and the `/gh-setup` flow.
   - **On GitLab, Gitea, or Forgejo?** There is no Forge App to install: those forges are **not supported for running a hive** today, and the rest of this guide assumes GitHub. See [Forge setup: GitLab, Gitea, and Forgejo](forge-app-setup.md) for what is and is not implemented.
2. **For hosted hives, finish setup promptly.** Unconfigured instances are reclaimed on a timer. Finish the Forge App install in your first session or your hive may be reaped — see [What if my hive disappeared?](#what-if-my-hive-disappeared-inactive-hive-reaping) below.
3. **Verify the connection.** Check the App's auth state and repository access. On a hosted hive, allow a few minutes for the next heartbeat to report the new state. A heartbeat does not clear unrelated backend or configuration errors; standalone hives do not need a hub heartbeat.

> **On GitHub Enterprise (IBM, corporate)?** Setup is slightly different: point Hive at your **GHE host URL**, not github.com. The Forge App install flow lives on your enterprise host — a 404 from the install button almost always means the wrong host.

> 💡 **Tip: name your hive like a team member.** The `ai_author` field is the login your agents use when opening PRs. Pick a name that signals what it does — `hive-bot`, `your-repo-ai`, `proj-assistant`. A clear name means your team immediately knows which PRs are agent-authored vs human-authored.

## Your first week (do exactly this)

1. **Connect and authenticate.** Finish Step 0, then follow [hosted first-run setup](hosted-hub.md#3-finish-first-run-setup-on-the-spoke-dashboard) for **Settings → Model Gateways** or the per-backend CLI login. A gateway's **Test** should report `OK — N models available`; a CLI backend must finish its login. Check errors rather than waiting for authentication to fix itself.
2. **Confirm limits before work starts.** Check the ACMM badge and **Settings → Budget**. Set Total Tokens, Period (days), and Critical Percent. To observe without new issues or PRs, use L2. Leave supervisor paused and brainstorm unchanged for this first exercise.
3. **Set cadences, then enable workers.** At L2, open the gear settings for scanner, quality, and guide. Set each available governor/load-mode cadence to **12h or 1d**, then un-pause them. Start with one agent if you want to distinguish its output. Check reviewer separately if enabled: it can post COMMENT-only reviews at L2, not approvals or merges.
4. **Verify setup.** Allow 2–3 minutes for configuration/status propagation. Confirm App access, backend authentication, the applied level, and the saved cadence. A first run can take 5–10 minutes or a full cadence interval; a heartbeat alone does not mean work has run. Check Activity and authentication errors if no run appears.
5. **Over days 3–7, read results.** Read dashboard beads and the advisory digest/tracking issue when available. Choose one useful finding to handle manually. At L2, new issues and PRs are not expected; the reviewer may comment on existing PRs.

**First success:** you can sign in, the App can access the intended repository, a backend is authenticated, your budget/cadences are saved, and an enabled worker produces a dashboard finding or a documented no-work result. Do not raise the level merely to make something happen.

If workers were already active when you received the hive, pause them while
confirming the level, budget, and cadences. For a self-hosted PAT deployment,
the same success check is repository access using that credential, not an
App-install requirement.

For the following weeks, review findings and improve tests before considering more permissions. The durations in the level sections below are conservative learning suggestions, not guarantees, prerequisites enforced by a timer, or a required promotion schedule.

---

## Community help

If you get stuck, [Join our Discord](https://hivecommons.dev/discord). The invite is permanent, and the Hive Commons community can help with setup, first-run questions, contributor relay, and choosing a safe next ACMM step. To see what a running hive looks like, [watch the hive live on Twitch](https://www.twitch.tv/hivecommons).

## Common gotchas (so you don't panic)

- **Dashboard full of warnings?** Normal. Most warnings clear automatically after the Forge App is installed and the first heartbeat runs. Don't panic.
- **"Install Forge App" gives a 404?** Check whether the hive is configured for GitHub.com or the correct GitHub Enterprise host, and whether the App exists on that host.
- **Changed a setting and nothing happened?** Agents pick up config changes on the next **heartbeat cycle**. Wait 2–3 minutes before assuming something is broken.
- **Hive vanished from Usage / old URL times out?** Your hive was probably reaped for inactivity. See the next section — recovery is quick.

---

## What if my hive disappeared? (inactive-hive reaping)

Hosted hives that stay **unconfigured or inactive are reclaimed ("reaped") on a timer** to free fleet capacity. When that happens:

- Your hive stops appearing under **Usage** on the hub.
- The old hive URL (`https://<id>.hive.hivecommons.dev`) **times out** — it is gone, not just sleeping.

**This is normal and recoverable.** Nothing is wrong with your account. To get going again:

1. **Request a new hive.** Go to the hub and click **Request a hive** (the `/get-started` wizard — on the hosted hub that's [hive.hivecommons.dev/get-started](https://hive.hivecommons.dev/get-started)). You'll get a fresh hive with a new URL — don't wait for the old URL to come back.
2. **Complete Step 0 right away.** Install the [GitHub App](#step-0--before-you-start-do-this-first) in your **first session** on the new hive. Keep the hive configured and active; installing the App is not a promise of indefinite hosting.
3. **Update your bookmarks** to the new hive URL.

> **Avoid a repeat:** finish App setup on day one and check that heartbeats and activity continue. Hosted capacity is reclaimed according to the hub's inactivity policy.

---

## The four agent modes (learn these first)

| Mode | What the agent can do |
|------|----------------------|
| **Advisory** | Watch only. Posts findings to your dashboard. Never touches your repo. |
| **Measured** | Can file issues on your git host. |
| **Holdgated** | Can open PRs, but every PR gets a `hold` label. A human must remove the hold to merge. |
| **Full** | Permitted autonomous lanes can open PRs and merge when CI and merge gates pass. Lane-specific restrictions still apply. |

These are **permission modes**. The cadence grid instead uses governor **load modes** (`surge`, `busy`, `quiet`, `idle`) to choose run frequency. Reviewer has a separate comment/review capability while remaining advisory; it does not gain issue creation or merge rights.

---

## ⚠️ Cadences: the #1 thing to get right

Every agent has a gear icon (⚙️). Click it and open its cadence settings. Set the cadence for each governor load mode (`surge`, `busy`, `quiet`, `idle`) to **12h or 1d when you first start**, before un-pausing that agent. These are not the advisory/measured/holdgated/full permission modes.

Here's why: agents run on a timer. A 15-minute cadence means an agent can run **96 times a day**. At scale, that burns your token budget in hours. A 12-hour cadence means 2 runs a day — steady, useful, not ruinous.

You can always shorten cadences later once you know how fast an agent is burning through your budget. **Start long. Shorten deliberately.**

Long cadences also limit the volume you need to review; they do not guarantee
better findings or deduplication.

---

## The advisory issue: start here every time

The dashboard advisory digest summarizes findings. A configured hive can also publish a tracking/advisory issue in your repo; it needs working App access and publication. If it has not appeared, use dashboard beads and check configuration rather than assuming an empty repository means no findings.

**Start here when you check in:** read the dashboard digest or the tracking issue, if published and pinned in your repository. It is a summary, not a complete audit log.

Read it like a weekly digest, not a to-do list. You don't have to act on everything — just stay oriented.

> 💡 **Tip: the advisory issue is a great standup input.** Every Monday morning, open it and skim the last week's entries. It takes 5 minutes and gives you a clear picture of what's been happening in your repo without reading every commit.

> 💡 **Power tip: feed it to your own AI tools.** The advisory issue isn't just for reading — it's the perfect input for your own agents. If you use Copilot, Claude, Cursor, or any other AI tool, paste the advisory issue into your conversation: *"Here's what my hive found this week — help me prioritize"* or *"Pick the three findings most likely to cause a production bug."* Your hive's findings + your own AI tools = faster triage. Think of the advisory issue as your hive's report card — readable by humans and by other AI agents alike.

---

## How to read hive-filed issues

Every issue a hive agent opens has the agent's name in the title — for example `[scanner] Possible nil pointer dereference in handler.go:142` or `[quality] Missing test coverage for payment flow`. The same holds for issues the hive files on its own behalf: `[reviewer] Review backlog (security): …`, `[dedupe] triage: …`, `[ci-maintainer] refactor: … is a merge hotspot`, `[selfreport] …`, `[governor] 🐝 Hive Advisory Report`. The tag always comes first, before any emoji, and the Activity Monitor draws it as a chip ahead of the title.

When you see a new issue in your project, check the title prefix — it tells you which agent filed it and what kind of finding it is:

- `[scanner]` = bugs
- `[quality]` = test gaps
- `[guide]` = doc gaps
- `[sec-check]` = security findings

You can filter your issues by label or search `[scanner]` to see only scanner's findings.

---

## When do PRs appear? (be precise about this)

New users often expect PRs at L2 (they don't happen) or are surprised when they appear at L3. Here's exactly what to expect:

| Level | GitHub activity |
|-------|-----------------|
| **L1** | Inception: guide and brainstorm advise on setup and ideas. No agent-created issues or PRs. |
| **L2** | Advisory: workers report beads; reviewer can post COMMENT-only reviews on existing PRs. No new issues, PRs, approvals, or merges. |
| **L3** | **Quality only** can open PRs. Every PR has a literal `hold` level-gate label — it will NOT merge until you remove the hold. No other agent opens PRs at L3. |
| **L4** | Quality, ci-maintainer, **and** sec-check can open PRs (all with literal `hold` level-gate labels). Scanner and guide file issues — not PRs. |
| **L5** | Delivery lanes propose issues and held PRs for human review. Supervisor and brainstorm remain advisory; reviewer never merges; dedupe only recommends consolidation. Telemetry and operations are paused opt-ins. |
| **L6** | Switching levels enables auto-merge for active repos; owners can then toggle each repo. Scanner is the default merge-capable agent; most delivery agents still propose PRs, which Hive's separate auto-merge path can merge after gates pass. Cross-repo outreach stays held. Specialist restrictions remain. |

> **The hold label is your safety net.** Hive uses literal `hold` as the level-gate merge-blocking PR label; `hive-pause/<hive-id>` is the dashboard's manual hold label and `hive/<hive-id>` is provenance only. At every level below L6, every PR an agent opens is blocked from merging until you remove `hold`. You are always in control. Nothing ships without your approval until you reach L6 — and you'll only reach L6 after months of trusting the system.
>
> Raising the level **does not automatically release existing holds**. A human removes them after review. An operator can deliberately request a one-off release of attributable Hive level holds with `release_level_holds: true`; human-applied or re-applied holds remain protected. See the [policy matrix](acmm-policy-matrix.md).

---

> **Moving between levels:** open the **Governor config** and set the level number. Changes take a heartbeat cycle (a few minutes) to propagate.

## Optional reference: levels and readiness

The policy names are **Inception**, **Advisory**, **Quality-Gated**,
**Security-Aware**, **Semi-Autonomous**, and **Autonomous**. The sections below
offer a conservative learning routine for those levels, not a substitute for
the [current per-agent policy matrix](acmm-policy-matrix.md). Enabling an agent
does not promote its permissions.

## L1 — Getting Started

**The level:** You're trusting the system with your *ideas*, nothing else. No agent can touch your repo.

**What you get:** Guide helps you structure your ideas. Brainstorm helps turn raw thoughts into real plans. Pure advice, posted to your dashboard.

1. Set guide's cadences for all governor load modes to **12h or 1d**.
2. Then un-pause guide. Leave brainstorm unchanged for this exercise. It
   remains advisory and can be used for inception when wanted; this is a
   conservative starting choice, not a permission rule.

**Using the findings:** Read guide's suggestions like a newsletter. This is your reading list, not your to-do list.

**Be patient:** After un-pausing, wait 5–10 minutes — agents run on a heartbeat cycle. Dashboard warnings usually clear on their own after the first heartbeat.

**When to move up:** After **about a week** — the Forge App is connected, the dashboard makes sense, and you're comfortable. Most people start at L2 anyway.

## L2 — Watch and Learn

**The level:** Workers look at code and report findings, without creating
issues or PRs. Reviewer may post COMMENT-only reviews on existing PRs;
it cannot approve or merge them.

**What you get:** Scanner looks for bugs in your code. Quality looks to add testing. Guide writes documentation for you. No changes, no issues filed — just a reading list on your dashboard (called **beads**). You will *not* see a flood of issues and PRs here — that's the point. You're reading, not reacting.

1. For scanner, quality, and guide, set every governor load-mode cadence to **12h or 1d**.
2. Then un-pause the chosen worker(s). Leave supervisor paused and brainstorm
   unchanged. Reviewer is a separate review-first entry point; check its
   cadence and output if enabled.

**Using the findings:** Read your dashboard beads and the pinned **advisory issue** in your repo. Pick **one finding per week** that you were already planning to fix, and fix it by hand. Ignore the rest for now — there will always be more findings than time.

**Building tests:** Notice what quality flags as missing tests. You're not acting on it yet — just learning what quality thinks your safety net needs.

**Be patient:** After changing a cadence or un-pausing an agent, wait 5–10 minutes before assuming something is wrong.

> 💡 **Tip: one agent at a time.** When you un-pause agents at a new level, don't un-pause them all on the same day. Un-pause one, watch it for 3–4 days, then add the next. You'll understand each agent's personality before the noise from multiple agents overlaps.

**When to move up:** **2–3 weeks minimum.** Run L2 for real: read the beads, act on a few findings yourself, and understand what the agents are seeing before you give them any write access. When you agree with their findings more than half the time, you're ready for L3. Don't rush this.

## L3 — Build Your Safety Net

**The level:** You're trusting one agent (quality) to *write code* — but every PR gets a `hold` label, so nothing merges without you.

**What you get:** Quality builds your tests, one held PR at a time. CI-maintainer joins to keep your builds healthy. This level exists to build the safety net that makes higher levels safe.

1. Set ci-maintainer's governor load-mode cadences to **12h or 1d**; re-check the existing workers.
2. Then un-pause ci-maintainer; quality, scanner, and guide can stay on from L2.
   Leave supervisor paused and brainstorm unchanged for this routine.

**Using the findings:** Beads are still your reading list. Same rule: one finding per week, matched to what you already planned.

**Building tests:** This is quality's big moment. Expect a few PRs per week — that's normal, not slow. Review every one, even the ones you don't merge. Reading them teaches you what quality thinks is missing. Merge the good ones; these tests will later *correct the agents* and keep their output honest.

> 💡 **Tip: the 3-issue rule.** When quality starts opening PRs, don't review them all at once. Pick the three smallest ones. Merge one, decline one, leave one. This teaches the agent what you value faster than reading 20 PRs and ignoring them all.

> 💡 **Tip: the hold label is a conversation.** You don't just approve or decline a held PR. You can edit it, add comments, push commits on top of it. The agent will see your changes on the next run and learn from the diff.

**Be patient:** After setting the level to 3 in the Governor config, give it a heartbeat cycle (5–10 minutes) before expecting PRs. Then settle in — this level is measured in weeks, not days.

**When to move up:** **3–4 weeks.** Let quality build your test suite. Get comfortable approving or declining hold-labeled PRs before adding more agents. Move up when your CI runs real tests and reviewing held PRs feels routine.

> 💡 **Tip: customize before you escalate.** Before moving from L3 to L4, take 30 minutes to edit each agent's policy template. Add your coding conventions, your preferred test framework, your off-limits directories. Agents follow instructions literally — the more specific you are, the better the output.

### Where agents actually run (read this before L3)

**Applies to contributor-host execution, not normal hosted-dashboard onboarding.** A hosted spoke runs agents in its provisioned environment; a self-hosted container follows its deployment's isolation controls. The contributor path (`just contribute-hive <backend>`) can instead run agents in a **tmux session on your host** by default: the backend CLI runs as your own user, with permission prompts bypassed, and nothing containing it to the assigned workspace unless the backend provides its own confinement. Review this before accepting delegated code execution, regardless of level.

**Confinement is not the same for every backend.** As of this writing:

| Backend | Confined? |
|---|---|
| `claude` / `litellm` | Yes — Claude Code's native OS sandbox |
| `codex` | Yes — its own `workspace-write` sandbox |
| `copilot` | Yes — Copilot CLI's own `--sandbox`, checked at launch |
| `opencode` | Partial — a command deny-list only, **not** a filesystem sandbox |
| `muse` | Yes — muse's own OS sandbox (bubblewrap/seccomp on Linux, seatbelt on macOS), narrowed further by hive |
| `goose`, `agy`, `bob`, `pi`, `aider` | No — these backends have no confinement mechanism at all. Local mode **refuses to launch** for them unless you explicitly set that backend's own `HIVE_<BACKEND>_DANGEROUSLY_RUN_UNCONFINED=1` |

If you see one of those `_DANGEROUSLY_RUN_UNCONFINED` variables mentioned in setup instructions, it means exactly what it says: that backend has no sandbox, and setting the variable is you accepting that the agent runs with full access to your machine. Prefer container mode (drop `local` from the command) or a confined/denylisted backend if you're running hive on a machine you care about.

This matters beyond backend choice, too: the hub-side Podman sandbox (`agent_sandbox`) is a separate, opt-in mechanism, and enabling it requires setting **both** the global `agent_sandbox.enabled` flag **and** a per-agent `sandbox.enabled` flag — the dashboard's Security tab only writes the global one, so turning that toggle on by itself does not sandbox any agent.

See [sandbox-isolation.md](sandbox-isolation.md) for the full threat model, the complete per-backend matrix, and the hub-side sandbox setup.

## L4 — Issues and Security

**The level:** You're trusting agents to *file issues on their own* and trusting sec-check to propose security fixes — still all hold-labeled.

**What you get:** Scanner and guide file issues automatically. Sec-check finds security holes and can open PRs (alongside quality and ci-maintainer). Automated bug reports, doc suggestions, and security findings, delivered to you.

1. Set sec-check's governor load-mode cadences to **12h or 1d**. Security scans can use substantial tokens.
2. Then un-pause sec-check. Leave supervisor paused and brainstorm unchanged
   unless deliberately configuring them separately.

**Using the findings:** Read the issues agents file. Add a 👍 to the ones that match your priorities — the agents will pick up the signal. You don't have to respond to everything.

> **Tip:** react to findings to communicate priorities, but do not assume a
> reaction overrides eligibility, holds, filters, or other scheduling gates.

> 💡 **Tip: scanner finds, quality fixes.** Scanner flags bugs. Quality fixes test gaps. They're a team. At L4, watch for scanner filing an issue and quality filing a PR that addresses it — that's the closed-loop feedback working.

**Shoring up security:** Sec-check's first run will probably find things. Don't panic. Read each finding, fix the critical ones yourself, and let sec-check open PRs for the medium ones — they'll have literal `hold` labels, so you approve before anything merges.

**Be patient:** The first sec-check run can take a full cadence cycle to appear. And yes — you'll get more issues and PRs at this level. Still review them one by one. The literal `hold` label exists precisely so nothing merges without you.

**When to move up:** **4–5 weeks.** Let sec-check find and fix security issues. Watch the pattern of what agents propose. Trust is earned slowly — move up when you're approving most agent change requests without changes.

## L5 — Propose and Review

**The level:** Delivery agents may propose issues and PRs — the system proposes, you decide. Every agent PR still has a literal `hold` level-gate label. Supervisor and brainstorm stay advisory, reviewer comments rather than merging, and dedupe posts recommendations rather than closing other people's work.

**What you get:** The full hive works for you. Architect produces RFCs for bigger design changes. You shift from doing the work to batch-reviewing it.

1. Configure each newly enabled agent's governor load-mode cadences to **12h or 1d**.
2. Then un-pause supervisor/architect if wanted. Brainstorm can be used
   deliberately here too, but remains advisory. Other L5 specialists retain
   their lane-specific rights.
3. Leave telemetry and operations paused unless completing both opt-in steps below.

### Optional: enable telemetry and operations

These agents first become available at L5 and remain **paused by default** at L5 and L6. They may propose issues and PRs but **never merge their own PRs**.

1. Open **Settings → Project Observability**. Declare the stack your managed project actually runs and save. This may be OpenTelemetry, Prometheus, Grafana, a `ServiceMonitor`, or a commercial backend. Without that declaration, their PR-capable policies fail closed.
2. Set each agent's cadence before un-pausing it; **24h** is a conservative start. Saving the observability declaration never changes cadences (#7261), so both steps are required.

Leaving them paused is valid. See [Telemetry agent](telemetry.md), [Operations agent](operations.md), and the `project_observability` block in [agent configuration](agent-configuration.md).

### L5 review routine

**Using the findings:** Batch-review on a schedule (say, twice a week). Approve the PRs you like, decline the ones you don't, 👍 the issues that match your roadmap.

> 💡 **Tip: batch-review in one sitting.** Reviewing ten agent change requests in a single hour teaches you the agents' patterns faster than reviewing one per day. Patterns jump out when the PRs sit side by side — repeated habits, favorite files, blind spots.

**Building tests:** By now, quality should have already added tests for your main flows. If it hasn't, go back to L3 habits before moving on — L6 depends on it.

**Be patient:** With everything un-paused, the dashboard gets busy. Give new agents a heartbeat cycle before judging their output.

**When to move up:** Only when you **genuinely trust the agents' judgment** — meaning you've reviewed enough of their PRs to know they're consistently doing the right thing, and your test suite is strong enough that passing checks genuinely means "safe to ship." There's no calendar for this one.

## L6 — Autonomous

**The level:** autonomous delivery where permitted. Auto-merge is off below L6;
switching to L6 enables it for active repos, after which owners can toggle each
repo. Permitted delivery lanes can merge when CI and gates pass; specialist
restrictions still apply. New PRs do not get a level hold except outreach into
unmanaged repositories. Existing holds are not automatically removed.

**What you get:** autonomous delivery where enabled. Tests, review/merge gates,
branch rules, and the permission boundary remain important safeguards.

The current L6 pack grants scanner `ISSUES_PRS_MERGE`; most other delivery
agents keep `ISSUES_AND_PRS`. A policy filename ending in `-full.md` does not
give every agent permission to merge its own work. Hive's system auto-merge
path is separate from the proposing agent's capability.

**Un-pause:** everything stays on from L5
**Leave paused:** telemetry, operations — still opt-in, unless you already enabled them at L5

⚠️ **Cadence check:** You can shorten cadences now if your token budget allows — but 12h/1d still works fine. Faster isn't better if you're not reading the output.

Telemetry and operations do not auto-enable at L6. Their templates change to the L6 policies if enabled, but their `ISSUES_AND_PRS` capability still forbids merging their own PRs. Brainstorm and supervisor remain advisory; reviewer and adjudicator never merge, and dedupe stays recommendation-only.

**Using the findings:** Spot-check merged PRs weekly. 👍 issues to steer agent priorities.

**Building tests:** Keep improving tests alongside review gates, holds, branch protection, and lane permissions. Green CI alone is not proof that a change is safe.

**If a bad PR merges:** pause the affected work or lower permissions if needed,
follow recovery/rollback, and add a regression test. Do not leave unsafe
automation running just to remain at L6.

> 💡 **Tip: L6 doesn't mean you're done reviewing.** Even at L6, read the merged PRs weekly. Not to intervene — to calibrate. If you see a pattern you don't like (agents always touching the same file, tests that are too shallow), update the policy template and the next run corrects course.

**When to move up:** There is no up. You made it. 🐝

---

## And after that?

**Weeks 2–3** — Stay at L2. When the agents' findings match what you'd find yourself, open the **Governor config** and set the level to **3**. Now quality can open PRs (with literal `hold` labels). Review and merge the ones you like.

**Weeks 4–7** — Live at L3 while quality builds your test suite. Then L4 for a month or so while sec-check hardens things. L5 and L6 come when trust is genuinely earned — *if* you ever want them at all. L4 or L5 forever is a perfectly good place to live.

These are suggested review periods, not promotion criteria. Keep the current
level until safeguards and output quality support the permissions you intend
to grant; remaining at a lower level is valid.

---

## Tips, Tricks & Where to Find Things

**Cadences:** Click the ⚙️ gear on any agent in the left sidebar → "Edit Agent" or "Configure" → cadence settings. Set each governor load mode (`surge`, `busy`, `quiet`, `idle`) to 12h or 1d **before** un-pausing. Shorten deliberately once you know the cost and output quality.

**Agent prompts:** Each agent has a policy template — the instructions it follows. Click the agent in the sidebar → gear icon → "Edit Policy" (or "Edit Prompt"). You can customize what the agent focuses on. Example: tell quality to focus only on your `/api` package, not your `/frontend` package. Be specific — agents follow instructions literally.

**Filtering which issues agents can work on:** In your hive config (Governor settings), you can set issue label filters — agents will only pick up issues that match specific labels. This is how you keep agents from running off and fixing things you didn't want touched. Add a label like `hive-ok` to issues you're comfortable with agents acting on. Set the filter to `hive-ok`. Now agents only work issues you've explicitly blessed.

**Reading the advisory issue:** Search your issues for `[hive]` or look for a pinned issue titled something like "Hive Advisory" or "Hive Tracking". This is your hive's running log. Bookmark it.

**Pointing other AI tools at hive findings:** Copy the advisory issue URL and paste it into Claude, Copilot, or Cursor: *"Summarize the most critical findings from this issue and suggest which three I should act on this week."* Your hive's findings + your preferred AI = faster triage.

**Checking what agents are doing:** Live Activity feed (dashboard → Activity tab) shows what's running right now. If an agent looks stuck, check cadence — it may just not be scheduled to run yet.

**If something looks wrong:** Wait 10 minutes. Most issues are just agents waiting for their next heartbeat cycle. If it's still wrong after 10 minutes, check the advisory issue for error messages before filing a support request.

> 💡 **Tip: pausing is free.** Pausing an agent never loses its findings or its learning — everything picks up where it left off. Going on vacation? Big refactor week? Pause aggressively and resume when you're back. The hive waits for you.

> 💡 **Tip: dropping a level is a feature, not a failure.** Planning a big architectural change? Drop from L4 to L3 (or L3 to L2) while the dust settles, then move back up. Levels are a dial, not a ladder you fall off.

---

For configuration beyond this first path, use [agent configuration](agent-configuration.md),
[settings](settings.md), and the [documentation map](documentation-map.md).

## What this guide doesn't cover (and why)

Policy templates, knowledge layers, custom agents, issue-label filters, and
multi-repo setups are optional next topics, not first-session requirements.
Use [Hive Labels and Control Signals](labels-and-control-signals.md) when
ready for label details.

For a specialist, **＋ Add agent** in the Agents sidebar or Agents section
opens quick-start templates (scanner, reviewer, quality, CI maintainer, guide)
and import-from-YAML for shared agent definitions.
