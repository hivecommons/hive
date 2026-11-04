// Code activity reader (#16475): changed regions as readable paragraphs on
// the kiosk. Opt-in like Matrix and fed by the same authorized pull endpoint
// (the shared SSE stream only nudges). Code is always set with textContent.
(function(){
  'use strict';
  var pull = function(){};
  window.hiveReaderAttach = function(source){
    if (!window.HIVE_READER) return;
    source.addEventListener('code-lines', function(){ pull(); });
    source.addEventListener('open', function(){ pull(); });
  };
  window.hiveReaderStart = function(){
    if (!window.HIVE_READER || document.getElementById('hive-reader')) return;
    // Cards kept on screen, regions waiting to be read, and how long one
    // paragraph stays in focus before the next arrives.
    var MAX_CARDS = 60, MAX_PENDING = 40, BASE_DWELL = 5000, LINE_DWELL = 600, MAX_DWELL = 30000;
    var NAV_KEYS = ['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End'];
    var STAGES = {'pushed-lines': 'pushed'};
    var root = document.createElement('main');
    root.id = 'hive-reader';
    root.setAttribute('aria-label', 'Code activity reader');
    document.body.appendChild(root);
    function element(tag, className, text, parent){
      var el = document.createElement(tag);
      el.className = className;
      if (text) el.textContent = text;
      (parent || root).appendChild(el);
      return el;
    }
    var bar = element('div', 'reader-bar');
    element('span', 'reader-source', 'Source: changes pushed by hive · public allowlisted repos', bar);
    var status = element('span', 'reader-status', 'Waiting for pushed changes…', bar);
    status.setAttribute('role', 'status');
    var button = element('button', 'reader-pause', 'Pause', bar);
    button.setAttribute('type', 'button');
    button.setAttribute('aria-pressed', 'false');
    button.setAttribute('title', 'Pause or resume (P / Space)');
    var scroller = element('div', 'reader-scroll');
    scroller.setAttribute('tabindex', '0');
    var feed = element('div', 'reader-feed', '', scroller);
    var motion = window.matchMedia('(prefers-reduced-motion: reduce)');
    var pending = [], current = null, readUntil = 0, timer = 0, lastSeq = 0;
    var enabled = false, manualPause = false, loading = false, again = false, problem = '';
    function paused(){ return manualPause || document.hidden; }
    function regionKey(line){ return [line.repo, line.pr, line.head || '', line.path].join('\u0000'); }
    // Consecutive lines of one file at one head form one contiguous region;
    // a gap in seq (a dropped line or another file) starts a new one. Only
    // unread regions grow, so a paragraph on screen never changes.
    function group(lines, stage){
      lines.forEach(function(line){
        var last = pending[pending.length - 1], seq = Number(line.seq) || 0;
        if (last && last.key === regionKey(line) && seq && seq === last.lastSeq + 1) {
          last.lines.push(line); last.lastSeq = seq; last.at = line.at;
          return;
        }
        pending.push({key: regionKey(line), repo: line.repo, pr: Number(line.pr) || 0, path: line.path,
          prURL: line.pr_url, stage: stage, lines: [line], lastSeq: seq, at: line.at});
      });
      if (pending.length > MAX_PENDING) pending = pending.slice(-MAX_PENDING);
    }
    function prLink(region){
      var url = typeof region.prURL === 'string' ? region.prURL : '';
      if (/^https:\/\/[^\s"'<>]+\/pull\/\d+$/.test(url)) return url;
      if (/^[\w.-]+\/[\w.-]+$/.test(region.repo || '') && region.pr > 0) {
        return 'https://github.com/' + region.repo + '/pull/' + region.pr;
      }
      return '';
    }
    function observed(at){
      var ms = Date.parse(at);
      if (!Number.isFinite(ms)) return {iso: '', text: 'observed time unknown'};
      var d = new Date(ms);
      // The shared helper honours the dashboard time-zone preference.
      var shown = typeof formatDashboardDateTime === 'function' ? formatDashboardDateTime(d) : d.toISOString();
      return {iso: d.toISOString(), text: 'observed ' + shown};
    }
    function card(region){
      var article = document.createElement('article');
      article.className = 'reader-card';
      var meta = element('header', 'reader-meta', '', article);
      var href = prLink(region);
      var repo = element(href ? 'a' : 'span', 'reader-repo', region.repo + (region.pr > 0 ? ' #' + region.pr : ''), meta);
      if (href) {
        repo.href = href;
        repo.setAttribute('target', '_blank');
        repo.setAttribute('rel', 'noopener noreferrer');
      }
      element('span', 'reader-file', region.path, meta);
      element('span', 'reader-stage', region.stage, meta);
      var when = observed(region.at);
      var time = element('time', 'reader-time', when.text, meta);
      if (when.iso) time.setAttribute('datetime', when.iso);
      var diff = element('pre', 'reader-diff', '', article);
      region.lines.forEach(function(line){
        var removed = line.op === '-';
        var row = element('span', 'reader-line ' + (removed ? 'reader-del' : 'reader-add'), '', diff);
        row.textContent = (removed ? '\u2212 ' : '+ ') + line.line + '\n';
      });
      return article;
    }
    function dwell(region){ return Math.min(MAX_DWELL, BASE_DWELL + region.lines.length * LINE_DWELL); }
    // Drop the oldest cards and keep the visible text where it was.
    function trim(){
      while (feed.children.length > MAX_CARDS) {
        var first = feed.children[0], height = first.offsetHeight || 0;
        feed.removeChild(first);
        scroller.scrollTop = Math.max(0, (scroller.scrollTop || 0) - height);
      }
    }
    function reveal(region, scroll){
      region.node = card(region);
      feed.appendChild(region.node);
      trim();
      current = region;
      readUntil = Date.now() + dwell(region);
      if (scroll && region.node.scrollIntoView) region.node.scrollIntoView({block: 'start', behavior: motion.matches ? 'auto' : 'smooth'});
    }
    function updateStatus(){
      var text = problem;
      if (!text && enabled) {
        if (pending.length) text = pending.length + (pending.length === 1 ? ' change' : ' changes') + ' waiting';
        else text = current ? 'Caught up · holding until new changes are pushed' : 'Waiting for pushed changes…';
      }
      if (paused()) text = 'paused' + (text ? ' · ' + text : '');
      status.textContent = text;
      button.textContent = manualPause ? 'Resume' : 'Pause';
      button.setAttribute('aria-pressed', manualPause ? 'true' : 'false');
    }
    function stop(){ clearTimeout(timer); timer = 0; }
    function advance(){
      timer = 0;
      if (!enabled || paused() || motion.matches) return;
      // Quiet hold when caught up: no replay, no filler, nothing scheduled.
      if (pending.length) {
        reveal(pending.shift(), true);
        timer = setTimeout(advance, dwell(current));
      }
      updateStatus();
    }
    // Starts the next reveal once the current paragraph's dwell ends; never
    // restarts a running dwell, so incoming work cannot interrupt reading.
    function schedule(fresh){
      if (timer || !enabled || paused()) { updateStatus(); return; }
      if (motion.matches) {
        // Static and scrollable: everything is shown, nothing scrolls by itself.
        while (pending.length) reveal(pending.shift(), false);
        updateStatus();
        return;
      }
      if (fresh && current) {
        readUntil = Date.now() + dwell(current);
        if (current.node && current.node.scrollIntoView) current.node.scrollIntoView({block: 'start', behavior: 'smooth'});
      }
      var wait = readUntil - Date.now();
      if (pending.length && wait <= 0) { advance(); return; }
      if (pending.length) timer = setTimeout(advance, wait);
      updateStatus();
    }
    function setPaused(value){
      if (manualPause === value) return;
      manualPause = value;
      stop();
      if (!manualPause) schedule(true);
      updateStatus();
    }
    function inspect(){ setPaused(true); }
    function clearContent(message){
      stop();
      enabled = false; pending = []; current = null; lastSeq = 0; problem = message;
      feed.replaceChildren();
      updateStatus();
    }
    pull = function(){
      if (loading) { again = true; return; }
      loading = true; again = false;
      fetch('/api/matrix/lines?since=' + lastSeq, {cache: 'no-store', credentials: 'same-origin'})
        .then(function(response){
          if (response.status === 404) { clearContent('The code activity reader is not enabled on this hive'); return null; }
          if (response.status === 401 || response.status === 403) { clearContent('Sign in with permission to read code activity'); return null; }
          if (!response.ok) throw new Error('reader pull failed');
          return response.json();
        }).then(function(data){
          if (!data) return;
          var seq = Number(data.seq) || 0;
          // A restarted hive can reset its sequence; start over cleanly.
          if (seq < lastSeq) { stop(); pending = []; current = null; feed.replaceChildren(); }
          var incoming = (Array.isArray(data.lines) ? data.lines : []).filter(function(line){
            return line && typeof line.line === 'string' && typeof line.repo === 'string' && typeof line.path === 'string';
          });
          var source = typeof data.source === 'string' ? data.source : '';
          group(incoming, STAGES[source] || source || 'pushed');
          lastSeq = seq;
          enabled = true; problem = '';
          schedule(false);
        }).catch(function(){ problem = 'Code activity unavailable · retrying'; updateStatus(); })
        .finally(function(){ loading = false; if (again) pull(); });
    };
    button.addEventListener('click', function(){ setPaused(!manualPause); });
    scroller.addEventListener('wheel', inspect, {passive: true});
    scroller.addEventListener('touchstart', inspect, {passive: true});
    document.addEventListener('visibilitychange', function(){
      if (document.hidden) { stop(); updateStatus(); return; }
      schedule(true); pull();
    });
    document.addEventListener('keydown', function(event){
      var target = event.target || {};
      if (event.ctrlKey || event.metaKey || event.altKey ||
          /INPUT|TEXTAREA|SELECT/.test(target.tagName) || target.isContentEditable) return;
      if (NAV_KEYS.indexOf(event.key) >= 0) { inspect(); return; }
      if (event.repeat) return;
      if (event.key === 'p' || event.key === 'P' || (event.code === 'Space' && target.tagName !== 'BUTTON')) {
        event.preventDefault(); setPaused(!manualPause);
      }
    });
    motion.addEventListener('change', function(){ stop(); schedule(false); });
    updateStatus(); pull();
    // Recover from missed nudges, endpoint activation and transient failures.
    if (typeof dashboardSetInterval === 'function') {
      dashboardSetInterval('reader-poll', function(){ if (!document.hidden) pull(); }, 30000);
    }
  };
})();
