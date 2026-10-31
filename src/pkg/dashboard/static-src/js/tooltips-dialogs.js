    // ── Shared help-tooltip helper ──
    // Every `.config-info` trigger (card "?" marks, settings help marks, Overview
    // ⓘ terms) renders its `.config-tooltip` into one body-level fixed portal.
    // Contract: hover opens after a short delay, focus opens immediately, moving
    // into the portal keeps it open (some tooltips carry buttons/links), leaving
    // both trigger and portal hides after a short grace, and Escape / outside
    // click / scroll / resize / any modal opening hide it. Only one is open at a
    // time. Hide decisions never depend on trigger identity, because the strip
    // that owns a trigger may re-render (detaching it) while it is hovered.
    const CONFIG_TOOLTIP_EDGE_PAD_PX = 8;
    const CONFIG_TOOLTIP_GAP_PX = 6;
    const CONFIG_TOOLTIP_DEFAULT_WIDTH_PX = 240;
    const CONFIG_TOOLTIP_OPEN_DELAY_MS = 120;
    const CONFIG_TOOLTIP_CLOSE_GRACE_MS = 150;
    const CONFIG_TOOLTIP_PORTAL_ID = 'config-tooltip-portal';
    let _configTooltipTrigger = null;
    let _configTooltipOriginalDescribedBy = '';
    let _configTooltipOpenTimer = 0;
    let _configTooltipCloseTimer = 0;

    function configTooltipPortal() {
      let portal = document.getElementById(CONFIG_TOOLTIP_PORTAL_ID);
      if (!portal) {
        portal = document.createElement('div');
        portal.id = 'config-tooltip-portal';
        portal.className = 'config-tooltip config-tooltip-portal';
        portal.setAttribute('role', 'tooltip');
        portal.hidden = true;
        document.body.appendChild(portal);
      }
      return portal;
    }

    function configTooltipIsOpen() {
      const portal = document.getElementById(CONFIG_TOOLTIP_PORTAL_ID);
      return !!(_configTooltipTrigger && portal && !portal.hidden);
    }

    function configTooltipOwns(node) {
      if (!node || typeof node.nodeType !== 'number') return false;
      if (_configTooltipTrigger && _configTooltipTrigger.contains(node)) return true;
      const portal = document.getElementById(CONFIG_TOOLTIP_PORTAL_ID);
      return !!(portal && !portal.hidden && portal.contains(node));
    }

    function configTooltipHovered() {
      const portal = document.getElementById(CONFIG_TOOLTIP_PORTAL_ID);
      try {
        return !!((_configTooltipTrigger && _configTooltipTrigger.isConnected && _configTooltipTrigger.matches(':hover'))
          || (portal && !portal.hidden && portal.matches(':hover')));
      } catch (_) {
        return false;
      }
    }

    function cancelConfigTooltipOpen() {
      if (_configTooltipOpenTimer) { clearTimeout(_configTooltipOpenTimer); _configTooltipOpenTimer = 0; }
    }

    function cancelConfigTooltipClose() {
      if (_configTooltipCloseTimer) { clearTimeout(_configTooltipCloseTimer); _configTooltipCloseTimer = 0; }
    }

    function positionConfigTooltip(info) {
      if (!info || !info.isConnected) return;
      const sourceTip = info.querySelector('.config-tooltip');
      if (!sourceTip) return;
      cancelConfigTooltipOpen();
      cancelConfigTooltipClose();
      const portal = configTooltipPortal();
      if (_configTooltipTrigger !== info) {
        if (_configTooltipTrigger) hideConfigTooltip();
        _configTooltipTrigger = info;
        _configTooltipOriginalDescribedBy = info.getAttribute('aria-describedby') || '';
      }
      const extraClasses = Array.from(sourceTip.classList).filter(c => c !== 'config-tooltip').join(' ');
      portal.className = ('config-tooltip config-tooltip-portal ' + extraClasses).trim();
      portal.innerHTML = sourceTip.innerHTML;
      portal.hidden = false;
      info.setAttribute('aria-describedby', portal.id);
      const rect = info.getBoundingClientRect();
      const tipW = portal.offsetWidth || CONFIG_TOOLTIP_DEFAULT_WIDTH_PX;
      let left = rect.left + rect.width / 2 - tipW / 2;
      if (left < CONFIG_TOOLTIP_EDGE_PAD_PX) left = CONFIG_TOOLTIP_EDGE_PAD_PX;
      if (left + tipW > window.innerWidth - CONFIG_TOOLTIP_EDGE_PAD_PX) left = window.innerWidth - CONFIG_TOOLTIP_EDGE_PAD_PX - tipW;
      const tipH = portal.offsetHeight || 0;
      const below = rect.bottom + CONFIG_TOOLTIP_GAP_PX;
      const above = rect.top - CONFIG_TOOLTIP_GAP_PX - tipH;
      let top = below;
      if (tipH && below + tipH > window.innerHeight - CONFIG_TOOLTIP_EDGE_PAD_PX && above >= CONFIG_TOOLTIP_EDGE_PAD_PX) top = above;
      if (tipH && top + tipH > window.innerHeight - CONFIG_TOOLTIP_EDGE_PAD_PX) top = Math.max(CONFIG_TOOLTIP_EDGE_PAD_PX, window.innerHeight - CONFIG_TOOLTIP_EDGE_PAD_PX - tipH);
      portal.style.left = left + 'px';
      portal.style.top = top + 'px';
    }

    function scheduleConfigTooltipOpen(info) {
      if (!info) return;
      cancelConfigTooltipOpen();
      if (info === _configTooltipTrigger && configTooltipIsOpen()) { cancelConfigTooltipClose(); return; }
      _configTooltipOpenTimer = setTimeout(() => {
        _configTooltipOpenTimer = 0;
        positionConfigTooltip(info);
      }, CONFIG_TOOLTIP_OPEN_DELAY_MS);
    }

    // Hides whatever is open. Returns true when a tooltip was actually open.
    function hideConfigTooltip() {
      cancelConfigTooltipOpen();
      cancelConfigTooltipClose();
      const wasOpen = configTooltipIsOpen();
      const portal = document.getElementById(CONFIG_TOOLTIP_PORTAL_ID);
      if (portal) portal.hidden = true;
      const info = _configTooltipTrigger;
      if (info) {
        if (_configTooltipOriginalDescribedBy) info.setAttribute('aria-describedby', _configTooltipOriginalDescribedBy);
        else info.removeAttribute('aria-describedby');
      }
      _configTooltipTrigger = null;
      _configTooltipOriginalDescribedBy = '';
      return wasOpen;
    }

    function scheduleConfigTooltipHide() {
      cancelConfigTooltipOpen();
      if (!configTooltipIsOpen()) return;
      cancelConfigTooltipClose();
      _configTooltipCloseTimer = setTimeout(() => {
        _configTooltipCloseTimer = 0;
        if (configTooltipHovered()) return;
        hideConfigTooltip();
      }, CONFIG_TOOLTIP_CLOSE_GRACE_MS);
    }

    function configTooltipPointerOver(target) {
      const info = target && target.closest ? target.closest('.config-info, .contributor-online-trigger') : null;
      if (info) { scheduleConfigTooltipOpen(info); return; }
      if (configTooltipOwns(target)) { cancelConfigTooltipClose(); return; }
      // Pointer is over something else: a sibling, a modal overlay that now
      // covers the trigger, or the strip that replaced a detached trigger.
      scheduleConfigTooltipHide();
    }

    function configTooltipPointerOut(target, relatedTarget) {
      if (configTooltipOwns(relatedTarget)) return;
      if (relatedTarget && relatedTarget.closest && relatedTarget.closest('.config-info, .contributor-online-trigger')) return;
      const info = target && target.closest ? target.closest('.config-info, .contributor-online-trigger') : null;
      if (info || configTooltipOwns(target)) scheduleConfigTooltipHide();
    }

    function configTooltipFocusIn(target) {
      const info = target && target.closest ? target.closest('.config-info, .contributor-online-trigger') : null;
      if (info) { positionConfigTooltip(info); return; }
      if (configTooltipOwns(target)) { cancelConfigTooltipClose(); return; }
      if (configTooltipIsOpen()) hideConfigTooltip();
    }

    function configTooltipFocusOut(target, relatedTarget) {
      if (configTooltipOwns(relatedTarget)) return;
      if (configTooltipOwns(target)) scheduleConfigTooltipHide();
    }

    document.addEventListener('mouseover', (e) => configTooltipPointerOver(e.target));
    document.addEventListener('mouseout', (e) => configTooltipPointerOut(e.target, e.relatedTarget));
    document.addEventListener('focusin', (e) => configTooltipFocusIn(e.target));
    document.addEventListener('focusout', (e) => configTooltipFocusOut(e.target, e.relatedTarget));
    document.addEventListener('click', (e) => { if (configTooltipIsOpen() && !configTooltipOwns(e.target)) hideConfigTooltip(); });
    window.addEventListener('scroll', (e) => { if (configTooltipIsOpen() && !configTooltipOwns(e.target)) hideConfigTooltip(); }, true);
    window.addEventListener('resize', () => { if (configTooltipIsOpen()) hideConfigTooltip(); });

    // ── Event delegation for actions with user-controlled data ──
    // Eliminates XSS via inline data-action="func" data-arg0="${unsafeValue}" injection.
    // All agent/group names are read from data-* attributes (HTML-decoded by the browser).
    //
    // #3848: this dispatcher is now GENERIC and covers every event type the UI
    // uses (click, change, input, keydown, keyup, mouseover, mouseout, scroll,
    // toggle, error), so no inline on*= handler attribute remains and CSP
    // script-src-attr is 'none'. An action resolves, in order, to:
    //   1. an entry in HIVE_GEN_HANDLERS (multi-statement former inline bodies), or
    //   2. a top-level function of the same name (window[action]).
    // Arguments come from data-arg0..data-argN. data-arg-types (comma-separated)
    // maps each position: s=string (default), j=JSON (number/bool/null),
    // t=element, e=event, v=element.value, w=element.value.trim(),
    // N=+element.value, V=value?Number(value):undefined, c=element.checked,
    // o=element.open. data-stop/data-prevent call stopPropagation/
    // preventDefault. For key events, data-keys lists accepted event.key values
    // ('*' = any; default 'Enter').
    function hiveResolveActionArgs(el, event) {
      const types = (el.dataset.argTypes || '').split(',').map(t => t.trim()).filter(Boolean);
      let n = types.length;
      while (el.dataset['arg' + n] !== undefined) n++;
      for (let i = 0; i < n; i++) if (!types[i]) types[i] = 's';
      return types.map((t, i) => {
        const raw = el.dataset['arg' + i];
        switch (t) {
          case 't': return el;
          case 'e': return event;
          case 'v': return el.value;
          case 'w': return (el.value || '').trim();
          case 'N': return +el.value;
          case 'V': return el.value ? Number(el.value) : undefined;
          case 'c': return el.checked;
          case 'o': return el.open;
          case 'j': try { return JSON.parse(raw); } catch { return raw; }
          default: return raw;
        }
      });
    }
    function hiveDispatchAction(attr, event) {
      const el = event.target.closest ? event.target.closest('[data-' + attr + ']') : null;
      if (!el) return false;
      if ((attr === 'mouseover-action' || attr === 'mouseout-action') && event.relatedTarget && el.contains(event.relatedTarget)) return false;
      const camel = attr.replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      const action = el.dataset[camel];
      if (!action) return false;
      if ((attr === 'keydown-action' || attr === 'keyup-action')) {
        const keys = el.dataset.keys || 'Enter';
        if (keys !== '*' && !keys.split(',').some(k => (k === '' ? ',' : k) === event.key)) return false;
      }
      const fn = (typeof HIVE_GEN_HANDLERS !== 'undefined' && HIVE_GEN_HANDLERS[action]) || window[action];
      if (typeof fn !== 'function') return false;
      if (el.dataset.stop === '1') event.stopPropagation();
      if (el.dataset.prevent === '1') event.preventDefault();
      fn.apply(el, HIVE_GEN_HANDLERS[action] ? [event, hiveResolveActionArgs(el, event)] : hiveResolveActionArgs(el, event));
      return true;
    }
    document.addEventListener('click', (e) => {
      const el = e.target.closest('[data-action]');
      if (!el) return;
      const action = el.dataset.action;
      if (action === 'toggleSection' && e.target.closest('[data-public-knowledge-control]')) return;
      const agent = el.dataset.agent || '';
      switch (action) {
        case 'toggleAgent': toggleAgent(agent, el.dataset.paused === 'true'); break;
        case 'startResumeAgent': startResumeAgent(agent); break;
        case 'restartAgent': restartAgent(agent); break;
        case 'openConfigDialog': { const ct = el.dataset.configType || el.dataset.arg0 || 'agent'; const tab = el.dataset.tab || el.dataset.arg2 || null; if (el.classList.contains('agent-link')) { closeConfigDialog(); setTimeout(() => openConfigDialog(ct, agent, tab), 100); } else { openConfigDialog(ct, agent || null, tab); } } break;
        case 'ocSendKick': ocSendKick(agent); break;
        case 'ocSelectAgent': { if (!e.target.closest('.oc-nav-actions')) ocSelectAgent(agent); } break;
        case 'openAgentCollapsedTile': openAgentCollapsedTile(agent); break;
        case 'kick': kick(agent); break;
        case 'kickEmpty': { const inp = document.getElementById('kick-prompt-' + agent); if (inp) inp.value = ''; kick(agent); } break;
        case 'openTerminal': e.preventDefault(); openTerminal(agent, el.href); break;
        case 'openAuthenticatedLink': e.preventDefault(); openAuthenticatedLink(el.href, el.hasAttribute('download')); break;
        case 'copyTerminalUrl': e.stopPropagation(); copyTerminalUrl(agent); break;
        case 'submitLoginCode': e.stopPropagation(); submitLoginCode(agent); break;
        case 'showTerminalKeyboardHint': e.stopPropagation(); showTerminalKeyboardHint(); break;
        case 'togglePin': togglePin(agent, el.dataset.pinType, el.dataset.unpin === 'true'); break;
        case 'resetRestarts': resetRestarts(agent); break;
        case 'dismissSystemAlert': dismissSystemAlert(el.dataset.arg0 || ''); break;
        case 'deleteAgentFromConfig': deleteAgentFromConfig(agent); break;
        case 'ocRenameSidebarGroup': ocRenameSidebarGroup(el.dataset.group); break;
        case 'ocRemoveSidebarGroup': ocRemoveSidebarGroup(el.dataset.group); break;
        case 'ocToggleSidebarGroup': ocToggleSidebarGroup(el); break;
        case 'switchConfigTab': switchConfigTab(el.dataset.tab || el.dataset.arg0); break;
        case 'applyCadenceTemplate': applyCadenceTemplate(el.dataset.arg0 || el.dataset.template); break;
        case 'nousSetMode': nousSetMode(el.dataset.mode || el.dataset.arg0); break;
        case 'nousOpenNewExperiment': nousOpenNewExperiment(); break;
        case 'nousEditExperiment': nousEditExperiment(el.dataset.arg0); break;
        case 'nousGenerateProposals': nousGenerateProposals(el.dataset.arg0); break;
        case 'nousCancelNewExperiment': nousCancelNewExperiment(); break;
        case 'nousSubmitNewExperiment': nousSubmitNewExperiment(); break;
        case 'nousNarrowScope': nousNarrowScope(); break;
        case 'nousPauseConflictAndStart': nousPauseConflictAndStart(el.dataset.arg0); break;
        case 'nousCampaignAction': nousCampaignAction(el.dataset.arg0, el.dataset.arg1); break;
        case 'toggleConfigSwitch': toggleConfigSwitch(el, el.dataset.section, el.dataset.key); break;
        case 'toggleContributorPRBaseSync': toggleContributorPRBaseSync(el); break;
        case 'toggleReviewRequireLabels': toggleReviewRequireLabels(el); break;
        case 'toggleSentinelEnabled': toggleSentinelEnabled(el); break;
        case 'toggleSentinelBehavior': toggleSentinelBehavior(el); break;
        case 'toggleSentinelTrustedAuthorsBlock': toggleSentinelTrustedAuthorsBlock(el); break;
        case 'resetSentinelPaths': resetSentinelPaths(); break;
        case 'wsWavefrontSetEnabled': wsWavefrontSetEnabled(el); break;
        case 'toggleAgentEnabled': setAgentEnabled(el.dataset.agent, el.dataset.enabled !== 'true'); break;
        case 'loadConvergenceSoak': loadConvergenceSoak(); break;
        case 'loadIntegrationOrchestration': loadIntegrationOrchestration(); break;
        case 'loadReviewOutcomes': loadReviewOutcomes(); break;
        case 'loadLinearAgentStatus': loadLinearAgentStatus(); break;
        case 'loadUpstreamWatch': loadUpstreamWatch(); break;
        case 'upstreamWatchToggle': upstreamWatchToggle(el); break;
        case 'upstreamWatchSaveInterval': upstreamWatchSaveInterval(); break;
        case 'upstreamWatchAddFork': upstreamWatchAddFork(); break;
        case 'upstreamWatchSaveRepo': upstreamWatchSaveRepo(el); break;
        case 'upstreamWatchRemoveRepo': upstreamWatchRemoveRepo(el); break;
        case 'linearAgentConnect': linearAgentConnect(); break;
        case 'linearAgentDisconnect': linearAgentDisconnect(); break;
        case 'toggleEscalationEnabled': toggleEscalationEnabled(el); break;
        case 'removeListItem': removeListItem(el.dataset.section || el.dataset.arg0, el.dataset.key || el.dataset.arg1, parseInt(el.dataset.index !== undefined ? el.dataset.index : el.dataset.arg2, 10)); break;
        case 'toggleRestriction': { if (el.dataset.index !== undefined) { const cb = el.querySelector('input[type=checkbox]') || el; toggleRestriction(parseInt(el.dataset.index, 10), cb.checked); } else { toggleRestriction(parseInt(el.dataset.arg0, 10), el.checked); } } break;
        case 'removeRestriction': removeRestriction(parseInt(el.dataset.index !== undefined ? el.dataset.index : el.dataset.arg0, 10)); break;
        case 'removeAgent': removeAgent(agent); break;
        case 'toggleACMMRepoPin': toggleACMMRepoPin(el.dataset.repo || '', el.dataset.pinned === 'true'); break;
        case 'planIssue': planIssue(el); break;
        case 'bobKeySet': case 'bobKeyClear': case 'bobKeyTest': break; // direct listeners attached at render time
        default: hiveDispatchAction('action', e); break;
      }
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        const el = e.target.closest('[data-enter-action]');
        if (el) {
          const agent = el.dataset.agent || '';
          switch (el.dataset.enterAction) {
            case 'ocSendKick': ocSendKick(agent); return;
            case 'kick': kick(agent); return;
            default: {
              const fn = window[el.dataset.enterAction];
              if (typeof fn === 'function') {
                if (el.dataset.prevent === '1') e.preventDefault();
                fn.apply(el, hiveResolveActionArgs(el, e));
                return;
              }
            }
          }
        }
      }
      hiveDispatchAction('keydown-action', e);
    });
    document.addEventListener('change', (e) => {
      const el = e.target.closest('[data-change-action]');
      if (!el) return;
      const action = el.dataset.changeAction;
      const agent = el.dataset.agent || '';
      switch (action) {
        case 'switchCli': switchCli(agent, el.value); el.selectedIndex = 0; break;
        case 'switchModel': switchModel(agent, el.value); el.selectedIndex = 0; break;
        case 'switchEffort': switchEffort(agent, el.value); el.selectedIndex = 0; break;
        default: hiveDispatchAction('change-action', e); break;
      }
    });
    document.addEventListener('input', (e) => hiveDispatchAction('input-action', e));
    document.addEventListener('keyup', (e) => hiveDispatchAction('keyup-action', e));
    document.addEventListener('mouseover', (e) => hiveDispatchAction('mouseover-action', e));
    document.addEventListener('mouseout', (e) => hiveDispatchAction('mouseout-action', e));
    // scroll/toggle/error do not bubble — delegate via the capture phase.
    document.addEventListener('scroll', (e) => hiveDispatchAction('scroll-action', e), true);
    document.addEventListener('toggle', (e) => hiveDispatchAction('toggle-action', e), true);
    document.addEventListener('error', (e) => hiveDispatchAction('error-action', e), true);
    // Expose every shared on/off switch as role=switch with a synced aria-checked and Space/Enter support.
    (function() {
      const SEL = '.config-toggle-switch';
      let queued = false;
      function sync() {
        queued = false;
        document.querySelectorAll(SEL).forEach((el) => {
          if (!el.hasAttribute('role')) el.setAttribute('role', 'switch');
          if (el.getAttribute('role') !== 'switch') return;
          if (!el.hasAttribute('tabindex') && !el.classList.contains('disabled')) el.setAttribute('tabindex', '0');
          const on = el.classList.contains('on') ? 'true' : 'false';
          if (el.getAttribute('aria-checked') !== on) el.setAttribute('aria-checked', on);
          if (el.classList.contains('disabled')) el.setAttribute('aria-disabled', 'true'); else el.removeAttribute('aria-disabled');
        });
      }
      function queue() { if (!queued) { queued = true; requestAnimationFrame(sync); } }
      new MutationObserver(queue).observe(document.body, { subtree: true, childList: true, attributes: true, attributeFilter: ['class'] });
      document.addEventListener('keydown', (e) => {
        if (e.key !== ' ' && e.key !== 'Enter') return;
        const el = e.target && e.target.closest ? e.target.closest(SEL) : null;
        if (!el || el.hasAttribute('data-keydown-action') || el.classList.contains('disabled')) return;
        e.preventDefault();
        el.click();
      });
      queue();
    })();

    const TOAST_DURATION_MS = 3000;
    const TOAST_PROGRESS_MAX_MS = 60000;
    /* Model add/remove notices: informational errors that self-clear — long
       enough to read, never a permanent sticky per model. */
    const TOAST_MODEL_CHANGE_MS = 20000;
    function hiveDialogEscape(value) {
      return String(value == null ? '' : value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }
    function hiveNormalizeDialogOptions(first, fallbackTitle) {
      if (first && typeof first === 'object') return Object.assign({}, first);
      return { message: String(first == null ? '' : first), title: fallbackTitle || '' };
    }
    function hiveConfirmIsDanger(cfg, text) {
      if (cfg.danger === true) return true;
      if (cfg.danger === false) return false;
      return /(^|\b)(delete|remove|clear|forget|revoke|deny|reset|unsubscribe)\b/i.test(String((cfg.title || '') + ' ' + text));
    }
    // Shared long-content scroll region (.modal-scroll). The fade affordance
    // (.is-scrollable-more) is on while content remains below the fold; it
    // clears at the bottom so the user knows they have reached the end.
    const HIVE_MODAL_SCROLL_EPS = 2;
    function hiveModalScrollSync(el) {
      if (!el || !el.classList) return false;
      const remaining = (el.scrollHeight || 0) - (el.clientHeight || 0) - (el.scrollTop || 0);
      const more = remaining > HIVE_MODAL_SCROLL_EPS;
      el.classList.toggle('is-scrollable-more', more);
      return more;
    }
    function hiveModalScrollInit(el) {
      if (!el || !el.classList) return el;
      if (!el.classList.contains('modal-scroll')) el.classList.add('modal-scroll');
      if (el.dataset && el.dataset.modalScrollWired) { hiveModalScrollSync(el); return el; }
      if (el.dataset) el.dataset.modalScrollWired = '1';
      const sync = () => hiveModalScrollSync(el);
      el.addEventListener('scroll', sync, { passive: true });
      // Content arrives async (release notes fetch) and hidden modals have no
      // layout until opened: re-check whenever size or children change.
      if (typeof ResizeObserver === 'function') new ResizeObserver(sync).observe(el);
      if (typeof MutationObserver === 'function') new MutationObserver(sync).observe(el, { childList: true, subtree: true, characterData: true });
      if (typeof window !== 'undefined' && window.addEventListener) window.addEventListener('resize', sync, { passive: true });
      sync();
      return el;
    }
    // On narrow viewports a .hive-dialog with a .modal-scroll body becomes a
    // full-screen sheet (CSS: .hive-dialog.modal-sheet). The sheet gets a ✕ in
    // the header sized to the mobile tap target; desktop hides it.
    function hiveModalSheetDecorate(overlay, onClose) {
      if (!overlay || !overlay.querySelectorAll) return;
      const scrollers = Array.from(overlay.querySelectorAll('.modal-scroll'));
      if (!scrollers.length) return;
      scrollers.forEach(hiveModalScrollInit);
      const dlg = overlay.querySelector('.hive-dialog') || overlay;
      dlg.classList.add('modal-sheet');
      if (overlay !== dlg && overlay.classList) overlay.classList.add('has-modal-sheet');
      if (dlg.querySelector('.modal-sheet-close')) return;
      const x = document.createElement('button');
      x.type = 'button';
      x.className = 'modal-sheet-close';
      x.setAttribute('aria-label', 'Close');
      x.textContent = '✕';
      x.addEventListener('click', () => { if (typeof onClose === 'function') onClose(); });
      dlg.insertBefore(x, dlg.firstChild);
    }
    function hiveWireDialog(overlay, resolve, cancelValue, okSelector, cancelSelector, submit) {
      let done = false;
      const focusableSelector = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';
      const close = (val) => {
        if (done) return;
        done = true;
        document.removeEventListener('keydown', onKey, true);
        overlay.remove();
        resolve(val);
      };
      overlay.hiveDialogClose = () => close(cancelValue);
      const onKey = (e) => {
        if (e.key === 'Escape') { e.preventDefault(); close(cancelValue); return; }
        if (e.key === 'Enter') {
          const tag = e.target && e.target.tagName ? e.target.tagName.toLowerCase() : '';
          if (tag !== 'textarea') { e.preventDefault(); submit(close); }
          return;
        }
        if (e.key !== 'Tab') return;
        const nodes = Array.from(overlay.querySelectorAll(focusableSelector)).filter(el => !el.disabled && el.offsetParent !== null);
        if (!nodes.length) return;
        const first = nodes[0];
        const last = nodes[nodes.length - 1];
        if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
        else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
      };
      overlay.addEventListener('click', (e) => { if (e.target === overlay) close(cancelValue); });
      const ok = overlay.querySelector(okSelector);
      const cancel = cancelSelector ? overlay.querySelector(cancelSelector) : null;
      if (ok) ok.addEventListener('click', () => submit(close));
      if (cancel) cancel.addEventListener('click', () => close(cancelValue));
      document.addEventListener('keydown', onKey, true);
      if (typeof hiveModalSheetDecorate === 'function') hiveModalSheetDecorate(overlay, () => close(cancelValue));
      const initial = overlay.querySelector('[data-initial-focus]') || ok || cancel;
      if (initial) setTimeout(() => { initial.focus(); if (initial.select) initial.select(); }, 0);
      return close;
    }
    // cfg.check = { label } adds a checkbox under the body (a "don't show this
    // again" opt-out); the promise then resolves { checked } instead of true
    // so the caller can persist the choice. Without cfg.check it is true/false.
    function hiveConfirm(opts, title) {
      const cfg = hiveNormalizeDialogOptions(opts, title);
      return new Promise(resolve => {
        const overlay = document.createElement('div');
        overlay.className = 'hive-dialog-overlay';
        hiveRememberModalOpen(overlay);
        const message = cfg.message || cfg.body || '';
        const parts = String(message).split('\n\n');
        const heading = cfg.title || (parts.length > 1 ? parts[0] : 'Confirm');
        const body = cfg.title ? message : (parts.length > 1 ? parts.slice(1).join('\n\n') : message);
        const confirmLabel = cfg.confirmLabel || cfg.ok || 'OK';
        const check = cfg.check && typeof cfg.check === 'object' ? cfg.check : null;
        overlay.innerHTML = `<div class="hive-dialog" role="dialog" aria-modal="true" aria-labelledby="hive-dialog-title">
          <div id="hive-dialog-title" class="hive-dialog-title">${hiveDialogEscape(heading)}</div>
          <div class="hive-dialog-body">${hiveDialogEscape(body)}</div>
          ${check ? `<label class="hive-dialog-check"><input type="checkbox" class="hive-dialog-check-input"${check.checked ? ' checked' : ''}>${hiveDialogEscape(check.label || 'Don\'t show this again')}</label>` : ''}
          <div class="hive-dialog-buttons">
            <button type="button" class="hv-btn btn-secondary hive-dialog-btn cancel">${hiveDialogEscape(cfg.cancelLabel || 'Cancel')}</button>
            <button type="button" class="hv-btn ${hiveConfirmIsDanger(cfg, message) ? 'btn-danger' : 'btn-primary'} hive-dialog-btn primary" data-initial-focus>${hiveDialogEscape(confirmLabel)}</button>
          </div></div>`;
        document.body.appendChild(overlay);
        const box = check ? overlay.querySelector('.hive-dialog-check-input') : null;
        hiveWireDialog(overlay, resolve, false, '.primary', '.cancel', close => close(check ? { checked: !!(box && box.checked) } : true));
      });
    }
    // cfg.check = { label } adds the same opt-out box as hiveConfirm; the
    // promise then resolves { value, checked } instead of the string.
    function hivePrompt(opts, defaultValue, legacyOpts) {
      const cfg = (opts && typeof opts === 'object') ? Object.assign({}, opts) : Object.assign({}, legacyOpts || {}, { title: String(opts || ''), defaultValue: defaultValue });
      const check = cfg.check && typeof cfg.check === 'object' ? cfg.check : null;
      return new Promise(resolve => {
        const overlay = document.createElement('div');
        overlay.className = 'hive-dialog-overlay';
        hiveRememberModalOpen(overlay);
        const id = 'hive-prompt-' + Math.random().toString(36).slice(2);
        const errId = id + '-error';
        overlay.innerHTML = `<div class="hive-dialog" role="dialog" aria-modal="true" aria-labelledby="${id}-title">
          <div id="${id}-title" class="hive-dialog-title">${hiveDialogEscape(cfg.title || 'Input')}</div>
          <label for="${id}" class="hive-dialog-label">${hiveDialogEscape(cfg.label || cfg.message || cfg.title || '')}</label>
          <input id="${id}" class="hive-dialog-input" type="text" value="${hiveDialogEscape(cfg.defaultValue || '')}" placeholder="${hiveDialogEscape(cfg.placeholder || '')}" autocomplete="off"${Array.isArray(cfg.options) && cfg.options.length ? ' list="' + id + '-list"' : ''} aria-describedby="${errId}" data-initial-focus>
          ${Array.isArray(cfg.options) && cfg.options.length ? '<datalist id="' + id + '-list">' + cfg.options.map(o => '<option value="' + hiveDialogEscape(o.value) + '">' + hiveDialogEscape(o.label || '') + '</option>').join('') + '</datalist>' : ''}
          <div id="${errId}" class="hive-dialog-error" role="alert"></div>
          ${check ? `<label class="hive-dialog-check"><input type="checkbox" class="hive-dialog-check-input"${check.checked ? ' checked' : ''}>${hiveDialogEscape(check.label || 'Don\'t ask again')}</label>` : ''}
          <div class="hive-dialog-buttons">
            <button type="button" class="hv-btn btn-secondary hive-dialog-btn cancel">Cancel</button>
            <button type="button" class="hv-btn btn-primary hive-dialog-btn primary">${hiveDialogEscape(cfg.confirmLabel || cfg.ok || 'OK')}</button>
          </div></div>`;
        document.body.appendChild(overlay);
        const input = overlay.querySelector('#' + id);
        const err = overlay.querySelector('#' + errId);
        const box = check ? overlay.querySelector('.hive-dialog-check-input') : null;
        hiveWireDialog(overlay, resolve, null, '.primary', '.cancel', close => {
          const value = input ? input.value.trim() : '';
          if (typeof cfg.validate === 'function') {
            const verdict = cfg.validate(value);
            if (verdict === false || typeof verdict === 'string') {
              if (err) err.textContent = typeof verdict === 'string' ? verdict : 'Please enter a valid value.';
              if (input) input.focus();
              return;
            }
          }
          close(check ? { value, checked: !!(box && box.checked) } : value);
        });
      });
    }
    function hiveAlert(message, title) {
      const text = String(message == null ? '' : message);
      if (typeof showToast === 'function' && text.length <= 220 && text.indexOf('\n') === -1) {
        showToast(text, 'info');
        return Promise.resolve();
      }
      return new Promise(resolve => {
        const overlay = document.createElement('div');
        overlay.className = 'hive-dialog-overlay';
        hiveRememberModalOpen(overlay);
        overlay.innerHTML = `<div class="hive-dialog" role="dialog" aria-modal="true" aria-labelledby="hive-alert-title">
          <div id="hive-alert-title" class="hive-dialog-title">${hiveDialogEscape(title || 'Notice')}</div>
          <div class="hive-dialog-body long modal-scroll">${hiveDialogEscape(text)}</div>
          <div class="hive-dialog-buttons">
            <button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-initial-focus>OK</button>
          </div></div>`;
        document.body.appendChild(overlay);
        hiveWireDialog(overlay, resolve, undefined, '.primary', null, close => close());
      });
    }

    function hiveFilenameFromDisposition(header, fallback) {
      const m = String(header || '').match(/filename="?([^";]+)"?/i);
      return (m && m[1]) ? m[1] : (fallback || 'hive-export.txt');
    }
    function hiveExportViewer(opts) {
      const cfg = opts || {};
      const title = cfg.title || cfg.filename || 'Export';
      const text = String(cfg.text == null ? '' : cfg.text);
      const overlay = document.createElement('div');
      overlay.className = 'hive-dialog-overlay';
      hiveRememberModalOpen(overlay);
      overlay.innerHTML = `<div class="hive-dialog hive-export-dialog" role="dialog" aria-modal="true" aria-labelledby="hive-export-title">
        <div id="hive-export-title" class="hive-dialog-title"></div>
        <div class="hive-dialog-body long modal-scroll hive-export-body">
          <div class="cmp-note hive-export-note"></div>
          <textarea class="hive-export-text" readonly spellcheck="false"></textarea>
        </div>
        <div class="hive-dialog-buttons">
          <a class="hv-btn btn-secondary hive-export-open" target="_blank" rel="noopener noreferrer" hidden>Open in new tab</a>
          <button type="button" class="hv-btn btn-secondary hive-export-copy">Copy to clipboard</button>
          <button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-initial-focus>Close</button>
        </div></div>`;
      overlay.querySelector('#hive-export-title').textContent = title;
      const note = overlay.querySelector('.hive-export-note');
      const textarea = overlay.querySelector('.hive-export-text');
      textarea.value = text;
      const sourceURL = cfg.sourceURL || '';
      const token = localStorage.getItem('hive-token') || '';
      const canOpen = !!(sourceURL && (cfg.allowOpen || !token));
      if (note) note.textContent = sourceURL && token && !cfg.allowOpen ? 'Rendered in-app because this dashboard uses a bearer token that a new tab would not receive.' : 'Rendered in-app so the browser does not prompt for downloads.';
      const open = overlay.querySelector('.hive-export-open');
      if (open && canOpen) { open.href = sourceURL; open.hidden = false; }
      overlay.querySelector('.hive-export-copy').addEventListener('click', function(e) {
        e.preventDefault();
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(text).then(function(){ showToast('Copied ' + title + ' to clipboard', 'success'); }, function(){ textarea.focus(); textarea.select(); showToast('Clipboard blocked — selected text for manual copy', 'error'); });
        } else {
          textarea.focus(); textarea.select();
          showToast('Selected text for manual copy', 'info');
        }
      });
      document.body.appendChild(overlay);
      hiveWireDialog(overlay, function(){}, undefined, '.primary', null, close => close());
    }
    function hiveBinaryExportNotice(name, detail) {
      hiveAlert((detail || 'This export is binary and cannot be rendered safely in the dashboard.') + '\n\nBrowser downloads are disabled by policy to avoid native permission prompts. Use the API or CLI from a trusted shell if you need to save this file.', name || 'Binary export');
    }
    async function hiveOpenTextEndpoint(url, fallbackName) {
      try {
        const headers = {};
        const token = localStorage.getItem('hive-token') || '';
        if (token) headers.Authorization = 'Bearer ' + token;
        const resp = await fetch(url, { credentials: 'same-origin', cache: 'no-store', headers });
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        const cd = resp.headers.get('Content-Disposition') || '';
        const name = hiveFilenameFromDisposition(cd, fallbackName || 'hive-export.txt');
        const text = await resp.text();
        const sourceURL = /^attachment/i.test(cd) ? '' : url;
        hiveExportViewer({title:name,filename:name,text:text,mime:resp.headers.get('Content-Type') || '',sourceURL:sourceURL});
      } catch (err) {
        showToast('Export failed: ' + (err && err.message ? err.message : err), 'error');
      }
    }
    function maybeShowLevelAutoMergeActiveModal(data) {
      if (!data || data.self_merge_sweep_active !== true) return Promise.resolve();
      const level = Number(data.level || 6);
      const stamp = String(data.level_changed_at || data.minStatusSeq || Date.now());
      const storageKey = `hive:l6-automerge-info:${level}:${stamp}`;
      try {
        if (localStorage.getItem(storageKey) === '1') return Promise.resolve();
      } catch (_) {}
      return new Promise(resolve => {
        const overlay = document.createElement('div');
        overlay.className = 'hive-dialog-overlay';
        const holds = Array.isArray(data.level_holds_pending) ? data.level_holds_pending : [];
        const repos = Array.isArray(data.repos) ? data.repos : [];
        const warningHTML = data.level_holds_warning ? `<p><strong>Hold inspection warning:</strong> ${hiveDialogEscape(data.level_holds_warning)}</p>` : '';
        const heldHTML = holds.length
          ? `<div class="level-automerge-list">${holds.map(pr => {
              const repo = hiveDialogEscape(pr.repo || '');
              const number = Number(pr.number || 0);
              const title = hiveDialogEscape(pr.title || '(untitled PR)');
              const url = hiveDialogEscape(pr.url || '');
              const agent = pr.agent ? ` · agent/${hiveDialogEscape(pr.agent)}` : '';
              const link = url ? `<a href="${url}" target="_blank" rel="noopener noreferrer">${repo}#${number}</a>` : `${repo}#${number}`;
              return `<div class="level-automerge-row"><div class="level-automerge-row-main"><span class="level-automerge-row-title">${title}</span><span class="level-automerge-row-meta">${link}${agent}</span></div></div>`;
            }).join('')}</div>`
          : '<p>No currently held App-authored PRs were found.</p>';
        const repoHTML = repos.length
          ? `<div class="level-automerge-list">${repos.map((repo, i) => {
              const name = hiveDialogEscape(repo.repo || '');
              const enabled = repo.auto_merge !== false;
              return `<div class="level-automerge-row" data-level-automerge-repo="${name}">
                <div class="level-automerge-row-main"><span class="level-automerge-row-title">${name}</span><span class="level-automerge-row-meta">Auto-merge ${enabled ? 'enabled' : 'disabled'}</span></div>
                <label class="config-toggle repo-automerge-toggle" title="${enabled ? 'Disable Hive auto-merge for this repo' : 'Enable Hive auto-merge for this repo'}"><span class="config-toggle-label">auto-merge</span><span role="switch" tabindex="0" aria-checked="${enabled ? 'true' : 'false'}" aria-label="${'auto-merge for ' + name}" class="config-toggle-switch repo-automerge-switch level-automerge-toggle ${enabled ? 'on' : ''}" data-repo-index="${i}" data-enabled="${enabled ? '1' : '0'}"></span></label>
              </div>`;
            }).join('')}</div>`
          : '<p>No active repositories are configured.</p>';
        overlay.innerHTML = `<div class="hive-dialog level-automerge-dialog" role="dialog" aria-modal="true" aria-labelledby="level-automerge-title">
          <div id="level-automerge-title" class="hive-dialog-title">Level 6 auto-merge is now active</div>
          <div class="hive-dialog-body long modal-scroll">
            <p>PRs currently labelled <code>hold</code> stay held, including holds Hive applied at the previous level. Hive never removes <code>hold</code> on a level change. Remove <code>hold</code> yourself, or use the PR's Release affordance if available, before L6 can merge those PRs automatically.</p>
            ${warningHTML}
            ${heldHTML}
            <p>Switching to Level 6 turns auto-merge on for all active repositories. Switch off any repository that should not participate in L6 auto-merge.</p>
            ${repoHTML}
          </div>
          <div class="hive-dialog-buttons">
            <button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-initial-focus>Got it</button>
          </div></div>`;
        document.body.appendChild(overlay);
        const markDismissed = () => {
          try { localStorage.setItem(storageKey, '1'); } catch (_) {}
        };
        overlay.querySelectorAll('.level-automerge-toggle').forEach(btn => {
          btn.addEventListener('keydown', e => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              btn.click();
            }
          });
          btn.addEventListener('click', async () => {
            const idx = Number(btn.dataset.repoIndex || -1);
            const repo = repos[idx];
            if (!repo || btn.dataset.busy === '1') return;
            const nextEnabled = btn.dataset.enabled !== '1';
            btn.dataset.busy = '1';
            btn.classList.toggle('on', nextEnabled);
            btn.setAttribute('aria-checked', nextEnabled ? 'true' : 'false');
            try {
              const resp = await fetch('/api/repos/auto-merge', {
                method: 'POST',
                headers: _inceptionAuthHeaders(),
                body: JSON.stringify({ repo: repo.repo, enabled: nextEnabled })
              });
              const resBody = await resp.json().catch(() => ({}));
              if (!resp.ok) throw new Error(resBody.error || ('HTTP ' + resp.status));
              repo.auto_merge = nextEnabled;
              btn.dataset.enabled = nextEnabled ? '1' : '0';
              btn.classList.toggle('on', nextEnabled);
              btn.setAttribute('aria-checked', nextEnabled ? 'true' : 'false');
              const row = btn.closest('.level-automerge-row');
              const meta = row && row.querySelector('.level-automerge-row-meta');
              if (meta) meta.textContent = 'Auto-merge ' + (nextEnabled ? 'enabled' : 'disabled');
              showToast((nextEnabled ? 'Enabled' : 'Disabled') + ' auto-merge for ' + repo.repo, 'success');
              noteStatusMutation(resBody.minStatusSeq);
              fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
            } catch (e) {
              showToast('Auto-merge toggle failed: ' + (e && e.message ? e.message : 'network error'), 'error');
              btn.classList.toggle('on', !nextEnabled);
              btn.setAttribute('aria-checked', !nextEnabled ? 'true' : 'false');
            } finally {
              btn.dataset.busy = '';
            }
          });
        });
        hiveWireDialog(overlay, () => { markDismissed(); resolve(); }, undefined, '.primary', null, close => close());
      });
    }
    // makeToastSticky turns a toast into one that never auto-dismisses. Used
    // for errors: a failed save may carry a validation message the user has
    // not finished reading, so it waits for an explicit click or × instead of
    // vanishing on a timer. Success/info toasts keep their timeout.
    function makeToastSticky(el) {
      if (el._sticky) return;
      el._sticky = true;
      el.classList.add('sticky');
      const close = document.createElement('button');
      close.className = 'toast-close';
      close.textContent = '×';
      close.setAttribute('aria-label', 'Dismiss');
      el.appendChild(close);
      el.addEventListener('click', () => dismissToast(el, null, null, true));
    }
    // toastCountdownBar arms a toast's auto-dismiss: one timer that closes
    // it after `ms`, and a .toast-bar whose drain animation runs for the
    // same `ms` (published on the toast as --toast-ttl), so the bar hits
    // zero as the toast leaves. Every timed toast path (success/info/
    // warning, error-with-deadline, in-flight toast resolved by
    // dismissToast) goes through here; sticky and persistent toasts never
    // call it and so show no bar.
    //
    // Hover or keyboard focus inside the toast holds the countdown: the
    // timer is cleared with its remaining milliseconds banked and the bar's
    // animation is paused at the same instant, so the two never drift.
    // Leaving re-arms the timer for exactly the banked remainder and lets
    // the bar run on from where it stopped. Re-arming (dismissToast
    // rewriting an in-flight toast) cancels any earlier countdown first.
    function toastCountdownBar(el, ms, onExpire) {
      toastCountdownCancel(el);
      const bar = document.createElement('span');
      bar.className = 'toast-bar';
      bar.setAttribute('aria-hidden', 'true');
      el.style.setProperty('--toast-ttl', ms + 'ms');
      el.appendChild(bar);
      const cd = { bar, remaining: ms, startedAt: 0, timer: 0, hovered: false, focused: false };
      el._countdown = cd;
      const fire = () => { cd.timer = 0; el._countdown = null; if (el.parentNode) onExpire(); };
      cd.resume = () => {
        if (cd.timer || cd.hovered || cd.focused || el._countdown !== cd) return;
        cd.startedAt = Date.now();
        cd.timer = setTimeout(fire, cd.remaining);
        bar.style.animationPlayState = 'running';
      };
      cd.pause = () => {
        if (!cd.timer) return;
        clearTimeout(cd.timer);
        cd.timer = 0;
        cd.remaining = Math.max(0, cd.remaining - (Date.now() - cd.startedAt));
        bar.style.animationPlayState = 'paused';
      };
      // A kiosk (#15594) has no pointer; the hidden cursor must never park a
      // toast on screen for the whole stream.
      if (!el._countdownWired && !(window.hiveKioskMode && window.hiveKioskMode())) {
        el._countdownWired = true;
        const c = () => el._countdown;
        el.addEventListener('mouseenter', () => { const x = c(); if (x) { x.hovered = true; x.pause(); } });
        el.addEventListener('mouseleave', () => { const x = c(); if (x) { x.hovered = false; x.resume(); } });
        el.addEventListener('focusin', () => { const x = c(); if (x) { x.focused = true; x.pause(); } });
        el.addEventListener('focusout', e => { const x = c(); if (x && !el.contains(e.relatedTarget)) { x.focused = false; x.resume(); } });
      }
      // A toast re-armed under the pointer stays held until it leaves.
      if (el.matches(':hover')) cd.hovered = true;
      if (el.contains(document.activeElement)) cd.focused = true;
      cd.resume();
      if (!cd.timer) bar.style.animationPlayState = 'paused';
    }
    // toastCountdownCancel drops a toast's pending countdown (timer and
    // bar) so a rewrite or manual dismissal cannot fire a stale timer.
    function toastCountdownCancel(el) {
      const cd = el._countdown;
      if (!cd) return;
      if (cd.timer) clearTimeout(cd.timer);
      cd.timer = 0;
      if (cd.bar && cd.bar.parentNode) cd.bar.remove();
      el._countdown = null;
    }
    function showToast(msg, type = 'info', persistent = false, autoDismissMs = 0) {
      const opts = (persistent && typeof persistent === 'object') ? persistent : {};
      if (persistent && typeof persistent === 'object') persistent = !!opts.persistent;
      if (opts.autoDismissMs !== undefined) autoDismissMs = opts.autoDismissMs;
      const container = document.getElementById('toast-container');
      const el = document.createElement('div');
      el.className = `toast ${type}`;
      el.textContent = msg;
      if (opts.actionLabel && typeof opts.onAction === 'function') {
        const action = document.createElement('button');
        action.type = 'button';
        action.className = 'nav-chip nav-chip--action toast-action';
        action.textContent = opts.actionLabel;
        action.addEventListener('click', (e) => { e.stopPropagation(); opts.onAction(); dismissToast(el, null, null, true); });
        el.appendChild(action);
      }
      container.appendChild(el);
      if (persistent) {
        const spinner = document.createElement('span');
        spinner.className = 'toast-spinner';
        el.prepend(spinner);
        el._persistent = true;
        setTimeout(() => { if (el.parentNode) dismissToast(el); }, TOAST_PROGRESS_MAX_MS);
        return el;
      }
      if (type === 'error') {
        makeToastSticky(el);
        /* autoDismissMs opts an error toast out of stick-forever: it keeps
           the × for early dismissal but self-clears after the deadline
           (forced — dismissToast ignores sticky toasts otherwise). */
        if (autoDismissMs > 0) toastCountdownBar(el, autoDismissMs, () => dismissToast(el, null, null, true));
        return el;
      }
      toastCountdownBar(el, autoDismissMs > 0 ? autoDismissMs : TOAST_DURATION_MS, () => dismissToast(el));
      return el;
    }
    function handleGitHubSetupToast() {
      if (!window.URL || !window.URLSearchParams || !history.replaceState) return;
      const url = new URL(window.location.href);
      const flag = url.searchParams.get('ghSetup');
      if (!flag) return;
      if (flag === 'ok') {
        showToast('GitHub App installed and configured automatically', 'success');
      } else if (flag === 'requested') {
        showToast('Install requested — an org admin must approve; this hive will configure itself once approved', 'info');
      } else {
        showToast('GitHub App setup could not be completed automatically. Check the Installation ID and app org.', 'error');
      }
      url.searchParams.delete('ghSetup');
      history.replaceState({}, document.title, url.pathname + (url.search ? url.search : '') + url.hash);
    }
    handleGitHubSetupToast();
    // dismissToast removes a toast, or (with newMsg) rewrites an in-flight
    // toast into its result. `force` bypasses stickiness for the user-initiated
    // click/× path, so a sticky error can still be dismissed on demand.
    function dismissToast(el, newMsg, newType, force) {
      if (!el || !el.parentNode) return;
      toastCountdownCancel(el);
      if (newMsg) {
        el.textContent = newMsg;
        el.className = `toast ${newType || 'success'}`;
        el._sticky = false;
        // A resolved in-flight toast that resolved to an error stays put for
        // the same reason a direct error toast does.
        if (newType === 'error') { makeToastSticky(el); return; }
        toastCountdownBar(el, TOAST_DURATION_MS, () => { el.style.animation = 'toast-out 0.3s ease-in forwards'; setTimeout(() => el.remove(), 300); });
      } else {
        if (el._sticky && !force) return;
        el.style.animation = 'toast-out 0.3s ease-in forwards';
        setTimeout(() => el.remove(), 300);
      }
    }

    // postJSON POSTs to an API endpoint and returns its decoded JSON body.
    //
    // The endpoints we control always answer JSON, but an intermediary need
    // not: an auth proxy can answer a session-expiry redirect, an ingress can
    // answer a 502/504, and both bodies are HTML. Calling res.json() on those
    // throws a parser error ("Unexpected token '<'"), and the caller's catch
    // then reports the parser's complaint instead of the actual failure —
    // hiding whether the fix is "sign in again" or "the spoke is unreachable".
    //
    // So sniff the content type first and, when it is not JSON, throw an Error
    // whose message names the real HTTP condition. Callers keep their existing
    // catch and now surface something an operator can act on.
    async function postJSON(url, opts) {
      const res = await fetch(url, opts);
      const contentType = res.headers.get('content-type') || '';
      if (!contentType.includes('application/json')) {
        // 401/403 is the session-expiry case and has a specific remedy; every
        // other non-JSON answer is reported by its status so a screenshot still
        // carries the diagnosis.
        throw new Error(res.status === 401 || res.status === 403
          ? 'session expired — reload and sign in again'
          : `server returned ${res.status} ${res.statusText || 'error'}`);
      }
      return await res.json();
    }

    /* ---- Session-expiry-aware fetch failures (#7405) ----
       On a hosted spoke every request passes the ingress auth gate, and an
       expired hub session used to surface as a cross-origin redirect to the
       IdP that fetch() cannot follow: Safari rejects with "TypeError: Load
       failed", so no `res.status === 401` branch in this file ever fired.
       Every caller then reported its own "…: Load failed" toast — one expired
       session produced three at once (config load, terminal assertion
       renewal, terminal handoff) and none of them said what to do.

       reportFetchFailure is the single funnel for those sites. On a rejected
       fetch (or an explicit 401/403) it probes the session ONCE, independent
       of the status code the original request did or did not carry, and when
       the session is gone shows one actionable "sign in again" toast instead
       of the caller's message. */
    const SESSION_EXPIRED_MESSAGE = 'Your session expired — sign in again';
    const SESSION_PROBE_PATH = '/api/gh-user-auth/status';
    let _sessionProbe = null;
    let _sessionExpiredToast = null;
    let _sessionSignIn = 'reload';

    /* probeSession classifies the current session as 'ok' (signed in),
       'expired' (the server or the gate said the session is gone) or
       'unknown' (the probe could not tell — the caller's failure is then an
       ordinary one). signIn names the recovery for an expired session:
       'reload' re-enters the hosted login redirect with a full navigation,
       which the browser CAN follow cross-origin; 'device' starts the
       self-hosted GitHub device flow. Concurrent callers share one in-flight
       probe, so three failing requests make one probe and one verdict. */
    function probeSession() {
      if (_sessionProbe) return _sessionProbe;
      _sessionProbe = (async () => {
        try {
          const r = await fetch(SESSION_PROBE_PATH, { cache: 'no-store', credentials: 'same-origin' });
          // The status endpoint itself is public on the spoke, so a 401/403
          // can only come from the hosted ingress gate: the hub session is
          // gone (the /api ingress answers a real 401 rather than redirecting
          // — see the hive-api-xhr Ingress in saas_provision.go).
          if (r.status === 401 || r.status === 403) return { state: 'expired', signIn: 'reload' };
          if (!r.ok) return { state: 'unknown' };
          const d = await r.json().catch(() => null);
          if (!d || typeof d.logged_in === 'undefined') return { state: 'unknown' };
          if (d.logged_in) return { state: 'ok' };
          // logged_in:false is the steady state of a token-mode hive with no
          // device-flow login at all; it only means "expired" when this page
          // had an identity earlier.
          return window._hiveUser ? { state: 'expired', signIn: 'device' } : { state: 'unknown' };
        } catch (e) {
          // The probe itself could not complete. On a hosted host that is the
          // gate's cross-origin bounce (a spoke outage answers 502/503 from
          // the ingress instead); elsewhere it is an unreachable hive.
          return isHostedHiveHost() ? { state: 'expired', signIn: 'reload' } : { state: 'unknown' };
        } finally {
          _sessionProbe = null;
        }
      })();
      return _sessionProbe;
    }

    /* A fetch that never produced a response (network error, or a redirect
       the browser refused to follow) — the shape an expired hosted session
       takes. Distinct from an HTTP error, which did answer. */
    function isFetchRejection(err) {
      return err instanceof TypeError || (!!err && err.name === 'TypeError');
    }
    function isAuthStatusError(err) {
      return !!err && /^HTTP (401|403)\b/.test(String(err.message || ''));
    }

    function signInAgain() {
      if (_sessionSignIn === 'device') startGHLogin();
      else window.location.reload();
    }

    /* One sticky toast for the whole page: a second expired-session report
       while the first toast is still up is folded into it. */
    function showSessionExpiredToast(signIn) {
      _sessionSignIn = signIn || 'reload';
      if (_sessionExpiredToast && _sessionExpiredToast.parentNode) return _sessionExpiredToast;
      const el = showToast(SESSION_EXPIRED_MESSAGE, 'error');
      const btn = document.createElement('button');
      btn.className = 'nav-chip nav-chip--action toast-action';
      btn.textContent = 'Sign in';
      // The sticky toast dismisses on any click inside it; the action must not
      // be swallowed by that.
      btn.addEventListener('click', (ev) => { ev.stopPropagation(); signInAgain(); });
      el.appendChild(btn);
      _sessionExpiredToast = el;
      return el;
    }

    /* reportFetchFailure(context, err) reports a failed authenticated request.
       Resolves true when the failure was an expired session — the caller
       should then render its own signed-out state and NOT retry or fall back
       to made-up data — and false when it was reported as an ordinary error
       under `context`. */
    async function reportFetchFailure(context, err) {
      if (isFetchRejection(err) || isAuthStatusError(err)) {
        const session = await probeSession();
        if (session.state === 'expired') {
          showSessionExpiredToast(session.signIn);
          return true;
        }
      }
      const detail = err && err.message ? err.message : String(err);
      showToast(context + ': ' + (isFetchRejection(err) ? 'could not reach this hive (' + detail + ')' : detail), 'error');
      return false;
    }

    // Kick delivery is asynchronous (#5325). POST /api/kick answers 202 as soon
    // as the message is queued; whether it actually reached the CLI is read
    // from GET /api/kick/{agent}/status. Poll that until it settles.
    //
    // The wait can legitimately run to the server's input-prompt timeout (120s)
    // when a CLI is slow to present its prompt, so the ceiling here is above it
    // — and running out of poll budget is reported as STILL PENDING, never as a
    // failure. That distinction is the whole point of the issue: an
    // indeterminate kick that gets rendered as failed is what made operators
    // click Kick again and deliver the work twice.
    const KICK_POLL_INTERVAL_MS = 2000;
    const KICK_POLL_TIMEOUT_MS = 150000;

    async function pollKickOutcome(agent) {
      const deadline = Date.now() + KICK_POLL_TIMEOUT_MS;
      while (Date.now() < deadline) {
        await new Promise(r => setTimeout(r, KICK_POLL_INTERVAL_MS));
        try {
          const res = await fetch(`/api/kick/${agent}/status`);
          const contentType = res.headers.get('content-type') || '';
          if (!contentType.includes('application/json')) continue;
          const data = await res.json();
          if (data.status === 'delivered') return { settled: true, ok: true };
          if (data.status === 'failed') return { settled: true, ok: false, error: data.error || 'unknown' };
        } catch (e) {
          // A transient poll failure says nothing about the kick. Keep polling.
        }
      }
      return { settled: false };
    }

    // reportKickOutcome renders a queued kick's true result, or leaves the
    // operator with an explicitly indeterminate message if it never settles.
    async function reportKickOutcome(agent, toast, queuedMsg) {
      const outcome = await pollKickOutcome(agent);
      if (!outcome.settled) {
        dismissToast(toast, `${queuedMsg} — still waiting for ${agent}'s CLI; check the terminal`, 'info');
        return;
      }
      if (outcome.ok) { dismissToast(toast, `Kicked ${agent}`, 'success'); return; }
      dismissToast(toast, 'Kick failed: ' + outcome.error, 'error');
    }

    async function kick(agent) {
      const input = document.getElementById(`kick-prompt-${agent}`);
      const prompt = input ? input.value.trim() : '';

      const opts = { method: 'POST', headers: { 'Content-Type': 'application/json' } };
      if (prompt) opts.body = JSON.stringify({ prompt });
      // Acknowledge the click immediately with a persistent toast, then rewrite
      // it into the result — mirrors planIssue() so a slow, failed, or thrown
      // request still surfaces feedback instead of silently doing nothing.
      const toast = showToast(`Kicking ${agent}...`, 'info', true);
      try {
        const data = await postJSON(`/api/kick/${agent}`, opts);
        if (!data.ok) { dismissToast(toast, 'Kick failed: ' + (data.error || 'unknown'), 'error'); return; }
        if (input) input.value = '';
        if (data.status === 'in-flight') {
          dismissToast(toast, `A kick is already being delivered to ${agent}`, 'info');
          return;
        }
        if (data.status === 'deferred') {
          dismissToast(toast, 'Kick deferred — waiting for first governor scan', 'info');
          return;
        }
        await reportKickOutcome(agent, toast, `Queued for ${agent}`);
      } catch (e) {
        dismissToast(toast, 'Kick failed: ' + e.message, 'error');
      }
    }

    // planIssue (Phase 4 "Plan this issue"): POST the issue to /api/plan/from-issue,
    // which mints an epic and kicks the architect to decompose it. The response
    // returns the epic id; decomposition is asynchronous (request-and-poll), so we
    // surface the epic in the plan-review view once the architect fills it in.
    async function planIssue(el) {
      const repo = el.dataset.repo || '';
      const number = parseInt(el.dataset.number, 10) || 0;
      const url = el.dataset.url || '';
      const title = el.dataset.title || '';
      el.disabled = true;
      const toast = showToast(`Planning ${repo}#${number || '?'}...`, 'info', true);
      try {
        const res = await fetch('/api/plan/from-issue', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ repo, number, url, title }),
        });
        const data = await res.json();
        if (!data.ok) { dismissToast(toast, 'Plan failed: ' + (data.error || 'unknown'), 'error'); el.disabled = false; return; }
        // The server returns a clear, state-specific message. When the architect
        // is paused it explains THAT is why the plan is queued and points at the
        // fix — surface it as a warning so the user notices.
        const msg = data.message || (`Epic ${data.epic_id} created`);
        dismissToast(toast, msg, data.architectPaused ? 'error' : 'success');
        // Open the plan-review view for the new epic if that navigation exists;
        // otherwise the epic surfaces via the existing "awaiting review" flow.
        if (typeof openPlanReview === 'function' && data.epic_id) openPlanReview(data.epic_id);
      } catch (e) {
        dismissToast(toast, 'Plan failed: ' + e.message, 'error');
        el.disabled = false;
      }
    }

    async function designIssue(el) {
      const repo = el.dataset.repo || '';
      const number = parseInt(el.dataset.number, 10) || 0;
      const url = el.dataset.url || '';
      const title = el.dataset.title || '';
      el.disabled = true;
      const toast = showToast(`Requesting design for ${repo}#${number || '?'}...`, 'info', true);
      try {
        const res = await fetch('/api/plan/from-issue/design', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ repo, number, url, title }),
        });
        const data = await res.json();
        if (!data.ok) { dismissToast(toast, 'Design failed: ' + (data.error || 'unknown'), 'error'); el.disabled = false; return; }
        dismissToast(toast, data.message || 'Design requested', 'success');
        if (typeof openPlanReview === 'function' && data.epic_id) openPlanReview(data.epic_id);
      } catch (e) {
        dismissToast(toast, 'Design failed: ' + e.message, 'error');
        el.disabled = false;
      }
    }
