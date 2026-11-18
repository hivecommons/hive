package authz

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

var (
	legacyAllow       = Verdict{Effect: EffectAllow, ReasonCode: ReasonOK}
	legacyProtected   = Verdict{Effect: EffectDeny, ReasonCode: ReasonProtectedPath}
	fixedShadowTime   = time.Date(2026, 11, 18, 14, 31, 2, 0, time.UTC)
	fixedShadowClock  = func() time.Time { return fixedShadowTime }
	secretShapedToken = "ghp_" + strings.Repeat("A1b2", 9)
)

func newTestShadow(capacity int) (*Shadow, *bytes.Buffer) {
	var logs bytes.Buffer
	s := NewShadow(capacity)
	s.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	s.Now = fixedShadowClock
	return s, &logs
}

func TestShadowOffRecordsNothing(t *testing.T) {
	var off *Shadow
	off.Observe(brokerRequest([]string{"OWNERS"}, testGuards), legacyProtected)
	if got := off.Events(); got != nil {
		t.Fatalf("a nil Shadow recorded %d events", len(got))
	}
	if got := off.Stats(); got != (Stats{}) {
		t.Fatalf("a nil Shadow has counters: %+v", got)
	}
}

func TestShadowFromEnvIsOffByDefault(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unset":        {},
		"empty":        {EnvShadow: ""},
		"false":        {EnvShadow: "false"},
		"garbage":      {EnvShadow: "yes please"},
		"buffer alone": {EnvShadowBuffer: "50"},
	} {
		t.Run(name, func(t *testing.T) {
			if s := ShadowFromEnv(func(k string) string { return env[k] }); s != nil {
				t.Fatalf("ShadowFromEnv = %+v, want nil (off)", s)
			}
		})
	}
	if s := ShadowFromEnv(nil); s != nil {
		t.Fatal("a nil getenv must be off")
	}
}

func TestShadowFromEnvOnAndBufferBounds(t *testing.T) {
	cases := []struct {
		buffer string
		want   int
	}{
		{"", DefaultBufferEvents},
		{"25", 25},
		{"0", DefaultBufferEvents},
		{"-4", DefaultBufferEvents},
		{"lots", DefaultBufferEvents},
		{fmt.Sprint(MaxBufferEvents * 10), MaxBufferEvents},
	}
	for _, tc := range cases {
		s := ShadowFromEnv(func(k string) string {
			switch k {
			case EnvShadow:
				return "true"
			case EnvShadowBuffer:
				return tc.buffer
			}
			return ""
		})
		if s == nil {
			t.Fatalf("buffer %q: shadow is off with %s=true", tc.buffer, EnvShadow)
		}
		if got := s.Stats().Capacity; got != tc.want {
			t.Fatalf("buffer %q: capacity = %d, want %d", tc.buffer, got, tc.want)
		}
	}
}

