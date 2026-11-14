package spoke

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

const (
	mirrorTestPrimaryKey = "primary-key-must-stay-with-primary"
	mirrorTestMirrorKey  = "mirror-key-never-logged-0123456789"
	mirrorTestWait       = 5 * time.Second
)

// TestHeartbeatMirrorFieldPolicyInventory fails when a HeartbeatPayload field
// has no mirror classification, or when the policy names a field that no
// longer exists. A new field must be classified before it can ship (#18148).
func TestHeartbeatMirrorFieldPolicyInventory(t *testing.T) {
	typ := reflect.TypeOf(HeartbeatPayload{})
	seen := map[string]bool{}
	var missing []string
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		seen[name] = true
		switch heartbeatMirrorFieldPolicy[name] {
		case mirrorFieldMirrored, mirrorFieldDropped:
		default:
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("HeartbeatPayload fields with no mirror classification: %v\n"+
			"add each to heartbeatMirrorFieldPolicy in heartbeat_mirror.go as "+
			"mirrorFieldMirrored or mirrorFieldDropped (credential-adjacent identifier, "+
			"authentication error text, signed-in users, activity digest, primary-only consent)", missing)
	}
	var stale []string
	for name := range heartbeatMirrorFieldPolicy {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("heartbeatMirrorFieldPolicy names fields HeartbeatPayload no longer has: %v", stale)
	}
}

// The fields the RFC names explicitly must stay dropped.
func TestHeartbeatMirrorFieldPolicyDropsRFCFields(t *testing.T) {
	for _, name := range []string{
		"DashboardTokenHash", "GitHubAppKeyFingerprint", "WebhookRelayKeyFingerprint", "GitHubAppKeysHeld",
		"InferenceAuthError", "GitHubAppTokenError",
		"ActiveSessionUsers", "EngagedSessionUsers", "UserLastActions",
		"Activity",
	} {
		if heartbeatMirrorFieldPolicy[name] != mirrorFieldDropped {
			t.Errorf("%s must be mirrorFieldDropped", name)
		}
	}
}

// Every name in the harmless table must exist on HeartbeatResponse, and the
// authority fields the hub fences for self-service hives must never be listed
// as harmless here.
func TestHeartbeatMirrorResponseHarmlessInventory(t *testing.T) {
	typ := reflect.TypeOf(HeartbeatResponse{})
	exists := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		exists[typ.Field(i).Name] = true
	}
	for name := range heartbeatMirrorResponseHarmless {
		if !exists[name] {
			t.Errorf("heartbeatMirrorResponseHarmless names %s, which HeartbeatResponse no longer has", name)
		}
	}
	for _, name := range []string{
		"GitHubAppConfig", "PendingGateway", "ProjectConfig", "ClusterID",
		"UpgradeTo", "UpgradeInterrupt", "SwitchToTag", "UpgradeAnnounce", "UpgradePolicy",
		"RestartSpoke", "ResetAgentRestarts", "Breaker", "SelfExperimentFlag",
		"IsPublic", "AuthorizedUsers", "AuthorizedUserNames", "HubBanner",
	} {
		if heartbeatMirrorResponseHarmless[name] {
			t.Errorf("%s carries authority and must not be harmless for a mirror", name)
		}
	}
}

// filledHeartbeatPayload sets every settable field of a HeartbeatPayload to a
// non-zero value, so the reducer test covers fields added after it was written.
func filledHeartbeatPayload(t *testing.T) *HeartbeatPayload {
	t.Helper()
	p := &HeartbeatPayload{}
	v := reflect.ValueOf(p).Elem()
	for i := 0; i < v.NumField(); i++ {
		fv := v.Field(i)
		if !fv.CanSet() {
			continue
		}
		mirrorFillNonZero(t, v.Type().Field(i).Name, fv)
		if fv.IsZero() {
			t.Fatalf("could not give HeartbeatPayload.%s a non-zero value", v.Type().Field(i).Name)
		}
	}
	return p
}

