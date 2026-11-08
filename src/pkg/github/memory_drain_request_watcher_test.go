package github

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessMemoryDrainRequestsOnce(t *testing.T) {
	dir := t.TempDir()
	prev := memoryDrainRequestDirForTest
	memoryDrainRequestDirForTest = dir
	t.Cleanup(func() { memoryDrainRequestDirForTest = prev })

	write := func(name string, req MemoryDrainRequest) string {
		b, _ := json.Marshal(req)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	okPath := write("ok.json", MemoryDrainRequest{Agent: "scanner", Cwd: "/data/agents/scanner/w", Truncate: true})
	deniedPath := write("denied.json", MemoryDrainRequest{Agent: "builder"})

	var got []MemoryDrainRequest
	var uids []int
	handle := func(req MemoryDrainRequest, fileUID int) MemoryDrainResponse {
		got, uids = append(got, req), append(uids, fileUID)
		if req.Agent != "scanner" {
			return MemoryDrainResponse{Agent: req.Agent, Error: "an agent may only drain its own memory"}
		}
		return MemoryDrainResponse{OK: true, Ran: true, Agent: req.Agent, Drained: 1, Truncated: 1}
	}
	ProcessMemoryDrainRequestsOnce(nil, handle, time.Now)

	if len(got) != 2 {
		t.Fatalf("handled %+v", got)
	}
	for _, uid := range uids {
		if uid != os.Getuid() {
			t.Fatalf("fileUID = %d, want the request file's owner %d", uid, os.Getuid())
		}
	}
	if _, err := os.Stat(okPath); !os.IsNotExist(err) {
		t.Fatalf("handled request not removed: %v", err)
	}
	if _, err := os.Stat(deniedPath + ".denied"); err != nil {
		t.Fatalf("refused request not set aside: %v", err)
	}
	var resp MemoryDrainResponse
	b, err := os.ReadFile(filepath.Join(dir, "ok.result.json"))
	if err != nil || json.Unmarshal(b, &resp) != nil || !resp.OK || resp.Truncated != 1 || resp.At == "" {
		t.Fatalf("result = %s, err=%v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(dir, "denied.result.json"))
	if err != nil || json.Unmarshal(b, &resp) != nil || resp.OK || resp.Error == "" {
		t.Fatalf("denied result = %s, err=%v", b, err)
	}
}
