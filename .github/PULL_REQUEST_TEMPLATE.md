## Summary

- 

## Related issues

Fixes #

## Testing

- [ ] `cd src && go build ./...`
- [ ] `cd src && go test ./...`
- [ ] Other / not run (explain):


<details>
<summary>Adding or changing a CLI backend? (delete if not applicable)</summary>

See the [backend support tiers and acceptance bar](../src/docs/backend-support-tiers.md).

- [ ] PR body states the claimed tier per launch path (`pod` / `local`).
- [ ] List parity: `config/backends.conf` (`KNOWN_BACKENDS`, binary, `backend_perm_flag`) and Go backend lists/tests agree.
- [ ] Local posture is declared and tested (`backend_perm_flag` plus sandbox / deny-list / refusal-gated posture).
- [ ] Safety flags are honored: include the exact flag string and invalid-value transcript.
- [ ] Credential detection is honest (no `--version`-only success unless it proves auth).
- [ ] Docs rows updated in `docs/backend-setup.md` and `src/docs/sandbox-isolation.md`.
- [ ] Installs are pinned by version and checksum when added to images.
- [ ] Metering posture is stated (metered, explicitly unmetered, or blocked for budget-gated hives).

</details>

## Contributor checklist

- [ ] **Changelog** (required when `src/` code changes, or CI fails): added `changelog.d/<added|changed|deprecated|fixed|security>-<pr-or-slug>.md` containing one `- ` bullet — e.g. `echo '- Fix X when Y' > changelog.d/fixed-1234.md` — **or** the change is not user-visible and a maintainer added the `no-changelog` label. Never edit `CHANGELOG.md` directly (#5675).
- [ ] PR targets `v2` unless a maintainer requested another branch.
- [ ] Title uses the repo emoji convention, for example `📖 docs: ...`, `🐛 fix: ...`, or `✨ feature: ...`.
- [ ] Commits include DCO sign-off (`git commit -s`).
- [ ] Docs, examples, and policies are updated when behavior changes.
- [ ] No secrets, credentials, or local runtime state are committed.