func mirrorFillNonZero(t *testing.T, name string, fv reflect.Value) {
	t.Helper()
	switch fv.Kind() {
	case reflect.Bool:
		fv.SetBool(true)
	case reflect.String:
		fv.SetString("x")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fv.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		fv.SetUint(1)
	case reflect.Float32, reflect.Float64:
		fv.SetFloat(1)
	case reflect.Slice:
		fv.Set(reflect.MakeSlice(fv.Type(), 1, 1))
	case reflect.Map:
		fv.Set(reflect.MakeMap(fv.Type()))
		key := reflect.New(fv.Type().Key()).Elem()
		if key.Kind() == reflect.String {
			key.SetString("k")
		}
		fv.SetMapIndex(key, reflect.Zero(fv.Type().Elem()))
	case reflect.Pointer:
		fv.Set(reflect.New(fv.Type().Elem()))
	case reflect.Struct:
		for i := 0; i < fv.NumField(); i++ {
			if fv.Field(i).CanSet() {
				mirrorFillNonZero(t, name, fv.Field(i))
				return
			}
		}
	default:
		t.Fatalf("mirrorFillNonZero: unhandled kind %s for HeartbeatPayload.%s", fv.Kind(), name)
	}
}

func TestReduceHeartbeatForMirrorZeroesDroppedFieldsOnly(t *testing.T) {
	src := filledHeartbeatPayload(t)
	before := clonePayload(src)

	reduced := reduceHeartbeatForMirror(src, "")
	if reduced == nil {
		t.Fatal("reduced payload is nil")
	}
	v := reflect.ValueOf(reduced).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		switch heartbeatMirrorFieldPolicy[name] {
		case mirrorFieldMirrored:
			if v.Field(i).IsZero() {
				t.Errorf("mirrored field %s was cleared", name)
			}
		default:
			if !v.Field(i).IsZero() {
				t.Errorf("dropped field %s still set in the mirror copy", name)
			}
		}
	}
	if !reflect.DeepEqual(clonePayload(src), before) {
		t.Error("the caller's payload was modified by the reducer")
	}

	if got := reduceHeartbeatForMirror(src, "byo-alice-lab"); got.HiveID != "byo-alice-lab" {
		t.Errorf("hive id override = %q, want byo-alice-lab", got.HiveID)
	}
	if got := reduceHeartbeatForMirror(src, ""); got.HiveID != src.HiveID {
		t.Errorf("hive id without override = %q, want %q", got.HiveID, src.HiveID)
	}
	if reduceHeartbeatForMirror(nil, "") != nil {
		t.Error("nil payload must reduce to nil")
	}
}

func TestLoadMirrorHubConfig(t *testing.T) {
	t.Setenv(EnvMirrorHubHeartbeatKey, mirrorTestMirrorKey)

	t.Setenv(EnvMirrorHubURL, "https://mirror.example.test/")
	cfg, err := loadMirrorHubConfig("https://hub.example.test")
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if cfg.url != "https://mirror.example.test" || cfg.key != mirrorTestMirrorKey || cfg.timeout != defaultMirrorHubTimeout {
		t.Errorf("cfg = %+v", mirrorHubConfig{url: cfg.url, hiveID: cfg.hiveID, timeout: cfg.timeout})
	}

	t.Setenv(EnvMirrorHubTimeout, "250ms")
	if cfg, err = loadMirrorHubConfig(""); err != nil || cfg.timeout != 250*time.Millisecond {
		t.Errorf("timeout override: cfg.timeout=%v err=%v", cfg.timeout, err)
	}
	t.Setenv(EnvMirrorHubTimeout, "nonsense")
	if cfg, err = loadMirrorHubConfig(""); err != nil || cfg.timeout != defaultMirrorHubTimeout {
		t.Errorf("invalid timeout must keep the default: cfg.timeout=%v err=%v", cfg.timeout, err)
	}

	if _, err := loadMirrorHubConfig("https://mirror.example.test"); err == nil {
		t.Error("mirror equal to the primary hub must be rejected")
	}
	t.Setenv(EnvMirrorHubURL, "mirror.example.test")
	if _, err := loadMirrorHubConfig(""); err == nil {
		t.Error("URL without a scheme must be rejected")
	}
	t.Setenv(EnvMirrorHubURL, "https://mirror.example.test")
	t.Setenv(EnvMirrorHubHeartbeatKey, "")
	_, err = loadMirrorHubConfig("")
	if err == nil {
		t.Fatal("missing mirror key must be rejected")
	}
	if !strings.Contains(err.Error(), EnvMirrorHubHeartbeatKey) || strings.Contains(err.Error(), mirrorTestMirrorKey) {
		t.Errorf("error should name the variable and never a key: %v", err)
	}
}

