
    function stageTrackerMotion(item) {
      const mods = Array.isArray(item && item.modifiers) ? item.modifiers : [];
      if (mods.indexOf('hold') >= 0) return STAGE_TRACKER_MOTION_STATIC;
      if (mods.some(m => STAGE_TRACKER_MOTION_HUMAN_MODS.indexOf(m) >= 0)) return STAGE_TRACKER_MOTION_HUMAN;
      const n = Number(item && item.stage) || 0;
      if (n >= STAGE_TRACKER_TERMINAL) return STAGE_TRACKER_MOTION_STATIC;
      if (STAGE_TRACKER_MOTION_AUTHOR_STAGES.indexOf(n) >= 0) {
        const ak = stageTrackerActorKind(item);
        return ak === 'hive' || ak === 'clanker' ? STAGE_TRACKER_MOTION_ACTIVE : STAGE_TRACKER_MOTION_STATIC;
      }
      return STAGE_TRACKER_MOTION_BY_STAGE[n] || STAGE_TRACKER_MOTION_STATIC;
    }
    const STAGE_TRACKER_TRAIL_HOPS = 4;
    // Sentinel alert on a PR (payload sentinel_alert, from the sentinel-alert
    // label or the head's evidence bundle): the human-decision lane names
    // it in the chevron word itself; every other lane adds a chip. The
    // words come from the hive's sentinel (possible security override,
    // privilege escalation or codebase damage), so a maintainer reads the
    // security reason before the row's "needs human".
    const STAGE_TRACKER_SENTINEL_WORD = 'Sentinel alert · needs human';
    const STAGE_TRACKER_SENTINEL_CHIP = 'sentinel';
    const STAGE_TRACKER_SENTINEL_TIP = 'Sentinel flagged security-relevant changes';
    const STAGE_TRACKER_SENTINEL_TIP_FALLBACK = 'possible security override, privilege escalation or codebase damage — a maintainer must review before merge';
    // Merge-gate pills the server held back (payload deferred_modifiers:
    // sentinel, needs-human, needs-decision, a merge-time unauthorized
    // step) because the PR has not reached the gate yet: nothing a person
    // does now moves the row, so no pill is drawn and the row stays out of
    // "Needs attention"; the row tooltip alone names what waits at the gate.
    const STAGE_TRACKER_DEFERRED_NOTE = 'Pending at merge queue: ';
    function stageTrackerDeferredMods(item) {
      return Array.isArray(item && item.deferred_modifiers) ? item.deferred_modifiers : [];
    }
    const STAGE_TRACKER_SHORT_SHA = 7;
    var _stageTracker = null;
    // The item rows the last stage-tracker render drew (tracker-filter-render.js).
    var _stageTrackerRows = null;
    var _stageTrackerSig = '';
    var _stageTrackerEtag = '';
    // Last poll outcome: '' while polls succeed, else a short reason ('HTTP
    // 504', 'unreachable'). Without it a payload that was "fresh" when it
    // arrived keeps saying so while every later poll fails — the operator
    // reads "updated 8m ago · fresh" and cannot tell a quiet hive from a
    // dashboard that has lost the server (#14416).
    var _stageTrackerPollError = '';
    var _stageTrackerPollErrorSince = 0;
    // Filter / sort / paging state. Everything but the page survives a reload
    // (localStorage, like the Review window) so a maintainer's view sticks.
    const STAGE_TRACKER_FILTER_KEY = 'hive-activity-monitor-filter';
    const STAGE_TRACKER_PAGE_SIZES = [25, 50, 100];
    const STAGE_TRACKER_DEFAULT_PAGE_SIZE = 50;
    const STAGE_TRACKER_LABEL_CHIP_LIMIT = 24;
    const STAGE_TRACKER_CLOCK_MS = 10000;
    const STAGE_TRACKER_AGE_MS = { '1h': 3600000, '1d': 86400000, '1w': 604800000 };
    const STAGE_TRACKER_ATTENTION_STAGES = [45, 71, 72, 85, 90];
    // "Needs human" (one definition with Go stageTrackerNeedsHuman; the
    // payload's needs_human flag/count come from it too): a person's
    // decision or action is the ONLY thing that advances the row — 015
    // triage, 045/085 needs-decision, 090 human merge; 071/072 only when
    // the author is a human; or a needs-human / needs-decision /
    // unauthorized pill. hold is "On hold" (its own chip), never this.
    const STAGE_TRACKER_NEEDS_HUMAN_STAGES = [15, 45, 85, 90];
    const STAGE_TRACKER_NEEDS_HUMAN_AUTHOR_STAGES = [71, 72];
    const STAGE_TRACKER_NEEDS_HUMAN_MODS = ['needs-human', 'needs-decision', 'unauthorized'];
    const STAGE_TRACKER_NEEDS_HUMAN_TIP = 'Only a person can move these: triage needed, needs decision, human author must act, human merge, or a needs-human / unauthorized pill';
    const STAGE_TRACKER_ON_HOLD_TIP = 'Paused on purpose with a hold label — waiting on nobody; remove the hold to resume';
    // Label families a chip row is built from, in display order; other
    // labels follow when at least two items carry them.
    const STAGE_TRACKER_LABEL_FAMILIES = ['stage/', 'needs-', 'author/', 'role/', 'hold', 'on-hold', 'do-not-merge', 'hive-pause/'];
    const STAGE_TRACKER_SORTS = ['furthest', 'waiting', 'newest', 'number'];
    // Attention tiers within a column (stageTrackerAttentionTier; same
    // values as Go): the pills that sink a row, and the direction button's
    // note that the arrow never flips the tier.
    const STAGE_TRACKER_TIER_ATTENTION = 0;
    const STAGE_TRACKER_TIER_NORMAL = 1;
    const STAGE_TRACKER_TIER_SUNK = 2;
    const STAGE_TRACKER_SUNK_MODS = ['hold', 'draft'];
    const STAGE_TRACKER_DIR_TIER_NOTE = ' · Attention-first within each column; arrow flips age order';
    // Repo chip + group-by-repo. The chip before an item's number names its
    // repo (short name; owner/repo in the tooltip; click = the Repo filter)
    // and is shown always / only when the payload spans several repos
    // (default) / never. Group puts rows under one collapsible header per
    // repo. Both settings and the collapsed set persist per browser.
    const STAGE_TRACKER_REPO_CHIP_KEY = 'hive.tracker.repochip';
    const STAGE_TRACKER_REPO_CHIP_MODES = [
      { id: 'always', label: 'Always' },
      { id: 'multi', label: 'Only when several repos are listed' },
      { id: 'never', label: 'Never' },
    ];
    const STAGE_TRACKER_REPO_CHIP_DEFAULT = 'multi';
    const STAGE_TRACKER_GROUP_KEY = 'hive.tracker.group';
    const STAGE_TRACKER_GROUP_COLLAPSED_KEY = 'hive.tracker.group.collapsed';
    const STAGE_TRACKER_GROUPS = ['none', 'repo'];
    const STAGE_TRACKER_GROUP_DEFAULT = 'none';
    const STAGE_TRACKER_GROUP_KEY_PREFIX = 'group:';
    // Palette slots --ham-repo-tint-0..N-1 in tokens.css.
    const STAGE_TRACKER_REPO_TINTS = 6;
    // Mini stage distribution on a group header: bar height steps.
    const STAGE_TRACKER_GROUP_DIST_STEPS = 4;
    var _stageTrackerGroup = STAGE_TRACKER_GROUP_DEFAULT;
    var _stageTrackerCollapsed = {};
    function stageTrackerDefaultFilter() {
      return { kind: 'all', repo: '', q: '', labels: [], needsHuman: false, onHold: false, waitingOnYou: false, age: 'any', author: 'any', attention: false, acted: 'any', sort: 'furthest', dir: 'desc', size: STAGE_TRACKER_DEFAULT_PAGE_SIZE, page: 1 };
    }
    var _stageTrackerFilter = stageTrackerDefaultFilter();
    // Type-to-filter text of the "all labels" dialog (not persisted).
    var _stageTrackerLabelQuery = '';

    function stageTrackerHeadline(item) {
      if (item.superseded && item.superseded.closer_pr) return 'Superseded by #' + item.superseded.closer_pr + ' — decide';
      const n = Number(item.stage) || 0;
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      const holdLabels = stageTrackerHoldLabels(item);
      if (mods.indexOf('stalled') >= 0) return 'Stalled';
      if (mods.indexOf('regressed') >= 0 && !stageTrackerRegressionHandled(item)) return 'Sent back';
      // Advisory (hivecommons/hive#15381): the chip is the headline.
      if (n === 0 && item.kind !== 'pr' && item.advisory && typeof item.advisory === 'object') return item.advisory.chip || 'Advisory · read & close';
      if (n === 0 && mods.indexOf('hold') >= 0) return 'Open · on hold' + (holdLabels.length ? ' (' + holdLabels.join(', ') + ')' : '');
      if (n === 0 && mods.indexOf('blocked') >= 0) return 'Open · blocked';
      if (n === 0 && mods.indexOf('draft') >= 0) return 'Open · draft';
      // Guarded: test harnesses lift this function by name without the 000-new helpers.
      if (n === 0 && typeof stageTrackerOpenTriageNeeded === 'function' && stageTrackerOpenTriageNeeded(item)) return 'Triage needed';
      if (n === 0 && typeof stageTrackerNewState === 'function' && stageTrackerNewState(item)) return 'New';
      // An issue parked on a person (045, or a pre-045 085 with no covering
      // PR) is parked itself, so no "(its PR)" suffix.
      const viaPR = item.kind === 'issue' && n >= 50 && (n !== 85 || item.via_pr) ? ' (its PR)' : '';
      switch (n) {
        case 0: return 'Open';
        case 10: return 'Waiting for a worker';
        case 15: return 'Triage needed';
        case 17: return 'Possibly exists · needs author';
        case 20: return 'Claimed';
        case 30: return 'PR in progress';
        case 40: return 'Covered by a PR, not yet in review';
        case 45: return 'Needs a human decision';
        case 50: return 'Waiting for review' + viaPR;
        case 60: return 'Reviewed' + viaPR;
        case 70: return 'Fixing review findings' + viaPR;
        case 71: return 'Waiting on the author' + viaPR;
        case 72: return 'Back with the contributor' + viaPR;
        case 74: return 'Hive took it over' + viaPR;
        case 80: return 'Not yet admitted to auto merge' + viaPR;
        case 85: return 'Needs a human decision' + viaPR;
        case 90: return 'Ready for a human merge' + (stageTrackerHumanMergeReasonWord(item) ? ' · ' + stageTrackerHumanMergeReasonWord(item) : '') + viaPR;
        case 95: return 'Pre-merge soak' + stageTrackerSoakLeft(item) + viaPR;
        case 100: return 'Admitted — merging' + viaPR;
        case 110: return 'Merged';
        default: return item.stage_label || 'Tracking';
      }
    }

    // " — Xm left" for a 095 row whose payload carries the soak remaining.
    function stageTrackerSoakLeft(item) {
      const sec = Number(item.soak_remaining_seconds) || (item.merge_gate && Number(item.merge_gate.soak_remaining_seconds)) || 0;
      if (sec <= 0) return '';
      const min = Math.ceil(sec / 60);
      return ' — ' + (min >= 60 ? Math.floor(min / 60) + 'h' + (min % 60 ? (min % 60) + 'm' : '') : min + 'm') + ' left';
    }

    /* ---- Merge gate (hivecommons/hive#15073) --------------------------------
       Pure helpers over the payload's merge_gate {state, reason,
       sweep_running, last_sweep_at, next_sweep_at, soak_until,
       soak_remaining_seconds}: the pill word with its live detail, the
       plain-words sentence the leading glyph carries as tooltip + aria-label,
       and the glyph itself. Every row on the gate gets the glyph — a sweep
       on it now spins, a wait shows the clock — so two rows in the same
       state never differ. Countdowns tick in place (data-ham-until) from the
       cue age timer; no re-render. Run under node by
       stage_tracker_merge_gate_test.go. */
    // Pure: a stage number's merge-gate state when the payload has none.
    function stageTrackerMergeGateStateOf(item) {
      if (item && item.merge_gate && item.merge_gate.state) return String(item.merge_gate.state);
      const n = Number(item && item.stage) || 0;
      if (n === 60) return 'gate-pending';
      if (n === 80) return 'ready';
      if (n === STAGE_TRACKER_PRE_MERGE_SOAK) return 'soaking';
      if (n === 100) return 'merging';
      if (n >= STAGE_TRACKER_TERMINAL) return 'merged';
      return '';
    }
    // Pure: "42s" / "3m" / "1h05m" for a positive duration; '' otherwise.
    function stageTrackerShortLeft(ms) {
      if (!(ms > 0)) return '';
      const s = Math.ceil(ms / 1000);
      if (s < 60) return s + 's';
      const m = Math.ceil(s / 60);
      if (m < 60) return m + 'm';
      return Math.floor(m / 60) + 'h' + (m % 60 ? String(m % 60).padStart(2, '0') + 'm' : '');
    }
    // Pure: the gate's reason token in plain words — "CI running
    // (build-and-test)", "CI failed: lint"; '' for none. Pending is never
    // "red" (hivecommons/docs#512 read "CI is red (check-pending)").
    const STAGE_TRACKER_GATE_REASON_WORDS = {
      'check-pending': 'CI running', 'required-check-pending': 'CI running', 'check-failing': 'CI failed', 'required-check-failing': 'CI failed',
      'optional-check-failing': 'an optional check is red', conflicting: 'branch has conflicts', 'needs-rebase': 'needs a rebase', 'dco-missing': 'DCO sign-off missing',
    };
    // Pure: the 060 "Review passed" pill's detail from the lane's recorded
    // reason (stagelabels RouteHuman): "CI in progress" (checks pending),
    // "checks unreadable", "soak 30m remaining"; a gate token falls back to
    // its gate words; '' when there is no reason.
    function stageTrackerReviewPassedDetail(reason) {
      const raw = String(reason || '').trim();
      if (!raw) return '';
      const lower = raw.toLowerCase();
      if (/^(required-)?check-pending\b|^ci-unverified$|check\(s\) still running|^pending\b|-pending\b/.test(lower)) return 'CI in progress';
      if (/could not be read|unreadable|checks-unknown|^ci-unknown$/.test(lower)) return 'checks unreadable';
      const soak = /soak(?:ing)?\W*(?:remaining=)?(\d+(?:h\d+)?(?:m\d*)?(?:s)?)\s*(?:remaining)?/.exec(lower);
      if (soak) return 'soak ' + soak[1].replace(/(\d+m)0s$/, '$1').replace(/(\d+h)0m$/, '$1') + ' remaining';
      return stageTrackerGateReasonText(raw);
    }
    function stageTrackerGateReasonText(reason) {
      const raw = String(reason || '').trim();
      if (!raw) return '';
      const i = raw.indexOf(':');
      const slug = (i >= 0 ? raw.slice(0, i) : raw).replace(/\(.*$/, '');
      const detail = i >= 0 ? raw.slice(i + 1).replace(/\(.*$/, '') : '';
      const words = STAGE_TRACKER_GATE_REASON_WORDS[slug];
      if (!words) return raw;
      if (!detail) return words;
      return /failed$/.test(words) ? words + ': ' + detail : words + ' (' + detail + ')';
    }
    // Pure: "open PR #15090 covers it · at 060-reviewed" from an issue row's
    // lane reason ("open PR #15090 covers it (PR at 060-reviewed)"); '' for
    // any other reason. The lane reason is the issue's, never a gate token,
    // so it must not be read as one (hivecommons/hive#15084 regression).
    const STAGE_TRACKER_COVER_REASON_RE = /^open PR #(\d+) covers it(?: \(PR at ([^)]*)\))?$/i;
    // Stage → operator phrase, mirrored from stageTrackerStagePhrase
    // (stage_tracker_vocabulary.go): every string the page renders names a
    // stage by its chevron's words, never by a code ("060 reviewed").
    const STAGE_TRACKER_STAGE_PHRASE = { 10: 'actionable', 15: 'waiting for triage', 17: 'waiting for the author to confirm the gap (prior art found)', 20: 'claimed', 30: 'a PR in progress', 40: 'covered by a PR', 45: 'waiting for a human decision', 50: 'in CI / review', 60: 'review passed · waiting for merge admission', 70: 'being fixed by hive', 71: 'waiting for its author', 72: 'returned to the contributor', 74: 'adopted by hive', 80: 'at the merge queue', 85: 'waiting for a human decision', 90: 'ready for human merge', 95: 'soaking before merge', 100: 'in the merge queue' };
    // Pure: a stage's name in plain words for cues and tooltips — its short
    // word when the stages table has one, else its label with the lane's
    // "stage/060-" slug prefix dropped and the slug humanised ("Reviewed").
    // Minimal local fix for "fell back to stage/060-reviewed"
    // (hivecommons/docs#512); the copy sweep retires stage slugs globally.
    function stageTrackerStageName(st) {
      if (!st) return '';
      if (st.short) return String(st.short);
      const label = String(st.label || '').trim();
      const m = /^(?:stage\/)?\d{3}-(.+)$/.exec(label);
      if (!m) return label;
      const words = m[1].replace(/[-_]+/g, ' ');
      return words.charAt(0).toUpperCase() + words.slice(1);
    }
    // Pure: a stage code as the lane spells it ("060-reviewed", "stage/060-reviewed") → its phrase; the input when unknown.
    function stageTrackerStagePhrase(code) {
      const m = /^(?:stage\/)?(\d{3})(?:[- ][a-z-]+)?$/i.exec(String(code || '').trim());
      const n = m ? Number(m[1]) : Number(code);
      return STAGE_TRACKER_STAGE_PHRASE[n] || String(code || '');
    }
    function stageTrackerCoverReasonText(item) {
      const m = STAGE_TRACKER_COVER_REASON_RE.exec(String((item && item.reason) || '').trim());
      if (!m) return '';
      return 'open PR #' + m[1] + ' covers it' + (m[2] ? ' · its PR is ' + stageTrackerStagePhrase(m[2]) : '');
    }
