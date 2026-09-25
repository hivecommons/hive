package worksource

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type jiraTestIssue struct {
	Key      string
	Summary  string
	Priority string // empty = absent
	Status   string
	Assignee string
	Reporter string
	Labels   []string
}

func jiraIssueJSON(it jiraTestIssue) map[string]any {
	fields := map[string]any{
		"summary": it.Summary,
		"labels":  it.Labels,
		"created": "2024-01-02T03:04:05.000+0000",
		"updated": "2024-01-03T03:04:05.000+0000",
	}
	if it.Status != "" {
		fields["status"] = map[string]any{"name": it.Status}
	}
	if it.Priority != "" {
		fields["priority"] = map[string]any{"name": it.Priority}
	}
	if it.Assignee != "" {
		fields["assignee"] = map[string]any{"displayName": it.Assignee}
	}
	if it.Reporter != "" {
		fields["reporter"] = map[string]any{"displayName": it.Reporter}
	}
	return map[string]any{"key": it.Key, "fields": fields}
}

// newJiraServer serves a fixed set of issues from the Jira Cloud enhanced search
// endpoint (POST /rest/api/3/search/jql) with nextPageToken pagination and
// records requests via the callback.
func newJiraServer(t *testing.T, issues []jiraTestIssue, onReq func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body struct {
			JQL           string   `json:"jql"`
			Fields        []string `json:"fields"`
			MaxResults    int      `json:"maxResults"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode search body: %v", err)
		}
		// Re-expose jql on the request so callbacks can inspect it uniformly.
		r.URL.RawQuery = url.Values{"jql": {body.JQL}}.Encode()
		if onReq != nil {
			onReq(r)
		}
		startAt := 0
		if body.NextPageToken != "" {
			fmt.Sscanf(body.NextPageToken, "%d", &startAt)
		}
		end := startAt + jiraMaxResults
		if end > len(issues) {
			end = len(issues)
		}
		var page []map[string]any
		if startAt < len(issues) {
			for _, it := range issues[startAt:end] {
				page = append(page, jiraIssueJSON(it))
			}
		}
		resp := map[string]any{"issues": page}
		if end < len(issues) {
			resp["nextPageToken"] = fmt.Sprintf("%d", end)
			resp["isLast"] = false
		} else {
			resp["isLast"] = true
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestJiraBasicEnumeration(t *testing.T) {
	srv := newJiraServer(t, []jiraTestIssue{
		{Key: "ENG-1", Summary: "First", Status: "Todo", Reporter: "Alice", Assignee: "Bob", Priority: "High"},
		{Key: "ENG-2", Summary: "Second", Status: "Backlog", Reporter: "Carol"},
	}, nil)
	defer srv.Close()

	src := NewJiraSource(JiraConfig{
		BaseURL: srv.URL, Email: "bot@x.com", APIToken: "tok",
		ProjectKeys: []string{"ENG"}, Repo: "my-org/my-repo",
	})
	if src.SourceType() != "jira" {
		t.Errorf("SourceType = %q, want jira", src.SourceType())
	}
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d issues, want 2", len(got))
	}
	first := got[0]
	if first.ExternalID != "ENG-1" || first.Number != 0 || first.Title != "First" ||
		first.Author != "Alice" || first.State != "Todo" || first.Repo != "my-org/my-repo" ||
		first.SourceType != "jira" || first.Priority != "high" {
		t.Errorf("unexpected first issue: %+v", first)
	}
	if len(first.Assignees) != 1 || first.Assignees[0] != "Bob" {
		t.Errorf("assignees = %v, want [Bob]", first.Assignees)
	}
	if got[1].Assignees != nil {
		t.Errorf("unassigned issue should have nil assignees, got %v", got[1].Assignees)
	}
	if first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Errorf("timestamps not parsed: %+v", first)
	}
	if first.URL != srv.URL+"/browse/ENG-1" {
		t.Errorf("URL = %q", first.URL)
	}
}

func TestJiraPagination(t *testing.T) {
	var issues []jiraTestIssue
	for i := 1; i <= 150; i++ {
		issues = append(issues, jiraTestIssue{Key: fmt.Sprintf("ENG-%d", i), Summary: "x", Status: "Todo"})
	}
	var requests int
	srv := newJiraServer(t, issues, func(*http.Request) { requests++ })
	defer srv.Close()

	src := NewJiraSource(JiraConfig{BaseURL: srv.URL, ProjectKeys: []string{"ENG"}, Repo: "o/r"})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 150 {
		t.Errorf("got %d issues, want 150", len(got))
	}
	if requests != 2 {
		t.Errorf("made %d requests, want 2", requests)
	}
	if got[100].ExternalID != "ENG-101" {
		t.Errorf("issue 100 = %q, want ENG-101", got[100].ExternalID)
	}
}

func TestJiraDefaultJQL(t *testing.T) {
	var gotJQL string
	srv := newJiraServer(t, nil, func(r *http.Request) { gotJQL = r.URL.Query().Get("jql") })
	defer srv.Close()

	src := NewJiraSource(JiraConfig{BaseURL: srv.URL, ProjectKeys: []string{"ENG", "OPS"}, Repo: "o/r"})
	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "project in (ENG,OPS) AND statusCategory != Done AND issuetype != Epic"
	if gotJQL != want {
		t.Errorf("jql = %q, want %q", gotJQL, want)
	}
}

func TestJiraCustomJQL(t *testing.T) {
	var gotJQL string
	srv := newJiraServer(t, nil, func(r *http.Request) { gotJQL = r.URL.Query().Get("jql") })
	defer srv.Close()

	custom := "status in (Todo, Backlog) AND assignee is EMPTY"
	src := NewJiraSource(JiraConfig{BaseURL: srv.URL, ProjectKeys: []string{"ENG"}, JQL: custom, Repo: "o/r"})
	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotJQL != custom {
		t.Errorf("jql = %q, want %q", gotJQL, custom)
	}
}

func TestJiraPriorityMapping(t *testing.T) {
	srv := newJiraServer(t, []jiraTestIssue{
		{Key: "ENG-1", Summary: "a", Priority: "Highest"},
		{Key: "ENG-2", Summary: "b", Priority: "Medium"},
		{Key: "ENG-3", Summary: "c"}, // absent
	}, nil)
	defer srv.Close()

	src := NewJiraSource(JiraConfig{BaseURL: srv.URL, ProjectKeys: []string{"ENG"}, Repo: "o/r"})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"urgent", "medium", "none"}
	for i, w := range want {
		if got[i].Priority != w {
			t.Errorf("issue %s priority = %q, want %q", got[i].ExternalID, got[i].Priority, w)
		}
	}
}

func TestJiraHoldLabelFiltering(t *testing.T) {
	srv := newJiraServer(t, []jiraTestIssue{
		{Key: "ENG-1", Summary: "held", Labels: []string{"hold"}},
		{Key: "ENG-2", Summary: "blocked", Labels: []string{"other", "blocked"}},
		{Key: "ENG-3", Summary: "free", Labels: []string{"bug"}},
	}, nil)
	defer srv.Close()

	src := NewJiraSource(JiraConfig{
		BaseURL: srv.URL, ProjectKeys: []string{"ENG"}, Repo: "o/r",
		HoldLabels: []string{"hold", "blocked"},
	})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ExternalID != "ENG-3" {
		t.Errorf("got %+v, want only ENG-3", got)
	}
}

func TestJiraAuthHeader(t *testing.T) {
	var gotAuth string
	srv := newJiraServer(t, nil, func(r *http.Request) { gotAuth = r.Header.Get("Authorization") })
	defer srv.Close()

	src := NewJiraSource(JiraConfig{
		BaseURL: srv.URL, Email: "bot@myorg.com", APIToken: "secret-token",
		ProjectKeys: []string{"ENG"}, Repo: "o/r",
	})
	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@myorg.com:secret-token"))
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

func TestJiraDataCenterSearchPaginationAuthAndContextPath(t *testing.T) {
	const jiraDataCenterTestIssueCount = 101
	var paths []string
	var authHeaders []string
	var users []jiraUser
	for i := 1; i <= jiraDataCenterTestIssueCount; i++ {
		users = append(users, jiraUser{Name: fmt.Sprintf("user%d", i), Key: fmt.Sprintf("JIRAUSER%d", i)})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		if r.URL.Path != "/jira/rest/api/2/search" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		startAt := 0
		fmt.Sscanf(r.URL.Query().Get("startAt"), "%d", &startAt)
		end := startAt + jiraMaxResults
		if end > len(users) {
			end = len(users)
		}
		issues := make([]map[string]any, 0, end-startAt)
		for i := startAt; i < end; i++ {
			issues = append(issues, map[string]any{
				"key": fmt.Sprintf("ENG-%d", i+1),
				"fields": map[string]any{
					"summary":  "dc",
					"status":   map[string]any{"name": "To Do"},
					"reporter": users[i],
					"assignee": users[i],
					"created":  "2024-01-02T03:04:05.000+0000",
					"updated":  "2024-01-03T03:04:05.000+0000",
				},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"startAt":    startAt,
			"maxResults": jiraMaxResults,
			"total":      len(users),
			"issues":     issues,
		})
	}))
	defer srv.Close()

	src := NewJiraSource(JiraConfig{
		Deployment:  jiraDeploymentDataCenter,
		BaseURL:     srv.URL + "/jira",
		APIToken:    "pat-secret",
		ProjectKeys: []string{"ENG"},
		Repo:        "o/r",
	})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(users) {
		t.Fatalf("got %d issues, want %d", len(got), len(users))
	}
	if len(paths) != 2 || paths[0] != "/jira/rest/api/2/search" || paths[1] != "/jira/rest/api/2/search" {
		t.Fatalf("paths = %v, want two context-path v2 searches", paths)
	}
	for _, h := range authHeaders {
		if h != "Bearer pat-secret" {
			t.Fatalf("Authorization = %q, want bearer PAT", h)
		}
	}
	if got[0].Author != "user1" || got[0].Assignees[0] != "user1" {
		t.Errorf("Data Center identity = author %q assignees %v, want name", got[0].Author, got[0].Assignees)
	}
	if got[0].URL != srv.URL+"/jira/browse/ENG-1" {
		t.Errorf("URL = %q, want context-path browse URL", got[0].URL)
	}
}

func TestJiraDataCenterBasicAuthFetchCommentAndTransition(t *testing.T) {
	type requestRecord struct {
		Method string
		Path   string
		Auth   string
		Body   string
	}
	var records []requestRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		records = append(records, requestRecord{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
			Body:   string(body),
		})
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/2/issue/ENG-7":
			_ = json.NewEncoder(w).Encode(jiraIssueJSON(jiraTestIssue{Key: "ENG-7", Summary: "fetched", Status: "To Do"}))
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/2/issue/ENG-7/comment":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"10000"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/2/issue/ENG-7/transitions":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ws := NewJiraSource(JiraConfig{
		Deployment: jiraDeploymentDataCenter,
		BaseURL:    srv.URL,
		Username:   "bot",
		Password:   "pw",
	})
	src, ok := ws.(*jiraSource)
	if !ok {
		t.Fatalf("source type %T", ws)
	}
	issue, err := src.fetchIssue(context.Background(), "ENG-7")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Key != "ENG-7" || issue.Fields.Summary != "fetched" {
		t.Fatalf("issue = %+v", issue)
	}
	if err := src.addComment(context.Background(), "ENG-7", "plain wiki-ish *comment*"); err != nil {
		t.Fatal(err)
	}
	const transitionID = "31"
	if err := src.transitionIssue(context.Background(), "ENG-7", transitionID); err != nil {
		t.Fatal(err)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot:pw"))
	for _, r := range records {
		if r.Auth != wantAuth {
			t.Fatalf("%s %s auth = %q, want %q", r.Method, r.Path, r.Auth, wantAuth)
		}
	}
	if !strings.Contains(records[1].Body, `"body":"plain wiki-ish *comment*"`) {
		t.Errorf("comment body = %s, want plain Data Center string body", records[1].Body)
	}
	if !strings.Contains(records[2].Body, `"id":"31"`) {
		t.Errorf("transition body = %s, want transition id", records[2].Body)
	}
}

func TestJiraCloudCommentUsesADF(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	ws := NewJiraSource(JiraConfig{BaseURL: srv.URL, Email: "bot@example.com", APIToken: "tok"})
	src, ok := ws.(*jiraSource)
	if !ok {
		t.Fatalf("source type %T", ws)
	}
	if err := src.addComment(context.Background(), "ENG-1", "hello"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"doc"`, `"version":1`, `"text":"hello"`} {
		if !strings.Contains(body, want) {
			t.Errorf("cloud comment body %s missing %s", body, want)
		}
	}
}

