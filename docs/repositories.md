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
| [crux](https://github.com/hivecommons/crux) | The governed AI agent runtime from Hive, published for reuse outside Hive: runs, observes and controls agent CLIs. A synced copy of this repository's `src/crux` module ([publishing model](../src/docs/crux-publish.md)); issues and code changes go to this repository. | Active | No, standalone release (Hive builds its in-tree `src/crux`) |
| [pluk](https://github.com/hivecommons/pluk) | Lightweight pub-sub event streaming for tmux sessions running AI coding agents. Used by the agent manager to observe agent sessions. | Active | Yes, installed in the `hive` image |
| [hotshot](https://github.com/hivecommons/hotshot) | Screenshot capture that lands directly in an AI coding assistant's terminal. Developer tooling used by contributors and agents. | Active | No, standalone tool |
| [promptargs](https://github.com/hivecommons/promptargs) | Template expansion and variable substitution for AI prompts across Claude Code, Copilot, Goose, Bob and others. | Active | No, standalone tool |
| [rationguard](https://github.com/hivecommons/rationguard) | Detects and rebuts rationalization patterns in AI agent output. Standalone tool for reviewing agent transcripts. | Active | No, standalone tool |
| [dibs](https://github.com/hivecommons/dibs) | Contributor attribution layer for AI-agent-era contributions: your idea, your credit, their code. | Active | No, standalone service |
| [spektacular](https://github.com/hivecommons/spektacular) | Spec-driven development for AI coding agents: a markdown spec becomes a reviewed plan and an agent-driven implementation (Claude Code, Bob, Codex). | Early development | No, standalone binary |
| [hive-demo](https://github.com/hivecommons/hive-demo) | GitHub template repository for trying Hive on a sample service and walking it up the ACMM levels. Work in progress. | Early development | No |
| [homebrew-hive](https://github.com/hivecommons/homebrew-hive) | Homebrew tap for one-command install of the Hive contribute app, hotshot and Crux. | Supporting | No, packaging only |
| [homebrew-repo](https://github.com/hivecommons/homebrew-repo) | Homebrew tap for Spektacular (`brew install hivecommons/homebrew-repo/spektacular`); the formula is updated by Spektacular's release automation. | Supporting | No, packaging only |
| [docs](https://github.com/hivecommons/docs) | Source for [docs.hivecommons.dev](https://docs.hivecommons.dev). Hive, hotshot, pluk, rationguard, promptargs, dibs and Spektacular docs are single-sourced from their repositories at build time. | Supporting | No |
| [spektacular-website](https://github.com/hivecommons/spektacular-website) | Source for [spektacular.dev](https://spektacular.dev). | Early development | No |
| [infra](https://github.com/hivecommons/infra) | Shared CI workflows, Prow configuration and org automation. | Supporting | No |
| [.github](https://github.com/hivecommons/.github) | Organization profile and community health files (code of conduct, security policy, issue templates). | Supporting | No |
| [.project](https://github.com/hivecommons/.project) | CNCF `.project` metadata for Hive Commons, read by CNCF services automation (including maintainer access provisioning). | Supporting | No |
| [cncf-sandbox-onboarding](https://github.com/hivecommons/cncf-sandbox-onboarding) | Tracker for completing CNCF Sandbox onboarding. | Supporting | No |
| [hive-redirect](https://github.com/hivecommons/hive-redirect) | GitHub Pages redirect from `hive.hivecommons.dev` to the hosted hub. | Placeholder | No |
| [hivecommons.github.io](https://github.com/hivecommons/hivecommons.github.io) | Source for the [hivecommons.dev](https://hivecommons.dev) community website. | Supporting | No |

## Standards that apply

Every repository in the list above is Apache-2.0 licensed, requires DCO
sign-off on every commit, and inherits the organization's code of conduct and
security policy from the `.github` repository. Repositories marked Active
follow the same review and merge rules as Hive: changes land through pull
requests gated by CI. Repositories marked Supporting or Placeholder carry no
release of their own and are changed only in service of an Active repository.

When a repository is added to or removed from the organization, update this
page in the same pull request.
