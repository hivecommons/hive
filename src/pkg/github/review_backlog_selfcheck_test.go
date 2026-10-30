package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type fakeCloserSink struct {
	fakeResolverSink
	closed []string
	notes  map[string]string
}

func (f *fakeCloserSink) CloseItem(_ context.Context, _ string, ref ReviewBacklogRef, note string) error {
	f.closed = append(f.closed, ref.Key)
	if f.notes == nil {
		f.notes = map[string]string{}
	}
	f.notes[ref.Key] = note
	return nil
}

// When the source PR merges the hive answers "does it still apply on base?"
// itself: the finding's file gone, or none of the symbols its title names
// left in it, closes the item as completed; a symbol still there lifts the
// block as before; no evidence file (legacy record) lifts; a sink that
// cannot close lifts.
func TestReconcileReviewBacklogBlocksSelfVerifiesOnMerge(t *testing.T) {
	withReviewBacklogState(t)
	contents := map[string]string{
		"pkg/keep.go":    "func prWarnOnce() {}\n",
		"pkg/renamed.go": "func warnOncePerPR() {}\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/o/r/pulls/5":
			_, _ = io.WriteString(w, `{"number":5,"state":"closed","merged":true,"base":{"ref":"v5"}}`)
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/contents/"):
			if r.URL.Query().Get("ref") != "v5" {
				t.Errorf("contents read at ref %q, want the PR's base v5", r.URL.Query().Get("ref"))
			}
			p := strings.TrimPrefix(r.URL.Path, "/repos/o/r/contents/")
			body, ok := contents[p]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"type":"file","encoding":"base64","name":%q,"path":%q,"content":%q}`, p, p, base64.StdEncoding.EncodeToString([]byte(body)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	sink := &fakeCloserSink{fakeResolverSink: fakeResolverSink{fakeBacklogSink: fakeBacklogSink{dest: config.ReviewBacklogLinear}}}
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{Backlog: config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear}, Sink: sink}
	})
	ledger := newReviewBacklogLedger()
	rec := func(key, file, title string) reviewBacklogFingerprintRecord {
		return reviewBacklogFingerprintRecord{Destination: config.ReviewBacklogLinear, ItemID: "id-" + key, ItemKey: key, BlockedOnPR: 5, BlockState: reviewBacklogBlockBlocked, File: file, Title: title}
	}
	for fp, r := range map[string]reviewBacklogFingerprintRecord{
		"fp-gone-file":   rec("GONE-FILE", "pkg/deleted.go", "Review backlog (style): helperThing re-covers prWarnOnce semantics"),
		"fp-gone-symbol": rec("GONE-SYM", "pkg/renamed.go", "Review backlog (style): prWarnOnce re-covers the dispatcher"),
		"fp-applies":     rec("APPLIES", "pkg/keep.go", "Review backlog (style): prWarnOnce re-covers the dispatcher"),
		"fp-no-symbol":   rec("NOSYM", "pkg/keep.go", "Review backlog (docs-currency): the comment is stale"),
		"fp-legacy":      rec("LEGACY", "", ""),
	} {
		ledger.setFingerprint("o/r", fp, r)
	}
	if err := ledger.save("", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	res := ReconcileReviewBacklogBlocks(context.Background(), c, nil)
	if res.Checked != 5 || res.Closed != 2 || res.Lifted != 3 || res.Errors != 0 {
		t.Fatalf("result = %+v, want checked 5, closed 2, lifted 3", res)
	}
	closed := strings.Join(sink.closed, ",")
	if !strings.Contains(closed, "GONE-FILE") || !strings.Contains(closed, "GONE-SYM") || len(sink.closed) != 2 {
		t.Fatalf("closed = %v", sink.closed)
	}
	lifted := strings.Join(sink.lifted, ",")
	for _, want := range []string{"APPLIES", "NOSYM", "LEGACY"} {
		if !strings.Contains(lifted, want) {
			t.Errorf("lifted %v lacks %s", sink.lifted, want)
		}
	}
	if n := sink.notes["GONE-FILE"]; !strings.Contains(n, blockedUnblockedMarker) || !strings.Contains(n, "o/r#5 merged and `pkg/deleted.go` no longer exists on v5") {
		t.Errorf("gone-file note = %q", n)
	}
	if n := sink.notes["GONE-SYM"]; !strings.Contains(n, "`prWarnOnce` is no longer in `pkg/renamed.go` on v5") {
		t.Errorf("gone-symbol note = %q", n)
	}
	saved, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for fp, r := range saved.Fingerprints["o/r"] {
		states[fp] = r.BlockState
	}
	if states["fp-gone-file"] != reviewBacklogBlockClosed || states["fp-gone-symbol"] != reviewBacklogBlockClosed || states["fp-applies"] != reviewBacklogBlockLifted {
		t.Fatalf("states = %v", states)
	}

	// A closed item is never looked at again.
	sink.closed, sink.lifted = nil, nil
	if res = ReconcileReviewBacklogBlocks(context.Background(), c, nil); res.Checked != 0 || res.Closed+res.Lifted != 0 {
		t.Fatalf("second pass = %+v, want nothing to do", res)
	}
}

// A sink without CloseItem keeps the lift-and-ask behaviour even when the
// file is gone.
func TestReconcileReviewBacklogBlocksLiftsWhenSinkCannotClose(t *testing.T) {
	withReviewBacklogState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/o/r/pulls/5" {
			_, _ = io.WriteString(w, `{"number":5,"state":"closed","merged":true,"base":{"ref":"v5"}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	sink := &fakeResolverSink{fakeBacklogSink: fakeBacklogSink{dest: config.ReviewBacklogLinear}}
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{Backlog: config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear}, Sink: sink}
	})
	ledger := newReviewBacklogLedger()
	ledger.setFingerprint("o/r", "fp", reviewBacklogFingerprintRecord{Destination: config.ReviewBacklogLinear, ItemID: "id", ItemKey: "ENG-1", BlockedOnPR: 5, BlockState: reviewBacklogBlockBlocked, File: "pkg/deleted.go", Title: "Review backlog (style): prWarnOnce"})
	if err := ledger.save("", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if res := ReconcileReviewBacklogBlocks(context.Background(), c, nil); res.Closed != 0 || res.Lifted != 1 || strings.Join(sink.lifted, ",") != "ENG-1" {
		t.Fatalf("result = %+v lifted=%v", res, sink.lifted)
	}
}
