package hub

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestNoNativeBrowserDialogsRatchet(t *testing.T) {
	native := regexp.MustCompile(`\b(?:window\.)?(?:prompt|alert|confirm)\s*\(|showModalDialog\s*\(|\.showModal\s*\(|onbeforeunload`)
	var offenders []string
	for i, line := range strings.Split(dashboardHTML, "\n") {
		if native.MatchString(line) {
			offenders = append(offenders, "dashboard.html:"+itoaHub(i+1)+": "+strings.TrimSpace(line))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("native browser dialog calls are forbidden in hub UI; use hivePrompt, hiveConfirm, or hiveNotify/hiveToast:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func itoaHub(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestHubPromptAndConfirmNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	script := hubDialogHarnessJS() +
		extractHubJSFunction(t, dashboardHTML, "hiveDialogOptions") +
		extractHubJSFunction(t, dashboardHTML, "hiveDialogWire") +
		extractHubJSFunction(t, dashboardHTML, "hiveConfirmIsDanger") +
		extractHubJSFunction(t, dashboardHTML, "hiveConfirm") +
		extractHubJSFunction(t, dashboardHTML, "hivePrompt") + `
function esc(s){return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
(async () => {
  const out = [];
  let p = hivePrompt({title:'Name', label:'View name', defaultValue:' old '});
  await tick(); document.querySelector('input').value = '  saved  '; document.querySelector('[data-act="yes"]').click(); out.push(await p);
  p = hivePrompt({title:'Name', label:'View name'});
  await tick(); dispatchKey('Escape'); out.push(await p);
  p = hiveConfirm({title:'Delete?', message:'Delete view?', danger:true});
  await tick(); document.querySelector('[data-act="yes"]').click(); out.push(await p);
  p = hiveConfirm({title:'Delete?', message:'Delete view?'});
  await tick(); document.querySelector('[data-act="no"]').click(); out.push(await p);
  console.log(JSON.stringify(out));
})();`
	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node dialog test failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != `["saved",null,true,false]` {
		t.Fatalf("unexpected dialog results: %s", got)
	}
}

func extractHubJSFunction(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "function "+name+"(")
	if start < 0 {
		t.Fatalf("function %s not found", name)
	}
	depth := 0
	seen := false
	for i := start; i < len(html); i++ {
		switch html[i] {
		case '{':
			depth++
			seen = true
		case '}':
			depth--
			if seen && depth == 0 {
				return html[start:i+1] + "\n"
			}
		}
	}
	t.Fatalf("function %s not closed", name)
	return ""
}

func hubDialogHarnessJS() string {
	return `
class Element{constructor(tag){this.tagName=tag.toUpperCase();this.children=[];this.parentNode=null;this.attributes={};this.dataset={};this.eventListeners={};this.disabled=false;this.offsetParent={};this.value='';this.textContent='';this.className='';}setAttribute(k,v){this.attributes[k]=String(v);if(k==='id')this.id=String(v);if(k==='class')this.className=String(v);if(k.startsWith('data-'))this.dataset[k.slice(5).replace(/-([a-z])/g,(_,c)=>c.toUpperCase())]=String(v);}appendChild(c){c.parentNode=this;this.children.push(c);return c;}remove(){if(this.parentNode)this.parentNode.children=this.parentNode.children.filter(x=>x!==this);}addEventListener(t,fn){(this.eventListeners[t]||(this.eventListeners[t]=[])).push(fn);}click(){(this.eventListeners.click||[]).forEach(fn=>fn({target:this}));if(this.onclick)this.onclick({target:this});}focus(){document.activeElement=this;}select(){this.selected=true;}querySelector(s){return query(this,s,true);}querySelectorAll(s){return query(this,s,false);}set innerHTML(html){this.children=[];parse(html,this);}}
function parse(html,root){const re=/<(div|button|input|label|h3|p)[^>]*>/g;let m;while((m=re.exec(html))){const el=new Element(m[1]);m[0].replace(/([a-zA-Z0-9_-]+)="([^"]*)"/g,(_,k,v)=>el.setAttribute(k,v));root.appendChild(el);}}
function match(el,s){if(s.startsWith('#'))return el.id===s.slice(1);if(s.startsWith('.'))return (el.className||'').split(/\s+/).includes(s.slice(1));if(s==='input')return el.tagName==='INPUT';let m=s.match(/^\[data-([^=\]]+)(?:="([^"]*)")?\]$/);if(m){let k=m[1].replace(/-([a-z])/g,(_,c)=>c.toUpperCase());return m[2]===undefined?el.dataset[k]!==undefined:el.dataset[k]===m[2];}return false;}
function query(root,s,one){let out=[];function walk(n){for(const c of n.children){if(match(c,s))out.push(c);walk(c);}}walk(root);return one?(out[0]||null):out;}
const document={body:new Element('body'),activeElement:null,listeners:{},createElement:t=>new Element(t),addEventListener(t,fn){(this.listeners[t]||(this.listeners[t]=[])).push(fn);},removeEventListener(t,fn){this.listeners[t]=(this.listeners[t]||[]).filter(x=>x!==fn);},querySelector(s){return this.body.querySelector(s);},querySelectorAll(s){return this.body.querySelectorAll(s);}};
function dispatchKey(key){(document.listeners.keydown||[]).forEach(fn=>fn({key,preventDefault(){}}));}
function tick(){return new Promise(r=>setTimeout(r,1));}
global.document=document;
`
}
