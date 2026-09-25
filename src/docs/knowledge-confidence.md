# Knowledge confidence scoring

Knowledge facts expose a numeric `confidence` for API compatibility plus two
additive fields:

- `confidence_scored`: whether the number is backed by real signals.
- `confidence_reason`: a short explanation for dashboards and operators.

The dashboard shows `unscored` instead of a percentage when
`confidence_scored` is false. This prevents legacy defaults from looking like
measured certainty.

## Formula

Explicit frontmatter or API confidence values remain authoritative unless they
are a legacy generated placeholder. Generated facts are rescored on read from
signals Hive already stores:

1. **Source provenance**
   - configured external document: `0.72`
   - HTTPS configured document: `0.73`
   - Context7 document: `0.74`
   - bead-synth fact: its existing deterministic bead classification score,
     or `0.56` if missing
2. **Workflow-only baseline**
   - facts without stored confidence or document/bead provenance start at
     `0.50` only when they have validation, related-link, source, or usage
     signals; otherwise they are unscored
3. **Human/workflow validation**
   - `verified`, `validated`, or `approved`: `+0.12`
   - `published`: `+0.06`
   - `draft`: `-0.08`
4. **Layer authority**
   - community: `+0.04`
   - org: `+0.03`
   - project: `+0.02`
   - personal: no change
5. **Corroboration and use**
   - related wiki links: `+0.02` each, capped at `+0.06`
   - additional sources: `+0.02` each after the first, capped at `+0.06`
   - vault access count: `+0.01` each, capped at `+0.05`
6. **Freshness**
   - newer than 30 days: `+0.03`
   - newer than 180 days: `+0.01`
   - older than 365 days: `-0.04`
   - freshness adjusts an otherwise scored fact; file modification time alone
     does not turn an otherwise signal-free fact into a scored one

Scores are clamped to `0.05..0.99` and rounded to percentage precision.
Facts with no explicit confidence and no provenance, validation, related-link,
source, or usage signal are returned as unscored.
