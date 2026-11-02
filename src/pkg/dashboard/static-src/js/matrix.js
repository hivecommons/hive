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
    var effect = option('effect', 'rain');
    if (['rain', 'cascade', 'ghost', 'terminal'].indexOf(effect) < 0) effect = 'rain';
    var captions = option('captions', 'on') !== 'off';
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
    var width = 0, height = 0, cell = 20, rowH = 20, charW = 10;
    // Ghost coder and Terminal type the delivered lines in order; once the
    // queue runs dry they replay the bounded latest-line window.
    var queue = [], tabs = [], typing = null, termRows = [], termHead = '', termClock = 0;
    var muted = '#6f8f78', syntaxColors = {keyword: '#7fd4ff', string: '#ffd27f', comment: muted, number: '#ff9cf0'};
    var languages = {
      go: {comment: '//', words: 'package import func return if else for range var const type struct interface map chan go defer select case switch default break continue nil true false'},
      js: {comment: '//', words: 'function var let const return if else for while do new class extends import export from default async await this null undefined true false typeof instanceof switch case break continue try catch finally throw of in'},
      py: {comment: '#', words: 'def class import from return if elif else for while in not and or is None True False with as lambda yield pass raise try except finally async await'},
      sh: {comment: '#', words: 'if then else elif fi for while do done case esac function local export return in'},
      rs: {comment: '//', words: 'fn let mut pub use mod struct enum impl trait match if else for while loop return self Self true false const static where async await'},
      c: {comment: '//', words: 'if else for while do return break continue switch case default new class public private protected static const struct enum typedef sizeof void int char long double float unsigned true false null this import package'}
    };
    var extensions = {go: 'go', js: 'js', mjs: 'js', cjs: 'js', ts: 'js', tsx: 'js', jsx: 'js', py: 'py', sh: 'sh', bash: 'sh',
      rs: 'rs', c: 'c', h: 'c', cc: 'c', cpp: 'c', hpp: 'c', java: 'c', cs: 'c'};
    Object.keys(languages).forEach(function(k){ languages[k].words = new Set(languages[k].words.split(' ')); });
    function paused(){ return manualPause || document.hidden; }
    function resize(){
      width = window.innerWidth; height = window.innerHeight;
      canvas.width = width; canvas.height = height;
      drops = [];
      ctx.font = '16px monospace';
      charW = (ctx.measureText && ctx.measureText('M').width) || 10;
    }
    function resetText(){ queue = []; tabs = []; typing = null; termRows = []; termHead = ''; termClock = 0; }
    function clearContent(message){
      enabled = false; lines = []; drops = []; glyphs.clear(); resetText();
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
    function prKey(line){ return line.repo + '#' + line.pr; }
    function nextLine(key){
      if (!queue.length) queue = lines.slice();
      var at = 0;
      for (var i = 0; key && i < queue.length; i++) if (prKey(queue[i]) === key) { at = i; break; }
      return queue.splice(at, 1)[0] || null;
    }
    // Minimal keyword/string/comment/number colouring keyed on the extension;
    // unknown extensions render in the palette colour.
    function tokens(path, text){
      var ext = (/\.([^.\/]+)$/.exec(path || '') || [])[1];
      var lang = languages[extensions[ext ? ext.toLowerCase() : '']];
      if (!lang) return [{text: text, color: color}];
      var out = [], last = 0, match;
      var pattern = /("(?:[^"\\]|\\.)*"?|'(?:[^'\\]|\\.)*'?|`[^`]*`?)|(\b\d[\w.]*)|([A-Za-z_$][\w$]*)|(\/\/|#)/g;
      function push(t, c){ if (t) out.push({text: t, color: c}); }
      while ((match = pattern.exec(text))) {
        push(text.slice(last, match.index), color);
        last = pattern.lastIndex;
        if (match[4] === lang.comment) { push(text.slice(match.index), syntaxColors.comment); last = text.length; break; }
        push(match[0], match[1] ? syntaxColors.string : match[2] ? syntaxColors.number :
          match[3] && lang.words.has(match[0]) ? syntaxColors.keyword : color);
      }
      push(text.slice(last), color);
      return out;
    }
    function openTab(line){
      var key = prKey(line), tab = null;
      tabs.forEach(function(t){ if (t.key === key) tab = t; });
      if (!tab) {
        tab = {key: key, rows: [], count: 0}; tabs.push(tab);
        if (tabs.length > 6) tabs.shift();
      }
      tab.path = line.path;
      return tab;
    }
    function startTyping(line){
      typing = line ? {tab: openTab(line), line: line, chars: Array.from(line.line), typed: 0} : null;
    }
    function drawGhost(dt, now){
      if (!typing) startTyping(nextLine(''));
      if (!typing) return;
      typing.typed += dt * 40;
      // A short beat at the end of each line before the next one starts.
      if (typing.typed >= typing.chars.length + 4) {
        var done = typing.tab;
        done.count++;
        done.rows.push({n: done.count, path: typing.line.path, text: typing.line.line});
        if (done.rows.length > 200) done.rows.shift();
        startTyping(nextLine(done.key));
        if (!typing) return;
      }
      var tab = typing.tab, top = 34, tabW = Math.min(240, width / Math.max(1, tabs.length));
      ctx.font = '13px monospace';
      tabs.forEach(function(t, i){
        ctx.fillStyle = t === tab ? '#0b2a16' : '#02140a';
        ctx.fillRect(i * tabW, 0, tabW - 2, 28);
        ctx.fillStyle = t === tab ? color : muted;
        ctx.fillText(t.path, i * tabW + 10, 19, tabW - 20);
      });
      if (captions) {
        ctx.fillStyle = '#b8ccbb'; ctx.font = '12px monospace';
        ctx.fillText(caption(typing.line), 10, 48, width - 20);
        top = 56;
      }
      ctx.font = '16px monospace';
      var visible = Math.max(1, Math.floor((height - top - 100) / rowH));
      var current = {n: tab.count + 1, path: typing.line.path, text: typing.chars.slice(0, Math.floor(typing.typed)).join(''), current: true};
      tab.rows.slice(-(visible - 1) || tab.rows.length).concat([current]).forEach(function(row, i){
        var y = top + (i + 1) * rowH, col = 0;
        ctx.fillStyle = muted; ctx.fillText(String(row.n), 10, y);
        tokens(row.path, row.text).forEach(function(token){
          ctx.fillStyle = token.color; ctx.fillText(token.text, 60 + col * charW, y);
          col += Array.from(token.text).length;
        });
        if (row.current && Math.floor(now / 530) % 2 === 0) { ctx.fillStyle = color; ctx.fillRect(60 + col * charW, y - 14, 2, 18); }
      });
    }
    function emitTerminal(){
      var line = nextLine('');
      if (!line) return;
      if (caption(line) !== termHead) {
        termHead = caption(line);
        if (captions) {
          if (termRows.length) termRows.push({text: '', color: color});
          termRows.push({text: termHead, color: '#b8ccbb'});
        }
      }
      termRows.push({text: '+ ' + line.line, color: color});
      var keep = Math.ceil(height / rowH) + 4;
      if (termRows.length > keep) termRows = termRows.slice(-keep);
    }
    function drawTerminal(dt){
      if (!termRows.length) emitTerminal();
      termClock += dt * 4;
      while (termClock >= 1) { termClock -= 1; emitTerminal(); }
      var bottom = height - 100;
      ctx.font = '16px monospace';
      termRows.forEach(function(row, i){
        var y = bottom - (termRows.length - 1 - i + termClock) * rowH;
        if (y < -rowH || !row.text) return;
        ctx.fillStyle = row.color; ctx.fillText(row.text, 10, y, width - 20);
      });
    }
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
        if (lines.length && effect === 'ghost') drawGhost(dt, now);
        else if (lines.length && effect === 'terminal') drawTerminal(dt);
        else if (lines.length) {
          var count = effect === 'rain' ? Math.ceil(width / cell) : 12;
          while (drops.length < count) drops.push(spawn(effect === 'rain' ? drops.length * cell : Math.random() * width * 0.5));
          drops.forEach(function(drop, i){
            drop.y += drop.speed * dt;
            if (drop.y > height + 40) { drops[i] = spawn(drop.x); return; }
            if (effect === 'cascade') {
              ctx.fillStyle = color; ctx.fillText(drop.line.line, drop.x, drop.y, width - drop.x);
              if (captions) {
                ctx.fillStyle = '#b8ccbb'; ctx.font = '12px monospace';
                ctx.fillText(caption(drop.line), drop.x, drop.y + 18, width - drop.x);
                ctx.font = '16px monospace';
              }
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
          if (seq < lastSeq) { lines = []; drops = []; resetText(); }
          if (seq !== lastSeq && incoming.length) {
            var at = Date.parse(incoming[incoming.length - 1].at);
            lastAt = Number.isFinite(at) ? at : Date.now();
          }
          lastSeq = seq;
          lines = lines.concat(incoming).slice(-120);
          queue = queue.concat(incoming).slice(-120);
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
