package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard"
)

type fakeStatusPublisher struct {
	epoch     uint64
	published []uint64
	accept    bool
}

func (f *fakeStatusPublisher) BeginStatusSnapshot() uint64 { return f.epoch }

func (f *fakeStatusPublisher) UpdateStatusIfFresh(_ *dashboard.StatusPayload, buildEpoch uint64) bool {
	f.published = append(f.published, buildEpoch)
	return f.accept
}

func TestPublishStatusBeforeKicks_SkipsWhenNoKicksDue(t *testing.T) {
	pub := &fakeStatusPublisher{accept: true}
	builds := 0
	published := publishStatusBeforeKicks(pub, 0, func() *dashboard.StatusPayload {
		builds++
		return &dashboard.StatusPayload{}
	}, restoreTestLogger())
	if published || builds != 0 || len(pub.published) != 0 {
		t.Fatalf("no kicks due: want no build and no publish, got published=%v builds=%d publishes=%d",
			published, builds, len(pub.published))
	}
}

func TestPublishStatusBeforeKicks_PublishesWithCapturedEpoch(t *testing.T) {
	pub := &fakeStatusPublisher{epoch: 7, accept: true}
	builds := 0
	published := publishStatusBeforeKicks(pub, 3, func() *dashboard.StatusPayload {
		builds++
		return &dashboard.StatusPayload{}
	}, restoreTestLogger())
	if !published || builds != 1 {
		t.Fatalf("kicks due: want one build and a publish, got published=%v builds=%d", published, builds)
	}
	if len(pub.published) != 1 || pub.published[0] != 7 {
		t.Fatalf("publish must carry the epoch captured before the build, got %v", pub.published)
	}
}

func TestPublishStatusBeforeKicks_DropLoggedAndReported(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	pub := &fakeStatusPublisher{accept: false}
	published := publishStatusBeforeKicks(pub, 2, func() *dashboard.StatusPayload {
		return &dashboard.StatusPayload{}
	}, logger)
	if published {
		t.Fatal("a dropped snapshot must be reported as not published")
	}
	if !strings.Contains(logBuf.String(), "pre-kick status snapshot dropped") ||
		!strings.Contains(logBuf.String(), "pending_kicks=2") {
		t.Fatalf("drop must be logged with the pending kick count, got:\n%s", logBuf.String())
	}
}

func TestPublishStatusBeforeKicks_NilBuildOrPayload(t *testing.T) {
	pub := &fakeStatusPublisher{accept: true}
	if publishStatusBeforeKicks(pub, 1, nil, restoreTestLogger()) {
		t.Fatal("nil build must not publish")
	}
	if publishStatusBeforeKicks(pub, 1, func() *dashboard.StatusPayload { return nil }, restoreTestLogger()) {
		t.Fatal("nil payload must not publish")
	}
	if publishStatusBeforeKicks(nil, 1, func() *dashboard.StatusPayload { return &dashboard.StatusPayload{} }, restoreTestLogger()) {
		t.Fatal("nil publisher must not publish")
	}
	if len(pub.published) != 0 {
		t.Fatalf("nothing should have reached the publisher, got %d calls", len(pub.published))
	}
}

// TestPublishStatusBeforeKicks_RealServerAdvancesSeq runs the pre-kick publish
// against a real dashboard server, then the end-of-cycle publish the eval loop
// performs, and asserts the dashboard sees BOTH: the evaluation lands before
// the kicks, and the sequence keeps advancing after them.
func TestPublishStatusBeforeKicks_RealServerAdvancesSeq(t *testing.T) {
	dashSrv := dashboard.NewServer(0, restoreTestLogger())
	build := func() *dashboard.StatusPayload { return &dashboard.StatusPayload{} }

	seq := func() float64 {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		rec := httptest.NewRecorder()
		dashSrv.Handler().ServeHTTP(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("status body is not JSON: %v\n%s", err, rec.Body.String())
		}
		got, _ := out["statusSeq"].(float64)
		return got
	}

	if !publishStatusBeforeKicks(dashSrv, 4, build, restoreTestLogger()) {
		t.Fatal("pre-kick publish should succeed on an idle server")
	}
	if got := seq(); got != 1 {
		t.Fatalf("after the pre-kick publish want statusSeq 1, got %v", got)
	}

	// Kicks run here in production (minutes). Then the end-of-cycle publish.
	epoch := dashSrv.BeginStatusSnapshot()
	if !dashSrv.UpdateStatusIfFresh(build(), epoch) {
		t.Fatal("end-of-cycle publish should succeed")
	}
	if got := seq(); got != 2 {
		t.Fatalf("after the end-of-cycle publish want statusSeq 2, got %v", got)
	}
}
