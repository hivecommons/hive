package sessiontaint

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type auditSink struct {
	mu      sync.Mutex
	entries []string
}

func (a *auditSink) add(agent, action, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, agent+"|"+action+"|"+detail)
}

func (a *auditSink) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.entries)
}

func publicAudience(string) Audience  { return AudiencePublic }
func privateAudience(string) Audience { return AudiencePrivate }

func newTestObserver(t *testing.T, mutate func(*Options)) (*Observer, *auditSink, *bytes.Buffer) {
	t.Helper()
	au := &auditSink{}
	var logs bytes.Buffer
	opts := Options{
		Enabled: true,
		Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
		Audit:   au.add,
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	if mutate != nil {
		mutate(&opts)
	}
	o := New(opts)
	if o == nil {
		t.Fatal("New returned nil for an enabled observer")
	}
	return o, au, &logs
}

// Observe-only guarantee: the observer's entry points return nothing, so no
// caller can branch on a verdict, and no verdict type can express a deny.
func TestObserverEntryPointsReturnNothing(t *testing.T) {
	typ := reflect.TypeOf(&Observer{})
	for _, name := range []string{"RecordLabel", "ObserveSink"} {
		m, ok := typ.MethodByName(name)
		if !ok {
			t.Fatalf("missing method %s", name)
		}
		if m.Type.NumOut() != 0 {
			t.Errorf("%s returns %d values; an observe-only entry point must return none", name, m.Type.NumOut())
		}
	}
	if string(VerdictHold) != "hold" || string(VerdictAllow) != "allow" {
		t.Fatalf("unexpected verdict vocabulary")
	}
}

func TestNothingIsDeniedWhateverTheSessionHolds(t *testing.T) {
	o, _, _ := newTestObserver(t, nil)
	for _, l := range []Label{LabelSecret, LabelPrivateRepo, LabelCredentialedResponse, LabelUnknown} {
		o.RecordLabel("agent-a", l, "hive-canary", SourceCanaryHit)
	}
	for _, k := range []SinkKind{SinkRelayIssue, SinkRelayComment, SinkRelayPR, SinkRelayReview} {
		o.ObserveSink("agent-a", k, "o/pub", publicAudience)
		o.ObserveSink("agent-a", k, "o/priv", privateAudience)
	}
	st := o.Stats()
	if len(st.Recent) == 0 {
		t.Fatal("expected would-act decisions to be recorded")
	}
	for _, d := range st.Recent {
		if d.Verdict != VerdictHold {
			t.Errorf("decision %+v has verdict %q; only would-hold is possible", d, d.Verdict)
		}
	}
	if st.Mode != "observe" {
		t.Errorf("mode = %q, want observe", st.Mode)
	}
}

func TestDisabledRecordsNothing(t *testing.T) {
	au := &auditSink{}
	var logs bytes.Buffer
	o := New(Options{Enabled: false, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Audit: au.add})
	if o != nil {
		t.Fatal("a disabled observer must be nil")
	}
	// Every entry point is a safe no-op on nil.
	o.RecordLabel("agent-a", LabelSecret, "hive-canary", SourceCanaryHit)
	looked := false
	o.ObserveSink("agent-a", SinkRelayIssue, "o/a", func(string) Audience { looked = true; return AudiencePublic })
	if looked {
		t.Error("audience lookup ran with the feature off")
	}
	if got := o.Labels("agent-a"); got != nil {
		t.Errorf("labels = %v, want none", got)
	}
	if st := o.Stats(); !reflect.DeepEqual(st, Stats{}) {
		t.Errorf("stats = %+v, want zero", st)
	}
	if au.count() != 0 || logs.Len() != 0 {
		t.Errorf("a disabled observer wrote audit=%d log=%q", au.count(), logs.String())
	}
}

func TestCleanSessionCostsNoLookupAndRecordsNothing(t *testing.T) {
	o, au, logs := newTestObserver(t, nil)
	looked := false
	o.ObserveSink("agent-a", SinkRelayIssue, "o/a", func(string) Audience { looked = true; return AudiencePublic })
	if looked {
		t.Error("audience lookup ran for a session with no labels")
	}
	st := o.Stats()
	if st.Sessions != 0 || st.SinksClean != 1 || len(st.Recent) != 0 || len(st.WouldAct) != 0 {
		t.Errorf("clean sink observation changed state: %+v", st)
	}
	if au.count() != 0 || logs.Len() != 0 {
		t.Errorf("clean session wrote audit=%d log=%q", au.count(), logs.String())
	}
}

func TestLabelThenSinkRecordsWouldHold(t *testing.T) {
	o, au, logs := newTestObserver(t, nil)
	o.RecordLabel("agent-a", LabelSecret, "hive-canary", SourceCanaryHit)
	o.ObserveSink("agent-a", SinkRelayComment, "o/pub", publicAudience)
	// Another agent that never read anything is not held.
	o.ObserveSink("agent-b", SinkRelayComment, "o/pub", publicAudience)

	st := o.Stats()
	if st.WouldAct[RuleSecretPublicWrite] != 1 {
		t.Fatalf("would-act counters = %v", st.WouldAct)
	}
	if len(st.Recent) != 1 || st.Recent[0].Agent != "agent-a" || st.Recent[0].Rule != RuleSecretPublicWrite {
		t.Fatalf("recent = %+v", st.Recent)
	}
	if au.count() != 2 { // one label entry, one would-act entry
		t.Fatalf("audit entries = %v", au.entries)
	}
	if !strings.Contains(logs.String(), "observe only") {
		t.Errorf("log does not say observe only: %s", logs.String())
	}
}

func TestRepeatedDecisionsAreCountedButLoggedOnce(t *testing.T) {
	o, au, _ := newTestObserver(t, nil)
	o.RecordLabel("agent-a", LabelSecret, "hive-canary", SourceCanaryHit)
	const requests = 500
	for i := 0; i < requests; i++ {
		o.ObserveSink("agent-a", SinkRelayComment, "o/pub", publicAudience)
	}
	st := o.Stats()
	if st.WouldAct[RuleSecretPublicWrite] != requests {
		t.Errorf("counter = %d, want %d", st.WouldAct[RuleSecretPublicWrite], requests)
	}
	if len(st.Recent) != 1 || au.count() != 2 {
		t.Errorf("recent=%d audit=%d, want one decision and two audit entries total", len(st.Recent), au.count())
	}
}

func TestLabelsAreAUnionAndOnlyGrow(t *testing.T) {
	o, au, _ := newTestObserver(t, nil)
	o.RecordLabel("agent-a", LabelSecret, "jwt", SourceCanaryHit)
	o.RecordLabel("agent-a", LabelSecret, "jwt", SourceCanaryHit) // repeat
	o.RecordLabel("agent-a", LabelPrivateRepo, "o/priv", SourceCanaryHit)
	if got := len(o.Labels("agent-a")); got != 2 {
		t.Fatalf("labels = %d, want 2", got)
	}
	if st := o.Stats(); st.LabelsRecorded != 2 || au.count() != 2 {
		t.Errorf("recorded=%d audit=%d, want 2 and 2", st.LabelsRecorded, au.count())
	}
}

func TestStoreIsBounded(t *testing.T) {
	const maxSessions, maxLabels, maxDecisions = 3, 2, 4
	o, au, _ := newTestObserver(t, func(op *Options) {
		op.MaxSessions, op.MaxLabelsPerSession, op.MaxDecisions = maxSessions, maxLabels, maxDecisions
	})
	const agents = 20
	for i := 0; i < agents; i++ {
		o.RecordLabel(fmt.Sprintf("agent-%d", i), LabelSecret, "jwt", SourceCanaryHit)
	}
	st := o.Stats()
	if st.Sessions != maxSessions || st.SessionsEvicted != agents-maxSessions {
		t.Errorf("sessions=%d evicted=%d, want %d and %d", st.Sessions, st.SessionsEvicted, maxSessions, agents-maxSessions)
	}
	// The most recent sessions survive, the oldest are gone.
	if len(o.Labels("agent-0")) != 0 || len(o.Labels(fmt.Sprintf("agent-%d", agents-1))) != 1 {
		t.Errorf("eviction did not keep the most recent sessions")
	}

	// Per-session label bound: distinct origins past the bound are dropped and counted.
	for i := 0; i < 10; i++ {
		o.RecordLabel("agent-x", LabelPrivateRepo, fmt.Sprintf("o/r%d", i), SourceCanaryHit)
	}
	if got := len(o.Labels("agent-x")); got != maxLabels {
		t.Errorf("labels = %d, want bound %d", got, maxLabels)
	}
	if o.Stats().LabelsDropped == 0 {
		t.Errorf("dropped labels were not counted")
	}

	// Decision ring bound: many distinct would-act decisions keep only the latest.
	o.RecordLabel("agent-y", LabelSecret, "jwt", SourceCanaryHit)
	before := au.count()
	for i := 0; i < 50; i++ {
		o.ObserveSink("agent-y", SinkRelayComment, fmt.Sprintf("o/d%d", i), publicAudience)
	}
	st = o.Stats()
	if len(st.Recent) != maxDecisions {
		t.Errorf("recent = %d, want ring bound %d", len(st.Recent), maxDecisions)
	}
	if st.Recent[len(st.Recent)-1].DestRepo != "o/d49" {
		t.Errorf("ring is not oldest-first: last = %s", st.Recent[len(st.Recent)-1].DestRepo)
	}
	if au.count() <= before {
		t.Errorf("expected audit entries for distinct decisions")
	}
	// The audit trail for one session is bounded even with unbounded distinct decisions.
	for i := 0; i < 10*maxDecisionKeysPerSession; i++ {
		o.ObserveSink("agent-y", SinkRelayIssue, fmt.Sprintf("o/e%d", i), publicAudience)
	}
	if grown := au.count() - before; grown > maxDecisionKeysPerSession+1 {
		t.Errorf("audit grew by %d, want at most %d per session", grown, maxDecisionKeysPerSession+1)
	}
}

// Restart behaviour in this phase: the store is memory only, so a restart is a
// fresh epoch. The new observer holds nothing, records nothing for a sink, and
// does not invent an unknown label (that is an enforcement-phase failure mode).
func TestRestartStartsCleanAndInventsNothing(t *testing.T) {
	first, _, _ := newTestObserver(t, nil)
	first.RecordLabel("agent-a", LabelSecret, "hive-canary", SourceCanaryHit)
	if len(first.Labels("agent-a")) != 1 {
		t.Fatal("setup: label not recorded")
	}

	second, au, logs := newTestObserver(t, nil) // the "restarted" process
	if got := second.Labels("agent-a"); len(got) != 0 {
		t.Fatalf("labels survived a restart: %v", got)
	}
	second.ObserveSink("agent-a", SinkRelayIssue, "o/pub", publicAudience)
	st := second.Stats()
	if st.Sessions != 0 || len(st.Recent) != 0 || len(st.WouldAct) != 0 || st.SinksClean != 1 {
		t.Errorf("restarted observer state = %+v", st)
	}
	if au.count() != 0 || logs.Len() != 0 {
		t.Errorf("restarted observer wrote audit=%d log=%q", au.count(), logs.String())
	}
}

// No content is stored: free-form or credential-shaped input is replaced by a
// fixed placeholder, never kept, logged or audited.
func TestNoContentOrCredentialValuesAreStored(t *testing.T) {
	o, au, logs := newTestObserver(t, nil)
	// Synthetic credential-shaped and free-form strings.
	synthetic := []string{
		"ghp_" + strings.Repeat("A1", 18),
		"line one\nline two with spaces",
		strings.Repeat("x", 500),
		"eyJhbGciOi.synthetic.payload",
	}
	for _, s := range synthetic {
		o.RecordLabel("agent-a", LabelSecret, s, Source(s))
	}
	o.RecordLabel("agent-a", LabelPrivateRepo, "contains spaces and\nnewline", SourceCanaryHit)
	for _, rec := range o.Labels("agent-a") {
		for _, field := range []string{rec.Origin, string(rec.Source)} {
			for _, s := range synthetic {
				if strings.Contains(field, s) || strings.Contains(field, "ghp_") {
					t.Errorf("stored field %q echoes input", field)
				}
			}
			if strings.ContainsAny(field, " \n") {
				t.Errorf("stored field %q contains whitespace", field)
			}
		}
	}
	// An unattributed or malformed agent is not recorded at all.
	o.RecordLabel("", LabelSecret, "jwt", SourceCanaryHit)
	o.RecordLabel("bad agent\nname", LabelSecret, "jwt", SourceCanaryHit)
	if st := o.Stats(); st.Sessions != 1 {
		t.Errorf("sessions = %d, want only the attributed agent", st.Sessions)
	}
	// An unknown label kind is ignored.
	o.RecordLabel("agent-a", Label("made-up"), "jwt", SourceCanaryHit)
	for _, rec := range o.Labels("agent-a") {
		if !knownLabel(rec.Label) {
			t.Errorf("unknown label stored: %s", rec.Label)
		}
	}
	// Nothing in the log or audit echoes the synthetic strings.
	all := logs.String() + strings.Join(au.entries, "\n")
	for _, s := range synthetic {
		if strings.Contains(all, s) {
			t.Errorf("log or audit contains synthetic input %q", s)
		}
	}
}

func TestLookupPanicIsAbsorbedAndTreatedAsPublic(t *testing.T) {
	o, _, _ := newTestObserver(t, nil)
	o.RecordLabel("agent-a", LabelSecret, "hive-canary", SourceCanaryHit)
	o.ObserveSink("agent-a", SinkRelayIssue, "o/a", func(string) Audience { panic("lookup failed") })
	st := o.Stats()
	if st.Panics != 1 {
		t.Errorf("panics = %d, want 1", st.Panics)
	}
	if st.WouldAct[RuleSecretPublicWrite] != 1 {
		t.Errorf("a failed destination lookup must be treated as public: %v", st.WouldAct)
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	o, _, _ := newTestObserver(t, func(op *Options) { op.MaxSessions = 4 })
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			agent := fmt.Sprintf("agent-%d", g)
			for i := 0; i < 200; i++ {
				o.RecordLabel(agent, LabelSecret, "jwt", SourceCanaryHit)
				o.ObserveSink(agent, SinkRelayComment, "o/pub", publicAudience)
				_ = o.Stats()
			}
		}(g)
	}
	wg.Wait()
}

