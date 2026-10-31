package toolapprove

import (
	"context"
	"fmt"
	"testing"
)

func TestDefaultSecurityScannerDestructiveCommandsWithoutTextFindings(t *testing.T) {
	scanner := NewDefaultSecurityScanner()
	// Isolate the built-in command check from the host's text scanner and
	// fakeTextScanner, which otherwise masks missed destructive commands.
	scanner.Text = textScannerFunc(func(string) TextVerdict { return TextVerdict{} })

	for _, command := range []string{
		"rm -rf /",
		"rm -rf / --no-preserve-root",
		"rm -rf --no-preserve-root /",
		"rm --no-preserve-root -rf /",
		"rm -rf -- /",
		"rm -rf -- ~",
		"rm -Rf -- $HOME",
		"rm -rf -- \"${HOME}\"",
		"rm --recursive --force /",
		"rm -r --force /",
		"rm --force -r /*",
		"rm -rfv /",
		"rm -rvf -- /.",
		"rm -rf -- /..",
		"rm -rf -- /./*",
		"rm -rf --no-preserve-root /./",
		"rm -rf --no-preserve-root /../",
		"rm --recursive --force /../",
		"rm --recursive --force ~/.",
		"rm -rf -- ~/.",
		"rm -rf -- $HOME/.",
		"rm -rf -- $HOME/..",
		"rm -rf -- ${HOME}/./",
		"sudo rm -rf --no-preserve-root /",
		"/bin/rm -rf --no-preserve-root /",
		"/usr/bin/rm -rf -- /",
		"/bin/rm --recursive --force -- ~",
		"sudo /bin/rm -rf --no-preserve-root /",
		"\"/bin/rm\" -rf -- /",
		"cd /tmp && rm -rf -- /; echo done",
		"rm -rf ~",
		"rm -rf /*",
		"rm -rf *",
		"rm -rf $HOME",
		"rm -rf $HOME/",
		"rm -rf $HOME/*",
		"rm -rf ~/",
		"rm -rf ~/*",
		"rm -rf $HOME/; echo done",
		"rm -RF /",
		"rm -rf //",
		"rm -rf /.",
		"rm -rf /..",
		"rm -rf /./",
		"rm -rf /../",
		"rm -rf /./*",
		"rm -rf /../*",
		"rm -rf ~/.",
		"rm -rf ~/..",
		"rm -rf $HOME/.",
		"rm -rf ${HOME}/./",
		"rm -rf \"/\"",
		"rm -rf '/'",
		"rm -r -f /",
		"rm -fr /",
		"rm -rf ${HOME}",
		"rm -rf \"${HOME}\"",
		"rm -rf /\n",
		"rm -rf /; echo done",
		"rm -rf ~&& echo done",
		"rm -rf /*| cat",
		"rm -rf $HOME&",
		":(){ :|:& };:",
		"echo before; :(){ :|:& };:",
		"mkfs /dev/sda",
		"mkfs.ext4 /dev/sda",
		"dd if=/dev/zero of=/dev/sda",
	} {
		t.Run(command, func(t *testing.T) {
			result, err := scanner.Scan(context.Background(), ToolRequest{Tool: "bash", Command: command})
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("destructive shell command detected: %q", command)
			if result.Passed || len(result.Violations) != 1 || result.Violations[0] != want {
				t.Fatalf("Scan(%q) = %+v, want only violation %q", command, result, want)
			}
		})
	}

	for _, command := range []string{
		"rm -rf /tmp/x",
		"rm -rf -- /tmp/x",
		"rm --recursive --force /tmp/x",
		"rm -rf --no-preserve-root /tmp/x",
		"rm -- /",
		"rm --force /",
		"rm -f -- ~",
		"rm -rf -- $HOME_backup",
		"rm -rf ./x",
		"rm -rf \"/tmp/x\"",
		"rm -rf //tmp/x",
		"rm -rf ${HOME}/build",
		"rm -rf ~/build",
		"rm -rf ~/.cache",
		"rm -rf /.cache",
		"rm -rf /...",
		"rm -rf ./build",
		"rm -rf $HOME/build",
		"rm -rf $HOME_backup",
		"rm -rf $HOME/*.tmp",
		"rm -rf ~/*.tmp",
		"rm -rf $HOMEDIR/",
		"rm -rf /*.tmp",
		"rm -rf *.tmp",
		"/bin/rm -rf ./build",
		"/usr/bin/rm -rf -- $HOME/build",
		"/bin/rmdir -- /",
		"echo harmless",
		"mkfshelper",
		"dd if=/dev/zeroes",
	} {
		t.Run(command, func(t *testing.T) {
			result, err := scanner.Scan(context.Background(), ToolRequest{Tool: "bash", Command: command})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Passed || len(result.Violations) != 0 {
				t.Fatalf("Scan(%q) = %+v, want clean scan", command, result)
			}
		})
	}
}
