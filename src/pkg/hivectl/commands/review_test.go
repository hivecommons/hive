package commands

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

var testEvidenceKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

func testEvidencePub() ed25519.PublicKey { return testEvidenceKey.Public().(ed25519.PublicKey) }

func testEvidenceBundle(t *testing.T, sign bool) *evidence.Bundle {
	t.Helper()
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion,
		ID:            evidence.BundleID("o/r", 7, "abc1234"),
		Repo:          "o/r",
		Number:        7,
		Author:        evidence.Author{Login: "alice", Kind: evidence.AuthorHuman},
		BaseSHA:       "base",
		HeadSHA:       "abc1234",
		CreatedAt:     at,
		UpdatedAt:     at,
	}
	var key ed25519.PrivateKey
	if sign {
		key = testEvidenceKey
	}
	if err := evidence.Seal(b, key); err != nil {
		t.Fatal(err)
	}
	return b
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testEvidenceZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTestFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEvidenceRef(t *testing.T) {
	for _, tc := range []struct {
		args []string
		repo string
		n    int
	}{
		{[]string{"hivecommons/hive#11061"}, "hivecommons/hive", 11061},
		{[]string{"https://github.com/hivecommons/hive/pull/11061"}, "hivecommons/hive", 11061},
		{[]string{"hivecommons/hive", "42"}, "hivecommons/hive", 42},
	} {
		repo, n, err := evidenceRef(tc.args)
		if err != nil || repo != tc.repo || n != tc.n {
			t.Fatalf("%v → %s#%d, %v", tc.args, repo, n, err)
		}
	}
	for _, bad := range []string{"hive#1", "o/r#0", "o/r", "o/r#x", "o/r/x#1", "/r#1"} {
		if _, _, err := evidenceRef([]string{bad}); ExitCode(err) != ExitUsage {
			t.Fatalf("%q: err %v exit %d, want usage", bad, err, ExitCode(err))
		}
	}
}

