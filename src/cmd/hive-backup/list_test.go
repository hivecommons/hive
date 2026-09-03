package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hubbackup"
)

// cmdList is the operator's incident-time view of what disaster-recovery
// archives actually exist. These tests drive it through the same re-exec
// harness as the other exit-code tests (runMainHelper), asserting both the
// fail-closed exit codes a CronJob alerts on and the newest-first listing an
// operator reads during a restore.

// testOCIKeyPEM returns a PKCS#1 RSA private key PEM acceptable to
// hubbackup.NewObjectStore's credential parsing.
func testOCIKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// listOCIEnv returns the OCI credential env a `list` subprocess needs, with
// requests redirected to the given endpoint (the hubbackup test seam).
func listOCIEnv(t *testing.T, endpoint string) map[string]string {
	t.Helper()
	return map[string]string{
		"OCI_TENANCY_OCID":         "ocid1.tenancy.oc1..test",
		"OCI_USER_OCID":            "ocid1.user.oc1..test",
		"OCI_FINGERPRINT":          "aa:bb:cc",
		"OCI_PRIVATE_KEY":          testOCIKeyPEM(t),
		"OCI_REGION":               "us-ashburn-1",
		hubbackup.EnvBucket:        "backups",
		"HIVE_BACKUP_OCI_ENDPOINT": endpoint,
	}
}

// listOCIMock serves namespace resolution plus a bucket listing that returns
// the given object names (in the order provided).
func listOCIMock(t *testing.T, names []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/n/" {
			json.NewEncoder(w).Encode("testnamespace")
			return
		}
		type obj struct {
			Name string `json:"name"`
		}
		var objs []obj
		for _, n := range names {
			objs = append(objs, obj{Name: n})
		}
		json.NewEncoder(w).Encode(map[string]any{"objects": objs})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Without OCI credentials the list command must fail with a nonzero exit code
// — never silently report an empty fleet of backups.
func TestMainListWithoutCredentialsExitsNonzero(t *testing.T) {
	code, out := runMainHelper(t, map[string]string{
		// Blank every credential so ambient CI env cannot leak in.
		"OCI_TENANCY_OCID": "",
		"OCI_USER_OCID":    "",
		"OCI_FINGERPRINT":  "",
		"OCI_PRIVATE_KEY":  "",
	}, "list")
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "list failed") {
		t.Errorf("credential-less list did not report failure:\n%s", out)
	}
}

// A bucket-listing failure (upstream 5xx) must exit nonzero: an operator
// checking archives during an incident must see the failure, not an empty
// listing they could mistake for "no backups exist".
func TestMainListUpstreamErrorExitsNonzero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/n/" {
			json.NewEncoder(w).Encode("testnamespace")
			return
		}
		http.Error(w, "object storage unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	code, out := runMainHelper(t, listOCIEnv(t, srv.URL), "list")
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "list failed") {
		t.Errorf("upstream-error list did not report failure:\n%s", out)
	}
}

// The happy path must print archive names newest first, include only
// hive-hub-backup-* objects, and exit zero.
func TestMainListPrintsArchivesNewestFirst(t *testing.T) {
	srv := listOCIMock(t, []string{
		"hive-hub-backup-20260101T000000Z.tar.gz.enc",
		"unrelated-object.txt",
		"hive-hub-backup-20260731T000000Z.tar.gz.enc",
	})

	code, out := runMainHelper(t, listOCIEnv(t, srv.URL), "list")
	if code != 0 {
		t.Fatalf("exit code = %d; want 0\n%s", code, out)
	}
	if strings.Contains(out, "unrelated-object.txt") {
		t.Errorf("list printed a non-backup object:\n%s", out)
	}
	newest := strings.Index(out, "hive-hub-backup-20260731T000000Z.tar.gz.enc")
	oldest := strings.Index(out, "hive-hub-backup-20260101T000000Z.tar.gz.enc")
	if newest == -1 || oldest == -1 {
		t.Fatalf("list output missing expected archives:\n%s", out)
	}
	if newest > oldest {
		t.Errorf("archives are not newest-first:\n%s", out)
	}
}

// An empty bucket is a valid (if worrying) answer: list must exit zero and
// print nothing rather than fabricate an error.
func TestMainListEmptyBucketExitsZero(t *testing.T) {
	srv := listOCIMock(t, nil)

	code, out := runMainHelper(t, listOCIEnv(t, srv.URL), "list")
	if code != 0 {
		t.Fatalf("exit code = %d; want 0\n%s", code, out)
	}
	if strings.Contains(out, "hive-hub-backup-") {
		t.Errorf("empty bucket printed archives:\n%s", out)
	}
}
