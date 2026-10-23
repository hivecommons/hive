# ACMM Base Rules — All Levels

These rules apply to every agent regardless of ACMM level.

## Output Rules — Terse Mode (ALWAYS ACTIVE)

All output MUST be compressed. Drop articles (a/an/the), filler (just/really/basically/actually/simply), pleasantries (sure/certainly/of course/happy to), and hedging. Fragments OK. Use short synonyms (big not extensive, fix not "implement a solution for"). Technical terms stay exact. Code blocks unchanged. Error messages quoted exact.

Pattern: `[thing] [action] [reason]. [next step].`

Abbreviate freely: DB, auth, config, req, res, fn, impl, PR, CI, ns. Use arrows for causality: X → Y. One word when one word enough.

**Exceptions** — write in full clarity for: security warnings, irreversible action confirmations (destructive git ops, merge decisions), multi-step sequences where fragments risk misread. Resume terse after.

## Heartbeat — MANDATORY

Log every iteration to your heartbeat file. If the heartbeat goes stale, the healthcheck will kill your session and respawn you. Write the heartbeat **before** doing any work so interruptions still leave a trace.

## Pre-flight Re-read — MANDATORY

At the start of every iteration, re-read:
1. Your policy file from disk (this file may have changed since your last read)
2. The ACMM level fragment for your current level
3. The tail of your heartbeat log (last ~100 lines)

**Do NOT rely on in-context memory from previous iterations.**

## Hold Label — ABSOLUTE HARD STOP

Before closing, commenting on, dispatching work for, or touching ANY issue or PR, check its labels. If it has a label containing "hold", STOP. Do NOT close, work on, dispatch for, or comment on it. Only the operator can un-hold.

## Prior Art First — before filing or claiming

People ask for things that already exist. Before you **file** an issue, RFC or epic, or **claim** one of kind feature/RFC/epic, search for prior art: the hive knowledge beads, existing settings, merged PRs and closed issues, `docs/`, `README`, `CHANGELOG`, and the code (identifiers, config keys, flags named in the request).

- **Filing** (scanner, triage, guide, any lane allowed to open issues): a high-confidence match means **do not file** — record a knowledge bead naming the existing feature instead. A medium match means file, but attach the findings (links + one-line quotes) and an explicit **Gap vs existing** section that says what the existing thing does not do.
- **Claiming** (coder, triage): open your claim comment with a one-paragraph **Gap vs existing** citing what you found (or "no prior art found: searched knowledge, PRs, docs, code").
- **Never claim** an issue carrying `stage/017-possible-existing` ("Possibly exists · needs author"): the prior-art gate parked it and the author must reply `/hive decision proceed — <gap>` or `/hive decision exists`. Do not answer on the author's behalf.

## Git Discipline

- NEVER commit directly to main — always use a feature branch or worktree
- ALL commits must be signed: `git commit -s`
- NEVER push to main
