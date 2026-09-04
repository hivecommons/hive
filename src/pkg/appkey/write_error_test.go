package appkey

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestResolver(t *testing.T) *Resolver {
	t.Helper()
	dir := t.TempDir()
	return &Resolver{
		DataDir:            dir,
		DataKeyPath:        filepath.Join(dir, "gh-app-key.pem"),
		ProvisionedDir:     filepath.Join(dir, "secrets"),
		ProvisionedKeyPath: filepath.Join(dir, "secrets", "gh-app-key.pem"),
		FileMode:           DefaultFileMode,
	}
}

func assertNoPerAppKey(t *testing.T, r *Resolver, appID int64) {
	t.Helper()
	if _, err := os.Stat(r.PerAppIDKeyPath(appID)); err == nil {
		t.Fatal("per-app key exists after failed write")
	}
}

func TestPerAppIDKeyPathFor(t *testing.T) {
	r := newTestResolver(t)

	if got := r.PerAppIDKeyPathFor(""); got != "" {
		t.Fatalf("empty app id path = %q, want empty", got)
	}
	if got, want := r.PerAppIDKeyPathFor("3568013"), r.PerAppIDKeyPath(3568013); got != want {
		t.Fatalf("string app id path = %q, want %q", got, want)
	}
}

func TestWritePerAppIDKeyRejectsInvalidInputWithoutCreatingKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		appID   int64
		pemData string
		wantErr string
	}{
		{
			name:    "non-positive app id",
			appID:   0,
			pemData: testRSAPrivateKeyPEM,
			wantErr: "non-positive app_id",
		},
		{
			name:    "not PEM",
			appID:   3568013,
			pemData: "not a private key",
			wantErr: "not PEM",
		},
		{
			name:    "unusable PEM",
			appID:   3568013,
			pemData: "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----",
			wantErr: "unusable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestResolver(t)
			fp, err := r.WritePerAppIDKey(tc.appID, tc.pemData)
			if err == nil {
				t.Fatal("WritePerAppIDKey succeeded, want error")
			}
			if fp != "" {
				t.Fatalf("fingerprint = %q, want empty on error", fp)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err, tc.wantErr)
			}
			if tc.appID > 0 {
				assertNoPerAppKey(t, r, tc.appID)
			}
		})
	}
}

func TestWritePerAppIDKeyFilesystemFailuresLeaveNoKey(t *testing.T) {
	t.Run("mkdirall fails when data dir is a file", func(t *testing.T) {
		parent := t.TempDir()
		dataDir := filepath.Join(parent, "not-a-dir")
		if err := os.WriteFile(dataDir, []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := &Resolver{DataDir: dataDir, FileMode: DefaultFileMode}

		_, err := r.WritePerAppIDKey(3568013, testRSAPrivateKeyPEM)
		if err == nil || !strings.Contains(err.Error(), "create app key dir") {
			t.Fatalf("error = %v, want create app key dir failure", err)
		}
		assertNoPerAppKey(t, r, 3568013)
	})

	t.Run("create temp fails in read-only data dir", func(t *testing.T) {
		r := newTestResolver(t)
		if err := os.Chmod(r.DataDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(r.DataDir, 0o700) })

		_, err := r.WritePerAppIDKey(3568013, testRSAPrivateKeyPEM)
		if err == nil || !strings.Contains(err.Error(), "create temp app key file") {
			t.Fatalf("error = %v, want create temp app key file failure", err)
		}
		assertNoPerAppKey(t, r, 3568013)
	})

	t.Run("rename fails when target path is a directory", func(t *testing.T) {
		r := newTestResolver(t)
		if err := os.MkdirAll(r.PerAppIDKeyPath(3568013), 0o700); err != nil {
			t.Fatal(err)
		}

		_, err := r.WritePerAppIDKey(3568013, testRSAPrivateKeyPEM)
		if err == nil || !strings.Contains(err.Error(), "rename app key into place") {
			t.Fatalf("error = %v, want rename app key into place failure", err)
		}
		matches, globErr := filepath.Glob(filepath.Join(r.DataDir, ".gh-app-key-3568013.pem.tmp*"))
		if globErr != nil {
			t.Fatal(globErr)
		}
		if len(matches) != 0 {
			t.Fatalf("temporary files left after failed rename: %v", matches)
		}
	})
}

type fakeTempKeyFile struct {
	name     string
	chmodErr error
	writeErr error
	syncErr  error
	closeErr error
	closed   bool
}

