# UI render evidence

A pull request that changes dashboard sources or assets carries viewable
screenshots of the pages it touches, so a reviewer or an agent can say "I looked
at it" and be right ([#18915](https://github.com/hivecommons/hive/issues/18915)).

**These are renders of fixture data, not live data.** They show layout, spacing,
wrapping, theming and phone behaviour of the built page. They do not show how
the page behaves with a real hive's data, a slow or failing API, real
authentication, or a long-running session.

## What runs, and when

| Piece | What it does |
|---|---|
| `.github/workflows/dashboard-render-evidence.yml` | On `pull_request`, only when files under `src/pkg/dashboard/static-src/`, `src/pkg/dashboard/static/`, `src/pkg/hub/static/` or `src/pkg/hub/assets/` (or the evidence tooling itself) change. Builds nothing new: it renders the committed pages. Uploads the `render-evidence` artifact (screenshots and `index.json`) and a small `render-evidence-index` artifact. |
| `src/scripts/dashboard-render-evidence.mjs` | Serves the committed pages on loopback, answers `/api/*` from `src/scripts/fixtures/render-evidence/api/`, and screenshots them in headless Chrome. Chrome resolves only `127.0.0.1`, so nothing can reach a real hive or any other network, and no credential is read. |
| `.github/workflows/dashboard-render-evidence-comment.yml` | After the capture run, posts or updates **one** PR comment and records a `UI render evidence` check run. Runs the default branch's copy of itself and reads the capture job's index as untrusted data. |

Because the comment needs a write token, which fork PRs do not get on
`pull_request`, it is posted by the `workflow_run` sibling, never by code from
the PR. The capture job uses a read-only token and no secrets.

## What is captured

For each affected page: a full-page overview at **desktop (1440 wide)** and
**phone (390 wide, touch)**, each in **light** and **dark**. For the hive
dashboard, each section whose `static-src` file changed is also captured expanded
on its own (matched by the section's `html` partial, or by the css/js file name,
for example `sessions.css` and the Sessions section), up to four sections. A
change to shared styles that matches no section is covered by the overview.
Hub pages are captured as full pages.

The page is first made ready (stylesheets loaded, two stable frames), and each
screenshot has its own timeout.

## Reading the evidence

1. Open the sticky comment on the PR. Its first line says the status:
   - **captured** means every planned screenshot was taken.
   - **partial** means some were not (the reason says why, for example the time
     budget was reached). The ones listed exist.
   - **neutral** means none could be taken (for example no browser was available
     on the runner). It is not a failure and does not block a merge.
2. Follow the artifact link (sign in to GitHub) and open the PNGs listed in the
   table. The file name is `<page>-<section>-<viewport>-<theme>.png`.
3. Compare the same screenshot in light and dark, and phone against desktop.
   Look for clipped text, horizontal scrolling on phone, controls under other
   controls, and colours that disappear in one theme.
4. If the page shows an empty or error state where you expected data, check
   whether a fixture exists: a route with no file under
   `src/scripts/fixtures/render-evidence/api/` answers 404, on purpose, so the
   evidence never silently shows data it did not have.
5. Agents: cite the screenshot file names you looked at, and say it was a fixture
   render. "Verified in a browser" is only true of what was looked at and with
   what data.

## Limits

- Fixtures are synthetic. Names, hashes and counts are placeholders and say
  nothing about any real hive. Do not put real organisation, cluster, customer
  or user names or credentials in them.
- Only routes with a fixture have data. Add a file mirroring the route's response
  shape to extend what a change can show.
- Interactions (clicks, dialogs, drag, keyboard) are not exercised. The existing
  `Dashboard Mobile Shots` job and the Node harnesses under
  `src/pkg/dashboard/webstatic/testdata/` cover specific interactions.
- Screenshots can be flaky. A flaky capture is reported as partial or neutral and
  never fails the PR, so a missing screenshot is not evidence of a problem and a
  present one is not evidence of correctness.
- Time budget: the harness stops starting new screenshots after
  `RENDER_EVIDENCE_BUDGET_MS` (8 minutes, in `src/scripts/lib/render-evidence.mjs`).
- Artifacts are kept for 14 days.
- Chrome renders with the runner's fonts, which can differ from a user's browser.

## Changing it

The pure parts (which pages and sections are affected, the status rules, how the
comment is composed and how artifact data is sanitised) live in
`src/scripts/lib/render-evidence.mjs` with tests beside them. The comment text is
built only from a validated, reduced copy of the artifact, so a PR author cannot
inject markup, mentions or links through it.