// mirrorLogSink is a goroutine-safe slog destination.
type mirrorLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *mirrorLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *mirrorLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// capturedBeat is one request a fake hub received.
type capturedBeat struct {
	auth string
	body map[string]any
}

// fakeHub is an httptest hub that records every heartbeat and answers with
// the configured response body.
type fakeHub struct {
	srv   *httptest.Server
	hits  atomic.Int64
	mu    sync.Mutex
	beats []capturedBeat
}

func newFakeHub(t *testing.T, status int, response any) *fakeHub {
	t.Helper()
	h := &fakeHub{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.beats = append(h.beats, capturedBeat{auth: r.Header.Get("Authorization"), body: body})
		h.mu.Unlock()
		h.hits.Add(1)
		w.WriteHeader(status)
		if response != nil {
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) last(t *testing.T) capturedBeat {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.beats) == 0 {
		t.Fatal("hub received no beat")
	}
	return h.beats[len(h.beats)-1]
}

func (h *fakeHub) host(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(h.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// setMirrorEnv points the mirror at srv with the test mirror key and the
// primary at the test primary key.
func setMirrorEnv(t *testing.T, mirrorURL string) {
	t.Helper()
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()
	t.Setenv(EnvHeartbeatKey, mirrorTestPrimaryKey)
	t.Setenv("HIVE_HUB_SECRET", "")
	t.Setenv(EnvMirrorHubURL, mirrorURL)
	t.Setenv(EnvMirrorHubHeartbeatKey, mirrorTestMirrorKey)
	t.Setenv(EnvMirrorHubHiveID, "")
	t.Setenv(EnvMirrorHubTimeout, "")
}

func mirrorTestPayload() *HeartbeatPayload {
	return &HeartbeatPayload{
		HiveID:                  "hive-primary",
		Org:                     "acme",
		Repos:                   []string{"acme/one", "acme/two"},
		PrimaryRepo:             "acme/one",
		ACMMLevel:               4,
		Agents:                  []AgentSummary{{Name: "reviewer", State: "running"}},
		DashboardTokenHash:      "deadbeef",
		GitHubAppKeyFingerprint: "sha256:aaaa",
		InferenceAuthError:      "401 from gateway",
		ActiveSessionUsers:      []string{"alice"},
		UserLastActions:         map[string]string{"alice": "2026-01-01T00:00:00Z"},
		Version:                 "v5",
	}
}

// droppedJSONNames returns the JSON keys of every dropped HeartbeatPayload field.
func droppedJSONNames() map[string]bool {
	out := map[string]bool{}
	typ := reflect.TypeOf(HeartbeatPayload{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if heartbeatMirrorFieldPolicy[f.Name] == mirrorFieldMirrored {
			continue
		}
		out[strings.Split(f.Tag.Get("json"), ",")[0]] = true
	}
	return out
}

func TestMirrorHeartbeat_SendsReducedCopyWithMirrorKey(t *testing.T) {
	primary := newFakeHub(t, http.StatusOK, HeartbeatResponse{OK: true})
	mirror := newFakeHub(t, http.StatusOK, HeartbeatResponse{OK: true})
	setMirrorEnv(t, mirror.srv.URL)
	t.Setenv(EnvMirrorHubHiveID, "byo-alice-hive")

	resp := postHeartbeatToHub(context.Background(), primary.srv.URL, mirrorTestPayload(), nil2Logger())
	if resp == nil || !resp.OK {
		t.Fatalf("primary response = %+v, want ok", resp)
	}
	testutil.Eventually(t, mirrorTestWait, func() bool { return mirror.hits.Load() == 1 }, "mirror hub never received the beat")

	p, m := primary.last(t), mirror.last(t)
	if p.auth != "Bearer "+mirrorTestPrimaryKey {
		t.Errorf("primary auth = %q, want the primary key", p.auth)
	}
	if m.auth != "Bearer "+mirrorTestMirrorKey {
		t.Errorf("mirror auth = %q, want the mirror key", m.auth)
	}
	for key := range droppedJSONNames() {
		if _, present := m.body[key]; present {
			t.Errorf("mirror copy still carries dropped field %q", key)
		}
		delete(p.body, key)
	}
	if m.body["hive_id"] != "byo-alice-hive" {
		t.Errorf("mirror hive_id = %v, want the override", m.body["hive_id"])
	}
	delete(p.body, "hive_id")
	delete(m.body, "hive_id")
	if !reflect.DeepEqual(p.body, m.body) {
		t.Errorf("mirror copy differs from the primary beat beyond the drop list and hive_id:\nprimary: %v\nmirror:  %v", p.body, m.body)
	}

	st, ok := MirrorHubStatusSnapshot()
	if !ok {
		t.Fatal("mirror status should be present when configured")
	}
	if st.URLHost != mirror.host(t) || st.LastOK == "" || st.LastError != "" || st.IgnoredFields != 0 {
		t.Errorf("mirror status = %+v", st)
	}
}

func TestMirrorHeartbeat_IgnoresAuthorityFieldsAndLogsOncePerWindow(t *testing.T) {
	primary := newFakeHub(t, http.StatusOK, HeartbeatResponse{OK: true})
	mirrorResp := map[string]any{
		"ok":                true,
		"upgrade_to":        "abc123",
		"switch_to_tag":     "v6",
		"restart_spoke":     true,
		"breaker":           BreakerEngage,
		"is_public":         true,
		"cluster_id":        "mirror-cluster",
		"authorized_users":  []string{"mallory:admin"},
		"github_app_config": map[string]any{"app_id": 42, "installation_id": 7, "private_key": "-----BEGIN RSA PRIVATE KEY-----"},
		"pending_gateway":   map[string]any{"name": "openrouter", "key": "sk-mirror-secret"},
		"project_config":    map[string]any{"org": "mallory"},
		"upgrade_policy":    map[string]any{"hub_managed": true},
	}
	mirror := newFakeHub(t, http.StatusOK, mirrorResp)
	setMirrorEnv(t, mirror.srv.URL)
	saved := mirrorIgnoredLogWindow
	mirrorIgnoredLogWindow = time.Hour
	t.Cleanup(func() { mirrorIgnoredLogWindow = saved })

	sink := &mirrorLogSink{}
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))

	const beats = 2
	const authorityFieldsPerResponse = 11
	for i := 0; i < beats; i++ {
		resp := postHeartbeatToHub(context.Background(), primary.srv.URL, mirrorTestPayload(), logger)
		if resp == nil || !resp.OK || resp.UpgradeTo != "" || resp.RestartSpoke || resp.GitHubAppConfig != nil || resp.PendingGateway != nil {
			t.Fatalf("primary response contaminated by the mirror: %+v", resp)
		}
		testutil.Eventually(t, mirrorTestWait, func() bool { return mirror.hits.Load() == int64(i+1) }, "mirror beat %d never arrived", i+1)
		testutil.Eventually(t, mirrorTestWait, func() bool {
			st, _ := MirrorHubStatusSnapshot()
			return st.IgnoredFields == int64(authorityFieldsPerResponse*(i+1))
		}, "ignored fields not counted for beat %d", i+1)
	}

	st, _ := MirrorHubStatusSnapshot()
	sort.Strings(st.LastIgnoredFields)
	want := []string{"authorized_users", "breaker", "cluster_id", "github_app_config", "is_public", "pending_gateway", "project_config", "restart_spoke", "switch_to_tag", "upgrade_policy", "upgrade_to"}
	if !reflect.DeepEqual(st.LastIgnoredFields, want) {
		t.Errorf("last ignored fields = %v, want %v", st.LastIgnoredFields, want)
	}

	logs := sink.String()
	if n := strings.Count(logs, "event="+mirrorResponseIgnoredEvent); n != 1 {
		t.Errorf("%s logged %d times across %d beats, want once per window:\n%s", mirrorResponseIgnoredEvent, n, beats, logs)
	}
	for _, secret := range []string{mirrorTestMirrorKey, mirrorTestPrimaryKey, "sk-mirror-secret", "BEGIN RSA"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, "mirror_host="+mirror.host(t)) {
		t.Errorf("log should name the mirror host:\n%s", logs)
	}
	if _, ok := LastHeartbeatSuccess(); !ok {
		t.Error("primary success must be recorded")
	}
}

func TestMirrorHeartbeat_RejectionDoesNotAffectPrimary(t *testing.T) {
	primary := newFakeHub(t, http.StatusOK, HeartbeatResponse{OK: true})
	mirror := newFakeHub(t, http.StatusInternalServerError, nil)
	setMirrorEnv(t, mirror.srv.URL)
	sink := &mirrorLogSink{}
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))

	resp := postHeartbeatToHub(context.Background(), primary.srv.URL, mirrorTestPayload(), logger)
	if resp == nil || !resp.OK {
		t.Fatalf("primary response = %+v, want ok despite the mirror 500", resp)
	}
	if _, ok := LastHeartbeatSuccess(); !ok {
		t.Error("primary success must be recorded")
	}
	testutil.Eventually(t, mirrorTestWait, func() bool {
		st, _ := MirrorHubStatusSnapshot()
		return strings.Contains(st.LastError, "HTTP 500")
	}, "mirror rejection not surfaced in status")
	st, _ := MirrorHubStatusSnapshot()
	if st.LastOK != "" || st.LastAttempt == "" {
		t.Errorf("mirror status = %+v", st)
	}
	if strings.Contains(sink.String(), mirrorTestMirrorKey) {
		t.Error("mirror key leaked into logs")
	}
}

