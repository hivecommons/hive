package hub

const dashboardHTMLLayout = `<!DOCTYPE html>
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
             public spoke image (ghcr.io/kubestellar/hive), bucketed by the
             ACTIVE release line's release boundaries (the line the "stable"
             channel currently resolves to — v4 today, v5 after the next
             rollover, with no code change). Gauges external adoption beyond
             the hosted fleet. Derived from GitHub's cumulative "Total
             downloads" counter — it is pulls, NOT unique downloads
             (uniqueness is not measurable). Populated by loadImagePulls(),
             which also fills the per-line mini charts in the
             "Latest available images" rows. Hidden until there is data. -->
        <div id="image-pulls-spark" style="display:none;margin-top:8px"
             title="Container-image pulls per release of the active release line: the pulls that landed while each of the last ~10 releases was the newest, of the public hive image (ghcr.io/kubestellar/hive). Derived from GitHub's cumulative download counter — pulls, not unique downloads."></div>
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
`
