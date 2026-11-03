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

Activity row headers share the progress strip's physical grid. Their issue
metadata spans Open through Claimed, even without a PR; PR metadata starts at
the canonical PR in progress boundary and spans through Merge gate, including
the independent CI and Review tracks. Modifier/action badges wrap in their own
full-width, far-right-anchored toolbar line, rather than overflowing the history
cap into a title or ending in an issue-side column. At narrow widths the header
slots stack and wrap; small-screen progress strips scroll horizontally without
moving header controls offscreen. `stage_tracker_header_spans.test.js`
checks rendered markup and track arithmetic in CI; these are not visual proof.
The existing Dashboard Mobile Shots workflow additionally runs the generated
SPA in real Chrome against `scripts/fixtures/dashboard-activity-density.mjs`,
preserving the preparation-rail suite. It adds activity-only desktop widths
2400, 1440, 1110 and 1024 to the existing mobile/tablet viewports (including
768 and 390), in both themes. Canonical lane boundaries are carried in
`data-ham-pr-start` and bound to the CSS custom property by the production
row materializer, including nested rows; no parallel hardcoded span is used.
Browser fixture evidence is explicitly mocked and rebased to the browser clock;
permissions are mocked only in this isolated file-page, restored on cleanup.
This does not validate private authenticated live projection or production rollout.
The existing `dashboard-mobile-shots` artifact contains `activity-layout.json`,
per-viewport/theme `*-activity-probes.json`, full-document `*-activity-density.png`
and actual-viewport `*-activity-density-viewport.png`, with
PR head, checkout SHA and generated-asset SHA256 provenance. Activity probe
violations fail capture after saving the results/screenshots for every viewport;
the existing reporting-only workflow policy and permissions are unchanged.
In addition to the DOM-handler probes, real CDP mouse movement and
ArrowDown/Enter/Space key dispatch check tooltip takeover and entry into an
already-open decision dialog. `*-activity-decision-pointer.png` and
`*-activity-decision-keyboard.png` capture the actual viewport during those
browser interactions, rather than a full-document composite of fixed dialogs;
the isolated headless page owns emulated focus, recorded in pointer preconditions.
Geometry failures include the actual control hit target or measured content gap;
no decision action is activated. Fixture/probe failures remain failures, not
skipped or string-assertion substitutes for browser proof.
Persistent human/preparation context overrides the generic event-entry animation
so transient translation cannot move neighboring narrow captions. Keyboard entry
focuses decision actions without scrolling, which would otherwise close the
dialog through its scroll-away listener. Control probes await a bounded stable
scroll/visual viewport before every mandatory hit-test; native pointer/keyboard
reports also retain each decision action's bounds and actual hit target.
Before any state probes or screenshots, bounded render readiness waits for
the actual body/root opacity and visibility and two stable layout frames.
It never forces styles or treats a fixed sleep as proof: the file-page cannot
receive the status refresh that normally reveals the body before its boot
fallback. Each viewport/theme report records page readiness; viewport captures
also record their own readiness. A real timeout remains a capture failure,
including when animation frames never arrive.
Fixture transitions use the existing native overlay dismissal policy for all
three legacy modal producers: Settings, Knowledge Sources, and terminal/export.
Cleanup is bounded, waits for the production modal observer, and rejects any
remaining fixture modal or root/body scroll lock. It does not delete modal DOM,
hide overlays manually, or remove scroll-lock classes to manufacture a pass.
Each activity report records cleanup evidence. Native pointer preconditions
also hit-test the trigger itself. Viewport captures explicitly target the
current document scroll rectangle; density and pointer/keyboard reports inspect
their actual PNG pixels, excluding scrollbars, and fail a uniform-background
capture. This paint check only rejects empty evidence; nonblank images still
require visual inspection and do not replace geometry or interaction checks.
The 12:36 screenshot shapes additionally exercise nested #16450 and root
#16451 recorded/unverified preparation fronts, #16446 awaiting a superseding
human decision, pluk #436 assigned review after passing CI, #16437 approved
review with unknown CI, and Hotshot #344 queued CI with approved review.
These identities/relationships are acceptance inputs, not live stage claims.
Every rendered progress strip must own a visible current location or delegate
to drawn PR children; closed adopted-original notes intentionally own no strip.
The existing preparation-root rail gutter is retained: cross-row comparison
uses like gutters, while every header is measured against its own lane edges.
The same probes can be invoked on a dedicated published-preview fixture tab:
invoke
`webstatic/testdata/tracker-header-geometry.js` as
`(<file contents>)({ dedicatedPreview: true })` there at desktop and narrow
widths with issue-only, linked, and PR-only rows present, including long titles
and waiting/regression badges. It asserts measured lane boundaries, metadata
containment, badge separation and header-to-progress separation. Every relevant
header control must stay horizontally contained and pass center hit-testing;
the probe deliberately scrolls vertically offscreen controls into view and
restores scroll positions rather than silently skipping them. It never activates
controls. Do not run interaction/scroll probes in the user's live dashboard tab.
Nested PRs use the same explicit bounded grid-track contract as root rows,
not implicit min-content tracks that long identity/context text can widen.
The probe also compares physical lane positions/heights across rows and bounds
vertical gaps by the actual CSS gap/margins, allowing real wrapped content but
rejecting unexplained empty space.

