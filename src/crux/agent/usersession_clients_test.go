package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestUserSessionProbeDetachesOrphanedClients: the probe lists the attached
// clients, detaches exactly those judged orphaned, leaves the unknown ones
// alone and reports the survivors as the attached count.
func TestUserSessionProbeDetachesOrphanedClients(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			return []byte("0 4 1700000000\n"), nil
		case "list-clients":
			return []byte("111 /dev/pts/1\n222 /dev/pts/2\n333 /dev/pts/3\n444 /dev/pts/4\nnot a client line\n"), nil
		}
		return nil, nil
	}}
	r := newTestSessionRuntime(t, f, nil)
	r.orphaned = func(pid int) (bool, bool) {
		switch pid {
		case 222, 444:
			return true, true
		case 333:
			return false, false // cannot tell: must be kept
		}
		return false, true
	}
	p := r.Probe(testSessionID)
	if !p.Exists || p.Dead || p.Attached != 2 {
		t.Errorf("probe = %+v, want 2 live clients", p)
	}
	var detached []string
	for _, c := range f.calls {
		if c.args[0] == "detach-client" {
			if c.user != testSessionSpecUser {
				t.Errorf("detach ran as %q, want the session identity %q", c.user, testSessionSpecUser)
			}
			detached = append(detached, strings.Join(c.args, " "))
		}
	}
	want := []string{"detach-client -t /dev/pts/2", "detach-client -t /dev/pts/4"}
	if strings.Join(detached, ",") != strings.Join(want, ",") {
		t.Errorf("detaches = %v, want %v", detached, want)
	}
}

// TestUserSessionProbeKeepsTmuxCountWhenReaperCannotRun: a failed detach keeps
// the client in the count; a failed list, a dead pane or no judge at all fall
// back to tmux's own session_attached and never detach anything.
func TestUserSessionProbeKeepsTmuxCountWhenReaperCannotRun(t *testing.T) {
	listErr := false
	f := &fakeTmux{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			return []byte("0 2 1700000000\n"), nil
		case "list-clients":
			if listErr {
				return nil, errors.New("no server")
			}
			return []byte("111 /dev/pts/1\n222 /dev/pts/2\n"), nil
		case "detach-client":
			return []byte("no such client"), errors.New("exit 1")
		}
		return nil, nil
	}}
	r := newTestSessionRuntime(t, f, nil)
	r.orphaned = func(int) (bool, bool) { return true, true }
	if p := r.Probe(testSessionID); p.Attached != 2 {
		t.Errorf("failed detaches dropped clients from the count: %+v", p)
	}
	listErr = true
	if p := r.Probe(testSessionID); p.Attached != 2 {
		t.Errorf("list failure changed the count: %+v", p)
	}
	listErr = false
	f.calls = nil
	r.orphaned = nil
	if p := r.Probe(testSessionID); p.Attached != 2 {
		t.Errorf("no judge changed the count: %+v", p)
	}
	for _, v := range f.verbs() {
		if v == "list-clients" || v == "detach-client" {
			t.Errorf("reaper ran without a judge: %v", f.verbs())
		}
	}
	f.calls = nil
	r.orphaned = func(int) (bool, bool) { return true, true }
	f.handler = func(args []string) ([]byte, error) { return []byte("1 2 1700000000\n"), nil }
	if p := r.Probe(testSessionID); !p.Dead || len(f.calls) != 1 {
		t.Errorf("a dead pane was reaped: %+v %v", p, f.verbs())
	}
}

func TestUserSessionReapIsBounded(t *testing.T) {
	var listing strings.Builder
	for i := 1; i <= userSessionReapMax+5; i++ {
		listing.WriteString(strconv.Itoa(1000+i) + " /dev/pts/" + strconv.Itoa(i) + "\n")
	}
	f := &fakeTmux{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			return []byte("0 " + strconv.Itoa(userSessionReapMax+5) + " 1700000000\n"), nil
		case "list-clients":
			return []byte(listing.String()), nil
		}
		return nil, nil
	}}
	r := newTestSessionRuntime(t, f, nil)
	r.orphaned = func(int) (bool, bool) { return true, true }
	p := r.Probe(testSessionID)
	detaches := 0
	for _, v := range f.verbs() {
		if v == "detach-client" {
			detaches++
		}
	}
	if detaches != userSessionReapMax || p.Attached != 5 {
		t.Errorf("detaches = %d (want %d), attached = %d (want 5)", detaches, userSessionReapMax, p.Attached)
	}
}

func TestParseUserSessionClients(t *testing.T) {
	got := parseUserSessionClients([]byte("12 /dev/pts/3\n\nabc /dev/pts/4\n0 /dev/pts/5\n7 pts/6\n8 /dev/pts/7 extra\n9 /dev/pts/8\n"))
	if len(got) != 2 || got[0] != (userSessionClient{12, "/dev/pts/3"}) || got[1] != (userSessionClient{9, "/dev/pts/8"}) {
		t.Errorf("parsed %+v", got)
	}
}

// fakeProc writes a /proc-shaped tree: self, and per pid a status with PPid
// and a comm.
func fakeProc(t *testing.T, procs map[int][2]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	for pid, pc := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		status := "Name:\t" + pc[1] + "\nState:\tS (sleeping)\nPid:\t" + strconv.Itoa(pid) + "\nPPid:\t" + pc[0] + "\n"
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(pc[1]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestProcClientOrphaned(t *testing.T) {
	root := fakeProc(t, map[int][2]string{
		7:   {"1", "ttyd"},
		50:  {"7", "bash"},   // wrapper under ttyd
		100: {"50", "tmux"},  // healthy client
		60:  {"1", "bash"},   // wrapper reparented: ttyd gone
		300: {"60", "tmux"},  // orphaned client
		400: {"0", "tmux"},   // attached from an exec shell outside ttyd
		500: {"501", "tmux"}, // ancestor unreadable
	})
	cases := []struct {
		pid             int
		orphaned, known bool
	}{
		{100, false, true},
		{300, true, true},
		{400, true, true},
		{200, true, true}, // process gone
		{500, false, false},
		{1, false, false},
		{0, false, false},
	}
	for _, c := range cases {
		o, k := procClientOrphanedIn(root, c.pid)
		if o != c.orphaned || k != c.known {
			t.Errorf("pid %d: orphaned=%v known=%v, want %v %v", c.pid, o, k, c.orphaned, c.known)
		}
	}
	// No procfs at all: nothing can be judged, so nothing is ever reaped.
	if o, k := procClientOrphanedIn(filepath.Join(root, "missing"), 100); o || k {
		t.Errorf("without procfs: orphaned=%v known=%v", o, k)
	}
}

func TestProcClientOrphanedWalkIsBounded(t *testing.T) {
	// A chain deeper than the bound with no ttyd and no top: unknown, not
	// orphaned, and it terminates.
	procs := map[int][2]string{}
	for i := 2; i <= userSessionAncestryDepth+5; i++ {
		procs[i] = [2]string{strconv.Itoa(i + 1), "bash"}
	}
	procs[userSessionAncestryDepth+6] = [2]string{"2", "bash"} // cycle, never reaches 1
	root := fakeProc(t, procs)
	if o, k := procClientOrphanedIn(root, 2); o || k {
		t.Errorf("deep chain: orphaned=%v known=%v", o, k)
	}
}
