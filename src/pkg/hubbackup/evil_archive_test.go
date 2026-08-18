package hubbackup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// buildEvilArchive constructs a valid, correctly-signed archive whose manifest
// and entries reference a path-traversal filename. It exists to prove Extract
// refuses to write outside the destination directory.
func buildEvilArchive(t *testing.T, key []byte) []byte {
	t.Helper()

	const evilPath = "../../etc/passwd"
	payload := []byte("pwned")

	var raw strings.Builder
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: evilPath, Mode: 0o600, Size: int64(len(payload)), ModTime: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}

	man := Manifest{FormatVersion: backupFormatVersion, CreatedAt: time.Now().UTC()}
	manJSON, err := json.MarshalIndent(&man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: manifestName, Mode: 0o644, Size: int64(len(manJSON)), ModTime: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(manJSON); err != nil {
		t.Fatal(err)
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	sealed, err := Seal(key, []byte(raw.String()))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}
