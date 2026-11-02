// Apply layout immediately and hide content until first data render.
// NOTE: only body classes are set here — the .layout-toggle buttons do NOT
// exist in the DOM yet at this point, so any label write here is a no-op.
// applyLayout() (defined in the main script) is the single source of truth
// for toggle labels; the DOMContentLoaded hook below guarantees it runs
// once the topbar/sidebar buttons exist, so the label and body class always
// sync from storage.
// Kiosk mode (#15594): `?kiosk=1` turns the dashboard into an unattended
// display for a headless streamer (src/deploy/stream). It is a pure
// presentation flag — it grants nothing, hides the chrome (sidebar, topbar,
// avatar menu, chat FAB, notices) and every section except the Hive activity
// monitor, suppresses the welcome/NPS dialogs and the toast hover-pause, and
// never writes to localStorage so a rolled SPA can't inherit stale state.
// The class is set here, before first paint, so the chrome never flashes.
// Tracker sounds are on by default in kiosk mode (the streamer captures them
// through a PulseAudio null sink); `&sound=off` keeps the display silent.
// Pop-out mode (#15829): `?popout=1` is the same chrome-less, tracker-only
// layout for an operator's second monitor — opened by the ⧉ button in the
// activity monitor header via window.open. Unlike kiosk it is attended: the
// cursor stays, the settings gear works, preferences persist as usual, and a
// "Return to dashboard" link focuses the opener. Reloading keeps the layout
// because the flag lives in the URL.
// Display mode (#15925): `?display=1` is the same chrome-less layout for an
// attended wall screen, entered from the 📺 header button. It is
// presentation only: the cursor stays, sound keeps the user's setting, it
// never writes layout or theme to localStorage, and it hides the row action
// affordances. Kiosk wins when both flags are present.
// Display options (#15931), honoured only together with display=1:
// `density=tv` (the only value; anything else is ignored) sets
// body.display-density-tv, a larger three-column wall layout, and `shift=1`
// sets body.display-shift, an OLED burn-in shift. Without display=1 both are
// ignored, so the ordinary dashboard and kiosk render exactly as before.
// Pop-out and display mode also set body.ham-chromeless, the one class the
// shared chrome-less layout in css/popout.css keys on (full screen adds it at
// runtime); popout-mode and display-mode keep their view-specific rules.
(function(){
  var kiosk=false, popout=false, display=false, view='', sound=true, density='', shift=false;
  try { var u=new URL(window.location.href); kiosk=u.searchParams.get('kiosk')==='1'; view=u.searchParams.get('view'); popout=u.searchParams.get('popout')==='1'; display=u.searchParams.get('display')==='1'; density=u.searchParams.get('density')==='tv'?'tv':''; shift=u.searchParams.get('shift')==='1'; sound=new URL(window.location.href).searchParams.get('sound')!=='off'; } catch(e) {}
  window.HIVE_MATRIX = kiosk && view === 'matrix';
  if(window.HIVE_MATRIX){
    document.body.classList.add('matrix-mode');
    document.addEventListener('DOMContentLoaded', function(){ typeof hiveMatrixStart === 'function' && hiveMatrixStart(); });
  }
  window.HIVE_KIOSK=kiosk;
  window.HIVE_POPOUT=popout&&!kiosk;
  window.HIVE_DISPLAY=display&&!kiosk;
  window.HIVE_DISPLAY_DENSITY=window.HIVE_DISPLAY?density:'';
  window.HIVE_DISPLAY_SHIFT=window.HIVE_DISPLAY&&shift;
  window.HIVE_KIOSK_SOUND=kiosk&&sound;
  window.hiveKioskMode=function(){ return !!window.HIVE_KIOSK; };
  window.hivePopoutMode=function(){ return !!window.HIVE_POPOUT; };
  window.hiveDisplayMode=function(){ return !!window.HIVE_DISPLAY; };
  window.hiveKioskSoundOn=function(){ return !!window.HIVE_KIOSK_SOUND; };
  if(kiosk){
    document.body.classList.add('kiosk-mode');
  }else if(window.HIVE_POPOUT){
    document.body.classList.add('popout-mode');
    document.body.classList.add('ham-chromeless');
  }
  if(kiosk||window.HIVE_POPOUT){
    if(!location.hash) history.replaceState(null,'',location.pathname+location.search+'#section-activity-monitor-section');
  }
  if(window.HIVE_DISPLAY){
    document.body.classList.add('display-mode');
    document.body.classList.add('ham-chromeless');
    if(window.HIVE_DISPLAY_DENSITY==='tv') document.body.classList.add('display-density-tv');
    if(window.HIVE_DISPLAY_SHIFT) document.body.classList.add('display-shift');
    if(!location.hash) history.replaceState(null,'',location.pathname+location.search+'#section-activity-monitor-section');
  }
})();
(function(){
  var m=localStorage.getItem('hive-layout-mode');
  if(m==='openclaw')m='light';
  if(m==='classic')m='dark';
  // A kiosk (#15594) is a broadcast surface with no operator to flip the
  // toggle; it always renders dark unless the URL says otherwise.
  if(window.HIVE_KIOSK){ var kt=null; try { kt=new URL(window.location.href).searchParams.get('theme'); } catch(e) {} m=(kt==='light')?'light':'dark'; }
  if(!m)m='light';
  if(m==='light'){document.body.classList.add('light-mode')}
  document.body.style.opacity='0';
  document.body.style.transition='opacity 0.15s ease-in';
  setTimeout(function(){ document.body.style.opacity='1'; }, 3000);
  document.addEventListener('DOMContentLoaded', function(){
    if (typeof applyLayout === 'function' && typeof getLayout === 'function') {
      applyLayout(getLayout());
    }
  });
})();
(function(){
  var STYLE_SCOPE = 'dashboard';
  var STYLE_ROOT_ID = 'hive-dashboard-root';
  function currentStyleSrc(){
    try { return new URL(window.location.href).searchParams.get('style') || ''; } catch(e) { return ''; }
  }
  function styleLabel(src){
    var parts=String(src||'').split('@')[0].split('/');
    return parts.length>=2?(parts[0]+'/'+parts[1]):'custom';
  }
  function styleURLWithHash(hash){
    try {
      var u=new URL(window.location.href);
      return u.pathname+u.search+(hash||'');
    } catch(e) { return (hash || location.pathname); }
  }
  window.hiveURLWithHash = styleURLWithHash;
  window.clearDashboardCustomStyleParam = function(){
    try {
      var u=new URL(window.location.href);
      u.searchParams.delete('style');
      history.replaceState(null,'',u.pathname+(u.searchParams.toString()?('?'+u.searchParams.toString()):'')+u.hash);
    } catch(e) {}
    window.HIVE_DASHBOARD_CUSTOM_STYLE_SRC='';
    var link=document.getElementById('dashboard-custom-style-link');
    if(link)link.remove();
    var note=document.getElementById('dashboard-custom-style-note');
    if(note)note.style.display='none';
    var root=document.getElementById(STYLE_ROOT_ID);
    if(root)root.classList.remove('custom-style-active');
  };
  window.showDashboardCustomStyleNotice = function(ok){
    var note=document.getElementById('dashboard-custom-style-note');
    if(!note)return;
    var src=window.HIVE_DASHBOARD_CUSTOM_STYLE_SRC||currentStyleSrc();
    var dropped=parseInt(window.HIVE_DASHBOARD_CUSTOM_STYLE_DROPPED||'0',10)||0;
    note.classList.toggle('warn', !ok);
    note.title=ok?(dropped>0?('Custom style active: '+src+' — '+dropped+' rules removed by sanitizer'):'Custom style active: '+src+' — remove ?style= to reset'):'Custom style could not be loaded — using default dashboard styles';
    note.textContent=ok?(dropped>0?('Custom style: '+styleLabel(src)+' ('+dropped+' removed)'):'Custom style: '+styleLabel(src)):'Custom style failed';
    var btn=document.createElement('button');
    btn.type='button';
    btn.textContent='×';
    btn.setAttribute('aria-label', ok?'Remove custom style':'Dismiss');
    btn.onclick=ok?window.clearDashboardCustomStyleParam:function(){note.style.display='none';};
    note.appendChild(btn);
    note.style.display='inline-flex';
    if(typeof showToast==='function')showToast(ok?'Custom style active: '+styleLabel(src):'Custom style could not be loaded — using default dashboard styles', ok?'info':'error');
  };
  var src=currentStyleSrc();
  if(src){
    window.HIVE_DASHBOARD_CUSTOM_STYLE_SRC=src;
    var styleState='pending';
    function renderStyleState(){
      var root=document.getElementById(STYLE_ROOT_ID);
      if(styleState==='loaded'){
        if(root)root.classList.add('custom-style-active');
        window.showDashboardCustomStyleNotice(true);
      }else if(styleState==='failed'){
        if(root)root.classList.remove('custom-style-active');
        window.showDashboardCustomStyleNotice(false);
      }
    }
    var link=document.createElement('link');
    link.id='dashboard-custom-style-link';
    link.rel='stylesheet';
    link.href='/api/style?src='+encodeURIComponent(src)+'&scope='+encodeURIComponent(STYLE_SCOPE);
    link.onload=function(){
      styleState='loaded';
      fetch('/api/style?src='+encodeURIComponent(src)+'&scope='+encodeURIComponent(STYLE_SCOPE)+'&report=1',{cache:'no-store'})
        .then(function(r){return r.ok?r.json():null;})
        .then(function(j){window.HIVE_DASHBOARD_CUSTOM_STYLE_DROPPED=(j&&j.report&&j.report.dropped)||0;renderStyleState();})
        .catch(function(){renderStyleState();});
    };
    link.onerror=function(){ styleState='failed'; renderStyleState(); };
    document.head.appendChild(link);
    document.addEventListener('DOMContentLoaded', renderStyleState);
  }
})();

