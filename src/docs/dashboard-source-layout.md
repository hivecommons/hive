# Dashboard source layout

`src/pkg/dashboard/static/index.html` — the single document the spoke
dashboard serves — is a **generated artifact**. Its sources live in
`src/pkg/dashboard/static-src/`, one file per dashboard area, and a tiny
generator concatenates them, byte for byte, into the committed `index.html`.

The rule, in one line:

> **Edit `static-src/**`, run `make dashboard-gen`, commit both.**

`TestIndexHTMLIsGenerated` (`pkg/dashboard/webstatic/indexgen_test.go`)
regenerates the document in memory and fails when the committed artifact
differs from its sources, so an edit made directly to `index.html` — or a
source edit without regeneration — is caught in CI, not in a review.

## Why a generated artifact and not a build step

`index.html` is embedded with `//go:embed static` (`server.go`), gzipped once
at startup, and its inline style sheet is hashed into the `Content-Security-Policy`
header. Dozens of tests read `../static/index.html` and assert on literal
ids, classes and markup; the node harnesses under `webstatic/testdata/` load JS
functions straight out of it. Keeping the artifact committed means none of that
changes: the served bytes are identical, CSP hashes are identical, and the
embed, the lint workflow and every existing test are untouched. What changes
is *where edits land* — a toast fix touches `css/alerts-toasts.css`, a tracker
fix touches `js/tracker-row.js`, and two such PRs no longer conflict on one
60 000-line file.

The sources sit in a sibling directory (`static-src/`, not `static/src/`) on
purpose: `//go:embed static` embeds everything under `static/` recursively, so
sources placed inside it would be compiled into the binary and served.

## Layout

```
src/pkg/dashboard/
├── static/
│   └── index.html          ← GENERATED. Do not edit.
├── static-src/
│   ├── manifest.txt        ← ordered list of parts; blank lines and # comments ignored
│   ├── head.html           ← <!DOCTYPE> … <head> … up to and including <style>
│   ├── css/<area>.css      ← inline style sheet, one file per area
│   ├── glue/*.html         ← the wrapper tags between regions (</style>…<body>…<script> etc.)
│   ├── js/boot.js          ← the small boot script that runs before the body
│   ├── html/<area>.html    ← body templates, one file per section
│   └── js/<area>.js        ← the main script, one file per function group
└── webstatic/
    ├── indexgen.go         ← ParseIndexManifest / ConcatIndex + the go:generate directive
    ├── indexgen_test.go    ← TestIndexHTMLIsGenerated and manifest hygiene tests
    └── gen/concat.go       ← `go run ./gen -src ../static-src -out ../static/index.html [-check]`
```

The generator is a pure concatenation: no separators, no rewriting, no
reformatting. The `glue/` files hold the `</style>`, `<body>`, `<script>` and
`</script>` tags, so every `.css` and `.js` part is pure CSS or JS that an
editor can lint on its own. Each part ends exactly where the next begins — at a
blank line or a section comment — so the document reads the same as before the
split.

### Areas

CSS (`css/`): `base` (tokens, reset, shared primitives), `sidebar`, `topbar`,
`agent-detail`, `agent-cards`, `auth`, `agent-state`, `tracker` (Hive activity
monitor), `agent-fields`, `governor`, `advisory-lifecycle`, `evidence`,
`throughput`, `repos`, `label-filter`, `overview`, `sparklines-indicators`,
`governor-matrix`, `cost`, `alerts-toasts`, `feedback-faq`, `config-dialog`,
`chat`, `nous`, `knowledge`, `sections-collapse`, `design-system`,
`responsive`, `recipes-utilities`, `welcome`, `review-campaigns`, `variables`.

Body HTML (`html/`): `auth`, `root-notices`, `chat`, `dialogs`, `sidebar`,
`topbar`, `overview-governor`, `throughput-advisory`, `cost`, `repos`,
`acmm-approvals-audit`, `review-queue`, `nous-inception-knowledge-contributors`,
`debug-logs`, `agents`, `tracker`, `faq`.

