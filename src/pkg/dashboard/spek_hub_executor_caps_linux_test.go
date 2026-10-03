//go:build linux

package dashboard

import (
	"errors"
	"slices"
	"testing"
)

func TestSpekHubDropCapsCommandLeavesCommandWithoutInheritableCaps(t *testing.T) {
	status := "Name:\thive\nCapInh:\t0000000000000000\nCapPrm:\t0000000000001000\nCapEff:\t0000000000001000\nCapAmb:\t0000000000000000\n"
	cmd := []string{"sh", "-lc", "copilot"}
	got, err := spekHubDropCapsCommandFor(cmd, status, func(string) (string, error) {
		t.Fatal("setpriv lookup not expected")
		return "", nil
	})
	if err != nil || !slices.Equal(got, cmd) {
		t.Fatalf("got %v, %v; want unchanged command", got, err)
	}
}

func TestSpekHubDropCapsCommandWrapsWithSetprivWhenAmbientCapsHeld(t *testing.T) {
	status := "CapInh:\t0000000000001000\nCapAmb:\t0000000000001000\n"
	got, err := spekHubDropCapsCommandFor([]string{"sh", "-lc", "copilot"}, status, func(string) (string, error) {
		return "/usr/bin/setpriv", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/setpriv", "--inh-caps=-all", "--ambient-caps=-all", "--no-new-privs", "--", "sh", "-lc", "copilot"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSpekHubDropCapsCommandFailsClosed(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not found") }
	if _, err := spekHubDropCapsCommandFor([]string{"sh"}, "CapInh:\t0\nCapAmb:\t0000000000001000\n", missing); err == nil {
		t.Fatal("ambient caps without setpriv must refuse the launch")
	}
	if _, err := spekHubDropCapsCommandFor([]string{"sh"}, "Name:\thive\n", missing); err == nil {
		t.Fatal("unreadable capability sets must refuse the launch")
	}
	if _, err := spekHubDropCapsCommandFor([]string{"sh"}, "CapInh:\tzz\nCapAmb:\t0\n", missing); err == nil {
		t.Fatal("unparseable capability sets must refuse the launch")
	}
}
