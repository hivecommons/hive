package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The public knowledge URL is what the owner copies and hands out, so it must
// never be the Go API's internal address that the Node proxy forwards to.
func TestPublicKnowledgeURLIsTheAddressReadersCanReach(t *testing.T) {
	cases := []struct {
		name      string
		publicURL string
		host      string
		headers   map[string]string
		want      string
	}{
		{
			name:      "public_url wins over every header",
			publicURL: "https://hive.example.org/",
			host:      "127.0.0.1:3002",
			headers:   map[string]string{"X-Forwarded-Host": "other.example", "Referer": "http://hive.lan:3001/"},
			want:      "https://hive.example.org/mcp/knowledge",
		},
		{
			name:    "X-Forwarded-Host before Host",
			host:    "127.0.0.1:3002",
			headers: map[string]string{"X-Forwarded-Host": "hive.example.org", "X-Forwarded-Proto": "https"},
			want:    "https://hive.example.org/mcp/knowledge",
		},
		{
			name:    "first X-Forwarded-Host of a list",
			host:    "10.0.0.5:3002",
			headers: map[string]string{"X-Forwarded-Host": "hive.example.org, inner.svc"},
			want:    "http://hive.example.org/mcp/knowledge",
		},
		{
			name:    "browser through the Node proxy",
			host:    "127.0.0.1:3002",
			headers: map[string]string{"Referer": "http://hive.lan:3001/"},
			want:    "",
		},
		{
			name:    "browser through the Node proxy on the same machine",
			host:    "127.0.0.1:3002",
			headers: map[string]string{"Referer": "http://127.0.0.1:3001/"},
			want:    "",
		},
		{
			name: "loopback Host with no page address",
			host: "localhost:3002",
			want: "",
		},
		{
			name:    "IPv6 loopback through the proxy",
			host:    "[::1]:3002",
			headers: map[string]string{"Origin": "http://hive.lan:3001"},
			want:    "",
		},
		{
			name: "reached directly by name",
			host: "hive.lan:3002",
			want: "http://hive.lan:3002/mcp/knowledge",
		},
		{
			name:    "reached directly on loopback",
			host:    "127.0.0.1:3002",
			headers: map[string]string{"Referer": "http://127.0.0.1:3002/"},
			want:    "http://127.0.0.1:3002/mcp/knowledge",
		},
		{
			name:    "reached directly on localhost, PUT carries Origin",
			host:    "localhost:3002",
			headers: map[string]string{"Origin": "http://localhost:3002"},
			want:    "http://localhost:3002/mcp/knowledge",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := publicKnowledgeServer(t)
			s.deps.Config.Dashboard.PublicURL = tc.publicURL
			req := httptest.NewRequest(http.MethodGet, "/api/knowledge/public", nil)
			req.Host = tc.host
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /api/knowledge/public = %d body=%q", rec.Code, rec.Body.String())
			}
			var got publicKnowledgeSettingResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.URL != tc.want {
				t.Fatalf("url = %q, want %q", got.URL, tc.want)
			}
			if got.Endpoint != publicKnowledgeMCPPath {
				t.Fatalf("endpoint = %q, want %q", got.Endpoint, publicKnowledgeMCPPath)
			}
		})
	}
}
