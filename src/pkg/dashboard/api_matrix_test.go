package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func helperAddMatrixLine(m *matrixSource, repo, text string) matrixLine {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	l := matrixLine{
		Seq:  m.seq,
		Repo: repo,
		PR:   1,
		Head: "sha-123",
		Path: "main.go",
		Lang: "go",
		Line: text,
		At:   time.Now().UTC(),
	}
	m.appendLocked(l)
	return l
}

func TestHandleMatrixLines_404WhenOff(t *testing.T) {
	s := newTestServer()

	// 1. Matrix source is nil
	req := httptest.NewRequest(http.MethodGet, "/api/matrix/lines", nil)
	req.Header.Set("X-Hive-Role", config.RoleRead)
	rr := httptest.NewRecorder()
	s.handleMatrixLines(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("nil matrix source: code = %d, want 404", rr.Code)
	}

	// 2. Matrix configured with Enabled: false
	s.MatrixObserver(config.MatrixConfig{Enabled: false, Repos: []string{"org/repo"}})
	rr = httptest.NewRecorder()
	s.handleMatrixLines(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled matrix config: code = %d, want 404", rr.Code)
	}

	// 3. Matrix configured with Enabled: true but empty repos (inactive)
	s.MatrixObserver(config.MatrixConfig{Enabled: true, Repos: []string{}})
	rr = httptest.NewRecorder()
	s.handleMatrixLines(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("inactive matrix config: code = %d, want 404", rr.Code)
	}
}

func TestHandleMatrixLines_AuthRejectionAndKiosk(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServerWithAuth(0, "matrix-secret-123", logger)
	s.mux.HandleFunc("GET /api/matrix/lines", s.handleMatrixLines)
	s.MatrixObserver(config.MatrixConfig{Enabled: true, Repos: []string{"org/repo"}})

	// isPublicPath must be false
	if isPublicPath("/api/matrix/lines") {
		t.Fatal("isPublicPath(\"/api/matrix/lines\") must be false")
	}

	// Unauthenticated request without kiosk
	req := httptest.NewRequest(http.MethodGet, "/api/matrix/lines", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: code = %d, want 401", rr.Code)
	}

	// Unauthenticated request with ?kiosk=1 must be rejected
	reqKiosk := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?kiosk=1", nil)
	rrKiosk := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrKiosk, reqKiosk)
	if rrKiosk.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ?kiosk=1: code = %d, want 401", rrKiosk.Code)
	}

	// Unauthenticated request with ?view=matrix must be rejected
	reqView := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?view=matrix", nil)
	rrView := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrView, reqView)
	if rrView.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ?view=matrix: code = %d, want 401", rrView.Code)
	}

	// Authenticated request succeeds
	reqAuth := httptest.NewRequest(http.MethodGet, "/api/matrix/lines", nil)
	reqAuth.Header.Set("Authorization", "Bearer matrix-secret-123")
	rrAuth := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrAuth, reqAuth)
	if rrAuth.Code != http.StatusOK {
		t.Fatalf("authenticated: code = %d, want 200 (body=%s)", rrAuth.Code, rrAuth.Body.String())
	}
}