func (f *fakeTempKeyFile) Name() string { return f.name }
func (f *fakeTempKeyFile) Chmod(os.FileMode) error {
	return f.chmodErr
}
func (f *fakeTempKeyFile) WriteString(string) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return 1, nil
}
func (f *fakeTempKeyFile) Sync() error {
	return f.syncErr
}
func (f *fakeTempKeyFile) Close() error {
	f.closed = true
	return f.closeErr
}

func withFakeTempKeyFile(t *testing.T, f *fakeTempKeyFile) {
	t.Helper()
	orig := createTempKey
	createTempKey = func(string, string) (tempKeyFile, error) {
		return f, nil
	}
	t.Cleanup(func() { createTempKey = orig })
}

func TestWritePerAppIDKeyTempFileErrorBranches(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configure  func(*fakeTempKeyFile)
		wantErr    string
		wantClosed bool
	}{
		{
			name: "chmod failure closes temp file",
			configure: func(f *fakeTempKeyFile) {
				f.chmodErr = errors.New("chmod boom")
			},
			wantErr:    "chmod temp app key file",
			wantClosed: true,
		},
		{
			name: "write failure closes temp file",
			configure: func(f *fakeTempKeyFile) {
				f.writeErr = errors.New("write boom")
			},
			wantErr:    "write temp app key file",
			wantClosed: true,
		},
		{
			name: "sync failure closes temp file",
			configure: func(f *fakeTempKeyFile) {
				f.syncErr = errors.New("sync boom")
			},
			wantErr:    "sync temp app key file",
			wantClosed: true,
		},
		{
			name: "close failure is returned",
			configure: func(f *fakeTempKeyFile) {
				f.closeErr = errors.New("close boom")
			},
			wantErr:    "close temp app key file",
			wantClosed: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestResolver(t)
			f := &fakeTempKeyFile{name: filepath.Join(r.DataDir, ".gh-app-key-3568013.pem.tmpfake")}
			tc.configure(f)
			withFakeTempKeyFile(t, f)

			_, err := r.WritePerAppIDKey(3568013, testRSAPrivateKeyPEM)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if f.closed != tc.wantClosed {
				t.Fatalf("closed = %v, want %v", f.closed, tc.wantClosed)
			}
			assertNoPerAppKey(t, r, 3568013)
		})
	}
}

func TestHeldFingerprintsSkipsUnusableEntries(t *testing.T) {
	r := newTestResolver(t)
	if got := r.HeldFingerprints(); got != nil {
		t.Fatalf("missing data dir fingerprints = %v, want nil", got)
	}
	if err := os.MkdirAll(r.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"other.pem",
		"gh-app-key-.pem",
		"gh-app-key-0.pem",
		"gh-app-key--1.pem",
		"gh-app-key-not-a-number.pem",
		"gh-app-key-123.txt",
		"gh-app-key-456.pem",
	} {
		if err := os.WriteFile(filepath.Join(r.DataDir, name), []byte("not a key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(r.DataDir, "gh-app-key-789.pem"), 0o700); err != nil {
		t.Fatal(err)
	}

	if got := r.HeldFingerprints(); got != nil {
		t.Fatalf("unusable fingerprints = %v, want nil", got)
	}
}

func TestReportedFingerprintAndHasPerHiveKeyMissingOrShadowed(t *testing.T) {
	r := newTestResolver(t)
	if got := r.ReportedFingerprint("", 0); got != "" {
		t.Fatalf("missing reported fingerprint = %q, want empty", got)
	}
	if r.HasPerHiveKey("", 0) {
		t.Fatal("missing provisioned key reported as per-hive")
	}

	if err := os.MkdirAll(r.ProvisionedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	provisionedFP := writeTestKey(t, r.ProvisionedKeyPath)
	if got := r.ReportedFingerprint("", 0); got != provisionedFP {
		t.Fatalf("provisioned reported fingerprint = %q, want %q", got, provisionedFP)
	}
	if !r.HasPerHiveKey("", 0) {
		t.Fatal("provisioned key in effect was not reported as per-hive")
	}

	dataFP := writeTestKey(t, r.DataKeyPath)
	if got := r.ReportedFingerprint("", 0); got != dataFP {
		t.Fatalf("data reported fingerprint = %q, want %q", got, dataFP)
	}
	if r.HasPerHiveKey("", 0) {
		t.Fatal("shadowed provisioned key reported as per-hive")
	}
}
