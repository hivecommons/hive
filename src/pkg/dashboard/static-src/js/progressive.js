    const DASHBOARD_BACKGROUND_CONCURRENCY = 2;
    const DASHBOARD_IDLE_TIMEOUT_MS = 1000;
    const DASHBOARD_YIELD_MS = 32;
    const DASHBOARD_BACKGROUND_DELAY_MS = 350;
    const DASHBOARD_TASK_SECTIONS = Object.freeze({
      'backend-discovery': ['debug-section'],
      'stat-sources': ['agents-section'],
      'cost': ['cost-panel'],
      'hive-advice': ['advisory-section'],
      'acmm-reco': ['advisory-section'],
      'lifecycle': ['advisory-section'],
      'pr-throughput': ['pr-throughput-section'],
      'approvals': ['approvals-section'],
      'fact-history': ['knowledge-section'],
      'overview-kpi-history': ['overview-section'],
      'history': ['overview-section', 'repos-section'],
      'budget-ignore': ['repos-section'],
      'budget-history': ['governor'],
      'nous-status': ['nous-section'],
      'public-knowledge': ['knowledge-section'],
      'knowledge-stats': ['knowledge-section'],
      'audit-log': ['audit-section'],
      'review-queue': ['review-queue-section'],
      'review-pipeline': ['review-queue-section'],
      'stage-tracker': ['activity-monitor-section'],
      'contributors': ['contributors-section'],
      'leaderboard': ['contributors-section'],
      'git-version': ['debug-section'],
      'acmm-evaluation': ['acmm-eval-section'],
      'version-check': ['debug-section'],
      'version-reload-check': ['debug-section'],
      'weekly-trends': ['overview-section'],
      'timeline': ['governor'],
      'agent-logs': ['logs-section'],
      'debug-section': ['debug-section'],
      'chat-capabilities': [],
      'chat-presence': [],
      'welcome-gh-auth': [],
      'welcome-claude-auth': [],
      'gh-auth-banner': [],
    });
    const _dashboardBackgroundJobs = new Map();
    const _dashboardRenderJobs = new Map();
    let _dashboardBackgroundRunning = 0;
    let _dashboardBackgroundScheduled = false;
    let _dashboardRenderScheduled = false;
    let _dashboardBackgroundReady = false;

    function dashboardWorkPriority(sections) {
      let priority = 2;
      for (const id of sections || []) {
        if (window.location.hash === '#section-' + id) return 0;
        const el = document.getElementById(id);
        if (!el || !el.getClientRects().length) continue;
        const rect = el.getBoundingClientRect();
        if (rect.bottom >= 0 && rect.top <= window.innerHeight) priority = 1;
      }
      return priority;
    }
    function dashboardNextWork(jobs) {
      let next;
      let priority = Infinity;
      for (const job of jobs.values()) {
        if (job.running) continue;
        const rank = dashboardWorkPriority(job.sections);
        if (rank < priority) { next = job; priority = rank; }
      }
      return next;
    }
    function dashboardIdle(callback) {
      if (typeof window.requestIdleCallback === 'function') {
        window.requestIdleCallback(callback, { timeout: DASHBOARD_IDLE_TIMEOUT_MS });
      } else {
        setTimeout(callback, DASHBOARD_YIELD_MS);
      }
    }
    function dashboardWakeBackground() {
      if (!_dashboardBackgroundReady || document.hidden || _dashboardBackgroundScheduled ||
          _dashboardBackgroundRunning >= DASHBOARD_BACKGROUND_CONCURRENCY ||
          !dashboardNextWork(_dashboardBackgroundJobs)) return;
      _dashboardBackgroundScheduled = true;
      dashboardIdle(function() {
        _dashboardBackgroundScheduled = false;
        if (document.hidden) return;
        const job = dashboardNextWork(_dashboardBackgroundJobs);
        if (!job) return;
        job.running = true;
        _dashboardBackgroundRunning++;
        function finished() {
          _dashboardBackgroundJobs.delete(job.key);
          _dashboardBackgroundRunning--;
          dashboardWakeBackground();
        }
        Promise.resolve().then(job.fn).then(function(value) {
          finished(); job.resolve(value);
        }, function(err) {
          finished(); job.reject(err);
        });
        dashboardWakeBackground();
      });
    }
    // Keep a slot through JSON decoding and rendering, not just response headers.
    // Repeated timer ticks share the same flight rather than adding stale work.
    function dashboardScheduleWork(key, fn) {
      const existing = _dashboardBackgroundJobs.get(key);
      if (existing) return existing.promise;
      let resolve, reject;
      const promise = new Promise(function(ok, fail) { resolve = ok; reject = fail; });
      _dashboardBackgroundJobs.set(key, {
        key, fn, promise, resolve, reject, running: false,
        sections: DASHBOARD_TASK_SECTIONS[key] || [],
      });
      dashboardWakeBackground();
      return promise;
    }
    function dashboardWakeRender() {
      if (document.hidden || _dashboardRenderScheduled || !_dashboardRenderJobs.size) return;
      _dashboardRenderScheduled = true;
      dashboardIdle(function() {
        _dashboardRenderScheduled = false;
        if (document.hidden) return;
        const job = dashboardNextWork(_dashboardRenderJobs);
        if (!job) return;
        _dashboardRenderJobs.delete(job.key);
        if (job.snapshot && !dashboardStatusCanRender(job.snapshot)) {
          dashboardWakeRender();
          return;
        }
        const anchor = typeof captureScrollAnchor === 'function' ? captureScrollAnchor() : null;
        try {
          job.fn();
          job.sections.forEach(function(id) {
            if (typeof applySectionCollapse === 'function') applySectionCollapse(id);
          });
          if (typeof refreshDashboardSectionSummaries === 'function') refreshDashboardSectionSummaries();
        } catch (err) {
          console.error('dashboard render ' + job.key + ' failed', err);
        } finally {
          if (anchor && typeof restoreScrollAnchor === 'function' &&
              (typeof _hashScrollPending === 'undefined' || !_hashScrollPending)) restoreScrollAnchor(anchor);
          dashboardWakeRender();
        }
      });
    }
    // Latest snapshot wins. Paint one section per idle turn so input and
    // navigation can run between large cards, even when SSE arrives in a burst.
    function dashboardRenderSection(key, sections, fn, snapshot) {
      _dashboardRenderJobs.set(key, { key, sections, fn, snapshot });
      dashboardWakeRender();
    }
    function dashboardProgressiveWake() {
      dashboardWakeBackground();
      dashboardWakeRender();
    }
    document.addEventListener('visibilitychange', dashboardProgressiveWake);
    setTimeout(function() {
      _dashboardBackgroundReady = true;
      dashboardWakeBackground();
    }, DASHBOARD_BACKGROUND_DELAY_MS);
