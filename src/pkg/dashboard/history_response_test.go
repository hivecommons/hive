package dashboard

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/governor"
)

func historyGET(handler http.HandlerFunc, path, etag, encoding string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("If-None-Match", etag)
	r.Header.Set("Accept-Encoding", encoding)
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func TestBoundedHistoryEndpoints(t *testing.T) {
	s, _ := apiServer(t)
	costs := make([]CostHistoryEntry, 1000)
	facts := make([]FactHistoryEntry, 1000)
	for i := range costs {
		ts := int64(i + 1)
		costs[i] = CostHistoryEntry{Timestamp: ts, USD: float64(i), Agents: map[string]float64{"scanner": float64(i)}}
		facts[i] = FactHistoryEntry{Timestamp: ts, Count: i}
	}
	s.SeedCostHistory(costs)
	s.SeedFactHistory(facts)
	for _, path := range []string{"/api/cost/history", "/api/knowledge/fact-history", "/api/timeseries?series=cost", "/api/timeseries?series=fact"} {
		t.Run(path, func(t *testing.T) {
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			for _, tc := range []struct {
				query string
				count int
				first int64
			}{
				{"", defaultHistoryPoints, 1},
				{"max_points=3", 3, 1},
				{"max_points=1", 1, 1000},
				{"max_points=10000", 1000, 1},
				{"since=995", 5, 996},
				{"since=995&max_points=2", 2, 996},
				{"since=1000", 0, 0},
				{"since=1001", 0, 0},
			} {
				url := path + sep + tc.query
				w := historyGET(s.mux.ServeHTTP, url, "", "")
				if w.Code != http.StatusOK {
					t.Fatalf("%s: status %d: %s", url, w.Code, w.Body.String())
				}
				var entries []struct {
					Timestamp int64 `json:"t"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
					t.Fatal(err)
				}
				if len(entries) != tc.count {
					t.Fatalf("%s: got %d rows, want %d", url, len(entries), tc.count)
				}
				if len(entries) > 0 && (entries[0].Timestamp != tc.first || entries[len(entries)-1].Timestamp != 1000) {
					t.Fatalf("%s: wrong endpoints: %+v", url, entries)
				}
				etag := w.Header().Get("ETag")
				if etag == "" || w.Header().Get("X-Hive-History-Returned") != fmt.Sprint(tc.count) {
					t.Fatalf("%s: missing validator/count: %v", url, w.Header())
				}
				cached := historyGET(s.mux.ServeHTTP, url, etag, "")
				if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 {
					t.Fatalf("%s: cache hit = %d, %d bytes", url, cached.Code, cached.Body.Len())
				}
			}
			for _, query := range []string{"since=no", "since=-1", "since=9223372036854775808", "max_points=0", "max_points=-1", "max_points=10001", "max_points=no"} {
				w := historyGET(s.mux.ServeHTTP, path+sep+query, "*", "")
				if w.Code != http.StatusBadRequest {
					t.Errorf("%s: status %d, want 400", query, w.Code)
				}
			}
		})
	}
	if !reflect.DeepEqual(s.CostHistory(), costs) || !reflect.DeepEqual(s.FactHistory(), facts) {
		t.Fatal("response sampling mutated retained history")
	}
	first := historyGET(s.handleCostHistory, "/?max_points=2", "", "")
	changedQuery := historyGET(s.handleCostHistory, "/?max_points=3", first.Header().Get("ETag"), "")
	if changedQuery.Code != http.StatusOK {
		t.Fatal("a different projection matched the old ETag")
	}
	costs[999].USD++
	s.SeedCostHistory(costs)
	changedData := historyGET(s.handleCostHistory, "/?max_points=2", first.Header().Get("ETag"), "")
	if changedData.Code != http.StatusOK || changedData.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatal("updated history matched the old ETag")
	}
}

func TestEvalHistoryResponseSampling(t *testing.T) {
	entries := make([]governor.EvalSnapshot, 1000)
	seed := make([]json.RawMessage, len(entries))
	for i := range entries {
		entries[i] = governor.EvalSnapshot{Timestamp: int64(i + 1), QueueIssues: i}
		seed[i], _ = json.Marshal(entries[i])
	}
	for _, payload := range []any{entries, seed} {
		handler := func(w http.ResponseWriter, r *http.Request) { evalHistoryResponse(w, r, payload) }
		for _, tc := range []struct {
			query string
			count int
			first int64
		}{
			{"/", 512, 1}, {"/?since=990&max_points=2", 2, 991},
		} {
			w := historyGET(handler, tc.query, "", "")
			var got []governor.EvalSnapshot
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.count || got[0].Timestamp != tc.first || got[len(got)-1].Timestamp != 1000 {
				t.Fatalf("%T %s: incorrect sampled eval history", payload, tc.query)
			}
		}
	}
}

func TestSeededEvalHistoryPreservesLegacyFields(t *testing.T) {
	seed := []json.RawMessage{
		json.RawMessage(`{"t":1,"legacy":{"extra":true}}`),
		json.RawMessage(`{"t":2,"legacy":{"extra":false}}`),
		json.RawMessage(`{"t":3,"govIssues":7}`),
	}
	handler := func(w http.ResponseWriter, r *http.Request) { evalHistoryResponse(w, r, seed) }
	w := historyGET(handler, "/?since=1&max_points=2", "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"legacy":{"extra":false}`) || strings.Contains(w.Body.String(), `"t":1`) {
		t.Fatalf("seeded history lost fields or ignored since: %d %s", w.Code, w.Body.String())
	}
}

func TestHistoryConditionalRequests(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		conditionalJSONResponse(w, r, []int{1, 2})
	}
	first := historyGET(handler, "/", "", "")
	etag := first.Header().Get("ETag")
	for _, match := range []string{etag, strings.TrimPrefix(etag, "W/"), `"unrelated", ` + etag, "*"} {
		w := historyGET(handler, "/", match, "")
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("ETag") != etag || w.Header().Get("Cache-Control") != "private, no-cache" {
			t.Fatalf("%s: invalid conditional response: %d %v %s", match, w.Code, w.Header(), w.Body.String())
		}
	}
	w := historyGET(handler, "/", `"other"`, "")
	if w.Code != http.StatusOK || w.Body.String() != "[1,2]\n" {
		t.Fatalf("non-match: %d %s", w.Code, w.Body.String())
	}
	bad := historyGET(func(w http.ResponseWriter, r *http.Request) {
		conditionalJSONResponse(w, r, make(chan int))
	}, "/", "*", "")
	if bad.Code != http.StatusInternalServerError || bad.Header().Get("ETag") != "" {
		t.Fatal("unencodable body was cached")
	}
}

func TestStatusSnapshotConditionalVariants(t *testing.T) {
	s := newTestServer()
	s.status = minimalPayload()
	for _, handler := range []http.HandlerFunc{s.handleStatus, s.handleStatusSummary} {
		first := historyGET(handler, "/", "", "")
		etag := first.Header().Get("ETag")
		if first.Code != http.StatusOK || etag == "" {
			t.Fatalf("initial status: %d %v", first.Code, first.Header())
		}
		compressed := historyGET(handler, "/", "", "gzip")
		zr, err := gzip.NewReader(compressed.Body)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil || string(body) != first.Body.String() || compressed.Header().Get("ETag") != etag {
			t.Fatal("gzip and identity are not the same representation")
		}
		for _, encoding := range []string{"", "gzip"} {
			w := historyGET(handler, "/", etag, encoding)
			if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("Vary") != "Accept-Encoding" || w.Header().Get("Content-Encoding") != "" {
				t.Fatalf("status conditional %q: %d %v", encoding, w.Code, w.Header())
			}
		}
	}
	full := historyGET(s.handleStatus, "/", "", "")
	for _, path := range []string{"/?fields=agents", "/?omit=repos"} {
		w := historyGET(s.handleStatus, path, full.Header().Get("ETag"), "")
		if w.Code != http.StatusOK || w.Header().Get("ETag") == full.Header().Get("ETag") {
			t.Fatalf("status projection reused full validator: %s", path)
		}
	}
	s.status.HiveID = "changed"
	if w := historyGET(s.handleStatus, "/", full.Header().Get("ETag"), ""); w.Code != http.StatusOK {
		t.Fatal("changed status matched old validator")
	}
}

func TestLaunchCostBoundedDetailsAndStableValidator(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	resp := collect.ComputeLaunchCost(nil, nil, now)
	resp.TotalTokens = 12345
	resp.TotalUSD = 42
	resp.AttributedTokens = 10000
	resp.Unattributed.Tokens = 2345
	for i := 0; i < 600; i++ {
		at := now.Add(-time.Duration(i) * time.Second).Format(time.RFC3339)
		resp.ByLaunch = append(resp.ByLaunch, collect.LaunchCostEntry{Agent: fmt.Sprint(i), LaunchedAt: at})
		resp.ByRef = append(resp.ByRef, collect.RefCostEntry{Ref: fmt.Sprintf("o/r#%d", i), LastLaunchedAt: at})
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		q, ok := parseHistoryQuery(w, r)
		if ok {
			launchCostResponse(w, r, resp, q)
		}
	}
	first := historyGET(handler, "/", "", "")
	var body struct {
		collect.LaunchCostResponse
		LaunchesTotal int  `json:"by_launch_total"`
		RefsTotal     int  `json:"by_ref_total"`
		Truncated     bool `json:"truncated"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.ByLaunch) != 512 || len(body.ByRef) != 512 || body.LaunchesTotal != 600 || body.RefsTotal != 600 || !body.Truncated || body.TotalTokens != 12345 || body.TotalUSD != 42 || body.Unattributed.Tokens != 2345 || !body.CollectedAt.Equal(now) {
		t.Fatalf("bounded details or accounting metadata wrong: %+v", body)
	}
	resp.CollectedAt = now.Add(time.Minute)
	cached := historyGET(handler, "/", first.Header().Get("ETag"), "")
	if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 || cached.Header().Get("X-Hive-Collected-At") != resp.CollectedAt.Format(time.RFC3339Nano) {
		t.Fatalf("collection time invalidated identical data: %d", cached.Code)
	}
	filtered := historyGET(handler, fmt.Sprintf("/?since=%d&max_points=3", now.Add(-10*time.Second).UnixMilli()), "", "")
	if err := json.Unmarshal(filtered.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.ByLaunch) != 3 || len(body.ByRef) != 3 || body.LaunchesTotal != 10 || body.RefsTotal != 10 || body.TotalTokens != 12345 {
		t.Fatal("launch filtering did not precede limiting or changed totals")
	}
	resp.ByLaunch[0].Tokens++
	if w := historyGET(handler, "/", first.Header().Get("ETag"), ""); w.Code != http.StatusOK {
		t.Fatal("launch spend update did not invalidate ETag")
	}
	if len(resp.ByLaunch) != 600 || len(resp.ByRef) != 600 {
		t.Fatal("bounded response mutated collector snapshot")
	}
}