func TestJiraDataCenterCustomCABundleForTLSServer(t *testing.T) {
	caPEM, serverCert, _, _ := jiraTestTLSMaterials(t)
	srv := newJiraTLSServer(t, serverCert, nil)
	defer srv.Close()

	src := NewJiraSource(JiraConfig{
		Deployment:  jiraDeploymentDataCenter,
		BaseURL:     srv.URL,
		ProjectKeys: []string{"ENG"},
		Repo:        "o/r",
	})
	if _, err := src.ListIssues(context.Background()); err == nil {
		t.Fatal("untrusted test CA should fail without ca_bundle")
	}

	trusted := NewJiraSource(JiraConfig{
		Deployment:  jiraDeploymentDataCenter,
		BaseURL:     srv.URL,
		CABundle:    caPEM,
		ProjectKeys: []string{"ENG"},
		Repo:        "o/r",
	})
	got, err := trusted.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("custom CA bundle should trust TLS server: %v", err)
	}
	if len(got) != 1 || got[0].ExternalID != "ENG-1" {
		t.Fatalf("issues = %+v, want ENG-1", got)
	}
}

func TestJiraDataCenterSkipTLSVerifyWarnsAndConnects(t *testing.T) {
	_, serverCert, _, _ := jiraTestTLSMaterials(t)
	srv := newJiraTLSServer(t, serverCert, nil)
	defer srv.Close()
	var logs bytes.Buffer

	src := NewJiraSource(JiraConfig{
		Deployment:         jiraDeploymentDataCenter,
		BaseURL:            srv.URL,
		InsecureSkipVerify: true,
		ProjectKeys:        []string{"ENG"},
		Repo:               "o/r",
		Logger:             slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatalf("skip TLS verify should connect to test server: %v", err)
	}
	if !strings.Contains(logs.String(), "TLS certificate verification is disabled") {
		t.Fatalf("warning log = %q, want skip-verify warning", logs.String())
	}
}

func TestJiraDataCenterBadCABundleRejected(t *testing.T) {
	err := ValidateJiraTLSConfig(JiraConfig{
		Deployment: jiraDeploymentDataCenter,
		CABundle:   "not pem",
	})
	if err == nil || !strings.Contains(err.Error(), "ca_bundle") || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("ValidateJiraTLSConfig bad CA = %v, want clear PEM error", err)
	}
}

