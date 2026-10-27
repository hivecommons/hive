package proxy

import (
	"log/slog"
	"net/http"
	"testing"
	"time"
)

type fakeRESTAccounting struct {
	plain, injected []string
	remaining       int
}

func (f *fakeRESTAccounting) RecordRESTRequest(caller, method, path string, status int, _ http.Header) {
	f.plain = append(f.plain, caller+" "+method+" "+path)
}

func (f *fakeRESTAccounting) RecordInjectedRESTRequest(caller, method, path string, status int, _ http.Header) {
	f.injected = append(f.injected, caller+" "+method+" "+path)
}

func (f *fakeRESTAccounting) CoreRateLimitSnapshot() (int, time.Time, bool) {
	return f.remaining, time.Time{}, true
}

// The proxy splits relayed REST responses by which credential made them and
// reads the shared core-bucket snapshot through the same seam; with nothing
// installed it records nothing and the budget sees no snapshot.
func TestSetGitHubRESTAccountingRoutesRecordsAndSnapshot(t *testing.T) {
	p := &GitHubProxy{logger: slog.Default()}
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/o/r/pulls", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}

	p.recordGitHubREST("scanner", req, resp)
	if _, _, ok := p.agentGitHubReserveSnapshot(); ok {
		t.Fatal("snapshot reported ok with no accounting installed")
	}

	acct := &fakeRESTAccounting{remaining: 4321}
	p.SetGitHubRESTAccounting(acct)
	p.recordGitHubREST("scanner", req, resp)
	p.injectGHAuth = true
	p.recordGitHubREST("scanner", req, resp)
	p.recordGitHubREST(internalCallerName, req, resp)

	if len(acct.plain) != 2 || acct.plain[0] != "agent:scanner GET /repos/o/r/pulls" || acct.plain[1] != "agent:"+internalCallerName+" GET /repos/o/r/pulls" {
		t.Fatalf("plain records = %q", acct.plain)
	}
	if len(acct.injected) != 1 || acct.injected[0] != "agent:scanner GET /repos/o/r/pulls" {
		t.Fatalf("injected records = %q", acct.injected)
	}
	if remaining, _, ok := p.agentGitHubReserveSnapshot(); !ok || remaining != 4321 {
		t.Fatalf("snapshot = %d, %v; want 4321 through the seam", remaining, ok)
	}

	p.SetGitHubRESTAccounting(nil)
	if _, _, ok := p.agentGitHubReserveSnapshot(); ok {
		t.Fatal("snapshot still reported ok after the accounting was removed")
	}
}

// A proxy with no usage sink keeps serving: every record helper is a no-op.
func TestUsageRecordHelpersTolerateNilSink(t *testing.T) {
	p := &GitHubProxy{logger: slog.Default()}
	p.recordUsage("a", "m", 1, 2)
	p.recordCopilotUsage("a", "m", 1, 2)
	p.recordCopilotUsageWithCache("a", "m", 1, 2, 3, 4)
	sink := &memUsageSink{}
	p.SetTokenSink(sink)
	p.recordUsage("a", "m", 1, 2)
	p.recordCopilotUsage("a", "m", 3, 4)
	p.recordCopilotUsageWithCache("a", "m", 5, 6, 7, 8)
	recs := sink.forAgent("a")
	if len(recs) != 3 || recs[0].copilot || !recs[1].copilot || recs[2].cacheCreation != 8 {
		t.Fatalf("records = %+v", recs)
	}
}
