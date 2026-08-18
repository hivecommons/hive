# Zero to Automation: Your First Week with Hive

Hive is a team of AI agents that watch your repo and help improve it — finding bugs, adding tests, writing docs. It works in **levels (L1–L6)**: at low levels agents only *suggest* things, and at high levels they can open and even merge pull requests. You climb the levels as you build trust in what the agents produce. Start low, watch, then let go of the wheel a little at a time.

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

## L1 — Getting started

- **TL;DR:** Two agents help you think. Nothing touches your repo yet — it's all just advice.

**What's happening:** **Guide** helps you structure your ideas. **Brainstorm** helps turn raw thoughts into real plans. They observe and post thoughts to your dashboard.

**What YOU do:** Nothing! Just read what shows up on the dashboard.

**What you get:** A feel for how agents "see" your project.

**When to move up:** When the dashboard makes sense to you and you want agents that look at your actual code. Most people start at L2 anyway.

## L2 — Agents wake up and watch

- **TL;DR:** Agents start watching your repo and reporting what they find. No changes, no issues, nothing automatic — you decide everything.

**What's happening:** **Scanner** looks for bugs in your code. **Quality** looks to add testing. **Guide** writes documentation for you. **Supervisor** and **brainstorm** are also available. They're just *watching and reporting* — no changes to your repo, no issues filed, nothing automatic. Findings appear as **beads** on your dashboard. You read what they find and decide what to do.

**What YOU do:**
1. Un-pause **scanner**, **quality**, and **guide**: click each one in the left menu, then click **Resume**.
2. Leave **supervisor** paused. Don't touch **brainstorm**.
3. For each of the 3 agents you un-paused: click the **gear icon** and set the cadence to **12h or 1d**.
4. Watch what they find.

**What you get:** A steady stream of findings — bugs, test gaps, doc gaps. Act on the ones you like, manually.

**When to move up:** When the agents' findings match what you'd find yourself, you trust them enough for L3.

## L3 — Building your safety net

- **TL;DR:** Quality is now allowed to open PRs (with a safety hold) to build your test suite.

**What's happening:** **Quality** can open PRs — each one gets a `hold` label, so a human has to approve it before it merges. **CI-maintainer** joins to keep your builds healthy. This level is about building the safety net that makes higher levels safe.

**What YOU do:** Open the **Governor config** and set the level to **3**. Then review quality's PRs: merge the good ones, close the rest.

**What you get:** This is where your test suite starts getting built. That matters a lot: in the future, when you add more automation, those tests will *correct the agents* — pushing agent output closer to what you need and containing better quality code.

**When to move up:** Your CI runs real tests and you're comfortably merging held PRs.

## L4 — Agents start filing issues

- **TL;DR:** Agents file issues on their own, and the security agent joins. You still approve everything before it merges.

**What's happening:** Agents start filing issues on their own. The security agent (**sec-check**) joins and can open PRs, alongside **quality** and **ci-maintainer** (all still hold-labeled). You're now getting automated bug reports, doc suggestions, and security findings.

**What YOU do:** Set the level to **4**. Triage incoming issues, review PRs, keep cadences long.

**What you get:** Security findings turn into fixes without you writing them.

**When to move up:** You're approving most agent PRs without changes.

## L5 — The system proposes, you decide

- **TL;DR:** Agents open PRs freely (all with hold labels). You batch-review them.

**What's happening:** Agents open PRs freely, but every PR has a hold label. **Architect** produces RFCs for bigger design changes. The system is proposing; you're still deciding.

**What YOU do:** Set the level to **5**. Batch-review the PRs — approve the ones you like, decline the ones you don't.

**What you get:** The hive does most of the work; you're the editor-in-chief.

**When to move up:** Your test suite is strong enough that green CI genuinely means "safe to ship."

## L6 — Full automation

- **TL;DR:** Agents open PRs and merge them automatically when CI goes green.

**What's happening:** Full automation. You trust the system. You've built up to this — the tests from L3 are now the guardrails that keep agents honest.

**What YOU do:** Set the level to **6**. Monitor, spot-check merges, and keep improving your tests.

**What you get:** A repo that improves itself while you sleep.

---

## Your first week (do exactly this)

**Day 1** — Complete **Step 0**: install the Forge App and wait for the heartbeat to connect. Then start at whatever level your hive is at (probably L2). Don't touch anything else. Just watch the dashboard for 24 hours — any leftover warnings should clear on their own.

**Day 2** — In L2: un-pause **scanner**, **quality**, and **guide**. Leave **supervisor** paused. Don't touch **brainstorm**. Click the gear icon on each of the 3 agents and set the cadence to **12h or 1d**. Give it a heartbeat cycle (2–3 minutes) to take effect, then watch what they find.

**Days 3–4** — Read the advisory beads on your dashboard. These are the agents' findings: bugs, missing tests, doc gaps. Pick **one** finding you care about and act on it manually. This builds intuition.

**Days 5–7** — When the agents' findings match what you'd find yourself, you're ready for L3. Open the **Governor config** and set the level to **3**. Now quality can open PRs (with hold labels). Review and merge the ones you like.

That's it. You now understand the full end-to-end of how Hive works — and you climbed one trust level in a week. 🐝
