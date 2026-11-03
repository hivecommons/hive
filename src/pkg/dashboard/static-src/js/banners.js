    function ghAppIsOperatorSide(state) {
      return GH_APP_OPERATOR_STATES[String(state || '').trim()] === true;
    }

    /* ── Installation-ID discoverability hint ──────────────────────────────
       The banner asks for a numeric Installation ID and gives no clue where it
       comes from, so users ask an operator instead of self-serving. The answer
       is that the ID is the LAST PATH SEGMENT of the org's installed-app
       settings URL. We render that URL as a mock rather than embedding a
       screenshot: a rendered mock is a fraction of the bytes in a
       single-file embedded dashboard, inherits the theme variables so it stays
       legible in light and dark, and is structurally incapable of leaking a
       real installation ID the way a real screenshot would. */

    /* Placeholder for the ID segment. The owner was explicit that the real
       digits must be masked so nobody copies the example into the field as
       their own ID; a run of filled circles reads as "a number goes here"
       while being obviously un-copyable. Nine is the length of a present-day
       GitHub installation ID, so the mock has the right visual weight. */
    var INSTALL_ID_MASK_CHAR = '●';           /* ● BLACK CIRCLE */
    var INSTALL_ID_MASK_LENGTH = 9;                /* typical installation ID digit count */
    var INSTALL_ID_MASK = new Array(INSTALL_ID_MASK_LENGTH + 1).join(INSTALL_ID_MASK_CHAR);
    /* Shown when the hive's org is not known yet — never a real org name. */
    var INSTALL_ID_ORG_PLACEHOLDER = 'your-org';
    /* Fallback host when the hive has not reported one. github.com is only
       ever a fallback: a GHE hive reports its own host and must see that. */
    var INSTALL_ID_DEFAULT_HOST = 'github.com';

    /* Host label for the hint, derived from the hive's configured GitHub host
       rather than hardcoded. Prefers the status payload's githubBaseURL (the
       same signal the banner title already uses to say "GitHub App Not
       Installed on github.ibm.com"), then the boot /api/config value, then
       github.com. */
    function ghAppHintHost(data) {
      var base = (data && data.githubBaseURL)
        || (typeof window !== 'undefined' && window._githubBaseUrl)
        || '';
      var host = String(base).replace(/^https?:\/\//, '').replace(/\/+$/, '');
      return host || INSTALL_ID_DEFAULT_HOST;
    }

    /* Org slug for the hint path, from the boot /api/config fetch. Falls back
       to a placeholder rather than inventing an org name. */
    function ghAppHintOrg() {
      var org = (typeof window !== 'undefined' && window._githubOrg) || '';
      org = String(org).trim();
      return org || INSTALL_ID_ORG_PLACEHOLDER;
    }

    /* Builds the popover markup: a short lead, a mocked browser address bar
       (lock glyph + URL) with the ID segment highlighted and MASKED — the
       highlight pill carries its own "this number" label via CSS ::after —
       and a footnote. Every interpolated value is escaped — org and host
       originate from server config. */
    function ghAppInstallIdHintHtml(data) {
      var host = escapeHtml(ghAppHintHost(data));
      var org = escapeHtml(ghAppHintOrg());
      return '<span class="gh-idhint-lead">Your Installation ID is the <strong>trailing number</strong> '
        + 'at the <strong>end</strong> of the URL — the segment after '
        + '<code>/installations/</code> — in your browser’s address bar on your org’s '
        + 'installed-app settings page:</span>'
        /* The URL sits in an END-anchored scroll column (.gh-idhint-urlcol,
           direction: rtl in CSS): at rest the visible portion is the URL's
           TAIL — …/settings/installations/<id> — and scrolling left reveals
           the start. The "this number" callout is a CSS ::after attached to
           seg-id itself, so it stays under the digits at every scroll
           position and for any org-name length; the URL is dynamic, so a
           separate character-aligned arrow row could never line up (the
           original defect showed the URL's start with arrows pointing at the
           truncation). */
        + '<span class="gh-idhint-url">'
        +   '<span class="seg-lock" aria-hidden="true">🔒</span>'
        +   '<span class="gh-idhint-urlcol">'
        +     '<span class="gh-idhint-urltext">'
        +       'https://<span class="seg-host">' + host + '</span>/organizations/'
        +       org + '/settings/installations/'
        +       '<span class="seg-id">' + INSTALL_ID_MASK + '</span>'
        +     '</span>'
        +   '</span>'
        + '</span>'
        + '<span class="gh-idhint-foot">Open <strong>' + host + '</strong> → your org → '
        + 'Settings → GitHub Apps → the Hive app → <em>Configure</em>. That lands you on the '
        + '<code>…/settings/installations/&lt;id&gt;</code> URL — copy the digits at the very end. '
        + 'The circles above are a placeholder — they are not a real ID.</span>';
    }

    /* Refreshes the hint content for the hive's current host/org. Safe to call
       repeatedly; called on every banner render so a late /api/config response
       still corrects a hint that first rendered with the fallback host. */
    function renderGitHubAppInstallIdHint(data) {
      var pop = document.getElementById('gh-app-install-id-hint-pop');
      if (!pop) return;
      pop.innerHTML = ghAppInstallIdHintHtml(data);
    }

    /* Viewport-edge collision handling for the install-ID hint popover.
       The panel's default CSS position is `bottom: 100%` — anchored above
       the `?` button. That's fine when the banner has room above it, but
       when the banner sits near the top of the page (e.g. right under the
       page header) there isn't 8px + the panel's rendered height of space,
       and the panel overflows off the top of the browser window: its first
       lines ("...this number") render above y=0 and are clipped.

       This measures the anchor via getBoundingClientRect() on every show
       and, when there isn't enough room above, toggles
       .gh-idhint-pop-below (see CSS) to flip the panel to render BELOW the
       anchor instead — the side that has room. It also clamps the panel
       horizontally so it can't run off the left/right edge on a narrow
       viewport or when the banner is near a side edge, the same way the
       account-menu at #oc-gh-user-menu already clamps via max-width.

       Dependency-free: no popover/positioning library, just
       getBoundingClientRect + inline style overrides of the CSS defaults. */
    function positionInstallIdHint() {
      var pop = document.getElementById('gh-app-install-id-hint-pop');
      var wrap = document.getElementById('gh-app-install-id-hint');
      if (!pop || !wrap) return;
      var POPOVER_EDGE_MARGIN_PX = 8; // matches the `bottom: calc(100% + 8px)` gap in CSS
      var anchorRect = wrap.getBoundingClientRect();
      var popRect = pop.getBoundingClientRect();
      var popHeight = popRect.height || pop.offsetHeight || 0;
      var viewportWidth = document.documentElement.clientWidth || window.innerWidth || 0;

      /* Flip below when there isn't room above the anchor for the panel
         plus its margin. getBoundingClientRect() returns 0-height while
         `display:none`, so read it while CSS already shows it (hover /
         focus-within / aria-expanded all apply before this runs on the
         relevant events) — a 0 height then just falls through to "fits". */
      var fitsAbove = (anchorRect.top - POPOVER_EDGE_MARGIN_PX - popHeight) >= 0;
      pop.classList.toggle('gh-idhint-pop-below', !fitsAbove);

      /* Horizontal clamp: the panel is centered on the anchor by default
         (`left: 50%; transform: translateX(-50%)`). If that would push
         either edge past the viewport, override with a fixed left offset
         instead of the centered transform so the whole panel stays
         visible on screen. */
      var popWidth = popRect.width || pop.offsetWidth || 0;
      var anchorCenter = anchorRect.left + (anchorRect.width / 2);
      var idealLeft = anchorCenter - (popWidth / 2);
      var minLeft = POPOVER_EDGE_MARGIN_PX;
      var maxLeft = viewportWidth - popWidth - POPOVER_EDGE_MARGIN_PX;
      if (maxLeft < minLeft) maxLeft = minLeft; // panel wider than viewport minus margins
      var clampedLeft = Math.min(Math.max(idealLeft, minLeft), maxLeft);
      if (clampedLeft !== idealLeft) {
        /* Reposition relative to the anchor, not the viewport, since the
           panel is `position: absolute` inside `.gh-idhint` (`position:
           relative`). Convert the clamped viewport-space left back into
           anchor-space and drop the centering transform. */
        pop.style.left = (clampedLeft - anchorRect.left) + 'px';
        pop.style.transform = 'none';
      } else {
        pop.style.left = '';
        pop.style.transform = '';
      }
    }

    /* Click/keyboard toggle. Enter and Space activate a <button class="hv-btn btn-secondary"> natively, so
       this one handler serves mouse, touch, and keyboard alike — the panel is
       never hover-only. */
    function toggleInstallIdHint(event) {
      if (event) event.stopPropagation();
      var btn = document.getElementById('gh-app-install-id-hint-btn');
      if (!btn) return;
      var open = btn.getAttribute('aria-expanded') === 'true';
      btn.setAttribute('aria-expanded', open ? 'false' : 'true');
      if (!open) {
        /* Panel is about to become visible (CSS reacts to aria-expanded
           synchronously) — measure and position it now. requestAnimationFrame
           ensures layout has settled before we read getBoundingClientRect(). */
        requestAnimationFrame(positionInstallIdHint);
      }
    }

    /* The panel is also shown by pure CSS on hover/focus-within (see
       .gh-idhint:hover / :focus-within in the stylesheet), with no JS in the
       loop at all. Hook those same triggers here so hover users get the same
       collision handling as click/keyboard users, not just a clipped panel. */
    (function () {
      var wrap = document.getElementById('gh-app-install-id-hint');
      if (!wrap) return;
      wrap.addEventListener('mouseenter', function () {
        requestAnimationFrame(positionInstallIdHint);
      });
      wrap.addEventListener('focusin', function () {
        requestAnimationFrame(positionInstallIdHint);
      });
      /* The popover's own content is swapped in asynchronously by
         renderGitHubAppInstallIdHint() (host/org arrive from /api/config
         after boot), which can change its rendered height. Re-measure on
         window resize too, since a narrower/shorter viewport can change
         whether the panel still fits above the anchor. */
      window.addEventListener('resize', function () {
        var btn = document.getElementById('gh-app-install-id-hint-btn');
        if (btn && btn.getAttribute('aria-expanded') === 'true') {
          positionInstallIdHint();
        }
      });
    })();

    /* Dismissal: a click anywhere outside the hint, or Escape, closes it.
       Without this a tapped panel on touch would have no way to close, since
       there is no hover-out event to rely on. */
    document.addEventListener('click', function (e) {
      var btn = document.getElementById('gh-app-install-id-hint-btn');
      if (!btn || btn.getAttribute('aria-expanded') !== 'true') return;
      var wrap = document.getElementById('gh-app-install-id-hint');
      if (wrap && e.target && wrap.contains(e.target)) return;
      btn.setAttribute('aria-expanded', 'false');
    });
    /* ── Onboarding → Getting Started guide tips ────────────────────────
       Server classifies setup problems (StatusPayload.onboardingIssues,
       SystemAlert.onboarding/guideStep, deepHealth checks[].guideStep). Every
       surface that renders one appends this tip, whose link opens the 🧭 Guide
       at the matching step. A new operator (GHE, no welcome checklist in
       sight) asked Slack for the install URL and "where is the guide?" — the
       dashboard must answer that itself. */
    var ONBOARDING_GUIDE_STEP_TITLES = {
      'github-app': 'Install the Forge App (GitHub App)',
      'repos': 'Add projects & mark the primary',
      'add-agent': 'Add a custom agent',
      'welcome': 'Welcome to your hive'
    };
    function onboardingGuideTipHtml(stepId, extra) {
      var step = ONBOARDING_GUIDE_STEP_TITLES[stepId] ? stepId : 'github-app';
      var what = step === 'github-app' ? ' for the install URL and permissions checklist' : '';
      return 'New hive? <a class="onboarding-guide-link" role="button" tabindex="0" data-action="openWelcomeDialog" data-arg0="' + esc(step) + '" data-keydown-action="openWelcomeDialog" data-keys="Enter, ">Open the 🧭 Guide</a> (top navbar) → step ‘' + esc(ONBOARDING_GUIDE_STEP_TITLES[step]) + '’' + what + '.'
        + (extra ? ' ' + extra : '');
    }
    function onboardingGuideTipFill(id, stepId, extra) {
      var el = document.getElementById(id);
      if (el) el.innerHTML = onboardingGuideTipHtml(stepId, extra);
    }
    /* Why the Governor Config gear may be missing for a new operator: the
       Guide, its entry points and the settings gear all require a non-read
       role (/api/role), granted via Manage Access on the hub. */
    var ONBOARDING_ROLE_NOTE = 'Settings (⚙️ Governor Config) and this guide require a read-write or owner role — ask the hive owner for access via Manage Access if you cannot see them.';
    /* Keep the Guide chip reachable while any onboarding problem is active,
       even if the checklist was dismissed earlier. Read-only roles stay
       gated (the guide drives write actions). */
    var _welcomeRoleIsRead = false;
    function renderOnboardingGuideChip(data) {
      var issues = (data && Array.isArray(data.onboardingIssues)) ? data.onboardingIssues : [];
      if (!issues.length || _welcomeRoleIsRead) return;
      var topBtn = document.getElementById('welcome-topbar-btn');
      if (topBtn && topBtn.style.display === 'none') topBtn.style.display = '';
      var navItem = document.getElementById('welcome-nav-item');
      if (navItem && navItem.style.display === 'none') navItem.style.display = '';
    }

    function renderGitHubAppBanner(data) {
      var banner = document.getElementById('gh-app-install-banner');
      if (!banner) return;
      /* This render runs on EVERY status poll. It updates title/desc/link only
         and must never disturb what the user is typing into the install-ID
         field: in Firefox on Linux a value that "doesn't stick" is consistent
         with the field being cleared mid-entry. Snapshot the value before the
         render and restore it after if anything cleared it, unless the user has
         since typed something new. Harmless in Safari/Chrome (value unchanged). */
      var idInput = document.getElementById('gh-app-install-id-input');
      var priorIdValue = idInput ? idInput.value : null;
      /* Keep the hint's host/org in step with whatever this render knows. */
      renderGitHubAppInstallIdHint(data);
      if (idInput && priorIdValue && !idInput.value) idInput.value = priorIdValue;
      var appState = data ? String(data.githubAppState || '').trim() : '';
      var isOperatorSide = data && data.githubAppRequired && ghAppIsOperatorSide(appState);

      /* Operator-side failure: the hive's App credentials were never delivered,
         or the key it holds belongs to a different App. The user did nothing
         wrong and has nothing to fix, so this banner states the cause, names
         who fixes it, and offers NO install link or config instruction. */
      if (isOperatorSide) {
        banner.classList.remove('perm-issue');
        banner.classList.add('operator-issue');
        var oTitle = document.querySelector('#gh-app-install-banner .gh-app-title');
        var oDesc = document.querySelector('#gh-app-install-banner .gh-app-desc');
        var oLink = document.getElementById('gh-app-install-link');
        if (oTitle) {
          oTitle.textContent = appState === GH_APP_STATE_NO_APP_ASSIGNED
            ? 'This hive was never assigned a GitHub App ID'
            : 'GitHub App credentials pending — no action needed from you';
        }
        if (oDesc) {
          /* Prefer the server-composed diagnosis; it already carries the
             accurate, non-blaming copy. textContent keeps any org name or API
             text safe to render verbatim. */
          oDesc.textContent = (typeof data.githubAppPermIssue === 'string' && data.githubAppPermIssue)
            ? data.githubAppPermIssue
            : (appState === GH_APP_STATE_NO_APP_ASSIGNED
                ? 'This hive was provisioned as a placeholder and still carries the placeholder github.app_id, which does not name a real GitHub App. It cannot authenticate no matter which installation ID is set — your App installation and installation ID are not at fault. Only the hub operator can assign the real App ID. Please contact your hub administrator.'
                : appState === GH_APP_STATE_KEY_INVALID
                ? 'This hive’s GitHub App private key does not match the App it authenticates as, so GitHub rejects its token. The key is supplied by the hub operator and cannot be corrected by you — your App installation and installation ID are not at fault. Please contact your hub administrator.'
                : 'This hive’s GitHub App credentials have not been provisioned yet. The private key is supplied by the hub operator, not by you. Agents will start working automatically once it arrives; contact your hub administrator if this persists.');
        }
        /* No install link: sending the user to an install page for a key
           problem is exactly the misdirection this banner exists to stop. */
        if (oLink) {
          oLink.removeAttribute('href');
          oLink.textContent = '';
          oLink.style.display = 'none';
        }
        var staleDetails = document.getElementById('gh-app-perm-details');
        if (staleDetails) staleDetails.remove();
        onboardingGuideTipFill('gh-app-guide-tip', 'github-app', 'The step explains which parts are yours and which the hub operator supplies.');
        banner.removeAttribute('hidden');
        banner.style.cssText = 'display:flex !important';
        return;
      }
      /* Not operator-side — restore the action link the branch above hides. */
      var resetLink = document.getElementById('gh-app-install-link');
      if (resetLink) resetLink.style.display = '';
      banner.classList.remove('operator-issue');

      /* githubAppPermIssue carries the diagnostic string for EVERY
         user-actionable App failure, including "not installed at all", so it
         cannot decide between "installed but under-permissioned" and "never
         installed". Using it alone told a hive carrying a placeholder app_id
         that "the app is installed but cannot write" and offered
         "View App Settings" — settings for an App that does not exist — while
         the banner's own body text said it was not yet linked to any App.
         githubAppState (added with the operator/user classification) is the
         authoritative signal, so an explicitly not-installed state is never
         treated as a permission problem. */
      /* PRIMARY trigger, checked FIRST and sufficient BY ITSELF: config truth
         says a real App is named but installation_id is 0/empty
         (githubAppInstallMissing, computed server-side from the RESOLVED
         config). No auth-state classification, recheck round-trip, or install
         URL may gate it: after a "Reset Forge App" the raw base_url/app_slug
         go blank, and keying visibility off those raw fields (via an empty
         githubAppInstallURL) kept this banner hidden on a hive whose auth
         state was literally not-installed (vllmd-13). */
      var isInstallMissing = !!data.githubAppInstallMissing;
      /* Classified auth states remain an ADDITIONAL trigger — they cover the
         id-present-but-invalid case the config test cannot see. */
      var isNotInstalled = isInstallMissing
        || appState === GH_APP_STATE_NOT_INSTALLED
        || appState === GH_APP_STATE_WRONG_INSTALLATION;
      var isPermIssue = data.githubAppRequired && data.githubAppPermIssue && !isNotInstalled;
      if ((data.githubAppRequired || isInstallMissing) && (isNotInstalled || data.githubAppInstallURL || isPermIssue)) {
        var link = document.getElementById('gh-app-install-link');
        var title = document.querySelector('#gh-app-install-banner .gh-app-title');
        var desc = document.querySelector('#gh-app-install-banner .gh-app-desc');
        var existingDetails = document.getElementById('gh-app-perm-details');
        if (isPermIssue) {
          banner.classList.add('perm-issue');
          if (title) title.textContent = 'GitHub App — Insufficient Permissions';
          if (desc) desc.textContent = 'The app is installed but cannot write to this project.';
          // Never fall back to a public github.com URL: on GitHub Enterprise
          // that sends the user to the wrong GitHub entirely and their org
          // admin never sees the request. Prefer the server-computed install
          // URL; otherwise send them to this hive's own GitHub host root and
          // say so, rather than guessing an app path/slug the client does not
          // know.
          if (link) {
            link.textContent = 'View App Settings';
            if (data.githubAppInstallURL) {
              link.href = data.githubAppInstallURL;
            } else if (data.githubBaseURL) {
              link.href = data.githubBaseURL;
              link.textContent = 'Open ' + String(data.githubBaseURL).replace(/^https?:\/\//, '');
            } else {
              link.removeAttribute('href');
              link.textContent = 'Install URL unavailable';
            }
          }
          var details = existingDetails;
          if (!details) {
            details = document.createElement('div');
            details.id = 'gh-app-perm-details';
            details.className = 'gh-app-perm-details';
            desc.parentElement.appendChild(details);
          }
          // The server composes the diagnosis (pending org approval vs.
          // installation_id pointing at the wrong org), so render its exact
          // message rather than assuming which case we're in. textContent
          // keeps the server string safe to display verbatim.
          if (typeof data.githubAppPermIssue === 'string' && data.githubAppPermIssue) {
            details.textContent = data.githubAppPermIssue;
          } else {
            details.innerHTML = '<strong>Current permissions:</strong><br>'
              + '<span class="perm-ok">✓ Issues: Read</span> — can find and read issues<br>'
              + '<span class="perm-missing">✗ Issues: Write</span> — cannot create or update issue comments<br>'
              + '<br><strong>Required:</strong> Issues: Read &amp; Write<br>'
              + '<em>The org owner must go to Organization Settings → GitHub Apps → hivecommons-hive → Configure and accept the updated permissions.</em>';
          }
        } else {
          banner.classList.remove('perm-issue');
          if (existingDetails) existingDetails.remove();
          var isGHE = data.githubBaseURL && data.githubBaseURL !== 'https://github.com';
          if (title) title.textContent = isGHE ? 'GitHub App Not Installed on ' + data.githubBaseURL.replace('https://', '') : 'GitHub App Not Installed';
          if (desc) desc.textContent = isGHE
            ? 'This hive cannot post advisory reports. Install the GitHub App on your GHE org; this hive checks every few minutes and will configure itself, or you can paste the Installation ID below.'
            : 'This hive cannot create issues or advisory reports. Install the Hive Hub GitHub App on your project; this hive checks every few minutes and will configure itself, or you can paste the Installation ID below.';
          /* A GitHub Enterprise host whose App slug was never configured yields
             an EMPTY install URL from the server, deliberately: the public
             "kubestellar-hive" slug names no App on an enterprise host, so a
             best-effort link would 404 and read as a broken product. Say what is
             missing instead of offering a dead button. */
          if (link) {
            if (data.githubAppInstallURL) {
              link.textContent = 'Install GitHub App';
              link.href = data.githubAppInstallURL;
              link.removeAttribute('aria-disabled');
            } else {
              link.removeAttribute('href');
              link.setAttribute('aria-disabled', 'true');
              link.textContent = isGHE ? 'Install link unavailable for this GitHub host' : 'Install URL unavailable';
              if (desc) {
                desc.textContent = isGHE
                  ? 'This hive cannot post advisory reports. The GitHub App slug for '
                    + String(data.githubBaseURL).replace(/^https?:\/\//, '')
                    + ' is not configured, so an install link cannot be built. Ask your hub operator to set github_app_slug for this cluster.'
                  : desc.textContent;
              }
            }
          }
        }
        onboardingGuideTipFill('gh-app-guide-tip', 'github-app');
        banner.removeAttribute('hidden');
        banner.style.cssText = 'display:flex !important';
      } else {
        banner.classList.remove('perm-issue');
        var oldDetails = document.getElementById('gh-app-perm-details');
        if (oldDetails) oldDetails.remove();
        banner.setAttribute('hidden', '');
        banner.style.cssText = 'display:none';
      }
    }

    function renderRepoTargetBanner(data) {
      var banner = document.getElementById('repo-target-banner');
      if (!banner) return;
      if (data && data.repoTargetMisconfigured) {
        var desc = document.getElementById('repo-target-banner-desc');
        if (desc) desc.textContent = data.repoTargetIssue || 'Project target misconfigured — expected owner/name. Fix in Settings → Projects.';
        onboardingGuideTipFill('repo-target-guide-tip', 'repos', ONBOARDING_ROLE_NOTE);
        banner.removeAttribute('hidden');
        banner.style.cssText = 'display:flex !important';
      } else {
        banner.setAttribute('hidden', '');
        banner.style.display = 'none';
      }
    }

    // #9972: watched repos with has_issues=false. Agents cannot file issues
    // there, so warn up front; each repo can be dismissed (persisted in
    // localStorage) for operators who keep Issues off on purpose.
    var ISSUES_DISABLED_DISMISSED_KEY = 'issues-disabled-dismissed';
    var _lastIssuesDisabledRepos = [];
    function issuesDisabledDismissed() {
      try { return JSON.parse(localStorage.getItem(ISSUES_DISABLED_DISMISSED_KEY) || '[]') || []; } catch (e) { return []; }
    }
    function renderIssuesDisabledBanner(data) {
      var banner = document.getElementById('issues-disabled-banner');
      if (!banner) return;
      _lastIssuesDisabledRepos = (data && data.issuesDisabledRepos) || [];
      var dismissed = issuesDisabledDismissed();
      var shown = _lastIssuesDisabledRepos.filter(function(r) { return r && r.repo && dismissed.indexOf(r.repo) === -1; });
      var list = document.getElementById('issues-disabled-banner-list');
      if (!shown.length || !list) {
        banner.setAttribute('hidden', '');
        banner.style.display = 'none';
        if (list) list.innerHTML = '';
        return;
      }
      list.innerHTML = shown.map(function(r) {
        return '<div class="issues-disabled-row">'
          + '<span class="issues-disabled-msg"><strong>' + escapeHtml(r.repo) + '</strong>' + (r.fork ? ' (fork)' : '') + ': ' + escapeHtml(r.message || '') + '</span>'
          + '<button type="button" class="hv-btn btn-icon btn-sm" data-action="dismissIssuesDisabledRepo" data-arg0="' + escapeHtml(r.repo) + '" title="Dismiss for this repo">✕</button>'
          + '</div>';
      }).join('');
      banner.removeAttribute('hidden');
      banner.style.cssText = 'display:flex !important';
    }
    function dismissIssuesDisabledRepo(repo) {
      if (!repo) return;
      var dismissed = issuesDisabledDismissed();
      if (dismissed.indexOf(repo) === -1) dismissed.push(repo);
      localStorage.setItem(ISSUES_DISABLED_DISMISSED_KEY, JSON.stringify(dismissed));
      renderIssuesDisabledBanner({ issuesDisabledRepos: _lastIssuesDisabledRepos });
    }

    var _hubBannerColorMap = {
      /* banner copy stays neutral (--text); the semantic color lives in the tint/border */
      green: {bg: 'rgba(22,163,74,0.12)', border: 'rgba(22,163,74,0.3)', color: 'var(--text)'},
      blue:  {bg: 'rgba(59,130,246,0.12)', border: 'rgba(59,130,246,0.3)', color: 'var(--text)'},
      amber: {bg: 'rgba(245,158,11,0.12)', border: 'rgba(245,158,11,0.3)', color: 'var(--text)'},
      gray:  {bg: 'rgba(107,114,128,0.12)', border: 'rgba(107,114,128,0.3)', color: 'var(--text)'}
    };

    // bannerMarkdown renders a SAFE, minimal markdown subset for hub banners:
    // links [text](http(s)://url) and **bold**. Everything is HTML-escaped
    // FIRST (XSS-safe), then only these two constructs are re-introduced, and
    // link hrefs are restricted to http/https. No other markdown/HTML passes.
    function bannerMarkdown(text, linkColor) {
      var esc = String(text || '')
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
      // Links: [label](url) — url must be http(s) and contain no quotes/spaces.
      esc = esc.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)"']+)\)/g, function (_, label, url) {
        return '<a href="' + url + '" target="_blank" rel="noopener noreferrer" style="color:'
          + (linkColor || 'inherit') + ';text-decoration:underline">' + label + '</a>';
      });
      // Bold: **text**
      esc = esc.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
      return esc;
    }

    function renderHubBanner(banner) {
      var el = document.getElementById('hub-banner');
      if (!el) return;
      if (!banner || !banner.message) { el.style.display = 'none'; el.innerHTML = ''; return; }
      var dismissKey = 'hub-banner-dismissed-' + banner.id;
      if (localStorage.getItem(dismissKey)) { el.style.display = 'none'; el.innerHTML = ''; return; }
      var s = _hubBannerColorMap[banner.color] || _hubBannerColorMap.green;
      el.style.display = 'block';
      el.innerHTML = '<div style="background:' + s.bg + ';border:1px solid ' + s.border + ';border-radius:8px;padding:var(--sp-5) var(--sp-6);margin-bottom:var(--sp-4);display:flex;align-items:center;gap:10px;font-size:0.85rem">'
        + '<span style="font-size:1.1rem">📢</span>'
        + '<span style="flex:1;color:' + s.color + '">' + bannerMarkdown(banner.message, s.color) + '</span>'
        + '<button class="hv-btn btn-primary" data-action="dismissHubBanner" data-arg0="' + esc(banner.id || '') + '" style="background:none;border:none;color:' + s.color + ';opacity:0.6;cursor:pointer;font-size:1.1rem;padding:var(--sp-0) var(--sp-2)" title="Dismiss">✕</button>'
        + '</div>';
    }

    function dismissHubBanner(id) {
      // Record the dismissal server-side so it is attributed to the signed-in
      // user in the spoke's audit log. The server authorizes: only an
      // authenticated write/owner user gets a 2xx. We hide the banner ONLY on
      // success — a read-only or signed-out viewer gets a 403 and the banner
      // stays visible (their dismissal isn't honored). Dismissal remains
      // per-browser (localStorage) for the view; the POST is just the audit
      // trail, so a network failure still lets the user hide it locally.
      fetch('/api/banner-dismissed', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: id })
      }).then(function(resp) {
        if (!resp.ok) return; // not authorized to dismiss — leave banner up
        localStorage.setItem('hub-banner-dismissed-' + id, '1');
        var el = document.getElementById('hub-banner');
        if (el) { el.style.display = 'none'; el.innerHTML = ''; }
      }).catch(function() {
        // Network/API failure: fall back to a local-only hide so the user is
        // never stuck with an undismissable banner (no audit line in this case).
        localStorage.setItem('hub-banner-dismissed-' + id, '1');
        var el = document.getElementById('hub-banner');
        if (el) { el.style.display = 'none'; el.innerHTML = ''; }
      });
    }

    // maybePlanningIntro shows a ONE-TIME callout the first time planning is
    // available (there is at least one repo to plan issues from), teaching the
    // two entry points: the ⧉ Plan button and the `plan` GitHub label. Dismissed
    // permanently via localStorage.
    var PLANNING_INTRO_KEY = 'planning-intro-dismissed';
    function maybePlanningIntro(data) {
      var el = document.getElementById('planning-intro');
      if (!el) return;
      if (localStorage.getItem(PLANNING_INTRO_KEY)) { el.hidden = true; el.innerHTML = ''; return; }
      var hasRepos = (data && data.repos && data.repos.length > 0);
      // Only introduce the feature once there is something to plan, and once any
      // planning activity exists we assume the user has found it — stay quiet.
      var plan = (data && data.planning) || {};
      var alreadyUsed = (plan.active_plans || 0) > 0 || (plan.pending_decompose || 0) > 0;
      // Planning needs ACMM L5+ (the architect that decomposes epics is only
      // scheduled there) — don't advertise a feature the hive can't act on.
      var levelOK = ((data && data.acmmLevel) || 0) >= 5;
      if (!hasRepos || alreadyUsed || !levelOK) { el.hidden = true; el.innerHTML = ''; return; }
      el.hidden = false;
      el.innerHTML = '<div class="dashboard-notice dashboard-notice--planning">'
        + '<span class="dashboard-notice__icon">🧭</span>'
        + '<span class="dashboard-notice__body"><strong>New: plan an issue.</strong> Add the <code>plan</code> label to any issue — or click <strong>⧉ Plan</strong> on an issue below — and the architect breaks it into a reviewable task plan you approve before work starts.</span>'
        + '<button class="hv-btn btn-icon btn-sm" data-action="dismissPlanningIntro" title="Dismiss">✕</button>'
        + '</div>';
    }
    function dismissPlanningIntro() {
      localStorage.setItem(PLANNING_INTRO_KEY, '1');
      var el = document.getElementById('planning-intro');
      if (el) { el.hidden = true; el.innerHTML = ''; }
    }

    function renderSystemAlerts(alerts) {
      var banner = document.getElementById('system-alerts-banner');
      if (!banner) return;
      if (!alerts || alerts.length === 0) {
        banner.style.display = 'none';
        banner.innerHTML = '';
        return;
      }
      banner.style.display = 'block';
      banner.innerHTML = alerts.map(function(a) {
        var bg = a.severity === 'error' ? 'color-mix(in srgb, var(--red) 14%, transparent)' : 'color-mix(in srgb, var(--yellow) 14%, transparent)';
        var border = a.severity === 'error' ? 'var(--red)' : 'var(--yellow)';
        var textColor = 'var(--text)'; // banner copy stays neutral; severity lives in the tint/border
        var dismiss = (a.id || '').indexOf('plan-stall-') === 0
          ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="dismissSystemAlert" data-arg0="' + esc(a.id || '') + '" data-stop="1" title="Dismiss this needs-human plan alert">Dismiss</button>'
          : '';
        var guideTip = a.onboarding ? '<div class="onboarding-guide-tip">' + onboardingGuideTipHtml(a.guideStep || 'github-app') + '</div>' : '';
        return '<div style="background:' + bg + ';border:1px solid ' + border + ';border-radius:8px;padding:var(--sp-5) var(--sp-6);margin-bottom:var(--sp-4);display:flex;align-items:center;gap:10px;font-size:0.85rem">'
          + '<span style="font-size:1.3rem">⚠️</span>'
          + '<span style="color:' + textColor + ';flex:1">' + linkifyGitHubRefs(a.message, window._githubBaseUrl) + guideTip + '</span>'
          + dismiss
          + '</div>';
      }).join('');
      stopSystemAlertLinkClicks(banner);
    }

    async function dismissSystemAlert(id) {
      id = (id || '').trim();
      if (!id) return;
      try {
        const resp = await fetch('/api/system-alerts/dismiss', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ id })
        });
        if (!resp.ok) throw new Error(await resp.text());
        const last = window._lastStatus;
        if (last && Array.isArray(last.systemAlerts)) {
          last.systemAlerts = last.systemAlerts.filter(a => a.id !== id);
          renderSystemAlerts(last.systemAlerts);
        }
        showToast('Alert dismissed', 'success');
      } catch (err) {
        showToast('Could not dismiss alert: ' + (err && err.message ? err.message : err), 'error');
      }
    }

    async function submitGitHubInstallationID() {
      var input = document.getElementById('gh-app-install-id-input');
      var btn = document.getElementById('gh-app-set-id-btn');
      var raw = (input && input.value || '').trim();
      var id = parseInt(raw, 10);
      if (!raw || isNaN(id) || id <= 0) {
        showToast('Enter the numeric installation ID (from github.com/settings/installations/<ID>)', 'error');
        return;
      }
      if (btn) { btn.disabled = true; btn.textContent = 'Saving...'; }
      try {
        // Persists installation_id and live-reinits the GitHub client (no
        // restart) via the existing config-github handler.
        var resp = await fetch('/api/config/github', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ installation_id: id })
        });
        var data = await resp.json().catch(function(){ return {}; });
        if (!resp.ok) {
          showToast('Failed: ' + (data.error || resp.status), 'error');
          return;
        }
        if (data.reinit === 'ok') {
          showToast('Installation ID set and GitHub App connected', 'success');
          var banner = document.getElementById('gh-app-install-banner');
          if (banner) { banner.classList.remove('perm-issue'); banner.setAttribute('hidden', ''); banner.style.cssText = 'display:none'; }
        } else if (data.reinit === 'failed') {
          showToast('ID saved, but connect failed: ' + (data.reinit_error || 'check app install/permissions'), 'error');
        } else {
          // Saved but reinit not attempted (e.g. app key missing) — re-check
          // will surface the remaining gap.
          showToast('Installation ID saved. Click Re-check to verify.', 'success');
        }
      } catch(e) {
        showToast('Error: ' + e.message, 'error');
      } finally {
        if (btn) { btn.disabled = false; btn.textContent = 'Save Installation ID'; }
      }
    }

    async function recheckGitHubApp() {
      var btn = document.getElementById('gh-app-recheck-btn');
      if (btn) { btn.disabled = true; btn.textContent = 'Checking...'; }
      try {
        var resp = await fetch('/api/github-app/recheck', { method: 'POST' });
        var data = await resp.json();
        if (data.status === 'installed') {
          var banner = document.getElementById('gh-app-install-banner');
          if (banner) { banner.classList.remove('perm-issue'); banner.setAttribute('hidden', ''); banner.style.cssText = 'display:none'; }
          var oldDetails = document.getElementById('gh-app-perm-details');
          if (oldDetails) oldDetails.remove();
        } else if (data.status === 'insufficient_permissions') {
          if (btn) btn.textContent = 'Permissions insufficient';
        } else {
          if (btn) btn.textContent = 'Not found — try again';
        }
      } catch(e) {
        if (btn) btn.textContent = 'Error — try again';
      } finally {
        if (btn) { btn.disabled = false; setTimeout(function() { btn.textContent = 'Re-check'; }, 3000); }
      }
    }