`webstatic/testdata/tracker-activity-density.json` records acceptance shapes from
the owner's 12:01 screenshot: #16404 with #16442/#16378, Hotshot #343/#344,
#16438 with #16446/#16449, issue-only #16420, and a PR-only #16437 view variant.
It preserves full GitHub-read titles, author identities, heads and long waiting
captions. Its stage/activity projections and frozen clock are deterministic test
inputs, not a claimed live backend snapshot. Use these structures plus long
unbroken-handle stress data for preview coverage; a fixture passing is not proof
that the published application renders correctly.

The split CI/Review lanes are chevrons like every other stage — the same notch
polygon, padding and content-driven height as their neighbours, never a bordered
card (the boxed opt-out of #16384 is gone). They express lane state the way the
other chevrons do, through fill, tint and text colour: `done` (passed/approved),
dimmed `todo`, the lit `active` fill for fresh observed work, a muted amber tint
for `ham-current` occupancy, and an amber-on-tint treatment for failing CI or
changes requested. They also distinguish position from activity: a steady
current-phase tint and accessible current-phase label identify the occupied
phase, even when evidence is unknown, work is queued or a PR is held. The running-work class and
motion still require fresh current-head activity evidence; completion remains
evidence-based. Both parallel tracks can occupy the current phase without
claiming either is running. Native stages, holds and merge authority do not
change.

All progress chevrons share a two-line text budget, wrapping even unbroken
handles, but a row is only as tall as its longest caption: one line of text gives
a one-line-tall chevron, two lines a two-line-tall one, never more (the grid row
sizes to content above a one-line floor; sibling cells stretch to the row's
tallest chevron, not to a fixed box). The caption, with its reserved icons, is
centred in the chevron both horizontally and vertically, as before the two-line
budget; a wrapped second line is centred under the first. Icons have reserved
space clear of the chevron points. Full details
remain in the native title and/or shared hover/focus tooltip portal; links and
decision controls remain interactive. Decision chevrons, including native
merge-gate claim conflicts, instead use their existing decision dialog as the
sole hover/focus detail surface. The gate caption remains a decision button;
full caption, reason and head metadata go to its keyboard-accessible Details,
without a competing tooltip or extra focus stop. The dedicated-preview
`webstatic/testdata/tracker-decision-interaction.js` probe uses the same invocation
to check pointer hover, focus, ArrowDown/Enter/Space entry, action hit-testing,
and tooltip exclusion. It never activates a decision action. Unknown, stale or old-head CI/Review
observations show only the existing 050 column glyph, with their full source,
reason, owner and head metadata available in the keyboard-accessible tooltip.
Unknown does not mean running, passed or approved. Known track captions use the
same two-line budget; fixer/diagnostic detail belongs in the tooltip.
PR source repair after failed CI or review findings is labelled **Remediating**
when current-head verified work is observed. Native `fixing` state identifiers
and `stage/070-fixing` remain unchanged; waiting for an author/fixer, stale
evidence and re-review do not become active remediation. General Fix actions,
PR titles and recorded historical text are not renamed.

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
monitor), `tracker-native-queue`, `agent-fields`, `governor`, `advisory-lifecycle`, `evidence`,
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
`tracker-words-core`, `tracker-words-stage`, `tracker-words-gate`,
`tracker-words`, `tracker-row-anchor`, `tracker-row`, `tracker-row-strip`,
`tracker-row-acted`, `tracker-human-attention`, `tracker-row-layout`,
`tracker-row-title`,
`tracker-row-render`, `tracker-motion`, `tracker-filter`,
`tracker-filter-state`, `nav-count-delta`, `tracker-preparation`,
`tracker-native-queue`, `tracker-filter-render`,
`tracker-filter-fetch`, `tracker-actions`, `tracker-merged`, `tracker-queue`,
`tracker-sound`, `tracker-waiting`, `tracker-popout`, `agent-cards`, `banners`,
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

### Shared with the hub

`js/tracker-row-anchor.js` (which strip column a stage lights) and
`js/tracker-row-title.js` (the escaped, text-only row title) are also embedded
by `src/pkg/webassets/trackerrow`, so the hub can draw rows with the same
code without importing `pkg/dashboard`. The generator only reads
`static-src/`, so the two copies are kept byte-identical by hand:
`TestTrackerRowSharedAssetCompat` fails when they drift or when
`index.html` stops carrying them verbatim, and
`TestTrackerRowSharedAssetRendersSpokeFixtures` renders the same fixtures
through both. Edit both copies in the same PR.

The same rule applies to the Activity Monitor URL-view and display-playlist
helpers: `stageTrackerDefaultFilter` (`js/tracker-words-stage.js`),
`stageTrackerNormalizeFilter` (`js/tracker-filter.js`) and the
`tracker url view helpers` and `tracker display playlist helpers` blocks of
`js/tracker-filter-state.js`. `src/pkg/webassets/trackerview/tracker-view.js`
carries a verbatim copy so the hub Fleet activity page reads `am.*`, `am.view`
and `interval` as a spoke display does
([#16064](https://github.com/hivecommons/hive/issues/16064)).
`TestTrackerViewSharedAssetCompat` (in `pkg/dashboard`) compares that copy
with the declarations in the committed `static/index.html` and fails when they
drift: after editing one of these spoke helpers, run `make dashboard-gen` and
re-copy them into `tracker-view.js` in the same PR.

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
