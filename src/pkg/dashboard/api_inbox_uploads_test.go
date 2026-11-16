package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
)

type uploadFile struct {
	name string
	data string
}

func newUploadTestServer(t *testing.T, up config.InboxUploads) (*Server, *inbox.Service) {
	t.Helper()
	s, deps := apiServer(t)
	base := t.TempDir()
	deps.Config.Inbox = config.InboxConfig{Uploads: up}
	svc := inbox.NewService(deps.Config.Inbox, inbox.NewStore(filepath.Join(base, "_inbox.json")), nil, nil, nil)
	svc.EnableUploads(&inbox.UploadSource{
		Store:         inbox.NewUploadStore(filepath.Join(base, "connector-state", "uploads")),
		ConnectorsDir: filepath.Join(base, "connectors"),
	})
	deps.Inbox = svc
	return s, svc
}

func postUpload(s *Server, role string, verified bool, files []uploadFile, hdr map[string]string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range files {
		w, _ := mw.CreateFormFile("file", f.name)
		_, _ = w.Write([]byte(f.data))
	}
	_ = mw.WriteField("note", "ignored")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/inbox/uploads", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
		req.Header.Set("X-Hive-User", role+"@example.com")
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeUploadResponse(t *testing.T, rec *httptest.ResponseRecorder) InboxUploadResponse {
	t.Helper()
	var resp InboxUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %d %s: %v", rec.Code, rec.Body.String(), err)
	}
	return resp
}

func TestInboxUploadRoleMatrix(t *testing.T) {
	s, svc := newUploadTestServer(t, config.InboxUploads{})
	md := []uploadFile{{"notes.md", "# Notes\n\nhello"}}
	for _, tc := range []struct {
		role     string
		verified bool
	}{{"", false}, {"read-only", false}, {"read", false}, {"owner", false}} {
		if rec := postUpload(s, tc.role, tc.verified, md, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("role %q verified=%v = %d, want 403", tc.role, tc.verified, rec.Code)
		}
	}
	if rec := postUpload(s, "owner", true, md, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site = %d", rec.Code)
	}
	for _, tc := range []struct {
		role     string
		verified bool
	}{{"read-write", false}, {"owner", true}} {
		rec := postUpload(s, tc.role, tc.verified, md, map[string]string{"Sec-Fetch-Site": "same-origin"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("role %s = %d %s", tc.role, rec.Code, rec.Body.String())
		}
		res := decodeUploadResponse(t, rec).Results
		if len(res) != 1 || !res[0].OK || res[0].Source != inbox.UploadType || res[0].Kind != inbox.UploadKindMarkdown || res[0].Title != "Notes" {
			t.Fatalf("results = %+v", res)
		}
	}
	items, err := svc.Items(t.Context())
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %+v, %v", items, err)
	}
	added := map[string]bool{}
	for _, it := range items {
		added[it.AddedBy] = true
	}
	if !added["read-write@example.com"] || !added["owner@example.com"] {
		t.Fatalf("added_by = %v", added)
	}
	var audited []AuditEntry
	for _, e := range s.audit.Recent(50) {
		if e.Action == "inbox_upload" {
			audited = append(audited, e)
		}
	}
	if len(audited) != 2 || !strings.Contains(audited[0].Detail, "outcome=added") || audited[0].User == "" {
		t.Fatalf("audit = %+v", audited)
	}
	for _, e := range audited {
		if strings.Contains(e.Detail, "hello") {
			t.Fatalf("audit leaks content: %q", e.Detail)
		}
	}

	// The listing carries the limits the drop zone needs.
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/inbox", nil))
	var list InboxListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || !list.Settings.Uploads.Enabled || list.Settings.Uploads.MaxFileBytes != config.DefaultInboxUploadsMaxFileBytes {
		t.Fatalf("listing uploads settings = %+v, %v", list.Settings.Uploads, err)
	}
	for _, it := range list.Items {
		if it.Origin != "upload · notes.md" {
			t.Fatalf("origin = %q", it.Origin)
		}
	}
}

