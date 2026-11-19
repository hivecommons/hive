    // ── Ask AI help (#19184) ─────────────────────────────────────────
    // Loads the shared panel component (pkg/askai/web, served from 'self'
    // at /ask-panel.js) only when /api/config says ask is on. With ask off
    // the page adds nothing and requests nothing. The panel sends only the
    // question and an id from the closed section vocabulary to this hive's
    // own /api/ask; the backend decides what, if anything, leaves.
    const ASK_PANEL_SCRIPT = '/ask-panel.js';
    const ASK_PANEL_STYLE = '/ask-panel.css';
    const ASK_SLOT_ID = 'hive-ask-slot';
    // A section counts as "current" once its top is above this fraction of
    // the viewport height.
    const ASK_SECTION_VIEWPORT_FRACTION = 3;
    let askPanelRequested = false;

    // askCurrentSection names where the person is, from our own ids only:
    // the Settings tab when the dialog is open, else the dashboard section
    // nearest the top of the viewport.
    function askCurrentSection() {
      const overlay = document.getElementById('config-overlay');
      if (overlay && !overlay.classList.contains('hidden')) {
        return (typeof _configState !== 'undefined' && _configState && _configState.tab === 'features') ? 'settings-features' : 'settings';
      }
      let best = '';
      let bestTop = -Infinity;
      const limit = window.innerHeight / ASK_SECTION_VIEWPORT_FRACTION;
      Object.keys(DASHBOARD_SECTION_HELP).forEach(id => {
        const el = document.getElementById(id);
        if (!el || !el.offsetParent) return;
        const top = el.getBoundingClientRect().top;
        if (top <= limit && top > bestTop) { best = id; bestTop = top; }
      });
      return best;
    }

    // askCurrentDocsHref is the docs-link fallback for where the person is,
    // built by the same function as every "?" help mark.
    function askCurrentDocsHref() {
      const section = askCurrentSection();
      if (section === 'settings-features') return dashboardDocsHref('settings', 'features');
      if (section === 'settings') return dashboardDocsHref('settings', '');
      const entry = DASHBOARD_SECTION_HELP[section];
      return dashboardDocsHref(DASHBOARD_HELP_PAGE, entry ? dashboardSectionHelpAnchor(entry.heading) : '');
    }

    function loadAskPanel(cfg) {
      if (!cfg || cfg.ask_enabled !== true || askPanelRequested) return;
      if (!document.getElementById(ASK_SLOT_ID)) return;
      askPanelRequested = true;
      const style = document.createElement('link');
      style.rel = 'stylesheet';
      style.href = ASK_PANEL_STYLE;
      document.head.appendChild(style);
      const script = document.createElement('script');
      script.src = ASK_PANEL_SCRIPT;
      script.addEventListener('load', () => {
        if (window.HiveAsk && typeof window.HiveAsk.mount === 'function') {
          window.HiveAsk.mount({ slot: ASK_SLOT_ID, base: '/api/ask', section: askCurrentSection, docsHref: askCurrentDocsHref });
        }
      });
      document.head.appendChild(script);
    }