func TestShadowRecordsOneVersionedEventPerCall(t *testing.T) {
	s, logs := newTestShadow(10)
	req := brokerRequest([]string{"pkg/a.go", "OWNERS"}, testGuards)
	req.Context.RequestID = "req-1"
	s.Observe(req, legacyProtected)

	events := s.Events()
	if len(events) != 1 {
		t.Fatalf("events = %d, want exactly one per call", len(events))
	}
	ev := events[0]
	if ev.V != SchemaVersion || !ev.TS.Equal(fixedShadowTime) {
		t.Fatalf("version/ts = %d/%v", ev.V, ev.TS)
	}
	if !strings.HasPrefix(ev.DecisionID, decisionIDPrefix) {
		t.Fatalf("decision id = %q", ev.DecisionID)
	}
	if ev.Surface != string(SurfaceBroker) || ev.Effect != string(EffectDeny) || !ev.Shadow ||
		ev.RuleID != RuleBrokerProtectedPath || ev.ReasonCode != ReasonProtectedPath {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Resource.PathsCount != 2 || ev.Resource.MatchedCount != 1 || ev.Resource.Repo != "org/name" || ev.Resource.Branch != "work" {
		t.Fatalf("resource = %+v", ev.Resource)
	}
	if ev.Actor.Agent != "scanner" || !ev.Actor.Identified || ev.Action.Kind != KindBranchPush || ev.RequestID != "req-1" {
		t.Fatalf("actor/action = %+v %+v", ev.Actor, ev.Action)
	}
	if ev.Parity != ParityMatch || ev.Legacy.Effect != string(EffectDeny) || ev.Legacy.ReasonCode != ReasonProtectedPath {
		t.Fatalf("parity/legacy = %q %+v", ev.Parity, ev.Legacy)
	}
	if ev.LatencyUS < 0 {
		t.Fatalf("latency = %d", ev.LatencyUS)
	}
	if st := s.Stats(); st.Observed != 1 || st.Mismatches != 0 || st.Dropped != 0 || st.Errors != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if logs.Len() != 0 {
		t.Fatalf("a matching decision must not log a divergence: %s", logs.String())
	}
}

func TestShadowEventJSONKeysAreStable(t *testing.T) {
	s, _ := newTestShadow(2)
	s.Observe(brokerRequest([]string{"pkg/a.go"}, testGuards), legacyAllow)
	raw, err := json.Marshal(s.Events()[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"v", "ts", "decision_id", "surface", "effect", "shadow", "rule_id", "reason_code", "policy_version", "actor", "action", "resource", "latency_us", "legacy", "parity"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("event JSON is missing %q: %s", key, raw)
		}
	}
	if v, ok := got["v"].(float64); !ok || int(v) != SchemaVersion {
		t.Fatalf("v = %v", got["v"])
	}
}

func TestShadowEventCarriesAttributesOnly(t *testing.T) {
	s, _ := newTestShadow(2)
	req := brokerRequest([]string{"secret-looking-dir/very-private-file-name.txt", "OWNERS"}, testGuards)
	req.Actor.Agent = "agent-" + secretShapedToken
	req.Resource.Branch = "topic/" + secretShapedToken + "\nforged log line"
	req.Resource.Repo = "org/name"
	req.Context.RequestID = "id-" + secretShapedToken
	s.Observe(req, legacyProtected)

	raw, err := json.Marshal(s.Events()[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, secretShapedToken) {
		t.Fatalf("a credential-shaped value reached the event: %s", text)
	}
	if strings.Contains(text, "very-private-file-name") || strings.Contains(text, "secret-looking-dir") {
		t.Fatalf("a changed path reached the event, only a count may: %s", text)
	}
	if ev := s.Events()[0]; strings.ContainsAny(ev.Resource.Branch, "\n\r") || strings.ContainsAny(ev.Actor.Agent+ev.RequestID, "\n\r") {
		t.Fatalf("a control character survived into the event: %q", ev.Resource.Branch)
	}
}

func TestShadowAttributesAreBounded(t *testing.T) {
	s, _ := newTestShadow(2)
	req := brokerRequest([]string{"a.go"}, testGuards)
	req.Resource.Branch = strings.Repeat("b", maxAttrBytes*8)
	// Three-byte runes, so the byte limit falls inside a rune and truncation
	// has to step back to a boundary.
	req.Actor.Agent = strings.Repeat("\u20ac", maxAttrBytes)
	s.Observe(req, legacyAllow)
	ev := s.Events()[0]
	if len(ev.Resource.Branch) > maxAttrBytes || len(ev.Actor.Agent) > maxAttrBytes {
		t.Fatalf("attributes not bounded: branch=%d agent=%d", len(ev.Resource.Branch), len(ev.Actor.Agent))
	}
	if !utf8.ValidString(ev.Actor.Agent) || !strings.HasPrefix(ev.Actor.Agent, "\u20ac") {
		t.Fatalf("a rune was split by truncation: %q", ev.Actor.Agent)
	}
}

func TestShadowLogsToTheDefaultLoggerWhenNoneIsSet(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	s := NewShadow(2)
	s.Observe(brokerRequest([]string{"OWNERS"}, testGuards), legacyAllow)
	if !strings.Contains(logs.String(), "authz shadow") {
		t.Fatalf("divergence not logged to the default logger: %q", logs.String())
	}
}

func TestShadowStreamIsBounded(t *testing.T) {
	const capacity, total = 3, 10
	s, _ := newTestShadow(capacity)
	for i := range total {
		req := brokerRequest([]string{"a.go"}, testGuards)
		req.Resource.Branch = fmt.Sprintf("b%d", i)
		s.Observe(req, legacyAllow)
	}
	events := s.Events()
	if len(events) != capacity {
		t.Fatalf("stream holds %d events, want the bound %d", len(events), capacity)
	}
	for i, ev := range events {
		if want := fmt.Sprintf("b%d", total-capacity+i); ev.Resource.Branch != want {
			t.Fatalf("event %d branch = %q, want %q (oldest first, newest kept)", i, ev.Resource.Branch, want)
		}
	}
	st := s.Stats()
	if st.Observed != total || st.Dropped != total-capacity || st.Buffered != capacity || st.Capacity != capacity {
		t.Fatalf("stats = %+v", st)
	}
}

func TestShadowLogsDivergenceAndEnforcesNothing(t *testing.T) {
	s, logs := newTestShadow(10)
	// The evaluator denies a protected path; pretend the existing gate allowed.
	req := brokerRequest([]string{"OWNERS"}, testGuards)
	s.Observe(req, legacyAllow)

	ev := s.Events()[0]
	if ev.Parity != ParityMismatch || ev.Effect != string(EffectDeny) || ev.Legacy.Effect != string(EffectAllow) {
		t.Fatalf("event = %+v", ev)
	}
	if st := s.Stats(); st.Mismatches != 1 {
		t.Fatalf("mismatches = %d, want 1", st.Mismatches)
	}
	out := logs.String()
	for _, want := range []string{"authz shadow", "existing gate stays in force", "shadow_effect=deny", "legacy_effect=allow", "rule_id=" + RuleBrokerProtectedPath} {
		if !strings.Contains(out, want) {
			t.Fatalf("divergence log missing %q: %s", want, out)
		}
	}
}

func TestShadowDivergenceLogIsDeduplicatedButStillCounted(t *testing.T) {
	s, logs := newTestShadow(10)
	for range 5 {
		s.Observe(brokerRequest([]string{"OWNERS"}, testGuards), legacyAllow)
	}
	if got := strings.Count(logs.String(), "authz shadow"); got != 1 {
		t.Fatalf("logged %d times, want once per divergence shape", got)
	}
	if st := s.Stats(); st.Mismatches != 5 || st.Observed != 5 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestShadowDivergenceLogKindsAreBounded(t *testing.T) {
	s, logs := newTestShadow(10)
	for i := range maxLoggedDivergenceKinds + 20 {
		s.Observe(brokerRequest([]string{"OWNERS"}, testGuards), Verdict{Effect: EffectAllow, ReasonCode: fmt.Sprintf("r%d", i)})
	}
	if got := strings.Count(logs.String(), "authz shadow"); got != maxLoggedDivergenceKinds {
		t.Fatalf("logged %d lines, want the bound %d", got, maxLoggedDivergenceKinds)
	}
	if st := s.Stats(); st.Mismatches != maxLoggedDivergenceKinds+20 {
		t.Fatalf("every divergence must still be counted: %+v", st)
	}
}

type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordingSink) Record(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

type panickingSink struct{}

func (panickingSink) Record(Event) { panic("sink bug") }

func TestShadowSinkReceivesEventsAndItsPanicIsContained(t *testing.T) {
	s, _ := newTestShadow(4)
	sink := &recordingSink{}
	s.Sink = sink
	s.Observe(brokerRequest([]string{"a.go"}, testGuards), legacyAllow)
	if len(sink.events) != 1 || sink.events[0].DecisionID != s.Events()[0].DecisionID {
		t.Fatalf("sink events = %+v", sink.events)
	}

	s.Sink = panickingSink{}
	s.Observe(brokerRequest([]string{"a.go"}, testGuards), legacyAllow)
	if st := s.Stats(); st.Errors != 1 || st.Observed != 2 || len(s.Events()) != 2 {
		t.Fatalf("a sink panic must be counted and contained: %+v", st)
	}
}

func TestShadowZeroValueDoesNotPanic(t *testing.T) {
	s := &Shadow{}
	s.Observe(brokerRequest([]string{"a.go"}, testGuards), legacyAllow)
	if got := s.Events(); got != nil {
		t.Fatalf("a Shadow without a stream recorded %d events", len(got))
	}
}

func TestShadowConcurrentObserve(t *testing.T) {
	const workers, perWorker, capacity = 8, 50, 16
	s, _ := newTestShadow(capacity)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				s.Observe(brokerRequest([]string{"a.go"}, testGuards), legacyAllow)
				_ = s.Events()
				_ = s.Stats()
			}
		}()
	}
	wg.Wait()
	st := s.Stats()
	if st.Observed != workers*perWorker || st.Buffered != capacity || st.Dropped != workers*perWorker-capacity {
		t.Fatalf("stats = %+v", st)
	}
}

func TestDefaultShadowIsStable(t *testing.T) {
	if DefaultShadow() != DefaultShadow() {
		t.Fatal("DefaultShadow must return the same instance for the life of the process")
	}
}

func TestDecisionIDsAreUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for range 100 {
		id := newDecisionID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate decision id %q", id)
		}
		seen[id] = struct{}{}
	}
}