func TestInboxUploadPerFileErrors(t *testing.T) {
	s, _ := newUploadTestServer(t, config.InboxUploads{MaxFileBytes: 64, MaxFilesPerDrop: 4})
	cases := []struct {
		name string
		file uploadFile
		code int
		want string
	}{
		{"too large", uploadFile{"big.txt", strings.Repeat("a", 65)}, http.StatusRequestEntityTooLarge, inbox.UploadErrTooLarge},
		{"disguised exe", uploadFile{"invoice.txt", "MZ\x90\x00\x03\x00"}, http.StatusUnsupportedMediaType, inbox.UploadErrUnsupported},
		{"legacy doc", uploadFile{"letter.doc", "words"}, http.StatusUnsupportedMediaType, inbox.UploadErrUnsupported},
		{"malformed json", uploadFile{"d.json", "{\"a\": [1,"}, http.StatusUnprocessableEntity, inbox.UploadErrParse},
		{"empty", uploadFile{"e.md", ""}, http.StatusBadRequest, inbox.UploadErrEmpty},
	}
	for _, tc := range cases {
		rec := postUpload(s, "owner", true, []uploadFile{tc.file}, nil)
		if rec.Code != tc.code {
			t.Fatalf("%s = %d %s", tc.name, rec.Code, rec.Body.String())
		}
		res := decodeUploadResponse(t, rec).Results
		if len(res) != 1 || res[0].OK || res[0].Code != tc.want || res[0].Error == "" {
			t.Fatalf("%s results = %+v", tc.name, res)
		}
	}
	// A mixed drop answers 200 with one result per file; files past the cap
	// are refused one by one.
	files := []uploadFile{{"a.md", "# A"}, {"b.exe", "x"}, {"c.txt", "c"}, {"d.txt", "d"}, {"e.txt", "e"}}
	rec := postUpload(s, "owner", true, files, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("mixed = %d %s", rec.Code, rec.Body.String())
	}
	res := decodeUploadResponse(t, rec).Results
	if len(res) != 5 || !res[0].OK || res[1].Code != inbox.UploadErrUnsupported || !res[2].OK || !res[3].OK || res[4].Code != inbox.UploadErrTooMany {
		t.Fatalf("mixed results = %+v", res)
	}
	refused := 0
	for _, e := range s.audit.Recent(50) {
		if e.Action == "inbox_upload" && strings.Contains(e.Detail, "outcome=refused") {
			refused++
		}
	}
	if refused != len(cases)+2 {
		t.Fatalf("refused audits = %d", refused)
	}
}