func TestJiraDataCenterBadTLSConfigFailsBeforeNetwork(t *testing.T) {
	src := NewJiraSource(JiraConfig{
		Deployment:  jiraDeploymentDataCenter,
		BaseURL:     "https://jira.invalid",
		CABundle:    "not pem",
		ProjectKeys: []string{"ENG"},
	})
	if _, err := src.ListIssues(context.Background()); err == nil || !strings.Contains(err.Error(), "ca_bundle") {
		t.Fatalf("ListIssues with bad TLS config = %v, want local CA bundle error", err)
	}

	js := src.(*jiraSource)
	if err := js.addComment(context.Background(), "ENG-1", "blocked"); err == nil || !strings.Contains(err.Error(), "ca_bundle") {
		t.Fatalf("addComment with bad TLS config = %v, want local CA bundle error", err)
	}
}

func TestJiraDataCenterTLSClientCertificatePairRequired(t *testing.T) {
	caPEM, _, clientCertPEM, _ := jiraTestTLSMaterials(t)
	cases := []struct {
		name string
		cfg  JiraConfig
	}{
		{
			name: "missing client key",
			cfg: JiraConfig{
				Deployment: jiraDeploymentDataCenter,
				CABundle:   caPEM,
				ClientCert: clientCertPEM,
			},
		},
		{
			name: "missing client cert",
			cfg: JiraConfig{
				Deployment: jiraDeploymentDataCenter,
				CABundle:   caPEM,
				ClientKey:  "-----BEGIN PRIVATE KEY-----\n-----END PRIVATE KEY-----",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateJiraTLSConfig(tc.cfg); err == nil || !strings.Contains(err.Error(), "client_cert and client_key must be set together") {
				t.Fatalf("ValidateJiraTLSConfig = %v, want pair-required error", err)
			}
		})
	}
}