func TestMirrorHeartbeat_TimeoutDoesNotDelayPrimary(t *testing.T) {
	primary := newFakeHub(t, http.StatusOK, HeartbeatResponse{OK: true})
	release := make(chan struct{})
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	// Cleanups run last-registered first: release the handler, then close.
	t.Cleanup(mirror.Close)
	t.Cleanup(func() { close(release) })
	setMirrorEnv(t, mirror.URL)
	t.Setenv(EnvMirrorHubTimeout, "50ms")

	// The mirror handler blocks until cleanup, so this call can only return if
	// the mirror POST is decoupled from the primary beat.
	resp := postHeartbeatToHub(context.Background(), primary.srv.URL, mirrorTestPayload(), nil2Logger())
	if resp == nil || !resp.OK {
		t.Fatalf("primary response = %+v, want ok while the mirror hangs", resp)
	}
	testutil.Eventually(t, mirrorTestWait, func() bool {
		st, _ := MirrorHubStatusSnapshot()
		return st.LastError == "unreachable: timeout"
	}, "mirror timeout not surfaced in status")
	testutil.Eventually(t, mirrorTestWait, func() bool { return !mirrorInFlight.Load() }, "mirror goroutine did not finish after its timeout")
}

func TestMirrorHeartbeat_SkipsWhileAnotherBeatInFlight(t *testing.T) {
	primary := newFakeHub(t, http.StatusOK, HeartbeatResponse{OK: true})
	mirror := newFakeHub(t, http.StatusOK, HeartbeatResponse{OK: true})
	setMirrorEnv(t, mirror.srv.URL)

	// Simulate a mirror POST that has not returned yet.
	mirrorInFlight.Store(true)
	postHeartbeatToHub(context.Background(), primary.srv.URL, mirrorTestPayload(), nil2Logger())
	st, _ := MirrorHubStatusSnapshot()
	if !strings.Contains(st.LastError, "in flight") {
		t.Errorf("skipped beat not recorded: %+v", st)
	}
	if mirror.hits.Load() != 0 {
		t.Error("a second mirror POST was started while one was in flight")
	}
	mirrorInFlight.Store(false)
}

