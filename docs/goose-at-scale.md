# Running Goose unattended at scale with Hive

[Goose](https://github.com/aaif-goose/goose) executes agent tasks;
[Hive](https://github.com/hivecommons/hive) schedules work across a fleet of
agents and applies operator-controlled policy. This guide covers the
contributor relay's headless Goose path, not the server-side manager's
interactive `goose run -s` path.

## Provider credentials and a one-shot task

Install Goose from its [canonical upstream releases](https://github.com/aaif-goose/goose/releases).
Select a provider and a model supported by that provider. For Anthropic,
load `ANTHROPIC_API_KEY` into your environment from your secret manager;
do not put the key in a command line, task prompt, or committed config.

```bash
export GOOSE_PROVIDER=anthropic
export GOOSE_MODEL=claude-sonnet-4-6
export GOOSE_MODE=auto
goose run --no-session -t "Summarize this repository's contribution instructions without changing files."
```

Goose's [provider guide](https://github.com/aaif-goose/goose/blob/main/documentation/docs/getting-started/providers.md)
documents credentials for other providers. `GOOSE_MODE=auto` permits unattended
tool execution; it is **not a sandbox**. `--no-session` avoids persisting a
conversation, and `-t` takes the task text as its value. See Goose's
[running-tasks guide](https://github.com/aaif-goose/goose/blob/main/documentation/docs/guides/running-tasks.md).
Run even a one-shot task only in an environment whose access you accept.

## Join a Hive as a headless contributor

From a Hive checkout, with the provider credential already in the environment:

```bash
just contribute-check goose
just contribute-setup goose
AGENT_BACKEND=goose GOOSE_PROVIDER=anthropic GOOSE_MODEL=claude-sonnet-4-6 \
  CONTRIBUTOR_MODE=headless just contribute-hive goose
```

Setup registers the contributor with the configured hub. The default
`contribute-hive` mode is containerized; it requires a supported container
runtime and the contributor image, as described in the
[contributor relay guide](../src/docs/contributor-relay.md).
Hive forwards supported provider environment variables into the container
by name rather than embedding their values in runtime arguments. A preflight
checks prerequisites; it does not prove that a credential authenticates.

For each assignment, `bin/contributor-relay.js` launches
`goose run --no-session -t "<task prompt>"` as a child process and uses its
exit status to report completion or failure to the hub. Goose selects its
model from `GOOSE_MODEL` on this path. Headless mode needs no attached TTY.
The hub controls assignment and contributor trust; an agent must still obey
Hive's write gates and must not merge its own PR.

## Confinement and the local refusal gate

Goose's approval modes do not provide an OS-enforced filesystem, process,
or network boundary. Hive therefore refuses the **local contributor** Goose
path unless the operator explicitly sets
`HIVE_GOOSE_DANGEROUSLY_RUN_UNCONFINED=1`. Keep that unset for normal use;
use the containerized path instead. Do not bypass the refusal gate merely
to make setup succeed.

A container limits host access only to the extent its runtime, mounts,
privileges, and network policy enforce it. Keep unrelated credentials and
sensitive host paths out of the task environment. Hive's policy and GitHub
write enforcement are separate controls, not a substitute for confinement.
See [sandbox isolation](../src/docs/sandbox-isolation.md) and
[backend setup](backend-setup.md).

## Upstream contribution status

This is Hive-maintained documentation, not an upstream Goose integration
endorsement. Goose requires an issue in **Ready** status before an external
PR, and its contribution guide asks a human to write the issue. A human
sponsor must propose this page and obtain Ready status before submitting it
upstream. Track that Goose documentation handoff in
[Hive #10627](https://github.com/hivecommons/hive/issues/10627) and the
[ecosystem landscape](../src/docs/landscape.md#agentic-ai-foundation-aaif-and-goose).
