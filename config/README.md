# Hive configuration files

This directory contains configuration used by the top-level Hive helper scripts. It is not the same as the v2 Go binary config in `src/hive.yaml.example`.

## `hive-project.yaml`

`config/hive-project.yaml.example` is the project file consumed by the deterministic shell pipeline under `bin/`:

- `bin/hive-config.sh` reads `${HIVE_PROJECT_CONFIG:-/etc/hive/hive-project.yaml}` and exports project, agent, dashboard, health-check, outreach, and GitHub App values for other scripts.
- `bin/run-pipeline.sh` and several pipeline stages read `${HIVE_PROJECT_YAML:-/etc/hive/hive-project.yaml}` to find `pipeline.stages` and project-specific classification inputs.
- If the file is missing, scripts fall back to built-in defaults or to the first example file they can find; those fallbacks are for development and may not match your project.

Install it at `/etc/hive/hive-project.yaml`, or point the scripts at another path with `HIVE_PROJECT_CONFIG` and `HIVE_PROJECT_YAML`.

## How it differs from `src/hive.yaml`

| File | Used by | Purpose |
|---|---|---|
| `config/hive-project.yaml.example` | Top-level `bin/` shell automation and deterministic pre-kick pipeline | Project/org/repo metadata, enabled script agents, beads base directory, health-check workflow names, outreach metadata, and optional GitHub App values for script token minting. |
| `src/hive.yaml.example` | The v2 Go `hive` binary, dashboard, governor, agent manager, hub, and API server | Runtime config for the containerized v2 service: project, agents, governor modes, GitHub auth, dashboard auth, data paths, knowledge, hub/spoke settings, and ACMM level. |

Use `src/hive.yaml` for normal v2 Docker/Kubernetes operation. Use `hive-project.yaml` only when you run the top-level `bin/` scripts or the deterministic pipeline directly.
