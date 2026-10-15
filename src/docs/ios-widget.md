# Hive on your iPhone Home Screen (widget)

You can already save a hive's URL to the Home Screen and get the mobile
dashboard. This adds a real **Home Screen widget** — the kind that sits in the
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
| **YOUR TURN · ISSUES** — actionable issues | `govIssues` | amber |
| **YOUR TURN · PRS** — actionable PRs | `govPrs` | amber |

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
2. **Set up this hive in Scriptable** — opens
   `scriptable:///run/Hive?url=<this hive>&setup=1`; the script stores the
   hive URL and asks only for the GitHub token (see below).
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

## Several hives

Add one widget per hive. In **Edit Widget**, set the **Parameter** field to
that hive's URL; the token in the Keychain is shared. A widget with no
parameter uses the URL you entered at setup.

## Changing URL or token

In Scriptable, run the script with the setup form again: open the script
editor, tap the ▶ with the parameter `?setup=1`, or simply delete the Keychain
entries by running

```js
Keychain.remove("hive.widget.github-token");
Keychain.remove("hive.widget.default-url");
```

in a new script.

## Troubleshooting

| Widget says | Meaning |
|---|---|
| "Open Scriptable and run this script once…" | No URL/token saved yet — do step 4. |
| "Can't reach hive HTTP 302" / "not JSON (login page?)" | The token was not accepted: wrong token, expired, or your GitHub user is not allowed on that hive. |
| "Can't reach hive HTTP 404" | The URL is not a hive dashboard (check for a path or typo). |
| Numbers with a red "stale · Nh ago" footer | Phone is offline or the hive is down; the last good numbers are shown. |

## Privacy and security notes

- The token is stored only in the iOS Keychain, never in the widget parameter
  (which iOS stores in plain text) and never written to the history file.
- The widget only calls `GET /api/history` and `GET /api/status/summary`.
- Use a token with no repository permissions: it is an identity proof, not an
  access grant.

## Not yet

- A dedicated read-only "widget token" minted from the hive's dashboard (so a
  GitHub token is not needed) and an "Add to iPhone" page with a QR code.
- Hub-level fleet widget (all your hives in one glance) — owner only.
- A native app with WidgetKit widgets and push notifications.
