# Work-source terminology

Hive can read work from GitHub today and has adapter paths for GitLab, Gitea / Forgejo, Linear, Jira, and other trackers. User-facing copy should name GitHub only when a feature is genuinely GitHub-specific.

## Glossary

- **Issue** — the neutral work item term. Jira users may say **ticket**; introduce "issue (ticket)" once when writing Jira-focused docs.
- **Change request / PR / MR** — use **pull/merge request** in docs and explanatory copy. Short dashboard labels can keep **PR** where the UI is summarizing GitHub- and GitLab-style change requests.
- **Work source** — the system Hive reads work items from: GitHub Issues, GitHub Projects, Linear, Jira, Spektacular runs, and similar systems.
- **Project / repository** — use **project** when the concept might be Linear/Jira/GitLab/Gitea. Use **repository** only for source control storage or clone targets.
- **Provider** — the connected service or forge instance, such as GitHub.com, GitHub Enterprise, GitLab, Gitea, Linear, or Jira.

## Audit table

| Term | Where | Count reviewed | Verdict | Replacement / rule |
| --- | --- | ---: | --- | --- |
| GitHub Issues / GitHub issue | Dashboard, hub, docs | 18 | Neutralize unless naming the default adapter | **issues** or **default issues**; keep **GitHub Issues** only in adapter names and setup docs. |
| GitHub PR / PRs | Dashboard, hub, docs | 64 | Mixed | Use **pull/merge request** in docs and explanatory copy; keep **PR** for compact dashboard metrics and GitHub-only implementation details. |
| Repos / repositories | Dashboard, hub, README, getting started | 120+ | Mixed | Use **projects** for user-facing managed-work surfaces; keep **repository/repo** for source control clone targets, config keys, examples, and GitHub-specific setup. |
| GitHub App / GitHub OAuth / device flow | Dashboard, hosted hub, setup docs | 40+ | Keep GitHub-specific | GitHub App auth is currently the production write path; surrounding copy now says provider/forge app when speaking generically. |
| GitHub Projects | Work-source settings, integration lists | 12 | Keep GitHub-specific | This is a product name for one adapter. |
| Linear/Jira team→repo mapping | Work-source settings | 9 | Neutralize partially | Label as **source repository** so tracker users understand the clone target without implying the work source is GitHub. |
| green CI / merge on green | Hub, README, getting started | 10 | Neutralize | **when checks pass** except architecture/history text explaining the GitHub implementation. |
| Checks / Actions / Dependabot | Setup docs, architecture docs | 30+ | Keep GitHub-specific where product-specific | These refer to GitHub permissions or GitHub-origin dependency alerts. Use **checks** generically outside setup. |
| github.com links | Dashboard/hub templates | 100+ | Keep/flag | Static source-code, docs, and GitHub-login links are intentional. Follow-up: dashboard chat autolinks and hub public-hive avatars still concatenate `https://github.com/` for GitHub identity/profile affordances. |
| gh CLI / PAT / GITHUB_TOKEN | README and setup docs | 50+ | Keep GitHub-specific | These are setup mechanisms for the current GitHub write path; do not use them in generic onboarding copy. |

## Changes made in this PR

- Spoke dashboard visible labels now say **Projects** for the managed-work card section and welcome checklist, while IDs, CSS classes, API routes, and config tab keys remain unchanged.
- Work-source settings explain GitHub Issues as the default adapter but describe Linear and Jira clone targets as **source repositories**.
- Hub portal hero, onboarding wizard, comparison, and ACMM copy now prefer **project**, **work source**, **pull/merge request**, and **checks pass** where the feature is not GitHub-only.
- README, `src/README.md`, `CONTRIBUTING.md`, and getting-started copy now use neutral wording in the first generic introduction points.

## Deliberately kept GitHub-specific

- GitHub App setup, OAuth/device login, GitHub Enterprise, token/PAT, Checks, Actions, and GitHub Projects copy because those flows and permissions are GitHub-specific today.
- Source-code links to `github.com/hivecommons/hive` and GitHub profile links because they intentionally leave Hive for GitHub.
- Config keys such as `github`, `repo`, `primary_repo`, and API fields because this PR changes copy only, not identifiers or behavior.

## Follow-up candidates

The wider docs corpus contains many historical issue links, GitHub-specific runbooks, and implementation notes. Future PRs can neutralize long-form docs that explain generic concepts with GitHub-only nouns, especially architecture walkthroughs and older operator guides, while leaving GitHub setup pages explicit.
