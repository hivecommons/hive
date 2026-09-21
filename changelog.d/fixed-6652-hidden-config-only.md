- `hiddenAgents` now reports agents dropped by the config-only pass of the
  dashboard status builder. Previously an agent present in the config but absent
  from the runtime status map was skipped silently, so a hive with no visible
  cards also reported an empty diagnostic list, making #6581-class reports
  impossible to triage.
