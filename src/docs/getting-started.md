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

> 💡 **Tip: name your hive like a team member.** The `ai_author` field is the login your agents use when opening PRs. Pick a name that signals what it does — `hive-bot`, `your-repo-ai`, `proj-assistant`. A clear name means your team immediately knows which PRs are agent-authored vs human-authored.

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

## ⚠️ Cadences: the #1 thing to get right

Every agent has a gear icon (⚙️). Click it. You'll see cadence settings for each mode (advisory, measured, holdgated, full). **Set them ALL to 12h or 1d when you first start.**

Here's why: agents run on a timer. A 15-minute cadence means an agent can run **96 times a day**. At scale, that burns your token budget in hours. A 12-hour cadence means 2 runs a day — steady, useful, not ruinous.

You can always shorten cadences later once you know how fast an agent is burning through your budget. **Start long. Shorten deliberately.**

This is a journey, not a sprint. Slow cadences = sustainable automation. Fast cadences = burned budget and noise.

> 💡 **Tip: cadences aren't just about tokens.** Long cadences also mean agents produce fewer, higher-quality findings instead of flooding you with marginal ones. A scanner that runs every 15 minutes might re-flag the same issue 50 times. A scanner on a 12h cadence files it once and moves on.

---

## The advisory issue: start here every time

Every hive creates a tracking/advisory issue in your repo. This is the central log of everything the agents have found and done.

