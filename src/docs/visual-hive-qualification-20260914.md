# Visual Hive secondary-App qualification

This candidate targets `dd` based on `ea23fe80d4b2ec5fb7907febca46db1e981c4474`. It reconciles #4059's sealed execution-token broker with the newer `SecondaryAppID` and cluster/App key store. It is not hosted-qualified.

## Behavior

- The Hub initializes the optional broker without requiring a fleet-wide key at startup. A per-Hive assignment and a valid key in that Hive's cluster store activate lease issuance on a later heartbeat.
- `PUT /api/saas/hives/{id}/secondary-app` remains the operator-controlled assignment. `PUT /api/saas/admin/cluster-app-keys/{clusterID}` with `for_app_id` remains the key provisioning path. No new public config fields or state schema versions are introduced.
- Registered public/enterprise Visual Hive App IDs, plus a custom legacy configured Visual Hive App ID, are excluded from generic secondary PEM delivery even when the spoke sends no token request. Enterprise Visual Hive token issuance is not implemented; it stays blocked rather than falling back to private-key delivery.
- Public Visual Hive leases use the assigned cluster/App key. A legacy Hub environment key remains readable for the same explicitly assigned App only when the registry key is absent. A present corrupt key blocks; it never silently falls back to stale material.
- The primary Hive App and key path are unchanged. Hive remains the only issue/PR writer. The optional App mints repository-scoped Actions/statuses/metadata execution tokens and retains the existing X25519 sealed-lease and recipient-pin formats.
- An explicit Hub broker denial clears the spoke's execution token immediately, including an unexpired token. An error-free heartbeat requiring no renewal preserves the lease. Expiry/renewal and immutable App/repository binding checks remain in force.
- The fork-only integrated release workflow now uses `src/` for source gates, runtime launchers, installer helpers, skill packaging and cache inputs. A checkout-existence check covers these inputs; earlier string-only tests agreed with the obsolete `v2/` paths. Fork-only publishing and versioned v2 JSON contracts are unchanged.

## Verification and remaining gates

Focused Linux Hub tests cover absent assignment/key, activation after provisioning without restart, existing-token reuse, renewal, assignment removal, mismatched repository, invalid key rotation, unchanged primary key, and no Visual Hive PEM delivery. Spoke coverage verifies immediate denial versus an ordinary no-renewal heartbeat. Generic secondary-App delivery tests use an unrelated App ID so they still prove the existing behavior.

The initial Windows run cannot compile pre-existing `pkg/beads/xproc_lock.go` (`syscall.Flock`); use Linux. Build/full relevant suites/vet/race and the real non-root entrypoint qualification must be completed before a merge-ready claim. Mocked credential tests do not prove an actual App installation, token mint, or hosted run.

Maintainer request: https://github.com/hivecommons/hive/issues/4030#issuecomment-5671578374. Required: selected-repo App installations and actual grants, isolated Hive/Hub, runtime/image configuration, secure operator provisioning/access, confirmation of `dd` promotion route, and production model testing. Current Visual Hive pin remains `a488292edfeb04fa4b2902782522ce4d9b79606f` until a newer release revision is accepted and revalidated.

## Migration / rollback

No primary-key migration, state-file rewrite, or automatic repository change is needed. Operators must explicitly assign the optional App before legacy global-key configuration can issue a lease. Keep per-spoke `HIVE_VISUAL_HIVE_GITHUB_APP_ENABLED=true` opt-in.

To disable, clear the optional assignment; the updated spoke clears execution authority on Hub denial. Use managed Hive uninstall/rebind when changing App/repository bindings and managed rollback/uninstall for repository settings. Preserve unrelated branch protections and ordinary Hive operation.

Before rolling the Hub back to an older build that can send Visual Hive PEMs, clear the secondary assignment and disable the optional spoke feature. Do not roll back with an active Visual Hive assignment. Human-reviewed repair PRs remain the qualification authority; no autonomous merges or fleet rollout are enabled.

## Recorded candidate checks (2026-09-14)

- Pinned Linux check image `sha256:bddc02ac77575a7904aaba6cc0787574d56bda7bfa2ba828eab029b73dbc938d`, UID/GID 1000, six CPUs, 6 GiB, disposable HOME: build, complete Hub/repair/integrated/cmd-hive package tests and vet passed.
- Relevant race tests passed for Hub assignment/secondary delivery, normal Visual Hive runtime, token lease denial and Codex prompt-echo handling.
- The packaged provider uses Codex 0.146.0. The patch now recognizes that exact reviewed banner when excluding the byte-identical initial prompt echo from output-secret scanning. Near matches, subsequent output secrets and unreviewed version banners remain scanned.
- Native 0.146.0 no-model health was tested in a separate non-root, read-only diagnostic container with no network, dropped capabilities, no-new-privileges, disposable HOME and an explicit executable temporary mount. A synthetic offline auth fixture was used: this proves containment/startup only, not model access. Without executable temporary storage, sealed-binary startup fails; without auth.json, authentication health blocks before any model call.
- Real App installation, hosted runtime and governed canary lifecycle qualification remain pending. No new paid model calls were made.

Full repository Linux build/tests/vet and relevant race checks subsequently passed on source `3538c53f1c4281bdf180b1a1b29f321f9f7922bd`, using image `sha256:27caf5e2488f850615628b311bce05c90296a2807f33d5a930d68d45d8d890d1` with a named non-root account, Node, tmux and writable disposable package directories. These receipts precede the release-workflow path fix; final revision results belong in the PR handoff.
