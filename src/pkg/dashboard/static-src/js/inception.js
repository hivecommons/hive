    // ── Inception section ──
    let _inceptionState = null;


    async function loadInceptionState() {
      try {
        const r = await fetch('/api/inception/state', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        _inceptionState = data.active ? data.state : null;
        renderInception();
        loadCampaigns();
      } catch (e) { console.error('inception state load failed', e); }
    }

    function renderInception() {
      refreshSectionCardShell('inception-section');
      setSectionSummary('inception-section', inceptionSectionSummary());
      const panel = document.getElementById('inception-panel');
      if (!panel) return;

      let wizard = '';
      if (!_inceptionState || !_inceptionState.phase) {
        wizard = renderInceptionModeSelect();
      } else {
        switch (_inceptionState.phase) {
          case 'capture':
            if (_inceptionState.idea_text) {
              wizard = renderInceptionProgress('idea') + renderInceptionWorking();
            } else {
              wizard = _inceptionState.mode === 'greenfield'
                ? renderInceptionCapture()
                : renderInceptionScanProgress();
            }
            break;
          case 'clarify':
            wizard = renderInceptionProgress('clarify') + renderInceptionClarify();
            break;
          case 'structure':
            wizard = renderInceptionProgress('structure') + renderInceptionStructure();
            break;
          case 'scaffold':
            wizard = renderInceptionProgress('scaffold') + renderInceptionScaffold();
            break;
          case 'complete':
            wizard = renderInceptionProgress('complete') + renderInceptionComplete();
            setTimeout(loadInceptionCompleteFiles, 100);
            break;
          default:
            wizard = renderInceptionModeSelect();
        }
      }
      // Rebuilding the panel resets its own scroll and the campaign filters;
      // carry both across so refreshes don't move the reader (#11032).
      const panelScrollTop = panel.scrollTop;
      const campaignFilters = {};
      panel.querySelectorAll('.campaigns-filters input').forEach(function(el) { campaignFilters[el.id] = el.value; });
      panel.innerHTML = '<div class="inception-spektacular-note">Project inception runs on <a href="https://github.com/jumppad-labs/spektacular" target="_blank" rel="noopener noreferrer">Spektacular</a>, a Hive Commons project.</div>' + renderCampaignsPanel() + '<div class="campaigns-card">' + wizard + '</div>';
      Object.keys(campaignFilters).forEach(function(id) {
        const el = document.getElementById(id);
        if (el) { el.defaultValue = campaignFilters[id]; el.value = campaignFilters[id]; }
      });
      panel.scrollTop = panelScrollTop;
      renderCampaignsList();
      if (!_inceptionPollTimer) startInceptionPoll();
    }

    function renderInceptionProgress(phase) {
      var STAGES = [
        { id: 'idea', label: 'Idea', icon: '💡' },
        { id: 'clarify', label: 'Clarify', icon: '🔍' },
        { id: 'structure', label: 'Structure', icon: '📐' },
        { id: 'scaffold', label: 'Scaffold', icon: '🏗️' },
        { id: 'complete', label: 'Complete', icon: '✅' }
      ];

      var currentIndex = STAGES.findIndex(function(s) { return s.id === phase; });
      if (currentIndex === -1) { currentIndex = 0; }

      var isDark = document.body.classList.contains('dark') ||
        window.matchMedia('(prefers-color-scheme: dark)').matches ||
        (document.body.style.backgroundColor || '').indexOf('0d1117') !== -1;

      var COLORS = isDark
        ? { bg: 'var(--panel)', border: 'var(--line)', text: 'var(--text)', muted: 'var(--muted)', blue: 'var(--blue)', green: 'var(--green)', lineBg: 'var(--line)' }
        : { bg: '#ffffff', border: '#d0d7de', text: '#1f2328', muted: '#656d76', blue: '#0969da', green: '#1a7f37', lineBg: '#d0d7de' };

      var STAGE_COUNT = STAGES.length;
      var CONNECTOR_SEGMENTS = STAGE_COUNT - 1;

      var html = '';
      html += '<div style="';
      html += 'display:flex;align-items:flex-start;justify-content:center;';
      html += 'padding:var(--sp-6) var(--sp-8) var(--sp-5);';
      html += 'background:' + COLORS.bg + ';';
      html += 'border:1px solid ' + COLORS.border + ';';
      html += 'border-radius:8px;';
      html += 'font-family:var(--font-ui);';
      html += 'position:relative;';
      html += 'margin-bottom:var(--sp-6);';
      html += '">';

      for (var i = 0; i < STAGE_COUNT; i++) {
        var stage = STAGES[i];
        var isPast = i < currentIndex;
        var isCurrent = i === currentIndex;
        var isFuture = i > currentIndex;

        var circleSize = isCurrent ? '32px' : '28px';
        var circleBorder = isCurrent ? '3px' : '2px';
        var circleBg, circleColor, circleBorderColor;

        if (isPast) {
          circleBg = COLORS.green;
          circleColor = '#ffffff';
          circleBorderColor = COLORS.green;
        } else if (isCurrent) {
          circleBg = COLORS.blue;
          circleColor = '#ffffff';
          circleBorderColor = COLORS.blue;
        } else {
          circleBg = 'transparent';
          circleColor = COLORS.muted;
          circleBorderColor = COLORS.lineBg;
        }

        var labelColor = isCurrent ? COLORS.text : (isPast ? COLORS.green : COLORS.muted);
        var labelWeight = isCurrent ? '600' : '400';
        var labelSize = isCurrent ? '12px' : '11px';

        html += '<div style="';
        html += 'display:flex;flex-direction:column;align-items:center;';
        html += 'flex:1;position:relative;z-index:1;';
        html += '">';

        /* Circle */
        html += '<div style="';
        html += 'width:' + circleSize + ';height:' + circleSize + ';';
        html += 'border-radius:50%;';
        html += 'background:' + circleBg + ';';
        html += 'border:' + circleBorder + ' solid ' + circleBorderColor + ';';
        html += 'display:flex;align-items:center;justify-content:center;';
        html += 'font-size:' + (isCurrent ? '15px' : '13px') + ';';
        html += 'color:' + circleColor + ';';
        html += 'transition:all 0.3s ease;';
        html += 'box-shadow:' + (isCurrent ? '0 0 0 4px ' + COLORS.blue + '33' : 'none') + ';';
        html += '">';

        if (isPast) {
          html += '<svg class="d-block" width="14" height="14" viewBox="0 0 16 16" fill="none">';
          html += '<path d="M13.78 4.22a.75.75 0 010 1.06l-7.25 7.25a.75.75 0 01-1.06 0L2.22 9.28a.75.75 0 011.06-1.06L6 10.94l6.72-6.72a.75.75 0 011.06 0z" fill="#ffffff"/>';
          html += '</svg>';
        } else if (isCurrent) {
          html += '<span style="line-height:1;">' + stage.icon + '</span>';
        } else {
          html += '<span style="font-size:11px;color:' + COLORS.muted + ';">' + (i + 1) + '</span>';
        }

        html += '</div>';

        /* Label */
        html += '<div style="';
        html += 'margin-top:var(--sp-4);';
        html += 'font-size:' + labelSize + ';';
        html += 'font-weight:' + labelWeight + ';';
        html += 'color:' + labelColor + ';';
        html += 'text-align:center;';
        html += 'white-space:nowrap;';
        html += '">' + stage.label + '</div>';

        html += '</div>';

        /* Connector line between stages */
        if (i < CONNECTOR_SEGMENTS) {
          var lineColor = (i < currentIndex) ? COLORS.green : COLORS.lineBg;
          var lineHeight = '2px';

          html += '<div style="';
          html += 'flex:2;';
          html += 'height:' + lineHeight + ';';
          html += 'background:' + lineColor + ';';
          html += 'margin-top:' + (isCurrent ? '16px' : '14px') + ';';
          html += 'border-radius:1px;';
          html += 'align-self:flex-start;';
          html += 'transition:background 0.3s ease;';
          html += '">';

          /* Animated pulse on the active connector */
          if (i === currentIndex) {
            html += '<div style="';
            html += 'width:30%;height:100%;';
            html += 'background:linear-gradient(90deg,transparent,' + COLORS.blue + ',transparent);';
            html += 'border-radius:1px;';
            html += 'animation:inceptionPulse 2s ease-in-out infinite;';
            html += '"></div>';
          }

          html += '</div>';
        }
      }

      /* spec-kit attribution */
      html += '<div style="text-align:center;margin-top:var(--sp-2);margin-bottom:-6px;">';
      html += '<span style="font-size:10px;color:' + COLORS.muted + ';letter-spacing:0.3px;">';
      html += 'Powered by <a href="https://github.com/github/spec-kit" target="_blank" rel="noopener" style="color:' + COLORS.muted + ';text-decoration:underline;text-underline-offset:2px;">GitHub Spec Kit</a>';
      html += ' &middot; Spec-Driven Development';
      html += '</span></div>';

      html += '</div>';

      /* Inject keyframes if not already present */
      if (!document.getElementById('inception-progress-styles')) {
        var styleEl = document.createElement('style');
        styleEl.id = 'inception-progress-styles';
        styleEl.textContent = '@keyframes inceptionPulse{0%{transform:translateX(0)}50%{transform:translateX(233%)}100%{transform:translateX(0)}}';
        document.head.appendChild(styleEl);
      }

      if (_inceptionState && _inceptionState.idea_text) {
        var ideaText = escapeHtml(_inceptionState.idea_text);
        html += '<div style="';
        html += 'margin-top:var(--sp-5);margin-bottom:var(--sp-6);padding:10px 14px;';
        html += 'background:' + (isDark ? '#1c2333' : '#f6f8fa') + ';';
        html += 'border:1px solid ' + COLORS.border + ';';
        html += 'border-radius:var(--r);';
        html += 'font-size:13px;color:' + COLORS.text + ';';
        html += 'line-height:1.4;';
        html += '">';
        html += '<span style="color:' + COLORS.muted + ';font-size:11px;text-transform:uppercase;letter-spacing:0.5px;">Idea</span><br>';
        html += ideaText;
        html += '</div>';
      }

      return html;
    }

    function renderInceptionModeSelect() {
      return `
        <div style="max-width:700px;margin:0 auto 16px">${emptyStateCard('💡', 'Project Inception — bootstrap a project with your hive',
          'Inception takes a project from raw idea to structured knowledge and a scaffolded repo. ' +
          'In greenfield mode a brainstorm agent captures your one-line idea, asks clarifying questions, structures the answers into vision, requirements, and constraints (stored as Knowledge Base facts), then scaffolds the project. ' +
          'In brownfield mode it scans an existing repo and captures its conventions and principles as KB facts instead. Pick a mode below to begin.')}</div>
        <div style="display:flex;gap:var(--sp-6);max-width:700px;margin:0 auto">
          <div data-action="startInceptionGreenfield" style="flex:1;background:var(--card-bg);border:1px solid var(--border);border-radius:8px;padding:var(--sp-8);cursor:pointer;text-align:center;transition:border-color 0.2s" data-mouseover-action="gh20" data-mouseout-action="gh21">
            <div class="fs-3xl mb-4">🌱</div>
            <div style="font-weight:600;margin-bottom:var(--sp-2)">Start from an idea</div>
            <div style="font-size:0.78rem;color:var(--muted)">New project — describe what you want to build and we'll scaffold it</div>
          </div>
          <div data-action="startInceptionBrownfield" style="flex:1;background:var(--card-bg);border:1px solid var(--border);border-radius:8px;padding:var(--sp-8);cursor:pointer;text-align:center;transition:border-color 0.2s" data-mouseover-action="gh20" data-mouseout-action="gh21">
            <div class="fs-3xl mb-4">🏗️</div>
            <div style="font-weight:600;margin-bottom:var(--sp-2)">Add to existing repo</div>
            <div style="font-size:0.78rem;color:var(--muted)">Scan an existing project and capture its principles as KB facts</div>
          </div>
        </div>`;
    }

    function renderInceptionCapture() {
      return `
        <div style="max-width:600px;margin:0 auto">
          <div style="font-size:0.85rem;color:var(--muted);margin-bottom:var(--sp-5)">Describe your project idea in a sentence or two.</div>
          <textarea id="inception-idea" placeholder="A CLI tool that syncs Obsidian vaults to a wiki server..." style="width:100%;min-height:80px;padding:var(--sp-5);border:1px solid var(--border);border-radius:8px;background:var(--card-bg);color:var(--fg);font-family:inherit;font-size:0.85rem;resize:vertical"></textarea>
          <div style="margin-top:var(--sp-5);display:flex;gap:var(--sp-4);justify-content:flex-end">
            <button class="hv-btn btn-danger" data-action="stopInception">Stop</button>
            <button class="hv-btn btn-danger" data-action="clearInception">Clear</button>
            <button class="hv-btn btn-primary" data-action="submitInceptionIdea">Start</button>
          </div>
        </div>`;
    }

    let _inceptionPollTimer = null;
    let _inceptionTailing = true;

    function onInceptionScroll() {
      const el = document.getElementById('inception-activity');
      const btn = document.getElementById('inception-follow-btn');
      if (!el) return;
      const SCROLL_THRESHOLD = 40;
      const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < SCROLL_THRESHOLD;
      _inceptionTailing = atBottom;
      if (btn) btn.classList.toggle('visible', !atBottom);
    }

    function inceptionResumeTail() {
      _inceptionTailing = true;
      const el = document.getElementById('inception-activity');
      if (el) el.scrollTop = el.scrollHeight;
      const btn = document.getElementById('inception-follow-btn');
      if (btn) btn.classList.remove('visible');
    }

    function renderInceptionWorking() {
      const phase = escapeHtml(_inceptionState.phase || 'capture');
      return `
        <div style="max-width:100%;margin:var(--sp-0)">
          <div style="display:flex;align-items:center;gap:var(--sp-4);margin-bottom:var(--sp-5)">
            <div style="width:8px;height:8px;border-radius:50%;background:var(--blue);animation:pulse 1.5s infinite"></div>
            <div style="font-size:var(--fs-base);font-weight:600">Brainstorm agent working</div>
            <div style="font-size:0.72rem;color:var(--muted);margin-left:auto">Phase: ${phase}</div>
          </div>
          <div style="position:relative">
          <div id="inception-activity" style="padding:var(--sp-6);font-size:var(--fs-sm);height:450px;overflow-y:auto;white-space:pre-wrap;resize:vertical;min-height:150px;max-height:calc(100vh - 200px)" data-scroll-action="onInceptionScroll">Waiting for brainstorm agent...</div>
          <button id="inception-follow-btn" class="oc-summary-follow-btn" data-action="inceptionResumeTail" title="Follow new output">⬇</button>
          </div>
          <div style="margin-top:var(--sp-5);display:flex;gap:var(--sp-4);justify-content:flex-end">
            <button class="hv-btn btn-danger" data-action="stopInception">Stop</button>
            <button class="hv-btn btn-danger" data-action="clearInception">Clear</button>
          </div>
        </div>`;
    }

    function startInceptionPoll() {
      stopInceptionPoll();
      _inceptionTailing = true;
      pollInceptionActivity();
      const INCEPTION_POLL_INTERVAL_MS = 3000;
      _inceptionPollTimer = dashboardSetInterval('inception-activity', pollInceptionActivity, INCEPTION_POLL_INTERVAL_MS);
    }

    function stopInceptionPoll() {
      if (_inceptionPollTimer) { dashboardClearInterval('inception-activity'); _inceptionPollTimer = null; }
    }

    function _inceptionStateKey(s) {
      if (!s) return 'inactive';
      return (s.idea_slug || '') + ':' + (s.phase || '');
    }

    async function pollInceptionActivity() {
      try {
        const r = await fetch('/api/pane/brainstorm?lines=500&source=buffer', {headers: _inceptionAuthHeaders()});
        if (!r.ok) return;
        const data = await r.json();
        const lines = data.lines || [];
        const el = document.getElementById('inception-activity');
        if (el) {
          el.textContent = lines.join('\n') || 'Waiting for brainstorm agent...';
          if (_inceptionTailing) el.scrollTop = el.scrollHeight;
        }
      } catch (e) { /* ignore poll errors */ }

      try {
        // Don't clobber the textarea while the user is typing their idea, nor
        // any panel field that is focused or holds unsaved input (#11032);
        // the next poll retries once the user is done.
        if (_inceptionState && _inceptionState.phase === 'capture' && !_inceptionState.idea_text) {
          return;
        }
        if (sectionHoldsUserInput(document.getElementById('inception-panel'), '.campaigns-filters input')) {
          return;
        }
        const sr = await fetch('/api/inception/state', {headers: _inceptionAuthHeaders()});
        const sd = await sr.json();
        if (!sd.ok) return;
        var oldKey = _inceptionStateKey(_inceptionState);
        var newKey = _inceptionStateKey(sd.state);
        if (oldKey !== newKey) {
          _inceptionState = sd.state || {};
          renderInception();
          if (typeof renderKnowledge === 'function') renderKnowledge();
        }
      } catch (e) { /* ignore */ }
    }

    function renderInceptionScanProgress() {
      return `
        <div style="max-width:600px;margin:0 auto;text-align:center;padding:40px 0">
          <div style="font-size:1.2rem;margin-bottom:var(--sp-5)">🔍</div>
          <div style="font-size:0.85rem;color:var(--muted)">Scanning project...</div>
          <div style="font-size:0.78rem;color:var(--muted);margin-top:var(--sp-2)">${escapeHtml(_inceptionState.repo_url || '')}</div>
          <div class="mt-6"><button class="hv-btn btn-danger btn-sm" data-action="resetInception">Cancel</button></div>
        </div>`;
    }

    function renderInceptionClarify() {
      const questions = _inceptionState.questions || [];
      let html = '<div style="max-width:600px;margin:0 auto">';
      html += '<div style="font-size:0.85rem;color:var(--muted);margin-bottom:var(--sp-6)">Answer these questions to refine your project definition.</div>';
      questions.forEach(q => {
        html += `<div class="mb-6">
          <label style="font-size:var(--fs-base);font-weight:600;display:block;margin-bottom:var(--sp-2)">${escapeHtml(q.text)}</label>
          <input type="text" id="inception-q-${q.id}" value="${escapeHtml(q.default || '')}"
            style="width:100%;padding:var(--sp-4) var(--sp-5);border:1px solid var(--border);border-radius:var(--r);background:var(--card-bg);color:var(--fg);font-size:var(--fs-base)" />
          <div style="font-size:0.7rem;color:var(--muted);margin-top:var(--sp-1)">${escapeHtml(q.category || '')}</div>
        </div>`;
      });
      html += `<div style="display:flex;gap:var(--sp-4);justify-content:flex-end">
        <button class="hv-btn btn-danger" data-action="resetInception">Reset</button>
        <button class="hv-btn btn-primary" data-action="submitInceptionAnswers">Submit</button>
      </div>`;
      html += '</div>';
      return html;
    }

    function renderInceptionStructure() {
      return `
        <div style="max-width:100%;margin:var(--sp-0)">
          <div style="display:flex;align-items:center;gap:var(--sp-4);margin-bottom:var(--sp-5)">
            <div style="width:8px;height:8px;border-radius:50%;background:var(--blue);animation:pulse 1.5s infinite"></div>
            <div style="font-size:var(--fs-base);font-weight:600">Brainstorm agent structuring project</div>
            <div style="font-size:0.72rem;color:var(--muted);margin-left:auto">Phase: structure</div>
          </div>
          <div style="position:relative">
          <div id="inception-activity" style="padding:var(--sp-6);font-size:var(--fs-sm);height:450px;overflow-y:auto;white-space:pre-wrap;resize:vertical;min-height:150px;max-height:calc(100vh - 200px)" data-scroll-action="onInceptionScroll">Brainstorm agent is producing vision, constitution, requirements, and acceptance criteria...</div>
          <button id="inception-follow-btn" class="oc-summary-follow-btn" data-action="inceptionResumeTail" title="Follow new output">⬇</button>
          </div>
          <div style="margin-top:var(--sp-5);display:flex;gap:var(--sp-4);justify-content:flex-end">
            <button class="hv-btn btn-danger" data-action="stopInception">Stop</button>
            <button class="hv-btn btn-danger" data-action="clearInception">Clear</button>
          </div>
        </div>`;
    }

    function renderInceptionScaffold() {
      setTimeout(loadInceptionScaffoldSummary, 100);
      return `
        <div style="max-width:700px;margin:0 auto">
          <div style="font-size:0.85rem;color:var(--muted);margin-bottom:var(--sp-5)">Your scaffold is ready. Review the generated files below, then approve to complete.</div>
          <div class="mb-6" id="inception-scaffold-summary"><div style="text-align:center;color:var(--muted);padding:var(--sp-7)">Loading scaffold summary...</div></div>
          <div style="display:flex;gap:var(--sp-4);justify-content:flex-end">
            <button class="hv-btn btn-danger" data-action="resetInception">Start Over</button>
            <button class="hv-btn btn-primary" data-action="approveInception">Approve</button>
          </div>
        </div>`;
    }

    async function loadInceptionScaffoldSummary() {
      const container = document.getElementById('inception-scaffold-summary');
      if (!container) return;
      try {
        const r = await fetch('/api/inception/scaffold', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        if (!data.ok || !data.scaffold || !data.scaffold.files || data.scaffold.files.length === 0) {
          container.innerHTML = '<div class="text-muted">No scaffold files generated.</div>';
          return;
        }
        const files = data.scaffold.files;
        const dirs = new Set();
        const exts = {};
        files.forEach(f => {
          const parts = f.path.split('/');
          if (parts.length > 1) dirs.add(parts[0]);
          const ext = f.path.includes('.') ? f.path.split('.').pop() : 'other';
          exts[ext] = (exts[ext] || 0) + 1;
        });
        let html = '<div style="background:var(--card-bg);border:1px solid var(--border);border-radius:8px;padding:var(--sp-6)">';
        html += '<div style="font-size:0.9rem;font-weight:600;margin-bottom:var(--sp-5)">📦 Scaffold Summary</div>';
        html += '<div style="display:flex;gap:var(--sp-8);flex-wrap:wrap;margin-bottom:var(--sp-5)">';
        html += '<div><span style="font-size:1.2rem;font-weight:700">' + files.length + '</span> <span style="color:var(--muted);font-size:0.8rem">files</span></div>';
        html += '<div><span style="font-size:1.2rem;font-weight:700">' + dirs.size + '</span> <span style="color:var(--muted);font-size:0.8rem">directories</span></div>';
        const totalChars = files.reduce((sum, f) => sum + f.content.length, 0);
        html += '<div><span style="font-size:1.2rem;font-weight:700">' + (totalChars > 1000 ? (totalChars/1000).toFixed(1)+'K' : totalChars) + '</span> <span style="color:var(--muted);font-size:0.8rem">chars</span></div>';
        html += '</div>';
        html += '<div style="display:flex;gap:var(--sp-3);flex-wrap:wrap;margin-bottom:var(--sp-5)">';
        Object.entries(exts).sort((a,b) => b[1]-a[1]).forEach(([ext,count]) => {
          html += '<span style="padding:var(--sp-1) var(--sp-4);background:var(--bg);border:1px solid var(--border);border-radius:var(--r-lg);font-size:0.72rem;color:var(--muted)">.' + escapeHtml(ext) + ' ×' + count + '</span>';
        });
        html += '</div>';
        html += '<details><summary style="cursor:pointer;font-size:0.8rem;color:var(--blue)">View file tree</summary>';
        html += '<div class="mt-4">' + renderFileTree(files) + '</div>';
        html += '</details></div>';
        container.innerHTML = html;
      } catch (e) { container.innerHTML = '<div style="color:var(--error)">Error: ' + escapeHtml(e.message) + '</div>'; }
    }

    function renderInceptionComplete() {
      const factCount = (_inceptionState.fact_slugs || []).length;
      return `
        <div style="max-width:100%;margin:var(--sp-0)">
          <div style="text-align:center;margin-bottom:var(--sp-7)">
            <div style="font-size:1rem;font-weight:600;margin-bottom:var(--sp-2)">Inception Complete</div>
            <div style="font-size:0.85rem;color:var(--muted)">${factCount} knowledge facts created. Your project is ready to grow.</div>
          </div>
          <div class="mb-6" id="inception-complete-files"></div>
          <div style="display:flex;gap:var(--sp-4);justify-content:center">
            <button class="hv-btn btn-danger" data-action="resetInception">New Inception</button>
            <button class="hv-btn btn-primary" data-action="downloadInceptionZip">Download Project</button>
            <button class="hv-btn btn-secondary btn-sm" data-action="loadInceptionCompleteFiles">Refresh Files</button>
          </div>
        </div>`;
    }

    async function loadInceptionCompleteFiles() {
      const container = document.getElementById('inception-complete-files');
      if (!container) return;
      container.innerHTML = '<div style="color:var(--muted);text-align:center;padding:var(--sp-5)">Loading scaffold files...</div>';
      try {
        const r = await fetch('/api/inception/scaffold', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        if (!data.ok || !data.scaffold || !data.scaffold.files || data.scaffold.files.length === 0) {
          container.innerHTML = '<div style="color:var(--muted);text-align:center;padding:var(--sp-5)">No scaffold files generated. The brainstorm agent may not have produced KB facts.</div>';
          return;
        }
        container.innerHTML = renderFileTree(data.scaffold.files);
      } catch (e) {
        container.innerHTML = '<div style="color:var(--muted);text-align:center;padding:var(--sp-5)">Error loading files: ' + escapeHtml(e.message) + '</div>';
      }
    }

    function renderFileTree(files) {
      // Build tree structure from flat paths
      const root = {};
      for (const f of files) {
        const parts = f.path.split('/');
        let node = root;
        for (let i = 0; i < parts.length; i++) {
          const part = parts[i];
          if (i === parts.length - 1) {
            node[part] = { _file: f };
          } else {
            if (!node[part]) node[part] = {};
            node = node[part];
          }
        }
      }

      function renderNode(node, prefix, isLast) {
        let html = '';
        const entries = Object.keys(node).sort((a, b) => {
          const aIsDir = !node[a]._file;
          const bIsDir = !node[b]._file;
          if (aIsDir !== bIsDir) return aIsDir ? -1 : 1;
          return a.localeCompare(b);
        });

        entries.forEach((name, idx) => {
          const isLastEntry = idx === entries.length - 1;
          const connector = isLastEntry ? '└── ' : '├── ';
          const childPrefix = prefix + (isLastEntry ? '    ' : '│   ');
          const entry = node[name];

          if (entry._file) {
            const f = entry._file;
            html += '<details style="margin:var(--sp-0);border:none">';
            html += '<summary style="padding:var(--sp-1) var(--sp-0);cursor:pointer;font-family:var(--font-mono);font-size:0.8rem;list-style:none;display:flex;align-items:center;gap:var(--sp-2)">';
            html += '<span style="color:var(--muted);white-space:pre">' + escapeHtml(prefix + connector) + '</span>';
            html += '<span>📄 ' + escapeHtml(name) + '</span>';
            html += '<span style="font-size:var(--fs-xs);color:var(--muted);margin-left:var(--sp-2)">' + f.content.length + ' chars</span>';
            html += '</summary>';
            html += '<pre style="padding:12px 12px 12px 40px;margin:var(--sp-0);font-size:0.72rem;overflow-x:auto;background:var(--bg);white-space:pre-wrap;border-left:2px solid var(--border);margin-left:' + (prefix.length * 8 + 24) + 'px">' + escapeHtml(f.content) + '</pre>';
            html += '</details>';
          } else {
            html += '<div style="padding:var(--sp-1) var(--sp-0);font-family:var(--font-mono);font-size:0.8rem;display:flex;align-items:center;gap:var(--sp-2)">';
            html += '<span style="color:var(--muted);white-space:pre">' + escapeHtml(prefix + connector) + '</span>';
            html += '<span>📁 <strong>' + escapeHtml(name) + '/</strong></span>';
            html += '</div>';
            html += renderNode(entry, childPrefix, isLastEntry);
          }
        });
        return html;
      }

      let html = '<div style="background:var(--card-bg);border:1px solid var(--border);border-radius:8px;padding:var(--sp-6);font-family:var(--font-mono)">';
      html += '<div style="font-size:var(--fs-base);font-weight:600;margin-bottom:var(--sp-4)">📦 Project Files (' + files.length + ')</div>';
      html += renderNode(root, '', true);
      html += '</div>';
      return html;
    }

    function startInceptionGreenfield() {
      _inceptionState = { phase: 'capture', mode: 'greenfield' };
      renderInception();
    }

    async function startInceptionBrownfield() {
      const url = await hivePrompt({title: 'Add to existing repo', label: 'Enter the GitHub repo URL:', placeholder: 'https://github.com/owner/repo'});
      if (!url) return;
      try {
        const r = await fetch('/api/inception/scan', {
          method: 'POST',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ repo_url: url })
        });
        const data = await r.json();
        if (data.ok) { _inceptionState = data.state; renderInception(); loadCampaigns(); }
        else showToast(data.error || 'Failed to start scan', 'error');
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function submitInceptionIdea() {
      const el = document.getElementById('inception-idea');
      const idea = el ? el.value.trim() : '';
      if (!idea) { showToast('Please describe your idea', 'error'); return; }
      try {
        const r = await fetch('/api/inception/start', {
          method: 'POST',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ idea })
        });
        const data = await r.json();
        if (data.ok) { _inceptionState = data.state; renderInception(); loadCampaigns(); }
        else showToast(data.error || 'Failed to start inception', 'error');
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function submitInceptionAnswers() {
      const questions = _inceptionState.questions || [];
      const answers = {};
      questions.forEach(q => {
        const el = document.getElementById('inception-q-' + q.id);
        if (el) answers[q.id] = el.value.trim();
      });
      const btn = document.querySelector('#inception-panel button[onclick*="submitInceptionAnswers"]');
      if (btn) { btn.disabled = true; btn.textContent = 'Submitting...'; btn.style.opacity = '0.6'; }
      try {
        const r = await fetch('/api/inception/answer', {
          method: 'POST',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ answers })
        });
        const data = await r.json();
        if (data.ok) { _inceptionState = data.state; renderInception(); }
        else { showToast(data.error || 'Failed to submit answers', 'error'); if (btn) { btn.disabled = false; btn.textContent = 'Submit'; btn.style.opacity = '1'; } }
      } catch (e) { showToast('Error: ' + e.message, 'error'); if (btn) { btn.disabled = false; btn.textContent = 'Submit'; btn.style.opacity = '1'; } }
    }

    async function loadInceptionScaffoldPreview() {
      const preview = document.getElementById('inception-scaffold-preview');
      if (!preview) return;
      preview.innerHTML = '<div class="text-muted fs-base">Loading...</div>';
      try {
        const r = await fetch('/api/inception/scaffold', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        if (!data.ok || !data.scaffold) { preview.innerHTML = '<div class="text-muted">No scaffold available yet.</div>'; return; }
        preview.innerHTML = renderFileTree(data.scaffold.files || []);
      } catch (e) { preview.innerHTML = '<div style="color:var(--error)">Error: ' + escapeHtml(e.message) + '</div>'; }
    }

    async function loadInceptionIdeationFacts() {
      try {
        const r = await fetch('/api/inception/ideation-facts', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        if (data.ok && data.facts && data.facts.length > 0) {
          let msg = 'Ideation Facts (' + data.facts.length + '):\n\n';
          data.facts.forEach(f => { msg += '- [' + f.type + '] ' + f.title + '\n'; });
          await hiveAlert(msg, 'Ideation Facts');
        } else {
          showToast('No ideation facts yet. The brainstorm agent is still working.', 'info');
        }
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function approveInception() {
      try {
        const r = await fetch('/api/inception/approve', { method: 'POST', headers: _inceptionAuthHeaders() });
        const data = await r.json();
        if (data.ok) { _inceptionState.phase = 'complete'; renderInception(); }
        else showToast(data.error || 'Failed to approve', 'error');
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function stopInception() {
      stopInceptionPoll();
      try {
        await fetch('/api/pause/brainstorm', { method: 'POST' });
      } catch (e) { /* ignore */ }
      await loadInceptionState();
    }

    async function clearInception() {
      stopInceptionPoll();
      try {
        const hasFilesResp = await fetch('/api/inception/has-files', {headers: _inceptionAuthHeaders()});
        const hasFilesData = await hasFilesResp.json();
        if (hasFilesData.has_files) {
          const choice = await hiveConfirm({title: 'Download scaffold?', message: 'Download existing scaffold before clearing?', confirmLabel: 'Download'});
          if (choice) {
            await downloadInceptionZip();
          }
        }
        await fetch('/api/inception/reset', { method: 'POST', headers: _inceptionAuthHeaders() });
        _inceptionState = null;
        renderInception();
        loadCampaigns();
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function resetInception() {
      await clearInception();
    }

    async function downloadInceptionZip() {
      try {
        const r = await fetch('/api/inception/download', {headers: _inceptionAuthHeaders()});
        if (!r.ok) { showToast('Download failed', 'error'); return; }
        hiveBinaryExportNotice('Inception project ZIP', 'Inception scaffold ZIP files are binary archives.');
      } catch (e) { showToast('Download error: ' + e.message, 'error'); }
    }

    // Load inception state on section navigate
    const _origOcNavigate = typeof ocNavigate === 'function' ? ocNavigate : null;
    document.addEventListener('DOMContentLoaded', () => {
      loadInceptionState();
      applySectionCollapse('advisory-digest-section');
      /* OpenRouter scan-to-fund return: show the result and clear the flag so a
         reload doesn't re-toast. */
      try {
        const p = new URLSearchParams(window.location.search);
        const or = p.get('openrouter');
        if (or === 'connected') {
          showToast('OpenRouter connected — the "openrouter" gateway is now configured', 'success');
        } else if (or === 'error') {
          showToast('OpenRouter connection failed — please try funding again', 'error');
        }
        if (or) {
          p.delete('openrouter');
          const qs = p.toString();
          history.replaceState(null, '', window.location.pathname + (qs ? '?' + qs : ''));
        }
      } catch (e) { /* non-fatal */ }
    });

    function navbarHealthGroups(data) {
      const failing = [];
      const warning = [];
      const passing = [];
      const deepChecks = Array.isArray((data.deepHealth || {}).checks) ? data.deepHealth.checks : [];
      const deepFailNames = [];
      for (const c of deepChecks) {
        let line = c.detail ? `${c.name}: ${c.detail}` : c.name;
        if (c.onboarding) line += ' — new hive? open the 🧭 Guide (top navbar) → step ‘' + (ONBOARDING_GUIDE_STEP_TITLES[c.guideStep] || ONBOARDING_GUIDE_STEP_TITLES['github-app']) + '’';
        if (c.status === 'fail') { deepFailNames.push(c.name); failing.push(line); }
        else if (c.status === 'warn') warning.push(line);
        else passing.push(line);
      }
      return { failing, warning, passing, deepFailNames };
    }

    function navbarHealthTitle(data) {
      const groups = navbarHealthGroups(data || {});
      const hoverLines = [];
      if (groups.failing.length) hoverLines.push('FAILING:\n' + groups.failing.map(f => '  ✗ ' + f).join('\n'));
      if (groups.warning.length) hoverLines.push('WARNINGS:\n' + groups.warning.map(wl => '  ⚠ ' + wl).join('\n'));
      if (groups.passing.length) hoverLines.push('PASSING:\n' + groups.passing.map(p => '  ✓ ' + p).join('\n'));
      return hoverLines.join('\n\n') || 'No health data';
    }

    function renderNavbarHealthBadge(ocHealth, data) {
      if (!ocHealth) return;
      const groups = navbarHealthGroups(data || {});
      ocHealth.title = navbarHealthTitle(data || {});
      if (groups.deepFailNames.length > 0) {
        // Any failing deep check ⇒ the spoke itself is degraded — same
        // verdict the hub renders for this hive's row. Repo workflow stats are
        // intentionally excluded: they are not this hive's runtime health.
        ocHealth.dataset.status = 'error';
        ocHealth.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">Degraded</span>';
      } else if (groups.failing.length > 0) {
        ocHealth.dataset.status = 'error';
        ocHealth.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">' + groups.failing.length + ' Issue' + (groups.failing.length > 1 ? 's' : '') + '</span>';
      } else {
        ocHealth.dataset.status = 'ok';
        ocHealth.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">Health OK</span>';
      }
      ocHealth.style.color = '';
      ocHealth.style.background = '';
      ocHealth.style.borderColor = '';
    }

    // Providers strip (ADR-0024): per-provider auth vs availability, two
    // separate dimensions so "needs login" never reads as "provider down".
    // Hidden while every provider is usable and available.
    function renderProviderStrip(el, providers) {
      if (!el) return;
      const bad = (providers || []).filter(p => p && (p.auth_state !== 'usable' || p.availability_state !== 'available'));
      if (!bad.length) {
        el.hidden = true;
        el.textContent = '';
        el.title = 'AI providers: all usable';
        el.setAttribute('aria-label', 'AI providers: all usable');
        return;
      }
      const severe = bad.some(p => p.auth_state === 'needs-login' || p.auth_state === 'store-inaccessible' ||
        p.availability_state === 'unavailable' || p.availability_state === 'quota-exhausted');
      el.dataset.status = severe ? 'error' : 'warn';
      el.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">Providers: ' +
        escapeHtml(bad.map(p => p.provider).join(', ')) + '</span>';
      el.title = bad.map(p => p.provider + ': auth ' + p.auth_state + ', availability ' + p.availability_state +
        (p.next_action ? ' — ' + p.next_action : '')).join('\n');
      el.setAttribute('aria-label', 'AI providers needing attention: ' + bad.map(p => p.provider).join(', '));
      el.hidden = false;
    }

    function qualityDiagnosticsAgent(agents) {
      return (agents || []).find(a => a && a.name === 'quality') || null;
    }

    function renderQualityStatValue(stat, agentName) {
      const v = resolveStatValue(stat, agentName);
      const style = stat.style || 'number';
      const hasValue = v !== undefined && v !== null && v !== '';
      const icon = stat.icon ? escapeHtml(stat.icon) + ' ' : '';

      if (style === 'dot') {
        const skip = v === -1;
        const ok = v === true || v === 1;
        const dot = skip ? '⊘' : ok ? '✓' : hasValue ? '✗' : '—';
        const label = skip ? 'skipped' : ok ? 'ok' : hasValue ? 'fail' : 'unknown';
        const cls = skip ? 'skip' : ok ? 'ok' : hasValue ? 'fail' : 'unknown';
        return `<span class="quality-stat-value ${cls}">${dot} ${label}</span>`;
      }

      if (style === 'pct') {
        const PCT_GOOD = 90;
        const PCT_WARN = 70;
        const cls = !hasValue ? 'unknown' : Number(v) >= PCT_GOOD ? 'good' : Number(v) >= PCT_WARN ? 'warn' : 'bad';
        return `<span class="health-ci ${cls}">${hasValue ? escapeHtml(String(v)) + '%' : '—'}</span>`;
      }

      if (style === 'pct-bar') {
        const numericTarget = typeof stat.target === 'number' ? stat.target : 100;
        const pct = hasValue ? Number(v) : 0;
        const TARGET_WARN_OFFSET = 10;
        const covCls = !hasValue ? '' : pct >= numericTarget ? 'ind-ok' : pct >= numericTarget - TARGET_WARN_OFFSET ? 'ind-warn' : 'ind-err';
        return `<span class="ind-num ${covCls}">${hasValue ? escapeHtml(String(v)) + '%' : '—'}</span> <span class="ind-dlabel">goal: ${numericTarget}%</span>`;
      }

      if (style === 'spark') {
        const statKey = stat.key || stat.field;
        const trendField = stat.trendField || stat.field;
        const color = SPARK_COLORS[trendField] || DEFAULT_SPARK_COLOR;
        const histSeries = historyData.map(s => s.agentStats?.[agentName]?.[statKey] ?? 0);
        const hasHistData = histSeries.some(v => v > 0);
        const trendSeries = hasHistData ? histSeries : (window._trendData || []).map(s => s[trendField] || 0);
        const trendTimes = hasHistData ? getHistoryTimes() : (window._trendData || []).map(s => s.t ?? s.timestamp);
        return `<span class="ind-num">${icon}${hasValue ? escapeHtml(String(v)) : '0'}</span> ${sparkSvg(trendSeries, color, trendTimes, `agent-stat:${agentName}:${statKey}`)}`;
      }

      return `<span class="ind-num">${icon}${hasValue ? escapeHtml(String(v)) : '0'}</span>`;
    }

    function renderQualityStatsCard(agents) {
      const quality = qualityDiagnosticsAgent(agents);
      const stats = (quality && Array.isArray(quality.statsConfig)) ? quality.statsConfig : [];
      const emptyAction = '<button class="hv-btn btn-secondary btn-sm" data-action="openConfigDialog" data-config-type="agent" data-agent="quality" data-tab="Stats">Configure quality stats</button>';
      if (!quality || stats.length === 0) {
        return `<div class="quality-stat-card"><div class="quality-stat-title">Quality stats</div><div class="quality-stat-empty">No stats configured — add them under quality → Configuration → Stats.</div>${emptyAction}</div>`;
      }
      const rows = stats.map(stat => {
        const labelText = stat.label || stat.key || stat.field || 'Stat';
        const icon = stat.icon ? `${escapeHtml(stat.icon)} ` : '';
        const desc = stat.desc ? ` title="${escapeHtml(stat.desc)}"` : '';
        const href = statTargetHref(stat);
        const labelHtml = href
          ? `<a href="${escapeHtml(href)}" target="_blank" rel="noopener"${desc}>${icon}${escapeHtml(labelText)}</a>`
          : `<span${desc}>${icon}${escapeHtml(labelText)}</span>`;
        return `<div class="quality-stat-row">${labelHtml}<span>${renderQualityStatValue(stat, quality.name)}</span></div>`;
      }).join('');
      return `<div class="quality-stat-card"><div class="quality-stat-title">Quality stats</div>${rows}</div>`;
    }

