package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/askai"
	"github.com/hivecommons/hive/pkg/config"
)

// relayFakeUpstream records the vendor thread each relayed call used.
type relayFakeUpstream struct {
	mu      sync.Mutex
	queries []string
	threads []string
}

func (f *relayFakeUpstream) Chat(_ context.Context, thread, query string) (askai.UpstreamAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	f.threads = append(f.threads, thread)
	return askai.UpstreamAnswer{
		Answer:   "Settings, Features.",
		ThreadID: "vendor-thread-" + string(rune('a'+len(f.threads)-1)),
		Sources:  []askai.UpstreamSource{{URL: "https://docs.hivecommons.dev/settings/", Title: "Settings"}},
	}, nil
}

func (f *relayFakeUpstream) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...), append([]string(nil), f.threads...)
}

func relayDo(s *HubServer, bearerHive, headerHive, body string) (*httptest.ResponseRecorder, askai.Result) {
	r := httptest.NewRequest(http.MethodPost, askai.RelayPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if bearerHive != "" {
		r.Header.Set("Authorization", heartbeatBearer(testHubSecret, bearerHive))
	}
	r.Header.Set(askai.RelayHiveIDHeader, headerHive)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	var res askai.Result
	if w.Code == http.StatusOK {
		_ = json.Unmarshal(w.Body.Bytes(), &res)
	}
	return w, res
}

func mkHostedHive(t *testing.T, id string) {
	t.Helper()
	if err := saveSaaSHive(&SaaSHive{ID: id, Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
}

func TestHubAskRelayIsAbsentWhenOff(t *testing.T) {
	s, _ := newAskTestHub(t, false, nil)
	mkHostedHive(t, "hosted-a")
	if w, _ := relayDo(s, "hosted-a", "hosted-a", `{"question":"q?"}`); w.Code != http.StatusNotFound {
		t.Fatalf("relay with Ask off answered %d", w.Code)
	}
}

func TestHubAskRelayAuthenticatesTheHiveAndServesHostedOnly(t *testing.T) {
	t.Setenv(config.EnvAskRelayDisabledHives, "hosted-off")
	t.Setenv(config.EnvAskLimitHivePerHour, "2")
	up := &relayFakeUpstream{}
	s, logs := newAskTestHub(t, true, up)
	for _, id := range []string{"hosted-a", "hosted-b", "hosted-off"} {
		mkHostedHive(t, id)
	}
	const q = `{"question":"Where is pause?","section":"agents-section"}`

	if w, _ := relayDo(s, "", "hosted-a", q); w.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", w.Code)
	}
	if w, _ := relayDo(s, "hosted-b", "hosted-a", q); w.Code != http.StatusUnauthorized {
		t.Fatalf("hive B's bearer for hive A: %d", w.Code)
	}
	if w, _ := relayDo(s, "hosted-a", "hosted-a", `{"question":"q?","person":"alice"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a person field must be rejected: %d", w.Code)
	}
	for _, id := range []string{"hosted-off", "not-provisioned"} {
		if w, r := relayDo(s, id, id, q); w.Code != http.StatusOK || r.Outcome != askai.OutcomeRelayDisabled {
			t.Fatalf("%s: %d %+v", id, w.Code, r)
		}
	}
	// A self-service (byo-) hive never authenticates with hub-derived
	// material, and is never served by the relay even if it did.
	if w, r := relayDo(s, "byo-alice--home", "byo-alice--home", q); w.Code != http.StatusUnauthorized && r.Outcome != askai.OutcomeRelayDisabled {
		t.Fatalf("byo hive: %d %+v", w.Code, r)
	}
	if askHiveIsHosted("byo-alice--home") {
		t.Fatal("a byo- hive is never hosted")
	}
	if q, _ := up.snapshot(); len(q) != 0 {
		t.Fatalf("refused hives reached the vendor: %q", q)
	}

	_, a1 := relayDo(s, "hosted-a", "hosted-a", q)
	if a1.Outcome != askai.OutcomeOK || a1.Conversation == "" || len(a1.Sources) != 1 {
		t.Fatalf("hosted hive: %+v", a1)
	}
	// Hive B presenting hive A's handle starts a fresh vendor thread.
	_, b1 := relayDo(s, "hosted-b", "hosted-b", `{"question":"and?","conversation":"`+a1.Conversation+`"}`)
	_, a2 := relayDo(s, "hosted-a", "hosted-a", `{"question":"and?","conversation":"`+a1.Conversation+`"}`)
	if b1.Outcome != askai.OutcomeOK || a2.Outcome != askai.OutcomeOK {
		t.Fatalf("follow-ups: %+v %+v", b1, a2)
	}
	queries, threads := up.snapshot()
	if want := []string{"", "", "vendor-thread-a"}; strings.Join(threads, ",") != strings.Join(want, ",") {
		t.Fatalf("threads %q, want %q", threads, want)
	}
	if queries[0] != "Dashboard section: Agents.\n\nWhere is pause?" {
		t.Fatalf("vendor query %q", queries[0])
	}
	// Hive A has spent its hourly quota of 2; hive B has not.
	if _, r := relayDo(s, "hosted-a", "hosted-a", q); r.Outcome != askai.OutcomeRateLimited {
		t.Fatalf("hive A quota: %+v", r)
	}
	if _, r := relayDo(s, "hosted-b", "hosted-b", q); r.Outcome != askai.OutcomeOK {
		t.Fatalf("hive B charged for hive A: %+v", r)
	}
	if !strings.Contains(logs.String(), "hive=hosted-a") || strings.Contains(logs.String(), "Where is pause") {
		t.Fatalf("relay audit lines:\n%s", logs)
	}
}

func TestHubAskRelayNoneTurnsTheRelayOff(t *testing.T) {
	t.Setenv(config.EnvAskRelayHives, config.AskRelayNone)
	up := &relayFakeUpstream{}
	s, _ := newAskTestHub(t, true, up)
	mkHostedHive(t, "hosted-a")
	if _, r := relayDo(s, "hosted-a", "hosted-a", `{"question":"q?"}`); r.Outcome != askai.OutcomeRelayDisabled {
		t.Fatalf("relay none: %+v", r)
	}
	t.Setenv(config.EnvAskRelayHives, config.AskRelayRegistered)
	s2, logs := newAskTestHub(t, true, up)
	mkHostedHive(t, "hosted-a")
	if _, r := relayDo(s2, "hosted-a", "hosted-a", `{"question":"q?"}`); r.Outcome != askai.OutcomeOK {
		t.Fatalf("registered behaves as hosted: %+v", r)
	}
	if w, r := relayDo(s2, "byo-bob--x", "byo-bob--x", `{"question":"q?"}`); w.Code != http.StatusUnauthorized && r.Outcome != askai.OutcomeRelayDisabled {
		t.Fatalf("self-hosted on registered: %d %+v", w.Code, r)
	}
	if !strings.Contains(logs.String(), "relay for registered self-hosted hives is not served yet") && !registeredWarned() {
		t.Fatalf("no registered-mode warning:\n%s", logs)
	}
}

// registeredWarned reports whether the once-only warning already fired in an
// earlier test of this process.
func registeredWarned() bool {
	fired := true
	registeredRelayWarnOnce.Do(func() { fired = false })
	return fired
}
