# House style: one file every agent reads first

Every kick Hive sends — on every backend, to every agent — opens with a short
**house style** section: how changes are to be made, as distinct from *what*
to work on (the kick body) and the repo's own rules (`AGENTS.md`). It is the
one place to write "smallest complete diff, no speculative abstractions,
tests for behaviour changes, sign your commits" once and have the whole fleet
hear it.

The section is laid directly under the kick header, ahead of every other
static seam, and is identical across kicks so prompt caches keep working.

```
[agent:scanner]

HOUSE STYLE (source: repo)
- Make the smallest complete change …
```

## Precedence

| Source | Where it comes from | Shown as |
|---|---|---|
| **repo** | `HIVE_STYLE.md` at the root of the agent's primary repo, read from the checkout `project.checkouts_dir` supplies (the same root [`AGENTS.md`](agents-md.md) is read from) | `repo` |
| **hive** | `project.style` (inline text) in `hive.yaml`, else the file `project.style_file` names on the hive host | `hive` |
| **built-in** | the text shipped with Hive (`housestyle.Builtin`) | `built-in` |

Repo wins over hive wins over built-in. Each candidate is used whole or not at
all; when one is skipped the next applies, so a kick always carries exactly one
section. Inline `project.style` wins over `project.style_file` when both are
set.

The built-in text:

```
- Make the smallest complete change that fully addresses the task; no speculative abstractions, no drive-by refactors.
- Follow the patterns already in the file and package you are editing before introducing new ones.
- Every behaviour change ships with a test that fails without it.
- Keep existing public names, ids and config keys working; add, do not rename.
- Explain why in the PR body, not what; the diff already says what.
- Sign every commit off (DCO, `git commit -s`).
```

## Size cap

Each candidate is capped at `project.style_max_bytes` (default **4096**,
minimum 512). A style file is a handful of rules; anything longer belongs in
`AGENTS.md`, which has its own, larger cap and section-aware clipping. An
oversize candidate is **skipped, not clipped**, and the next source applies.

## The repo file is untrusted input

`HIVE_STYLE.md` arrives from the managed repository, not from the operator, so
it goes through [ioscan](ioscan.md) with the same policy as an issue body.
A blocking finding **refuses the whole file** — nothing from it reaches the
agent — and the hive default or built-in text applies instead. Operator-supplied
text (`project.style`, `project.style_file`) is trusted and is not scanned.
With `ioscan.enabled: false` the repo file is taken as-is.

Every skipped candidate writes one audit line, action `house_style_skip`, with
the agent, source, path and reason (`exceeds size cap`, `blocked by ioscan:
rule=…`, or the read error); ioscan blocks also write their usual
`ioscan_block` lines. The scheduler logs a warning naming the source that
applied instead.

## Seeing which one applied

The agent card (Agents view) and the agent detail pane carry a **style** field
showing `repo`, `hive` or `built-in` for the agent's last kick; it is read back
out of the kick's `HOUSE STYLE (source: …)` header, so it reports what was
actually sent. `/api/status` exposes the same value as `agents[].styleSource`
(empty until the agent has been kicked on a build with this feature).

## Configuration

```yaml
project:
  checkouts_dir: /data/checkouts     # needed for the repo file, as for AGENTS.md
  style: |                           # hive-wide default; a repo HIVE_STYLE.md overrides it
    - Prefer table-driven tests.
    - Never add a dependency without an issue first.
  # style_file: /etc/hive/HIVE_STYLE.md   # used when `style` is empty
  # style_max_bytes: 4096                 # min 512; oversize candidates are skipped
```

## Non-goals

- No per-agent style: the point is one voice. Per-agent instructions belong in
  the agent's kick template.
- No fetch of `HIVE_STYLE.md` over the GitHub Contents API when there is no
  checkout; a hive without `project.checkouts_dir` gets the hive default, as it
  does for `AGENTS.md`.

## What to read next

- [agents-md.md](agents-md.md) — the repo-owned *rules* file, injected after the house style.
- [ioscan.md](ioscan.md) — the policy the repo file is scanned under.
- [operator-reference.md](operator-reference.md) — the `project.style*` keys.
