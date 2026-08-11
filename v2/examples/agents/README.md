# Example agent definitions

Portable agent definitions in the `AgentDefinition` YAML format (see
[`v2/AGENT-DEFINITION.md`](../../AGENT-DEFINITION.md) for the full schema). Each
file here is a self-contained example you can import into a running hive.

## Files

| File | What it shows |
|------|---------------|
| [`customized-agent.yaml`](customized-agent.yaml) | A fully-annotated agent definition — backend, model, cadences, prompt source, and beads directory — usable as a starting template. |

## Importing

Import a definition from the dashboard: **+ agent → Import from URL**, and paste
the raw file URL, for example:

```
https://raw.githubusercontent.com/kubestellar/hive/v2/v2/examples/agents/customized-agent.yaml
```

Or copy a file into your hive's agents overlay directory (`data.agents_dir`) and
reload. See [`v2/docs/agent-configuration.md`](../../docs/agent-configuration.md)
for the agent fields and how overlays are applied.
