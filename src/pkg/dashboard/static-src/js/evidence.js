    // Evidence sub-panel (#12064, RFC #12062): read-only reporting from
    // /api/evidence/summary for the Throughput window and repo selector.
    // Panels 1 (outcome by author kind), 2 (reviewer calibration), 3 (reviewer
    // accuracy × model), 4 (boundary enforcement) and 8 (shadow gates). UI only;
    // nothing here changes merge behaviour.
    const EV_PALETTE_SIZE = 6;
    let evLast = null;
    let evConfidenceSeries = 'self-reported';

    function evCalibrationSeries(dto) {
      const rows = evConfidenceSeries === 'derived' ? dto.reviewer_calibration_derived : dto.reviewer_calibration;
      return Array.isArray(rows) ? rows : [];
    }
    function evSelectConfidence(series) {
      evConfidenceSeries = series === 'derived' ? 'derived' : 'self-reported';
      if (evLast) renderEvidenceSummary(evLast);
    }
    function evCalibrationControls() {
      return `<div class="lc-window" role="group" aria-label="Confidence series">${['self-reported', 'derived'].map((series) => `<button type="button" class="${evConfidenceSeries === series ? 'active' : ''}" aria-pressed="${evConfidenceSeries === series}" data-action="evSelectConfidence" data-arg0="${series}" data-stop="1">${series === 'derived' ? 'Derived' : 'Self-reported'}</button>`).join('')}</div>`;
    }

    // Evidence window (#12062): 24h · 7d · 30d · custom from/to, independent of
    // the throughput pills and remembered per browser. The repo filter is
    // still shared with the throughput card.
    const EV_WINDOWS = [[24, '24h'], [168, '7d'], [720, '30d']];
    const EV_WINDOW_STORAGE_KEY = 'hive.evidence.window';
    const EV_WINDOW_DEFAULT_HOURS = 24;
    const EV_CUSTOM_MAX_DAYS = 3 * 366; // mirrors evidenceSummaryMaxRangeDays server-side; backfilled history reaches back years
    const EV_DATE_RE = /^\d{4}-\d{2}-\d{2}$/;
    let evWindow = (() => {
      try {
        const raw = JSON.parse(localStorage.getItem(EV_WINDOW_STORAGE_KEY) || 'null');
        if (raw && EV_DATE_RE.test(raw.from || '') && EV_DATE_RE.test(raw.to || '')) return { from: raw.from, to: raw.to };
        if (raw && EV_WINDOWS.some(([h]) => h === Number(raw.hours))) return { hours: Number(raw.hours) };
      } catch {}
      return { hours: EV_WINDOW_DEFAULT_HOURS };
    })();
    let evCustomOpen = !!evWindow.from;
    let evCustomError = '';
    const evExplainOpen = new Set();

    function evWindowQuery() {
      return evWindow.from ? `from=${encodeURIComponent(evWindow.from)}&to=${encodeURIComponent(evWindow.to)}` : `hours=${encodeURIComponent(evWindow.hours)}`;
    }
    // Hours the current window spans; a date-only `to` is inclusive.
    function evWindowHours() {
      if (!evWindow.from) return evWindow.hours;
      const ms = Date.parse(evWindow.to + 'T00:00:00Z') + 86400000 - Date.parse(evWindow.from + 'T00:00:00Z');
      return Math.max(1, Math.round(ms / 3600000));
    }
    function evShortDate(iso) {
      return formatDashboardDate(iso + 'T00:00:00Z', { month: 'short', day: 'numeric', timeZone: 'UTC' }, iso);
    }
    function evWindowLabel() {
      if (evWindow.from) return `${evShortDate(evWindow.from)} – ${evShortDate(evWindow.to)}`;
      const w = EV_WINDOWS.find(([h]) => h === evWindow.hours);
      return `last ${w ? w[1] : evWindow.hours + 'h'}`;
    }
    function evSaveWindow() {
      try { localStorage.setItem(EV_WINDOW_STORAGE_KEY, JSON.stringify(evWindow)); } catch {}
    }
    function setEvidenceWindow(hours) {
      const h = Number(hours);
      evWindow = { hours: EV_WINDOWS.some(([v]) => v === h) ? h : EV_WINDOW_DEFAULT_HOURS };
      evCustomOpen = false;
      evCustomError = '';
      evSaveWindow();
      renderEvidenceSummary(evLast);
      return fetchEvidenceSummary();
    }
    function toggleEvidenceCustomRange() {
      evCustomOpen = !evCustomOpen;
      evCustomError = '';
      renderEvidenceSummary(evLast);
      const input = document.querySelector('#evidence-card .ev-custom-range input[name="from"]');
      if (evCustomOpen && input) input.focus();
    }
    function applyEvidenceCustomRange(btn) {
      const form = btn && btn.closest ? btn.closest('.ev-custom-range') : null;
      const from = form ? String((form.querySelector('input[name="from"]') || {}).value || '') : '';
      const to = form ? String((form.querySelector('input[name="to"]') || {}).value || '') : '';
      if (!EV_DATE_RE.test(from) || !EV_DATE_RE.test(to)) evCustomError = 'Pick both a from and a to date.';
      else if (to < from) evCustomError = '"To" must not be before "from".';
      else if ((Date.parse(to) - Date.parse(from)) / 86400000 > EV_CUSTOM_MAX_DAYS) evCustomError = `Range must be ${EV_CUSTOM_MAX_DAYS} days or less.`;
      else evCustomError = '';
      if (evCustomError) { renderEvidenceSummary(evLast); return; }
      evWindow = { from, to };
      evSaveWindow();
      renderEvidenceSummary(evLast);
      return fetchEvidenceSummary();
    }
    function evWindowControls() {
      const pills = EV_WINDOWS.map(([h, label]) => `<button type="button" class="${!evWindow.from && evWindow.hours === h ? 'active' : ''}" data-action="setEvidenceWindow" data-arg0="${h}" data-stop="1" title="Last ${label}">${label}</button>`).join('')
        + `<button type="button" class="${evWindow.from || evCustomOpen ? 'active' : ''}" data-action="toggleEvidenceCustomRange" data-stop="1" aria-expanded="${evCustomOpen ? 'true' : 'false'}" aria-controls="ev-custom-range" title="Pick a from/to date range">custom</button>`;
      const today = new Date().toISOString().slice(0, 10);
      const range = `<div id="ev-custom-range" class="ev-custom-range" role="group" aria-label="Custom evidence date range" data-stop="1"${evCustomOpen ? '' : ' hidden'}><label>from <input type="date" name="from" value="${escapeHtml(evWindow.from || '')}" max="${today}"></label><label>to <input type="date" name="to" value="${escapeHtml(evWindow.to || '')}" max="${today}"></label><button type="button" data-action="applyEvidenceCustomRange" data-arg-types="t" data-stop="1">Apply</button>${evCustomError ? `<span class="ev-range-err" role="alert">${escapeHtml(evCustomError)}</span>` : ''}</div>`;
      return `<div class="gov-pr-models-toggle window-pills-toolbar ev-window-controls" data-stop="1">${pills}</div>${range}`;
    }
    window.evSelectConfidence = evSelectConfidence;
    window.setEvidenceWindow = setEvidenceWindow;
    window.toggleEvidenceCustomRange = toggleEvidenceCustomRange;
    window.applyEvidenceCustomRange = applyEvidenceCustomRange;

    function evRate(num, den) {
      const d = Number(den) || 0;
      return d > 0 ? (Number(num) || 0) / d : null;
    }
    function evPct(v) { return v == null ? '—' : Math.round(v * 100) + '%'; }

    // ---- Plain-language explainers ("How to read the Evidence card" in
    // docs/review-evidence.md). Every threshold below is documented there;
    // change both together. Grades describe the data in the window, never a
    // merge decision.
    const EV_GRADE_MIN_N = 20;              // fewer samples than this → "Not enough data"
    const EV_SHORT_WINDOW_HOURS = 48;       // windows this short get a "too early to judge" caveat
    const EV_CALIB_STRONG = 0.8;            // top-confidence bucket merge rate ≥ → Strong
    const EV_CALIB_FAIR = 0.6;              // ≥ → Fair, below → Weak
    const EV_ERR_STRONG = 0.10;             // worst of false-approve / false-block ≤ → Strong
    const EV_ERR_FAIR = 0.25;               // ≤ → Fair, above → Weak
    const EV_REVERT_STRONG = 0.05;          // reverts within 14 d / merged ≤ → Strong
    const EV_REVERT_FAIR = 0.15;            // ≤ → Fair, above → Weak
    const EV_SHADOW_FALSE_ALARM_STRONG = 0.25; // held-but-merged-clean share ≤ → Strong
    const EV_SHADOW_FALSE_ALARM_FAIR = 0.5;    // ≤ → Fair, above → Weak
    const EV_THROUGHPUT_EXCELLENT = 0.95;   // closed ÷ opened (issues) or merged ÷ opened (PRs) ≥ → Excellent
    const EV_THROUGHPUT_GOOD = 0.85;        // ≥ → Good
    const EV_THROUGHPUT_FAIR = 0.70;        // ≥ → Fair, below → Weak
    const EV_QUEUE_GROWTH_MAX = 0.10;       // week-end queue depth growing more than this week over week → Weak regardless of ratio
    const EV_BLOCKED_STRONG = 0.01;         // proxy-blocked share of ALL agent writes (direct + hive-mediated) ≤ → Strong
    const EV_BLOCKED_FAIR = 0.05;           // ≤ → Fair, above → Weak
    // CI gate, grade A (integrity): breaches ÷ merged PRs with a known merge-time check state.
    const EV_GATE_BREACH_GOOD = 0.01;       // 0 breaches → Excellent; ≤ this share → Good
    const EV_GATE_BREACH_FAIR = 0.03;       // ≤ this share → Fair, above → Weak
    // CI gate, grade B (convergence): median CI passes to first green AND abandoned share, both must hold.
    const EV_CONV_PASSES_EXCELLENT = 1.5, EV_CONV_ABANDON_EXCELLENT = 0.05;
    const EV_CONV_PASSES_GOOD = 2.5, EV_CONV_ABANDON_GOOD = 0.10;
    const EV_CONV_PASSES_FAIR = 4, EV_CONV_ABANDON_FAIR = 0.20;
    const EV_CONV_BUCKETS = ['1', '2', '3', '4+', 'never'];
    const EV_CALIB_MIN_BUCKET_N = 5;        // calibration buckets with fewer known-outcome verdicts are drawn hollow and not joined
    const EV_DATA_SINCE = '2026-10-08';     // first live evidence bundles were written
    const EV_REVERTS_SINCE = '2026-10-15';  // reverted_by links recorded from this date
    const EV_REOPENS_SINCE = '2026-10-20';  // issue_reopens recorded from this date (#13241)
    // `ungraded` is for report-only panels that have no threshold to hold
    // them to; unlike `none`, more data would not produce a grade.
    const EV_GRADES = { excellent: 'Excellent', good: 'Good', strong: 'Strong', fair: 'Fair', weak: 'Weak', none: 'Not enough data', ungraded: 'Not graded' };
    // Rank for "card grade = the lower of the two" comparisons.
    const EV_GRADE_RANK = { none: -1, weak: 0, fair: 1, good: 2, strong: 3, excellent: 4 };
    function evLowerGrade(a, b) { return EV_GRADE_RANK[a] <= EV_GRADE_RANK[b] ? a : b; }
    // evThroughputGrade: closed ÷ opened over the window on the four-band
    // scale, forced to Weak when the week-end queue grew more than
    // EV_QUEUE_GROWTH_MAX week over week at the end of the window.
    // evGateGrade: 0 breaches is Excellent only once enough merged PRs have a known state.
    function evGateGrade(breaches, withChecks) {
      if (!(Number(withChecks) >= EV_GRADE_MIN_N)) return 'none';
      const share = Number(breaches) / Number(withChecks);
      return Number(breaches) === 0 ? 'excellent' : share <= EV_GATE_BREACH_GOOD ? 'good' : share <= EV_GATE_BREACH_FAIR ? 'fair' : 'weak';
    }
    function evConvergenceGrade(medianPasses, abandonShare, settled) {
      if (medianPasses == null || abandonShare == null || !(Number(settled) >= EV_GRADE_MIN_N)) return 'none';
      if (medianPasses <= EV_CONV_PASSES_EXCELLENT && abandonShare < EV_CONV_ABANDON_EXCELLENT) return 'excellent';
      if (medianPasses <= EV_CONV_PASSES_GOOD && abandonShare < EV_CONV_ABANDON_GOOD) return 'good';
      if (medianPasses <= EV_CONV_PASSES_FAIR && abandonShare < EV_CONV_ABANDON_FAIR) return 'fair';
      return 'weak';
    }
    function evThroughputGrade(ratio, opened, queueGrowth) {
      if (ratio == null || !(Number(opened) >= EV_GRADE_MIN_N)) return 'none';
      if (queueGrowth != null && queueGrowth > EV_QUEUE_GROWTH_MAX) return 'weak';
      return ratio >= EV_THROUGHPUT_EXCELLENT ? 'excellent' : ratio >= EV_THROUGHPUT_GOOD ? 'good' : ratio >= EV_THROUGHPUT_FAIR ? 'fair' : 'weak';
    }
    // evQueueGrowth: relative change of the last two week-end depths, or null
    // when fewer than two weeks carry a depth sample.
    function evQueueGrowth(rows, key) {
      const depths = rows.map((r) => r[key]).filter((v) => v != null && Number.isFinite(Number(v))).map(Number);
      if (depths.length < 2) return null;
      const prev = depths[depths.length - 2], last = depths[depths.length - 1];
      return prev > 0 ? (last - prev) / prev : (last > 0 ? 1 : 0);
    }
    // Preset windows, longest first, keyed as the server's available_windows.
    const EV_AVAILABLE_KEYS = [['all', 'all time'], ['720', '30d'], ['168', '7d']];

    // evWindowHint: when the current window holds too few PRs for a grade but
    // a longer preset would hold more, say so instead of a bare "not enough
    // data". Uses available_windows from the same summary response (same repo
    // and author filter), so no second request is needed.
    function evWindowHint(dto) {
      const avail = dto && dto.available_windows && typeof dto.available_windows === 'object' ? dto.available_windows : null;
      if (!avail) return '';
      const hrs = evWindowHours();
      const here = Number(dto.n) || 0;
      const longer = EV_AVAILABLE_KEYS.filter(([k]) => k === 'all' || Number(k) > hrs).map(([k, label]) => ({ k, label, n: Number(avail[k]) || 0 })).filter((w) => w.n > here);
      if (!longer.length) return '';
      const counts = longer.slice().reverse().map((w) => `${w.label} ${w.n}`).join(', ');
      const presets = longer.filter((w) => w.k !== 'all').map((w) => w.label).reverse();
      return ` — window too short: ${counts} PRs; switch to ${presets.length ? presets.join(' or ') : 'a custom range'}`;
    }

    // evGrade: lower is better unless higherIsBetter. value null or n below the
    // minimum → "none".
    function evGrade(value, n, strongCut, fairCut, higherIsBetter) {
      if (value == null || !(Number(n) >= EV_GRADE_MIN_N)) return 'none';
      if (higherIsBetter) return value >= strongCut ? 'strong' : value >= fairCut ? 'fair' : 'weak';
      return value <= strongCut ? 'strong' : value <= fairCut ? 'fair' : 'weak';
    }
    function evLimitsSentence() {
      const hrs = evWindowHours();
      const short = hrs <= EV_SHORT_WINDOW_HOURS ? ` Only ${hrs}h of data is in this window, so treat the grade as an early read.` : '';
      return `Merged is not the same as correct. A PR whose outcome is not recorded (reviewed here, but merged by someone else, closed by hand, or still open — the bundle cannot tell) is reported as unknown and left out of every rate, never counted as "not merged". Live evidence (verdicts, CI, sentinel) has only been recorded since ${EV_DATA_SINCE}, reverts only since ${EV_REVERTS_SINCE} and issue re-opens only since ${EV_REOPENS_SINCE}; backfilled records reach further back but carry only author, outcome and dates.${short}`;
    }
    function evGradeLine(grade, why) {
      const label = grade === 'ungraded' ? EV_GRADES.ungraded : `Rated: ${EV_GRADES[grade]}`;
      return `<span class="ev-grade ${grade}">${label}</span>${why ? ` <span class="ev-grade-why">${escapeHtml(why)}</span>` : ''}`;
    }

    // Each explainer returns the four sections plus a grade. Text is written
    // for a reader with no statistics background.
    const EV_EXPLAINERS = {
      'outcome-by-author': (dto) => {
        const o = dto.outcomes_by_author_kind || {};
        const kinds = Object.keys(o).filter((k) => Number(o[k] && o[k].n) > 0);
        let merged = 0, decided = 0, reverted = 0, reopened = 0, unknown = 0;
        const parts = kinds.map((k) => {
          const x = o[k]; const d = (Number(x.merged) || 0) + (Number(x.closed) || 0) + (Number(x.open) || 0);
          const rv = Number(x.reverted_14d) || 0, ro = Number(x.issue_reopened_14d) || 0;
          merged += Number(x.merged) || 0; decided += d; reverted += rv; reopened += ro; unknown += Number(x.unknown) || 0;
          return `${k}: ${Number(x.merged) || 0} of ${d} with a known outcome merged (${evPct(evRate(x.merged, d))}), ${rv} reverted, ${ro} re-opened (${evPct(evRate(rv + ro, x.merged))} change failure), ${Number(x.closed) || 0} closed, ${Number(x.open) || 0} open, ${Number(x.unknown) || 0} unknown`;
        });
        const failed = reverted + reopened;
        const rr = evRate(failed, merged);
        const grade = evGrade(rr, merged, EV_REVERT_STRONG, EV_REVERT_FAIR, false);
        return {
          matters: 'A PR being accepted is not the same as the project being better off. If agent-written PRs merge far less often, or get undone (reverted, or their issue re-opened) far more often than human ones, the agents are making work rather than saving it.',
          measured: 'For every pull request (PR) that has an evidence record, who wrote it — a human, an AI agent, or a bot — and what happened to it: merged, closed without merging, still open, or merged and then undone within 14 days — either reverted by a later PR, or an issue it closed was re-opened by a person.',
          how: 'We take every PR opened in the chosen window (by the PR\u2019s open time, not when its record was written) and count outcomes per author kind, folding the newest live record with any backfilled record of the same PR. Bars are plain counts scaled to the biggest group. Merged share = merged ÷ PRs with a known outcome (merged, closed or open). Change-failure rate = (reverted + re-opened) within 14 days ÷ merged. A revert is found from text already fetched — a merged PR whose title or body says `Reverts #N`, `Revert "…"` or `This reverts commit <sha>` and names a PR merged in the previous 14 days — so a revert that names nothing is missed. A re-open is an issue the PR closed with a closing keyword that a person re-opened after the merge (issues.reopened webhook, or GitHub listing it as re-opened); the governor already tracks these to re-queue the issue. A live record only knows a merge this hive performed, and never a close, so the governor\u2019s review-outcome ledger fills the rest: when a reviewed PR leaves the open list the ledger asks GitHub what became of it (one fetch it already makes) and stamps the bundle merged-elsewhere or closed; a PR the ledger still sees open counts as open. A PR with no merge event, no backfilled record and no ledger row is "outcome unknown": counted and shown as a dashed bar, left out of the shares.',
          good: `Similar merged shares across author kinds, and a change-failure rate at or below ${Math.round(EV_REVERT_STRONG * 100)}% (Strong). ${Math.round(EV_REVERT_FAIR * 100)}% or less is Fair; above that is Weak. A big gap between agent and human merged shares is worth a look either way.`,
          ours: `${parts.length ? parts.join(' · ') + '. ' : 'No PRs in this window. '}${merged} merged in total, ${reverted} reverted and ${reopened} re-opened within 14 d (${evPct(rr)} change failure).${unknown ? ` ${unknown} PR(s) have no recorded outcome and are not in any share.` : ''} ${evLimitsSentence()}`,
          grade, reason: grade === 'none' ? `needs at least ${EV_GRADE_MIN_N} merged PRs${evWindowHint(dto)}` : `change-failure rate ${evPct(rr)} of ${merged} merged`,
        };
      },
      calibration: (dto) => {
        const calib = evCalibrationSeries(dto);
        let topN = 0, topMerged = 0, lowN = 0, lowMerged = 0, topName = '', unknown = 0;
        calib.forEach((p) => (p && p.buckets || []).forEach((b, i, arr) => {
          if (!b) return;
          const known = (Number(b.n) || 0) - (Number(b.unknown) || 0);
          unknown += Number(b.unknown) || 0;
          if (i === arr.length - 1) { topName = String(b.bucket || ''); topN += known; topMerged += Number(b.merged) || 0; }
          else if (i === 0) { lowN += known; lowMerged += Number(b.merged) || 0; }
        }));
        const topRate = evRate(topMerged, topN), lowRate = evRate(lowMerged, lowN);
        const grade = evGrade(topRate, topN, EV_CALIB_STRONG, EV_CALIB_FAIR, true);
        return {
          matters: 'Humans decide how much to trust a review by its confidence. If 90%-sure verdicts are wrong as often as 50%-sure ones, the number is noise and should not be shown to people.',
          measured: 'Whether self-reported certainty or evidence strength better predicts merges. Self-reports use a fixed verification rubric; derived scores use finding anchors, test changes, diff size, known CI and same-head perspective agreement. Neither series is a probability of correctness.',
          how: `Verdicts are grouped into confidence buckets (0.55, 0.65, 0.75, 0.85, 0.95; nearest bin, with midpoints rounded up). For each bucket we plot how often the PR ended up merged (filled dot; merged ÷ verdicts on PRs with a known outcome) and how often the verdict was a block (hollow circle; blocks ÷ all verdicts in the bucket). Dot size shows how many verdicts are in the bucket; one line per reviewer perspective. A bucket with fewer than ${EV_CALIB_MIN_BUCKET_N} known-outcome verdicts is drawn dashed and not joined to the line: one or two PRs cannot show a trend.`,
          good: `Merged rate rising from left to right: low-confidence verdicts should merge less often, and the ${topName || '0.95'} bucket should be near 90–100% merged. ${Math.round(EV_CALIB_STRONG * 100)}% or more in the top bucket is Strong, ${Math.round(EV_CALIB_FAIR * 100)}% or more is Fair, below that Weak.`,
          ours: `At ${topName || 'top'} confidence, ${evPct(topRate)} of ${topN} verdicts with a known outcome merged${lowN ? `; at the lowest bucket, ${evPct(lowRate)} of ${lowN} merged` : ''}.${unknown ? ` ${unknown} verdict(s) are on PRs with no recorded outcome and are not in these rates.` : ''} Merged share measures association with merges, not correctness. ${evLimitsSentence()}`,
          grade, reason: grade === 'none' ? `needs at least ${EV_GRADE_MIN_N} known-outcome verdicts in the top bucket${evWindowHint(dto)}` : `${evPct(topRate)} merged at ${topName} confidence`,
        };
      },
      'accuracy-by-model': (dto) => {
        const models = Array.isArray(dto.reviewer_accuracy_by_model) ? dto.reviewer_accuracy_by_model : [];
        let approvals = 0, fa = 0, blocks = 0, fb = 0, unknown = 0, advisory = 0, dissent = 0, enforcedAll = 0;
        const perModel = [];
        models.forEach((m) => {
          if (!m) return;
          let a = 0, f = 0, bl = 0, mb = 0;
          Object.values(m.by_author_kind || {}).forEach((st) => {
            // Denominators are verdicts on PRs with a known outcome; the rest is reported as unknown.
            a += (Number(st.approvals) || 0) - (Number(st.approved_unknown) || 0); f += Number(st.approved_not_merged) || 0;
            // Only blocks an enforcing gate read (policy.require_approval) can be false blocks (#12595);
            // advisory blocks on merged PRs are dissent before merge, never an error rate.
            bl += (Number(st.enforced_blocks) || 0) - (Number(st.enforced_blocked_unknown) || 0); mb += Number(st.merged_after_enforced_block) || 0;
            enforcedAll += Number(st.enforced_blocks) || 0;
            advisory += Number(st.advisory_blocks) || 0; dissent += Number(st.dissent_before_merge) || 0;
            unknown += (Number(st.approved_unknown) || 0) + (Number(st.blocked_unknown) || 0);
          });
          approvals += a; fa += f; blocks += bl; fb += mb;
          perModel.push({ model: String(m.model || 'unknown'), fa: evRate(f, a), a });
        });
        const faRate = evRate(fa, approvals), fbRate = evRate(fb, blocks);
        const allAdvisory = advisory > 0 && enforcedAll === 0;
        const dissentLine = advisory > 0
          ? (allAdvisory
            ? ` Perspective verdicts were advisory on this hive (review.require_approval off); ${dissent} blocks preceded a merge (dissent before merge, ${evPct(evRate(dissent, advisory))} of ${advisory} advisory blocks) — not counted as false blocks.`
            : ` ${advisory} block(s) were advisory (review.require_approval off when cast); ${dissent} of them preceded a merge (dissent before merge, ${evPct(evRate(dissent, advisory))}) — not counted as false blocks.`)
          : '';
        const worst = Math.max(faRate == null ? -1 : faRate, fbRate == null ? -1 : fbRate);
        const grade = evGrade(worst < 0 ? null : worst, approvals + blocks, EV_ERR_STRONG, EV_ERR_FAIR, false);
        const best = perModel.filter((m) => m.fa != null && m.a >= EV_GRADE_MIN_N).sort((x, y) => x.fa - y.fa)[0];
        return {
          matters: 'A reviewer that approves things that never merge wastes human time; one that blocks things that merge anyway teaches people to ignore it. This shows which models earn trust, and whether they treat agent and human authors alike.',
          measured: 'How often each AI reviewer model gets it wrong, split by who wrote the PR. A "false approve" is an approval on a PR whose record says it closed without merging (or was still open when backfilled). A "false block" is a block (changes requested or reject) that an enforcing gate (review.require_approval on) read, on a PR that merged anyway after a human override. A block cast while verdicts were advisory is "dissent before merge": a count and a share of advisory blocks, never an error rate.',
          how: 'For each reviewer model and author kind we count approvals and blocks, then compare each verdict with the PR\u2019s recorded outcome: false-approve rate = approvals on PRs known not to have merged ÷ approvals on PRs with a known outcome; false-block rate = enforced blocks on merged PRs ÷ enforced blocks on PRs with a known outcome, where a block is enforced when the PR\u2019s evidence bundle records policy.require_approval on. Advisory blocks on merged PRs are reported as dissent before merge (count and % of advisory blocks) and never enter the grade. Verdicts on PRs with no recorded outcome are counted and shown as unknown, not as errors. On a live-only hive a known outcome is almost always a merge, so until a backfill covers the window the false-approve rate is a lower bound and the false-block rate an upper bound; read them next to the unknown count.',
          good: `Both bars short. ${Math.round(EV_ERR_STRONG * 100)}% or less on the worse of the two rates is Strong, ${Math.round(EV_ERR_FAIR * 100)}% or less is Fair, above that Weak. Models should also look alike for human and agent authors; a model that only blocks agents is a bias, not accuracy.`,
          ours: `Across all models: false-approve ${evPct(faRate)} of ${approvals} approvals with a known outcome, false-block ${evPct(fbRate)} of ${blocks} enforced blocks with a known outcome.${dissentLine}${unknown ? ` ${unknown} verdict(s) are on PRs with no recorded outcome (unknown).` : ''}${best ? ` Lowest false-approve rate: ${best.model} at ${evPct(best.fa)}.` : ''} ${evLimitsSentence()}`,
          grade, reason: grade === 'none' ? `needs at least ${EV_GRADE_MIN_N} known-outcome verdicts${evWindowHint(dto)}` : `worse rate ${evPct(worst)}`,
        };
      },
      maintainability: (dto) => {
        const rows = evMaintainabilityRows(dto);
        const latest = rows.map((r) => r.months[r.months.length - 1]).filter(Boolean);
        const over = latest.reduce((sum, m) => sum + (Number(m.functions_over_threshold) || 0), 0);
        return {
          matters: 'Complexity that rises over time adds review and maintenance cost; this is an observation, not a causal claim about agents.',
          measured: 'Monthly Go cyclomatic complexity snapshots per repo: median, functions over threshold, and warnings by tool. ACMM markers show when each level was first observed.',
          how: 'Snapshots within the window include boundary months. Missing months and tool measurements stay gaps. n counts repos with at least two snapshots; single snapshots remain visible as points.',
          good: 'A flat or falling median and fewer functions over threshold. Complexity alone does not say whether a change was good.',
          ours: `${rows.length} repo(s) with snapshots; ${over} function(s) over threshold in their latest months. Snapshots are monthly, so a short window can hold one month or none. ACMM markers come from evidence bundles, which have only been recorded since ${EV_DATA_SINCE}.`,
          grade: 'ungraded', reason: 'report-only; no threshold to grade against',
        };
      },
      'issue-reopen': (dto) => {
        const rows = Array.isArray(dto.issue_reopen_rates_30d) ? dto.issue_reopen_rates_30d : [];
        const sum = (gate, key) => rows.filter((r) => r && Boolean(r.clarity_gate) === gate).reduce((acc, r) => acc + (Number(r[key]) || 0), 0);
        const gatedClosed = sum(true, 'closed'), gatedReopened = sum(true, 'reopened');
        const ungatedClosed = sum(false, 'closed'), ungatedReopened = sum(false, 'reopened');
        const repos = new Set(rows.filter((r) => r && Number(r.closed) > 0).map((r) => r.repo)).size;
        return {
          matters: 'An issue that is re-opened after a hive PR closed it is work that was not actually done. Splitting by the clarity-gate marker shows whether issues that were specified clearly enough to pass the gate stay closed more often than ones with no marker.',
          measured: 'Distinct issues closed by hive PRs in the last 30 days, per repository, split by whether the issue carried the clarity-gate marker at closure, with how many of them were later re-opened.',
          how: 'Closures come from evidence bundles and are placed by closure time, not by the selected PR window or author filter. Each issue is counted once (its latest closure). A re-open counts when it happened after that closure, as seen by the issues webhook or the issue listing. Rate = re-opened ÷ closed per row; issues whose historical clarity status is missing are excluded rather than counted as ungated.',
          good: 'Low reopen rates in both rows, and no large gap that favours the ungated row. Recent closures have had less time to re-open, so a short window understates the rate.',
          ours: `${repos} repo(s) with closures. Passed through the gate: ${gatedReopened} re-opened of ${gatedClosed} closed (${evPct(evRate(gatedReopened, gatedClosed))}). No marker: ${ungatedReopened} re-opened of ${ungatedClosed} closed (${evPct(evRate(ungatedReopened, ungatedClosed))}). This is a descriptive comparison, not proof of causality.`,
          grade: 'ungraded', reason: 'report-only; no threshold to grade against',
        };
      },
      'shadow-gates': (dto) => {
        const sg = dto.shadow_gate || {};
        let heldN = 0, heldReverted = 0, heldMerged = 0, passedN = 0, passedReverted = 0, shadowUnknown = 0;
        ['review_gate', 'intent'].forEach((k) => {
          const g = sg[k] || {}; const h = g.held || {}, p = g.passed || {};
          heldN += Number(h.n) || 0; heldReverted += Number(h.reverted) || 0; heldMerged += Number(h.merged) || 0;
          passedN += Number(p.n) || 0; passedReverted += Number(p.reverted) || 0;
          shadowUnknown += (Number(h.unknown) || 0) + (Number(p.unknown) || 0);
        });
        const falseAlarm = evRate(heldMerged - heldReverted, heldMerged);
        const grade = evGrade(falseAlarm, heldMerged, EV_SHADOW_FALSE_ALARM_STRONG, EV_SHADOW_FALSE_ALARM_FAIR, false);
        return {
          matters: 'Before a gate is allowed to hold real PRs, we want proof it would have caught problems without stopping good work. Shadow mode gives that proof without touching merges.',
          measured: 'What would have happened if two safety gates were switched on. In "shadow mode" the review gate and intent verification only write down whether they would have stopped a PR; nothing is actually held or blocked. The review gate is recorded on the merge-eligible lane (review.shadow) and, by default, on the self-merge lane just before the hive merges its own PR (no swarm review, review awaiting approval, or changes requested on the head); the panel splits the two lanes.',
          how: 'For each gate, PRs are split into "would have stopped" and "would have passed". Each bar shows how those PRs really turned out: merged, merged then reverted, or still open. A gate is useful when the PRs it would have stopped are the ones that later caused trouble.',
          good: `Reverts sitting mostly in the "would have stopped" bar and few in "would have passed". Of the stopped PRs that merged, ${Math.round(EV_SHADOW_FALSE_ALARM_STRONG * 100)}% or fewer merging cleanly (a false alarm) is Strong, ${Math.round(EV_SHADOW_FALSE_ALARM_FAIR * 100)}% or fewer is Fair, above that Weak.`,
          ours: `${Number(sg.n) || 0} PRs have a shadow observation. Would have stopped: ${heldN} (${heldMerged} merged, ${heldReverted} later reverted). Would have passed: ${passedN} (${passedReverted} later reverted).${shadowUnknown ? ` ${shadowUnknown} of these PRs have no recorded outcome.` : ''} ${evLimitsSentence()}`,
          grade, reason: grade === 'none' ? `needs at least ${EV_GRADE_MIN_N} stopped-and-merged PRs${evWindowHint(dto)}` : `false alarms ${evPct(falseAlarm)} of ${heldMerged} stopped PRs that merged`,
        };
      },
      'claim-vs-verdict': (dto) => {
        const weeks = (Array.isArray(dto.claim_vs_verdict) ? dto.claim_vs_verdict : []).filter((w) => w && Number(w.n) > 0);
        let claimed = 0, ciCaptured = 0, ciNotGreen = 0, ciPending = 0, notApprove = 0;
        weeks.forEach((w) => {
          claimed += Number(w.claimed) || 0; ciCaptured += Number(w.ci_captured) || 0; ciNotGreen += Number(w.ci_not_green) || 0; ciPending += Number(w.ci_pending) || 0;
          notApprove += Number(w.verdict_not_approve) || 0;
        });
        const rec = dto.ci_recovery && typeof dto.ci_recovery === 'object' ? dto.ci_recovery : {};
        const red = rec.red || {}, gate = rec.gate || {}, conv = rec.convergence || {}, pol = rec.policy || {};
        const passes = conv.passes || {};
        const settled = Number(conv.settled) || 0;
        const never = Number(passes.never) || 0;
        const abandon = evRate(never, settled);
        const firstPass = evRate(passes['1'], settled - never);
        const gradeA = evGateGrade(gate.breaches, gate.with_checks);
        const gradeB = evConvergenceGrade(conv.median_passes != null ? Number(conv.median_passes) : null, abandon, settled);
        const grade = evLowerGrade(gradeA, gradeB);
        const repoReq = pol.repo_required_checks && typeof pol.repo_required_checks === 'object' ? Object.keys(pol.repo_required_checks).sort().map(r => `${r}: ${(pol.repo_required_checks[r] || []).join(', ') || 'none'}`) : [];
        const reqCopy = `${Array.isArray(pol.required_checks) && pol.required_checks.length ? `required checks (default): ${pol.required_checks.join(', ')}` : 'no default required checks configured'}${repoReq.length ? `; per repo: ${repoReq.join('; ')}` : ''}`;
        const policy = pol.self_authored_automerge === false ? 'self-authored auto-merge is OFF on this hive, so a human merges after CI' : `self-authored auto-merge is ON (auto_merge.self_authored), so a red check simply holds the PR until the agent pushes a fix; ${reqCopy}`;
        const rsettled = (Number(red.recovered_merged) || 0) + (Number(red.abandoned) || 0);
        const cost = gate.median_green_to_merge_hours != null ? ` Gate latency: median ${Math.round(Number(gate.median_green_to_merge_hours) * 10) / 10} h from first green on the merged head to merge (${Number(gate.green_to_merge_n) || 0} PRs).` : '';
        return {
          matters: 'This hive hands off to CI by policy: agents open the PR as soon as they believe it is done and do not wait for checks; CI kicks back what is not ready. Two things then matter. Safety: did anything merge without green checks (gate integrity)? Efficiency: how many CI passes does an agent PR need before it is green, and how many never get there (convergence)? CI red at the moment of the claim is the design, not a defect, and is disclosed but not graded.',
          measured: 'Grade A, gate integrity: every merged PR in the window, any author, by the state of its checks on the head that merged — green, a confirmed breach (red or a policy-required check missing per GitHub), suspected (red only in a review-time snapshot, unconfirmed), still pending, or unknown. Grade B, convergence: for agent-authored PRs, the number of CI passes (distinct head SHAs that CI evaluated) until the first green one, bucketed 1 / 2 / 3 / 4+ / never (closed without merge and without a green), per ISO week of PR open time, plus the cumulative share green by pass k. Disclosed facts: CI state at claim, swarm non-approve verdicts, and the recovery read of red-at-claim PRs. Secondary: gate latency, first green → merge.',
          how: `The merged head is GitHub’s (the backfilled record’s head), else the newest reviewed head. Its state is GitHub’s check-run conclusions from the PR’s commit trail where the collector has reached it, else the newest live snapshot for that head. A review-time snapshot can predate a re-run on the same head, so a snapshot that was red is only "suspected" (${Number(gate.suspected) || 0}, listed, enriched first) and never a breach until GitHub confirms it; a snapshot that was green counts as green (${Number(gate.green_from_bundles) || 0} of the green rest on bundles). Merged PRs with no state on the merged head are "unknown" (${Number(gate.unknown) || 0}). Breach = a REQUIRED check GitHub reports red on the merged head, as the hive’s own merge gate judges it: the trail resolves the base branch’s required set as it is now (branch protection, else the configured required checks for that repo — auto_merge.repos.<repo>.required_checks, else the default; ${reqCopy}) and judges only those. A bundle-only snapshot is judged against the same per-repo config set, never the default alone, so the primary repo’s gate check is not demanded of repos that do not have it; a green snapshot missing a config-required check is queued for GitHub’s read, which confirms or clears it. A red check outside that set (a docs link check, a labeller, a cancelled optional build) never blocked the merge and is not a breach — it is disclosed (${Number(gate.non_required_red) || 0} of the green merged with one). Three readings cannot be confirmed and are “suspected”, listed, outside the rate: a required check with no run on the merged head (the gate accepts a path-filtered skip; this panel cannot verify one), a red read where the required set could not be resolved (${Number(gate.required_unknown) || 0} GitHub reads were judged on every check), and a review-time snapshot. A check made required after an older merge reads red for that merge, so each breach carries its merge date and who merged it: a breach is a bypass by the merger, not a fault of the author. Every breach is listed with its PR, the red check names, author kind and merger. Gate integrity = (with_checks − breaches) ÷ with_checks, where with_checks = green + breaches + pending; suspected and unknown are outside it. A pass is one CI evaluation of a new head SHA: the hive reads each PR’s commits and check-run conclusions through its app token (hourly, a capped batch per pass, red-at-claim and merged-unknown PRs first) into a durable rollup, scanning from the first commit until the first green or the fourth evaluated head; until a PR’s trail arrives, its live snapshots stand in (${Number(conv.from_github) || 0} settled reads from GitHub, ${Number(conv.from_bundles) || 0} from bundles only — bundles can miss heads nobody reviewed). Median passes counts 4+ as 4. Abandoned share = never ÷ settled; in-flight (open, not yet green: ${Number(conv.in_flight) || 0}) and unknown (${Number(conv.unknown) || 0}) are counts outside every denominator. First-pass share = green on pass 1 ÷ PRs that went green: a neutral line (the RFC’s C1 number), not graded. Policy: ${policy}. No knob makes agents wait for CI; the merge gate requiring green checks is the policy.`,
          good: `Grade A: 0 breaches is Excellent, at most ${Math.round(EV_GATE_BREACH_GOOD * 100)}% of merged-with-known-state Good, at most ${Math.round(EV_GATE_BREACH_FAIR * 100)}% Fair, more Weak. Grade B needs both conditions: median passes ≤ ${EV_CONV_PASSES_EXCELLENT} and under ${Math.round(EV_CONV_ABANDON_EXCELLENT * 100)}% abandoned is Excellent; ≤ ${EV_CONV_PASSES_GOOD} and under ${Math.round(EV_CONV_ABANDON_GOOD * 100)}% Good; ≤ ${EV_CONV_PASSES_FAIR} and under ${Math.round(EV_CONV_ABANDON_FAIR * 100)}% Fair; otherwise Weak. The panel grade is the lower of A and B. Red-at-claim share and swarm non-approve are not graded.`,
          ours: `Gate: ${Number(gate.merged_n) || 0} PRs merged, ${Number(gate.with_checks) || 0} with a known merge-time state → ${Number(gate.green) || 0} green, ${Number(gate.breaches) || 0} confirmed breach(es), ${Number(gate.pending) || 0} merged with checks still running; ${Number(gate.suspected) || 0} suspected (unconfirmed) and ${Number(gate.unknown) || 0} unknown outside the rate (${EV_GRADES[gradeA]}).${cost} Convergence: ${Number(conv.n) || 0} agent PRs, ${settled} settled → pass 1: ${Number(passes['1']) || 0} · 2: ${Number(passes['2']) || 0} · 3: ${Number(passes['3']) || 0} · 4+: ${Number(passes['4+']) || 0} · never: ${never}; median ${conv.median_passes != null ? conv.median_passes : '—'} pass(es), ${evPct(abandon)} abandoned, first-pass share ${evPct(firstPass)} (${EV_GRADES[gradeB]}). Disclosed: ${claimed} agent PRs claimed done across ${weeks.length} week(s); at claim ${Number((rec.at_claim || {}).green) || 0} green · ${Number((rec.at_claim || {}).red) || 0} red · ${Number((rec.at_claim || {}).pending) || 0} pending · ${Number((rec.at_claim || {}).none) || 0} no captured checks (bundle view: ${ciNotGreen} of ${ciCaptured} settled not green, ${ciPending} pending); ${notApprove} got a non-approve swarm verdict; of ${Number(red.n) || 0} red at claim, ${Number(red.recovered_merged) || 0} recovered and merged (${evPct(evRate(red.recovered_merged, rsettled))} of ${rsettled} settled), ${Number(red.abandoned) || 0} abandoned, ${Number(red.outcome_unknown) || 0} outcome unknown. ${evLimitsSentence()}`,
          grade, reason: grade === 'none' ? `needs at least ${EV_GRADE_MIN_N} merged PRs with a known merge-time check state and ${EV_GRADE_MIN_N} settled agent PRs${evWindowHint(dto)}` : `A gate integrity ${EV_GRADES[gradeA]}: ${Number(gate.breaches) || 0} breach(es) in ${Number(gate.with_checks) || 0} · B convergence ${EV_GRADES[gradeB]}: median ${conv.median_passes != null ? conv.median_passes : '—'} pass(es), ${evPct(abandon)} abandoned`,
        };
      },
      'work-balance': (dto) => {
        const weeks = (Array.isArray(dto.work_balance) ? dto.work_balance : []).filter((w) => w && Number(w.n) > 0);
        const src = dto.work_balance_sources && typeof dto.work_balance_sources === 'object' ? dto.work_balance_sources : {};
        const opened = { human: 0, agent: 0, bot: 0 }, prsOpened = { human: 0, agent: 0, bot: 0 };
        let closed = 0, merged = 0, prsClosed = 0;
        weeks.forEach((w) => {
          Object.keys(opened).forEach((k) => { opened[k] += Number((w.opened || {})[k]) || 0; prsOpened[k] += Number((w.prs_opened || {})[k]) || 0; });
          closed += Number(w.closed) || 0; merged += Number(w.prs_merged) || 0; prsClosed += Number(w.prs_closed) || 0;
        });
        const openedTotal = opened.human + opened.agent + opened.bot;
        const prsTotal = prsOpened.human + prsOpened.agent + prsOpened.bot;
        const issueRatio = evRate(closed, openedTotal), prRatio = evRate(merged, prsTotal);
        const issueGrowth = evQueueGrowth(weeks, 'queue_issues'), prGrowth = evQueueGrowth(weeks, 'queue_prs');
        const issueGrade = evThroughputGrade(issueRatio, openedTotal, issueGrowth);
        const prGrade = evThroughputGrade(prRatio, prsTotal, prGrowth);
        const grade = evLowerGrade(issueGrade, prGrade);
        const last = weeks.length ? weeks[weeks.length - 1] : {};
        const depth = last.queue_issues != null || last.queue_prs != null ? `Queue at the end of ${last.week}: ${last.queue_issues != null ? last.queue_issues : '—'} open issues · ${last.queue_prs != null ? last.queue_prs : '—'} open PRs.` : 'No week-end queue depth sample in this window.';
        const hrs = evWindowHours();
        const short = hrs <= EV_SHORT_WINDOW_HOURS ? ` Only ${hrs}h of data is in this window, so treat the grade as an early read.` : '';
        const auditNote = src.issue_series_weeks ? `${src.issue_series_weeks} historical repository-week(s) come from backfilled GitHub issue history; ${src.audit_only_weeks || 0} week(s) include audit fallback. The collector remains authoritative within its coverage.` : src.issues_github_since ? `Issue opens and closes come from GitHub (all authors, including humans and third-party bots) from ${src.issues_github_since}; ${src.audit_only_weeks ? `${src.audit_only_weeks} earlier week(s) fall back to the hive audit log, which only sees hive-mediated issue writes.` : 'every week in this window is GitHub-sourced.'}` : 'GitHub issue history has not been fetched yet on this hive, so issue opens and closes come from the hive audit log only: issues humans or other bots open directly on GitHub are not observed.';
        return {
          matters: 'The question is whether the queue keeps up: are issues and PRs being closed and merged about as fast as they are opened, and is the pile of open work at the end of each week shrinking, flat or growing? Raw volume is not the point — a hive that opens 400 issues a week and closes 400 is fine.',
          measured: 'Each ISO week: issues opened (stacked by human / agent / bot) against issues closed; PRs opened (same split) against PRs merged and PRs closed without merge; and the number of actionable open issues and open PRs at the end of the week — the same queue the dashboard’s queue section shows.',
          how: `${auditNote} PR opens, merges and closes come from the evidence bundles (live and backfilled) for PRs opened in the window; a PR’s author kind is the bundle’s. Week-end queue depth is the governor’s last state snapshot of each week (actionable open issues / open PRs, 30-day retention). Ratios: issues closed ÷ issues opened and PRs merged ÷ PRs opened over the whole window — nothing is dropped and PRs closed without merge are shown but not counted as merged.`,
          good: `Issues and PRs are graded separately and the card takes the lower. Ratio at or above ${Math.round(EV_THROUGHPUT_EXCELLENT * 100)}% is Excellent, ${Math.round(EV_THROUGHPUT_GOOD * 100)}% Good, ${Math.round(EV_THROUGHPUT_FAIR * 100)}% Fair, below that Weak — and any grade becomes Weak if the week-end queue grew more than ${Math.round(EV_QUEUE_GROWTH_MAX * 100)}% from the previous week.`,
          ours: `Issues: ${openedTotal} opened (human ${opened.human} · agent ${opened.agent} · bot ${opened.bot}), ${closed} closed → ${evPct(issueRatio)}, ${EV_GRADES[issueGrade]}${issueGrowth != null ? ` (queue ${issueGrowth >= 0 ? '+' : ''}${Math.round(issueGrowth * 100)}% week over week)` : ''}. PRs: ${prsTotal} opened (human ${prsOpened.human} · agent ${prsOpened.agent} · bot ${prsOpened.bot}), ${merged} merged, ${prsClosed} closed without merge → ${evPct(prRatio)}, ${EV_GRADES[prGrade]}${prGrowth != null ? ` (queue ${prGrowth >= 0 ? '+' : ''}${Math.round(prGrowth * 100)}% week over week)` : ''}. ${depth}${short}`,
          grade, reason: grade === 'none' ? `needs at least ${EV_GRADE_MIN_N} issues and ${EV_GRADE_MIN_N} PRs opened in the window` : `issues ${evPct(issueRatio)} closed ÷ opened (${EV_GRADES[issueGrade]}) · PRs ${evPct(prRatio)} merged ÷ opened (${EV_GRADES[prGrade]}); lower wins`,
        };
      },
      'boundary-enforcement': (dto) => {
        const act = dto.write_activity && typeof dto.write_activity === 'object' ? dto.write_activity : {};
        const direct = act.direct || {}, mediated = act.mediated || {};
        const total = Number(act.total_writes) || 0, blocked = Number(act.blocked) || 0;
        const byTier = direct.by_tier || {};
        const tiers = Object.keys(byTier).filter((k) => byTier[k] && (Number(byTier[k].attempted) > 0 || Number(byTier[k].blocked) > 0));
        const topReason = Object.entries(direct.blocked_by_reason || {}).filter(([, v]) => Number(v) > 0).sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1))[0];
        const intent = dto.intent_counters && typeof dto.intent_counters === 'object' ? dto.intent_counters : {};
        let wouldBlock = 0, evaluated = 0;
        Object.values(intent).forEach((c) => { wouldBlock += Number(c && c.blocked) || 0; evaluated += Number(c && c.attempted) || 0; });
        const sentinel = dto.sentinel_hits_per_week || {};
        const sentinelWeeks = Array.isArray(sentinel.weeks) ? sentinel.weeks : [];
        const sentinelHits = sentinelWeeks.reduce((sum, wk) => sum + (Number(wk && wk.hits) || 0), 0);
        const blockedRate = evRate(blocked, total);
        const grade = evGrade(blockedRate, total, EV_BLOCKED_STRONG, EV_BLOCKED_FAIR, false);
        const coverage = direct.recorded === false ? 'Direct-write counters have not been recorded on this hive yet, so the total only holds hive-mediated writes.' : direct.partial_coverage ? `Direct-write counters have only been recorded since ${direct.since}, so earlier weeks in this window hold hive-mediated writes only.` : '';
        return {
          matters: 'Agents change GitHub either through the hive (comments, issues, PRs, reviews it files on their behalf, all audited) or directly through a proxy that enforces the trust tiers. A handful of refusals out of thousands of writes means the boundary is doing its job quietly; a refusal rate in the percent range means agents are repeatedly trying things they are not allowed to do.',
          measured: 'The headline is blocked ÷ ALL agent GitHub writes in the window: every hive-mediated write from the audit log plus every direct write the proxy saw, per trust tier, and how many the proxy refused and why. Separately: how many evidence records per week carry a "sentinel hit", a canary that fires when an agent touched something it should not have.',
          how: `Denominator = hive-mediated writes (audit entries for agent comments, issues, PRs, reviews, labels, closes and pushes) + direct proxied writes (the proxy’s weekly tally per tier, kept under /data so it survives restarts). Numerator = writes the proxy refused (plus audited refusals of mediated writes). Per-tier bars show blocked ÷ direct writes for that tier. Would-block counts from intent verification are shown separately and are never in the blocked count. ${coverage}`,
          good: `Blocked at or below ${Math.round(EV_BLOCKED_STRONG * 100)}% of all agent writes is Strong — a boundary that is exercised but rarely needed — ${Math.round(EV_BLOCKED_FAIR * 100)}% or less is Fair, above that Weak. The sentinel line should sit at zero; any hit is worth reading the record for.`,
          ours: `${formatNumber(total)} agent writes in this window (${formatNumber(Number(mediated.n) || 0)} hive-mediated · ${formatNumber(Number(direct.attempted) || 0)} direct across ${tiers.length} tier(s)), ${blocked} blocked (${blockedRate == null ? '—' : (blockedRate * 100).toFixed(2) + '%'})${topReason ? `, most often for ${topReason[0]} (${topReason[1]})` : ''}.${evaluated ? ` Intent verification evaluated ${evaluated} write(s) and would have blocked ${wouldBlock} (not counted above).` : ''} ${sentinel.n ? `${sentinel.n} record(s) with ${sentinelHits} sentinel hit(s)` : 'No sentinel hits'} in this window. ${coverage} Direct writes that bypass both the hive and the proxy are not seen at all.`,
          grade, reason: grade === 'none' ? `needs at least ${EV_GRADE_MIN_N} agent writes` : `${blocked} of ${formatNumber(total)} agent writes blocked (${blockedRate == null ? '—' : (blockedRate * 100).toFixed(2) + '%'})`,
        };
      },
      'opens-merges': (dto) => {
        const om = dto.issue_opens_pr_merges && typeof dto.issue_opens_pr_merges === 'object' ? dto.issue_opens_pr_merges : {};
        const total = om.total || {}, band = om.expected || {}, roll = om.rolling_30d || {}, rollBand = om.rolling_30d_expected || {};
        const repos = Array.isArray(om.repos) ? om.repos : [];
        const n = Number(om.n) || 0;
        const grade = n >= EV_GRADE_MIN_N ? (om.status === 'in_band' ? 'strong' : 'weak') : 'none';
        return {
          matters: 'A hive that files issues faster than it lands PRs is manufacturing backlog; one that only merges and never scouts runs dry. Neither is a flat 1:1 target: when the queue is busy the hive should drain it (merges ahead of opens), and when it is quiet it may scout ahead (opens ahead of merges) within a bound. This card reads the ratio against what the current cadence mode expects, not against 1.0.',
          measured: 'Issues the hive itself opened (through its GitHub App identity, for an agent or as hive automation) ÷ PRs the hive merged or wrote, per repository and fleet-wide, in the selected window; the governor’s current cadence mode and the band it expects; and the same ratio over a fixed rolling 30 days, which should regress to parity.',
          how: `Opens are the hive audit log’s issue-create actions (agent-requested and hive-automation issues) — issues humans or third-party bots open directly on GitHub are not in the log and never count. Merges are evidence-bundle PRs that merged where this hive performed the merge or the author kind is agent (the Outcome by author kind card’s agent-authored set); a human or bot PR merged by someone else does not count. Bands: surge / busy expect ≤ ${EV_OM_DRAIN_MAX} opens per merge (drain), quiet / idle expect ${EV_OM_SCOUT_MIN}–${EV_OM_SCOUT_MAX} (scout, bounded), unknown mode and the rolling 30 d expect parity ±${Math.round(EV_OM_PARITY_TOLERANCE * 100)}%. The sparkline plots a trailing ${EV_OM_SPARK_TRAIL_DAYS}-day ratio per day with the parity band shaded.`,
          good: `Inside the band for the current mode. Over ${EV_OM_ROLLING_DAYS} days the ratio should sit near 1:1 whatever the mode did day to day: a sustained reading above the band means the backlog is growing faster than it is worked; below it in quiet mode means the hive is not scouting even though it has room.`,
          ours: `${om.headline || 'No reading yet.'} ${total.opens != null ? `${total.opens} hive issue opens against ${total.merges} hive PR merges across ${repos.length} repo(s) in this window.` : ''} Rolling ${EV_OM_ROLLING_DAYS} d: ${roll.opens || 0} opens / ${roll.merges || 0} merges = ${evOmRatioText(roll)} (${evOmStatusLabel(om.rolling_30d_status)}; expects ${rollBand.label || '—'}).`,
          grade, reason: grade === 'none' ? `needs at least ${EV_GRADE_MIN_N} hive issue opens + PR merges in the window` : `${evOmRatioText(total)} opens per merge, ${om.status === 'in_band' ? 'inside' : om.status === 'above' ? 'above' : 'below'} the ${band.mode || 'unknown'} band ${band.label || ''}`,
        };
      },
    };

    // Detail text that used to sit inline under a chart (denominators, policy,
    // coverage caveats). Each panel renderer stashes it here and the ⓘ popover
    // shows it as a "Details" section (#13204).
    const evDetails = {};
    // Letter grade (A+ … C-, or — for not enough data) computed by the server
    // in pkg/evidence/grade.go and shipped as /api/evidence/summary cards[key].
    function evServerCard(dto, key) {
      if (key === 'calibration' && evConfidenceSeries === 'derived') key = 'calibration-derived';
      const c = dto && dto.cards && typeof dto.cards === 'object' ? dto.cards[key] : null;
      return c && typeof c === 'object' ? c : null;
    }
    function evExplainerHtml(key, title, dto, note) {
      const fn = EV_EXPLAINERS[key];
      if (!fn) return { button: '', body: '' };
      const x = fn(dto || {});
      const id = `ev-explain-${key}`;
      const open = evExplainOpen.has(key);
      const section = (h, body) => body ? `<h4>${h}</h4><p>${escapeHtml(body)}</p>` : '';
      const sc = evServerCard(dto, key);
      const g = sc && sc.grade && typeof sc.grade === 'object' ? sc.grade : null;
      const gradeHtml = g
        ? `<div class="ev-grade-rubric"><b>${g.enough_data ? `Grade ${escapeHtml(String(g.letter || '—'))}` : 'Not graded yet'}:</b> ${escapeHtml(String(g.reason || ''))}.${g.rubric ? ` Rubric: ${escapeHtml(String(g.rubric))}.` : ''}</div>`
        : evGradeLine(x.grade, x.reason);
      const button = `<button type="button" class="ev-info" data-action="toggleEvidenceExplainer" data-arg0="${key}" data-stop="1" data-prevent="1" aria-expanded="${open ? 'true' : 'false'}" aria-controls="${id}" aria-label="Explain ${escapeHtml(title)}">ⓘ</button>`;
      const body = `<div id="${id}" class="ev-explainer" role="region" aria-label="${escapeHtml(title)} explained"${open ? '' : ' hidden'}>${section('What is measured', x.measured)}${section('Why it matters', x.matters)}${section('How it is measured', x.how)}${section('What good looks like', x.good)}${section('What ours shows', x.ours)}${section('Details', evDetails[key])}${section('What this does not show', note)}${gradeHtml}</div>`;
      return { button, body };
    }
    function toggleEvidenceExplainer(key) {
      if (evExplainOpen.has(key)) evExplainOpen.delete(key); else evExplainOpen.add(key);
      const card = document.getElementById('evidence-card');
      const pop = card && card.querySelector(`#ev-explain-${CSS.escape(key)}`);
      const btn = card && card.querySelector(`.ev-info[data-arg0="${CSS.escape(key)}"]`);
      const open = evExplainOpen.has(key);
      if (pop) pop.hidden = !open;
      if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
      const details = pop && pop.closest('details');
      if (open && details && !details.open) details.open = true;
    }
    window.toggleEvidenceExplainer = toggleEvidenceExplainer;
    // Esc closes any open explainer and returns focus to its ⓘ button.
    document.addEventListener('keydown', (e) => {
      if (e.key !== 'Escape' || !evExplainOpen.size) return;
      const card = document.getElementById('evidence-card');
      if (!card) return;
      const keys = Array.from(evExplainOpen);
      keys.forEach(toggleEvidenceExplainer);
      const btn = card.querySelector(`.ev-info[data-arg0="${CSS.escape(keys[keys.length - 1])}"]`);
      if (btn) btn.focus();
    });

    function evPanel(key, title, n, bodyHtml, note) {
      const card = document.getElementById('evidence-card');
      const prev = card && card.querySelector(`details[data-ev-panel="${key}"]`);
      const open = prev ? prev.open : true;
      const ex = evExplainerHtml(key, title, evLast, note);
      const sc = evServerCard(evLast, key);
      const g = sc && sc.grade && typeof sc.grade === 'object' ? sc.grade : null;
      const band = g && ['a', 'b', 'c'].includes(g.band) ? g.band : 'none';
      const letter = g && g.letter ? String(g.letter) : '—';
      const why = g ? (g.enough_data ? String(g.reason || '') : `Not enough data yet: ${g.reason || ''}`) : 'Grade not reported by this hive';
      const grade = `<span class="ev-letter ev-band-${band}" title="${escapeHtml(why)}" aria-label="Grade ${escapeHtml(letter)}: ${escapeHtml(why)}">${escapeHtml(letter)}</span>`;
      const plain = sc && sc.plain_summary ? `<div class="ev-plain">${escapeHtml(String(sc.plain_summary))}</div>` : '';
      return `<details class="ev-panel" data-ev-panel="${key}"${open ? ' open' : ''}><summary>${escapeHtml(title)}${ex.button}<span class="ev-n" title="Samples behind this panel">n=${Number(n) || 0}</span>${grade}</summary>${plain}${ex.body}${bodyHtml}</details>`;
    }

    // Panel 1 (#12364): PR outcome by author kind. One row per kind with a
    // grouped bar for merged / closed / reverted within 14 d of merge, each
    // scaled against the largest kind so rows are comparable. Rates in the
    // tooltip exclude still-open PRs.
    const EV_AUTHOR_KINDS = ['human', 'agent', 'bot'];
    // viewBox width of every .ev-svg; the stylesheet pins the rendered width
    // to the same px so labels keep the dashboard's font size (1 unit = 1px).
    const EV_SVG_W = 320;
    // Report-only, within-repo on/off contrasts. The server guards each
    // metric by its own denominator, not merely the number of tagged PRs.
    function evControlArmHtml(control) {
      control = control || {};
      const repos = Array.isArray(control.by_repo) ? control.by_repo : [];
      const minN = Math.max(20, Number(control.min_n) || 20);
      const unassigned = Number(control.unassigned) || 0;
      const note = `<div class="ev-note">Control arm: Hive dispatch on vs scheduled off, within repo. ${unassigned} PR(s) unassigned or conflicting; excluded, never inferred as on. Δ = on − off; withheld below n=${minN} in either metric denominator. Descriptive, not causal; assignment/collection rollout is not yet enabled.</div>`;
      if (!repos.length) return note + '<div class="ev-note">No prospectively assigned control-window observations.</div>';
      const metrics = [
        ['merged_rate', 'Merged / known outcomes'], ['closed_rate', 'Closed / known outcomes'],
        ['open_rate', 'Open / known outcomes'], ['reverted_14d_rate', 'Reverted / merged (14 d)'],
        ['issue_reopened_14d_rate', 'Issue reopened / merged (14 d)'], ['ci_pass_rate', 'CI pass / settled CI'],
        ['median_time_to_merge_hours', 'Median open → merge (hours)'],
      ];
      return note + repos.map((r) => {
        const on = r.on || {}, off = r.off || {}, deltas = r.delta || {};
        const rows = metrics.map(([key, label]) => {
          const a = (on.metrics || {})[key] || {}, b = (off.metrics || {})[key] || {};
          const hours = key === 'median_time_to_merge_hours';
          const format = (v) => v == null ? '—' : hours ? Number(v).toFixed(1) : (Number(v) * 100).toFixed(1) + '%';
          const d = deltas[key];
          const delta = d == null || Number(a.n) < minN || Number(b.n) < minN ? 'withheld' : (Number(d) > 0 ? '+' : '') + (hours ? Number(d).toFixed(1) + ' h' : (Number(d) * 100).toFixed(1) + ' pp');
          return `<tr><td>${label}</td><td>${format(a.value)} (n=${Number(a.n) || 0})</td><td>${format(b.value)} (n=${Number(b.n) || 0})</td><td>${delta}</td></tr>`;
        }).join('');
        return `<div class="ev-note"><b>${escapeHtml(r.repo || '')}</b> · on: ${Number((on.outcomes || {}).n) || 0} opened · off: ${Number((off.outcomes || {}).n) || 0} opened (unequal exposure; not a throughput comparison)</div><div class="ev-table-wrap"><table class="ev-table"><thead><tr><th>Control-arm metric</th><th>On</th><th>Off</th><th>Δ on − off</th></tr></thead><tbody>${rows}</tbody></table></div>`;
      }).join('');
    }

    function evOutcomesSvg(outcomes) {
      outcomes = outcomes || {};
      const kinds = EV_AUTHOR_KINDS.concat(Object.keys(outcomes).filter((k) => !EV_AUTHOR_KINDS.includes(k)).sort());
      const rows = kinds.map((kind) => {
        const o = outcomes[kind] || {};
        const reverted = Number(o.reverted_14d) || 0, reopened = Number(o.issue_reopened_14d) || 0;
        return { kind, n: Number(o.n) || 0, merged: Number(o.merged) || 0, mergedElsewhere: Number(o.merged_elsewhere) || 0, closed: Number(o.closed) || 0, open: Number(o.open) || 0, unknown: Number(o.unknown) || 0, reverted, reopened, failed: reverted + reopened };
      }).filter((r) => r.n > 0);
      if (!rows.length) return '<div class="ev-empty">— no PRs with an evidence bundle in this window</div>';
      const w = EV_SVG_W, label = 70, valW = 40, barH = 7, gap = 2, rowH = 4 * (barH + gap) + 8, t = 4;
      const barW = w - label - valW;
      const h = t + rows.length * rowH;
      const maxN = Math.max(1, ...rows.map((r) => r.n));
      const pct = (num, den) => { const v = evRate(num, den); return v == null ? '—' : Math.round(v * 100) + '%'; };
      const body = rows.map((row, i) => {
        const y0 = t + i * rowH;
        const known = row.merged + row.closed + row.open;
        const tip = escapeHtml(`${row.kind} authors: merged ${row.merged} (${pct(row.merged, known)} of ${known} with a known outcome; ${row.mergedElsewhere} merged by someone other than this hive) · closed ${row.closed} · open ${row.open} · undone within 14 d ${row.failed} (${row.reverted} reverted, ${row.reopened} issue re-opened; ${pct(row.failed, row.merged)} of merged = change-failure rate) · outcome unknown ${row.unknown} (not in any rate) · n=${row.n}`);
        const bar = (cls, v, k) => {
          const yy = y0 + k * (barH + gap);
          return `<rect class="ev-bar ${cls}" x="${label}" y="${yy}" width="${((v / maxN) * barW).toFixed(1)}" height="${barH}"><title>${tip}</title></rect><text class="ev-val" x="${w - 2}" y="${yy + barH}" text-anchor="end">${v}</text>`;
        };
        return `<g><text class="ev-lbl" x="0" y="${y0 + 13}">${escapeHtml(row.kind)}<title>${tip}</title></text><text class="ev-sub" x="0" y="${y0 + 24}">n=${row.n}</text>${bar('om', row.merged, 0)}${bar('oc', row.closed, 1)}${bar('or', row.failed, 2)}${bar('ou', row.unknown, 3)}</g>`;
      }).join('');
      return `<svg class="ev-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="PR outcome by author kind: merged, closed, undone within 14 days (reverted or issue re-opened) and outcome unknown for human, agent and bot authors">${body}</svg><div class="ev-legend"><span><i class="om"></i>merged</span><span><i class="oc"></i>closed without merge</span><span><i class="or"></i>undone within 14 d of merge (reverted or issue re-opened)</span><span><i class="ou"></i>outcome unknown (not in rates)</span></div>`;
    }

    function evCalibrationSvg(calib) {
      const rows = calib.filter((p) => p && Array.isArray(p.buckets) && p.buckets.length);
      if (!rows.length) return '<div class="ev-empty">— no reviewer verdicts with a confidence in this window</div>';
      const names = rows[0].buckets.map((b) => String((b && b.bucket) || ''));
      const w = EV_SVG_W, h = 150, l = 32, r = 20, t = 8, b = 22;
      const pw = w - l - r, ph = h - t - b;
      const x = (i) => l + (names.length > 1 ? (i * pw) / (names.length - 1) : pw / 2);
      const y = (v) => t + ph - v * ph;
      const maxN = Math.max(1, ...rows.map((p) => Math.max(0, ...p.buckets.map((k) => Number(k && k.n) || 0))));
      const radius = (n) => (2 + 5 * Math.sqrt(n / maxN)).toFixed(1);
      const pct = (v) => (v == null ? '—' : Math.round(v * 100) + '%');
      let grid = '';
      [0, 0.5, 1].forEach((v) => {
        grid += `<line class="ev-axis" x1="${l}" x2="${w - r}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}"/><text x="${l - 4}" y="${(y(v) + 3).toFixed(1)}" text-anchor="end">${v * 100}%</text>`;
      });
      names.forEach((name, i) => {
        grid += `<text x="${x(i).toFixed(1)}" y="${h - 6}" text-anchor="middle">${escapeHtml(name)}</text>`;
      });
      const series = rows.map((p, pi) => {
        const persp = String(p.perspective || 'unknown');
        // Merged rate is over verdicts whose PR has a recorded outcome; a
        // bucket with fewer than EV_CALIB_MIN_BUCKET_N of those is drawn
        // dashed and left out of the line so two PRs cannot paint a trend.
        const pts = p.buckets.map((k, i) => {
          const n = Number(k && k.n) || 0, unknown = Number(k && k.unknown) || 0, known = Math.max(0, n - unknown);
          return { i, n, unknown, known, thin: known < EV_CALIB_MIN_BUCKET_N, merge: evRate(k && k.merged, known), block: evRate(k && k.blocks, n) };
        }).filter((q) => q.n > 0);
        const solid = pts.filter((q) => !q.thin && q.merge != null);
        const line = solid.map((q) => `${x(q.i).toFixed(1)},${y(q.merge).toFixed(1)}`).join(' ');
        const dots = pts.map((q) => {
          const tip = escapeHtml(`${persp} · confidence ${names[q.i]}: merged ${pct(q.merge)} of ${q.known} with a known outcome · blocked ${pct(q.block)} of ${q.n} · outcome unknown ${q.unknown} · n=${q.n}${q.thin ? ` · fewer than ${EV_CALIB_MIN_BUCKET_N} known outcomes: not joined to the line` : ''}`);
          const thin = q.thin ? ' ev-thin' : '';
          const mergeDot = q.merge == null ? '' : `<circle class="ev-pt${thin}" cx="${x(q.i).toFixed(1)}" cy="${y(q.merge).toFixed(1)}" r="${radius(q.n)}"><title>${tip}</title></circle>`;
          return `${mergeDot}<circle class="ev-pt-block${thin}" cx="${x(q.i).toFixed(1)}" cy="${y(q.block).toFixed(1)}" r="${radius(q.n)}"><title>${tip}</title></circle>`;
        }).join('');
        return `<g class="ev-c${pi % EV_PALETTE_SIZE}">${solid.length > 1 ? `<polyline class="ev-line" points="${line}"/>` : ''}${dots}</g>`;
      }).join('');
      const unknownTotal = rows.reduce((sum, p) => sum + p.buckets.reduce((s2, k) => s2 + (Number(k && k.unknown) || 0), 0), 0);
      const legend = rows.map((p, pi) => `<span class="ev-c${pi % EV_PALETTE_SIZE}"><i class="dot"></i>${escapeHtml(String(p.perspective || 'unknown'))} (n=${Number(p.n) || 0})</span>`).join('');
      return `<svg class="ev-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="Reviewer calibration: merge rate (known outcomes only) and block rate by confidence bucket per perspective">${grid}${series}</svg><div class="ev-legend">${legend}<span>● merged rate (known outcomes) · ○ blocked rate · point size = samples · dashed = under ${EV_CALIB_MIN_BUCKET_N} known outcomes${unknownTotal ? ` · ${unknownTotal} verdict(s) on PRs with unknown outcome` : ''}</span></div>`;
    }

    function evAccuracySvg(models) {
      const rows = [];
      models.forEach((m) => {
        if (!m) return;
        const model = String(m.model || 'unknown');
        const by = m.by_author_kind || {};
        Object.keys(by).sort().forEach((kind) => {
          const s = by[kind] || {};
          const approvalsKnown = (Number(s.approvals) || 0) - (Number(s.approved_unknown) || 0);
          const blocksKnown = (Number(s.enforced_blocks) || 0) - (Number(s.enforced_blocked_unknown) || 0);
          const advisory = Number(s.advisory_blocks) || 0, dissent = Number(s.dissent_before_merge) || 0;
          rows.push({ model, kind, n: Number(s.n) || 0, approvals: approvalsKnown, blocks: blocksKnown, advisory, dissent,
            unknown: (Number(s.approved_unknown) || 0) + (Number(s.blocked_unknown) || 0),
            fa: evRate(s.approved_not_merged, approvalsKnown), fb: evRate(s.merged_after_enforced_block, blocksKnown) });
        });
      });
      if (!rows.length) return '<div class="ev-empty">— no reviewer verdicts in this window</div>';
      const unknownRows = rows.reduce((sum, r) => sum + r.unknown, 0);
      const advisoryRows = rows.reduce((sum, r) => sum + r.advisory, 0);
      const dissentRows = rows.reduce((sum, r) => sum + r.dissent, 0);
      const enforcedRows = rows.reduce((sum, r) => sum + r.enforced, 0);
      const w = EV_SVG_W, label = 140, valW = 34, rowH = 24, t = 4;
      const barW = w - label - valW;
      const h = t + rows.length * rowH;
      const pct = (v) => (v == null ? '—' : Math.round(v * 100) + '%');
      const body = rows.map((row, i) => {
        const y0 = t + i * rowH;
        const tip = escapeHtml(`${row.model} · ${row.kind} authors: false-approve ${pct(row.fa)} of ${row.approvals} approvals with a known outcome · false-block ${pct(row.fb)} of ${row.blocks} enforced blocks with a known outcome · dissent before merge ${row.dissent} of ${row.advisory} advisory blocks (not an error rate) · outcome unknown ${row.unknown} (not in either rate) · n=${row.n}`);
        const bar = (cls, v, yy) => `<rect class="ev-bar ${cls}" x="${label}" y="${yy}" width="${v == null ? 0 : (v * barW).toFixed(1)}" height="8"><title>${tip}</title></rect><text class="ev-val" x="${w - 2}" y="${yy + 7}" text-anchor="end">${pct(v)}</text>`;
        return `<g class="${row.model === 'unknown' ? 'ev-unknown' : ''}"><text class="ev-lbl" x="0" y="${y0 + 13}">${escapeHtml(`${row.model} · ${row.kind}`.slice(0, 24))}<title>${tip}</title></text>${bar('fa', row.fa, y0 + 2)}${bar('fb', row.fb, y0 + 12)}</g>`;
      }).join('');
      return `<svg class="ev-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="Reviewer accuracy by reviewer model and author kind: false-approve and false-block rates">${body}</svg><div class="ev-legend"><span><i class="fa"></i>false-approve (approved, closed without merge)</span><span><i class="fb"></i>false-block (enforced block, merged anyway)</span><span><i class="unknown"></i>model unknown</span><span>rates are over verdicts with a known PR outcome${unknownRows ? ` · ${unknownRows} verdict(s) on PRs with unknown outcome` : ''}</span>${advisoryRows ? `<span>${!enforcedRows ? 'Perspective verdicts were advisory on this hive (review.require_approval off); ' : `${advisoryRows} advisory block(s); `}${dissentRows} blocks preceded a merge (dissent before merge, ${pct(dissentRows / advisoryRows)} of advisory blocks; not an error rate)</span>` : ''}</div>`;
    }

    // Panel 8 (#12068): what the shadow review gate / intent verification
    // would have stopped, and how those PRs actually turned out. Each gate gets
    // a "would stop" and a "would pass" bar split into merged / reverted / open.
    function evShadowGateSvg(sg) {
      sg = sg || {};
      // The review gate is split by the lane that recorded it (#12592): the
      // merge-eligible lane (review.shadow) and the self-merge ReviewGate.
      const lanes = sg.review_gate_by_lane || {};
      const byLane = Number((lanes.self_merge || {}).n) > 0;
      const gates = byLane
        ? [[lanes.merge_eligible, 'Review · eligible', 'held'], [lanes.self_merge, 'Review · self-merge', 'held'], [sg.intent, 'Intent verification', 'blocked']]
        : [[sg.review_gate, 'Review gate', 'held'], [sg.intent, 'Intent verification', 'blocked']];
      const rows = [];
      gates.forEach(([gate, label, verb]) => {
        const g = gate || {};
        if (!(Number(g.n) > 0)) return;
        [['held', `would have ${verb}`], ['passed', 'would have passed']].forEach(([side, sideLabel]) => {
          const o = g[side] || {};
          const n = Number(o.n) || 0;
          const reverted = Number(o.reverted) || 0;
          rows.push({ label: `${label} · ${sideLabel}`, gate: label, side: sideLabel, n, merged: Math.max(0, (Number(o.merged) || 0) - reverted), reverted, open: Number(o.open) || 0, unknown: Number(o.unknown) || 0 });
        });
      });
      if (!rows.length) return '<div class="ev-empty">— no shadow-gate observations in this window (review.shadow / intent shadow mode is off, the self-merge review gate is off, or no PR was evaluated)</div>';
      const w = EV_SVG_W, label = 110, valW = 30, rowH = 26, t = 4;
      const barW = w - label - valW;
      const h = t + rows.length * rowH;
      const maxN = Math.max(1, ...rows.map((r) => r.n));
      const body = rows.map((row, i) => {
        const y0 = t + i * rowH;
        const tip = escapeHtml(`${row.label}: merged ${row.merged + row.reverted} (reverted ${row.reverted}) · open ${row.open} · outcome unknown ${row.unknown} · n=${row.n}`);
        let x0 = label;
        const seg = (cls, v) => {
          const sw = (v / maxN) * barW;
          const out = v > 0 ? `<rect class="ev-bar ${cls}" x="${x0.toFixed(1)}" y="${y0 + 7}" width="${sw.toFixed(1)}" height="10"><title>${tip}</title></rect>` : '';
          x0 += sw;
          return out;
        };
        return `<g><text class="ev-lbl" x="0" y="${y0 + 11}">${escapeHtml(row.gate)}<title>${tip}</title></text><text class="ev-sub" x="0" y="${y0 + 21}">${escapeHtml(row.side)}</text>${seg('sm', row.merged)}${seg('sr', row.reverted)}${seg('so', row.open)}${seg('su', row.unknown)}<text class="ev-val" x="${w - 2}" y="${y0 + 16}" text-anchor="end">${row.n}</text></g>`;
      }).join('');
      return `<svg class="ev-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="Shadow gates: PRs the review gate or intent verification would have stopped or passed, by actual outcome">${body}</svg><div class="ev-legend"><span><i class="sm"></i>merged</span><span><i class="sr"></i>merged, later reverted</span><span><i class="so"></i>open (backfilled)</span><span><i class="su"></i>outcome unknown</span></div>`;
    }

    // Panel 7 (#12369). Monthly observations are report-only and independent
    // of PR author kind. Missing tool measurements stay gaps, never zeroes.
    function evMaintainabilityRows(dto) {
      // Custom ranges carry from/to (to is exclusive) and hours is only the
      // rounded span, so the month window must come from from/to when present.
      const month = (ms) => new Date(ms).toISOString().slice(0, 7);
      const fromMs = Date.parse(dto.from || ''), toMs = Date.parse(dto.to || '');
      const custom = Number.isFinite(fromMs) && Number.isFinite(toMs) && toMs > fromMs;
      const end = custom ? month(toMs - 1) : month(Date.now());
      const start = custom ? month(fromMs) : Number(dto.hours) > 0 ? month(Date.now() - Number(dto.hours) * 3600000) : '';
      return (Array.isArray(dto.complexity_by_repo) ? dto.complexity_by_repo : [])
        .filter((r) => r && (!dto.repo || String(r.repo).toLowerCase() === String(dto.repo).toLowerCase()))
        .map((r) => ({ repo: String(r.repo), months: (Array.isArray(r.months) ? r.months : [])
          .filter((m) => m && /^\d{4}-\d{2}$/.test(m.month) && m.month >= start && m.month <= end)
          .slice().sort((a, b) => a.month.localeCompare(b.month)) }))
        .filter((r) => r.months.length).sort((a, b) => a.repo.localeCompare(b.repo));
    }

    function evMaintainabilitySvg(rows, changes) {
      if (!rows.length) return '<div class="ev-empty">— no monthly complexity snapshots in this window</div>';
      const months = [...new Set(rows.flatMap((r) => r.months.map((m) => m.month)))].sort();
      const ordinal = (m) => Number(m.slice(0, 4)) * 12 + Number(m.slice(5, 7)) - 1;
      const first = ordinal(months[0]), last = ordinal(months[months.length - 1]);
      const w = EV_SVG_W, left = 38, right = 10, laneH = 110;
      const x = (month) => left + (last > first ? (ordinal(month) - first) / (last - first) : 0.5) * (w - left - right);
      const tools = [...new Set(rows.flatMap((r) => r.months.flatMap((m) => Object.keys(m.static_warnings_by_tool || {}))))].sort();
      const metrics = [
        { label: 'Median cyclomatic (Go)', keys: ['median_cyclomatic'], value: (m, k) => m[k], tipSuffix: () => '' },
        { label: 'Functions over threshold', keys: ['functions_over_threshold'], value: (m, k) => m[k], tipSuffix: (m) => ' (threshold ' + m.threshold + ')' },
        { label: 'Warnings by tool', keys: tools, value: (m, k) => (m.static_warnings_by_tool || {})[k], dashed: true, labelByKey: true, tipSuffix: () => '' }
      ];
      const valid = (v) => typeof v === 'number' && Number.isFinite(v) && v >= 0;
      const body = metrics.map((metric, mi) => {
        const top = mi * laneH + 22, bottom = top + 64;
        const values = rows.flatMap((r) => r.months.flatMap((m) => metric.keys.map((k) => metric.value(m, k)))).filter(valid);
        const max = Math.max(1, ...values);
        const y = (v) => bottom - v / max * 64;
        let svg = `<text x="${left}" y="${top - 9}">${escapeHtml(metric.label)}</text><line class="ev-axis" x1="${left}" x2="${w - right}" y1="${bottom}" y2="${bottom}"/><text x="${left - 4}" y="${top + 4}" text-anchor="end">${max}</text><text x="${left - 4}" y="${bottom}" text-anchor="end">0</text>`;
        if (!values.length) svg += `<text x="${left}" y="${top + 30}">No tool measurements</text>`;
        rows.forEach((row, ri) => {
          const byMonth = new Map(row.months.map((m) => [m.month, m]));
          metric.keys.forEach((key, ki) => {
            let points = [], marks = '';
            const flush = () => {
              if (points.length > 1) marks += `<polyline class="ev-line" stroke-dasharray="${metric.dashed ? (ki + 1) * 3 + ' 3' : 'none'}" points="${points.join(' ')}"/>`;
              points = [];
            };
            // Include missing calendar months so lines do not imply measurements.
            for (let month = first; month <= last; month++) {
              const name = `${Math.floor(month / 12)}-${String(month % 12 + 1).padStart(2, '0')}`;
              const m = byMonth.get(name), v = m && metric.value(m, key);
              if (!valid(v)) { flush(); continue; }
              const xx = x(name).toFixed(1), yy = y(v).toFixed(1);
              points.push(`${xx},${yy}`);
              const tip = `${row.repo} · ${name} · ${metric.labelByKey ? key : metric.label}: ${v}${metric.tipSuffix(m)}`;
              marks += `<circle class="ev-pt" cx="${xx}" cy="${yy}" r="2.5"><title>${escapeHtml(tip)}</title></circle>`;
            }
            flush();
            svg += `<g class="ev-c${ri % EV_PALETTE_SIZE}">${marks}</g>`;
          });
          (Array.isArray(changes) ? changes : []).filter((c) => String(c.repo).toLowerCase() === row.repo.toLowerCase() && c.month >= months[0] && c.month <= months[months.length - 1]).forEach((c) => {
            const xx = x(c.month).toFixed(1);
            svg += `<g class="ev-c${ri % EV_PALETTE_SIZE}"><line class="ev-line" stroke-dasharray="2 3" x1="${xx}" x2="${xx}" y1="${top}" y2="${bottom}"><title>${escapeHtml(`${row.repo} · ${c.month}: ACMM L${c.level} first observed`)}</title></line></g>`;
          });
        });
        return svg;
      }).join('');
      const h = laneH * metrics.length + 12;
      const labels = months.filter((m, i) => i === 0 || i === months.length - 1 || (months.length <= 4)).map((m) => `<text x="${x(m).toFixed(1)}" y="${h - 4}" text-anchor="${m === months[0] ? 'start' : m === months[months.length - 1] ? 'end' : 'middle'}">${m}</text>`).join('');
      const legend = rows.map((r, i) => `<span class="ev-c${i % EV_PALETTE_SIZE}"><i class="dot"></i>${escapeHtml(r.repo)} (${r.months.length} months)</span>`).join('');
      return `<svg class="ev-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="Maintainability trend: median cyclomatic, over-threshold functions and warnings by tool; vertical ACMM first-observed markers">${body}${labels}</svg><div class="ev-legend">${legend}<span>Dashed: ${escapeHtml(tools.join(', ') || 'no warning tools')} · vertical dotted: ACMM level first observed</span></div>`;
    }

    // Panel 4 (#12368): boundary enforcement — the N behind "nothing bad
    // happened". Headline: blocked ÷ every agent GitHub write in the window
    // (hive-mediated from the audit log + direct through the proxy). Rows:
    // one per intent tier with direct writes (blocked ÷ direct writes for the
    // tier) plus one for hive-mediated writes. Counters are observations;
    // nothing here gates.
    const EV_WRITE_TIERS = ['ADVISORY', 'ISSUES_ONLY', 'ISSUES_AND_PRS', 'ISSUES_PRS_MERGE'];
    const EV_TOP_REASONS = 3;
    function evWriteActivitySvg(act) {
      if (!act || typeof act !== 'object') return '<div class="ev-empty">— no agent writes recorded in this window (no audit entries and no proxy counters)</div>';
      const direct = act.direct || {}, mediated = act.mediated || {};
      const byTier = direct.by_tier || {};
      const total = Number(act.total_writes) || 0, blocked = Number(act.blocked) || 0;
      const rate = evRate(blocked, total);
      evDetails['boundary-enforcement'] = `${formatNumber(Number(mediated.n) || 0)} hive-mediated · ${formatNumber(Number(direct.attempted) || 0)} direct through the proxy${direct.recorded === false ? ' · direct-write counters not recorded yet' : direct.partial_coverage ? ` · direct-write counters recorded since ${String(direct.since || '')}` : ''}.`;
      const headline = `<div class="ev-headline"><b>${blocked} of ${formatNumber(total)} agent writes blocked</b> · ${rate == null ? '—' : (rate * 100).toFixed(2) + '%'}</div>`;
      const tiers = EV_WRITE_TIERS.concat(Object.keys(byTier).filter((k) => !EV_WRITE_TIERS.includes(k)).sort());
      const rows = tiers.map((tier) => {
        const c = byTier[tier] || {};
        const idx = EV_WRITE_TIERS.indexOf(tier);
        return { label: idx >= 0 ? `${idx} · ${tier}` : tier, writes: Number(c.attempted) || 0, blocked: Number(c.blocked) || 0, kind: 'direct' };
      }).filter((r) => r.writes > 0 || r.blocked > 0);
      const reasons = Object.entries(direct.blocked_by_reason || {}).map(([k, v]) => [k, Number(v) || 0]).filter(([, v]) => v > 0).sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1));
      const topReasons = reasons.slice(0, EV_TOP_REASONS).map(([k, v]) => `${k} ${v}`).join(', ');
      if (Number(mediated.n) > 0) {
        const byAction = Object.entries(mediated.by_action || {}).map(([k, v]) => [k, Number(v) || 0]).filter(([, v]) => v > 0).sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1)).slice(0, EV_TOP_REASONS).map(([k, v]) => `${k} ${v}`).join(', ');
        rows.push({ label: 'hive-mediated', writes: Number(mediated.n) || 0, blocked: Number(mediated.refused) || 0, kind: 'mediated', detail: byAction });
      }
      if (!rows.length) return headline + '<div class="ev-empty">— no agent writes recorded in this window</div>';
      const w = EV_SVG_W, label = 118, valW = 72, barH = 7, gap = 2, rowH = 2 * (barH + gap) + 6, t = 4;
      const barW = w - label - valW;
      const h = t + rows.length * rowH;
      const maxN = Math.max(1, ...rows.map((r) => r.writes));
      const pct = (num, den) => { const v = evRate(num, den); return v == null ? '—' : (v * 100).toFixed(v > 0 && v < 0.1 ? 2 : 0) + '%'; };
      const body = rows.map((row, i) => {
        const y0 = t + i * rowH;
        const tip = escapeHtml(row.kind === 'direct'
          ? `tier ${row.label}: ${row.writes} direct writes through the proxy · ${row.blocked} blocked (${pct(row.blocked, row.writes)})${topReasons ? ` · top block reasons (all tiers): ${topReasons}` : ''}`
          : `hive-mediated writes from the audit log: ${row.writes} · ${row.blocked} refused${row.detail ? ` · top actions: ${row.detail}` : ''}`);
        const bar = (cls, v, k, txt) => {
          const yy = y0 + k * (barH + gap);
          return `<rect class="ev-bar ${cls}" x="${label}" y="${yy}" width="${Math.max(v > 0 ? 1 : 0, (v / maxN) * barW).toFixed(1)}" height="${barH}"><title>${tip}</title></rect><text x="${w - 2}" y="${yy + barH}" text-anchor="end">${txt}</text>`;
        };
        return `<g><text x="0" y="${y0 + 13}">${escapeHtml(row.label.slice(0, 22))}<title>${tip}</title></text>${bar(row.kind === 'direct' ? 'wa' : 'wm', row.writes, 0, formatNumber(row.writes))}${bar('wb', row.blocked, 1, `${row.blocked} · ${pct(row.blocked, row.writes)}`)}</g>`;
      }).join('');
      return `${headline}<svg class="ev-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="Boundary enforcement: agent writes per intent tier and hive-mediated, against writes blocked"><g>${body}</g></svg><div class="ev-legend"><span><i class="wa"></i>direct writes (proxy)</span><span><i class="wm"></i>hive-mediated writes (audit)</span><span><i class="wb"></i>blocked</span>${topReasons ? `<span>top block reasons: ${escapeHtml(topReasons)}</span>` : ''}</div>`;
    }

    function evSentinelSparkline(series) {
      const weeks = (series && Array.isArray(series.weeks) ? series.weeks : []).map((wk) => ({ week: String((wk && wk.week) || ''), hits: Number(wk && wk.hits) || 0 }));
      if (!weeks.length) return '<div class="ev-empty">— no sentinel hits recorded in this window</div>';
      const w = EV_SVG_W, h = 44, l = 4, r = 4, t = 6, b = 14;
      const pw = w - l - r, ph = h - t - b;
      const maxHits = Math.max(1, ...weeks.map((wk) => wk.hits));
      const x = (i) => l + (weeks.length > 1 ? (i * pw) / (weeks.length - 1) : pw / 2);
      const y = (v) => t + ph - (v / maxHits) * ph;
      const pts = weeks.map((wk, i) => `${x(i).toFixed(1)},${y(wk.hits).toFixed(1)}`).join(' ');
      const dots = weeks.map((wk, i) => `<circle class="ev-pt" cx="${x(i).toFixed(1)}" cy="${y(wk.hits).toFixed(1)}" r="2.5"><title>${escapeHtml(`week of ${wk.week}: ${wk.hits} sentinel hit(s)`)}</title></circle>`).join('');
      const first = weeks[0].week, last = weeks[weeks.length - 1].week;
      const total = weeks.reduce((sum, wk) => sum + wk.hits, 0);
      return `<svg class="ev-svg ev-spark" viewBox="0 0 ${w} ${h}" role="img" aria-label="Sentinel hits per ISO week"><g class="ev-c3">${weeks.length > 1 ? `<polyline class="ev-line" points="${pts}"/>` : ''}${dots}</g><text x="${l}" y="${h - 3}">${escapeHtml(first)}</text><text x="${w - r}" y="${h - 3}" text-anchor="end">${escapeHtml(last)}</text></svg><div class="ev-legend"><span class="ev-c3"><i class="dot"></i>sentinel hits per ISO week (UTC Monday) · ${total} in ${weeks.length} week(s) · peak ${maxHits}</span></div>`;
    }

    // Issue opens : PR merges — hive-authored issue opens ÷ hive-merged PRs,
    // read against the governor's cadence band rather than a flat 1.0. The
    // server (pkg/evidence/opens_merges.go) owns the counts, the band and the
    // verdict; the constants below mirror its thresholds for the explainer.
    const EV_OM_DRAIN_MAX = 1;            // surge / busy: opens per merge ≤ this (drain)
    const EV_OM_SCOUT_MIN = 1, EV_OM_SCOUT_MAX = 2; // quiet / idle: scouting, bounded
    const EV_OM_PARITY_TOLERANCE = 0.25;  // unknown mode and rolling 30 d: parity ± this
    const EV_OM_ROLLING_DAYS = 30;
    const EV_OM_SPARK_TRAIL_DAYS = 7;     // the sparkline plots a trailing 7-day ratio per day
    const EV_OM_BAR_MAX_RATIO = 3;        // the per-repo bar is full at this many opens per merge
    const EV_OM_SPARK_MAX_RATIO = 3;      // the sparkline's y axis tops out here; higher is clipped
    const EV_OM_STATUS = { in_band: 'in band', above: 'backlog growing', below: 'scouting below cadence', no_data: 'no data' };
    function evOmStatusLabel(status) { return EV_OM_STATUS[status] || 'no data'; }
    // evOmRatioText: "—" with nothing to count, "∞" with opens but no merges.
    function evOmRatioText(row) {
      row = row || {};
      if (row.ratio != null) return String(Math.round(Number(row.ratio) * 100) / 100);
      return Number(row.opens) > 0 ? '∞' : '—';
    }
    function evOmVerdict(row, band) {
      const opens = Number(row && row.opens) || 0, merges = Number(row && row.merges) || 0;
      if (!opens && !merges) return 'no_data';
      if (!merges) return 'above';
      const r = opens / merges;
      if (r > Number(band.max)) return 'above';
      if (Number(band.min) > 0 && r < Number(band.min)) return 'below';
      return 'in_band';
    }
    function evOmSparkline(days, band) {
      const rows = (Array.isArray(days) ? days : []).map((d) => ({ day: String((d && d.day) || ''), opens: Number(d && d.opens) || 0, merges: Number(d && d.merges) || 0 }));
      if (!rows.length) return '';
      const w = EV_SVG_W, h = 56, l = 22, r = 4, t = 6, b = 14;
      const pw = w - l - r, ph = h - t - b;
      const x = (i) => l + (rows.length > 1 ? (i * pw) / (rows.length - 1) : pw / 2);
      const y = (v) => t + ph - (Math.min(v, EV_OM_SPARK_MAX_RATIO) / EV_OM_SPARK_MAX_RATIO) * ph;
      const pts = [];
      rows.forEach((row, i) => {
        let opens = 0, merges = 0;
        for (let k = Math.max(0, i - EV_OM_SPARK_TRAIL_DAYS + 1); k <= i; k++) { opens += rows[k].opens; merges += rows[k].merges; }
        if (merges > 0) pts.push({ i, ratio: opens / merges, opens, merges, day: row.day });
      });
      const bandMin = Number(band && band.min) || 0, bandMax = Number(band && band.max) || 0;
      const shade = bandMax > bandMin ? `<rect class="ev-om-band" x="${l}" y="${y(bandMax).toFixed(1)}" width="${pw}" height="${(y(bandMin) - y(bandMax)).toFixed(1)}"><title>${escapeHtml(`rolling ${EV_OM_ROLLING_DAYS} d expects ${band.label || ''}`)}</title></rect>` : '';
      const parity = `<line class="ev-axis ev-om-parity" x1="${l}" x2="${w - r}" y1="${y(1).toFixed(1)}" y2="${y(1).toFixed(1)}"/><text x="${l - 4}" y="${(y(1) + 3).toFixed(1)}" text-anchor="end">1:1</text><text x="${l - 4}" y="${(y(EV_OM_SPARK_MAX_RATIO) + 3).toFixed(1)}" text-anchor="end">${EV_OM_SPARK_MAX_RATIO}+</text>`;
      const line = pts.length > 1 ? `<polyline class="ev-line" points="${pts.map((p) => `${x(p.i).toFixed(1)},${y(p.ratio).toFixed(1)}`).join(' ')}"/>` : '';
      const dots = pts.map((p) => `<circle class="ev-pt" cx="${x(p.i).toFixed(1)}" cy="${y(p.ratio).toFixed(1)}" r="2"><title>${escapeHtml(`${p.day}: trailing ${EV_OM_SPARK_TRAIL_DAYS} d ${p.opens} opens / ${p.merges} merges = ${Math.round(p.ratio * 100) / 100}`)}</title></circle>`).join('');
      const empty = pts.length ? '' : `<text x="${(l + pw / 2).toFixed(1)}" y="${(t + ph / 2 + 3).toFixed(1)}" text-anchor="middle">no hive merges in the last ${EV_OM_ROLLING_DAYS} days</text>`;
      return `<svg class="ev-svg ev-spark" viewBox="0 0 ${w} ${h}" role="img" aria-label="Issue opens per PR merge, trailing ${EV_OM_SPARK_TRAIL_DAYS}-day ratio per day over the last ${EV_OM_ROLLING_DAYS} days"><g class="ev-c0">${shade}${parity}${line}${dots}${empty}</g><text x="${l}" y="${h - 3}">${escapeHtml(rows[0].day)}</text><text x="${w - r}" y="${h - 3}" text-anchor="end">${escapeHtml(rows[rows.length - 1].day)}</text></svg>`;
    }
    function evOpensMergesHtml(om) {
      om = om && typeof om === 'object' ? om : {};
      const total = om.total || {}, band = om.expected || {}, roll = om.rolling_30d || {}, rollBand = om.rolling_30d_expected || {};
      const repos = Array.isArray(om.repos) ? om.repos : [];
      const status = om.status || 'no_data';
      const mark = status === 'in_band' ? '✓' : status === 'no_data' ? '' : `⚠ (${evOmStatusLabel(status)})`;
      const headline = `<div class="ev-headline ev-om-headline ev-om-${escapeHtml(status)}"><b>${escapeHtml(evOmRatioText(total))} opens per merge</b> · ${escapeHtml(band.mode || 'unknown')} mode expects ${escapeHtml(band.label || '—')} ${escapeHtml(mark)}<span class="ev-sub"> · rolling ${EV_OM_ROLLING_DAYS} d ${escapeHtml(evOmRatioText(roll))} (${escapeHtml(evOmStatusLabel(om.rolling_30d_status))}, expects ${escapeHtml(rollBand.label || '—')})</span></div>`;
      evDetails['opens-merges'] = `Current governor mode ${band.mode || 'unknown'} (${band.why || 'parity'}): ${band.label || '—'} opens per merge expected. Fleet ${Number(total.opens) || 0} opens / ${Number(total.merges) || 0} merges in this window; rolling ${EV_OM_ROLLING_DAYS} d ${Number(roll.opens) || 0} / ${Number(roll.merges) || 0}.`;
      const row = (r, cls) => {
        const ratio = r.ratio != null ? Number(r.ratio) : (Number(r.opens) > 0 ? Infinity : null);
        const verdict = evOmVerdict(r, band);
        const width = ratio == null ? 0 : Math.min(1, Math.min(ratio, EV_OM_BAR_MAX_RATIO) / EV_OM_BAR_MAX_RATIO) * 100;
        const tip = `${r.repo || 'fleet'}: ${Number(r.opens) || 0} hive issue opens / ${Number(r.merges) || 0} hive PR merges = ${evOmRatioText(r)} · ${evOmStatusLabel(verdict)} vs ${band.mode || 'unknown'} band ${band.label || ''}`;
        return `<tr class="${cls}" title="${escapeHtml(tip)}"><td>${escapeHtml(r.repo || 'Fleet')}</td><td class="num">${Number(r.opens) || 0}</td><td class="num">${Number(r.merges) || 0}</td><td class="num">${escapeHtml(evOmRatioText(r))}</td><td class="ev-om-barcell"><svg class="ev-om-bar" viewBox="0 0 100 6" preserveAspectRatio="none" aria-hidden="true"><rect class="ev-om-${verdict}" x="0" y="0" width="${width.toFixed(0)}" height="6" rx="3"/></svg></td></tr>`;
      };
      const table = repos.length || Number(total.opens) || Number(total.merges)
        ? `<div class="ev-table-wrap"><table class="ev-table ev-om-table"><thead><tr><th>Repository</th><th class="num">Opens</th><th class="num">Merges</th><th class="num">Ratio</th><th aria-label="Opens per merge, bar"></th></tr></thead><tbody>${repos.map((r) => row(r, '')).join('')}${row(total, 'ev-om-total')}</tbody></table></div>`
        : '<div class="ev-empty">— no hive issue opens or hive PR merges in this window</div>';
      const spark = evOmSparkline(om.rolling_30d_days, rollBand);
      const legend = spark ? `<div class="ev-legend"><span class="ev-c0"><i class="dot"></i>trailing ${EV_OM_SPARK_TRAIL_DAYS} d opens per merge</span><span><i class="ev-om-band-key"></i>parity band ${escapeHtml(rollBand.label || '')}</span></div>` : '';
      return `${headline}${table}${spark}${legend}<p class="ev-note">Descriptive, not causal; the ratio says how the hive’s filing pace compares with its landing pace, not whether either was right. Opens count hive-filed issues only (humans and third-party bots excluded); merges count PRs this hive merged or an agent wrote. A merge is not a closed issue and an open is not a commitment to work.</p>`;
    }

    // Panel 5 (#12365): claim vs verdict, read as CI recovery. Headline strip:
    // red-at-claim agent PRs by what happened next (recovered and merged ·
    // green but open · abandoned · still red/pending · outcome unknown).
    // Weekly bars below keep the claim-time facts: claimed done, CI not green
    // at claim, swarm not approve, both. Read-only; no gate reads it.
    // CI gate headline: grade A (integrity) and grade B (convergence) with
    // their denominators, breaches linkable, unknowns as counts.
    function evCIGateHtml(rec) {
      if (!rec || typeof rec !== 'object') return '';
      const gate = rec.gate || {}, conv = rec.convergence || {}, passes = conv.passes || {}, pol = rec.policy || {};
      const withChecks = Number(gate.with_checks) || 0, breaches = Number(gate.breaches) || 0;
      const integrity = evRate(withChecks - breaches, withChecks);
      const settled = Number(conv.settled) || 0, never = Number(passes.never) || 0;
      const abandon = evRate(never, settled);
      const firstPass = evRate(passes['1'], settled - never);
      const policy = pol.self_authored_automerge === false ? 'self-authored auto-merge off' : 'hands off to CI: agents do not wait for checks';
      const breachList = (Array.isArray(gate.breach_prs) ? gate.breach_prs : []).slice(0, 8).map((b) => {
        const label = `${b.repo}#${b.number}`;
        const why = b.state === 'missing_required' ? `required check(s) missing: ${(b.missing || []).join(', ')}` : `required check(s) red at merge${Array.isArray(b.red) && b.red.length ? `: ${b.red.join(', ')}` : ''}`;
        return `<a href="https://github.com/${encodeURIComponent(String(b.repo || '')).replace(/%2F/g, '/')}/pull/${Number(b.number) || 0}" target="_blank" rel="noopener" title="${escapeHtml(`${why} · author ${b.author_kind || '?'}${b.merged_by ? ` · merged by ${b.merged_by}` : ''} · ${b.source || ''}`)}">${escapeHtml(label)}</a>`;
      }).join(' ');
      const cost = gate.median_green_to_merge_hours != null ? ` · gate latency median ${Math.round(Number(gate.median_green_to_merge_hours) * 10) / 10} h first green → merge (${Number(gate.green_to_merge_n) || 0})` : '';
      const suspected = Number(gate.suspected) || 0;
      // The denominators and policy text live in the ⓘ popover; the card
      // keeps two short lines at the body size (#13204).
      evDetails['claim-vs-verdict'] = `Gate integrity: ${Number(gate.merged_n) || 0} merged: ${Number(gate.pending) || 0} with checks still running, ${suspected} suspected — a review-time snapshot was red on the merged head and GitHub has not confirmed yet, ${Number(gate.unknown) || 0} with no state on the merged head; all outside the rate · breach = a required check red on the merged head per GitHub, as the merge gate judges; ${Number(gate.non_required_red) || 0} green merged with a red non-required check, disclosed${cost}. Convergence: ${Number(conv.n) || 0} agent PRs: ${Number(conv.in_flight) || 0} in flight, ${Number(conv.unknown) || 0} unknown, outside the rates · ${Number(conv.from_github) || 0} read from GitHub, ${Number(conv.from_bundles) || 0} from bundles only · policy: ${policy}.`;
      const a = `<div class="ev-headline"><b>Gate integrity ${evPct(integrity)}</b> · ${withChecks - breaches} of ${withChecks} merged PRs green at merge · <b>${breaches} confirmed breach(es)</b>${breachList ? ` ${breachList}` : ''}</div>`;
      const b = `<div class="ev-headline"><b>Convergence: median ${conv.median_passes != null ? conv.median_passes : '—'} CI pass(es) to green</b> · ${evPct(abandon)} abandoned (${never} of ${settled} settled) · first-pass share ${evPct(firstPass)}</div>`;
      return a + b;
    }
    // Convergence chart: per ISO week a stacked bar of agent PRs by passes to
    // first green (1 / 2 / 3 / 4+ / never) with a line for the cumulative
    // share green by pass 1 over settled-and-green PRs.
    function evConvergenceSvg(conv) {
      const rows = (conv && Array.isArray(conv.by_week) ? conv.by_week : []).filter((w) => w && w.passes && EV_CONV_BUCKETS.some((k) => Number(w.passes[k]) > 0 || Number(w.in_flight) > 0 || Number(w.unknown) > 0));
      if (!rows.length) return '';
      const series = [['1', 'cp-1', 'green on pass 1'], ['2', 'cp-2', 'pass 2'], ['3', 'cp-3', 'pass 3'], ['4+', 'cp-4', 'pass 4+'], ['never', 'cp-never', 'never (closed unmerged)']];
      const w = EV_SVG_W, h = 150, l = 26, r = 30, t = 8, b = 22;
      const pw = w - l - r, ph = h - t - b;
      const groupW = pw / rows.length;
      const barW = Math.max(3, groupW * 0.6);
      const total = (row) => series.reduce((acc, [k]) => acc + (Number(row.passes[k]) || 0), 0);
      const maxN = Math.max(1, ...rows.map(total));
      const y = (v) => t + ph - (v / maxN) * ph;
      const yPct = (f) => t + ph - f * ph;
      let grid = '';
      [0, 0.5, 1].forEach((f) => {
        const v = Math.round(maxN * f);
        grid += `<line class="ev-axis" x1="${l}" x2="${w - r}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}"/><text x="${l - 4}" y="${(y(v) + 3).toFixed(1)}" text-anchor="end">${v}</text><text x="${w - r + 4}" y="${(yPct(f) + 3).toFixed(1)}">${Math.round(f * 100)}%</text>`;
      });
      const pts = [];
      const body = rows.map((row, i) => {
        const x0 = l + i * groupW + (groupW - barW) / 2;
        const n = total(row);
        const never = Number(row.passes.never) || 0;
        const green = n - never;
        const p1 = Number(row.passes['1']) || 0;
        const tip = escapeHtml(`${row.week}: ${n} settled agent PRs · pass 1: ${p1} · 2: ${Number(row.passes['2']) || 0} · 3: ${Number(row.passes['3']) || 0} · 4+: ${Number(row.passes['4+']) || 0} · never: ${never} · in flight ${Number(row.in_flight) || 0} · unknown ${Number(row.unknown) || 0}${green ? ` · green by pass 1: ${Math.round((p1 / green) * 100)}%` : ''}`);
        let acc = 0;
        const bars = series.map(([key, cls]) => {
          const v = Number(row.passes[key]) || 0;
          if (!(v > 0)) return '';
          const y1 = y(acc + v), y0 = y(acc);
          acc += v;
          return `<rect class="ev-bar ${cls}" x="${x0.toFixed(1)}" y="${y1.toFixed(1)}" width="${barW.toFixed(1)}" height="${(y0 - y1).toFixed(1)}"><title>${tip}</title></rect>`;
        }).join('');
        if (green > 0) pts.push([x0 + barW / 2, yPct(p1 / green), p1, green]);
        const label = rows.length > 8 ? String(row.week || '').replace(/^\d{4}-/, '') : String(row.week || '');
        return `<g>${bars}<text x="${(x0 + barW / 2).toFixed(1)}" y="${h - 6}" text-anchor="middle">${escapeHtml(label)}<title>${tip}</title></text></g>`;
      }).join('');
      const line = pts.length > 1 ? `<polyline class="ev-line cp-line" points="${pts.map(([x, yy]) => `${x.toFixed(1)},${yy.toFixed(1)}`).join(' ')}"/>` : '';
      const dots = pts.map(([x, yy, p1, green]) => `<circle class="ev-dot cp-line" cx="${x.toFixed(1)}" cy="${yy.toFixed(1)}" r="2.5"><title>${escapeHtml(`green by pass 1: ${p1} of ${green} (${Math.round((p1 / green) * 100)}%)`)}</title></circle>`).join('');
      const legend = series.map(([, cls, label]) => `<span><i class="${cls}"></i>${label}</span>`).join('') + '<span><i class="cp-line"></i>share green by pass 1 (right axis)</span>';
      return `<svg class="ev-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="CI convergence: agent PRs per week by CI passes to first green, with the share green on pass 1">${grid}${body}${line}${dots}</svg><div class="ev-legend">${legend}</div>`;
    }
    function evCIRecoveryHtml(rec) {
      if (!rec || typeof rec !== 'object' || !(Number(rec.n) > 0)) return '';
      const red = rec.red || {}, at = rec.at_claim || {}, pol = rec.policy || {};
      const segs = [['recovered_merged', 'cr-rec', 'recovered and merged'], ['green_open', 'cr-green', 'green, still open'], ['abandoned', 'cr-aband', 'abandoned (closed unmerged)'], ['pending', 'cr-pend', 'still red or pending'], ['outcome_unknown', 'cr-unk', 'outcome unknown']];
      const n = Number(red.n) || 0;
      const settled = (Number(red.recovered_merged) || 0) + (Number(red.abandoned) || 0);
      const rate = evRate(red.recovered_merged, settled);
      const policy = pol.self_authored_automerge === false ? 'self-authored auto-merge off' : 'hands off to CI: agents do not wait for checks';
      const med = red.median_pushes_to_green != null ? ` · median ${red.median_pushes_to_green} push(es), ${red.median_hours_to_green != null ? Math.round(Number(red.median_hours_to_green) * 10) / 10 : '—'} h to green` : '';
      evDetails['claim-vs-verdict'] = `${evDetails['claim-vs-verdict'] || ''} Red at claim (${policy}): of ${n} red at claim, ${Number(red.recovered_merged) || 0} of ${settled} settled recovered and merged (${evPct(rate)})${med}${Number(red.not_enriched) > 0 ? ` · ${red.not_enriched} not yet enriched from GitHub` : ''}.`.trim();
      const headline = `<div class="ev-disclosed">Disclosed, not graded: at claim ${Number(at.green) || 0} green · ${Number(at.red) || 0} red · ${Number(at.pending) || 0} pending · ${Number(at.none) || 0} no checks captured of ${Number(rec.n) || 0} agent claims.</div>`;
      if (!n) return headline;
      const w = EV_SVG_W, h = 14;
      let x = 0;
      const bars = segs.map(([key, cls, label]) => {
        const v = Number(red[key]) || 0;
        if (!(v > 0)) return '';
        const bw = (v / n) * w;
        const r = `<rect class="ev-bar ${cls}" x="${x.toFixed(1)}" y="1" width="${bw.toFixed(1)}" height="${h - 2}"><title>${escapeHtml(`${label}: ${v} of ${n} red-at-claim PRs`)}</title></rect>`;
        x += bw;
        return r;
      }).join('');
      const legend = segs.filter(([key]) => Number(red[key]) > 0).map(([key, cls, label]) => `<span><i class="${cls}"></i>${label} ${Number(red[key]) || 0}</span>`).join('');
      return `${headline}<svg class="ev-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="CI recovery: red-at-claim agent PRs by what happened next">${bars}</svg><div class="ev-legend">${legend}</div>`;
    }
    function evClaimVsVerdictSvg(weeks) {
      const rows = (Array.isArray(weeks) ? weeks : []).filter((w) => w && Number(w.n) > 0);
      if (!rows.length) return '<div class="ev-empty">— no agent-authored PRs with evidence in this window</div>';
      const series = [['claimed', 'cv-claim', 'claimed done'], ['ci_not_green', 'cv-ci', 'CI not green at claim'], ['verdict_not_approve', 'cv-swarm', 'swarm not approve'], ['both', 'cv-both', 'both']];
      const w = EV_SVG_W, h = 150, l = 26, r = 6, t = 8, b = 22;
      const pw = w - l - r, ph = h - t - b;
      const groupW = pw / rows.length;
      const barW = Math.max(2, (groupW * 0.8) / series.length);
      const maxN = Math.max(1, ...rows.map((row) => Number(row.claimed) || 0));
      const y = (v) => t + ph - (v / maxN) * ph;
      let grid = '';
      [0, 0.5, 1].forEach((f) => {
        const v = Math.round(maxN * f);
        grid += `<line class="ev-axis" x1="${l}" x2="${w - r}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}"/><text x="${l - 4}" y="${(y(v) + 3).toFixed(1)}" text-anchor="end">${v}</text>`;
      });
      const body = rows.map((row, i) => {
        const x0 = l + i * groupW + groupW * 0.1;
        const captured = Number(row.ci_captured) || 0;
        const tip = escapeHtml(`${row.week}: claimed ${Number(row.claimed) || 0} · CI not green ${Number(row.ci_not_green) || 0} of ${captured} with settled CI · CI still running when captured ${Number(row.ci_pending) || 0} · swarm not approve ${Number(row.verdict_not_approve) || 0} · both ${Number(row.both) || 0} · n=${Number(row.n) || 0}`);
        const bars = series.map(([key, cls], si) => {
          const v = Number(row[key]) || 0;
          if (!(v > 0)) return '';
          return `<rect class="ev-bar ${cls}" x="${(x0 + si * barW).toFixed(1)}" y="${y(v).toFixed(1)}" width="${barW.toFixed(1)}" height="${(t + ph - y(v)).toFixed(1)}"><title>${tip}</title></rect>`;
        }).join('');
        const label = rows.length > 8 ? String(row.week || '').replace(/^\d{4}-/, '') : String(row.week || '');
        return `<g>${bars}<text x="${(x0 + (groupW * 0.8) / 2).toFixed(1)}" y="${h - 6}" text-anchor="middle">${escapeHtml(label)}<title>${tip}</title></text></g>`;
      }).join('');
      const legend = series.map(([, cls, label]) => `<span><i class="${cls}"></i>${label}</span>`).join('');
      return `<svg class="ev-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="Claim vs verdict: agent-authored PRs per week that claimed done against CI not green and swarm not approve">${grid}${body}</svg><div class="ev-legend">${legend}</div>`;
    }

    // Panel 6 (#12366): work balance, read as "is the queue keeping up?".
    // Two charts per ISO week: issues (opened stacked by kind, closed line,
    // week-end open-issue depth dashed) and PRs (opened stacked by kind,
    // merged line, closed-unmerged tick, week-end open-PR depth dashed).
    // Throttle shading is drawn only for weeks the API flags. Read-only.
    function evFlowSvg(rows, cfg) {
      const kinds = [['human', 'wb-human', 'opened by humans'], ['agent', 'wb-agent', 'opened by agents'], ['bot', 'wb-bot', 'opened by bots']];
      const w = EV_SVG_W, h = 150, l = 26, r = 26, t = 8, b = 22;
      const pw = w - l - r, ph = h - t - b;
      const groupW = pw / rows.length;
      const barW = Math.max(2, groupW * 0.6);
      const opened = (row) => kinds.reduce((sum, [k]) => sum + (Number((row[cfg.opened] || {})[k]) || 0), 0);
      const maxN = Math.max(1, ...rows.map((row) => Math.max(opened(row), Number(row[cfg.done]) || 0, Number(row[cfg.closed]) || 0)));
      const depths = rows.map((row) => row[cfg.queue]).filter((v) => v != null).map(Number);
      const maxQ = Math.max(1, ...depths);
      const y = (v) => t + ph - (v / maxN) * ph;
      const yq = (v) => t + ph - (v / maxQ) * ph;
      const cx = (i) => l + i * groupW + groupW / 2;
      let grid = '';
      [0, 0.5, 1].forEach((f) => {
        const v = Math.round(maxN * f);
        grid += `<line class="ev-axis" x1="${l}" x2="${w - r}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}"/><text x="${l - 4}" y="${(y(v) + 3).toFixed(1)}" text-anchor="end">${v}</text>`;
        if (depths.length) grid += `<text class="wb-queue" x="${w - r + 4}" y="${(yq(Math.round(maxQ * f)) + 3).toFixed(1)}">${Math.round(maxQ * f)}</text>`;
      });
      const tipFor = (row) => {
        const o = row[cfg.opened] || {};
        const q = row[cfg.queue] != null ? ` · open at week end ${row[cfg.queue]}` : ' · no queue sample';
        return escapeHtml(`${row.week}: ${cfg.noun} opened ${opened(row)} (human ${Number(o.human) || 0} · agent ${Number(o.agent) || 0} · bot ${Number(o.bot) || 0}) · ${cfg.doneLabel} ${Number(row[cfg.done]) || 0}${cfg.closed ? ` · closed without merge ${Number(row[cfg.closed]) || 0}` : ''}${q}${cfg.source && row[cfg.source] ? ` · source ${row[cfg.source]}` : ''}`);
      };
      const body = rows.map((row, i) => {
        const o = row[cfg.opened] || {};
        const tip = tipFor(row);
        const shade = row.throttled ? `<rect class="ev-bar wb-throttle" x="${(l + i * groupW).toFixed(1)}" y="${t}" width="${groupW.toFixed(1)}" height="${ph}"><title>${tip}</title></rect>` : '';
        let acc = 0;
        const stack = kinds.map(([k, cls]) => {
          const v = Number(o[k]) || 0;
          if (!(v > 0)) return '';
          const top = y(acc + v), bottom = y(acc);
          acc += v;
          return `<rect class="ev-bar ${cls}" x="${(cx(i) - barW / 2).toFixed(1)}" y="${top.toFixed(1)}" width="${barW.toFixed(1)}" height="${(bottom - top).toFixed(1)}"><title>${tip}</title></rect>`;
        }).join('');
        const closedTick = cfg.closed && Number(row[cfg.closed]) > 0 ? `<line class="ev-tick wb-unmerged" x1="${(cx(i) - barW / 2).toFixed(1)}" x2="${(cx(i) + barW / 2).toFixed(1)}" y1="${y(Number(row[cfg.closed])).toFixed(1)}" y2="${y(Number(row[cfg.closed])).toFixed(1)}"><title>${tip}</title></line>` : '';
        const label = rows.length > 8 ? String(row.week || '').replace(/^\d{4}-/, '') : String(row.week || '');
        return `<g>${shade}${stack}${closedTick}<text x="${cx(i).toFixed(1)}" y="${h - 6}" text-anchor="middle">${escapeHtml(label)}<title>${tip}</title></text></g>`;
      }).join('');
      const donePts = rows.map((row, i) => `${cx(i).toFixed(1)},${y(Number(row[cfg.done]) || 0).toFixed(1)}`);
      const doneLine = `<g class="wb-closed">${rows.length > 1 ? `<polyline class="ev-line" points="${donePts.join(' ')}"/>` : ''}${donePts.map((pt) => `<circle class="ev-pt" cx="${pt.split(',')[0]}" cy="${pt.split(',')[1]}" r="2.5"/>`).join('')}</g>`;
      const qRows = rows.map((row, i) => [i, row[cfg.queue]]).filter(([, v]) => v != null);
      const qPts = qRows.map(([i, v]) => `${cx(i).toFixed(1)},${yq(Number(v)).toFixed(1)}`);
      const qLine = qPts.length ? `<g class="wb-queue">${qPts.length > 1 ? `<polyline class="ev-line" points="${qPts.join(' ')}"/>` : ''}${qRows.map(([i, v]) => `<circle class="ev-pt" cx="${cx(i).toFixed(1)}" cy="${yq(Number(v)).toFixed(1)}" r="2"><title>${escapeHtml(`${rows[i].week}: ${v} open ${cfg.noun} at week end`)}</title></circle>`).join('')}</g>` : '';
      const legend = kinds.map(([, cls, label]) => `<span><i class="${cls}"></i>${label}</span>`).join('') + `<span class="wb-closed"><i class="dot"></i>${cfg.doneLabel}</span>${cfg.closed ? '<span>— closed without merge</span>' : ''}<span class="wb-queue"><i class="dot"></i>open at week end (right axis)</span>`;
      return `<svg class="ev-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="${escapeHtml(cfg.aria)}">${grid}${body}${doneLine}${qLine}</svg><div class="ev-legend">${legend}</div>`;
    }
    function evWorkBalanceSvg(weeks, sources) {
      const rows = (Array.isArray(weeks) ? weeks : []).filter((w) => w && Number(w.n) > 0);
      if (!rows.length) return '<div class="ev-empty">— no issue or PR opens, closes or merges in this window</div>';
      const last = rows[rows.length - 1];
      const src = sources && typeof sources === 'object' ? sources : {};
      const sum = (key, sub) => rows.reduce((acc, r) => acc + (sub ? Object.values(r[key] || {}).reduce((a, v) => a + (Number(v) || 0), 0) : (Number(r[key]) || 0)), 0);
      const issuesOpened = sum('opened', true), issuesClosed = sum('closed'), prsOpened = sum('prs_opened', true), prsMerged = sum('prs_merged'), prsClosed = sum('prs_closed');
      evDetails['work-balance'] = `Issues ${formatNumber(issuesOpened)} opened / ${formatNumber(issuesClosed)} closed (${evPct(evRate(issuesClosed, issuesOpened))}) · PRs ${formatNumber(prsOpened)} opened / ${formatNumber(prsMerged)} merged / ${formatNumber(prsClosed)} closed unmerged (${evPct(evRate(prsMerged, prsOpened))})${src.issue_series_weeks ? ` · ${src.issue_series_weeks} historical repository-week(s) from GitHub backfill, ${src.audit_only_weeks || 0} week(s) include audit fallback` : src.issues_github_since ? ` · issues from GitHub since ${src.issues_github_since}${src.audit_only_weeks ? `, ${src.audit_only_weeks} earlier week(s) audit-log only` : ''}` : ' · issues from the hive audit log only'}.`;
      const headline = `<div class="ev-headline"><b>Queue at end of ${escapeHtml(String(last.week || ''))}: ${last.queue_issues != null ? last.queue_issues : '—'} open issues · ${last.queue_prs != null ? last.queue_prs : '—'} open PRs</b></div>`;
      const issues = evFlowSvg(rows, { noun: 'issues', opened: 'opened', done: 'closed', doneLabel: 'closed', queue: 'queue_issues', source: 'issue_source', aria: 'Work balance: issues opened per week by human, agent and bot, against issues closed and open issues at week end' });
      const prs = evFlowSvg(rows, { noun: 'PRs', opened: 'prs_opened', done: 'prs_merged', doneLabel: 'merged', closed: 'prs_closed', queue: 'queue_prs', aria: 'Work balance: PRs opened per week by human, agent and bot, against PRs merged and open PRs at week end' });
      return `${headline}<div class="ev-flow"><div><h5>Issues</h5>${issues}</div><div><h5>Pull requests</h5>${prs}</div></div>`;
    }

    // Issue reopen rate (rolling 30 d by closure time): one row per repo ×
    // clarity-gate marker. Rendered through evPanel so it carries the same
    // collapse toggle, ⓘ explainer, n= chip and grade pill as its siblings;
    // n = distinct issues closed (each issue lands in exactly one row).
    function evIssueReopenRatesHtml(dto) {
      const rows = Array.isArray(dto.issue_reopen_rates_30d) ? dto.issue_reopen_rates_30d : [];
      const closedN = rows.reduce((sum, r) => sum + (Number(r && r.closed) || 0), 0);
      const table = rows.length
        ? `<div class="ev-table-wrap"><table class="ev-table"><thead><tr><th>Repository</th><th>Clarity gate</th><th>Reopened / closed</th><th>Rate</th></tr></thead><tbody>${rows.map((r) => `<tr><td>${escapeHtml(String(r.repo || ''))}</td><td>${r.clarity_gate ? 'Passed through' : 'No marker'}</td><td>${Number(r.reopened) || 0} / ${Number(r.closed) || 0}</td><td>${evPct(r.rate)}</td></tr>`).join('')}</tbody></table></div>`
        : '<div class="ev-empty">— no captured issue closures in the last 30 days</div>';
      const body = `${table}<p class="ev-note">Distinct issues closed by hive PRs in the last 30 days, split by the clarity-gate marker at closure. Uses closure time, not the selected PR window or author filter. Missing historical clarity status is excluded, not counted as ungated. Recent closures have less time to reopen; this comparison does not prove causality.</p>`;
      return evPanel('issue-reopen', 'Issue reopen rate · rolling 30 days', closedN, body,
        'A fixed 30-day window by closure time, so the window pills and repo/author filters above do not apply. Only issues closed by hive PRs are counted; an issue closed by hand is not tracked, and a reopen is only seen through the issues webhook or the issue listing. Rows are descriptive and the clarity-gate split is not a controlled comparison.');
    }

    function renderEvidenceSummary(dto) {
      const card = document.getElementById('evidence-card');
      if (!card) return;
      dto = dto || {};
      evLast = dto;
      Object.keys(evDetails).forEach((k) => { delete evDetails[k]; });
      const calib = evCalibrationSeries(dto);
      const models = Array.isArray(dto.reviewer_accuracy_by_model) ? dto.reviewer_accuracy_by_model : [];
      const calibN = calib.reduce((sum, p) => sum + (Number(p && p.n) || 0), 0);
      const modelN = models.reduce((sum, m) => sum + (Number(m && m.n) || 0), 0);
      const unknown = Number((dto.missing || {}).verdicts_model_unknown) || 0;
      const shadow = dto.shadow_gate || {};
      const complexity = evMaintainabilityRows(dto);
      const complexityN = complexity.filter((r) => r.months.length >= 2).length;
      const activity = dto.write_activity && typeof dto.write_activity === 'object' ? dto.write_activity : null;
      const writesN = activity ? Number(activity.total_writes) || 0 : 0;
      const directCoverage = activity && activity.direct ? (activity.direct.recorded === false ? ' Direct-write counters have not been recorded on this hive yet, so the total holds hive-mediated writes only.' : activity.direct.partial_coverage ? ` Direct-write counters have only been recorded since ${activity.direct.since}; earlier weeks hold hive-mediated writes only.` : '') : '';
      const sentinel = dto.sentinel_hits_per_week || {};
      const outcomes = dto.outcomes_by_author_kind || {};
      const outcomeN = Object.keys(outcomes).reduce((sum, k) => sum + (Number(outcomes[k] && outcomes[k].n) || 0), 0);
      const claims = Array.isArray(dto.claim_vs_verdict) ? dto.claim_vs_verdict : [];
      const claimN = claims.reduce((sum, w) => sum + (Number(w && w.n) || 0), 0);
      const emptyCI = Number((dto.missing || {}).records_empty_ci) || 0;
      const pendingCI = Number((dto.missing || {}).records_ci_pending) || 0;
      const outcomeUnknown = Number((dto.missing || {}).prs_outcome_unknown) || 0;
      const outcomeLedger = Number((dto.missing || {}).prs_outcome_from_ledger) || 0;
      const outcomeElsewhere = Object.keys(outcomes).reduce((sum, k) => sum + (Number(outcomes[k] && outcomes[k].merged_elsewhere) || 0), 0);
      const outcomeClosed = Object.keys(outcomes).reduce((sum, k) => sum + (Number(outcomes[k] && outcomes[k].closed) || 0), 0);
      const outcomeOpen = Object.keys(outcomes).reduce((sum, k) => sum + (Number(outcomes[k] && outcomes[k].open) || 0), 0);
      const balance = Array.isArray(dto.work_balance) ? dto.work_balance : [];
      const balanceN = balance.reduce((sum, w) => sum + (Number(w && w.n) || 0), 0);
      const balanceSrc = dto.work_balance_sources && typeof dto.work_balance_sources === 'object' ? dto.work_balance_sources : {};
      const throttleNote = (dto.missing || {}).throttle_windows_unavailable === false ? 'Shaded weeks are governor issue-creation throttle windows.' : 'Throttle windows not reported by this hive, so no weeks are shaded.';
      const issueSourceNote = balanceSrc.issue_series_weeks ? `${balanceSrc.issue_series_weeks} historical repository-week(s) use backfilled GitHub issue history; ${balanceSrc.audit_only_weeks || 0} week(s) include audit fallback, where human and third-party bot opens may be missing. Collector-covered weeks keep the collector data.` : balanceSrc.issues_github_since ? `Issue opens and closes are GitHub-truth (all authors) from ${balanceSrc.issues_github_since}${balanceSrc.audit_only_weeks ? `; the ${balanceSrc.audit_only_weeks} earlier week(s) are audit-log only, where issues humans or third-party bots open directly on GitHub are not observed` : ''}.` : 'GitHub issue history has not been fetched yet, so issue opens and closes are audit-log only: issues humans or third-party bots open directly on GitHub are not observed and the human column is a floor.';
      const rec = dto.ci_recovery && typeof dto.ci_recovery === 'object' ? dto.ci_recovery : {};
      const recGate = rec.gate || {}, recConv = rec.convergence || {};
      const opensMerges = dto.issue_opens_pr_merges && typeof dto.issue_opens_pr_merges === 'object' ? dto.issue_opens_pr_merges : {};
      const scope = evWindowLabel() + (dto.repo ? ` · ${dto.repo}` : '');
      const windowHint = Number(dto.n) > 0 ? '' : evWindowHint(dto);
      card.innerHTML = `<div class="prt-head"><div class="lc-window">🔬 Evidence · ${escapeHtml(scope)}<span class="ev-n" title="PRs (latest evidence bundle per PR) in this window">n=${Number(dto.n) || 0} PRs</span></div>${evWindowControls()}</div>`
        + (windowHint ? `<div class="ev-note ev-window-hint">No PRs opened in this window${escapeHtml(windowHint)}.</div>` : '')
        + evPanel('outcome-by-author', 'Outcome by author kind', outcomeN, evOutcomesSvg(outcomes) + evControlArmHtml(dto.control_arm),
          `PRs are placed in the window by their open time, never by when a record was written. Undone counts two change-failure signals, each within 14 d of merge: reverted means a reverted_by link from a later merged PR whose title or body named this PR or its commit (Reverts #N, Revert "…", This reverts commit <sha>) — a revert that names nothing is missed; re-opened means an issue this PR closed was re-opened by a person after the merge, as seen by the issues webhook or the issue listing — an issue closed by hand rather than by a closing keyword is not tracked. A live bundle records a merge only when this hive merged, and never a close-without-merge; PRs reviewed here and then merged by someone else, closed by hand or left open are resolved from the governor's review-outcome ledger instead${outcomeLedger ? ` (${outcomeLedger} PR(s) in this window)` : ''}: ${outcomeElsewhere} merged elsewhere · ${outcomeClosed} closed without merge · ${outcomeOpen} open. A PR the ledger never saw${outcomeUnknown ? ` (${outcomeUnknown} PR(s) in this window)` : ''} is shown as unknown and excluded from every rate, not counted as open or as not merged. Merged is not the same as correct.`)
        + evIssueReopenRatesHtml(dto)
        + evPanel('calibration', 'Reviewer calibration', calibN, evCalibrationControls() + evCalibrationSvg(calib),
          `The self-reported series includes ${Number(dto.reviewer_calibration_legacy) || 0} legacy verdict(s) using the old verdict-based confidence. The derived series excludes verdicts without a recorded derived score; n counts only the selected series. Slopes pool perspectives and compare the endpoint rise per bin across bins with at least five known outcomes, using each series’ available records. Later CI results never rewrite a recorded derived score. This calibration panel plots merge and block rates. The merged rate counts only verdicts on PRs with a recorded outcome; verdicts on PRs with no recorded outcome are shown as unknown, not as unmerged. Buckets with fewer than ${EV_CALIB_MIN_BUCKET_N} known-outcome verdicts are dashed and not joined. Merged is not the same as correct.`)
        + evPanel('accuracy-by-model', 'Reviewer accuracy × model family', modelN, evAccuracySvg(models),
          `Outcome is recorded merge vs recorded close, not ground truth: an enforced block that was later fixed and merged counts as a false block (an advisory block is dissent before merge, never a false block), and an approval counts as a false approve only when the PR's record says it closed without merging. Verdicts on PRs with no recorded outcome (reviewed here but never resolved by the review-outcome ledger or a backfill) are excluded from both rates and reported as unknown.${unknown ? ` ${unknown} verdict(s) had no recorded reviewer model and are grouped as unknown.` : ''}`)
        + evPanel('maintainability', 'Maintainability trend', complexityN, evMaintainabilitySvg(complexity, dto.acmm_changes),
          'Report-only; Go-only cyclomatic complexity. No causal claim: complexity may rise. n = repos with at least 2 monthly snapshots in the selected window (boundary months included). Monthly repo measurements ignore author kind. Tool measurements can be unavailable; gaps are not zero. ACMM markers show the first month each level is seen in stored bundles, not the exact configuration change; returns to an earlier level are not shown.')
        + evPanel('boundary-enforcement', 'Boundary enforcement', writesN, evWriteActivitySvg(activity) + evSentinelSparkline(sentinel),
          `The denominator is every agent GitHub write the hive can see in the window: hive-mediated writes from the audit log plus direct writes through the proxy (weekly counters kept under /data). Blocked is what the proxy refused; would-block counts from intent verification are shown separately in the explainer and never counted as blocked.${directCoverage} Writes that bypass both the hive and the proxy are invisible here. Sentinel hits come from evidence bundles in the window. Blocked is not the same as harmful.`)
        + evPanel('claim-vs-verdict', 'CI gate — integrity + convergence', claimN, evCIGateHtml(rec) + evConvergenceSvg(rec.convergence) + evCIRecoveryHtml(rec) + evClaimVsVerdictSvg(claims),
          `Gate integrity covers every merged PR in the window (any author) whose check state on the merged head is on record; a review-time snapshot that was red is only suspected until GitHub confirms it${recGate.suspected ? ` (${recGate.suspected})` : ''}, and merged PRs with no snapshot on the merged head and no GitHub trail${recGate.unknown ? ` (${recGate.unknown})` : ''} are unknown — neither is green and neither is a breach. Convergence covers agent-authored PRs only: a pass is one CI evaluation of a new head SHA, read from GitHub’s commit trail where it has arrived and from this hive’s own snapshots otherwise (which can miss heads nobody reviewed); in-flight${recConv.in_flight ? ` (${recConv.in_flight})` : ''} and unknown${recConv.unknown ? ` (${recConv.unknown})` : ''} PRs are in no rate. This hive hands off to CI by policy (agents do not wait for CI; a red check holds the PR), so CI red at claim is by design and is disclosed, not graded. A PR with no captured checks${emptyCI ? ` (${emptyCI} in this window)` : ''} or still-running checks at claim${pendingCI ? ` (${pendingCI})` : ''} is left out of the CI column. A non-approve verdict is not proof the agent was wrong.`)
        + evPanel('work-balance', 'Work balance', balanceN, evWorkBalanceSvg(balance, balanceSrc),
          `${issueSourceNote} PR opens, merges and closes come from evidence bundles for PRs opened in the window (live and backfilled), so PRs never seen by this hive are missing. Week-end queue depth is the governor’s actionable open issues / PRs (held items excluded) from its last snapshot of the week, kept ${balanceSrc.queue_retention_days || 30} days${balanceSrc.queue_samples === 0 ? ' — no snapshot in this window' : ''}. Nothing is weighted by size, and an opened issue counts whether or not anyone acted on it. ${throttleNote}`)
        + evPanel('opens-merges', 'Issue opens : PR merges', opensMerges.n, evOpensMergesHtml(opensMerges),
          `Opens are hive-filed issues from the audit log only: an issue a human or a third-party bot opened directly on GitHub is not counted, so this is the hive’s own filing pace, not the repo’s. Merges follow the Outcome by author kind card: a PR this hive merged (any author) or an agent-authored PR merged by anyone; a human or bot PR merged outside the hive is not counted, and PRs never seen by this hive are missing. The band is the governor’s mode right now, not the mode when each open or merge happened; the rolling ${EV_OM_ROLLING_DAYS} d is judged against parity for that reason. Issue opens and PR merges are not the same unit of work — nothing here is weighted by size or links an open to the merge that closed it.`)
        + evPanel('shadow-gates', 'Shadow gates', shadow.n, evShadowGateSvg(shadow),
          'Shadow mode only records; no merge was held or blocked. Closed-without-merge is known only from backfilled records and the review-outcome ledger; a PR with no recorded outcome is shown as unknown rather than as open, and a revert only shows once the reverting PR is linked. Merged is not the same as correct.');
    }

    async function fetchEvidenceSummary() {
      const query = evWindowQuery();
      const repo = prtRepo;
      try {
        let url = `/api/evidence/summary?${query}`;
        if (repo) url += `&repo=${encodeURIComponent(repo)}`;
        const r = await fetch(url);
        if (query !== evWindowQuery() || repo !== prtRepo) return; // a newer selection is in flight
        if (!r.ok) { evLast = null; renderEvidenceSummary(null); return; } // 403/5xx: show n=0, never throw
        const dto = await r.json();
        if (query !== evWindowQuery() || repo !== prtRepo) return;
        evLast = dto;
        renderEvidenceSummary(dto);
      } catch (e) { if (!evLast) renderEvidenceSummary(null); }
    }

