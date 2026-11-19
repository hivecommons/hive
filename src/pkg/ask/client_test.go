package ask

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testProjectID     = "proj-0000-secret"
	testIntegrationID = "integ-1111-secret"
	testAPIKey        = "kapa-key-2222-secret"
)

// contractResponse is the vendor's documented non-streamed 200 body
// (POST /query/v1/projects/{project_id}/chat/): answer, thread_id,
// question_answer_id, is_uncertain and relevant_sources with source_url, title
// and contains_internal_data. It pins the shape the client decodes.
const contractResponse = `{
  "answer": "Open Settings, then Features.",
  "thread_id": "vendor-thread-1",
  "question_answer_id": "qa-1",
  "is_uncertain": false,
  "relevant_sources": [
    {"source_url": "https://docs.hivecommons.dev/docs/dashboard", "title": "Dashboard", "contains_internal_data": false}
  ]
}`

type recorded struct {
	path   string
	method string
	key    string
	ctype  string
	body   []byte
}

func fakeVendor(t *testing.T, status int, body string) (*httptest.Server, *[]recorded) {
	t.Helper()
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, recorded{r.URL.Path, r.Method, r.Header.Get("X-API-KEY"), r.Header.Get("Content-Type"), b})
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestClientChatContractAndExactRequestBody(t *testing.T) {
	srv, got := fakeVendor(t, 200, contractResponse)
	c := NewClient(srv.URL, testProjectID, testIntegrationID, testAPIKey, 5*time.Second)
	resp, err := c.Chat(context.Background(), "where is the Features toggle?", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Answer != "Open Settings, then Features." || resp.ThreadID != "vendor-thread-1" ||
		resp.QuestionAnswerID != "qa-1" || resp.IsUncertain || len(resp.RelevantSources) != 1 ||
		resp.RelevantSources[0].SourceURL != "https://docs.hivecommons.dev/docs/dashboard" ||
		resp.RelevantSources[0].Title != "Dashboard" || resp.RelevantSources[0].ContainsInternalData {
		t.Fatalf("response not decoded as the pinned shape: %+v", resp)
	}
	if len(*got) != 1 {
		t.Fatalf("requests = %d", len(*got))
	}
	r := (*got)[0]
	if r.method != "POST" || r.path != "/query/v1/projects/"+testProjectID+"/chat/" {
		t.Fatalf("unexpected request %s %s", r.method, r.path)
	}
	if r.key != testAPIKey || r.ctype != "application/json" {
		t.Fatalf("headers: key=%q type=%q", r.key, r.ctype)
	}
	// The body holds exactly the two fields in the design's "What leaves the
	// deployment" table that belong in the JSON body: no user object, no
	// metadata, no origin URL.
	var fields map[string]any
	if err := json.Unmarshal(r.body, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["integration_id"] != testIntegrationID || fields["query"] != "where is the Features toggle?" {
		t.Fatalf("request body = %s", r.body)
	}
	if strings.Contains(string(r.body), testAPIKey) || strings.Contains(string(r.body), testProjectID) {
		t.Fatalf("body leaks the key or project id: %s", r.body)
	}
}

func TestClientContinuesVendorThread(t *testing.T) {
	srv, got := fakeVendor(t, 200, contractResponse)
	c := NewClient(srv.URL, testProjectID, testIntegrationID, testAPIKey, 5*time.Second)
	if _, err := c.Chat(context.Background(), "and then?", "vendor-thread-1"); err != nil {
		t.Fatal(err)
	}
	if p := (*got)[0].path; p != "/query/v1/threads/vendor-thread-1/chat/" {
		t.Fatalf("follow-up path = %s", p)
	}
}

func TestClientErrorsCarryNoValues(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{429, "slow down " + testAPIKey, ErrRateLimited},
		{500, testProjectID, ErrUnavailable},
		{503, "", ErrUnavailable},
		{401, "bad key " + testAPIKey, ErrRejected},
		{404, testProjectID, ErrRejected},
		{200, "not json " + testAPIKey, ErrBadResponse},
	}
	for _, tc := range cases {
		srv, _ := fakeVendor(t, tc.status, tc.body)
		c := NewClient(srv.URL, testProjectID, testIntegrationID, testAPIKey, 5*time.Second)
		_, err := c.Chat(context.Background(), "q", "")
		if !errors.Is(err, tc.want) {
			t.Fatalf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
		for _, secret := range []string{testAPIKey, testProjectID, testIntegrationID, srv.URL} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q carries %q", err, secret)
			}
		}
	}
}

func TestClientTransportFailureAndTimeoutCarryNoURL(t *testing.T) {
	srv, _ := fakeVendor(t, 200, contractResponse)
	url := srv.URL
	srv.Close() // closed port
	c := NewClient(url, testProjectID, testIntegrationID, testAPIKey, time.Second)
	_, err := c.Chat(context.Background(), "q", "")
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), testProjectID) || strings.Contains(err.Error(), url) {
		t.Fatalf("transport error = %v", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	c = NewClient(slow.URL, testProjectID, testIntegrationID, testAPIKey, 50*time.Millisecond)
	if _, err := c.Chat(context.Background(), "q", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestClientDoesNotFollowRedirectsWithTheKey(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("X-API-KEY") != ""
	}))
	defer other.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	c := NewClient(redir.URL, testProjectID, testIntegrationID, testAPIKey, time.Second)
	if _, err := c.Chat(context.Background(), "q", ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v", err)
	}
	if leaked {
		t.Fatal("the key followed a redirect")
	}
}