func TestMirrorHeartbeat_WithoutMirrorKeySendsNothing(t *testing.T) {
	primary := newFakeHub(t, http.StatusOK, HeartbeatResponse{OK: true})
	mirror := newFakeHub(t, http.StatusOK, HeartbeatResponse{OK: true})
	setMirrorEnv(t, mirror.srv.URL)
	t.Setenv(EnvMirrorHubHeartbeatKey, "")

	resp := postHeartbeatToHub(context.Background(), primary.srv.URL, mirrorTestPayload(), nil2Logger())
	if resp == nil || !resp.OK {
		t.Fatalf("primary response = %+v, want ok", resp)
	}
	// No goroutine is started on a configuration error, so the status is
	// final as soon as postHeartbeatToHub returns.
	st, ok := MirrorHubStatusSnapshot()
	if !ok || !strings.Contains(st.LastError, EnvMirrorHubHeartbeatKey) {
		t.Errorf("mirror status = %+v, want a configuration error naming %s", st, EnvMirrorHubHeartbeatKey)
	}
	if mirror.hits.Load() != 0 {
		t.Error("the primary key must never be offered to the mirror hub: a beat was sent without a mirror key")
	}
	if primary.last(t).auth != "Bearer "+mirrorTestPrimaryKey {
		t.Error("primary auth changed")
	}
}