func TestReviewEvidenceFetch(t *testing.T) {
	bundle := mustJSON(t, testEvidenceBundle(t, true))
	var lastQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/review/evidence" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		lastQuery = r.URL.RawQuery
		q := r.URL.Query()
		switch q.Get("number") {
		case "404":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"ok":false,"error":"bundles for this PR were recorded but are no longer retained","status":"expired","expired":true}`)
		case "405":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"ok":false,"error":"no review evidence was recorded for this PR","status":"not_found","expired":false}`)
		case "403":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"ok":false,"error":"merger or owner access required"}`)
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `boom`)
		default:
			if q.Get("format") == "zip" {
				w.Header().Set("Content-Type", "application/zip")
				w.Write([]byte("PKzip"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(bundle)
		}
	}))
	defer server.Close()
	dir := t.TempDir()

	tests := []struct {
		name       string
		args       []string
		wantQuery  string
		wantStdout string
		wantStderr string
		wantFile   string
		wantBody   []byte
		wantExit   int
		wantErr    string
	}{
		{name: "latest to stdout", args: []string{"review", "evidence", "o/r#7"}, wantQuery: "number=7&repo=o%2Fr", wantStdout: string(bundle) + "\n"},
		{name: "head to file", args: []string{"review", "evidence", "o/r#7", "--head", " abc1234 ", "-o", filepath.Join(dir, "b.json")},
			wantQuery: "head=abc1234&number=7&repo=o%2Fr", wantFile: filepath.Join(dir, "b.json"), wantBody: bundle, wantStderr: "wrote "},
		{name: "dash is stdout", args: []string{"review", "evidence", "o/r", "7", "--output", "-"}, wantStdout: string(bundle) + "\n"},
		{name: "zip to file", args: []string{"review", "evidence", "o/r#7", "--zip", "-o", filepath.Join(dir, "b.zip")},
			wantQuery: "format=zip&number=7&repo=o%2Fr", wantFile: filepath.Join(dir, "b.zip"), wantBody: []byte("PKzip")},
		{name: "zip needs a file", args: []string{"review", "evidence", "o/r#7", "--zip"}, wantExit: ExitUsage, wantErr: "pass -o FILE"},
		{name: "bad ref", args: []string{"review", "evidence", "nope"}, wantExit: ExitUsage},
		{name: "too many args", args: []string{"review", "evidence", "o/r", "7", "x"}, wantExit: ExitUsage},
		{name: "expired", args: []string{"review", "evidence", "o/r#404"}, wantExit: ExitAPI, wantErr: "evidence expired: bundles for this PR"},
		{name: "never recorded", args: []string{"review", "evidence", "o/r#405"}, wantExit: ExitAPI, wantErr: "no review evidence was recorded"},
		{name: "forbidden", args: []string{"review", "evidence", "o/r#403"}, wantExit: ExitAuth},
		{name: "server error", args: []string{"review", "evidence", "o/r#500"}, wantExit: ExitAPI, wantErr: "boom"},
		{name: "unwritable file", args: []string{"review", "evidence", "o/r#7", "-o", filepath.Join(dir, "missing", "b.json")}, wantExit: ExitFailure, wantErr: "writing"},
		{name: "bad server", args: []string{"--server", "://bad", "review", "evidence", "o/r#7"}, wantExit: ExitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lastQuery = ""
			stdout, stderr, err := execute(t, server, "", tt.args...)
			if got := ExitCode(err); got != tt.wantExit {
				t.Fatalf("exit %d (err %v), want %d", got, err, tt.wantExit)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if tt.wantQuery != "" && lastQuery != tt.wantQuery {
				t.Fatalf("query = %q, want %q", lastQuery, tt.wantQuery)
			}
			if tt.wantStdout != "" && stdout != tt.wantStdout {
				t.Fatalf("stdout = %q", stdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr, tt.wantStderr) {
				t.Fatalf("stderr = %q", stderr)
			}
			if tt.wantFile != "" {
				got, err := os.ReadFile(tt.wantFile)
				if err != nil || !bytes.Equal(got, tt.wantBody) {
					t.Fatalf("file = %q, %v", got, err)
				}
			}
		})
	}
}

func TestReviewCommandShowsHelp(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	stdout, _, err := execute(t, server, "", "review")
	if err != nil || !strings.Contains(stdout, "evidence") {
		t.Fatalf("review help = %q, %v", stdout, err)
	}
}

func TestReviewEvidenceVerify(t *testing.T) {
	signed := testEvidenceBundle(t, true)
	signedJSON := mustJSON(t, signed)
	pubHex := hex.EncodeToString(testEvidencePub())

	tampered := *signed
	tampered.BaseSHA = "evil"
	unsigned := testEvidenceBundle(t, false)
	otherKey := hex.EncodeToString(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize)).Public().(ed25519.PublicKey))

	var withKey map[string]any
	if err := json.Unmarshal(signedJSON, &withKey); err != nil {
		t.Fatal(err)
	}
	withKey["public_key"] = pubHex

	files := map[string]string{
		"signed":    writeTestFile(t, "signed.json", signedJSON),
		"carried":   writeTestFile(t, "carried.json", mustJSON(t, withKey)),
		"tampered":  writeTestFile(t, "tampered.json", mustJSON(t, &tampered)),
		"unsigned":  writeTestFile(t, "unsigned.json", mustJSON(t, unsigned)),
		"notjson":   writeTestFile(t, "bad.json", []byte("{nope")),
		"badzip":    writeTestFile(t, "bad.zip", []byte("PK\x03\x04truncated")),
		"pubB64":    writeTestFile(t, "op.pub", []byte(base64.StdEncoding.EncodeToString(testEvidencePub())+"\n")),
		"zip":       writeTestFile(t, "e.zip", testEvidenceZip(t, map[string][]byte{"o-r-7-abc1234/bundle.json": signedJSON, "o-r-7-abc1234/manifest.json": mustJSON(t, map[string]string{"public_key": pubHex}), "o-r-7-abc1234/verdicts.json": []byte("{}")})),
		"zipNoKey":  writeTestFile(t, "nokey.zip", testEvidenceZip(t, map[string][]byte{"bundle.json": signedJSON})),
		"zipNoBndl": writeTestFile(t, "nobundle.zip", testEvidenceZip(t, map[string][]byte{"manifest.json": []byte("{}")})),
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	tests := []struct {
		name       string
		args       []string
		stdin      string
		wantExit   int
		wantErr    string
		wantSource string
	}{
		{name: "hex flag", args: []string{files["signed"], "--pubkey", pubHex}, wantSource: "flag"},
		{name: "key file base64", args: []string{files["signed"], "--pubkey", files["pubB64"]}, wantSource: "flag"},
		{name: "key carried in json", args: []string{files["carried"]}, wantSource: "embedded"},
		{name: "zip manifest key", args: []string{files["zip"]}, wantSource: "embedded"},
		{name: "stdin", args: []string{"-", "--pubkey", pubHex}, stdin: string(signedJSON), wantSource: "flag"},
		{name: "zip without key", args: []string{files["zipNoKey"]}, wantExit: ExitUsage, wantErr: "no public key"},
		{name: "no key", args: []string{files["signed"]}, wantExit: ExitUsage, wantErr: "no public key"},
		{name: "malformed key", args: []string{files["signed"], "--pubkey", "zz"}, wantExit: ExitUsage, wantErr: "public key must be"},
		{name: "wrong key", args: []string{files["signed"], "--pubkey", otherKey}, wantExit: ExitFailure, wantErr: "signature does not verify"},
		{name: "tampered", args: []string{files["tampered"], "--pubkey", pubHex}, wantExit: ExitFailure, wantErr: "hash mismatch"},
		{name: "unsigned", args: []string{files["unsigned"]}, wantExit: ExitFailure, wantErr: "unsigned"},
		{name: "not json", args: []string{files["notjson"]}, wantExit: ExitFailure, wantErr: "parsing bundle"},
		{name: "corrupt zip", args: []string{files["badzip"]}, wantExit: ExitFailure, wantErr: "reading evidence archive"},
		{name: "zip without bundle", args: []string{files["zipNoBndl"]}, wantExit: ExitFailure, wantErr: "no bundle.json"},
		{name: "missing file", args: []string{filepath.Join(t.TempDir(), "absent.json")}, wantExit: ExitFailure, wantErr: "reading"},
		{name: "needs a file", args: []string{}, wantExit: ExitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-o", "json", "review", "evidence", "verify"}, tt.args...)
			stdout, _, err := execute(t, server, tt.stdin, args...)
			if got := ExitCode(err); got != tt.wantExit {
				t.Fatalf("exit %d (err %v), want %d; stdout %s", got, err, tt.wantExit, stdout)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if tt.wantExit != ExitSuccess {
				return
			}
			var result map[string]any
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("stdout %q: %v", stdout, err)
			}
			if result["verified"] != true || result["hash_ok"] != true || result["public_key_source"] != tt.wantSource || result["public_key"] != pubHex || result["id"] != signed.ID {
				t.Fatalf("result = %v", result)
			}
		})
	}
}

func TestReviewEvidenceVerifyPrintError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	path := writeTestFile(t, "signed.json", mustJSON(t, testEvidenceBundle(t, true)))
	_, _, err := execute(t, server, "", "-o", "xml", "review", "evidence", "verify", path, "--pubkey", hex.EncodeToString(testEvidencePub()))
	if err == nil || !strings.Contains(err.Error(), "unsupported output format") {
		t.Fatalf("err = %v, want unsupported output format", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReadEvidenceInputStdinError(t *testing.T) {
	if _, err := readEvidenceInput(failingReader{}, "-"); err == nil || !strings.Contains(err.Error(), "reading stdin") {
		t.Fatalf("err = %v", err)
	}
}