JS (`js/`): `boot`, `core`, `feedback`, `sections`, `layout`, `agent-detail`,
`sidebar`, `history-sparklines`, `cost-trend`, `stats`, `agent-now`,
`tracker-words`, `tracker-row`, `tracker-row-strip`, `tracker-row-acted`,
`tracker-row-layout`, `tracker-motion`, `tracker-filter`,
`tracker-filter-state`, `tracker-filter-render`, `tracker-filter-fetch`,
`tracker-actions`, `tracker-merged`, `tracker-sound`, `agent-cards`, `banners`,
`governor-view`, `repos`, `overview`, `label-filter`, `pinned-filters`,
`repos-render`, `budget`, `backends-tokens`, `cost`, `acmm-lifecycle`,
`throughput`, `evidence`, `approvals`, `nous`, `knowledge`, `vault-audit`,
`review-queue`, `contributors`, `refresh`, `inception`, `debug-logs`,
`tooltips-dialogs`, `plan-agent-controls`, `acmm`, `version`, `sse-bootstrap`,
`config-state`, `settings-search`, `config-dialog`, `config-tabs`,
`config-hub`, `config-agent`, `config-governor`, `config-watchdog-security`,
`config-features`, `config-sentinel`, `compliance`, `config-helpers`,
`linear-repos-save`, `chat`, `gh-auth`, `nps`, `welcome`, `shimmy-handlers`.

Function groups were kept intact: a part begins at a section banner or the
blank line before a top-level function, never inside one. When a function
clearly belongs to another area than the file it sits in, move it — the
generator does not care about order inside a region as long as the manifest
order still yields a document where every definition precedes its first use.

## Working with it

```sh
make dashboard-gen          # regenerate static/index.html from static-src/
make dashboard-gen-check    # fail if the committed index.html is stale
cd src && go test ./pkg/dashboard/webstatic/ -run 'TestIndex|TestConcatIndex'
```

`make dashboard-gen` runs `go generate ./pkg/dashboard/webstatic/`, whose
directive is in `indexgen.go`.

**Adding an area.** Create the file under `css/`, `html/` or `js/`, add its
path to `manifest.txt` at the position where its bytes must appear (CSS inside
the `<style>` region, templates inside `<body>`, functions inside the main
script), regenerate. `TestIndexManifestEveryFileListedAndUsed` fails if a file
exists under `static-src/` but is not in the manifest, or vice versa.

**Reviewing.** Review the diff to `static-src/**`; the `index.html` hunk is the
same change again and can be collapsed. If a PR changes `index.html` without
a matching change under `static-src/`, it edited the artifact — ask for the
sources.

## Merge conflicts

Because the artifact is regenerated, conflicts in `static/index.html` are never
resolved by hand:

1. Take the base branch's copy: `git checkout --theirs src/pkg/dashboard/static/index.html`
   (during `git rebase`, "theirs" is the branch being rebased onto).
2. Resolve any real conflicts under `static-src/**` normally — they are small
   and per area.
3. `make dashboard-gen`, then `git diff --stat src/pkg/dashboard/static/index.html`
   should show only your intended change on top of the base.
4. `make dashboard-gen-check` and `git add` both.

If the base branch changed `index.html` directly (an edit that bypassed the
sources), step 3 shows the drift: move that delta into the right part file and
regenerate until the check passes.

No `.gitattributes` merge driver (`merge=union` or similar) is used for the
artifact: a union merge of a 60 000-line document silently interleaves
unrelated hunks and would produce an `index.html` that no longer matches its
sources, which `TestIndexHTMLIsGenerated` would then reject anyway. Regenerating
is both simpler and always correct.

## Related

- [Spoke dashboard](dashboard.md) — how the dashboard behaves and is built.
- [Dashboard design system](dashboard-design-system.md) — tokens and component variants the CSS parts use.
- [Dashboard sections explained](dashboard-sections.md) — operator help per section.