func TestJiraDataCenterTLSClientCertificateKeyMustMatch(t *testing.T) {
	caPEM, _, clientCertPEM, clientKeyPEM := jiraTestTLSMaterials(t)
	_, _, _, otherClientKeyPEM := jiraTestTLSMaterials(t)

	err := ValidateJiraTLSConfig(JiraConfig{
		Deployment: jiraDeploymentDataCenter,
		CABundle:   caPEM,
		ClientCert: clientCertPEM,
		ClientKey:  strings.Replace(clientKeyPEM, "RSA PRIVATE KEY", "PRIVATE KEY", 1),
	})
	if err == nil || !strings.Contains(err.Error(), "client_cert/client_key") {
		t.Fatalf("ValidateJiraTLSConfig malformed key = %v, want client key pair error", err)
	}

	err = ValidateJiraTLSConfig(JiraConfig{
		Deployment: jiraDeploymentDataCenter,
		CABundle:   caPEM,
		ClientCert: clientCertPEM,
		ClientKey:  otherClientKeyPEM,
	})
	if err == nil || !strings.Contains(err.Error(), "client_cert/client_key") {
		t.Fatalf("ValidateJiraTLSConfig mismatched key = %v, want client key pair error", err)
	}
}

func TestJiraCloudTLSSettingsIgnoredEvenIfMalformed(t *testing.T) {
	err := ValidateJiraTLSConfig(JiraConfig{
		Deployment: "cloud",
		CABundle:   "not a PEM bundle",
		ClientCert: "not a cert",
		ClientKey:  "not a key",
	})
	if err != nil {
		t.Fatalf("cloud TLS validation = %v, want nil because Data Center-only fields are ignored", err)
	}
}

