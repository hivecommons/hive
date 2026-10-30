    // ── Dashboard section layout: drag/keyboard reorder, persisted per viewer ──
    // Keyed by stable top-level section ids. Saved shape: {v:1,main:[...]}; unknown
    // ids are dropped and newly added sections are appended in template order.
    var DASHBOARD_LAYOUT_KEY='hive.dashboard.layout';
    var DASHBOARD_LAYOUT_VERSION=1;
    var DASHBOARD_LAYOUT_REGIONS=['main'];
    var DASHBOARD_LAYOUT_TEMPLATE={main:['overview-section','governor','pr-throughput-section','repos-section','activity-monitor-section','knowledge-section','review-queue-section','contributors-section','advisory-section','token-panel','cost-panel','inception-section','acmm-eval-section','debug-section','audit-section','approvals-section','nous-section','logs-section','agents-section','agent-activity-section','faq-section']};
    var DASHBOARD_LAYOUT_GRIP_SELECTOR='[data-dashboard-grip]';
    var DASHBOARD_LAYOUT_CARD_SELECTOR='[data-dashboard-section]';
    var DASHBOARD_LAYOUT_ANCHOR_ID='dash-notices';
    var DASHBOARD_LAYOUT_PRESETS_KEY='hive.layout.presets';
    var DASHBOARD_LAYOUT_APPLIED_KEY='hive.layout.applied';
    var DASHBOARD_HIDDEN_SECTIONS_KEY='hive.dashboard.hiddenSections';
    var DASHBOARD_HIDE_DENYLIST=['overview-section'];
    var DASHBOARD_LAYOUT_SNAPSHOT_VERSION=1;
    var DASHBOARD_LAYOUT_PRESET_LIMIT=5;
    var DASHBOARD_LAYOUT_DEFAULT_NAME='My layout';
    var DASHBOARD_REVIEW_MIGRATION_KEY='hive.dashboard.reviewSectionMigrated';
    function dashboardAliasSectionId(id){return id==='review-pipeline-section'?'review-queue-section':id;}
    function dashboardMigrateReviewSectionStorage(){try{if(localStorage.getItem(DASHBOARD_REVIEW_MIGRATION_KEY)==='1')return;['hive-section-collapsed-review-queue-section','hive-section-collapsed-review-pipeline-section'].forEach(function(k){localStorage.removeItem(k);});var raw=localStorage.getItem(DASHBOARD_HIDDEN_SECTIONS_KEY);if(raw){var arr=JSON.parse(raw);if(Array.isArray(arr)){arr=arr.map(dashboardAliasSectionId).filter(function(id,i,a){return id!=='review-queue-section'&&dashboardCanHideSection(id)&&a.indexOf(id)===i;});if(arr.length)localStorage.setItem(DASHBOARD_HIDDEN_SECTIONS_KEY,JSON.stringify(arr));else localStorage.removeItem(DASHBOARD_HIDDEN_SECTIONS_KEY);}}var layoutRaw=localStorage.getItem(DASHBOARD_LAYOUT_KEY);if(layoutRaw){var layout=JSON.parse(layoutRaw);if(layout&&Array.isArray(layout.main)){layout.main=layout.main.map(dashboardAliasSectionId).filter(function(id,i,a){return a.indexOf(id)===i;});localStorage.setItem(DASHBOARD_LAYOUT_KEY,JSON.stringify(layout));}}localStorage.setItem(DASHBOARD_REVIEW_MIGRATION_KEY,'1');}catch(e){}}
    dashboardSeedCollapseDefaults();
    dashboardMigrateReviewSectionStorage();
    var dashboardDragState=null;
    var dashboardKeyboardSnapshot=null;
    function dashboardFeatureEnabled(name){try{return !!(window._lastStatus&&window._lastStatus.features&&window._lastStatus.features[name]===true);}catch(e){return false;}}
    function experimentLabEnabled(){return dashboardFeatureEnabled('experiment_lab');}
    function dashboardFeatureSectionHidden(id){id=dashboardAliasSectionId(id);return id==='nous-section'&&!experimentLabEnabled();}
    function dashboardCanHideSection(id){id=dashboardAliasSectionId(id);return DASHBOARD_LAYOUT_TEMPLATE.main.indexOf(id)>=0&&DASHBOARD_HIDE_DENYLIST.indexOf(id)<0&&!dashboardFeatureSectionHidden(id);}
    function dashboardHiddenSectionsRead(){try{var raw=localStorage.getItem(DASHBOARD_HIDDEN_SECTIONS_KEY),arr=raw?JSON.parse(raw):[];return Array.isArray(arr)?arr.filter(function(id,i){return dashboardCanHideSection(id)&&arr.indexOf(id)===i;}):[];}catch(e){return [];}}
    function dashboardHiddenSectionsWrite(ids){try{var valid=(ids||[]).filter(function(id,i,a){return dashboardCanHideSection(id)&&a.indexOf(id)===i;});if(valid.length)localStorage.setItem(DASHBOARD_HIDDEN_SECTIONS_KEY,JSON.stringify(valid));else localStorage.removeItem(DASHBOARD_HIDDEN_SECTIONS_KEY);}catch(e){}dashboardUpdateLayoutMenuState();}
    function dashboardSectionHidden(id){return dashboardHiddenSectionsRead().indexOf(id)>=0;}
    function dashboardFirstVisibleSectionId(exclude){var hidden=dashboardHiddenSectionsRead();return dashboardLayoutNormalize(dashboardLayoutCurrent()).main.find(function(id){return id!==exclude&&hidden.indexOf(id)<0&&!dashboardFeatureSectionHidden(id);})||'overview-section';}
    function dashboardApplyHiddenSections(){var hidden=dashboardHiddenSectionsRead();DASHBOARD_LAYOUT_TEMPLATE.main.forEach(function(id){var isHidden=hidden.indexOf(id)>=0,section=document.getElementById(id);if(section){section.classList.toggle('dashboard-section-hidden',isHidden);}var item=document.querySelector('.oc-nav-item[data-section="'+id+'"]');if(item){item.classList.toggle('oc-nav-item-hidden',isHidden);if(isHidden)item.setAttribute('aria-disabled','true');else item.removeAttribute('aria-disabled');item.title=isHidden?'Hidden from the dashboard. Click to unhide this section.':'';var btn=item.querySelector(':scope > .oc-nav-hide-toggle');if(btn){var label=(item.querySelector('.oc-nav-text')||item).textContent.trim()||id;btn.textContent=isHidden?'👁':'⊘';btn.title=isHidden?'Unhide '+label:'Hide '+label;btn.setAttribute('aria-label',(isHidden?'Unhide ':'Hide ')+label);btn.setAttribute('aria-pressed',isHidden?'true':'false');}}});}
    function dashboardSetSectionHidden(sectionId,hidden){if(!dashboardCanHideSection(sectionId))return;var ids=dashboardHiddenSectionsRead().filter(function(id){return id!==sectionId;});if(hidden)ids.push(sectionId);dashboardHiddenSectionsWrite(ids);dashboardApplyHiddenSections();dashboardLayoutAnnounce((hidden?'Hid ':'Unhid ')+sectionId+'.');var active=document.querySelector('.oc-nav-item.active[data-section="'+sectionId+'"]');if(hidden&&active){active.classList.remove('active');ocNavigate(dashboardFirstVisibleSectionId(sectionId),true);}}
    function toggleDashboardSectionHidden(sectionId){if(sectionId&&typeof sectionId==='object')sectionId=sectionId.getAttribute&&sectionId.getAttribute('data-section');if(!sectionId)return;var wasHidden=dashboardSectionHidden(sectionId);dashboardSetSectionHidden(sectionId,!wasHidden);if(wasHidden)ocNavigate(sectionId);}
    function dashboardEnsureSections(){DASHBOARD_LAYOUT_TEMPLATE.main.forEach(function(id){var el=document.getElementById(id);if(el&&!el.getAttribute('data-dashboard-section'))el.setAttribute('data-dashboard-section',id);});dashboardApplyHiddenSections();}
    function dashboardRegionEl(name){return document.getElementById('hive-dashboard-root');}
    function dashboardCardEl(id){return document.querySelector('[data-dashboard-section="'+id+'"]');}
    function dashboardLayoutAnnounce(msg){var live=document.getElementById('dashboard-layout-live');if(live)live.textContent=msg;}
    function dashboardLayoutCardName(card){var h=card&&card.querySelector('.dash-card-title,.gov-title,.token-title,.cost-title,.section-header-toggle h2,.section-header-toggle,.section-label');if(h){var clone=h.cloneNode(true);Array.prototype.forEach.call(clone.querySelectorAll('button,.section-help-mark'),function(b){b.remove();});var txt=clone.textContent.trim();if(txt)return txt.replace(/^▼\s*/,'');}return card?card.getAttribute('data-dashboard-section'):'section';}
    function dashboardLayoutAllIds(){return DASHBOARD_LAYOUT_TEMPLATE.main.filter(function(id){return !dashboardFeatureSectionHidden(id);});}
    function dashboardLayoutRead(){try{var raw=localStorage.getItem(DASHBOARD_LAYOUT_KEY);return raw?JSON.parse(raw):null;}catch(e){return null;}}
    function dashboardLayoutCurrent(){var out={v:DASHBOARD_LAYOUT_VERSION,main:[]};var region=dashboardRegionEl('main');if(!region)return out;Array.prototype.forEach.call(region.querySelectorAll(':scope > '+DASHBOARD_LAYOUT_CARD_SELECTOR),function(card){var id=card.getAttribute('data-dashboard-section');if(id&&!dashboardFeatureSectionHidden(id))out.main.push(id);});return out;}
    // Sections missing from a saved layout (added after the layout was
    // saved) slot in next to their template neighbour instead of being
    // appended after the trailing FAQ section (#9426).
    function dashboardLayoutNormalize(saved){var valid=dashboardLayoutAllIds(),seen={},out={v:DASHBOARD_LAYOUT_VERSION,main:[]};var arr=saved&&Array.isArray(saved.main)?saved.main:[];arr.forEach(function(id){id=dashboardAliasSectionId(id);if(valid.indexOf(id)<0||seen[id])return;seen[id]=true;out.main.push(id);});var tpl=valid;tpl.forEach(function(id,i){if(seen[id])return;seen[id]=true;var at=-1;for(var j=i-1;j>=0&&at<0;j--){at=out.main.indexOf(tpl[j]);}if(at>=0){out.main.splice(at+1,0,id);return;}var before=out.main.length;for(var k=i+1;k<tpl.length;k++){var idx=out.main.indexOf(tpl[k]);if(idx>=0){before=idx;break;}}out.main.splice(before,0,id);});return out;}
    function dashboardLayoutEquals(a,b){a=dashboardLayoutNormalize(a);b=dashboardLayoutNormalize(b);if(a.main.length!==b.main.length)return false;for(var i=0;i<a.main.length;i++){if(a.main[i]!==b.main[i])return false;}return true;}
    function dashboardLayoutExtraKeys(){var keys=[];try{for(var i=0;i<localStorage.length;i++){var k=localStorage.key(i)||'';if(k.indexOf('hive-repos-pill-filter:')===0||k.indexOf('hive-repo-card-widths:')===0||k.indexOf(AGENT_COMPACT_LS_PREFIX)===0||k===ALL_AGENTS_COMPACT_KEY||k.indexOf('hive-agent-card-layout:')===0||k.indexOf('hive.repo.tile.order')===0||k.indexOf('hive.repo-tile.order')===0||k.indexOf('hive.repoTile.order')===0||k.indexOf('hive.dashboard.repoTileOrder')===0)keys.push(k);}}catch(e){}keys.sort();return keys;}
    function dashboardLayoutSnapshot(){var collapsed={},hidden={},extras={};dashboardLayoutAllIds().forEach(function(id){if(isSectionCollapsed(id))collapsed[id]=true;});dashboardHiddenSectionsRead().forEach(function(id){hidden[id]=true;});dashboardLayoutExtraKeys().forEach(function(k){try{var v=localStorage.getItem(k);if(v!==null)extras[k]=v;}catch(e){}});return {v:DASHBOARD_LAYOUT_SNAPSHOT_VERSION,layout:dashboardLayoutNormalize(dashboardLayoutCurrent()),collapsed:collapsed,hidden:hidden,extras:extras};}
    function dashboardNormalizeSnapshot(value){var src=value&&typeof value==='object'?value:{};var layout=src.layout?src.layout:src;var collapsed={},hidden={},extras={};if(src.collapsed&&typeof src.collapsed==='object'&&!Array.isArray(src.collapsed)){dashboardLayoutAllIds().forEach(function(id){if(src.collapsed[id])collapsed[id]=true;});}if(src.hidden&&typeof src.hidden==='object'&&!Array.isArray(src.hidden)){dashboardLayoutAllIds().forEach(function(id){if(src.hidden[id]&&dashboardCanHideSection(id))hidden[id]=true;});}if(src.extras&&typeof src.extras==='object'&&!Array.isArray(src.extras)){Object.keys(src.extras).forEach(function(k){if(k.indexOf('hive-repos-pill-filter:')===0||/^hive[-.]repo/.test(k)||k.indexOf('hive-agent-card-layout:')===0||k.indexOf(AGENT_COMPACT_LS_PREFIX)===0||k===ALL_AGENTS_COMPACT_KEY)extras[k]=String(src.extras[k]);});}return {v:DASHBOARD_LAYOUT_SNAPSHOT_VERSION,layout:dashboardLayoutNormalize(layout),collapsed:collapsed,hidden:hidden,extras:extras};}
    function dashboardSnapshotEquals(a,b){a=dashboardNormalizeSnapshot(a);b=dashboardNormalizeSnapshot(b);if(!dashboardLayoutEquals(a.layout,b.layout))return false;var ac=Object.keys(a.collapsed).sort(),bc=Object.keys(b.collapsed).sort();if(JSON.stringify(ac)!==JSON.stringify(bc))return false;var ah=Object.keys(a.hidden).sort(),bh=Object.keys(b.hidden).sort();if(JSON.stringify(ah)!==JSON.stringify(bh))return false;var ae=Object.keys(a.extras).sort(),be=Object.keys(b.extras).sort();if(JSON.stringify(ae)!==JSON.stringify(be))return false;for(var i=0;i<ae.length;i++){if(a.extras[ae[i]]!==b.extras[ae[i]])return false;}return true;}
    function dashboardLayoutDefaultCollapsed(){var collapsed={};dashboardLayoutAllIds().forEach(function(id){if(dashboardDefaultCollapsed(id))collapsed[id]=true;});return collapsed;}
    function dashboardLayoutDefaultSnapshot(){return dashboardNormalizeSnapshot({layout:null,collapsed:dashboardLayoutDefaultCollapsed(),hidden:{},extras:{}});}
    function dashboardReadLayoutPresets(){try{var raw=localStorage.getItem(DASHBOARD_LAYOUT_PRESETS_KEY);var arr=raw?JSON.parse(raw):[];return Array.isArray(arr)?arr.filter(function(p){return p&&p.id&&p.state;}).slice(0,DASHBOARD_LAYOUT_PRESET_LIMIT):[];}catch(e){return [];}}
    function dashboardWriteLayoutPresets(presets){try{localStorage.setItem(DASHBOARD_LAYOUT_PRESETS_KEY,JSON.stringify((presets||[]).slice(0,DASHBOARD_LAYOUT_PRESET_LIMIT)));}catch(e){}dashboardRenderLayoutPresets();}
    function dashboardReadAppliedLayout(){try{var raw=localStorage.getItem(DASHBOARD_LAYOUT_APPLIED_KEY);return raw?JSON.parse(raw):null;}catch(e){return null;}}
    function dashboardMarkAppliedLayout(kind,name,id,state){var applied={kind:kind||'custom',name:name||'',id:id||'',state:dashboardNormalizeSnapshot(state||dashboardLayoutSnapshot())};try{localStorage.setItem(DASHBOARD_LAYOUT_APPLIED_KEY,JSON.stringify(applied));}catch(e){}dashboardUpdateLayoutMenuState();}
    function dashboardUpdateLayoutMenuState(){var status=document.getElementById('oc-gh-menu-layout-status'),wrap=document.getElementById('oc-gh-avatar-wrap');var current=dashboardLayoutSnapshot(),applied=dashboardReadAppliedLayout(),base=applied&&applied.state?applied:{kind:'default',name:'',state:dashboardLayoutDefaultSnapshot()};var same=dashboardSnapshotEquals(current,base.state);var label='Layout: ';if(base.kind==='default')label+=same?'default':'custom (unsaved)';else label+=(base.name||'custom')+(same?'':' (modified)');if(status)status.textContent=label;if(wrap)wrap.classList.toggle('layout-unsaved',!same);dashboardRenderLayoutPresets();}
    function dashboardUpdateResetButton(){dashboardUpdateLayoutMenuState();}
    function dashboardLayoutWrite(){try{localStorage.setItem(DASHBOARD_LAYOUT_KEY,JSON.stringify(dashboardLayoutCurrent()));}catch(e){}dashboardUpdateResetButton();dashboardSortSidebar();}
    function dashboardApplySnapshotState(state){state=dashboardNormalizeSnapshot(state);try{localStorage.setItem(DASHBOARD_LAYOUT_KEY,JSON.stringify(state.layout));dashboardLayoutAllIds().forEach(function(id){writeSectionCollapsedKey(id,!!state.collapsed[id]);});dashboardHiddenSectionsWrite(Object.keys(state.hidden||{}));var keep=state.extras||{};dashboardLayoutExtraKeys().forEach(function(k){if(!Object.prototype.hasOwnProperty.call(keep,k))localStorage.removeItem(k);});Object.keys(keep).forEach(function(k){localStorage.setItem(k,keep[k]);});}catch(e){}dashboardApplyLayout(state.layout);dashboardLayoutAllIds().forEach(applySectionCollapse);dashboardApplyHiddenSections();if(typeof repaintReposForWidth==='function')repaintReposForWidth();if(typeof renderAgents==='function')renderAgents(((window._lastStatus||{}).agents)||[]);if(typeof ocUpdateSidebarAgents==='function')ocUpdateSidebarAgents();dashboardUpdateLayoutMenuState();}
    function dashboardRenderLayoutPresets(){var list=document.getElementById('oc-gh-menu-layout-list');if(!list)return;var presets=dashboardReadLayoutPresets();if(!presets.length){list.innerHTML='<div class="avatar-menu-layout-empty">No saved layouts yet.</div>';return;}list.innerHTML=presets.map(function(p){return '<div class="avatar-menu-layout-snapshot"><span class="avatar-menu-layout-name" title="'+esc(p.name||DASHBOARD_LAYOUT_DEFAULT_NAME)+'">'+esc(p.name||DASHBOARD_LAYOUT_DEFAULT_NAME)+'</span><button type="button" class="avatar-menu-layout-action" data-action="applyDashboardLayoutPreset" data-arg0="'+esc(p.id)+'">Apply</button><button type="button" class="avatar-menu-layout-action" data-action="renameDashboardLayoutPreset" data-arg0="'+esc(p.id)+'">Rename</button><button type="button" class="avatar-menu-layout-action" data-action="deleteDashboardLayoutPreset" data-arg0="'+esc(p.id)+'">Delete</button></div>';}).join('');}
    function toggleDashboardLayoutPresets(){var list=document.getElementById('oc-gh-menu-layout-list'),btn=document.getElementById('oc-gh-menu-layout-saved');if(!list)return;list.hidden=!list.hidden;if(btn){btn.setAttribute('aria-expanded',String(!list.hidden));btn.textContent=list.hidden?'Saved layouts ▸':'Saved layouts ▾';}dashboardRenderLayoutPresets();}
    async function saveDashboardLayoutPreset(){var name=(await hivePrompt({title:'Save dashboard layout',message:'Name this layout snapshot.',defaultValue:DASHBOARD_LAYOUT_DEFAULT_NAME,confirmLabel:'Save'}));name=String(name||'').trim();if(!name)return;var presets=dashboardReadLayoutPresets(),now=new Date().toISOString(),state=dashboardLayoutSnapshot(),existing=presets.find(function(p){return (p.name||'').toLowerCase()===name.toLowerCase();});if(existing){existing.state=state;existing.updatedAt=now;}else{presets.unshift({id:'layout-'+Date.now().toString(36),name:name,createdAt:now,updatedAt:now,state:state});}presets=presets.slice(0,DASHBOARD_LAYOUT_PRESET_LIMIT);dashboardWriteLayoutPresets(presets);var saved=presets.find(function(p){return (p.name||'').toLowerCase()===name.toLowerCase();})||presets[0];dashboardMarkAppliedLayout('preset',saved.name,saved.id,saved.state);showToast('Layout saved as '+saved.name,'success');}
    function applyDashboardLayoutPreset(id){var p=dashboardReadLayoutPresets().find(function(x){return x.id===id;});if(!p)return;dashboardApplySnapshotState(p.state);dashboardMarkAppliedLayout('preset',p.name,p.id,p.state);dashboardLayoutAnnounce('Applied layout '+p.name+'.');showToast('Layout applied: '+p.name,'success');}
    async function renameDashboardLayoutPreset(id){var presets=dashboardReadLayoutPresets(),p=presets.find(function(x){return x.id===id;});if(!p)return;var name=(await hivePrompt({title:'Rename layout',message:'New layout name.',defaultValue:p.name||DASHBOARD_LAYOUT_DEFAULT_NAME,confirmLabel:'Rename'}));name=String(name||'').trim();if(!name)return;p.name=name;p.updatedAt=new Date().toISOString();dashboardWriteLayoutPresets(presets);var applied=dashboardReadAppliedLayout();if(applied&&applied.id===id)dashboardMarkAppliedLayout('preset',p.name,p.id,p.state);}
    async function deleteDashboardLayoutPreset(id){var presets=dashboardReadLayoutPresets(),p=presets.find(function(x){return x.id===id;});if(!p)return;if(!await hiveConfirm({title:'Delete saved layout?',message:'Delete "'+(p.name||DASHBOARD_LAYOUT_DEFAULT_NAME)+'"? The current working layout will not change.',confirmLabel:'Delete',danger:true}))return;dashboardWriteLayoutPresets(presets.filter(function(x){return x.id!==id;}));var applied=dashboardReadAppliedLayout();if(applied&&applied.id===id)dashboardMarkAppliedLayout('custom','', '', dashboardLayoutSnapshot());}
    function dashboardDownloadJSON(filename,obj){hiveExportViewer({title:filename,filename:filename,text:JSON.stringify(obj,null,2)+'\n',mime:'application/json'});}
    function setAllDashboardSectionsCollapsed(collapsed){dashboardLayoutAllIds().forEach(function(id){if(dashboardSectionHidden(id))return;toggleSection(id,{collapsed:collapsed,animate:false});});dashboardUpdateLayoutMenuState();dashboardLayoutAnnounce(collapsed?'All dashboard sections collapsed.':'All dashboard sections expanded.');}
    function expandAllDashboardSections(){setAllDashboardSectionsCollapsed(false);}
    function collapseAllDashboardSections(){setAllDashboardSectionsCollapsed(true);}
    function exportDashboardLayoutJSON(){dashboardDownloadJSON('hive-dashboard-layout.json',dashboardLayoutSnapshot());showToast('Layout JSON ready to copy','success');}
    function importDashboardLayoutJSON(){var input=document.createElement('input');input.type='file';input.accept='application/json,.json';input.addEventListener('change',function(){var file=input.files&&input.files[0];if(!file)return;var reader=new FileReader();reader.onload=function(){try{var parsed=JSON.parse(String(reader.result||''));var state=dashboardNormalizeSnapshot(parsed);dashboardApplySnapshotState(state);dashboardMarkAppliedLayout('custom','', '', state);showToast('Layout imported','success');}catch(e){showToast('Layout import failed: invalid JSON','error');}};reader.readAsText(file);});input.click();}
    async function resetDashboardLayout(){var before=dashboardLayoutSnapshot();if(!await hiveConfirm({title:'Reset layout to default?',message:'This clears your working dashboard section order, collapsed sections, hidden sections, and saved layout display tweaks on this device. You can undo for a few seconds after reset.',confirmLabel:'Reset layout',danger:true}))return;try{localStorage.removeItem(DASHBOARD_LAYOUT_KEY);localStorage.removeItem(DASHBOARD_HIDDEN_SECTIONS_KEY);dashboardLayoutAllIds().forEach(function(id){localStorage.removeItem(SECTION_LS_PREFIX+id);});dashboardLayoutExtraKeys().forEach(function(k){localStorage.removeItem(k);});}catch(e){}dashboardApplyLayout(dashboardLayoutNormalize(null));dashboardLayoutAllIds().forEach(applySectionCollapse);dashboardApplyHiddenSections();if(typeof repaintReposForWidth==='function')repaintReposForWidth();if(typeof renderAgents==='function')renderAgents(((window._lastStatus||{}).agents)||window._lastAgents||[]);if(typeof ocUpdateSidebarAgents==='function')ocUpdateSidebarAgents();dashboardMarkAppliedLayout('default','', '', dashboardLayoutDefaultSnapshot());dashboardLayoutAnnounce('Dashboard layout reset to the default.');showToast('Dashboard layout reset to default','info',{actionLabel:'Undo',autoDismissMs:10000,onAction:function(){dashboardApplySnapshotState(before);dashboardMarkAppliedLayout('custom','', '', before);dashboardLayoutAnnounce('Dashboard layout reset undone.');}});}
    function dashboardIsSectionNode(node){return !!(node&&node.getAttribute&&node.getAttribute('data-dashboard-section'));}
    function dashboardTopAnchor(region){var anchor=document.getElementById(DASHBOARD_LAYOUT_ANCHOR_ID);if(!region||!anchor)return null;if(anchor.parentElement!==region||region.firstElementChild!==anchor)region.insertBefore(anchor,region.firstElementChild||null);return anchor;}
    function dashboardPinNoticeAnchor(region){var anchor=dashboardTopAnchor(region);if(!region||!anchor||!region.children)return;var children=Array.prototype.slice.call(region.children),moved=[];for(var i=0;i<children.length&&children[i]!==anchor;i++){if(dashboardIsSectionNode(children[i]))moved.push(children[i]);}if(!moved.length)return;var ref=anchor.nextElementSibling||null;moved.forEach(function(card){region.insertBefore(card,ref);});dashboardTopAnchor(region);}
    function dashboardFirstSection(region){if(!region)return null;dashboardPinNoticeAnchor(region);var anchor=dashboardTopAnchor(region);if(anchor&&region.children){var after=false,children=Array.prototype.slice.call(region.children);for(var i=0;i<children.length;i++){if(children[i]===anchor){after=true;continue;}if(after&&dashboardIsSectionNode(children[i]))return children[i];}}return region.querySelector(':scope > '+DASHBOARD_LAYOUT_CARD_SELECTOR);}
    function dashboardInsertSection(region,card,before){if(!region||!card)return;dashboardPinNoticeAnchor(region);if(before){var anchor=dashboardTopAnchor(region);if(anchor&&region.children){var children=Array.prototype.slice.call(region.children),anchorIdx=children.indexOf(anchor),beforeIdx=children.indexOf(before);if(anchorIdx>=0&&beforeIdx>=0&&beforeIdx<=anchorIdx)before=dashboardFirstSection(region);}region.insertBefore(card,before);dashboardPinNoticeAnchor(region);return;}var sections=region.querySelectorAll(':scope > '+DASHBOARD_LAYOUT_CARD_SELECTOR);var last=sections.length?sections[sections.length-1]:null;if(last&&last!==card){var siblings=Array.prototype.slice.call(region.children),next=siblings[siblings.indexOf(last)+1]||null;region.insertBefore(card,next);}else region.appendChild(card);dashboardPinNoticeAnchor(region);}
    function dashboardApplyLayout(layout){var region=dashboardRegionEl('main');if(!region)return;dashboardPinNoticeAnchor(region);var ids=(layout&&layout.main)||[];for(var i=ids.length-1;i>=0;i--){if(dashboardFeatureSectionHidden(ids[i]))continue;var card=dashboardCardEl(ids[i]);if(card)dashboardInsertSection(region,card,dashboardFirstSection(region));}dashboardApplyHiddenSections();dashboardUpdateResetButton();dashboardSortSidebar();}
    function dashboardSidebarNavItems(){var order=dashboardLayoutNormalize(dashboardLayoutCurrent()).main,rank={};order.forEach(function(id,i){rank[id]=i;});return Array.prototype.slice.call(document.querySelectorAll('.oc-nav-item[data-section]')).filter(function(item){return rank[item.getAttribute('data-section')]!==undefined;});}
    function dashboardEnsureSidebarHandles(){dashboardSidebarNavItems().forEach(function(item){var sectionId=item.getAttribute('data-section'),label=(item.querySelector('.oc-nav-text')||item).textContent.trim()||sectionId;if(!item.querySelector(':scope > .oc-nav-drag-handle')){var handle=document.createElement('button');handle.type='button';handle.className='oc-nav-drag-handle';handle.draggable=true;handle.setAttribute('data-nav-order-grip','dashboard');handle.setAttribute('data-section',sectionId);handle.setAttribute('data-stop','1');handle.setAttribute('aria-label','Move '+label+' section');handle.setAttribute('aria-pressed','false');handle.title='Drag or focus and press Alt+↑/↓ to reorder';handle.textContent='⠿';item.insertBefore(handle,item.firstChild);}if(dashboardCanHideSection(sectionId)&&!item.querySelector(':scope > .oc-nav-hide-toggle')){var hide=document.createElement('button');hide.type='button';hide.className='oc-nav-hide-toggle';hide.setAttribute('data-action','toggleDashboardSectionHidden');hide.setAttribute('data-arg0',sectionId);hide.setAttribute('data-section',sectionId);hide.setAttribute('data-stop','1');hide.setAttribute('aria-label','Hide '+label);hide.setAttribute('aria-pressed','false');hide.title='Hide '+label;hide.textContent='⊘';item.appendChild(hide);}});dashboardApplyHiddenSections();}
    function dashboardSidebarLayoutFromDom(){var ids=dashboardSidebarNavItems().map(function(item){return item.getAttribute('data-section');}).filter(Boolean);return dashboardLayoutNormalize({v:DASHBOARD_LAYOUT_VERSION,main:ids});}
    function dashboardApplyOrderFromSidebar(){var layout=dashboardSidebarLayoutFromDom();dashboardApplyLayout(layout);dashboardLayoutWrite();dashboardLayoutAnnounce('Dashboard sidebar order saved.');}
    function dashboardSortSidebar(){var order=dashboardLayoutNormalize(dashboardLayoutCurrent()).main,rank={};order.forEach(function(id,i){rank[id]=i;});var items=dashboardSidebarNavItems();items.sort(function(a,b){return rank[a.getAttribute('data-section')]-rank[b.getAttribute('data-section')];});var home=document.getElementById('dashboard-section-nav-group')||(items.length&&items[0].closest('.oc-nav-group'));if(!home)return;items.forEach(function(item){home.appendChild(item);});dashboardEnsureSidebarHandles();Array.prototype.forEach.call(document.querySelectorAll('.oc-nav-group'),function(group){if(group===home||group.id==='oc-agent-nav-group'){group.hidden=false;return;}group.hidden=group.querySelectorAll('.oc-nav-item').length===0;});}
    function dashboardLayoutSlot(region,x,y,skip){var cards=Array.prototype.filter.call(region.querySelectorAll(':scope > '+DASHBOARD_LAYOUT_CARD_SELECTOR),function(c){return c!==skip;});if(!cards.length)return {region:region,before:null};var best=null,bestDist=Infinity;cards.forEach(function(card){var rect=card.getBoundingClientRect(),before=y<(rect.top+rect.height/2),anchor=before?rect.top:rect.bottom,dist=Math.abs(y-anchor)+Math.abs(x-(rect.left+rect.width/2));if(dist<bestDist){bestDist=dist;best={region:region,before:before?card:card.nextElementSibling};}});return best||{region:region,before:null};}
    function dashboardNearestRegion(x,y){return dashboardRegionEl('main');}
    function dashboardMoveIndicator(slot){var indicator=dashboardDragState&&dashboardDragState.indicator;if(!indicator||!slot||!slot.region)return;dashboardInsertSection(slot.region,indicator,slot.before);}
    function dashboardClearIndicator(){if(dashboardDragState&&dashboardDragState.indicator&&dashboardDragState.indicator.parentElement)dashboardDragState.indicator.parentElement.removeChild(dashboardDragState.indicator);}
    function dashboardStartDrag(ev,grip){if(ev.button!==undefined&&ev.button!==0)return;var card=grip.closest(DASHBOARD_LAYOUT_CARD_SELECTOR);if(!card)return;ev.preventDefault();var rect=card.getBoundingClientRect();var ghost=card.cloneNode(true);ghost.classList.add('dashboard-section-ghost');ghost.style.width=rect.width+'px';ghost.style.left='0';ghost.style.top='0';ghost.style.setProperty('--dashboard-ghost-x',rect.left+'px');ghost.style.setProperty('--dashboard-ghost-y',rect.top+'px');var indicator=document.createElement('div');indicator.className='dashboard-drop-indicator';card.classList.add('dashboard-drag-source');document.body.appendChild(ghost);dashboardDragState={card:card,grip:grip,ghost:ghost,indicator:indicator,dx:ev.clientX-rect.left,dy:ev.clientY-rect.top,slot:null};try{grip.setPointerCapture(ev.pointerId);}catch(e){}grip.setAttribute('aria-pressed','true');dashboardLayoutAnnounce('Moving '+dashboardLayoutCardName(card)+'.');dashboardDragMove(ev);}
    function dashboardDragMove(ev){if(!dashboardDragState)return;var x=ev.clientX-dashboardDragState.dx,y=ev.clientY-dashboardDragState.dy;dashboardDragState.ghost.style.setProperty('--dashboard-ghost-x',x+'px');dashboardDragState.ghost.style.setProperty('--dashboard-ghost-y',y+'px');var region=dashboardNearestRegion(ev.clientX,ev.clientY),slot=dashboardLayoutSlot(region,ev.clientX,ev.clientY,dashboardDragState.card);dashboardDragState.slot=slot;dashboardMoveIndicator(slot);}
    function dashboardEndDrag(save){if(!dashboardDragState)return;var st=dashboardDragState;dashboardClearIndicator();if(st.ghost&&st.ghost.parentElement)st.ghost.parentElement.removeChild(st.ghost);st.card.classList.remove('dashboard-drag-source');st.grip.setAttribute('aria-pressed','false');if(save&&st.slot&&st.slot.region){dashboardInsertSection(st.slot.region,st.card,st.slot.before);dashboardLayoutWrite();dashboardLayoutAnnounce(dashboardLayoutCardName(st.card)+' moved.');}else{dashboardLayoutAnnounce('Move cancelled.');}dashboardDragState=null;}
    function dashboardRestoreSnapshot(){if(!dashboardKeyboardSnapshot)return;dashboardApplyLayout(dashboardKeyboardSnapshot);dashboardLayoutWrite();dashboardKeyboardSnapshot=dashboardLayoutCurrent();dashboardLayoutAnnounce('Keyboard reorder cancelled.');}
    function dashboardMoveCardByKeyboard(card,key){var current=card.parentElement;if(key==='ArrowUp'){var prev=card.previousElementSibling;while(prev&&!prev.matches(DASHBOARD_LAYOUT_CARD_SELECTOR))prev=prev.previousElementSibling;if(prev)dashboardInsertSection(current,card,prev);}else if(key==='ArrowDown'){var next=card.nextElementSibling;while(next&&!next.matches(DASHBOARD_LAYOUT_CARD_SELECTOR))next=next.nextElementSibling;if(next)dashboardInsertSection(current,next,card);}dashboardLayoutWrite();dashboardLayoutAnnounce(dashboardLayoutCardName(card)+' moved.');}
    function initDashboardSectionLayout(){dashboardEnsureSections();initializeDashboardSectionCards();dashboardApplyLayout(dashboardLayoutNormalize(dashboardLayoutRead()));var root=dashboardRegionEl('main');if(!root)return;root.addEventListener('click',function(ev){var grip=ev.target.closest&&ev.target.closest(DASHBOARD_LAYOUT_GRIP_SELECTOR);if(grip){ev.preventDefault();ev.stopPropagation();}},true);root.addEventListener('pointerdown',function(ev){var grip=ev.target.closest&&ev.target.closest(DASHBOARD_LAYOUT_GRIP_SELECTOR);if(grip)dashboardStartDrag(ev,grip);});root.addEventListener('pointermove',function(ev){if(dashboardDragState){ev.preventDefault();dashboardDragMove(ev);}});root.addEventListener('pointerup',function(ev){if(dashboardDragState){ev.preventDefault();dashboardEndDrag(true);}});root.addEventListener('pointercancel',function(){dashboardEndDrag(false);});root.addEventListener('keydown',function(ev){var grip=ev.target.closest&&ev.target.closest(DASHBOARD_LAYOUT_GRIP_SELECTOR);if(!grip)return;var key=ev.key,card=grip.closest(DASHBOARD_LAYOUT_CARD_SELECTOR);if(!card)return;if(key===' '||key==='Enter'){ev.preventDefault();var active=grip.getAttribute('aria-pressed')==='true';if(active){dashboardKeyboardSnapshot=dashboardLayoutCurrent();grip.setAttribute('aria-pressed','false');dashboardLayoutAnnounce('Dropped '+dashboardLayoutCardName(card)+'.');}else{dashboardKeyboardSnapshot=dashboardLayoutCurrent();grip.setAttribute('aria-pressed','true');dashboardLayoutAnnounce('Picked up '+dashboardLayoutCardName(card)+'. Use arrow keys to move, Escape to cancel.');}return;}if(key==='Escape'){ev.preventDefault();dashboardRestoreSnapshot();grip.setAttribute('aria-pressed','false');return;}if(key==='ArrowUp'||key==='ArrowDown'){ev.preventDefault();if(!dashboardKeyboardSnapshot)dashboardKeyboardSnapshot=dashboardLayoutCurrent();grip.setAttribute('aria-pressed','true');dashboardMoveCardByKeyboard(card,key);}});root.addEventListener('focusin',function(ev){var grip=ev.target.closest&&ev.target.closest(DASHBOARD_LAYOUT_GRIP_SELECTOR);if(grip)dashboardKeyboardSnapshot=dashboardLayoutCurrent();});root.addEventListener('focusout',function(ev){var grip=ev.target.closest&&ev.target.closest(DASHBOARD_LAYOUT_GRIP_SELECTOR);if(grip){dashboardKeyboardSnapshot=null;grip.setAttribute('aria-pressed','false');}});dashboardUpdateResetButton();initDashboardSidebarLayoutControls();}
    function initDashboardSidebarLayoutControls(){var group=document.getElementById('dashboard-section-nav-group');if(!group||group.dataset.dashboardNavLayoutControls==='1')return;group.dataset.dashboardNavLayoutControls='1';var dragged=null;function clear(){group.querySelectorAll(':scope > .oc-drop-indicator').forEach(function(el){el.remove();});group.querySelectorAll('.oc-nav-item.dragging').forEach(function(el){el.classList.remove('dragging');});group.querySelectorAll('.oc-nav-drag-handle[aria-pressed="true"]').forEach(function(el){el.setAttribute('aria-pressed','false');});dragged=null;}function targetFor(y){var items=dashboardSidebarNavItems().filter(function(item){return item!==dragged;});var best=null,dist=Infinity;items.forEach(function(item){var rect=item.getBoundingClientRect(),mid=rect.top+rect.height/2,d=Math.abs(y-mid);if(d<dist){dist=d;best={item:item,after:y>mid};}});return best;}group.addEventListener('click',function(ev){var grip=ev.target.closest&&ev.target.closest('[data-nav-order-grip="dashboard"]');if(grip){ev.preventDefault();ev.stopPropagation();}},true);group.addEventListener('dragstart',function(ev){var grip=ev.target.closest&&ev.target.closest('[data-nav-order-grip="dashboard"]');if(!grip)return;dragged=grip.closest('.oc-nav-item[data-section]');if(!dragged)return;dragged.classList.add('dragging');grip.setAttribute('aria-pressed','true');ev.dataTransfer.effectAllowed='move';ev.dataTransfer.setData('text/plain',dragged.getAttribute('data-section')||'');});group.addEventListener('dragover',function(ev){if(!dragged)return;ev.preventDefault();ev.dataTransfer.dropEffect='move';group.querySelectorAll(':scope > .oc-drop-indicator').forEach(function(el){el.remove();});var t=targetFor(ev.clientY),indicator=document.createElement('div');indicator.className='oc-drop-indicator';if(t){if(t.after)t.item.after(indicator);else t.item.before(indicator);}else group.appendChild(indicator);});group.addEventListener('drop',function(ev){if(!dragged)return;ev.preventDefault();var t=targetFor(ev.clientY);if(t){if(t.after)t.item.after(dragged);else t.item.before(dragged);}else group.appendChild(dragged);dashboardApplyOrderFromSidebar();clear();});group.addEventListener('dragend',clear);group.addEventListener('keydown',function(ev){var grip=ev.target.closest&&ev.target.closest('[data-nav-order-grip="dashboard"]');if(!grip||!ev.altKey||(ev.key!=='ArrowUp'&&ev.key!=='ArrowDown'))return;var item=grip.closest('.oc-nav-item[data-section]');if(!item)return;ev.preventDefault();if(ev.key==='ArrowUp'){var prev=item.previousElementSibling;while(prev&&!(prev.matches&&prev.matches('.oc-nav-item[data-section]')))prev=prev.previousElementSibling;if(prev)group.insertBefore(item,prev);}else{var next=item.nextElementSibling;while(next&&!(next.matches&&next.matches('.oc-nav-item[data-section]')))next=next.nextElementSibling;if(next)group.insertBefore(next,item);}dashboardApplyOrderFromSidebar();var nextGrip=item.querySelector('[data-nav-order-grip="dashboard"]');if(nextGrip)nextGrip.focus();});}
    document.addEventListener('DOMContentLoaded', initDashboardSectionLayout);

    /** Check whether an agent card should be compact */
    function isAgentCompact(agentName) {
      if (localStorage.getItem(ALL_AGENTS_COMPACT_KEY) === '1') return true;
      return localStorage.getItem(AGENT_COMPACT_LS_PREFIX + agentName) === '1';
    }
    /** Persist agent compact state */
    function setAgentCompact(agentName, compact) {
      if (compact) {
        localStorage.setItem(AGENT_COMPACT_LS_PREFIX + agentName, '1');
      } else {
        localStorage.removeItem(AGENT_COMPACT_LS_PREFIX + agentName);
      }
    }
    /** Toggle compact mode for one agent card */
    function toggleAgentCompact(agentName) {
      const compact = !isAgentCompact(agentName);
      setAgentCompact(agentName, compact);
      // Clear all-agents-compact if user is expanding a single card
      if (!compact) localStorage.removeItem(ALL_AGENTS_COMPACT_KEY);
      const card = document.querySelector(`.agent-card[data-agent="${agentName}"]`);
      if (card) card.classList.toggle('compact', compact);
      dashboardUpdateLayoutMenuState();
    }
    /** Toggle all agent cards compact/expanded */
    function toggleAllAgentsCompact() {
      const allCompact = localStorage.getItem(ALL_AGENTS_COMPACT_KEY) === '1';
      if (allCompact) {
        localStorage.removeItem(ALL_AGENTS_COMPACT_KEY);
        document.querySelectorAll('.agent-card').forEach(c => c.classList.remove('compact'));
        // Clear all individual compact states too
        Object.keys(localStorage).forEach(k => {
          if (k.startsWith(AGENT_COMPACT_LS_PREFIX)) localStorage.removeItem(k);
        });
      } else {
        localStorage.setItem(ALL_AGENTS_COMPACT_KEY, '1');
        document.querySelectorAll('.agent-card').forEach(c => c.classList.add('compact'));
      }
      // Update button text
      const btn = document.getElementById('agents-compact-all-btn');
      if (btn) btn.textContent = localStorage.getItem(ALL_AGENTS_COMPACT_KEY) === '1' ? '⊞ Expand All' : '⊟ Compact All';
      dashboardUpdateLayoutMenuState();
    }

    // ── Fleet breaker (kill-switch) ──
    // A single control that pauses every running agent at once ("engage") and
    // restores exactly the agents it paused ("release"). On-demand agents and
    // any agent the operator paused themselves are never touched by a release —
    // that invariant is enforced server-side (Manager.EngageBreaker/
    // ReleaseBreaker); the UI only reflects and triggers it.
    let _fleetBreakerEngaged = false;
    // Renders the topbar breaker as one status chip button: state dot + label +
    // action icon. Owners can toggle it; read-only viewers get the same chip in a
    // disabled status-only state (server also 403s non-owner POSTs).
    function _renderFleetBreaker(engaged, agents) {
      _fleetBreakerEngaged = !!engaged;
      const wrap = document.getElementById('fleet-breaker-wrap');
      if (!wrap) return;
      wrap.style.display = '';
      const count = (agents || []).length;
      const btn = document.getElementById('fleet-breaker-btn2');
      if (btn) {
        const fleetStatus = statusMeta(_fleetBreakerEngaged ? 'paused' : 'running');
        const canToggle = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
        const label = document.getElementById('fleet-breaker-pill');
        const dot = document.getElementById('fleet-breaker-dot');
        const icon = document.getElementById('fleet-breaker-icon');
        const stateText = _fleetBreakerEngaged ? 'paused' : fleetStatus.label;
        const actionText = _fleetBreakerEngaged ? 'resume paused agents' : 'pause all agents';
        btn.style.display = '';
        btn.classList.toggle('paused', _fleetBreakerEngaged);
        btn.dataset.status = fleetStatus.status;
        btn.disabled = !canToggle;
        btn.setAttribute('aria-disabled', canToggle ? 'false' : 'true');
        btn.setAttribute('aria-pressed', _fleetBreakerEngaged ? 'true' : 'false');
        btn.setAttribute('aria-label', canToggle
          ? ('Fleet breaker: ' + stateText + ' — click to ' + actionText)
          : ('Fleet breaker: ' + stateText));
        btn.title = _fleetBreakerEngaged
          ? ('Fleet breaker is ENGAGED — ' + count + ' agent' + (count === 1 ? '' : 's') +
             ' held paused. Release resumes only those; on-demand and manually-paused agents are untouched.')
          : 'Fleet breaker: pause every running agent at once. Releasing restores only the agents this breaker paused — on-demand agents and any agent you paused yourself are never touched.';
        if (label) label.textContent = stateText;
        if (dot) dot.textContent = _fleetBreakerEngaged ? '○' : '●';
        if (icon) icon.innerHTML = _fleetBreakerEngaged
          ? '<svg viewBox="0 0 12 12" aria-hidden="true"><path d="M3.5 2.2v7.6a.6.6 0 0 0 .92.5l6-3.8a.6.6 0 0 0 0-1l-6-3.8a.6.6 0 0 0-.92.5Z"/></svg>'
          : '<svg viewBox="0 0 12 12" aria-hidden="true"><rect x="2.5" y="2" width="2.4" height="8" rx="0.6"/><rect x="7.1" y="2" width="2.4" height="8" rx="0.6"/></svg>';
      }
      // When engaged, make it obvious the whole fleet is held.
      const section = document.getElementById('agents-section');
      if (section) section.style.outline = _fleetBreakerEngaged ? '2px solid var(--status-error)' : '';
    }
    async function refreshFleetBreakerState() {
      try {
        const res = await fetch('/api/breaker');
        const data = await res.json();
        if (data && data.ok) _renderFleetBreaker(data.engaged, data.agents);
      } catch (e) { /* leave last-known state on transient error */ }
    }
    async function toggleFleetBreaker() {
      const engaging = !_fleetBreakerEngaged;
      const path = engaging ? '/api/breaker/engage' : '/api/breaker/release';
      const btn = document.getElementById('fleet-breaker-btn2');
      if (btn) btn.disabled = true;
      try {
        const res = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' } });
        const data = await res.json();
        if (!data.ok) { showToast('Fleet breaker failed: ' + (data.error || 'unknown'), 'error'); return; }
        const n = (data.agents || []).length;
        if (engaging) showToast('Fleet breaker engaged — paused ' + n + ' agent' + (n === 1 ? '' : 's'), 'success');
        else showToast('Fleet breaker released — resumed ' + n + ' agent' + (n === 1 ? '' : 's'), 'success');
        _renderFleetBreaker(data.engaged, data.agents);
      } catch (e) {
        showToast('Fleet breaker failed: ' + e.message, 'error');
      } finally {
        if (btn) btn.disabled = !dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      }
    }
    document.addEventListener('DOMContentLoaded', refreshFleetBreakerState);

    // ── Layout mode (Dark / Light) ──
    const LAYOUT_KEY = 'hive-layout-mode';
    const LAYOUT_MODES = ['dark', 'light'];
    // State-explicit labels: the button names the ACTION ("Switch to …"),
    // never the current mode, so it cannot be misread as a state indicator.
    // Icon-only: sun ☀ when in dark mode (click to go light), moon ☾ when in
    // light mode (click to go dark). The full meaning stays in the title tooltip.
    const LAYOUT_LABELS = { dark: '☀', light: '☾' };
    const LAYOUT_TITLES = { dark: 'Switch to Light Mode', light: 'Switch to Dark Mode' };
    function getLayout() {
      let stored = localStorage.getItem(LAYOUT_KEY) || 'light';
      if (window.hiveKioskMode && window.hiveKioskMode()) { let kt = null; try { kt = new URL(window.location.href).searchParams.get('theme'); } catch (e) {} stored = kt === 'light' ? 'light' : 'dark'; }
      if (stored === 'openclaw') { stored = 'light'; setLayout(stored); }
      if (stored === 'classic') { stored = 'dark'; setLayout(stored); }
      return LAYOUT_MODES.includes(stored) ? stored : 'light';
    }
    function setLayout(mode) { localStorage.setItem(LAYOUT_KEY, mode); }
    function applyLayout(mode) {
      document.body.classList.toggle('light-mode', mode === 'light');
      const customStyleRoot = document.getElementById('hive-dashboard-root');
      if (customStyleRoot) { customStyleRoot.classList.toggle('light-mode', mode === 'light'); customStyleRoot.classList.toggle('dark-mode', mode !== 'light'); }
      const lbl = LAYOUT_LABELS[mode] || LAYOUT_LABELS.dark;
      const ttl = LAYOUT_TITLES[mode] || LAYOUT_TITLES.dark;
      document.querySelectorAll('.layout-toggle').forEach(btn => {
        btn.textContent = lbl;
        btn.title = ttl;
        btn.setAttribute('aria-label', ttl);
        btn.classList.toggle('active', mode !== 'dark');
      });
      const sidebar = document.getElementById('oc-sidebar');
      if (sidebar) sidebar.style.display = 'flex';
      const h1 = document.querySelector('body > h1');
      if (h1) h1.style.display = 'none';
      const topbar = document.getElementById('oc-topbar');
      if (topbar) topbar.style.display = '';
    }
    function toggleLayout() {
      const current = getLayout();
      const idx = LAYOUT_MODES.indexOf(current);
      const next = LAYOUT_MODES[(idx + 1) % LAYOUT_MODES.length];
      setLayout(next);
      applyLayout(next);
    }
    // Body scroll-lock while ANY modal overlay is open — otherwise wheel
    // scrolling leaks through the backdrop to the dashboard behind it. A
    // single observer covers every overlay (config, acmm, welcome, gh-auth,
    // hive-dialog, nous) and any added later; per-modal open/close handlers
    // no longer need to touch body.style.overflow.
    (function installModalScrollLock() {
      var OVERLAY_SELECTOR = '.config-overlay, .gh-auth-overlay, .cred-dialog-overlay, .hive-dialog-overlay, .nous-config-overlay, .acmm-dialog-overlay, .hive-chat-shortcuts, #oc-drawer-backdrop';
      function anyOverlayOpen() {
        var overlays = document.querySelectorAll(OVERLAY_SELECTOR);
        for (var i = 0; i < overlays.length; i++) {
          var el = overlays[i];
          if (el.classList.contains('hidden')) continue;
          // The mobile drawer backdrop is always display:block under 1024px and
          // animates on opacity, so display alone would read as permanently
          // open. It is open only while it carries .open.
          if (el.id === 'oc-drawer-backdrop') {
            if (el.classList.contains('open')) return true;
            continue;
          }
          var disp = el.style.display || getComputedStyle(el).display;
          if (disp !== 'none') return true;
        }
        return false;
      }
      // Toggle on BOTH the root and the body element (#7250):
      // `html { overflow-x: hidden }` makes the root the viewport scroll
      // container, so locking the body alone is a no-op and the page scrolls
      // behind the modal.
      function sync() {
        var open = anyOverlayOpen();
        document.documentElement.classList.toggle('modal-open', open);
        document.body.classList.toggle('modal-open', open);
        // Same hook wires the shared scroll affordance onto every modal
        // scroll region, static or injected later (nous/acmm bodies).
        if (typeof hiveModalScrollInit === 'function') {
          document.querySelectorAll('.modal-scroll:not([data-modal-scroll-wired])').forEach(hiveModalScrollInit);
        }
      }
      // Overlays created after startup (kbShowModal's #kb-modal-overlay, the
      // acmm dialog) are appended once and then toggled via style.display, so
      // they need their own attribute observers too — otherwise closing one
      // leaves body.modal-open stuck, and reopening it never re-locks (#4805).
      // Re-observing an already-observed node just replaces the registration,
      // so calling this on every childList mutation is safe.
      function observeOverlays() {
        document.querySelectorAll(OVERLAY_SELECTOR).forEach(function(el) {
          obs.observe(el, { attributes: true, attributeFilter: ['class', 'style'] });
        });
      }
      var obs = new MutationObserver(function(muts) {
        for (var i = 0; i < muts.length; i++) {
          if (muts[i].type === 'childList') { observeOverlays(); break; }
        }
        sync();
      });
      function start() {
        observeOverlays();
        // Catch overlays created later (e.g. acmm dialog appended on first open).
        obs.observe(document.body, { childList: true, subtree: false });
        sync();
      }
      if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', start);
      } else { start(); }
    })();

    const OC_TOPBAR_HEIGHT = 64;
