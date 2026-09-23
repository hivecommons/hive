# Hive Commons repositories

Hive is developed alongside a small set of sibling repositories in the
[hivecommons](https://github.com/hivecommons) GitHub organization. This page is
the authoritative list of those repositories, what each one is for, and its
current status, so that users and downstream reviewers can see what the project
produces beyond this repository (OpenSSF Baseline OSPS-QA-04).

Status values: **Active** (released, maintained, in the release train),
**Supporting** (maintained, no independent release cadence), **Early
development** (newly added, pre-release), **Placeholder** (exists for
infrastructure reasons only).

| Repository | Intent | Status | Compiled into a Hive release? |
| --- | --- | --- | --- |
| [hive](https://github.com/hivecommons/hive) | The Hive orchestrator: governor, agent manager, dashboard, hub, contributor relay. Publishes the `hive`, `hive-hub`, and `hive-contributor` container images. | Active | Yes, this is the release |
| [pluk](https://github.com/hivecommons/pluk) | Lightweight pub-sub event streaming for tmux sessions running AI coding agents. Used by the agent manager to observe agent sessions. | Active | Yes, installed in the `hive` image |
| [hotshot](https://github.com/hivecommons/hotshot) | Screenshot capture that lands directly in an AI coding assistant's terminal. Developer tooling used by contributors and agents. | Active | No, standalone tool |
| [promptargs](https://github.com/hivecommons/promptargs) | Template expansion and variable substitution for AI prompts across Claude Code, Copilot, Goose, Bob and others. | Active | No, standalone tool |
| [rationguard](https://github.com/hivecommons/rationguard) | Detects and rebuts rationalization patterns in AI agent output. Standalone tool for reviewing agent transcripts. | Active | No, standalone tool |
| [dibs](https://github.com/hivecommons/dibs) | Contributor attribution layer for AI-agent-era contributions: your idea, your credit, their code. | Active | No, standalone service |
| [spektacular](https://github.com/hivecommons/spektacular) | Spec-driven development for AI coding agents: a markdown spec becomes a reviewed plan and an agent-driven implementation (Claude Code, Bob, Codex). | Early development | No, standalone binary |
| [homebrew-hive](https://github.com/hivecommons/homebrew-hive) | Homebrew tap for one-command install of the Hive contribute app. | Supporting | No, packaging only |
| [docs](https://github.com/hivecommons/docs) | Source for [docs.hivecommons.dev](https://docs.hivecommons.dev). Hive, hotshot, pluk, rationguard and promptargs docs are single-sourced from their repositories at build time. | Supporting | No |
| [spektacular-website](https://github.com/hivecommons/spektacular-website) | Source for [spektacular.dev](https://spektacular.dev). | Early development | No |
| [infra](https://github.com/hivecommons/infra) | Shared CI workflows, Prow configuration and org automation. | Supporting | No |
| [.github](https://github.com/hivecommons/.github) | Organization profile and community health files (code of conduct, security policy, issue templates). | Supporting | No |
| [hive-redirect](https://github.com/hivecommons/hive-redirect) | GitHub Pages redirect from `hive.hivecommons.dev` to the hosted hub. | Placeholder | No |
| [hivecommons.github.io](https://github.com/hivecommons/hivecommons.github.io) | Placeholder site for `hivecommons.dev`. | Placeholder | No |

## Standards that apply

Every repository in the list above is Apache-2.0 licensed, requires DCO
sign-off on every commit, and inherits the organization's code of conduct and
security policy from the `.github` repository. Repositories marked Active
follow the same review and merge rules as Hive: changes land through pull
requests gated by CI. Repositories marked Supporting or Placeholder carry no
release of their own and are changed only in service of an Active repository.

When a repository is added to or removed from the organization, update this
page in the same pull request.
