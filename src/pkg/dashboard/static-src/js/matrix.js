// Matrix is an opt-in display only. Content always comes from the authorized
// pull endpoint; the shared SSE stream carries a content-free nudge.
(function(){
  'use strict';
  var pull = function(){};
  window.hiveMatrixAttach = function(source){
    if (!window.HIVE_MATRIX) return;
    source.addEventListener('code-lines', function(){ pull(); });
    source.addEventListener('open', function(){ pull(); });
  };
  window.hiveMatrixStart = function(){
    if (!window.HIVE_MATRIX || document.getElementById('hive-matrix')) return;
    var params = new URL(window.location.href).searchParams;
    function option(name, fallback){
      var value = params.get('mx.' + name);
      try {
        if (value === null) value = localStorage.getItem('hive.matrix.' + name);
        else localStorage.setItem('hive.matrix.' + name, value);
      } catch(e) {}
      return value === null ? fallback : value;
    }
    function number(name, fallback, min, max){
      var value = Number(option(name, String(fallback)));
      return Number.isFinite(value) ? Math.max(min, Math.min(max, value)) : fallback;
    }
    var effect = option('effect', 'rain') === 'cascade' ? 'cascade' : 'rain';
    var speed = number('speed', 1, 0.1, 5), fps = number('fps', 30, 1, 60);
    var palette = option('palette', 'green');
    var colors = {green: '#52ed86', amber: '#ffb347', gold: '#ffd700'};
    var color = colors[palette] || colors.green;
    var root = document.createElement('main');
    root.id = 'hive-matrix';
    root.style.setProperty('--matrix-color', color);
    document.body.appendChild(root);
    function element(tag, className, text){
      var el = document.createElement(tag);
      el.className = className;
      if (text) el.textContent = text;
      root.appendChild(el);
      return el;
    }
    var canvas = element('canvas', 'matrix-canvas');
    canvas.setAttribute('aria-label', 'Pushed code lines · Matrix ' + effect);
    var list = element('div', 'matrix-list');
    var notice = element('p', 'matrix-notice', 'Waiting for pushed lines…');
    notice.setAttribute('role', 'status');
    var labels = element('div', 'matrix-labels');
    var source = document.createElement('div');
    source.textContent = 'Source: lines pushed by hive · public allowlisted repos';
    var freshness = document.createElement('div');
    var pausedLabel = document.createElement('div');
    labels.append(source, freshness, pausedLabel);
    var ctx = canvas.getContext('2d');
    var motion = window.matchMedia('(prefers-reduced-motion: reduce)');
    var lines = [], lastSeq = 0, lastAt = 0, startedAt = Date.now();
    var enabled = false, manualPause = false, loading = false, pending = false;
    var frame = 0, previous = 0, page = 0, pageTimer = 0, drops = [];
    var atlas = document.createElement('canvas'), glyphs = new Map();
    var width = 0, height = 0, cell = 20;
    function paused(){ return manualPause || document.hidden; }
    function resize(){
      width = window.innerWidth; height = window.innerHeight;
      canvas.width = width; canvas.height = height;
      drops = [];
    }
    function clearContent(message){
      enabled = false; lines = []; drops = []; glyphs.clear();
      lastSeq = 0; lastAt = 0;
      list.replaceChildren(); ctx.clearRect(0, 0, width, height);
      notice.textContent = message;
      sync();
    }
    function updateLabels(){
      var minutes = Math.floor(Math.max(0, Date.now() - (lastAt || startedAt)) / 60000);
      freshness.textContent = minutes >= 10 ? 'stale · no new lines for ' + minutes + 'm' :
        (lastAt ? 'last line ' + (minutes ? minutes + 'm ago' : 'just now') : 'waiting for first line');
      pausedLabel.textContent = paused() ? 'paused' : 'p / Space to pause';
    }
    function caption(line){ return line.repo + '#' + line.pr + ' · ' + line.path; }
    function staticPage(){
      list.replaceChildren();
      var latest = lines.slice(-60).reverse();
      if (page * 8 >= latest.length) page = 0;
      latest.slice(page * 8, page * 8 + 8).forEach(function(line){
        var row = document.createElement('div');
        var label = document.createElement('small');
        label.textContent = caption(line);
        var text = document.createElement('pre');
        text.textContent = line.line;
        row.append(label, text); list.appendChild(row);
      });
    }
    function buildAtlas(){
      // Bounded by the same latest-line window as the renderer. No synthetic
      // characters: every glyph in a column belongs to an incoming line.
      glyphs.clear();
      lines.forEach(function(line){ Array.from(line.line).forEach(function(g){
        if (!glyphs.has(g)) glyphs.set(g, glyphs.size);
      }); });
      atlas.width = 64 * cell;
      atlas.height = Math.max(cell, Math.ceil(glyphs.size / 64) * cell);
      var ac = atlas.getContext('2d');
      ac.font = '16px monospace'; ac.fillStyle = color;
      glyphs.forEach(function(i, g){ ac.fillText(g, (i % 64) * cell, Math.floor(i / 64) * cell + 16); });
    }
    function spawn(x){
      var line = lines[Math.floor(Math.random() * lines.length)];
      return {x: x, y: -Math.random() * height, speed: 25 + Math.random() * 65,
        line: line, chars: Array.from(line.line)};
    }
    function draw(now){
      frame = 0;
      if (paused() || motion.matches || !enabled) return;
      if (!previous) previous = now;
      var elapsed = now - previous;
      if (elapsed >= 1000 / fps) {
        previous = now;
        var dt = Math.min(elapsed, 100) / 1000 * speed;
        ctx.fillStyle = effect === 'rain' ? 'rgba(0, 8, 4, 0.18)' : '#000804';
        ctx.fillRect(0, 0, width, height);
        ctx.font = '16px monospace';
        if (lines.length) {
          var count = effect === 'rain' ? Math.ceil(width / cell) : 12;
          while (drops.length < count) drops.push(spawn(effect === 'rain' ? drops.length * cell : Math.random() * width * 0.5));
          drops.forEach(function(drop, i){
            drop.y += drop.speed * dt;
            if (drop.y > height + 40) { drops[i] = spawn(drop.x); return; }
            if (effect === 'cascade') {
              ctx.fillStyle = color; ctx.fillText(drop.line.line, drop.x, drop.y, width - drop.x);
              ctx.fillStyle = '#b8ccbb'; ctx.font = '12px monospace';
              ctx.fillText(caption(drop.line), drop.x, drop.y + 18, width - drop.x);
              ctx.font = '16px monospace';
            } else if (drop.chars.length) {
              var g = drop.chars[Math.floor(drop.y / cell + drop.chars.length * 100) % drop.chars.length];
              var index = glyphs.get(g);
              if (index !== undefined) ctx.drawImage(atlas, (index % 64) * cell, Math.floor(index / 64) * cell, cell, cell, drop.x, drop.y, cell, cell);
              ctx.fillStyle = '#effff2'; ctx.fillText(g, drop.x, drop.y + 16);
            }
          });
        }
      }
      frame = requestAnimationFrame(draw);
    }
    function sync(){
      if (frame) cancelAnimationFrame(frame);
      frame = 0; previous = 0;
      clearTimeout(pageTimer); pageTimer = 0;
      updateLabels();
      canvas.hidden = motion.matches || !enabled;
      list.hidden = !motion.matches || !enabled;
      if (!enabled || paused()) return;
      if (motion.matches) {
        staticPage();
        pageTimer = setTimeout(function(){ page++; sync(); }, 15000);
      } else frame = requestAnimationFrame(draw);
    }
    pull = function(){
      if (loading) { pending = true; return; }
      loading = true; pending = false;
      fetch('/api/matrix/lines?since=' + lastSeq, {cache: 'no-store', credentials: 'same-origin'})
        .then(function(response){
          if (response.status === 404) { clearContent('Matrix mode is not enabled on this hive'); return null; }
          if (response.status === 401 || response.status === 403) { clearContent('Sign in with permission to read Matrix lines'); return null; }
          if (!response.ok) throw new Error('Matrix pull failed');
          return response.json();
        }).then(function(data){
          if (!data) return;
          var incoming = (Array.isArray(data.lines) ? data.lines : []).filter(function(line){ return typeof line.line === 'string'; });
          var seq = Number(data.seq) || 0;
          // A restarted hive can reset its sequence; replace the old backlog.
          if (seq < lastSeq) { lines = []; drops = []; }
          if (seq !== lastSeq && incoming.length) {
            var at = Date.parse(incoming[incoming.length - 1].at);
            lastAt = Number.isFinite(at) ? at : Date.now();
          }
          lastSeq = seq;
          lines = lines.concat(incoming).slice(-120);
          enabled = true;
          notice.textContent = lines.length ? '' : 'Waiting for pushed lines…';
          if (!motion.matches) buildAtlas();
          sync();
        }).catch(function(){ clearContent('Matrix lines unavailable · retrying'); })
        .finally(function(){ loading = false; if (pending) pull(); });
    };
    document.addEventListener('visibilitychange', function(){ sync(); if (!document.hidden) pull(); });
    document.addEventListener('keydown', function(event){
      if (event.ctrlKey || event.metaKey || event.altKey || event.repeat ||
          /INPUT|TEXTAREA|SELECT/.test(event.target.tagName) || event.target.isContentEditable) return;
      if (event.key === 'p' || event.key === 'P' || event.code === 'Space') {
        event.preventDefault(); manualPause = !manualPause; sync();
      }
    });
    motion.addEventListener('change', function(){
      ctx.clearRect(0, 0, width, height);
      if (!motion.matches) buildAtlas();
      sync();
    });
    window.addEventListener('resize', resize);
    resize(); sync(); pull();
    // Recover from missed nudges, endpoint activation and transient failures.
    if (typeof dashboardSetInterval === 'function') {
      dashboardSetInterval('matrix-poll', function(){ updateLabels(); if (!document.hidden) pull(); }, 30000);
    }
  };
})();
