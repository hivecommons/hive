// The embedded SaaS dashboard single-page document served by
// handleDashboard (moved verbatim from saas.go; see #5564).
package hub

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>🍯</text></svg>">
  <meta property="og:title" content="My Hives — Hive Hub">
  <meta property="og:description" content="Manage your AI agent hives. View local and hosted hive instances, monitor status, upgrade, and control access.">
  <meta property="og:type" content="website">
  <meta property="og:site_name" content="Hive Hub">
  <!-- GA4 --><script async src="https://www.googletagmanager.com/gtag/js?id=G-4707R797K3"></script><script>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments)}gtag("js",new Date());gtag("config","G-4707R797K3");</script>
  <title>My Hives — Hive Hub</title>
  <style>
    :root {
      color-scheme: dark;
      --bg: #080b0f;
      --bg-soft: #0d1218;
      --panel: #121922;
      --panel-strong: #17212d;
      --text: #f6f8fb;
      --fg: #f6f8fb;
      --muted: #a8b3c2;
      --line: #263545;
      --amber: #f4c75f;
      --green: #74df9a;
      --blue: #80bfff;
      --red: #ff7e7e;
      --purple: #b482ff;
      --shadow: 0 24px 80px #0006;
      /* Legacy token aliases used by dashboard markup and scripts */
      --surface: var(--panel);
      --border: var(--line);
      --accent: var(--amber);
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
    }
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body {
      background:
        radial-gradient(circle at 18% 8%, #80bfff2e, transparent 24rem),
        radial-gradient(circle at 86% 4%, #f4c75f29, transparent 20rem),
        linear-gradient(180deg, #090d12 0%, var(--bg) 48%, #0a0e13 100%);
      min-width: 320px;
      min-height: 100vh;
      color: var(--text);
    }
    a { color: var(--accent); text-decoration: none; }
    a:hover { text-decoration: underline; }
    input, textarea, select, button { font-family: inherit; }

    /* ── Header ── */
    .site-header {
      z-index: 50;
      backdrop-filter: blur(18px);
      -webkit-backdrop-filter: blur(18px);
      background: #080b0fc7;
      border-bottom: 1px solid #ffffff14;
      display: grid;
      grid-template-columns: 1fr auto 1fr;
      align-items: center;
      gap: 1.5rem;
      padding: 1rem clamp(1rem, 4vw, 4.5rem);
      position: sticky;
      top: 0;
    }
    .brand, .header-link, .site-header nav { align-items: center; display: flex; }
    .brand { letter-spacing: 0; gap: .75rem; font-weight: 800; color: var(--text); }
    .brand:hover { text-decoration: none; }
    .brand-mark {
      width: 2.5rem; height: 2.5rem;
      color: var(--amber);
      background: linear-gradient(145deg, #f4c75f2e, #80bfff1a);
      border: 1px solid #f4c75f6b;
      border-radius: .65rem;
      place-items: center;
      font-size: 1.2rem;
      display: grid;
    }
    .site-header nav { color: var(--muted); gap: 1.35rem; font-size: .94rem; flex-wrap: nowrap; }
    .site-header nav a { color: var(--muted); white-space: nowrap; }
    .site-header nav a:hover, .header-link:hover { color: var(--text); text-decoration: none; }
    .header-link {
      border: 1px solid var(--line);
      width: fit-content;
      color: var(--text);
      border-radius: .55rem;
      justify-self: end;
      padding: .72rem 1rem;
      font-weight: 700;
    }
    .header-right { display: flex; align-items: center; gap: .8rem; justify-self: end; }
    .nav-user { display: inline-flex; align-items: center; gap: 6px; white-space: nowrap; color: var(--text); }
    .nav-avatar { width: 28px; height: 28px; border-radius: 50%; }
    /* The country flag beside an avatar. Sized just under the surrounding text
       so it reads as a marker rather than as a second avatar, and given a fixed
       line-height so a tall emoji cannot stretch the row it sits in. No color
       of our own — the glyph carries its own, and the box is transparent, so
       this behaves identically in light and dark. */
    .country-flag { font-size: 0.95rem; line-height: 1; vertical-align: middle; margin-left: 2px; }
    /* The viewer's OWN flag is a button, not a label — it opens the country
       editor. Stripped of every default button chrome so it reads as the same
       inline glyph the read-only .country-flag is, and only gains a background
       on hover/focus to say it is interactive. Transparent by default and
       var(--muted)/var(--text) otherwise, so it inherits the theme in both
       light and dark rather than carrying colors of its own. */
    .country-edit-btn { background: none; border: 0; padding: 0 2px; margin: 0; cursor: pointer;
      line-height: 1; display: inline-flex; align-items: center; border-radius: 4px; color: var(--muted); }
    .country-edit-btn:hover, .country-edit-btn:focus-visible { background: var(--surface); color: var(--text); }
    /* The no-country state. A muted ＋ rather than a globe or a "??" box: it
       reads as "add one", which is the action available, instead of pretending
       to be a flag we do not have. */
    .country-flag-empty { font-size: 0.8rem; line-height: 1; color: var(--muted); }

    /* ── Layout ── */
    .content { max-width: 1600px; margin: 0 auto; padding: 2.5rem clamp(1rem, 4vw, 4.5rem) 3rem; }
    .section-label {
      color: var(--amber);
      letter-spacing: .12em;
      text-transform: uppercase;
      margin: 0 0 .8rem;
      font-size: .82rem;
      font-weight: 900;
    }
    h1 { letter-spacing: 0; font-size: clamp(2rem, 4vw, 3rem); line-height: .98; margin-bottom: .8rem; }
    .subtitle { color: var(--muted); font-size: 1.02rem; line-height: 1.7; margin-bottom: 32px; }

    /* ── Table ── */
    /* Status filter chips above the My Hives table. --chip-color is set inline
       per chip so one rule serves every state colour. */
    #hive-filter-bar { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin-bottom: 14px; }
    #hive-drift-summary { margin-bottom: 12px; padding: 10px 12px; border: 1px solid var(--border); border-radius: 8px; background: rgba(248,81,73,0.04); }
    .filter-chips { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
    .filter-chip { display: inline-flex; align-items: center; gap: 6px; padding: 5px 12px; background: var(--surface); color: var(--muted); border: 1px solid var(--border); border-radius: 9999px; font-size: 0.72rem; font-weight: 600; cursor: pointer; font-family: inherit; transition: all .15s; }
    .filter-chip:hover { border-color: var(--chip-color, var(--muted)); color: var(--text); }
    .filter-chip.on { color: var(--chip-color); border-color: var(--chip-color); background: rgba(255,255,255,0.06); }
    .filter-chip-dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; flex: none; }
    .filter-chip-count { font-size: 0.65rem; opacity: 0.75; font-variant-numeric: tabular-nums; }
    .filter-chip-clear { color: var(--muted); }
    .filter-summary { font-size: 0.72rem; color: var(--muted); white-space: nowrap; }

    /* ── Attention needed (fleet alerts) ── */
    /* The panel inverts the default reading of My Hives: instead of scanning 40+
       rows for problems, the operator reads this and stops. When the fleet is
       clean it collapses to a single quiet line, so "nothing needs you" is the
       normal state of the screen rather than an absence the eye has to infer.
       --alert-color is set inline per severity so one rule serves all three. */
    #fleet-alerts-panel { margin-bottom: 16px; }
    /* ── Fleet summary tiles ── */
    /* One-line fleet inventory above the alerts panel, fed by the server's
       hives_summary (computed over the caller's FULL visible set, never the
       current filter/page). Shown only past SUMMARY_TILES_MIN_HIVES so a
       two-hive user never sees a dashboard cosplaying as a fleet console. */
    #fleet-summary-tiles { margin-bottom: 12px; }
    .fleet-tiles { display: flex; gap: 8px; flex-wrap: wrap; }
    .fleet-tile { border: 1px solid var(--border); border-radius: 10px; background: var(--surface); padding: 8px 14px; min-width: 84px; text-align: center; }
    .fleet-tile-n { font-size: 1.15rem; font-weight: 700; font-variant-numeric: tabular-nums; }
    .fleet-tile-label { font-size: 0.62rem; color: var(--muted); text-transform: uppercase; letter-spacing: 0.04em; white-space: nowrap; }
    .fleet-tile.warn { border-color: rgba(245,158,11,0.4); }
    .fleet-tile.bad { border-color: rgba(248,81,73,0.45); }
    .alert-panel { border: 1px solid var(--border); border-radius: 10px; background: var(--surface); padding: 12px 16px; }
    .alert-panel.has-critical { border-color: rgba(248,81,73,0.45); background: rgba(248,81,73,0.06); }
    .alert-panel.has-warning { border-color: rgba(245,158,11,0.4); background: rgba(245,158,11,0.05); }
    .alert-panel-clean { display: flex; align-items: center; gap: 8px; font-size: 0.78rem; color: var(--muted); }
    .alert-panel-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
    .alert-panel-title { display: flex; align-items: center; gap: 8px; font-size: 0.85rem; font-weight: 700; color: var(--text); }
    .alert-sev-pill { display: inline-flex; align-items: center; gap: 5px; padding: 2px 9px; border-radius: 9999px; font-size: 0.68rem; font-weight: 700; background: rgba(255,255,255,0.05); color: var(--alert-color); border: 1px solid var(--alert-color); font-variant-numeric: tabular-nums; }
    .alert-type-chips { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-top: 10px; }
    .alert-rows { margin-top: 10px; display: flex; flex-direction: column; gap: 6px; }
    .alert-row { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; font-size: 0.74rem; padding: 5px 8px; border-radius: 6px; background: rgba(255,255,255,0.03); }
    .alert-row.acked { opacity: 0.55; }
    /* An alert row is a real button: clicking it jumps to that hive's row in the
       table below. Styled to look clickable (pointer, hover lift, focus ring)
       because the previous inert <div> gave no hint that anything would happen.
       width:100%/text-align:left undo the <button> defaults so the row lays out
       exactly as the old div did. */
    /* The jump button and the Acknowledge button are siblings (a button cannot
       nest inside a button), so this wrapper keeps them on one visual line. */
    .alert-row-wrap { display: flex; align-items: center; gap: 6px; }
    .alert-row-wrap .alert-ack-btn { flex: 0 0 auto; }
    button.alert-row { flex: 1 1 auto; min-width: 0; width: 100%; text-align: left; font-family: inherit; color: inherit; border: 1px solid transparent; cursor: pointer; }
    button.alert-row:hover { background: rgba(255,255,255,0.07); border-color: var(--border); }
    button.alert-row:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    /* Target highlight: a brief ring on the hive row the operator was sent to,
       so it is obvious WHICH row answered the click after the scroll settles. */
    .hive-row-targeted > td { background: rgba(88,166,255,0.16) !important; }
    .hive-row-targeted > td:first-child { box-shadow: inset 3px 0 0 var(--accent); }
    .alert-row-hive { font-weight: 700; color: var(--text); }
    .alert-row-reason { color: var(--muted); }
    .alert-row-age { color: var(--muted); font-size: 0.68rem; margin-left: auto; white-space: nowrap; }
    .alert-ack-btn { background: none; border: 1px solid var(--border); color: var(--muted); border-radius: 5px; padding: 1px 8px; font-size: 0.66rem; font-family: inherit; cursor: pointer; }
    .alert-ack-btn:hover { color: var(--text); border-color: var(--muted); }
    .alert-panel-more { margin-top: 8px; font-size: 0.7rem; color: var(--muted); background: none; border: none; font-family: inherit; cursor: pointer; text-decoration: underline; padding: 0; }

    /* ── Hive search + facets ──
       The facet panel is a CLICK-TOGGLED left tray, collapsed by default so the
       dashboard is uncluttered until the operator asks for filters. Collapsed it
       is a narrow rail (--facet-rail-w) carrying a filter glyph and, whenever
       anything is narrowing the list, an active-filter count badge. Clicking the
       rail expands the tray; clicking again collapses it. The state persists in
       localStorage (LS_FACET_TRAY_OPEN).

       No hover-reveal: a pointer-only affordance is unreachable on touch and
       flickers across the gap between rail and panel. A real <button> with
       aria-expanded works identically with mouse, touch and keyboard.

       Why overlay rather than push: pushing would re-flow the table on every
       toggle, which re-triggers .table-wrap's horizontal-scroll measurement.
       Overlaying leaves the scroll container completely untouched.

       Layering: the tray sits at z-index 40, deliberately BELOW the row hover
       panels (.hive-access-pop / healthBadge's custom panel, z-index 60), so a
       row's panel always draws above the tray rather than behind it. */
    #hive-search-row { display: flex; align-items: center; gap: 10px; margin-bottom: 12px; }
    #hive-search { flex: 1; min-width: 0; padding: 8px 14px; background: var(--surface); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: 0.85rem; font-family: inherit; }
    #hive-search:focus { outline: none; border-color: var(--accent); }
    #hive-search-clear { flex: none; }
    /* Collapsed rail width and expanded tray width. The layout grid reserves
       only the rail; the tray overhangs it. */
    :root { --facet-rail-w: 34px; --facet-tray-w: 240px; }
    .hive-layout { display: grid; grid-template-columns: var(--facet-rail-w) minmax(0, 1fr); gap: 12px; align-items: start; }
    /* The tray's positioning context. position:relative only — no overflow
       clipping, so the table's own hover panels are never cut off by it. */
    .facet-shell { position: relative; min-width: 0; }
    /* Always-visible collapsed affordance. .has-active turns it accent-coloured
       and reveals the badge, so a filtered list always has a visible cause even
       with the tray shut. */
    .facet-rail-tab { display: flex; flex-direction: column; align-items: center; gap: 6px; width: var(--facet-rail-w); padding: 8px 0; background: var(--surface); border: 1px solid var(--border); border-radius: 8px; color: var(--muted); font: inherit; font-size: 0.8rem; cursor: pointer; }
    .facet-rail-tab:hover, .facet-rail-tab:focus-visible { color: var(--text); border-color: var(--muted); }
    .facet-rail-tab.has-active { border-color: var(--accent); color: var(--accent); }
    .facet-rail-word { writing-mode: vertical-rl; letter-spacing: 0.08em; font-size: 0.6rem; text-transform: uppercase; }
    .facet-active-badge { display: inline-flex; align-items: center; justify-content: center; min-width: 18px; height: 18px; padding: 0 4px; border-radius: 9999px; background: var(--accent); color: #000; font-size: 0.62rem; font-weight: 800; font-variant-numeric: tabular-nums; }
    /* Closed by default; .facet-open (driven entirely by JS from the persisted
       flag) is the only thing that reveals it. display:none while closed keeps
       every control inside it out of the tab order. */
    .facet-tray { display: none; position: absolute; top: 0; left: 0; z-index: 40; width: var(--facet-tray-w); max-height: 70vh; overflow-y: auto; padding: 10px; background: var(--bg-soft, var(--surface)); border: 1px solid var(--border); border-radius: 10px; box-shadow: 0 12px 32px rgba(0,0,0,0.5); }
    .facet-shell.facet-open .facet-tray { display: block; }
    .facet-tray-head { display: flex; align-items: center; justify-content: space-between; gap: 6px; margin-bottom: 8px; color: var(--muted); font-size: 0.66rem; font-weight: 700; text-transform: uppercase; letter-spacing: 0.06em; }
    .facet-tray-close { background: none; border: none; color: var(--muted); font: inherit; font-size: 0.9rem; line-height: 1; cursor: pointer; padding: 2px 4px; }
    .facet-tray-close:hover { color: var(--text); }
    .facet-group { margin-bottom: 14px; border: 1px solid var(--border); border-radius: 8px; background: var(--surface); overflow: hidden; }
    .facet-group-head { display: flex; align-items: center; justify-content: space-between; gap: 6px; width: 100%; padding: 8px 10px; background: none; border: none; color: var(--muted); font: inherit; font-size: 0.7rem; font-weight: 700; text-transform: uppercase; letter-spacing: 0.5px; cursor: pointer; text-align: left; }
    .facet-group-head:hover { color: var(--text); }
    .facet-values { padding: 2px 6px 8px; }
    .facet-value { display: flex; align-items: center; justify-content: space-between; gap: 6px; width: 100%; padding: 4px 6px; background: none; border: none; border-radius: 4px; color: var(--muted); font: inherit; font-size: 0.74rem; cursor: pointer; text-align: left; }
    .facet-value:hover { background: rgba(255,255,255,0.05); color: var(--text); }
    .facet-value.on { color: var(--accent); font-weight: 700; background: rgba(244,199,95,0.08); }
    .facet-value-label { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .facet-value-count { flex: none; font-size: 0.65rem; opacity: 0.75; font-variant-numeric: tabular-nums; }
    /* Section headers double as expand/collapse buttons for the admin
       assigned/unassigned groups. */
    .hive-section-head td { cursor: pointer; user-select: none; }
    .hive-section-head td:hover { color: var(--text) !important; }
    .hive-section-caret { display: inline-block; width: 12px; margin-right: 4px; }
    /* Grouping + saved-view controls, sitting above the status chips. */
    #hive-view-bar { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin-bottom: 10px; }
    .view-ctls { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
    .view-ctl { display: inline-flex; align-items: center; gap: 6px; }
    .view-ctl-label { font-size: 0.68rem; color: var(--muted); text-transform: uppercase; letter-spacing: 0.5px; font-weight: 600; }
    .view-select { background: var(--surface); color: var(--text); border: 1px solid var(--border); border-radius: 6px; padding: 4px 8px; font-size: 0.72rem; font-family: inherit; cursor: pointer; max-width: 220px; }
    .view-btn { background: var(--surface); color: var(--muted); border: 1px solid var(--border); border-radius: 6px; padding: 4px 10px; font-size: 0.7rem; font-weight: 600; font-family: inherit; cursor: pointer; transition: all .15s; }
    .view-btn:hover:not(:disabled) { color: var(--text); border-color: var(--muted); }
    .view-btn:disabled { opacity: 0.4; cursor: not-allowed; }
    /* Group header rows nest one step in from the Assigned/Unassigned headers,
       so the outer section split stays legible as the outer structure. */
    .hive-group-head td { cursor: pointer; }
    .hive-group-head:hover td { color: var(--text) !important; }
    .hive-group-caret { display: inline-block; width: 12px; font-size: 0.7rem; }
    .table-wrap { overflow: visible; margin: 0 auto; position: relative; }
    .hive-menu-cell:hover .hive-menu-dropdown { display: block !important; }
    .hive-menu-dropdown a:hover, .hive-menu-dropdown div[onclick]:hover { background: rgba(244,199,95,0.08); border-radius: 4px; }
    .table-wrap::-webkit-scrollbar { height: 10px; display: block; }
    .table-wrap::-webkit-scrollbar-track { background: var(--bg-soft); border-radius: 4px; }
    .table-wrap::-webkit-scrollbar-thumb { background: var(--line); border-radius: 4px; min-width: 40px; }
    .table-wrap::-webkit-scrollbar-thumb:hover { background: var(--muted); }
    .table-wrap.has-scroll { padding-bottom: 4px; border-bottom: 2px solid var(--line); }
    .hive-table { width: 100%; border-collapse: collapse; font-size: 0.82rem; }
    /* Work-source badge: shown next to the issue count when a hive reads work
       from a non-default source (GitHub Projects, Linear, Jira). */
    .ws-badge { display:inline-block; font-size:10px; font-weight:600; padding:1px 6px; border-radius:3px; margin-left:4px; vertical-align:middle; }
    .ws-badge--github_projects { background:#1a3a5c; color:#58a6ff; }
    .ws-badge--linear { background:#2d1b69; color:#a78bfa; }
    .ws-badge--jira { background:#0052cc; color:#fff; }
    .hive-table th { text-align: center; padding: 10px 12px; border-bottom: 2px solid var(--line); color: var(--muted); font-weight: 600; font-size: 0.75rem; white-space: nowrap; text-transform: uppercase; letter-spacing: 0.5px; }
    .hive-table td { padding: 12px; border-bottom: 1px solid #ffffff0a; vertical-align: middle; text-align: center; }
    .hive-table td:first-child { text-align: left; }
    /* Zebra striping: a subtle alternating shade on every OTHER hive row so the
       eye can track one hive's cells straight across the wide table. Keyed off a
       per-hive class (.hive-row-alt) stamped from the render index, NOT CSS
       nth-child — a hive can emit a second <tr> (its pending-requests row), and
       nth-child would stripe those out of phase; the pending row inherits its
       parent hive's shade via the same class. Uses currentColor at a very low
       alpha so it is automatically correct in BOTH light and dark themes (no
       per-theme value to maintain) and never fights the palette tokens. The
       :hover rule is declared AFTER this so hover always wins over the stripe. */
    .hive-table tr.hive-row-alt td { background: rgba(128,128,128,0.045); }
    .hive-table tr:hover td { background: rgba(244,199,95,0.04); }
    /* Admin contact/CRM editor row. The .hive-table td rule above centres every
       cell, which is right for the dense status columns but wrong for a form:
       it centred the labels and the text inside the inputs. Fixed here on the
       panel cell, which owns the form, rather than with !important per field. */
    .contact-panel-cell { text-align: left; }
    .contact-panel-cell input, .contact-panel-cell textarea { text-align: left; }
    .online-dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; }
    .online-dot.on { background: var(--green); box-shadow: 0 0 6px rgba(116,223,154,0.5); }
    .online-dot.off { background: #6b7280; }
    /* Upgrading state: replace the normal health dot with a 50% larger (12px) blue
       dot that glows and blinks slowly, so a rollout-in-progress reads at a glance
       and is unmistakable versus the ok/degraded/critical/unknown health colors. */
    .online-dot.upgrading { width: 12px; height: 12px; background: #58a6ff; box-shadow: 0 0 8px rgba(88,166,255,0.9), 0 0 3px rgba(88,166,255,0.7); animation: hiveUpgradePulse 1.8s ease-in-out infinite; }
    @keyframes hiveUpgradePulse { 0%, 100% { opacity: 1; box-shadow: 0 0 10px rgba(88,166,255,1), 0 0 4px rgba(88,166,255,0.8); } 50% { opacity: 0.4; box-shadow: 0 0 4px rgba(88,166,255,0.4); } }
    @media (prefers-reduced-motion: reduce) { .online-dot.upgrading { animation: none; } }
    /* Heartbeat heart: a small red heart next to a hive's identity that
       appears ONLY when a new heartbeat lands, pulses exactly three times,
       then disappears until the next beat. The element is invisible by
       default (opacity 0) and the finite animation's forwards fill-mode
       leaves it invisible again after the third pulse; heartbeatHeart(h)
       additionally stops emitting the markup once the flash window passes.
       The 0.6s duration and 3-iteration count here MUST match
       HEART_PULSE_MS / HEART_PULSE_COUNT in heartbeatHeart(h). */
    .heartbeat-heart { color: #e5534b; vertical-align: middle; margin-left: 4px; opacity: 0; }
    .heartbeat-heart-flash { animation: heartbeatPulse 0.6s ease-in-out 3 forwards; }
    @keyframes heartbeatPulse { 0% { transform: scale(0.85); opacity: 0; } 30% { transform: scale(1.3); opacity: 1; } 70% { transform: scale(1); opacity: 0.9; } 100% { transform: scale(0.9); opacity: 0; } }
    @media (prefers-reduced-motion: reduce) { .heartbeat-heart-flash { animation: none; } }
    /* Quadrant hover: the same kite drawn large, with numbers. Hidden until
       hover/focus rather than built on demand so there is no work on mouseover
       and no layout thrash mid-render.

       The panel is positioned RIGHT-aligned and above-anchored because the
       Quadrant column is the last in a horizontally scrolling table — a
       left-anchored panel on the final column would open off-screen. The
       parent .table-wrap keeps overflow visible, so this escapes the cell. */
    .quadrant-cell .quadrant-hover {
      display: none; position: absolute; right: 0; bottom: calc(100% + 8px);
      z-index: 60; width: 240px; padding: 10px 12px; text-align: left;
      background: var(--bg-soft); border: 1px solid var(--line); border-radius: 8px;
      box-shadow: 0 8px 24px rgba(0,0,0,0.45); cursor: default; white-space: normal;
      font-size: 0.72rem; line-height: 1.4; color: var(--text);
    }
    .quadrant-cell:hover .quadrant-hover, .quadrant-cell:focus-within .quadrant-hover { display: block; }
    .hive-name { font-weight: 600; color: var(--text); }
    .hive-org { font-size: 0.75rem; color: var(--muted); }

    /* ── Badges ── */
    .role-badge { display: inline-block; padding: 2px 10px; border-radius: 9999px; font-size: 0.7rem; font-weight: 600; }
    .role-owner { background: rgba(244,199,95,0.15); color: var(--amber); border: 1px solid rgba(244,199,95,0.3); }
    .role-read { background: rgba(128,191,255,0.15); color: var(--blue); border: 1px solid rgba(128,191,255,0.3); }
    .role-read-write { background: rgba(116,223,154,0.15); color: var(--green); border: 1px solid rgba(116,223,154,0.3); }
    .role-merger { background: rgba(163,113,247,0.15); color: #a371f7; border: 1px solid rgba(163,113,247,0.3); }
    /* Editable role pill: a <select> styled to match the role badges. Options
       render in the native menu (dark on most platforms); the closed control
       keeps the role color. */
    /* A down-caret drawn as an SVG background hints that the pill is a dropdown.
       stroke=%236b7280 is a neutral gray that reads on every role color; the
       extra right-padding keeps the role text clear of the caret. */
    .role-select { font-weight: 700; -webkit-appearance: none; appearance: none; outline: none;
      padding-right: 20px !important;
      background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='10' height='10' viewBox='0 0 24 24' fill='none' stroke='%236b7280' stroke-width='3' stroke-linecap='round' stroke-linejoin='round'%3E%3Cpolyline points='6 9 12 15 18 9'/%3E%3C/svg%3E");
      background-repeat: no-repeat; background-position: right 7px center; }
    .role-select option { color: initial; background: #fff; }
    .acmm-badge { display: inline-block; padding: 4px 12px; border-radius: 9999px; font-size: 0.7rem; font-weight: 700; white-space: nowrap; cursor: help; }
    .acmm-1 { background: rgba(128,191,255,0.15); color: #80bfff; border: 1px solid rgba(128,191,255,0.3); }
    .acmm-2 { background: rgba(116,223,154,0.15); color: #74df9a; border: 1px solid rgba(116,223,154,0.3); }
    /* L3 gets its own teal so it is distinct from L2's green (they were both #74df9a). */
    .acmm-3 { background: rgba(45,212,191,0.15); color: #2dd4bf; border: 1px solid rgba(45,212,191,0.3); }
    .acmm-4 { background: rgba(244,199,95,0.15); color: #f4c75f; border: 1px solid rgba(244,199,95,0.3); }
    .acmm-5 { background: rgba(255,126,126,0.15); color: #ff7e7e; border: 1px solid rgba(255,126,126,0.3); }
    .acmm-6 { background: rgba(180,130,255,0.15); color: #b482ff; border: 1px solid rgba(180,130,255,0.3); }

    /* ── User-journey stage badges ──
       Severity drives the color so a stalled hive is scannable at a glance:
       green = nothing outstanding, blue = a gentle nudge, amber = overdue,
       red = a de-provision warning has been sent. */
    .journey-badge { display: inline-block; padding: 3px 9px; border-radius: 9999px; font-size: 0.65rem; font-weight: 700; white-space: nowrap; cursor: help; }
    .journey-none { background: rgba(116,223,154,0.12); color: #74df9a; border: 1px solid rgba(116,223,154,0.28); }
    .journey-gentle { background: rgba(128,191,255,0.15); color: #80bfff; border: 1px solid rgba(128,191,255,0.3); }
    .journey-firm { background: rgba(244,199,95,0.15); color: #f4c75f; border: 1px solid rgba(244,199,95,0.3); }
    .journey-deprovision-warning { background: rgba(255,126,126,0.18); color: #ff7e7e; border: 1px solid rgba(255,126,126,0.4); }
    .journey-snoozed { background: rgba(107,114,128,0.15); color: #9ca3af; border: 1px solid rgba(107,114,128,0.3); }
    /* The relay badge marks a hive satisfying stage 2 through human
       contributors rather than an assigned method/model — deliberately its own
       label so such a hive is never silently reported as "assigned". */
    .journey-relay { display: inline-block; margin-left: 4px; padding: 3px 8px; border-radius: 9999px; font-size: 0.62rem; font-weight: 700; white-space: nowrap; cursor: help; background: rgba(45,212,191,0.15); color: #2dd4bf; border: 1px solid rgba(45,212,191,0.3); }

    /* ── Buttons ── */
    .btn-primary { display: inline-flex; align-items: center; justify-content: center; padding: .72rem 1.2rem; background: var(--amber); color: #17110a; font-weight: 800; border-radius: .55rem; border: none; cursor: pointer; font-size: 0.85rem; transition: all .2s; }
    .btn-primary:hover { background: #f8d87a; text-decoration: none; }
    .btn-primary:disabled { opacity: 0.5; cursor: not-allowed; }

    /* ── Toasts & dialogs ── */
    .hive-toast { position: fixed; top: 70px; right: 24px; z-index: 200; padding: 12px 20px; border-radius: 8px; font-size: 0.85rem; max-width: 400px; animation: toast-in 0.3s ease; color: #fff; }
    .hive-toast.success { background: rgba(116,223,154,0.9); }
    .hive-toast.error { background: rgba(255,126,126,0.9); }
    .hive-toast.info { background: rgba(128,191,255,0.9); }
    @keyframes spin { to { transform: rotate(360deg); } }
    @keyframes toast-in { from { transform: translateX(100%); opacity: 0; } to { transform: translateX(0); opacity: 1; } }
    .hive-confirm-overlay { position: fixed; inset: 0; background: rgba(0,0,0,0.6); z-index: 150; display: flex; align-items: center; justify-content: center; }
    .hive-confirm { box-shadow: var(--shadow); background: linear-gradient(#17212df5, #0c1219f5); border: 1px solid #ffffff1a; border-radius: .85rem; padding: 24px; max-width: 400px; width: 90%; }
    .hive-confirm p { color: var(--text); margin-bottom: 16px; font-size: 0.9rem; }
    .hive-confirm-btns { display: flex; gap: 8px; justify-content: flex-end; }
    .empty-state { text-align: center; padding: 48px; color: var(--muted); }
    .dash-link { color: var(--blue); font-size: 0.8rem; }
    /* Access hover panel on the My Hives status dot. Guarded by hover:hover so
       touch devices don't latch it open with no way to dismiss — the row's own
       Manage Access dialog is the path there. */
    @media (hover: hover) {
      .hive-access-wrap:hover .hive-access-pop,
      .hive-access-wrap:focus-within .hive-access-pop { display: block !important; }
    }
    /* The panel is offset 6px below the dot. That gap is outside both the dot
       and the panel, so travelling to the panel's "View all activity" button
       would drop :hover and close it mid-move. This pseudo-element bridges the
       gap with a transparent strip so the pointer never leaves the wrapper.
       Height matches the 6px offset in the panel's inline top. */
    .hive-access-pop::before { content: ''; position: absolute; left: 0; right: 0; top: -6px; height: 6px; }
    /* The wrapper is cursor:help; the one thing inside that is genuinely
       clickable says so. */
    .hover-view-timeline { cursor: pointer; }
    .hover-view-timeline:hover { color: #79c0ff !important; }
    .repo-link { color: var(--blue); font-size: 0.8rem; }
    .hive-name-link { color: #58a6ff; font-weight: 700; text-decoration: none; }
    .hive-name-link:hover { color: #79c0ff; text-decoration: underline; }
    .hive-sub-link { color: #6b7280; font-weight: 400; text-decoration: none; }
    .hive-sub-link:hover { color: #58a6ff; text-decoration: underline; }
    .loading { text-align: center; padding: 32px; color: var(--muted); }

    /* ── Footer ── */
    .footer { border-top: 1px solid var(--line); padding: 2rem clamp(1rem, 4vw, 4.5rem); text-align: center; font-size: .82rem; color: var(--muted); }
    .footer-links { display: flex; justify-content: center; gap: 1.5rem; margin-bottom: .8rem; }
    .footer-links a { color: var(--muted); }
    .footer-links a:hover { color: var(--text); }

    /* ── Responsive ── */
    @media (max-width: 900px) {
      .site-header { grid-template-columns: 1fr; position: static; }
      .site-header nav { flex-wrap: wrap; gap: .85rem; }
      .header-link { justify-self: start; }
      /* Narrow screens stack the tray above the table instead of squeezing the
         grid. The tray stops being an overlay and becomes an in-flow
         disclosure, driven by the same .facet-open class and the same toggle
         button, so behaviour is identical — only the placement differs. */
      .hive-layout { grid-template-columns: minmax(0, 1fr); }
      .facet-rail-tab { flex-direction: row; width: auto; justify-content: center; gap: 8px; padding: 8px 12px; }
      .facet-rail-word { writing-mode: horizontal-tb; }
      .facet-tray { position: static; width: auto; max-height: none; box-shadow: none; margin-top: 8px; }
      .facet-group { margin-bottom: 10px; }
    }
    @media (max-width: 600px) {
      .content { padding: 1.5rem 12px 32px; }
      .site-header { padding: 10px 12px; }
      .brand { font-size: 0.95rem; }
      h1 { font-size: 1.4rem; }
      .table-wrap { overflow-x: auto; -webkit-overflow-scrolling: touch; }
      .hive-table { font-size: 0.72rem; min-width: 600px; }
      .hive-table td, .hive-table th { padding: 8px 6px; }
      .hive-modal { width: 95vw; max-height: 90vh; padding: 20px; }
      .empty-state { padding: 24px; }
      .hive-confirm-btns { flex-direction: column; }
      .hive-confirm-btns button { width: 100%; }
    }
    @media (max-width: 400px) {
      .content { padding: 1rem 8px 24px; }
      .site-header nav { gap: .5rem; font-size: 0.7rem; }
      .hive-modal { padding: 14px; }
      h1 { font-size: 1.2rem; }
    }
  </style>
</head>
<body>
  <!-- Admin "View as user" banner. Hidden until loadUser() detects an active
       impersonation session, then fixed to the very top of every hub page so it
       is unmissable. The Exit button POSTs the exit endpoint and reloads. -->
  <div id="impersonation-banner" style="display:none;position:fixed;top:0;left:0;right:0;z-index:2147483647;background:#b45309;color:#fff;font-size:0.85rem;font-weight:600;padding:8px 16px;box-shadow:0 2px 8px rgba(0,0,0,0.4);text-align:center">
    <span style="margin-right:6px">&#128065;</span>
    <span id="impersonation-banner-text">Viewing as user — read-only</span>
    <button type="button" onclick="exitImpersonation()" style="margin-left:14px;padding:3px 12px;background:#fff;color:#b45309;border:none;border-radius:4px;cursor:pointer;font-size:0.75rem;font-weight:700">Exit</button>
  </div>
  <header class="site-header">
    <a href="/" class="brand">
      <span class="brand-mark">🐝</span>
      <span>Hive</span>
      <span onclick="window.open(&#39;https://github.com/kubestellar/hive&#39;,&#39;_blank&#39;)" title="Source Code" style="opacity:0.6;margin-left:2px;cursor:pointer;display:inline-flex"><svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg></span>
    </a>
    <nav>
      <a href="/">Hives</a>
      <a href="/learn">Learn</a>
      <a href="/reading">Reading</a>
      <a href="/get-started">Get Started</a>
      <a href="/dashboard" style="color:var(--amber)">My Hives</a>
      <a href="/fleet" id="nav-fleet" style="display:none" title="Fleet health — agents the governor expects on but that can't work">Fleet</a>
      <a href="/api/docs" target="_blank">API</a>
      <a href="https://kubestellar.io/docs/hive/overview/introduction" target="_blank" rel="noopener">Docs</a>
      <span id="nav-user" class="nav-user"></span>
    </nav>
    <div class="header-right">
      <span id="hub-version"></span>
      <a href="#" class="header-link" onclick="fetch('/api/auth/logout',{method:'POST'}).then(function(){location.href='/'});return false;">Logout</a>
    </div>
  </header>

  <div class="content">
    <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:24px">
      <div>
        <p class="section-label">Dashboard</p>
        <h1>My Hives</h1>
        <p class="subtitle">Hive instances you own or have access to</p>
        <p style="margin-top:8px;padding:8px 14px;border:1px solid var(--line);border-radius:8px;background:rgba(244,199,95,0.08);font-size:0.85rem">👋 New to Hive? Read the <a href="https://docs.kubestellar.io/docs/hive/getting-started" target="_blank" rel="noopener" style="color:var(--amber);font-weight:700">Getting Started Guide</a> before diving in.</p>
        <p id="latest-image-sha" style="font-size:0.7rem;color:var(--muted);margin-top:4px"></p>
        <!-- Image-pulls bar chart: per-release container-image PULLS of the
             public spoke image (ghcr.io/hivecommons/hive), bucketed by the
             ACTIVE release line's release boundaries (the line the "stable"
             channel currently resolves to — v4 today, v5 after the next
             rollover, with no code change). Gauges external adoption beyond
             the hosted fleet. Derived from GitHub's cumulative "Total
             downloads" counter — it is pulls, NOT unique downloads
             (uniqueness is not measurable). Populated by loadImagePulls(),
             which also fills the per-line mini charts in the
             "Latest available images" rows. Hidden until there is data. -->
        <div id="image-pulls-spark" style="display:none;margin-top:8px"
             title="Container-image pulls per release of the active release line: the pulls that landed while each of the last ~10 releases was the newest, of the public hive image (ghcr.io/hivecommons/hive). Derived from GitHub's cumulative download counter — pulls, not unique downloads."></div>
      </div>
      <div style="display:flex;gap:8px;align-items:center">
        <button class="btn-primary" id="btn-send-banner-top" style="display:none;background:#d97706" onclick="_bannerTargetHive=null;document.getElementById('banner-modal').style.display='flex';loadBannerHiveList()">Send Banner</button>
        <!-- Register-your-own-hive: for a user who self-installed a standalone
             hive and wants to attach it to THIS hub. Points at the self-host
             guide (get-started.html #self-host).

             NOTE: attaching a self-hosted hive is NOT self-service. Setting
             HIVE_HUB_URL alone does not register anything — handleHeartbeat
             rejects every unauthenticated beat with 401 whenever the hub has a
             HIVE_HUB_SECRET configured (deliberate, #1077: "prevents registry
             pollution from unauthenticated sources"). The operator must issue
             the spoke a secret. The guide this links to now says so; keep the
             two in step if either changes. -->
        <a class="btn-primary" id="btn-register-hive" href="/get-started#self-host" style="background:var(--surface);color:var(--text);border:1px solid var(--border);text-decoration:none" title="You self-host the hive and attach it to this hub (requires a HIVE_HUB_SECRET issued by the hub operator)">Register your own hive</a>
        <!-- Request-a-hive: routes to the EXISTING Request-a-Hive wizard at
             /get-started (files a provision request via POST
             /api/saas/request-provision). Shown when the user has NO hosted
             quota (cannot self-create); hidden otherwise. No new modal. -->
        <a class="btn-primary" id="btn-request-hive" href="/get-started" style="display:none;text-decoration:none" title="We host the hive for you — files a provision request">Request a hive</a>
        <!-- Self-create path, for users who DO have hosted quota. Hidden when the
             user has no quota (the Request-a-hive link above takes over). -->
        <button class="btn-primary" id="btn-add-hive" disabled onclick="document.getElementById('create-modal').style.display='flex'">+ Add Hosted Hive</button>
      </div>
    </div>

    <div id="provision-request-banner" style="display:none"></div>
    <div id="admin-provision-requests" style="display:none;margin-bottom:24px">
      <h3 style="font-size:1rem;color:var(--accent);margin-bottom:12px">Pending Provision Requests</h3>
      <div id="admin-provision-list"></div>
      <h3 id="past-requests-header" role="button" tabindex="0" aria-expanded="false" aria-controls="past-requests-body"
          onclick="togglePastRequests()" onkeydown="if(event.key==='Enter'||event.key===' '){event.preventDefault();togglePastRequests();}"
          style="font-size:1rem;color:var(--accent);margin:20px 0 10px;cursor:pointer;user-select:none;display:flex;align-items:center;gap:6px">
        <span id="past-requests-toggle" aria-hidden="true">&#9656;</span><span>Past Requests</span>
        <span id="past-requests-count" style="font-size:0.7rem;color:var(--muted);font-weight:400"></span>
      </h3>
      <div id="past-requests-body" style="display:none;overflow-x:auto"><div id="admin-request-history"></div></div>
    </div>

    <!-- Usage renders ABOVE the attention strip on purpose: the strip's drift
         pills filter the hive list, so the strip and the list it narrows must
         sit directly adjacent with no unrelated card between them. -->
    <div id="usage-panel" style="display:none;margin-bottom:24px"></div>
    <div id="hive-drift-summary" style="display:none"></div>
    <div id="fleet-summary-tiles" style="display:none"></div>
    <div id="fleet-alerts-panel" style="display:none"></div>
    <div id="hive-view-bar" style="display:none"></div>
    <div id="hive-filter-bar" style="display:none"></div>
    <div id="bulk-action-bar" style="display:none"></div>
    <div class="hive-layout">
      <div class="facet-shell" id="hive-facet-shell">
        <button type="button" class="facet-rail-tab" id="hive-facet-toggle"
                aria-expanded="false" aria-controls="hive-facet-tray"
                title="Show filters">
          <span aria-hidden="true">&#9776;</span>
          <span class="facet-rail-word">Filters</span>
          <span class="facet-active-badge" id="hive-facet-active-badge" style="display:none"></span>
        </button>
        <div class="facet-tray" id="hive-facet-tray">
          <div class="facet-tray-head">
            <span>Filters</span>
            <button type="button" class="facet-tray-close" id="hive-facet-close"
                    title="Hide filters" aria-label="Hide filters">&#10005;</button>
          </div>
          <div id="hive-facet-rail"></div>
        </div>
      </div>
      <div>
        <div id="hive-search-row" style="display:none">
          <input type="text" id="hive-search" placeholder="Search hives — org, repo, cluster, branch, user… (space = OR)" oninput="onHiveSearchInput()" autocomplete="off">
          <button type="button" id="hive-search-clear" class="filter-chip filter-chip-clear" onclick="clearHiveSearch()">Clear</button>
        </div>
        <div id="hives-container"><div class="loading">Loading your hives...</div></div>
      </div>
    </div>

    <div id="public-hives-section" style="display:none;margin-top:48px">
      <h2 style="font-size:1.3rem;color:var(--accent);margin-bottom:16px">Public Hives</h2>
      <div id="public-hives-container"></div>
    </div>

    <div id="admin-section" style="display:none;margin-top:48px">
      <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:16px">
        <h2 id="admin-users-header" role="button" tabindex="0" aria-expanded="false" aria-controls="admin-users-body"
            onclick="toggleAdminUsers()" onkeydown="if(event.key==='Enter'||event.key===' '){event.preventDefault();toggleAdminUsers();}"
            style="font-size:1.3rem;color:var(--accent);margin:0;cursor:pointer;user-select:none;display:flex;align-items:center;gap:8px">
          <span id="admin-users-toggle" aria-hidden="true" style="font-size:0.7rem">&#9656;</span><span>Hub Admin &mdash; Users</span>
          <span id="admin-users-count" style="font-size:0.75rem;color:var(--muted);font-weight:400"></span>
        </h2>
        <input type="text" id="user-search" placeholder="Search users..." oninput="filterUsers()" style="padding:8px 14px;background:var(--surface);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;width:250px">
      </div>
      <div id="admin-users-body" style="display:none">
        <div id="users-container"><div class="loading">Loading users...</div></div>
        <!-- Fleet-wide geographic rollup of the user base. Lives directly under
             the Users table because it is the same data aggregated: the table
             answers "who", this answers "where, overall". Inside
             admin-users-body so it collapses with the section, and admin-gated
             twice over — the section is display:none until the admin check
             passes, and the endpoint it reads is behind requireAdmin.
             Rendered as a plain ranked bar list; no map, no charting library,
             no external asset (the hub forbids external CDNs/images). -->
        <div id="user-countries-container" style="margin-top:20px"></div>
      </div>
    </div>

    <div id="hub-banner-section" style="display:none;margin-top:48px">
      <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:16px">
        <h2 style="font-size:1.3rem;color:var(--accent)">Hub Banner</h2>
        <button class="btn-primary" id="btn-clear-banner" style="background:rgba(239,68,68,0.15);color:#f87171;display:none" onclick="clearHubBanner()">Clear All Banners</button>
      </div>
      <div id="active-banner-display" style="display:none;padding:16px;border-radius:8px;border:1px solid var(--border);background:var(--surface);margin-bottom:16px">
        <div style="font-size:0.8rem;color:var(--muted);margin-bottom:8px">Active Banner</div>
        <div id="active-banner-preview" style="padding:12px 16px;border-radius:6px;font-size:0.85rem;margin-bottom:8px"></div>
        <div id="active-banner-targets" style="font-size:0.75rem;color:var(--muted)"></div>
      </div>
    </div>

    <div id="cluster-health-section" style="display:none;margin-top:48px">
      <div onclick="toggleClusterHealth()" style="display:flex;align-items:center;gap:8px;cursor:pointer;user-select:none;margin-bottom:16px">
        <span id="cluster-health-toggle" style="font-size:0.7rem;color:var(--muted);transition:transform 0.2s">&#9654;</span>
        <h2 style="font-size:1.3rem;color:var(--accent);margin:0">Cluster Health</h2>
        <span id="cluster-health-summary-bar" style="font-size:0.8rem;color:var(--muted);margin-left:8px"></span>
      </div>
      <div id="cluster-health-body" style="display:none">
        <div id="cluster-health-grid" style="display:grid;grid-template-columns:repeat(2,1fr);gap:12px"></div>
      </div>
    </div>

    <!-- Scale Controls: every fleet-scale tunable (upgrade wave size,
         provisioning queue bounds, kubectl concurrency, per-cluster
         capacity/pool watermarks) is edited HERE, not in env vars or config
         files. Values persist server-side (scale_settings.json) and override
         the env/clusters.json defaults, which remain only as initial values.
         Admin-gated twice: hidden until the admin check passes, and the
         endpoints are behind requireAdmin. -->
    <div id="scale-controls-section" style="display:none;margin-top:48px">
      <div onclick="toggleScaleControls()" style="display:flex;align-items:center;gap:8px;cursor:pointer;user-select:none;margin-bottom:16px">
        <span id="scale-controls-toggle" style="font-size:0.7rem;color:var(--muted);transition:transform 0.2s">&#9654;</span>
        <h2 style="font-size:1.3rem;color:var(--accent);margin:0">Scale Controls</h2>
        <span id="scale-controls-summary" style="font-size:0.8rem;color:var(--muted);margin-left:8px"></span>
      </div>
      <div id="scale-controls-body" style="display:none">
        <div style="font-size:0.8rem;color:var(--muted);margin-bottom:12px">
          Fleet throughput knobs. Blank/0 = use the default shown. Changes apply live
          (worker-count reductions apply on the next hub restart).
        </div>
        <div id="scale-globals" style="display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px;margin-bottom:16px"></div>
        <div style="font-size:0.85rem;color:var(--text);margin:12px 0 8px;font-weight:600">Per-cluster limits</div>
        <div class="table-wrap"><table style="width:100%;font-size:0.85rem"><thead><tr>
          <th style="text-align:left">Cluster</th><th style="text-align:right">Hives</th><th style="text-align:right">Pool avail</th>
          <th style="text-align:right">Max hives</th><th style="text-align:right">Pool min</th><th style="text-align:right">Pool target</th>
        </tr></thead><tbody id="scale-clusters-body"></tbody></table></div>
        <div style="display:flex;align-items:center;gap:12px;margin-top:14px">
          <button class="btn-primary" onclick="saveScaleSettings()">Save Scale Settings</button>
          <span id="scale-save-status" style="font-size:0.8rem;color:var(--muted)"></span>
        </div>
      </div>
    </div>

    <div id="reach-section" style="display:none;margin-top:48px">
      <div onclick="toggleReach()" style="display:flex;align-items:center;gap:8px;cursor:pointer;user-select:none;margin-bottom:16px">
        <span id="reach-toggle" style="font-size:0.7rem;color:var(--muted);transition:transform 0.2s">&#9654;</span>
        <h2 style="font-size:1.3rem;color:var(--accent);margin:0">PR Reach</h2>
        <span id="reach-summary-bar" style="font-size:0.8rem;color:var(--muted);margin-left:8px"></span>
      </div>
      <div id="reach-body" style="display:none">
        <div class="table-wrap" id="reach-table-container"></div>
      </div>
    </div>
  </div>

  <footer class="footer">
    <div class="footer-links">
      <a href="https://github.com/kubestellar/hive">Source Code</a>
      <a href="https://arxiv.org/abs/2604.09388">ACMM Paper</a>
      <a href="https://kubestellar.io">KubeStellar</a>
    </div>
    <p style="color:#3a4555">Hive is an open source project by KubeStellar</p>
  </footer>

  <script>
    function esc(s) { var d = document.createElement('div'); d.textContent = s || ''; return d.innerHTML; }

    /* jsArg renders a string as a quoted JS string literal safe to embed in an
       inline handler attribute. esc() alone is not enough here: it leaves the
       apostrophe intact, and an org or branch name containing one would close
       the handler's quote early. Backslash first (so later escapes are not
       re-escaped), then the quote, then newlines, and finally HTML-escape the
       whole literal because it lands inside an attribute value. */
    function jsArg(s) {
      var lit = "'" + String(s === null || s === undefined ? '' : s)
        .replace(/\\/g, '\\\\')
        .replace(/'/g, "\\'")
        .replace(/\r/g, '\\r')
        .replace(/\n/g, '\\n') + "'";
      return esc(lit);
    }

    // escAttr escapes for a QUOTED ATTRIBUTE value. esc() goes through
    // textContent -> innerHTML, which per spec escapes & < > but NOT quotes, so
    // esc() alone is unsafe between double quotes: a hive name containing a '"'
    // closes the attribute and everything after it is parsed as markup, which
    // is enough to inject an event handler. Hive names and org/repo strings are
    // spoke-reported, i.e. untrusted. Use escAttr for anything interpolated
    // into an attribute; esc() remains correct for text nodes.
    function escAttr(s) { return esc(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }
    /* wsBadge renders a small work-source badge ("Linear", "Jira", "GH Projects")
       next to a hive's issue count when the spoke reads work from a non-default
       source. Empty/default ("", "github") renders nothing. */
    function wsBadge(ws) {
      if (!ws || ws === 'github') return '';
      var labels = { github_projects: 'GH Projects', linear: 'Linear', jira: 'Jira' };
      var cls = /^[a-z_]+$/.test(ws) ? ws : 'unknown';
      return '<span class="ws-badge ws-badge--' + cls + '" title="Work source: ' + escAttr(ws) + '">' + esc(labels[ws] || ws) + '</span>';
    }

    /* ---- Quadrant kite -------------------------------------------------
       Trust / Efficiency / Satisfaction / Productivity, drawn as a four-axis
       polygon. ONE renderer at two sizes: a ~22px shape in the table column and
       the same shape at ~170px with labels and numbers in its hover. They must
       never fork — the small kite is legible only because it is literally the
       large one shrunk, so a shape learned in a hover is recognisable in the
       column.

       Axis positions are FIXED: Trust north, Productivity east, Satisfaction
       south, Efficiency west. Reordering per row would make every previously
       learned shape mean something else. QUADRANT_AXES mirrors Go's
       QuadrantAxisOrder and the two must not drift. */
    var QUADRANT_AXES = ['trust', 'productivity', 'satisfaction', 'efficiency'];
    var QUADRANT_AXIS_LABELS = { trust: 'Trust', productivity: 'Prod', satisfaction: 'Satis', efficiency: 'Effic' };
    /* Angles clockwise from north, index-aligned with QUADRANT_AXES. */
    var QUADRANT_ANGLES = [0, 90, 180, 270];

    /* quadrantAxis pulls one axis off a quadrant, returning an unscored stub
       when absent so a partial payload can never throw mid-render. */
    function quadrantAxis(q, name) {
      var axes = (q && q.axes) || [];
      for (var i = 0; i < axes.length; i++) {
        if (axes[i] && axes[i].axis === name) return axes[i];
      }
      return { axis: name, score: 0, scored: false };
    }

    /* quadrantPoint maps an axis index and score to a coordinate.

       An UNSCORED axis returns the exact centre, so the polygon visibly caves
       in on that side. This is the visual half of the rule that absent evidence
       is not a zero: a collapsed spoke reads as "not measured", where a small
       symmetric shape would read as "mediocre everywhere". */
    function quadrantPoint(idx, score, scored, cx, cy, radius) {
      if (!scored) return [cx, cy];
      var frac = Math.max(0, Math.min(1, (score || 0) / 100));
      var rad = QUADRANT_ANGLES[idx % 4] * Math.PI / 180;
      /* -cos on y because SVG grows downward and north must point up. */
      return [cx + radius * frac * Math.sin(rad), cy - radius * frac * Math.cos(rad)];
    }

    function quadrantPolygonPoints(q, cx, cy, radius) {
      var pts = [];
      for (var i = 0; i < QUADRANT_AXES.length; i++) {
        var a = quadrantAxis(q, QUADRANT_AXES[i]);
        var p = quadrantPoint(i, a.score, a.scored, cx, cy, radius);
        pts.push(p[0].toFixed(2) + ',' + p[1].toFixed(2));
      }
      return pts.join(' ');
    }

    /* quadrantSVG draws the kite. fleet is the reference polygon behind it, so
       a row reads as a deviation from normal rather than an absolute the viewer
       must calibrate; it is omitted when the fleet scored nothing, since an
       empty reference would collapse to a dot and read as data. */
    function quadrantSVG(q, fleet, size, labelled) {
      var pad = labelled ? size * 0.26 : 2;
      var cx = size / 2, cy = size / 2, radius = size / 2 - pad;
      var s = '<svg viewBox="0 0 ' + size + ' ' + size + '" width="' + size + '" height="' + size +
        '" role="img" aria-label="' + escAttr(quadrantAriaLabel(q)) + '" style="display:block;overflow:visible">';
      /* Faint concentric rings orient the eye without competing with the data. */
      [0.33, 0.66, 1].forEach(function(ring) {
        var pts = [];
        for (var i = 0; i < 4; i++) {
          var p = quadrantPoint(i, 100, true, cx, cy, radius * ring);
          pts.push(p[0].toFixed(2) + ',' + p[1].toFixed(2));
        }
        s += '<polygon points="' + pts.join(' ') + '" fill="none" stroke="var(--line)" stroke-width="0.5" opacity="0.35"/>';
      });
      if (labelled) {
        for (var i = 0; i < 4; i++) {
          var p = quadrantPoint(i, 100, true, cx, cy, radius);
          s += '<line x1="' + cx + '" y1="' + cy + '" x2="' + p[0].toFixed(2) + '" y2="' + p[1].toFixed(2) +
            '" stroke="var(--line)" stroke-width="0.5" opacity="0.35"/>';
        }
      }
      if (fleet && fleet.scored_axes > 0) {
        s += '<polygon points="' + quadrantPolygonPoints(fleet, cx, cy, radius) +
          '" fill="var(--muted)" fill-opacity="0.10" stroke="var(--muted)" stroke-width="0.75" stroke-dasharray="2,2" opacity="0.6"/>';
      }
      s += '<polygon points="' + quadrantPolygonPoints(q, cx, cy, radius) +
        '" fill="var(--accent)" fill-opacity="0.22" stroke="var(--accent)" stroke-width="1.5" stroke-linejoin="round"/>';
      for (var i = 0; i < 4; i++) {
        var a = quadrantAxis(q, QUADRANT_AXES[i]);
        if (!a.scored) continue;
        var p = quadrantPoint(i, a.score, true, cx, cy, radius);
        s += '<circle cx="' + p[0].toFixed(2) + '" cy="' + p[1].toFixed(2) + '" r="' + (labelled ? 2.5 : 1.5) + '" fill="var(--accent)"/>';
      }
      if (labelled) {
        for (var i = 0; i < 4; i++) {
          var a = quadrantAxis(q, QUADRANT_AXES[i]);
          var lp = quadrantPoint(i, 122, true, cx, cy, radius);
          var anchor = QUADRANT_ANGLES[i] === 90 ? 'start' : (QUADRANT_ANGLES[i] === 270 ? 'end' : 'middle');
          /* An unscored axis prints a dash, never a 0 — the whole point of
             tracking scored separately from score. */
          var val = '—';
          if (a.scored) {
            val = String(a.score);
            if (a.delta) val += ' ' + (a.delta < 0 ? '−' : '+') + Math.abs(a.delta);
          }
          s += '<text x="' + lp[0].toFixed(2) + '" y="' + lp[1].toFixed(2) + '" text-anchor="' + anchor +
            '" font-size="8" fill="var(--muted)" style="text-transform:uppercase;letter-spacing:0.5px">' +
            esc(QUADRANT_AXIS_LABELS[QUADRANT_AXES[i]]) + '</text>';
          s += '<text x="' + lp[0].toFixed(2) + '" y="' + (lp[1] + 9).toFixed(2) + '" text-anchor="' + anchor +
            '" font-size="7.5" fill="var(--text)" opacity="0.85">' + esc(val) + '</text>';
        }
      }
      return s + '</svg>';
    }

    /* A shape-only kite is invisible to a screen reader without this. */
    function quadrantAriaLabel(q) {
      if (!q || !q.scored_axes) return 'Quadrant: not enough data';
      return 'Quadrant: ' + QUADRANT_AXES.map(function(name) {
        var a = quadrantAxis(q, name);
        return QUADRANT_AXIS_LABELS[name] + (a.scored ? ' ' + a.score : ' not measured');
      }).join(', ');
    }

    /* quadrantCell is the table column: the small kite plus its own hover
       panel, so a lopsided shape can be diagnosed without leaving the row.
       Renders nothing at all for a hive with no quadrant — the server omits it
       for callers who may not see it and for hives with nothing scored, and an
       empty chart would imply a hive scoring zero everywhere. */
    function quadrantCell(h) {
      var q = h && h.quadrant;
      if (!q) return '';
      return '<span class="quadrant-cell" style="position:relative;display:inline-block;cursor:help">' +
        quadrantSVG(q, _fleetQuadrant, 22, false) +
        '<span class="quadrant-hover">' + quadrantPanelHTML(q) + '</span>' +
        '</span>';
    }

    /* quadrantPanelHTML is the shared hover body — the same chart drawn large,
       with the composite and whichever nudges apply. Used by BOTH the column
       hover and the status hover so the two can never drift. */
    function quadrantPanelHTML(q) {
      if (!q) return '';
      var nudges = QUADRANT_AXES.map(function(name) { return quadrantAxis(q, name); })
        .filter(function(a) { return a.nudge; })
        .map(function(a) {
          return '<div style="display:flex;gap:6px;align-items:flex-start;margin-top:4px">' +
            '<span style="color:var(--accent);flex:0 0 auto">→</span><span>' + esc(a.nudge) + '</span></div>';
        }).join('');
      /* Reasons explain a collapsed spoke, so a gap reads as "not measured yet"
         rather than as something the viewer has to interpret. */
      var reasons = QUADRANT_AXES.map(function(name) { return quadrantAxis(q, name); })
        .filter(function(a) { return !a.scored && a.reason; })
        .map(function(a) {
          return '<div style="color:var(--muted);margin-top:2px">' +
            esc(QUADRANT_AXIS_LABELS[a.axis]) + ': ' + esc(a.reason) + '</div>';
        }).join('');
      return '<div style="display:flex;flex-direction:column;align-items:center;gap:6px">' +
          quadrantSVG(q, _fleetQuadrant, 170, true) +
          '<div style="font-size:0.7rem;color:var(--muted)">Composite <span style="color:var(--text);font-weight:600">' +
            (q.scored_axes ? q.composite : '—') + '</span> · ' + (q.scored_axes || 0) + ' of 4 axes scored</div>' +
        '</div>' +
        (nudges ? '<div style="font-size:0.7rem;margin-top:6px;border-top:1px solid var(--line);padding-top:6px">' + nudges + '</div>' : '') +
        (reasons ? '<div style="font-size:0.65rem;margin-top:4px">' + reasons + '</div>' : '');
    }

    /* fleetQuadrantHeaderHTML is the aggregate above the table: the same kite,
       averaged over the CURRENT filtered view.

       This is the surface that turns the instrument from per-hive feedback into
       a platform signal. One hive with a collapsed efficiency spoke is that
       hive's problem; thirty of them collapsed the same way is not thirty
       nudges, it is one platform problem, and only the aggregate shows that.

       It re-renders with the table, so narrowing the filter re-aggregates over
       whatever is now in view — which is why the rows and this shape always
       agree with each other.

       Renders nothing when nothing scored, rather than an empty chart: at that
       point there is no finding to show, and a collapsed aggregate would read
       as a fleet failing on every axis. */
    function fleetQuadrantHeaderHTML() {
      var q = _fleetQuadrant;
      if (!q || !q.scored_axes) return '';
      var axes = QUADRANT_AXES.map(function(name) {
        var a = quadrantAxis(q, name);
        var val = a.scored ? String(a.score) : '—';
        return '<div style="display:flex;flex-direction:column;align-items:center;gap:1px;min-width:52px">' +
          '<span style="font-size:0.62rem;text-transform:uppercase;letter-spacing:0.5px;color:var(--muted)">' +
            esc(QUADRANT_AXIS_LABELS[name]) + '</span>' +
          '<span style="font-size:0.95rem;font-weight:600;color:' + (a.scored ? 'var(--text)' : 'var(--muted)') + '">' +
            esc(val) + '</span></div>';
      }).join('');
      /* No fleet ghost behind the aggregate — it IS the fleet, and drawing it
         against itself would render two identical overlaid polygons. */
      return '<div style="display:flex;align-items:center;gap:16px;margin:0 auto 12px;padding:10px 14px;' +
          'background:var(--bg-soft);border:1px solid var(--line);border-radius:8px;width:fit-content">' +
          quadrantSVG(q, null, 78, false) +
          '<div>' +
            '<div style="font-size:0.72rem;color:var(--muted);margin-bottom:4px">' +
              'Fleet quadrant · ' + (_allDashHives || []).length + ' hives in view</div>' +
            '<div style="display:flex;gap:14px">' + axes + '</div>' +
          '</div>' +
        '</div>';
    }

    /* ---- Clickable user avatars ---------------------------------------
       Every face in this dashboard is a link to that person's GitHub profile.
       The avatar IS the affordance: a separate ↗ glyph or a separately-linked
       username next to it was two controls for one destination, so those are
       gone and the picture carries the click.

       ALWAYS github.com, never a github_host. The account that signs in to the
       hub is a github.com account even when the ORG it works on lives on a
       GitHub Enterprise host — the same reasoning that kept the Past Requests
       user link on github.com while its REPO link is built from the request's
       github_host (see ghRepoURL). A profile link is about the person; the
       repo link is about the instance.

       The username lands in two hostile contexts and gets a different helper in
       each: encodeURIComponent for the URL path segment, escAttr for quoted
       attribute values (esc() leaves quotes intact and a crafted login could
       close the attribute). */
    function ghProfileURL(username) {
      return 'https://github.com/' + encodeURIComponent(String(username || ''));
    }

    /* Wrap avatar markup in a profile anchor.

       display:inline-block with line-height:0 keeps the anchor from adding a
       text baseline's worth of height to whatever row or cell holds it: a bare
       inline anchor inherits the line-height of its parent and can push a dense
       table row taller than its neighbours. The avatars themselves are 16-28px
       inline images and keep their own vertical-align.

       Returns the avatar unwrapped when there is no username to link to (a
       placeholder or a malformed record) — an anchor to a profile that does not
       exist is worse than no anchor.

       label is the accessible name; it also becomes the native tooltip, so any
       role information the caller already showed there is preserved. */
    function avatarProfileLink(username, label, avatarHTML) {
      var uname = String(username || '');
      if (!uname) return avatarHTML;
      // Fold "logged into their hive now" into the anchor's OWN tooltip for every
      // avatar surface, so a live user's face reads identity (+ stats) AND the
      // live state in one tooltip. The green ring wrapper deliberately carries no
      // title of its own — a wrapper title would sit on top of this and hide it.
      var title = label || uname;
      if (isUserLive(uname)) title = title + '\n● Logged into their hive now';
      return '<a href="' + escAttr(ghProfileURL(uname)) + '" target="_blank" rel="noopener noreferrer" ' +
        'title="' + escAttr(title) + '" aria-label="' + escAttr(label || uname) + '" ' +
        'style="display:inline-block;line-height:0;text-decoration:none">' + avatarHTML + '</a>';
    }

    /* Round avatar <img> for a github.com login, at the given rendered size in
       CSS pixels. Requests 2x from GitHub so the face stays sharp on HiDPI.
       github.com/<login>.png is a redirect to GitHub's CDN and returns 403 when
       the request is unauthenticated or rate-limited — common for a hub visited
       by a browser without an active GitHub session. onerror used to just hide
       the broken image (visibility:hidden), which left the face's OWN size/shape
       box empty; inside a live-ring wrapper that reads as an empty dashed ring
       with nobody home. onerror now swaps to a same-size, same-shape initials
       avatar instead, so a failed load never leaves a hole. this.onerror=null
       before the swap stops a data: URI (which cannot itself 403) from ever
       re-triggering onerror and looping. extraStyle appends to the inline style
       (borders, flex sizing) and defaults to nothing. */
    var AVATAR_HIDPI_SCALE = 2;
    /* avatarInitialsSVG builds a self-contained data: URI — no network request,
       so it always renders and is exempt from the CSP's img-src allowance for
       https: (the CSP already allows img-src data:, so no policy change is
       needed). The background color is deterministic from the username (a
       simple string hash into a fixed hue set) so the same person gets the same
       color every time rather than a random one on every failed load. */
    var AVATAR_FALLBACK_HUES = [0, 25, 50, 145, 175, 200, 230, 260, 290, 330];
    function avatarInitials(username) {
      var clean = String(username || '').replace(/[^A-Za-z0-9]/g, '');
      if (!clean) return '?';
      return clean.length === 1 ? clean.charAt(0).toUpperCase() : (clean.charAt(0) + clean.charAt(clean.length - 1)).toUpperCase();
    }
    function avatarFallbackHue(username) {
      var s = String(username || '');
      var hash = 0;
      for (var i = 0; i < s.length; i++) hash = (hash * 31 + s.charCodeAt(i)) >>> 0;
      return AVATAR_FALLBACK_HUES[hash % AVATAR_FALLBACK_HUES.length];
    }
    function avatarInitialsSVG(username, px) {
      var initials = avatarInitials(username);
      var hue = avatarFallbackHue(username);
      var r = px / 2;
      var fontSize = Math.round(px * 0.42);
      var svg = '<svg xmlns="http://www.w3.org/2000/svg" width="' + px + '" height="' + px + '" viewBox="0 0 ' + px + ' ' + px + '">' +
        '<circle cx="' + r + '" cy="' + r + '" r="' + r + '" fill="hsl(' + hue + ',45%,32%)"/>' +
        '<text x="50%" y="50%" dy="0.35em" text-anchor="middle" font-family="system-ui,sans-serif" ' +
        'font-size="' + fontSize + '" font-weight="600" fill="#fff">' + initials + '</text></svg>';
      return 'data:image/svg+xml;charset=UTF-8,' + encodeURIComponent(svg);
    }
    /* A user LOGGED INTO THEIR HIVE right now gets a thick green dashed ring, so
       "who is active this moment" reads at a glance wherever a face appears. The
       border is drawn on a wrapper (a border on the round <img> itself would clip
       against border-radius and fight any per-call-site border in extraStyle). The
       live set is admin-only, so non-admin views never ring anyone. */
    function isUserLive(username) {
      try { return _liveHiveUsers && _liveHiveUsers.has(String(username || '').toLowerCase()); }
      catch (e) { return false; }
    }
    function avatarImg(username, px, extraStyle) {
      var img = '<img src="' + escAttr(ghProfileURL(username)) + '.png?size=' + (px * AVATAR_HIDPI_SCALE) + '" alt="" ' +
        'style="width:' + px + 'px;height:' + px + 'px;border-radius:50%;vertical-align:middle;' +
        (extraStyle || '') + '" ' +
        'onerror="this.onerror=null;this.src=' + jsArg(avatarInitialsSVG(username, px)) + '">';
      if (isUserLive(username)) {
        // Concentric green dashed ring. The wrapper is a fixed square exactly the
        // face's size plus the ring gap+width on every side (px + 2*(gap+border)),
        // with box-sizing:border-box and the ring drawn as its border, so the
        // round border-radius stays perfectly centered on the round face — the
        // earlier padding:1px inline-flex version drifted off-center. No title on
        // the wrapper: the "logged in now" line lives in the face's OWN tooltip
        // (accessAvatarTitle) so a wrapper title can't shadow the identity+stats.
        var RING_GAP = 2, RING_W = 3, box = px + 2 * (RING_GAP + RING_W);
        return '<span style="display:inline-block;box-sizing:border-box;width:' + box + 'px;height:' + box + 'px;' +
          'padding:' + RING_GAP + 'px;border:' + RING_W + 'px dashed var(--green);border-radius:50%;' +
          'vertical-align:middle;line-height:0">' + img + '</span>';
      }
      return img;
    }

    /* The common case: a linked, round avatar for a github.com login. */
    function linkedAvatar(username, px, label, extraStyle) {
      return avatarProfileLink(username, label, avatarImg(username, px, extraStyle));
    }

    /* userDisplayLabel: the human-facing name for a stored user record. OIDC
       users are KEYED as provider:sub (stable, but cryptic — "google:1178…");
       what a human should see is the provider-asserted display_name, else the
       admin-entered full_name, else the email, else (GitHub users, legacy
       records) the username key itself. */
    function userDisplayLabel(u) {
      if (!u) return '';
      return u.display_name || u.full_name || u.email || u.github_username || '';
    }

    /* userAvatar: avatar for a stored user record. Prefers the provider-stored
       avatar_url (Google/Microsoft picture claim); GitHub users keep the derived
       github.com/<login>.png via avatarImg. An OIDC user with no stored avatar
       (IBMid sends no picture claim) gets initials derived from their REAL
       display label — never from the provider:sub key (which produced tiles
       like "M0"). Failed loads fall back to the same initials. */
    function userAvatar(u, px, extraStyle) {
      var label = userDisplayLabel(u);
      var style = 'width:' + px + 'px;height:' + px + 'px;border-radius:50%;vertical-align:middle;' + (extraStyle || '');
      if (u && u.avatar_url) {
        return '<img src="' + escAttr(u.avatar_url) + '" alt="" style="' + style + '" ' +
          'onerror="this.onerror=null;this.src=' + jsArg(avatarInitialsSVG(label, px)) + '">';
      }
      return '<img src="' + escAttr(avatarInitialsSVG(label, px)) + '" alt="" style="' + style + '">';
    }

    /* ---- Country flag ---------------------------------------------------
       A user's OPTIONAL country renders as a small flag beside their avatar.

       The glyph is DERIVED from the stored ISO 3166-1 alpha-2 code, never
       fetched: a flag emoji is just the two regional-indicator code points for
       the code's letters (REGIONAL INDICATOR SYMBOL LETTER A is U+1F1E6), so
       the hub needs no image assets and no external image host.

       Mirrors normalizeCountryCode / countryFlagEmoji in user_country.go. The
       server already normalizes before storing, so this is defence in depth for
       a legacy record or a hand-edited file — and it is what guarantees the
       render sites can never emit half a code point.

       Unknown or unset country renders NOTHING. No globe placeholder, no "??"
       box: absence of evidence must look like absence, not like a broken flag. */
    var REGIONAL_INDICATOR_BASE = 0x1F1E6;  /* U+1F1E6 REGIONAL INDICATOR SYMBOL LETTER A */
    var COUNTRY_CODE_LEN = 2;               /* ISO 3166-1 alpha-2 */

    function normalizeCountryCode(code) {
      var c = String(code == null ? '' : code).trim().toUpperCase();
      if (c.length !== COUNTRY_CODE_LEN) return '';
      /* Shape check only — two ASCII letters. The hub does not adjudicate which
         territories exist; it just refuses anything that would break a render. */
      if (!/^[A-Z]{2}$/.test(c)) return '';
      return c;
    }

    function countryFlagEmoji(code) {
      var c = normalizeCountryCode(code);
      if (!c) return '';
      return String.fromCodePoint(
        REGIONAL_INDICATOR_BASE + (c.charCodeAt(0) - 65),
        REGIONAL_INDICATOR_BASE + (c.charCodeAt(1) - 65));
    }

    /* countryFlagHTML: the flag as an inline <span>, or '' when there is no
       country. Every caller appends the result unconditionally, so returning ''
       is what makes an unknown country render as silence.

       The code is escaped into the title/aria-label even though it is already
       normalized to [A-Z]{2} — the render path must not depend on the validator
       upstream of it staying correct. Colors come from theme tokens; the glyph
       is the emoji's own, so only the sizing is ours. */
    function countryFlagHTML(code) {
      var c = normalizeCountryCode(code);
      if (!c) return '';
      var glyph = countryFlagEmoji(c);
      return '<span class="country-flag" title="' + escAttr(c) + '" ' +
        'role="img" aria-label="' + escAttr(c) + '">' + glyph + '</span>';
    }

    /* ---- Self-service country editor ------------------------------------
       The nav flag is the affordance: clicking it opens a small overlay where
       the signed-in user sets or clears their OWN country. It is the only such
       surface — the get-started wizard is a one-time gate you pass before you
       have a hive, so an existing user otherwise has no way to correct or
       remove a country (including one the hub merely GUESSED from their
       browser's language). See handleMyCountry in user_country.go.

       Deliberately not a profile page. One field, one overlay, reusing the
       overlay pattern the assign/timeline/prompt dialogs already use. */

    /* countryDisplayName: the English name for a code, via Intl.DisplayNames —
       a browser built-in, so the dashboard carries no 250-row country table of
       its own and cannot drift from one. Falls back to the bare code where the
       API is missing or the code is unassigned; a code is always better than an
       empty label. */
    function countryDisplayName(code) {
      var c = normalizeCountryCode(code);
      if (!c) return '';
      try {
        var dn = new Intl.DisplayNames(['en'], {type: 'region'});
        return dn.of(c) || c;
      } catch (e) { return c; }
    }

    /* The viewer's own country, mirrored from the auth payload so the editor
       opens showing what is on file rather than blank. Updated in place after a
       successful save so the nav and the next open agree without a reload. */
    var _myCountry = '';

    /* countryNavHTML: the nav's country control for the SIGNED-IN viewer.

       Distinct from countryFlagHTML, which is the read-only glyph used wherever
       someone ELSE's flag is shown. Here the flag is a button, and — the part
       that matters for the fleet this endpoint exists to serve — when there is
       NO country it still renders, as a muted outline, because a user with no
       flag is exactly the user who needs a way to add one. An invisible control
       would leave them in the same dead end as before.

       var(--muted) for the empty state and no color of our own for the set
       state (the emoji carries its own), so both are light- and dark-safe. */
    function countryNavHTML(code) {
      var c = normalizeCountryCode(code);
      var inner, title;
      if (c) {
        inner = '<span class="country-flag">' + countryFlagEmoji(c) + '</span>';
        title = countryDisplayName(c) + ' — click to change';
      } else {
        inner = '<span class="country-flag-empty">＋</span>';
        title = 'Set your country';
      }
      return '<button type="button" class="country-edit-btn" onclick="openCountryEditor()" ' +
        'title="' + escAttr(title) + '" aria-label="' + escAttr(title) + '">' + inner + '</button>';
    }

    /* openCountryEditor: the overlay. Reads the current value from _myCountry,
       writes through PUT /api/saas/me/country.

       The code rides the JSON BODY, never a path or query string: country is
       personal data and a URL is the one place it would land in access logs,
       Referer headers and browser history. Clearing sends an explicit empty
       string — which the hub records as a DECISION, so the login-path
       Accept-Language inference will not quietly put a flag back. */
    function openCountryEditor() {
      var overlay = document.createElement('div');
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:3000;display:flex;align-items:center;justify-content:center';
      var btn = 'padding:7px 14px;border-radius:6px;border:1px solid var(--border);cursor:pointer;font-size:0.8rem';
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--text);border:1px solid var(--border);border-radius:6px;box-sizing:border-box;font-size:0.85rem;text-transform:uppercase';
      overlay.innerHTML =
        '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:22px;max-width:400px;width:90%">' +
        '<h3 style="margin:0 0 10px 0;font-size:1rem">Your country</h3>' +
        '<p style="margin:0 0 12px 0;color:var(--muted);font-size:0.82rem;line-height:1.5">' +
        'Shows a small flag beside your avatar. Two-letter country code (ISO 3166-1 alpha-2), ' +
        'for example GB or JP. Leave it blank for no flag &mdash; we will not guess one for you.</p>' +
        '<input id="_country-input" type="text" maxlength="2" autocomplete="country" ' +
        'value="' + escAttr(_myCountry) + '" style="' + fld + '">' +
        '<div id="_country-preview" style="margin-top:8px;font-size:0.82rem;color:var(--muted);min-height:1.2em"></div>' +
        '<div id="_country-err" style="display:none;margin-top:8px;font-size:0.8rem;color:#f85149"></div>' +
        '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:18px">' +
        '<button data-act="no" style="' + btn + ';background:transparent;color:var(--text)">Cancel</button>' +
        '<button data-act="yes" style="' + btn + ';background:var(--accent,#3fb950);color:#fff;border-color:transparent;font-weight:600">Save</button>' +
        '</div></div>';

      function close() {
        document.removeEventListener('keydown', onKey);
        overlay.remove();
      }
      function onKey(e) {
        if (e.key === 'Escape') close();
        if (e.key === 'Enter') save();
      }
      /* Live echo of what the code resolves to, so a typo is visible BEFORE
         saving rather than as a surprise flag afterwards. Blank input reads as
         the explicit "no flag" state, not as an error. */
      function preview() {
        var el = document.getElementById('_country-input');
        var pv = document.getElementById('_country-preview');
        if (!el || !pv) return;
        var c = normalizeCountryCode(el.value);
        if (!el.value.trim()) { pv.textContent = 'No flag will be shown.'; return; }
        if (!c) { pv.textContent = 'Not a two-letter country code yet.'; return; }
        pv.textContent = countryFlagEmoji(c) + '  ' + countryDisplayName(c);
      }
      async function save() {
        var el = document.getElementById('_country-input');
        var err = document.getElementById('_country-err');
        var raw = el ? el.value.trim() : '';
        /* '' is a legitimate value here (clear); anything else must be a valid
           code. Checked client-side for a fast message, and again server-side
           because a client check is a convenience, never a control. */
        if (raw !== '' && !normalizeCountryCode(raw)) {
          if (err) { err.textContent = 'Enter a two-letter country code, or leave it blank.'; err.style.display = ''; }
          return;
        }
        try {
          var resp = await fetch('/api/saas/me/country', {
            method: 'PUT',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({country: normalizeCountryCode(raw)})
          });
          var data = await resp.json();
          if (!resp.ok) throw new Error(data.error || 'save failed');
          _myCountry = normalizeCountryCode(data.country);
          var nav = document.getElementById('nav-country');
          if (nav) nav.innerHTML = countryNavHTML(_myCountry);
          close();
        } catch (e) {
          if (err) { err.textContent = 'Error: ' + e.message; err.style.display = ''; }
        }
      }

      overlay.addEventListener('click', function(e) {
        if (e.target === overlay) { close(); return; }
        var act = e.target.getAttribute && e.target.getAttribute('data-act');
        if (act === 'yes') save();
        else if (act === 'no') close();
      });
      overlay.addEventListener('input', preview);
      document.addEventListener('keydown', onKey);
      document.body.appendChild(overlay);
      var inp = document.getElementById('_country-input');
      if (inp) { inp.focus(); inp.select(); }
      preview();
    }

    /* Rendered avatar sizes, in CSS pixels, one per surface. They differ because
       the surfaces differ in density, not arbitrarily: the status-dot hover
       panel and the compact request/access lists are tight vertical lists; the
       admin Users table row and the pending provision cards have more room. */
    var PANEL_ACCESS_AVATAR_PX = 18;   /* rows inside the status-dot hover panel */
    var LIST_AVATAR_PX = 20;           /* compact request/access lists */
    var TABLE_AVATAR_PX = 24;          /* admin Users table + provision cards */
    var NAV_AVATAR_PX = 28;            /* top nav viewer avatar; mirrors .nav-avatar CSS */

    // ghRepoURL builds the URL for an org/repo on the RIGHT GitHub instance.
    // Hives are not all on public github.com: a provision request carries a
    // github_host ('' = public github.com, otherwise a GitHub Enterprise host
    // such as github.ibm.com), so the host is taken from the record and never
    // hardcoded. Returns '' when there is no org or no repo to link to, so
    // callers can fall back to plain text rather than emitting a dead link.
    // hiveForgeHost returns the GitHub instance a HIVE ROW lives on. The hive
    // JSON (RegistryEntry/MyHiveEntry) spells the field githubHost; older
    // payloads and provision records spell it github_host. Reading only one
    // spelling silently yielded undefined on every hive row, which made the
    // repo links and their tooltips claim github.com even for a hive whose
    // default repo is on github.ibm.com. Empty means "unknown" so ghRepoURL
    // and the tooltip apply their own public-GitHub default.
    function hiveForgeHost(h) {
      if (!h) return '';
      return h.githubHost || h.github_host || h.default_forge || '';
    }

    function ghRepoURL(host, org, repo) {
      if (!org || !repo) return '';
      var h = (host || 'github.com').replace(/^https?:\/\//, '').replace(/\/+$/, '');
      // Path segments are encoded; the host is not (it is a hostname, and
      // encoding would break the dots) but is constrained to a hostname shape.
      if (!/^[A-Za-z0-9.-]+$/.test(h)) return '';
      return 'https://' + h + '/' + encodeURIComponent(org) + '/' + encodeURIComponent(repo);
    }

    // sameShaJS: two git SHAs are the same commit even if one is a short
    // prefix of the other. The hub stores 7-char short SHAs while a spoke may
    // report a longer one; a raw === left the "Upgrading" badge spinning
    // forever on a hive already at the target. Mirrors hub sameCommit().
    function sameShaJS(a, b) {
      if (!a || !b) return false;
      var n = Math.min(a.length, b.length);
      return a.slice(0, n).toLowerCase() === b.slice(0, n).toLowerCase();
    }

    function dismissBanner(key, btn) {
      var dismissed = JSON.parse(localStorage.getItem('hive-dismissed-banners') || '{}');
      dismissed[key] = Date.now();
      localStorage.setItem('hive-dismissed-banners', JSON.stringify(dismissed));
      btn.parentNode.remove();
    }

    function hiveToast(msg, type) {
      var t = document.createElement('div');
      t.className = 'hive-toast ' + (type || 'info');
      t.textContent = msg;
      var toastBaseTop = 70;
      var toastGap = 8;
      var existing = document.querySelectorAll('.hive-toast');
      var offset = 0;
      existing.forEach(function(e) { offset += e.offsetHeight + toastGap; });
      t.style.top = (toastBaseTop + offset) + 'px';
      document.body.appendChild(t);
      setTimeout(function() { t.remove(); }, 4000);
    }

    /* copyHiveText copies a plain string to the clipboard for the small
       copy-on-click affordances in the hive rows (e.g. the Kubernetes
       namespace). Prefers the async Clipboard API and falls back to a hidden
       textarea + execCommand for older/insecure-context browsers, so the copy
       works whether or not the dashboard is served over HTTPS. Always shows a
       toast so the (silent) copy is confirmed. */
    function copyHiveText(text, label) {
      var msg = 'Copied ' + (label || 'value');
      function ok() { hiveToast(msg, 'success'); }
      function fail() { hiveToast('Copy failed — select and copy manually', 'error'); }
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(ok, function() {
          if (!legacyCopy(text)) fail(); else ok();
        });
        return;
      }
      if (legacyCopy(text)) ok(); else fail();
    }
    function legacyCopy(text) {
      try {
        var ta = document.createElement('textarea');
        ta.value = text;
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        ta.select();
        var okc = document.execCommand('copy');
        ta.remove();
        return okc;
      } catch (e) { return false; }
    }

    function hiveConfirm(msg, rawHTML) {
      return new Promise(function(resolve) {
        var overlay = document.createElement('div');
        overlay.className = 'hive-confirm-overlay';
        overlay.innerHTML = '<div class="hive-confirm"><p>' + (rawHTML ? msg : esc(msg)) + '</p><div class="hive-confirm-btns">' +
          '<button style="padding:8px 16px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>' +
          '<button style="padding:8px 16px;background:var(--red);color:#fff;border:none;border-radius:6px;cursor:pointer" id="hive-confirm-ok">Confirm</button></div></div>';
        document.body.appendChild(overlay);
        var done = false;
        function finish(val) { if (done) return; done = true; overlay.remove(); document.removeEventListener('keydown', onKey); resolve(val); }
        function onKey(e) { if (e.key === 'Escape') finish(false); if (e.key === 'Enter') finish(true); }
        document.addEventListener('keydown', onKey);
        overlay.querySelector('#hive-confirm-ok').onclick = function() { finish(true); };
        overlay.querySelector('button:first-child').onclick = function() { finish(false); };
        overlay.querySelector('#hive-confirm-ok').focus();
      });
    }

    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Escape') return;
      var createModal = document.getElementById('create-modal');
      if (createModal && createModal.style.display === 'flex') { createModal.style.display = 'none'; return; }
      var accessOverlay = document.querySelector('.hive-confirm-overlay');
      if (accessOverlay) { accessOverlay.remove(); return; }
      var timelineModal = document.getElementById('timeline-modal');
      if (timelineModal && timelineModal.style.display === 'flex') { timelineModal.style.display = 'none'; return; }
      var accessModal = document.getElementById('access-modal');
      if (accessModal && accessModal.style.display === 'flex') { accessModal.style.display = 'none'; }
    });

    var ACMM_LABELS = {1:'L1 Inception',2:'L2 Advisory',3:'L3 Quality-Gated',4:'L4 Security-Aware',5:'L5 Semi-Autonomous',6:'L6 Fully Autonomous'};
    /* One-line "what this level actually does" per ACMM level, hoisted out of
       acmmBadge so the badge tooltip and the Request-a-Hive level picker read
       from the SAME strings. They used to be a local inside acmmBadge, and the
       request modal carried its own hand-written copy that had drifted into
       plain wrong ("L3 CI/CD", "L4 Auto PR", "L6 Fully Autonomous") — L4 in
       particular is NOT fully autonomous, it opens hold-gated PRs for human
       review. Anything describing a level must use this object. */
    var ACMM_TIPS = {1:'L1 Inception — Advisory only.',2:'L2 Advisory — Advisory beads, no GitHub writes.',3:'L3 Quality-Gated — Hold-gated PRs, CI gates.',4:'L4 Security-Aware — Agents open issues, sec-check.',5:'L5 Semi-Autonomous — PRs with hold label, batch review.',6:'L6 Fully Autonomous — Auto-merge on green CI.'};
    /* The tip strings above lead with the level label; the picker renders the
       label separately, so strip the redundant "Lx Name — " prefix there. */
    function acmmTipDetail(level) {
      var tip = ACMM_TIPS[level] || '';
      var dash = tip.indexOf('—');
      return dash >= 0 ? tip.slice(dash + 1).trim() : tip;
    }
    function sparkline(points, color, w, h) {
      if (!points || points.length < 2) return '';
      var vals = points.map(function(p) { return p.v; });
      var mn = Math.min.apply(null, vals);
      var mx = Math.max.apply(null, vals);
      var range = mx - mn || 1;
      var sw = w || 60;
      var sh = h || 16;
      var step = sw / (vals.length - 1);
      var pts = vals.map(function(v, i) {
        return (i * step).toFixed(1) + ',' + (sh - ((v - mn) / range) * sh).toFixed(1);
      }).join(' ');
      return '<svg width="' + sw + '" height="' + sh + '" style="vertical-align:middle;margin-right:4px"><polyline points="' + pts + '" fill="none" stroke="' + (color || '#6b7280') + '" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
    }

    function acmmBadge(level) {
      var l = level || 0;
      return '<span class="acmm-badge acmm-' + l + '" title="' + esc(ACMM_TIPS[l] || '') + '">' + (ACMM_LABELS[l] || 'L' + l) + '</span>';
    }
    /* Classified GitHub App auth states, matching github.AppAuthState.String()
       on the spoke and operatorSideAppStates in journey.go. The two OPERATOR
       states describe a failure the hive OWNER cannot fix: the App private key
       is distributed by the hub, so a missing or mismatched key is our problem,
       not theirs. The UI must never present these as something the user should
       install or reconfigure. */
    var GH_APP_STATE_KEY_MISSING = 'key-missing';
    var GH_APP_STATE_KEY_INVALID = 'key-invalid';
    var GH_APP_OPERATOR_STATES = {};
    GH_APP_OPERATOR_STATES[GH_APP_STATE_KEY_MISSING] = true;
    GH_APP_OPERATOR_STATES[GH_APP_STATE_KEY_INVALID] = true;
    /* An unreported or unrecognised state is deliberately NOT operator-side:
       a spoke too old to classify keeps the existing behaviour. */
    function ghAppIsOperatorSide(state) {
      return GH_APP_OPERATOR_STATES[String(state || '').trim()] === true;
    }
    /* Popover label for a user-side App issue, from the classified state.
       The blanket "permissions insufficient" wording is reserved for genuine
       permission states (and for unclassified reports from older spokes);
       the other states name their real cause so the hover stops sending an
       admin to approve permissions when the installation_id is what's wrong. */
    function ghAppPermIssueLabel(state) {
      var s = String(state || '').trim();
      if (s === 'wrong-installation') return 'wrong installation (installation_id points at another account)';
      if (s === 'write-forbidden') return 'write forbidden (repo not in the App installation)';
      if (s === 'no-app-assigned') return 'no App assigned yet';
      return 'permissions insufficient';
    }

    /* Labels for each journey stage, matching JourneyStage.String() on the hub. */
    var JOURNEY_STAGE_LABELS = {
      'none': 'On track',
      'github-app': 'GitHub App',
      'method-model': 'Method/model',
      'acmm-level': 'ACMM',
    };
    /* What each stage means, for the hover tooltip. */
    var JOURNEY_STAGE_TIPS = {
      'none': 'No outstanding adoption steps.',
      'github-app': 'Stage 1 — the GitHub App is not installed, so no agent can open issues or PRs.',
      'method-model': 'Stage 2 — no agent has a method/model assigned and ClankeR (the contributor relay) is not in use.',
      'acmm-level': 'Stage 3 — running steadily; due a gentle suggestion to consider the next ACMM level. Never a de-provision risk.',
    };
    /* Rank a journey status for sorting. Snoozed hives rank lowest — an admin
       has already dealt with them — then on-track, then by escalation. */
    var JOURNEY_SEVERITY_RANK = {'none': 0, 'gentle': 1, 'firm': 2, 'deprovision-warning': 3};
    function journeySortValue(j) {
      if (!j) return -1;
      if (j.snoozed) return -1;
      var sev = JOURNEY_SEVERITY_RANK[j.severity || 'none'] || 0;
      /* SEVERITY_SPAN spaces the stage ranks apart so severity orders within a
         stage without ever bleeding into the next stage's band. */
      var SEVERITY_SPAN = 10;
      return (j.stageNum || 0) * SEVERITY_SPAN + sev;
    }
    function journeyBadge(j) {
      if (!j) return '<span style="color:var(--muted)">—</span>';
      var stage = j.stage || 'none';
      var label = JOURNEY_STAGE_LABELS[stage] || stage;
      var tip = JOURNEY_STAGE_TIPS[stage] || '';
      if (j.stalledFor && stage !== 'none') tip += ' Stalled for ' + j.stalledFor + '.';
      /* A snoozed hive shows as snoozed regardless of stage — an admin has
         exempted it, so it must not read as an active problem. */
      var cls, text;
      if (j.snoozed) {
        cls = 'journey-snoozed';
        text = label + ' (snoozed)';
        tip = 'Nudges snoozed by an admin' + (j.snoozedUntil ? ' until ' + j.snoozedUntil : '') + '. ' + tip;
      } else {
        var sev = j.severity || 'none';
        cls = 'journey-' + (sev === 'none' ? 'none' : sev);
        text = label;
        if (sev === 'deprovision-warning') text = label + ' ⚠';
      }
      var html = '<span class="journey-badge ' + cls + '" title="' + esc(tip) + '">' + esc(text) + '</span>';
      /* Stage 2 satisfied via ClankeR gets its own visible badge. */
      if (j.viaRelay) {
        html += '<span class="journey-relay" title="' + esc('Stage 2 is satisfied through ClankeR, the contributor relay (human contributors picking up tasks), not by an assigned method/model.') + '">relay</span>';
      }
      return html;
    }
    function roleBadge(role) {
      var cls = role === 'owner' ? 'role-owner' : role === 'merger' ? 'role-merger' : role === 'read-write' ? 'role-read-write' : 'role-read';
      return '<span class="role-badge ' + cls + '">' + esc(role) + '</span>';
    }

    /* ---- Inline hive access avatars -----------------------------------
       The status-dot hover panel (healthBadge) is the DETAIL view of who can
       reach a hive: avatar + username + role, one per line. These inline
       avatars are the scannable SUMMARY of the same h.access list, rendered in
       the name cell so "who else is on this hive" is answerable without
       hovering 30 rows one at a time. Both read the same server-built list
       (accessForHive), so they can never disagree about membership.

       Role colours live here and are reused by the hover panel's rows so a
       face in the row and its line in the panel read as the same role. */
    var ACCESS_ROLE_COLORS = {'owner': '#d29922', 'merger': '#a371f7', 'read-write': '#3fb950'};
    var ACCESS_ROLE_COLOR_DEFAULT = '#6b7280';
    function accessRoleColor(role) {
      return ACCESS_ROLE_COLORS[role] || ACCESS_ROLE_COLOR_DEFAULT;
    }
    function roleAtLeast(role, tier) {
      var rank = {'read': 1, 'read-write': 2, 'merger': 3, 'owner': 4};
      return (rank[role] || 0) >= (rank[tier] || 0);
    }

    /* One-line description of what each grantable role allows. Shown as the
       native tooltip on every role <option>/<select> and as the inline hint
       under the Manage Access "Add User" role dropdown (#4144), so an owner
       understands what a role grants BEFORE granting it. Keep in sync with
       the role validation in hub.handleAccessAdd / config.ValidRole. */
    var ROLE_DESCRIPTIONS = {
      'read': 'View-only: dashboard, agents and config. Cannot change anything.',
      'read-write': 'Everything Read grants, plus contribute: queue work, manage the queue and open a terminal.',
      'merger': 'Everything Read-Write grants, plus approve and queue other contributors\' work for auto-merge.',
      'owner': 'Full control: manage access, settings and budget for this hive.'
    };
    function roleDescription(role) {
      return ROLE_DESCRIPTIONS[role] || '';
    }
    /* updateAccessRoleHint repaints the inline description under the Add User
       role dropdown to match the currently selected role. */
    function updateAccessRoleHint() {
      var sel = document.getElementById('access-role');
      var hint = document.getElementById('access-role-hint');
      if (!sel || !hint) return;
      var d = roleDescription(sel.value);
      hint.textContent = d ? sel.options[sel.selectedIndex].text + ' \u2014 ' + d : '';
    }

    /* Faces shown inline before collapsing the rest into a "+N" chip. The hive
       table is 16 columns and already dense; four 16px faces plus the chip fit
       beside the role badge without widening the name column, and a hive with
       a dozen members must not push the row wide. The full list stays one
       hover away on the status dot. */
    var INLINE_ACCESS_AVATAR_MAX = 4;
    /* Rendered size of an inline face, in CSS pixels — small enough to sit on
       the name cell's second line beside the role badge. */
    var INLINE_ACCESS_AVATAR_PX = 16;
    /* The pixel size requested from GitHub is INLINE_ACCESS_AVATAR_PX *
       AVATAR_HIDPI_SCALE, applied by the shared avatarImg() helper so every
       face in the dashboard stays sharp on HiDPI by the same rule. */

    /* Everyone with access to h EXCEPT the signed-in viewer. Their own
       membership is implied by the row being visible to them, so showing their
       own face on every row would be noise. GitHub logins are case-insensitive,
       hence the lowercased compare. Entries with no username are dropped: they
       would render as a permanently broken image.

       Returns [] for placeholder (unassigned) rows — a pool slot nobody has
       been granted has no meaningful membership to summarise — and for rows
       where the server withheld the access list (h.access is only populated for
       rows the caller owns, or for the hub admin). */
    function otherHiveMembers(h) {
      if (!h || isPlaceholderHive(h)) return [];
      var out = [];
      var list = (h.access || []);
      for (var i = 0; i < list.length; i++) {
        var a = list[i] || {};
        var uname = String(a.username || '');
        if (!uname) continue;
        if (_currentUser && uname.toLowerCase() === _currentUser) continue;
        out.push(a);
      }
      return out;
    }

    /* One inline face, linked to that user's GitHub profile. Carries a native
       title ONLY — deliberately no custom hover panel on this element. The
       status dot owns the one custom panel in this row (see healthBadge and
       TestSingleHoverPanelInvariant); an element with both draws the browser
       tooltip on top of the panel, which is the overlap bug that was fixed once
       already. The title moves to the anchor, which is still a plain native
       tooltip, not a panel.

       The role stays in the tooltip so the face still answers "who and at what
       permission" on hover, exactly as before it became a link. The title is
       enriched with the person's contact metadata (full name, Slack handle, and
       — for a hub admin only — notes) when the access entry carries it, so the
       face answers "who IS this" and not just "which handle". Each field is a
       separate line: title attributes render "\n" as a line break, and this stays
       a native tooltip (never a custom panel) to preserve the single-hover-panel
       invariant. The server only populates these fields for owner/admin-visible
       rows and withholds notes from non-admins, so nothing here needs to re-gate
       them — a field that is absent simply produces no line. */
    /* accessAvatarTitle builds the NATIVE multi-line tooltip for a co-member face.
       It deliberately stays a title attribute (never a hive-access-pop custom
       panel — see TestInlineAvatarsCarryNoCustomPanel; the status dot owns the
       row's one panel). It carries the same engagement info the admin Users card
       shows — identity, logins, time-in-hive, task activity, and the verdict — so
       who-to-elevate/help reads on hover anywhere a face appears. The stat lines
       are admin-only (accessForHive only fills login_count/session_seconds for an
       admin) and each line is conditional, so a stat-less user degrades to today's
       "handle — role". Newlines render as line breaks in a native title. */
    function accessAvatarTitle(a) {
      var uname = String(a.username || '');
      var role = String(a.role || '');
      // display_label is resolved hub-side (accessForHive) with the same
      // precedence used everywhere else a friendly name is shown, and always
      // falls back to the raw key — so it's only worth a separate first line
      // when it's actually friendlier than uname itself. The raw key rides
      // on every title regardless (first line), same as before this field
      // existed.
      var label = String(a.display_label || '');
      var lines = [uname + (role ? ' — ' + role : '')];
      if (label && label !== uname) lines.splice(0, 0, label);
      // ("Logged into their hive now" is appended generically in avatarProfileLink
      // for EVERY avatar surface, so it is not added here — doing both would
      // double the line.)
      if (a.full_name) lines.push(String(a.full_name));
      if (a.slack_id) lines.push('Slack: ' + String(a.slack_id));
      if (a.notes) lines.push('Notes: ' + String(a.notes));
      // Engagement stats (admin-only; absent → these lines are simply skipped).
      if (a.login_count) lines.push('Logins: ' + a.login_count);
      if (a.session_seconds) lines.push('Time in hive: ' + fmtHours(a.session_seconds));
      if (a.engaged_seconds) lines.push('Engaged time: ' + fmtHours(a.engaged_seconds));
      if (a.last_action_at) lines.push('Last real action: ' + fmtUserTS(a.last_action_at));
      var act = userTaskActivity(uname);
      if (act.done || act.failed) lines.push('Tasks: ' + act.done + ' done / ' + act.failed + ' failed');
      // The lifecycle verdict, from the same helper the admin card uses. It reads
      // logins/time/tasks; a co-member's hive-journey list isn't on the access
      // entry so pass empty — the verdict still classifies from engagement, only
      // the ACMM-graduation refinement is unavailable here. Shown only when there
      // is some signal to classify (any stat present), so a bare face stays bare.
      if (a.login_count || a.session_seconds || act.done) {
        var verdict = userVerdict({login_count: a.login_count || 0, session_seconds: a.session_seconds || 0, engaged_seconds: a.engaged_seconds || 0, last_action_at: a.last_action_at || ''}, act, []);
        if (verdict && verdict.label) lines.push(verdict.label);
      }
      return lines.join('\n');
    }
    function inlineAccessAvatar(a) {
      var uname = String(a.username || '');
      var role = String(a.role || '');
      var provider = a.provider || identityProviderFromKey(uname);
      var extraStyle = 'border:1px solid ' + accessRoleColor(role) + ';background:var(--surface);flex:0 0 auto';
      // A non-GitHub key (ibmid/google/microsoft) has no github.com profile to
      // link to — linkedAvatar would build a 404'ing image and a link to
      // someone else's account by coincidence of URL-shape. userAvatar uses
      // the provider-stored avatar_url when present, else initials derived
      // from the real display label (never from the opaque provider:sub key).
      var avatar = provider === 'github'
        ? linkedAvatar(uname, INLINE_ACCESS_AVATAR_PX, accessAvatarTitle(a), extraStyle)
        : userAvatar({display_name: a.display_label, avatar_url: a.avatar_url, github_username: uname},
            INLINE_ACCESS_AVATAR_PX, extraStyle);
      if (provider === 'github') return avatar;
      // userAvatar returns a bare <img> with no title/tooltip and no profile
      // link (there is none to link to) — wrap it so the same rich tooltip
      // accessAvatarTitle gives GitHub faces is not lost for everyone else.
      return '<span title="' + escAttr(accessAvatarTitle(a)) + '" style="display:inline-block;line-height:0">' + avatar + '</span>';
    }

    /* Inline summary of the OTHER users on this hive, or '' when there are
       none. Returning the empty string (rather than an empty container) matters:
       an empty wrapper would still occupy its gap and margin and make rows with
       no co-members sit a few pixels wider than their neighbours. */
    function hiveAccessAvatars(h) {
      var members = otherHiveMembers(h);
      if (!members.length) return '';
      var shown = members.slice(0, INLINE_ACCESS_AVATAR_MAX);
      var faces = '';
      for (var i = 0; i < shown.length; i++) faces += inlineAccessAvatar(shown[i]);
      var overflow = members.length - shown.length;
      if (overflow > 0) {
        /* The +N chip names the people it hides, so the hidden members are
           still identifiable without opening the hover panel. */
        var hiddenNames = [];
        for (var j = shown.length; j < members.length; j++) {
          /* display_label (when present) is the resolved human name for an
             opaque OIDC key — same precedence the visible faces use. */
          hiddenNames.push(String(members[j].display_label || members[j].username || ''));
        }
        faces += '<span title="' + escAttr(hiddenNames.join(', ')) + '" ' +
          'style="font-size:0.62rem;color:var(--muted);font-weight:600;white-space:nowrap;cursor:help">+' + overflow + '</span>';
      }
      var label = members.length === 1
        ? '1 other user with access'
        : members.length + ' other users with access';
      return '<span class="hive-access-faces" aria-label="' + escAttr(label) + '" ' +
        'style="display:inline-flex;align-items:center;gap:2px;margin-left:6px;vertical-align:middle">' + faces + '</span>';
    }
    function fmtTokens(n) {
      n = Number(n) || 0;
      if (n <= 0) return '<span style="color:var(--muted)">—</span>';
      if (n >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, '') + 'B';
      if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
      if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'K';
      return String(n);
    }
    function clusterBadge(clusterId, clusterName) {
      var cid = clusterId || 'hive-oke';
      var isGPU = cid === 'vllm-d' || (clusterName || '').toLowerCase().indexOf('gpu') >= 0;
      var bg = isGPU ? 'rgba(34,197,94,0.15)' : 'rgba(59,130,246,0.15)';
      var color = isGPU ? '#3fb950' : '#60a5fa';
      var border = isGPU ? 'rgba(34,197,94,0.3)' : 'rgba(59,130,246,0.3)';
      var title = clusterName || cid;
      return '<span class="cluster-badge" style="display:inline-block;padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:' + bg + ';color:' + color + ';border:1px solid ' + border + '" title="' + esc(title) + '">' + esc(cid) + '</span>';
    }
    function modeBadge(mode) {
      var m = (mode || 'idle').toUpperCase();
      var levels = {IDLE:0, QUIET:1, BUSY:2, SURGE:3};
      var colors = {IDLE:'#6b7280', QUIET:'#3b82f6', BUSY:'#f59e0b', SURGE:'#ef4444'};
      var fill = levels[m] !== undefined ? levels[m] : 0;
      var c = colors[m] || '#6b7280';
      var bars = '';
      for (var i = 0; i < 4; i++) {
        var h = 6 + i * 4;
        var bc = i <= fill ? c : '#1e1e2e';
        bars += '<rect x="' + (i * 6) + '" y="' + (20 - h) + '" width="4" height="' + h + '" rx="1" fill="' + bc + '"/>';
      }
      return '<span title="' + m + '" style="display:inline-flex;align-items:center;gap:4px"><svg width="24" height="20" viewBox="0 0 24 20">' + bars + '</svg><span style="font-size:0.7rem;color:' + c + ';font-weight:600">' + m + '</span></span>';
    }
    /* Status-filter keys for the My Hives list. Kept as named constants so the
       filter chips, the classifier and the persisted selection can never drift
       apart on a typo'd string literal. */
    var HIVE_FILTER_APP_MISSING = 'app-missing';
    var HIVE_FILTER_NO_TOKENS = 'no-tokens';
    var HIVE_FILTER_DEGRADED = 'degraded';
    var HIVE_FILTER_OK = 'ok';

    /* A hive has "used no tokens" when its last heartbeat reported a cumulative
       token total of zero — the same value the Tokens column renders as "—". */
    var NO_TOKENS_THRESHOLD = 0;

    /* hiveStatusFlags derives the four filterable states from data the hub
       ALREADY has per hive (all reported over the spoke heartbeat):
         - githubAppRequired / githubAppPermIssue  (RegistryEntry, server.go)
         - health.status                            (RegistryEntry.Health)
         - totalTokens24h                           (RegistryEntry)
         - online                                   (RegistryEntry)
       The GitHub-App and degraded rules deliberately mirror healthBadge()
       exactly, so a row's dot and the chip it matches can never disagree. */
    function hiveStatusFlags(h) {
      h = h || {};
      var hp = h.health || {};
      var st = hp.status || 'unknown';
      /* githubAppRequired means the spoke wants the App but it is not usable:
         with a perm issue it is installed-but-insufficient, without one it is
         not installed at all. healthBadge() forces "degraded" in both cases —
         EXCEPT on an unassigned placeholder, where having no usable App is the
         pool's designed state (mirrored here so dot and chip never disagree). */
      var appMissing = !isPlaceholderHive(h) && !!h.githubAppRequired && !h.githubAppPermIssue;
      var repoTargetBad = !isPlaceholderHive(h) && !!h.repoTargetMisconfigured;
      var degraded = repoTargetBad || (!isPlaceholderHive(h) && !!h.githubAppRequired) || st === 'degraded' || st === 'critical';
      /* An offline hive has no live reading at all, so it is not "OK" even if
         its last stored health snapshot said ok. */
      var ok = !degraded && st === 'ok' && !!h.online;
      return {
        appMissing: appMissing,
        noTokens: (Number(h.totalTokens24h) || 0) <= NO_TOKENS_THRESHOLD,
        degraded: degraded,
        ok: ok
      };
    }

    // uptimeCell renders process uptime for the Uptime column.
    //
    // This used to be an inline pill next to the hive name, which wrapped long
    // org names onto a second line. In its own column it can always show a
    // value, so the column reads as real data rather than an occasional
    // warning — but the COLOUR still carries the signal, since a hive can sit
    // at 1/1 Running while restarting every few minutes and otherwise look
    // perfectly healthy.
    function uptimeCell(h) {
      // Restart just happened; red. Matches the old inline pill's threshold.
      var RECENT_RESTART_SECS = 600;   // 10 min
      // Below this, still worth noticing — the pod has not settled yet.
      var SETTLING_SECS = 3600;        // 1 hour
      var SHOW_SECONDS_BELOW = 90;     // finer granularity while very fresh
      var HOUR_SECS = 3600, DAY_SECS = 86400;
      if (!h.startedAt) return '<span style="color:var(--muted)">—</span>';
      var secs = (Date.now() - new Date(h.startedAt).getTime()) / 1000;
      if (!isFinite(secs) || secs < 0) return '<span style="color:var(--muted)">—</span>';
      var label;
      if (secs < SHOW_SECONDS_BELOW) label = Math.round(secs) + 's';
      else if (secs < HOUR_SECS) label = Math.round(secs / 60) + 'm';
      else if (secs < DAY_SECS) label = (secs / HOUR_SECS).toFixed(1).replace(/\.0$/, '') + 'h';
      else label = Math.floor(secs / DAY_SECS) + 'd';
      var c = secs < RECENT_RESTART_SECS ? 'var(--red)'
            : (secs < SETTLING_SECS ? 'var(--yellow, #d29922)' : 'var(--muted)');
      var title = secs < SETTLING_SECS
        ? 'Restarted recently — a short value that keeps resetting means the pod is restarting'
        : 'Process uptime since the last restart';
      return '<span title="' + esc(title) + '" style="font-size:0.72rem;color:' + c + ';cursor:help">' + esc(label) + '</span>';
    }
    /* ---- Assignment / claim-pending counter --------------------------------
       When a placeholder is ASSIGNED but its spoke has never reported the
       project back (h.assignedUnclaimed, i.e. Status==assigned &&
       !ClaimDelivered), the operator has no cue how long the slot has been
       wedged. fmtAssignAge renders a compact human-friendly elapsed label from
       h.assignedAt, and claimPendingPill wraps it in a subtle muted pill that
       tints amber once the age crosses h.assignStuckSeconds — the SAME threshold
       the self-heal sweep uses to auto-reset the slot — so a stuck assignment is
       obvious at a glance and clearly "about to be auto-reset". Both self-suppress
       (return '') for any row that is not assigned-but-unclaimed or lacks a
       timestamp, so a claimed/available row shows nothing. The pill carries a
       data-assign-since attribute so a 1s ticker (startAssignCounterTicker) can
       live-update it between the 30s row refreshes without a full re-render. */
    function fmtAssignAge(secs) {
      var MIN = 60, HOUR = 3600;
      if (secs < MIN) return Math.floor(secs) + 's';
      if (secs < HOUR) {
        var m = Math.floor(secs / MIN), s = Math.floor(secs % MIN);
        return s > 0 ? m + 'm' + s + 's' : m + 'm';
      }
      var h = Math.floor(secs / HOUR), rem = Math.floor((secs % HOUR) / MIN);
      return rem > 0 ? h + 'h ' + rem + 'm' : h + 'h';
    }
    /* Render the inner span for a claim-pending pill given an assignedAt epoch
       (ms) and the stuck threshold (secs). Returns the muted-or-amber styled
       markup, or '' when there is no valid timestamp. Shared by the initial
       server-driven render and the live ticker so both stay in lockstep. */
    function claimPendingInner(sinceMs, stuckSecs) {
      if (!isFinite(sinceMs) || sinceMs <= 0) return '';
      var secs = (Date.now() - sinceMs) / 1000;
      if (!isFinite(secs) || secs < 0) secs = 0;
      var stuck = stuckSecs > 0 && secs >= stuckSecs;
      var color = stuck ? '#d29922' : 'var(--muted)';
      var title = stuck
        ? 'This assignment has exceeded the auto-reset threshold — the self-heal sweep will return this slot to the pool.'
        : 'Assigned but the spoke has not reported the project back yet — waiting for the claim to complete.';
      return '<span title="' + esc(title) + '" style="color:' + color + '">'
        + (stuck ? '⚠ ' : '')
        + 'claim pending · ' + esc(fmtAssignAge(secs)) + '</span>';
    }
    function claimPendingPill(h) {
      if (!h.assignedUnclaimed || !h.assignedAt) return '';
      var sinceMs = new Date(h.assignedAt).getTime();
      if (!isFinite(sinceMs) || sinceMs <= 0) return '';
      var stuckSecs = h.assignStuckSeconds || 0;
      var inner = claimPendingInner(sinceMs, stuckSecs);
      if (!inner) return '';
      /* data-* attributes let the 1s ticker recompute in place. The outer span
         keeps the pill styling; only the inner span's text/colour changes. */
      return '<span class="claim-pending-pill" data-assign-since="' + sinceMs
        + '" data-assign-stuck="' + stuckSecs
        + '" style="display:inline-block;margin-left:6px;padding:1px 7px;border-radius:9px;'
        + 'background:rgba(255,255,255,0.04);border:1px solid var(--border);font-size:0.68rem;white-space:nowrap;vertical-align:middle">'
        + inner + '</span>';
    }
    /* Live-tick every claim-pending pill once a second so the counter advances
       between the 30s row polls. Each tick re-derives colour+label from the
       pill's own data-* attributes, so a re-render that replaces the pill is
       picked up automatically (no per-pill timer to leak). Runs unconditionally
       and is a no-op when no pills are present. */
    var _assignCounterTimer = null;
    function tickAssignCounters() {
      var pills = document.querySelectorAll('.claim-pending-pill');
      for (var i = 0; i < pills.length; i++) {
        var el = pills[i];
        var sinceMs = parseFloat(el.getAttribute('data-assign-since'));
        var stuckSecs = parseInt(el.getAttribute('data-assign-stuck'), 10) || 0;
        var inner = claimPendingInner(sinceMs, stuckSecs);
        if (inner) el.innerHTML = inner;
      }
    }
    function startAssignCounterTicker() {
      if (_assignCounterTimer) return;
      _assignCounterTimer = setInterval(tickAssignCounters, 1000);
    }
    /* ---- Per-check health detail -------------------------------------------
       Health.checks[] arrives over the heartbeat as free-form JSON decoded into
       map[string]any on the hub (RegistryEntry.Health, server.go). It may be
       absent, null, an empty array, or hold entries missing name/status/detail.
       Every accessor below therefore guards each field individually: one
       malformed spoke payload must never blank the whole table. */

    /* Check statuses that count as a FAILURE for the inline row summary, the
       hover grouping and the per-check filter. 'warn' is included because an
       operator scanning for "what is wrong here" wants warnings surfaced too;
       'skip' is not a problem and 'pass' obviously is not. */
    var FAILING_CHECK_STATUSES = {fail: true, warn: true, critical: true, error: true};

    /* How many failing check NAMES to spell out inline in the row before
       collapsing to a bare count. The name column is already three lines deep,
       so one name is all that fits without pushing the row taller. */
    var INLINE_FAILING_CHECK_NAMES = 1;

    /* Longest failing-check name rendered inline before it is ellipsised.
       Check names are spoke-supplied and unbounded. */
    var INLINE_CHECK_NAME_MAXLEN = 22;

    /* healthChecks returns the hive's checks as a clean array of
       {name, status, detail} strings — never null, never holding non-objects. */
    function healthChecks(h) {
      var hp = (h && h.health) || {};
      var raw = hp.checks;
      if (!raw || !raw.length) return [];
      var out = [];
      for (var i = 0; i < raw.length; i++) {
        var ck = raw[i];
        if (!ck || typeof ck !== 'object') continue;
        var nm = typeof ck.name === 'string' ? ck.name : '';
        if (!nm) continue;   /* an unnamed check cannot be shown or filtered on */
        out.push({
          name: nm,
          status: typeof ck.status === 'string' ? ck.status : 'unknown',
          detail: typeof ck.detail === 'string' ? ck.detail : ''
        });
      }
      return out;
    }

    /* failingChecks returns only the checks in a failing/warning state. */
    function failingChecks(h) {
      return healthChecks(h).filter(function(ck) { return !!FAILING_CHECK_STATUSES[ck.status]; });
    }

    /* failingCheckSummary renders the compact in-row failure summary: the name
       of the single failing check, or "N checks failing" when there are more.
       Returns '' when nothing is failing, so healthy rows stay exactly as
       dense as they are today. */
    function failingCheckSummary(h) {
      var bad = failingChecks(h);
      if (!bad.length) return '';
      var label;
      if (bad.length <= INLINE_FAILING_CHECK_NAMES) {
        var nm = bad[0].name;
        if (nm.length > INLINE_CHECK_NAME_MAXLEN) nm = nm.substring(0, INLINE_CHECK_NAME_MAXLEN - 1) + '…';
        label = nm;
      } else {
        label = bad.length + ' checks failing';
      }
      /* Any 'fail'-class check is red; a row that is only warning stays amber so
         the pill colour agrees with the row dot. */
      var anyHard = bad.some(function(ck) { return ck.status !== 'warn'; });
      var col = anyHard ? '#f85149' : '#d29922';
      var full = bad.map(function(ck) {
        return ck.name + (ck.detail ? ': ' + ck.detail : '');
      }).join('\n');
      /* One element, one tooltip source: this pill owns a plain title and never
         also carries a custom panel (that is healthBadge()'s job). */
      return '<span title="' + esc(full) + '" style="display:inline-block;margin-left:6px;padding:0 6px;' +
        'border-radius:9999px;font-size:0.62rem;font-weight:600;line-height:1.5;cursor:help;white-space:nowrap;' +
        'color:' + col + ';background:' + (anyHard ? 'rgba(248,81,73,0.12)' : 'rgba(210,153,34,0.12)') + ';' +
        'border:1px solid ' + (anyHard ? 'rgba(248,81,73,0.35)' : 'rgba(210,153,34,0.35)') + '">' +
        esc(label) + '</span>';
    }

    /* advisoryStaleSummary renders a small "stale advisory" pill next to the
       failing-check pill when the hub has flagged this hive's advisory digest as
       stale. The flag (h.advisoryStale) and its reason are computed server-side
       by advisoryStale() — gated on advisory-mode participation AND app-can-write
       AND past-threshold — so this renderer never re-derives the rule and a
       not-installed or PR-mode hive is already filtered out before it gets here.
       Amber, matching the "worth noticing" band of the uptime pill: a stale
       digest is an operator nudge, not a hard row failure. */
    function advisoryStaleSummary(h) {
      if (!h || !h.advisoryStale) return '';
      var col = '#d29922';
      var title = h.advisoryStaleReason || 'Advisory digest has gone stale';
      return '<span title="' + esc(title) + '" style="display:inline-block;margin-left:6px;padding:0 6px;' +
        'border-radius:9999px;font-size:0.62rem;font-weight:600;line-height:1.5;cursor:help;white-space:nowrap;' +
        'color:' + col + ';background:rgba(210,153,34,0.12);border:1px solid rgba(210,153,34,0.35)">' +
        'stale advisory</span>';
    }

    /* advisoryFindingsSummary renders the finding count from this hive's last
       posted advisory digest, and — when the spoke's top-N cap held findings
       back — says so as "(top N)". Without that suffix a "10 findings" row on a
       capped hive reads as the complete picture while dozens more sit unshown,
       which is the exact misreading the cap's digest note exists to prevent.
       Silent (empty string) for a hive that has never posted a digest, so rows
       for non-advisory hives are pixel-identical. */
    function advisoryFindingsSummary(h) {
      if (!h || !h.advisoryLastPostedAt) return '';
      var shown = h.advisoryFindingCount || 0;
      var overflow = h.advisoryOverflowCount || 0;
      if (!shown && !overflow) return '';
      var total = shown + overflow;
      var label = total + (total === 1 ? ' finding' : ' findings');
      var title = 'Findings in this hive last advisory digest';
      if (overflow > 0) {
        /* The COUNT is the true total and the suffix is how many of them the
           digest actually rendered: "12 findings (top 10)". Showing the capped
           count in both places would say nothing the bare count does not. */
        label += ' (top ' + shown + ')';
        title = shown + ' of ' + total + ' findings shown — the rest are held back by this hive advisory max_findings setting';
      }
      return '<span title="' + esc(title) + '" style="display:inline-block;margin-left:6px;padding:0 6px;' +
        'border-radius:9999px;font-size:0.62rem;font-weight:600;line-height:1.5;cursor:help;white-space:nowrap;' +
        'color:#6b7280;background:rgba(107,114,128,0.12);border:1px solid rgba(107,114,128,0.35)">' +
        esc(label) + '</span>';
    }

    /* deadLinkSummary renders a "dead link" pill when this hive's own public
       URL did not serve on the last several probes. Red, not amber: unlike a
       stale digest this is user-facing breakage — the link in this very row
       returns an error page, and because the spoke itself is healthy and
       heartbeating, NOTHING else on the row indicates a problem.
       h.urlUnreachable and its reason are computed server-side from the same
       alert list the panel renders (see urlUnreachableFacet), gated on
       consecutive failures AND minimum age AND cluster-outage suppression, so
       this renderer never re-derives the rule and a converging or
       whole-cluster-out hive is filtered out before it gets here. */
    function deadLinkSummary(h) {
      if (!h || !h.urlUnreachable) return '';
      var title = h.urlUnreachableReason || 'This hive public URL did not respond';
      return '<span title="' + esc(title) + '" style="display:inline-block;margin-left:6px;padding:0 6px;' +
        'border-radius:9999px;font-size:0.62rem;font-weight:600;line-height:1.5;cursor:help;white-space:nowrap;' +
        'color:#f85149;background:rgba(248,81,73,0.12);border:1px solid rgba(248,81,73,0.35)">' +
        'dead link</span>';
    }

    /* privateURLSummary renders an informational chip when the hub cannot
       reach the dashboard URL from the public internet, but the hive is still
       heartbeating and the spoke has not reported its own self-check failing.
       It is intentionally muted rather than red: the condition is a network
       vantage-point mismatch, not evidence that the hive is broken. */
    function privateURLSummary(h) {
      if (!h || !h.privateUrl) return '';
      var title = h.privateUrlReason || 'Hub cannot reach this dashboard URL from the public internet, but the hive reports healthy';
      return '<span title="' + esc(title) + '" style="display:inline-block;margin-left:6px;padding:0 6px;' +
        'border-radius:9999px;font-size:0.62rem;font-weight:600;line-height:1.5;cursor:help;white-space:nowrap;' +
        'color:#8b949e;background:rgba(139,148,158,0.10);border:1px solid rgba(139,148,158,0.30)">' +
        'private URL</span>';
    }

    /* inferenceAuthSummary renders an "inference auth" pill when this hive's
       self-hosted inference backend is rejecting every call with 401 (a stale
       gateway key). Red, like the dead-link pill: this is the ROOT cause of an
       otherwise silent outage — the hive heartbeats and looks online while
       every agent is dead in the water because no inference call succeeds, and
       NOTHING else on the row indicates it. h.inferenceAuthError is the
       spoke-reported, log-safe cause carried verbatim on the RegistryEntry (the
       spoke latches it only after several consecutive 401s and clears it on the
       next success), so this renderer never re-derives the rule and a healthy
       or transiently-blipping hive is already filtered out before it gets here.
       Deliberately NOT gated on h.online for the same reason as the dead-link
       pill: the hive IS online, that is the whole point. */
    function inferenceAuthSummary(h) {
      if (!h || !h.inferenceAuthError) return '';
      var title = h.inferenceAuthError || 'Inference backend is rejecting every call (401)';
      return '<span title="' + esc(title) + '" style="display:inline-block;margin-left:6px;padding:0 6px;' +
        'border-radius:9999px;font-size:0.62rem;font-weight:600;line-height:1.5;cursor:help;white-space:nowrap;' +
        'color:#f85149;background:rgba(248,81,73,0.12);border:1px solid rgba(248,81,73,0.35)">' +
        'inference auth</span>';
    }

    /* ---- ClankeR, the contributor relay (hover panel) ------------------
       The relay is a first-class SUBSTITUTE for assigning a method/model:
       when humans are picking up this hive's tasks through /contribute, the
       "tokens for an agent" requirement is satisfied. The product rule is
       that this must read as its OWN state — never silently folded into
       "assigned"/"OK" — so an operator can see the requirement is being met
       *via the relay*.

       The signal is h.journey.viaRelay, computed on the hub by relayInUse()
       (pkg/hub/journey.go) and already shipped on every My Hives row for the
       Journey column's "relay" badge. Reading the SAME field here is what
       keeps the two surfaces from ever disagreeing about one hive — there is
       no second, browser-side relay rule to drift out of sync. */

    /* Teal, matching the .journey-relay badge, so "relay" reads as the same
       thing in the Journey column and in this panel. */
    var RELAY_COLOR = '#2dd4bf';

    /* relayLine renders the relay state plus how many contributors are behind
       it. ContributorCount is the durable registered-profile count; active is
       the live-WebSocket count, which legitimately drops to zero when nobody
       is connected right now — so the registered count is what's headlined and
       the active count is only added when it is non-zero. */
    function relayLine(h) {
      var j = h.journey || {};
      if (!j.viaRelay) return '';
      var registered = h.contributorCount || 0;
      var active = h.activeContributors || 0;
      var who;
      if (registered > 0) {
        who = registered === 1 ? '1 contributor' : registered + ' contributors';
        if (active > 0) who += ', ' + active + ' active';
      } else if (active > 0) {
        who = active === 1 ? '1 active contributor' : active + ' active contributors';
      } else {
        /* viaRelay with no counts means the signal came from a leaderboard
           entry with a task in flight; say so rather than printing "0". */
        who = 'task in progress';
      }
      return '<div style="padding:1px 0;color:' + RELAY_COLOR + '">' +
        esc('◆ ClankeR · ' + who) + '</div>' +
        '<div style="padding:0 0 1px 12px;color:var(--muted);font-size:0.65rem">' +
        esc('satisfies the method/model requirement') + '</div>';
    }

    /* ---- Recent activity (hover panel) --------------------------------
       Events ride the My Hives payload as h.recentEvents (owner/admin only,
       see MyHiveEntry.RecentEvents) rather than being fetched on hover, so
       sweeping the pointer down the table costs zero requests. The full
       history stays in the timeline modal. */

    /* How many events the panel renders. The server already trims to
       myHivesRecentEventCount; this is the browser-side guard so an older or
       hand-crafted payload can never stretch the panel. */
    var HOVER_EVENT_COUNT = 3;

    /* An event detail is a full sentence; the panel is 300px wide at most, so
       long details are clipped to keep one event on one line. */
    var HOVER_EVENT_DETAIL_MAXLEN = 48;

    function hoverEventRows(h) {
      var events = (h.recentEvents || []).slice(0, HOVER_EVENT_COUNT);
      if (!events.length) return '';
      var rows = events.map(function(ev) {
        var kind = TIMELINE_KINDS[ev.kind] || { label: ev.kind || 'Event', color: '#8b949e' };
        var detail = ev.detail || '';
        if (detail.length > HOVER_EVENT_DETAIL_MAXLEN) {
          detail = detail.substring(0, HOVER_EVENT_DETAIL_MAXLEN - 1) + '…';
        }
        return '<div style="display:flex;gap:6px;align-items:baseline;padding:1px 0">' +
          '<span style="flex:0 0 auto;color:' + kind.color + ';font-size:0.6rem;font-weight:600">' + esc(kind.label) + '</span>' +
          '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--muted)">' + esc(detail) + '</span>' +
          '<span style="flex:0 0 auto;color:var(--muted);font-size:0.6rem">' + esc(timelineAgo(ev.ts)) + '</span>' +
          '</div>';
      }).join('');
      /* "View all" opens the existing 200-event modal. It is a data-attribute
         button read by a delegated listener, NOT an inline onclick: a hive name
         is spoke-reported and esc() escapes neither quotes nor apostrophes, so
         building a handler string from it would be an injection. The values
         land in quoted attributes, so they go through escAttr(), not esc(). */
      var viewAll = '<div style="padding:3px 0 0"><button type="button" class="hover-view-timeline" ' +
        'data-hive-id="' + escAttr(h.id) + '" data-hive-name="' + escAttr(h.name || h.id) + '" ' +
        'style="background:none;border:none;padding:0;color:var(--blue);font-size:0.62rem;cursor:pointer;text-decoration:underline">' +
        'View all activity</button></div>';
      return '<span style="display:block;border-top:1px solid var(--border);margin:6px 0 4px"></span>' +
        '<span style="display:block;color:var(--muted);font-size:0.62rem;text-transform:uppercase;letter-spacing:0.04em;margin-bottom:4px">Recent activity</span>' +
        rows + viewAll;
    }

    function healthBadge(h) {
      var hp = h.health || {};
      var st = hp.status || 'unknown';
      var colors = {ok:'#3fb950',warning:'#d29922',degraded:'#f85149',critical:'#ff4040',unknown:'#6b7280'};
      var icons = {ok:'✓',warning:'⚠',degraded:'⚠',critical:'✕',unknown:'?'};
      var checkIcons = {pass:'✓',fail:'✕',warn:'⚠',skip:'–'};
      var c = colors[st] || colors.unknown;
      var ic = icons[st] || '?';
      var isUpgrading = _upgradingHives[h.id];
      var isDeleting = _deletingHives[h.id];
      if (isDeleting) { c = colors.warning; ic = '⏳'; }
      var statusLabel = isDeleting ? 'Deleting…' : (isUpgrading ? 'Starting up after upgrade' : st.charAt(0).toUpperCase() + st.slice(1));
      /* Checks, failures first, so the reason for a bad status reads before the
         wall of passing checks. Stable within each group (spoke report order). */
      var checks = healthChecks(h).slice().sort(function(a, b) {
        var fa = FAILING_CHECK_STATUSES[a.status] ? 0 : 1;
        var fb = FAILING_CHECK_STATUSES[b.status] ? 0 : 1;
        return fa - fb;
      });
      var lines = [statusLabel];
      for (var i = 0; i < checks.length; i++) {
        var ck = checks[i];
        var ci = checkIcons[ck.status] || '?';
        var line = ci + ' ' + ck.name;
        if (ck.detail) line += ': ' + ck.detail;
        lines.push(line);
      }
      if (isPlaceholderHive(h)) {
        /* An UNASSIGNED pool slot has no GitHub auth BY DESIGN — credentials
           only arrive when a project is claimed — so none of the App-degraded
           rules below may fire here. The hub has already reclassified the
           auth-class checks (sanitizePlaceholderRows, placeholder_health.go);
           this line says WHY the row is green instead of claiming an App. */
        lines.push('– Unassigned — GitHub auth not configured by design');
      }
      else if (h.repoTargetMisconfigured) {
        lines.push('⚠ ' + (h.repoTargetIssue || 'Repo target misconfigured — expected org/repo. Fix in Settings → Repos.'));
        if (st === 'ok' || st === 'unknown') {
          st = 'warning'; c = colors.warning; ic = icons.warning; statusLabel = 'Warning'; lines[0] = statusLabel;
        }
      }
      else if (h.githubAppRequired && ghAppIsOperatorSide(h.githubAppState)) {
        /* Operator-side: the key we distribute has not landed, or is for the
           wrong App. Still degraded — the hive genuinely cannot work — but the
           hover must name the real cause so an admin does not chase the user
           about an installation that is already correct. */
        lines.push(h.githubAppState === GH_APP_STATE_KEY_INVALID
          ? '⚠ GitHub App: key does not match the App (operator must push the correct key)'
          : '⚠ GitHub App: credentials not yet delivered by the hub (operator action: upload the App key — PUT /api/saas/admin/cluster-app-keys/{cluster})');
        st = 'degraded'; c = colors.degraded; ic = icons.degraded; statusLabel = 'Degraded'; lines[0] = statusLabel;
      }
      else if (h.githubAppRequired && h.githubAppPermIssue) { lines.push('✓ GitHub App installed'); lines.push('⚠ GitHub App: ' + ghAppPermIssueLabel(h.githubAppState)); st = 'degraded'; c = colors.degraded; ic = icons.degraded; statusLabel = 'Degraded'; lines[0] = statusLabel; }
      else if (h.githubAppRequired) { lines.push('✕ GitHub App not installed'); st = 'degraded'; c = colors.degraded; ic = icons.degraded; statusLabel = 'Degraded'; lines[0] = statusLabel; }
      else if (!h.githubAppRequired) { lines.push('✓ GitHub App: not in use'); }
      if (!checks.length) lines.push('No check data');
      // Heartbeat freshness — so a reading that is minutes old isn't mistaken
      // for current health (the source of "stuck Degraded" reports: the last
      // heartbeat carried a transient failure and never got refreshed).
      if (h.lastHeartbeat) {
        var ageMs = Date.now() - new Date(h.lastHeartbeat).getTime();
        if (!isNaN(ageMs) && ageMs >= 0) {
          var ageMin = Math.floor(ageMs / 60000);
          var ageStr = ageMin < 1 ? 'just now' : (ageMin === 1 ? '1 min ago' : ageMin + ' min ago');
          lines.push('— as of ' + ageStr);
          // A reading older than 2× the heartbeat interval is stale; don't
          // present it as a live status.
          var staleAfterMs = 5 * 60000;
          if (ageMs > staleAfterMs && st !== 'unknown') {
            statusLabel = statusLabel + ' (stale)';
            lines[0] = statusLabel;
            c = colors.warning; ic = icons.warning;
          }
        }
      }
      // Provisioned time. Moved here from a dedicated table column — it is
      // low-frequency reference metadata (the hub's first-seen time for this
      // hive, preserved across restarts), so it belongs in the on-demand hover
      // beside the other temporal lines, not in a permanent column competing
      // with live metrics. hiveProvisionTime is the single source of truth for
      // "is registeredAt usable"; an em dash, never 'Invalid Date', when not.
      if (hiveProvisionTime(h) !== null) {
        lines.push('— provisioned ' + fmtUserTS(h.registeredAt));
      }
      // Kubernetes namespace. A hosted hive's namespace is the deterministic
      // "hive-hosted-<id>" (see hostedNamespaceForHive in
      // pkg/hub/hosted_namespace_identity.go) and — unlike the hive's
      // name/org — NEVER changes after a claim: the namespace this hive was
      // first provisioned into keeps its original (often placeholder) name
      // forever, because Kubernetes has no atomic namespace rename. Surfacing
      // it here is what lets an operator map "some namespace on the cluster"
      // back to "this hive" without cross-referencing the registry by hand —
      // exactly the gap the hive.kubestellar.io/* namespace labels close from
      // the cluster side. Computed client-side (not a separate stored field)
      // so it can never drift from the one place the namespace name is
      // decided; omitted for a non-hosted row, which has no such namespace.
      var hns = hiveNamespace(h);
      if (hns) {
        lines.push('ns: ' + hns);
      }
      var access = h.access || [];
      var dotMarkup = '<span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:' + c + '"></span>' +
        '<span style="font-size:0.7rem;color:' + c + ';font-weight:600">' + ic + '</span>';

      // No access list (not an owner of this row): nothing to render beyond the
      // health lines, so the native title tooltip is enough and cheapest.
      //
      // recentEvents is populated under the SAME owner/admin rule as access, so
      // a row without an access list has no events either — there is nothing to
      // consolidate here and this branch stays a plain title. The relay state is
      // carried as a text line so this branch still reports it, rather than the
      // relay being visible only to owners.
      if (!access.length) {
        var plainLines = lines.slice();
        if ((h.journey || {}).viaRelay) {
          plainLines.push('◆ ClankeR (contributor relay) in use — satisfies the method/model requirement');
        }
        return '<span title="' + esc(plainLines.join('\n')) + '" style="display:inline-flex;align-items:center;gap:4px;cursor:help;white-space:pre-line">' + dotMarkup + '</span>';
      }

      // ONE panel holding health AND access. Setting a title attribute here too
      // would make the browser draw its own tooltip on top of this panel — two
      // overlapping boxes saying different things, which is exactly what the
      // first cut of this did.
      var healthRows = (lines || []).map(function(l, i) {
        // lines[0] is the status word ("Warning", "Degraded", ...); render it as
        // the panel's title in the status colour, the rest as check lines.
        if (i === 0) {
          return '<span style="display:block;color:' + c + ';font-weight:600;margin-bottom:4px">' + esc(l) + '</span>';
        }
        /* Failing check lines (built above with ✕/⚠ icons) are lifted out of the
           muted grey so the reason for the status is the first thing read. The
           lines are already sorted failures-first. */
        var lc = 'var(--muted)';
        if (l.indexOf('✕') === 0) lc = '#f85149';
        else if (l.indexOf('⚠') === 0) lc = '#d29922';
        return '<div style="padding:1px 0;color:' + lc + '">' + esc(l) + '</div>';
      }).join('');

      var accessRows = access.map(function(a) {
        /* Shared with the inline row faces (accessRoleColor) so a face in the
           name cell and its line in this panel read as the same role. */
        var rc = accessRoleColor(a.role);
        /* The face links to the profile. The title lives on the ANCHOR, which is
           a plain native tooltip inside the panel — the invariant that matters
           is that no title sits on the panel's own root element (see
           TestSingleHoverPanelInvariant), not that the panel's contents are
           tooltip-free. */
        return '<div style="display:flex;align-items:center;gap:6px;padding:2px 0">' +
          linkedAvatar(a.username, PANEL_ACCESS_AVATAR_PX,
            accessAvatarTitle(a), 'flex:0 0 auto') +
          '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(a.username) + '</span>' +
          '<span style="color:' + rc + ';font-size:0.62rem;font-weight:600;white-space:nowrap">' + esc(a.role) + '</span>' +
          '</div>';
      }).join('');
      var heading = access.length === 1 ? '1 user with access' : access.length + ' users with access';

      return '<span class="hive-access-wrap" style="position:relative;display:inline-flex;align-items:center;gap:4px;cursor:help">' + dotMarkup +
        '<span class="hive-access-pop" style="display:none;position:absolute;left:0;top:calc(100% + 6px);z-index:60;' +
        'min-width:210px;max-width:300px;padding:8px 10px;border-radius:8px;border:1px solid var(--border);' +
        'background:var(--surface);box-shadow:0 6px 20px rgba(0,0,0,0.35);font-size:0.72rem;text-align:left;font-weight:400;white-space:normal">' +
        healthRows +
        relayLine(h) +
        '<span style="display:block;border-top:1px solid var(--border);margin:6px 0 4px"></span>' +
        '<span style="display:block;color:var(--muted);font-size:0.62rem;text-transform:uppercase;letter-spacing:0.04em;margin-bottom:4px">' + esc(heading) + '</span>' +
        accessRows +
        hoverEventRows(h) + '</span></span>';
    }
    /* ---- Heartbeat heart ----------------------------------------------
       heartbeatHeart renders a small red heart next to a hive's identity
       ONLY when a heartbeat is actually received: it flashes exactly three
       times, then disappears until the next beat. "Received" is detected by
       watching h.lastHeartbeat advance across renders. The maps below live
       in JS, keyed by hive id — not on the element — so the state survives
       the periodic full re-render exactly like the other row-state trackers
       in this file (_upgradingHives et al.). On the FIRST sighting of a hive
       (initial page load, or a hive newly appearing in the table) the
       timestamp is seeded silently so the whole table does not flash at
       once; only a subsequent advance triggers the flash. */
    // Duration of ONE pulse; must match the 0.6s in .heartbeat-heart-flash.
    var HEART_PULSE_MS = 600;
    // Pulses per received heartbeat; must match the CSS iteration count (3).
    var HEART_PULSE_COUNT = 3;
    // Total on-screen window for one flash sequence.
    var HEART_FLASH_TOTAL_MS = HEART_PULSE_MS * HEART_PULSE_COUNT;
    // hive id -> the last h.lastHeartbeat (epoch ms) this hive was seen with.
    var _heartSeenBeats = {};
    // hive id -> epoch ms until which the flash markup should still render.
    var _heartFlashUntil = {};
    function heartbeatHeart(h) {
      if (!h || !h.id || !h.lastHeartbeat) return '';
      var beatMs = new Date(h.lastHeartbeat).getTime();
      if (isNaN(beatMs)) return '';
      var prev = _heartSeenBeats[h.id];
      _heartSeenBeats[h.id] = beatMs;
      if (prev !== undefined && beatMs > prev) {
        // A NEW heartbeat landed since the last render: open the flash window.
        _heartFlashUntil[h.id] = Date.now() + HEART_FLASH_TOTAL_MS;
      }
      var until = _heartFlashUntil[h.id];
      if (!until || Date.now() >= until) return '';
      // A re-render mid-flash rebuilds the element; a negative delay resumes
      // the animation where it left off instead of restarting the 3-pulse
      // run, so the total flash never exceeds one sequence per heartbeat.
      var elapsedMs = HEART_FLASH_TOTAL_MS - (until - Date.now());
      var ageMs = Date.now() - beatMs;
      var ageStr = ageMs < 1000 ? 'just now' : (ageMs < 60000 ? Math.floor(ageMs / 1000) + 's ago' : Math.floor(ageMs / 60000) + 'm ago');
      return '<svg class="heartbeat-heart heartbeat-heart-flash" width="11" height="11" viewBox="0 0 16 16" fill="currentColor" ' +
        'style="animation-delay:-' + elapsedMs + 'ms" ' +
        'aria-hidden="true" title="' + 'Last heartbeat: ' + ageStr + '">' +
        '<path d="M8 14.2l-1.1-1C3 9.9 0.6 7.7 0.6 5 0.6 2.8 2.3 1.1 4.5 1.1c1.2 0 2.4 0.6 3.1 1.5 0.7-0.9 1.9-1.5 3.1-1.5 2.2 0 3.9 1.7 3.9 3.9 0 2.7-2.4 4.9-6.3 8.2L8 14.2z"/>' +
        '</svg>';
    }
    /* After the third pulse the forwards fill-mode already leaves the heart
       at opacity 0; this delegated listener (bound ONCE, so it survives every
       table re-render) also removes the spent element from hit-testing so an
       invisible heart cannot linger as a hover/tooltip target until the next
       refresh replaces the row. */
    document.addEventListener('animationend', function(e) {
      var t = e.target;
      if (t && t.classList && t.classList.contains('heartbeat-heart-flash')) t.style.display = 'none';
    });
    /* ---- Config drift -------------------------------------------------
       The server computes drift signals per hive (pkg/hub/drift.go) and ships
       them on the My Hives payload as h.drift = {signals, count, worstSeverity}.
       Everything below only RENDERS that; no drift rule lives in the browser,
       so the badge, the summary and any future consumer can never disagree
       about what counts as drift. */

    /* Palette reused from healthBadge() so a critical drift signal reads as the
       same red as a critical health dot. */
    var DRIFT_SEVERITY_COLORS = {info: '#58a6ff', warn: '#d29922', critical: '#f85149'};
    var DRIFT_SEVERITY_LABELS = {info: 'Info', warn: 'Warning', critical: 'Critical'};
    /* Display order, worst first, for the fleet-exceptions breakdown. */
    var DRIFT_SEVERITY_ORDER = ['critical', 'warn', 'info'];

    /* Human labels for the server's stable drift kind identifiers. A kind with
       no entry here falls back to its raw identifier rather than rendering
       blank, so a new server-side signal is still legible before the UI
       catches up. */
    var DRIFT_KIND_LABELS = {
      'version-behind':  'Version differs from fleet',
      'branch-mismatch': 'Branch differs from fleet',
      'pinned-image':    'Pinned to an immutable image',
      'heartbeat-stale': 'Heartbeat stale',
      'app-missing':     'GitHub App not installed',
      'app-perm-issue':  'GitHub App permissions',
      'app-creds-operator': 'GitHub App key (operator)',
      'app-id-placeholder': 'Placeholder App ID (operator)',
      'health-degraded': 'Health degraded',
      'upgrade-stuck':   'Upgrade stuck',
      'upgrading':       'Upgrading',
      'acmm-unset':      'ACMM level unset',
      'no-agents':       'No agents running',
      'duplicate-spoke': 'Duplicate spoke instances',
      'status-flipping': 'Status flipping'
    };

    function driftKindLabel(kind) { return DRIFT_KIND_LABELS[kind] || kind || 'Unknown'; }

    /* driftOf normalizes the server's report so every caller can treat signals
       as an array and count as a number, even for a row the server predates
       (h.drift undefined) or a payload that arrived malformed. */
    function driftOf(h) {
      var d = (h && h.drift) || {};
      var signals = Array.isArray(d.signals) ? d.signals : [];
      return {
        signals: signals,
        count: typeof d.count === 'number' ? d.count : signals.length,
        worstSeverity: d.worstSeverity || ''
      };
    }

    /* driftBadge renders the Drift column: the signal count coloured by the
       worst severity, with a hover panel listing every reason.

       It follows healthBadge()'s ONE-panel rule exactly: a custom hover panel
       and NO title attribute on the same element. Setting both makes the
       browser draw its native tooltip on top of the panel — two overlapping
       boxes saying different things. */
    function driftBadge(h) {
      var d = driftOf(h);
      if (!d.count) {
        return '<span title="No configuration drift detected" style="color:var(--muted);font-size:0.72rem;cursor:help">—</span>';
      }
      var c = DRIFT_SEVERITY_COLORS[d.worstSeverity] || DRIFT_SEVERITY_COLORS.info;

      var rows = (d.signals || []).map(function(s) {
        s = s || {};
        var sc = DRIFT_SEVERITY_COLORS[s.severity] || DRIFT_SEVERITY_COLORS.info;
        var sevLabel = DRIFT_SEVERITY_LABELS[s.severity] || s.severity || 'Info';
        /* First-seen: the server stamps when this (hive, kind) signal was
           first observed and keeps it stable while the signal persists (see
           stampDriftFirstSeen in drift.go). Rendered compactly ("since
           3:42 PM" today, "since Jul 30, 3:42 PM" otherwise) with the full
           absolute datetime as the title. An absent or unparseable stamp
           (older hub, hand-built report) renders nothing rather than a
           misleading "since Invalid Date". */
        var since = '';
        if (s.firstSeen) {
          var fd = new Date(s.firstSeen);
          if (!isNaN(fd.getTime())) {
            var timeStr = fd.toLocaleTimeString([], {hour: 'numeric', minute: '2-digit'});
            var label = fd.toDateString() === new Date().toDateString()
              ? timeStr
              : fd.toLocaleDateString([], {month: 'short', day: 'numeric'}) + ', ' + timeStr;
            since = '<div style="color:var(--muted);font-size:0.62rem;margin-top:2px" title="' +
              esc(fd.toLocaleString()) + '">since ' + esc(label) + '</div>';
          }
        }
        /* overflow-wrap on the reason: a signal can quote an unbroken token
           (an image ref, a URL, a pod name) longer than the panel is wide,
           and without a break opportunity it runs out of the dialog. */
        return '<div style="padding:4px 0;border-top:1px solid var(--border)">' +
          '<div style="display:flex;align-items:center;gap:6px;margin-bottom:2px">' +
          '<span style="display:inline-block;width:6px;height:6px;border-radius:50%;background:' + sc + ';flex:0 0 auto"></span>' +
          '<span style="color:' + sc + ';font-weight:600">' + esc(driftKindLabel(s.kind)) + '</span>' +
          '<span style="margin-left:auto;color:var(--muted);font-size:0.6rem;text-transform:uppercase;letter-spacing:0.04em">' + esc(sevLabel) + '</span>' +
          '</div>' +
          '<div style="color:var(--muted);line-height:1.4;overflow-wrap:anywhere;word-break:break-word">' + esc(s.reason || '') + '</div>' +
          since +
          '</div>';
      }).join('');

      var heading = d.count === 1 ? '1 drift signal' : d.count + ' drift signals';

      return '<span class="hive-access-wrap" style="position:relative;display:inline-flex;align-items:center;gap:4px;cursor:help">' +
        '<span style="display:inline-block;min-width:18px;padding:1px 6px;border-radius:9999px;font-size:0.65rem;font-weight:700;' +
        'color:' + c + ';background:rgba(255,255,255,0.04);border:1px solid ' + c + '">' + d.count + '</span>' +
        '<span class="hive-access-pop" style="display:none;position:absolute;right:0;top:calc(100% + 6px);z-index:60;' +
        'min-width:260px;max-width:380px;padding:8px 10px;border-radius:8px;border:1px solid var(--border);' +
        'background:var(--surface);box-shadow:0 6px 20px rgba(0,0,0,0.35);font-size:0.72rem;text-align:left;font-weight:400;' +
        'white-space:normal;overflow-wrap:anywhere;word-break:break-word">' +
        '<span style="display:block;color:' + c + ';font-weight:600;margin-bottom:2px">' + esc(heading) + '</span>' +
        rows + '</span></span>';
    }

    function dashboardLink(h) {
      var isHosted = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
      // Open hosted spokes via the hub's SSO handoff endpoint so the user's hub
      // login follows them to the spoke (no second GitHub login), including for
      // direct-route/firewalled spokes that the hub nginx can't front. The
      // endpoint mints a signed, single-hive token and 302s to the spoke's /sso;
      // if SSO can't be used it falls back to the plain dashboard URL. The
      // visible label still shows the spoke host.
      if (isHosted && h.id) {
        var ssoHref = '/api/saas/hives/' + encodeURIComponent(h.id) + '/open';
        var label = (h.dashboardUrl && !h.dashboardUrl.includes('localhost'))
          ? h.dashboardUrl.replace(/^https?:\/\//, '').substring(0, 30) + '...'
          : esc(h.id) + '.hive...';
        return '<a href="' + ssoHref + '" target="_blank" class="dash-link">' + esc(label) + '</a>';
      }
      if (h.dashboardUrl && !h.dashboardUrl.includes('localhost'))
        return '<a href="' + esc(h.dashboardUrl) + '" target="_blank" class="dash-link">' + esc(h.dashboardUrl.replace(/^https?:\/\//,'').substring(0,30)) + '...</a>';
      return '<span style="color:var(--muted);font-size:0.75rem">—</span>';
    }
    function snapshotLink(h) {
      if (h.snapshotUrl) return '<a href="' + esc(h.snapshotUrl) + '" target="_blank" class="dash-link">↗</a>';
      return '';
    }
    function apiLink(h) {
      var isHosted = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
      var base = '';
      if (h.dashboardUrl && !h.dashboardUrl.includes('localhost')) {
        base = esc(h.dashboardUrl);
      } else if (isHosted) {
        base = 'https://' + esc(h.id) + '.hive.kubestellar.io';
      }
      if (!base) return '';
      return '<a href="' + base + '/api/docs" target="_blank" style="padding:3px 10px;background:rgba(88,166,255,0.15);color:#58a6ff;border:1px solid rgba(88,166,255,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none;white-space:nowrap">API ↗</a>';
    }
    function resolvedBase(h) {
      if (h.dashboardUrl && !h.dashboardUrl.includes('localhost')) return h.dashboardUrl;
      var isH = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
      if (isH) return 'https://' + h.id + '.hive.kubestellar.io';
      return '';
    }

    /* SSO_NAV_ATTR marks an anchor whose href is a DISPLAY url (the friendly
       vanity host) but whose click must actually go through the hub's /open
       handoff. The real navigation target is carried here rather than in href
       because browsers show the raw href in the status bar on hover, and the
       placeholder-id /open path is exactly what we do not want a user to read
       there. Named so the delegated click listener below and the builder agree
       on one string. */
    var SSO_NAV_ATTR = 'data-sso-open';

    /* ssoDisplayLink builds the hive-name anchor.

       href      = displayUrl (vanity) when we have one, so the status bar, the
                   context menu's "Copy link address" and middle-click all show
                   and use the friendly host.
       data attr = openUrl, the hub /open endpoint that mints the 90s HMAC SSO
                   handoff token. A plain left-click is intercepted and sent
                   there instead, so the normal path still lands authenticated.

       The cmd-click / middle-click divergence is DELIBERATE and safe: the spoke
       serves a real "Sign in with GitHub" device-flow page at its root, so a
       token-less arrival on the vanity host is an ordinary login, not a dead
       end. Trading a rare extra sign-in for a readable URL on every hover is
       the intended bargain.

       When displayUrl is empty (unclaimed placeholder, or vanity adoption
       skipped) there is nothing friendlier to show, so href stays the /open
       url and behavior is byte-for-byte what it was before. */
    function ssoDisplayLink(openUrl, displayUrl, text, cls) {
      var shown = displayUrl || openUrl;
      var navAttr = displayUrl ? ' ' + SSO_NAV_ATTR + '="' + esc(openUrl) + '"' : '';
      return '<a href="' + esc(shown) + '" target="_blank"' + navAttr +
        ' class="' + cls + '" title="Open dashboard">' + esc(text) + '</a>';
    }

    /* One delegated listener rather than per-row handlers: the table is
       re-rendered wholesale on every refresh, so inline handlers would be
       re-attached constantly. Only a plain primary-button click is taken over;
       cmd/ctrl/shift/alt-click and middle-click fall through to the browser so
       "open in new tab" keeps its native meaning (landing on the vanity login
       page, per the note above). */
    document.addEventListener('click', function(ev) {
      var a = ev.target && ev.target.closest ? ev.target.closest('[' + SSO_NAV_ATTR + ']') : null;
      if (!a) return;
      if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
      var target = a.getAttribute(SSO_NAV_ATTR);
      if (!target) return;
      ev.preventDefault();
      window.open(target, '_blank');
    });

    // Show or hide the sticky "Viewing as … read-only" banner and nudge the page
    // down so the fixed banner never covers the header. Reads the impersonation
    // fields folded into /api/auth/user.
    function applyImpersonationBanner(data) {
      var banner = document.getElementById('impersonation-banner');
      if (!banner) return;
      if (data && data.impersonating) {
        document.getElementById('impersonation-banner-text').textContent =
          'Viewing as ' + (data.viewing_as || 'user') + ' — read-only';
        banner.style.display = '';
        document.body.style.paddingTop = banner.offsetHeight + 'px';
      } else {
        banner.style.display = 'none';
        document.body.style.paddingTop = '';
      }
    }

    // exitImpersonation ends the current "View as" session and reloads so every
    // view re-renders as the real admin again.
    async function exitImpersonation() {
      try { await fetch('/api/saas/admin/impersonate/exit', {method:'POST'}); } catch(e) {}
      location.reload();
    }

    // startImpersonation enters read-only "View as <username>" and reloads so
    // all per-user views re-render as that user. Admin-only (the server refuses
    // any non-admin caller with 403).
    async function startImpersonation(username) {
      try {
        var resp = await fetch('/api/saas/admin/impersonate/' + encodeURIComponent(username), {method:'POST'});
        if (!resp.ok) {
          var e = {}; try { e = await resp.json(); } catch(_) {}
          hiveToast('Cannot view as ' + username + ': ' + (e.error || resp.status), 'error');
          return;
        }
        location.reload();
      } catch(err) {
        hiveToast('Cannot view as ' + username, 'error');
      }
    }

    async function loadUser() {
      try {
        var resp = await fetch('/api/auth/user');
        var data = await resp.json();
        applyImpersonationBanner(data);
        if (data.authenticated) {
          _isAdmin = !!data.hub_admin;
          /* The signed-in login is the ONE piece of viewer identity the hive
             rows need: the inline access avatars omit the viewer, whose own
             membership is implied by the row being visible at all. Stored
             lowercased because GitHub usernames are case-insensitive and the
             roster and the auth payload can disagree on casing. */
          _currentUser = String(data.login || '').toLowerCase();
          /* The viewer's own country, mirrored so the editor opens showing what
             is on file. Absent from the payload for a user who has none, which
             normalizes to '' and renders the empty ＋ control. */
          _myCountry = normalizeCountryCode(data.country);
          var roleText = data.hub_admin ? 'Hub Admin' : 'User';
          /* The viewer's own face links to their own profile, like every other
             face in the dashboard. avatar_url comes from the auth payload (it is
             GitHub's CDN URL, not derivable from the login), so this one builds
             its <img> directly rather than via avatarImg — but the anchor, the
             role tooltip, and the initials fallback on load failure are shared.
             NAV_AVATAR_PX mirrors the .nav-avatar CSS rule's fixed 28px size, so
             the fallback SVG matches the box it is replacing. */
          document.getElementById('nav-user').innerHTML =
            avatarProfileLink(data.login, String(data.login || '') + ' — ' + roleText,
              '<img src="' + escAttr(data.avatar_url) + '" class="nav-avatar" alt="" ' +
              'onerror="this.onerror=null;this.src=' + jsArg(avatarInitialsSVG(data.login, NAV_AVATAR_PX)) + '">') +
            /* The viewer's country, immediately after their face — and, unlike
               every other flag in the dashboard, CLICKABLE, because this is the
               viewer's own record. Wrapped in a stable #nav-country host so a
               save can repaint just this control without re-rendering the whole
               nav (and without a page reload).

               countryNavHTML also renders in the EMPTY state, as a muted ＋.
               countryFlagHTML returns '' there, which is right for someone
               else's flag but would hide the control from precisely the users
               who have no country and need to set one. */
            '<span id="nav-country">' + countryNavHTML(data.country) + '</span>' +
            '<span style="font-size:0.85rem">' + esc(data.login) + '</span>' +
            '<span style="font-size:0.65rem;color:var(--muted);margin-left:6px">' + roleText + '</span>';
        }
      } catch(e) {}
    }

    var _userQuota = 0, _userUsed = 0, _isAdmin = false;
    /* Lowercased login of the signed-in viewer, set by loadUser(). Empty until
       that resolves, and empty is safe: hiveAccessAvatars() then simply omits
       nobody, which is a cosmetic over-render for one paint rather than a
       leak — every name it can show was already in h.access. */
    var _currentUser = '';
    var _latestSHA = '';
    var _stableV4SHA = '';
    var _latestSHAs = {};
    var _latestSHAMessages = {};
    var _latestImageStatus = {};
    /* branch -> unix-millis when the currently-building image started building,
       from the hub (getImageBuildStartTimes). Drives the elapsed build timer. */
    var _latestBuildStarted = {};

    /* Format elapsed build time from an epoch-ms start as "Mm Ss" (or "Ss" under
       a minute). Clamps negatives (clock skew) to 0. */
    function fmtBuildElapsed(startMs) {
      var s = Math.max(0, Math.floor((Date.now() - Number(startMs)) / 1000));
      var m = Math.floor(s / 60);
      var rem = s % 60;
      return m > 0 ? (m + 'm ' + rem + 's') : (rem + 's');
    }

    /* Live-update every rendered build timer once a second, in place, so the
       count climbs without re-rendering the whole image panel or hitting the
       hub. Runs off a single shared interval started once below. */
    function _tickBuildTimers() {
      var els = document.querySelectorAll('.build-timer');
      for (var i = 0; i < els.length; i++) {
        var start = els[i].getAttribute('data-build-start');
        if (start) els[i].textContent = fmtBuildElapsed(start);
      }
    }
    if (typeof window !== 'undefined' && !window._buildTimerInterval) {
      window._buildTimerInterval = setInterval(_tickBuildTimers, 1000);
    }
    var _trackedBranchesList = [];
    /* Release channels: the ordered channel names, and the server-resolved
       channel -> image association (see release_channels.go). _channelTargets
       entries are {channel, branch, sha, digest}; branch/sha are EMPTY when the
       channel points at a digest that is not any tracked branch's latest, and
       the UI must render that honestly rather than guessing a branch. */
    var _releaseChannels = [];
    var _channelTargets = [];
    /* Width of the channel-name pill in the "channel -> image" block. Fixed so
       the three arrows line up in a column; sized for the longest channel name
       ("candidate") at 0.6rem. */
    var CHANNEL_NAME_MIN_W_PX = 62;

    /* A registry digest is "sha256:<64 hex>" — far too long for a table cell.
       Show the same 7 hex chars a short git SHA uses, keeping the full digest
       in the title so it is still copyable/verifiable. */
    function shortDigest(d) {
      if (!d) return '';
      var hex = d.indexOf(':') >= 0 ? d.slice(d.indexOf(':') + 1) : d;
      return hex.length > 7 ? hex.slice(0, 7) : hex;
    }

    /* Version label for the My Hives version pill and the picker's CHANNELS
       entries. A release channel is a moving tag, so its bare name ("stable")
       tells the operator nothing about what code the hive is actually
       tracking; append the branch the channel CURRENTLY resolves to —
       "stable (v4)". The association comes from _channelTargets, the server's
       digest-derived mapping (release_channels.go matches each channel tag's
       registry digest against the tracked branches' "<branch>-latest"
       digests on the hub's poll cadence) — deliberately NOT a hardcoded
       channel->branch table, so when CI re-points a channel the label follows
       with no hub code change. Never guesses: a channel resolved to a digest
       no tracked branch owns shows the short digest instead, and a channel
       with no resolution yet shows "(?)". Branch names pass through
       untouched. DISPLAY-ONLY: every selection and comparison still uses the
       bare channel name — only the rendered text changes. */
    function versionLabel(v) {
      for (var i = 0; i < _channelTargets.length; i++) {
        var t = _channelTargets[i];
        if (t && t.channel === v) {
          if (t.branch) return v + ' (' + t.branch + ')';
          if (t.digest) return v + ' (' + shortDigest(t.digest) + ')';
          return v + ' (?)';
        }
      }
      /* A channel the payload names but the resolver has no row for (e.g. a
         cold channel cache) — still mark it as unresolved, never bare. */
      if (_releaseChannels.indexOf(v) >= 0) return v + ' (?)';
      return v;
    }
    var _clusterList = [];
    var _commitMessages = {};
    var _allDashHives = [];
    /* Fleet-average quadrant for the CURRENT view, served alongside the rows.
       Null until the first payload lands, which quadrantSVG handles by drawing
       no reference polygon rather than an empty one collapsed at the centre. */
    var _fleetQuadrant = null;
    /* Seeded to the A-Z default rather than '' (registry/arrival order) so the
       very first paint — including paintCachedHives, which runs before any
       network call — is already alphabetical. loadHiveSortPrefs overwrites both
       from localStorage when the operator has a stored choice; see
       HIVE_SORT_DEFAULT_KEY for why a stored choice wins over this default. */
    var _dashSortKey = 'name', _dashSortAsc = true;
    var _hivesLoading = false;
    var _lastHivesJSON = '';
    var _lastUsersJSON = '';

    /* ── Instant-paint cache for the hive list ──
       A returning operator used to stare at "Loading your hives..." for the
       whole round-trip. We persist the last good list and paint it on load,
       then reconcile when the fresh payload lands.

       Bounded and versioned on purpose:
       - HIVES_CACHE_TTL_MS keeps a stale fleet from being presented as current.
       - HIVES_CACHE_VERSION is bumped whenever the row shape changes, so an
         old-shaped cache can never be rendered by new code (that would
         reintroduce exactly the class of render crash this fixes).
       Reads NEVER throw: storage can be disabled, full, or hold junk, and none
       of that may block the network path. */
    var LS_HIVES_CACHE = 'hive-my-hives-cache';
    /* Bump on ANY change to the hive row shape consumed by renderHives().
       v2: rows carry trackedChannel (the hub-persisted release-channel
       selection the version pill renders); a v1 cache would repaint a
       channel-pinned hive as its bare branch for the pre-network paint.
       v3 (#4041): agent rows carry pause provenance (pausedTrigger/
       pausedReason/pausedBy/pausedAt) rendered into the Agents tooltip; a
       v2 cache would paint paused agents provenance-less until the poll.
       v4: rows carry quadrant, and the cache carries the fleet average that
       every kite is drawn against; a v3 cache would paint the new column empty
       until the poll landed.
       v5: rows carry the fleet-divergence view (fleetRollup + agentVerdicts:
       expected/actual/able per agent); a v4 cache would omit the new per-agent
       drill-down until the first poll landed. */
    var HIVES_CACHE_VERSION = 5;
    /* 10 minutes: long enough to cover a reload or a tab restore, short enough
       that a cached fleet is never wildly out of date before the poll lands. */
    var HIVES_CACHE_TTL_MS = 10 * 60 * 1000;
    /* Cap what we persist. Only the fields renderHives() needs for the first
       paint are stored, but a huge fleet could still overflow the ~5 MB quota
       and make every write fail. */
    var HIVES_CACHE_MAX_ROWS = 200;

    function readHivesCache() {
      try {
        var raw = window.localStorage.getItem(LS_HIVES_CACHE);
        if (!raw) return null;
        var c = JSON.parse(raw);
        if (!c || c.version !== HIVES_CACHE_VERSION) return null;
        if (!c.savedAt || (Date.now() - c.savedAt) > HIVES_CACHE_TTL_MS) return null;
        if (!Array.isArray(c.hives) || !c.hives.length) return null;
        return c;
      } catch (e) {
        /* Disabled/!full/corrupt storage must never break the load path. */
        console.warn('[hive] hive cache read failed, using network only:', e);
        return null;
      }
    }

    function writeHivesCache(hives) {
      try {
        var rows = (hives || []).slice(0, HIVES_CACHE_MAX_ROWS);
        window.localStorage.setItem(LS_HIVES_CACHE, JSON.stringify({
          version: HIVES_CACHE_VERSION,
          savedAt: Date.now(),
          hives: rows,
          /* Cached WITH the rows: the reference polygon is a property of the
             population those rows came from, so pairing them keeps a cached
             paint self-consistent even though the rows are truncated. */
          fleetQuadrant: _fleetQuadrant
        }));
      } catch (e) {
        /* Quota errors are expected on large fleets — caching is best-effort. */
        console.warn('[hive] hive cache write failed:', e);
      }
    }

    /* Paint the cached fleet before the first request returns. Guarded so any
       failure (including a stale-shaped row slipping past the version check)
       falls back to the normal spinner + network path rather than killing
       init(). */
    function paintCachedHives() {
      var c = readHivesCache();
      if (!c) return false;
      try {
        _allDashHives = c.hives;
        _hiveRegistry = c.hives;
        _fleetQuadrant = c.fleetQuadrant || null;
        renderHives(sortedDashHives(), true);
        return true;
      } catch (e) {
        console.error('[hive] cached render failed, falling back to network:', e);
        _allDashHives = [];
        _hiveRegistry = [];
        _lastHivesJSON = '';
        var container = document.getElementById('hives-container');
        if (container) container.innerHTML = '<div class="loading">Loading your hives...</div>';
        return false;
      }
    }

    /* Active status filters, keyed by HIVE_FILTER_*. Multi-select: several may
       be on at once and a hive matches if it satisfies ANY active one (OR), so
       turning on more chips widens the list rather than narrowing it to nothing
       — "Degraded" and "OK" are mutually exclusive and an AND would always be
       empty. Empty object = no filtering, show everything. */
    var _dashStatusFilters = {};

    /* Chip definitions, in display order. Colours reuse the health palette from
       healthBadge() so a chip reads as the same state as the row dot. */
    var HIVE_FILTER_CHIPS = [
      {key: HIVE_FILTER_APP_MISSING, label: 'GitHub App not installed', color: '#f85149'},
      {key: HIVE_FILTER_NO_TOKENS, label: 'No tokens used', color: '#6b7280'},
      {key: HIVE_FILTER_DEGRADED, label: 'Degraded', color: '#f85149'},
      {key: HIVE_FILTER_OK, label: 'OK', color: '#3fb950'}
    ];

    /* Active failing-check-name filter: '' = off, otherwise a check name that a
       hive must be FAILING for it to be shown. This is an AND against the status
       chips (chips OR among themselves), because "degraded hives" and
       "github_auth is failing" are two different questions and the useful answer
       is their intersection. */
    var _dashFailingCheckFilter = '';

    /* Max distinct failing check names offered in the picker. Names come from
       spokes, so an unbounded fleet could otherwise produce a huge menu. */
    var MAX_FAILING_CHECK_FILTER_OPTIONS = 12;

    /* If a single check is failing on at least this many hives, it is called out
       as a fleet-wide signal rather than reading as per-hive noise. */
    var FLEET_CHECK_SIGNAL_MIN_HIVES = 2;

    /* failingCheckCounts tallies, over the hives given, how many have each check
       in a failing state — {name: hiveCount}. Callers pass the ASSIGNED set so
       placeholders never contribute. */
    function failingCheckCounts(hives) {
      var counts = {};
      (hives || []).forEach(function(h) {
        var seen = {};
        failingChecks(h).forEach(function(ck) {
          if (seen[ck.name]) return;   /* count each hive once per check name */
          seen[ck.name] = true;
          counts[ck.name] = (counts[ck.name] || 0) + 1;
        });
      });
      return counts;
    }

    /* setFailingCheckFilter selects (or clears, with '') the check-name filter. */
    function setFailingCheckFilter(name) {
      _dashFailingCheckFilter = name || '';
      renderHives(_allDashHives, true);
    }

    /* The drift-kind currently selected in the fleet-exceptions summary, or ''
       for none. Single-select (unlike the multi-select status chips): the
       summary's purpose is "show me the hives with THIS problem", and an
       OR across several problem types is what the status chips already do.
       It composes with the status chips by AND — the chips answer "which
       states", this answers "which specific misconfiguration". */
    var _dashDriftFilter = '';

    /* Search text stashed away while a drift pill is selected, or null when no
       stash is held. Clicking a pill clears the search box (a stale search
       silently intersecting with the pill filter reads as hives having
       disappeared) but remembers what was typed; deselecting the last pill
       restores it. Typing WHILE a pill is active drops the stash — the new
       text is what the user wants and must survive deselection. */
    var _driftSearchStash = null;

    /* hiveMatchesDriftFilter answers whether a hive carries the selected drift
       kind. Guarded so a row with no drift report never throws. */
    function hiveMatchesDriftFilter(h) {
      if (!_dashDriftFilter) return true;
      var signals = driftOf(h).signals;
      for (var i = 0; i < signals.length; i++) {
        if (signals[i] && signals[i].kind === _dashDriftFilter) return true;
      }
      return false;
    }

    /* ── Upgrade-state filter ──
       Which upgrade state the list is narrowed to, or '' for none. Single-
       select like the drift filter, not multi-select: "upgrading" and "queued"
       are successive stages of one lifecycle, so a hive is in exactly one of
       them and an OR across both would just mean "any pending upgrade" — which
       is what clearing the filter already shows.

       This exists so an operator can WATCH an upgrade roll across the fleet,
       and — the reason it was asked for — pull up every wedged hive at once
       when a rollout stalls. */
    var UPGRADE_FILTER_UPGRADING = 'upgrading';
    var UPGRADE_FILTER_QUEUED = 'queued';
    var _dashUpgradeFilter = '';

    /* hiveUpgradeState classifies a hive as 'upgrading', 'queued' or '' (in
       neither state).

       The definitions mirror the Version cell's render conditions on purpose,
       so the facet count and the rows can never disagree:
         upgrading — the hub says an upgrade is in flight (h.upgrading).
         queued    — auto-upgrade is on and the hive is BEHIND latest, but the
                     hub has not instructed the upgrade yet. This is the state
                     with no upgradeStartedAt, which is precisely why the
                     elapsed counter must not claim to know one.
       'upgrading' is tested FIRST because an in-flight upgrade outranks a
       queued one; without that ordering a hive would be double-counted. */
    /* hiveIsUpgradingNow is THE predicate for "this hive's row shows the
       Upgrading/Switching spinner". Both the row badge and the filter pill call
       it, so the pill can never disagree with what the operator sees in the
       list.

       It previously did not exist: the pill tested only h.upgrading while the
       row OR'd together three sources, so the pill under-reported. A hive
       mid-branch-switch, or one carrying a just-clicked client-side sentinel,
       rendered a spinner the pill could not match; and the row's own
       "isCurrent -> not really upgrading" suppression was invisible to the
       pill, which counted such a hive as upgrading with no badge to show for
       it. Three sources, one of them purely client-side, mean the answer cannot
       be derived from the hub JSON alone — hence a shared function rather than
       a shared field.

       branchName / branchLatest are the row's already-resolved values; when the
       caller has not resolved them (the facet counter iterating raw hives) they
       are recomputed here from the same globals the row uses, so both paths
       reach the identical answer. */
    function hiveIsUpgradingNow(h, branchName, branchLatest) {
      if (!h) return false;
      if (branchName === undefined || branchName === null || branchName === '') {
        branchName = h.gitBranch || 'v2';
      }
      if (branchLatest === undefined || branchLatest === null) {
        branchLatest = _latestSHAs[branchName] || _latestSHA || '';
      }
      /* A branch switch is an upgrade in flight even though h.upgrading may
         still be false — the spoke keeps reporting the OLD branch until the new
         pod heartbeats. */
      if (hiveSwitchState(h, branchName).isSwitching) return true;
      /* Client-side sentinel: the operator clicked Upgrade and the hive still
         reports the pre-click SHA. The hub has not yet flipped h.upgrading, but
         the row is already spinning. */
      var sentinel = _upgradingHives[h.id];
      var sha = h.gitHash || '';
      if (sentinel && sha === sentinel) return true;
      /* Hub-reported. Suppressed when latest is unresolved, because the row
         suppresses the spinner then too and the pill must match.

         NOT suppressed merely because the hive reads as "current". The fleet
         runs floating TAGS (v4-latest) while the hub tracks progress by git
         SHA, so 'isCurrent' is a SHA comparison against the branch tip and a
         spoke can sit behind the tip for many minutes with its tag unchanged
         and zero Kubernetes-visible drift. Suppressing on isCurrent was only
         ever a client-side patch over the stale server latch that the
         stale_upgrade.go convergence sweep now clears at the source; keeping it
         here would re-hide genuinely in-flight upgrades. */
      var latestUnknown = !branchLatest;
      if (latestUnknown) return false;
      if (h.upgrading) return true;
      /* Behind the branch tip with a target ALREADY armed by the hub. The hub
         has instructed this SHA and is waiting for the spoke to report it, so
         the rollout is in flight even before 'upgrading' latches — this is the
         window that made the pill under-report against a moving tag. A hive
         that is behind with NO armed target is not upgrading; it is "queued"
         (auto-upgrade on) or simply out of date, and both are other states. */
      var isCurrent = sameShaJS(sha, branchLatest);
      if (!isCurrent && h.upgradeTarget && !h.autoUpgrade &&
          !sameShaJS(sha, h.upgradeTarget)) {
        return true;
      }
      return false;
    }

    /* normalizeUpgradeState expires the client-side sentinels for ONE hive.
       It is the only writer of _upgradingHives during a render.

       This has to run over the whole list BEFORE anything filters, counts or
       draws, because _upgradingHives is global mutable state that
       hiveIsUpgradingNow reads. While expiry lived inside the row loop, the
       facet counter and applyDashFilters — both of which run first, over every
       hive — saw a different map than the rows did, so the pill and the badge
       could disagree even though they now call the same predicate. Sharing a
       function only guarantees agreement if it is also given the same inputs.

       Returns nothing; callers re-read _upgradingHives through the predicate. */
    function normalizeUpgradeState(h) {
      if (!h) return;
      var branchName = h.gitBranch || 'v2';
      var st = hiveSwitchState(h, branchName);
      /* A switch sentinel that no longer resolves to a switch (the target
         became a same-branch SHA) is stale and must not force upgrading. */
      if (st.switchSentinelStale) {
        delete _upgradingHives[h.id];
        delete _switchStartedAt[h.id];
        return;
      }
      if (st.isSwitching) return;
      /* The hive has moved off the SHA it carried when Upgrade was clicked, so
         the click has landed and the sentinel has done its job. */
      var sentinel = _upgradingHives[h.id];
      if (sentinel && (h.gitHash || '') !== sentinel) {
        delete _upgradingHives[h.id];
        delete _switchStartedAt[h.id];
      }
    }

    /* normalizeUpgradeStates applies the above across the fleet. Called at the
       top of renderHives, ahead of every filter, facet count and row. */
    function normalizeUpgradeStates(hives) {
      for (var i = 0; i < (hives || []).length; i++) normalizeUpgradeState(hives[i]);
    }

    function hiveUpgradeState(h) {
      if (!h) return '';
      if (hiveIsUpgradingNow(h)) return UPGRADE_FILTER_UPGRADING;
      if (h.autoUpgrade && h.upgradeTarget) return UPGRADE_FILTER_QUEUED;
      return '';
    }

    /* hiveMatchesUpgradeFilter answers whether a hive survives the upgrade-
       state filter. Composes by AND with the status chips and facets. */
    function hiveMatchesUpgradeFilter(h) {
      if (!_dashUpgradeFilter) return true;
      return hiveUpgradeState(h) === _dashUpgradeFilter;
    }

    /* ── Stale-advisory filter ──
       When true, the list is narrowed to hives whose advisory digest the hub
       has flagged stale. A single boolean, not a value set: the underlying
       signal is itself boolean, so there is no second value to OR against.
       Composes by AND with the status chips and the facets, like the drift
       filter above. */
    var _dashAdvisoryStaleFilter = false;

    /* hiveMatchesAdvisoryStaleFilter answers whether a hive survives the
       stale-advisory filter. Reads the SERVER-computed h.advisoryStale flag —
       the browser must never re-derive the threshold or the advisory-mode /
       app-can-write gating, or the filter and the row pill would drift apart
       (see advisoryStale() in advisory_staleness.go). */
    function hiveMatchesAdvisoryStaleFilter(h) {
      if (!_dashAdvisoryStaleFilter) return true;
      return !!(h && h.advisoryStale);
    }

    /* ────────────────────────────────────────────────────────────────────────
       Grouping and saved views for My Hives.

       Grouping applies WITHIN the existing Assigned/Unassigned split — that
       split is the outer structure and survives untouched. A group-by turns
       each side into several labelled, collapsible subsections.
       ──────────────────────────────────────────────────────────────────────── */

    /* Group-by dimension keys. Named constants so the <select>, the persisted
       value, the grouper and the collapse-state keys can never drift on a
       typo'd string literal. */
    var HIVE_GROUP_NONE = 'none';
    var HIVE_GROUP_CLUSTER = 'cluster';
    var HIVE_GROUP_ORG = 'org';
    var HIVE_GROUP_OWNER = 'owner';
    var HIVE_GROUP_ACMM = 'acmm';
    var HIVE_GROUP_BRANCH = 'branch';
    var HIVE_GROUP_UPGRADE_STATE = 'upgradeState';

    /* Label shown for a hive whose grouping field is empty/unreported. Kept as
       one constant so every dimension buckets blanks identically. */
    var HIVE_GROUP_UNKNOWN_LABEL = 'Unspecified';

    /* localStorage keys. Prefix matches the existing convention in this file
       ('hive-dismissed-banners', 'hive-cluster-health-collapsed'). */
    var LS_HIVE_GROUP_BY = 'hive-group-by';
    var LS_HIVE_GROUP_COLLAPSED = 'hive-group-collapsed';
    var LS_HIVE_SAVED_VIEWS = 'hive-saved-views';
    var LS_HIVE_DEFAULT_VIEW = 'hive-default-view';
    var LS_HIVE_SORT = 'hive-sort';
    var LS_HIVE_SCROLL = 'hive-scroll';

    /* HIVE_SORT_DEFAULT_KEY is the sort a FIRST-TIME visitor gets: the Hive
       column, ascending — i.e. plain A-Z over the label the name cell actually
       displays (see hiveNameSortValue / hiveLabel). Before this the table
       rendered in registry order, which is arrival order and looks arbitrary.

       This is a DEFAULT, not an override: loadHiveSortPrefs applies it only
       when there is no usable stored choice, so an operator who sorted by
       Tokens still returns to Tokens. */
    var HIVE_SORT_DEFAULT_KEY = 'name';
    var HIVE_SORT_DEFAULT_ASC = true;

    /* HIVE_SORT_KEYS is every key a header can sort by — the allowlist that a
       persisted value is validated against. A stored key that is no longer a
       column (a renamed field, a hand-edited value, a downgrade) degrades to
       the A-Z default instead of silently sorting on an absent property, which
       reads as "sorting is broken" because every row compares equal. */
    var HIVE_SORT_KEYS = ['name', 'clusterId', 'startedAt', 'acmmLevel', 'aiAuthor',
      'journey', 'agentCount', 'totalTokens24h', 'governorMode', 'actionableIssues',
      'actionablePRs', 'activeContributors', 'registeredAt'];

    function isHiveSortKey(key) {
      for (var i = 0; i < (HIVE_SORT_KEYS || []).length; i++) {
        if (HIVE_SORT_KEYS[i] === key) return true;
      }
      return false;
    }

    /* Cap on stored saved views. localStorage is a small shared budget and an
       unbounded list would let one runaway page fill it for every other
       feature on this origin. */
    var HIVE_SAVED_VIEWS_MAX = 50;

    /* Cap on a saved-view name, so the picker stays readable and a pasted wall
       of text cannot blow the storage budget on its own. */
    var HIVE_VIEW_NAME_MAX_LEN = 60;

    /* Group-by dimension definitions, in <select> order.
       - key:   stable identifier, persisted
       - label: shown in the control
       - of(h): the group label for a hive; '' means "unspecified"
       - sort:  optional comparator over group labels; default is locale sort */
    var HIVE_GROUP_DIMENSIONS = [
      {key: HIVE_GROUP_NONE, label: 'No grouping', of: function() { return ''; }},
      {key: HIVE_GROUP_CLUSTER, label: 'Cluster', of: function(h) { return (h && (h.clusterName || h.clusterId)) || ''; }},
      {key: HIVE_GROUP_ORG, label: 'Org', of: function(h) { return (h && h.org) || ''; }},
      {key: HIVE_GROUP_OWNER, label: 'Owner', of: function(h) { /* ownerName resolves opaque OIDC ids ("ibmid:…") to display names; grouping by the label keeps headers human while row data stays raw. */ return (h && (h.ownerName || h.owner)) || ''; }},
      {key: HIVE_GROUP_ACMM, label: 'ACMM level', of: function(h) {
        /* acmmLevel is numeric; render as "Level N" so the header reads as a
           label rather than a bare digit. 0/absent falls through to
           HIVE_GROUP_UNKNOWN_LABEL like every other dimension. */
        var lvl = h && h.acmmLevel;
        return lvl ? 'Level ' + lvl : '';
      }, sort: function(a, b) {
        /* Numeric order, so Level 10 sorts after Level 2 rather than before it.
           Non-"Level N" labels (i.e. Unspecified) sort last. */
        var na = _acmmGroupOrder(a), nb = _acmmGroupOrder(b);
        return na - nb;
      }},
      {key: HIVE_GROUP_BRANCH, label: 'Branch', of: function(h) { return (h && h.gitBranch) || ''; }},
      {key: HIVE_GROUP_UPGRADE_STATE, label: 'Upgrade state', of: function(h) {
        /* Reuse hiveUpgradeState(h) — the SAME predicate the row's Upgrading
           pill and the upgrade-state filter facet use (it routes through
           hiveIsUpgradingNow) — so the group a hive lands in can never disagree
           with the badge the operator already sees. Do NOT re-derive the states
           here or the grouping would drift from the pill.

           hiveUpgradeState returns:
             'upgrading' — a rollout is in flight (h.upgrading / armed target /
                           branch switch / just-clicked sentinel).
             'queued'    — behind latest, auto-upgrade ON, hub has not instructed
                           the rollout yet (ready, not yet triggered). This is
                           the operator's target bucket.
             ''          — neither: up to date, or behind with auto-upgrade off
                           and no pending action.
           Map each to a human-readable header. '' is rendered as its own
           "Up to date" group rather than falling through to Unspecified, since
           for this dimension "no pending upgrade" is a meaningful, expected
           bucket rather than missing data. */
        var st = hiveUpgradeState(h);
        if (st === UPGRADE_FILTER_QUEUED) return HIVE_UPGRADE_GROUP_QUEUED;
        if (st === UPGRADE_FILTER_UPGRADING) return HIVE_UPGRADE_GROUP_UPGRADING;
        return HIVE_UPGRADE_GROUP_UPTODATE;
      }, sort: function(a, b) {
        /* Queued first — it is why this dimension exists (systems READY for an
           upgrade that have not triggered) — then Upgrading (in flight), then
           Up to date. Any unexpected label sorts last. */
        return _upgradeGroupOrder(a) - _upgradeGroupOrder(b);
      }}
    ];

    /* Header labels for the Upgrade-state dimension. Named constants so the
       of() grouper and the sort comparator can never drift on a string typo.
       "Queued for auto-upgrade" matches the row badge and the facet pill —
       three surfaces, one wording. */
    var HIVE_UPGRADE_GROUP_QUEUED = 'Queued for auto-upgrade (not yet upgrading)';
    var HIVE_UPGRADE_GROUP_UPGRADING = 'Upgrading';
    var HIVE_UPGRADE_GROUP_UPTODATE = 'Up to date';

    /* Sort weight for an upgrade-state group header. Queued (the actionable
       "ready but not triggered" bucket) sorts first, then Upgrading, then Up to
       date; anything else sorts last. */
    var UPGRADE_GROUP_UNKNOWN_ORDER = Number.MAX_SAFE_INTEGER;
    function _upgradeGroupOrder(label) {
      if (label === HIVE_UPGRADE_GROUP_QUEUED) return 0;
      if (label === HIVE_UPGRADE_GROUP_UPGRADING) return 1;
      if (label === HIVE_UPGRADE_GROUP_UPTODATE) return 2;
      return UPGRADE_GROUP_UNKNOWN_ORDER;
    }

    /* Sort weight for an ACMM group label. "Level N" → N; anything else (the
       Unspecified bucket) sorts after every real level. */
    var ACMM_GROUP_UNKNOWN_ORDER = Number.MAX_SAFE_INTEGER;
    function _acmmGroupOrder(label) {
      var m = /^Level (\d+)$/.exec(label || '');
      return m ? parseInt(m[1], 10) : ACMM_GROUP_UNKNOWN_ORDER;
    }

    /* Active group-by key, and per-group collapse state as {groupId: true}
       where true means COLLAPSED (absent = expanded, so a fresh browser shows
       everything open). */
    var _dashGroupBy = HIVE_GROUP_NONE;
    var _dashGroupCollapsed = {};

    /* Saved views: [{name, groupBy, filters, sortKey, sortAsc}]. Client-side
       only — a view is a personal lens over data the page already has, so it
       needs no server round-trip and no server state to go stale. */
    var _dashSavedViews = [];
    var _dashDefaultView = '';

    /* lsGetJSON reads and parses a JSON value from localStorage. localStorage
       can hold anything a previous version (or another tab, or a user poking
       at devtools) left behind, and can be disabled or full outright — so every
       failure falls back to the caller's default rather than throwing and
       blanking the page. */
    function lsGetJSON(key, fallback) {
      try {
        var raw = localStorage.getItem(key);
        if (!raw) return fallback;
        var v = JSON.parse(raw);
        return (v === null || v === undefined) ? fallback : v;
      } catch (e) { return fallback; }
    }

    /* lsSetJSON persists a JSON value, swallowing quota/disabled errors: losing
       a preference is not worth breaking the render. */
    function lsSetJSON(key, value) {
      try { localStorage.setItem(key, JSON.stringify(value)); } catch (e) { /* storage full or disabled */ }
    }

    /* isGroupByKey validates a persisted/incoming group-by against the known
       dimensions, so a stale or hand-edited value degrades to "no grouping"
       instead of silently grouping by nothing. */
    function isGroupByKey(key) {
      for (var i = 0; i < (HIVE_GROUP_DIMENSIONS || []).length; i++) {
        if (HIVE_GROUP_DIMENSIONS[i].key === key) return true;
      }
      return false;
    }

    /* groupDimension returns the dimension definition for a key, or the
       "none" entry when unknown. */
    function groupDimension(key) {
      for (var i = 0; i < (HIVE_GROUP_DIMENSIONS || []).length; i++) {
        if (HIVE_GROUP_DIMENSIONS[i].key === key) return HIVE_GROUP_DIMENSIONS[i];
      }
      return HIVE_GROUP_DIMENSIONS[0];
    }

    /* sanitizeSavedView coerces one stored entry into a well-formed view, or
       returns null if it is unusable. Everything in localStorage is untrusted
       input. */
    function sanitizeSavedView(v) {
      if (!v || typeof v !== 'object') return null;
      var name = typeof v.name === 'string' ? v.name.trim() : '';
      if (!name) return null;
      var filters = {};
      if (v.filters && typeof v.filters === 'object') {
        for (var k in v.filters) {
          if (Object.prototype.hasOwnProperty.call(v.filters, k) && v.filters[k]) filters[k] = true;
        }
      }
      return {
        name: name.slice(0, HIVE_VIEW_NAME_MAX_LEN),
        groupBy: isGroupByKey(v.groupBy) ? v.groupBy : HIVE_GROUP_NONE,
        filters: filters,
        sortKey: typeof v.sortKey === 'string' ? v.sortKey : '',
        sortAsc: v.sortAsc !== false
      };
    }

    /* loadDashViewPrefs restores group-by, collapse state, saved views and the
       default view from localStorage. Called once before the first render. */
    function loadDashViewPrefs() {
      var gb = null;
      try { gb = localStorage.getItem(LS_HIVE_GROUP_BY); } catch (e) { gb = null; }
      _dashGroupBy = isGroupByKey(gb) ? gb : HIVE_GROUP_NONE;

      var collapsed = lsGetJSON(LS_HIVE_GROUP_COLLAPSED, {});
      _dashGroupCollapsed = {};
      if (collapsed && typeof collapsed === 'object') {
        for (var ck in collapsed) {
          if (Object.prototype.hasOwnProperty.call(collapsed, ck) && collapsed[ck]) _dashGroupCollapsed[ck] = true;
        }
      }

      var views = lsGetJSON(LS_HIVE_SAVED_VIEWS, []);
      _dashSavedViews = [];
      if (Object.prototype.toString.call(views) === '[object Array]') {
        for (var vi = 0; vi < views.length && _dashSavedViews.length < HIVE_SAVED_VIEWS_MAX; vi++) {
          var sv = sanitizeSavedView(views[vi]);
          if (sv) _dashSavedViews.push(sv);
        }
      }

      var def = null;
      try { def = localStorage.getItem(LS_HIVE_DEFAULT_VIEW); } catch (e) { def = null; }
      _dashDefaultView = (typeof def === 'string' && findSavedView(def)) ? def : '';
      if (_dashDefaultView) applySavedView(_dashDefaultView, true);
    }

    /* findSavedView looks a view up by name (names are the identity — the
       picker shows them and rename edits them in place). */
    function findSavedView(name) {
      for (var i = 0; i < (_dashSavedViews || []).length; i++) {
        if (_dashSavedViews[i].name === name) return _dashSavedViews[i];
      }
      return null;
    }

    function persistSavedViews() { lsSetJSON(LS_HIVE_SAVED_VIEWS, _dashSavedViews); }
    function persistGroupCollapsed() { lsSetJSON(LS_HIVE_GROUP_COLLAPSED, _dashGroupCollapsed); }

    /* groupIdFor builds the stable identity used both as the collapse-state
       localStorage key and as the DOM id for a group's rows. Scoped by
       dimension and by section (assigned/unassigned) so the same org name in
       two sections collapses independently, and so switching dimensions does
       not inherit an unrelated group's collapse state. */
    function groupIdFor(section, label) {
      return _dashGroupBy + '|' + section + '|' + label;
    }

    /* setDashGroupBy switches the grouping dimension and re-renders. */
    function setDashGroupBy(key) {
      _dashGroupBy = isGroupByKey(key) ? key : HIVE_GROUP_NONE;
      try { localStorage.setItem(LS_HIVE_GROUP_BY, _dashGroupBy); } catch (e) { /* storage full or disabled */ }
      renderHives(_allDashHives, true);
    }

    /* toggleDashGroup flips one group's collapse state. */
    function toggleDashGroup(groupId) {
      if (_dashGroupCollapsed[groupId]) delete _dashGroupCollapsed[groupId];
      else _dashGroupCollapsed[groupId] = true;
      persistGroupCollapsed();
      renderHives(_allDashHives, true);
    }

    /* groupHives buckets hives by the active dimension, preserving the incoming
       order within each bucket (the caller has already applied sortDashHives).
       Returns [{label, id, hives}] with empty buckets impossible by
       construction — a group only exists because a hive landed in it, so no
       empty headers can render. */
    function groupHives(hives, section) {
      var dim = groupDimension(_dashGroupBy);
      var order = [], byLabel = {};
      for (var i = 0; i < (hives || []).length; i++) {
        var h = hives[i];
        var label = dim.of(h) || HIVE_GROUP_UNKNOWN_LABEL;
        if (!Object.prototype.hasOwnProperty.call(byLabel, label)) { byLabel[label] = []; order.push(label); }
        byLabel[label].push(h);
      }
      order.sort(dim.sort || function(a, b) { return String(a).localeCompare(String(b)); });
      var out = [];
      for (var oi = 0; oi < order.length; oi++) {
        out.push({label: order[oi], id: groupIdFor(section, order[oi]), hives: byLabel[order[oi]]});
      }
      return out;
    }

    /* ── Saved views ── */

    /* currentViewState snapshots the parts of the UI a saved view captures.
       Search terms and facets are not on v2 yet; when the search/facets branch
       lands, extend this one function and sanitizeSavedView to match. */
    function currentViewState() {
      var filters = {};
      for (var k in (_dashStatusFilters || {})) {
        if (Object.prototype.hasOwnProperty.call(_dashStatusFilters, k) && _dashStatusFilters[k]) filters[k] = true;
      }
      return {groupBy: _dashGroupBy, filters: filters, sortKey: _dashSortKey, sortAsc: _dashSortAsc};
    }

    /* saveCurrentView names and stores the current group-by + filters + sort.
       Saving over an existing name overwrites it, which is what "save" means
       when the picker already shows that name. */
    async function saveCurrentView() {
      var raw = await hivePrompt('Name this view', '');
      if (raw === null) return;
      var name = String(raw).trim().slice(0, HIVE_VIEW_NAME_MAX_LEN);
      if (!name) { hiveToast('View name cannot be empty', 'error'); return; }
      var state = currentViewState();
      state.name = name;
      var existing = findSavedView(name);
      if (existing) {
        if (!await hiveConfirm('A view named "' + name + '" already exists. Overwrite it?')) return;
        existing.groupBy = state.groupBy;
        existing.filters = state.filters;
        existing.sortKey = state.sortKey;
        existing.sortAsc = state.sortAsc;
      } else {
        if (_dashSavedViews.length >= HIVE_SAVED_VIEWS_MAX) {
          hiveToast('Saved-view limit reached (' + HIVE_SAVED_VIEWS_MAX + '). Delete one first.', 'error');
          return;
        }
        _dashSavedViews.push(state);
      }
      persistSavedViews();
      _dashActiveView = name;
      renderHives(_allDashHives, true);
      hiveToast('View "' + name + '" saved', 'success');
    }

    /* Name of the view most recently applied or saved, for the picker's
       selected option. Not persisted: it is a UI cursor, not a preference. */
    var _dashActiveView = '';

    /* applySavedView restores a stored view. quiet=true suppresses the
       re-render, for the load path where the caller renders once afterwards. */
    function applySavedView(name, quiet) {
      var v = findSavedView(name);
      if (!v) return;
      _dashGroupBy = isGroupByKey(v.groupBy) ? v.groupBy : HIVE_GROUP_NONE;
      _dashStatusFilters = {};
      for (var k in (v.filters || {})) {
        if (Object.prototype.hasOwnProperty.call(v.filters, k) && v.filters[k]) _dashStatusFilters[k] = true;
      }
      _dashSortKey = v.sortKey || '';
      _dashSortAsc = v.sortAsc !== false;
      _dashActiveView = name;
      try { localStorage.setItem(LS_HIVE_GROUP_BY, _dashGroupBy); } catch (e) { /* storage full or disabled */ }
      if (!quiet) renderHives(sortedDashHives(), true);
    }

    /* onSavedViewPick handles the picker's change event: '' means "no view". */
    function onSavedViewPick(name) {
      if (!name) { _dashActiveView = ''; renderHives(_allDashHives, true); return; }
      applySavedView(name);
    }

    /* renameSavedView renames a view in place, keeping the default-view pointer
       in sync so a renamed default stays the default. */
    async function renameSavedView() {
      if (!_dashActiveView) { hiveToast('Select a view to rename', 'error'); return; }
      var v = findSavedView(_dashActiveView);
      if (!v) return;
      var raw = await hivePrompt('Rename view', v.name);
      if (raw === null) return;
      var name = String(raw).trim().slice(0, HIVE_VIEW_NAME_MAX_LEN);
      if (!name) { hiveToast('View name cannot be empty', 'error'); return; }
      if (name !== v.name && findSavedView(name)) { hiveToast('A view named "' + name + '" already exists', 'error'); return; }
      var wasDefault = _dashDefaultView === v.name;
      v.name = name;
      _dashActiveView = name;
      persistSavedViews();
      if (wasDefault) {
        _dashDefaultView = name;
        try { localStorage.setItem(LS_HIVE_DEFAULT_VIEW, name); } catch (e) { /* storage full or disabled */ }
      }
      renderHives(_allDashHives, true);
    }

    /* deleteSavedView removes the selected view, clearing the default pointer
       if it pointed at the deleted view. */
    async function deleteSavedView() {
      if (!_dashActiveView) { hiveToast('Select a view to delete', 'error'); return; }
      if (!await hiveConfirm('Delete view "' + _dashActiveView + '"?')) return;
      var kept = [];
      for (var i = 0; i < (_dashSavedViews || []).length; i++) {
        if (_dashSavedViews[i].name !== _dashActiveView) kept.push(_dashSavedViews[i]);
      }
      _dashSavedViews = kept;
      if (_dashDefaultView === _dashActiveView) {
        _dashDefaultView = '';
        try { localStorage.removeItem(LS_HIVE_DEFAULT_VIEW); } catch (e) { /* storage disabled */ }
      }
      _dashActiveView = '';
      persistSavedViews();
      renderHives(_allDashHives, true);
    }

    /* toggleDefaultView marks/unmarks the selected view as the one applied on
       load. */
    function toggleDefaultView() {
      if (!_dashActiveView) { hiveToast('Select a view first', 'error'); return; }
      if (_dashDefaultView === _dashActiveView) {
        _dashDefaultView = '';
        try { localStorage.removeItem(LS_HIVE_DEFAULT_VIEW); } catch (e) { /* storage disabled */ }
        hiveToast('Default view cleared', 'success');
      } else {
        _dashDefaultView = _dashActiveView;
        try { localStorage.setItem(LS_HIVE_DEFAULT_VIEW, _dashDefaultView); } catch (e) { /* storage full or disabled */ }
        hiveToast('"' + _dashDefaultView + '" is now the default view', 'success');
      }
      renderHives(_allDashHives, true);
    }

    /* hiveMatchesFilters answers whether a hive survives the active chips. */
    function hiveMatchesFilters(h) {
      if (_dashFailingCheckFilter) {
        var hit = failingChecks(h).some(function(ck) { return ck.name === _dashFailingCheckFilter; });
        if (!hit) return false;
      }
      if (!hiveMatchesDriftFilter(h)) return false;
      if (!hiveMatchesUpgradeFilter(h)) return false;
      if (!hiveMatchesAdvisoryStaleFilter(h)) return false;
      var active = Object.keys(_dashStatusFilters || {}).filter(function(k) { return _dashStatusFilters[k]; });
      if (!active.length) return true;
      var f = hiveStatusFlags(h);
      var byKey = {};
      byKey[HIVE_FILTER_APP_MISSING] = f.appMissing;
      byKey[HIVE_FILTER_NO_TOKENS] = f.noTokens;
      byKey[HIVE_FILTER_DEGRADED] = f.degraded;
      byKey[HIVE_FILTER_OK] = f.ok;
      for (var i = 0; i < active.length; i++) {
        if (byKey[active[i]]) return true;
      }
      return false;
    }

    /* ── Fleet alerts ("Attention needed") ────────────────────────────────
       Server-evaluated (see alerts.go); the client only renders and filters.
       Deliberately NOT recomputed here: a second, drifting implementation of
       "what is wrong" is exactly how the panel and the rows start disagreeing. */

    /* EMPTY_ALERT_SUMMARY is the shape every consumer can assume. Frozen so a
       caller cannot accidentally mutate the shared fallback. */
    var EMPTY_ALERT_SUMMARY = {alerts: [], countsBySeverity: {}, countsByType: {}, total: 0, acknowledgedTotal: 0};

    var _fleetAlerts = EMPTY_ALERT_SUMMARY;
    /* Server-computed fleet inventory counts (hives_summary on the my-hives
       payload). Null until the first load; renderSummaryTiles guards. */
    var _hivesSummary = null;
    /* Active alert-type filter: '' = no alert filtering. Single-select, unlike
       the status chips — "show me the crash-looping hives" is a drill-down, and
       OR-ing several alert types back together just reproduces the full list. */
    var _alertTypeFilter = '';
    /* Whether the acknowledged alerts are expanded into view. */
    var _alertShowAcked = false;

    /* Severity display order + colour, mirroring alerts.go's ranking. */
    var ALERT_SEVERITIES = [
      {key: 'critical', label: 'Critical', color: '#f85149'},
      {key: 'warning', label: 'Warning', color: '#f59e0b'},
      {key: 'info', label: 'Info', color: '#60a5fa'}
    ];

    /* Human labels for the alert type chips. Keys MUST match the AlertType*
       constants in alerts.go; an unknown type falls back to its raw key rather
       than rendering blank. */
    var ALERT_TYPE_LABELS = {
      'crash-loop': 'Crash-looping',
      'offline': 'Offline',
      'stuck-upgrade': 'Stuck upgrade',
      'health-check-failing': 'Health check failing',
      'token-burn': 'Token burn',
      'provision-error': 'Provision error',
      /* Added by the hub in #2308. Without a label here the chip for a
         genuinely failed upgrade would render as the raw key 'failed-upgrade'. */
      'failed-upgrade': 'Failed upgrade',
      'advisory-stale': 'Stale advisory',
      /* Agents that are up but doing nothing — a session that has gone, a
         login prompt, or no output while work is queued. Deliberately paused
         agents are excluded server-side and never counted here. */
      'agents-inactive': 'Idle agents',
      /* Raised by the auth-audit loop since #2306 but never labelled, so its
         chip rendered the raw key. */
      'url-unreachable': 'Dashboard URL unreachable',
      'url-private-network': 'Private dashboard URL',
      /* The hive's inference gateway is rejecting every call with 401 (a stale
         key) — the ROOT cause of an otherwise silent outage where the hive
         looks online but every agent is dead in the water. */
      'inference-auth-failed': 'Inference auth failing',
      /* GitHub App credentials are in an operator-side state (key-missing /
         key-invalid / no-app-assigned). The owner cannot fix these — the App
         key is hub-distributed — so this alert is the operator's only active
         signal (#4316). The reason carries the PUT remedy. */
      'app-creds-undelivered': 'App credentials undelivered'
    };

    /* How many alert rows are listed before the panel collapses the remainder
       behind the "+N more" toggle. Enough to act on, few enough that the panel
       never pushes the hive list off the screen. */
    var ALERT_ROWS_SHOWN_MIN = 6;

    /* Which halves of the panel are expanded past ALERT_ROWS_SHOWN_MIN. Keyed
       'live'/'acked' so the two lists expand independently. Not persisted: an
       expanded list is a momentary "show me the rest", not a view preference,
       and a panel that reloads 50 rows deep would bury the hive table. */
    var _alertRowsExpanded = {live: false, acked: false};

    /* toggleAlertRowsExpanded flips one half of the panel open or closed. */
    function toggleAlertRowsExpanded(which) {
      var key = (which === 'acked') ? 'acked' : 'live';
      _alertRowsExpanded[key] = !_alertRowsExpanded[key];
      renderHives(_allDashHives, true);
    }

    function alertTypeLabel(t) { return ALERT_TYPE_LABELS[t] || t || 'Unknown'; }

    /* alertsForType returns the UNACKNOWLEDGED alerts of one type. Acknowledged
       ones are excluded so filtering by a type never selects hives whose alert
       the operator has already dealt with. */
    function alertsForType(t) {
      return ((_fleetAlerts && _fleetAlerts.alerts) || []).filter(function(a) {
        return a && !a.acknowledged && a.type === t;
      });
    }

    /* hiveMatchesAlertFilter answers whether a hive survives the active alert
       drill-down. No filter = everything passes. */
    function hiveMatchesAlertFilter(h) {
      if (!_alertTypeFilter) return true;
      if (!h || !h.id) return false;
      var matches = alertsForType(_alertTypeFilter);
      for (var i = 0; i < matches.length; i++) {
        if (matches[i].hiveId === h.id) return true;
      }
      return false;
    }

    /* toggleAlertFilter drills into (or back out of) one alert type. Clicking
       the active chip again clears it. */
    function toggleAlertFilter(t) {
      _alertTypeFilter = (_alertTypeFilter === t) ? '' : t;
      renderHives(_allDashHives, true);
    }

    function clearAlertFilter() {
      _alertTypeFilter = '';
      renderHives(_allDashHives, true);
    }

    /* clearAllHiveFilters resets EVERY narrowing control at once — status chips,
       the failing-check and drift filters, the alert-type filter, the search box
       and the facets. The empty-state escape hatch uses it, because any one of
       them alone can empty the list and the user usually cannot tell which is
       responsible. */
    function clearAllHiveFilters() {
      _dashStatusFilters = {};
      _dashFailingCheckFilter = '';
      _dashDriftFilter = '';
      /* Everything is being cleared, search included — a search stashed at
         drift-pill-selection time must not come back later. */
      _driftSearchStash = null;
      _dashUpgradeFilter = '';
      _dashAdvisoryStaleFilter = false;
      _alertTypeFilter = '';
      _dashFacets = {};
      _dashSearchQuery = '';
      var searchEl = document.getElementById('hive-search');
      if (searchEl) searchEl.value = '';
      renderHives(_allDashHives, true);
    }

    function toggleAlertAcked() {
      _alertShowAcked = !_alertShowAcked;
      renderHives(_allDashHives, true);
    }

    /* alertAge renders how long a condition has been firing, from the server's
       firstSeen. Rounded coarsely so it does not churn every poll. */
    function alertAge(firstSeen) {
      if (!firstSeen) return '';
      var ms = Date.now() - new Date(firstSeen).getTime();
      if (!isFinite(ms) || ms < 0) return '';
      var MIN = 60000, HOUR = 3600000, DAY = 86400000;
      if (ms < MIN) return 'just now';
      if (ms < HOUR) return Math.round(ms / MIN) + 'm';
      if (ms < DAY) return Math.round(ms / HOUR) + 'h';
      return Math.floor(ms / DAY) + 'd';
    }

    /* ackAlert silences (or un-silences) one alert. Admin-only server-side; the
       button is only rendered for admins, but the server is the real gate. */
    async function ackAlert(hiveId, type, clear) {
      try {
        var resp = await fetch('/api/saas/admin/alert-ack', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({hiveId: hiveId, type: type, clear: !!clear})
        });
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) {
          hiveToast(data.error || 'Failed to update alert', 'error');
          return;
        }
        hiveToast(clear ? 'Alert restored' : 'Alert acknowledged', 'success');
        /* Re-fetch rather than mutating locally: the server owns whether an ack
           actually applies (it refuses one for a condition that is not live). */
        loadHives();
      } catch (e) {
        hiveToast('Error: ' + e.message, 'error');
      }
    }

    /* ── Jumping from an alert row to the hive's row in the table ──

       hiveRowDomId builds the anchor id renderHives puts on each <tr>. Prefixed
       so it can never collide with another element id, and encodeURIComponent'd
       because a hive id is user-influenced and may contain characters that are
       not valid raw in an id/selector. */
    var HIVE_ROW_DOM_ID_PREFIX = 'hive-row-';
    function hiveRowDomId(hiveId) {
      return HIVE_ROW_DOM_ID_PREFIX + encodeURIComponent(String(hiveId || ''));
    }

    /* How long the targeted row stays highlighted. Long enough to find the row
       after the smooth scroll finishes, short enough that a stale highlight does
       not linger and get mistaken for a status. */
    var HIVE_ROW_TARGET_HIGHLIGHT_MS = 2600;

    /* Handle of the pending highlight-clear timer, so two jumps in quick
       succession do not leave the first row highlighted forever. */
    var _hiveRowTargetTimer = null;

    /* focusHiveRow scrolls one hive's table row into view and highlights it.
       Returns false when the row is not currently in the DOM. */
    function focusHiveRow(hiveId) {
      var row = document.getElementById(hiveRowDomId(hiveId));
      if (!row) return false;
      /* Clear any previous target first — only ever one highlighted row. */
      if (_hiveRowTargetTimer) { clearTimeout(_hiveRowTargetTimer); _hiveRowTargetTimer = null; }
      var prev = document.querySelectorAll('.hive-row-targeted');
      (prev || []).forEach(function(el) { el.classList.remove('hive-row-targeted'); });

      row.scrollIntoView({behavior: 'smooth', block: 'center'});
      row.classList.add('hive-row-targeted');
      /* Move keyboard focus to the row as well, so a keyboard user lands there
         rather than being scrolled somewhere their focus ring is not. */
      if (!row.hasAttribute('tabindex')) row.setAttribute('tabindex', '-1');
      try { row.focus({preventScroll: true}); } catch (e) { /* focus is best-effort */ }

      _hiveRowTargetTimer = setTimeout(function() {
        row.classList.remove('hive-row-targeted');
        _hiveRowTargetTimer = null;
      }, HIVE_ROW_TARGET_HIGHLIGHT_MS);
      return true;
    }

    /* jumpToHiveRow is what an "Attention needed" row does when clicked.

       The hard case is a target the current view is HIDING. The hive list now
       carries persisted filters, facets, a search box, an alert drill-down and
       collapsible sections/groups (#2309, #2317), so the alerted hive may well
       not be on screen. Scrolling to a row that is not in the DOM would look
       like the click did nothing.

       The choice made here: try the current view FIRST, and only if the row is
       genuinely absent do we widen the view — clearing every narrowing control
       and expanding the collapsed sections/groups. Filters are the user's state,
       so they are never discarded silently: we clear them only when that is the
       one thing that can satisfy the click, and we say so in a toast that names
       the hive. The alternative (refusing to navigate and just warning) leaves
       the operator to work out which of six controls is hiding the row, which is
       the problem the panel exists to avoid. */
    function jumpToHiveRow(hiveId, hiveName) {
      if (!hiveId) return;
      if (focusHiveRow(hiveId)) return;

      /* Not rendered under the current view. Widen everything that can hide a
         row, re-render synchronously, then try again. */
      clearAllHiveFilters();
      expandAllHiveSections();
      _dashGroupCollapsed = {};
      persistGroupCollapsed();
      renderHives(_allDashHives, true);

      if (focusHiveRow(hiveId)) {
        hiveToast('Filters cleared to show ' + (hiveName || hiveId), 'info');
        return;
      }
      /* Still absent: the hive is genuinely not in the loaded list (for example
         it was deleted between the alert being evaluated and now). Say so rather
         than leaving a click that silently did nothing. */
      hiveToast('Could not find ' + (hiveName || hiveId) + ' in the hive list', 'error');
    }

    /* SUMMARY_TILES_MIN_HIVES gates the fleet tiles strip: below this many
       visible hives the tiles restate what the eye already sees in the table,
       so they stay hidden and the page keeps its small-user simplicity. */
    var SUMMARY_TILES_MIN_HIVES = 8;

    /* renderSummaryTiles draws the fleet inventory strip above the alerts
       panel from the server-computed hives_summary — always the caller's FULL
       visible set, never the active filter, so the numbers stay truthful
       during any drill-down. Zero-count exception tiles self-suppress. */
    function renderSummaryTiles() {
      var el = document.getElementById('fleet-summary-tiles');
      if (!el) return;
      var s = _hivesSummary;
      if (!s || (Number(s.total) || 0) < SUMMARY_TILES_MIN_HIVES) {
        el.style.display = 'none';
        return;
      }
      /* [key, label, css-class-when-nonzero, always-show] */
      var defs = [
        ['total', 'Hives', '', true],
        ['online', 'Online', '', true],
        ['offline', 'Offline', 'warn', true],
        ['pool_available', 'Pool', '', false],
        ['assigned_unclaimed', 'Unclaimed', 'warn', false],
        ['provisioning', 'Provisioning', '', false],
        ['upgrading', 'Upgrading', '', false],
        ['upgrade_failed', 'Upgrade failed', 'bad', false],
        ['errors', 'Errors', 'bad', false]
      ];
      var tiles = '';
      for (var i = 0; i < defs.length; i++) {
        var n = Number(s[defs[i][0]]) || 0;
        if (!n && !defs[i][3]) continue;
        var cls = n && defs[i][2] ? ' ' + defs[i][2] : '';
        tiles += '<div class="fleet-tile' + cls + '"><div class="fleet-tile-n">' + n +
          '</div><div class="fleet-tile-label">' + esc(defs[i][1]) + '</div></div>';
      }
      el.innerHTML = '<div class="fleet-tiles">' + tiles + '</div>';
      el.style.display = '';
    }

    /* renderAlertsPanel draws the "Attention needed" panel above the hive list.
       When the fleet is clean it renders one quiet line instead of hiding
       entirely, so the operator can trust that the check actually ran. */
    function renderAlertsPanel() {
      var panel = document.getElementById('fleet-alerts-panel');
      if (!panel) return;
      var summary = _fleetAlerts || EMPTY_ALERT_SUMMARY;
      var all = summary.alerts || [];
      var counts = summary.countsBySeverity || {};
      var byType = summary.countsByType || {};
      var total = Number(summary.total) || 0;
      var ackedTotal = Number(summary.acknowledgedTotal) || 0;

      /* Nothing at all to say — and no hives yet — stay out of the way. */
      if (!total && !ackedTotal && !(_allDashHives || []).length) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';

      if (!total) {
        var cleanNote = ackedTotal
          ? ' <button type="button" class="alert-panel-more" onclick="toggleAlertAcked()">' +
            ackedTotal + ' acknowledged</button>'
          : '';
        var ackedRows = (_alertShowAcked && ackedTotal) ? renderAlertRows(all, true) : '';
        panel.innerHTML = '<div class="alert-panel">' +
          '<div class="alert-panel-clean"><span style="color:#3fb950">&#10003;</span>' +
          '<span>Nothing needs your attention.</span>' + cleanNote + '</div>' +
          ackedRows + '</div>';
        return;
      }

      var cls = 'alert-panel';
      if (counts.critical) cls += ' has-critical';
      else if (counts.warning) cls += ' has-warning';

      var pills = (ALERT_SEVERITIES || []).map(function(s) {
        var n = Number(counts[s.key]) || 0;
        if (!n) return '';
        return '<span class="alert-sev-pill" style="--alert-color:' + s.color + '">' +
          n + ' ' + esc(s.label) + '</span>';
      }).join('');

      /* Type chips, ordered by the severity of the alerts they represent so the
         most urgent drill-down is leftmost. */
      var typeKeys = Object.keys(byType).filter(function(k) { return Number(byType[k]) > 0; });
      typeKeys.sort(function(a, b) {
        var ra = alertTypeWorstSeverityRank(a), rb = alertTypeWorstSeverityRank(b);
        if (ra !== rb) return ra - rb;
        return a < b ? -1 : (a > b ? 1 : 0);
      });
      var chips = typeKeys.map(function(k) {
        var on = _alertTypeFilter === k;
        var color = ALERT_SEVERITY_COLORS[alertTypeWorstSeverity(k)] || '#8b949e';
        return '<button type="button" class="filter-chip' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') +
          '" onclick="toggleAlertFilter(\'' + esc(k) + '\')" style="--chip-color:' + color + '">' +
          '<span class="filter-chip-dot" style="background:' + color + '"></span>' +
          esc(alertTypeLabel(k)) +
          '<span class="filter-chip-count">' + (Number(byType[k]) || 0) + '</span></button>';
      }).join('');
      var clearChip = _alertTypeFilter
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="clearAlertFilter()">Show all hives</button>'
        : '';

      var ackNote = ackedTotal
        ? '<button type="button" class="alert-panel-more" onclick="toggleAlertAcked()">' +
          (_alertShowAcked ? 'Hide' : 'Show') + ' ' + ackedTotal + ' acknowledged</button>'
        : '';

      panel.innerHTML = '<div class="' + cls + '">' +
        '<div class="alert-panel-head">' +
          '<div class="alert-panel-title"><span>&#9888;</span><span>Attention needed</span></div>' +
          '<div style="display:flex;gap:6px;flex-wrap:wrap">' + pills + '</div>' +
        '</div>' +
        '<div class="alert-type-chips">' + chips + clearChip + '</div>' +
        renderAlertRows(all, false) +
        (_alertShowAcked ? renderAlertRows(all, true) : '') +
        ackNote +
      '</div>';
    }

    /* ALERT_SEVERITY_COLORS maps a severity to its chip colour, derived from
       ALERT_SEVERITIES so the two can never disagree. */
    var ALERT_SEVERITY_COLORS = (function() {
      var m = {};
      (ALERT_SEVERITIES || []).forEach(function(s) { m[s.key] = s.color; });
      return m;
    })();

    /* alertTypeWorstSeverity / ...Rank find the most urgent severity currently
       present for a type, so a type chip is coloured by the worst thing in it. */
    function alertTypeWorstSeverity(t) {
      var worst = '', worstRank = ALERT_SEVERITIES.length;
      alertsForType(t).forEach(function(a) {
        var r = alertSeverityRankJS(a.severity);
        if (r < worstRank) { worstRank = r; worst = a.severity; }
      });
      return worst;
    }
    function alertTypeWorstSeverityRank(t) {
      var r = alertSeverityRankJS(alertTypeWorstSeverity(t));
      return r;
    }
    /* alertSeverityRankJS mirrors alerts.go's ordering; unknown sorts last. */
    function alertSeverityRankJS(sev) {
      for (var i = 0; i < ALERT_SEVERITIES.length; i++) {
        if (ALERT_SEVERITIES[i].key === sev) return i;
      }
      return ALERT_SEVERITIES.length;
    }

    /* renderAlertRows lists individual alerts. acked selects which half of the
       list is drawn: the live alerts, or the acknowledged ones. */
    function renderAlertRows(all, acked) {
      var rows = (all || []).filter(function(a) { return a && !!a.acknowledged === !!acked; });
      /* When a type drill-down is active, the rows follow it too — otherwise the
         panel would still list alerts for hives the table is no longer showing. */
      if (_alertTypeFilter) {
        rows = rows.filter(function(a) { return a.type === _alertTypeFilter; });
      }
      if (!rows.length) return '';
      /* Each half of the panel (live vs acknowledged) expands independently, so
         expanding the acknowledged list does not also dump every live alert. */
      var expanded = !!_alertRowsExpanded[acked ? 'acked' : 'live'];
      var limit = expanded ? rows.length : ALERT_ROWS_SHOWN_MIN;
      var shown = rows.slice(0, limit);
      var html = shown.map(function(a) {
        var color = ALERT_SEVERITY_COLORS[a.severity] || '#8b949e';
        var age = alertAge(a.firstSeen);
        /* Every interpolated value below is spoke- or admin-supplied and is
           escaped: hive names, reasons (which embed check names and provision
           error text), and the acking username. */
        var ackBtn = '';
        if (_isAdmin) {
          /* jsArg for the same reason as the jump handler below: esc() leaves an
             apostrophe intact, which would close the handler's quote early. */
          ackBtn = acked
            ? '<button type="button" class="alert-ack-btn" onclick="ackAlert(' +
              jsArg(a.hiveId) + ',' + jsArg(a.type) + ',true)">Restore</button>'
            : '<button type="button" class="alert-ack-btn" onclick="ackAlert(' +
              jsArg(a.hiveId) + ',' + jsArg(a.type) + ',false)">Acknowledge</button>';
        }
        var ackedBy = (acked && a.ackBy) ? '<span class="alert-row-reason">— ack by ' + esc(a.ackByName || a.ackBy) + '</span>' : '';
        /* The row itself is a <button> that jumps to this hive's row in the
           table below. A real button (not a div with role/tabindex) gets Enter,
           Space and the focus ring for free.

           The Acknowledge button is a SIBLING, not a child: nesting a button
           inside a button is invalid HTML and browsers drop the inner one, which
           would silently break acknowledging. Both sit in a flex wrapper so the
           row still reads as one line. */
        var label = a.hiveName || a.hiveId;
        var jump = '<button type="button" class="alert-row' + (acked ? ' acked' : '') +
          '" title="Show ' + escAttr(label) + ' in the hive list below"' +
          /* jsArg, not esc: a hive or org name containing an apostrophe would
             close the handler's quote early and break the click. */
          ' onclick="jumpToHiveRow(' + jsArg(a.hiveId) + ',' + jsArg(label) + ')">' +
          '<span class="filter-chip-dot" style="background:' + color + '"></span>' +
          '<span class="alert-row-hive">' + esc(label) + '</span>' +
          '<span class="alert-row-reason">' + esc(a.reason) + '</span>' + ackedBy +
          '<span class="alert-row-age">' + esc(age) + '</span>' +
        '</button>';
        return '<div class="alert-row-wrap">' + jump + ackBtn + '</div>';
      }).join('');
      /* "+N more" used to be inert text, which was actively misleading once the
         rows above it became clickable: it looked like the same kind of thing
         and did nothing. It is now a real toggle that expands the full list in
         place (and collapses it again), so every alerted hive is reachable —
         which is the point, since the hive the operator needs is as likely to be
         #7 as #1. */
      var more = '';
      if (rows.length > limit) {
        more = '<button type="button" class="alert-panel-more" onclick="toggleAlertRowsExpanded(' +
          jsArg(acked ? 'acked' : 'live') + ')">+' + (rows.length - limit) +
          ' more</button>';
      } else if (expanded && rows.length > ALERT_ROWS_SHOWN_MIN) {
        more = '<button type="button" class="alert-panel-more" onclick="toggleAlertRowsExpanded(' +
          jsArg(acked ? 'acked' : 'live') + ')">Show fewer</button>';
      }
      return '<div class="alert-rows">' + html + more + '</div>';
    }

    /* ── Free-text search over the hive list ──
       OR semantics: whitespace-separated terms, a hive matches if ANY term is a
       case-insensitive substring of its searchable text. Like the status chips,
       search only narrows ASSIGNED hives — the unassigned placeholder pool is
       inventory, not a search result, and making it vanish as an admin types
       would hide the very slots they are about to assign. See _dashSearchQuery
       and hiveMatchesSearch below. */

    /* ── Collapsible admin sections ──
       Keys follow the 'hive-*' localStorage convention already used by
       hive-dismissed-banners / hive-cluster-health-collapsed. */
    var HIVE_SECTION_ASSIGNED = 'assigned';
    var HIVE_SECTION_UNASSIGNED = 'unassigned';
    var LS_SECTION_ASSIGNED_COLLAPSED = 'hive-section-assigned-collapsed';
    var LS_SECTION_UNASSIGNED_COLLAPSED = 'hive-section-unassigned-collapsed';

    /* Defaults: Assigned EXPANDED (the hives you actually operate), Unassigned
       COLLAPSED (a pool that can run to dozens of identical slots). Stored as
       the string 'true'/'false'; anything else falls back to the default, so a
       first visit and a corrupted value behave the same. */
    var _dashSectionCollapsed = {};
    _dashSectionCollapsed[HIVE_SECTION_ASSIGNED] =
      localStorage.getItem(LS_SECTION_ASSIGNED_COLLAPSED) === 'true';
    _dashSectionCollapsed[HIVE_SECTION_UNASSIGNED] =
      localStorage.getItem(LS_SECTION_UNASSIGNED_COLLAPSED) !== 'false';

    /* expandAllHiveSections opens both sections and PERSISTS that, matching
       toggleHiveSection. An in-memory-only reset would silently re-collapse on
       the next reload, so the row the operator was just sent to would vanish
       again — the collapsed Unassigned pool is the default, so this matters for
       any alert on an unassigned placeholder. */
    function expandAllHiveSections() {
      _dashSectionCollapsed[HIVE_SECTION_ASSIGNED] = false;
      _dashSectionCollapsed[HIVE_SECTION_UNASSIGNED] = false;
      try {
        localStorage.setItem(LS_SECTION_ASSIGNED_COLLAPSED, 'false');
        localStorage.setItem(LS_SECTION_UNASSIGNED_COLLAPSED, 'false');
      } catch(e) {} /* private-browsing quota — expansion still works in-session */
    }

    function toggleHiveSection(sectionKey) {
      _dashSectionCollapsed[sectionKey] = !_dashSectionCollapsed[sectionKey];
      var lsKey = sectionKey === HIVE_SECTION_ASSIGNED
        ? LS_SECTION_ASSIGNED_COLLAPSED : LS_SECTION_UNASSIGNED_COLLAPSED;
      try {
        localStorage.setItem(lsKey, _dashSectionCollapsed[sectionKey] ? 'true' : 'false');
      } catch(e) {} /* private-browsing quota — collapse still works in-session */
      renderHives(_allDashHives, true);
    }

    var _dashSearchQuery = '';
    /* Debounce keystrokes so a large hive list is not re-rendered per character.
       120ms is below the ~200ms threshold where typing starts to feel laggy. */
    var HIVE_SEARCH_DEBOUNCE_MS = 120;
    var _hiveSearchTimer = null;

    /* hiveSearchText flattens a hive's user-visible metadata into one lowercase
       haystack. Includes the access list usernames so an admin can find "which
       hive did I grant octocat?" — access is only populated on owned rows. */
    function hiveSearchText(h) {
      if (!h) return '';
      var parts = [
        h.id, h.name, h.org, h.primaryRepo, h.clusterId, h.clusterName,
        h.role, h.gitBranch, h.gitHash, h.dashboardUrl,
        /* The tracked release channel is what the version pill shows for a
           channel-pinned hive, so "stable" has to find those rows even though
           their gitBranch reports the underlying release branch. */
        h.trackedChannel,
        /* The GitHub host is shown as a pill in the Location column, so it has
           to be searchable too — "github.ibm.com" is how an admin narrows to
           the GHE fleet. Absent means public GitHub, which is what the pill
           renders, so search on that same default rather than nothing. */
        h.githubHost || PAST_REQUESTS_DEFAULT_GITHUB_HOST
      ];
      (h.repos || []).forEach(function(r) { parts.push(r); });
      (h.access || []).forEach(function(a) {
        if (!a) return;
        parts.push(a.username);
        parts.push(a.role);
      });
      return parts.filter(function(p) { return p; }).join(' ').toLowerCase();
    }

    /* hiveMatchesSearch answers whether a hive survives the search box. */
    function hiveMatchesSearch(h) {
      var terms = (_dashSearchQuery || '').toLowerCase().split(/\s+/).filter(function(t) { return t; });
      if (!terms.length) return true;
      var hay = hiveSearchText(h);
      for (var i = 0; i < terms.length; i++) {
        if (hay.indexOf(terms[i]) !== -1) return true; /* OR */
      }
      return false;
    }

    /* onHiveSearchInput debounces then re-renders from the unfiltered cache so
       search composes with the chips, the facets and the current sort. */
    function onHiveSearchInput() {
      var el = document.getElementById('hive-search');
      var next = el ? el.value : '';
      if (_hiveSearchTimer) clearTimeout(_hiveSearchTimer);
      /* Typing while a drift pill is active supersedes the search stashed at
         pill-selection time: drop the stash immediately (not in the debounce)
         so a quick type-then-deselect cannot resurrect the old text. */
      if (_dashDriftFilter) _driftSearchStash = null;
      _hiveSearchTimer = setTimeout(function() {
        _dashSearchQuery = next;
        renderHives(_allDashHives, true);
      }, HIVE_SEARCH_DEBOUNCE_MS);
    }

    function clearHiveSearch() {
      var el = document.getElementById('hive-search');
      if (el) el.value = '';
      _dashSearchQuery = '';
      /* An explicit Clear is a statement the user wants NO search — do not
         resurrect a stashed one when the drift pills are later deselected. */
      _driftSearchStash = null;
      renderHives(_allDashHives, true);
    }

    /* ── Faceted search ──
       Facet groups are derived from the assigned hives themselves, so a group
       only appears when the data has something to offer. Within one group the
       selected values are OR'd (pick two clusters => hives in either); across
       groups they are AND'd (that cluster AND that role), which is the standard
       faceted-search contract users expect from e-commerce-style filters. */
    var FACET_CLUSTER = 'cluster';
    var FACET_ACMM = 'acmm';
    var FACET_ROLE = 'role';
    var FACET_STATUS = 'status';
    var FACET_BRANCH = 'branch';
    /* GitHub instance the hive targets. Worth its own facet because the
       GHE-vs-public split is a real operational boundary — a github.ibm.com
       hive can only be reached from a cluster that can route to it. */
    var FACET_GITHUB_HOST = 'github-host';

    /* Value shown when a hive has no value for a facet, so those rows stay
       reachable instead of silently dropping out of every facet count. */
    var FACET_UNKNOWN = '—';

    /* Group keys for the two filters moved into the tray from the old chip bar.
       They are NOT in HIVE_FACET_GROUPS — those are derived from hive fields via
       hiveFacetValues, whereas these keep their own pre-existing state
       (_dashStatusFilters / _dashFailingCheckFilter) and matching semantics.
       They share _dashFacetCollapsed only for the collapse chrome. */
    var FACET_GROUP_HEALTH = 'health';
    var FACET_GROUP_FAILING_CHECK = 'failing-check';
    /* Upgrade-state group. Follows the HEALTH/FAILING_CHECK pattern rather than
       the derived-facet one: the values are a fixed, hand-written lifecycle
       ('upgrading', 'queued') with their own matching rule and their own state
       (_dashUpgradeFilter), not a dimension enumerated from the data like
       cluster or branch, so it stays OUT of HIVE_FACET_GROUPS. */
    var FACET_GROUP_UPGRADE = 'upgrade-state';
    /* Stale-advisory group. Follows the HEALTH/FAILING_CHECK pattern rather
       than the derived-facet one: "is this hive's advisory digest stale" is a
       boolean health signal read off a server-computed flag, not an enumerated
       dimension of the hive like cluster or branch, so it keeps its own state
       (_dashAdvisoryStaleFilter) and its own matching rule and stays OUT of
       HIVE_FACET_GROUPS. */
    var FACET_GROUP_ADVISORY_STALE = 'advisory-stale';

    var HIVE_FACET_GROUPS = [
      {key: FACET_CLUSTER, label: 'Location'},
      {key: FACET_ACMM, label: 'ACMM level'},
      {key: FACET_ROLE, label: 'Your role'},
      {key: FACET_STATUS, label: 'Status'},
      {key: FACET_BRANCH, label: 'Branch'},
      {key: FACET_GITHUB_HOST, label: 'GitHub'}
    ];

    /* Selected facet values: {facetKey: {value: true}}. */
    var _dashFacets = {};
    /* Collapsed facet GROUPS (chrome only, not a filter): {facetKey: true}. */
    var _dashFacetCollapsed = {};

    /* ── Facet tray open/closed ──
       Click-toggled, persisted, and COLLAPSED BY DEFAULT so the dashboard is
       uncluttered until filters are asked for. Key follows the same 'hive-*'
       localStorage convention as hive-section-assigned-collapsed and
       hive-cluster-health-collapsed. Open only when explicitly stored 'true',
       so a first visit and a corrupted value both fall back to closed. */
    var LS_FACET_TRAY_OPEN = 'hive-facet-tray-open';
    var _dashFacetTrayOpen = false;
    try {
      _dashFacetTrayOpen = localStorage.getItem(LS_FACET_TRAY_OPEN) === 'true';
    } catch (e) { _dashFacetTrayOpen = false; } /* storage disabled */

    /* applyFacetTrayState paints the DOM from _dashFacetTrayOpen. Split out so
       the initial paint and every toggle go through exactly one code path and
       can never disagree about aria-expanded. */
    function applyFacetTrayState() {
      var shell = document.getElementById('hive-facet-shell');
      var toggle = document.getElementById('hive-facet-toggle');
      if (shell) shell.classList.toggle('facet-open', _dashFacetTrayOpen);
      if (toggle) {
        toggle.setAttribute('aria-expanded', _dashFacetTrayOpen ? 'true' : 'false');
        toggle.title = _dashFacetTrayOpen ? 'Hide filters' : 'Show filters';
      }
    }

    function setFacetTrayOpen(open) {
      _dashFacetTrayOpen = !!open;
      try {
        localStorage.setItem(LS_FACET_TRAY_OPEN, _dashFacetTrayOpen ? 'true' : 'false');
      } catch (e) {} /* private-browsing quota — the toggle still works in-session */
      applyFacetTrayState();
    }

    function toggleFacetTray() { setFacetTrayOpen(!_dashFacetTrayOpen); }

    /* activeFilterCount is what the collapsed rail's badge shows. EVERY control
       that can narrow the list is counted, because the badge is the only thing
       telling the operator why the table is short while the tray is shut. Each
       selected facet value counts individually — "3" must mean three choices,
       not three groups. */
    function activeFilterCount() {
      var n = 0;
      n += Object.keys(_dashStatusFilters || {}).length;
      if (_dashFailingCheckFilter) n++;
      if (_dashDriftFilter) n++;
      if (_dashUpgradeFilter) n++;
      if (_dashAdvisoryStaleFilter) n++;
      if (_alertTypeFilter) n++;
      if (_dashSearchQuery) n++;
      var groups = Object.keys(_dashFacets || {});
      for (var i = 0; i < groups.length; i++) {
        n += Object.keys(_dashFacets[groups[i]] || {}).length;
      }
      return n;
    }

    /* renderFacetTrayAffordance keeps the collapsed rail honest: accent border
       plus a count badge whenever anything is filtering. */
    function renderFacetTrayAffordance() {
      var toggle = document.getElementById('hive-facet-toggle');
      var badge = document.getElementById('hive-facet-active-badge');
      var n = activeFilterCount();
      if (toggle) toggle.classList.toggle('has-active', n > 0);
      if (badge) {
        badge.style.display = n > 0 ? '' : 'none';
        badge.textContent = n > 0 ? String(n) : '';
        badge.title = n === 1 ? '1 active filter' : n + ' active filters';
      }
      if (toggle) {
        var base = _dashFacetTrayOpen ? 'Hide filters' : 'Show filters';
        toggle.title = n > 0 ? base + ' — ' + (n === 1 ? '1 filter active' : n + ' filters active') : base;
        toggle.setAttribute('aria-label', toggle.title);
      }
    }

    /* Bound once at parse time via delegation on document, so the handlers
       survive every re-render of the tray's contents and no inline onclick has
       to interpolate anything. */
    document.addEventListener('click', function(ev) {
      var t = ev.target;
      if (!t || !t.closest) return;
      if (t.closest('#hive-facet-toggle')) { toggleFacetTray(); return; }
      if (t.closest('#hive-facet-close')) { setFacetTrayOpen(false); return; }
    });
    /* Escape closes the tray when focus is inside it, and returns focus to the
       toggle so the keyboard user is not stranded on a hidden control. */
    document.addEventListener('keydown', function(ev) {
      if (ev.key !== 'Escape' || !_dashFacetTrayOpen) return;
      var tray = document.getElementById('hive-facet-tray');
      if (!tray || !tray.contains(document.activeElement)) return;
      setFacetTrayOpen(false);
      var toggle = document.getElementById('hive-facet-toggle');
      if (toggle) toggle.focus();
    });
    /* Paint the persisted state immediately. This script runs after the tray
       markup, so the elements already exist and there is no open-then-snap-shut
       flash on a reload with the tray remembered open. */
    applyFacetTrayState();

    /* hiveFacetValues returns the facet values a single hive belongs to. A hive
       contributes at most one value per group. */
    function hiveFacetValues(h) {
      h = h || {};
      var f = {};
      f[FACET_CLUSTER] = h.clusterName || h.clusterId || 'local';
      f[FACET_ACMM] = (h.acmmLevel != null && h.acmmLevel !== '') ? ('L' + h.acmmLevel) : FACET_UNKNOWN;
      f[FACET_ROLE] = h.role || FACET_UNKNOWN;
      f[FACET_BRANCH] = h.gitBranch || FACET_UNKNOWN;
      /* Absent host means public GitHub — bucket it under the same label the
         pill renders, not FACET_UNKNOWN, so old spokes group with github.com
         rather than forming a meaningless "—" bucket. */
      f[FACET_GITHUB_HOST] = h.githubHost || PAST_REQUESTS_DEFAULT_GITHUB_HOST;
      var flags = hiveStatusFlags(h);
      f[FACET_STATUS] = flags.degraded ? 'Degraded' : (h.online ? 'Online' : 'Offline');
      return f;
    }

    /* hiveMatchesFacets: OR within a group, AND across groups. */
    function hiveMatchesFacets(h) {
      var vals = hiveFacetValues(h);
      var groups = Object.keys(_dashFacets || {});
      for (var g = 0; g < groups.length; g++) {
        var picked = Object.keys(_dashFacets[groups[g]] || {}).filter(function(v) { return _dashFacets[groups[g]][v]; });
        if (!picked.length) continue;
        if (picked.indexOf(String(vals[groups[g]])) === -1) return false;
      }
      return true;
    }

    function toggleFacetValue(facetKey, value) {
      if (!_dashFacets[facetKey]) _dashFacets[facetKey] = {};
      if (_dashFacets[facetKey][value]) {
        delete _dashFacets[facetKey][value];
        if (!Object.keys(_dashFacets[facetKey]).length) delete _dashFacets[facetKey];
      } else {
        _dashFacets[facetKey][value] = true;
      }
      renderHives(_allDashHives, true);
    }

    function clearHiveFacets() {
      _dashFacets = {};
      renderHives(_allDashHives, true);
    }

    function toggleFacetGroup(facetKey) {
      _dashFacetCollapsed[facetKey] = !_dashFacetCollapsed[facetKey];
      renderHives(_allDashHives, true);
    }

    /* facetGroupShell wraps one collapsible group so the status/failing-check
       groups moved in from the old chip bar are visually and behaviourally
       identical to the derived facet groups. */
    function facetGroupShell(key, label, collapsed, bodyHTML) {
      return '<div class="facet-group">' +
        '<button type="button" class="facet-group-head" aria-expanded="' + (collapsed ? 'false' : 'true') +
        '" onclick="toggleFacetGroup(\'' + esc(key) + '\')">' +
        '<span>' + esc(label) + '</span><span>' + (collapsed ? '▸' : '▾') + '</span></button>' +
        (collapsed ? '' : bodyHTML) + '</div>';
    }

    /* renderStatusFacetGroup renders the four health chips — GitHub App not
       installed / No tokens used / Degraded / OK — as a facet group inside the
       tray, replacing the old standalone chip row. Semantics are UNCHANGED:
       these still OR among themselves via _dashStatusFilters and
       hiveMatchesFilters, exactly like a facet group ORs within itself.
       Counts are over the FULL assigned set (placeholders already excluded by
       the caller), matching what the old bar advertised. */
    function renderStatusFacetGroup(assignedNoPlaceholders) {
      var counts = {};
      counts[HIVE_FILTER_APP_MISSING] = 0;
      counts[HIVE_FILTER_NO_TOKENS] = 0;
      counts[HIVE_FILTER_DEGRADED] = 0;
      counts[HIVE_FILTER_OK] = 0;
      (assignedNoPlaceholders || []).forEach(function(h) {
        var f = hiveStatusFlags(h);
        if (f.appMissing) counts[HIVE_FILTER_APP_MISSING]++;
        if (f.noTokens) counts[HIVE_FILTER_NO_TOKENS]++;
        if (f.degraded) counts[HIVE_FILTER_DEGRADED]++;
        if (f.ok) counts[HIVE_FILTER_OK]++;
      });
      var body = '<div class="facet-values">' + (HIVE_FILTER_CHIPS || []).map(function(c) {
        var on = !!_dashStatusFilters[c.key];
        return '<button type="button" class="facet-value' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') +
          '" onclick="toggleStatusFilter(\'' + esc(c.key) + '\')" title="' + esc(c.label) + '">' +
          '<span class="facet-value-label">' + esc(c.label) + '</span>' +
          '<span class="facet-value-count">' + counts[c.key] + '</span></button>';
      }).join('') +
      /* Group-scoped reset for the health + failing-check pair, so a user can
         undo just these without also dropping their search term and facets. */
      (Object.keys(_dashStatusFilters || {}).length || _dashFailingCheckFilter || _dashDriftFilter || _dashUpgradeFilter || _dashAdvisoryStaleFilter
        ? '<button type="button" class="facet-value" onclick="clearStatusFilters()" title="Clear the health, failing-check, drift, upgrade-state and stale-advisory filters">' +
          '<span class="facet-value-label">Clear health filters</span></button>'
        : '') + '</div>';
      return facetGroupShell(FACET_GROUP_HEALTH, 'Health',
        !!_dashFacetCollapsed[FACET_GROUP_HEALTH], body);
    }

    /* renderFailingCheckFacetGroup renders the failing-check picker as a facet
       group. Semantics are UNCHANGED: still SINGLE-select and still ANDed
       against the health chips by hiveMatchesFilters. That is deliberately not
       the OR-within-group rule the derived facets use — "degraded hives" and
       "github_auth is failing" are different questions whose useful answer is
       the intersection — so the group head says so, rather than silently
       looking like the others. Returns '' on a healthy fleet. */
    function renderFailingCheckFacetGroup(assignedNoPlaceholders) {
      var fcCounts = failingCheckCounts(assignedNoPlaceholders);
      var fcNames = Object.keys(fcCounts).sort(function(a, b) {
        return fcCounts[b] - fcCounts[a] || a.localeCompare(b);
      }).slice(0, MAX_FAILING_CHECK_FILTER_OPTIONS);
      if (!fcNames.length && !_dashFailingCheckFilter) return '';
      /* Keep a selected check listed even at count 0 so it can be clicked off. */
      if (_dashFailingCheckFilter && fcNames.indexOf(_dashFailingCheckFilter) === -1) {
        fcNames.unshift(_dashFailingCheckFilter);
        if (fcCounts[_dashFailingCheckFilter] == null) fcCounts[_dashFailingCheckFilter] = 0;
      }
      var body = '<div class="facet-values">' + fcNames.map(function(nm) {
        var on = _dashFailingCheckFilter === nm;
        var n = fcCounts[nm] || 0;
        var tip = n >= FLEET_CHECK_SIGNAL_MIN_HIVES
          ? nm + ' failing on ' + n + ' hives — likely fleet-wide'
          : nm + ' failing on ' + n + (n === 1 ? ' hive' : ' hives');
        return '<button type="button" class="facet-value' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + esc(tip) + '"' +
          ' onclick="setFailingCheckFilter(' + (on ? "''" : "'" + esc(nm).replace(/'/g, '&#39;') + "'") + ')">' +
          '<span class="facet-value-label">' + esc(nm) + '</span>' +
          '<span class="facet-value-count">' + n + '</span></button>';
      }).join('') + '</div>';
      return facetGroupShell(FACET_GROUP_FAILING_CHECK, 'Failing check (one at a time)',
        !!_dashFacetCollapsed[FACET_GROUP_FAILING_CHECK], body);
    }

    /* How long a hive must have been upgrading before the facet's tooltip
       calls the fleet out as wedged rather than busy. Same limit the counter
       and the hub's stuck-upgrade alert use — one definition of "too long",
       reused, so the rail, the row and the alert panel cannot disagree. */
    var UPGRADE_FACET_STUCK_MS = UPGRADE_ELAPSED_RED_MS;

    /* renderUpgradeFacetGroup renders the upgrade-state picker. Single-select,
       ANDed against the health chips like the failing-check group.

       The 'upgrading' value additionally reports how many of those hives are
       past the stuck limit, because the whole point of this filter is catching
       a rollout that has stopped moving — a bare count of 15 upgrading hives
       looks identical whether they are 10 seconds or 40 minutes in.

       Returns '' when nothing is upgrading or queued AND no filter is on, so a
       settled fleet sees no extra chrome; renders while the filter is on even
       at count 0 so it can always be clicked back off. */
    function renderUpgradeFacetGroup(assignedNoPlaceholders) {
      var hives = assignedNoPlaceholders || [];
      var counts = {};
      counts[UPGRADE_FILTER_UPGRADING] = 0;
      counts[UPGRADE_FILTER_QUEUED] = 0;
      var stuck = 0;
      var now = Date.now();
      for (var i = 0; i < hives.length; i++) {
        var st = hiveUpgradeState(hives[i]);
        if (!st) continue;
        counts[st]++;
        if (st === UPGRADE_FILTER_UPGRADING) {
          var ms = upgradeElapsedMs(hives[i], now);
          /* null (unknown start time) is NOT counted as stuck — an unknown
             elapsed is not evidence of a fault, and inflating this number
             would make the warning untrustworthy. */
          if (ms !== null && ms >= UPGRADE_FACET_STUCK_MS) stuck++;
        }
      }
      var total = counts[UPGRADE_FILTER_UPGRADING] + counts[UPGRADE_FILTER_QUEUED];
      if (!total && !_dashUpgradeFilter) return '';

      var values = [
        {key: UPGRADE_FILTER_UPGRADING, label: 'Upgrading'},
        {key: UPGRADE_FILTER_QUEUED, label: 'Queued for auto-upgrade'}
      ];
      var body = '<div class="facet-values">' + (values || []).map(function(v) {
        var on = _dashUpgradeFilter === v.key;
        var n = counts[v.key] || 0;
        var tip;
        if (v.key === UPGRADE_FILTER_UPGRADING) {
          tip = n + (n === 1 ? ' hive is' : ' hives are') + ' upgrading now';
          if (stuck) {
            tip += ' — ' + stuck + ' past ' + Math.round(UPGRADE_FACET_STUCK_MS / 60000) +
              ' minutes and likely wedged rather than slow';
          }
        } else {
          tip = n + (n === 1 ? ' hive is' : ' hives are') +
            ' behind latest with auto-upgrade on, waiting to be instructed';
        }
        /* The count turns red when any upgrade is past the stuck limit, so the
           fault is visible in the rail without opening the filter. */
        var countStyle = (v.key === UPGRADE_FILTER_UPGRADING && stuck)
          ? ' style="color:var(--red);font-weight:600"' : '';
        return '<button type="button" class="facet-value' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + escAttr(tip) + '"' +
          ' onclick="toggleUpgradeFilter(' + jsArg(v.key) + ')">' +
          '<span class="facet-value-label">' + esc(v.label) + '</span>' +
          '<span class="facet-value-count"' + countStyle + '>' + n + '</span></button>';
      }).join('') + '</div>';
      return facetGroupShell(FACET_GROUP_UPGRADE, 'Upgrade state (one at a time)',
        !!_dashFacetCollapsed[FACET_GROUP_UPGRADE], body);
    }

    /* renderAdvisoryStaleFacetGroup renders the stale-advisory toggle as a
       facet group. One value, because the signal is boolean: picking it means
       "only hives whose advisory digest has gone stale".

       Returns '' when no hive is affected AND the filter is off, so a fleet
       with healthy digests sees no new chrome at all — the same self-
       suppressing contract advisoryStaleSummary() uses for the row pill. When
       the filter IS on the group always renders, even at count 0, so it can
       always be clicked back off (the failing-check group does the same). */
    function renderAdvisoryStaleFacetGroup(assignedNoPlaceholders) {
      var n = (assignedNoPlaceholders || []).filter(function(h) {
        return !!(h && h.advisoryStale);
      }).length;
      if (!n && !_dashAdvisoryStaleFilter) return '';
      var on = _dashAdvisoryStaleFilter;
      var tip = n === 1
        ? '1 hive should be posting advisory digests but its digest has gone stale'
        : n + ' hives should be posting advisory digests but their digests have gone stale';
      var body = '<div class="facet-values">' +
        '<button type="button" class="facet-value' + (on ? ' on' : '') +
        '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + esc(tip) + '"' +
        ' onclick="toggleAdvisoryStaleFilter()">' +
        '<span class="facet-value-label">Stale advisory</span>' +
        '<span class="facet-value-count">' + n + '</span></button></div>';
      return facetGroupShell(FACET_GROUP_ADVISORY_STALE, 'Advisory digest',
        !!_dashFacetCollapsed[FACET_GROUP_ADVISORY_STALE], body);
    }

    /* renderFacetRail draws the tray's body: the health chips and failing-check
       picker moved in from the old standalone bar, then the derived facet
       groups. Derived-group counts are computed over the assigned hives after
       the chips and the search box, but BEFORE the facets in the same group are
       applied — so a group keeps showing its alternatives once you pick one,
       rather than collapsing to the single value you chose. */
    function renderFacetRail(assignedAll) {
      var rail = document.getElementById('hive-facet-rail');
      /* The rail's affordance reflects filter state even when the rail body
         itself has nothing to draw, so update it before any early return. */
      renderFacetTrayAffordance();
      if (!rail) return;
      /* The health and failing-check groups filter ASSIGNED hives only — the
         unassigned pool is inventory. The caller already scopes to assigned;
         strip any placeholder defensively so the counts match the old bar. */
      var assignedReal = (assignedAll || []).filter(function(h) { return !isPlaceholderHive(h); });
      var base = (assignedAll || []).filter(hiveMatchesFilters).filter(hiveMatchesSearch);
      var head = renderStatusFacetGroup(assignedReal) + renderFailingCheckFacetGroup(assignedReal) +
        renderUpgradeFacetGroup(assignedReal) + renderAdvisoryStaleFacetGroup(assignedReal);
      if (!base.length && !Object.keys(_dashFacets || {}).length) {
        rail.innerHTML = head + clearFacetsButton();
        return;
      }
      var html = head;
      (HIVE_FACET_GROUPS || []).forEach(function(grp) {
        /* Count within this group ignoring this group's own selections. */
        var others = {};
        Object.keys(_dashFacets || {}).forEach(function(k) { if (k !== grp.key) others[k] = _dashFacets[k]; });
        var saved = _dashFacets;
        _dashFacets = others;
        var pool = base.filter(hiveMatchesFacets);
        _dashFacets = saved;

        var counts = {};
        pool.forEach(function(h) {
          var v = String(hiveFacetValues(h)[grp.key]);
          counts[v] = (counts[v] || 0) + 1;
        });
        /* Always surface a currently-selected value, even at count 0, so the
           user can always click it off again. */
        Object.keys((_dashFacets[grp.key] || {})).forEach(function(v) { if (counts[v] == null) counts[v] = 0; });
        var values = Object.keys(counts).sort();
        if (!values.length) return;
        var collapsed = !!_dashFacetCollapsed[grp.key];
        var valuesHTML = '';
        if (!collapsed) {
          valuesHTML = '<div class="facet-values">' + values.map(function(v) {
            var on = !!(_dashFacets[grp.key] && _dashFacets[grp.key][v]);
            return '<button type="button" class="facet-value' + (on ? ' on' : '') + '" aria-pressed="' + (on ? 'true' : 'false') +
              '" onclick="toggleFacetValue(\'' + esc(grp.key) + '\',\'' + esc(v).replace(/'/g, '&#39;') + '\')" title="' + esc(v) + '">' +
              '<span class="facet-value-label">' + esc(v) + '</span>' +
              '<span class="facet-value-count">' + counts[v] + '</span></button>';
          }).join('') + '</div>';
        }
        html += facetGroupShell(grp.key, grp.label, collapsed, valuesHTML);
      });
      rail.innerHTML = html + clearFacetsButton();
    }

    /* One Clear button for the whole tray. It calls clearAllHiveFilters(), not
       clearHiveFacets(), because the health chips and the failing-check picker
       now live in the same tray: a button labelled "Clear" that left two of the
       three groups still filtering would be a lie. */
    function clearFacetsButton() {
      return activeFilterCount() > 0
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="clearAllHiveFilters()">Clear all filters</button>'
        : '';
    }

    /* applyDashFilters filters the assigned hives the caller wants rendered.
       Unassigned pool placeholders are handled separately in renderHives:
       health/status/facet filters do not hide inventory, but the search box
       still narrows every displayed row so typing text cannot leave unrelated
       placeholders visible under an active search.
       For assigned rows all four narrowing mechanisms compose as an AND: the
       status chips (by state), the alert-type filter (hives carrying that
       alert), the search box and the facets. That is what "click an alert type
       to see those hives" has to mean when a chip or a search term is already
       active. */
    function applyDashFilters(hives) {
      return (hives || []).filter(function(h) {
        return hiveMatchesFilters(h) && hiveMatchesAlertFilter(h) &&
          hiveMatchesSearch(h) && hiveMatchesFacets(h);
      });
    }

    /* toggleStatusFilter flips one chip and re-renders from the unfiltered
       cache, so chips compose with whatever sort is currently applied. */
    function toggleStatusFilter(key) {
      _dashStatusFilters[key] = !_dashStatusFilters[key];
      if (!_dashStatusFilters[key]) delete _dashStatusFilters[key];
      renderHives(_allDashHives, true);
    }

    function clearStatusFilters() {
      _dashStatusFilters = {};
      /* "Clear filters" is offered from the empty state, so it must clear
         EVERY filter — leaving one on would keep the list empty and make the
         button look broken. */
      _dashFailingCheckFilter = '';
      _dashDriftFilter = '';
      /* The drift filter is being force-cleared without going through
         toggleDriftFilter, so drop any stashed search with it. */
      _driftSearchStash = null;
      _dashUpgradeFilter = '';
      _dashAdvisoryStaleFilter = false;
      renderHives(_allDashHives, true);
    }

    /* toggleUpgradeFilter selects an upgrade state, or clears it when the
       already-selected value is clicked again (single-select, like the drift
       filter). */
    function toggleUpgradeFilter(state) {
      _dashUpgradeFilter = (_dashUpgradeFilter === state) ? '' : (state || '');
      renderHives(_allDashHives, true);
    }

    /* toggleAdvisoryStaleFilter flips the stale-advisory narrowing on or off. */
    function toggleAdvisoryStaleFilter() {
      _dashAdvisoryStaleFilter = !_dashAdvisoryStaleFilter;
      renderHives(_allDashHives, true);
    }

    /* toggleDriftFilter selects (or deselects) one drift kind in the fleet
       exceptions summary.

       Pill selection also owns the search box: selecting the first pill
       stashes and clears the search (so a stale search cannot silently
       intersect with the pill filter), switching between pills keeps the
       stash, and deselecting the last pill restores the stashed text —
       unless the user typed a new search while a pill was active, in which
       case the stash was dropped and their new text wins. */
    function toggleDriftFilter(kind) {
      var prev = _dashDriftFilter;
      _dashDriftFilter = (_dashDriftFilter === kind) ? '' : kind;
      var searchEl = document.getElementById('hive-search');
      if (!prev && _dashDriftFilter) {
        /* First pill selected: stash the search and clear it. */
        _driftSearchStash = _dashSearchQuery;
        _dashSearchQuery = '';
        if (searchEl) searchEl.value = '';
      } else if (prev && !_dashDriftFilter) {
        /* Last pill deselected: restore the stashed search, if it is still
           ours to restore. */
        if (_driftSearchStash !== null) {
          _dashSearchQuery = _driftSearchStash;
          if (searchEl) searchEl.value = _dashSearchQuery;
        }
        _driftSearchStash = null;
      }
      renderHives(_allDashHives, true);
    }

    /* renderDriftSummary draws the "N hives need attention" strip above the
       table, broken down by drift kind and clickable to filter.

       Counts are computed over the ASSIGNED set the caller passes, matching
       the status chips: an unassigned placeholder is never flagged for the
       claimed-hive concerns (no App, ACMM 0, no agents) server-side, and
       scoping the summary the same way keeps the headline count equal to the
       number of rows a human can actually act on. */
    function renderDriftSummary(assignedHives) {
      var el = document.getElementById('hive-drift-summary');
      if (!el) return;
      var hives = assignedHives || [];

      var hivesWithDrift = 0;
      var worstOverall = '';
      var kindCounts = {};   // kind -> number of HIVES carrying it
      var kindWorst = {};    // kind -> worst severity seen for that kind
      for (var i = 0; i < hives.length; i++) {
        var d = driftOf(hives[i]);
        if (!d.count) continue;
        hivesWithDrift++;
        if (DRIFT_SEVERITY_ORDER.indexOf(d.worstSeverity) >= 0 &&
            (worstOverall === '' || DRIFT_SEVERITY_ORDER.indexOf(d.worstSeverity) < DRIFT_SEVERITY_ORDER.indexOf(worstOverall))) {
          worstOverall = d.worstSeverity;
        }
        /* A hive can legitimately report the same kind once only, but count
           distinct kinds per hive defensively so a duplicated signal cannot
           inflate the breakdown past the headline. */
        var seenKinds = {};
        for (var j = 0; j < d.signals.length; j++) {
          var s = d.signals[j] || {};
          if (!s.kind || seenKinds[s.kind]) continue;
          seenKinds[s.kind] = true;
          kindCounts[s.kind] = (kindCounts[s.kind] || 0) + 1;
          var prev = kindWorst[s.kind];
          if (!prev || DRIFT_SEVERITY_ORDER.indexOf(s.severity) < DRIFT_SEVERITY_ORDER.indexOf(prev)) {
            kindWorst[s.kind] = s.severity;
          }
        }
      }

      // Hide the strip only when there is nothing to attend to AND no attention
      // filter is currently applied. If a filter IS active (e.g. the user clicked
      // "upgrading" and the count has since dropped to 0), the strip MUST stay so
      // its chip + "Show all" escape remain reachable — otherwise the filter keeps
      // narrowing the list with no visible way to turn it off.
      if (!hivesWithDrift && !_dashDriftFilter) {
        el.style.display = 'none';
        el.innerHTML = '';
        return;
      }
      el.style.display = '';

      /* Sort the breakdown worst-severity-first, then by how many hives are
         affected, then by label — so the most urgent, most widespread problem
         is always the first thing read. */
      var kinds = Object.keys(kindCounts).sort(function(a, b) {
        var sa = DRIFT_SEVERITY_ORDER.indexOf(kindWorst[a]);
        var sb = DRIFT_SEVERITY_ORDER.indexOf(kindWorst[b]);
        if (sa !== sb) return sa - sb;
        if (kindCounts[b] !== kindCounts[a]) return kindCounts[b] - kindCounts[a];
        return driftKindLabel(a).localeCompare(driftKindLabel(b));
      });
      // Keep the ACTIVE filter's chip listed even after its count falls to 0, so
      // it can always be clicked off (mirrors renderFailingCheckFacetGroup). Without
      // this the chip vanishes the moment its last hive clears while the filter is
      // still applied, stranding the user on an empty, un-clearable filtered view.
      if (_dashDriftFilter && kinds.indexOf(_dashDriftFilter) === -1) {
        kinds.unshift(_dashDriftFilter);
        if (kindCounts[_dashDriftFilter] == null) kindCounts[_dashDriftFilter] = 0;
        if (kindWorst[_dashDriftFilter] == null) kindWorst[_dashDriftFilter] = 'info';
      }

      var chips = kinds.map(function(k) {
        var color = DRIFT_SEVERITY_COLORS[kindWorst[k]] || DRIFT_SEVERITY_COLORS.info;
        var on = _dashDriftFilter === k;
        var cls = on ? 'filter-chip on' : 'filter-chip';
        return '<button type="button" class="' + cls + '" aria-pressed="' + (on ? 'true' : 'false') +
          '" onclick="toggleDriftFilter(\'' + esc(k) + '\')" style="--chip-color:' + color + '">' +
          '<span class="filter-chip-dot" style="background:' + color + '"></span>' +
          esc(driftKindLabel(k)) + '<span class="filter-chip-count">' + kindCounts[k] + '</span></button>';
      }).join('');

      var headColor = DRIFT_SEVERITY_COLORS[worstOverall] || DRIFT_SEVERITY_COLORS.info;
      var headline = hivesWithDrift === 1
        ? '1 hive needs attention'
        : hivesWithDrift + ' hives need attention';
      var clearBtn = _dashDriftFilter
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="toggleDriftFilter(\'' + esc(_dashDriftFilter) + '\')">Show all</button>'
        : '';

      el.innerHTML =
        '<div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">' +
        '<span style="color:' + headColor + ';font-weight:600;font-size:0.8rem">⚠ ' + esc(headline) + '</span>' +
        '<span style="color:var(--muted);font-size:0.7rem">configuration drift detected from the fleet norm</span>' +
        '</div>' +
        '<div class="filter-chips" style="margin-top:8px">' + chips + clearBtn + '</div>';
    }

    /* renderStatusFilterBar draws the "Showing X of Y" summary and the Clear
       button. The health chips and the failing-check picker used to live here as
       standalone chip rows; they now render inside the facet tray
       (renderStatusFacetGroup / renderFailingCheckFacetGroup). What stays is the
       one thing that must NOT be hidden behind a collapsed tray: the statement
       that the list is currently narrowed, and the way back out.

       Unassigned placeholders are excluded from the totals — they are
       inventory, never filtered by these controls. */
    function renderStatusFilterBar(allHivesIn, shownCount) {
      var bar = document.getElementById('hive-filter-bar');
      if (!bar) return;
      var allHives = (allHivesIn || []).filter(function(h) { return !isPlaceholderHive(h); });
      /* activeFilterCount covers EVERY narrowing control — the health chips,
         the failing-check picker, drift, the alert drill-down, the search box
         and the facets. Omitting one would hide the Clear button while the list
         is still filtered, which reads as hives having disappeared. */
      var activeN = activeFilterCount();
      var anyActive = activeN > 0;
      var total = (allHives || []).length;
      var summary = anyActive
        ? 'Showing ' + shownCount + ' of ' + total + ' assigned hives' +
          ' &middot; ' + activeN + (activeN === 1 ? ' filter active' : ' filters active')
        : total + (total === 1 ? ' assigned hive' : ' assigned hives');
      var clearBtn = anyActive
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="clearAllHiveFilters()">Clear filters</button>'
        : '';
      bar.innerHTML = '<span class="filter-summary">' + summary + '</span>' + clearBtn;
      /* The bar no longer carries chip rows, so on a fleet with no assigned
         hives it would render as an empty flex box still claiming its
         margin-bottom. Collapse it outright in that case rather than leaving a
         gap above the table. */
      bar.style.display = total ? '' : 'none';
    }

    /* renderViewBar draws the group-by <select> and the saved-view controls.
       Every user-controlled string here — saved-view names the operator typed —
       is escaped on render; group-by labels are our own constants but go
       through esc() too so the rule needs no exceptions. */
    function renderViewBar() {
      var bar = document.getElementById('hive-view-bar');
      if (!bar) return;
      var opts = (HIVE_GROUP_DIMENSIONS || []).map(function(d) {
        return '<option value="' + esc(d.key) + '"' + (d.key === _dashGroupBy ? ' selected' : '') + '>' + esc(d.label) + '</option>';
      }).join('');
      var groupCtl = '<label class="view-ctl"><span class="view-ctl-label">Group by</span>' +
        '<select id="hive-group-by" class="view-select" onchange="setDashGroupBy(this.value)">' + opts + '</select></label>';

      var viewOpts = '<option value="">No saved view</option>' + (_dashSavedViews || []).map(function(v) {
        var isDefault = v.name === _dashDefaultView;
        return '<option value="' + esc(v.name) + '"' + (v.name === _dashActiveView ? ' selected' : '') + '>' +
          esc(v.name) + (isDefault ? ' ★' : '') + '</option>';
      }).join('');
      var hasSel = !!_dashActiveView && !!findSavedView(_dashActiveView);
      var dis = hasSel ? '' : ' disabled';
      var defaultLabel = (hasSel && _dashDefaultView === _dashActiveView) ? 'Unset default' : 'Set default';
      var viewCtl = '<label class="view-ctl"><span class="view-ctl-label">View</span>' +
        '<select id="hive-saved-view" class="view-select" onchange="onSavedViewPick(this.value)">' + viewOpts + '</select></label>' +
        '<button type="button" class="view-btn" onclick="saveCurrentView()" title="Save the current grouping, filters and sort as a named view">Save view</button>' +
        '<button type="button" class="view-btn" onclick="renameSavedView()"' + dis + '>Rename</button>' +
        '<button type="button" class="view-btn" onclick="deleteSavedView()"' + dis + '>Delete</button>' +
        '<button type="button" class="view-btn" onclick="toggleDefaultView()"' + dis +
        ' title="Apply this view automatically the next time My Hives loads">' + defaultLabel + '</button>';

      bar.innerHTML = '<div class="view-ctls">' + groupCtl + '</div><div class="view-ctls">' + viewCtl + '</div>';
    }

    /* hiveProvisionTime returns when this hive came into existence, as epoch
       ms, or null when that is genuinely unknown.

       Why registeredAt and NOT the per-hive meta.json created_at: created_at is
       declared on SaaSHive and written on exactly one provisioning path, so
       every hive that predates it — or that was created any other way — carries
       the empty string. On the live fleet at the time of writing, 26 of 50
       meta.json files have "created_at": "", including long-running assigned
       hives, while registeredAt was populated on 50 of 50 registry entries.
       registeredAt is also preserved across heartbeats (server.go copies it
       from the previous entry rather than restamping it), so it stays the
       first-seen time instead of drifting to "now" on every beat.

       Returns null — never NaN and never a rendered 'Invalid Date' — for a
       missing or unparseable value, so the caller can order those rows
       explicitly instead of comparing against NaN, which is false in both
       directions and would make the sort non-deterministic. */
    function hiveProvisionTime(h) {
      var raw = h && h.registeredAt;
      if (typeof raw !== 'string' || !raw) return null;
      var t = Date.parse(raw);
      return isNaN(t) ? null : t;
    }

    /* sortedDashHives applies the CURRENT sort key/direction to the unfiltered
       cache. Split out of sortDashHives so restoring a saved view (which sets
       the key/direction directly rather than by clicking a header) reproduces
       the same ordering without re-implementing the comparator. */
    function sortedDashHives() {
      var key = _dashSortKey;
      if (!key) return _allDashHives;
      return (_allDashHives || []).slice().sort(function(a, b) {
        if (key === 'journey') {
          /* journey is an object, so sort by how urgent it is rather than by
             stringifying it. Higher = needs attention sooner, so the default
             ascending sort puts on-track hives first and the most-escalated
             last; click again to bring the problems to the top. */
          var ja = journeySortValue(a && a.journey);
          var jb = journeySortValue(b && b.journey);
          return _dashSortAsc ? ja - jb : jb - ja;
        }
        if (key === 'registeredAt') {
          /* Provision time. Compared as epoch ms, not as text: registeredAt is
             RFC3339 so it happens to sort lexically today, but a value that
             ever arrives with an offset ('...+02:00') or without the 'Z' would
             collate wrong, and a string compare cannot express "unknown last".

             Hives with no parseable timestamp sort LAST in both directions
             rather than clumping at whichever end '' collates to — they are
             the least informative rows, so they stay out of the way whether
             the operator asked for oldest-first or newest-first. */
          var ra = hiveProvisionTime(a);
          var rb2 = hiveProvisionTime(b);
          if (ra === null && rb2 === null) return 0;
          if (ra === null) return 1;
          if (rb2 === null) return -1;
          return _dashSortAsc ? ra - rb2 : rb2 - ra;
        }
        if (key === 'quadrant' || key.indexOf('quadrant') === 0) {
          /* Quadrant sorts rank by composite or by one axis. A hive with no
             quadrant — unscored, or one the viewer may not see — sorts LAST in
             both directions rather than clumping at whichever end 0 collates
             to. It is the least informative row either way, so it stays out of
             the operator's path whether they asked for strongest or weakest.
             Sorting by weakest axis is the point of the column: it turns the
             table into a worklist. */
          var qa = quadrantSortValue(a, key);
          var qb = quadrantSortValue(b, key);
          if (qa === null && qb === null) return 0;
          if (qa === null) return 1;
          if (qb === null) return -1;
          return _dashSortAsc ? qa - qb : qb - qa;
        }
        var va = key === 'name' ? hiveNameSortValue(a) : ((a && a[key]) || '');
        var vb = key === 'name' ? hiveNameSortValue(b) : ((b && b[key]) || '');
        if (typeof va === 'number' && typeof vb === 'number') return _dashSortAsc ? va - vb : vb - va;
        return _dashSortAsc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
      });
    }

    /* nameEditAffordance renders the inline pencil that lets an OWNER (or the hub
       admin) rename a hive from its top line. It is a display-only convenience —
       the server's owner-or-admin gate on PUT /name is the real boundary — so a
       non-owner simply gets no pencil, and even if they forged the request the
       handler rejects it 403. Empty string for anyone who cannot rename, so those
       rows are pixel-identical to today. Gated the same way as the row's other
       owner-only actions (h.role === 'owner' || _isAdmin — see isBulkEligible).
       stopPropagation keeps the click from triggering the row's own open/SSO. */
    function canRenameHive(h) {
      return !!h && (h.role === 'owner' || _isAdmin);
    }
    function nameEditAffordance(h) {
      if (!canRenameHive(h)) return '';
      return ' <span onclick="event.stopPropagation();dashRenameHive(' + jsArg(h.id) + ')" ' +
        'title="Rename this hive" role="button" tabindex="0" ' +
        'style="cursor:pointer;opacity:0.45;font-size:0.75rem;vertical-align:middle;margin-left:2px">' +
        '&#9998;</span>';
    }

    /* dashRenameHive prompts for a new name and PUTs it to the rename endpoint.
       A blank submission clears the custom name (the row falls back to the
       org/repo label), which the server accepts deliberately. Cancel (null)
       leaves the hive untouched. On success we reload so every viewer-derived
       surface (label, sort value) re-renders from the fresh registry. */
    async function dashRenameHive(id) {
      var h = (_allDashHives || []).reduce(function(m, x) { return x && x.id === id ? x : m; }, null) || {};
      var current = (h.projectName || h.project_name || '');
      var next = await hivePrompt('Rename hive', current, {ok: 'Rename'});
      if (next === null) return; /* cancelled */
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/name', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({project_name: next})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to rename hive', 'error'); loadHives(); return; }
        hiveToast(next ? ('Renamed to ' + next) : 'Name cleared', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadHives(); }
    }

    /* hiveLabel derives the two-line name-cell label from the hive's PROJECT
       (org + primary repo) rather than by splitting h.name.

       Why not h.name: the hub synthesises name as org + "/" + primaryRepo on
       every heartbeat (see RegistryEntry construction in server.go), so
       splitting it back apart is a lossy round-trip of the two fields we
       already have on the row — and it breaks whenever either field itself
       contains a slash. Live fleet evidence: a URL-parsing bug put a HOST in
       the org position, so the listing showed 'github.ibm.com / forthehive'.
       Reading org/primaryRepo directly makes the label reflect the project.

       Precedence, most specific first:
         1. org + primaryRepo  — the real project, the whole point
         2. h.name             — a hive that reported a name but no project
         3. h.id               — last resort, never blank

       Returns {line1, line2}. line2 is '' whenever there is no SECOND thing
       worth saying, so the caller never renders an empty link or a stray '/'.
       An unassigned placeholder (org 'available-*' or 'placeholder', no
       primaryRepo) therefore keeps showing its org on line 1 with no empty
       second line — the same headline it shows today.

       primaryRepo is rendered VERBATIM, including any embedded slash
       ('enricom-ibm/jackrabbit' is a real live value produced by the same
       corruption). Taking only the last segment would silently hide half of
       a legitimate value, and the repo path is what the adjacent GitHub link
       resolves to — the label and the link must agree. */
    function hiveLabel(h) {
      if (!h) return { line1: '', line2: '' };
      var org = h.org || '';
      var repo = h.primaryRepo || '';
      /* Second line is the PRIMARY REPO rendered as a real GitHub path, so the
         row states the full project. Fall back to repos[0] when primaryRepo is
         unset, and never emit a dangling slash: with only one half known, show
         that half alone; with neither, leave line2 empty.

         DOUBLING GUARD (mirrors Go repoDisplayLine): primaryRepo is stored
         verbatim and, for some hives, already carries a full 'owner/repo' path.
         Live fleet evidence: a github.io/GHE hive was recorded with
         org='castrojo.github.io' (a HOST wrongly parsed into the org field) and
         primaryRepo='castrojo/endusers' (already owner/repo). Joining org + '/' +
         repo produced 'castrojo.github.io/castrojo/endusers' — the host posing as
         an org, owner doubled. When repo already contains a slash it is a
         complete path and is used as-is; only a bare repo name is qualified with
         the org. Keep this in sync with hub.repoDisplayLine. */
      var repoLine = '';
      var fallbackRepo = repo || ((h.repos || [])[0] || '');
      if (fallbackRepo && fallbackRepo.indexOf('/') !== -1) { repoLine = fallbackRepo; }
      else if (org && fallbackRepo) { repoLine = org + '/' + fallbackRepo; }
      else if (org || fallbackRepo) { repoLine = org || fallbackRepo; }
      /* Top line is the operator-editable hive name (project_name / projectName)
         when set. Blank keeps EXACTLY today's fallback: the org, else the repo,
         else the synthesized name, else the id — so an un-renamed hive is
         pixel-identical to before this feature. */
      var custom = String(h.projectName || h.project_name || '').trim();
      if (custom) return { line1: custom, line2: repoLine };
      var fallback = org || repo || h.name || h.id || '';
      /* When line1 falls back to the org/repo identity, avoid repeating org on
         both lines: if the fallback IS the org and we already have a repoLine,
         line1 keeps the org and line2 shows org/repo — the same two facts the
         old label showed (org on top, repo below), just with the repo now
         qualified by its org. */
      return { line1: fallback, line2: (repoLine === fallback ? '' : repoLine) };
    }

    /* hiveNameSortValue is the value the Hive column SORTS on. It must track
       the label the name cell DISPLAYS, so it delegates to hiveLabel —
       otherwise clicking "Hive ⇅" orders rows by text nobody can see.

       Sorting on the raw h.name is what diverges: the hub synthesises name as
       org + "/" + primaryRepo, so a hive with no org sorts under "/repo" (the
       leading slash collates before every letter) and a hive with neither
       sorts under "" (always first), while both DISPLAY an ordinary word. This
       reproduces hiveLabel's precedence and joins the two lines with a space,
       so the sort reads top line first, then second line — exactly how the
       cell is read. */
    function hiveNameSortValue(h) {
      var label = hiveLabel(h);
      return label.line2 ? label.line1 + ' ' + label.line2 : label.line1;
    }

    /* quadrantSortValue resolves a quadrant sort key to a number, or null when
       the hive has nothing to rank on that key.

       Null rather than 0 for an unscored axis, deliberately: zero is a real
       score and would place an unmeasured hive among the genuinely weak ones,
       which is precisely the confusion the whole scored/unscored split exists
       to prevent. */
    function quadrantSortValue(h, key) {
      var q = h && h.quadrant;
      if (!q || !q.scored_axes) return null;
      if (key === 'quadrant') return q.composite;
      var name = key.slice('quadrant'.length).toLowerCase();
      var a = quadrantAxis(q, name);
      return a.scored ? a.score : null;
    }

    function sortDashHives(key) {
      if (_dashSortKey === key) { _dashSortAsc = !_dashSortAsc; } else { _dashSortKey = key; _dashSortAsc = true; }
      persistHiveSort();
      renderHives(sortedDashHives(), true);
    }

    /* subSort renders ONE inline, clickable sort control for a combined-column
       header. A folded column (Maturity, Activity) hosts two or three sort keys,
       so the whole <th> can no longer own a single onclick — each key gets its
       own ⇅ span instead, keeping every folded sort reachable directly from the
       header exactly as the standalone columns' ⇅ were.

       key      — the sortDashHives sort key (a fixed literal, never user input);
       label    — the visible ⇅ text (e.g. 'Prov ⇅');
       titleRaw — the hover text, inserted VERBATIM into the title attribute to
                  match how the original standalone headers embedded literals
                  (some carry pre-encoded entities like &apos;). Callers pass
                  trusted string constants only.

       event.stopPropagation() so a sub-sort inside a <th> that ALSO has its own
       onclick (Uptime hosts the Prov sub-sort) fires only its own key, not the
       parent's. jsArg supplies the quotes around the key so an apostrophe could
       never break out of the handler. */
    function subSort(key, label, titleRaw) {
      return '<span onclick="event.stopPropagation();sortDashHives(' + jsArg(key) + ')" ' +
        'style="cursor:pointer;font-weight:400;color:var(--muted);margin-left:2px"' +
        (titleRaw ? ' title="' + titleRaw + '"' : '') + '>' + label + '</span>';
    }

    /* stackHeader lays a combined column's header out on TWO lines instead of one
       inline string: the title on line 1, its sub-sort ⇅ chips on line 2. The
       inline form ("Uptime ⇅ Prov ⇅", "Activity Iss ⇅ PRs ⇅ Ctr ⇅") set each
       folded column's width from the full concatenated string, which made the
       fleet table far wider than it needed to be. Stacking makes the column width
       track the widest SINGLE line instead.

       .hive-table th sets white-space:nowrap, which would keep everything on one
       line; a flex column overrides that for layout, and the chip row is allowed
       to wrap (flex-wrap) so three chips fold onto a further line on a truly
       narrow table rather than forcing the column wide. Colours come from the
       existing --muted token and the inherited th colour, so both themes track.

       titleHTML    — the line-1 content. May be plain text (e.g. 'Maturity') or
                      an already-built clickable span; inserted VERBATIM, callers
                      pass trusted markup only.
       subSortsHTML — the line-2 content: one or more subSort() spans (each already
                      carries its own onclick + event.stopPropagation()). Passed
                      through unchanged, so every folded sort stays reachable and
                      the stopPropagation contract is untouched. */
    function stackHeader(titleHTML, subSortsHTML) {
      return '<span style="display:inline-flex;flex-direction:column;align-items:center;gap:1px;line-height:1.15">' +
        '<span style="white-space:nowrap">' + titleHTML + '</span>' +
        '<span style="display:inline-flex;flex-wrap:wrap;justify-content:center;gap:2px;font-size:0.9em;color:var(--muted)">' +
        subSortsHTML + '</span></span>';
    }

    /* persistHiveSort records the operator's EXPLICIT sort choice. Written only
       from sortDashHives (a header click), never from loadHiveSortPrefs, so
       restoring a preference can't rewrite it and a first visit leaves the key
       absent rather than storing the default — which is what lets
       loadHiveSortPrefs tell "never chose" apart from "chose the default". */
    function persistHiveSort() {
      lsSetJSON(LS_HIVE_SORT, {key: _dashSortKey, asc: _dashSortAsc});
    }

    /* ---- Scroll position across refresh --------------------------------
       "Location on page" is the WINDOW's scroll offset, not a container's:
       .table-wrap is overflow:visible at desktop widths (it only becomes an
       overflow-x scroller in the narrow media query, and that axis is
       horizontal), so the hive list scrolls the document itself.

       Writes are throttled through requestAnimationFrame because scroll fires
       at input rate and localStorage.setItem is synchronous — persisting on
       every event would jank the scroll it is trying to record. */
    var HIVE_SCROLL_MAX_AGE_MS = 30 * 60 * 1000; /* 30 min — see readHiveScroll */
    var HIVE_SCROLL_RESTORE_FRAMES = 3;          /* see restoreHiveScroll */
    var _hiveScrollWritePending = false;
    var _hiveScrollRestored = false;

    function persistHiveScroll() {
      if (_hiveScrollWritePending) return;
      _hiveScrollWritePending = true;
      window.requestAnimationFrame(function() {
        _hiveScrollWritePending = false;
        var y = window.pageYOffset || document.documentElement.scrollTop || 0;
        lsSetJSON(LS_HIVE_SCROLL, {y: y, t: Date.now()});
      });
    }

    /* readHiveScroll returns the stored offset, or 0 when there is nothing
       usable. Stale entries are discarded: coming back to the dashboard after
       hours, the fleet has changed underneath and the old offset points at
       unrelated rows, so landing at the top is the honest result. Guards the
       shape too — a hand-edited or truncated value must not yield NaN, which
       window.scrollTo would silently treat as 0 anyway but which would also
       poison the comparison below. */
    function readHiveScroll() {
      var s = lsGetJSON(LS_HIVE_SCROLL, null);
      if (!s || typeof s !== 'object') return 0;
      var y = Number(s.y), t = Number(s.t);
      if (!isFinite(y) || y <= 0) return 0;
      if (!isFinite(t) || (Date.now() - t) > HIVE_SCROLL_MAX_AGE_MS) return 0;
      return y;
    }

    /* restoreHiveScroll re-applies the saved offset ONCE per page load.

       Why not a single scrollTo at init: the rows are painted asynchronously
       (paintCachedHives, then loadHives' await, then renderHives), and the
       document is only as tall as what has been painted so far. Scrolling to
       y before the table exists clamps to the current maximum — silently
       landing at the top, which is exactly the bug this is meant to fix.

       So it retries across a few animation frames and stops as soon as the
       document is actually tall enough for the offset to survive, which is the
       observable definition of "the rows have rendered". The frame budget is
       small and bounded so a genuinely shorter list (hives removed since) costs
       three frames and then gives up rather than fighting the user forever. */
    function restoreHiveScroll() {
      if (_hiveScrollRestored) return;
      var y = readHiveScroll();
      if (!y) { _hiveScrollRestored = true; return; }
      var attempts = 0;
      var tryScroll = function() {
        var maxY = Math.max(0, document.documentElement.scrollHeight - window.innerHeight);
        if (maxY >= y) {
          window.scrollTo(0, y);
          _hiveScrollRestored = true;
          return;
        }
        if (++attempts >= HIVE_SCROLL_RESTORE_FRAMES) {
          /* Never tall enough — go as far as the page allows so the operator
             at least lands near where they were. */
          window.scrollTo(0, maxY);
          _hiveScrollRestored = true;
          return;
        }
        window.requestAnimationFrame(tryScroll);
      };
      window.requestAnimationFrame(tryScroll);
    }

    /* loadHiveSortPrefs resolves the stored sort against the A-Z default.

       Precedence: a PERSISTED choice wins, the default applies only when there
       is nothing usable stored — first visit, cleared storage, or a value that
       no longer validates. Applying A-Z unconditionally would undo the operator's
       choice on every refresh, which is the exact complaint this fixes.

       Every failure mode degrades to the default rather than throwing: lsGetJSON
       already swallows disabled storage and malformed JSON, and isHiveSortKey
       rejects a stale key so a removed column cannot leave the table sorting on
       a property no row has. */
    function loadHiveSortPrefs() {
      var stored = lsGetJSON(LS_HIVE_SORT, null);
      if (stored && typeof stored === 'object' && isHiveSortKey(stored.key)) {
        _dashSortKey = stored.key;
        /* Only an explicit false flips direction — a stored object missing
           'asc' still means ascending. */
        _dashSortAsc = stored.asc !== false;
        return;
      }
      _dashSortKey = HIVE_SORT_DEFAULT_KEY;
      _dashSortAsc = HIVE_SORT_DEFAULT_ASC;
    }

    /* ---- Usage attribution panel ----------------------------------------
       Token rollups by org / owner / cluster. TOKENS ONLY: the hub receives a
       single blended token total per hive with no per-model split, so no
       dollar figure is derivable here (see usage.go). The panel shows the
       server's currencyNote rather than inventing a rate. */

    /* USAGE_TOP_N is how many rollup rows each dimension shows collapsed. At
       42+ hives the full org/owner lists do not fit on screen; 5 keeps the
       three tables side-by-side and readable, with "Show all" to expand. */
    var USAGE_TOP_N = 5;
    /* USAGE_ZERO_TOP_N bounds the zero-consumption list the same way. Slightly
       larger than USAGE_TOP_N because this list is the actionable one — these
       are claimed hives burning nothing, i.e. possibly broken. */
    var USAGE_ZERO_TOP_N = 8;
    /* USAGE_SPARK_MIN_POINTS is the minimum sampled points before a trend line
       is drawn. Two points is the honest minimum for a line — with one sample
       there is no trend, only a dot, and drawing it would imply history the
       hub does not have. */
    var USAGE_SPARK_MIN_POINTS = 2;

    var _usageData = null;
    var _usageExpanded = {};

    function toggleUsageSection(key) {
      _usageExpanded[key] = !_usageExpanded[key];
      renderUsagePanel();
    }

    /* jumpToUsageBucket links a usage rollup row back to the fleet table.
       One contributing hive → jump straight to (and highlight) its row via
       the same jumpToHiveRow the alerts panel uses. Several → put the bucket
       key in the hive search box (it matches org, repo, cluster and user)
       so the table narrows to exactly that bucket's hives, then scroll to
       the table. Falls back to the first id when the key is the synthetic
       "(unattributed)" bucket, which the search text would never match. */
    function jumpToUsageBucket(key, hiveIds) {
      hiveIds = hiveIds || [];
      if (hiveIds.length === 1) { jumpToHiveRow(hiveIds[0], key); return; }
      if (!hiveIds.length) return;
      var isSynthetic = !key || key.charAt(0) === '(';
      if (isSynthetic) { jumpToHiveRow(hiveIds[0], key); return; }
      var el = document.getElementById('hive-search');
      if (el) el.value = key;
      _dashSearchQuery = key;
      /* A search only narrows what is rendered — make sure hidden sections
         cannot swallow the result, mirroring jumpToHiveRow's widening. */
      expandAllHiveSections();
      renderHives(_allDashHives, true);
      var table = document.querySelector('.hive-table') || document.getElementById('hive-search-row');
      if (table) table.scrollIntoView({behavior: 'smooth', block: 'start'});
      hiveToast('Hive list filtered to ' + key + ' (' + hiveIds.length + ' hives) — Clear to reset', 'info');
    }

    /* Renders one rollup dimension as a compact table. rows are already sorted
       by consumption descending server-side. */
    function renderUsageSection(title, rows, key) {
      rows = rows || [];
      if (!rows.length) {
        return '<div style="flex:1;min-width:200px">' +
          '<div style="font-size:0.72rem;text-transform:uppercase;letter-spacing:0.04em;color:var(--muted);margin-bottom:6px">' + esc(title) + '</div>' +
          '<div style="color:var(--muted);font-size:0.75rem;padding:4px 0">No data</div></div>';
      }
      var expanded = !!_usageExpanded[key];
      var shown = expanded ? rows.length : Math.min(rows.length, USAGE_TOP_N);
      var html = '';
      for (var i = 0; i < shown; i++) {
        var r = rows[i] || {};
        var pct = Number(r.sharePct) || 0;
        /* label (when present) is the server-resolved display name for an
           opaque OIDC key. Display only — jumpToUsageBucket and hive-id
           filtering keep operating on the RAW r.key. */
        var rLabel = String(r.label || r.key || '');
        /* The key cell links back to the fleet table: one hive jumps to its
           row, several filter the table to the bucket. encodeURIComponent
           then esc() so a user-influenced key can neither break out of the
           attribute nor the onclick string. */
        var ids = r.hiveIds || [];
        var keyCell;
        if (ids.length) {
          keyCell = '<a href="#" onclick="jumpToUsageBucket(decodeURIComponent(\'' + esc(encodeURIComponent(String(r.key || ''))) + '\'),JSON.parse(decodeURIComponent(\'' + esc(encodeURIComponent(JSON.stringify(ids))) + '\')));return false"' +
            ' style="color:inherit;text-decoration:none;border-bottom:1px dotted var(--muted)"' +
            ' title="' + esc(rLabel) + ' — show ' + (ids.length === 1 ? 'this hive' : 'these ' + ids.length + ' hives') + ' in the table">' + esc(rLabel) + '</a>';
        } else {
          keyCell = esc(rLabel);
        }
        html += '<tr>' +
          '<td style="padding:2px 6px 2px 0;max-width:150px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + esc(rLabel) + '">' + keyCell + '</td>' +
          '<td style="padding:2px 6px;text-align:right;white-space:nowrap">' + fmtTokens(r.tokens) + '</td>' +
          '<td style="padding:2px 6px;text-align:right;color:var(--muted);white-space:nowrap">' + (Number(r.hives) || 0) + '</td>' +
          /* Inline share bar — cheap visual ranking without a chart library. */
          '<td style="padding:2px 0 2px 6px;width:64px">' +
            '<div style="position:relative;height:9px;background:var(--surface);border-radius:2px;overflow:hidden" title="' + pct.toFixed(1) + '% of total">' +
            '<div style="position:absolute;left:0;top:0;bottom:0;width:' + Math.max(0, Math.min(100, pct)).toFixed(1) + '%;background:var(--accent);opacity:0.65"></div>' +
            '</div></td>' +
          '<td style="padding:2px 0 2px 6px;text-align:right;color:var(--muted);font-size:0.68rem;white-space:nowrap">' + pct.toFixed(1) + '%</td>' +
        '</tr>';
      }
      var more = '';
      if (rows.length > USAGE_TOP_N) {
        more = '<div style="margin-top:4px"><a href="#" onclick="toggleUsageSection(\'' + esc(key) + '\');return false" style="font-size:0.7rem;color:var(--accent);text-decoration:none">' +
          (expanded ? 'Show top ' + USAGE_TOP_N : 'Show all ' + rows.length) + '</a></div>';
      }
      return '<div style="flex:1;min-width:200px">' +
        '<div style="font-size:0.72rem;text-transform:uppercase;letter-spacing:0.04em;color:var(--muted);margin-bottom:6px">' + esc(title) + '</div>' +
        '<table style="width:100%;border-collapse:collapse;font-size:0.75rem"><tbody>' + html + '</tbody></table>' + more +
        '</div>';
    }

    function renderUsagePanel() {
      var el = document.getElementById('usage-panel');
      if (!el) return;
      var d = _usageData;
      if (!d) { el.style.display = 'none'; return; }
      el.style.display = 'block';

      var isFleet = d.scope === 'fleet';
      var scopeLabel = isFleet
        ? 'Fleet-wide'
        : 'Your hives only';

      /* Trend: only drawn from genuinely sampled history. With fewer than
         USAGE_SPARK_MIN_POINTS points we say so instead of drawing a line. */
      var hist = d.history || [];
      var trend = '';
      if (hist.length >= USAGE_SPARK_MIN_POINTS) {
        var pts = [];
        for (var i = 0; i < hist.length; i++) {
          /* sparkline() reads .v — map the snapshot shape onto it. */
          pts.push({ t: hist[i].t, v: Number(hist[i].v) || 0 });
        }
        trend = '<span title="Fleet total, sampled every 15 min (cumulative)">' + sparkline(pts, '#3fb950', 90, 16) + '</span>';
      } else if (isFleet) {
        trend = '<span style="font-size:0.68rem;color:var(--muted)" title="Snapshots are sampled every 15 minutes; a trend line needs at least two.">trend building…</span>';
      }

      var zero = d.zeroConsumption || [];
      var zeroHtml = '';
      if (zero.length) {
        var zExpanded = !!_usageExpanded['zero'];
        var zShown = zExpanded ? zero.length : Math.min(zero.length, USAGE_ZERO_TOP_N);
        var chips = '';
        for (var z = 0; z < zShown; z++) {
          var h = zero[z] || {};
          var dot = h.online
            ? '<span style="color:#3fb950" title="Online but consuming nothing — idle or stuck">●</span>'
            : '<span style="color:var(--muted)" title="Offline">○</span>';
          chips += '<span onclick="jumpToHiveRow(decodeURIComponent(\'' + esc(encodeURIComponent(String(h.id || ''))) + '\'),decodeURIComponent(\'' + esc(encodeURIComponent(String(h.name || h.id || ''))) + '\'))" style="display:inline-block;padding:2px 7px;margin:2px 4px 2px 0;background:var(--surface);border:1px solid var(--border);border-radius:10px;font-size:0.7rem;cursor:pointer" title="' + esc((h.org || '') + ((h.ownerName || h.owner) ? ' · ' + (h.ownerName || h.owner) : '')) + ' — show this hive in the table">' + dot + ' ' + esc(h.name || h.id) + '</span>';
        }
        var zMore = '';
        if (zero.length > USAGE_ZERO_TOP_N) {
          zMore = ' <a href="#" onclick="toggleUsageSection(\'zero\');return false" style="font-size:0.7rem;color:var(--accent);text-decoration:none">' +
            (zExpanded ? 'show fewer' : 'show all ' + zero.length) + '</a>';
        }
        zeroHtml = '<div style="margin-top:12px;padding-top:10px;border-top:1px solid var(--border)">' +
          '<div style="font-size:0.72rem;text-transform:uppercase;letter-spacing:0.04em;color:var(--muted);margin-bottom:6px" title="Claimed hives reporting no tokens. Unassigned pool slots are excluded — those are supposed to consume nothing.">' +
          'Zero consumption (' + zero.length + ')</div>' + chips + zMore + '</div>';
      }

      el.innerHTML =
        '<div style="background:var(--card,var(--surface));border:1px solid var(--border);border-radius:8px;padding:14px 16px">' +
          '<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-bottom:12px">' +
            '<h3 style="font-size:0.95rem;color:var(--accent);margin:0">Usage</h3>' +
            '<span style="font-size:0.7rem;color:var(--muted);border:1px solid var(--border);border-radius:10px;padding:1px 7px">' + esc(scopeLabel) + '</span>' +
            '<span style="flex:1"></span>' +
            trend +
            '<span style="font-size:0.8rem;color:var(--text)"><strong>' + fmtTokens(d.totalTokens) + '</strong> tokens' +
            ' <span style="color:var(--muted);font-size:0.72rem">across ' + (Number(d.hiveCount) || 0) + ' hive' + ((Number(d.hiveCount) || 0) === 1 ? '' : 's') + '</span></span>' +
          '</div>' +
          '<div style="display:flex;gap:20px;flex-wrap:wrap">' +
            /* byOrg buckets on org/repo (usageOrgRepoKey in usage.go), so the
               label says so; the JSON field keeps its original name. */
            renderUsageSection('By org / repo', d.byOrg, 'org') +
            renderUsageSection('By owner', d.byOwner, 'owner') +
            renderUsageSection('By cluster', d.byCluster, 'cluster') +
          '</div>' +
          zeroHtml +
          '<div style="margin-top:10px;font-size:0.66rem;color:var(--muted);line-height:1.4">' + esc(d.currencyNote || '') + '</div>' +
        '</div>';
    }

    async function loadUsage() {
      try {
        var resp = await fetch('/api/saas/usage');
        if (!resp.ok) { _usageData = null; renderUsagePanel(); return; }
        _usageData = await resp.json();
      } catch (e) {
        _usageData = null;
      }
      renderUsagePanel();
    }

    async function loadHives() {
      if (_hivesLoading) return;
      _hivesLoading = true;
      try {
        var resp = await fetch('/api/saas/my-hives');
        if (resp.status === 401) {
          window.location.href = '/login';
          return;
        }
        var data = await resp.json();
        _userQuota = data.saas_quota || 0;
        _userUsed = data.saas_used || 0;
        _allDashHives = data.hives || [];
        _hiveRegistry = data.hives || [];
        /* The fleet reference polygon behind every kite. Captured here rather
           than derived in the browser: it is the average over the SAME
           population the server scored, and recomputing it client-side from
           the rows the caller may see would silently exclude the ones they may
           not, quietly moving the reference. */
        _fleetQuadrant = data.fleet_quadrant || null;
        /* Who is logged into their hive right now (admin-only in the payload) →
           the green-dashed avatar border. A Set of lowercased usernames so the
           avatar lookup is case-insensitive, matching GitHub handle semantics. */
        _liveHiveUsers = new Set((data.live_hive_users || []).map(function(n) { return String(n).toLowerCase(); }));
        _liveEngagedUsers = new Set((data.live_engaged_users || []).map(function(n) { return String(n).toLowerCase(); }));
        /* Alerts ride along on the same payload — see handleMyHives. Normalise
           to the empty summary so every consumer can iterate without guarding. */
        _fleetAlerts = data.alerts || EMPTY_ALERT_SUMMARY;
        _hivesSummary = data.hives_summary || null;
        _latestSHA = data.latest_sha || _latestSHA;
        _stableV4SHA = data.stable_v4_sha || _stableV4SHA;
        if (data.latest_shas) _latestSHAs = data.latest_shas;
        if (data.tracked_branches) _trackedBranchesList = data.tracked_branches;
        if (data.release_channels) _releaseChannels = data.release_channels;
        if (data.channel_targets) _channelTargets = data.channel_targets;
        if (data.latest_sha_messages) _latestSHAMessages = data.latest_sha_messages;
        if (data.latest_sha_image_status) _latestImageStatus = data.latest_sha_image_status;
        _latestBuildStarted = data.latest_sha_build_started || {};
        if (data.commit_messages) _commitMessages = data.commit_messages;
        if (data.hub_auto_upgrade !== undefined) _hubAutoUpgrade = data.hub_auto_upgrade;
        if (data.upgrade_pause) _upgradePause = data.upgrade_pause;
        var shaEl = document.getElementById('latest-image-sha');
        if (shaEl) {
          var lines = '';
          var branches = Object.keys(_latestSHAs).sort();
          if (branches.length) {
            for (var bi = 0; bi < branches.length; bi++) {
              var br = branches[bi];
              var brMsg = _latestSHAMessages[br] || '';
              var brStatus = _latestImageStatus[br] || 'ready';
              var brStatusHTML = '';
              if (brStatus === 'building') {
                var _bStart = _latestBuildStarted[br];
                /* Elapsed build timer: a span the live ticker (_tickBuildTimers)
                   refreshes each second. data-build-start carries the epoch-ms
                   start so the ticker can recompute without a server round-trip.
                   Omitted when the hub hasn't reported a start time yet. */
                var _timerHTML = _bStart
                  ? '<span class="build-timer" data-build-start="' + _bStart + '" style="font-size:0.65rem;color:var(--muted);opacity:0.7;white-space:nowrap;font-variant-numeric:tabular-nums">' + fmtBuildElapsed(_bStart) + '</span>'
                  : '';
                brStatusHTML = '<span style="display:inline-block;flex:none;width:10px;height:10px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite" title="Container image for this commit is still building"></span><span style="font-size:0.65rem;color:var(--muted);opacity:0.7;white-space:nowrap">building image…</span>' + _timerHTML;
              } else if (brStatus === 'failed') {
                brStatusHTML = '<span style="color:var(--red);font-size:0.7rem;cursor:help" title="Image build failed for this commit — upgrades keep using the previous image">✗</span>';
              }
              lines += '<div style="display:flex;align-items:center;gap:6px;margin-bottom:2px"><span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3)">' + esc(br) + '</span><span style="font-family:monospace;color:var(--muted)">' + esc(_latestSHAs[br]) + '</span>' + (brMsg ? '<span style="font-size:0.7rem;color:var(--muted);opacity:0.7">: ' + esc(brMsg) + '</span>' : '') + brStatusHTML +
                /* Per-line pulls mini chart: a .line-pulls span each
                   row, filled from the cached /api/hub/image-pulls payload now
                   and refreshed by loadImagePulls() on every dashboard poll.
                   A line with no pull data renders "—", never an error. */
                '<span class="line-pulls" data-branch="' + escAttr(br) + '" style="display:inline-flex;align-items:center;margin-left:4px">' + linePullSparkHTML(br) + '</span>' +
                '</div>';
            }
          } else if (_latestSHA) {
            lines = '<span style="font-family:monospace;color:var(--muted)">' + esc(_latestSHA) + '</span>';
          }
          /* Channel block, rendered ABOVE the per-branch rows as
                 stable    ->  v4 3d31590 : <subject>
             so the moving tags read as pointers INTO the branch list below
             rather than as extra branches. The arrow column is fixed-width so
             the three channel names left-align and the targets line up.
             Nothing here assumes a channel tracks any particular branch: the
             target comes from the server's digest match, and a channel with no
             match renders its digest (or "unknown") instead of a branch. */
          var chLines = '';
          for (var ci = 0; ci < _channelTargets.length; ci++) {
            var ct = _channelTargets[ci] || {};
            var ctTarget;
            if (ct.branch) {
              var ctMsg = _latestSHAMessages[ct.branch] || '';
              ctTarget = '<span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3)">' + esc(ct.branch) + '</span><span style="font-family:monospace;color:var(--muted);margin-left:6px">' + esc(ct.sha || '') + '</span>' + (ctMsg ? '<span style="font-size:0.7rem;color:var(--muted);opacity:0.7">: ' + esc(ctMsg) + '</span>' : '');
            } else if (ct.digest) {
              /* Resolved, but to something no tracked branch points at — a
                 pinned or mid-promotion build. Show the digest so the operator
                 can still identify it; do NOT attribute it to a branch. */
              ctTarget = '<span style="font-family:monospace;color:var(--muted)" title="' + escAttr(ct.digest) + '">' + esc(shortDigest(ct.digest)) + '</span>';
            } else {
              ctTarget = '<span style="color:var(--muted);opacity:0.7;font-size:0.7rem" title="Channel tag could not be resolved on GHCR">unknown</span>';
            }
            chLines += '<div style="display:flex;align-items:center;gap:6px;margin-bottom:2px"><span style="display:inline-block;min-width:' + CHANNEL_NAME_MIN_W_PX + 'px;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(34,197,94,0.15);color:#4ade80;border:1px solid rgba(34,197,94,0.3);text-align:center" title="Release channel — a moving tag that follows whichever build is currently promoted to this track">' + esc(ct.channel) + '</span><span style="color:var(--muted);opacity:0.6;font-size:0.7rem">-&gt;</span>' + ctTarget + '</div>';
          }
          if (chLines) {
            chLines = '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:2px">Release channels:</div>' + chLines +
              '<div style="height:6px"></div>';
          }
          /* The branch rows keep their original header; the channel block gets
             its own above it, so neither reads as a subheading of the other. */
          if (lines) lines = '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:2px">Latest available images:</div>' + lines;
          lines = chLines + lines;
          shaEl.innerHTML = lines ? lines :'<div style="display:flex;align-items:center;gap:6px;font-size:0.7rem;color:var(--muted)"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite"></span>Resolving latest available images…</div>';
        }
        var hubHash = data.hub_git_hash || '';
        var hubBranch = data.hub_git_branch || 'v2';
        if (hubHash) {
          var el = document.getElementById('hub-version');
          if (el) {
            var hubBranchLatest = _latestSHAs[hubBranch] || _latestSHA;
            var hubLatestUnknown = !hubBranchLatest;
            var isCurrent = hubBranchLatest && sameShaJS(hubHash, hubBranchLatest);
            var hubUpgradeBtn = '';
            // Distinguish two states, matching the per-hive badges:
            //   - "queued"    = behind latest with auto-upgrade ON, but the
            //     rollout hasn't started yet (auto-upgrade will apply shortly).
            //   - "Upgrading" = a rollout is ACTUALLY in progress (auto OR admin).
            // The server now tells us which via hub_upgrade_state, so an AUTO
            // rollout in progress shows "Upgrading" too — previously it was
            // mislabeled "queued" because the frontend could only see the
            // admin-click flag (_hubUpgrading) and had no signal for an auto roll.
            var hubState = data.hub_upgrade_state || '';
            // Trust the server state; fall back to the optimistic admin-click flag
            // so the badge flips to "Upgrading" immediately on click, before the
            // next /api/saas/my-hives poll reports "upgrading".
            var hubIsUpgrading = hubState === 'upgrading' || _hubUpgrading;
            // The container image for the latest hub SHA can still be building
            // (same signal the per-hive rows use). While it builds there is nothing
            // to upgrade TO yet, so the pill must read "queued" (matching the per-hive
            // "building image…" state) rather than offer a clickable green "Upgrade"
            // that would pull the PREVIOUS image. Only "Upgrading" (an active rollout)
            // outranks it.
            var hubImageBuilding = (_latestImageStatus[hubBranch] || '') === 'building';
            var hubQueued = !hubIsUpgrading && (hubState === 'queued' || hubImageBuilding ||
              (hubState === '' && !isCurrent && hubBranchLatest && _hubAutoUpgrade));
            if (!isCurrent && hubBranchLatest && _isAdmin && !hubIsUpgrading && !hubQueued) {
              hubUpgradeBtn = ' <button id="hub-upgrade-btn" onclick="upgradeHub(\'' + esc(hubHash) + '\')" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem;margin-left:6px;white-space:nowrap">Upgrade</button>';
            } else if (hubIsUpgrading) {
              hubUpgradeBtn = ' <span title="Upgrading to ' + esc(hubBranchLatest || '?') + '" style="display:inline-block;padding:2px 8px;background:var(--surface);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap;opacity:0.8"><span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:3px"></span>Upgrading</span>';
            } else if (hubQueued) {
              // While the image is still building we do NOT offer click-to-upgrade-now
              // (there is no built image to roll to); once built, an admin can click.
              var hubQueuedTitle = hubImageBuilding
                ? 'Image for ' + esc(hubBranchLatest || '?') + ' is still building — upgrade will be available once it is published'
                : 'Auto-upgrade will apply ' + esc(hubBranchLatest || '?') + ' shortly' + (_isAdmin ? ' — click to upgrade now' : '');
              var hubQueuedClickable = _isAdmin && !hubImageBuilding;
              hubUpgradeBtn = ' <span title="' + hubQueuedTitle + '"' + (hubQueuedClickable ? ' onclick="upgradeHub(\'' + esc(hubHash) + '\')" style="cursor:pointer;' : ' style="') + 'display:inline-block;padding:2px 8px;background:var(--surface);color:var(--muted);border:1px dashed var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap">queued</span>';
            } else if (hubLatestUnknown && _isAdmin) {
              hubUpgradeBtn = ' <button disabled title="Waiting for latest version…" style="padding:2px 8px;background:var(--surface);color:var(--muted);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap;cursor:not-allowed;opacity:0.5">Upgrade</button>';
            }
            if (isCurrent) { _hubUpgrading = false; }
            var hubAutoCheck = '';
            if (_isAdmin) {
              hubAutoCheck = ' <label style="margin-left:6px;font-size:0.6rem;color:var(--muted);cursor:pointer;white-space:nowrap" title="Auto-upgrade hub when a new image is available"><input type="checkbox" ' + (_hubAutoUpgrade ? 'checked' : '') + ' onchange="toggleHubAutoUpgrade(this.checked)" style="vertical-align:middle;margin-right:2px;cursor:pointer">auto</label>';
              /* Upgrade kill switches (admin-only): checked = paused. Titles
                 carry who/when; the dashboard banner repeats it prominently.
                 Rendered as ONE compact bordered column so the pair reads as a
                 control cluster, stays vertically aligned with the header row,
                 and wraps as a unit instead of the second toggle dangling onto
                 its own line. */
              hubAutoCheck += ' <span style="display:inline-flex;flex-direction:column;align-items:flex-start;gap:2px;margin-left:8px;padding:3px 8px;border:1px solid var(--border);border-radius:6px;background:var(--surface);vertical-align:middle;line-height:1.2">' +
                upgradePauseToggleHTML('hub', 'hub upgrades',
                'Kill switch: freeze the hub on its current build — no self-upgrades (auto or manual) until resumed') +
                upgradePauseToggleHTML('spokes', 'spoke upgrades',
                'Kill switch: freeze ALL automatic image changes to spokes, fleet-wide, until resumed') +
                '</span>';
            }
            var hubStatusIcon = hubLatestUnknown
              ? ' <span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-left:3px" title="Resolving latest version…"></span>'
              : isCurrent ? '<span style="color:var(--green);margin-left:3px" title="hub is on latest">✓</span>' : '<span style="color:var(--red);margin-left:3px" title="hub is behind latest ' + esc(hubBranchLatest) + '">↑</span>';
            var hubBranchPill = '<span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);margin-right:4px">' + esc(hubBranch) + '</span>';
            var hubMsg = _latestSHAMessages[hubBranch] || '';
            el.innerHTML = hubBranchPill + '<span style="font-family:monospace;font-size:0.7rem;color:var(--muted)" title="' + esc(hubMsg) + '">' + esc(hubHash) + '</span>' +
              hubStatusIcon + hubUpgradeBtn + hubAutoCheck;
          }
        }
        var canCreate = _userQuota < 0 || _userQuota > _userUsed;
        var addBtn = document.getElementById('btn-add-hive');
        var requestBtn = document.getElementById('btn-request-hive');
        if (addBtn) {
          addBtn.disabled = !canCreate;
          addBtn.title = canCreate ? '' : 'No hosted quota — contact hub admin';
          /* No hosted quota: hide the dead-end self-create button entirely and
             surface the active "Request a hive" link in its place. With quota,
             keep the working + Add Hosted Hive button and hide the request link. */
          addBtn.style.display = canCreate ? '' : 'none';
        }
        if (requestBtn) {
          requestBtn.style.display = canCreate ? 'none' : '';
        }
        /* Render through sortedDashHives() rather than the raw payload so an
           active sort — whether the operator clicked a column or restored a
           saved view that carries one — survives each poll instead of snapping
           back to server order every REFRESH cycle. With no sort key set this
           returns _allDashHives unchanged, i.e. the previous behaviour. */
        renderHives(sortedDashHives());
        /* Persist AFTER a successful render: a payload we could not render must
           never be cached, or the next page load would replay the same crash
           from localStorage before the network could correct it. */
        writeHivesCache(data.hives || []);
        renderUpgradePauseBanner();
        renderPendingBanner(data.hives || []);
        renderUserAccessBanner();
        renderProvisionRequestBanner(data.my_provision_request || null);
        renderAdminProvisionRequests(data.provision_requests || []);
        loadPublicHives(data.hives || []);
        loadUsage();
        loadImagePulls();
      } catch(e) {
        /* Surface EVERY failure in the load→render path, not just fetch errors.
           The old guard only painted a message when _allDashHives was empty —
           but _allDashHives is assigned from the payload BEFORE renderHives()
           runs, so a throw inside rendering left the container showing
           "Loading your hives..." forever with nothing in the console. That is
           how an unbalanced brace in the inline JS (which silently nested a
           third of the dashboard's top-level declarations inside
           renderAlertRows) reached production unnoticed. Never swallow: log the
           real error AND give the operator a way to retry. */
        console.error('[hive] my-hives load/render failed:', e);
        renderHivesError(e);
      } finally {
        _hivesLoading = false;
      }
    }

    /* loadImagePulls fetches the per-RELEASE pull series and paints (a) a small
       inline-SVG bar chart near the header for the ACTIVE release line — the
       line the "stable" channel currently resolves to, reported by the server
       as data.line, so a v4→v5 rollover re-labels and re-buckets the chart with
       no frontend change — and (b) a mini per-line chart in each
       "Latest available images" row from data.lines. One bar per release,
       newest on the right. Gauges external adoption of the public spoke image
       (ghcr.io/hivecommons/hive) beyond the hosted fleet. Honest labelling:
       derived from GitHub's cumulative download counter (pulls, NOT unique
       downloads; GitHub publishes one package-wide counter, not per-tag). Cold
       start (fewer than two release snapshots → no window can be closed yet)
       shows "collecting…". */
    var _imagePullLines = {};   // branch → per-line series from /api/hub/image-pulls

    /* pullBarsSVG renders a per-release pull series as an inline SVG bar
       chart, newest bar highlighted, per-bar <title> tooltips with the exact
       numbers. Shared by the header widget and the per-row mini charts so the
       two can never drift apart visually. */
    function pullBarsSVG(points, BAR_W, BAR_GAP, BAR_H) {
      var BAR_PAD = 2; // px, top breathing room so the tallest bar isn't clipped
      var vals = points.map(function(p) { return Math.max(0, Number(p.pulls) || 0); });
      var maxV = Math.max.apply(null, vals);
      if (maxV <= 0) maxV = 1; // avoid divide-by-zero on an all-zero window
      var chartW = points.length * BAR_W + (points.length - 1) * BAR_GAP;
      var usableH = BAR_H - BAR_PAD;
      var bars = '';
      points.forEach(function(p, i) {
        var v = vals[i];
        var h = Math.max(1, (v / maxV) * usableH); // min 1px so a zero-pull release is still visible
        var x = i * (BAR_W + BAR_GAP);
        var y = BAR_H - h;
        var sha = esc(String(p.sha || ''));
        var tip = sha + ': ' + v + ' pulls' + (p.date ? ' (since ' + esc(String(p.date)) + ')' : '');
        // The newest bar (last) is highlighted; older ones muted.
        var fill = (i === points.length - 1) ? '#60a5fa' : 'rgba(96,165,250,0.45)';
        bars += '<rect x="' + x.toFixed(1) + '" y="' + y.toFixed(1) + '" width="' + BAR_W + '" height="' + h.toFixed(1) + '" rx="1.5" fill="' + fill + '"><title>' + tip + '</title></rect>';
      });
      return '<svg width="' + chartW + '" height="' + BAR_H + '" viewBox="0 0 ' + chartW + ' ' + BAR_H + '" ' +
        'style="display:block;overflow:visible">' + bars + '</svg>';
    }

    /* linePullSparkHTML is one branch row's mini pulls chart: compact bars plus
       the newest release's count, wrapped in a tooltip with the exact totals.
       No data for the line (a retired line pre-history, or a freshly cut line
       like v5 with no closed release window yet) renders a muted "—". */
    function linePullSparkHTML(br) {
      var s = _imagePullLines[br];
      var points = (s && s.points) || [];
      if (!s || s.collecting || points.length < 1) {
        return '<span style="color:var(--muted);opacity:0.5;font-size:0.65rem;cursor:help" title="No image-pull data for this line yet — a bar appears once two releases have been published on it">—</span>';
      }
      var latest = Number(s.latest) || 0;
      var total = Number(s.total_window) || 0;
      var tip = 'Image pulls per ' + esc(br) + ' release: ' + latest + ' on ' +
        esc(String(points[points.length - 1].sha || '')) + ' \u00b7 ' + total +
        ' over last ' + points.length + ' release' + (points.length === 1 ? '' : 's') +
        ' (package-wide pulls landing during each release window)';
      return '<span style="display:inline-flex;align-items:center;gap:4px" title="' + escAttr(tip) + '">' +
        pullBarsSVG(points, 5, 2, 14) +
        '<span style="font-size:0.65rem;color:#60a5fa;font-variant-numeric:tabular-nums">' + esc(String(latest)) + '</span>' +
        '</span>';
    }

    /* fillLinePullSparks refreshes every rendered .line-pulls placeholder from
       the cached series — called after each loadImagePulls fetch so the row
       charts update even though the rows themselves rendered earlier in the
       poll from the (possibly stale) cache. */
    function fillLinePullSparks() {
      var spans = document.querySelectorAll('.line-pulls');
      for (var i = 0; i < spans.length; i++) {
        spans[i].innerHTML = linePullSparkHTML(spans[i].getAttribute('data-branch') || '');
      }
    }

    async function loadImagePulls() {
      var host = document.getElementById('image-pulls-spark');
      if (!host) return;
      var data;
      try {
        var resp = await fetch('/api/hub/image-pulls');
        if (!resp.ok) { host.style.display = 'none'; return; }
        data = await resp.json();
      } catch (e) {
        /* Adoption chart is non-critical chrome — never let it break the
           dashboard. Just leave it hidden on any failure. */
        host.style.display = 'none';
        return;
      }
      _imagePullLines = (data && data.lines) || {};
      fillLinePullSparks();
      var points = (data && data.points) || [];
      /* The server names the ACTIVE line (stable channel's branch). Fall back
         to a lineless label rather than guessing a branch. */
      var line = (data && data.line) ? esc(String(data.line)) : '';
      var lineLabel = 'Pulls per ' + (line ? line + ' ' : '') + 'release';
      host.style.display = 'block';

      /* Cold start: needs at least two release snapshots to close one window. */
      if ((data && data.collecting) || points.length < 1) {
        host.innerHTML = '<div style="font-size:0.7rem;color:var(--muted)">Image ' + lineLabel.toLowerCase() + ': <span style="opacity:0.7">collecting… (a bar appears once a second release is published)</span></div>';
        return;
      }

      /* Bar-chart geometry. Named so there are no bare magic numbers below. */
      var BAR_W = 14;      // px, per-release bar width
      var BAR_GAP = 3;     // px, gap between bars
      var BAR_H = 30;      // px, drawing height

      var svg = pullBarsSVG(points, BAR_W, BAR_GAP, BAR_H);

      var latest = (data && Number(data.latest)) || 0;
      var total = (data && Number(data.total_window)) || 0;
      var newestSHA = points.length ? esc(String(points[points.length - 1].sha || '')) : '';
      host.innerHTML =
        '<div style="display:flex;align-items:center;gap:10px">' +
          '<div style="font-size:0.7rem;color:var(--muted);white-space:nowrap">' + lineLabel + '</div>' +
          svg +
          '<div style="font-size:0.7rem;color:var(--muted);white-space:nowrap">' +
            '<span style="color:#60a5fa;font-weight:600">' + esc(String(latest)) + '</span> on ' + newestSHA +
            ' &middot; ' + esc(String(total)) + ' over last ' + points.length + ' releases' +
          '</div>' +
        '</div>';
    }

    /* renderHivesError replaces the eternal spinner with an actionable message.
       Written with DOM APIs + addEventListener rather than innerHTML with an
       inline handler so the error text can never be interpreted as markup. */
    function renderHivesError(err) {
      var container = document.getElementById('hives-container');
      if (!container) return;
      /* Keep a good list on screen if we already have one — a failed refresh
         should not blank out the fleet the operator is looking at. */
      if (_allDashHives.length && !/Loading your hives/.test(container.innerHTML) &&
          !container.querySelector('.hives-load-error')) {
        return;
      }
      var box = document.createElement('div');
      box.className = 'empty-state hives-load-error';
      var title = document.createElement('p');
      title.style.cssText = 'font-size:1.1rem;margin-bottom:8px;color:var(--red)';
      title.textContent = 'Could not load your hives';
      var detail = document.createElement('p');
      detail.style.cssText = 'font-size:0.8rem;color:var(--muted);word-break:break-word';
      detail.textContent = (err && (err.message || String(err))) || 'Unknown error';
      var hint = document.createElement('p');
      hint.style.cssText = 'font-size:0.75rem;color:var(--muted);opacity:0.8;margin-top:6px';
      hint.textContent = 'Details were logged to the browser console.';
      var wrap = document.createElement('p');
      wrap.style.marginTop = '12px';
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'filter-chip filter-chip-clear';
      btn.textContent = 'Retry';
      btn.addEventListener('click', function() {
        container.innerHTML = '<div class="loading">Loading your hives...</div>';
        /* Force the next render past the signature short-circuit — the cached
           signature may still match the payload that failed to render. */
        _lastHivesJSON = '';
        loadHives();
      });
      wrap.appendChild(btn);
      box.appendChild(title);
      box.appendChild(detail);
      box.appendChild(hint);
      box.appendChild(wrap);
      container.innerHTML = '';
      container.appendChild(box);
    }

    async function loadPublicHives(myHives) {
      try {
        var resp = await fetch('/api/registry');
        var data = await resp.json();
        var allPublic = (data.hives || []).filter(function(h) { return h.isPublic !== false && h.hiveType === 'hosted'; });
        var myIds = {};
        (myHives || []).forEach(function(h) { myIds[h.id] = true; });
        var otherHives = allPublic.filter(function(h) { return !myIds[h.id]; });
        var section = document.getElementById('public-hives-section');
        if (!otherHives.length) { section.style.display = 'none'; return; }
        section.style.display = '';
        var statusResp = await fetch('/api/saas/access-status');
        var statusData = await statusResp.json();
        var accessMap = statusData.hives || {};
        var rows = otherHives.map(function(h) {
          var repoPath = h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : h.primaryRepo || '';
          // Link on the hive's own GitHub instance (github_host) — a GHE repo must
          // point at github.ibm.com, not 404 on github.com. Falls back to plain
          // text when there is no org/repo to build a valid link from.
          var repoHref = ghRepoURL(hiveForgeHost(h), h.org, h.primaryRepo);
          var repoLink = repoPath ? (repoHref ? '<a href="' + repoHref + '" target="_blank" class="repo-link">' + esc(h.primaryRepo) + '</a>' : '<span class="repo-link">' + esc(h.primaryRepo) + '</span>') : '';
          var actionCell = '';
          var access = accessMap[h.id];
          // /contribute is a PUBLIC endpoint (see publicPaths) — lending compute
          // does NOT require access — so Contribute is the PRIMARY action for
          // EVERYONE on a public hive, regardless of access status. Use the
          // hive's heartbeat-reported dashboard URL (resolvedBase), NOT a
          // hardcoded <id>.hive.kubestellar.io host. Firewalled spokes (e.g.
          // the heartbeat-only cluster on its OpenShift-route domain) live on a different
          // domain, so the hardcoded host 503'd/failed to resolve.
          var cBase = resolvedBase(h);
          var contributeAction = cBase
            ? '<a href="' + cBase + '/contribute" target="_blank" style="padding:3px 10px;background:rgba(34,197,94,0.15);color:#4ade80;border:1px solid rgba(34,197,94,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none">Contribute</a>'
            : '<span style="padding:3px 10px;color:var(--muted);font-size:0.7rem" title="hive has not reported its dashboard URL yet">Contribute unavailable</span>';
          // Access is the SECONDARY, less-prominent action: a small link for the
          // user who genuinely wants sign-in / manage access. Pending state is
          // preserved (no re-request while one is outstanding).
          var accessSecondary;
          if (access && access.status === 'accepted') {
            accessSecondary = '<span style="font-size:0.65rem;color:var(--green)" title="you have access to this hive">✓ access</span>';
          } else if (access && access.status === 'pending') {
            accessSecondary = '<span style="font-size:0.65rem;color:#fbbf24" title="access request pending">Pending</span>';
          } else {
            accessSecondary = '<a href="#" onclick="dashRequestAccess(\'' + esc(h.id) + '\',this);return false" style="font-size:0.65rem;color:#60a5fa;text-decoration:none" title="request sign-in / manage access to this hive">Request access</a>';
          }
          actionCell = '<div style="display:flex;gap:8px;align-items:center;justify-content:flex-end">' + contributeAction + accessSecondary + '</div>';
          return '<tr>' +
            '<td style="text-align:left">' + esc(h.name || h.id) + '</td>' +
            '<td>' + repoLink + '</td>' +
            '<td>' + acmmBadge(h.acmmLevel) + '</td>' +
            '<td>' + actionCell + '</td>' +
            '</tr>';
        }).join('');
        document.getElementById('public-hives-container').innerHTML =
          '<table class="hive-table"><thead><tr><th style="text-align:left">Hive</th><th>Repo</th><th>ACMM</th><th></th></tr></thead><tbody>' + rows + '</tbody></table>';
      } catch(e) {}
    }

    var _requestAccessHiveId = null;
    var _requestAccessBtn = null;

    function dashRequestAccess(hiveId, btn) {
      // A justification note is required, so collect it via the modal
      // rather than firing the request immediately.
      _requestAccessHiveId = hiveId;
      _requestAccessBtn = btn || null;
      var label = document.getElementById('request-access-hive-label');
      if (label) label.textContent = hiveId;
      var ta = document.getElementById('request-access-note');
      if (ta) ta.value = '';
      var submit = document.getElementById('request-access-submit');
      if (submit) { submit.disabled = false; submit.textContent = 'Send Request'; }
      document.getElementById('request-access-modal').style.display = 'flex';
      if (ta) ta.focus();
    }

    function closeRequestAccessModal() {
      document.getElementById('request-access-modal').style.display = 'none';
      _requestAccessHiveId = null;
      _requestAccessBtn = null;
    }

    async function submitRequestAccess() {
      var hiveId = _requestAccessHiveId;
      if (!hiveId) { closeRequestAccessModal(); return; }
      var ta = document.getElementById('request-access-note');
      var note = ta ? ta.value.trim() : '';
      if (!note) { hiveToast('Please explain why you need access', 'error'); if (ta) ta.focus(); return; }
      var submit = document.getElementById('request-access-submit');
      if (submit) { submit.disabled = true; submit.textContent = 'Sending...'; }
      var btn = _requestAccessBtn;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/request-access', {
          method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({note: note})
        });
        var data = await resp.json();
        if (!resp.ok) {
          hiveToast(data.error || 'Request failed', 'error');
          if (submit) { submit.disabled = false; submit.textContent = 'Send Request'; }
          return;
        }
        if (btn) btn.outerHTML = '<span style="padding:3px 10px;background:rgba(245,158,11,0.15);color:#fbbf24;border:1px solid rgba(245,158,11,0.3);border-radius:4px;font-size:0.7rem">Pending</span>';
        hiveToast('Access request sent!', 'success');
        closeRequestAccessModal();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
        if (submit) { submit.disabled = false; submit.textContent = 'Send Request'; }
      }
    }

    /* isPlaceholderHive identifies an UNASSIGNED pool slot: provStatus
       'available' is authoritative, with an 'available-*' org prefix as the
       fallback for placeholders that have not reported provStatus yet. Shared by
       the status-filter scoping and the Assigned/Unassigned section split so the
       two can never disagree about what counts as a placeholder. */
    function isPlaceholderHive(h) {
      if (!h) return false;
      if (h.provStatus === 'available') return true;
      /* An assigned slot is NOT inventory, even while its org still reads
         "available-<id>". The assign path writes the real org/repo to meta
         immediately, but entry.org is spoke-reported and only catches up on the
         next heartbeat + config adoption. Keying on the org prefix alone parked
         a freshly-approved hive under "Unassigned hives" for that whole window,
         which reads as the approval having silently failed. provStatus and
         assignedUnclaimed both come from meta, so either one settles it the
         instant the approval lands. */
      if (h.provStatus === 'assigned' || h.assignedUnclaimed) return false;
      return !!h.org && h.org.indexOf('available-') === 0;
    }

    /* hiveNamespace returns the Kubernetes namespace a hosted hive's spoke
       runs in, or '' for a non-hosted (local) hive, which has no such
       namespace. Mirrors hostedNamespaceForHive in
       pkg/hub/hosted_namespace_identity.go — "hive-hosted-" + id — computed
       here rather than shipped as its own field so it can never disagree
       with the one place (server-side) that decides the namespace name. */
    function hiveNamespace(h) {
      if (!h || !h.id) return '';
      var isHosted = h.hiveType === 'hosted' || h.id.indexOf('hosted-') === 0 || h.id.indexOf('saas-') === 0;
      if (!isHosted) return '';
      return 'hive-hosted-' + h.id;
    }

    /* ---------- Bulk multi-select actions ----------
       _bulkSelected is the authoritative selection, keyed by hive id, so it
       survives re-render, re-sort and filter changes: the checkbox state is
       derived from this map on every render rather than read back out of the
       DOM. Ids that are no longer visible are pruned in syncBulkSelection so a
       hidden hive can never be swept into a bulk action the user cannot see. */
    var _bulkSelected = {};

    /* Client-side mirror of the server's maxBulkHivesPerRequest. The server is
       the enforcing authority; this only gives a better message than a 400. */
    var BULK_MAX_HIVES = 100;

    /* Actions that disrupt a running hive and therefore require confirmation
       naming the count. Auto-upgrade toggles only change a stored preference,
       so they are not in this set. */
    var BULK_DISRUPTIVE_ACTIONS = {'restart': 1, 'upgrade': 1, 'switch-branch': 1};

    /* Only hosted hives the user owns (or any hosted hive, for the hub admin)
       can be targeted — the same predicate the single-hive buttons use. Local
       hives have no hub-managed deployment to restart, and unassigned
       placeholders have nothing running yet. */
    function isBulkEligible(h) {
      if (!h || !h.id) return false;
      if (isPlaceholderHive(h)) return false;
      var isHosted = h.hiveType === 'hosted' || (h.id.indexOf('hosted-') === 0 || h.id.indexOf('saas-') === 0);
      if (!isHosted) return false;
      return h.role === 'owner' || _isAdmin;
    }

    function bulkSelectedIds() {
      var out = [];
      for (var k in _bulkSelected) { if (_bulkSelected[k]) out.push(k); }
      return out;
    }

    /* Drop selections for hives that are no longer present/eligible in the
       current view. Called at the top of every render so the count in the bulk
       bar always matches what is actually on screen. */
    function syncBulkSelection(visibleHives) {
      var live = {};
      var list = visibleHives || [];
      for (var i = 0; i < list.length; i++) {
        if (isBulkEligible(list[i])) live[list[i].id] = true;
      }
      for (var k in _bulkSelected) {
        if (!live[k]) delete _bulkSelected[k];
      }
    }

    function toggleBulkHive(id, checked) {
      if (checked) _bulkSelected[id] = true; else delete _bulkSelected[id];
      renderBulkBar();
      syncBulkSectionHeaderBoxes();
    }

    /* Select-all is per section (Assigned / Unassigned) so an admin cannot
       accidentally sweep the whole page from one box. section is the key used
       by the header checkbox id. */
    function toggleBulkSection(section, checked) {
      var boxes = document.querySelectorAll('input[type=checkbox][data-bulk-section="' + section + '"]');
      for (var i = 0; i < boxes.length; i++) {
        var id = boxes[i].getAttribute('data-bulk-id');
        if (!id) continue;
        boxes[i].checked = checked;
        if (checked) _bulkSelected[id] = true; else delete _bulkSelected[id];
      }
      renderBulkBar();
    }

    /* Put each section header box into checked / indeterminate / unchecked to
       match its rows. */
    function syncBulkSectionHeaderBoxes() {
      var heads = document.querySelectorAll('input[type=checkbox][data-bulk-section-head]');
      for (var i = 0; i < heads.length; i++) {
        var section = heads[i].getAttribute('data-bulk-section-head');
        var boxes = document.querySelectorAll('input[type=checkbox][data-bulk-section="' + section + '"]');
        var total = boxes.length, sel = 0;
        for (var j = 0; j < boxes.length; j++) {
          if (_bulkSelected[boxes[j].getAttribute('data-bulk-id')]) sel++;
        }
        heads[i].checked = total > 0 && sel === total;
        heads[i].indeterminate = sel > 0 && sel < total;
      }
    }

    function clearBulkSelection() {
      _bulkSelected = {};
      var boxes = document.querySelectorAll('input[type=checkbox][data-bulk-id]');
      for (var i = 0; i < boxes.length; i++) { boxes[i].checked = false; }
      renderBulkBar();
      syncBulkSectionHeaderBoxes();
    }

    /* Row checkbox markup. section scopes it to a select-all group. */
    function bulkCheckboxCell(h, section) {
      if (!isBulkEligible(h)) {
        return '<td style="width:26px;text-align:center"></td>';
      }
      var checked = _bulkSelected[h.id] ? ' checked' : '';
      return '<td style="width:26px;text-align:center">' +
        '<input type="checkbox" data-bulk-id="' + esc(h.id) + '" data-bulk-section="' + esc(section) + '"' + checked +
        ' onclick="event.stopPropagation()" onchange="toggleBulkHive(\'' + esc(h.id) + '\',this.checked)"' +
        ' title="Select for bulk actions" style="cursor:pointer"></td>';
    }

    function bulkSectionCheckbox(section) {
      /* stopPropagation because the section-header row is itself a click target
         that expands/collapses the section — without this, ticking select-all
         would also collapse the very rows it just selected. */
      return '<input type="checkbox" data-bulk-section-head="' + esc(section) + '"' +
        ' onclick="event.stopPropagation()"' +
        ' onchange="toggleBulkSection(\'' + esc(section) + '\',this.checked)"' +
        ' title="Select all in this section" style="cursor:pointer;margin-right:8px;vertical-align:middle">';
    }

    function renderBulkBar() {
      var bar = document.getElementById('bulk-action-bar');
      if (!bar) return;
      var ids = bulkSelectedIds();
      if (!ids.length) { bar.style.display = 'none'; bar.innerHTML = ''; return; }
      var btn = 'padding:5px 12px;border-radius:4px;cursor:pointer;font-size:0.72rem;white-space:nowrap;border:1px solid var(--border);background:var(--surface);color:var(--text)';
      var over = ids.length > BULK_MAX_HIVES;
      var branchOpts = '';
      var branches = _trackedBranchesList.length > 0 ? _trackedBranchesList : Object.keys(_latestSHAs);
      for (var bi = 0; bi < branches.length; bi++) {
        branchOpts += '<option value="' + esc(branches[bi]) + '">' + esc(branches[bi]) + '</option>';
      }
      var branchPicker = branches.length > 0
        ? '<select id="bulk-branch-select" style="' + btn + ';padding:4px 8px"><option value="">Switch branch…</option>' + branchOpts + '</select>'
        : '';
      bar.innerHTML =
        '<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-bottom:14px;padding:10px 14px;' +
        'background:rgba(59,130,246,0.08);border:1px solid rgba(59,130,246,0.3);border-radius:8px">' +
        '<strong style="font-size:0.8rem;color:#60a5fa">' + ids.length + (ids.length === 1 ? ' hive' : ' hives') + ' selected</strong>' +
        (over ? '<span style="font-size:0.72rem;color:var(--red)">Over the ' + BULK_MAX_HIVES + '-hive limit — deselect some.</span>' : '') +
        '<span style="flex:1"></span>' +
        '<button type="button" onclick="runBulkAction(\'restart\')" style="' + btn + '">Restart</button>' +
        '<button type="button" onclick="runBulkAction(\'upgrade\')" style="' + btn + '">Upgrade to latest</button>' +
        '<button type="button" onclick="runBulkAction(\'enable-auto-upgrade\')" style="' + btn + '">Auto: instant</button>' +
        '<button type="button" onclick="runBulkAction(\'daily-auto-upgrade\')" style="' + btn + '" title="Upgrade at most once a day, midday — keeps a stable hive from being restarted mid-work, and puts a bad roll in staffed hours">Auto: daily 1pm ET</button>' +
        '<button type="button" onclick="runBulkAction(\'weekly-auto-upgrade\')" style="' + btn + '" title="Upgrade at most once a week, Tuesday midday — the least disruptive cadence that still keeps the hive current">Auto: Tue 1pm ET</button>' +
        '<button type="button" onclick="runBulkAction(\'disable-auto-upgrade\')" style="' + btn + '">Auto-upgrade off</button>' +
        branchPicker +
        '<button type="button" onclick="clearBulkSelection()" style="' + btn + ';color:var(--muted)">Clear</button>' +
        '</div>';
      bar.style.display = '';
      var sel = document.getElementById('bulk-branch-select');
      if (sel) {
        sel.onchange = function() {
          var b = sel.value;
          sel.value = '';
          if (b) runBulkAction('switch-branch', b);
        };
      }
    }

    var BULK_ACTION_LABELS = {
      'restart': 'Restart',
      'upgrade': 'Upgrade to latest',
      'enable-auto-upgrade': 'Enable instant auto-upgrade on',
      'daily-auto-upgrade': 'Enable daily 1pm ET auto-upgrade on',
      'weekly-auto-upgrade': 'Enable weekly Tuesday 1pm ET auto-upgrade on',
      'disable-auto-upgrade': 'Disable auto-upgrade on',
      'switch-branch': 'Switch branch for'
    };

    async function runBulkAction(action, branch) {
      var ids = bulkSelectedIds();
      if (!ids.length) { hiveToast('No hives selected', 'error'); return; }
      if (ids.length > BULK_MAX_HIVES) {
        hiveToast('Select at most ' + BULK_MAX_HIVES + ' hives per bulk action', 'error');
        return;
      }
      var label = BULK_ACTION_LABELS[action] || action;
      var noun = ids.length === 1 ? 'hive' : 'hives';
      /* Confirmation names BOTH the action and the count — restarting a batch
         of live hives must never be one misclick. Disruptive actions also list
         the hives so the user can see exactly what is in the batch. */
      if (BULK_DISRUPTIVE_ACTIONS[action]) {
        var names = ids.slice(0, 10).map(function(i) { return esc(i); }).join('<br>');
        if (ids.length > 10) names += '<br>… and ' + (ids.length - 10) + ' more';
        var msg = '<strong>' + esc(label) + (branch ? ' ' + esc(branch) : '') + '</strong> on <strong>' +
          ids.length + ' ' + noun + '</strong>?<br><br>' +
          '<div style="font-family:monospace;font-size:0.75rem;color:var(--muted);text-align:left;max-height:180px;overflow:auto">' + names + '</div>';
        if (!await hiveConfirm(msg, true)) return;
      }
      hiveToast(label + ' — ' + ids.length + ' ' + noun + '…', 'info');
      try {
        var resp = await fetch('/api/saas/hives/bulk', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({action: action, hive_ids: ids, branch: branch || ''})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast((data && data.error) || 'Bulk action failed', 'error'); return; }
        showBulkResults(label, data);
        clearBulkSelection();
        loadHives();
        setTimeout(loadHives, 10000);
        setTimeout(loadHives, 30000);
        setTimeout(loadHives, 60000);
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
      }
    }

    /* Partial failure is the normal case, so always show the per-hive
       breakdown rather than a single success toast: the user needs to know
       WHICH hives failed and why. */
    function showBulkResults(label, data) {
      var results = (data && data.results) || [];
      var ok = data.succeeded || 0, failed = data.failed || 0;
      var failedRows = '';
      var heartbeatCount = 0;
      for (var i = 0; i < results.length; i++) {
        var r = results[i] || {};
        if (r.ok) { if (r.via === 'heartbeat') heartbeatCount++; continue; }
        failedRows += '<div style="display:flex;gap:8px;padding:3px 0;font-size:0.72rem">' +
          '<span style="font-family:monospace;color:var(--red);flex:none">' + esc(r.hive_id || '?') + '</span>' +
          '<span style="color:var(--muted)">' + esc(r.error || 'failed') + '</span></div>';
      }
      if (!failed) {
        var extra = heartbeatCount
          ? ' (' + heartbeatCount + ' queued for delivery on next check-in)'
          : '';
        hiveToast(label + ': ' + ok + ' succeeded' + extra, 'success');
        return;
      }
      hiveConfirm('<strong>' + esc(label) + '</strong><br><br>' +
        '<span style="color:var(--green)">' + ok + ' succeeded</span>' +
        (heartbeatCount ? ' <span style="color:var(--muted);font-size:0.75rem">(' + heartbeatCount + ' via heartbeat)</span>' : '') +
        ' &nbsp; <span style="color:var(--red)">' + failed + ' failed</span>' +
        '<div style="margin-top:10px;text-align:left;max-height:220px;overflow:auto">' + failedRows + '</div>', true);
    }

    /* True while any row's branch/channel dropdown is open. renderHives
       rebuilds the row DOM, which would destroy the open menu mid-click —
       the fleet heartbeats change some hive field on nearly every poll, so
       without this guard the dropdown closed itself within seconds of
       opening. */
    function branchMenuOpen() {
      var menus = document.querySelectorAll('[id^="branch-menu-"]');
      for (var i = 0; i < menus.length; i++) {
        if (menus[i].style.display !== 'none') return true;
      }
      return false;
    }
    /* One phrase answering WHO paused an agent, WHY and WHEN, from the pause
       provenance the spoke reports over the heartbeat (#4041) — e.g.
       "paused by bketelsen via dashboard, 3d ago" or "paused (login-detector:
       login required detected), 2h ago". A deliberate owner quiesce must read
       differently from a malfunction in the fleet view. Old cached rows and
       old spokes lack the fields; falls back to a bare "paused". */
    function agentPauseProvenance(a) {
      var line;
      if (a.pausedTrigger === 'dashboard-api') {
        line = 'paused by ' + (a.pausedBy || 'an operator') + ' via dashboard';
      } else if (a.pausedTrigger) {
        line = 'paused (' + a.pausedTrigger + (a.pausedReason ? ': ' + a.pausedReason : '') + ')';
      } else {
        line = 'paused';
      }
      if (a.pausedAt) {
        /* Self-contained relative age (coarse on purpose): timelineAgo lives
           in a later <script> block, and renderHives can run during this
           block's init (paintCachedHives) before that block is parsed. */
        var t = Date.parse(a.pausedAt);
        if (!isNaN(t)) {
          var PAUSE_MS_PER_MIN = 60000, PAUSE_MIN_PER_HOUR = 60, PAUSE_HOURS_PER_DAY = 24;
          var mins = Math.floor((Date.now() - t) / PAUSE_MS_PER_MIN);
          if (mins < 1) line += ', just now';
          else if (mins < PAUSE_MIN_PER_HOUR) line += ', ' + mins + 'm ago';
          else if (mins < PAUSE_MIN_PER_HOUR * PAUSE_HOURS_PER_DAY) line += ', ' + Math.floor(mins / PAUSE_MIN_PER_HOUR) + 'h ago';
          else line += ', ' + Math.floor(mins / (PAUSE_MIN_PER_HOUR * PAUSE_HOURS_PER_DAY)) + 'd ago';
        }
      }
      return line;
    }
    function renderHives(allHives, force) {
      allHives = allHives || [];
      /* Defer the whole render while a branch/channel menu is open. Skipping
         BEFORE the signature is stored means the change is not swallowed: the
         next render call (poll tick, or the catch-up fired when the menu
         closes) sees a stale _lastHivesJSON and repaints normally. */
      if (branchMenuOpen()) return;
      /* The signature must include EVERY piece of render-affecting view state,
         otherwise changing it while the hive data is unchanged is silently a
         no-op — toggling a chip, drilling into an alert type, expanding the
         acknowledged list, typing in the search box, picking a facet,
         collapsing a section, switching group-by, collapsing a group or
         applying a saved view would all appear to do nothing. */
      var sig = JSON.stringify(allHives) + '|' + JSON.stringify(_dashStatusFilters) +
        '|' + _dashFailingCheckFilter + '|' + _dashDriftFilter + '|' + _dashUpgradeFilter +
        '|' + _dashAdvisoryStaleFilter +
        '|' + _alertTypeFilter + '|' + _alertShowAcked + '|' + JSON.stringify(_fleetAlerts) +
        /* Without this, expanding "+N more" changes no hive data and the
           early-return below would make the click a silent no-op. */
        '|' + JSON.stringify(_alertRowsExpanded) +
        '|' + _dashSearchQuery + '|' + JSON.stringify(_dashFacets) +
        '|' + JSON.stringify(_dashFacetCollapsed) + '|' + JSON.stringify(_dashSectionCollapsed) +
        '|' + _dashGroupBy + '|' + JSON.stringify(_dashGroupCollapsed) +
        '|' + _dashActiveView + '|' + _dashDefaultView + '|' + JSON.stringify(_dashSavedViews) +
        '|' + _dashFacetTrayOpen;
      if (!force && sig === _lastHivesJSON) return;
      _lastHivesJSON = sig;
      /* Status/facet filters describe ASSIGNED hives only. An unassigned
         placeholder has no GitHub App, no tokens and no real health to speak of,
         so every chip would appear to "hide" the whole pool — and filtering to
         e.g. Degraded made the Unassigned section vanish, which reads as the
         placeholders having been deleted. The free-text search is different:
         it promises to filter the displayed rows by visible metadata, so it
         also scopes placeholders by name/id/cluster/repo instead of leaving
         unrelated inventory visible while the header says a search is active. */
      var assignedAll = [], unassignedAll = [];
      for (var _si = 0; _si < allHives.length; _si++) {
        (isPlaceholderHive(allHives[_si]) ? unassignedAll : assignedAll).push(allHives[_si]);
      }
      /* Expire client-side upgrade sentinels ONCE, before anything reads them.
         Filtering, facet counting and row drawing are all pure readers of
         _upgradingHives from here on, so they cannot observe each other's
         mutations and the pill can no longer disagree with the badge. */
      normalizeUpgradeStates(assignedAll);
      var filteredAssigned = applyDashFilters(assignedAll);
      var filteredUnassigned = (_dashSearchQuery || '').trim()
        ? unassignedAll.filter(hiveMatchesSearch)
        : unassignedAll;
      var hives = filteredAssigned.concat(filteredUnassigned);
      var filterBar = document.getElementById('hive-filter-bar');
      if (filterBar) filterBar.style.display = allHives.length ? '' : 'none';
      var searchRow = document.getElementById('hive-search-row');
      if (searchRow) searchRow.style.display = allHives.length ? '' : 'none';
      /* Nothing renderable below means no rows to act on — drop the selection
         so the bulk bar can't linger over an empty or fully-filtered table. */
      if (!allHives.length || !hives.length) {
        _bulkSelected = {};
        renderBulkBar();
      }
      /* The view bar shows whenever there are hives at all — including when the
         status filters currently match none of them, so the operator can still
         switch grouping or restore a saved view from the empty state. */
      var viewBar = document.getElementById('hive-view-bar');
      if (viewBar) viewBar.style.display = allHives.length ? '' : 'none';
      renderViewBar();
      /* Counts are over the assigned set only, matching what the chips filter. */
      renderStatusFilterBar(assignedAll, filteredAssigned.length);
      /* Facets are offered over the assigned set for the same reason the chips
         are: a placeholder carries no cluster, role or branch worth faceting. */
      renderFacetRail(assignedAll);
      /* Drift exceptions are scoped to assigned hives for the same reason: a
         placeholder is never flagged for claimed-hive concerns server-side. */
      renderDriftSummary(assignedAll);
      /* Drawn BEFORE the empty-state early-returns below: a fleet whose every
         hive is filtered out still has alerts worth showing, and the panel is
         how the operator gets back out of a drill-down. */
      renderSummaryTiles();
      renderAlertsPanel();
      if (!allHives.length) {
        var driftEl0 = document.getElementById('hive-drift-summary');
        if (driftEl0) driftEl0.style.display = 'none';
        document.getElementById('hives-container').innerHTML =
          '<div class="empty-state">' +
          '<p style="font-size:1.2rem;margin-bottom:8px">No hives yet</p>' +
          '<p style="margin-bottom:16px">Get started in one of two ways:</p>' +
          '<div style="display:flex;gap:12px;justify-content:center;flex-wrap:wrap;margin-bottom:12px">' +
          /* Request-a-hive → the EXISTING Request-a-Hive wizard at /get-started
             (POST /api/saas/request-provision). We host it for you. */
          '<a class="btn-primary" href="/get-started" style="text-decoration:none" title="We host the hive for you">Request a hive</a>' +
          /* Register-your-own-hive → the EXISTING self-host guide at
             /get-started#self-host. Not self-service: the operator must issue
             the spoke a HIVE_HUB_SECRET or its heartbeats 401 (see #1077 and
             the note on the other copy of this CTA above). */
          '<a class="btn-primary" href="/get-started#self-host" style="background:var(--surface);color:var(--text);border:1px solid var(--border);text-decoration:none" title="You self-host the hive and attach it to this hub">Register your own hive</a>' +
          '</div>' +
          '<p style="color:var(--muted);font-size:0.85rem">…or contribute to a public hive below.</p>' +
          '</div>';
        return;
      }
      if (!hives.length) {
        /* Hives exist, but every one was filtered out — say so, and offer the
           way back rather than looking like the list failed to load. */
        document.getElementById('hives-container').innerHTML =
          '<div class="empty-state">' +
          '<p style="font-size:1.2rem;margin-bottom:8px">No hives match these filters</p>' +
          '<p>' + allHives.length + (allHives.length === 1 ? ' hive is' : ' hives are') + ' hidden by the search, facets or status filters.</p>' +
          /* clearAllHiveFilters, not clearStatusFilters: a search term, a facet
             or an alert drill-down can empty the list too, and a button that
             only clears the chips would leave the operator stuck looking at an
             empty table. */
          '<p style="margin-top:12px"><button type="button" class="filter-chip filter-chip-clear" onclick="clearAllHiveFilters()">Clear filters</button></p>' +
          '</div>';
        return;
      }
      /* Prune selections for hives that are no longer visible BEFORE building
         rows, so the checkbox state and the bulk bar's count agree. */
      syncBulkSelection(hives);
      var repoPath = function(h) { return h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : h.primaryRepo || ''; };
      var buildRow = function(h, i, section) {
        // While a rollout is in flight, the status dot becomes the distinct
        // blue glowing/slow-blinking "upgrading" dot (see .online-dot.upgrading) —
        // it outranks the normal online/offline health dot so an upgrade reads at
        // a glance and is never mistaken for the ok/degraded/critical/unknown state.
        // The OFFLINE dot (grey, no heartbeat) previously carried no tooltip, so
        // an operator hovering a dead row learned nothing \u2014 not even which
        // Kubernetes namespace the hive lives in. healthBadge() already gives the
        // ONLINE dot a rich hover (status + ns: + heartbeat age); this mirrors the
        // reference facts an operator needs to go find the offline hive on the
        // cluster. Built defensively \u2014 every part is skipped when its field is
        // empty \u2014 and escaped with escAttr since it lands inside a quoted title.
        var offlineDotTitle = (function() {
          var parts = ['Offline'];
          var ns = hiveNamespace(h);            // 'hive-hosted-<id>' or '' for local
          if (ns) parts.push('ns: ' + ns);
          var cluster = h.clusterName || h.clusterId || '';
          if (cluster) parts.push('cluster: ' + cluster);
          if (h.lastHeartbeat) parts.push('last seen: ' + fmtUserTS(h.lastHeartbeat));
          return parts.join('\n');
        })();
        var dot = h.upgrading
          ? '<span class="online-dot upgrading" title="Upgrading \u2014 a rollout is in progress"></span>'
          : (h.online ? healthBadge(h) : '<span class="online-dot off" title="' + escAttr(offlineDotTitle) + '" style="cursor:help"></span>');
        var rp = repoPath(h);
        // Link on the hive's own GitHub instance (github_host) so a GHE repo
        // points at github.ibm.com, not 404 on github.com.
        var repoHref = ghRepoURL(hiveForgeHost(h), h.org, h.primaryRepo);
        var repoLink = rp ? (repoHref ? '<a href="' + repoHref + '" target="_blank" class="repo-link">' + esc(h.primaryRepo) + '</a>' : '<span class="repo-link">' + esc(h.primaryRepo) + '</span>') : '';
        var repoCount = (h.repos || []).length;
        var isHosted = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
        var isLocal = !isHosted;
        var canConvert = isLocal && h.role === 'owner' && (_userQuota < 0 || _userQuota > _userUsed);
        var modeCell = h.provStatus === 'error'
          ? '<span style="color:var(--red);cursor:help;white-space:nowrap" title="' + esc(h.provError || '') + '">⚠ ERROR</span>'
          : h.assigning
          ? '<span style="color:var(--accent);white-space:nowrap" title="Waiting for the spoke to report the new project via heartbeat"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>Assigning to ' + esc(h.assigningTo || '?') + '</span>'
          : h.provStatus === 'provisioning'
          ? '<span style="color:var(--accent);white-space:nowrap">⏳ Provisioning</span>'
          : modeBadge(h.governorMode);
        /* A placeholder wedged at assigned && !claim_delivered gets a subtle
           live-ticking "claim pending · Nm" pill so a stuck assignment is
           obvious. Self-suppresses (empty) for every other row. */
        modeCell += claimPendingPill(h);
        var rb = resolvedBase(h);
        var contributeUrl = rb ? rb + '/contribute' : '';
        var actions = '';
        if (canConvert) {
          actions = '<button onclick="openConvert(this)" data-hive-id="' + esc(h.id) + '" data-dash-url="' + esc(h.dashboardUrl||'') + '" data-org="' + esc(h.org) + '" data-repos="' + esc((h.repos||[]).join(', ')) + '" data-primary="' + esc(h.primaryRepo) + '" data-level="' + (h.acmmLevel||1) + '" data-name="' + esc(h.name||'') + '" style="padding:3px 10px;background:var(--accent);color:#000;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap">Convert to Hosted</button>';
          if (h.role === 'owner') {
            actions += '<br style="margin-bottom:4px"><button onclick="removeLocalHive(\'' + esc(h.id) + '\')" style="margin-top:6px;padding:3px 10px;background:var(--surface);color:var(--muted);border:1px solid var(--border);border-radius:4px;cursor:pointer;font-size:0.65rem;white-space:nowrap" title="Remove from registry (does not delete the hive)">Remove</button>';
          }
        } else if (isHosted && (roleAtLeast(h.role, 'read-write'))) {
          actions = '<button onclick="openAccessModal(\'' + esc(h.id) + '\',\'' + esc(h.dashboardUrl || '') + '\')" style="padding:3px 10px;background:var(--blue);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap;margin-right:4px">Permissions</button>';
          if (h.role === 'owner') {
            actions += '<button onclick="deleteHive(\'' + esc(h.id) + '\')" style="padding:3px 10px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap">Delete</button>';
          }
        }
        var menuId = 'hive-menu-' + i;
        var dashUrl = dashboardLink(h);
        var snapUrl = snapshotLink(h);
        var apiUrl = apiLink(h);
        var menuItems = [];
        var mi = 'display:block;padding:7px 14px;color:#c9d1d9;text-decoration:none;font-size:0.78rem;cursor:pointer';
        if (_isAdmin && h.provStatus === 'available' && !h.assigning) menuItems.push('<div onclick="openAssignModal(\'' + esc(h.id) + '\')" style="' + mi + ';color:#3fb950;font-weight:600">Assign / Claim</div><div style="border-top:1px solid #30363d;margin:4px 0"></div>');
        /* Reset assignment: escape hatch for a placeholder wedged at
           assigned && !claim_delivered (assignedUnclaimed) — its spoke never
           reported the project back, so it can be neither re-approved (needs a
           pending request) nor re-assigned (needs an available slot). This
           returns the slot to the pool so it can be re-armed. Admin-only,
           mirroring the endpoint's own guard. */
        if (_isAdmin && h.assignedUnclaimed) menuItems.push('<div onclick="resetAssignment(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + ';color:#d29922;font-weight:600">Reset assignment</div><div style="border-top:1px solid #30363d;margin:4px 0"></div>');
        if (contributeUrl) menuItems.push('<a href="' + contributeUrl + '" target="_blank" style="' + mi + '">Contribute</a>');
        if (h.snapshotUrl) menuItems.push('<a href="' + esc(h.snapshotUrl) + '" target="_blank" style="' + mi + '">Preview</a>');
        var apiBase = rb ? esc(rb) : '';
        if (apiBase) menuItems.push('<a href="' + apiBase + '/api/docs" target="_blank" style="' + mi + '">API Docs</a>');
        if (menuItems.length > 0 && (canConvert || isHosted || isLocal)) menuItems.push('<div style="border-top:1px solid #30363d;margin:4px 0"></div>');
        if (canConvert) menuItems.push('<div onclick="openConvert(this)" data-hive-id="' + esc(h.id) + '" data-dash-url="' + esc(h.dashboardUrl||'') + '" data-org="' + esc(h.org) + '" data-repos="' + esc((h.repos||[]).join(', ')) + '" data-primary="' + esc(h.primaryRepo) + '" data-level="' + (h.acmmLevel||1) + '" data-name="' + esc(h.name||'') + '" style="' + mi + '">Convert to Hosted</div>');
        if (isHosted && (roleAtLeast(h.role, 'read-write'))) menuItems.push('<div onclick="openAccessModal(\'' + esc(h.id) + '\',\'' + esc(h.dashboardUrl || '') + '\')" style="' + mi + '">Permissions</div>');
        /* Timeline is owner-or-admin, matching the API's authorization. */
        if (isHosted && (h.role === 'owner' || _isAdmin)) menuItems.push('<div onclick="openTimelineModal(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Activity Timeline</div>');
        if (roleAtLeast(h.role, 'read-write') || _isAdmin) menuItems.push('<div onclick="openOpenRouterFundModal(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">⚡ Fund with OpenRouter</div>');
        if (_isAdmin && isHosted) menuItems.push('<div onclick="openBannerForHive(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Send Banner</div>');
        /* Reset App clears ONLY the spoke's installation_id, which makes
           HasUsableApp() false and prompts the owner to install the App again.
           Admin-only and hosted-only, matching the endpoint's own guard. The
           case it exists for: a hive provisioned with its own GitHub App keeps
           that App's installation_id after its identity is moved to the fleet
           App, so every field reads correct while freshly minted tokens 404. */
        if (_isAdmin && isHosted) menuItems.push('<div onclick="resetHiveApp(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Reset Forge App</div>');
        /* Restart Spoke rolling-restarts every instance reporting as this
           hive. The case it exists for: two instances alternating as one hive
           (conflictingReporters drift) on a cluster the hub cannot reach —
           the heartbeat is the only channel, and a restart sheds the stale
           instance while costing the healthy one a single rolling restart. */
        if (_isAdmin && isHosted) menuItems.push('<div onclick="restartHiveSpoke(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Restart Spoke</div>');
        if (isLocal && h.role === 'owner') menuItems.push('<div onclick="removeLocalHive(\'' + esc(h.id) + '\')" style="' + mi + '">Remove</div>');
        if (isHosted && h.role === 'owner') menuItems.push('<div style="border-top:1px solid #30363d;margin:4px 0"></div><div onclick="deleteHive(\'' + esc(h.id) + '\')" style="' + mi + ';color:#f85149">Delete</div>');
        var sha = h.gitHash || '';
        /* Drift folded ONTO Version: config drift is overwhelmingly "this hive's
           version/branch differs from the fleet", so the drift indicator reads as
           a property of the Version cell rather than earning its own column. Only
           emitted when there IS drift (driftOf().count > 0); driftBadge() returns
           its own colored count pill with the full per-signal hover panel, so no
           datum is lost — hover the dot for every drift reason exactly as before.
           A clean hive shows nothing here, keeping the cell as tight as it was.
           driftBadge carries its own hive-access-pop panel and NO title, so the
           single-hover-panel invariant holds (it is a badge, not an inline face). */
        var driftDot = driftOf(h).count > 0 ? driftBadge(h) : '';
        var versionCell = '';
        if (sha) {
          var branchName = h.gitBranch || 'v2';
          /* Effective version SELECTION, distinct from the running branch. A
             hive following a release channel heartbeats the channel image's
             baked-in branch (a "stable" retag of a v4 build reports
             gitBranch "v4"), so gitBranch alone forgets the channel within
             one beat of the switch — the pill read "v4" on a stable hive.
             trackedChannel is the hub-persisted selection
             (SaaSHive.TrackedChannel: written only by a branch/channel
             switch, never by heartbeats). When set it is what the pill shows
             and what the picker treats as currently selected; branchName
             keeps driving everything about the code actually running
             (latest/behind, drift, upgrade state). */
          var versionSel = h.trackedChannel || branchName;
          var branchLatest = _latestSHAs[branchName] || _latestSHA;
          var _trackedBranches = _trackedBranchesList.length > 0 ? _trackedBranchesList : Object.keys(_latestSHAs);
          if (_trackedBranches.length === 0) _trackedBranches = ['v2'];
          /* A release channel is as valid an upgrade target as a branch — the
             hive gets pinned to the moving tag and follows promotions — so the
             same menu offers both. Channels are listed in their own section
             below the branches, because picking a channel is a different KIND
             of choice (track a promotion policy vs. track a git branch) and
             mixing them into one flat list hides that. */
          var canSwitchBranch = isHosted && h.role === 'owner' &&
            (_trackedBranches.length > 1 || _releaseChannels.length > 0) && !h.upgrading;
          var branchOptions = '';
          if (canSwitchBranch) {
            for (var bi = 0; bi < _trackedBranches.length; bi++) {
              var tb = _trackedBranches[bi];
              /* Compared against the SELECTION, not the running branch: a hive
                 tracking "stable" (currently a v4 image) must still offer v4
                 here — picking it is a real action, un-tracking the channel. */
              if (tb !== versionSel) {
                branchOptions += '<div onclick="event.stopPropagation();switchBranch(\'' + esc(h.id) + '\',\'' + esc(tb) + '\',this)" style="padding:4px 10px;cursor:pointer;font-size:0.65rem;white-space:nowrap;color:#c9d1d9;border-radius:4px" onmouseover="this.style.background=\'rgba(59,130,246,0.2)\'" onmouseout="this.style.background=\'transparent\'">' + esc(tb) + '</div>';
              }
            }
            var chOpts = '';
            for (var cbi = 0; cbi < _releaseChannels.length; cbi++) {
              var cb = _releaseChannels[cbi];
              /* The channel the hive already tracks is the current selection —
                 omit it exactly as the running branch is omitted above. Keyed
                 on versionSel so "stable" reads as selected, not "v4". */
              if (cb === versionSel) continue;
              /* Name what the channel resolves to RIGHT NOW in the hover, so
                 the operator is not picking a word with no visible meaning.
                 Falls back to the channel's own description when unresolved. */
              var cbT = null;
              for (var cti = 0; cti < _channelTargets.length; cti++) {
                if (_channelTargets[cti] && _channelTargets[cti].channel === cb) { cbT = _channelTargets[cti]; break; }
              }
              var cbTitle = cbT && cbT.branch
                ? 'Currently ' + cbT.branch + ' ' + (cbT.sha || '') + ' — the hive follows this channel as it is re-pointed'
                : 'Moving release channel — the hive follows it as it is re-pointed';
              /* Label carries the channel's CURRENT branch — "stable (v4)" —
                 while the switch VALUE stays the bare channel name: the hive
                 is pinned to the moving tag, not to the branch it happens to
                 resolve to today. */
              chOpts += '<div onclick="event.stopPropagation();switchBranch(\'' + esc(h.id) + '\',\'' + esc(cb) + '\',this)" title="' + escAttr(cbTitle) + '" style="padding:4px 10px;cursor:pointer;font-size:0.65rem;white-space:nowrap;color:#4ade80;border-radius:4px" onmouseover="this.style.background=\'rgba(34,197,94,0.2)\'" onmouseout="this.style.background=\'transparent\'">' + esc(versionLabel(cb)) + '</div>';
            }
            if (chOpts) {
              branchOptions += '<div style="border-top:1px solid #30363d;margin:4px 0"></div>' +
                '<div style="padding:2px 10px;font-size:0.55rem;color:var(--muted);text-transform:uppercase;letter-spacing:0.05em">Channels</div>' + chOpts;
            }
          }
          /* Pill text is the SELECTION (versionSel) through versionLabel, so
             a hive tracking a channel reads "stable (v4)" — the channel plus
             the branch it currently resolves to — and keeps reading that
             after the spoke heartbeats its baked-in branch. A plain-branch
             hive renders unchanged. Display-only: versionSel/branchName stay
             bare values, and every switch payload sends the bare name. */
          var branch = canSwitchBranch
            ? '<span id="branch-pill-' + esc(h.id) + '" style="display:inline-block;position:relative;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);cursor:pointer" onclick="toggleBranchMenu(\'' + esc(h.id) + '\')" title="Click to switch branch">' + esc(versionLabel(versionSel)) + ' ▾<div id="branch-menu-' + esc(h.id) + '" style="display:none;position:absolute;top:100%;left:0;margin-top:4px;background:#1c2128;border:1px solid #30363d;border-radius:6px;padding:4px 0;z-index:1000;min-width:60px;box-shadow:0 4px 12px rgba(0,0,0,0.4)">' + branchOptions + '</div></span>'
            : '<span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3)">' + esc(versionLabel(versionSel)) + '</span>';
          var latestUnknown = !branchLatest;
          var isCurrent = branchLatest && sameShaJS(sha, branchLatest);
          /* Branch switch in flight: the hive still reports the OLD branch
             (often current on it) until the new pod heartbeats — without
             this, isCurrent suppresses every progress indicator. */
          /* Switch-vs-upgrade is decided in ONE place (hiveUpgradeState) so the
             spinner, label and title always agree. Only a target on a DIFFERENT
             branch is a switch; a plain-SHA (auto-upgrade) target always reads
             "Upgrading", even if a stale switch sentinel lingers. */
          var upgradeState = hiveSwitchState(h, branchName);
          var isSwitching = upgradeState.isSwitching;
          var targetBranch = upgradeState.targetBranch;
          /* Sentinel expiry used to happen HERE, mid-render. That is what kept
             the pill and the badge disagreeing even after they were given a
             shared predicate: applyDashFilters (and the facet counter) run over
             the whole list BEFORE this loop body executes for any row, so the
             pill read the pre-expiry sentinel map while the row read the
             post-expiry one — and on the next paint the pill read a map mutated
             by the PREVIOUS render. Same function, different inputs, different
             answers. Expiry now happens in normalizeUpgradeState(), once, ahead
             of filtering; this loop is a pure reader. */
          /* ONE predicate, shared with the filter pill (hiveUpgradeState ->
             hiveIsUpgradingNow), so a spinning row is always matched by the
             "Upgrading" facet and vice versa. */
          var isUpgrading = hiveIsUpgradingNow(h, branchName, branchLatest);
          var imageBuilding = (_latestImageStatus[branchName] || '') === 'building';
          var buildingHint = imageBuilding ? ' (image still building — upgrading now pulls the previous image)' : '';
          var status = latestUnknown
            ? ' <span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-left:3px" title="Resolving latest version…"></span>'
            : isCurrent ? '<span style="color:var(--green);margin-left:3px" title="latest">✓</span>' : '<span style="color:var(--red);margin-left:3px" title="behind latest ' + esc(branchLatest) + '">↑</span>';
          var upgradeIcon = '';
          if (isUpgrading) {
            var switchStale = isSwitching && _switchStartedAt[h.id] && (Date.now() - _switchStartedAt[h.id] > SWITCH_STALE_MS);
            var progressLabel = isSwitching ? (switchStale ? 'Switching to ' + esc(targetBranch) + ' — taking longer than expected' : 'Switching to ' + esc(targetBranch)) : 'Upgrading';
            var progressTitle = isSwitching ? (switchStale ? 'The hive has not reported branch ' + esc(targetBranch) + ' yet — it may be offline or its build predates in-cluster switch support. It will apply on its next successful check-in.' : 'Rolling out ' + esc(h.upgradeTarget || '') + ' — the pill updates when the hive reports the new branch') : 'Upgrading to ' + esc(branchLatest || h.upgradeTarget || '?');
            /* In-flight state is not clickable, so it loses the box entirely:
               a border around a non-target was the padding that made this the
               tall line. The spinner is the affordance-free progress signal. */
            /* The elapsed counter rides INSIDE the pill so a long-running
               upgrade cannot be read as progress. It self-suppresses when the
               start time is unknown or the rollout is still fast (see
               renderUpgradeElapsed), so a healthy fleet looks unchanged. */
            upgradeIcon = '<span title="' + progressTitle + '" style="font-size:0.7rem;white-space:nowrap;opacity:0.8;color:var(--muted)"><span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:currentColor;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>' + progressLabel + '</span>' + renderUpgradeElapsed(h);
          } else if (!isCurrent && !latestUnknown && isHosted && h.role === 'owner' && h.autoUpgrade) {
            /* STATE and ACTION are split. The old single "queued" pill WAS the
               click-to-upgrade affordance, and owners could not find it (real
               feedback: "that line that says 'queued' is the upgrade button —
               it's not straightforward"). The badge now only NAMES the state
               and its target; the manual escape hatch — never gated by the
               schedule — is its own labelled "Upgrade now" action beside it.
               A daily-mode hive is NOT upgrading "shortly" — saying so would be
               a lie the operator would notice hours later, so the window is
               still named on the badge. */
            var queuedDaily = h.autoUpgradeMode === AUTO_UPGRADE_DAILY;
            var queuedLabel = queuedDaily ? 'Queued for auto-upgrade · 1pm ET' : 'Queued for auto-upgrade';
            /* The tooltip names the TARGET — SHA plus the tracked
               branch/channel label — so "queued for what?" never needs a
               second click to answer. */
            var queuedTitle = queuedDaily
              ? 'Auto-upgrade will apply ' + branchLatest + ' (' + versionLabel(versionSel) + ') at the next 1pm ET window' + buildingHint
              : 'Auto-upgrade will apply ' + branchLatest + ' (' + versionLabel(versionSel) + ') shortly' + buildingHint;
            /* REFUSED, not queued. autoUpgradeBlocked is the hub's own evaluated
               decision (upgradeCollectible false), sent by the fleet API rather
               than re-derived here — the badge must never claim an upgrade is
               coming for a hive triggerAutoUpgrades() declines every cycle and
               will keep declining until an operator intervenes. The mode-derived
               label above says nothing about eligibility, which is exactly how a
               hive sat 89 commits behind still advertising "· 1pm ET".
               "Upgrade now" stays beside it: the manual path is pushed through a
               different route and remains the operator's escape hatch. */
            if (h.autoUpgradeBlocked) {
              queuedLabel = 'Auto-upgrade blocked';
              queuedTitle = 'The hub will not arm an auto-upgrade for this hive: ' +
                (h.autoUpgradeBlockedReason || 'it cannot collect the upgrade instruction') +
                '. It will not resolve on its own.' + buildingHint;
            }
            /* escAttr, not esc, for the title: esc() leaves quotes intact and a
               branch name or commit subject carrying one would break out of the
               attribute. jsArg supplies its own quotes for the handler args. */
            upgradeIcon = '<span title="' + escAttr(queuedTitle) + '" style="' + UPGRADE_STATE_BADGE_STYLE + '">' + esc(queuedLabel) + '</span>' +
              '<span id="upgrade-' + escAttr(h.id) + '" role="button" tabindex="0"' +
              ' onclick="upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')"' +
              ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')}"' +
              ' title="' + escAttr('Upgrade to ' + branchLatest + ' now instead of waiting for auto-upgrade' + buildingHint) + '" style="' + UPGRADE_LINK_STYLE + ';color:var(--green);font-weight:600;font-size:0.7rem">Upgrade now</span>';
          } else if (!isCurrent && !latestUnknown && isHosted && h.role === 'owner') {
            /* Manual path (auto-upgrade OFF): a real primary-ish button, not
               text-with-an-underline. The bare word "Upgrade" read as a label,
               not an action; naming the target ("Upgrade available → <sha>")
               plus the box, the ↑ glyph and a hover state make it unmissably
               the thing to click. */
            var latestShort = branchLatest ? String(branchLatest).substring(0, 7) : 'latest';
            upgradeIcon = '<span id="upgrade-' + escAttr(h.id) + '" role="button" tabindex="0"' +
              ' onclick="upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')"' +
              ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')}"' +
              ' onmouseover="this.style.background=\'' + UPGRADE_BTN_HOVER_BG + '\'" onmouseout="this.style.background=\'' + UPGRADE_BTN_BG + '\'"' +
              ' title="' + escAttr('Current: ' + sha + ' → Latest: ' + branchLatest + buildingHint) + '" style="' + UPGRADE_BTN_STYLE + '">↑ Upgrade available → ' + esc(latestShort) + '</span>';
          } else if (!isCurrent && !latestUnknown && isHosted) {
            /* Non-owners get the STATE with no action: they cannot trigger the
               upgrade, so a button would be a lie, but saying nothing made a
               behind hive read as current. Passive badge, no handler. */
            upgradeIcon = '<span title="' + escAttr('A newer build is available (' + branchLatest + ') — the hive owner or an admin can trigger the upgrade') + '" style="' + UPGRADE_STATE_BADGE_STYLE + '">Update available</span>';
          } else if (latestUnknown && isHosted && h.role === 'owner') {
            upgradeIcon = '<span title="Waiting for latest version…" style="font-size:0.7rem;color:var(--muted);white-space:nowrap;cursor:not-allowed;opacity:0.5">Upgrade</span>';
          }
          /* Auto-upgrade control: a single select replaces the old checkbox.
             The dense table has no room for a checkbox PLUS a mode dropdown,
             and two controls for one setting invites the illegal-looking
             "off but daily" state. One select shows the effective mode at a
             glance — no dialog, no hover needed — which is the whole point:
             an operator scanning the fleet must be able to see which hives
             restart on sight of a new commit and which wait for the evening.
             The id travels in a data-* attribute and the handler is bound by
             delegation (see bindAutoUpgradeSelects) because esc() does NOT
             escape quotes and is unsafe to interpolate into an attribute. */
          var autoUpgradeCheck = '';
          if (isHosted && h.role === 'owner') {
            var mode = h.autoUpgrade
              ? ((h.autoUpgradeMode === AUTO_UPGRADE_DAILY || h.autoUpgradeMode === AUTO_UPGRADE_WEEKLY) ? h.autoUpgradeMode : AUTO_UPGRADE_INSTANT)
              : AUTO_UPGRADE_OFF;
            var opts = '';
            for (var oi = 0; oi < AUTO_UPGRADE_OPTIONS.length; oi++) {
              var opt = AUTO_UPGRADE_OPTIONS[oi];
              /* Each OPTION carries its own full-meaning title too, so opening
                 the list explains the three icons without a legend elsewhere. */
              opts += '<option value="' + escAttr(opt.value) + '" title="' + escAttr(opt.ariaLabel) + '"' + (mode === opt.value ? ' selected' : '') + '>' + esc(opt.label) + '</option>';
            }
            var d = document.createElement('select');
            d.className = 'auto-upgrade-select';
            d.setAttribute('data-hive-id', h.id);
            /* The label is now icons only, so the meaning has to live in the
               accessible name and the tooltip or it is simply lost. aria-label
               names the CURRENT mode; title explains what the modes are. */
            d.setAttribute('aria-label', autoUpgradeAriaLabel(mode));
            d.title = autoUpgradeAriaLabel(mode) + ' — ' + AUTO_UPGRADE_TITLE;
            d.setAttribute('style', 'font-size:0.65rem;color:var(--muted);background:var(--surface);border:1px solid var(--border);border-radius:4px;' +
              'height:' + AUTO_SELECT_H_PX + 'px;min-width:' + AUTO_SELECT_MIN_W_PX + 'px;padding:0 ' + AUTO_SELECT_PAD_X_PX + 'px;cursor:pointer;vertical-align:middle;line-height:' + AUTO_SELECT_H_PX + 'px');
            d.innerHTML = opts;
            autoUpgradeCheck = d.outerHTML;
          }
          var shaMsg = _commitMessages[sha] || _latestSHAMessages[branchName] || '';
          /* FOUR stacked lines, ONE element per line. The column is as wide as
             its widest LINE, so as long as no two controls share a line the
             width collapses to the widest single element — which, now that the
             select is icon-only, is the 56px select rather than the ~150px
             "[queued · 1pm ET] [Auto: daily 1pm ET]" pair that used to set it.
               1. the branch pill, which the owner can CHANGE (it is a switcher,
                  so it owns a line and stays a full tap target);
               2. the SHA and its current/behind glyph — two views of the same
                  immutable fact, "what commit is this hive on". These are one
                  element for width purposes: the glyph is 10px and rides
                  alongside the hash rather than setting the line's width.
               3. what happens NEXT: the upgrade affordance alone;
               4. the icon-only auto-upgrade mode select alone.
             Lines 3 and 4 are omitted entirely when empty — a read-only viewer
             gets two lines, not four with two blanks. Every fragment below is
             embedded verbatim, so ids, handlers and tooltips are preserved. */
          /* The drift dot rides on the SHA line, right of the current/behind
             glyph: "what commit is this hive on, and does it match the fleet" is
             one thought. It sets no extra line, so a drifting hive is no taller. */
          var behindKnown = h.commitsBehindStableV4 !== undefined && h.commitsBehindStableV4 !== null;
          var behindBadge = '';
          if (behindKnown && h.commitsBehindStableV4 > 0) {
            behindBadge = ' <span style="display:inline-block;padding:1px 6px;border-radius:999px;font-size:0.6rem;background:rgba(210,153,34,0.14);color:var(--yellow);border:1px solid rgba(210,153,34,0.35);white-space:nowrap" title="' + escAttr(h.commitsBehindStableV4 + ' commits behind stable v4 tip ' + (_stableV4SHA || '')) + '">' + esc(h.commitsBehindStableV4) + ' behind</span>';
          } else if (!behindKnown && _stableV4SHA && sha && !sameShaJS(sha, _stableV4SHA)) {
            behindBadge = ' <span style="display:inline-block;padding:1px 6px;border-radius:999px;font-size:0.6rem;background:rgba(210,153,34,0.10);color:var(--yellow);border:1px solid rgba(210,153,34,0.25);white-space:nowrap" title="' + escAttr('Could not compare this commit with stable v4 tip ' + _stableV4SHA) + '">? behind</span>';
          }
          var shaLine = '<span style="font-family:monospace;color:var(--muted)" title="' + escAttr(shaMsg) + '">' + esc(sha) + '</span>' + status + behindBadge + (driftDot ? ' ' + driftDot : '');
          versionCell = '<div style="' + STACKED_CELL_STYLE + '">' +
            '<div style="' + STACKED_LINE_STYLE + '">' + branch + '</div>' +
            '<div style="' + STACKED_LINE_STYLE + '">' + shaLine + '</div>' +
            (upgradeIcon ? '<div style="' + STACKED_LINE_STYLE + '">' + upgradeIcon + '</div>' : '') +
            (autoUpgradeCheck ? '<div style="' + STACKED_LINE_STYLE + '">' + autoUpgradeCheck + '</div>' : '') +
            '</div>';
        /* No version reported, but drift can still exist (e.g. heartbeat-stale on a
           spoke too old to report a SHA) — surface the dot beside the dash so
           folding Drift into Version never hides a signal. */
        } else { versionCell = '<span style="color:var(--muted)">—</span>' + (driftDot ? ' ' + driftDot : ''); }
        var pendingBadge = (h.pendingRequestCount > 0 && (roleAtLeast(h.role, 'read-write')))
          ? '<span style="position:absolute;top:-2px;right:-2px;background:var(--blue);color:#fff;border-radius:50%;width:16px;height:16px;font-size:0.6rem;display:flex;align-items:center;justify-content:center;font-weight:700">' + h.pendingRequestCount + '</span>'
          : '';
        var pendingPill = '';
        if (h.pendingRequestCount > 0 && (roleAtLeast(h.role, 'read-write'))) {
          pendingPill = '<a href="#" onclick="togglePendingRow(\'' + esc(h.id) + '\');return false" style="display:inline-flex;align-items:center;gap:4px;padding:3px 10px;background:rgba(59,130,246,0.12);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none;cursor:pointer;white-space:nowrap">&#x1F514; ' + h.pendingRequestCount + ' pending</a>';
        }
        // 12 columns after the 15-to-9 fold. The visible cells are: bulk-select,
        // the ⋮ menu, Hive, Location, Uptime, Version, Repos, Maturity, Agents,
        // Tokens, Mode, Activity. Four folds collapsed six columns:
        //   PROV    → date lives in the status hover; sort rides the Uptime header
        //   DRIFT   → dot on the Version cell (driftBadge, full hover preserved)
        //   ACMM+JOURNEY  → one Maturity cell (both badges stacked, both sorts kept)
        //   ISSUES+PRS+CONTRIB → one Activity cell (all 3 stats + sparklines, 3 sorts kept)
        // Counted against the <th> cells in the header and the <td> cells emitted
        // below (bulkCheckboxCell contributes one).
        var TOTAL_COLUMNS = 13;
        /* Visibility moved OUT of its own column and under Location: "where
           does this hive run" and "who can see it" are both facts about the
           hive's placement, so they read as one cell, and folding them saves a
           whole column in a table that had 17.

           All THREE original branches survive unchanged — the owner toggle
           (with its 'vis-<id>' element id and onchange handler), the local
           link into Settings, and the read-only ✓/— for everyone else.
           They are deliberately NOT flattened: each answers a different
           question about who may change the setting. */
        var visibilityCell = (function() {
          var pub = !!h.isPublic;
          /* escAttr, not esc: this id lands inside a quoted attribute and a
             hive id carrying a quote would otherwise break out of it. Still
             'vis-' + the hive id, so it stays unique per row exactly as before. */
          var tid = 'vis-' + escAttr(h.id);
          /* Branch 1 — hosted hive the viewer OWNS: a live toggle. The owner is
             the only person who may change listing, so they get the control. */
          if (isHosted && h.role === 'owner') {
            return '<label style="position:relative;display:inline-block;width:' + VIS_SWITCH_W_PX + 'px;height:' + VIS_SWITCH_H_PX + 'px;cursor:pointer">' +
              '<input type="checkbox" id="' + tid + '"' + (pub ? ' checked' : '') +
              /* jsArg (not esc) for the handler argument: esc leaves an
                 apostrophe intact, which would close the attribute quote early.
                 jsArg supplies its own surrounding quotes. */
              ' onchange="toggleVisibility(' + jsArg(h.id) + ',this.checked)" style="opacity:0;width:0;height:0">' +
              '<span style="position:absolute;inset:0;background:' + (pub ? 'var(--green)' : 'var(--border)') + ';border-radius:' + VIS_SWITCH_RADIUS_PX + 'px;transition:background 0.2s"></span>' +
              '<span style="position:absolute;top:' + VIS_KNOB_INSET_PX + 'px;left:' + (pub ? VIS_KNOB_ON_LEFT_PX : VIS_KNOB_INSET_PX) + 'px;width:' + VIS_KNOB_PX + 'px;height:' + VIS_KNOB_PX + 'px;background:#fff;border-radius:50%;transition:left 0.2s"></span>' +
              '</label>';
          }
          /* Branch 2 — LOCAL hive: the hub cannot write its config, so this is
             a signpost to where the setting actually lives (Governor → Hub),
             not a control. Falls back to a plain badge with no dashboard URL. */
          if (isLocal) {
            var dh = h.dashboardUrl && !h.dashboardUrl.includes('localhost') ? h.dashboardUrl : '';
            var badge = pub ? '<span style="color:var(--green)">Public</span>' : '<span style="color:var(--muted)">Private</span>';
            return dh
              ? '<a href="' + escAttr(dh) + '#config/governor/Hub" target="_blank" title="Change in Settings → Hub tab" style="text-decoration:none;cursor:pointer">' + badge + ' <span style="font-size:0.6rem;color:var(--muted)">↗</span></a>'
              : badge;
          }
          /* Branch 3 — everyone else: read-only state, no affordance implied. */
          return pub ? '<span style="color:var(--green)">✓</span>' : '<span style="color:var(--muted)">—</span>';
        })();
        var locationBadge = isLocal ? '<span style="display:inline-block;padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(107,114,128,0.15);color:#9ca3af;border:1px solid rgba(107,114,128,0.3)">local</span>' : clusterBadge(h.clusterId, h.clusterName);
        /* Location on top, visibility beneath, GitHub host last — three stacked
           lines, matching the three-line Version treatment. The owner toggle is
           a 36x20 switch and keeps its own box on its own line, so it stays an
           easy tap target.

           The host line reuses githubHostPill — the same helper the pending
           action cards and the Past Requests table use — so a GHE hive reads
           identically wherever it appears. An absent host renders as
           "github.com" rather than a blank chip, which is what an old
           heartbeat-only spoke that cannot yet report its host should show. */
        /* The Kubernetes namespace is NOT shown in the location column — it is
           low-frequency reference metadata (an operator only needs it to build a
           kubectl -n <ns> exec), so it lives in the status hover as an "ns:" line
           (see hiveNamespace(h) / the lines[] builder) rather than competing for
           space in a permanent column. */
        var locationCell = '<div style="' + STACKED_CELL_STYLE + '">' +
          '<div style="' + STACKED_LINE_STYLE + '">' + locationBadge + '</div>' +
          '<div style="' + STACKED_LINE_STYLE + ';font-size:0.7rem">' + visibilityCell + '</div>' +
          '</div>';
        var pendingExpandRow = '';
        if (h.pendingRequestCount > 0 && (roleAtLeast(h.role, 'read-write')) && (h.pending_requests || []).length > 0) {
          var prItems = (h.pending_requests || []).map(function(pr) {
            var avatar = linkedAvatar(pr.username, LIST_AVATAR_PX, pr.username, 'margin-right:6px');
            var note = (pr.note || '').trim();
            var noteHtml = note
              ? '<div style="margin-top:4px;font-size:0.75rem;color:var(--text);white-space:pre-wrap;word-break:break-word;background:rgba(0,0,0,0.15);border-left:2px solid var(--accent);padding:4px 8px;border-radius:2px">' + esc(note) + '</div>'
              : '<div style="margin-top:4px;font-size:0.72rem;color:var(--muted);font-style:italic">(no note)</div>';
            return '<div style="padding:6px 0;border-bottom:1px solid var(--border)">' +
              '<div style="display:flex;align-items:center;justify-content:space-between">' +
              '<div>' + avatar + '<span style="font-size:0.85rem">' + esc(pr.username) + '</span></div>' +
              '<div style="display:flex;gap:4px">' +
              '<button onclick="inlineApproveAccess(\'' + esc(h.id) + '\',\'' + esc(pr.username) + '\',this)" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Approve</button>' +
              '<button onclick="inlineDenyAccess(\'' + esc(h.id) + '\',\'' + esc(pr.username) + '\',this)" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Deny</button>' +
              '</div></div>' + noteHtml + '</div>';
          }).join('');
          pendingExpandRow = '<tr id="pending-row-' + esc(h.id) + '"' + ((i % 2 === 1) ? ' class="hive-row-alt"' : '') + ' style="display:none"><td colspan="' + TOTAL_COLUMNS + '"><div style="padding:8px 16px;background:rgba(59,130,246,0.05);border-radius:6px;margin:4px 0">' + prItems + '</div></td></tr>';
        }
        /* Stable per-hive anchor so the "Attention needed" panel can scroll a
           specific row into view and highlight it. Built from the hive id, which
           is unique across both the assigned and unassigned sections. */
        return '<tr id="' + escAttr(hiveRowDomId(h.id)) + '"' + ((i % 2 === 1) ? ' class="hive-row-alt"' : '') + '>' +
          bulkCheckboxCell(h, section || 'all') +
          '<td class="hive-menu-cell" style="position:relative;width:30px;text-align:center;overflow:visible">' + ('<span style="cursor:pointer;font-size:1.1rem;color:var(--muted);user-select:none">⋮</span>' + pendingBadge + '<div class="hive-menu-dropdown" style="display:none;position:absolute;left:0;bottom:auto;background:#1c2128;border:1px solid #30363d;border-radius:8px;min-width:160px;padding:4px 0;z-index:1000;box-shadow:0 8px 24px rgba(0,0,0,0.5)">' + menuItems.join('') + '</div>') + '</td>' +
          '<td style="text-align:left;line-height:1.4">' + (function() { var isHostedRow = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-'))); var dh = isHostedRow && h.id ? ('/api/saas/hives/' + encodeURIComponent(h.id) + '/open') : (rb ? esc(rb) : ''); /* Label derived from the PROJECT (org + primary repo), not by splitting h.name — see hiveLabel. */ var label = hiveLabel(h); var orgName = label.line1; var repoName = label.line2; /* rp is the repo path shown in the GitHub-icon tooltip; use the same doubling-safe path the label shows (repoDisplayLine / hiveLabel), never org + '/' + primaryRepo which doubles the owner on a github.io/GHE hive whose primaryRepo is already 'owner/repo'. */ var rp = repoName || ''; /* The icon href must resolve to a real GitHub path. When primaryRepo already carries 'owner/repo', that pair IS the owner/repo the URL needs (not org, which may be a mis-parsed host); otherwise fall back to org + primaryRepo. */ var hasRepoPath = h.primaryRepo && h.primaryRepo.indexOf('/') !== -1; var rpOwner = hasRepoPath ? h.primaryRepo.split('/')[0] : h.org; var rpName = hasRepoPath ? h.primaryRepo.split('/').slice(1).join('/') : h.primaryRepo; var rpHref = ghRepoURL(hiveForgeHost(h), rpOwner, rpName); var ghIcon = (rp && rpHref) ? '<a href="' + rpHref + '" target="_blank" style="opacity:0.5;vertical-align:middle" title="' + esc(rp) + '"><svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor" style="vertical-align:middle"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg></a>' : (rp ? '<span style="opacity:0.5;vertical-align:middle" title="' + esc(rp) + '"><svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor" style="vertical-align:middle"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg></span>' : ''); /* vanityDisplay is the friendly host shown in the status bar on hover. rb is resolvedBase(h), which the hub has already overlaid with the claimed vanity_url; it is only a DISPLAY url here, never the click target — see ssoDisplayLink. Empty for a row with no vanity host, which falls back to today's /open href. Only meaningful on hosted rows: a non-hosted row's dh IS rb already. */ var vanityDisplay = isHostedRow && rb ? rb : ''; var link = function(text, bold) { if (dh) { return ssoDisplayLink(dh, vanityDisplay, text, bold ? 'hive-name-link' : 'hive-sub-link'); } var s = bold ? 'font-weight:700;color:inherit' : 'color:#6b7280;font-weight:400'; return '<span style="' + s + '">' + esc(text) + '</span>'; }; /* repoLink wraps the second line (the org/repo) in a link to the ACTUAL FORGE REPO (github.com / GHE host), NOT the spoke dashboard: the top line already opens the dashboard, so the repo path should take an operator to the code. rpHref is the same doubling-safe forge URL the GitHub icon uses (ghRepoURL(hiveForgeHost(h), rpOwner, rpName)); when it is empty (BYO/mis-parsed host) fall back to plain non-link text, never to the dashboard. */ var repoLink = function(text) { if (rpHref) { return '<a href="' + rpHref + '" target="_blank" rel="noopener" class="hive-sub-link" title="Open repository on ' + escAttr((hiveForgeHost(h) || 'github.com')) + '">' + esc(text) + '</a>'; } return '<span style="color:#6b7280;font-weight:400">' + esc(text) + '</span>'; }; var line1 = dot + ' ' + link(orgName, true) + heartbeatHeart(h) + nameEditAffordance(h); var fcPill = h.online ? failingCheckSummary(h) : ''; /* Advisory-staleness pill sits right beside the failing-check pill: both are "something is quietly wrong with this working hive" signals, and advisoryStaleSummary already self-suppresses (empty string) unless the hub flagged the digest stale, so unaffected rows are pixel-identical. */ var advPill = h.online ? advisoryStaleSummary(h) : ''; /* Advisory finding-count pill rides beside the staleness pill and carries the "(top N)" marker when the spoke capped its digest. */ var advCountPill = h.online ? advisoryFindingsSummary(h) : ''; /* Dead-link pill is deliberately NOT gated on h.online: the entire point is a hive that IS online and heartbeating while its public URL is broken, so gating it the way the other pills are gated would hide exactly the case it exists to surface. */ var dlPill = deadLinkSummary(h); var privatePill = privateURLSummary(h); /* Inference-auth pill: like the dead-link pill it is NOT gated on h.online, because the hive being online while every inference call 401s is exactly the case it surfaces. */ var iaPill = inferenceAuthSummary(h); /* Inline access faces sit on the name cell's second line, immediately after this row's own role badge: the badge already answers "what am I on this hive", so the co-members read as the natural continuation of the same thought, in the one cell that is left-aligned and has room to grow. It also keeps them out of the 16 dense metric columns, none of which is about people. Empty string when the viewer is the only member, so those rows are pixel-identical to today. */ var accessFaces = hiveAccessAvatars(h); /* Keyed off repoName, not orgName: line 1 now always carries SOME identity, so the presence of a second line is decided purely by whether there is a repo to put on it. Without a repo the row still shows the GitHub icon, role badge, faces and failing-check pill on the compact variant. */ var line2 = repoName ? '<div style="padding-left:18px;font-size:0.8rem">' + repoLink(repoName) + ' ' + ghIcon + ' ' + roleBadge(h.role) + accessFaces + fcPill + advPill + advCountPill + dlPill + privatePill + iaPill + '</div>' : '<div style="padding-left:18px">' + ghIcon + ' ' + roleBadge(h.role) + accessFaces + fcPill + advPill + advCountPill + dlPill + privatePill + iaPill + '</div>'; var line3 = pendingPill ? '<div style="margin-top:4px;padding-left:18px">' + pendingPill + '</div>' : ''; return line1 + line2 + line3; })() + '</td>' +
          '<td>' + locationCell + '</td>' +
          '<td style="white-space:nowrap">' + uptimeCell(h) + '</td>' +
          /* No white-space:nowrap on the cell itself: the stacked lines each
             carry their own nowrap, so the SHA still never breaks mid-hash,
             but the COLUMN is no longer forced to the width of every part laid
             end to end. That nowrap was the main reason this column was wide. */
          '<td style="font-size:0.7rem">' + versionCell + '</td>' +
          /* Repo count over the GitHub-instance pill: the host qualifies WHICH
             GitHub these repos live on, so the two belong together. Stacked
             rather than inline to keep the numeric column narrow. */
          /* Repos cell. AI Author is stacked as the third line here (it used to own
             a column): who this hive authors PRs as is naturally repo/identity
             context, and folding it in reclaims a column. aiAuthorEffective already
             folds in ai_author (it returns ai_author when set, else the App bot
             "<slug>[bot]", else empty); a hive with no usable GitHub App renders
             "—", never a stale personal ai_author it can't act as. Prefixed "as:"
             so the line reads as the authoring identity, not another repo. */
          '<td title="' + esc((h.repos || []).join('\n')) + '" style="cursor:' + (repoCount > 0 ? 'help' : 'default') + '">' +
            '<div style="' + STACKED_CELL_STYLE + '">' +
              '<div style="' + STACKED_LINE_STYLE + '">' + repoCount + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + '" title="GitHub instance these repos live on">' + githubHostPill(hiveForgeHost(h)) + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + ';color:var(--muted)" title="GitHub identity this hive opens PRs/commits as (— = no GitHub App installed yet)">as: ' + esc(h.aiAuthorEffective || '—') + '</div>' +
            '</div>' +
          '</td>' +
          /* MATURITY: the ACMM level badge and the Journey status stacked into
             ONE cell. Both answer "how far along is this hive's adoption" — the
             ACMM level is where it IS, the journey is the next step it OWES — so
             they read as one column instead of two. Each badge keeps its own
             hover (acmmBadge's title explains the level, journeyBadge's the
             stage), so no datum is lost; both sorts stay reachable from the
             MATURITY header's two ⇅ controls (acmmLevel and journey). */
          '<td>' +
            '<div style="' + STACKED_CELL_STYLE + '">' +
              '<div style="' + STACKED_LINE_STYLE + '">' + acmmBadge(h.acmmLevel) + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + '">' + journeyBadge(h.journey) + '</div>' +
            '</div>' +
          '</td>' +
          '<td title="' + esc((h.agents || []).map(function(a){ var label = a.name + ' (' + a.state + ')'; if (a.mode === 'on_demand') label += ' — on demand'; if (a.paused) label += ' — ' + agentPauseProvenance(a); return label; }).join('\n')) + '" style="cursor:' + ((h.agentCount || 0) > 0 ? 'help' : 'default') + '">' + (h.agentCount || 0) + '</td>' +
          '<td title="Cumulative tokens consumed, as of the last heartbeat" style="white-space:nowrap;cursor:help">' + fmtTokens(h.totalTokens24h || 0) + '</td>' +
          '<td>' + modeCell + '</td>' +
          /* ACTIVITY: Issues, PRs and Contributors — three mini-stats that were
             three columns — stacked densely into ONE cell. Every number and both
             sparklines survive verbatim; each line is labelled (I/PR/C) and
             carries a native title so a folded value is never ambiguous. All
             three sorts stay reachable from the ACTIVITY header's three ⇅
             controls (actionableIssues, actionablePRs, activeContributors). */
          '<td style="font-size:0.72rem">' +
            '<div style="' + STACKED_CELL_STYLE + ';align-items:flex-start">' +
              '<div style="' + STACKED_LINE_STYLE + '" title="Actionable issues"><span style="color:var(--muted);min-width:24px;display:inline-block">Iss</span>' + sparkline(h.issueHistory, '#f59e0b', 40, 12) + (h.actionableIssues || 0) + wsBadge(h.workSource) + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + '" title="Actionable PRs"><span style="color:var(--muted);min-width:24px;display:inline-block">PRs</span>' + sparkline(h.prHistory, '#3b82f6', 40, 12) + (h.actionablePRs || 0) + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + '" title="Active contributors"><span style="color:var(--muted);min-width:24px;display:inline-block">Ctr</span>' + (h.activeContributors || 0) + '</div>' +
            '</div>' +
          '</td>' +
          /* QUADRANT: the small kite. Shape only at this size — the numbers
             live in its hover, which is the same chart drawn large. */
          '<td>' + quadrantCell(h) + '</td>' +
          '</tr>' + pendingExpandRow;
      };
      /* Section-header row: a labeled separator spanning all columns, styled to
         match the table's muted uppercase heading treatment (see .hive-table th). */
      /* Count of <th> cells in the hive table header below. The section-header
         row spans all of them; a stale value would leave the separator short
         and the table visibly ragged. 12 after the 15-to-9 fold (PROV, DRIFT,
         ACMM/JOURNEY→Maturity, ISSUES/PRS/CONTRIB→Activity), then 13 with the
         Quadrant column. Must stay equal to TOTAL_COLUMNS. */
      var TOTAL_COLUMNS_HEADER = 13;
      /* The header is a click target that expands/collapses its section. The
         caret mirrors aria-expanded so the affordance and the a11y state can
         never disagree. sectionKey also scopes the select-all checkbox to THIS
         section's rows only (Assigned and Unassigned select independently);
         the checkbox stops propagation so selecting does not also collapse.
         selectable=false suppresses the box for sections whose rows are never
         bulk-eligible (unassigned placeholders), where it would be a control
         that visibly does nothing. */
      var sectionHeader = function(label, count, sectionKey, selectable) {
        var collapsed = !!_dashSectionCollapsed[sectionKey];
        return '<tr class="hive-section-head" role="button" tabindex="0" aria-expanded="' + (collapsed ? 'false' : 'true') + '" ' +
          'onclick="toggleHiveSection(\'' + esc(sectionKey) + '\')" ' +
          'onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();toggleHiveSection(\'' + esc(sectionKey) + '\')}" ' +
          'title="Click to ' + (collapsed ? 'expand' : 'collapse') + '">' +
          '<td colspan="' + TOTAL_COLUMNS_HEADER + '" ' +
          'style="padding:14px 12px 6px;color:var(--muted);font-weight:600;font-size:0.75rem;' +
          'text-transform:uppercase;letter-spacing:0.5px;text-align:left">' +
          '<span class="hive-section-caret" aria-hidden="true">' + (collapsed ? '▸' : '▾') + '</span>' +
          (sectionKey && selectable !== false ? bulkSectionCheckbox(sectionKey) : '') +
          esc(label) + ' (' + count + ')</td></tr>';
      };
      /* Group-header row: nested one indent step inside a section header, with
         a caret showing collapse state and a count. Clicking toggles. Labels are
         user-controlled (org, owner, cluster and branch names all come from
         spoke heartbeats) so they are escaped here — and again in the onclick
         argument, where the id is additionally quote-escaped.

         This deliberately mirrors sectionHeader above — same caret glyphs, same
         role/tabindex/aria-expanded, same keyboard activation — because a group
         header and a section header are the same affordance at two nesting
         levels. Only the persistence key differs: sections store one flag each
         under hive-section-*-collapsed, groups store a set under
         hive-group-collapsed (their ids are dynamic and unbounded). */
      var GROUP_HEADER_INDENT_PX = 26;
      var groupHeader = function(label, count, collapsed, groupId) {
        var toggle = 'toggleDashGroup(' + jsArg(groupId) + ')';
        return '<tr class="hive-group-head" role="button" tabindex="0" aria-expanded="' + (collapsed ? 'false' : 'true') + '" ' +
          'onclick="' + toggle + '" ' +
          'onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();' + toggle + '}" ' +
          'title="Click to ' + (collapsed ? 'expand' : 'collapse') + '">' +
          '<td colspan="' + TOTAL_COLUMNS_HEADER + '" ' +
          'style="padding:8px 12px 6px ' + GROUP_HEADER_INDENT_PX + 'px;color:var(--muted);font-weight:600;' +
          'font-size:0.72rem;letter-spacing:0.3px;text-align:left">' +
          '<span class="hive-group-caret" aria-hidden="true">' + (collapsed ? '▸' : '▾') + '</span>' +
          esc(label) + ' <span style="opacity:0.7">(' + count + ')</span></td></tr>';
      };

      /* renderSection emits one Assigned/Unassigned section: its header, then
         either a flat run of rows or the grouped subsections. It takes the
         running index by reference through the counter object so the caller's
         GLOBAL index keeps advancing across sections AND groups — menu ids
         (hive-menu-<i>) must stay unique across the whole table or the ⋮
         dropdowns open the wrong row's menu.

         Critically, the index advances for rows inside COLLAPSED groups too:
         the index is a per-hive identity, not a per-visible-row position, so
         expanding a group must not renumber anything below it. */
      var renderSection = function(label, sectionKey, list, counter, selectable) {
        if (!(list || []).length) return '';
        var out = sectionHeader(label, list.length, sectionKey, selectable);
        /* The section is the OUTER collapse: when it is closed nothing inside
           it renders — not its rows and not its group headers. The counter
           still advances over every hive so menu ids stay stable. */
        var sectionOpen = !_dashSectionCollapsed[sectionKey];
        if (_dashGroupBy === HIVE_GROUP_NONE) {
          for (var fi = 0; fi < list.length; fi++) {
            var flatRow = buildRow(list[fi], counter.i++, sectionKey);
            if (sectionOpen) out += flatRow;
          }
          return out;
        }
        /* groupHives only ever emits non-empty buckets, so no empty group
           header can render. */
        var groups = groupHives(list, sectionKey);
        for (var gi = 0; gi < groups.length; gi++) {
          var g = groups[gi];
          var isCollapsed = !!_dashGroupCollapsed[g.id];
          if (sectionOpen) out += groupHeader(g.label, g.hives.length, isCollapsed, g.id);
          for (var ri = 0; ri < g.hives.length; ri++) {
            var rowHTML = buildRow(g.hives[ri], counter.i++, sectionKey);
            if (sectionOpen && !isCollapsed) out += rowHTML;
          }
        }
        return out;
      };

      var rows;
      if (_isAdmin) {
        /* Admin-only organizational aid: split into assigned (real, claimed)
           hives and unassigned placeholders — see isPlaceholderHive. Preserve
           incoming order so each section stays sorted.

           This split is the OUTER structure — grouping subdivides each side,
           it never replaces the split. */
        var assigned = [], unassigned = [];
        for (var _hi = 0; _hi < hives.length; _hi++) {
          var _h = hives[_hi];
          if (isPlaceholderHive(_h)) unassigned.push(_h); else assigned.push(_h);
        }
        /* Global running index across BOTH sections so menu ids (hive-menu-<i>)
           never collide between sections and the ⋮ dropdowns keep working.
           It advances over EVERY row, including rows inside a collapsed section
           or a collapsed group, so collapsing anything never renumbers the
           menus below it. renderSection carries the counter by reference. */
        var _counter = {i: 0};
        rows = renderSection('Assigned hives', HIVE_SECTION_ASSIGNED, assigned, _counter) +
          /* Unassigned placeholders are never bulk-eligible (nothing is running
             yet), so the section collapses but gets no select-all box. */
          renderSection('Unassigned hives', HIVE_SECTION_UNASSIGNED, unassigned, _counter, false);
      } else if (_dashGroupBy !== HIVE_GROUP_NONE) {
        /* Non-admin with grouping on: one flat set of groups, no section
           headers (a non-admin sees no placeholders to split off). The same
           global counter rule applies. */
        var _flatCounter = {i: 0};
        var _flatGroups = groupHives(hives, 'all');
        rows = '';
        for (var _fgi = 0; _fgi < _flatGroups.length; _fgi++) {
          var _fg = _flatGroups[_fgi];
          var _fgCollapsed = !!_dashGroupCollapsed[_fg.id];
          rows += groupHeader(_fg.label, _fg.hives.length, _fgCollapsed, _fg.id);
          for (var _fri = 0; _fri < _fg.hives.length; _fri++) {
            var _fgRow = buildRow(_fg.hives[_fri], _flatCounter.i++, 'all');
            if (!_fgCollapsed) rows += _fgRow;
          }
        }
      } else {
        /* Non-admin, no grouping: single flat list, exactly as before. */
        rows = hives.map(function(h, i) { return buildRow(h, i, 'all'); }).join('');
      }
      document.getElementById('hives-container').innerHTML =
        fleetQuadrantHeaderHTML() +
        '<div class="table-wrap"><table class="hive-table"><thead><tr>' +
        /* Non-admin lists have no section headers, so the flat list's
           select-all lives in the table head instead. */
        '<th style="width:26px;text-align:center">' + (_isAdmin ? '' : bulkSectionCheckbox('all')) + '</th>' +
        /* A combined-column header carries ONE sort control per folded field: the
           header cell can no longer own a single onclick once it hosts two or three
           sort keys, so each key gets its own inline clickable span. subSort() is
           the shared helper; every folded sort therefore stays reachable directly
           from the header, exactly as the standalone columns were. */
        '<th></th><th onclick="sortDashHives(\'name\')" style="cursor:pointer">Hive ⇅</th><th onclick="sortDashHives(\'clusterId\')" style="cursor:pointer;vertical-align:middle" title="Where this hive runs, and whether it is listed publicly">' + stackHeader('Location /', 'Public ⇅') + '</th>' +
        /* Uptime hosts BOTH temporal sorts: live uptime (startedAt) and the folded
           provision-date sort (registeredAt, the old "Prov ⇅"). The date itself now
           lives in the status hover; only its sort trigger rides here. */
        '<th onclick="sortDashHives(\'startedAt\')" style="cursor:pointer;vertical-align:middle" title="Process uptime since the last restart — a short value that keeps resetting means the pod is restarting">' + stackHeader('Uptime ⇅', subSort('registeredAt', 'Prov ⇅', 'Sort by when this hive was first provisioned (the hub&apos;s first-seen time). The date itself now lives in the status hover.')) + '</th>' +
        '<th title="Version, branch and any configuration drift from the fleet norm — a coloured dot appears beside the commit when this hive drifts; hover it for the specific signals">Version</th><th>Repos</th>' +
        /* MATURITY folds ACMM (where it is) and Journey (what it owes next); both
           sorts survive as inline ⇅ controls. */
        '<th style="vertical-align:middle" title="Adoption maturity: ACMM level and the next journey step">' + stackHeader('Maturity', subSort('acmmLevel', 'ACMM ⇅', 'Sort by ACMM level') + subSort('journey', 'Journey ⇅', 'Where this hive is on the adoption journey: install the GitHub App, assign a method/model (or run ClankeR, the contributor relay), then raise the ACMM level')) + '</th>' +
        '<th onclick="sortDashHives(\'agentCount\')" style="cursor:pointer">Agents ⇅</th><th onclick="sortDashHives(\'totalTokens24h\')" style="cursor:pointer" title="Cumulative tokens consumed, as of the last heartbeat">Tokens ⇅</th><th onclick="sortDashHives(\'governorMode\')" style="cursor:pointer">Mode ⇅</th>' +
        /* ACTIVITY folds Issues, PRs and Contrib; each keeps its own sort ⇅. */
        '<th style="vertical-align:middle" title="Actionable issues, actionable PRs and active contributors">' + stackHeader('Activity', subSort('actionableIssues', 'Iss ⇅', 'Sort by actionable issues') + subSort('actionablePRs', 'PRs ⇅', 'Sort by actionable PRs') + subSort('activeContributors', 'Ctr ⇅', 'Sort by active contributors')) + '</th>' +
        /* QUADRANT folds the four axis sorts plus the composite. Sorting by a
           single axis is the point of the column: "weakest efficiency" turns
           the table into a worklist rather than a picture. */
        '<th style="vertical-align:middle" title="Trust, Efficiency, Satisfaction and Productivity, each scored against the hives in the current view. Hover a kite for the numbers.">' + stackHeader('Quadrant', subSort('quadrant', 'All ⇅', 'Sort by the composite of every scored axis') + subSort('quadrantTrust', 'T ⇅', 'Sort by Trust: autonomy level, governor posture, merge acceptance and enrolled scope') + subSort('quadrantEfficiency', 'E ⇅', 'Sort by Efficiency: token burn rate, spend per merged PR, rework and output per agent') + subSort('quadrantProductivity', 'P ⇅', 'Sort by Productivity: merged PRs, relay throughput, work-source autonomy and work stalled on a human')) + '</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table></div>';
      /* Delegated, so binding once is enough no matter how often the table is
         re-rendered. The guard keeps repeated renders from stacking listeners. */
      if (!_autoUpgradeSelectsBound) { bindAutoUpgradeSelects(); _autoUpgradeSelectsBound = true; }
      /* Re-derive the bar and the select-all tri-state from _bulkSelected now
         that the fresh checkboxes exist in the DOM. */
      renderBulkBar();
      syncBulkSectionHeaderBoxes();
      setTimeout(function() {
        var tw = document.querySelector('.table-wrap');
        if (tw && tw.scrollWidth > tw.clientWidth) tw.classList.add('has-scroll');
      }, 0);
    }

    async function toggleVisibility(id, isPublic) {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/visibility', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({is_public: isPublic})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to change visibility', 'error'); loadHives(); return; }
        hiveToast(id + ' is now ' + (isPublic ? 'public' : 'private'), 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadHives(); }
    }

    /* Auto-upgrade select values. AUTO_UPGRADE_OFF is a UI-only sentinel: on
       the wire "off" is auto_upgrade:false, and the two real modes match the
       server's allowlist ("instant" / "daily"). */
    var _autoUpgradeSelectsBound = false;
    var AUTO_UPGRADE_OFF = 'off';
    var AUTO_UPGRADE_INSTANT = 'instant';
    var AUTO_UPGRADE_DAILY = 'daily';
    var AUTO_UPGRADE_WEEKLY = 'weekly';
    /* Copy is framed around DISRUPTION, not cron mechanics: the operator is
       choosing when it is acceptable to interrupt a hive that is working. */
    var AUTO_UPGRADE_TITLE = 'When to apply new versions. Instant restarts the hive as soon as a new version lands; Daily restarts it at most once a day at 1pm ET; Weekly restarts it at most once a week, Tuesday at 1pm ET, so a stable hive is disturbed as rarely as possible.';
    /* Icon-only labels. The "Auto:" prefix repeated on every row was the widest
       single element in the Version column and said nothing a scanning operator
       did not already know from the column it sits in — so it is carried by the
       title/aria-label instead of being spelled out on every line.

       The three glyphs are chosen to be DISTINCT SHAPES, not intensities of one
       shape: 'off' must not read as a greyed-out 'instant'.
         ⦸  off     — a struck-through circle: the universal "disabled" mark,
                      and the only glyph here with a diagonal stroke.
         ⚡ instant  — a bolt: applies the moment a version lands.
         🕔 5p      — a clock face, plus the literal window "5p", because the
                      hour is the whole point of the mode and no icon conveys it.
       All three are plain text glyphs rather than SVG or background images, so
       they inherit the select's currentColor. The hub renders on one dark
       surface today, but nothing here hard-codes a fill or stroke colour, so a
       light surface would not make any of the three vanish. The label text
       after the glyph is the shortest string that still disambiguates — and it
       is there precisely so the control never degrades to "a picture only":
       ⚡ and 🕔 are emoji whose rendering varies by platform, so the word
       carries the meaning if the glyph renders as a box.

       ariaLabel carries the FULL meaning for screen readers and for the
       per-option tooltip, so nothing is lost by dropping the inline words. */
    var AUTO_UPGRADE_OPTIONS = [
      {value: AUTO_UPGRADE_OFF, label: '⦸ off', ariaLabel: 'Auto-upgrade: off'},
      {value: AUTO_UPGRADE_INSTANT, label: '⚡ instant', ariaLabel: 'Auto-upgrade: instantly when a new version lands'},
      {value: AUTO_UPGRADE_DAILY, label: '🕐 1p', ariaLabel: 'Auto-upgrade: daily at 1pm ET'},
      {value: AUTO_UPGRADE_WEEKLY, label: '🗓 tue 1p', ariaLabel: 'Auto-upgrade: weekly on Tuesday at 1pm ET'}
    ];
    /* Accessible name for the select itself, resolved from the CURRENT mode so
       the control announces what it is set to rather than only what it does. */
    function autoUpgradeAriaLabel(mode) {
      for (var ai = 0; ai < AUTO_UPGRADE_OPTIONS.length; ai++) {
        if (AUTO_UPGRADE_OPTIONS[ai].value === mode) return AUTO_UPGRADE_OPTIONS[ai].ariaLabel;
      }
      return 'Auto-upgrade';
    }

    /* One delegated listener on the container, bound once at startup, so every
       re-render of the table keeps working without rebinding per row. */
    function bindAutoUpgradeSelects() {
      var container = document.getElementById('hives-container');
      if (!container) return;
      container.addEventListener('change', function(e) {
        var el = e.target;
        if (!el || !el.classList || !el.classList.contains('auto-upgrade-select')) return;
        var id = el.getAttribute('data-hive-id');
        if (!id) return;
        setAutoUpgradeMode(id, el.value);
      });
    }

    /* Maps the select's value onto the existing endpoint's two fields. "off"
       sends auto_upgrade:false; the mode is still sent so the server records a
       concrete preference rather than leaving a blank to be re-interpreted. */
    async function setAutoUpgradeMode(id, value) {
      var enabled = value !== AUTO_UPGRADE_OFF;
      var mode = (value === AUTO_UPGRADE_DAILY || value === AUTO_UPGRADE_WEEKLY) ? value : AUTO_UPGRADE_INSTANT;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/auto-upgrade', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({auto_upgrade: enabled, auto_upgrade_mode: mode})
        });
        if (!resp.ok) { hiveToast('Failed to update auto-upgrade', 'error'); loadHives(); return; }
        var label = !enabled ? 'off'
          : (mode === AUTO_UPGRADE_DAILY ? 'daily at 1pm ET'
          : (mode === AUTO_UPGRADE_WEEKLY ? 'weekly on Tuesday at 1pm ET' : 'instant'));
        hiveToast(id + ' auto-upgrade: ' + label, 'success');
        loadHives();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
        loadHives();
      }
    }

    async function toggleAutoUpgrade(id, enabled) {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/auto-upgrade', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({auto_upgrade: enabled})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed', 'error'); loadHives(); return; }
        hiveToast(id + ' auto-upgrade ' + (enabled ? 'enabled' : 'disabled'), 'success');
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadHives(); }
    }

    var _hubUpgrading = false;
    var _hubAutoUpgrade = false;
    async function toggleHubAutoUpgrade(enabled) {
      try {
        var resp = await fetch('/api/saas/hub/auto-upgrade', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({auto_upgrade: enabled})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed', 'error'); return; }
        _hubAutoUpgrade = enabled;
        hiveToast('Hub auto-upgrade ' + (enabled ? 'enabled' : 'disabled'), 'success');
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }
    async function upgradeHub(currentSHA) {
      var toSHA = _latestSHA ? _latestSHA.substring(0, 7) : 'latest';
      var fromSHA = currentSHA ? currentSHA.substring(0, 7) : '?';
      if (!await hiveConfirm('Upgrade Hive Hub?<br><br><span style="font-family:monospace;font-size:0.85rem;color:var(--muted)">' + fromSHA + '</span> → <span style="font-family:monospace;font-size:0.85rem;color:var(--green)">' + toSHA + '</span>', true)) return;
      var btn = document.getElementById('hub-upgrade-btn');
      if (btn) { btn.disabled = true; btn.innerHTML = '<span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:3px"></span>Upgrading'; btn.style.opacity = '0.6'; }
      try {
        var resp = await fetch('/api/saas/hub/upgrade', {method: 'POST'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Hub upgrade failed', 'error'); return; }
        _hubUpgrading = true;
        hiveToast('Hub upgrade started — page will refresh when ready', 'success');
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    /* Admin upgrade kill switch. _upgradePause mirrors the server's persisted
       state ({hub:{paused,by,at}, spokes:{...}}); it rides the my-hives
       top-level payload. Checked = PAUSED. */
    var _upgradePause = {hub: {paused: false}, spokes: {paused: false}};
    function upgradePauseToggleHTML(target, label, title) {
      var sw = (_upgradePause && _upgradePause[target]) || {paused: false};
      var t = title + (sw.paused ? ' — paused by ' + (sw.by || '?') + ' since ' + (sw.at || '?') : '');
      /* Pause/play ICON button with the plain-text label beside it. The icon
         shows the ACTION a click performs: running → ⏸ (pause), paused → ▶
         (resume, red). Text-variation selector (\uFE0E) keeps the glyphs
         monochrome instead of emoji. The bordered flex-column wrapper at the
         call site owns spacing/alignment for the pair. */
      var icon = sw.paused ? '\u25B6\uFE0E' : '\u23F8\uFE0E';
      var btnStyle = 'width:18px;height:18px;display:inline-flex;align-items:center;justify-content:center;' +
        'padding:0;font-size:0.62rem;line-height:1;border-radius:4px;cursor:pointer;' +
        (sw.paused
          ? 'background:color-mix(in srgb, var(--red) 18%, transparent);border:1px solid var(--red);color:var(--red)'
          : 'background:var(--surface);border:1px solid var(--border);color:var(--muted)');
      return '<span style="display:inline-flex;align-items:center;gap:5px;white-space:nowrap" title="' + escAttr(t) + '">' +
        '<button onclick="toggleUpgradePause(\'' + target + '\', ' + (sw.paused ? 'false' : 'true') + ')" style="' + btnStyle + '" aria-label="' + escAttr((sw.paused ? 'Resume ' : 'Pause ') + label) + '">' + icon + '</button>' +
        '<span style="font-size:0.6rem;color:' + (sw.paused ? 'var(--red)' : 'var(--muted)') + ';font-weight:' + (sw.paused ? '700' : '400') + '">' + esc(label) + '</span>' +
        '</span>';
    }
    async function toggleUpgradePause(target, paused) {
      try {
        var resp = await fetch('/api/saas/upgrade-pause', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({target: target, paused: paused})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to update upgrade pause', 'error'); loadHives(); return; }
        if (data.state) _upgradePause = data.state;
        hiveToast((target === 'hub' ? 'Hub' : 'Spoke') + ' upgrades ' + (paused ? 'PAUSED' : 'resumed'), 'success');
        renderUpgradePauseBanner();
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }
    /* renderUpgradePauseBanner paints a prominent banner above the hive list
       while EITHER kill switch is engaged, so a paused fleet is impossible to
       miss. Same placement/styling family as renderPendingBanner. */
    function renderUpgradePauseBanner() {
      var existing = document.getElementById('upgrade-pause-banner');
      if (existing) existing.remove();
      var hubSw = (_upgradePause && _upgradePause.hub) || {};
      var spokeSw = (_upgradePause && _upgradePause.spokes) || {};
      if (!hubSw.paused && !spokeSw.paused) return;
      var container = document.getElementById('hives-container');
      if (!container || !container.parentNode) return;
      var line = function(sw, label, detail) {
        return '<div style="display:flex;align-items:center;gap:10px">' +
          '<span style="font-size:1.1rem">&#x23F8;&#xFE0F;</span>' +
          '<span style="font-size:0.85rem;color:var(--text)"><strong>' + label + '</strong> ' + detail +
          ' — paused by <strong>' + esc(sw.by || 'an admin') + '</strong>' +
          (sw.at ? ' since <span style="font-family:monospace;font-size:0.8rem">' + esc(sw.at) + '</span>' : '') +
          '</span></div>';
      };
      var html = '';
      if (hubSw.paused) html += line(hubSw, 'Hub upgrades are PAUSED', '(the hub stays on its current build)');
      if (spokeSw.paused) html += line(spokeSw, 'Spoke upgrades are PAUSED fleet-wide', '(no automatic image changes reach any hive)');
      var banner = document.createElement('div');
      banner.id = 'upgrade-pause-banner';
      banner.style.cssText = 'background:rgba(239,68,68,0.12);border:1px solid rgba(239,68,68,0.4);border-radius:8px;padding:12px 16px;margin-bottom:16px;display:flex;flex-direction:column;gap:6px';
      banner.innerHTML = html;
      container.parentNode.insertBefore(banner, container);
    }

    var _upgradingHives = {};
    // _deletingHives[id] = true while a hive delete is in flight, so the hub
    // table shows a "Deleting…" status on that row instead of the row just
    // silently vanishing (or looking normal) mid-teardown.
    var _deletingHives = {};
    var _switchStartedAt = {}; // hiveId → ms timestamp the switch was initiated
    var SWITCH_STALE_MS = 8 * 60 * 1000; // warn if a switch hasn't landed in 8 min

    /* ── Upgrade elapsed-time counter ──
       A hive that has been "Upgrading" for a long time is a FAULT SIGNAL, not
       progress: the incident this exists to prevent had 15 hives pinned at
       upgrading:true for 20+ minutes while their spokes exited 0 in a restart
       loop, and a static spinner made a permanently-wedged fleet look
       identical to a merely slow one.

       The thresholds below escalate the counter's colour so that state is
       obvious at a glance instead of requiring the operator to notice a
       number. UPGRADE_ELAPSED_RED_MS deliberately equals staleUpgradeTimeout
       in server.go (10m) — the same instant the hub raises its stuck-upgrade
       alert. Turning red EARLIER would accuse a hive the hub still considers
       healthy; turning red LATER would leave a window where the alert panel
       says stuck and the row still looks fine. The amber step sits at half of
       that, an early warning while the upgrade is still plausibly in flight. */
    var UPGRADE_ELAPSED_RED_MS = 10 * 60 * 1000;  // == staleUpgradeTimeout (server.go): hub calls this stuck
    var UPGRADE_ELAPSED_AMBER_MS = 5 * 60 * 1000; // half the stuck limit: early warning, still plausible

    /* Below this the counter is suppressed entirely. Every upgrade begins at
       0s, and a counter that flickers "1s, 2s…" on every healthy rollout would
       train the operator to ignore it — which is exactly the habit that let the
       incident go unnoticed. It only appears once an upgrade is slow enough to
       be worth a human's attention. */
    var UPGRADE_ELAPSED_MIN_SHOW_MS = 30 * 1000; // don't nag during a normal fast rollout

    /* Clocks are not synchronised. The browser's Date.now() and the hub's
       upgradeStartedAt can disagree by seconds, which yields a small NEGATIVE
       elapsed on a freshly-started upgrade. Tolerate that as "just started"
       rather than rendering a negative duration; anything more negative than
       this is real clock skew and the counter is suppressed instead of shown
       as a lie. */
    var UPGRADE_ELAPSED_SKEW_TOLERANCE_MS = 60 * 1000; // treat small negatives as 0

    /* Go serialises a zero time.Time as "0001-01-01T00:00:00Z" and — because
       json:"...,omitempty" does NOT omit a zero struct — the hub emits exactly
       that string for a hive whose UpgradeStartedAt was never set or was reset.
       Date.parse() accepts it happily, and now − year 0001 is ~17.7 MILLION
       hours: the live "Upgrading 17755944h28m" wedge on ibm-alchemy. Any
       upgradeStartedAt at or before this cutoff is not a real start time but a
       lost/zero one, and the counter must be suppressed rather than rendered as
       a two-thousand-year duration. 2020-01-01 predates the project itself, so
       no genuine upgrade can legitimately fall before it. */
    var UPGRADE_STARTED_MIN_SANE_MS = Date.UTC(2020, 0, 1); // any earlier start time is a lost/zero timestamp

    /* upgradeElapsedMs returns how long a hive has been upgrading, in ms, or
       null when that cannot be known — a MISSING, EMPTY, UNPARSEABLE or
       PRE-EPOCH (zero/lost) upgradeStartedAt, or a negative elapsed beyond the
       skew tolerance. Callers must render nothing on null; they must never
       render NaN, a negative duration, a two-thousand-year duration, or a
       fabricated "0s" that implies the upgrade just started when in truth the
       timestamp was lost.

       Note a real server behaviour this relies on: upgradeStartedAt is stamped
       ONLY where Upgrading is set true, so a hive that is merely QUEUED has no
       timestamp and correctly gets null here rather than a bogus counter. */
    function upgradeElapsedMs(h, nowMs) {
      if (!h) return null;
      var started = h.upgradeStartedAt;
      if (!started || typeof started !== 'string') return null;
      var t = Date.parse(started);
      if (isNaN(t)) return null; /* unparseable timestamp — show nothing, never NaN */
      if (t < UPGRADE_STARTED_MIN_SANE_MS) return null; /* zero/lost start time (year 0001) — never a real "17.7M hour" upgrade */
      var elapsed = (typeof nowMs === 'number' ? nowMs : Date.now()) - t;
      if (elapsed < 0) {
        /* Clock skew. A small negative is the browser being marginally behind
           the hub on an upgrade that genuinely just started; clamp it to zero.
           A large negative means the timestamp is not trustworthy at all. */
        if (elapsed > -UPGRADE_ELAPSED_SKEW_TOLERANCE_MS) return 0;
        return null;
      }
      return elapsed;
    }

    /* formatUpgradeElapsed renders a compact duration: "45s", "3m", "1h12m".
       Minutes are the operator's working unit here, so past an hour it keeps
       the minutes rather than collapsing to a bare "1h" that would hide a
       72-minute wedge behind the same string as a 61-minute one. */
    function formatUpgradeElapsed(ms) {
      if (typeof ms !== 'number' || isNaN(ms) || ms < 0) return '';
      var totalSec = Math.floor(ms / 1000);
      if (totalSec < 60) return totalSec + 's';
      var totalMin = Math.floor(totalSec / 60);
      if (totalMin < 60) return totalMin + 'm';
      var hours = Math.floor(totalMin / 60);
      return hours + 'h' + (totalMin % 60) + 'm';
    }

    /* upgradeElapsedColor escalates the counter past the thresholds above.
       Muted while the rollout is plausibly healthy, amber as an early warning,
       red once the hub itself would call the upgrade stuck. */
    function upgradeElapsedColor(ms) {
      if (typeof ms !== 'number' || isNaN(ms)) return 'var(--muted)';
      if (ms >= UPGRADE_ELAPSED_RED_MS) return 'var(--red)';
      if (ms >= UPGRADE_ELAPSED_AMBER_MS) return 'var(--amber, #f59e0b)';
      return 'var(--muted)';
    }

    /* renderUpgradeElapsed builds the counter fragment appended to the
       "Upgrading" pill. Returns '' whenever the elapsed time is unknown or
       still below the nag threshold, so the common healthy case renders
       exactly what it renders today. */
    function renderUpgradeElapsed(h, nowMs) {
      var ms = upgradeElapsedMs(h, nowMs);
      if (ms === null) return '';
      if (ms < UPGRADE_ELAPSED_MIN_SHOW_MS) return '';
      var label = formatUpgradeElapsed(ms);
      if (!label) return '';
      var stuck = ms >= UPGRADE_ELAPSED_RED_MS;
      /* Past the red line the wording stops describing progress and names the
         fault, because at that point the hub has raised its stuck-upgrade
         alert and "still upgrading" would be the same misleading reassurance
         the incident turned on. */
      var tip = stuck
        ? 'Upgrading for ' + label + ' — past the ' + Math.round(UPGRADE_ELAPSED_RED_MS / 60000) +
          ' minute limit. This upgrade is almost certainly wedged, not slow; check the hive for a failed self-upgrade.'
        : 'Upgrading for ' + label;
      return '<span title="' + escAttr(tip) + '" style="margin-left:4px;font-variant-numeric:tabular-nums;color:' +
        upgradeElapsedColor(ms) + (stuck ? ';font-weight:600' : '') + '">' + esc(label) + '</span>';
    }

    /* Version and Location cells stack their contents vertically so neither
       column is forced as wide as the sum of its parts. The Version cell used
       to lay branch pill + SHA + status + upgrade affordance + auto-upgrade
       select on ONE nowrap line, which made it the widest column in a table
       that already carries 16 of them.

       STACKED_LINE_HEIGHT is deliberately tighter than the table's default so
       the stack stays as short as four lines can be.

       ONE ELEMENT PER LINE. The three-line arrangement did not actually narrow
       the column, because its last line carried TWO controls side by side (the
       upgrade affordance and the auto-upgrade select). A column is as wide as
       its widest line, so that pair — not the short lines above it — set the
       width, and the stacking bought nothing. Splitting them onto their own
       lines is what makes the widest line a single element.

       THE HONEST NUMBERS — do not "simplify" these away and do not restate
       them from arithmetic alone. A previous change to this cell shipped a
       claim of height-neutrality that was false. These were MEASURED in
       Chromium on the rendered cell (getBoundingClientRect), for an owner row
       that is behind latest with auto-upgrade set to daily — the widest and
       tallest variant:
         column width   266.4px -> 110.4px   (-156.0px, -58.6%)
         cell height     66.8px ->  79.8px   (+13.0px)
         stack height    50.8px ->  63.8px   (+13.0px)
       The four lines measure 15.0 / 12.9 / 12.9 / 20.0 px plus 3 gaps of
       STACKED_ROW_GAP_PX. So ROWS DO GROW, by 13px on owner rows. That is the
       price of the width win and it is stated rather than hidden. Two things
       hold it down: the upgrade affordance is now text-with-an-underline rather
       than a padded button (8px of padding and border removed), and the select
       is pinned to AUTO_SELECT_H_PX rather than left to the UA's default
       control height, which is taller.

       What actually sets the width now is the LONGEST upgrade label — the
       "Queued for auto-upgrade · 1pm ET" badge plus its "Upgrade now" action
       (the state/action split that answers the "the 'queued' line IS the
       upgrade button?" confusion) — not the select, which measures 77px.
       The old width was set by the "queued · 1pm ET" label and the
       "Auto: daily 1pm ET" select sitting on ONE line together.

       Non-owner rows are UNCHANGED: they still get at most two lines (28.9px),
       under the two-line name cell's 40.3px, which continues to set their
       height. Only owner rows pay.

       STACKED_ROW_GAP_PX separates the stacked lines without adding a full line
       box. 1px proved too tight in practice: the branch pill, SHA, upgrade link
       and auto-upgrade select read as one crowded block, and the Location cell's
       cluster badge sat flush against its visibility toggle. 4px is enough to
       group them as distinct rows while still costing far less than a line box. */
    var STACKED_LINE_HEIGHT = 1.15;
    var STACKED_ROW_GAP_PX = 4;
    /* Shared style for a stacked cell: a vertical flex column, centred to match
       the table's default text-align:center for non-first cells. */
    var STACKED_CELL_STYLE = 'display:flex;flex-direction:column;align-items:center;justify-content:center;gap:' + STACKED_ROW_GAP_PX + 'px;line-height:' + STACKED_LINE_HEIGHT;
    /* Horizontal gap between the items that share one stacked line (e.g. the
       SHA and its current/behind glyph). Small enough that a line stays visibly
       one group rather than reading as separate columns. */
    var STACKED_ITEM_GAP_PX = 4;
    /* One line inside a stacked cell. Items within a line stay on one row (the
       SHA must never wrap mid-hash) but the LINES themselves are what narrow
       the column. */
    var STACKED_LINE_STYLE = 'display:flex;align-items:center;justify-content:center;gap:' + STACKED_ITEM_GAP_PX + 'px;white-space:nowrap';

    /* Icon-only auto-upgrade select geometry. The control lost its "Auto:"
       prefix, so its box must be sized deliberately rather than left to shrink
       to a glyph: 20px tall and at least 56px wide keeps it a comfortable
       pointer and touch target (the row is 63.8px tall, so 20px is roughly a
       third of it — visibly a control, not a decoration) while still being far
       narrower than the "Auto: daily 1pm ET" string it replaces. Widening it
       would give back the column width this change exists to reclaim; making it
       smaller would make it fiddly to hit. */
    var AUTO_SELECT_H_PX = 20;
    var AUTO_SELECT_MIN_W_PX = 56;
    /* Horizontal padding inside the select. 4px, not the previous 3px, because
       an icon needs a little breathing room from the border to read as a glyph
       rather than as part of the frame. */
    var AUTO_SELECT_PAD_X_PX = 4;
    /* The upgrade affordance is text-with-an-underline, not a padded button:
       on its own line a button's 3px padding and 1px border added 8px of row
       height for no extra affordance, since the whole line is already the
       click target. The underline plus the pointer cursor is what says
       "clickable"; colour alone would not, and would fail in one theme. */
    var UPGRADE_LINK_STYLE = 'cursor:pointer;text-decoration:underline;text-underline-offset:2px;white-space:nowrap';
    /* The manual-path upgrade affordance (auto-upgrade OFF) is a real
       primary-ish button again: real users could not find the upgrade action
       when it was quiet text, so this trades a few px of row height for an
       unmissable target. Background is declared separately so the hover state
       can restore it without re-stating the whole style. */
    var UPGRADE_BTN_BG = 'rgba(63,185,80,0.15)';
    var UPGRADE_BTN_HOVER_BG = 'rgba(63,185,80,0.3)';
    var UPGRADE_BTN_STYLE = 'cursor:pointer;white-space:nowrap;display:inline-block;padding:2px 8px;border-radius:4px;' +
      'background:' + UPGRADE_BTN_BG + ';color:var(--green);border:1px solid rgba(63,185,80,0.4);font-weight:600;font-size:0.7rem';
    /* Passive upgrade-STATE badge: "Queued for auto-upgrade" and the
       non-owner "Update available". Deliberately affordance-free (no cursor,
       no underline, no handler) — the state/action split only works if the
       state never looks clickable. */
    var UPGRADE_STATE_BADGE_STYLE = 'display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.65rem;' +
      'background:var(--surface);color:var(--muted);border:1px solid var(--border);white-space:nowrap';

    /* Visibility toggle switch geometry, used by the Location cell's owner
       branch. A 36x20 track with a 16px knob inset 2px is the standard iOS-style
       switch proportion: the knob travels W - knob - 2*inset = 18px, which is
       why the "on" position is 18px from the left. Keeping these as constants
       means the track, the knob and the travel distance can never drift apart. */
    var VIS_SWITCH_W_PX = 36;
    var VIS_SWITCH_H_PX = 20;
    var VIS_KNOB_PX = 16;
    var VIS_KNOB_INSET_PX = 2;
    var VIS_SWITCH_RADIUS_PX = VIS_SWITCH_H_PX / 2; // fully rounded track
    var VIS_KNOB_ON_LEFT_PX = VIS_SWITCH_W_PX - VIS_KNOB_PX - VIS_KNOB_INSET_PX;

    /* Prefix marking a client-side branch-switch sentinel in _upgradingHives.
       The intended target branch follows the prefix (e.g. "switch:v3") so the
       label can tell a genuine branch switch apart from a same-branch upgrade
       purely from state — a bare sentinel could not. */
    var SWITCH_SENTINEL_PREFIX = 'switch:';
    /* Suffix the hub appends to a branch-switch upgrade target. */
    var BRANCH_TARGET_SUFFIX = '-latest';

    /* Single source of truth for switch-vs-upgrade, consumed by the spinner,
       the label and the title so they can never disagree. Resolves the target
       branch from the server's "<branch>-latest" upgradeTarget or, before the
       server reflects it, from the optimistic client sentinel ("switch:<branch>").
       A genuine branch switch means a target branch that is present AND differs
       from the branch the hive currently reports. A plain-SHA target — an
       auto-upgrade — yields no target branch and is therefore an upgrade, never
       a switch, regardless of any sticky sentinel. */
    function hiveSwitchState(h, branchName) {
      var sentinel = _upgradingHives[h.id];
      var hasSwitchSentinel = typeof sentinel === 'string'
        && sentinel.indexOf(SWITCH_SENTINEL_PREFIX) === 0;
      var upgradeTarget = h.upgradeTarget || '';
      var isBranchTarget = upgradeTarget.length > BRANCH_TARGET_SUFFIX.length
        && upgradeTarget.slice(-BRANCH_TARGET_SUFFIX.length) === BRANCH_TARGET_SUFFIX;
      var targetBranch = '';
      if (isBranchTarget) {
        targetBranch = upgradeTarget.slice(0, -BRANCH_TARGET_SUFFIX.length);
      } else if (upgradeTarget && (_releaseChannels || []).indexOf(upgradeTarget) !== -1) {
        /* A channel switch arms the CHANNEL NAME as the target ("stable"),
           not a "-latest" tag, so the suffix parse above never sees it —
           after a page refresh (client sentinel gone) the "Switching to
           stable" indicator silently vanished while the switch was still
           running server-side. */
        targetBranch = upgradeTarget;
      } else if (hasSwitchSentinel) {
        targetBranch = sentinel.slice(SWITCH_SENTINEL_PREFIX.length);
      }
      /* A same-branch target is not a switch — drop it so nothing downstream
         mistakes an auto-upgrade for one. */
      var isSwitching = !!(targetBranch && targetBranch !== branchName);
      if (!isSwitching) targetBranch = '';
      /* A switch sentinel that no longer resolves to a switch (target became a
         same-branch SHA / auto-upgrade) is stale and must not force upgrading. */
      var switchSentinelStale = hasSwitchSentinel && !isSwitching;
      return { isSwitching: isSwitching, targetBranch: targetBranch, switchSentinelStale: switchSentinelStale };
    }

    async function upgradeHive(id, currentSHA, branch) {
      var fromSHA = currentSHA ? currentSHA.substring(0, 7) : '?';
      var branchLatest = (branch && _latestSHAs[branch]) || _latestSHA;
      var toSHA = branchLatest ? branchLatest.substring(0, 7) : 'latest';
      if (!await hiveConfirm('Upgrade ' + id + '?<br><br><span style="font-family:monospace;font-size:0.85rem;color:var(--muted)">' + fromSHA + '</span> → <span style="font-family:monospace;font-size:0.85rem;color:var(--green)">' + toSHA + '</span>', true)) return;
      var btn = document.getElementById('upgrade-' + id);
      if (btn) { btn.disabled = true; btn.innerHTML = '<span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>Upgrading'; btn.style.opacity = '0.6'; }
      try {
        hiveToast('Upgrading ' + id + '...', 'info');
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/upgrade', {method: 'POST'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Upgrade failed', 'error'); delete _upgradingHives[id]; loadHives(); return; }
        _upgradingHives[id] = currentSHA;
        hiveToast('Upgrade started for ' + id + ' — waiting for rollout', 'success');
        loadHives();
        setTimeout(loadHives, 10000);
        setTimeout(loadHives, 30000);
        setTimeout(loadHives, 60000);
        setTimeout(loadHives, 90000);
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); delete _upgradingHives[id]; loadHives(); }
    }

    function toggleBranchMenu(hiveId) {
      var menu = document.getElementById('branch-menu-' + hiveId);
      if (!menu) return;
      var isOpen = menu.style.display !== 'none';
      document.querySelectorAll('[id^="branch-menu-"]').forEach(function(m) { m.style.display = 'none'; });
      if (!isOpen) {
        menu.style.display = 'block';
        var closeHandler = function(e) {
          if (!e.target.closest('#branch-pill-' + hiveId)) {
            menu.style.display = 'none';
            document.removeEventListener('click', closeHandler);
            /* Catch up on the renders the open menu deferred (see
               renderHives guard) so the rows reflect current data
               immediately instead of waiting for the next poll. */
            renderHives(sortedDashHives(), true);
          }
        };
        setTimeout(function() { document.addEventListener('click', closeHandler); }, 0);
      }
    }

    var _switchTimers = {};
    /* Reset App: clears the spoke's installation_id so the owner is prompted
       to install the GitHub App again. Confirmed first because it costs the
       owner a re-install, and the reset is delivered on the spoke's next
       heartbeat rather than immediately. */
    async function resetHiveApp(hiveId, hiveName) {
      var ok = await hiveConfirm('Reset the Forge App for "' + hiveName + '"?\n\nThe spoke clears its installation ID on the next heartbeat and the owner is prompted to install the App again. The App ID, slug and key are left alone.');
      if (!ok) return;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/reset-app', {method: 'POST'});
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) { await hiveNotify('Reset failed', String(data.error || resp.status)); return; }
        await hiveNotify('Forge App reset armed',
          'Hive: ' + hiveName + '\n' +
          'The spoke clears its installation on the next heartbeat (about 30 seconds), then shows the install prompt.');
        if (typeof loadHives === 'function') loadHives();
      } catch (e) {
        await hiveNotify('Reset failed', String(e));
      }
    }

    /* Reset assignment: returns an assigned-but-unclaimed placeholder to the
       available pool so it can be re-armed. Shown only on a wedged row
       (assigned && !claim_delivered), matching the endpoint's guard — a
       delivered claim is a live hive and the server refuses to reset it. */
    async function resetAssignment(hiveId, hiveName) {
      var ok = await hiveConfirm('Reset the assignment for "' + hiveName + '"?\n\n' +
        'This slot was assigned but its spoke never reported the project back, so it is stuck "Assigning" and cannot be re-approved or re-assigned. ' +
        'Resetting returns it to the available pool — its project, owner and org are cleared — so it can be assigned again cleanly. ' +
        'A live, fully-claimed hive is never affected.');
      if (!ok) return;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/reset-assignment', {method: 'POST'});
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) { await hiveNotify('Reset failed', String(data.error || resp.status)); return; }
        var extra = data.reopened_request_for ? '\nThe provision request for ' + data.reopened_request_for + ' was reopened so you can re-approve it.' : '';
        await hiveNotify('Assignment reset',
          'Hive: ' + hiveName + '\n' +
          'The slot is back in the available pool and can be assigned again.' + extra);
        if (typeof loadHives === 'function') loadHives();
      } catch (e) {
        await hiveNotify('Reset failed', String(e));
      }
    }

    /* Restart Spoke: arms a rolling restart delivered to every spoke instance
       reporting as this hive on their next heartbeats (bounded window). Used
       to shed a stale duplicate instance the hub cannot delete directly. */
    async function restartHiveSpoke(hiveId, hiveName) {
      if (!await hiveConfirm('Restart the spoke for "' + hiveName + '"?\n\nEvery instance reporting as this hive rolling-restarts on its next heartbeat (up to 5 minutes). The image and configuration are unchanged.')) return;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/restart-spoke', {method: 'POST'});
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) { await hiveNotify('Restart failed', String(data.error || resp.status)); return; }
        await hiveNotify('Spoke restart armed',
          'Hive: ' + hiveName + '\n' +
          'Instances restart on their next heartbeat within the 5-minute window.');
        if (typeof loadHives === 'function') loadHives();
      } catch (e) {
        await hiveNotify('Restart failed', String(e));
      }
    }

    function switchBranch(hiveId, newBranch, el) {
      if (el) el.closest('[id^="branch-menu-"]').style.display = 'none';
      if (_switchTimers[hiveId]) {
        clearInterval(_switchTimers[hiveId].interval);
        delete _switchTimers[hiveId];
      }
      var SWITCH_DELAY_SEC = 5;
      var remaining = SWITCH_DELAY_SEC;
      var pill = document.getElementById('branch-pill-' + hiveId);
      if (!pill) return;
      var origHTML = pill.innerHTML;
      pill.style.background = 'rgba(234,179,8,0.2)';
      pill.style.borderColor = 'rgba(234,179,8,0.5)';
      pill.style.color = '#eab308';
      pill.onclick = null;
      // Install the cancel handler after the selecting click finishes
      // bubbling — the menu lives inside the pill, so the same click
      // would otherwise cancel the switch it just started.
      setTimeout(function() {
        pill.onclick = function() { cancelBranchSwitch(hiveId, origHTML); };
      }, 0);
      pill.innerHTML = esc(newBranch) + ' in ' + remaining + 's ✕';
      pill.title = 'Click to cancel';
      var interval = setInterval(function() {
        remaining--;
        if (remaining <= 0) {
          clearInterval(interval);
          delete _switchTimers[hiveId];
          pill.innerHTML = '<span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:3px"></span>Switching to ' + esc(newBranch);
          pill.style.cursor = 'default';
          pill.onclick = null;
          // Set the switching sentinel BEFORE the request so any loadHives()
          // re-render during the POST round-trip keeps showing "Switching to
          // <branch>" — previously the sentinel was set only after the fetch
          // resolved, leaving a gap where the pill reverted to plain status.
          _upgradingHives[hiveId] = SWITCH_SENTINEL_PREFIX + newBranch;
          _switchStartedAt[hiveId] = Date.now();
          doSwitchBranch(hiveId, newBranch);
          return;
        }
        pill.innerHTML = esc(newBranch) + ' in ' + remaining + 's ✕';
      }, 1000);
      _switchTimers[hiveId] = { interval: interval, origHTML: origHTML };
    }

    function cancelBranchSwitch(hiveId, origHTML) {
      if (_switchTimers[hiveId]) {
        clearInterval(_switchTimers[hiveId].interval);
        delete _switchTimers[hiveId];
      }
      var pill = document.getElementById('branch-pill-' + hiveId);
      if (!pill) return;
      pill.style.background = 'rgba(59,130,246,0.15)';
      pill.style.borderColor = 'rgba(59,130,246,0.3)';
      pill.style.color = '#60a5fa';
      pill.innerHTML = origHTML;
      pill.onclick = function() { toggleBranchMenu(hiveId); };
      pill.title = 'Click to switch branch';
      hiveToast('Branch switch cancelled', 'info');
    }

    async function doSwitchBranch(hiveId, newBranch) {
      try {
        hiveToast('Switching ' + hiveId + ' to ' + newBranch + '...', 'info');
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/switch-branch', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ branch: newBranch })
        });
        var data = await resp.json();
        if (!resp.ok) {
          delete _upgradingHives[hiveId]; delete _switchStartedAt[hiveId]; // clear switching state on failure
          hiveToast(data.error || 'Branch switch failed', 'error');
          loadHives();
          return;
        }
        // Sentinel already set before the fetch; keep it until the hive
        // reports the new branch (cleared in the render path).
        hiveToast('Switching ' + hiveId + ' to ' + newBranch + ' — applies on next check-in', 'success');
        loadHives();
        setTimeout(loadHives, 10000);
        setTimeout(loadHives, 30000);
        setTimeout(loadHives, 60000);
      } catch(e) {
        delete _upgradingHives[hiveId]; delete _switchStartedAt[hiveId]; // clear switching state on error
        hiveToast('Error: ' + e.message, 'error');
        loadHives();
      }
    }

    function autoRequestAccessFromUrl() {
      var params = new URLSearchParams(window.location.search);
      // Deep link from an access-request notification (access_notify.go):
      // open Manage Access directly so the owner lands on the pending request.
      var manageId = params.get('manage_access');
      if (manageId) {
        window.history.replaceState({}, '', '/dashboard');
        openAccessModal(manageId, '');
        return;
      }
      var hiveId = params.get('request_hive');
      if (!hiveId) return;
      window.history.replaceState({}, '', '/dashboard');
      // A justification note is required, so open the request modal to
      // collect it instead of firing a note-less request.
      dashRequestAccess(hiveId, null);
    }

    function togglePendingRow(hiveId) {
      var row = document.getElementById('pending-row-' + hiveId);
      if (row) row.style.display = row.style.display === 'none' ? '' : 'none';
    }

    async function inlineApproveAccess(hiveId, username, btn) {
      btn.disabled = true;
      btn.textContent = '...';
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/approve-access/' + encodeURIComponent(username), {method: 'PUT'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Approve failed', 'error'); btn.disabled = false; btn.textContent = 'Approve'; return; }
        hiveToast(username + ' approved', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); btn.disabled = false; btn.textContent = 'Approve'; }
    }

    async function inlineDenyAccess(hiveId, username, btn) {
      btn.disabled = true;
      btn.textContent = '...';
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/deny-access/' + encodeURIComponent(username), {method: 'DELETE'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Deny failed', 'error'); btn.disabled = false; btn.textContent = 'Deny'; return; }
        hiveToast(username + ' denied', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); btn.disabled = false; btn.textContent = 'Deny'; }
    }

    function renderPendingBanner(hives) {
      var existing = document.getElementById('pending-banner');
      if (existing) existing.remove();
      var pending = (hives || []).filter(function(h) { return (roleAtLeast(h.role, 'read-write')) && h.pendingRequestCount > 0; });
      if (!pending.length) return;
      var total = pending.reduce(function(sum, h) { return sum + h.pendingRequestCount; }, 0);
      var banner = document.createElement('div');
      banner.id = 'pending-banner';
      banner.style.cssText = 'background:rgba(59,130,246,0.12);border:1px solid rgba(59,130,246,0.3);border-radius:8px;padding:12px 16px;margin-bottom:16px;display:flex;align-items:center;gap:10px';
      banner.innerHTML = '<span style="font-size:1.1rem">📬</span><span style="font-size:0.85rem;color:var(--text)">' + total + ' pending access request' + (total > 1 ? 's' : '') + ' across ' + pending.length + ' hive' + (pending.length > 1 ? 's' : '') + '. Open <strong>Permissions</strong> on each hive to approve or deny.</span>';
      var container = document.getElementById('hives-container');
      container.parentNode.insertBefore(banner, container);
    }

    async function renderUserAccessBanner() {
      var existing = document.getElementById('user-access-banner');
      if (existing) existing.remove();
      try {
        var resp = await fetch('/api/saas/access-status');
        var data = await resp.json();
        var hives = data.hives || {};
        var pendingIds = [];
        var acceptedIds = [];
        for (var hid in hives) {
          var info = hives[hid];
          if (info.status === 'pending') pendingIds.push(hid);
          if (info.status === 'accepted' && info.role !== 'owner') acceptedIds.push(hid);
        }
        if (!pendingIds.length && !acceptedIds.length) return;
        var container = document.getElementById('hives-container');
        var banner = document.createElement('div');
        banner.id = 'user-access-banner';
        banner.style.cssText = 'margin-bottom:16px';
        var html = '';
        var dismissed = JSON.parse(localStorage.getItem('hive-dismissed-banners') || '{}');
        if (pendingIds.length) {
          var pKey = 'pending:' + pendingIds.sort().join(',');
          if (!dismissed[pKey]) {
            html += '<div style="background:rgba(245,158,11,0.12);border:1px solid rgba(245,158,11,0.3);border-radius:8px;padding:12px 16px;margin-bottom:8px;display:flex;align-items:center;gap:10px">' +
              '<span style="font-size:1.1rem">&#x1F514;</span><span style="flex:1;font-size:0.85rem;color:var(--text)">Access pending: <strong>' + pendingIds.map(esc).join(', ') + '</strong></span>' +
              '<button onclick="dismissBanner(\'' + pKey.replace(/'/g,'') + '\',this)" style="margin-left:auto;background:none;border:none;color:var(--muted);cursor:pointer;font-size:1rem;padding:0 4px" title="Dismiss">&times;</button></div>';
          }
        }
        if (acceptedIds.length) {
          var aKey = 'accepted:' + acceptedIds.sort().join(',');
          if (!dismissed[aKey]) {
            html += '<div style="background:rgba(34,197,94,0.12);border:1px solid rgba(34,197,94,0.3);border-radius:8px;padding:12px 16px;margin-bottom:8px;display:flex;align-items:center;gap:10px">' +
              '<span style="font-size:1.1rem">&#x2705;</span><span style="flex:1;font-size:0.85rem;color:var(--text)">Access granted: <strong>' + acceptedIds.map(esc).join(', ') + '</strong> — Start contributing!</span>' +
              '<button onclick="dismissBanner(\'' + aKey.replace(/'/g,'') + '\',this)" style="margin-left:auto;background:none;border:none;color:var(--muted);cursor:pointer;font-size:1rem;padding:0 4px" title="Dismiss">&times;</button></div>';
          }
        }
        banner.innerHTML = html;
        container.parentNode.insertBefore(banner, container);
      } catch(e) {}
    }

    function renderProvisionRequestBanner(req) {
      var el = document.getElementById('provision-request-banner');
      if (!el) return;
      if (!req) { el.style.display = 'none'; return; }
      el.style.display = '';
      var project = esc(req.org) + '/' + esc(req.primary_repo || req.repos);
      var status = req.status || 'pending';
      var icon, bg, border, msg;
      if (status === 'approved') {
        icon = '&#x2705;';
        bg = 'rgba(34,197,94,0.12)'; border = 'rgba(34,197,94,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> has been approved! An admin will assign your hive shortly.';
      } else if (status === 'denied') {
        icon = '&#x274C;';
        bg = 'rgba(239,68,68,0.12)'; border = 'rgba(239,68,68,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> was denied by an admin.';
      } else {
        icon = '&#x1F3D7;&#xFE0F;';
        bg = 'rgba(245,158,11,0.12)'; border = 'rgba(245,158,11,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> is pending admin approval.';
      }
      /* The banner is purely informational in EVERY state now that the
         Provision button is gone (hives are assigned by an admin), so all
         three states are dismissable. The earlier carve-out that kept
         'approved' non-dismissable existed only because that state carried the
         Provision button — the sole place the action was offered — and
         dismissals never expire (see dismissBanner: the stored timestamp is
         written but no reader ever compares it), so a stray click would have
         stranded the user. With no unique action left in any state, there is
         nothing to strand and the carve-out no longer applies.

         The key embeds BOTH the request identity and its status, so a
         dismissed 'pending' banner re-raises on its own once the request flips
         to approved or denied — a stray click can never permanently hide a
         status change. ProvisionRequest carries no id, so identity is the
         org/primary-repo pair it targets: the same thing the banner text
         names. */
      var dismissKey = ('provision:' + (req.org || '') + '/' +
        (req.primary_repo || req.repos || '') + ':' + status).replace(/'/g, '');
      var dismissedReqs = {};
      try {
        dismissedReqs = JSON.parse(localStorage.getItem('hive-dismissed-banners') || '{}') || {};
      } catch (e) { dismissedReqs = {}; } /* corrupted value — show the banner */
      if (dismissedReqs[dismissKey]) { el.style.display = 'none'; return; }
      el.innerHTML = '<div style="background:' + bg + ';border:1px solid ' + border + ';border-radius:8px;padding:12px 16px;margin-bottom:16px;display:flex;align-items:center;gap:10px">' +
        '<span style="font-size:1.1rem">' + icon + '</span>' +
        '<span style="flex:1;font-size:0.85rem;color:var(--text)">' + msg + '</span>' +
        '<button onclick="dismissBanner(\'' + esc(dismissKey) + '\',this)" style="margin-left:auto;background:none;border:none;color:var(--muted);cursor:pointer;font-size:1rem;padding:0 4px" title="Dismiss">&times;</button>' +
        '</div>';
    }

    // normalizeProvisionStatus trims and lowercases a request status so the
    // pending/decided split cannot be defeated by casing or stray whitespace in
    // a stored record. Returns '' for a missing status, which legacy records
    // written before the field existed have — those are treated as pending.
    function normalizeProvisionStatus(status) {
      return String(status == null ? '' : status).trim().toLowerCase();
    }

    // --- Past Requests (decided provision requests) ---
    // Collapsed by default: this is an audit trail, not a work queue, so it
    // should never push the pending action cards off the fold. The choice is
    // remembered per browser under the same 'hive-*-collapsed' key convention
    // used by the cluster health panel.
    var PAST_REQUESTS_COLLAPSED_KEY = 'hive-past-requests-collapsed';
    // A blank github_host on a request means the public instance; a GitHub
    // Enterprise request carries its host (github.ibm.com, …). Used both as the
    // pill label and as the origin for repo links, so Enterprise requesters get
    // a link that actually resolves instead of a dead github.com URL.
    var PAST_REQUESTS_DEFAULT_GITHUB_HOST = 'github.com';
    // Above this many other hives the cell shows a count instead of the full
    // list, with every hive still readable in the title tooltip. Four keeps a
    // typical footprint inline while stopping a heavy user from turning one
    // table cell into a wall of hive IDs.
    var PAST_REQUESTS_MAX_INLINE_OTHER_HIVES = 4;
    // Absent key means collapsed; only an explicit 'false' expands the section.
    var _pastRequestsCollapsed = localStorage.getItem(PAST_REQUESTS_COLLAPSED_KEY) !== 'false';

    // applyPastRequestsCollapsed pushes _pastRequestsCollapsed onto the DOM.
    // Split out from the toggle so the initial render can restore the persisted
    // state without duplicating the show/hide and aria bookkeeping.
    function applyPastRequestsCollapsed() {
      var body = document.getElementById('past-requests-body');
      var toggle = document.getElementById('past-requests-toggle');
      var header = document.getElementById('past-requests-header');
      if (body) body.style.display = _pastRequestsCollapsed ? 'none' : '';
      // ▸ collapsed / ▾ expanded
      if (toggle) toggle.innerHTML = _pastRequestsCollapsed ? '&#9656;' : '&#9662;';
      if (header) header.setAttribute('aria-expanded', _pastRequestsCollapsed ? 'false' : 'true');
    }

    function togglePastRequests() {
      _pastRequestsCollapsed = !_pastRequestsCollapsed;
      localStorage.setItem(PAST_REQUESTS_COLLAPSED_KEY, _pastRequestsCollapsed ? 'true' : 'false');
      applyPastRequestsCollapsed();
    }

    // githubHostPill renders the GitHub instance a request targets. Blank
    // github_host means public github.com; a GHE host is called out with the
    // accent treatment so the admin can place the hive on a cluster that can
    // reach it. Shared by the pending action cards and the Past Requests table
    // so the same concept does not drift into two different styles.
    function githubHostPill(githubHost) {
      /* Public github.com reads green, GitHub Enterprise reads blue, so the
         two instances are distinguishable at a glance without reading the
         text. An absent host is treated as public, matching the label below. */
      var isGHE = !!githubHost && githubHost !== PAST_REQUESTS_DEFAULT_GITHUB_HOST;
      var border = isGHE ? 'var(--blue);color:var(--blue)' : 'var(--green);color:var(--green)';
      return '<span style="font-size:0.68rem;padding:1px 7px;border-radius:999px;border:1px solid ' + border + '">' +
        esc(githubHost || PAST_REQUESTS_DEFAULT_GITHUB_HOST) + '</span>';
    }

    // otherHivesCell summarises the rest of a requester's fleet: which other
    // hives they can sign in to and their role on each, so an admin reading one
    // row sees the person's whole footprint. The list arrives on the payload
    // (admin-only) so this never costs an API call per row.
    function otherHivesCell(otherHives) {
      var list = (otherHives || []).filter(function(e) { return e && e.hive_id; });
      if (!list.length) return '<span style="color:var(--muted);font-size:0.7rem">—</span>';
      // Plain text for the tooltip — a title attribute is not HTML, and esc()
      // does not escape quotes, so nothing user-controlled may be interpolated
      // into the attribute string. Set it via the DOM instead.
      var full = list.map(function(e) { return e.hive_id + ' · ' + (e.role || '?'); }).join('\n');
      var span = document.createElement('span');
      span.style.fontSize = '0.7rem';
      if (list.length > PAST_REQUESTS_MAX_INLINE_OTHER_HIVES) {
        span.textContent = list.length + ' hives';
        span.style.color = 'var(--muted)';
        span.style.borderBottom = '1px dotted var(--border)';
        span.style.cursor = 'help';
      } else {
        span.textContent = list.map(function(e) {
          return e.hive_id + ' · ' + (e.role || '?');
        }).join(', ');
        span.style.color = 'var(--muted)';
        span.style.fontFamily = 'ui-monospace,monospace';
      }
      span.title = full;
      // outerHTML of a textContent-populated node is escaped by the DOM, which
      // covers quotes and apostrophes that esc() would leave through.
      return span.outerHTML;
    }

    // renderRequestHistory renders every already-decided request as a table:
    // who asked, for what, what was decided, by whom, and — for an approval —
    // which hive they actually got. Pending requests stay as action cards above;
    // this is the audit trail, not a work queue, so it is deliberately a dense
    // table rather than more cards.
    // provisionRepoLabel renders a provision request's org/repo as a link to
    // the repository on the GitHub instance the request actually targets.
    // github_host is authoritative: '' means public github.com, a value like
    // github.ibm.com means GitHub Enterprise — hardcoding github.com would send
    // an admin to a 404 (or worse, an unrelated public repo of the same name).
    // Requests with no repo recorded fall back to plain escaped text.
    /* provisionRequesterPrimary returns the identifier admins should scan first
       on provision-request rows. The durable request key (username) may be an
       opaque native provider subject such as "ibmid:695000VVZ9"; user_id is the
       GitHub/GHE login or best human-readable identity captured/enriched by the
       server. Older records without user_id fall back to username. */
    function provisionRequesterPrimary(pr) {
      if (!pr) return '';
      var id = String(pr.user_id || '').trim();
      return id || String(pr.username || '').trim();
    }

    function provisionRequesterNativeSubject(pr) {
      if (!pr) return '';
      var native = String(pr.username || '').trim();
      var primary = provisionRequesterPrimary(pr);
      return native && primary && native !== primary ? native : '';
    }

    function provisionNativeSubjectChip(pr) {
      var native = provisionRequesterNativeSubject(pr);
      if (!native) return '';
      return '<span title="Native provider subject" style="font-size:0.68rem;padding:1px 6px;border-radius:999px;border:1px solid var(--border);color:var(--muted);font-family:ui-monospace,monospace">' +
        esc(native) + '</span>';
    }

    function provisionRequesterAvatar(pr, px, extraStyle) {
      var primary = provisionRequesterPrimary(pr);
      if (pr && pr.user_id_source === 'github') {
        return linkedAvatar(primary, px, primary, extraStyle);
      }
      return userAvatar({display_name: primary, github_username: String((pr && pr.username) || '')}, px, extraStyle);
    }

    /* provisionRequesterLabel renders the requester's real name, Slack ID, and
       native provider subject beside the primary user ID on the review card.
       Mapping a login to a person is the reason the wizard asks for a name at
       all, and this is where the operator deciding the request needs it.

       Both are free text a user typed. esc() everywhere, and the whole label is
       built as a text node's escaped HTML rather than interpolated into an
       attribute — nothing here is ever placed inside an inline handler, where
       esc() alone would leave an apostrophe live. Older requests predate these
       fields and simply render nothing. */
    function provisionRequesterLabel(pr) {
      if (!pr) return '';
      var name = (pr.full_name || '').trim();
      var slack = (pr.slack_id || '').trim();
      var native = provisionRequesterNativeSubject(pr);
      if (!name && !slack && !native) return '';
      var primary = provisionRequesterPrimary(pr);
      var bits = [];
      if (name && name !== primary) bits.push(esc(name));
      if (slack) bits.push('slack: ' + esc(slack));
      if (native) bits.push(provisionNativeSubjectChip(pr));
      if (!bits.length) return '';
      return '<span style="font-size:0.75rem;color:var(--muted);margin-left:8px">' +
        bits.join(' &middot; ') + '</span>';
    }

    function provisionRepoLabel(pr) {
      if (!pr) return '';
      var org = pr.org || '';
      var repo = pr.primary_repo || pr.repos || '';
      var text = esc(org) + '/' + esc(repo);
      // repos may be a comma-separated list; only a single concrete repo can be
      // turned into a URL, so leave a multi-repo value as plain text.
      var url = (repo.indexOf(',') === -1) ? ghRepoURL(pr.github_host, org, repo) : '';
      if (!url) return text;
      return '<a href="' + escAttr(url) + '" target="_blank" rel="noopener noreferrer"' +
        ' style="color:inherit;text-decoration:underline;text-decoration-style:dotted">' + text + '</a>';
    }

    function renderRequestHistory(requests) {
      var host = document.getElementById('admin-request-history');
      if (!host) return;
      var decided = (requests || []).filter(function(pr) {
        return pr && normalizeProvisionStatus(pr.status) !== '' && normalizeProvisionStatus(pr.status) !== 'pending';
      });
      // Restore the persisted collapse state on every render — the section is
      // rebuilt on each dashboard poll, so applying it once at load is not enough.
      applyPastRequestsCollapsed();
      var countEl = document.getElementById('past-requests-count');
      if (countEl) countEl.textContent = decided.length ? '(' + decided.length + ')' : '';
      if (!decided.length) {
        host.innerHTML = '<div style="color:var(--muted);font-size:0.75rem;padding:6px 0">No past requests yet.</div>';
        return;
      }
      // Most recently decided first; fall back to requested_at on legacy records
      // that predate decided_at so they still sort somewhere sensible.
      decided.sort(function(a, b) {
        var av = a.decided_at || a.requested_at || '';
        var bv = b.decided_at || b.requested_at || '';
        return bv.localeCompare(av);
      });
      var rows = decided.map(function(pr) {
        var approved = pr.status === 'approved';
        var color = approved ? 'var(--green)' : 'var(--red)';
        var primary = provisionRequesterPrimary(pr);
        // Link the requester avatar only when the server says the primary user
        // ID is a GitHub login. Email/name fallbacks get an initials avatar
        // instead, avoiding bogus github.com links for OIDC subjects.
        //
        // The AVATAR carries that link now. The username used to be a second
        // anchor to the same profile; two controls for one destination is noise,
        // so the name is plain text and the face is the affordance.
        var userCell =
          provisionRequesterAvatar(pr, PANEL_ACCESS_AVATAR_PX, 'margin-right:6px') +
          (primary
            ? '<span>' + esc(primary) + '</span>' + provisionRequesterLabel(pr)
            : '<span style="color:var(--muted)">—</span>');
        // Repo link, built by the shared provisionRepoLabel(): github_host is
        // empty for public github.com and otherwise a GitHub Enterprise host
        // (github.ibm.com, …), so the origin comes from the record rather than
        // being hardcoded — hardcoding github.com would produce dead links for
        // every Enterprise requester. The helper also declines to link a
        // comma-separated multi-repo value, which has no single URL, and falls
        // back to plain escaped text when there is nothing to link.
        var org = pr.org || '';
        var repo = pr.primary_repo || pr.repos || '';
        var repoCell = (org || repo)
          ? provisionRepoLabel(pr)
          : '<span style="color:var(--muted)">—</span>';
        // For an approval the outcome is the hive they received — linked to the
        // existing SSO handoff endpoint, the same affordance My Hives uses —
        // plus their role on it. For a denial it is the reason, if one was
        // given. Legacy records have neither.
        var outcome;
        if (approved && pr.assigned_hive) {
          var roleSuffix = pr.assigned_role
            ? ' <span style="color:var(--muted);font-size:0.68rem">&middot; ' + esc(pr.assigned_role) + '</span>'
            : '';
          outcome =
            '<a href="/api/saas/hives/' + encodeURIComponent(pr.assigned_hive) + '/open" target="_blank" rel="noopener noreferrer" style="font-family:ui-monospace,monospace;font-size:0.7rem;color:inherit" title="Open dashboard">' +
            esc(pr.assigned_hive) + '</a>' + roleSuffix;
        } else if (!approved && pr.deny_reason) {
          outcome = '<span style="color:var(--muted)">' + esc(pr.deny_reason) + '</span>';
        } else {
          outcome = '<span style="color:var(--muted)">—</span>';
        }
        return '<tr>' +
          '<td style="white-space:nowrap">' + userCell + '</td>' +
          '<td>' + repoCell + '</td>' +
          '<td style="white-space:nowrap">' + githubHostPill(pr.github_host) + '</td>' +
          '<td style="white-space:nowrap">' + acmmBadge(pr.acmm_level) + '</td>' +
          '<td style="white-space:nowrap;color:var(--muted);font-size:0.7rem">' + esc((pr.requested_at || '').substring(0, 10)) + '</td>' +
          '<td style="white-space:nowrap"><span style="color:' + color + ';font-weight:600;font-size:0.72rem">' + esc(pr.status) + '</span></td>' +
          '<td style="white-space:nowrap">' + esc(pr.decided_by || '—') + '</td>' +
          '<td style="white-space:nowrap;color:var(--muted);font-size:0.7rem">' + esc((pr.decided_at || '').substring(0, 10) || '—') + '</td>' +
          '<td>' + outcome + '</td>' +
          '<td>' + otherHivesCell(pr.other_hives) + '</td>' +
          '</tr>';
      }).join('');
      // Header cells here MUST stay in lockstep with the <td> cells emitted
      // above — 10 columns: User, Requested, Host, ACMM, On, Decision, By,
      // When, Assigned / reason, Other hives.
      host.innerHTML =
        '<table class="hive-table" style="width:100%;font-size:0.78rem">' +
        '<thead><tr><th>User</th><th>Requested</th><th>Host</th><th>ACMM</th><th>On</th>' +
        '<th>Decision</th><th>By</th><th>When</th><th>Assigned / reason</th><th>Other hives</th></tr></thead>' +
        '<tbody>' + rows + '</tbody></table>';
    }

    function renderAdminProvisionRequests(requests) {
      var section = document.getElementById('admin-provision-requests');
      var list = document.getElementById('admin-provision-list');
      if (!section || !list) return;
      // History covers decided requests; the cards below cover pending ones. Do
      // this before the pending early-return, or the history disappears the
      // moment the queue empties — which is exactly when it matters most.
      renderRequestHistory(requests);
      // Only genuinely pending requests get Approve/Deny cards. Anything already
      // approved or denied belongs in Past Requests, never in the action queue.
      var pending = (requests || []).filter(function(pr) {
        if (!pr) return false;
        var st = normalizeProvisionStatus(pr.status);
        return st === '' || st === 'pending';
      });
      if (!pending.length) {
        // Keep the section visible when there is history to show, so the table
        // does not vanish along with the empty queue.
        var anyDecided = (requests || []).some(function(pr) {
          if (!pr) return false;
          var st = normalizeProvisionStatus(pr.status);
          return st !== '' && st !== 'pending';
        });
        list.innerHTML = '<div style="color:var(--muted);font-size:0.75rem;padding:6px 0">No pending requests.</div>';
        section.style.display = anyDecided ? '' : 'none';
        return;
      }
      section.style.display = '';
      // Stash by username so the approve-picker modal can read the full request
      // (org/repos/acmm) without threading every field through an onclick string.
      _provisionRequestsByUser = {};
      pending.forEach(function(pr) { _provisionRequestsByUser[pr.username] = pr; });
      var rows = pending.map(function(pr) {
        var primary = provisionRequesterPrimary(pr);
        var avatar = provisionRequesterAvatar(pr, TABLE_AVATAR_PX, 'margin-right:8px');
        return '<div style="display:flex;align-items:center;justify-content:space-between;padding:10px 14px;background:var(--surface);border:1px solid var(--border);border-radius:8px;margin-bottom:8px">' +
          '<div style="display:flex;align-items:center;gap:8px">' +
          avatar +
          '<div>' +
          '<span style="font-size:0.85rem;font-weight:600">' + esc(primary || pr.username) + '</span>' +
          // Who the requester actually IS. Mapping a login to a person is the
          // reason the wizard asks for a name, and this review card is where an
          // operator needs it. esc() only — free text, never markup.
          provisionRequesterLabel(pr) +
          // org/repo links to the repo on the instance the request targets.
          '<span style="font-size:0.75rem;color:var(--muted);margin-left:8px">' + provisionRepoLabel(pr) + '</span>' +
          // Show which GitHub instance the request targets — same pill the Past
          // Requests table uses, so pending and decided rows read alike.
          ' ' + githubHostPill(pr.github_host) +
          ' ' + acmmBadge(pr.acmm_level) +
          '<div style="font-size:0.7rem;color:var(--muted)">' + esc((pr.requested_at || '').substring(0, 10)) + '</div>' +
          '</div></div>' +
          '<div style="display:flex;gap:6px">' +
          '<button onclick="openApproveModal(\'' + esc(pr.username) + '\')" style="padding:5px 14px;background:var(--green);color:#fff;border:none;border-radius:6px;cursor:pointer;font-size:0.78rem;font-weight:600">Approve</button>' +
          '<button onclick="denyProvision(\'' + esc(pr.username) + '\',this)" style="padding:5px 14px;background:var(--red);color:#fff;border:none;border-radius:6px;cursor:pointer;font-size:0.78rem;font-weight:600">Deny</button>' +
          '</div></div>';
      }).join('');
      list.innerHTML = rows;
    }

    var _provisionRequestsByUser = {};

    /* openAssignForUser is the entry point from a click on a user in the admin
       users table. It routes to whichever existing flow fits, rather than adding
       a third assign path:
         - the user has a PENDING provision request  → openApproveModal, which
           approves the request AND assigns a placeholder in one step;
         - otherwise → openAssignModal on an available placeholder, pre-filled
           with the user as owner. openAssignModal needs a concrete placeholder
           id, so we ask the same endpoint the approve picker uses.
       Authorization is unchanged: both modals post to admin-only endpoints
       (/api/saas/approve-provision, /api/saas/hives/{id}/assign), which
       re-check the caller server-side, so this is purely a UI shortcut. */
    async function openAssignForUser(username) {
      if (!_isAdmin) return;
      if (_provisionRequestsByUser[username]) { openApproveModal(username); return; }
      try {
        var resp = await fetch('/api/saas/admin/available-placeholders');
        if (!resp.ok) { hiveToast('Could not load available placeholders', 'error'); return; }
        var data = await resp.json();
        var placeholders = data.placeholders || [];
        if (!placeholders.length) {
          hiveToast('No available placeholder hives to assign — provision one first.', 'error');
          return;
        }
        openAssignModal(placeholders[0].id, username, placeholders);
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function openApproveModal(username) {
      var pr = _provisionRequestsByUser[username] || {username: username};
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var lbl = 'display:block;font-size:0.75rem;color:var(--muted);margin:10px 0 4px';
      var reposText = pr.primary_repo || pr.repos || '';
      var summary =
        '<div style="padding:10px 12px;background:var(--surface);border:1px solid var(--border);border-radius:6px;font-size:0.8rem;margin-bottom:4px">' +
        '<div><span style="color:var(--muted)">User:</span> <strong>' + esc(username) + '</strong></div>' +
        '<div><span style="color:var(--muted)">Org:</span> ' + esc(pr.org || '') + '</div>' +
        '<div><span style="color:var(--muted)">Repos:</span> ' + esc(pr.repos || '') + '</div>' +
        '<div><span style="color:var(--muted)">Primary:</span> ' + esc(pr.primary_repo || '') + '</div>' +
        '<div><span style="color:var(--muted)">ACMM:</span> ' + (pr.acmm_level != null ? pr.acmm_level : '—') + '</div>' +
        '<div><span style="color:var(--muted)">GitHub:</span> ' + esc(pr.github_host || PUBLIC_GITHUB_HOST) + '</div>' +
        '</div>';
      /* The requester's own github_host pre-fills the picker — they already
         told us which GitHub their org lives on. It is only the cluster
         default when the request carries none. The cluster is unknown until a
         placeholder is picked (auto-pick resolves it server-side), so the
         picker opens on "Cluster default" and the note fills in on selection. */
      var approveRequestHost = pr.github_host || '';
      var content =
        summary +
        '<label style="' + lbl + '">Placeholder to assign</label>' +
        '<select id="approve-hive" style="' + fld + '"><option value="">Loading placeholders…</option></select>' +
        '<div style="font-size:0.72rem;color:var(--muted);margin-top:6px">Auto-pick chooses an available placeholder from the request&#39;s pool.</div>' +
        githubHostChoiceMarkup('approve', '', approveRequestHost, fld, lbl) +
        '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:16px">' +
        '<button onclick="closeApproveModal()" style="padding:6px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Cancel</button>' +
        '<button id="approve-submit" onclick="confirmApprove(\'' + esc(username) + '\')" style="padding:6px 16px;background:#3fb950;color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Approve</button>' +
        '</div>';
      var overlay = document.createElement('div');
      overlay.id = 'approve-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:440px;width:90%;max-height:88vh;overflow-y:auto">' +
        '<h3 style="margin:0 0 12px 0;font-size:1rem">Approve &amp; assign placeholder</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeApproveModal(); });
      document.body.appendChild(overlay);
      /* No cluster is known yet (auto-pick is the default selection), so the
         "Cluster default" note stays generic until a placeholder is chosen. */
      wireGitHubHostChoice('approve', '');
      // Populate the dropdown with available placeholders (auto-pick default).
      var _approvePlaceholders = [];
      try {
        var resp = await fetch('/api/saas/admin/available-placeholders');
        var data = await resp.json();
        var sel = document.getElementById('approve-hive');
        if (!sel) return;
        _approvePlaceholders = data.placeholders || [];
        var opts = '<option value="">Auto-pick</option>';
        _approvePlaceholders.forEach(function(p) {
          opts += '<option value="' + esc(p.id) + '">' + esc(p.id) + '  (' + esc(p.cluster_id || 'default') + ')</option>';
        });
        sel.innerHTML = opts;
        /* Picking a concrete placeholder reveals its cluster, so the "Cluster
           default" option can finally name the GitHub instance the hive will
           land on. Auto-pick ('') leaves it unnamed — the server resolves the
           cluster and backfills the host itself. */
        sel.addEventListener('change', function() {
          var ph = (_approvePlaceholders || []).reduce(function(m, p) { return (p && p.id === sel.value) ? p : m; }, null);
          var host = ph ? clusterGitHubHost(ph.cluster_id || '') : '';
          var hostSel = document.getElementById('approve-ghhost');
          if (hostSel && hostSel.options && hostSel.options.length) {
            hostSel.options[0].textContent = host ? 'Cluster default — ' + host : 'Cluster default';
          }
          wireGitHubHostChoice('approve', host);
        });
      } catch(e) {
        var sel2 = document.getElementById('approve-hive');
        if (sel2) sel2.innerHTML = '<option value="">Auto-pick</option>';
      }
    }
    function closeApproveModal() {
      var ov = document.getElementById('approve-overlay');
      if (ov) ov.remove();
    }
    async function confirmApprove(username) {
      var sel = document.getElementById('approve-hive');
      var hiveId = sel ? sel.value : '';
      var submit = document.getElementById('approve-submit');
      if (submit) { submit.disabled = true; submit.textContent = 'Assigning...'; }
      try {
        // Approving assigns a placeholder (the chosen one, or auto-pick from the
        // correct pool when hive_id is empty) and marks the request fulfilled.
        var resp = await fetch('/api/saas/approve-provision/' + encodeURIComponent(username), {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          /* github_host: '' = use the request's host, else the cluster
             default (server-side); 'public' = force public github.com. */
          body: JSON.stringify({hive_id: hiveId, github_host: githubHostChoiceValue('approve')})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Assign failed', 'error'); if (submit) { submit.disabled = false; submit.textContent = 'Approve'; } return; }
        closeApproveModal();
        hiveToast('Approved ' + username + ' → ' + (hiveId || 'auto') + ' (' + (data.hive_id || 'a hive') + ')', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); if (submit) { submit.disabled = false; submit.textContent = 'Approve'; } }
    }

    async function denyProvision(username, btn) {
      if (!await hiveConfirm('Deny provision request from ' + username + '?')) return;
      btn.disabled = true;
      btn.textContent = 'Denying...';
      try {
        var resp = await fetch('/api/saas/deny-provision/' + encodeURIComponent(username), {method: 'DELETE'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Deny failed', 'error'); btn.disabled = false; btn.textContent = 'Deny'; return; }
        hiveToast('Provision request denied for ' + username, 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); btn.disabled = false; btn.textContent = 'Deny'; }
    }


    // --- Cluster Health Panel ---
    var CLUSTER_HEALTH_POLL_MS = 30000;
    var CLUSTER_CPU_WARN_PCT = 60;
    var CLUSTER_CPU_DANGER_PCT = 80;
    var CLUSTER_MEM_WARN_PCT = 60;
    var CLUSTER_MEM_DANGER_PCT = 80;
    // Disk thresholds mirror kubelet's own behaviour on these nodes rather
    // than round numbers: image garbage collection starts at
    // imageGCHighThresholdPercent (85% used) and hard eviction fires at
    // evictionHard nodefs.available<10% (90% used). Amber therefore means
    // "kubelet is already reclaiming disk", red means "pods are being evicted".
    var CLUSTER_DISK_WARN_PCT = 85;
    var CLUSTER_DISK_DANGER_PCT = 90;
    // MiB per GiB, for rendering disk_*_mb values as GiB.
    var MB_PER_GB = 1024;
    var _clusterHealthCollapsed = localStorage.getItem('hive-cluster-health-collapsed') !== 'false';

    function toggleClusterHealth() {
      _clusterHealthCollapsed = !_clusterHealthCollapsed;
      localStorage.setItem('hive-cluster-health-collapsed', _clusterHealthCollapsed ? 'true' : 'false');
      var body = document.getElementById('cluster-health-body');
      var toggle = document.getElementById('cluster-health-toggle');
      if (body) body.style.display = _clusterHealthCollapsed ? 'none' : '';
      if (toggle) toggle.style.transform = _clusterHealthCollapsed ? '' : 'rotate(90deg)';
    }

    /* ---- Scale Controls (admin) ----
       Renders inputs from GET /api/saas/admin/scale-settings and saves the
       whole document via POST. The GET payload carries saved values,
       EFFECTIVE values (after the saved > env > default chain) and the
       built-in defaults, so each input can show what is actually in force. */
    var _scaleControlsCollapsed = localStorage.getItem('hive-scale-controls-collapsed') !== 'false';
    var _scaleData = null;
    function toggleScaleControls() {
      _scaleControlsCollapsed = !_scaleControlsCollapsed;
      localStorage.setItem('hive-scale-controls-collapsed', _scaleControlsCollapsed ? 'true' : 'false');
      var body = document.getElementById('scale-controls-body');
      var toggle = document.getElementById('scale-controls-toggle');
      if (body) body.style.display = _scaleControlsCollapsed ? 'none' : '';
      if (toggle) toggle.style.transform = _scaleControlsCollapsed ? '' : 'rotate(90deg)';
    }
    var SCALE_GLOBAL_KNOBS = [
      { key: 'upgrade_wave_size', label: 'Upgrade wave size', hint: 'auto-upgrades per cluster per tick' },
      { key: 'provision_workers', label: 'Provision workers', hint: 'total concurrent provisions (grows live; shrink needs restart)' },
      { key: 'provision_per_cluster', label: 'Provisions per cluster', hint: 'concurrent provisions per target cluster' },
      { key: 'kubectl_per_cluster', label: 'kubectl per cluster', hint: 'concurrent kubectl processes per cluster' }
    ];
    function scaleNumInput(id, value, placeholder) {
      return '<input type="number" min="0" max="10000" id="' + id + '" value="' + (value || '') + '"' +
        ' placeholder="' + placeholder + '"' +
        ' style="width:90px;padding:6px 8px;background:var(--surface);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;text-align:right">';
    }
    async function loadScaleSettings() {
      if (!_isAdmin) return;
      try {
        var resp = await fetch('/api/saas/admin/scale-settings');
        if (!resp.ok) { document.getElementById('scale-controls-section').style.display = 'none'; return; }
        var data = await resp.json();
        _scaleData = data;
        document.getElementById('scale-controls-section').style.display = '';
        var body = document.getElementById('scale-controls-body');
        var toggle = document.getElementById('scale-controls-toggle');
        if (body) body.style.display = _scaleControlsCollapsed ? 'none' : '';
        if (toggle) toggle.style.transform = _scaleControlsCollapsed ? '' : 'rotate(90deg)';
        var eff = data.effective || {};
        document.getElementById('scale-controls-summary').textContent =
          'wave ' + eff.upgrade_wave_size + ' · workers ' + eff.provision_workers +
          ' · per-cluster ' + eff.provision_per_cluster + ' · kubectl ' + eff.kubectl_per_cluster;
        /* Don't clobber in-progress edits: only re-render when collapsed or
           on first load. */
        if (document.activeElement && document.activeElement.id && document.activeElement.id.indexOf('scale-') === 0) return;
        var saved = data.saved || {};
        var defaults = data.defaults || {};
        document.getElementById('scale-globals').innerHTML = SCALE_GLOBAL_KNOBS.map(function(k) {
          return '<div style="padding:12px;border:1px solid var(--border);border-radius:8px;background:var(--surface)">' +
            '<div style="font-size:0.8rem;font-weight:600;margin-bottom:2px">' + k.label + '</div>' +
            '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:8px">' + k.hint + '</div>' +
            '<div style="display:flex;align-items:center;gap:8px">' +
            scaleNumInput('scale-' + k.key, saved[k.key], 'default ' + defaults[k.key]) +
            '<span style="font-size:0.75rem;color:var(--muted)">in force: ' + (eff[k.key] != null ? eff[k.key] : '?') + '</span>' +
            '</div></div>';
        }).join('');
        var overrides = saved.clusters || {};
        document.getElementById('scale-clusters-body').innerHTML = (data.clusters || []).map(function(c) {
          var o = overrides[c.id] || {};
          function cell(field, effective) {
            var savedVal = (o[field] != null) ? o[field] : '';
            return '<td style="text-align:right;padding:6px 4px">' +
              scaleNumInput('scale-c-' + c.id + '-' + field, savedVal, String(effective)) + '</td>';
          }
          return '<tr><td style="padding:6px 4px">' + c.id + '</td>' +
            '<td style="text-align:right;padding:6px 4px">' + c.hives + '</td>' +
            '<td style="text-align:right;padding:6px 4px">' + c.available_placeholders + '</td>' +
            cell('max_hives', c.max_hives) + cell('pool_min', c.pool_min) + cell('pool_target', c.pool_target) + '</tr>';
        }).join('');
      } catch (e) { /* transient — next poll retries */ }
    }
    async function saveScaleSettings() {
      var status = document.getElementById('scale-save-status');
      function intOrZero(id) {
        var el = document.getElementById(id);
        if (!el || el.value === '') return 0;
        var n = parseInt(el.value, 10);
        return isNaN(n) ? 0 : n;
      }
      var body = {
        upgrade_wave_size: intOrZero('scale-upgrade_wave_size'),
        provision_workers: intOrZero('scale-provision_workers'),
        provision_per_cluster: intOrZero('scale-provision_per_cluster'),
        kubectl_per_cluster: intOrZero('scale-kubectl_per_cluster'),
        clusters: {}
      };
      ((_scaleData && _scaleData.clusters) || []).forEach(function(c) {
        var row = {};
        ['max_hives', 'pool_min', 'pool_target'].forEach(function(f) {
          var el = document.getElementById('scale-c-' + c.id + '-' + f);
          /* Blank = no override (fall back to clusters.json). An explicit
             number — including 0 — is an override. */
          if (el && el.value !== '') {
            var n = parseInt(el.value, 10);
            if (!isNaN(n)) row[f] = n;
          }
        });
        if (Object.keys(row).length > 0) body.clusters[c.id] = row;
      });
      try {
        var resp = await fetch('/api/saas/admin/scale-settings', {
          method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
        });
        if (!resp.ok) {
          var err = {};
          try { err = await resp.json(); } catch (_) {}
          status.textContent = 'Save failed: ' + (err.error || resp.status);
          status.style.color = 'var(--red)';
          return;
        }
        status.textContent = 'Saved — in effect now.';
        status.style.color = 'var(--green)';
        setTimeout(function() { status.textContent = ''; }, 5000);
        loadScaleSettings();
      } catch (e) {
        status.textContent = 'Save failed: ' + e;
        status.style.color = 'var(--red)';
      }
    }

    function healthBarColor(pct, warnThreshold, dangerThreshold) {
      if (pct >= dangerThreshold) return 'var(--red)';
      if (pct >= warnThreshold) return 'var(--accent)';
      return 'var(--green)';
    }

    var SPARKLINE_BLOCKS = '▁▂▃▄▅▆▇█';
    var SPARKLINE_HISTORY_LEN = 10;
    var _clusterSparkHistory = {};

    function pushSparkPoint(nodeKey, metric, value) {
      var key = nodeKey + ':' + metric;
      if (!_clusterSparkHistory[key]) _clusterSparkHistory[key] = [];
      _clusterSparkHistory[key].push(value);
      if (_clusterSparkHistory[key].length > SPARKLINE_HISTORY_LEN) _clusterSparkHistory[key].shift();
    }

    function renderUnicodeSparkline(nodeKey, metric, color) {
      var key = nodeKey + ':' + metric;
      var pts = _clusterSparkHistory[key] || [];
      if (pts.length < 2) return '';
      var max = Math.max.apply(null, pts);
      if (max === 0) max = 1;
      var blocks = SPARKLINE_BLOCKS;
      var spark = pts.map(function(v) {
        var idx = Math.round((v / max) * (blocks.length - 1));
        return blocks[Math.min(idx, blocks.length - 1)];
      }).join('');
      return '<span style="font-family:monospace;font-size:0.7rem;color:' + color + ';letter-spacing:-1px">' + spark + '</span>';
    }

    function renderHealthMetric(label, used, total, unit, pct, warnThreshold, dangerThreshold, nodeKey, metric) {
      var color = healthBarColor(pct, warnThreshold, dangerThreshold);
      pushSparkPoint(nodeKey, metric, pct);
      var spark = renderUnicodeSparkline(nodeKey, metric, color);
      return '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:4px">' +
        '<span style="font-size:0.7rem;color:var(--muted)">' + label + '</span>' +
        '<span style="display:flex;align-items:center;gap:6px">' +
        spark +
        '<span style="font-family:monospace;font-size:0.75rem;color:' + color + '">' + used + ' / ' + total + ' ' + unit + '</span>' +
        '<span style="font-size:0.7rem;min-width:28px;text-align:right;color:' + color + '">' + pct + '%</span>' +
        '</span></div>';
    }

    // Renders a metric row whose value could not be collected. Deliberately
    // dashed rather than 0%, so an unreachable node never reads as healthy.
    function renderHealthMetricUnknown(label, reason) {
      return '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:4px" title="' + esc(reason || 'No data') + '">' +
        '<span style="font-size:0.7rem;color:var(--muted)">' + label + '</span>' +
        '<span style="display:flex;align-items:center;gap:6px">' +
        '<span style="font-family:monospace;font-size:0.75rem;color:var(--muted)">— / — GiB</span>' +
        '<span style="font-size:0.7rem;min-width:28px;text-align:right;color:var(--muted)">—</span>' +
        '</span></div>';
    }

    // Disk usage is optional: the collector omits it when the kubelet
    // stats endpoint was unreachable, so absent means unknown, not zero.
    function renderNodeDiskMetric(n, nk) {
      if (n.disk_percent == null || n.disk_total_mb == null || n.disk_used_mb == null) {
        return renderHealthMetricUnknown('DISK', 'Disk usage unavailable for this node');
      }
      var diskUsedGB = (n.disk_used_mb / MB_PER_GB).toFixed(1);
      var diskTotalGB = Math.round(n.disk_total_mb / MB_PER_GB);
      return renderHealthMetric('DISK', diskUsedGB, diskTotalGB, 'GiB', n.disk_percent,
        CLUSTER_DISK_WARN_PCT, CLUSTER_DISK_DANGER_PCT, nk, 'disk');
    }

    function renderNodeCard(n) {
      var nk = n.name;
      var readyBadge = (n.conditions || []).indexOf('Ready') >= 0
        ? '<span style="color:var(--green);font-size:0.7rem;font-weight:600">Ready</span>'
        : '<span style="color:var(--red);font-size:0.7rem;font-weight:600">NotReady</span>';
      var diskWarn = n.disk_pressure
        ? '<div style="margin-top:6px;padding:4px 8px;background:rgba(239,68,68,0.1);border:1px solid rgba(239,68,68,0.3);border-radius:4px;font-size:0.7rem;color:var(--red)">⚠ Disk Pressure</div>'
        : '';
      var cpuUsed = (n.cpu_used_millicores / 1000).toFixed(1);
      var memUsedGB = (n.mem_used_mb / 1024).toFixed(1);
      var memTotalGB = Math.round(n.mem_total_mb / 1024);
      var hiveCount = n.hive_count || 0;
      var hivePill = '<span style="padding:2px 8px;background:var(--bg);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;color:var(--muted)">' + hiveCount + (hiveCount === 1 ? ' hive' : ' hives') + '</span>';
      return '<div style="background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:14px">' +
        '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:8px">' +
        '<span style="font-family:monospace;font-size:0.8rem;color:var(--text)">' + esc(nk) + '</span>' +
        '<span style="display:flex;align-items:center;gap:6px">' + readyBadge +
        hivePill +
        '<span style="padding:2px 8px;background:var(--bg);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;color:var(--muted)">' + (n.pods || 0) + '/' + (n.pod_capacity || 0) + ' pods</span>' +
        '</span></div>' +
        renderHealthMetric('CPU', cpuUsed, n.cpu_cores, 'cores', n.cpu_percent, CLUSTER_CPU_WARN_PCT, CLUSTER_CPU_DANGER_PCT, nk, 'cpu') +
        renderHealthMetric('MEM', memUsedGB, memTotalGB, 'GB', n.mem_percent, CLUSTER_MEM_WARN_PCT, CLUSTER_MEM_DANGER_PCT, nk, 'mem') +
        renderNodeDiskMetric(n, nk) +
        diskWarn +
        '</div>';
    }

    async function loadClusterHealth() {
      if (!_isAdmin) return;
      try {
        var resp = await fetch('/api/saas/cluster-health');
        if (resp.status === 403) {
          document.getElementById('cluster-health-section').style.display = 'none';
          return;
        }
        if (!resp.ok) return;
        var data = await resp.json();
        document.getElementById('cluster-health-section').style.display = '';

        var s = data.summary || {};
        var summaryBar = document.getElementById('cluster-health-summary-bar');
        if (summaryBar) {
          pushSparkPoint('_cluster', 'cpu', s.total_cpu_percent || 0);
          pushSparkPoint('_cluster', 'mem', s.total_mem_percent || 0);
          var cpuColor = healthBarColor(s.total_cpu_percent || 0, CLUSTER_CPU_WARN_PCT, CLUSTER_CPU_DANGER_PCT);
          var memColor = healthBarColor(s.total_mem_percent || 0, CLUSTER_MEM_WARN_PCT, CLUSTER_MEM_DANGER_PCT);
          var clusterCount = (data.clusters || []).length;
          // Disk is omitted when no node reported usage; showing 0% there
          // would claim the fleet is healthy on no evidence at all.
          var diskSegment = '';
          if (s.total_disk_percent != null) {
            pushSparkPoint('_cluster', 'disk', s.total_disk_percent);
            var diskColor = healthBarColor(s.total_disk_percent, CLUSTER_DISK_WARN_PCT, CLUSTER_DISK_DANGER_PCT);
            diskSegment = renderUnicodeSparkline('_cluster', 'disk', diskColor) +
              ' <span style="color:' + diskColor + '">' + s.total_disk_percent + '% disk</span> · ';
          } else {
            diskSegment = '<span style="color:var(--muted)" title="No node reported live disk usage">— disk</span> · ';
          }
          summaryBar.innerHTML = clusterCount + ' cluster' + (clusterCount !== 1 ? 's' : '') + ' · ' +
            (s.total_nodes || 0) + ' nodes · ' + (s.total_cpu_cores || 0) + ' vCPU · ' +
            renderUnicodeSparkline('_cluster', 'cpu', cpuColor) + ' <span style="color:' + cpuColor + '">' + (s.total_cpu_percent || 0) + '% cpu</span> · ' +
            renderUnicodeSparkline('_cluster', 'mem', memColor) + ' <span style="color:' + memColor + '">' + (s.total_mem_percent || 0) + '% mem</span> · ' +
            diskSegment +
            (s.hive_count || 0) + ' hives';
        }

        var body = document.getElementById('cluster-health-body');
        var toggle = document.getElementById('cluster-health-toggle');
        if (!_clusterHealthCollapsed) {
          if (body) body.style.display = '';
          if (toggle) toggle.style.transform = 'rotate(90deg)';
        }

        var grid = document.getElementById('cluster-health-grid');
        if (!grid) return;

        // Render per-cluster sections if available, otherwise fall back to flat nodes.
        var clusters = data.clusters || [];
        if (clusters.length > 0) {
          grid.style.display = 'block';
          grid.innerHTML = clusters.map(function(c) {
            var cLabel = c.name || c.id;
            var cs = c.summary || {};
            var cCpuColor = healthBarColor(cs.total_cpu_percent || 0, CLUSTER_CPU_WARN_PCT, CLUSTER_CPU_DANGER_PCT);
            var cMemColor = healthBarColor(cs.total_mem_percent || 0, CLUSTER_MEM_WARN_PCT, CLUSTER_MEM_DANGER_PCT);
            // Absent disk data (e.g. an unreachable cluster) renders dashed,
            // never as 0% — a full disk and an unknown disk must not look alike.
            var cDiskSegment;
            if (cs.total_disk_percent != null) {
              var cDiskColor = healthBarColor(cs.total_disk_percent, CLUSTER_DISK_WARN_PCT, CLUSTER_DISK_DANGER_PCT);
              cDiskSegment = '<span style="color:' + cDiskColor + '">' + cs.total_disk_percent + '% disk</span> · ';
            } else {
              cDiskSegment = '<span style="color:var(--muted)" title="No node in this cluster reported live disk usage">— disk</span> · ';
            }
            var gpuLine = '';
            if (c.gpu_summary) {
              gpuLine = ' · <span style="color:var(--green)">' + c.gpu_summary.allocatable_gpus + '/' + c.gpu_summary.total_gpus + ' GPUs</span>';
            }
            // Remaining hive capacity: omitted entirely when the collector had
            // no pod request data (field absent); 0 means the cluster is full.
            var capacityLine = '';
            if (cs.hive_capacity_remaining != null) {
              var capRemaining = cs.hive_capacity_remaining;
              capacityLine = ' · <span title="Estimated headroom: per-hive CPU/memory request footprint bin-packed into free (allocatable minus requested) capacity on Ready, schedulable nodes only">room for ~' + capRemaining + ' more hive' + (capRemaining === 1 ? '' : 's') + '</span>';
            }
            // Stuck (orphaned Terminating) hive pods — the residue of nodes
            // disappearing without draining (#5328). ABSENT means the hub could
            // not determine it and nothing is claimed; a present zero is
            // deliberately silent so a healthy fleet stays quiet. Only a real
            // non-zero count renders, because the whole cost of this incident
            // was that 27 orphans across 15 namespaces accumulated for three
            // weeks with nothing reporting them.
            var stuckLine = '';
            if (c.stuck_pods && c.stuck_pods.total > 0) {
              var sp = c.stuck_pods;
              var nsList = (sp.namespaces || []).map(function(x) { return x.namespace + ' (' + x.count + ')'; }).join(', ');
              if (sp.truncated) { nsList += ', …'; }
              var stuckTitle = 'Hive pods stuck Terminating past the reaper threshold: deletionTimestamp set, no finalizers, not Running. ' +
                'Signature of a node removed without draining. Affected namespaces: ' + nsList;
              stuckLine = ' · <span style="color:var(--red)" title="' + esc(stuckTitle) + '">' +
                sp.total + ' stuck pod' + (sp.total === 1 ? '' : 's') +
                ' in ' + sp.namespaces_affected + ' ns</span>';
            }
            var errorLine = c.error ? '<div style="margin:8px 0;padding:6px 10px;background:rgba(239,68,68,0.1);border:1px solid rgba(239,68,68,0.3);border-radius:6px;font-size:0.75rem;color:var(--red)">' + esc(c.error) + '</div>' : '';
            var headerHtml = '<div style="display:flex;align-items:center;gap:8px;margin:16px 0 8px">' +
              clusterBadge(c.id, c.name) +
              '<span style="font-size:0.85rem;color:var(--text);font-weight:600">' + esc(cLabel) + '</span>' +
              '<span style="font-size:0.75rem;color:var(--muted)">' +
              (cs.total_nodes || 0) + ' nodes · ' + (cs.total_cpu_cores || 0) + ' vCPU · ' +
              '<span style="color:' + cCpuColor + '">' + (cs.total_cpu_percent || 0) + '% cpu</span> · ' +
              '<span style="color:' + cMemColor + '">' + (cs.total_mem_percent || 0) + '% mem</span> · ' +
              cDiskSegment +
              (c.hive_count || 0) + ' hives' + capacityLine + gpuLine + stuckLine +
              '</span></div>';
            var nodesHtml = (c.nodes || []).length > 0
              ? '<div style="display:grid;grid-template-columns:repeat(2,1fr);gap:12px">' + (c.nodes || []).map(renderNodeCard).join('') + '</div>'
              : '';
            return headerHtml + errorLine + nodesHtml;
          }).join('');
        } else {
          // Fallback: flat node list (single cluster / backward compat).
          grid.style.display = 'grid';
          grid.style.gridTemplateColumns = 'repeat(2,1fr)';
          var nodes = data.nodes || [];
          if (!nodes.length) {
            grid.innerHTML = '<div style="color:var(--muted);font-size:0.85rem;grid-column:span 2">No node data available</div>';
            return;
          }
          grid.innerHTML = nodes.map(renderNodeCard).join('');
        }
      } catch(e) {
        console.error('cluster health error:', e);
      }
    }

    // --- PR Reach Panel (#3995, phase 2c of #3973) ---
    // Admin-only, same contract as the Cluster Health panel above: hidden
    // until /api/reach answers OK, hidden again on 403, collapse state in
    // localStorage. Polls far slower than cluster health — every load fans
    // out merged-PR + ancestry lookups hub-side (GitHub API), so 5 minutes
    // is the courtesy floor, and reach itself moves at heartbeat cadence.
    var REACH_POLL_MS = 300000;
    var _reachCollapsed = localStorage.getItem('hive-reach-collapsed') !== 'false';

    function toggleReach() {
      _reachCollapsed = !_reachCollapsed;
      localStorage.setItem('hive-reach-collapsed', _reachCollapsed ? 'true' : 'false');
      var body = document.getElementById('reach-body');
      var toggle = document.getElementById('reach-toggle');
      if (body) body.style.display = _reachCollapsed ? 'none' : '';
      if (toggle) toggle.style.transform = _reachCollapsed ? '' : 'rotate(90deg)';
    }

    /* Renders merge→first-execution latency compactly; null means nothing
       qualifying has executed yet and must read as absent, never as 0. */
    function reachLatency(seconds) {
      if (seconds == null || !isFinite(seconds) || seconds < 0) return '<span style="color:var(--muted)">—</span>';
      var SEC_PER_MIN = 60, SEC_PER_HOUR = 3600, SEC_PER_DAY = 86400;
      if (seconds < SEC_PER_HOUR) return Math.round(seconds / SEC_PER_MIN) + 'm';
      if (seconds < SEC_PER_DAY) return (seconds / SEC_PER_HOUR).toFixed(1) + 'h';
      return (seconds / SEC_PER_DAY).toFixed(1) + 'd';
    }

    /* Renders the error-delta cell from a report's per-component deltas.
       The measured flag is the contract (#3995): an unmeasured delta must be
       DISTINGUISHABLE from a measured zero, so unmeasured renders as the word
       "unmeasured", never as 0.0. Measured deltas show the worst (largest
       magnitude) component, red when errors rose, green when they fell; the
       tooltip itemizes every component either way. */
    function reachDeltaCell(deltas) {
      deltas = deltas || [];
      var measured = deltas.filter(function(d) { return d && d.measured; });
      var PCT = 100;
      var tip = deltas.map(function(d) {
        if (!d) return '';
        if (!d.measured) return d.component + ': unmeasured';
        return d.component + ': ' + (d.delta >= 0 ? '+' : '') + (d.delta * PCT).toFixed(2) +
          'pp over ' + d.windows_compared + 'w (' + (d.error_rate_before * PCT).toFixed(2) +
          '% -> ' + (d.error_rate_after * PCT).toFixed(2) + '%)';
      }).filter(Boolean).join('\n');
      if (!measured.length) {
        return '<span style="color:var(--muted)" title="' + esc(tip || 'No deploy window observed yet, or not enough before/after history') + '">unmeasured</span>';
      }
      var worst = measured[0];
      measured.forEach(function(d) { if (Math.abs(d.delta) > Math.abs(worst.delta)) worst = d; });
      var color = worst.delta > 0 ? 'var(--red)' : (worst.delta < 0 ? 'var(--green)' : 'var(--muted)');
      var sign = worst.delta >= 0 ? '+' : '';
      return '<span style="color:' + color + ';font-family:monospace" title="' + esc(tip) + '">' +
        sign + (worst.delta * PCT).toFixed(2) + 'pp</span>';
    }

    function renderReachRow(r) {
      var comps = (r.attribution && r.attribution.components) || [];
      var coveragePct = Math.round(((r.attribution && r.attribution.coverage) || 0) * 100);
      var compCell = comps.length
        ? '<span title="Attribution coverage ' + coveragePct + '% of changed files">' + esc(comps.join(', ')) + '</span>'
        : '<span style="color:var(--muted)" title="No changed file maps to an instrumented component (docs/workflows/deploy)">unattributable</span>';
      var hives = r.reach_hives || [];
      var reachCell = r.reach_count
        ? '<span title="' + esc(hives.join(', ')) + '">' + r.reach_count + '</span>'
        : '<span style="color:var(--muted)">0</span>';
      var flags = [];
      if (r.never_ran) flags.push('<span style="padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(239,68,68,0.15);color:var(--red);border:1px solid rgba(239,68,68,0.3)" title="Merged and deployed, but no touched component has executed anywhere in ' + (r.never_ran_threshold_days || 0) + ' days — the category acceptance-rate cannot see">never ran</span>');
      else if (!r.deployed) flags.push('<span style="padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(107,114,128,0.15);color:#9ca3af;border:1px solid rgba(107,114,128,0.3)" title="No hive reports running a commit containing this PR yet (merged is not deployed)">not deployed</span>');
      if ((r.shared_with || []).length) flags.push('<span style="padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(128,191,255,0.15);color:#80bfff;border:1px solid rgba(128,191,255,0.3)" title="Co-deployed with PR ' + esc((r.shared_with || []).map(function(n) { return '#' + n; }).join(', ')) + ' — reach and deltas are shared by construction (D4)">shared</span>');
      return '<tr>' +
        '<td style="white-space:nowrap"><a href="https://github.com/kubestellar/hive/pull/' + r.pr + '" target="_blank">#' + r.pr + '</a>' +
        (r.title ? ' <span style="color:var(--muted);font-size:0.75rem" title="' + esc(r.title) + '">' + esc(r.title.length > 48 ? r.title.slice(0, 48) + '…' : r.title) + '</span>' : '') + '</td>' +
        '<td>' + compCell + '</td>' +
        '<td>' + reachCell + '</td>' +
        '<td>' + reachLatency(r.first_execution_latency_seconds) + '</td>' +
        '<td>' + reachDeltaCell(r.error_deltas) + '</td>' +
        '<td style="white-space:nowrap">' + (flags.join(' ') || '<span style="color:var(--muted)">—</span>') + '</td>' +
        '</tr>';
    }

    async function loadReach() {
      if (!_isAdmin) return;
      try {
        var resp = await fetch('/api/reach');
        if (resp.status === 403) {
          document.getElementById('reach-section').style.display = 'none';
          return;
        }
        /* 503 = hub running without GitHub credentials: no PR facts exist, so
           the section stays hidden rather than rendering an empty shell. */
        if (!resp.ok) return;
        var data = await resp.json();
        document.getElementById('reach-section').style.display = '';

        var reports = data.reports || [];
        var summaryBar = document.getElementById('reach-summary-bar');
        if (summaryBar) {
          var neverRan = reports.filter(function(r) { return r.never_ran; }).length;
          var reached = reports.filter(function(r) { return (r.reach_count || 0) > 0; }).length;
          summaryBar.textContent = reports.length + ' merged PR' + (reports.length !== 1 ? 's' : '') +
            ' · ' + (data.hives_reporting || 0) + ' hives reporting · ' + reached + ' reached' +
            (neverRan ? ' · ' + neverRan + ' never ran' : '');
        }

        var body = document.getElementById('reach-body');
        var toggle = document.getElementById('reach-toggle');
        if (!_reachCollapsed) {
          if (body) body.style.display = '';
          if (toggle) toggle.style.transform = 'rotate(90deg)';
        }

        var container = document.getElementById('reach-table-container');
        if (!container) return;
        if (!reports.length) {
          container.innerHTML = '<div style="color:var(--muted);font-size:0.85rem">No recently merged PRs to report on</div>';
          return;
        }
        container.innerHTML =
          '<table class="hive-table"><thead><tr>' +
          '<th style="text-align:left">PR</th><th>Components</th><th title="Distinct hives running a commit containing the PR whose touched components executed since merge">Reach (hives)</th><th title="Merge to first qualifying execution anywhere in the fleet">First exec</th><th title="Span error-rate after vs before the PR&#39;s deploy window; unmeasured when either side lacks data (#3995)">Error Δ</th><th>Flags</th>' +
          '</tr></thead><tbody>' + reports.map(renderReachRow).join('') + '</tbody></table>';
      } catch(e) {
        console.error('reach error:', e);
      }
    }

    async function loadClusters() {
      try {
        var resp = await fetch('/api/hub/clusters');
        if (!resp.ok) return;
        var clusters = await resp.json();
        _clusterList = clusters || [];
        var sel = document.getElementById('f-cluster');
        if (!sel || !clusters || !clusters.length) return;
        sel.innerHTML = clusters.map(function(c) {
          var caps = [];
          if (c.has_gpu) caps.push('GPU');
          if (c.arch) caps.push(c.arch);
          /* Surface the GitHub host: on a GitHub Enterprise cluster the App
             lives on that GHE instance, and picking the wrong cluster is the
             difference between an install request the org admin can see and
             one that silently lands on public github.com. */
          if (c.github_host && c.github_host !== PUBLIC_GITHUB_HOST) caps.push(c.github_host);
          var label = c.name || c.id;
          if (caps.length) label += ' (' + caps.join(', ') + ')';
          return '<option value="' + esc(c.id) + '">' + esc(label) + '</option>';
        }).join('');
        sel.removeEventListener('change', syncCreateModalInstallLinks);
        sel.addEventListener('change', syncCreateModalInstallLinks);
        syncCreateModalInstallLinks();
      } catch(e) { /* cluster dropdown stays at default */ }
    }

    /* PUBLIC_GITHUB_HOST is the bare hostname the hub reports for a cluster
       with no GitHub Enterprise override. Anything else is a GHE instance. */
    var PUBLIC_GITHUB_HOST = 'github.com';

    /* selectedClusterEntry returns the ClusterListEntry for the cluster chosen
       in the create-hive modal, or null when the list has not loaded. */
    function selectedClusterEntry() {
      var sel = document.getElementById('f-cluster');
      if (!sel || !sel.value) return null;
      var list = _clusterList || [];
      for (var i = 0; i < list.length; i++) {
        if (list[i] && list[i].id === sel.value) return list[i];
      }
      return null;
    }

    /* PUBLIC_GITHUB_SENTINEL is the value the assign/approve APIs accept to
       FORCE public github.com on a cluster whose defaults point at a GitHub
       Enterprise instance. It mirrors githubHostPublic on the server, where
       effectiveGitHubBaseURL resolves it to "" (public). It is deliberately
       NOT a hostname — the server exempts it from the hostname validator. */
    var PUBLIC_GITHUB_SENTINEL = 'public';

    /* clusterEntryById returns the ClusterListEntry for a cluster id, or null
       when the cluster list has not loaded (or the id is unknown). An empty id
       is how the hub records "the default cluster", so fall through to a
       lookup rather than guessing a host. */
    function clusterEntryById(clusterId) {
      if (!clusterId) return null;
      var list = _clusterList || [];
      for (var i = 0; i < list.length; i++) {
        if (list[i] && list[i].id === clusterId) return list[i];
      }
      return null;
    }

    /* clusterGitHubHost returns the bare GitHub host a cluster's hives default
       to ('github.com' for public GitHub), or '' when the cluster is unknown /
       the list has not loaded yet. Never guess github.com for an unknown
       cluster: silently claiming "public" is the exact bug this control
       exists to surface. */
    function clusterGitHubHost(clusterId) {
      var c = clusterEntryById(clusterId);
      return (c && c.github_host) || '';
    }

    /* githubHostChoiceMarkup renders the GitHub-instance picker shared by the
       assign and approve modals.

       Options are: the cluster's own host (the default, and the right answer
       in almost every case), explicit public github.com, and a free-text host
       for the rare cross-instance assignment. clusterHost may be '' when the
       cluster list has not loaded — the control then opens on "Use cluster
       default", which submits nothing and lets the SERVER's cluster backfill
       decide, exactly as if the control did not exist. */
    function githubHostChoiceMarkup(idPrefix, clusterHost, requestHost, fld, lbl) {
      var isGHE = clusterHost && clusterHost !== PUBLIC_GITHUB_HOST;
      var defaultLabel = clusterHost
        ? 'Cluster default — ' + esc(clusterHost)
        : 'Cluster default';
      /* A provision request's github_host, when present, is what the REQUESTER
         told us their org lives on. Prefer it over the cluster default: they
         know which GitHub their org is on and we should not silently override
         them. */
      var preselectCustom = !!(requestHost && requestHost !== clusterHost && requestHost !== PUBLIC_GITHUB_HOST);
      var opts =
        '<option value="">' + defaultLabel + '</option>' +
        '<option value="' + esc(PUBLIC_GITHUB_SENTINEL) + '">Public github.com' + (isGHE ? ' (override)' : '') + '</option>' +
        '<option value="__custom__"' + (preselectCustom ? ' selected' : '') + '>Other GitHub Enterprise host…</option>';
      return '<label style="' + lbl + '">GitHub instance</label>' +
        '<select id="' + esc(idPrefix) + '-ghhost" style="' + fld + '">' + opts + '</select>' +
        '<input id="' + esc(idPrefix) + '-ghhost-custom" style="' + fld + ';margin-top:6px;display:' + (preselectCustom ? 'block' : 'none') + '" ' +
          'placeholder="github.example.com" value="' + esc(preselectCustom ? requestHost : '') + '">' +
        '<div id="' + esc(idPrefix) + '-ghhost-note" style="font-size:0.72rem;color:var(--muted);margin-top:6px"></div>';
    }

    /* wireGitHubHostChoice attaches the behaviour for a picker rendered by
       githubHostChoiceMarkup: show the free-text box only for "Other", and
       keep a live note of exactly which GitHub instance the hive will target.
       Uses addEventListener rather than inline handlers because the values
       involved (hosts, org names) are user-controlled. */
    function wireGitHubHostChoice(idPrefix, clusterHost) {
      var sel = document.getElementById(idPrefix + '-ghhost');
      var custom = document.getElementById(idPrefix + '-ghhost-custom');
      var note = document.getElementById(idPrefix + '-ghhost-note');
      if (!sel) return;
      function render() {
        var isCustom = sel.value === '__custom__';
        if (custom) custom.style.display = isCustom ? 'block' : 'none';
        if (!note) return;
        var target;
        if (isCustom) {
          target = (custom && custom.value.trim()) || '';
        } else if (sel.value === PUBLIC_GITHUB_SENTINEL) {
          target = PUBLIC_GITHUB_HOST;
        } else {
          target = clusterHost || '';
        }
        if (!target) {
          note.textContent = 'This hive will use its cluster’s GitHub instance.';
          note.style.color = 'var(--muted)';
          return;
        }
        var ghe = target !== PUBLIC_GITHUB_HOST;
        note.textContent = 'This hive will target ' + target + '.' +
          (ghe ? ' The GitHub App must be installed on the org on ' + target + ' — a public github.com App will not authenticate there.' : '');
        note.style.color = ghe ? 'var(--accent, #58a6ff)' : 'var(--muted)';
      }
      /* Idempotent: this is re-invoked when the assign modal's placeholder
         picker changes the cluster, and binding a second listener each time
         would leave a stale closure (holding the OLD clusterHost) racing the
         new one. Store the current renderer and swap it out. */
      if (sel._ghHostRender) {
        sel.removeEventListener('change', sel._ghHostRender);
        if (custom) custom.removeEventListener('input', sel._ghHostRender);
      }
      sel._ghHostRender = render;
      sel.addEventListener('change', render);
      if (custom) custom.addEventListener('input', render);
      render();
    }

    /* githubHostChoiceValue returns what to send as github_host: '' for
       "cluster default" (the server backfills), the 'public' sentinel, or a
       typed GHE hostname. */
    function githubHostChoiceValue(idPrefix) {
      var sel = document.getElementById(idPrefix + '-ghhost');
      if (!sel) return '';
      if (sel.value !== '__custom__') return sel.value;
      var custom = document.getElementById(idPrefix + '-ghhost-custom');
      return (custom && custom.value.trim()) || '';
    }

    /* syncCreateModalInstallLinks points every "install the app" link in the
       create-hive modal at the SELECTED cluster's GitHub host. The hub serves
       one page to admins provisioning onto both public GitHub and GitHub
       Enterprise clusters, so a static github.com anchor is wrong for half of
       them: a GHE user following it lands on public github.com and their org
       admin never sees the pending install request.

       The URL comes from the server (config.GitHubConfig.AppInstallURL), which
       also honours a per-cluster app slug — a GHE instance hosts its own App
       registration and it is rarely named "kubestellar-hive". */
    function syncCreateModalInstallLinks() {
      var c = selectedClusterEntry();
      var url = (c && c.app_install_url) || '';
      var host = (c && c.github_host) || PUBLIC_GITHUB_HOST;
      var isGHE = host !== PUBLIC_GITHUB_HOST;
      var ids = ['auth-info-app-link', 'auth-info-later-link', 'auth-later-install-link'];
      for (var i = 0; i < ids.length; i++) {
        var el = document.getElementById(ids[i]);
        if (!el) continue;
        if (url) {
          el.href = url;
          el.removeAttribute('aria-disabled');
        } else {
          /* No URL yet (clusters still loading). Do not fall back to a
             github.com guess — a dead link is safer than one that sends a
             GHE admin to the wrong GitHub. */
          el.removeAttribute('href');
          el.setAttribute('aria-disabled', 'true');
        }
      }
      var appLink = document.getElementById('auth-info-later-link');
      if (appLink) appLink.textContent = '→ Install app on ' + host + ' now';
      var btn = document.getElementById('auth-later-install-link');
      if (btn) btn.textContent = 'Install app on your ' + (isGHE ? host + ' ' : '') + 'org';
      var note = document.getElementById('auth-later-ghe-note');
      if (note) {
        if (isGHE) {
          note.textContent = 'This cluster targets ' + host + '. The GitHub App must be registered and'
            + ' installed on that GitHub Enterprise instance — a public github.com install will not'
            + ' reach it, and the ' + host + ' org admin will see no pending request.';
          note.style.display = '';
        } else {
          note.textContent = '';
          note.style.display = 'none';
        }
      }
    }

    // OpenRouter scan-to-fund return: toast the result and clear the query flag
    // so a reload doesn't re-toast.
    function handleOpenRouterReturn() {
      try {
        var p = new URLSearchParams(window.location.search);
        var or = p.get('openrouter');
        if (or === 'connected') hiveToast('OpenRouter funded — the key is being delivered to the hive', 'success');
        else if (or === 'error') hiveToast('OpenRouter funding failed — please try again', 'error');
        if (or) {
          p.delete('openrouter');
          var qs = p.toString();
          history.replaceState(null, '', window.location.pathname + (qs ? '?' + qs : ''));
        }
      } catch (e) { /* non-fatal */ }
    }

    async function init() {
      /* Restore grouping, collapse state and saved views (including applying
         the default view) BEFORE the first loadHives(), so the initial render
         already reflects the operator's view rather than flashing the ungrouped
         list and then rearranging. */
      loadDashViewPrefs();
      /* Sort is resolved AFTER loadDashViewPrefs because applying a default
         saved view sets a key/direction of its own: the operator's explicitly
         stored sort is the more specific signal and must have the last word.
         Both run before the first paint so no render happens in the wrong
         order. */
      loadHiveSortPrefs();
      /* Progressive paint: put the last known fleet on screen immediately so a
         returning operator sees their hives instead of a spinner. The network
         path below still runs unconditionally and reconciles the list; this
         only changes what fills the gap while it is in flight. Must come after
         loadDashViewPrefs() so the cached rows honour the active view. */
      paintCachedHives();
      /* Record the offset from here on. Attached before the awaits below so a
         scroll during a slow load is still captured. */
      window.addEventListener('scroll', persistHiveScroll, {passive: true});
      await loadUser();
      await autoRequestAccessFromUrl();
      await loadHives();
      /* Restore AFTER the real rows are in the DOM — loadHives' renderHives is
         what gives the document its full height, and restoreHiveScroll still
         waits for that height before it commits (see its frame loop). Calling
         it any earlier clamps the offset to a short page and lands at the top. */
      restoreHiveScroll();
      await loadAdminUsers();
      if (!_adminLoaded) setTimeout(loadAdminUsers, 2000);
      loadClusterHealth();
      loadScaleSettings();
      loadReach();
      loadClusters();
      handleOpenRouterReturn();
    }
    init();
    var POLL_INTERVAL_MS = 30000;
    setInterval(loadHives, POLL_INTERVAL_MS);
    /* Live-tick any claim-pending counters once a second so an assigned-but-
       unclaimed row's "claim pending · Nm" advances between the 30s polls. */
    startAssignCounterTicker();
    setInterval(loadAdminUsers, POLL_INTERVAL_MS);
    setInterval(loadClusterHealth, CLUSTER_HEALTH_POLL_MS);
    setInterval(loadScaleSettings, CLUSTER_HEALTH_POLL_MS);
    setInterval(loadReach, REACH_POLL_MS);
    var _refreshTimer = null;
    var REFRESH_DEBOUNCE_MS = 500;
    function debouncedRefresh() {
      if (_refreshTimer) return;
      _refreshTimer = setTimeout(function() { _refreshTimer = null; loadHives(); loadAdminUsers(); }, REFRESH_DEBOUNCE_MS);
    }
    document.addEventListener('visibilitychange', function() { if (!document.hidden) debouncedRefresh(); });
    window.addEventListener('focus', debouncedRefresh);

    var _allUsers = [];
    var _adminLoaded = false;
    var _adminExpandedUsers = {};
    var _hiveRegistry = [];
    /* Lowercased usernames with a live hive session right now (admin payload).
       Drives the green-dashed avatar border via avatarImg. Empty for non-admins. */
    var _liveHiveUsers = new Set();
    /* The honest subset of _liveHiveUsers: focused tab + recent input, per the
       spokes' presence reports. live minus engaged = idle open tabs. */
    var _liveEngagedUsers = new Set();
    var _userSortKey = 'created_at', _userSortAsc = false;

    /* --- Collapsible "Hub Admin — Users" section ---
       The roster runs to ~90 records, so an expanded users table pushes the
       Hub Banner and Cluster Health sections far below the fold. An operator
       is normally here for hives, not for the roster, so this follows the same
       convention as the other long admin sections (Past Requests, Cluster
       Health): collapsed by default, remembered per browser under the same
       'hive-*-collapsed' localStorage key convention. Absent key means
       collapsed; only an explicit 'false' expands, so a first visit and a
       corrupted value behave identically. */
    var ADMIN_USERS_COLLAPSED_KEY = 'hive-admin-users-collapsed';
    var _adminUsersCollapsed = localStorage.getItem(ADMIN_USERS_COLLAPSED_KEY) !== 'false';

    /* applyAdminUsersCollapsed pushes _adminUsersCollapsed onto the DOM. Split
       out from the toggle so the initial render and expandAdminUsersSection()
       can restore state without duplicating the show/hide + aria bookkeeping. */
    function applyAdminUsersCollapsed() {
      var body = document.getElementById('admin-users-body');
      var toggle = document.getElementById('admin-users-toggle');
      var header = document.getElementById('admin-users-header');
      if (body) body.style.display = _adminUsersCollapsed ? 'none' : '';
      /* ▸ collapsed / ▾ expanded */
      if (toggle) toggle.innerHTML = _adminUsersCollapsed ? '&#9656;' : '&#9662;';
      if (header) header.setAttribute('aria-expanded', _adminUsersCollapsed ? 'false' : 'true');
    }

    /* persistAdminUsersCollapsed writes the current state. localStorage throws
       under private-browsing quota; collapsing must still work in-session, so
       the write is best-effort. */
    function persistAdminUsersCollapsed() {
      try {
        localStorage.setItem(ADMIN_USERS_COLLAPSED_KEY, _adminUsersCollapsed ? 'true' : 'false');
      } catch(e) {}
    }

    function toggleAdminUsers() {
      _adminUsersCollapsed = !_adminUsersCollapsed;
      persistAdminUsersCollapsed();
      applyAdminUsersCollapsed();
    }

    /* expandAdminUsersSection opens the section and PERSISTS that, mirroring
       expandAllHiveSections (#2348). Anything that reveals a row inside this
       section must go through here rather than flipping display directly: an
       in-memory-only expand silently re-collapses on the next reload, so the
       row the operator was just sent to would vanish again. */
    function expandAdminUsersSection() {
      if (!_adminUsersCollapsed) return;
      _adminUsersCollapsed = false;
      persistAdminUsersCollapsed();
      applyAdminUsersCollapsed();
    }

    /* The count beside the header is the only signal of roster size while the
       section is collapsed, so it is kept current on every render. */
    function updateAdminUsersCount(shown, total) {
      var el = document.getElementById('admin-users-count');
      if (!el) return;
      var n = Number(total) || 0;
      el.textContent = (Number(shown) === n)
        ? '(' + n + ')'
        : '(' + (Number(shown) || 0) + ' of ' + n + ')';
    }

    function fmtUserTS(ts) {
      if (!ts) return '';
      var d = new Date(ts);
      if (isNaN(d.getTime())) return ts.substring(0, 10);
      // 12-hour clock with a compact single-letter meridiem (e.g. "9:21p").
      var parts = d.toLocaleString('en-US', {year:'numeric',month:'2-digit',day:'2-digit',hour:'numeric',minute:'2-digit',hour12:true,timeZone:'America/New_York'}).replace(',','').split(' ');
      var meridiem = (parts.pop() || '').toLowerCase().charAt(0); // "AM"->"a", "PM"->"p"
      return parts.join(' ') + meridiem + ' EDT';
    }

    function sortUsers(key) {
      if (_userSortKey === key) { _userSortAsc = !_userSortAsc; } else { _userSortKey = key; _userSortAsc = true; }
      applySortUsers();
    }

    function applySortUsers() {
      var key = _userSortKey;
      var q = (document.getElementById('user-search') ? document.getElementById('user-search').value : '').toLowerCase();
      // Search matches the GitHub login and the contact fields, so an admin can
      // find someone by real name or by something they wrote in the notes —
      // which is the point of keeping notes here in the first place.
      var filtered = (_allUsers || []).filter(function(u) {
        if (!q) return true;
        if (!u) return false;
        var hay = [u.github_username, u.display_name, u.email, u.full_name, u.slack_id, u.company, u.notes]
          .filter(function(v) { return !!v; }).join(' ').toLowerCase();
        return hay.includes(q);
      });
      var sorted = filtered.slice().sort(function(a, b) {
        var va, vb;
        if (key === 'hiveCount') {
          var regIds = new Set((_hiveRegistry || []).map(function(h) { return h.id; }));
          va = Object.keys(a.hives || {}).filter(function(h) { return regIds.has(h); }).length;
          vb = Object.keys(b.hives || {}).filter(function(h) { return regIds.has(h); }).length;
        } else if (key === 'status') {
          va = userTierRank(a);
          vb = userTierRank(b);
        } else {
          va = a[key] || ''; vb = b[key] || '';
        }
        if (typeof va === 'number' && typeof vb === 'number') return _userSortAsc ? va - vb : vb - va;
        return _userSortAsc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
      });
      updateAdminUsersCount(sorted.length, (_allUsers || []).length);
      renderUsers(sorted, true);
    }

    function toggleAdminExpand(username) {
      _adminExpandedUsers[username] = !_adminExpandedUsers[username];
      var el = document.getElementById('expand-' + username);
      if (el) el.style.display = _adminExpandedUsers[username] ? '' : 'none';
    }
    var _adminLoading = false;
    async function loadAdminUsers() {
      if (_adminLoading) return;
      _adminLoading = true;
      try {
        var resp = await fetch('/api/saas/admin/users');
        if (resp.status === 403) {
          if (!_adminLoaded) { document.getElementById('admin-section').style.display = 'none'; document.getElementById('hub-banner-section').style.display = 'none'; document.getElementById('btn-send-banner-top').style.display = 'none'; }
          return;
        }
        _adminLoaded = true;
        document.getElementById('admin-section').style.display = '';
        var navFleet = document.getElementById('nav-fleet');
        if (navFleet) navFleet.style.display = '';
        /* The section is display:none until the admin check passes, so this is
           the first point at which the persisted collapse state can be pushed
           onto real DOM. Idempotent, so running it on every poll is fine. */
        applyAdminUsersCollapsed();
        document.getElementById('hub-banner-section').style.display = '';
        document.getElementById('btn-send-banner-top').style.display = '';
        loadActiveBanner();
        var data = await resp.json();
        _allUsers = data.users || [];
        try { applySortUsers(); } catch(re) { console.error('renderUsers error:', re); }
        /* Rollup rides the same admin poll as the table it sits under, so the
           two can never disagree about the roster. Not awaited: a slow or
           failed rollup must not delay or break the users table. */
        loadUserCountries();
      } catch(e) {
        if (!_adminLoaded) document.getElementById('admin-section').style.display = 'none';
      } finally {
        _adminLoading = false;
      }
    }

    function filterUsers() {
      applySortUsers();
    }

    /* ---- Fleet country rollup -------------------------------------------
       Where the hub's user base is, in aggregate. Reads
       /api/saas/admin/user-countries, which is requireAdmin and returns COUNTS
       only — no usernames — so this surface cannot be used to look up an
       individual.

       Deliberately NOT a map and NOT a chart: the hub ships no external CDN and
       no external images, so a map would mean vendoring geometry and a charting
       library would mean a script tag we cannot serve. A ranked bar list says
       the same thing in less space and is readable at a glance.

       The unknown bucket is rendered as its own row, always, with the same
       weight as a country. Country is optional and best-effort, so early on
       unknown IS the majority; hiding it would turn "3 of 200 users told us
       they're in DE" into something that looks like "the fleet is German". */

    /* Width of the widest bar, in percent of the list. The bars are scaled
       against the LARGEST bucket rather than the total so the smaller
       countries stay visible when one bucket dominates — which it will, since
       unknown starts at ~100%. */
    var COUNTRY_BAR_MAX_PCT = 100;

    /* countryRollupRow renders one bucket. label is pre-built HTML (a flag+code
       chip, or the plain "Unknown" text); everything else is numeric. */
    function countryRollupRow(label, count, max, total) {
      /* max is guaranteed >= 1 by the caller — see renderCountryRollup, which
         returns early on an empty population. Guarding again here anyway
         because a zero would silently produce NaN% and a bar that vanishes
         rather than an error anyone would notice. */
      var pct = (max > 0) ? Math.round((count / max) * COUNTRY_BAR_MAX_PCT) : 0;
      var share = (total > 0) ? Math.round((count / total) * 100) : 0;
      return '<div style="display:flex;align-items:center;gap:8px;margin-bottom:4px">' +
        '<div style="flex:none;width:72px;font-size:0.72rem;color:var(--text)">' + label + '</div>' +
        '<div style="flex:1;height:8px;background:var(--border);border-radius:4px;overflow:hidden">' +
          '<div style="width:' + pct + '%;height:100%;background:var(--accent)"></div>' +
        '</div>' +
        '<div style="flex:none;width:84px;text-align:right;font-size:0.7rem;color:var(--muted)">' +
          count + ' &middot; ' + share + '%</div>' +
      '</div>';
    }

    function renderCountryRollup(data) {
      var el = document.getElementById('user-countries-container');
      if (!el) return;
      var countries = (data && data.countries) || [];
      var unknown = Number(data && data.unknown) || 0;
      var total = Number(data && data.total) || 0;
      /* No users at all is the only case with nothing to say. An all-unknown
         population is NOT this case: it still renders, as a single 100% Unknown
         bar, which is exactly the state the operator needs to see on day one. */
      if (total <= 0) {
        el.innerHTML = '<div style="font-size:0.75rem;color:var(--muted)">No users yet.</div>';
        return;
      }
      /* Scale against the biggest bucket, unknown included, so no bar can ever
         exceed 100%. Floored at 1 so the divide is safe even if a future caller
         reaches here with all-zero counts. */
      var max = unknown;
      countries.forEach(function(c) { if (Number(c.count) > max) max = Number(c.count); });
      if (max < 1) max = 1;

      var rows = countries.map(function(c) {
        /* The server normalizes before sending; normalizing again is what
           guarantees a legacy or hand-edited record can never emit half a code
           point into the flag. A code that fails the check is dropped rather
           than rendered raw. */
        var code = normalizeCountryCode(c && c.code);
        if (!code) return '';
        var label = '<span style="display:inline-flex;align-items:center;gap:4px">' +
          countryFlagHTML(code) + esc(code) + '</span>';
        return countryRollupRow(label, Number(c.count) || 0, max, total);
      }).join('');

      /* Unknown is last (it is not a country and should not head a ranked list
         of them) but always present, and labelled in --muted so it reads as an
         absence rather than as a place. */
      var unknownLabel = '<span style="color:var(--muted)">Unknown</span>';
      rows += countryRollupRow(unknownLabel, unknown, max, total);

      el.innerHTML =
        '<div style="font-size:0.85rem;color:var(--accent);font-weight:600;margin-bottom:8px">' +
          'Where users are &mdash; ' + countries.length + ' countr' + (countries.length === 1 ? 'y' : 'ies') +
          ', ' + unknown + ' unknown of ' + total +
        '</div>' + rows;
    }

    async function loadUserCountries() {
      try {
        var resp = await fetch('/api/saas/admin/user-countries');
        /* 403 means not admin. Leave the container empty rather than writing an
           error: a non-admin should learn nothing about this surface, not even
           that it exists and refused them. */
        if (!resp.ok) return;
        renderCountryRollup(await resp.json());
      } catch(e) {}
    }

    // --- Admin Users: contact / CRM fields -------------------------------
    // Full name, Slack ID and notes are admin-maintained free text on the user
    // record. They are edited in a per-user panel row rather than inline in the
    // main row: notes is expected to be the longest field and a multi-line box
    // simply does not fit in a table cell next to quota and the action buttons.
    // The main row shows a one-line summary plus an Edit toggle, so the common
    // case (glance at who someone is) costs nothing and the edit case is one
    // click away.

    // Number of columns in the admin users table. Panel/expand rows span the
    // full width, so this must track the <th> count in renderUsers.
    // 9 since the Country column landed.
    var USERS_TABLE_COLSPAN = 9;
    // Rows of the notes textarea. Big enough for a short paragraph without
    // pushing the next user off screen. It is the only free-form prose field,
    // so it gets the height as well as the width; still user-resizable.
    var CONTACT_NOTES_ROWS = 4;
    // Characters of a note shown in the collapsed summary before ellipsis.
    var CONTACT_NOTES_PREVIEW_CHARS = 40;
    // Client-side maxlength on each field. Mirrors the server caps in saas.go
    // (maxContactNameLen / maxContactSlackIDLen / maxContactNotesLen) — the
    // server still truncates; this is only so the admin sees the limit.
    var CONTACT_MAX_NAME = 128;
    var CONTACT_MAX_SLACK = 64;
    var CONTACT_MAX_NOTES = 8192;

    // Field widths in the contact editor panel. The panel is its own full-width
    // row spanning every column (USERS_TABLE_COLSPAN), so widening these does
    // NOT add horizontal pressure to the already-dense main table — the fields
    // only compete with each other for the row's width.
    //
    // Expressed as flex "grow basis" pairs in percent so the row reflows with
    // the viewport instead of locking to pixel widths. The three bases sum to
    // less than 100% so the gap between fields has room; the grow factors then
    // divide the remainder, giving Notes the lion's share because it is
    // free-form prose while the other two are short identifiers.
    var CONTACT_W_NAME_BASIS = '22%';
    var CONTACT_W_NAME_MIN = '190px';
    var CONTACT_W_SLACK_BASIS = '18%';
    var CONTACT_W_SLACK_MIN = '150px';
    // Notes grows ~3x faster than the single-line fields and starts widest.
    var CONTACT_W_NOTES_BASIS = '48%';
    var CONTACT_W_NOTES_MIN = '320px';
    var CONTACT_NOTES_GROW = 3;
    /* Country is a two-letter code plus a live flag preview, so it is the
       narrowest field in the panel and the only one with a fixed-ish basis: it
       can never need more room than "GB  United Kingdom" takes to echo. */
    var CONTACT_W_COUNTRY_BASIS = '12%';
    var CONTACT_W_COUNTRY_MIN = '150px';
    /* ISO 3166-1 alpha-2. Mirrors countryCodeLen in user_country.go — the
       maxlength is a courtesy that stops a third letter being typed at all;
       normalizeCountryCode on both sides is the actual control. */
    var CONTACT_MAX_COUNTRY = 2;
    var CONTACT_MAX_COMPANY = 128;
    var CONTACT_W_COMPANY_BASIS = '18%';
    var CONTACT_W_COMPANY_MIN = '170px';

    /* ISO 3166-1 alpha-2 codes for the Country dropdown. We do NOT ship a
       250-row name table (the reason the field used to be free text): the codes
       are a compact closed list, and countryDisplayName() turns each into its
       localized English name via the browser's Intl.DisplayNames at render
       time — so the <select> options are labeled without a duplicated table and
       stay in step with the same name source the flag/preview already use. */
    var ISO_COUNTRY_CODES = ("AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW").split(" ");

    /* Minimum number of users sharing a country before it floats to the
       "frequent" group at the top of the Country dropdown. "More than one":
       a single assignment is not yet a pattern worth reordering the list for. */
    var COUNTRY_FREQUENT_MIN_USERS = 2;
    /* The disabled divider between the frequent group and the full
       alphabetical list. value-less and disabled, so it can never be picked. */
    var COUNTRY_FREQUENT_SEPARATOR = '──────────';

    // frequentCountryCodes returns the codes held by at least
    // COUNTRY_FREQUENT_MIN_USERS of the given users, ordered by count
    // descending then localized name — the group countrySelectOptionsHTML
    // floats to the top of the dropdown. Empty or malformed countries never
    // count (normalizeCountryCode gates them out), so a roster full of
    // country-less users produces no group at all. Recomputed on every panel
    // render from the roster the admin poll keeps fresh (_allUsers), so the
    // ordering tracks assignments live as countries are added or cleared.
    function frequentCountryCodes(users) {
      var counts = {};
      (users || []).forEach(function (u) {
        var c = normalizeCountryCode(u && u.country);
        if (c) counts[c] = (counts[c] || 0) + 1;
      });
      return Object.keys(counts)
        .filter(function (c) { return counts[c] >= COUNTRY_FREQUENT_MIN_USERS; })
        .map(function (c) { return { code: c, count: counts[c], name: countryDisplayName(c) || c }; })
        .sort(function (a, b) {
          if (a.count !== b.count) return b.count - a.count;
          return a.name.localeCompare(b.name);
        })
        .map(function (o) { return o.code; });
    }

    // countrySelectOptionsHTML builds the <option>s for the Country dropdown,
    // sorted by localized display name, marking the current code selected and
    // a leading blank ("— none —") so an admin can clear a country.
    //
    // Countries already assigned to more than one loaded user float to a
    // "frequent" group right under "— none —" (frequentCountryCodes), followed
    // by a disabled separator and then the FULL alphabetical list — frequent
    // codes stay duplicated there on purpose, so an admin whose muscle memory
    // says "scroll to U" still finds United States where it always was. The
    // current code is marked selected exactly once (the frequent copy when it
    // has one), because duplicate selected attributes would make the browser
    // pick the later, alphabetical copy and scroll the closed select there.
    function countrySelectOptionsHTML(current) {
      var cur = (normalizeCountryCode(current) || '');
      var opts = ISO_COUNTRY_CODES.map(function (code) {
        return { code: code, name: countryDisplayName(code) || code };
      }).sort(function (a, b) { return a.name.localeCompare(b.name); });
      var html = '<option value=""' + (cur ? '' : ' selected') + '>— none —</option>';
      var frequent = frequentCountryCodes(_allUsers);
      var curInFrequent = frequent.indexOf(cur) !== -1;
      frequent.forEach(function (code) {
        html += '<option value="' + escAttr(code) + '"' + (code === cur ? ' selected' : '') +
          '>' + esc(countryDisplayName(code) || code) + ' (' + esc(code) + ')</option>';
      });
      if (frequent.length) {
        html += '<option value="" disabled>' + COUNTRY_FREQUENT_SEPARATOR + '</option>';
      }
      opts.forEach(function (o) {
        html += '<option value="' + escAttr(o.code) + '"' + (o.code === cur && !curInFrequent ? ' selected' : '') +
          '>' + esc(o.name) + ' (' + esc(o.code) + ')</option>';
      });
      return html;
    }

    // companyDatalistOptionsHTML builds the autocomplete suggestions for the
    // Company combobox from the DISTINCT company values already entered across
    // all loaded users — so the list "compiles as you enter them" without any
    // server-side vocabulary. _allUsers is the admin roster the Users table
    // already holds. The field stays a free-text <input list=…>, so a brand-new
    // company can always be typed and becomes a suggestion on the next render.
    function companyDatalistOptionsHTML() {
      var seen = {};
      (_allUsers || []).forEach(function (u) {
        var c = (u && u.company ? String(u.company).trim() : '');
        if (c) seen[c] = true;
      });
      return Object.keys(seen).sort(function (a, b) { return a.localeCompare(b); })
        .map(function (c) { return '<option value="' + escAttr(c) + '"></option>'; }).join('');
    }

    // Which users currently have their contact panel open, keyed by username.
    // Re-rendering on the admin poll must not slam a panel shut mid-edit.
    var _contactExpandedUsers = {};

    // --- Protecting in-progress contact edits from the admin poll ---------
    // renderUsers() rebuilds the whole users table via innerHTML, which
    // destroys and recreates every contact input. The contact fields save on
    // blur, so a value that has been typed but not yet blurred lives ONLY in
    // the DOM node the re-render is about to throw away — losing it is real
    // data loss, not a cosmetic reflow.
    //
    // Approach: defer the render while an edit is in progress, rather than
    // trying to restore focus/caret/selection afterwards. Caret restoration
    // across an innerHTML swap is fiddly and fails in exactly the cases that
    // matter (IME composition, native undo history, a textarea scrolled
    // mid-note), and it cannot restore the browser's undo stack at all. The
    // users table is admin-only, low-churn data; postponing a refresh for a
    // few seconds while someone types costs nothing.
    //
    // "In progress" deliberately means anywhere in the table, not just the
    // focused node: an admin who typed a name, tabbed to Slack ID and is
    // still typing has an unsaved name too, and yanking the row out from
    // under them loses it just the same.

    // How long after the last contact keystroke/blur the table is still
    // considered "being edited". Long enough to cover thinking-pauses while
    // composing a note, short enough that the table is not stale for long
    // after the admin walks away mid-edit.
    var CONTACT_EDIT_QUIET_MS = 5000;
    // How often a deferred render re-checks whether editing has finished.
    // Also the ceiling on how late a deferred render can be beyond the quiet
    // window above.
    var CONTACT_DEFER_RECHECK_MS = 1000;

    // Timestamp of the most recent contact-field interaction, and the handle
    // of the pending re-check timer (null when no render is deferred).
    var _contactLastEditAt = 0;
    var _contactDeferTimer = null;
    // Set while a deferred render is waiting. The poll keeps updating _allUsers
    // regardless; this only gates the DOM write, so the deferred render always
    // paints the newest data rather than a stale snapshot.
    var _contactRenderPending = false;

    // Per-field dirty values: what the admin has typed but not yet saved,
    // keyed by "username::field". Survives a render so that if one does slip
    // through, the typed text is re-applied rather than lost. Multiple users'
    // fields can be dirty at once (type a note, click into another user's
    // Slack ID without blurring back), so this is a map, not a single slot.
    var _contactDirty = {};

    function contactDirtyKey(username, field) { return username + '::' + field; }

    // markContactEditing records activity so the poll backs off.
    function markContactEditing() { _contactLastEditAt = Date.now(); }

    // contactEditInProgress is true when the table must not be rebuilt:
    // either a contact control currently has focus, or something is typed and
    // unsaved, or a keystroke landed within the quiet window.
    function contactEditInProgress() {
      var container = document.getElementById('users-container');
      if (container) {
        var active = document.activeElement;
        if (active && active.getAttribute && active.getAttribute('data-contact-field') &&
            container.contains(active)) {
          return true;
        }
      }
      for (var k in _contactDirty) {
        if (Object.prototype.hasOwnProperty.call(_contactDirty, k)) return true;
      }
      return (Date.now() - _contactLastEditAt) < CONTACT_EDIT_QUIET_MS;
    }

    // contactPanelId maps a username to a DOM id. Usernames are GitHub logins
    // (alphanumerics and hyphens) but this is defensive: anything outside that
    // set is replaced so a stored username can never inject markup through an
    // id attribute or break querySelector.
    function contactPanelId(username) {
      return 'contact-panel-' + String(username || '').replace(/[^A-Za-z0-9_-]/g, '_');
    }

    /* Same sanitizing rule as contactPanelId, for the country field's live
       preview node. Separate id per user because every panel row is emitted for
       every user up front (hidden until expanded), so a shared id would collide
       across hundreds of rows and every lookup would find the first one. */
    function contactCountryPreviewId(username) {
      return 'contact-country-preview-' + String(username || '').replace(/[^A-Za-z0-9_-]/g, '_');
    }

    /* countryPreviewHTML: what a typed code resolves to, as flag + English name.

       Reuses countryFlagEmoji / countryDisplayName from the shared block above
       rather than restating them, so the admin editor and the self-service
       editor can never disagree about what a code means.

       Three distinct states, deliberately worded so none of them reads as an
       error the admin caused:
         blank   -> "No flag" (a legitimate value: clearing the country)
         partial -> "Two-letter code" (still typing; not a failure)
         valid   -> the flag and the country's name

       The code is escaped even though normalizeCountryCode has already reduced
       it to [A-Z]{2}, and the name is escaped because Intl.DisplayNames returns
       a localized string this render path does not own. */
    function countryPreviewHTML(raw) {
      var typed = String(raw == null ? '' : raw).trim();
      if (!typed) return 'No flag';
      var c = normalizeCountryCode(typed);
      if (!c) return 'Two-letter code';
      return countryFlagEmoji(c) + ' ' + esc(countryDisplayName(c));
    }

    /* refreshContactCountryPreview re-renders one user's preview from whatever
       is currently in their input. Called on every keystroke, so it reads the
       DOM rather than _allUsers — the point is to reflect the UNSAVED text. */
    function refreshContactCountryPreview(username, value) {
      var el = document.getElementById(contactCountryPreviewId(username));
      if (!el) return;
      el.innerHTML = countryPreviewHTML(value);
    }

    /* providerBadge renders the user's login method (auth provider) as a small
       chip next to their name in the admin Users table, so an admin can see at a
       glance who signed in with GitHub vs Google vs IBMid vs Red Hat vs
       Microsoft. The provider rides the users payload as u.provider (derived
       server-side via userProvider). A legacy record with no provider resolves
       to github, so every existing row shows the GitHub chip — no blank. */
    // providerLogoSVG returns a small inline brand SVG per login provider, matching
    // the marks used on the /login picker, so the badge is recognizable at a glance.
    function providerLogoSVG(p) {
      var s = 'width="12" height="12" style="flex:none" aria-hidden="true"';
      switch (p) {
        case 'github':
          return '<svg viewBox="0 0 16 16" fill="currentColor" ' + s + '><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z"/></svg>';
        case 'google':
          return '<svg viewBox="0 0 18 18" ' + s + '><path fill="#4285F4" d="M17.64 9.2c0-.64-.06-1.25-.16-1.84H9v3.48h4.84a4.14 4.14 0 0 1-1.8 2.72v2.26h2.92c1.7-1.57 2.68-3.88 2.68-6.62z"/><path fill="#34A853" d="M9 18c2.43 0 4.47-.8 5.96-2.18l-2.92-2.26c-.8.54-1.84.86-3.04.86-2.34 0-4.32-1.58-5.03-3.7H.96v2.33A9 9 0 0 0 9 18z"/><path fill="#FBBC05" d="M3.97 10.72a5.4 5.4 0 0 1 0-3.44V4.95H.96a9 9 0 0 0 0 8.1l3.01-2.33z"/><path fill="#EA4335" d="M9 3.58c1.32 0 2.5.45 3.44 1.35l2.58-2.58C13.46.89 11.42 0 9 0A9 9 0 0 0 .96 4.95l3.01 2.33C4.68 5.16 6.66 3.58 9 3.58z"/></svg>';
        case 'ibmid':
          // Text-based IBM mark (license-safe; the striped-wordmark SVG rendered
          // as a broken glyph at badge size). IBM blue, bold, sized to match the
          // other 12px brand marks.
          return '<svg viewBox="0 0 26 14" width="20" height="12" style="flex:none" aria-hidden="true">' +
            '<text x="13" y="11" text-anchor="middle" font-family="system-ui,-apple-system,sans-serif" ' +
            'font-size="11" font-weight="700" fill="#0f62fe">IBM</text></svg>';
        case 'redhat':
          return '<svg viewBox="0 0 24 24" fill="#EE0000" ' + s + '><path d="M16.35 14.4c1.6 0 3.9-.33 3.9-2.24a1.8 1.8 0 0 0-.04-.44l-.95-4.12c-.22-.9-.41-1.31-2-2.12-1.24-.63-3.94-1.67-4.74-1.67-.74 0-.96.96-1.85.94-.86-.02-1.5-.74-2.3-.74-.77 0-1.27.52-1.66 1.6 0 0-1.08 3.05-1.22 3.49a.83.83 0 0 0-.03.25c0 1.2 4.71 5.32 10.88 5.32M20.47 12.94c.22 1.05.22 1.16.22 1.3 0 1.8-2.02 2.79-4.67 2.79-6 0-11.25-3.51-11.25-5.83 0-.32.07-.63.18-.93C2.94 10.36 1 10.63 1 12.34c0 2.8 6.63 6.26 11.87 6.26 4.02 0 5.03-1.82 5.03-3.25 0-1.13-.97-2.4-2.43-2.41"/></svg>';
        case 'microsoft':
          return '<svg viewBox="0 0 23 23" ' + s + '><path fill="#F25022" d="M0 0h11v11H0z"/><path fill="#7FBA00" d="M12 0h11v11H12z"/><path fill="#00A4EF" d="M0 12h11v11H0z"/><path fill="#FFB900" d="M12 12h11v11H12z"/></svg>';
        default:
          return '';
      }
    }

    function providerBadge(u) {
      var p = String((u && u.provider) || 'github').toLowerCase();
      var meta = {
        github: {label: 'GitHub', color: '#c9d1d9', bg: 'rgba(110,118,129,0.25)'},
        google: {label: 'Google', color: '#e8eaed', bg: 'rgba(66,133,244,0.22)'},
        ibmid:  {label: 'IBMid',  color: '#e8eaed', bg: 'rgba(15,98,254,0.22)'},
        redhat: {label: 'Red Hat',color: '#f5c2c7', bg: 'rgba(238,0,0,0.22)'},
        microsoft: {label: 'Microsoft', color: '#e8eaed', bg: 'rgba(0,120,215,0.22)'}
      }[p] || {label: p || 'unknown', color: 'var(--muted)', bg: 'rgba(110,118,129,0.25)'};
      var logo = providerLogoSVG(p);
      return '<span title="Signed in with ' + escAttr(meta.label) + '"' +
        ' style="display:inline-flex;align-items:center;gap:4px;padding:1px 7px;border-radius:9999px;' +
        'font-size:0.6rem;font-weight:600;color:' + meta.color + ';background:' + meta.bg + '">' +
        (logo ? logo : '') + esc(meta.label) + '</span>';
    }

    /* countryCell renders the user's country in the admin Users table as
       flag + alpha-2 code, following the providerBadge chip above rather than
       inventing a second visual language for a per-row attribute: same
       inline-flex chip, same 4px gap, same small type.

       Unknown or unset renders NOTHING — an empty cell, no globe, no dash, no
       "unknown" chip. That is #4371's rule and it matters most here, where a
       column of placeholders would read as data we have. The rollup below the
       table is where the size of the unknown bucket is stated explicitly; the
       row is not the place to repeat it 200 times.

       countryFlagHTML already normalizes and escapes; the code is escaped again
       for the visible text because the render path must not depend on the
       validator upstream of it staying correct. Color is the --muted theme
       token so the code recedes next to the name in both light and dark. */
    function countryCell(u) {
      var code = normalizeCountryCode(u && u.country);
      if (!code) return '';
      return '<span style="display:inline-flex;align-items:center;gap:4px;font-size:0.7rem;color:var(--muted)">' +
        countryFlagHTML(code) + esc(code) + '</span>';
    }

    // renderContactCell is the collapsed summary shown in the main user row.
    function renderContactCell(u) {
      var bits = [];
      if (u.full_name) bits.push('<span style="font-size:0.78rem">' + esc(u.full_name) + '</span>');
      if (u.company) bits.push('<span style="font-size:0.7rem;color:var(--muted)">' + esc(u.company) + '</span>');
      if (u.slack_id) bits.push('<span style="font-size:0.7rem;color:var(--muted)">slack: ' + esc(u.slack_id) + '</span>');
      if (u.notes) {
        var preview = u.notes.length > CONTACT_NOTES_PREVIEW_CHARS
          ? u.notes.substring(0, CONTACT_NOTES_PREVIEW_CHARS) + '…' : u.notes;
        // title= carries the full note on hover, so it needs attribute escaping.
        bits.push('<span title="' + escAttr(u.notes) + '" style="font-size:0.7rem;color:var(--muted);font-style:italic">' + esc(preview) + '</span>');
      }
      // align-items:flex-start + text-align:left keep the contact lines
      // left-justified. Without them the cell inherits the users table's
      // centred alignment, so name/slack/notes rendered ragged-centre — each
      // line a different width, which is hard to scan down a column.
      var summary = bits.length
        ? '<div style="display:flex;flex-direction:column;align-items:flex-start;text-align:left;gap:1px;max-width:220px;overflow:hidden">' + bits.join('') + '</div>'
        : '<span style="color:var(--muted);font-size:0.72rem">—</span>';
      var open = !!(_contactExpandedUsers && _contactExpandedUsers[u.github_username]);
      var btn = '<button type="button" data-contact-toggle="' + escAttr(u.github_username) + '"' +
        ' id="' + contactToggleId(u.github_username) + '"' +
        ' aria-controls="' + contactPanelId(u.github_username) + '"' +
        ' aria-expanded="' + (open ? 'true' : 'false') + '"' +
        ' aria-label="' + escAttr(contactToggleAriaLabel(u.github_username, open)) + '"' +
        ' style="' + contactToggleStyle(open) + '">' + contactToggleLabel(open) + '</button>';
      return summary + btn;
    }

    /* --- The Edit/Save toggle ---
       The button in the CONTACT column is the operator's primary affordance:
       "Edit" while the panel is shut, "Save" while it is open. Clicking Save
       commits every pending field and closes the panel.

       Save is styled as the primary action (accent border and text) so the eye
       lands on it; the × and the Close button inside the panel remain the
       escape hatches, and Escape still works. Making the row button the commit
       is what gives an explicit save point WITHOUT fighting the per-field blur
       saves: blur keeps storing silently as the admin tabs around, and this
       button is the only thing that closes on success. */

    /* Style is derived from state in one place so the initial markup and the
       live in-place update below can never drift apart. */
    function contactToggleStyle(open) {
      var color = open ? 'var(--accent)' : 'var(--muted)';
      return 'margin-top:2px;padding:1px 7px;background:none;border:1px solid ' + color +
        ';border-radius:4px;color:' + color + ';cursor:pointer;font-size:0.65rem' +
        (open ? ';font-weight:600' : '');
    }

    function contactToggleLabel(open) { return open ? 'Save' : 'Edit'; }

    /* The accessible name carries the username as well as the verb, because a
       screen-reader user tabbing a table of ~90 identical "Save" buttons needs
       to know which row they are on. */
    function contactToggleAriaLabel(username, open) {
      return (open ? 'Save contact details for ' : 'Edit contact details for ') + String(username || '');
    }

    function contactToggleId(username) {
      return 'contact-toggle-' + String(username || '').replace(/[^A-Za-z0-9_-]/g, '_');
    }

    /* refreshContactToggleLabel re-derives the button from _contactExpandedUsers
       and writes label, style, aria-expanded and the accessible name TOGETHER.
       Updating the visible text without the aria state would leave a screen
       reader announcing "Edit, collapsed" over an open editor. */
    function refreshContactToggleLabel(username) {
      if (!username) return;
      var btn = document.getElementById(contactToggleId(username));
      if (!btn) return;
      var open = !!(_contactExpandedUsers && _contactExpandedUsers[username]);
      btn.textContent = contactToggleLabel(open);
      btn.setAttribute('aria-expanded', open ? 'true' : 'false');
      btn.setAttribute('aria-label', contactToggleAriaLabel(username, open));
      btn.setAttribute('style', contactToggleStyle(open));
    }

    // renderContactPanelRow is the expandable editor row that follows each user
    // row. It is always emitted (hidden unless expanded) so toggling is a
    // display flip rather than a re-render, which keeps focus and caret intact.
    function renderContactPanelRow(u) {
      var open = !!(_contactExpandedUsers && _contactExpandedUsers[u.github_username]);
      var fld = 'padding:5px 7px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);font-size:0.78rem;box-sizing:border-box;width:100%';
      var lbl = 'display:block;font-size:0.66rem;color:var(--muted);margin-bottom:3px;text-transform:uppercase;letter-spacing:0.04em';
      var user = escAttr(u.github_username);
      return '<tr id="' + contactPanelId(u.github_username) + '" style="display:' + (open ? '' : 'none') + '">' +
        /* position:relative anchors the absolutely-positioned × below. */
        '<td class="contact-panel-cell" colspan="' + USERS_TABLE_COLSPAN + '" style="background:var(--surface);position:relative">' +
        '<div style="padding:10px 14px 12px 40px;display:flex;gap:14px;flex-wrap:wrap;align-items:flex-start">' +
          '<div style="flex:1 1 ' + CONTACT_W_NAME_BASIS + ';min-width:' + CONTACT_W_NAME_MIN + '">' +
            '<label style="' + lbl + '">Full name</label>' +
            '<input type="text" data-contact-user="' + user + '" data-contact-field="full_name"' +
              ' maxlength="' + CONTACT_MAX_NAME + '" value="' + escAttr(u.full_name || '') + '" style="' + fld + '">' +
          '</div>' +
          '<div style="flex:1 1 ' + CONTACT_W_SLACK_BASIS + ';min-width:' + CONTACT_W_SLACK_MIN + '">' +
            '<label style="' + lbl + '">Slack ID</label>' +
            '<input type="text" data-contact-user="' + user + '" data-contact-field="slack_id"' +
              ' maxlength="' + CONTACT_MAX_SLACK + '" value="' + escAttr(u.slack_id || '') + '" style="' + fld + '">' +
          '</div>' +
          /* Company. Admin-entered CRM free text, NOT collected in the hive
             request form (operator fills it manually). A combobox: a plain
             <input> backed by a <datalist> of the company names already entered
             across users (companyDatalistOptionsHTML), so it autocompletes from
             the growing set as they are entered while still accepting a new one. */
          '<div style="flex:1 1 ' + CONTACT_W_COMPANY_BASIS + ';min-width:' + CONTACT_W_COMPANY_MIN + '">' +
            '<label style="' + lbl + '">Company</label>' +
            '<input type="text" data-contact-user="' + user + '" data-contact-field="company"' +
              ' list="contact-company-suggestions"' +
              ' maxlength="' + CONTACT_MAX_COMPANY + '" value="' + escAttr(u.company || '') + '" style="' + fld + '">' +
          '</div>' +
          /* Country. An ADMIN ASSIGNMENT on the user's behalf (recorded as
             CountrySource "admin"). Now a <select> of ISO 3166-1 codes labeled
             with their localized names via Intl.DisplayNames — no duplicated
             250-row table (countrySelectOptionsHTML), a blank "— none —" clears
             it. The live preview still echoes the flag + name for the current
             selection so the row's Country column agrees on open. */
          '<div style="flex:1 1 ' + CONTACT_W_COUNTRY_BASIS + ';min-width:' + CONTACT_W_COUNTRY_MIN + '">' +
            '<label style="' + lbl + '">Country</label>' +
            '<select data-contact-user="' + user + '" data-contact-field="country"' +
              ' aria-describedby="' + contactCountryPreviewId(u.github_username) + '"' +
              ' style="' + fld + '">' + countrySelectOptionsHTML(u.country) + '</select>' +
            '<div id="' + contactCountryPreviewId(u.github_username) + '"' +
              ' style="margin-top:4px;font-size:0.66rem;color:var(--muted);min-height:1.1em">' +
              countryPreviewHTML(u.country) + '</div>' +
          '</div>' +
          '<div style="flex:' + CONTACT_NOTES_GROW + ' 1 ' + CONTACT_W_NOTES_BASIS + ';min-width:' + CONTACT_W_NOTES_MIN + '">' +
            '<label style="' + lbl + '">Notes</label>' +
            '<textarea data-contact-user="' + user + '" data-contact-field="notes" rows="' + CONTACT_NOTES_ROWS + '"' +
              ' maxlength="' + CONTACT_MAX_NOTES + '" style="' + fld + ';resize:vertical;font-family:inherit">' + esc(u.notes || '') + '</textarea>' +
          '</div>' +
          '<div style="align-self:flex-end;display:flex;align-items:center;gap:10px;padding-bottom:4px">' +
            '<span style="font-size:0.65rem;color:var(--muted)">Save closes &middot; Esc cancels</span>' +
            '<button type="button" data-contact-close="' + user + '"' +
              ' style="padding:4px 12px;background:var(--bg);border:1px solid var(--border);border-radius:4px;' +
              'color:var(--muted);cursor:pointer;font-size:0.7rem">Cancel</button>' +
          '</div>' +
        '</div>' +
        /* The × sits top-right of the panel, the conventional place to look for
           a dismiss control, and duplicates the Close button so the affordance
           is visible without reading to the end of a wide row. */
        '<button type="button" data-contact-close="' + user + '" aria-label="Close editor for ' + user + '"' +
          ' style="position:absolute;top:6px;right:10px;background:none;border:none;color:var(--muted);' +
          'cursor:pointer;font-size:1rem;line-height:1;padding:2px 6px">&#10005;</button>' +
        '</td></tr>';
    }

    /* contactPanelHasUnsavedEdits reports whether anything in this user's panel
       has been typed but not yet handed to the save path. The fields save on
       blur, so text that has never been blurred lives ONLY in the DOM node —
       closing the panel without a warning would be real data loss. */
    function contactPanelHasUnsavedEdits(username) {
      if (!username) return false;
      for (var k in _contactDirty) {
        if (Object.prototype.hasOwnProperty.call(_contactDirty, k) &&
            k.indexOf(username + '::') === 0) {
          return true;
        }
      }
      return false;
    }

    /* closeContactPanel is the single exit for the editor: the ×, the Close
       button, Escape, and a successful save all funnel through here.

       Unsaved text is committed rather than discarded when the admin confirms:
       blur normally saves, but Escape and the × can fire while a field still
       has focus and has never blurred, so the value would otherwise be lost. */
    async function closeContactPanel(username, opts) {
      if (!username) return;
      var force = !!(opts && opts.force);
      if (!force && contactPanelHasUnsavedEdits(username)) {
        var keep = await hiveConfirm('You have unsaved changes for ' + username +
          '. Save them and close?');
        if (!keep) return;
        /* Save first, and close only if the save actually landed. A failed
           save keeps the editor open with the typed text still in the fields
           so the admin can read the toast, fix the cause and retry — closing
           here would discard the very edit the server just rejected. */
        var saved = await commitContactPanelEdits(username);
        if (!saved) return;
      }
      setContactPanelOpen(username, false);
    }

    /* commitContactPanelEdits flushes every dirty field for one user through
       the normal save path and resolves true only when every one succeeded.
       saveContactField is a no-op for unchanged values, so this is safe to
       call broadly. Dirty marks are cleared only for fields that saved, so a
       rejected field stays dirty and the poll keeps backing off rather than
       rebuilding the table over text the admin has not managed to store. */
    async function commitContactPanelEdits(username) {
      var prefix = username + '::';
      var keys = Object.keys(_contactDirty || {}).filter(function(k) {
        return k.indexOf(prefix) === 0;
      });
      /* Also flush whatever is currently in the panel's inputs. The commit can
         be triggered by a click on Save while a field still holds focus and has
         never blurred, so its text may not be in _contactDirty yet. */
      var panel = document.getElementById(contactPanelId(username));
      if (panel) {
        var live = panel.querySelectorAll('[data-contact-field]');
        Array.prototype.forEach.call(live || [], function(el) {
          var f = el.getAttribute('data-contact-field');
          if (!f) return;
          var k = prefix + f;
          _contactDirty[k] = el.value;
          if (keys.indexOf(k) < 0) keys.push(k);
        });
      }
      /* Saves are sequential, not Promise.all: the handler is a read-modify-write
         over one JSON file per user, so three concurrent PUTs for the same user
         can interleave and lose a field. */
      var allOK = true;
      for (var i = 0; i < (keys || []).length; i++) {
        var k = keys[i];
        var field = k.substring(prefix.length);
        /* Not silent: this IS the explicit commit, so a failure here is exactly
           what the admin needs to see. */
        var ok = await saveContactField(username, field, _contactDirty[k]);
        if (ok) { delete _contactDirty[k]; } else { allOK = false; }
      }
      /* A field can be clean here yet still have failed earlier during a silent
         blur-save. Surface that recorded reason rather than closing on what
         would look like a no-op success. */
      if (allOK && _contactLastError[username]) {
        hiveToast('Could not save ' + username + ': ' + _contactLastError[username], 'error');
        return false;
      }
      return allOK;
    }

    /* openContactPanelUsername returns the username of the contact editor that
       currently owns focus, or '' when focus is elsewhere. Escape must only
       close the editor the admin is actually working in — a stray Escape with
       focus on the page body should fall through to the global handler that
       closes create-modal and friends (#2321). */
    function openContactPanelUsername() {
      var active = document.activeElement;
      if (!active || !active.closest) return '';
      var cell = active.closest('.contact-panel-cell');
      if (!cell) return '';
      var field = cell.querySelector('[data-contact-user]');
      return field ? (field.getAttribute('data-contact-user') || '') : '';
    }

    /* Escape-to-close for the contact editor. Registered in the CAPTURE phase
       so it can decide before the global Escape handler (#2321) runs, and it
       stops propagation ONLY when it actually closed an editor — otherwise the
       global handler keeps its existing behaviour for create-modal, the
       request modal, the confirm overlay and the timeline/access modals. */
    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Escape') return;
      /* hiveConfirm puts a modal overlay on top and binds its own Escape; while
         one is up it owns the key, so never steal it. */
      if (document.querySelector('.hive-confirm-overlay')) return;
      var username = openContactPanelUsername();
      if (!username) return;
      e.stopPropagation();
      e.preventDefault();
      closeContactPanel(username);
    }, true);

    // bindContactPanels attaches listeners after renderUsers writes the table.
    // Listeners rather than inline on* attributes: esc() leaves apostrophes
    // alone, so a value like O'Brien inside onchange="save('...')" would break
    // the handler (and be an injection vector). Nothing user-controlled is ever
    // interpolated into executable markup here.
    function bindContactPanels() {
      var container = document.getElementById('users-container');
      if (!container) return;
      var toggles = container.querySelectorAll('[data-contact-toggle]');
      Array.prototype.forEach.call(toggles || [], function(btn) {
        btn.addEventListener('click', function() {
          toggleContactPanel(btn.getAttribute('data-contact-toggle'));
        });
      });
      /* The × and the Close button share data-contact-close, so one binding
         covers both. Bound here rather than as inline on* attributes for the
         same reason as the toggles: nothing user-controlled is interpolated
         into executable markup. */
      var closers = container.querySelectorAll('[data-contact-close]');
      Array.prototype.forEach.call(closers || [], function(btn) {
        btn.addEventListener('click', function() {
          closeContactPanel(btn.getAttribute('data-contact-close'));
        });
      });
      var fields = container.querySelectorAll('[data-contact-field]');
      Array.prototype.forEach.call(fields || [], function(el) {
        var user = el.getAttribute('data-contact-user');
        var field = el.getAttribute('data-contact-field');
        var key = contactDirtyKey(user, field);
        // Re-apply anything typed but not yet saved. Belt-and-braces: the
        // render gate should mean we never rebuild mid-edit, but if a render
        // does land (e.g. one already in flight when typing began), the typed
        // text is restored instead of silently reverting to the server value.
        if (Object.prototype.hasOwnProperty.call(_contactDirty, key)) {
          el.value = _contactDirty[key];
        }
        // Any keystroke marks the table as being edited and records the
        // in-progress value, so the poll backs off and nothing typed is only
        // ever held in a DOM node that is about to be replaced.
        var onEdit = function() {
          markContactEditing();
          _contactDirty[key] = el.value;
          /* Country is the one field whose stored form (two letters) does not
             say what it means, so it echoes as you type/select. The other
             fields are their own preview. */
          if (field === 'country') refreshContactCountryPreview(user, el.value);
        };
        el.addEventListener('input', onEdit);
        // A <select> (the Country dropdown) fires 'change', not always 'input'.
        // Also save on change so a dropdown pick commits without needing a blur.
        el.addEventListener('change', function() {
          onEdit();
          if (el.tagName === 'SELECT') {
            var pending = el.value;
            saveContactField(user, field, pending, {silent: true}).then(function(ok) {
              if (ok && _contactDirty[key] === pending) delete _contactDirty[key];
              refreshContactToggleLabel(user);
            });
          }
        });
        // focus/blur also count as activity: tabbing between fields must not
        // leave a gap the poll can render into.
        el.addEventListener('focus', markContactEditing);
        // Save on blur so an admin can tab between fields and type freely
        // without a request per keystroke.
        //
        // Blur-saves are SILENT: no toast on success, and on failure the value
        // stays dirty so the field keeps the typed text and the Save button
        // keeps reading "Save". Blur fires constantly (tabbing between the
        // three fields, clicking the Save button itself), so a toast per blur
        // would be noise, and a blur-triggered error would fire while the
        // admin is mid-sentence in the next field. The Save button is the
        // explicit commit point where failures are reported — see
        // commitContactPanelEdits.
        el.addEventListener('blur', function() {
          markContactEditing();
          // Clear dirty only once the save has actually landed, so there is no
          // window where the value is neither dirty nor stored. A rejected
          // blur-save therefore leaves the panel dirty and Save still armed.
          var pending = el.value;
          saveContactField(user, field, pending, {silent: true}).then(function(ok) {
            if (ok && _contactDirty[key] === pending) delete _contactDirty[key];
            refreshContactToggleLabel(user);
          });
        });
      });
    }

    /* The row button is stateful: "Edit" when shut, "Save" when open.

       Shut  -> open the panel (a plain display flip, which is what keeps focus
                and caret intact) and relabel to Save.
       Open  -> this IS the Save action: commit every pending field, and close
                only if the commit succeeded. A failure keeps the panel open,
                keeps the label on Save, and lets the error toast stand — with
                a visible Save button, a false success is the worst outcome, so
                nothing here closes on an unverified save.

       Note this deliberately does NOT go through closeContactPanel: that path
       is for the escape hatches (×, Close, Escape), where the right question is
       "you have unsaved changes, save them?". Pressing Save has already
       answered that question. */
    async function toggleContactPanel(username) {
      if (!username) return;
      if (_contactExpandedUsers[username]) {
        var saved = await commitContactPanelEdits(username);
        if (!saved) return;
        setContactPanelOpen(username, false);
        return;
      }
      setContactPanelOpen(username, true);
    }

    /* setContactPanelOpen is the ONLY place panel visibility changes, so the
       row/panel display and the button's label + aria state can never fall out
       of step with _contactExpandedUsers. */
    function setContactPanelOpen(username, open) {
      _contactExpandedUsers[username] = !!open;
      var row = document.getElementById(contactPanelId(username));
      if (row) row.style.display = open ? '' : 'none';
      refreshContactToggleLabel(username);
    }

    // Last value saved per user+field, so a blur that changed nothing (tabbing
    // through, or a re-render restoring focus) does not fire a pointless PUT.
    var _contactLastSaved = {};

    // Always returns a Promise<boolean> so callers can sequence on the outcome;
    // a no-op resolves true because "nothing to do" is not a failure.
    // opts.silent suppresses the failure toast, for blur-saves where the Save
    // button is the reporting point. The boolean result is unaffected.
    function saveContactField(username, field, value, opts) {
      if (!username || !field) return Promise.resolve(true);
      var key = username + '::' + field;
      var current = _allUsers ? (_allUsers.find(function(x) { return x.github_username === username; }) || {}) : {};
      var previous = (_contactLastSaved[key] !== undefined) ? _contactLastSaved[key] : (current[field] || '');
      var next = (value || '').trim();
      /* Country is stored upper-cased by the server, so normalize it to the
         SAME form here before the equality check. Without this, re-opening the
         panel and typing "gb" over a stored "GB" would read as a change and fire
         a pointless PUT, and the optimistic cache below would hold a value the
         server never wrote. A half-typed or invalid code is left alone rather
         than blanked, so the field still holds what was typed while the server
         does the actual rejecting. */
      if (field === 'country') {
        var norm = normalizeCountryCode(next);
        if (norm) next = norm;
      }
      if (next === previous) return Promise.resolve(true);
      _contactLastSaved[key] = next;
      // Keep the in-memory copy in step so the next poll's signature check does
      // not treat our own edit as an external change and re-render mid-typing.
      if (current) current[field] = next;
      var payload = {};
      payload[field] = next;
      // On failure, roll the optimistic bookkeeping back to the previous value.
      // Without this the cache claims the value was stored, so the
      // next-equals-previous short-circuit above would skip the retry of the very
      // same value — the edit could never be saved again without a reload.
      return updateUser(username, payload, opts).then(function(ok) {
        if (!ok) {
          _contactLastSaved[key] = previous;
          if (current) current[field] = previous;
        }
        return ok;
      });
    }

    // scheduleDeferredUsersRender keeps re-checking until editing has stopped
    // and then renders. This is what guarantees a deferred refresh actually
    // happens: the timer re-arms itself on every check that still sees an edit
    // in progress, so the only way out of the loop is an actual render. It is
    // never cleared without rendering, and only one timer is ever in flight.
    function scheduleDeferredUsersRender() {
      _contactRenderPending = true;
      if (_contactDeferTimer) return;
      _contactDeferTimer = setTimeout(function() {
        _contactDeferTimer = null;
        if (contactEditInProgress()) { scheduleDeferredUsersRender(); return; }
        _contactRenderPending = false;
        // Re-derive from the latest _allUsers rather than replaying the
        // snapshot that was deferred, so the render that finally lands shows
        // current data and not whatever the poll saw minutes ago.
        try { applySortUsers(); } catch (e) { console.error('deferred renderUsers error:', e); }
      }, CONTACT_DEFER_RECHECK_MS);
    }

    /* ── User engagement / lifecycle card ────────────────────────────────
       Shown on hover of a user's name in the admin Users table. It exists to
       answer one operator question — "what should I DO with this person?" — from
       real signals rather than a single last-login date:
         • ELEVATE   — logging in, real time in their hive, tasks landing, and a
                       hive resting at its ACMM level (ready to graduate).
         • NEEDS HELP— logs in but stuck on an early journey stage or barely any
                       activity; a journey nudge is the lever.
         • AT RISK   — assigned a hive yet ~no logins, ~no hive time, no tasks, or
                       a hive already at deprovision-warning; pressure / reclaim.
       The verdict is ADVISORY: it classifies and points at existing levers (ACMM,
       journey snooze/nudge, de-provision) — it changes no state. All inputs are
       admin-only (this table is requireAdmin, and leaderboard is scrubbed for
       non-admins server-side). */
    var USERSTAT_ACTIVE_LOGIN_MIN = 3;        // legacy fallback: logins that count as "engaged"
    var USERSTAT_ACTIVE_HOURS_MIN = 1;        // hive-hours (engaged when available) that count as "engaged"
    var USERSTAT_SECS_PER_HOUR = 3600;
    var USERSTAT_ACTIVE_WINDOW_MS = 7 * 24 * 3600 * 1000;  // mirrors engagementActiveWindow (7 days) hub-side
    var USERSTAT_DORMANT_HOURS_MAX = 0.25;    // under this much hive time still reads as "basically none"

    /* Status tier presentation. The tier ITSELF (u.status_tier) is computed
       hub-side by userStatusTier from the honest signals - last audited real
       action, focus-engaged presence, live/engaged-now sets - so this map only
       styles and explains it. Keys mirror the Go tier constants. */
    var USER_TIER_META = {
      live:    {color: 'var(--green)', weight: 700, tip: 'Connected right now AND engaged: tab focused with input in the last minute.'},
      active:  {color: 'var(--green)', weight: 600, tip: 'A real audited action or focus-engaged time within the last 7 days.'},
      idle:    {color: 'var(--amber)', weight: 600, tip: 'Has logins/sessions - often just an open tab - but nothing real within the last 7 days.'},
      dormant: {color: 'var(--muted)', weight: 600, tip: 'No signal at all (no login, session, or action) within the last 30 days.'},
      never:   {color: 'var(--muted)', weight: 400, tip: 'Never completed a hub login.'},
      blocked: {color: 'var(--red)',   weight: 600, tip: 'Blocked by an admin - cannot sign in. Unblock to restore access.'}
    };
    /* Sort order for the Status column: most engaged first on ascending. */
    var USER_TIER_RANK = {live: 0, active: 1, idle: 2, dormant: 3, never: 4, blocked: 5};

    /* userTier resolves a user's tier with a fallback for a payload that
       predates status_tier (or a mid-rollout hub): blocked stays blocked and
       everyone else reads as idle - the honest default when we cannot prove
       engagement - never as "active". */
    function userTier(u) {
      return u.status_tier || (u.blocked ? 'blocked' : 'idle');
    }
    function userTierRank(u) {
      var r = USER_TIER_RANK[userTier(u)];
      return (r == null) ? USER_TIER_RANK.idle : r;
    }
    function statusTierBadge(u) {
      var tier = userTier(u);
      var meta = USER_TIER_META[tier] || USER_TIER_META.idle;
      var label = (tier === 'blocked') ? 'BLOCKED' : tier;
      return '<span title="' + escAttr(meta.tip) + '" style="color:' + meta.color +
        ';font-weight:' + meta.weight + ';cursor:help">' + esc(label) + '</span>';
    }

    /* userTaskActivity sums this user's per-hive leaderboard entries across every
       hive in _hiveRegistry, matching on github_username. leaderboard is present
       only for an admin (scrubbed otherwise), so a non-admin simply gets zeros. */
    function userTaskActivity(username) {
      var done = 0, failed = 0, activeNow = false;
      (_hiveRegistry || []).forEach(function(h) {
        (h.leaderboard || []).forEach(function(e) {
          if (e && e.github_username === username) {
            done += (e.tasksCompleted || e.TasksCompleted || 0);
            failed += (e.tasksFailed || e.TasksFailed || 0);
            if (e.active || e.Active) activeNow = true;
          }
        });
      });
      return {done: done, failed: failed, activeNow: activeNow};
    }

    /* userHiveJourneys returns [{name, role, journey, acmm, worstSeverity}] for the
       user's known hives, reusing _hiveRegistry (which carries journey + acmmLevel
       per hive). Used both to render per-hive badges and to derive the verdict. */
    function userHiveJourneys(u) {
      var hivesObj = u.hives || {};
      var out = [];
      Object.keys(hivesObj).forEach(function(hid) {
        var reg = (_hiveRegistry || []).find(function(h) { return h.id === hid; });
        if (!reg) return;
        out.push({
          id: hid,
          name: reg.name || hid,
          role: hivesObj[hid],
          journey: reg.journey || null,
          acmm: reg.acmmLevel || reg.acmm_level || 0
        });
      });
      return out;
    }

    /* userVerdict derives the lifecycle recommendation from the engagement signals
       and the user's hives' journey stages. Returns {key, label, color, tip}. */
    function userVerdict(u, act, hives) {
      var logins = u.login_count || 0;
      var hours = (u.session_seconds || 0) / USERSTAT_SECS_PER_HOUR;
      /* Honest signals, when the record has them: engaged_seconds accrues only
         while the tab was focused with recent input, and last_action_at is the
         newest audit-logged real action. Records that predate these fields (or
         whose spokes do not report presence yet) have NEITHER - for them fall
         back to the old open-tab inputs rather than treating absence as
         zero-engagement-now. */
      var engagedHours = (u.engaged_seconds || 0) / USERSTAT_SECS_PER_HOUR;
      var lastActionMs = u.last_action_at ? Date.parse(u.last_action_at) : NaN;
      var actedRecently = !isNaN(lastActionMs) && (Date.now() - lastActionMs) <= USERSTAT_ACTIVE_WINDOW_MS;
      var hasHonestData = !!(u.engaged_seconds || u.last_action_at);
      var anyDeprovision = hives.some(function(h) { return h.journey && h.journey.severity === 'deprovision-warning'; });
      var anyEarlyStage = hives.some(function(h) { return h.journey && (h.journey.stage === 'github-app' || h.journey.stage === 'method-model'); });
      var anyRestingACMM = hives.some(function(h) { return h.journey && h.journey.stage === 'acmm-level'; });
      var engaged = hasHonestData
        ? (actedRecently || engagedHours >= USERSTAT_ACTIVE_HOURS_MIN)
        : (logins >= USERSTAT_ACTIVE_LOGIN_MIN && hours >= USERSTAT_ACTIVE_HOURS_MIN);
      var dormant = hives.length > 0 && act.done === 0 && (hasHonestData
        ? (!actedRecently && engagedHours < USERSTAT_DORMANT_HOURS_MAX)
        : (logins <= 1 && hours < USERSTAT_DORMANT_HOURS_MAX));

      if (dormant || anyDeprovision) {
        return {key: 'at-risk', label: 'At risk — pressure or de-provision', color: 'var(--red)',
          tip: 'Assigned a hive but little/no engagement (or already at a de-provision warning). Consider a firm nudge or reclaiming the spoke.'};
      }
      if (engaged && act.done > 0 && anyRestingACMM) {
        return {key: 'elevate', label: 'Elevate — ready to graduate', color: 'var(--green)',
          tip: 'Active, spending real time in their hive, and tasks are landing while a hive rests at its ACMM level. Good candidate to raise autonomy / quota.'};
      }
      if (anyEarlyStage || act.done === 0) {
        return {key: 'needs-help', label: 'Needs help — nudge the journey', color: 'var(--accent)',
          tip: 'Signed in but stuck early on the adoption path or barely any activity. A journey nudge (not auto-sent) is the lever.'};
      }
      return {key: 'on-track', label: 'On track', color: 'var(--muted)',
        tip: 'Engaged and progressing; nothing to do.'};
    }

    function fmtHours(secs) {
      var h = (secs || 0) / USERSTAT_SECS_PER_HOUR;
      if (h <= 0) return '—';
      if (h < 1) return (Math.round(h * 10) / 10) + ' h';
      return (Math.round(h * 10) / 10).toString().replace(/\.0$/, '') + ' h';
    }

    function renderUserStatsCard(u, hasPendingReq) {
      var act = userTaskActivity(u.github_username);
      var hives = userHiveJourneys(u);
      var verdict = userVerdict(u, act, hives);
      var row = function(k, v) {
        return '<div style="display:flex;justify-content:space-between;gap:12px;padding:1px 0">' +
          '<span style="color:var(--muted)">' + esc(k) + '</span>' +
          '<span style="color:var(--text);text-align:right">' + v + '</span></div>';
      };
      var liveDot = act.activeNow
        ? '<span title="active on a task now" style="display:inline-block;width:7px;height:7px;border-radius:50%;background:var(--green);margin-left:6px;vertical-align:middle"></span>'
        : '';
      /* "Time in hive" is the legacy open-tab total; the two rows after it are
         the honest signals - focus-engaged time and the last audit-logged real
         action - so an admin reading the card sees exactly why the tier is
         what it is. Presence distinguishes "engaged now" from a merely-open
         idle tab (the old signal's blind spot). */
      var unameLower = String(u.github_username || '').toLowerCase();
      var presenceNow = (_liveEngagedUsers && _liveEngagedUsers.has(unameLower)) ? 'engaged now'
        : ((_liveHiveUsers && _liveHiveUsers.has(unameLower)) ? 'tab open (idle)' : '—');
      var stats =
        row('Hub logins', esc(String(u.login_count || 0)) + (u.last_login ? ' <span style="color:var(--muted)">(last ' + esc(fmtUserTS(u.last_login)) + ')</span>' : '')) +
        row('Time in hive', esc(fmtHours(u.session_seconds))) +
        row('Engaged time', esc(fmtHours(u.engaged_seconds))) +
        row('Last real action', esc(u.last_action_at ? fmtUserTS(u.last_action_at) : '—')) +
        row('Presence', esc(presenceNow)) +
        row('Joined', esc(fmtUserTS(u.created_at))) +
        row('Tasks', esc(String(act.done)) + ' done / ' + esc(String(act.failed)) + ' failed' + liveDot);

      var hiveLines = hives.length
        ? hives.map(function(h) {
            return '<div style="display:flex;align-items:center;gap:6px;padding:2px 0">' +
              '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(h.name) + '</span>' +
              '<span style="color:var(--muted);font-size:0.62rem">' + esc(h.role) + '</span>' +
              journeyBadge(h.journey) + '</div>';
          }).join('')
        : '<div style="color:var(--muted)">No hives assigned</div>';

      var verdictBlock = '<div style="margin:6px 0 2px;padding:4px 6px;border-radius:6px;border:1px solid ' + verdict.color + ';color:' + verdict.color + ';font-weight:700" title="' + escAttr(verdict.tip) + '">' + esc(verdict.label) + '</div>';

      // Assign flow relocated here as an explicit button — the click that used to
      // live on the (now inert) name. Approve-vs-assign copy mirrors the old title.
      var assignBtn = '<button onclick="openAssignForUser(\'' + escAttr(u.github_username) + '\');return false" ' +
        'style="margin-top:6px;width:100%;padding:5px 8px;border:1px solid var(--accent);background:transparent;color:var(--accent);border-radius:6px;cursor:pointer;font-size:0.72rem;font-weight:600">' +
        (hasPendingReq ? 'Approve request &amp; assign a hive' : 'Assign an available hive') + '</button>';

      return '<span class="hive-access-pop" style="display:none;position:absolute;left:0;top:calc(100% + 6px);z-index:60;' +
        'min-width:240px;max-width:320px;padding:9px 11px;border-radius:8px;border:1px solid var(--border);' +
        'background:var(--surface);box-shadow:0 6px 20px rgba(0,0,0,0.35);font-size:0.72rem;text-align:left;font-weight:400;white-space:normal">' +
        '<div style="font-weight:700;margin-bottom:4px">' + esc(userDisplayLabel(u)) +
          (userDisplayLabel(u) !== u.github_username
            ? '<span style="display:block;font-weight:400;font-size:0.62rem;color:var(--muted)">' + esc(u.github_username) + '</span>'
            : '') +
        '</div>' +
        verdictBlock +
        stats +
        '<span style="display:block;border-top:1px solid var(--border);margin:6px 0 4px"></span>' +
        '<span style="display:block;color:var(--muted);font-size:0.62rem;text-transform:uppercase;letter-spacing:0.04em;margin-bottom:4px">Hives &amp; journey</span>' +
        hiveLines +
        assignBtn +
        '</span>';
    }

    function renderUsers(users, force) {
      var sig = JSON.stringify(users);
      if (!force && sig === _lastUsersJSON) return;
      // Never rebuild the table out from under an in-progress contact edit —
      // the inputs save on blur, so unblurred text exists only in the DOM.
      // The data is already in _allUsers; only the DOM write is postponed.
      if (contactEditInProgress()) { scheduleDeferredUsersRender(); return; }
      _lastUsersJSON = sig;
      if (!users.length) { document.getElementById('users-container').innerHTML = '<div class="loading">No users found</div>'; return; }
      var rows = users.map(function(u) {
        /* Engagement tier, hub-computed (see statusTierBadge / userStatusTier).
           The old cell rendered "active" for EVERY non-blocked user - an idle
           open tab, a never-logged-in user, and a daily driver all looked the
           same. */
        var statusCell = statusTierBadge(u);
        /* GitHub users keep the linked github.com avatar; OIDC users get the
           stored provider avatar (or initials from their real name) and no
           GitHub profile link — provider:sub is not a github.com login. */
        var isGitHubUser = String(u.provider || 'github').toLowerCase() === 'github';
        var avatar = isGitHubUser
          ? linkedAvatar(u.github_username, TABLE_AVATAR_PX,
              u.github_username + ' — GitHub profile', 'margin-right:6px')
          : userAvatar(u, TABLE_AVATAR_PX, 'margin-right:6px');
        var isAdmin = u.github_username === 'clubanderson';
        var hivesObj = u.hives || {};
        var registryIds = new Set((_hiveRegistry || []).map(function(h) { return h.id; }));
        var hiveIds = Object.keys(hivesObj).filter(function(hid) { return registryIds.has(hid); });
        var hiveCount = hiveIds.length;
        var expandId = 'expand-' + esc(u.github_username);
        var isExpanded = _adminExpandedUsers && _adminExpandedUsers[u.github_username];

        var hiveRows = '';
        if (hiveCount > 0) {
          hiveRows = '<tr id="' + expandId + '" style="display:' + (isExpanded ? '' : 'none') + '"><td colspan="' + USERS_TABLE_COLSPAN + '"><div style="padding:8px 12px 8px 40px;font-size:0.75rem">';
          hiveRows += '<table style="width:100%;border-collapse:collapse"><thead><tr style="color:var(--muted);font-size:0.7rem"><th style="text-align:left;padding:4px 8px">Hive</th><th>Role</th><th>Type</th><th>Link</th></tr></thead><tbody>';
          hiveIds.forEach(function(hid) {
            var role = hivesObj[hid];
            var isHosted = hid.startsWith('hosted-') || hid.startsWith('saas-');
            var regEntry = (_hiveRegistry || []).find(function(h) { return h.id === hid; });
            var hiveName = regEntry ? (regEntry.name || hid) : hid;
            // Prefer the hive's heartbeat-reported dashboard URL so firewalled
            // spokes (the heartbeat-only cluster etc.) link to their real route, not a dead
            // <id>.hive.kubestellar.io host. Fall back to the hub-reachable-cluster pattern.
            var linkBase = (regEntry && regEntry.dashboardUrl && !regEntry.dashboardUrl.includes('localhost'))
              ? regEntry.dashboardUrl : (isHosted ? 'https://' + esc(hid) + '.hive.kubestellar.io' : '');
            var linkLabel = linkBase.replace(/^https?:\/\//, '');
            var link = linkBase ? '<a href="' + esc(linkBase) + '" target="_blank" class="dash-link">' + esc(linkLabel) + '</a>' : '<span style="color:var(--muted)">local</span>';
            var typeBadge = isHosted ? '<span style="color:#60a5fa">hosted</span>' : '<span style="color:#9ca3af">local</span>';
            hiveRows += '<tr><td style="padding:4px 8px">' + esc(hiveName) + '</td><td style="text-align:center">' + esc(role) + '</td><td style="text-align:center">' + typeBadge + '</td><td>' + link + '</td></tr>';
          });
          hiveRows += '</tbody></table></div></td></tr>';
        }

        /* The name no longer NAVIGATES. Hovering it opens an engagement/lifecycle
           card (renderUserStatsCard) so an admin can read who is active enough to
           elevate, who is stuck and needs a nudge, and who is dormant and at risk
           of de-provision — WITHOUT a click that jumps somewhere. The old assign
           flow moves INTO the card as an explicit button, so it is still one hover
           away but never fires by accident. The GitHub profile stays on the avatar.
           The wrapper reuses the hive-access-wrap/pop hover panel and carries NO
           title= (TestUserStatsCardNoTitle / the single-hover-panel invariant). */
        var hasPendingReq = !!_provisionRequestsByUser[u.github_username];
        var nameCell = avatar +
          '<span class="hive-access-wrap" style="position:relative;display:inline-flex;align-items:center;gap:4px;cursor:help">' +
            '<span style="color:var(--text);font-weight:600">' + esc(userDisplayLabel(u)) + '</span>' +
            providerBadge(u) +
            (hasPendingReq ? ' <span style="color:var(--accent);font-size:0.65rem">&#9679; request</span>' : '') +
            renderUserStatsCard(u, hasPendingReq) +
          '</span>';
        return '<tr>' +
          '<td>' + nameCell + (isAdmin ? ' <span style="color:var(--accent);font-size:0.7rem">admin</span>' : '') + '</td>' +
          '<td style="font-size:0.75rem;color:var(--muted)">' + esc(fmtUserTS(u.created_at)) + '</td>' +
          '<td style="font-size:0.75rem;color:var(--muted)">' + esc(fmtUserTS(u.last_login)) + '</td>' +
          '<td style="text-align:left">' + renderContactCell(u) + '</td>' +
          '<td>' + countryCell(u) + '</td>' +
          '<td>' + statusCell + '</td>' +
          '<td><input type="number" min="0" max="10" value="' + (u.saas_quota || 0) + '" style="width:50px;padding:4px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);text-align:center" onchange="updateUser(\'' + esc(u.github_username) + '\',{saas_quota:parseInt(this.value)||0})"></td>' +
          '<td>' + (hiveCount > 0 ? '<a href="#" onclick="toggleAdminExpand(\'' + esc(u.github_username) + '\');return false" style="color:var(--blue);font-size:0.8rem">' + hiveCount + ' hive' + (hiveCount > 1 ? 's' : '') + '</a>' : '<span style="color:var(--muted)">0</span>') + '</td>' +
          '<td>' + (isAdmin ? '' : '<button onclick="startImpersonation(\'' + esc(u.github_username) + '\')" title="See the hub exactly as this user does — read-only" style="padding:3px 10px;background:#b45309;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">View as</button> <button onclick="updateUser(\'' + esc(u.github_username) + '\',{blocked:' + (!u.blocked) + '})" style="padding:3px 10px;background:' + (u.blocked ? 'var(--green)' : 'var(--amber)') + ';color:' + (u.blocked ? '#fff' : '#1a1a1a') + ';border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">' + (u.blocked ? 'Unblock' : 'Block') + '</button> <button onclick="deleteUser(\'' + esc(u.github_username) + '\',' + hiveCount + ')" style="padding:3px 10px;background:#b02a2a;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">Delete</button>') + '</td>' +
          '</tr>' + renderContactPanelRow(u) + hiveRows;
      }).join('');
      document.getElementById('users-container').innerHTML =
        // Shared autocomplete vocabulary for the Company combobox, rebuilt each
        // render from the distinct company values across all users so it grows
        // as new companies are entered.
        '<datalist id="contact-company-suggestions">' + companyDatalistOptionsHTML() + '</datalist>' +
        '<table class="hive-table"><thead><tr>' +
        '<th onclick="sortUsers(\'github_username\')" style="cursor:pointer">User ⇅</th><th onclick="sortUsers(\'created_at\')" style="cursor:pointer">Joined ⇅</th><th onclick="sortUsers(\'last_login\')" style="cursor:pointer">Last Login ⇅</th><th onclick="sortUsers(\'full_name\')" style="cursor:pointer">Contact ⇅</th><th onclick="sortUsers(\'country\')" style="cursor:pointer">Country ⇅</th><th onclick="sortUsers(\'status\')" style="cursor:pointer">Status ⇅</th><th onclick="sortUsers(\'saas_quota\')" style="cursor:pointer">Quota ⇅</th><th onclick="sortUsers(\'hiveCount\')" style="cursor:pointer">Hives ⇅</th><th>Actions</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table>';
      // The contact panels are built as raw HTML above; wire their listeners
      // once the rows are actually in the DOM. Inline on* handlers are avoided
      // for these fields because esc() does not escape apostrophes, so a name
      // or note containing one would break out of an inline handler string.
      bindContactPanels();
    }

    /* updateUser previously ignored the response entirely: a 404/400/500 was
       indistinguishable from success, so a rejected edit looked like it had
       been saved until the next poll quietly reverted the field. It now checks
       resp.ok and reports the server's reason, and returns a boolean so
       callers can decide whether to close the editor.

       readErrorMessage is used rather than a bare resp.json(): the handler
       writes errors with writeJSONError so the body IS JSON, but a proxy or
       middleware fault can still return HTML/plain text, and resp.json() on
       that throws — which is exactly how #2348's handleAlertAck turned a real
       cause into a generic "failed". */
    async function readErrorMessage(resp, fallback) {
      try {
        var text = await resp.text();
        if (text) {
          try {
            var parsed = JSON.parse(text);
            if (parsed && parsed.error) return String(parsed.error);
          } catch(je) {
            /* Not JSON — show the raw text, which is still more useful than
               a bare status code. */
            return text.trim().substring(0, ERROR_TEXT_MAX_CHARS);
          }
        }
      } catch(e) {}
      return fallback + ' (HTTP ' + resp.status + ')';
    }

    /* Longest slice of a non-JSON error body shown in a toast. Long enough for
       a real proxy/server message, short enough not to fill the screen. */
    var ERROR_TEXT_MAX_CHARS = 200;

    /* Reason for the most recent failed save, per user. A silent blur-save
       records the cause here instead of toasting it, so the Save button can
       report the REAL reason rather than a generic "failed" when the admin
       finally commits. Cleared on any success for that user. */
    var _contactLastError = {};

    /* opts.silent suppresses the toast (blur-saves); the failure is still
       recorded in _contactLastError and still returns false. */
    async function updateUser(username, updates, opts) {
      var silent = !!(opts && opts.silent);
      function fail(reason) {
        _contactLastError[username] = reason;
        if (!silent) hiveToast('Could not save ' + username + ': ' + reason, 'error');
        return false;
      }
      try {
        var resp = await fetch('/api/saas/admin/users/' + encodeURIComponent(username), {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(updates)
        });
        if (!resp.ok) {
          return fail(await readErrorMessage(resp, 'update failed'));
        }
        delete _contactLastError[username];
        loadAdminUsers();
        return true;
      } catch(e) {
        return fail(e.message);
      }
    }

    async function deleteUser(username, hiveCount) {
      if (hiveCount > 0) {
        hiveToast('Cannot delete ' + username + ' — they still own ' + hiveCount + ' hive(s). Delete or reassign those first.', 'error');
        return;
      }
      if (!await hiveConfirm('Delete hub user ' + username + '? This removes their hub account record (login, quota, saved token). It does not touch GitHub.')) return;
      try {
        var resp = await fetch('/api/saas/admin/users/' + encodeURIComponent(username), { method: 'DELETE' });
        if (!resp.ok) {
          var e = await resp.json().catch(function(){ return {}; });
          hiveToast('Delete failed: ' + (e.error || resp.status), 'error');
          return;
        }
        hiveToast('Deleted ' + username, 'success');
        loadAdminUsers();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function deleteHive(id) {
      // Confirm with the friendly hive NAME the operator sees in the table, not
      // the raw id / vanity-URL slug. Fall back to the id if the row is gone.
      var _row = document.querySelector('[data-hive-id="' + (window.CSS && CSS.escape ? CSS.escape(id) : id) + '"]');
      var name = (_row && _row.getAttribute('data-hive-name')) || id;
      if (!await hiveConfirm('Delete "' + name + '"? This removes the namespace, PV, OCI storage, and all data.')) return;
      var btns = document.querySelectorAll('button[onclick*="deleteHive"]');
      btns.forEach(function(b) { b.disabled = true; b.textContent = 'Deleting...'; b.style.opacity = '0.6'; });
      // Mark the row as deleting so its status shows "Deleting…" until the next
      // refresh removes it (or clears the flag on failure).
      _deletingHives[id] = true;
      try {
        gtag('event','hive_deleted',{hive_id:id});
        hiveToast('Deleting "' + name + '"…', 'info');
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id), {method: 'DELETE'});
        if (!resp.ok) {
          var d = await resp.json().catch(function(){ return {}; });
          hiveToast(d.error || 'Delete failed', 'error');
          delete _deletingHives[id];
          // Refresh regardless: the hub removes the registry entry even when
          // teardown fails, so the listing may already be correct.
          loadHives();
          return;
        }
        var ok = await resp.json().catch(function(){ return {}; });
        if (ok.status === 'partially_deleted') {
          /* partially_deleted is a SUCCESS, not a failure: the registry row is
             gone (which is what the user asked for and what they can see), but
             no cluster config was available to tear down the namespace/PV/OCI
             storage, so those may survive. Rendering it as an 'error' toast made
             a partial success look like the delete had failed even though the
             row vanished. Show it as an informational notice instead. */
          hiveToast('Removed "' + name + '" from the hub; some cloud resources may need manual cleanup' + (ok.warning ? ' (' + ok.warning + ')' : ''), 'info');
        } else {
          hiveToast('Deleted "' + name + '"', 'success');
        }
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); delete _deletingHives[id]; }
      finally { btns.forEach(function(b) { b.disabled = false; b.textContent = 'Delete'; b.style.opacity = '1'; }); }
    }

    function toggleHiveMenu(menuId) {
      var menu = document.getElementById(menuId);
      var wasOpen = menu.style.display !== 'none';
      document.querySelectorAll('[id^="hive-menu-"]').forEach(function(m) { m.style.display = 'none'; });
      if (!wasOpen) menu.style.display = 'block';
    }
    document.addEventListener('click', function(e) {
      if (!e.target.closest('[id^="hive-menu-"]') && !e.target.closest('[onclick*="toggleHiveMenu"]')) {
        document.querySelectorAll('[id^="hive-menu-"]').forEach(function(m) { m.style.display = 'none'; });
      }
    });

    /* "View all activity" in the status hover panel. Delegated rather than an
       inline onclick because the hive name is user-controlled and esc() does
       not escape apostrophes — a name containing one would break out of an
       inline handler string. The values are read back from data attributes,
       where esc() IS sufficient. */
    document.addEventListener('click', function(e) {
      var btn = e.target.closest ? e.target.closest('.hover-view-timeline') : null;
      if (!btn) return;
      openTimelineModal(btn.getAttribute('data-hive-id') || '', btn.getAttribute('data-hive-name') || '');
    });


    var ASSIGN_DEFAULT_ACMM = 2;
    /* openAssignModal claims one placeholder for a real project.
       prefillOwner (optional) pre-fills the Owner field — used when the flow is
       started by clicking a user in the admin users table.
       placeholders (optional) turns the fixed hive id into a dropdown, so that
       user-first entry point can retarget without backing out to the hive list.
       Both are omitted by the original ⋮ → "Assign / Claim" call site. */
    /* hiveNotify (below) and hivePrompt replace the native alert and prompt
       dialogs; the native confirm dialog is replaced by the existing hiveConfirm
       defined earlier. Native dialogs are jarring in a themed dashboard, cannot
       be styled, and block the whole tab; they also read as a browser warning
       rather than as part of the product. These follow the same overlay pattern
       the assign, access and timeline modals already use.

       Promise-based so they drop into the existing await-style call sites
       without restructuring them. Escape and a backdrop click both resolve
       false, matching what a user expects from a dismissable dialog. */
    function _hiveDialog(opts) {
      return new Promise(function(resolve) {
        var overlay = document.createElement('div');
        overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:3000;display:flex;align-items:center;justify-content:center';
        var btn = 'padding:7px 14px;border-radius:6px;border:1px solid var(--border);cursor:pointer;font-size:0.8rem';
        var body = (opts.body || '').split('\n').map(function(line) {
          return line ? '<p style="margin:0 0 8px 0;color:var(--muted);font-size:0.85rem;line-height:1.5">' + esc(line) + '</p>' : '';
        }).join('');
        overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:22px;max-width:460px;width:90%">' +
          '<h3 style="margin:0 0 10px 0;font-size:1rem">' + esc(opts.title || '') + '</h3>' + body +
          '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:18px">' +
          (opts.cancel ? '<button data-act="no" style="' + btn + ';background:transparent;color:var(--fg)">' + esc(opts.cancel) + '</button>' : '') +
          '<button data-act="yes" style="' + btn + ';background:' + (opts.danger ? '#da3633' : 'var(--accent,#3fb950)') + ';color:#fff;border-color:transparent;font-weight:600">' + esc(opts.ok || 'OK') + '</button>' +
          '</div></div>';
        function done(v) {
          document.removeEventListener('keydown', onKey);
          overlay.remove();
          resolve(v);
        }
        function onKey(e) { if (e.key === 'Escape') done(false); }
        overlay.addEventListener('click', function(e) {
          if (e.target === overlay) { done(false); return; }
          var act = e.target.getAttribute && e.target.getAttribute('data-act');
          if (act) done(act === 'yes');
        });
        document.addEventListener('keydown', onKey);
        document.body.appendChild(overlay);
        var y = overlay.querySelector('[data-act="yes"]');
        if (y) y.focus();
      });
    }
    /* hiveConfirm already exists above (msg, rawHTML) -> Promise<bool>, added by
       the themed-overlay work this dashboard already relies on; this feature
       reuses it rather than redefining it. hiveNotify below is the native-alert
       replacement, following the same overlay pattern. */
    function hiveNotify(title, body) {
      return _hiveDialog({title: title, body: body, ok: 'OK'});
    }
    /* hivePrompt replaces window.prompt(). Resolves to the trimmed string, or
       null when cancelled — same contract as the native call it replaces, so
       existing null checks keep working. */
    function hivePrompt(title, defaultValue, opts) {
      opts = opts || {};
      return new Promise(function(resolve) {
        var overlay = document.createElement('div');
        overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:3000;display:flex;align-items:center;justify-content:center';
        var btn = 'padding:7px 14px;border-radius:6px;border:1px solid var(--border);cursor:pointer;font-size:0.8rem';
        overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:22px;max-width:420px;width:90%">' +
          '<h3 style="margin:0 0 12px 0;font-size:1rem">' + esc(title || '') + '</h3>' +
          '<input id="_hive-prompt-input" type="text" value="' + esc(defaultValue || '') + '" style="width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box;font-size:0.85rem">' +
          '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:16px">' +
          '<button data-act="no" style="' + btn + ';background:transparent;color:var(--fg)">Cancel</button>' +
          '<button data-act="yes" style="' + btn + ';background:var(--accent,#3fb950);color:#fff;border-color:transparent;font-weight:600">' + esc(opts.ok || 'Save') + '</button>' +
          '</div></div>';
        function done(v) {
          document.removeEventListener('keydown', onKey);
          overlay.remove();
          resolve(v);
        }
        function submit() {
          var el = document.getElementById('_hive-prompt-input');
          done(el ? el.value.trim() : null);
        }
        function onKey(e) {
          if (e.key === 'Escape') done(null);
          if (e.key === 'Enter') submit();
        }
        overlay.addEventListener('click', function(e) {
          if (e.target === overlay) { done(null); return; }
          var act = e.target.getAttribute && e.target.getAttribute('data-act');
          if (act === 'yes') submit();
          else if (act === 'no') done(null);
        });
        document.addEventListener('keydown', onKey);
        document.body.appendChild(overlay);
        var inp = document.getElementById('_hive-prompt-input');
        if (inp) { inp.focus(); inp.select(); }
      });
    }

    function openAssignModal(hiveId, prefillOwner, placeholders) {
      var h = (_allDashHives || []).reduce(function(m, x) { return x.id === hiveId ? x : m; }, null) || {};
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var lbl = 'display:block;font-size:0.75rem;color:var(--muted);margin:10px 0 4px';
      var acmmOpts = '';
      for (var lv = 0; lv <= 6; lv++) { acmmOpts += '<option value="' + lv + '"' + (lv === ASSIGN_DEFAULT_ACMM ? ' selected' : '') + '>' + lv + '</option>'; }
      var orgVal = esc(h.org || '');
      var reposVal = esc((h.repos || []).join(', '));
      /* With a placeholder list, let the admin retarget in place; without one,
         keep the original fixed-hive wording. */
      var hasPicker = !!(placeholders && placeholders.length);
      var hiveField;
      if (hasPicker) {
        var phOpts = (placeholders || []).map(function(p) {
          return '<option value="' + esc(p.id) + '"' + (p.id === hiveId ? ' selected' : '') + '>' +
            esc(p.id) + '  (' + esc(p.cluster_id || 'default') + ')</option>';
        }).join('');
        hiveField =
          '<div style="margin-bottom:4px;font-size:0.8rem;color:var(--muted)">Claim an available placeholder for a real project.</div>' +
          '<label style="' + lbl + '">Placeholder to assign *</label>' +
          '<select id="assign-hive-pick" style="' + fld + '">' + phOpts + '</select>';
      } else {
        hiveField = '<div style="margin-bottom:4px;font-size:0.8rem;color:var(--muted)">Claim placeholder <strong style="color:var(--fg)">' + esc(hiveId) + '</strong> for a real project.</div>';
      }
      /* The GitHub instance defaults to the SELECTED placeholder's cluster.
         With a picker the selection can change, so resolve the cluster of the
         initially-selected placeholder and re-resolve on change below. */
      function clusterIdForSelection(id) {
        var fromList = (placeholders || []).reduce(function(m, p) { return (p && p.id === id) ? p : m; }, null);
        if (fromList) return fromList.cluster_id || '';
        var fromDash = (_allDashHives || []).reduce(function(m, x) { return (x && x.id === id) ? x : m; }, null);
        return (fromDash && fromDash.clusterId) || '';
      }
      var assignClusterHost = clusterGitHubHost(clusterIdForSelection(hiveId));
      var content =
        hiveField +
        '<label style="' + lbl + '">Owner (GitHub login) *</label>' +
        '<input id="assign-owner" style="' + fld + '" value="' + esc(prefillOwner || '') + '" placeholder="octocat">' +
        '<label style="' + lbl + '">Org *</label>' +
        '<input id="assign-org" style="' + fld + '" value="' + orgVal + '" placeholder="my-org">' +
        githubHostChoiceMarkup('assign', assignClusterHost, '', fld, lbl) +
        '<label style="' + lbl + '">Repos * (comma-separated)</label>' +
        '<input id="assign-repos" style="' + fld + '" value="' + reposVal + '" placeholder="repo-a, repo-b">' +
        '<label style="' + lbl + '">Primary repo (optional, defaults to first)</label>' +
        '<input id="assign-primary" style="' + fld + '" placeholder="repo-a">' +
        '<label style="' + lbl + '">Project name (optional)</label>' +
        '<input id="assign-name" style="' + fld + '" placeholder="My Project">' +
        '<label style="' + lbl + '">ACMM level</label>' +
        '<select id="assign-acmm" style="' + fld + '">' + acmmOpts + '</select>' +
        '<label style="display:flex;align-items:center;gap:8px;margin:12px 0 4px;font-size:0.8rem;cursor:pointer"><input type="checkbox" id="assign-public"> Public (visible to everyone)</label>' +
        '<div onclick="toggleAssignAdvanced()" id="assign-adv-toggle" style="margin-top:12px;font-size:0.78rem;color:var(--accent);cursor:pointer;user-select:none">▸ GitHub App credentials (optional)</div>' +
        '<div id="assign-adv" style="display:none;margin-top:8px">' +
        '<div style="font-size:0.72rem;color:var(--muted);margin-bottom:8px">Optional — the owner can install the App from their dashboard after assignment.</div>' +
        '<label style="' + lbl + '">App ID</label>' +
        '<input id="assign-app-id" style="' + fld + '" placeholder="123456">' +
        '<label style="' + lbl + '">Installation ID</label>' +
        '<input id="assign-install-id" style="' + fld + '" placeholder="654321">' +
        '<label style="' + lbl + '">App private key (PEM)</label>' +
        '<textarea id="assign-app-key" rows="4" style="' + fld + ';font-family:monospace;font-size:0.7rem" placeholder="-----BEGIN RSA PRIVATE KEY-----"></textarea>' +
        '</div>' +
        '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:16px">' +
        '<button onclick="closeAssignModal()" style="padding:6px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Cancel</button>' +
        '<button id="assign-submit" onclick="confirmAssign(\'' + esc(hiveId) + '\')" style="padding:6px 16px;background:#3fb950;color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Assign</button>' +
        '</div>';
      var overlay = document.createElement('div');
      overlay.id = 'assign-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:440px;width:90%;max-height:88vh;overflow-y:auto">' +
        '<h3 style="margin:0 0 12px 0;font-size:1rem">Assign placeholder hive</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeAssignModal(); });
      document.body.appendChild(overlay);
      wireGitHubHostChoice('assign', assignClusterHost);
      /* Retargeting the placeholder can change the cluster, and with it the
         default GitHub instance. Re-resolve so the note never claims the
         previous cluster's host. An explicit override the admin already made
         (public / a typed host) is left alone — only the "cluster default"
         label and note follow the selection. */
      var phPick = document.getElementById('assign-hive-pick');
      if (phPick) {
        phPick.addEventListener('change', function() {
          assignClusterHost = clusterGitHubHost(clusterIdForSelection(phPick.value));
          var sel = document.getElementById('assign-ghhost');
          if (sel && sel.options && sel.options.length) {
            sel.options[0].textContent = assignClusterHost
              ? 'Cluster default — ' + assignClusterHost
              : 'Cluster default';
          }
          wireGitHubHostChoice('assign', assignClusterHost);
        });
      }
    }
    function toggleAssignAdvanced() {
      var adv = document.getElementById('assign-adv');
      var tog = document.getElementById('assign-adv-toggle');
      if (!adv || !tog) return;
      var open = adv.style.display === 'none';
      adv.style.display = open ? 'block' : 'none';
      tog.textContent = (open ? '▾' : '▸') + ' GitHub App credentials (optional)';
    }
    function closeAssignModal() {
      var ov = document.getElementById('assign-overlay');
      if (ov) ov.remove();
    }
    async function confirmAssign(hiveId) {
      /* When the modal was opened with a placeholder picker, the dropdown — not
         the id baked into this onclick — is the authority on the target. */
      var pick = document.getElementById('assign-hive-pick');
      if (pick && pick.value) hiveId = pick.value;
      var owner = document.getElementById('assign-owner').value.trim();
      var org = document.getElementById('assign-org').value.trim();
      var repos = document.getElementById('assign-repos').value.trim();
      var primary = document.getElementById('assign-primary').value.trim();
      var name = document.getElementById('assign-name').value.trim();
      var acmm = parseInt(document.getElementById('assign-acmm').value, 10) || 0;
      var isPublic = document.getElementById('assign-public').checked;
      var appId = document.getElementById('assign-app-id').value.trim();
      var installId = document.getElementById('assign-install-id').value.trim();
      var appKey = document.getElementById('assign-app-key').value.trim();
      if (!owner) { hiveToast('Owner is required', 'error'); return; }
      if (!org) { hiveToast('Org is required', 'error'); return; }
      if (!repos) { hiveToast('Repos are required', 'error'); return; }
      var submit = document.getElementById('assign-submit');
      if (submit) { submit.disabled = true; submit.textContent = 'Assigning...'; }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/assign', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          /* github_host: '' = let the server backfill from the cluster,
             'public' = force public github.com, otherwise a GHE hostname. */
          body: JSON.stringify({owner: owner, org: org, github_host: githubHostChoiceValue('assign'), repos: repos, primary_repo: primary, project_name: name, acmm_level: acmm, is_public: isPublic, app_id: appId, installation_id: installId, app_private_key: appKey})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Assignment failed', 'error'); if (submit) { submit.disabled = false; submit.textContent = 'Assign'; } return; }
        closeAssignModal();
        hiveToast('Assigned ' + hiveId + ' to ' + owner, 'success');
        loadHives();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
        if (submit) { submit.disabled = false; submit.textContent = 'Assign'; }
      }
    }

    // ---- OpenRouter scan-to-fund (hub side) ---------------------------------
    // Fund a SPECIFIC hive: pick a default model, scan a QR (or open the link)
    // to authorize on OpenRouter. The hub exchanges the code for a scoped key and
    // delivers it to the hive as the "openrouter" gateway over the next heartbeat.
    var _orFundPoll = null;
    async function openOpenRouterFundModal(hiveId, hiveName) {
      var models = { suggested: [], models: [], default: '' };
      try {
        var r = await fetch('/api/openrouter/models');
        if (r.ok) models = await r.json();
      } catch (e) { /* fall back to manual entry */ }
      var opts = (models.suggested || []).map(function(m) {
        return '<option value="' + esc(m.id) + '">' + esc(m.label) + '</option>';
      }).join('') + '<option value="__manual__">Enter a model id manually…</option>';
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var content =
        '<div style="font-size:0.8rem;color:var(--muted);margin-bottom:10px">Fund <strong style="color:var(--fg)">' + esc(hiveName) + '</strong> with an OpenRouter key. Scan the QR (or open the link) to authorize on OpenRouter — the scoped key is delivered to the hive as its <code>openrouter</code> gateway.</div>' +
        '<label style="display:block;font-size:0.75rem;color:var(--muted);margin-bottom:4px">Default model</label>' +
        '<select id="orf-model" onchange="orfModelChange()" style="' + fld + ';margin-bottom:8px">' + opts + '</select>' +
        '<input id="orf-model-manual" placeholder="e.g. anthropic/claude-opus-4.8" style="display:none;' + fld + ';margin-bottom:8px">' +
        '<div style="display:flex;gap:8px;margin-top:4px">' +
        '<button onclick="orfStart(\'' + esc(hiveId) + '\')" style="padding:7px 16px;background:var(--accent,#58a6ff);color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Generate QR</button>' +
        '<button onclick="closeOrFundModal()" style="padding:7px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Close</button>' +
        '</div>' +
        '<div id="orf-qr" style="margin-top:12px"></div>' +
        '<div id="orf-status" style="margin-top:8px;font-size:0.78rem"></div>';
      var overlay = document.createElement('div');
      overlay.id = 'orf-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:460px;width:90%;max-height:88vh;overflow-y:auto">' +
        '<h3 style="margin:0 0 12px 0;font-size:1rem">⚡ Fund with OpenRouter</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeOrFundModal(); });
      document.body.appendChild(overlay);
    }
    function orfModelChange() {
      var sel = document.getElementById('orf-model');
      var man = document.getElementById('orf-model-manual');
      if (sel && man) man.style.display = sel.value === '__manual__' ? 'block' : 'none';
    }
    function orfChosenModel() {
      var sel = document.getElementById('orf-model');
      if (!sel) return '';
      if (sel.value === '__manual__') {
        var man = document.getElementById('orf-model-manual');
        return (man && man.value.trim()) || '';
      }
      return sel.value;
    }
    async function orfStart(hiveId) {
      var model = orfChosenModel();
      var qr = document.getElementById('orf-qr');
      var status = document.getElementById('orf-status');
      if (qr) qr.innerHTML = '<span style="color:var(--muted);font-size:0.78rem">Preparing…</span>';
      try {
        var res = await fetch('/api/openrouter/connect/start?hive_id=' + encodeURIComponent(hiveId) + '&model=' + encodeURIComponent(model));
        if (!res.ok) { var e = await res.json().catch(function(){return{};}); throw new Error(e.error || ('HTTP ' + res.status)); }
        var data = await res.json();
        var authURL = data.authorize_url;
        var qrSrc = '/api/openrouter/qr?data=' + encodeURIComponent(authURL);
        if (qr) qr.innerHTML =
          '<div style="display:flex;gap:16px;align-items:center;flex-wrap:wrap">' +
          '<img src="' + esc(qrSrc) + '" alt="OpenRouter QR" width="180" height="180" style="border:6px solid #fff;border-radius:8px;background:#fff">' +
          '<div style="font-size:0.78rem"><div style="margin-bottom:6px">Scan with your phone, or on this device:</div>' +
          '<a href="' + esc(authURL) + '" target="_blank" rel="noopener" style="color:var(--accent,#58a6ff);font-weight:600">Open OpenRouter authorization ↗</a></div></div>';
        if (status) status.innerHTML = '<span style="color:var(--muted)">Waiting for authorization…</span>';
        orfStartPolling(hiveId);
      } catch (e) {
        if (qr) qr.innerHTML = '<span style="color:#f85149;font-size:0.78rem">Failed: ' + esc(e.message) + '</span>';
      }
    }
    function orfStartPolling(hiveId) {
      if (_orFundPoll) clearInterval(_orFundPoll);
      var ORF_POLL_MS = 4000;
      _orFundPoll = setInterval(function() { orfCheck(hiveId); }, ORF_POLL_MS);
    }
    async function orfCheck(hiveId) {
      var status = document.getElementById('orf-status');
      try {
        var res = await fetch('/api/openrouter/credit?hive_id=' + encodeURIComponent(hiveId));
        if (!res.ok) return;
        var d = await res.json();
        // pending_delivery flips true the moment the fund completes on the hub;
        // it then flips back to false once the hive drains it on a heartbeat.
        if (d.pending_delivery) {
          if (status) status.innerHTML = '<span style="color:var(--green,#3fb950);font-weight:600">✓ Funded — delivering to the hive on its next heartbeat…</span>';
        }
      } catch (e) { /* keep polling */ }
    }
    function closeOrFundModal() {
      if (_orFundPoll) { clearInterval(_orFundPoll); _orFundPoll = null; }
      var ov = document.getElementById('orf-overlay');
      if (ov) ov.remove();
    }

    async function removeLocalHive(id) {
      if (!await hiveConfirm('Remove ' + id + ' from the registry? The hive itself is not affected — it will reappear if it sends another heartbeat.')) return;
      try {
        var resp = await fetch('/api/hub/registry/' + encodeURIComponent(id), {method: 'DELETE'});
        if (!resp.ok) { var d = await resp.json(); hiveToast(d.error || 'Remove failed', 'error'); return; }
        hiveToast('Removed ' + id + ' from registry', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    function openConvert(btn) {
      document.getElementById('f-org').value = btn.dataset.org || '';
      document.getElementById('f-repos').value = btn.dataset.repos || '';
      document.getElementById('f-primary').value = btn.dataset.primary || '';
      document.getElementById('f-name').value = btn.dataset.name || '';
      document.getElementById('f-level').value = btn.dataset.level || '1';
      document.getElementById('create-modal').style.display = 'flex';
      var dashUrl = (btn.dataset.dashUrl || '').replace(/\/$/, '');
      var dlLink = document.getElementById('yaml-download-link');
      var dlHref = document.getElementById('yaml-download-href');
      if (dashUrl && dlLink && dlHref) {
        dlHref.href = dashUrl + '/api/config/download';
        dlLink.style.display = '';
      } else if (dlLink) {
        dlLink.style.display = 'none';
      }
    }

    var _createInProgress = false;
    async function createHive() {
      if (_createInProgress) return;
      _createInProgress = true;
      document.getElementById('btn-go').disabled = true;
      document.getElementById('btn-go').textContent = 'Provisioning...';
      var org = document.getElementById('f-org').value.trim();
      var repos = document.getElementById('f-repos').value.trim();
      var primary = document.getElementById('f-primary').value.trim();
      var name = document.getElementById('f-name').value.trim();
      var level = parseInt(document.getElementById('f-level').value) || 1;
      var clusterSel = document.getElementById('f-cluster');
      var clusterId = clusterSel ? clusterSel.value : '';
      var method = document.querySelector('input[name="auth-method"]:checked').value;
      var token = document.getElementById('f-token').value.trim();
      var appId = (document.getElementById('f-app-id') || {}).value || '';
      var installId = (document.getElementById('f-install-id') || {}).value || '';
      var appKey = (document.getElementById('f-app-key') || {}).value || '';

      gtag('event','hive_create_started',{org:org,primary_repo:primary,acmm_level:level,cluster_id:clusterId});
      if (!org || !repos) { hiveToast('Org and repos are required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'pat' && !token) { hiveToast('GitHub token is required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'app' && (!appId || !installId || !appKey)) { hiveToast('App ID, Installation ID, and Private Key are required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'later') { method = 'app'; appId = '3568013'; installId = ''; appKey = ''; }

      try {
        var body = {org: org, repos: repos, primary_repo: primary || repos.split(',')[0].trim(), project_name: name, acmm_level: level, cluster_id: clusterId, auth_method: method, is_public: document.getElementById('f-public').checked};
        if (method === 'pat') body.github_token = token;
        else { body.app_id = appId.trim(); body.installation_id = installId.trim(); body.app_private_key = appKey.trim(); }

        var resp = await fetch('/api/saas/hives', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(body)
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to create hive', 'error'); return; }

        document.getElementById('create-modal').style.display = 'none';
        document.getElementById('btn-go').disabled = false;
        document.getElementById('btn-go').textContent = 'Go';

        hiveToast('Hive ' + data.id + ' is provisioning!', 'success');
        loadHives();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
      } finally {
        _createInProgress = false;
        document.getElementById('btn-go').disabled = false;
        document.getElementById('btn-go').textContent = 'Go';
      }
    }

    function parseHiveYaml(text) {
      var cfg = {};
      var lines = text.split('\n');
      var section = '';
      for (var i = 0; i < lines.length; i++) {
        var line = lines[i];
        var trimmed = line.replace(/\s+$/, '');
        if (/^project:/.test(trimmed)) { section = 'project'; continue; }
        if (/^github:/.test(trimmed)) { section = 'github'; continue; }
        if (/^governor:/.test(trimmed)) { section = 'governor'; continue; }
        if (/^\S/.test(trimmed) && /:/.test(trimmed)) { section = ''; continue; }
        if (section === 'project') {
          var m;
          if ((m = trimmed.match(/^\s+org:\s*(.+)/))) cfg.org = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+repos:\s*$/))) { cfg.repos = []; for (var j = i + 1; j < lines.length && /^\s+-\s/.test(lines[j]); j++) { cfg.repos.push(lines[j].replace(/^\s+-\s*/, '').trim().replace(/^["']|["']$/g, '')); } }
          if ((m = trimmed.match(/^\s+repos:\s*\[(.+)\]/))) cfg.repos = m[1].split(',').map(function(r) { return r.trim().replace(/^["']|["']$/g, ''); });
          if ((m = trimmed.match(/^\s+primary_repo:\s*(.+)/))) cfg.primary = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+name:\s*(.+)/))) cfg.name = m[1].trim().replace(/^["']|["']$/g, '');
        }
        if (section === 'github') {
          var m;
          if ((m = trimmed.match(/^\s+token:\s*(.+)/))) cfg.token = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+app_id:\s*(\d+)/))) cfg.appId = m[1];
          if ((m = trimmed.match(/^\s+installation_id:\s*(\d+)/)) && !trimmed.match(/docs_installation_id/)) cfg.installId = m[1];
        }
        if (section === 'governor') {
          var m;
          if ((m = trimmed.match(/^\s+acmm_level:\s*(\d+)/))) cfg.level = parseInt(m[1]);
        }
      }
      return cfg;
    }

    function applyYamlConfig(cfg) {
      if (cfg.org) document.getElementById('f-org').value = cfg.org;
      if (cfg.repos) document.getElementById('f-repos').value = cfg.repos.join(', ');
      if (cfg.primary) document.getElementById('f-primary').value = cfg.primary;
      if (cfg.name) document.getElementById('f-name').value = cfg.name;
      if (cfg.level) document.getElementById('f-level').value = cfg.level;
      if (cfg.appId) {
        document.querySelector('input[name="auth-method"][value="app"]').checked = true;
        document.getElementById('auth-pat').style.display = 'none';
        document.getElementById('auth-app').style.display = '';
        document.getElementById('f-app-id').value = cfg.appId;
        if (cfg.installId) document.getElementById('f-install-id').value = cfg.installId;
      } else if (cfg.token) {
        document.getElementById('f-token').value = cfg.token;
      }
      var drop = document.getElementById('yaml-drop');
      drop.innerHTML = '<div style="font-size:0.82rem;color:var(--green)">✓ Config loaded</div>';
    }

    function readYamlFile(file) {
      var reader = new FileReader();
      reader.onload = function() {
        var cfg = parseHiveYaml(reader.result);
        applyYamlConfig(cfg);
        hiveToast('Config loaded from ' + file.name, 'success');
      };
      reader.readAsText(file);
    }
  </script>

  <div id="banner-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:540px;width:90%;max-height:90vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 16px;margin:0;color:var(--accent);flex-shrink:0">Send Hub Banner</h2>
      <div style="flex:1;overflow-y:auto;padding:0 32px">
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Message *</label>
          <div style="display:flex;gap:6px;margin-bottom:6px">
            <button type="button" onclick="bannerFmtBold()" title="Bold (**text**)" style="padding:3px 10px;background:var(--bg);border:1px solid var(--border);border-radius:5px;color:var(--text);cursor:pointer;font-weight:700;font-size:0.8rem">B</button>
            <button type="button" onclick="bannerFmtLink()" title="Insert link ([text](url))" style="padding:3px 10px;background:var(--bg);border:1px solid var(--border);border-radius:5px;color:var(--text);cursor:pointer;font-size:0.8rem">🔗 Link</button>
          </div>
          <textarea id="banner-message" rows="3" maxlength="500" oninput="updateBannerPreview()" placeholder="Announce a new capability... — use the buttons above for bold and links" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;resize:vertical;font-family:inherit"></textarea>
          <div style="font-size:0.7rem;color:var(--muted);display:flex;justify-content:space-between;margin-top:2px"><span>Markdown: <code>**bold**</code>, <code>[text](https://url)</code></span><span><span id="banner-char-count">0</span>/500</span></div>
          <div style="font-size:0.7rem;color:var(--muted);margin:8px 0 3px">Preview</div>
          <div id="banner-preview" style="padding:10px 14px;border:1px dashed var(--border);border-radius:6px;font-size:0.85rem;min-height:1.4em;color:var(--text)"></div>
        </div>
        <div style="margin-bottom:16px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:8px">Color</label>
          <div id="banner-color-picker" style="display:flex;gap:8px;flex-wrap:wrap">
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(22,163,74,0.12)">
              <input type="radio" name="banner-color" value="green" checked style="accent-color:#4ade80"> <span style="color:#4ade80;font-size:0.82rem">Green</span>
            </label>
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(59,130,246,0.12)">
              <input type="radio" name="banner-color" value="blue" style="accent-color:#93c5fd"> <span style="color:#93c5fd;font-size:0.82rem">Blue</span>
            </label>
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(245,158,11,0.12)">
              <input type="radio" name="banner-color" value="amber" style="accent-color:#fcd34d"> <span style="color:#fcd34d;font-size:0.82rem">Amber</span>
            </label>
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(107,114,128,0.12)">
              <input type="radio" name="banner-color" value="gray" style="accent-color:#d1d5db"> <span style="color:#d1d5db;font-size:0.82rem">Gray</span>
            </label>
          </div>
        </div>
        <div style="margin-bottom:16px">
          <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:8px">
            <label style="font-size:0.8rem;color:var(--muted)">Target Hives *</label>
            <div style="display:flex;gap:8px">
              <button onclick="toggleAllBannerHives(true)" style="font-size:0.72rem;color:var(--accent);background:none;border:none;cursor:pointer;text-decoration:underline">Select All</button>
              <button onclick="toggleAllBannerHives(false)" style="font-size:0.72rem;color:var(--muted);background:none;border:none;cursor:pointer;text-decoration:underline">Deselect All</button>
            </div>
          </div>
          <div id="banner-hive-list" style="max-height:200px;overflow-y:auto;border:1px solid var(--border);border-radius:6px;background:var(--bg);padding:4px"></div>
        </div>
      </div>
      <div style="display:flex;justify-content:flex-end;gap:8px;padding:16px 32px 32px;flex-shrink:0">
        <button onclick="document.getElementById('banner-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>
        <button onclick="sendHubBanner()" class="btn-primary" id="btn-send-banner">Send Banner</button>
      </div>
    </div>
  </div>

  <div id="create-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:640px;width:90%;max-height:90vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 16px;margin:0;color:var(--accent);flex-shrink:0">Create Hosted Hive</h2>
      <div style="flex:1;overflow-y:auto;padding:0 32px">
      <div id="yaml-drop" style="margin-bottom:16px;border:2px dashed var(--border);border-radius:8px;padding:16px;text-align:center;cursor:pointer;transition:border-color 0.2s"
        ondragover="event.preventDefault();this.style.borderColor='var(--accent)'"
        ondragleave="this.style.borderColor='var(--border)'"
        ondrop="event.preventDefault();this.style.borderColor='var(--border)';var f=event.dataTransfer.files[0];if(f)readYamlFile(f)"
        onclick="document.getElementById('yaml-upload').click()">
        <div style="font-size:0.82rem;color:var(--muted)">Drop a <code>hive.yaml</code> here or <span style="color:var(--accent);text-decoration:underline">browse</span></div>
        <div style="font-size:0.7rem;color:var(--muted);margin-top:4px">Auto-fills all fields including GitHub App credentials</div>
        <div id="yaml-download-link" style="display:none;font-size:0.7rem;margin-top:6px"><a id="yaml-download-href" href="#" target="_blank" style="color:var(--accent)" onclick="event.stopPropagation()">⬇ Download hive.yaml from your local hive</a></div>
        <input type="file" id="yaml-upload" accept=".yaml,.yml" style="display:none" onchange="if(this.files[0])readYamlFile(this.files[0])">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">GitHub Organization *</label>
        <input id="f-org" type="text" placeholder="my-org" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Repositories * <span style="font-size:0.7rem">(comma-separated)</span></label>
        <input id="f-repos" type="text" placeholder="repo1, repo2" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Primary Repository</label>
        <input id="f-primary" type="text" placeholder="defaults to first repo" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Project Name</label>
        <input id="f-name" type="text" placeholder="defaults to org/repo" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="display:flex;gap:12px;margin-bottom:12px">
        <div style="flex:1">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">ACMM Level</label>
          <select id="f-level" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <option value="1">L1 — Inception (Assisted)</option>
            <option value="2">L2 — Advisory (Instructed)</option>
            <option value="3" selected>L3 — Quality-Gated (Measured)</option>
            <option value="4">L4 — Security-Aware (Adaptive)</option>
            <option value="5">L5 — Semi-Autonomous (Semi-Automated)</option>
            <option value="6">L6 — Fully Autonomous</option>
          </select>
        </div>
        <div style="flex:1">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Target Cluster</label>
          <select id="f-cluster" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <option value="">Loading clusters...</option>
          </select>
        </div>
      </div>
      <div style="margin-bottom:12px">
        <label style="display:flex;align-items:center;gap:8px;cursor:pointer;font-size:0.8rem;color:var(--muted)"><input type="checkbox" id="f-public" checked> Publicly visible in the hive registry <span style="font-size:0.7rem">(owners can toggle later from My Hives)</span></label>
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Authentication Method</label>
        <div style="display:flex;gap:12px;margin-top:4px;flex-wrap:wrap">
          <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:0.8rem"><input type="radio" name="auth-method" value="pat" checked onchange="document.getElementById('auth-pat').style.display='';document.getElementById('auth-app').style.display='none';document.getElementById('auth-later').style.display='none';document.getElementById('auth-info-pat').style.display='';document.getElementById('auth-info-app').style.display='none';document.getElementById('auth-info-later').style.display='none'"> Personal Access Token</label>
          <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:0.8rem"><input type="radio" name="auth-method" value="app" onchange="document.getElementById('auth-pat').style.display='none';document.getElementById('auth-app').style.display='';document.getElementById('auth-later').style.display='none';document.getElementById('auth-info-pat').style.display='none';document.getElementById('auth-info-app').style.display='';document.getElementById('auth-info-later').style.display='none'"> GitHub App <span style="font-size:0.65rem;color:#3fb950;font-weight:600">Recommended</span></label>
          <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:0.8rem"><input type="radio" name="auth-method" value="later" onchange="document.getElementById('auth-pat').style.display='none';document.getElementById('auth-app').style.display='none';document.getElementById('auth-later').style.display='';document.getElementById('auth-info-pat').style.display='none';document.getElementById('auth-info-app').style.display='none';document.getElementById('auth-info-later').style.display=''"> Configure Later</label>
        </div>
        <div id="auth-info-pat" style="font-size:0.7rem;color:var(--muted);margin-top:8px;line-height:1.5;padding:8px 10px;background:rgba(255,255,255,0.03);border-radius:6px;border:1px solid var(--border)">
          The hive uses this token for all GitHub API calls — creating issues, posting advisory comments, reading repos, pushing code, and merging PRs. All actions appear as the token owner. Permissions cannot be scoped per agent trust tier.
        </div>
        <div id="auth-info-app" style="display:none;font-size:0.7rem;color:var(--muted);margin-top:8px;line-height:1.5;padding:8px 10px;background:rgba(255,255,255,0.03);border-radius:6px;border:1px solid var(--border)">
          The hive generates short-lived installation tokens scoped to each agent's trust tier — newcomers get issues-only access, contributors get code + PR access, and trusted agents can merge. Actions appear as the app, not a personal account. Requires a <a id="auth-info-app-link" href="" target="_blank" rel="noopener" style="color:var(--accent)">GitHub App</a> installed on the target org/repo.
        </div>
        <div id="auth-info-later" style="display:none;font-size:0.7rem;color:var(--muted);margin-top:8px;line-height:1.5;padding:8px 10px;background:rgba(255,255,255,0.03);border-radius:6px;border:1px solid var(--border)">
          The hive will be provisioned with the <strong>kubestellar-hive</strong> GitHub App pre-configured (App ID: 3568013). Agents will be unable to access GitHub until the app is installed on the target org and the installation ID is supplied via the hive config.<br><br>
          <a id="auth-info-later-link" href="" target="_blank" rel="noopener" style="color:var(--accent);font-weight:600">→ Install app now</a>
        </div>
      </div>
      <div id="auth-pat">
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">GitHub Token *</label>
          <input id="f-token" type="password" placeholder="ghp_xxxxxxxxxxxx" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
          <div style="font-size:0.7rem;color:var(--muted);margin-top:6px;line-height:1.5">
            Create a <a href="https://github.com/settings/tokens?type=beta" target="_blank">Fine-grained PAT</a>: Contents, Issues, Pull requests (read/write), Metadata (read).<br>
            Classic tokens (<code>ghp_</code>) work with <code>repo</code> scope.
          </div>
        </div>
      </div>
      <div id="auth-app" style="display:none">
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">App ID *</label>
          <input id="f-app-id" type="text" placeholder="123456" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        </div>
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Installation ID *</label>
          <input id="f-install-id" type="text" placeholder="78901234" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        </div>
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Private Key (PEM) *</label>
          <textarea id="f-app-key" rows="6" placeholder="-----BEGIN RSA PRIVATE KEY-----&#10;Paste or drag a .pem file here...&#10;-----END RSA PRIVATE KEY-----" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.8rem;font-family:monospace;resize:vertical" ondragover="event.preventDefault();this.style.borderColor='var(--accent)'" ondragleave="this.style.borderColor='var(--border)'" ondrop="event.preventDefault();this.style.borderColor='var(--border)';var f=event.dataTransfer.files[0];if(f){var r=new FileReader();r.onload=function(){document.getElementById('f-app-key').value=r.result};r.readAsText(f)}"></textarea>
          <div style="font-size:0.7rem;color:var(--muted);margin-top:4px">Download from your <a href="https://github.com/settings/apps" target="_blank">GitHub App settings</a> → Private keys.</div>
        </div>
      </div>
      <div id="auth-later" style="display:none">
        <div style="margin-bottom:12px;padding:12px;background:rgba(59,130,246,0.08);border:1px solid rgba(59,130,246,0.2);border-radius:8px">
          <div style="font-size:0.85rem;font-weight:600;color:var(--text);margin-bottom:8px">Hive App: kubestellar-hive</div>
          <div style="font-size:0.75rem;color:var(--muted);line-height:1.5">App ID: <code>3568013</code> (pre-configured)<br>The hive will start without GitHub access. Install the app on the target org, then supply the Installation ID and Private Key via the hive config.</div>
          <div id="auth-later-ghe-note" style="display:none;font-size:0.75rem;color:var(--muted);line-height:1.5;margin-top:8px"></div>
          <a id="auth-later-install-link" href="" target="_blank" rel="noopener" style="display:inline-block;margin-top:8px;padding:6px 14px;background:var(--accent);color:#fff;border-radius:6px;font-size:0.8rem;font-weight:600;text-decoration:none">Install app on your org</a>
        </div>
      </div>
      </div>
      <div style="display:flex;gap:12px;justify-content:flex-end;padding:16px 32px;border-top:1px solid var(--border);flex-shrink:0">
        <button onclick="document.getElementById('create-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>
        <button id="btn-go" onclick="createHive()" class="btn-primary">Go</button>
      </div>
    </div>
  </div>

  <div id="access-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:860px;width:min(96vw,860px);max-height:88vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 16px;margin:0;color:var(--accent);flex-shrink:0">Manage Access</h2>
      <div style="flex:1;overflow-y:auto;padding:0 32px 32px">
      <p style="font-size:0.8rem;color:var(--muted);margin-bottom:16px" id="access-hive-label"></p>
      <div id="access-list"><div class="loading">Loading...</div></div>
      <div id="access-bulk-bar" style="display:none;align-items:center;gap:8px;margin-top:8px;padding:8px 10px;background:var(--bg);border:1px solid var(--border);border-radius:6px">
        <span id="access-bulk-count" style="font-size:0.75rem;color:var(--muted);flex:1"></span>
        <select id="access-bulk-role" style="padding:4px 8px;background:var(--surface);border:1px solid var(--border);border-radius:4px;color:var(--text);font-size:0.75rem">
          <option value="read">Read</option>
          <option value="read-write">Read-Write</option>
          <option value="merger">Merger</option>
          <option value="owner">Owner</option>
        </select>
        <button onclick="bulkChangeAccessRole()" class="btn-primary" style="padding:4px 10px;font-size:0.7rem">Change Role</button>
        <button onclick="bulkRemoveAccess()" style="padding:4px 10px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">Remove</button>
      </div>
      <div style="margin-top:12px;border-top:1px solid var(--border);padding-top:12px">
        <h3 style="font-size:0.9rem;margin-bottom:8px;color:var(--accent)">Pending Requests</h3>
        <div id="pending-requests"><span style="color:var(--muted);font-size:0.8rem">Loading...</span></div>
      </div>
      <div style="margin-top:12px;border-top:1px solid var(--border);padding-top:12px">
        <h3 style="font-size:0.9rem;margin-bottom:4px;color:var(--accent)">Audit Log</h3>
        <p style="font-size:0.72rem;color:var(--muted);margin:0 0 8px">Every grant, role change and removal — who did it and when. Append-only.</p>
        <div id="access-audit-log" style="max-height:180px;overflow-y:auto"><span style="color:var(--muted);font-size:0.8rem">Loading...</span></div>
      </div>
      <div style="margin-top:16px;border-top:1px solid var(--border);padding-top:16px">
        <h3 style="font-size:0.9rem;margin-bottom:8px;color:var(--text)">Add User</h3>
        <input id="access-user-search" type="text" placeholder="Search users..." autocomplete="off"
          oninput="filterAccessUserDropdown()"
          style="width:100%;box-sizing:border-box;margin-bottom:8px;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        <div style="display:flex;flex-wrap:wrap;gap:8px">
          <select id="access-username" style="flex:1 1 280px;min-width:220px;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem"><option value="">Select user...</option></select>
          <select id="access-role" onchange="updateAccessRoleHint()" title="Permission level to grant" style="flex:0 0 auto;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <option value="read" title="View-only: dashboard, agents and config. Cannot change anything.">Read</option>
            <option value="read-write" title="Everything Read grants, plus contribute: queue work, manage the queue and open a terminal.">Read-Write</option>
            <option value="merger" title="Everything Read-Write grants, plus approve and queue other contributors' work for auto-merge.">Merger</option>
            <option value="owner" title="Full control: manage access, settings and budget for this hive.">Owner</option>
          </select>
          <!-- A checkbox owns whether the new grant expires; the date input is
               HIDDEN until it is checked.
               An empty <input type="date"> cannot express "no expiry" — the
               browser paints its own mm/dd/yyyy placeholder, so an untouched
               field looks like a date the operator already chose. Adding a
               label alone did not fix it: the control then showed a name and a
               date that disagreed. Hiding the input when unchecked leaves
               exactly one visible answer. -->
          <span style="display:flex;align-items:center;gap:6px;flex:0 0 auto">
            <input type="checkbox" id="access-expiry-enabled" onchange="toggleAddExpiryVisible()"
              title="Off — access lasts until it is removed. On — pick the last day of access."
              style="cursor:pointer;margin:0">
            <label for="access-expiry-enabled" style="font-size:0.7rem;color:var(--muted);cursor:pointer;white-space:nowrap">Expires</label>
            <input id="access-expiry" type="date" title="Access is revoked automatically after this date (UTC)."
              style="display:none;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <span id="access-expiry-never" style="font-size:0.7rem;color:var(--text)">Never</span>
          </span>
          <button onclick="addAccess()" class="btn-primary" style="flex:0 0 auto;padding:8px 16px;font-size:0.8rem">Add</button>
        </div>
        <div id="access-role-hint" style="margin-top:6px;font-size:0.72rem;color:var(--muted);line-height:1.4"></div>
      </div>
      </div>
      <div style="display:flex;justify-content:space-between;align-items:center;padding:16px 32px;border-top:1px solid var(--border);flex-shrink:0">
        <button id="access-export-btn" onclick="exportAccessCSV()" title="Download this hive's access list as CSV for audit/compliance" style="padding:8px 16px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);cursor:pointer;font-size:0.8rem">&#11015;&#65039; Export CSV</button>
        <button onclick="document.getElementById('access-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Close</button>
      </div>
    </div>
  </div>

  <div id="timeline-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:640px;width:92%;max-height:82vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 8px;margin:0;color:var(--accent);flex-shrink:0">Activity Timeline</h2>
      <p style="font-size:0.8rem;color:var(--muted);margin:0;padding:0 32px 16px;flex-shrink:0" id="timeline-hive-label"></p>
      <div style="flex:1;overflow-y:auto;padding:0 32px 24px">
        <div id="timeline-list"><div class="loading">Loading...</div></div>
      </div>
      <div style="display:flex;justify-content:flex-end;padding:16px 32px;border-top:1px solid var(--border);flex-shrink:0">
        <button onclick="document.getElementById('timeline-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Close</button>
      </div>
    </div>
  </div>

  <div id="request-access-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:480px;width:90%;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 8px;margin:0;color:var(--accent)">Request Access</h2>
      <div style="padding:0 32px 24px">
        <p style="font-size:0.85rem;color:var(--muted);margin-bottom:12px">Requesting access to <strong id="request-access-hive-label" style="color:var(--text)"></strong>. The owner will review your request.</p>
        <label for="request-access-note" style="display:block;font-size:0.8rem;color:var(--text);margin-bottom:6px">Why do you need access? <span style="color:var(--red)">*</span></label>
        <textarea id="request-access-note" rows="4" placeholder="Explain why you should be granted access to this hive..." style="width:100%;padding:10px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;resize:vertical;box-sizing:border-box"></textarea>
      </div>
      <div style="display:flex;justify-content:flex-end;gap:8px;padding:16px 32px;border-top:1px solid var(--border)">
        <button onclick="closeRequestAccessModal()" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>
        <button id="request-access-submit" onclick="submitRequestAccess()" class="btn-primary" style="padding:8px 20px">Send Request</button>
      </div>
    </div>
  </div>


  <script>
    var _accessHiveId = '';
    // Last-loaded access list for the open manage-access modal; lets
    // removeAccess() detect self-removal and last-owner cases client-side.
    var _accessUsers = [];
    var _timelineHiveId = '';

    /* Per-event presentation: a colour and a short label per event kind, so the
       timeline is scannable rather than a wall of text. Kinds come from the
       Timeline* constants in timeline.go; unknown kinds fall through to a
       neutral default so an older dashboard never renders a blank row. */
    var TIMELINE_KINDS = {
      version_changed:   { label: 'Version',    color: '#3fb950' },
      branch_changed:    { label: 'Branch',     color: '#60a5fa' },
      went_offline:      { label: 'Offline',    color: '#f85149' },
      came_online:       { label: 'Online',     color: '#3fb950' },
      health_changed:    { label: 'Health',     color: '#f59e0b' },
      upgrade_started:   { label: 'Upgrade',    color: '#60a5fa' },
      upgrade_completed: { label: 'Upgraded',   color: '#3fb950' },
      upgrade_stale:     { label: 'Stuck',      color: '#f85149' },
      restarted:         { label: 'Restart',    color: '#f59e0b' },
      acmm_changed:      { label: 'ACMM',       color: '#a371f7' },
      agents_changed:    { label: 'Agents',     color: '#a371f7' },
      github_app_changed:{ label: 'GitHub App', color: '#f59e0b' },
      access:            { label: 'Access',     color: '#60a5fa' },
      ownership:         { label: 'Ownership',  color: '#60a5fa' },
      admin:             { label: 'Admin',      color: '#8b949e' }
    };

    /* Relative time, coarse on purpose: "what happened to this hive" is
       answered by ordering and rough age, not by seconds. */
    var TIMELINE_MS_PER_MIN = 60000;
    var TIMELINE_MIN_PER_HOUR = 60;
    var TIMELINE_HOURS_PER_DAY = 24;
    function timelineAgo(ts) {
      var t = Date.parse(ts);
      if (isNaN(t)) return '';
      var mins = Math.floor((Date.now() - t) / TIMELINE_MS_PER_MIN);
      if (mins < 1) return 'just now';
      if (mins < TIMELINE_MIN_PER_HOUR) return mins + 'm ago';
      var hours = Math.floor(mins / TIMELINE_MIN_PER_HOUR);
      if (hours < TIMELINE_HOURS_PER_DAY) return hours + 'h ago';
      return Math.floor(hours / TIMELINE_HOURS_PER_DAY) + 'd ago';
    }

    async function openTimelineModal(hiveId, hiveName) {
      _timelineHiveId = hiveId;
      document.getElementById('timeline-hive-label').textContent = hiveName || hiveId;
      document.getElementById('timeline-list').innerHTML = '<div class="loading">Loading...</div>';
      document.getElementById('timeline-modal').style.display = 'flex';
      await loadTimeline();
    }

    async function loadTimeline() {
      var el = document.getElementById('timeline-list');
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_timelineHiveId) + '/timeline');
        if (!resp.ok) {
          el.innerHTML = '<div style="color:var(--red);font-size:0.85rem">Could not load this hive\'s timeline.</div>';
          return;
        }
        var data = await resp.json();
        var events = (data && data.events) || [];
        if (events.length === 0) {
          el.innerHTML = '<div style="color:var(--muted);font-size:0.85rem">No recorded activity yet. Events appear here when something actually changes — a version lands, health flips, the hive restarts or goes offline.</div>';
          return;
        }
        /* Server returns newest first; render in that order. */
        el.innerHTML = events.map(function(ev) {
          var kind = TIMELINE_KINDS[ev.kind] || { label: ev.kind || 'Event', color: '#8b949e' };
          /* actorName resolves opaque OIDC identities server-side. */
          var actor = ev.actor
            ? '<span style="color:var(--muted);font-size:0.7rem;margin-left:6px">by ' + esc(ev.actorName || ev.actor) + '</span>'
            : '';
          return '<div style="display:flex;gap:10px;padding:9px 0;border-bottom:1px solid var(--border)">' +
            '<div style="flex-shrink:0;width:84px"><span style="display:inline-block;padding:2px 7px;border-radius:9999px;font-size:0.62rem;font-weight:600;white-space:nowrap;background:' + kind.color + '22;color:' + kind.color + ';border:1px solid ' + kind.color + '55">' + esc(kind.label) + '</span></div>' +
            '<div style="flex:1;min-width:0">' +
              '<div style="font-size:0.82rem;color:var(--text);word-break:break-word">' + esc(ev.detail || '') + actor + '</div>' +
              '<div style="font-size:0.68rem;color:var(--muted);margin-top:2px" title="' + esc(ev.ts || '') + '">' + esc(timelineAgo(ev.ts)) + '</div>' +
            '</div></div>';
        }).join('');
      } catch(e) {
        el.innerHTML = '<div style="color:var(--red);font-size:0.85rem">Failed to load timeline: ' + esc(e.message) + '</div>';
      }
    }

    async function openAccessModal(hiveId, dashUrl) {
      _accessHiveId = hiveId;
      // Show the hive's URL alongside its id. The raw placeholder id
      // (hosted-available-oke-05-placeholder-6q84) says nothing about where the
      // hive actually lives, and the vanity URL is what an owner recognises.
      var label = document.getElementById('access-hive-label');
      label.textContent = 'Hive: ' + hiveId;
      var urlEl = document.getElementById('access-hive-url');
      if (!urlEl) {
        urlEl = document.createElement('div');
        urlEl.id = 'access-hive-url';
        urlEl.style.cssText = 'padding:0 32px 4px;font-size:0.8rem';
        label.parentNode.insertBefore(urlEl, label.nextSibling);
      }
      if (dashUrl && dashUrl.indexOf('localhost') === -1) {
        urlEl.innerHTML = '<a href="' + esc(dashUrl) + '" target="_blank" rel="noopener" style="color:var(--blue);text-decoration:none">' +
          esc(dashUrl.replace(/^https?:\/\//, '')) + '</a>';
        urlEl.style.display = '';
      } else {
        urlEl.textContent = '';
        urlEl.style.display = 'none';
      }
      document.getElementById('access-modal').style.display = 'flex';
      updateAccessRoleHint();
      _bulkAccessSel = {};
      updateBulkAccessBar();
      await loadAccessList();
      await loadAccessUserDropdown();
      await loadPendingRequests();
      await loadAccessAuditLog();
    }

    /* Audit log for permission changes (#4148): a read-only, append-only view
       sourced from the hive timeline, filtered server-side to access/ownership
       events. Only owners can open Manage Access, and the endpoint enforces
       the same rule, so a non-owner never sees this. */
    async function loadAccessAuditLog() {
      var el = document.getElementById('access-audit-log');
      if (!el) return;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access-log');
        if (!resp.ok) {
          el.innerHTML = '<span style="color:var(--muted);font-size:0.8rem">Audit log unavailable</span>';
          return;
        }
        var data = await resp.json();
        var events = (data && data.events) || [];
        if (!events.length) {
          el.innerHTML = '<span style="color:var(--muted);font-size:0.8rem">No permission changes recorded yet</span>';
          return;
        }
        /* Server returns newest first; render in that order. */
        el.innerHTML = events.map(function(ev) {
          /* actorName resolves opaque OIDC identities server-side. */
          var actor = ev.actor
            ? '<span style="color:var(--muted);font-size:0.7rem;margin-left:6px">by ' + esc(ev.actorName || ev.actor) + '</span>'
            : '';
          return '<div style="padding:6px 0;border-bottom:1px solid var(--border)">' +
            '<div style="font-size:0.78rem;color:var(--text);word-break:break-word">' + esc(ev.detail || '') + actor + '</div>' +
            '<div style="font-size:0.66rem;color:var(--muted);margin-top:2px" title="' + esc(ev.ts || '') + '">' + esc(timelineAgo(ev.ts)) + '</div>' +
            '</div>';
        }).join('');
      } catch(e) {
        el.innerHTML = '<span style="color:var(--muted);font-size:0.8rem">Audit log unavailable</span>';
      }
    }

    async function loadPendingRequests() {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests');
        if (!resp.ok) return;
        var data = await resp.json();
        var reqs = data.requests || [];
        var el = document.getElementById('pending-requests');
        if (!el) return;
        if (!reqs.length) { el.innerHTML = '<span style="color:var(--muted);font-size:0.8rem">No pending requests</span>'; return; }
        el.innerHTML = reqs.map(function(r) {
          var avatar = linkedAvatar(r.username, LIST_AVATAR_PX, r.username, 'margin-right:6px');
          var note = (r.note || '').trim();
          var noteHtml = note
            ? '<div style="margin-top:4px;font-size:0.75rem;color:var(--text);white-space:pre-wrap;word-break:break-word;background:var(--bg);border-left:2px solid var(--accent);padding:4px 8px;border-radius:2px">' + esc(note) + '</div>'
            : '<div style="margin-top:4px;font-size:0.72rem;color:var(--muted);font-style:italic">(no note)</div>';
          return '<div style="padding:6px 0;border-bottom:1px solid var(--border)">' +
            '<div style="display:flex;align-items:center;justify-content:space-between">' +
            '<div>' + avatar + '<span style="font-size:0.85rem">' + esc(r.username) + '</span> <span style="font-size:0.7rem;color:var(--muted)">' + esc(r.requested_at.substring(0,10)) + '</span></div>' +
            '<div style="display:flex;gap:4px">' +
            '<select id="req-role-' + esc(r.username) + '" title="Role to grant on approval" style="padding:2px 6px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);font-size:0.7rem"><option value="read" title="' + escAttr(roleDescription('read')) + '">Read</option><option value="read-write" title="' + escAttr(roleDescription('read-write')) + '">Read-Write</option><option value="merger" title="' + escAttr(roleDescription('merger')) + '">Merger</option></select>' +
            '<button onclick="approveRequest(\'' + esc(r.username) + '\')" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Approve</button>' +
            '<button onclick="denyRequest(\'' + esc(r.username) + '\')" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Deny</button>' +
            '</div></div>' + noteHtml + '</div>';
        }).join('');
      } catch(e) {}
    }

    async function approveRequest(username) {
      var role = (document.getElementById('req-role-' + username) || {}).value || 'read';
      try {
        await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests/' + encodeURIComponent(username) + '/approve', {
          method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({role: role})
        });
        loadPendingRequests();
        loadAccessList();
        loadAccessAuditLog();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function denyRequest(username) {
      try {
        await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests/' + encodeURIComponent(username) + '/deny', {method: 'POST'});
        loadPendingRequests();
        loadAccessAuditLog();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    /* ── GitHub display-name enrichment (#4145) ─────────────────────────
       The Manage Access rows and the Add User picker key on the stable login,
       but a human recognizes "Andy Anderson" faster than "clubanderson". The
       GitHub profile API carries that name, so we fetch it lazily and paint it
       in AFTER the username is already on screen — enrichment only ever ADDS
       information, so a failed/rate-limited lookup leaves the UI exactly as it
       was (graceful fallback to username, no blocking delay).

       Results are cached twice: in-memory for this page, and in localStorage
       for a day so re-opening the dialog doesn't re-spend the unauthenticated
       api.github.com rate budget (60 req/hr/IP). Confirmed misses (404) are
       cached like hits; transient failures (rate limit, network) are cached
       for this page only so the next page load can retry. */
    var GH_NAME_CACHE_KEY = 'hiveGhDisplayNames';
    var GH_NAME_CACHE_TTL_MS = 24 * 60 * 60 * 1000;
    var _ghNameMem = {};      /* login(lower) -> name string | null (known-none) */
    var _ghNamePending = {};  /* login(lower) -> in-flight Promise (dedupes) */

    function ghNameCacheRead() {
      try { return JSON.parse(localStorage.getItem(GH_NAME_CACHE_KEY) || '{}') || {}; }
      catch (e) { return {}; }
    }
    function ghNameCacheWrite(login, name) {
      try {
        var c = ghNameCacheRead();
        c[login] = { n: name, t: Date.now() };
        localStorage.setItem(GH_NAME_CACHE_KEY, JSON.stringify(c));
      } catch (e) {}
    }
    /* Only plain github.com logins have a profile to ask about; OIDC identity
       keys ("google:1078…") never do — same ":" rule as splitIdentityKey. */
    function isPlainGitHubLogin(username) {
      var u = String(username || '');
      return u !== '' && u.indexOf(':') === -1;
    }
    /* fetchGitHubDisplayName resolves to the profile name for a github.com
       login, or null when there is none / it cannot be fetched. Never rejects. */
    function fetchGitHubDisplayName(username) {
      var login = String(username || '').toLowerCase();
      if (!isPlainGitHubLogin(login)) return Promise.resolve(null);
      if (login in _ghNameMem) return Promise.resolve(_ghNameMem[login]);
      var cached = ghNameCacheRead()[login];
      if (cached && (Date.now() - (cached.t || 0)) < GH_NAME_CACHE_TTL_MS) {
        _ghNameMem[login] = cached.n || null;
        return Promise.resolve(_ghNameMem[login]);
      }
      if (_ghNamePending[login]) return _ghNamePending[login];
      var p = fetch('https://api.github.com/users/' + encodeURIComponent(login))
        .then(function(r) {
          if (r.status === 404) return null;      /* confirmed no such user: cache the miss */
          if (!r.ok) return undefined;            /* transient (rate limit etc): don't persist */
          return r.json().then(function(d) { return (d && d.name) ? String(d.name) : null; },
                               function() { return undefined; });
        }, function() { return undefined; })
        .then(function(name) {
          delete _ghNamePending[login];
          if (name === undefined) { _ghNameMem[login] = null; return null; }
          _ghNameMem[login] = name;
          ghNameCacheWrite(login, name);
          return name;
        });
      _ghNamePending[login] = p;
      return p;
    }
    /* enrichGhDisplayNames fills every .gh-display-name placeholder under root
       once its profile name arrives. The placeholder is rendered empty next to
       the username, so nothing shifts or blocks while lookups are in flight,
       and a name identical to the login is skipped (it would only repeat). */
    function enrichGhDisplayNames(root) {
      if (!root) return;
      var els = root.querySelectorAll('.gh-display-name[data-gh-login]');
      Array.prototype.forEach.call(els, function(el) {
        var login = el.getAttribute('data-gh-login') || '';
        fetchGitHubDisplayName(login).then(function(name) {
          if (!name || name.toLowerCase() === login.toLowerCase()) return;
          if (!el.isConnected) return; /* the list re-rendered meanwhile */
          el.textContent = name;
        });
      });
    }

    /* The roster fetched from grantable-users, kept so the search box can
       re-filter without another network round trip. Each entry carries the
       stable identity key (id — what the grant POST sends) and the normalized
       human label (label — what the owner reads). */
    var _grantableUsers = [];

    /* enrichGrantableUserLabels upgrades GitHub rows whose label is still the
       bare login with the profile display name, then re-renders the dropdown
       ONCE after the batch settles — preserving the active filter and any
       selection (renderAccessUserOptions already restores both). Rows the hub
       already labeled (OIDC display_name) are left alone. */
    function enrichGrantableUserLabels() {
      var updated = false, waiting = 0;
      _grantableUsers.forEach(function(e) {
        if (!e || e.id !== e.label) return;
        if (e.provider && e.provider !== 'github') return;
        if (!isPlainGitHubLogin(e.id)) return;
        waiting++;
        fetchGitHubDisplayName(e.id).then(function(name) {
          waiting--;
          if (name && name !== e.label) { e.label = name; updated = true; }
          if (waiting === 0 && updated) filterAccessUserDropdown();
        });
      });
    }

    /* identityProviderFromKey classifies a raw identity key by its prefix so
       the UI can show a provider mark next to the name: "google:…" → google,
       "ibmid:…" → ibmid, "microsoft:…" or "ms:…" → microsoft, a "github.com/…"
       prefix or a bare login (no prefix at all) → github. Unknown prefixes
       pass through unchanged and render as the generic person icon. */
    function identityProviderFromKey(key) {
      key = String(key || '');
      if (key.indexOf('github.com/') === 0) return 'github';
      var m = key.match(/^([a-z][a-z0-9_-]*):/);
      if (!m) return 'github';
      var p = m[1];
      if (p === 'ms') return 'microsoft';
      return p;
    }

    /* PROVIDER_ICON_SVG: small inline SVG provider marks — inline so no
       external fetches happen (CSP-safe). Sized 14px to sit alongside the
       0.85rem row labels. Providers without an SVG fall back to emoji. */
    var PROVIDER_ICON_SVG = {
      github: '<svg viewBox="0 0 16 16" width="14" height="14" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z"/></svg>',
      google: '<svg viewBox="0 0 48 48" width="14" height="14" aria-hidden="true"><path fill="#EA4335" d="M24 9.5c3.54 0 6.71 1.22 9.21 3.6l6.85-6.85C35.9 2.38 30.47 0 24 0 14.62 0 6.51 5.38 2.56 13.22l7.98 6.19C12.43 13.72 17.74 9.5 24 9.5z"/><path fill="#4285F4" d="M46.98 24.55c0-1.57-.15-3.09-.38-4.55H24v9.02h12.94c-.58 2.96-2.26 5.48-4.78 7.18l7.73 6c4.51-4.18 7.09-10.36 7.09-17.65z"/><path fill="#FBBC05" d="M10.53 28.59c-.48-1.45-.76-2.99-.76-4.59s.27-3.14.76-4.59l-7.98-6.19C.92 16.46 0 20.12 0 24c0 3.88.92 7.54 2.56 10.78l7.97-6.19z"/><path fill="#34A853" d="M24 48c6.48 0 11.93-2.13 15.89-5.81l-7.73-6c-2.15 1.45-4.92 2.3-8.16 2.3-6.26 0-11.57-4.22-13.47-9.91l-7.98 6.19C6.51 42.62 14.62 48 24 48z"/></svg>',
      microsoft: '<svg viewBox="0 0 23 23" width="14" height="14" aria-hidden="true"><rect x="1" y="1" width="10" height="10" fill="#f25022"/><rect x="12" y="1" width="10" height="10" fill="#7fba00"/><rect x="1" y="12" width="10" height="10" fill="#00a4ef"/><rect x="12" y="12" width="10" height="10" fill="#ffb900"/></svg>',
      ibmid: '<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"><g fill="#0f62fe"><rect x="1" y="3" width="14" height="2"/><rect x="1" y="7" width="14" height="2"/><rect x="1" y="11" width="14" height="2"/></g></svg>'
    };

    /* PROVIDER_EMOJI: plain-text stand-ins for native <select> options, which
       cannot render SVG. Anything unmapped shows the generic person glyph. */
    var PROVIDER_EMOJI = { google: '🔵', ibmid: '🔷', microsoft: '🟦', github: '🐙' };

    /* providerIconHTML returns a small inline icon (SVG when we have one,
       emoji otherwise) vertically aligned with the label text beside it. */
    function providerIconHTML(provider) {
      var body = PROVIDER_ICON_SVG[provider] || '👤';
      return '<span title="' + esc(provider || 'unknown provider') + '" style="display:inline-flex;align-items:center;vertical-align:middle;margin-right:5px;width:14px;height:14px;line-height:1;font-size:12px">' + body + '</span>';
    }

    /* providerOptionEmoji is the <option>-safe variant of providerIconHTML. */
    function providerOptionEmoji(provider) {
      return (PROVIDER_EMOJI[provider] || '👤') + ' ';
    }

    /* accessOptionText renders one dropdown row: a provider mark, the friendly
       label, plus a short parenthesised hint of the underlying ID when the two
       differ so an owner can tell two "Jane Doe"s apart without seeing a full
       raw token. */
    function accessOptionText(e) {
      var icon = providerOptionEmoji(e.provider || identityProviderFromKey(e.id));
      if (!e.id || e.id === e.label) return icon + e.label;
      var hint = e.id.length > 24 ? e.id.slice(0, 24) + '…' : e.id;
      return icon + e.label + ' (' + hint + ')';
    }

    /* renderAccessUserOptions rebuilds the select from _grantableUsers,
       keeping only entries whose label OR raw id matches the filter
       (case-insensitive). The current selection is preserved when it
       survives the filter so typing never silently discards a choice. */
    function renderAccessUserOptions(filter) {
      var sel = document.getElementById('access-username');
      if (!sel) return;
      var prev = sel.value;
      var q = (filter || '').toLowerCase();
      var matches = _grantableUsers.filter(function(e) {
        if (!q) return true;
        return e.label.toLowerCase().indexOf(q) !== -1 || e.id.toLowerCase().indexOf(q) !== -1;
      });
      if (!_grantableUsers.length) {
        sel.innerHTML = '<option value="">No users yet — they must sign in to the hub once</option>';
        return;
      }
      if (!matches.length) {
        sel.innerHTML = '<option value="">No users match "' + esc(filter) + '"</option>';
        return;
      }
      sel.innerHTML = '<option value="">Select user...</option>' + matches.map(function(e) {
        return '<option value="' + esc(e.id) + '">' + esc(accessOptionText(e)) + '</option>';
      }).join('');
      if (prev && matches.some(function(e) { return e.id === prev; })) sel.value = prev;
    }

    function filterAccessUserDropdown() {
      var box = document.getElementById('access-user-search');
      renderAccessUserOptions(box ? box.value : '');
    }

    async function loadAccessUserDropdown() {
      var sel = document.getElementById('access-username');
      if (!sel) return;
      var box = document.getElementById('access-user-search');
      if (box) box.value = '';
      try {
        // grantable-users, NOT admin/users: the latter is admin-only, so every
        // non-admin owner got a 403 and an empty dropdown with no explanation.
        var resp = await fetch('/api/saas/grantable-users');
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        var data = await resp.json();
        // Prefer the enriched entries (stable id + normalized label); fall back
        // to the bare username list from an older hub, where id doubles as label.
        _grantableUsers = (data.entries || (data.users || []).map(function(u) {
          return { id: u, label: u };
        })).filter(function(e) { return e && e.id; });
        renderAccessUserOptions('');
        // Usernames are on screen already; profile names swap in when they
        // arrive (#4145) — never block the dropdown on the GitHub API.
        enrichGrantableUserLabels();
      } catch(e) {
        // Never leave the control looking merely empty — an empty dropdown is
        // indistinguishable from "no such users", which is what made this
        // confusing in the first place.
        _grantableUsers = [];
        sel.innerHTML = '<option value="">Could not load users</option>';
      }
    }

    async function loadAccessList() {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access');
        var data = await resp.json();
        var users = data.access || [];
        _accessUsers = users;
        // Drop selections for users no longer on the roster.
        Object.keys(_bulkAccessSel).forEach(function(name) {
          if (!users.some(function(u) { return u.username === name; })) delete _bulkAccessSel[name];
        });
        if (!users.length) {
          document.getElementById('access-list').innerHTML = '<div style="color:var(--muted);font-size:0.85rem">No users have access yet</div>';
          updateBulkAccessBar();
          return;
        }
        var ownerCount = users.filter(function(u) { return u.role === 'owner'; }).length;
        var rows = users.map(function(u) {
          // u.username is the raw identity key — the actual auth key and
          // allowlist match — and is NEVER altered for display. u.display_label
          // is resolved hub-side (accessForHive → provisionRequestUserIdentity,
          // the SAME precedence used everywhere else a friendly name is shown)
          // and always falls back to u.username, so hasFriendlyName is exactly
          // "the hub found something better than the raw key".
          var provider = u.provider || identityProviderFromKey(u.username);
          var hasFriendlyName = !!(u.display_label && u.display_label !== u.username);
          var avatar = provider === 'github'
            ? linkedAvatar(u.username, LIST_AVATAR_PX,
                String(u.username || '') + (u.role ? ' — ' + u.role : ''), 'margin-right:6px')
            : userAvatar({display_name: u.display_label, avatar_url: u.avatar_url, github_username: u.username},
                LIST_AVATAR_PX, 'margin-right:6px');
          // The last owner can be neither removed nor demoted — doing so would
          // orphan the hive with no one able to manage access.
          var isLastOwner = (u.role === 'owner' && ownerCount <= 1);
          var canRemove = !isLastOwner;
          // The last owner gets no checkbox: it can't be bulk-removed or
          // bulk-demoted, so offering to select it would only mislead.
          var checkbox = isLastOwner ?
            '<span style="display:inline-block;width:13px;margin-right:8px"></span>' :
            '<input type="checkbox" class="access-bulk-cb" style="margin:0 8px 0 0;cursor:pointer;vertical-align:middle"' +
              (_bulkAccessSel[u.username] ? ' checked' : '') +
              ' onchange="toggleBulkAccess(this, \'' + esc(u.username) + '\')" title="Select for bulk actions">';
          var removeBtn = canRemove ?
            '<button onclick="removeAccess(\'' + esc(u.username) + '\')" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Remove</button>' :
            '<span style="font-size:0.6rem;color:var(--muted)">last owner</span>';
          // The role pill is an editable dropdown: changing it POSTs the new role
          // (the add endpoint upserts). The last owner's role is locked (shown as
          // a static pill) so the hive can't be left without an owner.
          var ROLES = ['read', 'read-write', 'merger', 'owner'];
          var roleControl = isLastOwner ?
            '<span class="role-badge role-' + u.role.replace(' ','-') + '" style="font-size:0.7rem" title="The last owner\'s role cannot be changed">' + esc(u.role) + '</span>' :
            '<select class="role-select role-' + u.role.replace(' ','-') + '" style="font-size:0.7rem;padding:2px 6px;border-radius:9999px;cursor:pointer" title="Change this user\'s permission" onchange="changeAccessRole(\'' + esc(u.username) + '\', this.value, \'' + esc(u.role) + '\')">' +
              ROLES.map(function(r) { return '<option value="' + r + '" title="' + escAttr(roleDescription(r)) + '"' + (r === u.role ? ' selected' : '') + '>' + r + '</option>'; }).join('') +
            '</select>';
          /* Last-active is coarse and relative on purpose ("3d ago") — the
             owner's question is "is this account dormant?", not "when exactly".
             The absolute time rides in the title. "—" means never active. */
          var lastActive = u.last_active ?
            '<span style="font-size:0.7rem;color:var(--muted)" title="Last active ' + esc(fmtUserTS(u.last_active)) + '">' + esc(timelineAgo(u.last_active) || fmtUserTS(u.last_active)) + '</span>' :
            '<span style="font-size:0.7rem;color:var(--muted)" title="Never active on this hub">—</span>';
          // The expiry editor doubles as the display: it shows the grant's
          // last valid day when set, and empty means permanent. Changing it
          // extends (or clears) the expiry; the last owner cannot be expired
          // for the same reason they cannot be removed or demoted.
          // A checkbox owns whether this grant expires; the date input EXISTS
          // only when it does.
          //
          // A bare <input type="date"> cannot express "no expiry": an empty one
          // still paints the browser's mm/dd/yyyy placeholder, so a permanent
          // grant and one expiring today look the same. Labelling it was not
          // enough — the row then read "Expires: Never 08/27/2026", stating
          // both answers at once and leaving the operator to guess which the
          // system believed. Removing the control entirely when unchecked is
          // the only version with exactly one visible answer.
          var hasExpiry = !!u.expires_at;
          var expiryToggleId = 'expcb-' + esc(u.username).replace(/[^A-Za-z0-9_-]/g, '_');
          // Checking the box needs a date to submit; today would expire the
          // grant immediately, so default to 30 days out and let the operator
          // adjust. Unchecking sends '' — the API's existing "permanent".
          var expiryControl = isLastOwner ? '' :
            '<span style="display:inline-flex;align-items:center;gap:4px" title="' +
              (hasExpiry ? 'Access is revoked automatically after this date (UTC).' : 'No expiry — this grant lasts until it is removed.') + '">' +
            '<input type="checkbox" id="' + expiryToggleId + '"' + (hasExpiry ? ' checked' : '') +
            ' onchange="toggleAccessExpiry(\'' + esc(u.username) + '\', \'' + esc(u.role) + '\', this.checked)"' +
            ' style="cursor:pointer;margin:0">' +
            '<label for="' + expiryToggleId + '" style="font-size:0.6rem;color:var(--muted);cursor:pointer">Expires</label>' +
            (hasExpiry ?
              '<input type="date" class="access-expiry-input" value="' + esc(expiryToDateInput(u.expires_at)) + '"' +
              ' onchange="changeAccessExpiry(\'' + esc(u.username) + '\', \'' + esc(u.role) + '\', this.value)"' +
              ' title="Access is revoked automatically after this date (UTC)."' +
              ' style="font-size:0.65rem;padding:2px 4px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--amber)">'
              : '<span style="font-size:0.6rem;color:var(--text)">Never</span>') +
            '</span>';
          // Primary label: the resolved friendly name when the hub found one,
          // else the raw key exactly as before. The raw key is NEVER hidden —
          // it rides as a muted secondary line (and the avatar's title) any
          // time a friendly name is shown, so support/debugging always has it
          // one glance away. A GitHub user whose label is still the bare
          // login keeps the existing async profile-name enrichment
          // (.gh-display-name / enrichGhDisplayNames, #4145).
          var primaryLabel = hasFriendlyName ? u.display_label : u.username;
          var rawKeyLine = hasFriendlyName
            ? '<span style="display:block;font-size:0.7rem;color:var(--muted);word-break:break-word" title="Auth key">' + esc(u.username) + '</span>'
            : '';
          var ghEnrichPlaceholder = (provider === 'github' && !hasFriendlyName)
            /* Empty placeholder the async GitHub profile lookup fills in
               (#4145): display name lands beside the username when it arrives,
               and stays empty on any failure — the login always renders first. */
            ? '<span class="gh-display-name" data-gh-login="' + escAttr(u.username) + '" style="margin-left:6px;font-size:0.75rem;color:var(--muted)"></span>'
            : '';
          return '<div style="display:flex;flex-wrap:wrap;align-items:center;justify-content:space-between;gap:8px;padding:8px 0;border-bottom:1px solid var(--border)">' +
            '<div style="display:flex;align-items:center;gap:4px;flex:1 1 240px;min-width:0">' + checkbox + avatar + providerIconHTML(provider) +
            '<span style="font-size:0.85rem;word-break:break-word;min-width:0" title="' + escAttr(u.username) + '">' + esc(primaryLabel) + rawKeyLine + '</span>' +
            ghEnrichPlaceholder + '</div>' +
            '<div style="display:flex;align-items:center;justify-content:flex-end;flex-wrap:wrap;gap:8px;flex:1 1 300px;min-width:0">' +
            lastActive +
            roleControl +
            expiryControl +
            removeBtn +
            '</div></div>';
        }).join('');
        document.getElementById('access-list').innerHTML = rows;
        enrichGhDisplayNames(document.getElementById('access-list'));
        updateBulkAccessBar();
      } catch(e) {
        document.getElementById('access-list').innerHTML = '<div style="color:var(--red)">Failed to load</div>';
      }
    }

    /* RFC-4180-style field quoting: wrap in double quotes when the value
       contains a comma, quote, or newline; embedded quotes double up. */
    function csvField(v) {
      var s = String(v == null ? '' : v);
      if (/[",\r\n]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
      return s;
    }

    /* exportAccessCSV downloads the currently loaded access list (_accessUsers,
       cached by loadAccessList) as CSV, entirely client-side (Blob + synthetic
       <a> click) — no extra round trip and, critically, no server-side file is
       ever written (#4152). Columns follow the audit shape from #4152:
       username, role, granted date (blank — grants are not timestamped today,
       the column keeps exports forward-compatible) and last-active (the user's
       most recent hub activity — see latestUserActivity). Only owners can
       load the list at all, so the button is inherently owner-scoped. */
    function exportAccessCSV() {
      if (!_accessUsers.length) { hiveToast('Nothing to export yet', 'error'); return; }
      var lines = ['username,role,granted_at,last_active'];
      _accessUsers.forEach(function(u) {
        lines.push([
          csvField(u.username),
          csvField(u.role),
          '', // granted date: not tracked per-grant yet
          csvField(u.last_active || '')
        ].join(','));
      });
      var blob = new Blob([lines.join('\r\n') + '\r\n'], {type: 'text/csv;charset=utf-8'});
      var a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = 'hive-access-' + (_accessHiveId || 'unknown') + '-' + new Date().toISOString().slice(0, 10) + '.csv';
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      URL.revokeObjectURL(a.href);
      hiveToast('Access list exported', 'success');
    }

    /* expiryToDateInput maps a stored RFC3339 expiry to the value a
       <input type=date> shows: the grant's LAST VALID day (UTC). The server
       canonicalizes a picked date D to midnight UTC of D+1, so stepping the
       instant back one second always lands on the through-day, for exact
       midnights and arbitrary timestamps alike. Empty in → empty out. */
    function expiryToDateInput(iso) {
      if (!iso) return '';
      var t = Date.parse(iso);
      if (isNaN(t)) return '';
      return new Date(t - 1000).toISOString().slice(0, 10);
    }

    async function addAccess() {
      var username = document.getElementById('access-username').value;
      var role = document.getElementById('access-role').value;
      var expiry = document.getElementById('access-expiry').value;
      if (!username) { hiveToast('Select a user', 'error'); return; }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          // expires_at always rides on the Add path: the picked date, or ""
          // (permanent) when the picker was left empty — never omitted, so a
          // re-add of an existing user resets the expiry to what the form shows.
          body: JSON.stringify({username: username, role: role, expires_at: expiry || ''})
        });
        if (!resp.ok) { var d = await resp.json(); hiveToast(d.error || 'Failed', 'error'); return; }
        document.getElementById('access-username').value = '';
        // Reset the checkbox too, not just the date: clearing one without the
        // other leaves the form claiming an expiry is set while submitting ''
        // (permanent), which is the desync this control exists to prevent.
        document.getElementById('access-expiry-enabled').checked = false;
        toggleAddExpiryVisible();
        loadAccessList();
        loadAccessAuditLog();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    /* changeAccessExpiry sets, extends, or clears (empty value) the expiry on
       an existing grant. The role is re-sent unchanged — the add endpoint
       upserts — and expires_at carries the new bound ("" = permanent). */
    // toggleAddExpiryVisible shows the Add User date input only while the
    // checkbox is on, and swaps in a literal "Never" while it is off, so the
    // row always states one answer rather than showing a placeholder date the
    // operator never chose.
    function toggleAddExpiryVisible() {
      var on = document.getElementById('access-expiry-enabled').checked;
      var input = document.getElementById('access-expiry');
      var never = document.getElementById('access-expiry-never');
      input.style.display = on ? '' : 'none';
      if (never) never.style.display = on ? 'none' : '';
      if (on) {
        // Seed a usable date so checking the box cannot submit an empty value
        // (which the API reads as permanent — the opposite of what was asked).
        if (!input.value) {
          var d = new Date();
          d.setDate(d.getDate() + defaultExpiryDays);
          input.value = d.toISOString().slice(0, 10);
        }
      } else {
        input.value = '';
      }
    }

    // defaultExpiryDays is how far out the date lands when an operator turns
    // expiry ON. It must not be 0: checking the box would then expire the
    // grant the moment it was checked, which reads as the UI revoking access
    // rather than scheduling it.
    var defaultExpiryDays = 30;

    // toggleAccessExpiry flips a grant between permanent and expiring.
    // Unchecking sends '' — the API's existing "permanent" — and checking
    // sends a real date, because the server cannot store "expires, date TBD".
    async function toggleAccessExpiry(username, role, checked) {
      if (!checked) { changeAccessExpiry(username, role, ''); return; }
      var d = new Date();
      d.setDate(d.getDate() + defaultExpiryDays);
      changeAccessExpiry(username, role, d.toISOString().slice(0, 10));
    }

    async function changeAccessExpiry(username, role, value) {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({username: username, role: role, expires_at: value || ''})
        });
        if (!resp.ok) { var d = await resp.json().catch(function(){return {};}); hiveToast(d.error || 'Failed to change expiry', 'error'); loadAccessList(); return; }
        hiveToast(value ? username + '\'s access now expires ' + value : username + '\'s access is now permanent', 'success');
        loadAccessList();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadAccessList(); }
    }

    async function changeAccessRole(username, newRole, oldRole) {
      if (newRole === oldRole) return;
      // Granting owner is significant — confirm it.
      if (newRole === 'owner' && !await hiveConfirm('Make ' + username + ' an owner? Owners can manage access and delete the hive.')) {
        loadAccessList();
        return;
      }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({username: username, role: newRole})
        });
        if (!resp.ok) { var d = await resp.json().catch(function(){return {};}); hiveToast(d.error || 'Failed to change role', 'error'); loadAccessList(); return; }
        hiveToast(username + ' is now ' + newRole, 'success');
        loadAccessList();
        loadAccessAuditLog();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadAccessList(); }
    }

    async function removeAccess(username) {
      var entry = _accessUsers.filter(function(u) { return u.username === username; })[0];
      var isOwner = !!(entry && entry.role === 'owner');
      if (isOwner) {
        // Block removing the last owner client-side (mirrors the server check)
        // so the user gets a clear error instead of a silent no-op.
        var ownerCount = _accessUsers.filter(function(u) { return u.role === 'owner'; }).length;
        if (ownerCount <= 1) {
          hiveToast('At least one owner is required — cannot remove the last owner', 'error');
          return;
        }
      }
      var isSelf = _currentUser && String(username || '').toLowerCase() === _currentUser;
      if (isOwner && isSelf) {
        // Removing yourself as owner is irreversible from your side — confirm
        // explicitly. Non-self removal keeps the ordinary confirmation below.
        if (!await hiveConfirm('You will lose owner access to this hive — are you sure you want to remove yourself?')) return;
      } else {
        if (!await hiveConfirm('Remove access for ' + username + '?')) return;
      }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access/' + encodeURIComponent(username), {method: 'DELETE'});
        if (!resp.ok) {
          var d = await resp.json().catch(function(){return {};});
          hiveToast(d.error || 'Failed to remove access', 'error');
        }
        loadAccessList();
        loadAccessAuditLog();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    /* Bulk selection state for the Manage Access list: username -> true.
       _accessUsers (declared above) mirrors the last roster fetch so the
       bulk guards can count owners without another network round trip. */
    var _bulkAccessSel = {};

    function toggleBulkAccess(cb, username) {
      if (cb.checked) { _bulkAccessSel[username] = true; } else { delete _bulkAccessSel[username]; }
      updateBulkAccessBar();
    }

    /* The bulk bar only appears once 2+ users are selected — a single
       selection is served just as well by the per-row controls. */
    function updateBulkAccessBar() {
      var bar = document.getElementById('access-bulk-bar');
      if (!bar) return;
      var count = Object.keys(_bulkAccessSel).length;
      if (count >= 2) {
        bar.style.display = 'flex';
        document.getElementById('access-bulk-count').textContent = count + ' users selected';
      } else {
        bar.style.display = 'none';
      }
    }

    /* wouldOrphanOwners: true when acting on the selection would leave the
       hive with zero owners. The per-row last-owner lock already hides the
       single-owner case; this catches "select every owner at once". */
    function wouldOrphanOwners(selected) {
      var owners = _accessUsers.filter(function(u) { return u.role === 'owner'; });
      if (!owners.length) return false;
      return owners.every(function(o) { return selected.indexOf(o.username) !== -1; });
    }

    async function bulkChangeAccessRole() {
      var selected = Object.keys(_bulkAccessSel);
      if (selected.length < 2) return;
      var newRole = document.getElementById('access-bulk-role').value;
      if (newRole !== 'owner' && wouldOrphanOwners(selected)) {
        hiveToast('Cannot demote all owners — the hive must keep at least one', 'error');
        return;
      }
      if (newRole === 'owner' && !await hiveConfirm('Make ' + selected.length + ' users owners? Owners can manage access and delete the hive.')) return;
      var failed = [];
      for (var i = 0; i < selected.length; i++) {
        try {
          var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({username: selected[i], role: newRole})
          });
          if (!resp.ok) failed.push(selected[i]);
        } catch(e) { failed.push(selected[i]); }
      }
      if (failed.length) {
        hiveToast('Failed to change role for: ' + failed.join(', '), 'error');
      } else {
        hiveToast(selected.length + ' users are now ' + newRole, 'success');
      }
      _bulkAccessSel = {};
      updateBulkAccessBar();
      loadAccessList();
    }

    async function bulkRemoveAccess() {
      var selected = Object.keys(_bulkAccessSel);
      if (selected.length < 2) return;
      if (wouldOrphanOwners(selected)) {
        hiveToast('Cannot remove all owners — the hive must keep at least one', 'error');
        return;
      }
      if (!await hiveConfirm('Remove access for ' + selected.length + ' users (' + selected.join(', ') + ')?')) return;
      var failed = [];
      for (var i = 0; i < selected.length; i++) {
        try {
          var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access/' + encodeURIComponent(selected[i]), {method: 'DELETE'});
          if (!resp.ok) failed.push(selected[i]);
        } catch(e) { failed.push(selected[i]); }
      }
      if (failed.length) {
        hiveToast('Failed to remove: ' + failed.join(', '), 'error');
      } else {
        hiveToast('Removed ' + selected.length + ' users', 'success');
      }
      _bulkAccessSel = {};
      updateBulkAccessBar();
      loadAccessList();
    }

    /* Banner text is NEUTRAL (matches the spoke's banner-contrast rule);
       the color choice shows in the tint and border only. */
    var _bannerColorStyles = {
      green: {bg: 'rgba(22,163,74,0.12)', border: '1px solid rgba(22,163,74,0.3)', color: 'var(--text)'},
      blue:  {bg: 'rgba(59,130,246,0.12)', border: '1px solid rgba(59,130,246,0.3)', color: 'var(--text)'},
      amber: {bg: 'rgba(245,158,11,0.12)', border: '1px solid rgba(245,158,11,0.3)', color: 'var(--text)'},
      gray:  {bg: 'rgba(107,114,128,0.12)', border: '1px solid rgba(107,114,128,0.3)', color: 'var(--text)'}
    };
    /* Set by the per-hive "Send Banner" menu item so the banner modal targets a
       single spoke; sendHubBanner() still reads .banner-hive-cb:checked, so this
       is only bookkeeping for the open path — the checked cb is the source of truth. */
    var _bannerTargetHive = null;

    (function() {
      var msgEl = document.getElementById('banner-message');
      if (msgEl) {
        msgEl.addEventListener('input', function() {
          document.getElementById('banner-char-count').textContent = this.value.length;
          updateBannerPreview();
        });
      }
      var radios = document.querySelectorAll('input[name="banner-color"]');
      radios.forEach(function(r) { r.addEventListener('change', updateBannerPreview); });
    })();

    // Same safe minimal-markdown as the spoke's renderHubBanner: escape first,
    // then re-introduce only **bold** and [text](http(s)://url) links. Keep this
    // in sync with bannerMarkdown() in the spoke dashboard.
    function bannerMarkdown(text, linkColor) {
      var out = String(text || '')
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
      out = out.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)"']+)\)/g, function(_, label, url) {
        return '<a href="' + url + '" target="_blank" rel="noopener noreferrer" style="color:'
          + (linkColor || 'inherit') + ';text-decoration:underline">' + label + '</a>';
      });
      return out.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    }

    function updateBannerPreview() {
      var msg = (document.getElementById('banner-message').value || '').trim();
      var color = (document.querySelector('input[name="banner-color"]:checked') || {}).value || 'green';
      var s = _bannerColorStyles[color];
      var preview = document.getElementById('banner-preview');
      preview.style.background = s.bg;
      preview.style.border = s.border;
      preview.style.color = s.color;
      preview.innerHTML = msg ? ('📢 ' + bannerMarkdown(msg, s.color)) : '<em style="opacity:0.6">Type a message above to preview...</em>';
    }

    // Toolbar helpers: wrap the current selection (or insert a template) at the
    // cursor, then refresh the char count + preview.
    function bannerInsert(before, after, placeholder) {
      var el = document.getElementById('banner-message');
      var start = el.selectionStart, end = el.selectionEnd;
      var sel = el.value.slice(start, end) || placeholder;
      el.value = el.value.slice(0, start) + before + sel + after + el.value.slice(end);
      // Re-select the inserted content so the user can type over the placeholder.
      el.focus();
      el.selectionStart = start + before.length;
      el.selectionEnd = start + before.length + sel.length;
      document.getElementById('banner-char-count').textContent = el.value.length;
      updateBannerPreview();
    }
    function bannerFmtBold() { bannerInsert('**', '**', 'bold text'); }
    function bannerFmtLink() { bannerInsert('[', '](https://)', 'link text'); }

    function loadBannerHiveList() {
      var container = document.getElementById('banner-hive-list');
      container.innerHTML = '';
      var hives = _hiveRegistry || [];
      if (!hives.length) {
        container.innerHTML = '<div style="padding:12px;color:var(--muted);font-size:0.8rem;text-align:center">No hives found</div>';
        return;
      }
      hives.forEach(function(h) {
        var label = h.name || h.id;
        var div = document.createElement('div');
        div.style.cssText = 'display:flex;align-items:center;gap:8px;padding:6px 10px;border-bottom:1px solid var(--border)';
        div.innerHTML = '<label style="display:flex;align-items:center;gap:8px;cursor:pointer;flex:1;font-size:0.82rem;color:var(--text)">' +
          '<input type="checkbox" class="banner-hive-cb" value="' + esc(h.id) + '" checked style="accent-color:var(--accent)"> ' + esc(label) +
          '</label>';
        container.appendChild(div);
      });
    }

    function toggleAllBannerHives(checked) {
      document.querySelectorAll('.banner-hive-cb').forEach(function(cb) { cb.checked = checked; });
    }

    /* Per-hive entry point: opens the SAME banner modal but pre-scoped to one
       spoke. Instead of loadBannerHiveList()'s multi-hive checklist, we render a
       single non-editable target line plus one hidden checked .banner-hive-cb so
       the unchanged sendHubBanner() (which reads .banner-hive-cb:checked) posts
       exactly this hive_id to POST /api/saas/admin/hub-banner. */
    function openBannerForHive(hiveId, hiveName) {
      _bannerTargetHive = hiveId;
      document.getElementById('banner-modal').style.display = 'flex';
      /* Reset message + color to match the global open path's fresh state. */
      document.getElementById('banner-message').value = '';
      document.getElementById('banner-char-count').textContent = '0';
      document.querySelector('input[name="banner-color"][value="green"]').checked = true;
      updateBannerPreview();
      var container = document.getElementById('banner-hive-list');
      container.innerHTML = '<div style="display:flex;align-items:center;gap:8px;padding:10px;color:var(--text);font-size:0.82rem">' +
        '<span style="color:var(--muted)">Sending to:</span> <strong>' + esc(hiveName) + '</strong>' +
        '<input type="checkbox" class="banner-hive-cb" value="' + esc(hiveId) + '" checked style="display:none">' +
        '</div>';
    }

    async function sendHubBanner() {
      var msg = (document.getElementById('banner-message').value || '').trim();
      if (!msg) { hiveToast('Message is required', 'error'); return; }
      var color = (document.querySelector('input[name="banner-color"]:checked') || {}).value || 'green';
      var hiveIDs = [];
      document.querySelectorAll('.banner-hive-cb:checked').forEach(function(cb) { hiveIDs.push(cb.value); });
      if (!hiveIDs.length) { hiveToast('Select at least one hive', 'error'); return; }
      var btn = document.getElementById('btn-send-banner');
      btn.disabled = true;
      btn.textContent = 'Sending...';
      try {
        var resp = await fetch('/api/saas/admin/hub-banner', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({message: msg, color: color, hive_ids: hiveIDs})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to send', 'error'); return; }
        hiveToast('Banner sent to ' + data.hive_count + ' hive(s)', 'success');
        document.getElementById('banner-modal').style.display = 'none';
        document.getElementById('banner-message').value = '';
        document.getElementById('banner-char-count').textContent = '0';
        document.querySelector('input[name="banner-color"][value="green"]').checked = true;
        updateBannerPreview();
        loadActiveBanner();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
      finally { btn.disabled = false; btn.textContent = 'Send Banner'; }
    }

    async function loadActiveBanner() {
      try {
        var resp = await fetch('/api/saas/admin/hub-banner');
        if (!resp.ok) return;
        var data = await resp.json();
        var banners = data.banners || [];
        var display = document.getElementById('active-banner-display');
        var clearBtn = document.getElementById('btn-clear-banner');
        if (!banners.length) {
          display.style.display = 'none';
          clearBtn.style.display = 'none';
          return;
        }
        var first = banners[0];
        var s = _bannerColorStyles[first.color] || _bannerColorStyles.green;
        var preview = document.getElementById('active-banner-preview');
        preview.style.background = s.bg;
        preview.style.border = s.border;
        preview.style.color = s.color;
        preview.textContent = first.message;
        var targets = document.getElementById('active-banner-targets');
        var hiveNames = banners.map(function(b) { return b.hive_id; });
        targets.textContent = 'Sent to ' + banners.length + ' hive(s): ' + hiveNames.join(', ');
        display.style.display = '';
        clearBtn.style.display = '';
      } catch(e) { /* ignore */ }
    }

    async function clearHubBanner() {
      if (!await hiveConfirm('Clear all active hub banners?')) return;
      try {
        var resp = await fetch('/api/saas/admin/hub-banner', {method: 'DELETE'});
        if (!resp.ok) { hiveToast('Failed to clear', 'error'); return; }
        hiveToast('All banners cleared', 'success');
        loadActiveBanner();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }
  </script>
</body>
</html>`
