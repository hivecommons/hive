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
		"rm -rf ~/build",
		"rm -rf $HOME/build",
		"rm -rf $HOME_backup",
		"rm -rf $HOME/*.tmp",
		"rm -rf ~/*.tmp",
		"rm -rf $HOMEDIR/",
		"rm -rf /*.tmp",
		"rm -rf *.tmp",
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
