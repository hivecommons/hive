# Hub Fleet activity page

The **Fleet activity** page at `https://<hub>/activity` is a read-only board of
the open issues and pull requests that the hives you can see are working on.
It is the hub-side view of the Activity Monitor: each spoke attaches a bounded
activity digest to its heartbeat, and the hub merges the digests of your hives
into one board.

Open it from the **📡 Fleet activity** link next to **🔬 Evidence** on the
dashboard's Fleet quadrant card, or go to `/activity` directly. You must be
signed in; a browser without a hub session is sent to `/login`.

The design is RFC [#15861](https://github.com/hivecommons/hive/issues/15861)
by @hanthor, delivered in [#15965](https://github.com/hivecommons/hive/issues/15965)
and its children. The page itself is [#16063](https://github.com/hivecommons/hive/issues/16063).

## Who sees what

The page shows exactly what `GET /api/hub/activity` returns for you. That
endpoint decides access per hive on the server before it reads any data:

- A hub admin sees every registered hive.
- Everyone else sees the hives they own and the hives they hold an unexpired
  grant for.
- Another hive's rows, counts and freshness never reach your browser, so they
  are not in the page and not in its ETag.

The page itself carries no fleet data. It is a static shell that loads the
board from the endpoint.

## Reading the board

Each row is one open issue or PR:

| Column | Shows |
| --- | --- |
| **Hive** | A colour chip per hive. The colour is stable per hive id. The chip opens that hive's own dashboard and tracker through the hub's hive route (`/api/saas/hives/<id>/open`), which signs you in or explains why it cannot. |
| **Item** | `owner/repo#N` and whether it is an issue or a PR. It links to the item on GitHub. |
| **Title** | The item title as plain text, capped at 200 characters by the hub. A leading `[agent]` tag is drawn as a chip. |
| **Stage** | The strip column the spoke tracker would light (Open, Actionable, Claimed, PR in progress, CI / Review, Merge gate, Merged) and the stage slug. |
| **Author** | `human`, `clanker` or `hive` when the spoke knows it. |

Rows come in the endpoint's order: most urgent first, then furthest stage.

**Group by hive** is on by default. It draws one header row per hive, even when
the hive has no open items. The header shows:

- the item count, plus the hive's tracked total when the digest held some back;
- freshness:
  - `fresh`: the hive's activity report is recent;
  - `stale`: the last activity report is older than two missed heartbeats
    (more than 4 minutes);
  - `not reporting`: the hive heartbeats but sends no activity block.
    Upgrade it to report activity;
  - `missing`: you hold a grant for a hive the hub registry does not know.

Turn grouping off for one flat list. The Hive column still tells rows apart.

## What the board leaves out

The list above the table is always visible. It never hides in a tooltip. It
names every gap in the data:

- items left out by the hub's response cap;
- items a hive's own digest cap left out (`truncated`);
- events dropped before they reached the hub (`events_dropped`);
- stale, not-reporting and missing hives.

When nothing is missing it says so: "All N hives reporting; nothing left out".

## Refresh

The page polls `GET /api/hub/activity` every 15 seconds with `If-None-Match`.
The hub answers `304 Not Modified` while nothing in your view changed, and the
board is redrawn only when the ETag changes. Polling pauses while the tab is
hidden and resumes, with an immediate refresh, when you come back. The status
line shows when the data last changed and when it was last checked. If your
session expires, the page sends you to `/login`.

## What the page does not do

- It has no action menus. The hub is visibility-only: act on an item on
  GitHub, or from the hive's own tracker.
- Display mode (`?kiosk=1`, `am.*` URL state, `?display=1` and a
  `hub-activity` playlist view) is not part of this page yet; it is tracked in
  [#16064](https://github.com/hivecommons/hive/issues/16064).

## Security

- The page is served with its own Content-Security-Policy. Scripts and styles
  load only as files from the hub (`'self'`). Inline scripts, inline event
  handlers and inline style attributes are refused, and the page loads nothing
  from other origins.
- Titles and every other value from a spoke are written as text, never parsed
  as HTML.
- The page, its script and the shared row-presentation asset are revalidated
  on every load (`Cache-Control: no-cache` with a content ETag), so a hub
  upgrade is picked up on the next visit.
- `/activity` is marked `noindex` and is disallowed in `robots.txt`.

## For operators

- A spoke publishes its digest only when `hub.activity.enabled` is on. It is
  on by default for hosted spokes and opt-in for self-hosted ones. A hive that
  heartbeats without it shows as `not reporting`.
- Digests live in hub memory only. After a hub restart, hives show
  `not reporting` until a later heartbeat carries a full digest again.
- Rows are drawn with the tracker's own row-presentation asset
  (`src/pkg/webassets/trackerrow`), which the hub serves under
  `/activity/assets/`. The page defines the stage constants that asset reads,
  with the spoke's values, so a hub row lights the same column as the spoke
  tracker.
