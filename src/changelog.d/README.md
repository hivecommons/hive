# Wrong directory — changelog fragments live at the repository root

Changelog fragments go in [`changelog.d/`](../../changelog.d/README.md) at the
**repository root**, not here under `src/`. `src/scripts/compile-changelog.sh`
only reads the root directory, so a fragment placed here is never compiled into
`CHANGELOG.md`, and the `changelog-fragment-guard` check rejects it (#11919).

Name the file `changelog.d/<added|changed|deprecated|fixed|security>-<pr-or-slug>.md`
and make its content a single `- ` bullet. If you already added one here, move it:

```sh
git mv src/changelog.d/<file>.md changelog.d/<file>.md
```

See [`changelog.d/README.md`](../../changelog.d/README.md) for the full format.
