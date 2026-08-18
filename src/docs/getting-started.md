# Zero to Automation: Getting Started with Hive

Hive is a team of AI agents that watch your repo and help improve it — finding bugs, adding tests, writing docs. It works in **levels (L1–L6)**: at low levels agents only *suggest* things, and at high levels they can open and even merge pull requests. You climb the levels as you build trust in what the agents produce — over **weeks per level, not days**.

## The Hive Way

> Hive is not a fire-and-forget automation tool. It's a trust-building process. You run each level long enough to understand what the agents are doing and agree with their judgment — then you give them a little more autonomy. Most people spend 2–3 weeks at L2, 3–4 weeks at L3, 4–5 weeks at L4. By the time you reach L5 or L6, you won't be surprised by what the agents produce — because you've been watching them work for months. **That's the point.**

The biggest mistake new users make: seeing agent output and either (a) panicking, or (b) immediately jumping to a higher level to get more automation. Neither is right. Read, review, and let trust build at its own pace.

---

## Step 0 — Before you start (do this first!)

None of the level guidance below works until your hive is connected to your git host. Do this in your **first session**:

1. **Install the Forge App** (the app you install on your git host — GitHub, GitLab, Gitea, etc.). This is how Hive talks to your repo. Click **Install Forge App** from your dashboard and grant it access to your repo.
2. **⏰ Don't put this off.** Unconfigured hive instances are reclaimed on a timer. Finish the Forge App install in your first session or your hive may be reaped.
3. **Wait for the first heartbeat.** After installing, a heartbeat cycle has to run (a few minutes) before everything lights up green.

> **On GitHub Enterprise (IBM, corporate)?** Setup is slightly different: point Hive at your **GHE host URL**, not github.com. The Forge App install flow lives on your enterprise host.

---

## Common gotchas (so you don't panic)

- **Dashboard full of warnings?** Normal. Most warnings clear automatically after the Forge App is installed and the first heartbeat runs. Don't panic.
- **"Install Forge App" gives a 404?** Usually a GHE-vs-github.com mixup, or the app isn't available on your host yet. Double-check which git host you're on.
- **Changed a setting and nothing happened?** Agents pick up config changes on the next **heartbeat cycle**. Wait 2–3 minutes before assuming something is broken.

---

## The four agent modes (learn these first)

| Mode | What the agent can do |
|------|----------------------|
| **Advisory** | Watch only. Posts findings to your dashboard. Never touches your repo. |
| **Measured** | Can file issues on your git host. |
| **Holdgated** | Can open PRs, but every PR gets a `hold` label. A human must remove the hold to merge. |
| **Full** | Opens PRs and merges automatically when CI is green. |

---

## ⚠️ Token burn warning (read this before anything else)

Agents run on a schedule (a **cadence**). Short cadences = agents run constantly = they chew through your token allocation fast.

**Set cadences long when starting out — 12h or 1d for every active agent** so it doesn't chew through your token allocation. Click the agent in the left menu → click the **gear icon** → change the cadence for each mode. You lose nothing — the findings will still be there when you check in.

---

> **Moving between levels:** open the **Governor config** and set the level number. Changes take a heartbeat cycle (a few minutes) to propagate.

## L1 — Getting Started

**The level:** You're trusting the system with your *ideas*, nothing else. No agent can touch your repo.

**What you get:** Guide helps you structure your ideas. Brainstorm helps turn raw thoughts into real plans. Pure advice, posted to your dashboard.

**Un-pause:** guide
**Leave paused:** (nothing else to worry about) — don't touch brainstorm

⚠️ **Set cadences first:** Click the gear on guide → set all modes to **12h or 1d**. Do this before you walk away.

**Using the findings:** Read guide's suggestions like a newsletter. This is your reading list, not your to-do list.

**Be patient:** After un-pausing, wait 5–10 minutes — agents run on a heartbeat cycle. Dashboard warnings usually clear on their own after the first heartbeat.

**When to move up:** After **about a week** — the Forge App is connected, the dashboard makes sense, and you're comfortable. Most people start at L2 anyway.

## L2 — Watch and Learn

**The level:** You're trusting agents to *look at your code* — but they can only report, never change anything.

**What you get:** Scanner looks for bugs in your code. Quality looks to add testing. Guide writes documentation for you. No changes, no issues filed — just a reading list on your dashboard (called **beads**). You will *not* see a flood of issues and PRs here — that's the point. You're reading, not reacting.

**Un-pause:** scanner, quality, guide
**Leave paused:** supervisor — and don't touch brainstorm

⚠️ **Set cadences first:** Click the gear on each agent you un-pause → set all modes to **12h or 1d**. Do this before anything else or agents will run every few minutes and burn your token budget. This is the #1 mistake new users make.

**Using the findings:** Read your dashboard beads. Pick **one finding per week** that you were already planning to fix, and fix it by hand. Ignore the rest for now — there will always be more findings than time.

**Building tests:** Notice what quality flags as missing tests. You're not acting on it yet — just learning what quality thinks your safety net needs.

**Be patient:** After changing a cadence or un-pausing an agent, wait 5–10 minutes before assuming something is wrong.

**When to move up:** **2–3 weeks minimum.** Run L2 for real: read the beads, act on a few findings yourself, and understand what the agents are seeing before you give them any write access. When you agree with their findings more than half the time, you're ready for L3. Don't rush this.

## L3 — Build Your Safety Net

**The level:** You're trusting one agent (quality) to *write code* — but every PR gets a `hold` label, so nothing merges without you.

**What you get:** Quality builds your tests, one held PR at a time. CI-maintainer joins to keep your builds healthy. This level exists to build the safety net that makes higher levels safe.

