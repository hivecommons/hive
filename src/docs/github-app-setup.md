# GitHub App setup

> **Terminology:** the dashboard and docs call this the **Forge App** — the app Hive installs on your forge (your source control system, e.g., GitHub, GitHub Enterprise, GitLab, or Gitea). On **GitHub.com and GitHub Enterprise (GHE)** the Forge App **is a GitHub App**; this page covers creating and installing it. Dashboard controls live under **Governor Config → Forge App**.

> **On GitLab, Gitea, or Forgejo?** This page does not apply, and there is no
> equivalent setup to perform: those forges are **not supported for running a
> hive** today. The adapters exist in the source tree but are not wired into any
> running code path, and the agent execution path is GitHub-only. See
> [Forge setup: GitLab, Gitea, and Forgejo](forge-app-setup.md) for exactly what
> is and is not implemented before you attempt an install.

Hive can authenticate with either a personal access token or a GitHub App. Use a GitHub App for production hives because installation tokens are scoped to selected repositories and can author PRs as the app bot when `github.app_authored_prs` is enabled.

> **A PAT is enough only for L1–L2. L3 and above need a GitHub App.** Agents do not write to GitHub themselves. They write request files ([`hive-open-issue`](hive-open-issue.md), [`hive-open-pr`](hive-open-pr.md), review, merge and push-branch requests), and the hive's request relays carry them out. The relays start only when the hive has a usable App: `github.app_id` set and a non-zero `github.installation_id` (`HasUsableApp()`). Without one, no agent can file, comment on or claim an issue, open a PR, post a review verdict or request a merge, at any level. The requests pile up on disk and nothing processes them. Nothing in the log or dashboard warns about this: the hive's own writes, such as stage labels and the advisory digest comment, still go through the PAT, so the hive looks partly healthy. Once a usable App is configured, the relays start on the next GitHub client rebuild, with no restart needed. L1–L2 agents are advisory and file nothing, so a PAT covers them.

## Personal access token (PAT) scopes

The PAT path is `HIVE_GITHUB_TOKEN` (or `github.token` in `hive.yaml`); the App path is `github.app_id`/`github.key_file` as described below. If you set `app_id`/`key_file` this section does not apply — the App permission table further down does.

Hive never validates token scopes at startup. A PAT with missing scopes fails **at request time** with generic GitHub 403 responses (fine-grained PATs typically say `Resource not accessible by personal access token`). The visible symptoms are agents reporting no actionable work, advisory digests not appearing on the tracking issue, empty fleet-stats widgets, or failed issue/PR writes in agent logs — none of which name the missing scope. If you see unexplained 403s, check the scopes below first.

### Classic PATs

Classic scopes are coarse, so one scope covers the hive's own writes at every ACMM tier. At L3 and above, the agents' writes also need a GitHub App (see the note above). The PAT scopes in the L3–L6 rows cover only the hive's own calls:

| ACMM tier | Minimum classic scopes | Why |
| --- | --- | --- |
| L1–L2 (advisory) | `repo` (private repos) or `public_repo` (public repos only) | Read issues/PRs/contents; post advisory digest comments to the pinned tracking issue (an issues **write**, even though the tier is otherwise read-only). A PAT alone is enough at this tier. |
| L3–L4 (issue filing) | same, **plus a usable GitHub App** | The hive's own issue writes (stage labels, digest comments). Agent issue filing, comments and claims go through the App-gated issue relay, so they need the App. |
| L5–L6 (PR/merge) | same, plus `workflow` if the hive's own pushes may touch `.github/workflows/`, **plus a usable GitHub App** | The hive's own reads of checks/statuses and its direct writes. Agent branch pushes, PRs, reviews and merges go through the App-gated relays and use the App installation token. |

Add `read:org` when Hive should identify org members and contributor roles (recommended for org-owned hives).

### Fine-grained PATs

Fine-grained PATs are supported — the token is sent as a plain bearer, same as a classic PAT. Grant the token access to every repository the hive works on, with repository permissions mirroring the App table below. As with classic PATs, the L3–L4 and L5–L6 columns cover the hive's own calls only. Agent issues, claims, reviews, PRs and merges at those tiers need a usable GitHub App, because the request relays do not start without one:

| Permission | L1–L2 (advisory) | L3–L4 (issues) | L5–L6 (PR/merge) |
| --- | --- | --- | --- |
| Metadata | Read | Read | Read |
| Contents | Read | Read | Read and write |
| Issues | Read and write | Read and write | Read and write |
| Pull requests | Read | Read | Read and write |
| Checks | Read | Read | Read |
| Actions | Read and write | Read and write | Read and write — needed by the default-on gated-run approval sweep (`governor.ci.auto_approve_workflows`); Read suffices with it set to `false`, see below |
| Commit statuses | Read | Read | Read and write — needed when `governor.labels.automerge` is the default `lgtm` on a Prow repository, so the queue action can set the `prow/lgtm` status beside the label. With only Read, every queue action still applies the label but posts a PR comment that the status could not be set. |