func TestJiraDataCenterMTLSClientCertificate(t *testing.T) {
	caPEM, serverCert, clientCertPEM, clientKeyPEM := jiraTestTLSMaterials(t)
	caPool := x509.NewCertPool()
	if ok := caPool.AppendCertsFromPEM([]byte(caPEM)); !ok {
		t.Fatal("test CA did not parse")
	}
	srv := newJiraTLSServer(t, serverCert, caPool)
	defer srv.Close()

	src := NewJiraSource(JiraConfig{
		Deployment:  jiraDeploymentDataCenter,
		BaseURL:     srv.URL,
		CABundle:    caPEM,
		ClientCert:  clientCertPEM,
		ClientKey:   clientKeyPEM,
		ProjectKeys: []string{"ENG"},
		Repo:        "o/r",
	})
	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatalf("mTLS Jira client should connect: %v", err)
	}
}

func newJiraTLSServer(t *testing.T, cert tls.Certificate, clientCAs *x509.CertPool) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/2/search" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"startAt":    0,
			"maxResults": jiraMaxResults,
			"total":      1,
			"issues": []map[string]any{jiraIssueJSON(jiraTestIssue{
				Key:     "ENG-1",
				Summary: "TLS",
				Status:  "To Do",
			})},
		})
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	if clientCAs != nil {
		srv.TLS.ClientCAs = clientCAs
		srv.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	}
	srv.StartTLS()
	return srv
}

