package dashboard

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestNoNativeBrowserDialogsRatchet(t *testing.T) {
	native := regexp.MustCompile(`\b(?:window\.)?(?:prompt|alert|confirm)\s*\(|showModalDialog\s*\(|\.showModal\s*\(|onbeforeunload`)
	files := map[string]string{
		"static/index.html":     indexHTML(t),
		"contribute_landing.go": mustReadDashboardFile(t, "contribute_landing.go"),
	}
	var offenders []string
	for name, body := range files {
		for i, line := range strings.Split(body, "\n") {
			if native.MatchString(line) {
				offenders = append(offenders, name+":"+itoaNoNativeDialog(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("native browser dialog calls are forbidden in spoke UI; use hivePrompt, hiveConfirm, hiveAlert/showToast, or the contribute in-app admin modal:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func mustReadDashboardFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

func itoaNoNativeDialog(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestHivePromptAndConfirmNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := dialogHarnessJS() +
		extractJSFunction(t, html, "hiveDialogEscape") + "\n    }\n" +
		extractJSFunction(t, html, "hiveNormalizeDialogOptions") + "\n    }\n" +
		extractJSFunction(t, html, "hiveWireDialog") + "\n    }\n" +
		extractJSFunction(t, html, "hiveConfirmIsDanger") + "\n    }\n" +
		extractJSFunction(t, html, "hiveConfirm") + "\n    }\n" +
		extractJSFunction(t, html, "hivePrompt") + "\n    }\n" + `
(async () => {
  const out = [];
  let p = hivePrompt({title:'Repo', label:'GitHub repo', defaultValue:' old ', placeholder:'url'});
  await tick(); document.querySelector('.hive-dialog-input').value = '  new/repo  '; document.querySelector('.primary').click();
  out.push(await p);
  p = hivePrompt({title:'Repo', label:'GitHub repo'});
  await tick(); dispatchKey('Escape'); out.push(await p);
  p = hiveConfirm({title:'Delete?', message:'Delete repo?', confirmLabel:'Delete', danger:true});
  await tick(); document.querySelector('.primary').click(); out.push(await p);
  p = hiveConfirm({title:'Delete?', message:'Delete repo?'});
  await tick(); document.querySelector('.cancel').click(); out.push(await p);
  console.log(JSON.stringify(out));
})();`
	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node dialog test failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != `["new/repo",null,true,false]` {
		t.Fatalf("unexpected dialog results: %s", got)
	}
}

func dialogHarnessJS() string {
	return `
class Element {
  constructor(tag){ this.tagName=tag.toUpperCase(); this.children=[]; this.parentNode=null; this.attributes={}; this.dataset={}; this.eventListeners={}; this.disabled=false; this.offsetParent={}; this.value=''; this.textContent=''; this.className=''; }
  setAttribute(k,v){ this.attributes[k]=String(v); if(k==='id')this.id=String(v); if(k==='class'){this.className=String(v); this.classList=this.className.split(/\s+/);} if(k.startsWith('data-'))this.dataset[k.slice(5).replace(/-([a-z])/g,(_,c)=>c.toUpperCase())]=String(v); }
  appendChild(c){ c.parentNode=this; this.children.push(c); return c; }
  remove(){ if(this.parentNode)this.parentNode.children=this.parentNode.children.filter(x=>x!==this); }
  addEventListener(t,fn){ (this.eventListeners[t]||(this.eventListeners[t]=[])).push(fn); }
  click(){ (this.eventListeners.click||[]).forEach(fn=>fn({target:this})); if(this.onclick)this.onclick({target:this}); }
  focus(){ document.activeElement=this; }
  select(){ this.selected=true; }
  querySelector(sel){ return query(this,sel,true); }
  querySelectorAll(sel){ return query(this,sel,false); }
  set innerHTML(html){ this.children=[]; parse(html,this); }
}
function parse(html, root){ const re=/<(div|button|input|label)[^>]*>/g; let m; while((m=re.exec(html))){ const tag=m[1]; const el=new Element(tag); const attrs=m[0]; attrs.replace(/([a-zA-Z0-9_-]+)="([^"]*)"/g,(_,k,v)=>el.setAttribute(k,v)); root.appendChild(el); } }
function match(el,sel){ if(sel.startsWith('#'))return el.id===sel.slice(1); if(sel.startsWith('.'))return (el.className||'').split(/\s+/).includes(sel.slice(1)); if(sel==='button')return el.tagName==='BUTTON'; if(sel==='input')return el.tagName==='INPUT'; let m=sel.match(/^\[data-([^=\]]+)(?:="([^"]*)")?\]$/); if(m){let k=m[1].replace(/-([a-z])/g,(_,c)=>c.toUpperCase()); return m[2]===undefined ? el.dataset[k]!==undefined : el.dataset[k]===m[2];} return false; }
function query(root,sel,one){ let out=[]; function walk(n){ for(const c of n.children){ if(match(c,sel))out.push(c); walk(c); } } walk(root); return one ? (out[0]||null) : out; }
const document = { body:new Element('body'), activeElement:null, listeners:{}, createElement:t=>new Element(t), addEventListener(t,fn){(this.listeners[t]||(this.listeners[t]=[])).push(fn);}, removeEventListener(t,fn){this.listeners[t]=(this.listeners[t]||[]).filter(x=>x!==fn);}, querySelector(sel){return this.body.querySelector(sel);}, querySelectorAll(sel){return this.body.querySelectorAll(sel);} };
function dispatchKey(key){ (document.listeners.keydown||[]).forEach(fn=>fn({key, preventDefault(){}})); }
function tick(){ return new Promise(r=>setTimeout(r,1)); }
global.document=document;
`
}
