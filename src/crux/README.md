# Crux

Crux is an agent runtime: it starts coding agents (Copilot, Claude, Codex,
Gemini and other CLIs) in managed tmux sessions, feeds them work ("kicks"),
watches their output, restarts or stops them, and exposes all of that as a
Go interface, a command line and a local HTTP+JSON API. It is the runtime
inside [Hive](https://github.com/hivecommons/hive) and is built to run on
its own: this directory is the Go module `github.com/hivecommons/hive/crux`
and depends on nothing else in the Hive repository.

The design, the boundary rules and the roadmap are in
[`src/docs/design/crux.md`](../docs/design/crux.md); the operator pages are
[`crux-cli.md`](../docs/crux-cli.md) and [`crux-serve.md`](../docs/crux-serve.md).

## What is in the module

| Package | Role |
|---|---|
| `crux` | the public surface: `crux.Runtime` (`Definitions`, `Lifecycle`, `Observer`, `Kicker`), `crux.Process`, `crux.ProjectContext` |
| `crux/agent` | the manager: tmux sessions, backends, restarts, kick delivery, the only place a process is touched |
| `crux/config` | the configuration leaf: `config.AgentConfig` and the other YAML-shaped types, stdlib only |
| `crux/cli` | the `crux` command: `run`, `ps`, `logs`, `kick`, `stop`, `serve`, `agy-turn` (the pane-side agy subprocess), and `--server` remote mode |
| `crux/api`, `crux/serve` | the wire contract of `crux serve` (stdlib-only client and types, `api.RuntimeRoutes`: every `crux.Runtime` method → route) and the server |
| `crux/api/client` | `client.Runtime`: `crux.Runtime` over the API; what `crux/runtime.New` returns for `CRUX_MODE=sidecar` |
| `crux/api/callback`, `crux/hivecap` | the hive callback contract (capabilities hive serves the runtime: scoped tokens, agent mint, Bob credential, PRs, Linear, scanners, conditions, host policy, inference routes, effects, usage) and the runtime-side adapters `crux serve --hive-callback` installs; `api.HostSetters` is the table that accounts for every `crux.Host` setter (route / callback / config key / in-process-only) |
| `crux/runtime` | `runtime.New` (mode selection: `CRUX_MODE=inprocess` default, `sidecar`) and `runtime.Supervisor`, the child supervisor hive uses for `crux serve` when `CRUX_SUPERVISE=child` |
| `crux/defs` | the file-based agent-definition loader the module's binary uses |
| `crux/cmd/crux` | the binary |
| the rest | observation and policy: `watchdog`, `trajectory`, `logscrub`, `proxy`, `sandbox`, `inference`, … |

## Build

```sh
make build                 # -> bin/crux, version from `git describe`
GOWORK=off go build ./cmd/crux   # the same, without make
go install github.com/hivecommons/hive/crux/cmd/crux@latest
```

The Makefile sets `GOWORK=off`, so every target proves the module builds
without the `src/go.work` workspace it sits in. `make test` runs the unit
tests (the `crux/agent` tests need `tmux`), `make vet` vets, `make smoke`
builds and exercises `crux serve` end to end on a private unix socket (no
agents are started; CI runs the same `scripts/smoke.sh`).

## Run an agent

Agent definitions are YAML files. Either the portable `AgentDefinition`
document (the format Hive's dashboard imports and exports):

```yaml
# defs/scout.yaml
apiVersion: hive.kubestellar.io/v1
kind: AgentDefinition
metadata:
  name: scout
  displayName: Scout
spec:
  backend: copilot
  model: claude-sonnet-4-6
  role: scout
  clearOnKick: true
```

or a bare per-agent file, named by its file name (`defs/scout.yaml` defines
`scout`):

```yaml
backend: copilot
model: claude-sonnet-4-6
```

Then:

```sh
export CRUX_DEFS=./defs            # or --defs ./defs on every command; a single file works too
crux run scout                     # start scout, stream its output, stop it on Ctrl-C
crux run --detach scout            # start it and return
crux ps                            # what is running
crux logs scout -n 50              # its recent pane output (--buffer, --full, --json)
crux kick scout "look at issue 42" # send it work
crux stop scout
crux run ./defs/scout.yaml         # a definition file can be named directly
```

Agents live under `--workdir` (default `$HIVE_WORK_DIR`, then `$HOME/.crux`).
While a foreground `crux run` is up it serves a control socket in the work
dir, and `ps`, `logs`, `kick` and `stop` talk to it so they see the live
runtime; `--local` builds a fresh one instead.

## Serve the runtime

```sh
crux serve &                                   # unix socket: $CRUX_SOCKET, else $XDG_RUNTIME_DIR/crux/crux.sock
crux serve --socket /run/crux/crux.sock
crux serve --listen 127.0.0.1:7777 --token "$T" # TCP too; loopback only, token required
```

`crux serve` exposes the runtime over HTTP+JSON under `/v1`: list and get
agents, run from a definition, logs (pane, buffer, full, or followed as
server-sent events), kick, stop, health, and an event stream of starts,
stops, kicks and state changes. The contract is `crux/api`, which also has
a Go client (`api.NewClient`). [`crux-serve.md`](../docs/crux-serve.md)
has the routes.

## Remote mode

Every verb except `serve` can drive a `crux serve` instead of an in-process
runtime:

```sh
export CRUX_SERVER=/run/crux/crux.sock   # or --server; unix:///path, host:port and http://… also work
export CRUX_TOKEN=...                    # or --token, when the server has one
crux ps
crux run ./defs/scout.yaml               # the file is read here and sent to the server
crux logs -f scout                       # follows over SSE
```

`crux --version` prints the version embedded at build time
(`-ldflags "-X main.version=…"`, `dev` otherwise).

## Telemetry

The runtime records OpenTelemetry spans (`agent.send_kick`, …) and two kick
metrics through the OTel API. The `crux` binary exports them over OTLP/HTTP
only when the standard `OTEL_EXPORTER_OTLP_ENDPOINT` (or a per-signal
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`)
is set; with none set nothing is installed and nothing is sent. There is no
`/metrics` endpoint: point crux at an OpenTelemetry Collector and export from
there. Variables and instruments are listed in
[`crux-cli.md`](../docs/crux-cli.md#telemetry).

## Using Crux from Go

```go
import (
    "github.com/hivecommons/hive/crux"
    "github.com/hivecommons/hive/crux/agent"
    "github.com/hivecommons/hive/crux/defs"
)

defs, _ := defs.Load("./defs")
var rt crux.Runtime = agent.AsRuntime(agent.NewManager(defs, logger, crux.ProjectContext{}))
```

Everything a host needs is on `crux.Runtime`; `*agent.Manager` is the
constructor and nothing more. Hive itself is such a host: its `hive` binary
builds the manager the same way, and its own `crux` build
(`src/cmd/crux` in the root module) is the binary here with Hive's
`hive.yaml` loader plugged into `cli.DefinitionLoader` in place of
`crux/defs`.

## Boundaries

Nothing under this module imports the Hive root module; a test in the root
repository (`src/internal/testutil/crux_outbound_ratchet_test.go`) keeps it
that way. `crux/config` and `crux/api` are stdlib-only. The tmux-backed
tests in `crux/agent` create and kill real sessions: do not run them from
inside an agent's own tmux pane.
