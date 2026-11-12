package dashboard

import (
	"log/slog"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// contribute_queue_stream_refresh_test.go pins why the Operations "Ready-work
// queue" panel kept closed issues, with the labels they were filed with, for
// hours on a tab whose event stream stayed open.
//
// The server side was never stale: /api/contribute/queue and the SSE hello frame
// both run admissionQueueSnapshot over the live status, whose ActionableIssues
// carry github.Issue values that webhook re-reads patch and remove. The page was
// stale: the stream sends the queue once, in its hello frame, and the page
// stopped its queue poll the moment the stream opened, so nothing re-read the
// queue until the stream dropped. The server tests below pin the half that was
// already right (they pass before and after the fix); the page tests pin the
// half that was wrong (they fail before it).

// rwqServer returns a server with the contribute hub wired and one repository
// whose actionable list holds issues as the status builder stores them:
// github.Issue values, not decoded maps.
func rwqServer(t *testing.T, issues ...github.Issue) *Server {
	t.Helper()
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))
	raw := make([]any, 0, len(issues))
	for _, is := range issues {
		raw = append(raw, is)
	}
	status := minimalPayload()
	status.Repos = []FrontendRepo{{Name: "widgets", Full: "acme/widgets", ActionableIssues: raw}}
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	return s
}

func rwqQueueItem(q []ReadyQueueItem, number int) (ReadyQueueItem, bool) {
	for _, it := range q {
		if it.Repo == "acme/widgets" && it.Number == number {
			return it, true
		}
	}
	return ReadyQueueItem{}, false
}

// TestReadyQueueFollowsWebhookReadsOnLiveIssueType: a labeled re-read relabels
// the queue row, a closed re-read removes it, an older open read that lands after
// the close cannot put it back, and neither can the governor publishing a
// snapshot it built before the close.
func TestReadyQueueFollowsWebhookReadsOnLiveIssueType(t *testing.T) {
	filed := time.Date(2026, 11, 11, 19, 42, 34, 0, time.UTC)
	one := github.Issue{Repo: "acme/widgets", Number: 1, Title: "Add a widget", State: "open", Labels: []string{"kind/feature"}, UpdatedAt: filed}
	two := github.Issue{Repo: "acme/widgets", Number: 2, Title: "Tidy widgets", State: "open", Labels: []string{"kind/cleanup"}, UpdatedAt: filed}
	s := rwqServer(t, one, two)
	if _, ok := rwqQueueItem(s.contributeHub.ReadyQueue(readyQueueDefaultLimit), 1); !ok {
		t.Fatal("fixture: #1 should start in the ready queue")
	}

	labeledAt := filed.Add(5 * time.Minute)
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{
		Repo: "acme/widgets", Number: 2, State: "open", Title: two.Title,
		Labels: []string{"kind/cleanup", "approved-direction"}, UpdatedAt: labeledAt, FetchedAt: labeledAt,
	}})
	got, ok := rwqQueueItem(s.contributeHub.ReadyQueue(readyQueueDefaultLimit), 2)
	if !ok || !reflect.DeepEqual(got.Labels, []string{"kind/cleanup", "approved-direction"}) {
		t.Fatalf("labeled re-read: queue row #2 = %+v (present=%v), want the re-read labels", got, ok)
	}

	closedAt := filed.Add(2 * time.Hour)
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{
		Repo: "acme/widgets", Number: 1, State: "closed", Title: one.Title, UpdatedAt: closedAt, FetchedAt: closedAt,
	}})
	if it, ok := rwqQueueItem(s.contributeHub.ReadyQueue(readyQueueDefaultLimit), 1); ok {
		t.Fatalf("closed re-read: #1 still in the ready queue: %+v", it)
	}

	// The `opened` delivery's read, landing late. It carries an actionable
	// classification, so only the remembered close keeps it out.
	late := one
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{
		Repo: "acme/widgets", Number: 1, State: "open", Title: one.Title, Labels: one.Labels,
		UpdatedAt: filed, FetchedAt: closedAt.Add(time.Second),
		Patch: github.WebhookItemPatch{Bucket: github.WebhookBucketActionable, Issue: &late},
	}})
	if it, ok := rwqQueueItem(s.contributeHub.ReadyQueue(readyQueueDefaultLimit), 1); ok {
		t.Fatalf("an older open read resurrected closed #1: %+v", it)
	}

	// The governor publishes the snapshot its cycle built before the close.
	stale := minimalPayload()
	stale.Repos = []FrontendRepo{{Name: "widgets", Full: "acme/widgets", ActionableIssues: []any{one, two}}}
	s.UpdateStatus(stale)
	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if it, ok := rwqQueueItem(q, 1); ok {
		t.Fatalf("a pre-close governor snapshot resurrected #1: %+v", it)
	}
	if got, ok := rwqQueueItem(q, 2); !ok || !reflect.DeepEqual(got.Labels, []string{"kind/cleanup", "approved-direction"}) {
		t.Fatalf("pre-label governor snapshot: queue row #2 = %+v (present=%v), want the re-read labels replayed", got, ok)
	}
}

// TestDispatchSkipsClosedActionableEntry: even an entry that says closed is never
// offered, because the one decoder selectTask and the queue share drops it.
func TestDispatchSkipsClosedActionableEntry(t *testing.T) {
	var seen []int
	forEachActionableIssue(nil, "", "acme/widgets", []any{
		github.Issue{Repo: "acme/widgets", Number: 1, State: "closed"},
		github.Issue{Repo: "acme/widgets", Number: 2, State: "open"},
	}, func(_ map[string]any, ref worksource.Ref) { seen = append(seen, ref.Number) })
	if !reflect.DeepEqual(seen, []int{2}) {
		t.Fatalf("decoded issues = %v, want only the open #2", seen)
	}
}

