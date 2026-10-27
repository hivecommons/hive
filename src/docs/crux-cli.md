# `crux` CLI

`crux` is the command line for the agent runtime
([design record](design/crux.md), RFC
[#13536](https://github.com/hivecommons/hive/issues/13536)). It runs an agent
from a definition and observes and controls it through `crux.Runtime`
(`src/pkg/crux/runtime.go`), with none of the governor, GitHub integration or
dashboard around it. It is the Stage 1 proof that the runtime surface is
enough to drive an agent; the Stage 2 binary ships the same commands from the
nested module.

## Build

Two hosts build the same CLI (`crux/cli`); they differ only in where agent
definitions come from:

```sh
cd src && go build -o crux ./cmd/crux          # Hive-hosted: --config hive.yaml, pkg/config + pkg/defsrc
cd src/crux && make build                      # module's own: --defs <dir|file>, plain YAML (crux/defs)
cd src/crux && GOWORK=off go build ./cmd/crux  # the same, proving the module builds alone
```

The image ships the Hive-hosted one at `/usr/local/bin/crux`. The module's
binary is what a downstream checkout of `github.com/hivecommons/hive/crux`
builds; see [`src/crux/README.md`](../crux/README.md). `--version` prints the
version set with `-ldflags "-X main.version=…"` (`dev` otherwise).

## Commands

Every subcommand maps onto one or two `crux.Runtime` methods and nothing else:

| Command | Runtime methods |
|---|---|
| `crux run <definition.yaml \| agent>` | `Definitions.AddAgent` (definition file) then `Lifecycle.Start`; streams `Observer.GetBufferOutput`; `Lifecycle.Stop` on interrupt |
| `crux ps [--json]` | `Observer.AllStatuses` |
| `crux logs <agent> [-n N] [--json]` | `Observer.ResolveAgent`, `Observer.GetOutput` |
| `crux logs <agent> --buffer` | `Observer.GetBufferOutput` |
| `crux logs <agent> --full` | `Observer.FullLogHeader`, `Observer.CaptureFullLog` |
| `crux kick <agent> <message...\|->` | `Observer.ResolveAgent`, `Kicker.SendKick` (`-` reads the message from stdin) |
| `crux stop <agent>` | `Observer.ResolveAgent`, `Lifecycle.Stop` |
| `crux serve [--socket P] [--listen H:P] [--token T]` | the whole surface, over HTTP+JSON ([crux-serve.md](crux-serve.md)) |

Global flags go before the subcommand:

| Flag | Meaning |
|---|---|
| `--workdir DIR` | agent work dir; default `$HIVE_WORK_DIR`, then `$HOME/.crux`. Exported to the runtime as `HIVE_WORK_DIR`. |
| `--config PATH`, `--defs PATH` | the definition source; synonyms, default `$CRUX_DEFS`. For the Hive-hosted binary a hive config whose enabled `agents:` define the runtime, loaded with the same `config.Load` as `hive` itself; for the module's binary a directory of `*.yaml` definition files, or one file |
| `--socket PATH` | control socket; default `<workdir>/crux.sock` |
| `--local` | never use a running `crux run`'s socket; build a fresh runtime in this process |
| `--server ADDR` | drive a `crux serve` at `ADDR` (socket path, `unix:///path`, `host:port`, `http://…`) instead of an in-process runtime; default `$CRUX_SERVER` |
| `--token T` | bearer token for `--server`; default `$CRUX_TOKEN` |
| `--version` | print the version and exit |

`run` flags: `--detach` (return after `Start`, leave the agent running),
`--interval D` (output poll interval, default 1s), `--no-socket`.

`logs -f` follows the agent's ring buffer; it is available in remote mode
only, where the server streams it.

Exit codes: 0 ok, 1 runtime error, 2 usage.

## Definitions

`crux run` reads the two definition formats hive already has, so a file that
works in hive works here unchanged:

- a **portable `AgentDefinition`** (`kind: AgentDefinition`, the dashboard's
  import/export schema; see `src/examples/agents/customized-agent.yaml`). It
  is parsed with `defsrc.ParseDefinition` and merged over the import defaults
  with `defsrc.MergeAllowedFields`, so it can set exactly the fields a
  dashboard import can. The agent is named by `metadata.name`.
- a **per-agent overlay** (a bare `AgentConfig` document, the `agents_dir`
  file format read by `config.LoadAgentOverrides`). The agent is named by the
  file's base name, so `fixer.yaml` defines `fixer`.

Either way `Config.ApplyAgentDefaults` fills the per-agent defaults a
`hive.yaml` load would, and the result goes to `Definitions.AddAgent`.

With `--config`, `crux run <agent>` starts an agent already defined in that
`hive.yaml`; the runtime is built from `cfg.EnabledAgents()` and a
`ProjectContext` derived the way `cmd/hive` derives it (org, repos, policy
dir, ACMM level), minus the fields that need a GitHub client.

```sh
crux run examples/agents/customized-agent.yaml   # foreground; Ctrl-C stops it
crux --config /etc/hive/hive.yaml run scout      # from a hive config
crux run --detach fixer.yaml                     # start and return
```

## The runtime is in-process

`*agent.Manager` holds lifecycle state in memory: which agents are running,
their ring buffers, kick history. That is why `crux run` is the long-lived
command. While it is in the foreground it serves a unix control socket at
`<workdir>/crux.sock`; `ps`, `logs`, `kick` and `stop` connect to it when it
is present, so they act on the runtime that actually started the agent:

```sh
crux run fixer.yaml &          # terminal 1 (or a second terminal, in the foreground)
crux ps                        # terminal 2: fixer  running
crux logs fixer -n 50
crux kick fixer "look at the failing build on main"
crux stop fixer
```

The socket carries one JSON request per connection (the subcommand's argv)
and one JSON response (what the command wrote, and its exit code). The
serving side runs the same command code the local path runs. It is a local
convenience for one work dir, not the `crux serve` API.

## Remote mode

`crux serve` is the long-lived process when something other than a
foreground `crux run` should own the agents: it builds the runtime from
`--config` and exposes it over HTTP+JSON on a unix socket (optionally TCP
with a bearer token). With `--server` or `CRUX_SERVER` set, `run`, `ps`,
`logs`, `kick` and `stop` talk to it and build nothing locally:

```sh
crux --config hive.yaml serve &                       # serving on unix:///run/user/1000/crux/crux.sock
export CRUX_SERVER=/run/user/$(id -u)/crux/crux.sock
crux run fixer.yaml                                   # parsed here, sent as JSON, output followed
crux ps
crux logs fixer -f
crux stop fixer
```

`serve` itself ignores `--server`/`CRUX_SERVER` and always runs locally, so a
server address left in a shell profile does not stop a new server from
starting. Routes, auth, defaults and curl examples are in
[crux-serve.md](crux-serve.md).

Without a live socket, the observer commands build a fresh runtime from
`--config`. That runtime knows the definitions and can read the pane of a
tmux session that outlived its `crux run` (`logs --full`, which captures the
pane directly), but it holds no lifecycle state, so `ps` reports the
definitions as stopped and `kick`/`stop` are refused as not running. A socket
file nobody answers on (a `run` that died without cleanup) is reported and
treated as absent. `--detach` leaves an agent running in tmux with no
supervisor; use it when something else will own the session.

## Telemetry

The runtime opens OpenTelemetry spans (for example `agent.send_kick`) and
records two kick instruments through the OTel API
([hivecommons/crux#19](https://github.com/hivecommons/crux/issues/19)):

| Instrument | Type | Attributes |
|---|---|---|
| `crux.agent.kick.duration` | histogram, seconds | `agent.role`, `outcome` |
| `crux.agent.kick.outcomes` | counter | `agent.role`, `outcome` |

`agent.role` is the configured agent name (`unknown` when the kicked name is
not a configured agent); `outcome` is `ok`, `cancelled` (the agent restarted
mid-kick) or `error`. No issue or PR numbers, run ids or free text are ever
attributes.

Export is off by default. Both `crux` builds install an OTLP/HTTP trace and
metric pipeline at startup only when the standard OpenTelemetry environment
names an endpoint, and leave the no-op providers in place otherwise:

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | base collector URL for both signals (e.g. `http://collector:4318`) |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | enable and address one signal only |
| `OTEL_TRACES_EXPORTER=none` / `OTEL_METRICS_EXPORTER=none` | turn one signal off |
| `OTEL_SDK_DISABLED=true` | turn both off |
| `OTEL_EXPORTER_OTLP_HEADERS`, `…_CERTIFICATE`, `…_TIMEOUT`, `…_COMPRESSION` | read by the exporters as usual |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | override the resource (`service.name` defaults to `crux`; `service.version` is the build version) |

Only the `http/protobuf` protocol is built in: setting
`OTEL_EXPORTER_OTLP_PROTOCOL` (or a per-signal variant) to anything else
makes crux print `crux: telemetry disabled: …` and run without export rather
than fail the command. There is no `/metrics` endpoint and no Prometheus
dependency; scrape or forward from an OpenTelemetry Collector instead. Spans
and metrics are flushed on exit, bounded to five seconds: with an endpoint set
and the collector unreachable, each `crux` invocation can take up to that long
to exit after its work is done — that is the flush timing out, not a hang.
Unset the endpoint (or set `OTEL_SDK_DISABLED=true`) to avoid it. Inside Hive the
same instruments flow through Hive's own `tracing:` configuration instead.

## What this is not

The CLI reaches `*agent.Manager` only to construct it
(`pkg/crux/cli/runtime.go`). It does not touch the governor's kick schedule,
GitHub relays, ACMM packs, the dashboard or the hub. An agent run this way has
exactly the runtime's own behaviour: tmux launch, engine adapter, kick
delivery, crash and stall handling, sandbox when configured.
