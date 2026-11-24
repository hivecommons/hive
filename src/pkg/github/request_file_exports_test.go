package github

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRequestFileExports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	if err := WriteRequestResult(path, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("WriteRequestResult: %v", err)
	}
	data, err := ReadRequestFile(path)
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("ReadRequestFile = %q, %v", data, err)
	}
	if uid := RequestFileOwnerUID(path); uid != os.Getuid() {
		t.Errorf("RequestFileOwnerUID = %d, want %d", uid, os.Getuid())
	}
	if !RequestFileTorn(path, time.Now()) {
		t.Error("a fresh file is not reported as possibly torn")
	}
	if RequestFileTorn(path, time.Now().Add(time.Hour)) {
		t.Error("an old file is still reported as possibly torn")
	}

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if _, err := ReadRequestFile(link); !IsRequestFileRejected(err) {
		t.Errorf("symlink read err = %v, want a rejection", err)
	}
	if _, err := ReadRequestFile(filepath.Join(dir, "missing.json")); err == nil || IsRequestFileRejected(err) {
		t.Errorf("missing file err = %v, want a plain not-found error", err)
	}

	if InvalidPRHeadReason("fix-1") != "" || InvalidPRHeadReason("HEAD") == "" {
		t.Error("InvalidPRHeadReason does not match invalidHeadReason")
	}
}

func TestReservedLabelForAgentsExport(t *testing.T) {
	for _, l := range []string{"hold", "needs-human", "stage/010-actionable", "hive/x", "queue"} {
		if !ReservedLabelForAgents(l, "queue") {
			t.Errorf("%q not reserved", l)
		}
	}
	if ReservedLabelForAgents("kind/bug", "queue") {
		t.Error("kind/bug reserved")
	}
}
