    /* ---- Top-bar right strip on phones -------------------------------------
       Below 768px the strip scrolls sideways when its chips don't fit
       (responsive.css). It starts scrolled to the end, so Help and the account
       menu stay in view, and stays there as chips come and go, until the user
       scrolls it; scrolling back to the end pins it again. On wider screens
       the strip never overflows and this does nothing. */
    const TOPBAR_STRIP_END_EPS = 2;
    function topbarStripAtEnd(strip) {
      return (strip.scrollLeft || 0) + (strip.clientWidth || 0) >= (strip.scrollWidth || 0) - TOPBAR_STRIP_END_EPS;
    }
    function initTopbarStripScroll() {
      const strip = document.querySelector('.oc-topbar-right');
      if (!strip || !strip.dataset || strip.dataset.stripScrollWired) return;
      strip.dataset.stripScrollWired = '1';
      let pinned = true;
      const pin = () => { if (pinned) strip.scrollLeft = strip.scrollWidth; };
      strip.addEventListener('scroll', () => { pinned = topbarStripAtEnd(strip); }, { passive: true });
      if (typeof ResizeObserver === 'function') new ResizeObserver(pin).observe(strip);
      if (typeof MutationObserver === 'function') new MutationObserver(pin).observe(strip, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['hidden', 'style', 'class'] });
      if (window.addEventListener) window.addEventListener('resize', pin, { passive: true });
      pin();
    }
    initTopbarStripScroll();