func TestHandleMatrixLines_PerRepoFilter(t *testing.T) {
	s := newTestServer()
	cfg := config.MatrixConfig{
		Enabled: true,
		Repos:   []string{"hivecommons/allowed"},
	}
	s.MatrixObserver(cfg)

	src := s.matrix
	helperAddMatrixLine(src, "hivecommons/allowed", "line1")
	helperAddMatrixLine(src, "hivecommons/denied", "line2")
	helperAddMatrixLine(src, "hivecommons/allowed", "line3")

	// 1. Normal read sees only allowlisted repo lines
	req := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?since=0", nil)
	req.Header.Set("X-Hive-Role", config.RoleRead)
	rr := httptest.NewRecorder()
	s.handleMatrixLines(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}

	var resp matrixLinesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Source != "pushed-lines" {
		t.Fatalf("source = %q, want pushed-lines", resp.Source)
	}
	if len(resp.Lines) != 2 {
		t.Fatalf("expected 2 allowed lines, got %d", len(resp.Lines))
	}
	for _, l := range resp.Lines {
		if l.Repo != "hivecommons/allowed" {
			t.Fatalf("unexpected repo in output: %s", l.Repo)
		}
	}

	// 2. Caller without read access receives 403 Forbidden
	reqNone := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?since=0", nil)
	reqNone.Header.Set("X-Hive-Role", "none")
	rrNone := httptest.NewRecorder()
	s.handleMatrixLines(rrNone, reqNone)
	if rrNone.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rrNone.Code)
	}

	// 3. Removing repo from allowlist hides its lines immediately
	updatedCfg := config.MatrixConfig{
		Enabled: true,
		Repos:   []string{"hivecommons/other"},
	}
	s.MatrixObserver(updatedCfg)

	rrUpdated := httptest.NewRecorder()
	s.handleMatrixLines(rrUpdated, req)
	if rrUpdated.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rrUpdated.Code)
	}
	var respUpdated matrixLinesResponse
	if err := json.Unmarshal(rrUpdated.Body.Bytes(), &respUpdated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(respUpdated.Lines) != 0 {
		t.Fatalf("expected 0 lines after repo removed from allowlist, got %d", len(respUpdated.Lines))
	}
}

func TestHandleMatrixLines_PagingAndClamp(t *testing.T) {
	s := newTestServer()
	cfg := config.MatrixConfig{
		Enabled: true,
		Repos:   []string{"hivecommons/hive"},
	}
	s.MatrixObserver(cfg)
	src := s.matrix

	for i := 1; i <= 10; i++ {
		helperAddMatrixLine(src, "hivecommons/hive", "code line")
	}

	// Response header: Cache-Control: no-store
	req := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?since=0", nil)
	req.Header.Set("X-Hive-Role", config.RoleRead)
	rr := httptest.NewRecorder()
	s.handleMatrixLines(rr, req)
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	// Paging with since=5 returns strictly newer lines (seq 6..10)
	reqSince := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?since=5", nil)
	reqSince.Header.Set("X-Hive-Role", config.RoleRead)
	rrSince := httptest.NewRecorder()
	s.handleMatrixLines(rrSince, reqSince)
	var respSince matrixLinesResponse
	if err := json.Unmarshal(rrSince.Body.Bytes(), &respSince); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(respSince.Lines) != 5 {
		t.Fatalf("since=5: got %d lines, want 5", len(respSince.Lines))
	}
	for _, l := range respSince.Lines {
		if l.Seq <= 5 {
			t.Fatalf("expected seq > 5, got seq %d", l.Seq)
		}
	}
	if respSince.Seq != 10 {
		t.Fatalf("resp.Seq = %d, want 10", respSince.Seq)
	}

	// Clamping: max=3 returns 3 lines
	reqMax3 := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?since=0&max=3", nil)
	reqMax3.Header.Set("X-Hive-Role", config.RoleRead)
	rrMax3 := httptest.NewRecorder()
	s.handleMatrixLines(rrMax3, reqMax3)
	var respMax3 matrixLinesResponse
	_ = json.Unmarshal(rrMax3.Body.Bytes(), &respMax3)
	if len(respMax3.Lines) != 3 {
		t.Fatalf("max=3: got %d lines, want 3", len(respMax3.Lines))
	}

	// Clamping: max=0 clamped to min (1)
	reqMax0 := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?since=0&max=0", nil)
	reqMax0.Header.Set("X-Hive-Role", config.RoleRead)
	rrMax0 := httptest.NewRecorder()
	s.handleMatrixLines(rrMax0, reqMax0)
	var respMax0 matrixLinesResponse
	_ = json.Unmarshal(rrMax0.Body.Bytes(), &respMax0)
	if len(respMax0.Lines) != 1 {
		t.Fatalf("max=0: got %d lines, want 1", len(respMax0.Lines))
	}

	// Clamping: max=-5 clamped to min (1)
	reqMaxNeg := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?since=0&max=-5", nil)
	reqMaxNeg.Header.Set("X-Hive-Role", config.RoleRead)
	rrMaxNeg := httptest.NewRecorder()
	s.handleMatrixLines(rrMaxNeg, reqMaxNeg)
	var respMaxNeg matrixLinesResponse
	_ = json.Unmarshal(rrMaxNeg.Body.Bytes(), &respMaxNeg)
	if len(respMaxNeg.Lines) != 1 {
		t.Fatalf("max=-5: got %d lines, want 1", len(respMaxNeg.Lines))
	}

	// Clamping: max=999 clamped to max (500)
	reqMax999 := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?since=0&max=999", nil)
	reqMax999.Header.Set("X-Hive-Role", config.RoleRead)
	rrMax999 := httptest.NewRecorder()
	s.handleMatrixLines(rrMax999, reqMax999)
	var respMax999 matrixLinesResponse
	_ = json.Unmarshal(rrMax999.Body.Bytes(), &respMax999)
	// We only inserted 10 lines, so all 10 are returned
	if len(respMax999.Lines) != 10 {
		t.Fatalf("max=999: got %d lines, want 10", len(respMax999.Lines))
	}
}

