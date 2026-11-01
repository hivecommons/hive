package github

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDestinationMergeLeaseExclusiveAndReentrant(t *testing.T) {
	ctx, release, err := AcquireDestinationMerge(context.Background(), "", "ACME/widget", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, _, err := AcquireDestinationMerge(context.Background(), "", "acme/widget", "main"); !errors.Is(err, ErrDestinationMergeBusy) {
		t.Fatalf("overlapping mutation allowed: %v", err)
	}
	_, nested, err := AcquireDestinationMerge(ctx, "", "acme/widget", "main")
	if err != nil {
		t.Fatal(err)
	}
	nested()
	if _, _, err := AcquireDestinationMerge(ctx, "", "acme/widget", "other"); err == nil {
		t.Fatal("retarget borrowed a different destination")
	}
	_, other, err := AcquireDestinationMerge(context.Background(), "", "acme/widget", "MAIN")
	if err != nil {
		t.Fatal(err)
	}
	other()
}

func TestDestinationMergeDurableCrossProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "destination-merges")
	_, release, err := AcquireDestinationMerge(context.Background(), dir, "acme/widget", "main")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		release()
		t.Fatal(err)
	}
	run := func(mode string) {
		t.Helper()
		cmd := exec.Command(executable, "-test.run=^TestDestinationMergeChild$")
		cmd.Env = append(os.Environ(), "HIVE_MERGE_LOCK_TEST_DIR="+dir, "HIVE_MERGE_LOCK_TEST_MODE="+mode)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("cross-process %s: %v\n%s", mode, err, out)
		}
	}
	run("busy")
	release()
	run("acquire")
	// The child exited without explicitly releasing the file descriptor.
	_, release, err = AcquireDestinationMerge(context.Background(), dir, "acme/widget", "main")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestDestinationMergeChild(t *testing.T) {
	dir := os.Getenv("HIVE_MERGE_LOCK_TEST_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	_, _, err := AcquireDestinationMerge(context.Background(), dir, "acme/widget", "main")
	switch os.Getenv("HIVE_MERGE_LOCK_TEST_MODE") {
	case "busy":
		if !errors.Is(err, ErrDestinationMergeBusy) {
			t.Fatalf("guard did not exclude another process: %v", err)
		}
	case "acquire":
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown helper mode")
	}
}