**Un-pause:** ci-maintainer (quality, scanner, guide stay on from L2)
**Leave paused:** supervisor — and still don't touch brainstorm

⚠️ **Set cadences first:** Gear icon on ci-maintainer → all modes to **12h or 1d**. Re-check the others while you're there.

**Using the findings:** Beads are still your reading list. Same rule: one finding per week, matched to what you already planned.

**Building tests:** This is quality's big moment. Expect a few PRs per week — that's normal, not slow. Review every one, even the ones you don't merge. Reading them teaches you what quality thinks is missing. Merge the good ones; these tests will later *correct the agents* and keep their output honest.

**Be patient:** After setting the level to 3 in the Governor config, give it a heartbeat cycle (5–10 minutes) before expecting PRs. Then settle in — this level is measured in weeks, not days.

**When to move up:** **3–4 weeks.** Let quality build your test suite. Get comfortable approving or declining hold-labeled PRs before adding more agents. Move up when your CI runs real tests and reviewing held PRs feels routine.

## L4 — Issues and Security

**The level:** You're trusting agents to *file issues on their own* and trusting sec-check to propose security fixes — still all hold-labeled.

**What you get:** Scanner and guide file issues automatically. Sec-check finds security holes and can open PRs (alongside quality and ci-maintainer). Automated bug reports, doc suggestions, and security findings, delivered to you.

**Un-pause:** sec-check
**Leave paused:** supervisor — brainstorm still off-limits

⚠️ **Set cadences first:** Gear icon on sec-check → all modes to **12h or 1d** before it runs. Security scans are token-hungry.

**Using the findings:** Read the issues agents file. Add a 👍 to the ones that match your priorities — the agents will pick up the signal. You don't have to respond to everything.

**Shoring up security:** Sec-check's first run will probably find things. Don't panic. Read each finding, fix the critical ones yourself, and let sec-check open PRs for the medium ones — they'll have hold labels, so you approve before anything merges.

**Be patient:** The first sec-check run can take a full cadence cycle to appear. And yes — you'll get more issues and PRs at this level. Still review them one by one. The hold label exists precisely so nothing merges without you.

**When to move up:** **4–5 weeks.** Let sec-check find and fix security issues. Watch the pattern of what agents propose. Trust is earned slowly — move up when you're approving most agent PRs without changes.

## L5 — Propose and Review

**The level:** You're trusting *every* agent to open issues and PRs — the system proposes, you decide. Every PR still has a hold label.

**What you get:** The full hive works for you. Architect produces RFCs for bigger design changes. You shift from doing the work to batch-reviewing it.

**Un-pause:** supervisor, architect — and yes, now you can un-pause brainstorm too
**Leave paused:** nothing

⚠️ **Set cadences first:** Every newly un-paused agent gets the gear treatment: all modes **12h or 1d**. More agents running = faster token burn, so this matters more than ever.

**Using the findings:** Batch-review on a schedule (say, twice a week). Approve the PRs you like, decline the ones you don't, 👍 the issues that match your roadmap.

**Building tests:** By now, quality should have already added tests for your main flows. If it hasn't, go back to L3 habits before moving on — L6 depends on it.

**Be patient:** With everything un-paused, the dashboard gets busy. Give new agents a heartbeat cycle before judging their output.

**When to move up:** Only when you **genuinely trust the agents' judgment** — meaning you've reviewed enough of their PRs to know they're consistently doing the right thing, and your test suite is strong enough that green CI genuinely means "safe to ship." There's no calendar for this one.

## L6 — Full Automation

**The level:** Full trust. Agents open PRs and merge them automatically when CI goes green. No hold label.

**What you get:** A repo that improves itself while you sleep. The tests quality built at L3 are now the guardrails that keep agents honest.

**Un-pause:** everything stays on from L5
**Leave paused:** nothing

⚠️ **Cadence check:** You can shorten cadences now if your token budget allows — but 12h/1d still works fine. Faster isn't better if you're not reading the output.

**Using the findings:** Spot-check merged PRs weekly. 👍 issues to steer agent priorities.

**Building tests:** Keep improving your tests — they're your only gatekeeper now. Every test you add makes the automation safer.

**Be patient:** Trust the loop. If a bad PR merges, that's a signal to add a test, not to panic and drop levels.

**When to move up:** There is no up. You made it. 🐝

---

## Your first week (do exactly this)

**Day 1** — Complete **Step 0**: install the Forge App and wait for the heartbeat to connect. Then start at whatever level your hive is at (probably L2). Don't touch anything else. Just watch the dashboard for 24 hours — any leftover warnings should clear on their own.

**Day 2** — In L2: un-pause **scanner**, **quality**, and **guide**. Leave **supervisor** paused. Don't touch **brainstorm**. Click the gear icon on each of the 3 agents and set the cadence to **12h or 1d**. Give it a heartbeat cycle (2–3 minutes) to take effect, then watch what they find.

**Days 3–4** — Read the advisory beads on your dashboard. These are the agents' findings: bugs, missing tests, doc gaps. Pick **one** finding you care about and act on it manually. This builds intuition.

**Days 5–7** — Keep reading, keep acting on one finding at a time. Resist the urge to move up — you're staying at L2 for **2–3 weeks**. That's not slow; that's the Hive Way.

## And after that?

**Weeks 2–3** — Stay at L2. When the agents' findings match what you'd find yourself, open the **Governor config** and set the level to **3**. Now quality can open PRs (with hold labels). Review and merge the ones you like.

**Weeks 4–7** — Live at L3 while quality builds your test suite. Then L4 for a month or so while sec-check hardens things. L5 and L6 come when trust is genuinely earned.

That's it. You now understand the full end-to-end of how Hive works — one trust level at a time. 🐝
