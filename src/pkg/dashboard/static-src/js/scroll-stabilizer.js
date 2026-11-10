    // ── Scroll stabilizer ──────────────────────────────────────────────
    // render() pins the reader's section across its own repaint, but the
    // page has many other repaint paths (SSE panel updates, lazy section
    // loads, sparkline replays, pane polls) and each of them can change the
    // height of content above the viewport, dragging what the reader is
    // looking at up or down. Browser-native scroll anchoring does not help
    // here: it gives up whenever the anchor node itself is replaced, which
    // is exactly what an innerHTML rerender does.
    //
    // This keeps one live anchor (same shape as captureScrollAnchor: the
    // topmost visible section, or the first visible agent card by key) and
    // re-applies it from a ResizeObserver. ResizeObserver callbacks run
    // after layout and before paint, so the correction lands in the same
    // frame as the shift and the reader never sees the page move. Because
    // the correction is delta-based against a persistent node, it composes
    // with native anchoring instead of double-counting it.
    const SCROLL_STABILIZER_MIN_DELTA_PX = 1;
    let _ssAnchor = null;
    let _ssCaptureQueued = false;
    let _ssSelfScroll = false;
    let _ssObserver = null;
    let _ssDomObserver = null;

    function _ssSuppressed() {
      // A hash/sidebar navigation is settling: ocSettleScroll owns the
      // scroll until it is done, and we must not fight it.
      if (typeof _hashScrollPending !== 'undefined' && _hashScrollPending) return true;
      if (typeof _ocScrollSettleCancel !== 'undefined' && _ocScrollSettleCancel) return true;
      return false;
    }

    function _ssCapture() {
      _ssCaptureQueued = false;
      if (typeof captureScrollAnchor !== 'function') return;
      _ssAnchor = window.scrollY > 0 ? captureScrollAnchor() : null;
    }

    function _ssQueueCapture() {
      if (_ssCaptureQueued) return;
      _ssCaptureQueued = true;
      requestAnimationFrame(_ssCapture);
    }

    // Returns the vertical delta the anchor has drifted by, or 0.
    function _ssDrift(anchor) {
      if (!anchor) return 0;
      if (anchor.cardKey && anchor.el && anchor.el.isConnected) {
        const safeKey = (window.CSS && CSS.escape) ? CSS.escape(anchor.cardKey) : String(anchor.cardKey).replace(/["\\]/g, '\\$&');
        const card = anchor.el.querySelector(`.agent-card[data-agent="${safeKey}"]`);
        if (card && card.getClientRects().length) return card.getBoundingClientRect().top - anchor.cardTop;
      }
      if (anchor.el && anchor.el.isConnected && anchor.el.getClientRects().length) {
        return anchor.el.getBoundingClientRect().top - anchor.top;
      }
      return 0;
    }

    function _ssCorrect() {
      if (_ssSuppressed() || !_ssAnchor || window.scrollY <= 0) { _ssQueueCapture(); return; }
      const delta = _ssDrift(_ssAnchor);
      if (Math.abs(delta) >= SCROLL_STABILIZER_MIN_DELTA_PX) {
        const before = window.scrollY;
        _ssSelfScroll = true;
        window.scrollBy(0, delta);
        // Clamped at an edge: no scroll event will follow, so clear the flag.
        if (window.scrollY === before) _ssSelfScroll = false;
      }
      // Content may have changed which section is topmost; refresh the
      // anchor so the next shift is measured from where the reader is now.
      _ssQueueCapture();
    }

    function _ssObserveSections() {
      if (!_ssObserver) return;
      const sections = document.querySelectorAll('[data-dashboard-section]');
      for (let i = 0; i < sections.length; i++) _ssObserver.observe(sections[i]);
    }

    function initScrollStabilizer() {
      if (_ssObserver || typeof ResizeObserver === 'undefined') return;
      _ssObserver = new ResizeObserver(_ssCorrect);
      _ssObserver.observe(document.documentElement);
      if (document.body) _ssObserver.observe(document.body);
      _ssObserveSections();
      // Sections that are rendered later (lazy panels) must be observed too;
      // observe() is idempotent so re-walking on any subtree change is safe.
      if (typeof MutationObserver !== 'undefined') {
        let walkQueued = false;
        _ssDomObserver = new MutationObserver(function() {
          if (walkQueued) return;
          walkQueued = true;
          requestAnimationFrame(function() { walkQueued = false; _ssObserveSections(); });
        });
        _ssDomObserver.observe(document.body || document.documentElement, { childList: true, subtree: true });
      }
      // The reader's own scrolling moves the anchor; our corrections do not.
      window.addEventListener('scroll', function() {
        if (_ssSelfScroll) { _ssSelfScroll = false; return; }
        _ssQueueCapture();
      }, { passive: true });
      // A viewport resize changes every rect; re-measure instead of correcting.
      window.addEventListener('resize', function() { _ssAnchor = null; _ssQueueCapture(); }, { passive: true });
      _ssQueueCapture();
    }

    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', initScrollStabilizer);
    } else {
      initScrollStabilizer();
    }