func jiraTestTLSMaterials(t *testing.T) (string, tls.Certificate, string, string) {
	t.Helper()
	const rsaKeyBits = 2048
	caKey, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "jira-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))

	serverKey, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	clientKey, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "jira-client"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	clientKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)})
	return caPEM, serverCert, string(clientCertPEM), string(clientKeyPEM)
}

func TestJiraTimestampUnmarshal(t *testing.T) {
	var ts jiraTimestamp
	if err := ts.UnmarshalJSON([]byte(`"2026-01-02T15:04:05.000-0700"`)); err != nil {
		t.Fatalf("jira format: %v", err)
	}
	if ts.Year() != 2026 {
		t.Errorf("year = %d, want 2026", ts.Year())
	}

	var ts2 jiraTimestamp
	if err := ts2.UnmarshalJSON([]byte(`"2026-01-02T15:04:05Z"`)); err != nil {
		t.Fatalf("RFC3339: %v", err)
	}

	var ts3 jiraTimestamp
	if err := ts3.UnmarshalJSON([]byte(`""`)); err != nil {
		t.Fatalf("empty string: %v", err)
	}
	if !ts3.IsZero() {
		t.Error("empty string should leave zero time")
	}

	var ts4 jiraTimestamp
	if err := ts4.UnmarshalJSON([]byte(`"not-a-timestamp"`)); err == nil {
		t.Error("expected error for unparseable timestamp")
	}

	var ts5 jiraTimestamp
	if err := ts5.UnmarshalJSON([]byte(`12345`)); err == nil {
		t.Error("expected error for non-string JSON")
	}
}

func TestJiraIssueNullAssignee(t *testing.T) {
	raw := `{"key":"ENG-1","fields":{"summary":"s","status":{"name":"To Do"},
		"priority":null,"assignee":null,"reporter":null,"labels":[],
		"created":"2026-01-02T15:04:05.000-0700","updated":"2026-01-02T15:04:05.000-0700"}}`
	var it jiraIssue
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if it.Fields.Assignee != nil || it.Fields.Priority != nil || it.Fields.Reporter != nil {
		t.Error("null fields should decode to nil pointers")
	}
}

func TestNormalizeJiraPriority(t *testing.T) {
	cases := map[string]string{
		"Highest": "urgent", "Critical": "urgent", "Blocker": "urgent", "P0": "urgent",
		"High": "high", "P1": "high",
		"Medium": "medium", "P2": "medium",
		"Low": "low", "Lowest": "low", "P3": "low", "P4": "low",
		"whatever": "none", "": "none",
	}
	for in, want := range cases {
		if got := normalizeJiraPriority(in); got != want {
			t.Errorf("normalizeJiraPriority(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJiraSearchPageErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"401", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"errorMessages":["unauthorized"]}`, http.StatusUnauthorized)
		}},
		{"500", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		{"non-json body", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "not json")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			src := NewJiraSource(JiraConfig{BaseURL: srv.URL, ProjectKeys: []string{"ENG"}, Repo: "o/r"})
			if _, err := src.ListIssues(context.Background()); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestJiraRequestFailure(t *testing.T) {
	src := NewJiraSource(JiraConfig{BaseURL: "http://127.0.0.1:1", ProjectKeys: []string{"ENG"}})
	if _, err := src.ListIssues(context.Background()); err == nil {
		t.Fatal("expected connection error, got nil")
	}
}