func TestNewAppliesDefaultsForNonPositiveBounds(t *testing.T) {
	o := New(Options{Enabled: true, MaxSessions: -1, MaxLabelsPerSession: 0, MaxDecisions: -5})
	if o.store.maxSessions != DefaultMaxSessions || o.store.maxLabels != DefaultMaxLabelsPerSession || o.opts.MaxDecisions != DefaultMaxDecisions {
		t.Errorf("bounds = %d %d %d, want the defaults", o.store.maxSessions, o.store.maxLabels, o.opts.MaxDecisions)
	}
	if o.opts.Now == nil {
		t.Error("a nil clock must default")
	}
	one := New(Options{Enabled: true, MaxSessions: 1, MaxLabelsPerSession: 1, MaxDecisions: 1})
	if one.store.maxSessions != 1 || one.store.maxLabels != 1 || one.opts.MaxDecisions != 1 {
		t.Errorf("a bound of 1 must be kept")
	}
}

func TestFieldShapeBoundaries(t *testing.T) {
	long := strings.Repeat("a", maxFieldLen)
	if cleanField(long) != long {
		t.Errorf("a field of exactly %d bytes must be kept", maxFieldLen)
	}
	if cleanField(long+"a") != invalidField {
		t.Errorf("a field over %d bytes must be refused", maxFieldLen)
	}
	if cleanField("  ") != "" || cleanField(" ok ") != "ok" {
		t.Errorf("trim behaviour changed")
	}
	slug := strings.Repeat("a", maxCategoryLen)
	if !isCategorySlug(slug) || isCategorySlug(slug+"a") || isCategorySlug("") {
		t.Errorf("category slug length boundary wrong")
	}
	if !isCategorySlug("a-z") || isCategorySlug("A") || isCategorySlug("a1") || isCategorySlug("a_b") || !isCategorySlug("-") {
		t.Errorf("category slug alphabet wrong")
	}
	for repo, want := range map[string]bool{
		"o/r": true, "My-Org/my.repo_2": true, "o/": false, "/r": false, "o": false,
		"o/r/x": false, "o r/x": false, "o/r x": false, "o/r@x": false, "": false,
	} {
		if got := isRepoID(repo); got != want {
			t.Errorf("isRepoID(%q) = %v, want %v", repo, got, want)
		}
	}
	for host, want := range map[string]bool{
		"api.example.com": true, "host:8080": true, "": false, "a b": false, "a/b": false, "a_b": false,
	} {
		if got := isHost(host); got != want {
			t.Errorf("isHost(%q) = %v, want %v", host, got, want)
		}
	}
	if !validOrigin(LabelSecret, invalidField) || !validOrigin(LabelPrivateRepo, unspecifiedOrigin) {
		t.Errorf("the placeholders must always be allowed")
	}
	if validOrigin(Label("made-up"), "x") {
		t.Errorf("an unknown label has no valid origin")
	}
}
