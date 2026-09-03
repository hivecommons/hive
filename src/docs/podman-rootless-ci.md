# Rootless Podman: CI seam (#2535, Option C)

## Why this doc exists

Issue [#2535](https://github.com/hivecommons/hive/issues/2535) raises three options for
container-runtime auto-detect:

- **Option A** — re-order auto-detect to prefer rootless Podman over Docker when both are
  present.
- **Option B** — leave the detect order alone, but make the privilege implication of each
  runtime legible at the point of choice (done: see the posture note above the runtime
  detect logic in `Justfile`, in the `container_runtime` doc comment, and in the page copy
  in `src/pkg/dashboard/api_contribute.go`).
- **Option C** — make rootless Podman a *tested* path in CI, so the handling doesn't
  silently regress, before A is even considered.

The reporter's stated position: **"B now, C before A."** This doc plus the CI job it
describes is Option C's seam. **It does not change the auto-detect default** — Docker
still wins when both engines are present (see `Justfile`, the `contribute-hive` recipe's
runtime-resolution block). A default flip (Option A) is explicitly out of scope until this
seam has run long enough to be trusted, per the reporter's own downstream experience
(`projectbluefin/review`'s ten-commit `--userns=keep-id` regression history, cited in
#2535).

## What "rootless Podman handling" means here

The parts of Hive that change behavior specifically for Podman, all in the `contribute-hive`
recipe of `Justfile`:

1. `--userns=keep-id` — rootless UID mapping, so the container's `dev` user can read
   bind-mounted host config (`~/.config/hive`, `~/.claude`, `~/.config/gh`, etc.) without
   a UID mismatch.
2. `:Z` / `:ro,Z` volume-mount suffixes — SELinux relabeling so the container can actually
   access those same bind mounts on an SELinux-enforcing host.
3. The macOS carve-out — `podman machine`'s "host" is the Podman VM, not the Mac itself, so
   `--network host` is dropped on Darwin for Podman (Docker Desktop on macOS has the same
   constraint and is handled the same way).
4. No engine socket is ever mounted into the contributor container
   (`/var/run/docker.sock`, `/run/podman/podman.sock`) — already true, and covered by the
   marker check below so it stays true if a future PR touches the mount list.

Any of these four regressing would reproduce exactly the failure mode #2535 cites from
`projectbluefin/review`: correct-looking code that silently stops doing rootless mapping,
SELinux labeling, or socket confinement.

## The seam: what should be tested, and how

This PR does not attempt the full "spin up rootless Podman + SELinux on a runner" matrix —
that is real infrastructure work (GitHub-hosted runners are not SELinux-enforcing by
default, and rootless Podman-in-CI has its own sandboxing wrinkles). Per #2535, "even a
lightweight CI job or a documented test-intent" is the bar for Option C. What's in place:

- **A static contract check** (`src/scripts/check-podman-contract.sh`, wired into
  `.github/workflows/podman-contract.yml`) that greps the `Justfile`'s `contribute-hive`
  recipe and asserts, on every push/PR touching `Justfile`:
  - `--userns=keep-id` is present in the Podman branch.
  - `:Z` is present in the Podman volume-suffix variables.
  - The macOS network carve-out still special-cases Podman (`darwin` + network flags).
  - Neither `/var/run/docker.sock` nor `/run/podman/podman.sock` appears anywhere in the
    recipe (mirrors the same static-contract-test idea `projectbluefin/review` uses in
    `tests/image-contract.sh`, cited in #2535 as "the test outlived the refactor").

  This catches the exact regression pattern #2535 describes ("kept regressing until the
  invariant was pinned by a test") without requiring a live rootless-Podman runner. It is
  cheap, fast, and runs on every PR.

- **Documented test-intent for the live path** (this section) — the next increment beyond
  the static check, for whoever picks up full Option C coverage:
  1. A self-hosted or container-in-container runner with rootless Podman + an
     SELinux-enforcing filesystem (GitHub-hosted `ubuntu-latest` runners are not
     SELinux-enforcing, so this can't run there as-is).
  2. Actually invoke `just contribute-hive <backend> ` with `HIVE_CONTAINER_RUNTIME=podman`
     forced, using a throwaway `HIVE_HUB` / fixture credentials, and assert the container
     starts, the bind mounts are readable inside it (proves `keep-id` + `:Z` both worked),
     and it exits cleanly.
  3. Track flakiness separately from the static contract check — a live rootless
     container test is inherently noisier than a grep-based one, and conflating the two
     would make the fast static gate less trustworthy.

  Steps 1-3 are the "before A" work; this PR establishes the static seam (bar cleared per
  #2535's "even a lightweight CI job or a documented test-intent") and leaves the live
  runner work as a tracked follow-up rather than guessing at infrastructure this PR can't
  validate.

## Non-goals (explicitly, per #2535)

- **No default flip.** `contribute-hive`'s runtime auto-detect still tries `docker` before
  `podman`. `HIVE_CONTAINER_RUNTIME` and the dashboard runtime selector remain the only way
  to force Podman.
- **No `--network host` change.** #2535 flags this as "a separate, smaller question rather
  than an ask" — out of scope here.
- **No full SELinux CI matrix.** #2535 explicitly says a lightweight seam is sufficient for
  now: "Do NOT need a full rootless-SELinux CI matrix; establish the seam + document it."
