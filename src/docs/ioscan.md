# ioscan status in v2

`ioscan` is not present in v2 HEAD. There is no `ioscan` package, no `ioscan:` config block in `src/hive.yaml.example`, and no canary/fail-mode implementation wired into the v2 kick path.

If you are reading older notes that mention:

```yaml
ioscan:
  enabled: true
  fail_mode: open
  canaries: false
```

that configuration is stale for v2 and has no effect unless a downstream fork has added its own scanner.

## What exists today

- Prompt and agent-definition sourcing fail closed on unallowlisted GitHub repositories.
- Prompt fetch failures fail open to the last cached or embedded template so kicks are not blanked by transient GitHub/API outages.
- GitHub PR claim and trust checks generally fail closed where duplicate work or privilege escalation is possible.

## What does not exist today

- No v2 canary injection for kick prompts.
- No v2 canary-trip handling.
- No v2 `fail_mode: open` redaction path.
- No v2 `fail_mode: closed` kick-blocking path.

Operators should not enable ioscan in v2 configs. If ioscan is reintroduced, document the exact package, config schema, fail-open/fail-closed behavior, canary format, logging, and interaction with the deterministic pipeline in this file as part of the same change.