**Start here every time you check in on your hive:** open the advisory issue in your repo (it's pinned — look for it in your issues). This is your hive's journal. Every finding, every action, every note the agents leave is summarized here.

Read it like a weekly digest, not a to-do list. You don't have to act on everything — just stay oriented.

> 💡 **Tip: the advisory issue is a great standup input.** Every Monday morning, open it and skim the last week's entries. It takes 5 minutes and gives you a clear picture of what's been happening in your repo without reading every commit.

> 💡 **Power tip: feed it to your own AI tools.** The advisory issue isn't just for reading — it's the perfect input for your own agents. If you use Copilot, Claude, Cursor, or any other AI tool, paste the advisory issue into your conversation: *"Here's what my hive found this week — help me prioritize"* or *"Pick the three findings most likely to cause a production bug."* Your hive's findings + your own AI tools = faster triage. Think of the advisory issue as your hive's report card — readable by humans and by other AI agents alike.

---

## How to read hive-filed issues

Every issue a hive agent opens has the agent's name in the title — for example `[scanner] Possible nil pointer dereference in handler.go:142` or `[quality] Missing test coverage for payment flow`.

When you see a new issue in your repo, check the title prefix — it tells you which agent filed it and what kind of finding it is:

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
| **L1, L2** | No issues, no PRs. Dashboard beads only. If you see no repo activity, that's correct. |
| **L3** | **Quality only** can open PRs. Every PR has a `hold` label — it will NOT merge until you remove the hold. No other agent opens PRs at L3. |
| **L4** | Quality **and** sec-check can open PRs (both with hold labels). Scanner and guide file issues — not PRs. |
| **L5** | All agents can open PRs, all with hold labels. Nothing auto-merges. You batch-review. |
| **L6** | PRs auto-merge when CI goes green. No hold labels. Full automation. |

> **The hold label is your safety net.** At every level below L6, every PR an agent opens is blocked from merging until you remove the `hold` label. You are always in control. Nothing ships without your approval until you reach L6 — and you'll only reach L6 after months of trusting the system.

> **L6 is earned, not chased.** Auto-merge is not the goal you're optimizing for on day one — it's the outcome you earn after months of watching agents work and building confidence in their judgment. Most teams spend months at L4 and L5. That's not failure — that's the system working as designed.

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

**Using the findings:** Read your dashboard beads and the pinned **advisory issue** in your repo. Pick **one finding per week** that you were already planning to fix, and fix it by hand. Ignore the rest for now — there will always be more findings than time.

**Building tests:** Notice what quality flags as missing tests. You're not acting on it yet — just learning what quality thinks your safety net needs.

**Be patient:** After changing a cadence or un-pausing an agent, wait 5–10 minutes before assuming something is wrong.

> 💡 **Tip: one agent at a time.** When you un-pause agents at a new level, don't un-pause them all on the same day. Un-pause one, watch it for 3–4 days, then add the next. You'll understand each agent's personality before the noise from multiple agents overlaps.

**When to move up:** **2–3 weeks minimum.** Run L2 for real: read the beads, act on a few findings yourself, and understand what the agents are seeing before you give them any write access. When you agree with their findings more than half the time, you're ready for L3. Don't rush this.

## L3 — Build Your Safety Net

**The level:** You're trusting one agent (quality) to *write code* — but every PR gets a `hold` label, so nothing merges without you.

**What you get:** Quality builds your tests, one held PR at a time. CI-maintainer joins to keep your builds healthy. This level exists to build the safety net that makes higher levels safe.

**Un-pause:** ci-maintainer (quality, scanner, guide stay on from L2)
**Leave paused:** supervisor — and still don't touch brainstorm

⚠️ **Set cadences first:** Gear icon on ci-maintainer → all modes to **12h or 1d**. Re-check the others while you're there.

**Using the findings:** Beads are still your reading list. Same rule: one finding per week, matched to what you already planned.

**Building tests:** This is quality's big moment. Expect a few PRs per week — that's normal, not slow. Review every one, even the ones you don't merge. Reading them teaches you what quality thinks is missing. Merge the good ones; these tests will later *correct the agents* and keep their output honest.

> 💡 **Tip: the 3-issue rule.** When quality starts opening PRs, don't review them all at once. Pick the three smallest ones. Merge one, decline one, leave one. This teaches the agent what you value faster than reading 20 PRs and ignoring them all.

> 💡 **Tip: the hold label is a conversation.** You don't just approve or decline a held PR. You can edit it, add comments, push commits on top of it. The agent will see your changes on the next run and learn from the diff.

**Be patient:** After setting the level to 3 in the Governor config, give it a heartbeat cycle (5–10 minutes) before expecting PRs. Then settle in — this level is measured in weeks, not days.

**When to move up:** **3–4 weeks.** Let quality build your test suite. Get comfortable approving or declining hold-labeled PRs before adding more agents. Move up when your CI runs real tests and reviewing held PRs feels routine.

> 💡 **Tip: customize before you escalate.** Before moving from L3 to L4, take 30 minutes to edit each agent's policy template. Add your coding conventions, your preferred test framework, your off-limits directories. Agents follow instructions literally — the more specific you are, the better the output.

## L4 — Issues and Security

**The level:** You're trusting agents to *file issues on their own* and trusting sec-check to propose security fixes — still all hold-labeled.

**What you get:** Scanner and guide file issues automatically. Sec-check finds security holes and can open PRs (alongside quality and ci-maintainer). Automated bug reports, doc suggestions, and security findings, delivered to you.

**Un-pause:** sec-check
**Leave paused:** supervisor — brainstorm still off-limits

⚠️ **Set cadences first:** Gear icon on sec-check → all modes to **12h or 1d** before it runs. Security scans are token-hungry.

**Using the findings:** Read the issues agents file. Add a 👍 to the ones that match your priorities — the agents will pick up the signal. You don't have to respond to everything.

> 💡 **Tip: your advisory issue is a living document.** Don't just read it — react to findings with 👍/👎. Agents use these signals to prioritize. A finding with 5 👍 reactions gets picked up sooner than one with none.

> 💡 **Tip: scanner finds, quality fixes.** Scanner flags bugs. Quality fixes test gaps. They're a team. At L4, watch for scanner filing an issue and quality filing a PR that addresses it — that's the closed-loop feedback working.

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

> 💡 **Tip: batch-review in one sitting.** Reviewing ten agent PRs in a single hour teaches you the agents' patterns faster than reviewing one per day. Patterns jump out when the PRs sit side by side — repeated habits, favorite files, blind spots.

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

> 💡 **Tip: L6 doesn't mean you're done reviewing.** Even at full automation, read the merged PRs weekly. Not to intervene — to calibrate. If you see a pattern you don't like (agents always touching the same file, tests that are too shallow), update the policy template and the next run corrects course.

**When to move up:** There is no up. You made it. 🐝

---

## Your first week (do exactly this)

**Day 1** — Complete **Step 0**: install the Forge App and wait for the heartbeat to connect. Then start at whatever level your hive is at (probably L2). Don't touch anything else. Just watch the dashboard for 24 hours — any leftover warnings should clear on their own.

**Day 2** — In L2: un-pause **scanner**, **quality**, and **guide**. Leave **supervisor** paused. Don't touch **brainstorm**. Click the gear icon on each of the 3 agents and set the cadence to **12h or 1d**. Give it a heartbeat cycle (2–3 minutes) to take effect, then watch what they find.

**Days 3–4** — Read the advisory beads on your dashboard, and open the pinned **advisory issue** in your repo — your hive's journal. Pick **one** finding you care about and act on it manually. This builds intuition.

**Days 5–7** — Keep reading, keep acting on one finding at a time. Resist the urge to move up — you're staying at L2 for **2–3 weeks**. That's not slow; that's the Hive Way.

## And after that?

**Weeks 2–3** — Stay at L2. When the agents' findings match what you'd find yourself, open the **Governor config** and set the level to **3**. Now quality can open PRs (with hold labels). Review and merge the ones you like.

**Weeks 4–7** — Live at L3 while quality builds your test suite. Then L4 for a month or so while sec-check hardens things. L5 and L6 come when trust is genuinely earned.

This is a marathon, not a sprint. The teams who get the most out of Hive are the ones who resist the urge to rush. Set long cadences. Read the advisory issue weekly. **Move up a level when it feels boring — not when it feels exciting.**

That's it. You now understand the full end-to-end of how Hive works — one trust level at a time. 🐝

---

## Tips, Tricks & Where to Find Things

**Cadences:** Click the ⚙️ gear on any agent in the left sidebar → "Edit Agent" or "Configure" → cadence settings. Set each mode (advisory, measured, holdgated, full) to 12h or 1d. Do this for every agent you un-pause. You can come back and shorten later.

**Agent prompts:** Each agent has a policy template — the instructions it follows. Click the agent in the sidebar → gear icon → "Edit Policy" (or "Edit Prompt"). You can customize what the agent focuses on. Example: tell quality to focus only on your `/api` package, not your `/frontend` package. Be specific — agents follow instructions literally.

**Filtering which issues agents can work on:** In your hive config (Governor settings), you can set issue label filters — agents will only pick up issues that match specific labels. This is how you keep agents from running off and fixing things you didn't want touched. Add a label like `hive-ok` to issues you're comfortable with agents acting on. Set the filter to `hive-ok`. Now agents only work issues you've explicitly blessed.

**Reading the advisory issue:** Search your issues for `[hive]` or look for a pinned issue titled something like "Hive Advisory" or "Hive Tracking". This is your hive's running log. Bookmark it.

**Pointing other AI tools at hive findings:** Copy the advisory issue URL and paste it into Claude, Copilot, or Cursor: *"Summarize the most critical findings from this issue and suggest which three I should act on this week."* Your hive's findings + your preferred AI = faster triage.

**Checking what agents are doing:** Live Activity feed (dashboard → Activity tab) shows what's running right now. If an agent looks stuck, check cadence — it may just not be scheduled to run yet.

**If something looks wrong:** Wait 10 minutes. Most issues are just agents waiting for their next heartbeat cycle. If it's still wrong after 10 minutes, check the advisory issue for error messages before filing a support request.

> 💡 **Tip: pausing is free.** Pausing an agent never loses its findings or its learning — everything picks up where it left off. Going on vacation? Big refactor week? Pause aggressively and resume when you're back. The hive waits for you.

> 💡 **Tip: dropping a level is a feature, not a failure.** Planning a big architectural change? Drop from L4 to L3 (or L3 to L2) while the dust settles, then move back up. Levels are a dial, not a ladder you fall off.
