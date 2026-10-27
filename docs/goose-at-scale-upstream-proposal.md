# Proposed upstream issue: Goose-at-scale guide

This is the ready-to-paste text for the issue a maintainer files by hand in
[aaif-goose/goose](https://github.com/aaif-goose/goose), following its
issue-first workflow. Upstream requires a human-written issue to reach
**Ready** before any external PR, so Hive does not open it. Everything below
the rule is the issue body.

---

**Title:** Docs: running Goose unattended at fleet scale

## Summary

Propose a guide under `documentation/docs/guides/` for running Goose
unattended: headless `goose run --no-session -t` from CI jobs and
orchestrators, with notes on confinement and exit behaviour.

## Motivation

Orchestrators and CI systems increasingly launch Goose as a child process,
one task per invocation, with no attached terminal. The existing
[running-tasks guide](https://github.com/aaif-goose/goose/blob/main/documentation/docs/guides/running-tasks.md)
covers the flags, but a few things that matter at scale are not documented:

- **Exit-code semantics.** Which exit statuses mean success, a failed task,
  a provider or credential error, or an interrupted run, so a caller can
  report completion or failure reliably.
- **TTY-less behaviour.** What `goose run` does without an attached TTY,
  including prompts, approvals, and output formatting.
- **Confinement.** `GOOSE_MODE=auto` permits unattended tool execution but is
  not a sandbox. Operators need guidance on running Goose inside a container
  or similar boundary, and on keeping unrelated credentials out of the task
  environment.
- **Credential handling.** Passing provider keys (for example
  `ANTHROPIC_API_KEY`) through the environment rather than command lines,
  task prompts, or committed config.

## Proposed page outline

1. Overview and scope: one-shot headless runs versus interactive sessions.
2. Provider credentials and a one-shot task (`GOOSE_PROVIDER`,
   `GOOSE_MODEL`, `GOOSE_MODE`, `--no-session`, `-t`).
3. Running without a TTY: expected behaviour and limits.
4. Exit codes and how a calling process should interpret them.
5. Confinement: what `GOOSE_MODE=auto` does and does not guarantee, and
   container-based isolation.
6. Links to the provider and environment-variable references.

Facts in the page would be checked against the current CLI before submission,
and open questions (such as exit codes) would be settled in this issue first.

## Relationship to #12691

[aaif-goose/goose#12691](https://github.com/aaif-goose/goose/issues/12691)
asks where community integrations that run Goose should be listed. This
proposal is complementary: it covers how to run Goose unattended, not a list
of tools. It can be folded into whatever structure that thread settles on.

## Reference implementation

Hive maintains a guide covering the same ground, which can serve as a draft:
[goose-at-scale.md](https://github.com/hivecommons/hive/blob/v5/docs/goose-at-scale.md).

## Offer

Once this issue reaches Ready, a PR implementing the agreed outline will
follow.
