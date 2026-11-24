# Dashboard UI map

Where things are in the Hive dashboard, using the words the screen shows. Use
this page to find a section or a Setting by name, then follow the link for the
full explanation. For what each section means, see
[Dashboard sections explained](dashboard-sections.md); for every Settings tab,
see [Dashboard settings explained](settings.md); for the words the dashboard
uses, see the [dashboard glossary](dashboard-glossary.md).

The tables under [The map](#the-map) are generated from the dashboard source
(the sidebar markup, the section help registry and the Settings tab lists) by
`src/pkg/dashboard/uimap/render-dashboard-ui-map.py`, and CI fails when they go stale, so
the labels here match the labels on screen. The rest of the page is written by
hand.

## Signing in

Which sign-in you need depends on where the dashboard runs.

**The hosted hub** (<https://hive.hivecommons.dev>). Click **Sign in** or open
<https://hive.hivecommons.dev/login>, then pick a provider: the hosted hub
offers **Continue with GitHub**, **Continue with Google**, **Continue with
IBMid** and **Continue with Microsoft**. After signing in, **My Hives**
(`/dashboard`, also `/my-hives`) lists your hives. A self-hosted hub shows only
the providers its operator configured, and goes straight to the provider when
there is only one; see
[Self-hosted hub deployment](hub-deployment.md#minimal-hub-environment). The
full hosted walkthrough is [Hosted Hive Hub onboarding](hosted-hub.md#1-sign-in).

**A hosted hive's dashboard** (`<id>.hive.hivecommons.dev`). Open it from
**My Hives** on the hub. The hub checks your hub sign-in before it lets a
request through to the hive, so there is no second login; if you are not
signed in you land on the hub sign-in page.

**A self-hosted hive's dashboard.** Click **Sign in** in the top bar. The
dashboard opens **Sign in with GitHub** and shows a short code: click **Copy**,
then **Open GitHub →**, enter the code at <https://github.com/login/device> and
approve. The dashboard notices the approval and signs you in. If it shows
**GitHub App Setup Required** instead, the hive has no GitHub App or OAuth
client configured yet; follow the choice it offers. Who may sign in, and which
paths stay public, are set under `dashboard:` in `hive.yaml`; see
[Dashboard auth and access problems](troubleshooting.md#dashboard-auth-and-access-problems).

Signing in to the dashboard is not the same as logging in an agent's coding
tool. The **Login** buttons on an agent card, and `/login` inside an agent's
terminal, sign in that agent's CLI (for example Copilot or Claude); see
[Troubleshooting](troubleshooting.md).

## Common actions

| To do this | Go here |
|---|---|
| Open Settings | **⚙ Settings** in the top bar, or **Settings** in the account menu. A link of the form `<dashboard>/#config/governor/<tab>` opens a given tab ([Linking to a setting](settings.md#linking-to-a-setting)). |
| Find a setting by name | Open Settings and type in its search box. |
| Get help | **? Help** in the top bar opens a menu with the same entries as the sidebar Help group: **🚀 Getting started** (the setup checklist, for users who can change the hive; it links to the full guide), **📖 Docs** (docs.hivecommons.dev, new tab), **❓ FAQ** and **🐛 Report an issue**. Arrow keys move through it and Esc closes it. |
| Add a repository | Settings, **Repos** tab: type `org/repo` or a GitHub URL, click **+ Add**, then **Save & close**. See [Repos](settings.md#repos). |
| Pause or resume one agent | Sidebar, **Agents**, **Control panel**: on the agent's card click **⏸ pause**, and **▶ resume** to lift it. Owner only. See [Resuming a paused agent](dashboard.md#resuming-a-paused-agent). |
| Pause every agent at once | The fleet breaker chip in the top bar. Releasing it restores only the agents the breaker paused. |
| Configure one agent | The ⚙ button on the agent's card opens its settings tabs (listed below). |
| Add an agent | Sidebar, **Agents** group: **＋ Add agent**. |
| Change the ACMM level | The level chip in the top bar, or Settings, **General**. See [ACMM levels](agent-configuration.md#acmm-levels-agent-rosters-as-packs). |
| See what is waiting on you | Sidebar, **Dashboard**, **Hive activity monitor**. |
| Approve or reject a proposed action | The **Approvals** section. |
| Change the dashboard theme | Settings, **Appearance**. See [Appearance themes](dashboard.md#appearance-themes). |
| Choose which sections show and in what order | Drag a section by its ⠿ grip, or reorder the **Dashboard** group in the sidebar. See [Reorder sections](dashboard.md#reorder-sections). |
| Read what a section means | The **?** mark beside its heading opens its entry in [Dashboard sections explained](dashboard-sections.md). |
| Report a bug | Sidebar, **Help**, **Report an Issue**. |

## The map

<!-- BEGIN GENERATED: dashboard-ui-map (src/pkg/dashboard/uimap/render-dashboard-ui-map.py) -->

### Sidebar

| Sidebar group | Entry | Section heading | What it shows | Help |
|---|---|---|---|---|
| Dashboard | **Overview** | Overview | How many issues and pull requests Hive is tracking right now, and how many of them it can work on. | [Overview](dashboard-sections.md#overview) |
| Dashboard | **Hive activity monitor** | Hive activity monitor | A progress strip per open issue and pull request showing which step of the pipeline it is on and who must act next. | [Hive activity monitor](dashboard-sections.md#hive-activity-monitor) |
| Dashboard | **Governor** | Governor | How busy Hive thinks your projects are right now, and how often it wakes its workers as a result. | [Governor](dashboard-sections.md#governor) |
| Dashboard | **Throughput** | Throughput | How many pull requests were opened, merged and closed over a chosen time window, and who did the work. | [Throughput](dashboard-sections.md#throughput) |
| Collaborate | **Inbox** | Inbox | Voice recordings and dictations waiting for you to file them as an RFC or keep as knowledge. | [Inbox](dashboard-sections.md#inbox) |
| Collaborate | **Calendar** | Calendar | Meetings hive will join, about a minute before each starts: add one, approve or decline requests, or skip and cancel. | [Calendar](dashboard-sections.md#calendar) |
| Collaborate | **Meetings** | Meetings | Meetings hive joins to take notes: send it to one, follow the live transcript, notes and research, or make it leave. | [Meetings](dashboard-sections.md#meetings) |
| Collaborate | **Customers** (only when switched on) | Customers | The people you build for: who each one is, what they are trying to get done, and how it is going for them. | [Customers](dashboard-sections.md#customers) |
| Collaborate | **Inception (spek)** (only when switched on) | Inception (spek) | Named terminal sessions running command-line AI tools inside Hive; everyone who can see one watches live and one person at a time holds the keyboard. | [Inception (spek)](dashboard-sections.md#inception-spek) |
| Agents | **Control panel** | Control panel | One card per worker with its status and buttons to pause, restart or configure it. | [Control panel](dashboard-sections.md#control-panel) |
| Agents | **Activity** | Activity | One line per worker showing what it is doing now, how long since it last acted, and which issues it has reserved. | [Activity](dashboard-sections.md#activity) |
| Agents | **All agents (n)**: one entry per agent, plus **＋ Add agent** and **＋ Group** | | Opens that agent's card. Collapsing the list keeps the entries above it visible. | [Control panel](dashboard-sections.md#control-panel) |
| Resources | **Projects** | Projects | One card per repository Hive looks after, listing its open issues and pull requests grouped by what each one needs next. | [Projects](dashboard-sections.md#projects) |
| Resources | **Contributors** | Contributors | The people who lend their computers or AI helpers to your projects, and how far Hive trusts each of them. | [Contributors](dashboard-sections.md#contributors) |
| Intelligence | **Advisory** | Advisory | Reports, advice and suggestions Hive has written about your projects and about how it is running. | [Advisory](dashboard-sections.md#advisory) |
| Intelligence | **ACMM Eval** | ACMM Eval | How ready your repositories are for Hive to work on them alone, scored against a published checklist. | [ACMM Eval](dashboard-sections.md#acmm-eval) |
| Intelligence | **Knowledge** | Knowledge | The facts Hive has learned about your projects, where they came from, and how much Hive trusts each one. | [Knowledge](dashboard-sections.md#knowledge) |
| Intelligence | **Experiment Lab (nous)** (only when switched on) | Experiment Lab | An experimental planner that tries out code changes in your repositories and records what it learned. | [Experiment Lab](dashboard-sections.md#experiment-lab) |
| Pull requests | **Review** | Review | Review Pipeline shows pull requests by stage first; Review Queue lists the same open pull requests in Hive's suggested review order. | [Review](dashboard-sections.md#review) |
| Pull requests | **PR timeline** | PR timeline | A timeline of recent pull requests, from when they were opened to when they were merged or got stuck. | [PR timeline](dashboard-sections.md#pr-timeline) |
| Usage | **Tokens** | Tokens | How much text the AI models read and wrote for Hive recently, broken down by model and by worker. | [Tokens](dashboard-sections.md#tokens) |
| Usage | **Cost** | Cost | What Hive's use of AI models would cost at public per-token list prices; an estimate, not a bill. | [Cost](dashboard-sections.md#cost) |
| Troubleshooting | **Diagnostics** | Diagnostics | Health checks for Hive itself, such as its connections, credentials and safety switches, to help you find out why something is not working. | [Diagnostics](dashboard-sections.md#diagnostics) |
| Troubleshooting | **Audit log** | Audit log | A searchable record of everything Hive has done, newest first. The collapsed “sensitive” badge opens a 24h sensitive-only view, matching the server-side risk count. | [Audit log](dashboard-sections.md#audit-log) |
| Help | **Getting started** | | A link or dialog, not a dashboard section. | |
| Help | **Ask Hive** | | A link or dialog, not a dashboard section. | |
| Help | **FAQ** | FAQ | Short answers to the questions people ask once a hive is running, each linking to the guide that explains it fully. | [FAQ](dashboard-sections.md#faq) |
| Help | **Docs** | | A link or dialog, not a dashboard section. | |
| Help | **API reference** | | A link or dialog, not a dashboard section. | |
| Help | **Report an issue** | | A link or dialog, not a dashboard section. | |
| Community | **Discord** | | A link or dialog, not a dashboard section. | |
| Community | **Live on Twitch** | | A link or dialog, not a dashboard section. | |

### Settings tabs

Open **⚙ Settings** in the top bar. The tabs, in the order the dialog shows them:

| Tab | Explained in |
|---|---|
| **General** | [Settings: General](settings.md#general) |
| **Appearance** | [Settings: Appearance](settings.md#appearance) |
| **Dashboard** | [Settings: Dashboard](settings.md#dashboard) |
| **Agents** | [Settings: Agents](settings.md#agents) |
| **Thresholds** | [Settings: Thresholds](settings.md#thresholds) |
| **Labels** | [Settings: Labels](settings.md#labels) |
| **Repos** | [Settings: Repos](settings.md#repos) |
| **Workflows** | [Settings: Workflows](settings.md#workflows) |
| **Budget** | [Settings: Budget](settings.md#budget) |
| **Notifications** | [Settings: Notifications](settings.md#notifications) |
| **Health** | [Settings: Health](settings.md#health) |
| **Sensing** | [Settings: Sensing](settings.md#sensing) |
| **Logging** | [Settings: Logging](settings.md#logging) |
| **Features** | [Settings: Features](settings.md#features) |
| **Knowledge** | [Settings: Knowledge](settings.md#knowledge) |
| **Inbox** | [Settings: Inbox](settings.md#inbox) |
| **Project Observability** | [Settings: Project Observability](settings.md#project-observability) |
| **Meetings** | [Settings: Meetings](settings.md#meetings) |
| **Meeting attendee** | [Settings: Meeting attendee](settings.md#meeting-attendee) |
| **Advisory** | [Settings: Advisory](settings.md#advisory) |
| **Work Source** | [Settings: Work Source](settings.md#work-source) |
| **Hub** | [Settings: Hub](settings.md#hub) |
| **Forge App** | [Settings: Forge App](settings.md#forge-app) |
| **Model Gateways** | [Settings: Model Gateways](settings.md#model-gateways) |
| **Bob** | [Settings: Bob](settings.md#bob) |
| **Variables** | [Settings: Variables](settings.md#variables) |
| **Security** | [Settings: Security and Access](settings.md#security-and-access) |
| **Compliance** | [Settings: Compliance](settings.md#compliance) |

### Agent settings tabs

Open an agent's settings from its card (the ⚙ button) or from **＋ Add agent**. The tabs:

**General**, **Cadences**, **Pipeline**, **Hooks**, **Restrictions**, **Channels**, **Tools**, **Connections**, **Variables**, **Stats**, **Prompt Template**, **Prior Prompts**, **Export**.

<!-- END GENERATED: dashboard-ui-map -->

Sections without a sidebar entry (for example **Governor** sub-cards such as
**PRs by model**, **Advisory Digest** and **Hive Advice**) appear inside the
section above them and are explained in
[Dashboard sections explained](dashboard-sections.md).
