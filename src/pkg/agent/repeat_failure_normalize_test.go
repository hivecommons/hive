package agent

import "testing"

func TestNormalizeRepeatFailureCommand(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bare", "ls /nonexistent", "ls /nonexistent"},
		{"claude wrapper",
			"source /home/u/.claude/shell-snapshots/snapshot-bash-1700000000000-abc.sh && eval 'ls /nonexistent' \\< /dev/null && pwd -P >| /tmp/claude-1a2b-cwd",
			"ls /nonexistent"},
		{"claude wrapper embedded quote",
			`source /s/snap.sh && eval 'echo '\''hi'\''' < /dev/null`,
			"echo 'hi'"},
		{"claude wrapper no tail", "source /s/snap.sh && eval 'false'", "false"},
		{"gemini wrapper",
			"{ ls /nonexistent; }; __code=$?; pgrep -g 0 >/tmp/shell_pgrep_ab12.tmp 2>&1; exit $__code;",
			"ls /nonexistent"},
		{"gemini wrapper with shopt",
			"shopt -u promptvars nullglob extglob nocaseglob dotglob; { ls /nonexistent; }; __code=$?; pgrep -g 0 >/tmp/shell_pgrep_ab12.tmp 2>&1; exit $__code;",
			"ls /nonexistent"},
		{"whitespace", "  gh   pr\tview \n 123  ", "gh pr view 123"},
		{"different args stay different 123", "gh pr view 123", "gh pr view 123"},
		{"different args stay different 456", "gh pr view 456", "gh pr view 456"},
		{"tmp path", "cat /tmp/abc123/out.log", "cat <tmp>"},
		{"var tmp path", "cat /var/tmp/x.txt | wc -l", "cat <tmp> | wc -l"},
		{"TMPDIR path", "rm -f $TMPDIR/foo-1", "rm -f <tmp>"},
		{"TMPDIR braces", "rm -f ${TMPDIR}/foo", "rm -f <tmp>"},
		{"tmp quoted", `cat "/tmp/a-b"`, `cat "<tmp>"`},
		{"tmp assignment", "OUT=/tmp/x make", "OUT=<tmp> make"},
		{"non-tmp path kept", "cat /home/u/tmp/x /data/tmp/y", "cat /home/u/tmp/x /data/tmp/y"},
		{"iso timestamp", "git log --since=2026-08-04T10:11:12Z", "git log --since=<ts>"},
		{"iso timestamp offset", "git log --until 2026-08-04T10:11:12.123+02:00", "git log --until <ts>"},
		{"unix 10", "date -d @1754300000", "date -d @<ts>"},
		{"unix 13", "curl 'x?t=1754300000123'", "curl 'x?t=<ts>'"},
		{"short number kept", "gh pr view 1234567", "gh pr view 1234567"},
		{"long number kept", "echo 12345678901", "echo 12345678901"},
		{"empty", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeRepeatFailureCommand(tc.in); got != tc.want {
				t.Errorf("NormalizeRepeatFailureCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeRepeatFailureCommandWrappedEqualsBare(t *testing.T) {
	bare := NormalizeRepeatFailureCommand("ls /nonexistent")
	for _, wrapped := range []string{
		"source /home/u/.claude/shell-snapshots/snapshot-bash-1.sh && eval 'ls /nonexistent' \\< /dev/null && pwd -P >| /tmp/claude-x-cwd",
		"{ ls /nonexistent; }; __code=$?; pgrep -g 0 >/tmp/shell_pgrep_x.tmp 2>&1; exit $__code;",
	} {
		if got := NormalizeRepeatFailureCommand(wrapped); got != bare {
			t.Errorf("wrapped %q normalized to %q, want %q", wrapped, got, bare)
		}
	}
}

func TestNormalizeRepeatFailureCommandIdempotent(t *testing.T) {
	in := "source /s/snap.sh && eval 'cat /tmp/a-1754300000 2026-08-04T10:11:12Z'"
	once := NormalizeRepeatFailureCommand(in)
	if twice := NormalizeRepeatFailureCommand(once); twice != once {
		t.Errorf("not idempotent: %q then %q", once, twice)
	}
}

func TestRepeatFailureCommandExcluded(t *testing.T) {
	defaults := DefaultRepeatFailureExclusions()
	cases := []struct {
		name     string
		cmd      string
		prefixes []string
		want     bool
	}{
		{"default pr checks", "gh pr checks", defaults, true},
		{"default pr checks with args", "gh pr checks 123 --watch", defaults, true},
		{"default run watch", "gh run watch 99", defaults, true},
		{"pr view not excluded", "gh pr view 123", defaults, false},
		{"prefix only at start", "echo gh pr checks", defaults, false},
		{"custom prefix", "make wait-ready", []string{"make wait"}, true},
		{"custom prefix whitespace", "make wait-ready", []string{"  make   wait "}, true},
		{"empty entry ignored", "gh pr view 1", []string{"", "  "}, false},
		{"nil list", "gh pr checks", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepeatFailureCommandExcluded(NormalizeRepeatFailureCommand(tc.cmd), tc.prefixes); got != tc.want {
				t.Errorf("excluded(%q, %v) = %v, want %v", tc.cmd, tc.prefixes, got, tc.want)
			}
		})
	}
}

func TestRepeatFailureExclusionDefaultsAreCopied(t *testing.T) {
	d := DefaultRepeatFailureExclusions()
	if len(d) != 2 || d[0] != "gh pr checks" || d[1] != "gh run watch" {
		t.Fatalf("unexpected defaults: %v", d)
	}
	d[0] = "changed"
	if DefaultRepeatFailureExclusions()[0] != "gh pr checks" {
		t.Error("mutating the returned slice changed the defaults")
	}
}
