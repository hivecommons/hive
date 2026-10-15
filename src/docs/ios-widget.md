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
| "Can't reach hive HTTP 401 /api/widget/summary" | The widget token was replaced or revoked. Create a new one in the dashboard and scan its QR code again. |
| "Can't reach hive HTTP 404" | The URL is not a hive dashboard (check for a path or typo). |
| Numbers with a red "stale · Nh ago" footer | Phone is offline or the hive is down; the last good numbers are shown. |

## Privacy and security notes

- The token is stored only in the iOS Keychain, never in the widget parameter
  (which iOS stores in plain text) and never written to the history file.
- With a widget token the widget only calls `GET /api/widget/summary`; with a
  GitHub token it calls `GET /api/history` and `GET /api/status/summary`.
- The QR code encodes a `scriptable://` link, so the token goes straight from
  the camera to Scriptable and is never sent to any web server.
- If you use a GitHub token, use one with no repository permissions: it is an
  identity proof, not an access grant.

## Not yet

- Hub-level fleet widget (all your hives in one glance) — owner only.
- A native app with WidgetKit widgets and push notifications.
