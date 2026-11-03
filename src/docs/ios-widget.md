# Hive on your iPhone Home Screen (widget)

You can already save a hive's URL to the Home Screen and get the mobile
dashboard (Safari → Share → **Add to Home Screen**; the bookmark is named
"Hive" and uses the bee icon from the dashboard's web app manifest). This adds a real **Home Screen widget** — the kind that sits in the
grid next to Weather and Calendar — showing the hive's open issues, open PRs,
and the "your turn" counts for each, every one with a sparkline. Tap it and the
hive dashboard opens.

iOS does not let a web page be a widget, so the widget runs inside
[Scriptable](https://scriptable.app) (free, App Store), which hosts
JavaScript widgets. The script lives in this repo at
[`src/pkg/dashboard/static/ios-widget/hive-widget.js`](../pkg/dashboard/static/ios-widget/hive-widget.js)
and every spoke serves its own copy at `/ios-widget/hive-widget.js`, so the
dashboard can hand it straight to Scriptable.

## What it shows

| Cell | Source | Colour |
|---|---|---|
| **ISSUES** — open issues across the hive's repos | sum of `repos[*].issues` from `/api/history` | blue |
| **PRS** — open PRs across the hive's repos | sum of `repos[*].prs` | green |
| **YOUR TURN · ISSUES** — your-turn issues that are root causes | `yourTurn.root_issues` (live) and `yourTurnRootIssues` (history); `govIssues` for older samples | amber |
| **YOUR TURN · PRS** — your-turn PRs that are root causes | `yourTurn.root_prs` (live) and `yourTurnRootPrs` (history); `govPrs` for older samples | amber |

The YOUR TURN cells count root causes: your-turn items that wait on no other
your-turn item, so eight PRs held up by one fix count once (see "The critical
path" in [dashboard.md](dashboard.md)). The live number comes from
`/api/status/summary` (or `/api/widget/summary` with a widget token), the
history from `/api/trend/history` (`yourTurnHistory` on the token path); samples from before the hive recorded root
causes keep the governor's actionable figure.

Each cell has the current number, a ▲/▼ change over the sparkline window
(down is green: fewer open items is good), and a sparkline. The footer shows
the window length and when the data was last fetched; it turns red if the
last fetch failed and you are looking at cached numbers.

Sizes: **medium** (2×4, recommended), **small** (2×2, abbreviated labels), and
a one-line **Lock Screen** variant (`🐝 20i 8pr · turn 1/2`).

Sparkline window: the spoke keeps roughly the last 7 hours of eval history in
memory; the widget stores one sample per 15 minutes on the phone and keeps 7
days, so the sparklines lengthen the longer the widget is installed.

## Install from the dashboard (recommended, about a minute)

On your iPhone, open the hive dashboard, tap your **avatar** → **📱 iPhone
widget**. The dialog walks through three steps:

1. **Add script to Scriptable** — fetches `/ios-widget/hive-widget.js` from
   the spoke and opens `scriptable:///add?scriptName=Hive&scriptContent=…`;
   Scriptable shows the script and asks you to confirm **Add**. If Scriptable
   is not installed the page stays in the foreground, and the dialog reveals
   an App Store link instead.
2. **Set up this hive.** If you are the hive's owner, tap **Create widget
   token & QR**. The dashboard mints a [read-only widget
   token](#read-only-widget-token) and shows a QR code; scan it with the
   iPhone camera (or tap **Open in Scriptable** when you are already on the
   phone). That opens `scriptable:///run/Hive?url=<this hive>&widgetToken=…`
   and the script stores both in the iOS Keychain — nothing to type.
   Not the owner? **Set up this hive in Scriptable** opens
   `scriptable:///run/Hive?url=<this hive>&setup=1`; the script stores the
   hive URL and asks only for a GitHub token (see below).
3. Add the Scriptable widget to the Home Screen and point it at `Hive`.

## Install by hand (about two minutes)

1. Install **Scriptable** from the App Store.
2. Create a GitHub token to prove who you are. Fine-grained, **no repository
   permissions**, any expiry you like:
   <https://github.com/settings/personal-access-tokens/new>. The hub's
   auth-check accepts `Authorization: Bearer <GitHub token>` for any user
   already allowed on the hive, which is why nothing has to be configured on
   the hive.
3. Add the script to Scriptable. Either:
   - open `https://<your hive>/ios-widget/hive-widget.js` (or
     <https://raw.githubusercontent.com/hivecommons/hive/v5/src/pkg/dashboard/static/ios-widget/hive-widget.js>)
     on the phone, select all, copy; in Scriptable tap **+**, paste, and name
     it `Hive`; or
   - AirDrop / iCloud the file into Scriptable's folder in Files.
4. Run the script once inside Scriptable (tap it). It asks for the **hive URL**
   (e.g. `https://hosted-kubestellar-console-4vkt.hive.hivecommons.dev`) and
   the **token**, saves both in the iOS Keychain, and shows a preview.
5. Long-press the Home Screen → **+** → search **Scriptable** → pick the
   medium size → **Add Widget**. While it still shows "Choose script",
   long-press the widget → **Edit Widget** → Script: `Hive`.

That is it. The widget refreshes itself roughly every 15 minutes (iOS decides
the exact cadence) and opens the hive when tapped.

## Read-only widget token

The owner of a hive can mint a widget token from the dashboard (avatar →
**📱 iPhone widget** → **Create widget token & QR**). It is the preferred
credential for the widget:

- It can do exactly one thing: `GET /api/widget/summary`, which returns the
  eval history for the sparklines and each agent's name and state. Every other
  endpoint — including every mutation and the token endpoints themselves —
  answers 401 to it. It is not a session and is never accepted in
  `Authorization`.
- The widget sends it in the `X-Hive-Widget-Token` header. On hosted hives the
  hub lets that one path through to the spoke, which checks the token; the hub
  never sees it as a GitHub token.
- One token per hive. Creating a new one replaces the old one immediately;
  **Revoke widget token** deletes it and widgets using it stop updating.
- Only the token's SHA-256 is stored on the hive (`/data/widget-token.json`,
  mode 0600). The plaintext is shown once, when it is created, and lives only
  in the iOS Keychain afterwards (per hive, so several hives can each have
  their own).
- The dialog shows when the token was created and when a widget last used
  it, so a stale token is obvious.
- Only owners can create, revoke, or see the status of the token
  (`GET`/`POST`/`DELETE /api/widget/token`).

To set it up by hand instead of scanning the QR code, paste the `hwt_…` token
into the token field of the script's setup form.

## Several hives

Add one widget per hive. In **Edit Widget**, set the **Parameter** field to
that hive's URL; the token in the Keychain is shared. A widget with no
parameter uses the URL you entered at setup.

## Fleet widget (hub mode)

The same script can show every hive you can see on the hub at once. Set the
widget's **Parameter** to `hub` (or `hub:<hub URL>` for a hub other than
`https://hive.hivecommons.dev`). To set it up, run the script once in Scriptable
via `scriptable:///run/Hive?hub=1` (or `?hub=<hub URL>`); it asks for a GitHub
token if none is stored yet. Add `&setup=1` to change the hub URL or token later.

| Size | Shows |
|---|---|
| **medium** | Fleet totals — open issues, open PRs, and the "your turn" issues and PRs — each with a sparkline, plus **N hives need you** (hives with any actionable issue or PR) |
| **large** | The medium view plus the hives with the most actionable work (`act_issues + act_prs`), each with its online dot, counts and ACMM level; tap a row to open that hive |
| **small** | The two "your turn" totals and the "need you" line |
| **Lock Screen** | `🐝 12 hives · 3 need you · turn 5/4` |

Tapping the widget opens the hub's **Fleet** page.

- Data comes from one call, `GET /api/hub/widget`, with the GitHub token in
  `Authorization: Bearer`. It uses the hub's normal sign-in check: hub admins
  see every hive, everyone else sees the hives they own or have been granted.
  A per-hive widget token (`hwt_…`) does not work on the hub.
- The "your turn" sparklines use the history the hub already keeps from
  heartbeats (one sample per 15 minutes, 7 days). The hub does not keep a
  history of total open issues and PRs, so those sparklines are built from the
  samples the widget stores on the phone and grow the longer it is installed.
- Open issue and PR counts come from each hive's heartbeat. Hives running a
  build that does not report them yet show 0 until they upgrade.

## Changing URL or token

In Scriptable, run the script with the setup form again: open the script
editor, tap the ▶ with the parameter `?setup=1`, or simply delete the Keychain
entries by running

```js
Keychain.remove("hive.widget.github-token");
Keychain.remove("hive.widget.default-url");
Keychain.remove("hive.widget.hub-url");
```

in a new script.

## Troubleshooting

| Widget says | Meaning |
|---|---|
| "Open Scriptable and run this script once…" | No URL/token saved yet — do step 4. |
| "Can't reach hive HTTP 302" / "not JSON (login page?)" | The token was not accepted: wrong token, expired, or your GitHub user is not allowed on that hive. |
| "Can't reach hive HTTP 401 /api/widget/summary" | The widget token was replaced or revoked. Create a new one in the dashboard and scan its QR code again. |
| "Can't reach hive HTTP 404" | The URL is not a hive dashboard (check for a path or typo). |
| Numbers with a red "stale · Nh ago" footer | Phone is offline or the hive is down; the last good numbers are shown. |

## Privacy and security notes

- The token is stored only in the iOS Keychain, never in the widget parameter
  (which iOS stores in plain text) and never written to the history file.
- With a widget token the widget only calls `GET /api/widget/summary`; with a
  GitHub token it calls `GET /api/history` and `GET /api/status/summary`.
  In hub mode it calls only `GET /api/hub/widget` on the hub.
- The QR code encodes a `scriptable://` link, so the token goes straight from
  the camera to Scriptable and is never sent to any web server.
- If you use a GitHub token, use one with no repository permissions: it is an
  identity proof, not an access grant.

## Not yet

- The Trust/Prod/Satis/Effic fleet quadrant in the fleet widget. The hub already serves it as `fleet_quadrant` on `/api/saas/my-hives` (per-axis median with `n`, `criteria[]` and a plain `summary`; the shape only grows — see [Fleet quadrant](fleet-health.md#fleet-quadrant)), so a widget can read it today.
- A native app with WidgetKit widgets and push notifications.