// TestReadyQueueKeepsPollingWhileStreamOpen is the page half. The onopen handler
// must not stop the queue poll, the poll must keep a slow cadence while the
// stream is open, and a shown tab must re-read at once. Before the fix onopen
// called ccStopFallback() and the poll only ever ran at the 6s fallback cadence.
func TestReadyQueueKeepsPollingWhileStreamOpen(t *testing.T) {
	body := opsPageBody(t)
	connect := jsFunc(t, body, "connect")
	open := connect[strings.Index(connect, "ccEs.onopen="):]
	open = open[:strings.Index(open, "};")]
	if strings.Contains(open, "ccStopFallback") {
		t.Errorf("onopen stops the queue poll, so an open stream freezes the queue at its hello frame: %s", open)
	}
	if !strings.Contains(open, "setTimeout(ccQueuePoll,CC_QUEUE_LIVE_POLL_MS)") {
		t.Errorf("onopen does not schedule the slow queue poll: %s", open)
	}
	poll := jsFunc(t, body, "ccQueuePoll")
	if !strings.Contains(poll, "setTimeout(ccQueuePoll,ccStreamOpen?CC_QUEUE_LIVE_POLL_MS:CC_QUEUE_POLL_MS)") {
		t.Errorf("ccQueuePoll does not pick its cadence from the stream state: %s", poll)
	}
	if !strings.Contains(body, "if(document.visibilityState==='visible')ccQueuePoll();") {
		t.Error("a tab shown again does not re-read the queue")
	}
}

// TestReadyQueueStreamRefreshBehaviour runs the page's own connect / ccQueuePoll /
// ccStopFallback under node with a fake clock, EventSource and fetch. Before the
// fix the open stream never re-read the queue, so the closed issue stayed listed.
func TestReadyQueueStreamRefreshBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the queue refresh was NOT executed by this run")
	}
	body := opsPageBody(t)
	script := `
var fails=[];function fail(m){fails.push(m);}
var realImmediate=setImmediate;function flush(){return new Promise(function(r){realImmediate(r);});}
var timers=[],now=0;
globalThis.setTimeout=function(f,d){var t={f:f,at:now+(d||0),live:true};timers.push(t);return t;};
globalThis.clearTimeout=function(t){if(t)t.live=false;};
function advance(ms){var end=now+ms;for(;;){var due=timers.filter(function(t){return t.live&&t.at<=end;}).sort(function(a,b){return a.at-b.at;})[0];if(!due)break;due.live=false;now=due.at;due.f();}now=end;}
var served=[],fetches=[];
globalThis.fetch=function(u){fetches.push(u);var b={queue:served.slice()};return Promise.resolve({json:function(){return Promise.resolve(b);}});};
function FakeES(u){this.url=u;this.readyState=0;FakeES.last=this;}
FakeES.prototype.close=function(){this.readyState=2;};
globalThis.EventSource=FakeES;
var ccQueue=[],ccQueuePollTimer=null,ccEs=null,ccStreamOpen=false,CC_QUEUE_POLL_MS=6000,CC_QUEUE_LIVE_POLL_MS=30000,live='';
function ccSetLive(s){live=s;}
function ccHydrate(p){if(p.queue)ccQueue=p.queue.slice();}
function ccRenderQueue(){}
function ccApplyInterestsFromResponse(){}
function ccOnActivity(){}function ccSetAnnouncement(){}function ccOnWallEvent(){}function ccSetHelpLinks(){}function ccOnGap(){}
` + jsFunc(t, body, "ccQueuePoll") + "\n" + jsFunc(t, body, "ccStopFallback") + "\n" + jsFunc(t, body, "connect") + `
(async function(){
  connect();
  var es=FakeES.last;es.readyState=1;es.onopen();
  es.onmessage({data:JSON.stringify({type:'hello',queue:[{key:'acme/widgets#1'},{key:'acme/widgets#2'}]})});
  if(ccQueue.length!==2)fail('hello did not hydrate the queue');
  served=[{key:'acme/widgets#2'}]; // #1 closed on GitHub
  advance(CC_QUEUE_LIVE_POLL_MS+1);await flush();
  if(fetches.indexOf('/api/contribute/queue')<0)fail('an open stream never re-read the queue');
  if(ccQueue.length!==1||ccQueue[0].key!=='acme/widgets#2')fail('queue still lists the closed issue: '+JSON.stringify(ccQueue));
  var n=fetches.length;served=[{key:'acme/widgets#3'}];
  es.readyState=0;es.onerror();await flush();
  if(fetches.length!==n+1)fail('a dropped stream did not re-read the queue at once');
  if(!ccQueue.length||ccQueue[0].key!=='acme/widgets#3')fail('drop re-read not applied: '+JSON.stringify(ccQueue));
  n=fetches.length;advance(CC_QUEUE_POLL_MS+1);await flush();
  if(fetches.length!==n+1)fail('the fast cadence did not resume after the drop');
  var pending=timers.filter(function(t){return t.live&&t.f===ccQueuePoll;}).length;
  if(pending!==1)fail('want one pending queue poll, got '+pending);
  if(fails.length){console.log('FAIL '+fails.join('; '));}else{console.log('ok');}
})();
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") || strings.Contains(string(out), "FAIL") {
		t.Fatalf("ready-work queue stream refresh regressed: %v\n%s", err, out)
	}
}
