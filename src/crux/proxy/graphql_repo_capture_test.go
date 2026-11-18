package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

func TestGraphQLRepoCapturePublishesBeforeFinalBytes(t *testing.T) {
	for _, length := range []int64{3, -1} {
		observed := false
		capture := &graphQLRepoCapture{
			ReadCloser: io.NopCloser(strings.NewReader("abc")),
			length:     length,
			observe: func(body []byte) {
				if string(body) != "abc" {
					t.Errorf("observed %q", body)
				}
				observed = true
			},
		}
		if length == 3 {
			// HTTP relays with Content-Length may never perform an EOF read.
			body := make([]byte, 3)
			if _, err := io.ReadFull(capture, body); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := io.Copy(io.Discard, capture); err != nil {
				t.Fatal(err)
			}
		}
		if !observed {
			t.Fatalf("length=%d: response completed before cache publication", length)
		}
	}
}

func TestGraphQLRepoResponseBadEncodingAndGzipLimit(t *testing.T) {
	lookup := scopeGraphQLBody(`query { repository(owner:"acme",name:"widgets"){id} }`, nil)
	for _, tc := range []struct {
		encoding string
		body     []byte
	}{
		{"gzip", []byte("broken")},
		{"br", []byte(`{"data":{"repository":{"id":"R"}}}`)},
		{"gzip", gzipScopeResponse(t, []byte(strings.Repeat(" ", graphQLRepoResponseLimit)+`{"data":{"repository":{"id":"R"}}}`))},
	} {
		p := newTestProxy()
		p.observeGraphQLRepoResponse("schema", lookup, tc.body, tc.encoding)
		if p.graphQLRepos.get("schema", "R") != "" {
			t.Fatal("invalid/oversized response seeded cache")
		}
	}
}

func gzipScopeResponse(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
