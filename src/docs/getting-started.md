# Zero to Automation: Your First Week with Hive

Hive is a team of AI agents that watch your repo and help improve it — finding bugs, adding tests, writing docs. It works in **levels (L1–L6)**: at low levels agents only *suggest* things, and at high levels they can open and even merge pull requests. You climb the levels as you build trust in what the agents produce. Start low, watch, then let go of the wheel a little at a time.

---

## Step 0 — Before you start (do this first!)

None of the level guidance below works until your hive is connected to GitHub. Do this in your **first session**:

1. **Install the GitHub App.** This is how Hive talks to your repo. Click **Install GitHub App** from your dashboard and grant it access to your repo.
2. **⏰ Don't put this off.** Unconfigured hive instances are reclaimed on a timer. Finish the App install in your first session or your hive may be reaped.
3. **Wait for the first heartbeat.** After installing, a heartbeat cycle has to run (a few minutes) before everything lights up green.

> **On GitHub Enterprise (IBM, corporate)?** Setup is slightly different: point Hive at your **GHE host URL**, not github.com. The App install flow lives on your enterprise host.

---

## Common gotchas (so you don't panic)

- **Dashboard full of warnings?** Normal. Most warnings clear automatically after the GitHub App is installed and the first heartbeat runs. Don't panic.
- **"Install GitHub App" gives a 404?** Usually a GHE-vs-github.com mixup, or the App isn't available on your host yet. Double-check which GitHub you're on.
- **Changed a setting and nothing happened?** Agents pick up config changes on the next **heartbeat cycle**. Wait 2–3 minutes before assuming something is broken.

---

## The four agent modes (learn these first)

| Mode | What the agent can do |
|------|----------------------|
| **Advisory** | Watch only. Posts findings to your dashboard. Never touches GitHub. |
| **Measured** | Can file GitHub issues. |
| **Holdgated** | Can open PRs, but every PR gets a `hold` label. A human must remove the hold to merge. |
| **Full** | Opens PRs and merges automatically when CI is green. |

---

## ⚠️ Token burn warning (read this before anything else)

Agents run on a schedule (a **cadence**). Short cadences = agents run constantly = your token allocation melts fast.

**When starting out, set every active agent's cadence to 12h or 1d.** Click the agent in the left menu → click the **gear icon** → change the cadence for each mode. You lose nothing — the findings will still be there when you check in.

---

## L1 — Inception

- **TL;DR:** Two agents help you think. Nothing touches GitHub.

**What's happening:** Only **guide** (writes documentation suggestions) and **brainstorm** (helps structure ideas) are active, both advisory. They observe and post thoughts to your dashboard.

**What YOU do:** Nothing! Just read what shows up on the dashboard.

**What you get:** A feel for how agents "see" your project.

**When to move up:** When the dashboard makes sense to you and you want agents that look at your actual code. Most people start at L2 anyway.

## L2 — Advisory

- **TL;DR:** Five agents watch your repo and post findings. Still no GitHub writes — you decide everything.

**What's happening:** **supervisor**, **scanner** (looks for bugs in code), **quality** (looks for missing tests), **guide** (writes documentation for you), and **brainstorm** are available — all advisory. Findings appear as **beads** on your dashboard. No issues, no PRs.

**What YOU do:**
1. Un-pause **scanner**, **quality**, and **guide**: click each one in the left menu, then click **Resume**.
2. Leave **supervisor** paused. Don't touch **brainstorm**.
3. For each of the 3 agents you un-paused: click the **gear icon** and set the cadence to **12h or 1d**.
4. Watch what they find.

**What you get:** A steady stream of findings — bugs, test gaps, doc gaps. Act on the ones you like, manually.

**When to move up:** When the agents' findings match what you'd find yourself, you trust them enough for L3.

## L3 — Quality-Gated

- **TL;DR:** The quality agent starts opening real PRs (with a safety hold) to build your test suite.

**What's happening:** **quality** becomes holdgated — it can open PRs, but each one carries a `hold` label so nothing merges without you. **ci-maintainer** joins the team.

**What YOU do:** Open the **Governor config** and set the ACMM level to **3**. Then review quality's PRs: merge the good ones, close the rest.

**What you get:** This level builds your **test infrastructure**. That matters a lot: in the future, when you add more automation, those tests will *correct the agents* — pushing agent output closer to what you need and containing better quality code.

**When to move up:** Your CI runs real tests and you're comfortably merging holdgated PRs.

## L4 — Security-Aware

- **TL;DR:** Agents start filing issues and fixing security problems. The feedback loop closes.

**What's happening:** **scanner** and **guide** open GitHub issues. **sec-check**, **quality**, and **ci-maintainer** can open PRs (still holdgated). Agents now respond to what CI and other agents report — closed-loop feedback begins.

**What YOU do:** Set ACMM level to **4**. Triage incoming issues, review PRs, keep cadences long.

**What you get:** Security findings turn into fixes without you writing them.

**When to move up:** You're approving most agent PRs without changes.

## L5 — Semi-Autonomous

- **TL;DR:** Every agent can open issues and PRs. You review in batches.

**What's happening:** All agents open issues and PRs with hold labels. **architect** produces RFCs for bigger design changes.

**What YOU do:** Set ACMM level to **5**. Shift from doing work to *reviewing* work — batch-review PRs and approve.

**What you get:** The hive does most of the work; you're the editor-in-chief.

**When to move up:** Your test suite is strong enough that green CI genuinely means "safe to ship."

## L6 — Fully Autonomous

- **TL;DR:** Agents merge their own PRs when CI is green. No hold label.

**What's happening:** Full automation. Agents open PRs and auto-merge on green CI. Your tests are the gatekeeper now, not you.

**What YOU do:** Set ACMM level to **6**. Monitor, spot-check merges, and keep improving your tests.

**What you get:** A repo that improves itself while you sleep.

---

## Your first week (do exactly this)

**Day 1** — Complete **Step 0**: install the GitHub App and wait for the heartbeat to connect. Then start at whatever level your hive is at (probably L2). Don't touch anything else. Just watch the dashboard for 24 hours — any leftover warnings should clear on their own.

**Day 2** — In L2: un-pause **scanner**, **quality**, and **guide**. Leave **supervisor** paused. Don't touch **brainstorm**. Click the gear icon on each of the 3 agents and set the cadence to **12h or 1d**. Give it a heartbeat cycle (2–3 minutes) to take effect, then watch what they find.

**Days 3–4** — Read the advisory beads on your dashboard. These are the agents' findings: bugs, missing tests, doc gaps. Pick **one** finding you care about and act on it manually. This builds intuition.

**Days 5–7** — When the agents' findings match what you'd find yourself, you're ready for L3. Open the **Governor config** and set the ACMM level to **3**. Now quality can open PRs (with hold labels). Review and merge the ones you like.

That's it. You now understand the full end-to-end of how Hive works — and you climbed one trust level in a week. 🐝