func TestMatrixLinesNudger_CoalescingBurst(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	timers := &fakeTimers{}
	nudger := &stageTrackerNudger{interval: matrixLinesNudgeInterval, now: func() time.Time { return now }, newTimer: timers.newTimer}

	var sent []uint64
	send := func(seq uint64) { sent = append(sent, seq) }

	// First notification sends immediately
	nudger.nudge(1, send)
	if len(sent) != 1 || sent[0] != 1 {
		t.Fatalf("first nudge must send immediately: sent=%v", sent)
	}

	// Burst of 49 notifications inside 1 s interval (total 50 notifications)
	for i := 2; i <= 50; i++ {
		now = now.Add(15 * time.Millisecond) // total 735 ms < 1 s
		nudger.nudge(uint64(i), send)
	}

	// Still only 1 frame sent, and 1 timer armed
	if len(sent) != 1 || timers.count() != 1 {
		t.Fatalf("burst of 50 inside 1 s must collapse into 1 pending frame: sent=%v timers=%d", sent, timers.count())
	}

	// Advance clock and fire timer
	now = now.Add(matrixLinesNudgeInterval)
	timers.last().fire()

	// Total frames sent must be at most 2 (first immediate + one trailing)
	if len(sent) != 2 {
		t.Fatalf("expected at most 2 frames from burst of 50 in 1 s, got %d (sent=%v)", len(sent), sent)
	}
	if sent[1] != 50 {
		t.Fatalf("trailing frame must carry newest seq (50), got %d", sent[1])
	}
}

func TestMatrixLinesSSEFrame_ContentFree(t *testing.T) {
	frame := matrixCodeLinesSSEFrame(123)
	expected := "event: code-lines\ndata: {\"seq\":123}\n\n"
	if frame != expected {
		t.Fatalf("frame = %q, want %q", frame, expected)
	}

	// Verify data payload contains only seq
	data := `{"seq":123}`
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload) != 1 || payload["seq"] != float64(123) {
		t.Fatalf("payload must contain only seq: %v", payload)
	}

	// Server broadcasts nothing when flag is off
	s := newTestServer()
	ch := make(chan []byte, 10)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	s.nudgeMatrixLines(99)
	if len(ch) != 0 {
		t.Fatalf("flag off: expected no broadcast, got %d frames", len(ch))
	}

	// Server broadcasts when flag is active
	s.MatrixObserver(config.MatrixConfig{Enabled: true, Repos: []string{"org/repo"}})
	s.nudgeMatrixLines(100)
	if len(ch) != 1 {
		t.Fatalf("flag active: expected 1 broadcast frame, got %d", len(ch))
	}
	got := string(<-ch)
	if got != matrixCodeLinesSSEFrame(100) {
		t.Fatalf("broadcast payload = %q, want %q", got, matrixCodeLinesSSEFrame(100))
	}
}
