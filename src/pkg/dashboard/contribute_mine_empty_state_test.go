package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #12111: the hub owner opened the Operations tab of their own hosted spoke
// and the "Your contribution" card said "could not be loaded. This is a bug".
// They had no contributor profile there, so the spoke answered 403 — and the
// Ingress, inheriting a controller-wide custom-http-errors list, replaced that
// JSON with an HTML error page served as 200. ccLoadMine parsed HTML as JSON,
// threw, and reported the fault. The server now answers that state as a 200
// with has_profile:false; this test RUNS ccLoadMine against the three answers
// a spoke can give and checks what the card paints for each.
//
// Skips loudly when node is unavailable, like the other behavioural tests.
func TestMineCardEmptyStateBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the Your-contribution empty-state behaviour was NOT executed by this run")
	}

	html := renderContributePage(t)
	script := strings.Join([]string{
		mineCardHarness,
		jsFunc(t, html, "ccLoadMine"),
		jsFunc(t, html, "ccRenderMineMessage"),
		jsFunc(t, html, "ccRenderMineSignIn"),
		jsFunc(t, html, "ccRenderMineNoProfile"),
		jsFunc(t, html, "ccRenderMineError"),
		mineCardAssertions,
	}, "\n")

	path := filepath.Join(t.TempDir(), "mine.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("Your-contribution empty-state behaviour check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

// mineCardHarness is the minimum of the page ccLoadMine touches: the globals
// it reads and writes, a DOM of the four ids it paints, and a fetch whose
// answer each scenario sets. ccRenderMine is a stub that records it ran, so a
// scenario can assert the tile grid was or was not painted.
const mineCardHarness = `
var ccMineData=null, ccMineLast=0, ccMineMinGap=30000, ccMineHandled={}, ccMeUsername='';
var tilesPainted=0;
function ccRenderMine(){tilesPainted++;}
function ccRenderMineSpark(){}
function esc(s){return String(s).replace(/[&<>"']/g,function(c){return '&#'+c.charCodeAt(0)+';';});}
function ccSignInCTA(label){return '<a href="/login">'+label+'</a>';}
var els={};
function el(id){els[id]={id:id,innerHTML:'',textContent:'',style:{display:'none'},classList:{_s:{},add:function(c){this._s[c]=1;},remove:function(c){delete this._s[c];},contains:function(c){return !!this._s[c];}}};return els[id];}
['cc-mine-card','cc-mine-body','cc-mine-tier','spark-mine','cc-mine-note'].forEach(el);
var document={getElementById:function(id){return els[id]||null;}};
var nextResponse=null;
var consoleErrors=[];
var realError=console.error;
console.error=function(){consoleErrors.push(Array.prototype.slice.call(arguments).map(String).join(' '));};
function fetch(url){return Promise.resolve(nextResponse);}
function respond(status,body,contentType){
  var text=typeof body==='string'?body:JSON.stringify(body);
  return {status:status,ok:status>=200&&status<300,
    headers:{get:function(k){return k.toLowerCase()==='content-type'?contentType:null;}},
    json:function(){return Promise.resolve().then(function(){return JSON.parse(text);});}};
}
function reset(){['cc-mine-card','cc-mine-body','cc-mine-tier','spark-mine','cc-mine-note'].forEach(el);ccMineData=null;ccMineLast=0;ccMeUsername='';tilesPainted=0;consoleErrors=[];}
`

// mineCardAssertions runs the scenarios. Each awaits the promise chain
// ccLoadMine starts (it returns nothing, so a tick is awaited instead).
const mineCardAssertions = `
var fails=0;
function check(name,cond,detail){if(!cond){fails++;console.log('FAIL '+name+(detail?('\n  '+detail):''));}}
function settle(){return new Promise(function(r){setTimeout(r,0);}).then(function(){return new Promise(function(r){setTimeout(r,0);});});}
var body=function(){return els['cc-mine-body'].innerHTML;};
var card=function(){return els['cc-mine-card'];};

(async function(){
  // 1. The #12111 state on a fixed spoke: 200, has_profile:false. The card
  //    must reveal an empty state naming the viewer, and must NOT say "bug".
  reset();
  nextResponse=respond(200,{github_username:'clubanderson',has_profile:false},'application/json');
  ccLoadMine(true); await settle();
  check('empty state reveals the card', card().style.display==='', 'display='+card().style.display);
  check('empty state names the viewer', body().indexOf('clubanderson')>=0, body());
  check('empty state points at connecting an agent', body().indexOf('connect a contributor agent')>=0, body());
  check('empty state is not the bug message', body().indexOf('This is a bug')<0, body());
  check('empty state paints no tiles', tilesPainted===0, 'tilesPainted='+tilesPainted);
  check('empty state logs no error', consoleErrors.length===0, consoleErrors.join(' | '));
  check('empty state adopts the identity', ccMeUsername==='clubanderson', ccMeUsername);
  check('empty state keeps no stats', ccMineData===null, JSON.stringify(ccMineData));

  // 2. A spoke older than the fix still answers 403; same empty state.
  reset(); ccMeUsername='clubanderson';
  nextResponse=respond(403,{ok:false,error:'You need a contributor profile'},'application/json');
  ccLoadMine(true); await settle();
  check('403 reveals the empty state', card().style.display===''&&body().indexOf('connect a contributor agent')>=0, body());
  check('403 is not the bug message', body().indexOf('This is a bug')<0, body());

  // 3. The symptom as the browser saw it: the Ingress error page as a 200.
  //    That IS a fault, and the console must say which layer answered.
  reset();
  nextResponse=respond(200,'<!DOCTYPE html><html>Access Denied</html>','text/html');
  ccLoadMine(true); await settle();
  check('HTML 200 is reported as a fault', body().indexOf('This is a bug')>=0, body());
  check('HTML 200 names the intermediary', consoleErrors.join(' ').indexOf('intermediary')>=0, consoleErrors.join(' | '));
  check('HTML 200 paints no tiles', tilesPainted===0, 'tilesPainted='+tilesPainted);

  // 4. A populated record still paints the tiles.
  reset();
  nextResponse=respond(200,{github_username:'mrbobbytables',has_profile:true,total_tasks_completed:3},'application/json');
  ccLoadMine(true); await settle();
  check('populated record paints tiles', tilesPainted===1, 'tilesPainted='+tilesPainted);
  check('populated record is kept', ccMineData&&ccMineData.total_tasks_completed===3, JSON.stringify(ccMineData));

  // 5. Anonymous: the sign-in prompt, unchanged.
  reset();
  nextResponse=respond(401,{ok:false,error:'Sign in'},'application/json');
  ccLoadMine(true); await settle();
  check('401 prompts sign-in', body().indexOf('Sign in with GitHub')>=0, body());

  if(fails){console.log(fails+' check(s) failed');process.exit(1);}
})().catch(function(e){console.log('harness threw: '+(e&&e.stack||e));process.exit(1);});
`
