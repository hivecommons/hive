package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/scheduler"
)

func TestGovernedEvidenceArtifactReaderRechecksReceiptAndSkipsBinary(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".visual-hive")
	if err := os.MkdirAll(filepath.Join(root, "evidence"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "screenshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	detail := []byte(`{"mutationOperator":"replace-success-with-500","survivor":"checkout-ready","missingTest":"assert the error recovery flow"}`)
	detailPath := filepath.Join(root, "evidence", "mutation.json")
	if err := os.WriteFile(detailPath, detail, 0o600); err != nil {
		t.Fatal(err)
	}
	pixels := []byte("not-decoded-png")
	if err := os.WriteFile(filepath.Join(root, "screenshots", "checkout.png"), pixels, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewGovernedEvidenceArtifactReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	request := scheduler.WorkerEvidenceArtifactRequest{Artifacts: []scheduler.EvidenceArtifact{
		{Reference: ".visual-hive/evidence/mutation.json", SHA256: governedEvidenceTestDigest(detail), Bytes: int64(len(detail)), Kind: "json", ContentType: "application/json"},
		{Reference: ".visual-hive/screenshots/checkout.png", SHA256: governedEvidenceTestDigest(pixels), Bytes: int64(len(pixels)), Kind: "image", ContentType: "image/png"},
	}}
	read, err := reader.ReadGovernedEvidenceArtifacts(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Artifacts) != 1 || read.Artifacts[0].Reference != request.Artifacts[0].Reference || read.Artifacts[0].Content != string(detail) {
		t.Fatalf("structured artifact selection lost exact bytes or embedded binary content: %+v", read)
	}

	if err := os.WriteFile(detailPath, []byte(`{"tampered":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadGovernedEvidenceArtifacts(request); err == nil || !strings.Contains(err.Error(), "receipt") && !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("changed artifact bytes were accepted under the old receipt: %v", err)
	}
}

func TestGovernedEvidenceArtifactReaderRejectsUnsafePathAndLink(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, ".visual-hive")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	reader, err := NewGovernedEvidenceArtifactReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	content := []byte(`{}`)
	receipt := scheduler.EvidenceArtifact{Reference: "../escape.json", SHA256: governedEvidenceTestDigest(content), Bytes: int64(len(content)), Kind: "json", ContentType: "application/json"}
	if _, err := reader.ReadGovernedEvidenceArtifacts(scheduler.WorkerEvidenceArtifactRequest{Artifacts: []scheduler.EvidenceArtifact{receipt}}); err == nil {
		t.Fatal("evidence reader accepted a path outside the verified root")
	}

	target := filepath.Join(parent, "target.json")
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation is unavailable on this platform: %v", err)
	}
	receipt.Reference = ".visual-hive/linked.json"
	if _, err := reader.ReadGovernedEvidenceArtifacts(scheduler.WorkerEvidenceArtifactRequest{Artifacts: []scheduler.EvidenceArtifact{receipt}}); err == nil {
		t.Fatal("evidence reader accepted a symlinked artifact")
	}
}

func governedEvidenceTestDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