func TestMirrorHeartbeat_UnsetIsANoop(t *testing.T) {
	primary := newFakeHub(t, http.StatusOK, HeartbeatResponse{OK: true})
	setMirrorEnv(t, "")

	if resp := postHeartbeatToHub(context.Background(), primary.srv.URL, mirrorTestPayload(), nil2Logger()); resp == nil || !resp.OK {
		t.Fatalf("primary response = %+v, want ok", resp)
	}
	if st, ok := MirrorHubStatusSnapshot(); ok {
		t.Errorf("mirror status present with %s unset: %+v", EnvMirrorHubURL, st)
	}
	if mirrorInFlight.Load() {
		t.Error("a mirror goroutine was started with no mirror configured")
	}
}

func TestMirrorResponseAuthorityFields(t *testing.T) {
	names, err := mirrorResponseAuthorityFields([]byte(`{"ok":true,"hub_git_hash":"h","latest_sha":"s","sig_seq":3,"activity_etag":"e"}`))
	if err != nil || len(names) != 0 {
		t.Errorf("harmless response = %v, %v; want no authority fields", names, err)
	}
	names, err = mirrorResponseAuthorityFields([]byte(`{"ok":true,"upgrade_to":"x","hub_banner":{"id":"b","message":"m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"hub_banner", "upgrade_to"}) {
		t.Errorf("authority fields = %v", names)
	}
	if _, err := mirrorResponseAuthorityFields([]byte("not json")); err == nil {
		t.Error("malformed response must report an error, not a count")
	}
}
