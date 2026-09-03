package hub

const dashboardHTMLModalScripts = `    function orfStartPolling(hiveId) {
      if (_orFundPoll) clearInterval(_orFundPoll);
      var ORF_POLL_MS = 4000;
      _orFundPoll = setInterval(function() { orfCheck(hiveId); }, ORF_POLL_MS);
    }
    async function orfCheck(hiveId) {
      var status = document.getElementById('orf-status');
      try {
        var res = await fetch('/api/openrouter/credit?hive_id=' + encodeURIComponent(hiveId));
        if (!res.ok) return;
        var d = await res.json();
        // pending_delivery flips true the moment the fund completes on the hub;
        // it then flips back to false once the hive drains it on a heartbeat.
        if (d.pending_delivery) {
          if (status) status.innerHTML = '<span style="color:var(--green,#3fb950);font-weight:600">✓ Funded — delivering to the hive on its next heartbeat…</span>';
        }
      } catch (e) { /* keep polling */ }
    }
    function closeOrFundModal() {
      if (_orFundPoll) { clearInterval(_orFundPoll); _orFundPoll = null; }
      var ov = document.getElementById('orf-overlay');
      if (ov) ov.remove();
    }

    async function removeLocalHive(id) {
      if (!await hiveConfirm('Remove ' + id + ' from the registry? The hive itself is not affected — it will reappear if it sends another heartbeat.')) return;
      try {
        var resp = await fetch('/api/hub/registry/' + encodeURIComponent(id), {method: 'DELETE'});
        if (!resp.ok) { var d = await resp.json(); hiveToast(d.error || 'Remove failed', 'error'); return; }
        hiveToast('Removed ' + id + ' from registry', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    function openConvert(btn) {
      document.getElementById('f-org').value = btn.dataset.org || '';
      document.getElementById('f-repos').value = btn.dataset.repos || '';
      document.getElementById('f-primary').value = btn.dataset.primary || '';
      document.getElementById('f-name').value = btn.dataset.name || '';
      document.getElementById('f-level').value = btn.dataset.level || '1';
      document.getElementById('create-modal').style.display = 'flex';
      var dashUrl = (btn.dataset.dashUrl || '').replace(/\/$/, '');
      var dlLink = document.getElementById('yaml-download-link');
      var dlHref = document.getElementById('yaml-download-href');
      if (dashUrl && dlLink && dlHref) {
        dlHref.href = dashUrl + '/api/config/download';
        dlLink.style.display = '';
      } else if (dlLink) {
        dlLink.style.display = 'none';
      }
    }

    var _createInProgress = false;
    async function createHive() {
      if (_createInProgress) return;
      _createInProgress = true;
      document.getElementById('btn-go').disabled = true;
      document.getElementById('btn-go').textContent = 'Provisioning...';
      var org = document.getElementById('f-org').value.trim();
      var repos = document.getElementById('f-repos').value.trim();
      var primary = document.getElementById('f-primary').value.trim();
      var name = document.getElementById('f-name').value.trim();
      var level = parseInt(document.getElementById('f-level').value) || 1;
      var clusterSel = document.getElementById('f-cluster');
      var clusterId = clusterSel ? clusterSel.value : '';
      var method = document.querySelector('input[name="auth-method"]:checked').value;
      var token = document.getElementById('f-token').value.trim();
      var appId = (document.getElementById('f-app-id') || {}).value || '';
      var installId = (document.getElementById('f-install-id') || {}).value || '';
      var appKey = (document.getElementById('f-app-key') || {}).value || '';

      gtag('event','hive_create_started',{org:org,primary_repo:primary,acmm_level:level,cluster_id:clusterId});
      if (!org || !repos) { hiveToast('Org and repos are required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'pat' && !token) { hiveToast('GitHub token is required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'app' && (!appId || !installId || !appKey)) { hiveToast('App ID, Installation ID, and Private Key are required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'later') { method = 'app'; appId = '3568013'; installId = ''; appKey = ''; }

      try {
        var body = {org: org, repos: repos, primary_repo: primary || repos.split(',')[0].trim(), project_name: name, acmm_level: level, cluster_id: clusterId, auth_method: method, is_public: document.getElementById('f-public').checked};
        if (method === 'pat') body.github_token = token;
        else { body.app_id = appId.trim(); body.installation_id = installId.trim(); body.app_private_key = appKey.trim(); }

        var resp = await fetch('/api/saas/hives', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(body)
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to create hive', 'error'); return; }

        document.getElementById('create-modal').style.display = 'none';
        document.getElementById('btn-go').disabled = false;
        document.getElementById('btn-go').textContent = 'Go';

        hiveToast('Hive ' + data.id + ' is provisioning!', 'success');
        loadHives();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
      } finally {
        _createInProgress = false;
        document.getElementById('btn-go').disabled = false;
        document.getElementById('btn-go').textContent = 'Go';
      }
    }

    function parseHiveYaml(text) {
      var cfg = {};
      var lines = text.split('\n');
      var section = '';
      for (var i = 0; i < lines.length; i++) {
        var line = lines[i];
        var trimmed = line.replace(/\s+$/, '');
        if (/^project:/.test(trimmed)) { section = 'project'; continue; }
        if (/^github:/.test(trimmed)) { section = 'github'; continue; }
        if (/^governor:/.test(trimmed)) { section = 'governor'; continue; }
        if (/^\S/.test(trimmed) && /:/.test(trimmed)) { section = ''; continue; }
        if (section === 'project') {
          var m;
          if ((m = trimmed.match(/^\s+org:\s*(.+)/))) cfg.org = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+repos:\s*$/))) { cfg.repos = []; for (var j = i + 1; j < lines.length && /^\s+-\s/.test(lines[j]); j++) { cfg.repos.push(lines[j].replace(/^\s+-\s*/, '').trim().replace(/^["']|["']$/g, '')); } }
          if ((m = trimmed.match(/^\s+repos:\s*\[(.+)\]/))) cfg.repos = m[1].split(',').map(function(r) { return r.trim().replace(/^["']|["']$/g, ''); });
          if ((m = trimmed.match(/^\s+primary_repo:\s*(.+)/))) cfg.primary = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+name:\s*(.+)/))) cfg.name = m[1].trim().replace(/^["']|["']$/g, '');
        }
        if (section === 'github') {
          var m;
          if ((m = trimmed.match(/^\s+token:\s*(.+)/))) cfg.token = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+app_id:\s*(\d+)/))) cfg.appId = m[1];
          if ((m = trimmed.match(/^\s+installation_id:\s*(\d+)/)) && !trimmed.match(/docs_installation_id/)) cfg.installId = m[1];
        }
        if (section === 'governor') {
          var m;
          if ((m = trimmed.match(/^\s+acmm_level:\s*(\d+)/))) cfg.level = parseInt(m[1]);
        }
      }
      return cfg;
    }

    function applyYamlConfig(cfg) {
      if (cfg.org) document.getElementById('f-org').value = cfg.org;
      if (cfg.repos) document.getElementById('f-repos').value = cfg.repos.join(', ');
      if (cfg.primary) document.getElementById('f-primary').value = cfg.primary;
      if (cfg.name) document.getElementById('f-name').value = cfg.name;
      if (cfg.level) document.getElementById('f-level').value = cfg.level;
      if (cfg.appId) {
        document.querySelector('input[name="auth-method"][value="app"]').checked = true;
        document.getElementById('auth-pat').style.display = 'none';
        document.getElementById('auth-app').style.display = '';
        document.getElementById('f-app-id').value = cfg.appId;
        if (cfg.installId) document.getElementById('f-install-id').value = cfg.installId;
      } else if (cfg.token) {
        document.getElementById('f-token').value = cfg.token;
      }
      var drop = document.getElementById('yaml-drop');
      drop.innerHTML = '<div style="font-size:0.82rem;color:var(--green)">✓ Config loaded</div>';
    }

    function readYamlFile(file) {
      var reader = new FileReader();
      reader.onload = function() {
        var cfg = parseHiveYaml(reader.result);
        applyYamlConfig(cfg);
        hiveToast('Config loaded from ' + file.name, 'success');
      };
      reader.readAsText(file);
    }
  </script>

  <div id="banner-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:540px;width:90%;max-height:90vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 16px;margin:0;color:var(--accent);flex-shrink:0">Send Hub Banner</h2>
      <div style="flex:1;overflow-y:auto;padding:0 32px">
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Message *</label>
          <div style="display:flex;gap:6px;margin-bottom:6px">
            <button type="button" onclick="bannerFmtBold()" title="Bold (**text**)" style="padding:3px 10px;background:var(--bg);border:1px solid var(--border);border-radius:5px;color:var(--text);cursor:pointer;font-weight:700;font-size:0.8rem">B</button>
            <button type="button" onclick="bannerFmtLink()" title="Insert link ([text](url))" style="padding:3px 10px;background:var(--bg);border:1px solid var(--border);border-radius:5px;color:var(--text);cursor:pointer;font-size:0.8rem">🔗 Link</button>
          </div>
          <textarea id="banner-message" rows="3" maxlength="500" oninput="updateBannerPreview()" placeholder="Announce a new capability... — use the buttons above for bold and links" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;resize:vertical;font-family:inherit"></textarea>
          <div style="font-size:0.7rem;color:var(--muted);display:flex;justify-content:space-between;margin-top:2px"><span>Markdown: <code>**bold**</code>, <code>[text](https://url)</code></span><span><span id="banner-char-count">0</span>/500</span></div>
          <div style="font-size:0.7rem;color:var(--muted);margin:8px 0 3px">Preview</div>
          <div id="banner-preview" style="padding:10px 14px;border:1px dashed var(--border);border-radius:6px;font-size:0.85rem;min-height:1.4em;color:var(--text)"></div>
        </div>
        <div style="margin-bottom:16px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:8px">Color</label>
          <div id="banner-color-picker" style="display:flex;gap:8px;flex-wrap:wrap">
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(22,163,74,0.12)">
              <input type="radio" name="banner-color" value="green" checked style="accent-color:#4ade80"> <span style="color:#4ade80;font-size:0.82rem">Green</span>
            </label>
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(59,130,246,0.12)">
              <input type="radio" name="banner-color" value="blue" style="accent-color:#93c5fd"> <span style="color:#93c5fd;font-size:0.82rem">Blue</span>
            </label>
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(245,158,11,0.12)">
              <input type="radio" name="banner-color" value="amber" style="accent-color:#fcd34d"> <span style="color:#fcd34d;font-size:0.82rem">Amber</span>
            </label>
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(107,114,128,0.12)">
              <input type="radio" name="banner-color" value="gray" style="accent-color:#d1d5db"> <span style="color:#d1d5db;font-size:0.82rem">Gray</span>
            </label>
          </div>
        </div>
        <div style="margin-bottom:16px">
          <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:8px">
            <label style="font-size:0.8rem;color:var(--muted)">Target Hives *</label>
            <div style="display:flex;gap:8px">
              <button onclick="toggleAllBannerHives(true)" style="font-size:0.72rem;color:var(--accent);background:none;border:none;cursor:pointer;text-decoration:underline">Select All</button>
              <button onclick="toggleAllBannerHives(false)" style="font-size:0.72rem;color:var(--muted);background:none;border:none;cursor:pointer;text-decoration:underline">Deselect All</button>
            </div>
          </div>
          <div id="banner-hive-list" style="max-height:200px;overflow-y:auto;border:1px solid var(--border);border-radius:6px;background:var(--bg);padding:4px"></div>
        </div>
      </div>
      <div style="display:flex;justify-content:flex-end;gap:8px;padding:16px 32px 32px;flex-shrink:0">
        <button onclick="document.getElementById('banner-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>
        <button onclick="sendHubBanner()" class="btn-primary" id="btn-send-banner">Send Banner</button>
      </div>
    </div>
  </div>

  <div id="create-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:640px;width:90%;max-height:90vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 16px;margin:0;color:var(--accent);flex-shrink:0">Create Hosted Hive</h2>
      <div style="flex:1;overflow-y:auto;padding:0 32px">
      <div id="yaml-drop" style="margin-bottom:16px;border:2px dashed var(--border);border-radius:8px;padding:16px;text-align:center;cursor:pointer;transition:border-color 0.2s"
        ondragover="event.preventDefault();this.style.borderColor='var(--accent)'"
        ondragleave="this.style.borderColor='var(--border)'"
        ondrop="event.preventDefault();this.style.borderColor='var(--border)';var f=event.dataTransfer.files[0];if(f)readYamlFile(f)"
        onclick="document.getElementById('yaml-upload').click()">
        <div style="font-size:0.82rem;color:var(--muted)">Drop a <code>hive.yaml</code> here or <span style="color:var(--accent);text-decoration:underline">browse</span></div>
        <div style="font-size:0.7rem;color:var(--muted);margin-top:4px">Auto-fills all fields including GitHub App credentials</div>
        <div id="yaml-download-link" style="display:none;font-size:0.7rem;margin-top:6px"><a id="yaml-download-href" href="#" target="_blank" style="color:var(--accent)" onclick="event.stopPropagation()">⬇ Download hive.yaml from your local hive</a></div>
        <input type="file" id="yaml-upload" accept=".yaml,.yml" style="display:none" onchange="if(this.files[0])readYamlFile(this.files[0])">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">GitHub Organization *</label>
        <input id="f-org" type="text" placeholder="my-org" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Repositories * <span style="font-size:0.7rem">(comma-separated)</span></label>
        <input id="f-repos" type="text" placeholder="repo1, repo2" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Primary Repository</label>
        <input id="f-primary" type="text" placeholder="defaults to first repo" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Project Name</label>
        <input id="f-name" type="text" placeholder="defaults to org/repo" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="display:flex;gap:12px;margin-bottom:12px">
        <div style="flex:1">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">ACMM Level</label>
          <select id="f-level" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <option value="1">L1 — Inception (Assisted)</option>
            <option value="2">L2 — Advisory (Instructed)</option>
            <option value="3" selected>L3 — Quality-Gated (Measured)</option>
            <option value="4">L4 — Security-Aware (Adaptive)</option>
            <option value="5">L5 — Semi-Autonomous (Semi-Automated)</option>
            <option value="6">L6 — Fully Autonomous</option>
          </select>
        </div>
        <div style="flex:1">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Target Cluster</label>
          <select id="f-cluster" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <option value="">Loading clusters...</option>
          </select>
        </div>
      </div>
      <div style="margin-bottom:12px">
        <label style="display:flex;align-items:center;gap:8px;cursor:pointer;font-size:0.8rem;color:var(--muted)"><input type="checkbox" id="f-public" checked> Publicly visible in the hive registry <span style="font-size:0.7rem">(owners can toggle later from My Hives)</span></label>
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Authentication Method</label>
        <div style="display:flex;gap:12px;margin-top:4px;flex-wrap:wrap">
          <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:0.8rem"><input type="radio" name="auth-method" value="pat" checked onchange="document.getElementById('auth-pat').style.display='';document.getElementById('auth-app').style.display='none';document.getElementById('auth-later').style.display='none';document.getElementById('auth-info-pat').style.display='';document.getElementById('auth-info-app').style.display='none';document.getElementById('auth-info-later').style.display='none'"> Personal Access Token</label>
          <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:0.8rem"><input type="radio" name="auth-method" value="app" onchange="document.getElementById('auth-pat').style.display='none';document.getElementById('auth-app').style.display='';document.getElementById('auth-later').style.display='none';document.getElementById('auth-info-pat').style.display='none';document.getElementById('auth-info-app').style.display='';document.getElementById('auth-info-later').style.display='none'"> GitHub App <span style="font-size:0.65rem;color:#3fb950;font-weight:600">Recommended</span></label>
          <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:0.8rem"><input type="radio" name="auth-method" value="later" onchange="document.getElementById('auth-pat').style.display='none';document.getElementById('auth-app').style.display='none';document.getElementById('auth-later').style.display='';document.getElementById('auth-info-pat').style.display='none';document.getElementById('auth-info-app').style.display='none';document.getElementById('auth-info-later').style.display=''"> Configure Later</label>
        </div>
        <div id="auth-info-pat" style="font-size:0.7rem;color:var(--muted);margin-top:8px;line-height:1.5;padding:8px 10px;background:rgba(255,255,255,0.03);border-radius:6px;border:1px solid var(--border)">
          The hive uses this token for all GitHub API calls — creating issues, posting advisory comments, reading repos, pushing code, and merging PRs. All actions appear as the token owner. Permissions cannot be scoped per agent trust tier.
        </div>
        <div id="auth-info-app" style="display:none;font-size:0.7rem;color:var(--muted);margin-top:8px;line-height:1.5;padding:8px 10px;background:rgba(255,255,255,0.03);border-radius:6px;border:1px solid var(--border)">
          The hive generates short-lived installation tokens scoped to each agent's trust tier — newcomers get issues-only access, contributors get code + PR access, and trusted agents can merge. Actions appear as the app, not a personal account. Requires a <a id="auth-info-app-link" href="" target="_blank" rel="noopener" style="color:var(--accent)">GitHub App</a> installed on the target org/repo.
        </div>
        <div id="auth-info-later" style="display:none;font-size:0.7rem;color:var(--muted);margin-top:8px;line-height:1.5;padding:8px 10px;background:rgba(255,255,255,0.03);border-radius:6px;border:1px solid var(--border)">
          The hive will be provisioned with the <strong>kubestellar-hive</strong> GitHub App pre-configured (App ID: 3568013). Agents will be unable to access GitHub until the app is installed on the target org and the installation ID is supplied via the hive config.<br><br>
          <a id="auth-info-later-link" href="" target="_blank" rel="noopener" style="color:var(--accent);font-weight:600">→ Install app now</a>
        </div>
      </div>
      <div id="auth-pat">
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">GitHub Token *</label>
          <input id="f-token" type="password" placeholder="ghp_xxxxxxxxxxxx" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
          <div style="font-size:0.7rem;color:var(--muted);margin-top:6px;line-height:1.5">
            Create a <a href="https://github.com/settings/tokens?type=beta" target="_blank">Fine-grained PAT</a>: Contents, Issues, Pull requests (read/write), Metadata (read).<br>
            Classic tokens (<code>ghp_</code>) work with <code>repo</code> scope.
          </div>
        </div>
      </div>
      <div id="auth-app" style="display:none">
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">App ID *</label>
          <input id="f-app-id" type="text" placeholder="123456" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        </div>
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Installation ID *</label>
          <input id="f-install-id" type="text" placeholder="78901234" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        </div>
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Private Key (PEM) *</label>
          <textarea id="f-app-key" rows="6" placeholder="-----BEGIN RSA PRIVATE KEY-----&#10;Paste or drag a .pem file here...&#10;-----END RSA PRIVATE KEY-----" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.8rem;font-family:monospace;resize:vertical" ondragover="event.preventDefault();this.style.borderColor='var(--accent)'" ondragleave="this.style.borderColor='var(--border)'" ondrop="event.preventDefault();this.style.borderColor='var(--border)';var f=event.dataTransfer.files[0];if(f){var r=new FileReader();r.onload=function(){document.getElementById('f-app-key').value=r.result};r.readAsText(f)}"></textarea>
          <div style="font-size:0.7rem;color:var(--muted);margin-top:4px">Download from your <a href="https://github.com/settings/apps" target="_blank">GitHub App settings</a> → Private keys.</div>
        </div>
      </div>
      <div id="auth-later" style="display:none">
        <div style="margin-bottom:12px;padding:12px;background:rgba(59,130,246,0.08);border:1px solid rgba(59,130,246,0.2);border-radius:8px">
          <div style="font-size:0.85rem;font-weight:600;color:var(--text);margin-bottom:8px">Hive App: kubestellar-hive</div>
          <div style="font-size:0.75rem;color:var(--muted);line-height:1.5">App ID: <code>3568013</code> (pre-configured)<br>The hive will start without GitHub access. Install the app on the target org, then supply the Installation ID and Private Key via the hive config.</div>
          <div id="auth-later-ghe-note" style="display:none;font-size:0.75rem;color:var(--muted);line-height:1.5;margin-top:8px"></div>
          <a id="auth-later-install-link" href="" target="_blank" rel="noopener" style="display:inline-block;margin-top:8px;padding:6px 14px;background:var(--accent);color:#fff;border-radius:6px;font-size:0.8rem;font-weight:600;text-decoration:none">Install app on your org</a>
        </div>
      </div>
      </div>
      <div style="display:flex;gap:12px;justify-content:flex-end;padding:16px 32px;border-top:1px solid var(--border);flex-shrink:0">
        <button onclick="document.getElementById('create-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>
        <button id="btn-go" onclick="createHive()" class="btn-primary">Go</button>
      </div>
    </div>
  </div>

  <div id="access-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:860px;width:min(96vw,860px);max-height:88vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 16px;margin:0;color:var(--accent);flex-shrink:0">Manage Access</h2>
      <div style="flex:1;overflow-y:auto;padding:0 32px 32px">
      <p style="font-size:0.8rem;color:var(--muted);margin-bottom:16px" id="access-hive-label"></p>
      <div id="access-list"><div class="loading">Loading...</div></div>
      <div id="access-bulk-bar" style="display:none;align-items:center;gap:8px;margin-top:8px;padding:8px 10px;background:var(--bg);border:1px solid var(--border);border-radius:6px">
        <span id="access-bulk-count" style="font-size:0.75rem;color:var(--muted);flex:1"></span>
        <select id="access-bulk-role" style="padding:4px 8px;background:var(--surface);border:1px solid var(--border);border-radius:4px;color:var(--text);font-size:0.75rem">
          <option value="read">Read</option>
          <option value="read-write">Read-Write</option>
          <option value="merger">Merger</option>
          <option value="owner">Owner</option>
        </select>
        <button onclick="bulkChangeAccessRole()" class="btn-primary" style="padding:4px 10px;font-size:0.7rem">Change Role</button>
        <button onclick="bulkRemoveAccess()" style="padding:4px 10px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">Remove</button>
      </div>
      <div style="margin-top:12px;border-top:1px solid var(--border);padding-top:12px">
        <h3 style="font-size:0.9rem;margin-bottom:8px;color:var(--accent)">Pending Requests</h3>
        <div id="pending-requests"><span style="color:var(--muted);font-size:0.8rem">Loading...</span></div>
      </div>
      <div style="margin-top:12px;border-top:1px solid var(--border);padding-top:12px">
        <h3 style="font-size:0.9rem;margin-bottom:4px;color:var(--accent)">Audit Log</h3>
        <p style="font-size:0.72rem;color:var(--muted);margin:0 0 8px">Every grant, role change and removal — who did it and when. Append-only.</p>
        <div id="access-audit-log" style="max-height:180px;overflow-y:auto"><span style="color:var(--muted);font-size:0.8rem">Loading...</span></div>
      </div>
      <div style="margin-top:16px;border-top:1px solid var(--border);padding-top:16px">
        <h3 style="font-size:0.9rem;margin-bottom:8px;color:var(--text)">Add User</h3>
        <input id="access-user-search" type="text" placeholder="Search users..." autocomplete="off"
          oninput="filterAccessUserDropdown()"
          style="width:100%;box-sizing:border-box;margin-bottom:8px;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        <div style="display:flex;flex-wrap:wrap;gap:8px">
          <select id="access-username" style="flex:1 1 280px;min-width:220px;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem"><option value="">Select user...</option></select>
          <select id="access-role" onchange="updateAccessRoleHint()" title="Permission level to grant" style="flex:0 0 auto;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <option value="read" title="View-only: dashboard, agents and config. Cannot change anything.">Read</option>
            <option value="read-write" title="Everything Read grants, plus contribute: queue work, manage the queue and open a terminal.">Read-Write</option>
            <option value="merger" title="Everything Read-Write grants, plus approve and queue other contributors' work for auto-merge.">Merger</option>
            <option value="owner" title="Full control: manage access, settings and budget for this hive.">Owner</option>
          </select>
          <!-- A checkbox owns whether the new grant expires; the date input is
               HIDDEN until it is checked.
               An empty <input type="date"> cannot express "no expiry" — the
               browser paints its own mm/dd/yyyy placeholder, so an untouched
               field looks like a date the operator already chose. Adding a
               label alone did not fix it: the control then showed a name and a
               date that disagreed. Hiding the input when unchecked leaves
               exactly one visible answer. -->
          <span style="display:flex;align-items:center;gap:6px;flex:0 0 auto">
            <input type="checkbox" id="access-expiry-enabled" onchange="toggleAddExpiryVisible()"
              title="Off — access lasts until it is removed. On — pick the last day of access."
              style="cursor:pointer;margin:0">
            <label for="access-expiry-enabled" style="font-size:0.7rem;color:var(--muted);cursor:pointer;white-space:nowrap">Expires</label>
            <input id="access-expiry" type="date" title="Access is revoked automatically after this date (UTC)."
              style="display:none;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <span id="access-expiry-never" style="font-size:0.7rem;color:var(--text)">Never</span>
          </span>
          <button onclick="addAccess()" class="btn-primary" style="flex:0 0 auto;padding:8px 16px;font-size:0.8rem">Add</button>
        </div>
        <div id="access-role-hint" style="margin-top:6px;font-size:0.72rem;color:var(--muted);line-height:1.4"></div>
      </div>
      </div>
      <div style="display:flex;justify-content:space-between;align-items:center;padding:16px 32px;border-top:1px solid var(--border);flex-shrink:0">
        <button id="access-export-btn" onclick="exportAccessCSV()" title="Download this hive's access list as CSV for audit/compliance" style="padding:8px 16px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);cursor:pointer;font-size:0.8rem">&#11015;&#65039; Export CSV</button>
        <button onclick="document.getElementById('access-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Close</button>
      </div>
    </div>
  </div>

  <div id="timeline-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:640px;width:92%;max-height:82vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 8px;margin:0;color:var(--accent);flex-shrink:0">Activity Timeline</h2>
      <p style="font-size:0.8rem;color:var(--muted);margin:0;padding:0 32px 16px;flex-shrink:0" id="timeline-hive-label"></p>
      <div style="flex:1;overflow-y:auto;padding:0 32px 24px">
        <div id="timeline-list"><div class="loading">Loading...</div></div>
      </div>
      <div style="display:flex;justify-content:flex-end;padding:16px 32px;border-top:1px solid var(--border);flex-shrink:0">
        <button onclick="document.getElementById('timeline-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Close</button>
      </div>
    </div>
  </div>

  <div id="request-access-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:480px;width:90%;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 8px;margin:0;color:var(--accent)">Request Access</h2>
      <div style="padding:0 32px 24px">
        <p style="font-size:0.85rem;color:var(--muted);margin-bottom:12px">Requesting access to <strong id="request-access-hive-label" style="color:var(--text)"></strong>. The owner will review your request.</p>
        <label for="request-access-note" style="display:block;font-size:0.8rem;color:var(--text);margin-bottom:6px">Why do you need access? <span style="color:var(--red)">*</span></label>
        <textarea id="request-access-note" rows="4" placeholder="Explain why you should be granted access to this hive..." style="width:100%;padding:10px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;resize:vertical;box-sizing:border-box"></textarea>
      </div>
      <div style="display:flex;justify-content:flex-end;gap:8px;padding:16px 32px;border-top:1px solid var(--border)">
        <button onclick="closeRequestAccessModal()" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>
        <button id="request-access-submit" onclick="submitRequestAccess()" class="btn-primary" style="padding:8px 20px">Send Request</button>
      </div>
    </div>
  </div>


  <script>
    var _accessHiveId = '';
    // Last-loaded access list for the open manage-access modal; lets
    // removeAccess() detect self-removal and last-owner cases client-side.
    var _accessUsers = [];
    var _timelineHiveId = '';

    /* Per-event presentation: a colour and a short label per event kind, so the
       timeline is scannable rather than a wall of text. Kinds come from the
       Timeline* constants in timeline.go; unknown kinds fall through to a
       neutral default so an older dashboard never renders a blank row. */
    var TIMELINE_KINDS = {
      version_changed:   { label: 'Version',    color: '#3fb950' },
      branch_changed:    { label: 'Branch',     color: '#60a5fa' },
      went_offline:      { label: 'Offline',    color: '#f85149' },
      came_online:       { label: 'Online',     color: '#3fb950' },
      health_changed:    { label: 'Health',     color: '#f59e0b' },
      upgrade_started:   { label: 'Upgrade',    color: '#60a5fa' },
      upgrade_completed: { label: 'Upgraded',   color: '#3fb950' },
      upgrade_stale:     { label: 'Stuck',      color: '#f85149' },
      restarted:         { label: 'Restart',    color: '#f59e0b' },
      acmm_changed:      { label: 'ACMM',       color: '#a371f7' },
      agents_changed:    { label: 'Agents',     color: '#a371f7' },
      github_app_changed:{ label: 'GitHub App', color: '#f59e0b' },
      access:            { label: 'Access',     color: '#60a5fa' },
      ownership:         { label: 'Ownership',  color: '#60a5fa' },
      admin:             { label: 'Admin',      color: '#8b949e' }
    };

    /* Relative time, coarse on purpose: "what happened to this hive" is
       answered by ordering and rough age, not by seconds. */
    var TIMELINE_MS_PER_MIN = 60000;
    var TIMELINE_MIN_PER_HOUR = 60;
    var TIMELINE_HOURS_PER_DAY = 24;
    function timelineAgo(ts) {
      var t = Date.parse(ts);
      if (isNaN(t)) return '';
      var mins = Math.floor((Date.now() - t) / TIMELINE_MS_PER_MIN);
      if (mins < 1) return 'just now';
      if (mins < TIMELINE_MIN_PER_HOUR) return mins + 'm ago';
      var hours = Math.floor(mins / TIMELINE_MIN_PER_HOUR);
      if (hours < TIMELINE_HOURS_PER_DAY) return hours + 'h ago';
      return Math.floor(hours / TIMELINE_HOURS_PER_DAY) + 'd ago';
    }

    async function openTimelineModal(hiveId, hiveName) {
      _timelineHiveId = hiveId;
      document.getElementById('timeline-hive-label').textContent = hiveName || hiveId;
      document.getElementById('timeline-list').innerHTML = '<div class="loading">Loading...</div>';
      document.getElementById('timeline-modal').style.display = 'flex';
      await loadTimeline();
    }

    async function loadTimeline() {
      var el = document.getElementById('timeline-list');
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_timelineHiveId) + '/timeline');
        if (!resp.ok) {
          el.innerHTML = '<div style="color:var(--red);font-size:0.85rem">Could not load this hive\'s timeline.</div>';
          return;
        }
        var data = await resp.json();
        var events = (data && data.events) || [];
        if (events.length === 0) {
          el.innerHTML = '<div style="color:var(--muted);font-size:0.85rem">No recorded activity yet. Events appear here when something actually changes — a version lands, health flips, the hive restarts or goes offline.</div>';
          return;
        }
        /* Server returns newest first; render in that order. */
        el.innerHTML = events.map(function(ev) {
          var kind = TIMELINE_KINDS[ev.kind] || { label: ev.kind || 'Event', color: '#8b949e' };
          /* actorName resolves opaque OIDC identities server-side. */
          var actor = ev.actor
            ? '<span style="color:var(--muted);font-size:0.7rem;margin-left:6px">by ' + esc(ev.actorName || ev.actor) + '</span>'
            : '';
          return '<div style="display:flex;gap:10px;padding:9px 0;border-bottom:1px solid var(--border)">' +
            '<div style="flex-shrink:0;width:84px"><span style="display:inline-block;padding:2px 7px;border-radius:9999px;font-size:0.62rem;font-weight:600;white-space:nowrap;background:' + kind.color + '22;color:' + kind.color + ';border:1px solid ' + kind.color + '55">' + esc(kind.label) + '</span></div>' +
            '<div style="flex:1;min-width:0">' +
              '<div style="font-size:0.82rem;color:var(--text);word-break:break-word">' + esc(ev.detail || '') + actor + '</div>' +
              '<div style="font-size:0.68rem;color:var(--muted);margin-top:2px" title="' + esc(ev.ts || '') + '">' + esc(timelineAgo(ev.ts)) + '</div>' +
            '</div></div>';
        }).join('');
      } catch(e) {
        el.innerHTML = '<div style="color:var(--red);font-size:0.85rem">Failed to load timeline: ' + esc(e.message) + '</div>';
      }
    }

    async function openAccessModal(hiveId, dashUrl) {
      _accessHiveId = hiveId;
      // Show the hive's URL alongside its id. The raw placeholder id
      // (hosted-available-oke-05-placeholder-6q84) says nothing about where the
      // hive actually lives, and the vanity URL is what an owner recognises.
      var label = document.getElementById('access-hive-label');
      label.textContent = 'Hive: ' + hiveId;
      var urlEl = document.getElementById('access-hive-url');
      if (!urlEl) {
        urlEl = document.createElement('div');
        urlEl.id = 'access-hive-url';
        urlEl.style.cssText = 'padding:0 32px 4px;font-size:0.8rem';
        label.parentNode.insertBefore(urlEl, label.nextSibling);
      }
      if (dashUrl && dashUrl.indexOf('localhost') === -1) {
        urlEl.innerHTML = '<a href="' + esc(dashUrl) + '" target="_blank" rel="noopener" style="color:var(--blue);text-decoration:none">' +
          esc(dashUrl.replace(/^https?:\/\//, '')) + '</a>';
        urlEl.style.display = '';
      } else {
        urlEl.textContent = '';
        urlEl.style.display = 'none';
      }
      document.getElementById('access-modal').style.display = 'flex';
      updateAccessRoleHint();
      _bulkAccessSel = {};
      updateBulkAccessBar();
      await loadAccessList();
      await loadAccessUserDropdown();
      await loadPendingRequests();
      await loadAccessAuditLog();
    }

    /* Audit log for permission changes (#4148): a read-only, append-only view
       sourced from the hive timeline, filtered server-side to access/ownership
       events. Only owners can open Manage Access, and the endpoint enforces
       the same rule, so a non-owner never sees this. */
    async function loadAccessAuditLog() {
      var el = document.getElementById('access-audit-log');
      if (!el) return;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access-log');
        if (!resp.ok) {
          el.innerHTML = '<span style="color:var(--muted);font-size:0.8rem">Audit log unavailable</span>';
          return;
        }
        var data = await resp.json();
        var events = (data && data.events) || [];
        if (!events.length) {
          el.innerHTML = '<span style="color:var(--muted);font-size:0.8rem">No permission changes recorded yet</span>';
          return;
        }
        /* Server returns newest first; render in that order. */
        el.innerHTML = events.map(function(ev) {
          /* actorName resolves opaque OIDC identities server-side. */
          var actor = ev.actor
            ? '<span style="color:var(--muted);font-size:0.7rem;margin-left:6px">by ' + esc(ev.actorName || ev.actor) + '</span>'
            : '';
          return '<div style="padding:6px 0;border-bottom:1px solid var(--border)">' +
            '<div style="font-size:0.78rem;color:var(--text);word-break:break-word">' + esc(ev.detail || '') + actor + '</div>' +
            '<div style="font-size:0.66rem;color:var(--muted);margin-top:2px" title="' + esc(ev.ts || '') + '">' + esc(timelineAgo(ev.ts)) + '</div>' +
            '</div>';
        }).join('');
      } catch(e) {
        el.innerHTML = '<span style="color:var(--muted);font-size:0.8rem">Audit log unavailable</span>';
      }
    }

    async function loadPendingRequests() {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests');
        if (!resp.ok) return;
        var data = await resp.json();
        var reqs = data.requests || [];
        var el = document.getElementById('pending-requests');
        if (!el) return;
        if (!reqs.length) { el.innerHTML = '<span style="color:var(--muted);font-size:0.8rem">No pending requests</span>'; return; }
        el.innerHTML = reqs.map(function(r) {
          var avatar = linkedAvatar(r.username, LIST_AVATAR_PX, r.username, 'margin-right:6px');
          var note = (r.note || '').trim();
          var noteHtml = note
            ? '<div style="margin-top:4px;font-size:0.75rem;color:var(--text);white-space:pre-wrap;word-break:break-word;background:var(--bg);border-left:2px solid var(--accent);padding:4px 8px;border-radius:2px">' + esc(note) + '</div>'
            : '<div style="margin-top:4px;font-size:0.72rem;color:var(--muted);font-style:italic">(no note)</div>';
          return '<div style="padding:6px 0;border-bottom:1px solid var(--border)">' +
            '<div style="display:flex;align-items:center;justify-content:space-between">' +
            '<div>' + avatar + '<span style="font-size:0.85rem">' + esc(r.username) + '</span> <span style="font-size:0.7rem;color:var(--muted)">' + esc(r.requested_at.substring(0,10)) + '</span></div>' +
            '<div style="display:flex;gap:4px">' +
            '<select id="req-role-' + esc(r.username) + '" title="Role to grant on approval" style="padding:2px 6px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);font-size:0.7rem"><option value="read" title="' + escAttr(roleDescription('read')) + '">Read</option><option value="read-write" title="' + escAttr(roleDescription('read-write')) + '">Read-Write</option><option value="merger" title="' + escAttr(roleDescription('merger')) + '">Merger</option></select>' +
            '<button onclick="approveRequest(\'' + esc(r.username) + '\')" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Approve</button>' +
            '<button onclick="denyRequest(\'' + esc(r.username) + '\')" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Deny</button>' +
            '</div></div>' + noteHtml + '</div>';
        }).join('');
      } catch(e) {}
    }

    async function approveRequest(username) {
      var role = (document.getElementById('req-role-' + username) || {}).value || 'read';
      try {
        await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests/' + encodeURIComponent(username) + '/approve', {
          method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({role: role})
        });
        loadPendingRequests();
        loadAccessList();
        loadAccessAuditLog();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function denyRequest(username) {
      try {
        await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests/' + encodeURIComponent(username) + '/deny', {method: 'POST'});
        loadPendingRequests();
        loadAccessAuditLog();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    /* ── GitHub display-name enrichment (#4145) ─────────────────────────
       The Manage Access rows and the Add User picker key on the stable login,
       but a human recognizes "Andy Anderson" faster than "clubanderson". The
       GitHub profile API carries that name, so we fetch it lazily and paint it
       in AFTER the username is already on screen — enrichment only ever ADDS
       information, so a failed/rate-limited lookup leaves the UI exactly as it
       was (graceful fallback to username, no blocking delay).

       Results are cached twice: in-memory for this page, and in localStorage
       for a day so re-opening the dialog doesn't re-spend the unauthenticated
       api.github.com rate budget (60 req/hr/IP). Confirmed misses (404) are
       cached like hits; transient failures (rate limit, network) are cached
       for this page only so the next page load can retry. */
    var GH_NAME_CACHE_KEY = 'hiveGhDisplayNames';
    var GH_NAME_CACHE_TTL_MS = 24 * 60 * 60 * 1000;
    var _ghNameMem = {};      /* login(lower) -> name string | null (known-none) */
    var _ghNamePending = {};  /* login(lower) -> in-flight Promise (dedupes) */

    function ghNameCacheRead() {
      try { return JSON.parse(localStorage.getItem(GH_NAME_CACHE_KEY) || '{}') || {}; }
      catch (e) { return {}; }
    }
    function ghNameCacheWrite(login, name) {
      try {
        var c = ghNameCacheRead();
        c[login] = { n: name, t: Date.now() };
        localStorage.setItem(GH_NAME_CACHE_KEY, JSON.stringify(c));
      } catch (e) {}
    }
    /* Only plain github.com logins have a profile to ask about; OIDC identity
       keys ("google:1078…") never do — same ":" rule as splitIdentityKey. */
    function isPlainGitHubLogin(username) {
      var u = String(username || '');
      return u !== '' && u.indexOf(':') === -1;
    }
    /* fetchGitHubDisplayName resolves to the profile name for a github.com
       login, or null when there is none / it cannot be fetched. Never rejects. */
    function fetchGitHubDisplayName(username) {
      var login = String(username || '').toLowerCase();
      if (!isPlainGitHubLogin(login)) return Promise.resolve(null);
      if (login in _ghNameMem) return Promise.resolve(_ghNameMem[login]);
      var cached = ghNameCacheRead()[login];
      if (cached && (Date.now() - (cached.t || 0)) < GH_NAME_CACHE_TTL_MS) {
        _ghNameMem[login] = cached.n || null;
        return Promise.resolve(_ghNameMem[login]);
      }
      if (_ghNamePending[login]) return _ghNamePending[login];
      var p = fetch('https://api.github.com/users/' + encodeURIComponent(login))
        .then(function(r) {
          if (r.status === 404) return null;      /* confirmed no such user: cache the miss */
          if (!r.ok) return undefined;            /* transient (rate limit etc): don't persist */
          return r.json().then(function(d) { return (d && d.name) ? String(d.name) : null; },
                               function() { return undefined; });
        }, function() { return undefined; })
        .then(function(name) {
          delete _ghNamePending[login];
          if (name === undefined) { _ghNameMem[login] = null; return null; }
          _ghNameMem[login] = name;
          ghNameCacheWrite(login, name);
          return name;
        });
      _ghNamePending[login] = p;
      return p;
    }
    /* enrichGhDisplayNames fills every .gh-display-name placeholder under root
       once its profile name arrives. The placeholder is rendered empty next to
       the username, so nothing shifts or blocks while lookups are in flight,
       and a name identical to the login is skipped (it would only repeat). */
    function enrichGhDisplayNames(root) {
      if (!root) return;
      var els = root.querySelectorAll('.gh-display-name[data-gh-login]');
      Array.prototype.forEach.call(els, function(el) {
        var login = el.getAttribute('data-gh-login') || '';
        fetchGitHubDisplayName(login).then(function(name) {
          if (!name || name.toLowerCase() === login.toLowerCase()) return;
          if (!el.isConnected) return; /* the list re-rendered meanwhile */
          el.textContent = name;
        });
      });
    }

    /* The roster fetched from grantable-users, kept so the search box can
       re-filter without another network round trip. Each entry carries the
       stable identity key (id — what the grant POST sends) and the normalized
       human label (label — what the owner reads). */
    var _grantableUsers = [];

    /* enrichGrantableUserLabels upgrades GitHub rows whose label is still the
       bare login with the profile display name, then re-renders the dropdown
       ONCE after the batch settles — preserving the active filter and any
       selection (renderAccessUserOptions already restores both). Rows the hub
       already labeled (OIDC display_name) are left alone. */
    function enrichGrantableUserLabels() {
      var updated = false, waiting = 0;
      _grantableUsers.forEach(function(e) {
        if (!e || e.id !== e.label) return;
        if (e.provider && e.provider !== 'github') return;
        if (!isPlainGitHubLogin(e.id)) return;
        waiting++;
        fetchGitHubDisplayName(e.id).then(function(name) {
          waiting--;
          if (name && name !== e.label) { e.label = name; updated = true; }
          if (waiting === 0 && updated) filterAccessUserDropdown();
        });
      });
    }

    /* identityProviderFromKey classifies a raw identity key by its prefix so
       the UI can show a provider mark next to the name: "google:…" → google,
       "ibmid:…" → ibmid, "microsoft:…" or "ms:…" → microsoft, a "github.com/…"
       prefix or a bare login (no prefix at all) → github. Unknown prefixes
       pass through unchanged and render as the generic person icon. */
    function identityProviderFromKey(key) {
      key = String(key || '');
      if (key.indexOf('github.com/') === 0) return 'github';
      var m = key.match(/^([a-z][a-z0-9_-]*):/);
      if (!m) return 'github';
      var p = m[1];
      if (p === 'ms') return 'microsoft';
      return p;
    }

    /* PROVIDER_ICON_SVG: small inline SVG provider marks — inline so no
       external fetches happen (CSP-safe). Sized 14px to sit alongside the
       0.85rem row labels. Providers without an SVG fall back to emoji. */
    var PROVIDER_ICON_SVG = {
      github: '<svg viewBox="0 0 16 16" width="14" height="14" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z"/></svg>',
      google: '<svg viewBox="0 0 48 48" width="14" height="14" aria-hidden="true"><path fill="#EA4335" d="M24 9.5c3.54 0 6.71 1.22 9.21 3.6l6.85-6.85C35.9 2.38 30.47 0 24 0 14.62 0 6.51 5.38 2.56 13.22l7.98 6.19C12.43 13.72 17.74 9.5 24 9.5z"/><path fill="#4285F4" d="M46.98 24.55c0-1.57-.15-3.09-.38-4.55H24v9.02h12.94c-.58 2.96-2.26 5.48-4.78 7.18l7.73 6c4.51-4.18 7.09-10.36 7.09-17.65z"/><path fill="#FBBC05" d="M10.53 28.59c-.48-1.45-.76-2.99-.76-4.59s.27-3.14.76-4.59l-7.98-6.19C.92 16.46 0 20.12 0 24c0 3.88.92 7.54 2.56 10.78l7.97-6.19z"/><path fill="#34A853" d="M24 48c6.48 0 11.93-2.13 15.89-5.81l-7.73-6c-2.15 1.45-4.92 2.3-8.16 2.3-6.26 0-11.57-4.22-13.47-9.91l-7.98 6.19C6.51 42.62 14.62 48 24 48z"/></svg>',
      microsoft: '<svg viewBox="0 0 23 23" width="14" height="14" aria-hidden="true"><rect x="1" y="1" width="10" height="10" fill="#f25022"/><rect x="12" y="1" width="10" height="10" fill="#7fba00"/><rect x="1" y="12" width="10" height="10" fill="#00a4ef"/><rect x="12" y="12" width="10" height="10" fill="#ffb900"/></svg>',
      ibmid: '<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"><g fill="#0f62fe"><rect x="1" y="3" width="14" height="2"/><rect x="1" y="7" width="14" height="2"/><rect x="1" y="11" width="14" height="2"/></g></svg>'
    };

    /* PROVIDER_EMOJI: plain-text stand-ins for native <select> options, which
       cannot render SVG. Anything unmapped shows the generic person glyph. */
    var PROVIDER_EMOJI = { google: '🔵', ibmid: '🔷', microsoft: '🟦', github: '🐙' };

    /* providerIconHTML returns a small inline icon (SVG when we have one,
       emoji otherwise) vertically aligned with the label text beside it. */
    function providerIconHTML(provider) {
      var body = PROVIDER_ICON_SVG[provider] || '👤';
      return '<span title="' + esc(provider || 'unknown provider') + '" style="display:inline-flex;align-items:center;vertical-align:middle;margin-right:5px;width:14px;height:14px;line-height:1;font-size:12px">' + body + '</span>';
    }

    /* providerOptionEmoji is the <option>-safe variant of providerIconHTML. */
    function providerOptionEmoji(provider) {
      return (PROVIDER_EMOJI[provider] || '👤') + ' ';
    }

    /* accessOptionText renders one dropdown row: a provider mark, the friendly
       label, plus a short parenthesised hint of the underlying ID when the two
       differ so an owner can tell two "Jane Doe"s apart without seeing a full
       raw token. */
    function accessOptionText(e) {
      var icon = providerOptionEmoji(e.provider || identityProviderFromKey(e.id));
      if (!e.id || e.id === e.label) return icon + e.label;
      var hint = e.id.length > 24 ? e.id.slice(0, 24) + '…' : e.id;
      return icon + e.label + ' (' + hint + ')';
    }

    /* renderAccessUserOptions rebuilds the select from _grantableUsers,
       keeping only entries whose label OR raw id matches the filter
       (case-insensitive). The current selection is preserved when it
       survives the filter so typing never silently discards a choice. */
    function renderAccessUserOptions(filter) {
      var sel = document.getElementById('access-username');
      if (!sel) return;
      var prev = sel.value;
      var q = (filter || '').toLowerCase();
      var matches = _grantableUsers.filter(function(e) {
        if (!q) return true;
        return e.label.toLowerCase().indexOf(q) !== -1 || e.id.toLowerCase().indexOf(q) !== -1;
      });
      if (!_grantableUsers.length) {
        sel.innerHTML = '<option value="">No users yet — they must sign in to the hub once</option>';
        return;
      }
      if (!matches.length) {
        sel.innerHTML = '<option value="">No users match "' + esc(filter) + '"</option>';
        return;
      }
      sel.innerHTML = '<option value="">Select user...</option>' + matches.map(function(e) {
        return '<option value="' + esc(e.id) + '">' + esc(accessOptionText(e)) + '</option>';
      }).join('');
      if (prev && matches.some(function(e) { return e.id === prev; })) sel.value = prev;
    }

    function filterAccessUserDropdown() {
      var box = document.getElementById('access-user-search');
      renderAccessUserOptions(box ? box.value : '');
    }

    async function loadAccessUserDropdown() {
      var sel = document.getElementById('access-username');
      if (!sel) return;
      var box = document.getElementById('access-user-search');
      if (box) box.value = '';
      try {
        // grantable-users, NOT admin/users: the latter is admin-only, so every
        // non-admin owner got a 403 and an empty dropdown with no explanation.
        var resp = await fetch('/api/saas/grantable-users');
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        var data = await resp.json();
        // Prefer the enriched entries (stable id + normalized label); fall back
        // to the bare username list from an older hub, where id doubles as label.
        _grantableUsers = (data.entries || (data.users || []).map(function(u) {
          return { id: u, label: u };
        })).filter(function(e) { return e && e.id; });
        renderAccessUserOptions('');
        // Usernames are on screen already; profile names swap in when they
        // arrive (#4145) — never block the dropdown on the GitHub API.
        enrichGrantableUserLabels();
      } catch(e) {
        // Never leave the control looking merely empty — an empty dropdown is
        // indistinguishable from "no such users", which is what made this
        // confusing in the first place.
        _grantableUsers = [];
        sel.innerHTML = '<option value="">Could not load users</option>';
      }
    }

    async function loadAccessList() {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access');
        var data = await resp.json();
        var users = data.access || [];
        _accessUsers = users;
        // Drop selections for users no longer on the roster.
        Object.keys(_bulkAccessSel).forEach(function(name) {
          if (!users.some(function(u) { return u.username === name; })) delete _bulkAccessSel[name];
        });
        if (!users.length) {
          document.getElementById('access-list').innerHTML = '<div style="color:var(--muted);font-size:0.85rem">No users have access yet</div>';
          updateBulkAccessBar();
          return;
        }
        var ownerCount = users.filter(function(u) { return u.role === 'owner'; }).length;
        var rows = users.map(function(u) {
          // u.username is the raw identity key — the actual auth key and
          // allowlist match — and is NEVER altered for display. u.display_label
          // is resolved hub-side (accessForHive → provisionRequestUserIdentity,
          // the SAME precedence used everywhere else a friendly name is shown)
          // and always falls back to u.username, so hasFriendlyName is exactly
          // "the hub found something better than the raw key".
          var provider = u.provider || identityProviderFromKey(u.username);
          var hasFriendlyName = !!(u.display_label && u.display_label !== u.username);
          var avatar = provider === 'github'
            ? linkedAvatar(u.username, LIST_AVATAR_PX,
                String(u.username || '') + (u.role ? ' — ' + u.role : ''), 'margin-right:6px')
            : userAvatar({display_name: u.display_label, avatar_url: u.avatar_url, github_username: u.username},
                LIST_AVATAR_PX, 'margin-right:6px');
          // The last owner can be neither removed nor demoted — doing so would
          // orphan the hive with no one able to manage access.
          var isLastOwner = (u.role === 'owner' && ownerCount <= 1);
          var canRemove = !isLastOwner;
          // The last owner gets no checkbox: it can't be bulk-removed or
          // bulk-demoted, so offering to select it would only mislead.
          var checkbox = isLastOwner ?
            '<span style="display:inline-block;width:13px;margin-right:8px"></span>' :
            '<input type="checkbox" class="access-bulk-cb" style="margin:0 8px 0 0;cursor:pointer;vertical-align:middle"' +
              (_bulkAccessSel[u.username] ? ' checked' : '') +
              ' onchange="toggleBulkAccess(this, \'' + esc(u.username) + '\')" title="Select for bulk actions">';
          var removeBtn = canRemove ?
            '<button onclick="removeAccess(\'' + esc(u.username) + '\')" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Remove</button>' :
            '<span style="font-size:0.6rem;color:var(--muted)">last owner</span>';
          // The role pill is an editable dropdown: changing it POSTs the new role
          // (the add endpoint upserts). The last owner's role is locked (shown as
          // a static pill) so the hive can't be left without an owner.
          var ROLES = ['read', 'read-write', 'merger', 'owner'];
          var roleControl = isLastOwner ?
            '<span class="role-badge role-' + u.role.replace(' ','-') + '" style="font-size:0.7rem" title="The last owner\'s role cannot be changed">' + esc(u.role) + '</span>' :
            '<select class="role-select role-' + u.role.replace(' ','-') + '" style="font-size:0.7rem;padding:2px 6px;border-radius:9999px;cursor:pointer" title="Change this user\'s permission" onchange="changeAccessRole(\'' + esc(u.username) + '\', this.value, \'' + esc(u.role) + '\')">' +
              ROLES.map(function(r) { return '<option value="' + r + '" title="' + escAttr(roleDescription(r)) + '"' + (r === u.role ? ' selected' : '') + '>' + r + '</option>'; }).join('') +
            '</select>';
          /* Last-active is coarse and relative on purpose ("3d ago") — the
             owner's question is "is this account dormant?", not "when exactly".
             The absolute time rides in the title. "—" means never active. */
          var lastActive = u.last_active ?
            '<span style="font-size:0.7rem;color:var(--muted)" title="Last active ' + esc(fmtUserTS(u.last_active)) + '">' + esc(timelineAgo(u.last_active) || fmtUserTS(u.last_active)) + '</span>' :
            '<span style="font-size:0.7rem;color:var(--muted)" title="Never active on this hub">—</span>';
          // The expiry editor doubles as the display: it shows the grant's
          // last valid day when set, and empty means permanent. Changing it
          // extends (or clears) the expiry; the last owner cannot be expired
          // for the same reason they cannot be removed or demoted.
          // A checkbox owns whether this grant expires; the date input EXISTS
          // only when it does.
          //
          // A bare <input type="date"> cannot express "no expiry": an empty one
          // still paints the browser's mm/dd/yyyy placeholder, so a permanent
          // grant and one expiring today look the same. Labelling it was not
          // enough — the row then read "Expires: Never 08/27/2026", stating
          // both answers at once and leaving the operator to guess which the
          // system believed. Removing the control entirely when unchecked is
          // the only version with exactly one visible answer.
          var hasExpiry = !!u.expires_at;
          var expiryToggleId = 'expcb-' + esc(u.username).replace(/[^A-Za-z0-9_-]/g, '_');
          // Checking the box needs a date to submit; today would expire the
          // grant immediately, so default to 30 days out and let the operator
          // adjust. Unchecking sends '' — the API's existing "permanent".
          var expiryControl = isLastOwner ? '' :
            '<span style="display:inline-flex;align-items:center;gap:4px" title="' +
              (hasExpiry ? 'Access is revoked automatically after this date (UTC).' : 'No expiry — this grant lasts until it is removed.') + '">' +
            '<input type="checkbox" id="' + expiryToggleId + '"' + (hasExpiry ? ' checked' : '') +
            ' onchange="toggleAccessExpiry(\'' + esc(u.username) + '\', \'' + esc(u.role) + '\', this.checked)"' +
            ' style="cursor:pointer;margin:0">' +
            '<label for="' + expiryToggleId + '" style="font-size:0.6rem;color:var(--muted);cursor:pointer">Expires</label>' +
            (hasExpiry ?
              '<input type="date" class="access-expiry-input" value="' + esc(expiryToDateInput(u.expires_at)) + '"' +
              ' onchange="changeAccessExpiry(\'' + esc(u.username) + '\', \'' + esc(u.role) + '\', this.value)"' +
              ' title="Access is revoked automatically after this date (UTC)."' +
              ' style="font-size:0.65rem;padding:2px 4px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--amber)">'
              : '<span style="font-size:0.6rem;color:var(--text)">Never</span>') +
            '</span>';
          // Primary label: the resolved friendly name when the hub found one,
          // else the raw key exactly as before. The raw key is NEVER hidden —
          // it rides as a muted secondary line (and the avatar's title) any
          // time a friendly name is shown, so support/debugging always has it
          // one glance away. A GitHub user whose label is still the bare
          // login keeps the existing async profile-name enrichment
          // (.gh-display-name / enrichGhDisplayNames, #4145).
          var primaryLabel = hasFriendlyName ? u.display_label : u.username;
          var rawKeyLine = hasFriendlyName
            ? '<span style="display:block;font-size:0.7rem;color:var(--muted);word-break:break-word" title="Auth key">' + esc(u.username) + '</span>'
            : '';
          var ghEnrichPlaceholder = (provider === 'github' && !hasFriendlyName)
            /* Empty placeholder the async GitHub profile lookup fills in
               (#4145): display name lands beside the username when it arrives,
               and stays empty on any failure — the login always renders first. */
            ? '<span class="gh-display-name" data-gh-login="' + escAttr(u.username) + '" style="margin-left:6px;font-size:0.75rem;color:var(--muted)"></span>'
            : '';
          return '<div style="display:flex;flex-wrap:wrap;align-items:center;justify-content:space-between;gap:8px;padding:8px 0;border-bottom:1px solid var(--border)">' +
            '<div style="display:flex;align-items:center;gap:4px;flex:1 1 240px;min-width:0">' + checkbox + avatar + providerIconHTML(provider) +
            '<span style="font-size:0.85rem;word-break:break-word;min-width:0" title="' + escAttr(u.username) + '">' + esc(primaryLabel) + rawKeyLine + '</span>' +
            ghEnrichPlaceholder + '</div>' +
            '<div style="display:flex;align-items:center;justify-content:flex-end;flex-wrap:wrap;gap:8px;flex:1 1 300px;min-width:0">' +
            lastActive +
            roleControl +
            expiryControl +
            removeBtn +
            '</div></div>';
        }).join('');
        document.getElementById('access-list').innerHTML = rows;
        enrichGhDisplayNames(document.getElementById('access-list'));
        updateBulkAccessBar();
      } catch(e) {
        document.getElementById('access-list').innerHTML = '<div style="color:var(--red)">Failed to load</div>';
      }
    }

    /* RFC-4180-style field quoting: wrap in double quotes when the value
       contains a comma, quote, or newline; embedded quotes double up. */
    function csvField(v) {
      var s = String(v == null ? '' : v);
      if (/[",\r\n]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
      return s;
    }

    /* exportAccessCSV downloads the currently loaded access list (_accessUsers,
       cached by loadAccessList) as CSV, entirely client-side (Blob + synthetic
       <a> click) — no extra round trip and, critically, no server-side file is
       ever written (#4152). Columns follow the audit shape from #4152:
       username, role, granted date (blank — grants are not timestamped today,
       the column keeps exports forward-compatible) and last-active (the user's
       most recent hub activity — see latestUserActivity). Only owners can
       load the list at all, so the button is inherently owner-scoped. */
    function exportAccessCSV() {
      if (!_accessUsers.length) { hiveToast('Nothing to export yet', 'error'); return; }
      var lines = ['username,role,granted_at,last_active'];
      _accessUsers.forEach(function(u) {
        lines.push([
          csvField(u.username),
          csvField(u.role),
          '', // granted date: not tracked per-grant yet
          csvField(u.last_active || '')
        ].join(','));
      });
      var blob = new Blob([lines.join('\r\n') + '\r\n'], {type: 'text/csv;charset=utf-8'});
      var a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = 'hive-access-' + (_accessHiveId || 'unknown') + '-' + new Date().toISOString().slice(0, 10) + '.csv';
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      URL.revokeObjectURL(a.href);
      hiveToast('Access list exported', 'success');
    }

    /* expiryToDateInput maps a stored RFC3339 expiry to the value a
       <input type=date> shows: the grant's LAST VALID day (UTC). The server
       canonicalizes a picked date D to midnight UTC of D+1, so stepping the
       instant back one second always lands on the through-day, for exact
       midnights and arbitrary timestamps alike. Empty in → empty out. */
    function expiryToDateInput(iso) {
      if (!iso) return '';
      var t = Date.parse(iso);
      if (isNaN(t)) return '';
      return new Date(t - 1000).toISOString().slice(0, 10);
    }

    async function addAccess() {
      var username = document.getElementById('access-username').value;
      var role = document.getElementById('access-role').value;
      var expiry = document.getElementById('access-expiry').value;
      if (!username) { hiveToast('Select a user', 'error'); return; }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          // expires_at always rides on the Add path: the picked date, or ""
          // (permanent) when the picker was left empty — never omitted, so a
          // re-add of an existing user resets the expiry to what the form shows.
          body: JSON.stringify({username: username, role: role, expires_at: expiry || ''})
        });
        if (!resp.ok) { var d = await resp.json(); hiveToast(d.error || 'Failed', 'error'); return; }
        document.getElementById('access-username').value = '';
        // Reset the checkbox too, not just the date: clearing one without the
        // other leaves the form claiming an expiry is set while submitting ''
        // (permanent), which is the desync this control exists to prevent.
        document.getElementById('access-expiry-enabled').checked = false;
        toggleAddExpiryVisible();
        loadAccessList();
        loadAccessAuditLog();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    /* changeAccessExpiry sets, extends, or clears (empty value) the expiry on
       an existing grant. The role is re-sent unchanged — the add endpoint
       upserts — and expires_at carries the new bound ("" = permanent). */
    // toggleAddExpiryVisible shows the Add User date input only while the
    // checkbox is on, and swaps in a literal "Never" while it is off, so the
    // row always states one answer rather than showing a placeholder date the
    // operator never chose.
    function toggleAddExpiryVisible() {
      var on = document.getElementById('access-expiry-enabled').checked;
      var input = document.getElementById('access-expiry');
      var never = document.getElementById('access-expiry-never');
      input.style.display = on ? '' : 'none';
      if (never) never.style.display = on ? 'none' : '';
      if (on) {
        // Seed a usable date so checking the box cannot submit an empty value
        // (which the API reads as permanent — the opposite of what was asked).
        if (!input.value) {
          var d = new Date();
          d.setDate(d.getDate() + defaultExpiryDays);
          input.value = d.toISOString().slice(0, 10);
        }
      } else {
        input.value = '';
      }
    }

    // defaultExpiryDays is how far out the date lands when an operator turns
    // expiry ON. It must not be 0: checking the box would then expire the
    // grant the moment it was checked, which reads as the UI revoking access
    // rather than scheduling it.
    var defaultExpiryDays = 30;

    // toggleAccessExpiry flips a grant between permanent and expiring.
    // Unchecking sends '' — the API's existing "permanent" — and checking
    // sends a real date, because the server cannot store "expires, date TBD".
    async function toggleAccessExpiry(username, role, checked) {
      if (!checked) { changeAccessExpiry(username, role, ''); return; }
      var d = new Date();
      d.setDate(d.getDate() + defaultExpiryDays);
      changeAccessExpiry(username, role, d.toISOString().slice(0, 10));
    }

    async function changeAccessExpiry(username, role, value) {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({username: username, role: role, expires_at: value || ''})
        });
        if (!resp.ok) { var d = await resp.json().catch(function(){return {};}); hiveToast(d.error || 'Failed to change expiry', 'error'); loadAccessList(); return; }
        hiveToast(value ? username + '\'s access now expires ' + value : username + '\'s access is now permanent', 'success');
        loadAccessList();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadAccessList(); }
    }

    async function changeAccessRole(username, newRole, oldRole) {
      if (newRole === oldRole) return;
      // Granting owner is significant — confirm it.
      if (newRole === 'owner' && !await hiveConfirm('Make ' + username + ' an owner? Owners can manage access and delete the hive.')) {
        loadAccessList();
        return;
      }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({username: username, role: newRole})
        });
        if (!resp.ok) { var d = await resp.json().catch(function(){return {};}); hiveToast(d.error || 'Failed to change role', 'error'); loadAccessList(); return; }
        hiveToast(username + ' is now ' + newRole, 'success');
        loadAccessList();
        loadAccessAuditLog();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadAccessList(); }
    }

    async function removeAccess(username) {
      var entry = _accessUsers.filter(function(u) { return u.username === username; })[0];
      var isOwner = !!(entry && entry.role === 'owner');
      if (isOwner) {
        // Block removing the last owner client-side (mirrors the server check)
        // so the user gets a clear error instead of a silent no-op.
        var ownerCount = _accessUsers.filter(function(u) { return u.role === 'owner'; }).length;
        if (ownerCount <= 1) {
          hiveToast('At least one owner is required — cannot remove the last owner', 'error');
          return;
        }
      }
      var isSelf = _currentUser && String(username || '').toLowerCase() === _currentUser;
      if (isOwner && isSelf) {
        // Removing yourself as owner is irreversible from your side — confirm
        // explicitly. Non-self removal keeps the ordinary confirmation below.
        if (!await hiveConfirm('You will lose owner access to this hive — are you sure you want to remove yourself?')) return;
      } else {
        if (!await hiveConfirm('Remove access for ' + username + '?')) return;
      }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access/' + encodeURIComponent(username), {method: 'DELETE'});
        if (!resp.ok) {
          var d = await resp.json().catch(function(){return {};});
          hiveToast(d.error || 'Failed to remove access', 'error');
        }
        loadAccessList();
        loadAccessAuditLog();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    /* Bulk selection state for the Manage Access list: username -> true.
       _accessUsers (declared above) mirrors the last roster fetch so the
       bulk guards can count owners without another network round trip. */
    var _bulkAccessSel = {};

    function toggleBulkAccess(cb, username) {
      if (cb.checked) { _bulkAccessSel[username] = true; } else { delete _bulkAccessSel[username]; }
      updateBulkAccessBar();
    }

    /* The bulk bar only appears once 2+ users are selected — a single
       selection is served just as well by the per-row controls. */
    function updateBulkAccessBar() {
      var bar = document.getElementById('access-bulk-bar');
      if (!bar) return;
      var count = Object.keys(_bulkAccessSel).length;
      if (count >= 2) {
        bar.style.display = 'flex';
        document.getElementById('access-bulk-count').textContent = count + ' users selected';
      } else {
        bar.style.display = 'none';
      }
    }

    /* wouldOrphanOwners: true when acting on the selection would leave the
       hive with zero owners. The per-row last-owner lock already hides the
       single-owner case; this catches "select every owner at once". */
    function wouldOrphanOwners(selected) {
      var owners = _accessUsers.filter(function(u) { return u.role === 'owner'; });
      if (!owners.length) return false;
      return owners.every(function(o) { return selected.indexOf(o.username) !== -1; });
    }

    async function bulkChangeAccessRole() {
      var selected = Object.keys(_bulkAccessSel);
      if (selected.length < 2) return;
      var newRole = document.getElementById('access-bulk-role').value;
      if (newRole !== 'owner' && wouldOrphanOwners(selected)) {
        hiveToast('Cannot demote all owners — the hive must keep at least one', 'error');
        return;
      }
      if (newRole === 'owner' && !await hiveConfirm('Make ' + selected.length + ' users owners? Owners can manage access and delete the hive.')) return;
      var failed = [];
      for (var i = 0; i < selected.length; i++) {
        try {
          var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({username: selected[i], role: newRole})
          });
          if (!resp.ok) failed.push(selected[i]);
        } catch(e) { failed.push(selected[i]); }
      }
      if (failed.length) {
        hiveToast('Failed to change role for: ' + failed.join(', '), 'error');
      } else {
        hiveToast(selected.length + ' users are now ' + newRole, 'success');
      }
      _bulkAccessSel = {};
      updateBulkAccessBar();
      loadAccessList();
    }

    async function bulkRemoveAccess() {
      var selected = Object.keys(_bulkAccessSel);
      if (selected.length < 2) return;
      if (wouldOrphanOwners(selected)) {
        hiveToast('Cannot remove all owners — the hive must keep at least one', 'error');
        return;
      }
      if (!await hiveConfirm('Remove access for ' + selected.length + ' users (' + selected.join(', ') + ')?')) return;
      var failed = [];
      for (var i = 0; i < selected.length; i++) {
        try {
          var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access/' + encodeURIComponent(selected[i]), {method: 'DELETE'});
          if (!resp.ok) failed.push(selected[i]);
        } catch(e) { failed.push(selected[i]); }
      }
      if (failed.length) {
        hiveToast('Failed to remove: ' + failed.join(', '), 'error');
      } else {
        hiveToast('Removed ' + selected.length + ' users', 'success');
      }
      _bulkAccessSel = {};
      updateBulkAccessBar();
      loadAccessList();
    }

    /* Banner text is NEUTRAL (matches the spoke's banner-contrast rule);
       the color choice shows in the tint and border only. */
    var _bannerColorStyles = {
      green: {bg: 'rgba(22,163,74,0.12)', border: '1px solid rgba(22,163,74,0.3)', color: 'var(--text)'},
      blue:  {bg: 'rgba(59,130,246,0.12)', border: '1px solid rgba(59,130,246,0.3)', color: 'var(--text)'},
      amber: {bg: 'rgba(245,158,11,0.12)', border: '1px solid rgba(245,158,11,0.3)', color: 'var(--text)'},
      gray:  {bg: 'rgba(107,114,128,0.12)', border: '1px solid rgba(107,114,128,0.3)', color: 'var(--text)'}
    };
    /* Set by the per-hive "Send Banner" menu item so the banner modal targets a
       single spoke; sendHubBanner() still reads .banner-hive-cb:checked, so this
       is only bookkeeping for the open path — the checked cb is the source of truth. */
    var _bannerTargetHive = null;

    (function() {
      var msgEl = document.getElementById('banner-message');
      if (msgEl) {
        msgEl.addEventListener('input', function() {
          document.getElementById('banner-char-count').textContent = this.value.length;
          updateBannerPreview();
        });
      }
      var radios = document.querySelectorAll('input[name="banner-color"]');
      radios.forEach(function(r) { r.addEventListener('change', updateBannerPreview); });
    })();

    // Same safe minimal-markdown as the spoke's renderHubBanner: escape first,
    // then re-introduce only **bold** and [text](http(s)://url) links. Keep this
    // in sync with bannerMarkdown() in the spoke dashboard.
    function bannerMarkdown(text, linkColor) {
      var out = String(text || '')
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
      out = out.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)"']+)\)/g, function(_, label, url) {
        return '<a href="' + url + '" target="_blank" rel="noopener noreferrer" style="color:'
          + (linkColor || 'inherit') + ';text-decoration:underline">' + label + '</a>';
      });
      return out.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    }

    function updateBannerPreview() {
      var msg = (document.getElementById('banner-message').value || '').trim();
      var color = (document.querySelector('input[name="banner-color"]:checked') || {}).value || 'green';
      var s = _bannerColorStyles[color];
      var preview = document.getElementById('banner-preview');
      preview.style.background = s.bg;
      preview.style.border = s.border;
      preview.style.color = s.color;
      preview.innerHTML = msg ? ('📢 ' + bannerMarkdown(msg, s.color)) : '<em style="opacity:0.6">Type a message above to preview...</em>';
    }

    // Toolbar helpers: wrap the current selection (or insert a template) at the
    // cursor, then refresh the char count + preview.
    function bannerInsert(before, after, placeholder) {
      var el = document.getElementById('banner-message');
      var start = el.selectionStart, end = el.selectionEnd;
      var sel = el.value.slice(start, end) || placeholder;
      el.value = el.value.slice(0, start) + before + sel + after + el.value.slice(end);
      // Re-select the inserted content so the user can type over the placeholder.
      el.focus();
      el.selectionStart = start + before.length;
      el.selectionEnd = start + before.length + sel.length;
      document.getElementById('banner-char-count').textContent = el.value.length;
      updateBannerPreview();
    }
    function bannerFmtBold() { bannerInsert('**', '**', 'bold text'); }
    function bannerFmtLink() { bannerInsert('[', '](https://)', 'link text'); }

    function loadBannerHiveList() {
      var container = document.getElementById('banner-hive-list');
      container.innerHTML = '';
      var hives = _hiveRegistry || [];
      if (!hives.length) {
        container.innerHTML = '<div style="padding:12px;color:var(--muted);font-size:0.8rem;text-align:center">No hives found</div>';
        return;
      }
      hives.forEach(function(h) {
        var label = h.name || h.id;
        var div = document.createElement('div');
        div.style.cssText = 'display:flex;align-items:center;gap:8px;padding:6px 10px;border-bottom:1px solid var(--border)';
        div.innerHTML = '<label style="display:flex;align-items:center;gap:8px;cursor:pointer;flex:1;font-size:0.82rem;color:var(--text)">' +
          '<input type="checkbox" class="banner-hive-cb" value="' + esc(h.id) + '" checked style="accent-color:var(--accent)"> ' + esc(label) +
          '</label>';
        container.appendChild(div);
      });
    }

    function toggleAllBannerHives(checked) {
      document.querySelectorAll('.banner-hive-cb').forEach(function(cb) { cb.checked = checked; });
    }

    /* Per-hive entry point: opens the SAME banner modal but pre-scoped to one
       spoke. Instead of loadBannerHiveList()'s multi-hive checklist, we render a
       single non-editable target line plus one hidden checked .banner-hive-cb so
       the unchanged sendHubBanner() (which reads .banner-hive-cb:checked) posts
       exactly this hive_id to POST /api/saas/admin/hub-banner. */
    function openBannerForHive(hiveId, hiveName) {
      _bannerTargetHive = hiveId;
      document.getElementById('banner-modal').style.display = 'flex';
      /* Reset message + color to match the global open path's fresh state. */
      document.getElementById('banner-message').value = '';
      document.getElementById('banner-char-count').textContent = '0';
      document.querySelector('input[name="banner-color"][value="green"]').checked = true;
      updateBannerPreview();
      var container = document.getElementById('banner-hive-list');
      container.innerHTML = '<div style="display:flex;align-items:center;gap:8px;padding:10px;color:var(--text);font-size:0.82rem">' +
        '<span style="color:var(--muted)">Sending to:</span> <strong>' + esc(hiveName) + '</strong>' +
        '<input type="checkbox" class="banner-hive-cb" value="' + esc(hiveId) + '" checked style="display:none">' +
        '</div>';
    }

    async function sendHubBanner() {
      var msg = (document.getElementById('banner-message').value || '').trim();
      if (!msg) { hiveToast('Message is required', 'error'); return; }
      var color = (document.querySelector('input[name="banner-color"]:checked') || {}).value || 'green';
      var hiveIDs = [];
      document.querySelectorAll('.banner-hive-cb:checked').forEach(function(cb) { hiveIDs.push(cb.value); });
      if (!hiveIDs.length) { hiveToast('Select at least one hive', 'error'); return; }
      var btn = document.getElementById('btn-send-banner');
      btn.disabled = true;
      btn.textContent = 'Sending...';
      try {
        var resp = await fetch('/api/saas/admin/hub-banner', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({message: msg, color: color, hive_ids: hiveIDs})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to send', 'error'); return; }
        hiveToast('Banner sent to ' + data.hive_count + ' hive(s)', 'success');
        document.getElementById('banner-modal').style.display = 'none';
        document.getElementById('banner-message').value = '';
        document.getElementById('banner-char-count').textContent = '0';
        document.querySelector('input[name="banner-color"][value="green"]').checked = true;
        updateBannerPreview();
        loadActiveBanner();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
      finally { btn.disabled = false; btn.textContent = 'Send Banner'; }
    }

    async function loadActiveBanner() {
      try {
        var resp = await fetch('/api/saas/admin/hub-banner');
        if (!resp.ok) return;
        var data = await resp.json();
        var banners = data.banners || [];
        var display = document.getElementById('active-banner-display');
        var clearBtn = document.getElementById('btn-clear-banner');
        if (!banners.length) {
          display.style.display = 'none';
          clearBtn.style.display = 'none';
          return;
        }
        var first = banners[0];
        var s = _bannerColorStyles[first.color] || _bannerColorStyles.green;
        var preview = document.getElementById('active-banner-preview');
        preview.style.background = s.bg;
        preview.style.border = s.border;
        preview.style.color = s.color;
        preview.textContent = first.message;
        var targets = document.getElementById('active-banner-targets');
        var hiveNames = banners.map(function(b) { return b.hive_id; });
        targets.textContent = 'Sent to ' + banners.length + ' hive(s): ' + hiveNames.join(', ');
        display.style.display = '';
        clearBtn.style.display = '';
      } catch(e) { /* ignore */ }
    }

    async function clearHubBanner() {
      if (!await hiveConfirm('Clear all active hub banners?')) return;
      try {
        var resp = await fetch('/api/saas/admin/hub-banner', {method: 'DELETE'});
        if (!resp.ok) { hiveToast('Failed to clear', 'error'); return; }
        hiveToast('All banners cleared', 'success');
        loadActiveBanner();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }
  </script>
</body>
</html>`