Since the gated-run approval sweep (`governor.ci.auto_approve_workflows`, **default on**, [#16603](https://github.com/hivecommons/hive/issues/16603)) Hive approves `action_required` `pull_request` runs with the App token — `POST /actions/runs/{id}/approve`, once per PR head, for Dependabot/Renovate/App-authored or -triggered runs and for heads a verified owner or merger admitted from the dashboard — which needs **Actions: Read and write**. With only **Actions: Read** the call fails with `403`, the hive records one `permission_error` in `/api/status` `workflow_approvals`, logs `workflow approval refused; needs workflow approval on GitHub`, and the CI caption reads `CI waiting · needs workflow approval on GitHub`; the refusal is recorded per PR head and not retried, so granting the permission afterwards takes effect for new heads (or after a hive restart), while an already-refused head needs **Approve and run** on GitHub or a fresh push; set `governor.ci.auto_approve_workflows: false` to stay on Read and leave approvals to the `/ok-to-test` workflow described below. The other direct approval Hive performs — a contributor-lane PR whose author the contributor registry admitted at a `require_tiers` tier (`auto_merge.contributor_lane.approve_held_runs_for_registry_trusted`, default on) — needs the same grant. For that path it is **optional**: without it the call fails with `403 Resource not accessible by integration`, the hive labels the PR `hive:unauthorized`, posts one comment naming the permission and the human step (**Approve and run** on the checks banner or Actions tab, with the run ids), logs `lane unauthorized` once per head, parks the PR at 085 (`Awaiting human: approve workflow runs` in the activity monitor), and clears all of it the tick the required checks report ([#14589](https://github.com/hivecommons/hive/issues/14589)). When GitHub leaves same-repository `pull_request` runs in `action_required` on trusted Dependabot/Renovate/App PRs or registry-trusted `author/clanker` PRs, and the approval sweep is off or lacks the grant, Hive posts `/ok-to-test` once for that head and reports `workflow-approval-required:<ctx>` / `stage/085-needs-decision` until the repository's own self-contained `hive-ok-to-test` workflow approves the held runs. The approval is performed by that repository's `GITHUB_TOKEN` through `.github/workflows/hive-ok-to-test.yml`; that path needs no Prow and no Hive App Actions-write grant, and it never covers fork heads. Fork heads are reached only by the two App-token paths above — the gated-run approval sweep and the registry-trusted contributor lane — since GitHub holds only fork `pull_request` runs behind the gate; a trusted head's workflow files then run on the repository's runners, which is what the **Actions: Read and write** grant buys. No path approves runs on PRs carrying `sentinel-alert`.

Install the workflow in each governed repository: run `hivectl ok-to-test-template > .github/workflows/hive-ok-to-test.yml` against your hive (it reads the App bot login from `/api/config`; pass `--app-login <your-app>[bot]` to override), or copy `src/docs/templates/hive-ok-to-test.yml` and replace `HIVE_APP_LOGIN[bot]` with the App bot login shown by Hive for that installation.

The rendered workflow is **self-contained**: it triggers on `issue_comment`, runs with job-level `permissions: actions: write, contents: read, issues: write, pull-requests: read`, and performs the whole approval in one `bash` + `gh` step with the repository `GITHUB_TOKEN`. It carries no `uses: hivecommons/hive/...` reference, so it works in repositories whose Actions policy only allows selected actions/reusable workflows, on GitHub Enterprise Server, and in organizations that do not permit workflows from `hivecommons`. The step:

1. exits quietly unless the comment is on a pull request and its body is exactly the command (`/ok-to-test` by default);
2. requires the commenter to be a configured Hive App login or to hold write/maintain/admin (`GET /repos/{owner}/{repo}/collaborators/{login}/permission`);
3. requires the PR head repository to equal the base repository (same-repo only) and the PR author to be in `TRUSTED_AUTHORS` (`dependabot[bot],renovate[bot]` plus the App login(s) by default);
4. refuses when the `sentinel-alert` label is present;
5. lists the head SHA's `status=action_required` runs and `POST /repos/{owner}/{repo}/actions/runs/{id}/approve`s each, then posts one summary comment carrying an idempotent marker (re-runs for the same head only add a 👍 reaction);
6. fails the job with `::error::` if the token cannot approve runs, so a missing `actions: write` grant or a read-only workflow-permissions setting is visible instead of silent.

Organization permission: **Members: Read** where contributor/owner identification is configured. Note that Issues read-and-write is needed even at advisory tiers because digests are posted as issue comments.

## Create the app

In GitHub, open **Settings → Developer settings → GitHub Apps → New GitHub App** (or the equivalent organization settings page). On **GitHub Enterprise**, do this on your enterprise host (`https://<your-ghe-host>/settings/apps/new`), not github.com — the app, its install page (`https://<your-ghe-host>/github-apps/<app-slug>`), and the source control host Hive is configured for must all be the same host.

Recommended values:

- **GitHub App name**: any unique operator-owned name.
- **Homepage URL**: your project or Hive dashboard URL.
- **Setup URL** (optional): `https://<hive-host>/gh-setup`. `<hive-host>` means
  **the address you type into your browser to reach this hive** — not where the
  hive process runs. GitHub redirects *your browser* here; it never fetches this
  URL itself, and neither does the hive. See
  [Choosing a Setup URL](#choosing-a-setup-url) before setting it, and leave it
  blank if the hive is not reachable from the browser you administer it with —
  Hive discovers `installation_id` on its own.
- **Redirect on update**: enabled — but only alongside a Setup URL that is
  actually reachable. It re-fires the redirect on every repository add or
  removal, so an unreachable Setup URL produces a dead browser tab every time
  you change the installation, not just once at install.
- **Webhook**: inactive unless you separately configure webhook channels; the dashboard setup flow does not require webhooks.
- **Device Flow**: enabled, so dashboard login can use the app's client ID.
- **Visibility**: private for an organization-specific app; public only if you intentionally operate one app for many unrelated owners.

Repository permissions used by the dashboard setup UI are:

| Permission | Level | Why |
| --- | --- | --- |
| Metadata | Read-only | Required by GitHub. |
| Contents | Read/write | Clone, branch, and push agent changes. |
| Issues | Read/write | Enumerate, comment on, create, and close issues. |
| Pull requests | Read/write | Create, update, approve/merge, and inspect PRs. |
| Checks | Read-only | Monitor CI status. |
| Actions | Read and write (Read-only with `governor.ci.auto_approve_workflows: false`) | Inspect workflow runs; the default-on approval sweep approves gated `pull_request` runs for trusted bots, the App and dashboard-admitted heads with the App token ([#16603](https://github.com/hivecommons/hive/issues/16603)). If the sweep is off or refused and GitHub holds trusted Dependabot/App/clanker runs in `action_required`, Hive posts `/ok-to-test`; the repository-local `hive-ok-to-test` workflow uses its own `GITHUB_TOKEN` with Actions write to approve those runs. With Read and write, the contributor lane also approves held runs directly for a PR author the contributor registry admitted at a `require_tiers` tier (`auto_merge.contributor_lane.approve_held_runs_for_registry_trusted`). Optional: without it the hive marks such a PR `hive:unauthorized` and waits for a maintainer to click **Approve and run** ([#14589](https://github.com/hivecommons/hive/issues/14589)). |
| Administration | Read-only | Read branch-protection rules before the merge-request watcher lands a PR; Hive refuses closed if it cannot verify the base branch is protected. |
| Commit statuses | Read/write | Needed when `governor.labels.automerge` is the default `lgtm` on a repository running Prow's commit-bound `lgtm` (prow-github-actions v3): the queue action sets a `prow/lgtm` success status on the approved head so Prow does not strip the label. Without the write grant the label and approval still land, but every queue action takes the failure branch and posts a PR comment saying the status could not be set. Choose a distinct label such as `hive/auto-merge` if you would rather keep this at Read. |
| Dependabot alerts | Read-only | Required for the sec-check agent's Dependabot scanning (`gh api repos/<org>/<repo>/dependabot/alerts`, [#17426](https://github.com/hivecommons/hive/issues/17426)). Least privilege: only the token minted for an agent with role `sec-check` at ISSUES_AND_PRS or ISSUES_PRS_MERGE ([#17496](https://github.com/hivecommons/hive/issues/17496)) requests `vulnerability_alerts: read`; every other agent's token is unchanged. The App itself must still declare the permission, and that is a GitHub UI change only the App owner can make — see [Adding a permission to an existing App](#adding-a-permission-to-an-existing-app). Optional: until it is declared and re-accepted, the sec-check mint logs `scoped token mint with vulnerability_alerts:read failed; retrying without it`, mints without the grant, and the Dependabot call returns 403; the hive still mints tokens and every other agent is unaffected. |
| Workflows | Read/write | Let trusted-tier agents push branches that modify `.github/workflows/`, and let the contributor lane merge an owner-approved PR that edits `.github/workflows/**` (`auto_merge.contributor_lane.owner_approval_releases_workflow_fence`, [#14664](https://github.com/hivecommons/hive/issues/14664)); without the grant such a merge is refused and the hive marks the PR `hive:unauthorized` (`merge-workflow-path`) and parks it at 090 for a maintainer's **Merge** click. Without it GitHub also rejects any GitHub App-token push touching those files server-side ("refusing to allow a GitHub App to create or update workflow … without workflows permission") no matter what the token requests. Maintainer user credentials are separate and can still push workflow-file changes when their account has the normal repository rights; the Hive App cannot until this grant is added **and re-accepted on each installation** (an existing install must approve the new permission under Settings → Integrations → the app → Review request). See [`hive-open-pr`](hive-open-pr.md#workflow-file-pushes-rejected-by-app-tokens) for the agent delivery procedure and [#6985](https://github.com/hivecommons/hive/issues/6985) for the outstanding permission change. |

Organization permission:

| Permission | Level | Why |
| --- | --- | --- |
| Members | Read-only | Identify contributors and owners where configured. |

### Adding a permission to an existing App

Hive does not ship an App manifest and never edits the App's permissions for
you: GitHub exposes no API for changing a GitHub App's declared permissions, so
when a Hive release starts requesting a grant the App does not yet declare
(Workflows in [#6985](https://github.com/hivecommons/hive/issues/6985),
Dependabot alerts in [#17426](https://github.com/hivecommons/hive/issues/17426)),
the token mint is refused with `422 The permissions requested are not granted
to this installation`, Hive logs a warning, retries without the grant, and the
feature that needed it degrades (a 403 on that API call) until two people have
clicked through the GitHub UI:

1. **The App owner** (whoever owns the App under **Developer settings**, e.g.
   the organization that created it) opens **Settings → Developer settings →
   GitHub Apps → _your app_ → Permissions & events → Repository
   permissions**, sets the new permission (for example **Dependabot alerts:
   Read-only**), and clicks **Save changes** at the bottom of the page.
   Nothing changes for any installation until this is saved.
2. **Each installation owner** then sees **Permission updates requested** on
   the installation page — **Settings → Integrations → Applications →
   _your app_ → Configure** for a user account, or **Organization settings →
   Third-party access → GitHub Apps → _your app_ → Configure** for an
   organization — and must click **Review request → Accept new permissions**.
   GitHub also emails the installation owner. Until an installation accepts,
   that installation keeps minting without the grant; installations that have
   accepted get it on their next mint without a hive restart.

The hive confirms the grant landed when the `scoped token mint with
vulnerability_alerts:read failed` (or `workflows:write failed`) warning stops
appearing in the spoke log on the next mint for that tier.

### Optional shared-CI recovery

The shared-CI rerun sweep (`shared_ci_rerun.enabled`, default on) can inspect
failed runs with Actions read, but `rerun-failed-jobs` requires **Actions: Read
and write** on the repository's recovery identity. This is optional, not a new
baseline permission for every Hive App. A refusal is an operator handoff, not
a source-code defect: Hive records `unauthorized:rerun-shared-ci`, posts one
head-scoped actionable note using the existing unauthorized mechanism, and
retries no more than once per repository per permission cooldown (one hour).
An unchanged PR remains eligible after the cooldown; a successful rerun or
observing no failed runs clears the recovery block.

A maintainer may re-run the named workflow with their already-authorized
identity. That manual recovery does **not** change the App's permissions. For
automatic recovery, the App owner must explicitly request Actions read and
write for the intended recovery installation, and its installation owner must
accept the update in **Settings → GitHub Apps → Configure**. Do not broaden
unrelated Apps or repositories or silently substitute a PAT. Leaving the grant
at read is supported; an operator may disable the optional sweep instead.
Neither action grants merge approval or bypasses required checks.

### The optional Visual Hive App (#4030)

Visual Hive needs two grants the Hive App does not otherwise require — Actions
at write (to dispatch its installed workflow) and Commit statuses at write (to
publish the provenance-bound setup authorization status). The Hive App only
asks for Commit statuses write for the `prow/lgtm` auto-merge binding above,
and never for Actions write. Rather than widen the
Hive App, those grants live in a separate, optional, KubeStellar-owned App, so
enabling Visual Hive never changes what an installation that will never use it
has approved.

| App | App ID | Forge |
| --- | --- | --- |
| `kubestellar-viz-hive` | 4729416 | github.com |
| `kubestellar-viz-hive-ghe` | 5945 | github.ibm.com |

Its permission set is exactly:

| Permission | Level | Why |
| --- | --- | --- |
| Actions | Read/write | Dispatch the installed Visual Hive workflow. |
| Commit statuses | Read/write | Publish the setup authorization status. |
| Metadata | Read-only | Required by GitHub. |

Nothing else — no Contents, Workflows, Issues, Pull requests or Checks — and no
webhooks. Install it on **selected repositories**, not all repositories.

These identities are declared in `src/pkg/config/provenance.go` and are inert on
this branch: nothing resolves a key for them, delivers one, or authenticates as
them. Registering them ahead of the feature only means a config naming one is
described accurately instead of reported as an unknown App.

#### Reading the granted Actions and Commit-statuses permissions

App auth classification turns solely on the Issues permission, and an
installation that has not been granted the two Visual Hive permissions is
healthy — its `state` is `ok`. That is correct, and it is also why those grants
have to be reported separately: without it, an installation that has approved
them and one that has not are indistinguishable.

Every credential verdict now logs what is actually granted, including when the
verdict is `ok`:

```
github app credential verdict owner=<org> state=ok grants="actions=read statuses=none" visual_hive_execution_grants=false
```

`grants` reports the level GitHub returned for each permission (`none` when
there is no grant), and `visual_hive_execution_grants` is true only when both
are at `write`. Emitting it for a healthy verdict is the point: an installation
that has not approved a permission update is the one that still looks fine, and
GitHub keeps an App on its old permissions until an org owner accepts, so a
partly approved fleet is otherwise invisible.

Permission verdicts are computed where a GitHub call has failed and on the
dashboard's **Re-check** button, not on every eval cycle. Repository coverage
is checked separately on the self-heal loop, including on a healthy hive, as
described below. To read a specific installation's grants on demand — for
example to confirm the demo/canary install — press Re-check on that hive and
read the line above from its logs.

## PR-contents gate posted by the hive

From 2026-12-02 GitHub blocks `pull_request_target` workflows by default in public repositories. A repository whose required `verify PR contents` check comes from such a workflow would stop reporting it, and its PRs would park as `required-check-missing`. The hive can post that gate itself as a **commit status** on the exact PR head, so customers do not have to change anything in their repositories. It needs the Hive App's **Commit statuses: Read and write** grant (the same optional grant the `prow/lgtm` binding uses above).

It is configured in `hive.yaml` only, ships **off**, and needs an explicit repository list:

```yaml
auto_merge:
  pr_gate:
    mode: shadow            # off (default) | shadow | enforce
    repos: [your-org/your-repo]
```

- `off` posts nothing and changes nothing.
- `shadow` posts only `verify PR contents (hive)` (a name no branch rule requires) on each PR head and compares the verdicts with the workflow's own `PR Type`, `PR Issue` and `verify PR contents` results. Nothing a branch rule reads can change.
- `enforce` posts the real names `PR Type`, `PR Issue` and `verify PR contents` on each PR head, with the verdicts the workflow records: `PR Type` and `PR Issue` carry the title verdict, and `verify PR contents` is green unless a verdict could not be recorded on that head (then it is red and says which one, and the next event retries). It does so only while the grant check has confirmed the installation's Commit statuses write grant. Until then it runs as `shadow`; with the grant known missing it posts nothing and raises the banner below.

The verdict logic is a Go port of the PR title verifier (`PR Type`, `PR Issue`); both implementations replay one shared golden corpus in CI, so they cannot drift. The poster can write exactly four contexts: `verify PR contents`, `PR Type`, `PR Issue` and `verify PR contents (hive)`. Any other context, `DCO` included, is refused in code before a request is made. Drafts and closed PRs are skipped, the head is re-read from GitHub before posting, and an identical status is never posted twice.

**Backstop sweep.** Every 10 minutes the hive lists the open PRs (one page, most recently updated first) of each repository in scope and posts the gate on any non-draft head that does not carry it yet: a webhook lost while the hive restarted, a burst it dropped, or a `github.token` autofix push. It reads a head's statuses from GitHub before posting, so a restart does not repost every head, and a head whose title changed while the hive was down gets the new verdict. Each missed head goes through the same path as a webhook event (the listed head is pinned, the PR is re-read, drafts and closed PRs are skipped), at most 25 heads are posted per sweep, and a repository in `off`, with the grant missing or in its 403 cooldown costs no request. The log line is `pr-gate: backstop posted the gate on a head the hive had missed`. From the cutoff (2026-12-02), when the grant is still missing and none of a repository's three most recent non-draft heads carries `verify PR contents` (as a check run or a status), the sweep logs `pr-gate: verify PR contents is missing on recent heads: the GitHub App lacks the statuses: write permission`, which the fleet report files under `github_app_statuses_permission_missing`. A head that still carries the context (the Actions policy allows `pull_request_target`) raises nothing.

**Grant check and visible failure.** The gate never fails silently. At startup and every 15 minutes the hive reads the Commit statuses permission the installation was actually granted (an owner must accept a permission change before an installation holds it). A missing grant forces the gate off, never promotes it to `enforce`, and logs `pr-gate: the installation has no Commit statuses write grant, the gate is forced off`. A 403 on a status write is recorded per repository, counted, and not retried for 15 minutes. While either holds, the dashboard shows a **PR-contents gate cannot be posted** banner that names the installation, says what is missing and where an owner accepts it (the App's pending permission request in the organization settings), and counts the days left before the cutoff. The hub's hive health shows the same cause with a remediation hint: red when a required check depends on the gate, amber otherwise (see [fleet health](fleet-health.md#pr-gate-cannot-be-posted)). In `enforce`, a PR whose required gate context is missing for this reason shows the held reason `unauthorized:post-pr-gate-status` ("the hive's GitHub App is not permitted to post pr gate status") instead of a bare `required-check-missing`.

**Fork-head canary.** A commit status is written to the base repository with the base installation's token, also for a fork PR (the fork's commit is reachable through the PR's ref) and for a Dependabot PR. The hive records the evidence from its own writes: `/api/status` carries `prGate.canary` with the latest gate write per head origin (`same-repo`, `fork`, `dependabot`), each with the repository, PR, head, context, result (`posted`, `forbidden` or `error`) and `lastSuccessHead`, the latest head GitHub accepted. The first accepted write per origin logs `pr-gate: canary: a gate status landed on a head of this origin`. Before switching a repository that takes fork or Dependabot PRs to `enforce`, run it in `shadow` until `prGate.canary.fork.lastSuccessHead` (and `dependabot` where it applies) is set, then confirm on GitHub that the head's statuses list `verify PR contents (hive)`: `gh api repos/<owner>/<repo>/commits/<head>/statuses --jq '.[].context'`.

**Verify in shadow.** Open or edit a PR in a listed repository. Within seconds its **Checks** box shows a status named `verify PR contents (hive)` whose description reads `PR Type: success; PR Issue: success` (or `failure`). In the hive log, `pr-gate: status posted` names the repository, head and context, and `pr-gate: comparison with the workflow result agrees` (or `DISAGREES`, logged as a warning) appears when the workflow's check lands on the same head. A `pr-gate: status refused with 403` line means the installation has no Commit statuses write grant: accept the App's pending permission request in the organization settings (see [Adding a permission to an existing App](#adding-a-permission-to-an-existing-app)).

### Switch a repository to enforce

Do this before 2026-12-02 for every repository whose required `verify PR contents` check comes from `pr-verifier.yml`. Each step is an operator action; the hive changes no setting itself.

1. **Grant.** In the organization settings, GitHub Apps, open the Hive App and confirm **Commit statuses: Read and write** is granted (accept a pending permission request if there is one). The hive log line `github app credential verdict ... grants="... statuses=write ..."` confirms it.
2. **Shadow first.** Set `mode: shadow` with the repository listed, open or edit a few PRs (include a fork PR if the repository takes them), and check that `verify PR contents (hive)` appears on each head and the log shows `pr-gate: comparison with the workflow result agrees` with no `DISAGREES` line.
3. **Native-queue gate.** If the repository runs `native-queue-policy.yml`, set the repository variable `HIVE_PR_GATE_CREATOR_LOGIN` to the Hive App's bot login (for example `your-app[bot]`, the creator shown on the hive's statuses), so the `gate` job accepts the hive's `verify PR contents` status (see [native merge queue](native-merge-queue.md)).
4. **Enforce.** Set `mode: enforce`. On the next event for a PR, its head shows `PR Type`, `PR Issue` and `verify PR contents` as commit statuses created by the Hive App, next to the workflow's check runs of the same names while the workflow still runs. `/api/status` shows `prGate.effective: enforce`; `shadow` there means the grant is not confirmed yet, `off` means it is missing (the banner names the installation).
5. **Pin the context to the App (recommended).** In the branch rule or ruleset, set the source of the required `verify PR contents` check to the Hive App instead of "any source". Only the Hive App's status then satisfies it. Do this only after step 4 shows the hive's status on new heads: from that moment the workflow's own check run no longer counts, and a repository whose hive is in `shadow` or `off` would block every merge.
6. **Roll back** by setting `mode: shadow` (or `off`) and, if step 5 was done, setting the check's source back to any source.

## Install and configure Hive

1. After creating the app, note the **App ID**, **Client ID**, and app slug from the app page/URL.
2. Generate a private key and mount it into the hive, for example `./secrets/gh-app-key.pem` on the host mounted as `/secrets/gh-app-key.pem`.
3. Install the app on the organization/repositories Hive manages.
4. Configure Hive:

```yaml
github:
  app_id: <app-id>
  installation_id: <installation-id>  # optional; auto-discovered (see below)
  app_slug: <app-slug>
  key_file: /secrets/gh-app-key.pem
  oauth_client_id: <client-id>
```

For GitHub Enterprise, also set `api_url`/`base_url` or the supported `forge` value so install URLs and API calls target the same host.

### Signed commits on agent PRs (`app_signed_commits`)

Agents commit with plain `git` in their pane and cannot sign: a GitHub App has no account to hold a GPG or SSH key. A base branch whose ruleset **requires signed commits** therefore blocks every agent PR from merging, however many approvals it has. GitHub does sign commits it creates itself — commits authored through the `createCommitOnBranch` GraphQL mutation are GPG-signed by GitHub and shown as **Verified**, and a GitHub App may author them directly.

```yaml
github:
  app_signed_commits: true   # opt-in; needs an installed App
```

In the dashboard the same switch is **Settings → Forge App → Re-author agent PRs as GitHub-signed commits** (Owner role required to save).

With it on, the PR-request watcher (the choke point every `hive-open-pr` request passes through) re-authors the head branch before it opens the PR: it reads the `base...head` diff, builds one commit through the mutation with the App installation token — same tree, the agents' original commit messages and DCO trailers as the message — force-updates the branch to it, and then opens the PR. The PR arrives with a single commit, signed by GitHub, authored by `<app_slug>[bot]`, that a `required_signatures` rule accepts.

**DCO.** The mutation takes no author: GitHub stamps its own bot account on the commit (`<app_slug>[bot] <id+slug[bot]@users.noreply.github.com>`). That is fine for the DCO checks - probot-dco exempts a commit whose GitHub author resolves to a Bot account, and Prow's `dco` plugin only requires a `Signed-off-by` line to be present. The `Signed-off-by` trailers copied from the agents' commits are what the hive controls, and they are re-addressed to the bracket-free form the pane identity uses since #6276: a `<slug>[bot]@users.noreply.github.com` sign-off (with or without GitHub's numeric prefix) becomes `<slug>@hive.kubestellar.io` (`HIVE_GIT_BOT_EMAIL_DOMAIN`), because probot-dco rejects the bracketed local-part as "not a valid email address" wherever it does validate it (#6251). Every other sign-off address, a human's or the legacy `hive-bot@kubestellar.io`, is copied unchanged.

What it does not do, and falls back on (the PR still opens on the agent's own commits, the reason lands in the request's `.result.json` as `signed_skipped` and in the hive log at WARN):

- changes the mutation cannot express: executable bits, symlinks, submodule pointers;
- changes above 20 MiB of file content, or past the compare API's 300-file list;
- a head branch that moved (the agent pushed again) between reading the diff and updating the ref — nothing is replaced that the signed commit does not carry;
- a head branch that already has an open PR — that PR is reused untouched at open time, and the follow-up pass below signs it instead.

**Follow-up commits.** An agent's later pushes to its open PR (a CI fix, review follow-ups, a rebase) are plain git and unsigned, and so is a PR that opened on a `signed_skipped` fallback. With `app_signed_commits` on, the PR-request watcher also runs a reconcile pass, at most once a minute (#9364). A PR needs signing when *any* of its commits is not **Verified**, not only its head: a person can sign their own commit on top of an agent's unsigned one, which leaves the head Verified while the PR is still blocked (#9531). For every open PR authored by the App bot, the pass finds the first unverified commit (oldest to newest) and re-authors everything from there through the head as one GitHub-signed commit on top of the commit right before it. The tree is the same, and the message carries the range's messages and DCO trailers, re-addressed as above. When the PR's first commit is the unverified one, it signs against the PR's base branch, as at open time. A fully Verified PR is left alone. The open path's guards all still apply. A head that moves mid-rewrite is retried on the next pass, and so is a head GitHub could not be reached for (a 5xx or network error). A head GitHub *refuses* to sign (a 4xx answer, such as the 422 on the scratch ref in #16193) is retried on three consecutive passes and then settled, with the `hive:signed-commits-blocked` comment quoting the error; there is no person's commit to squash in that case, so the remedy is to push a new head (which restarts signing) or fix the repository-side cause the error names. A PR whose head hasn't changed since the last pass costs no calls beyond the open-PR list.

The pass re-signs only the hive's own work. Every commit in that range, Verified or not, must have an author and committer that are the App bot or an address in the pane identity domain (`HIVE_GIT_BOT_EMAIL_DOMAIN`, default `hive.kubestellar.io`). Verified App-bot commits in the range are folded in. A person's commit (even a Verified one on top), or a merge commit, leaves the branch as pushed. The PR gets one comment (marked `<!-- hive:signed-commits-blocked -->`, posted once per PR) naming the blocker. To unblock it, squash the unsigned commit into a commit the person signs, or drop the person's commit so the hive can re-sign the range and then push it again, signed, on top.

Changes the signing mutation cannot safely express also leave the branch untouched and get a single comment with the reason. For example, it cannot preserve executable files, symlinks or submodule pointers. A contributor can re-author the unsigned commits with their own verified signature, preserving their contents and modes, and push the signed branch. These deliberate skips are logged at INFO rather than as API failures; a new head is examined again.

**Agents after a push.** The rewrite replaces the agent's commits on the remote, so its local branch no longer matches, even though the tree is identical. Before committing again on a branch with an open PR, run `git fetch && git rebase origin/<branch>`. The rebase drops the now-empty local commits and keeps any new work. Never force-push over the signed commit: that puts the unsigned commits back, and the next pass has to sign them again.

Leave it off (the default) on hives whose base branches do not require signatures; it rewrites agent branches for no benefit there.

## Choosing a Setup URL

The Setup URL is a **browser redirect target**, not a callback GitHub's servers
make. After an install — and, with **Redirect on update** enabled, after every
repository add or removal — GitHub sends whatever browser you were using to
that address. So the only question that matters is:

> Can the browser I administer this hive from open that URL?

That is a property of how *you* reach the hive, not of where the hive runs:

| How you reach the dashboard | Correct Setup URL |
| --- | --- |
| Desktop session on the hive machine | `http://localhost:3001/gh-setup` |
| SSH tunnel (`ssh -L 3001:localhost:3001 you@hive`) | `http://localhost:3001/gh-setup` — the tunnel makes `localhost` genuinely the hive |
| Another machine on the network | `http://<hive-lan-name-or-ip>:3001/gh-setup` |
| Public hostname behind TLS | `https://<hive-host>/gh-setup` |
| Not reachable from your browser at all | **leave it blank** — see below |

`localhost` is the common trap. It is correct only for the first two rows, and
it fails silently everywhere else: set it while sitting at the hive machine and
it works, then administer the hive from a laptop and every install-update
redirect lands on the laptop's own port 3001, which is nothing at all.

### Headless, NATed, and remotely administered hives

**The Setup URL is optional. A hive that is not reachable from anywhere should
leave it blank and turn Redirect on update off.** Nothing is lost.

The callback's only job is to save you from pasting an `installation_id`, and
Hive already resolves that itself: it asks GitHub `GET /orgs/{org}/installation`
authenticated with the **App JWT**, falling back to walking
`GET /app/installations`. That runs when the hive starts, whenever App
credentials are (re)loaded, and when you press **Re-check** on the dashboard's
Forge App panel — adopting and persisting an unambiguous match, so it corrects a
missing *or wrong* `installation_id` without any redirect. An ambiguous or
absent result leaves your configuration untouched rather than guessing.

Every one of those calls is **outbound, hive to GitHub**. A headless Fedora
CoreOS host, a VM on a NAT network, or any server you only reach over SSH needs
no inbound reachability for GitHub App authentication to work. The
preconditions are the ones App auth already has: the private key mounted at
`key_file`, and `project.org` set so Hive knows whose installation to look for.

If you do leave a Setup URL configured that your browser cannot reach, the
resulting dead redirect is cosmetic. Adding a repository modifies the existing
installation rather than creating a new one, so the `installation_id` in that
redirect is one the hive already holds — the failed page does not mean App
authentication is broken.

An installation can authenticate successfully while excluding a private
repository under **Only select repositories**. When adding a repo in the
dashboard, use the reported installation settings link to add it to the
selection, then re-check access. For repos already configured, the Forge App
banner identifies missing repository coverage; update the installation and
re-check it. A rejected advisory issue creation can then be retried without
waiting for the next UTC day.

Boot coverage checks have a time limit; a timeout is inconclusive. The
background self-heal loop rechecks installation coverage every two minutes,
including on a healthy hive, using the cached installation token. Each check
walks up to 10 pages of `GET /installation/repositories`. If that bounded
listing is incomplete, the dashboard checks the repository's App installation
directly. Temporary GitHub errors still block adding the repository until a
check succeeds.

## `/gh-setup` flow

When the app's Setup URL points at `https://<hive-host>/gh-setup`, GitHub redirects back with `setup_action` and, for install/update, `installation_id`.

Hive accepts `setup_action=install` and `setup_action=update`, verifies that:

- a real `github.app_id` is configured,
- a private key file is resolvable,
- the installation ID can mint/verify a GitHub App installation, and
- the installation account matches the configured project org.

If verification succeeds, Hive persists `github.installation_id`, reinitializes the GitHub client, and redirects the browser to `/?ghSetup=ok`. `setup_action=request` records a pending approval redirect but does not configure an installation.

The setup endpoint is intentionally public because GitHub opens it in a browser that may not have a Hive session. The query string is not trusted; verification is done with the app key and GitHub API.

## Rotation and recovery

To rotate a private key, generate a new key in GitHub, mount it at the configured `key_file`, restart Hive, then delete the old key in GitHub after the new one is confirmed working. If an installation was replaced, use `/gh-setup` again or update `github.installation_id` and restart/reload the hive.

## API quota: conditional requests and post-reset pacing

An installation token has an hourly REST allowance (about 6,650 requests for a 16-repo installation) shared by everything that uses that installation — the hive's own sweeps and every agent's scoped token. Two behaviours in the shared HTTP transport keep the hive's share small ([#7430](https://github.com/hivecommons/hive/issues/7430)):

- **Conditional requests.** Every repeat `GET` of a resource the hive has already fetched carries `If-None-Match` with the remembered `ETag`; GitHub answers `304 Not Modified`, which is **not charged** against the quota, and the hive replays the remembered body. Unchanged issue and PR lists — the common case between two governor cycles — cost nothing. Cached bodies are keyed by the requesting identity as well as the URL, so an agent's scoped token never reads a body the App fetched. A replayed response carries `X-Hive-Cache: revalidated`. The cache is bounded — `github.etag_cache_max_mb` (default `128` MiB of gzip-compressed bodies, env `HIVE_GITHUB_ETAG_CACHE_MAX_MB`) and `github.etag_cache_max_entries` (default `8192`) — and evicts least-recently-used bodies past either bound. A body evicted before its next poll costs a charged fetch instead of a free `304`, so a hive whose hit rate slides while **Debug → GitHub API budget → ETag cache** sits at the cap should raise the cap; the change applies from Settings → Forge App without a restart.
- **Post-reset pacing.** When GitHub answers a primary-limit `403` (`X-RateLimit-Remaining: 0`), the hive does not fire everything it had queued the instant the window resets — that is how a spoke re-exhausted its quota one second after every reset. For ten minutes after the reset, requests are spaced ~2 s apart (the same slow start already used after a secondary limit), so the new window is spent gradually.

If a hive is still exhausting its quota, look for consumers outside the hive process on the same installation: agents' `gh` calls and the API proxy count against it too.

## Hub-distributed App keys (hosted fleet) — operator runbook

On a hosted fleet the hub — not the hive owner — is the App-key authority. The hub keeps one PEM per cluster at `/data/saas/app-keys/<clusterID>.pem` (owner-only file modes, on the same PVC as the hub's other secrets) and reconciles it to every spoke on that cluster over the heartbeat. A claimed hive whose cluster has no stored key is delivered `key_delivered=false` at claim time and then receives nothing on any beat — it stays `Degraded` on `key-missing` forever until an operator uploads a key. **The hive owner cannot see, supply, or fix this key**; every owner-facing surface deliberately stays silent for the operator-side states (`key-missing`, `key-invalid`, `no-app-assigned`).

### Uploading (or replacing) a cluster's App key

The upload endpoint is the ONLY way key material enters the hub. Admin-gated:

```
PUT /api/saas/admin/cluster-app-keys/{clusterID}
Content-Type: application/json

{"private_key": "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----", "app_id": 123456}
```

- `private_key` (required) — the GitHub App's PEM private key. Validated before it is persisted; a non-PEM or unparsable key is rejected with 400 and nothing is stored.
- `app_id` (optional) — the App's numeric ID; when supplied it is persisted alongside in `clusters.json` so the cluster carries a complete identity.
- An unknown `{clusterID}` returns 404.

The response echoes back only the key's **fingerprint** (never the key), so you can verify the right key landed:

```json
{"cluster_id": "oke-frankfurt-1", "app_id": 123456, "has_key": true, "fingerprint": "sha256:..."}
```

Compare that fingerprint with one computed locally from the PEM you meant to upload. Delivery to the spokes then happens automatically on the next heartbeats — no restart needed.

### Checking which clusters hold a key

```
GET /api/saas/admin/cluster-app-keys
```

returns `[{cluster_id, app_id, has_key, fingerprint}, ...]` for every cluster. A cluster with `has_key: false` will strand the first App-requiring hive claimed onto it — check this *before* pointing a pool at a new cluster.

### The `app-creds-undelivered` fleet alert

When a claimed, online hive reports an operator-side App-credential state, the hub raises a **critical** fleet alert (type `app-creds-undelivered`) in the dashboard's "Attention needed" panel. The alert names the hive and its cluster, shows how long it has been stranded, and carries the exact `PUT` remedy above. One alert per hive; it clears automatically once the key is delivered and the spoke reports healthy. The states it covers:

- `key-missing` — no key ever reached the spoke (usually: the cluster has no stored PEM — upload one).
- `key-invalid` — a key is present but GitHub rejects the JWTs it signs (it belongs to a different App — replace it with the correct PEM).
- `no-app-assigned` — the hive still carries the placeholder `app_id` and was never assigned a real App (assign the cluster's App and upload its key).