func TestInboxUploadRequestErrors(t *testing.T) {
	s, _ := newUploadTestServer(t, config.InboxUploads{})
	if rec := postUpload(s, "owner", true, nil, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("no files = %d", rec.Code)
	}
	if rec := doPostRole(s, "/api/inbox/uploads", "owner", true, map[string]string{"a": "b"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("json body = %d", rec.Code)
	}
	// No upload store: 404.
	plain, deps := apiServer(t)
	deps.Inbox = inbox.NewService(config.InboxConfig{}, inbox.NewStore(""), nil, nil, nil)
	if rec := postUpload(plain, "owner", true, []uploadFile{{"a.md", "a"}}, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("no store = %d", rec.Code)
	}
	for code, want := range map[string]int{inbox.UploadErrQuota: http.StatusInsufficientStorage, inbox.UploadErrTooMany: http.StatusBadRequest, inbox.UploadErrInternal: http.StatusInternalServerError} {
		if got := inboxUploadStatus(code); got != want {
			t.Errorf("status(%s) = %d, want %d", code, got, want)
		}
	}
}

func TestInboxUploadURL(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<title>Linked page</title><p>linked text</p>")
	}))
	t.Cleanup(site.Close)
	s, svc := newUploadTestServer(t, config.InboxUploads{})
	svc.Uploads().Fetcher = &inbox.LinkFetcher{
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, site.Listener.Addr().String())
		},
		Proxy: func(string) *url.URL { return nil },
	}
	path := "/api/inbox/uploads/url"
	body := map[string]string{"url": "https://docs.example/page"}
	for _, role := range []string{"", "read-only", "read"} {
		if rec := doPostRole(s, path, role, false, body); rec.Code != http.StatusForbidden {
			t.Fatalf("role %q = %d", role, rec.Code)
		}
	}
	// The test site speaks plain http.
	body["url"] = "http://docs.example/page"
	rec := doPostRole(s, path, "read-write", false, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("link = %d %s", rec.Code, rec.Body.String())
	}
	res := decodeUploadResponse(t, rec).Results
	if len(res) != 1 || !res[0].OK || res[0].Kind != inbox.UploadKindHTML || res[0].Title != "Linked page" || res[0].Filename != "docs.example" {
		t.Fatalf("results = %+v", res)
	}
	for raw, want := range map[string]int{
		"http://169.254.169.254/latest/meta-data/": http.StatusBadRequest,
		"http://user:pw@docs.example/":             http.StatusBadRequest,
		"ftp://docs.example/x":                     http.StatusBadRequest,
	} {
		rec := doPostRole(s, path, "owner", true, map[string]string{"url": raw})
		if rec.Code != want {
			t.Fatalf("%s = %d %s", raw, rec.Code, rec.Body.String())
		}
		if r := decodeUploadResponse(t, rec).Results; len(r) != 1 || r[0].Code != inbox.UploadErrLinkRefused {
			t.Fatalf("%s results = %+v", raw, r)
		}
	}
	if rec := doPostRole(s, path, "owner", true, map[string]string{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("no url = %d", rec.Code)
	}
	var added, refused int
	for _, e := range s.audit.Recent(50) {
		if e.Action != "inbox_upload" || !strings.Contains(e.Detail, "origin=link") {
			continue
		}
		if strings.Contains(e.Detail, "pw") || strings.Contains(e.Detail, "meta-data") {
			t.Fatalf("audit leaks the URL: %q", e.Detail)
		}
		if strings.Contains(e.Detail, "outcome=added") {
			added++
		} else {
			refused++
		}
	}
	if added != 1 || refused != 3 {
		t.Fatalf("audits added=%d refused=%d", added, refused)
	}
	if inboxUploadStatus(inbox.UploadErrFetch) != http.StatusBadGateway {
		t.Fatal("fetch_failed must be 502")
	}
}

func TestInboxUploadAudioStreamsAndPlays(t *testing.T) {
	s, svc := newUploadTestServer(t, config.InboxUploads{})
	audio := "ID3\x04\x00\x00\x00\x00\x00\x00" + strings.Repeat("a", 4096)
	rec := postUpload(s, "read-write", false, []uploadFile{{"standup.mp3", audio}}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("audio = %d %s", rec.Code, rec.Body.String())
	}
	res := decodeUploadResponse(t, rec).Results
	if len(res) != 1 || res[0].Kind != inbox.UploadKindAudio {
		t.Fatalf("results = %+v", res)
	}
	// The generic same-origin stream serves it from disk.
	req := httptest.NewRequest(http.MethodGet, "/api/inbox/upload/"+res[0].ItemID+"/audio/stream", nil)
	req.Header.Set("X-Hive-Role", "read-write")
	play := httptest.NewRecorder()
	s.mux.ServeHTTP(play, req)
	if play.Code != http.StatusOK || play.Body.Len() != len(audio) || !strings.HasPrefix(play.Header().Get("Content-Type"), "audio/mpeg") {
		t.Fatalf("stream = %d %q %d", play.Code, play.Header().Get("Content-Type"), play.Body.Len())
	}
	if rec := postUpload(s, "owner", true, []uploadFile{{"fake.mp3", "just text"}}, nil); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("fake mp3 = %d", rec.Code)
	}
	items, _ := svc.Items(t.Context())
	if len(items) != 1 || !items[0].Pending {
		t.Fatalf("items = %+v", items)
	}
	if st := svc.Settings().Uploads; st.MaxMediaBytes != config.DefaultInboxRecordingMaxBytes {
		t.Fatalf("settings = %+v", st)
	}
}
